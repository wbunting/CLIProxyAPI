package auth

import (
	"strconv"
	"strings"
)

// quotaBandCount is the number of coarse remaining-quota bands used when several
// concrete routes share one alias.
//
// Quota telemetry is a snapshot of the last upstream response for a credential,
// not a continuously sampled counter, so it is too sparse and too bursty to rank
// routes on a continuous scale. Coarse bands keep routing stable: a route only
// loses to another once their observed remaining quota differs by a full band.
const quotaBandCount = 4

// quotaBandUnknown is the band assigned to candidates with no usable remaining-quota
// signal. Missing telemetry is not evidence of exhaustion, so unknown candidates join
// the top band and remain ordered by configured model preference. This also keeps
// selection unchanged for deployments where no provider reports quota at all.
const quotaBandUnknown = quotaBandCount - 1

// RemainingQuotaFraction reports the remaining quota fraction in [0,1] implied by a
// provider's observed quota signals, and whether any usable signal was present.
//
// Only providers that publish a usage watermark are supported (Claude unified
// rate-limit utilization and Codex used-percent windows). The most constrained
// window wins, because the first window to reach its limit blocks the route.
func RemainingQuotaFraction(provider, model string, signals map[string]string) (float64, bool) {
	if len(signals) == 0 {
		return 0, false
	}
	switch strings.ToLower(strings.TrimSpace(provider)) {
	case "claude":
		windows := []string{"5h", "7d"}
		if strings.Contains(strings.ToLower(model), "fable") {
			windows = append(windows, "7d_oi")
		}
		remaining, found := 1.0, false
		for _, window := range windows {
			value, ok := quotaSignalFloat(signals, "Anthropic-Ratelimit-Unified-"+window+"-Utilization")
			if !ok {
				continue
			}
			if candidate := clampQuotaFraction(1 - value); !found || candidate < remaining {
				remaining = candidate
				found = true
			}
		}
		return remaining, found
	case "codex":
		remaining, found := 1.0, false
		for key, value := range signals {
			if !strings.HasSuffix(strings.ToLower(strings.TrimSpace(key)), "-used-percent") {
				continue
			}
			used, errParse := strconv.ParseFloat(strings.TrimSpace(value), 64)
			if errParse != nil {
				continue
			}
			if candidate := clampQuotaFraction(1 - used/100); !found || candidate < remaining {
				remaining = candidate
				found = true
			}
		}
		return remaining, found
	default:
		return 0, false
	}
}

func quotaSignalFloat(signals map[string]string, name string) (float64, bool) {
	for key, value := range signals {
		if !strings.EqualFold(strings.TrimSpace(key), name) {
			continue
		}
		parsed, errParse := strconv.ParseFloat(strings.TrimSpace(value), 64)
		return parsed, errParse == nil
	}
	return 0, false
}

func clampQuotaFraction(value float64) float64 {
	if value < 0 {
		return 0
	}
	if value > 1 {
		return 1
	}
	return value
}

// quotaBandFromRemaining maps a remaining fraction to a coarse band index.
func quotaBandFromRemaining(remaining float64) int {
	band := int(clampQuotaFraction(remaining) * float64(quotaBandCount))
	if band >= quotaBandCount {
		band = quotaBandCount - 1
	}
	if band < 0 {
		band = 0
	}
	return band
}

// routeQuotaBand reports the coarse remaining-quota band observed for one credential
// on one concrete upstream model.
//
// Model-level observations win over credential-wide ones: a credential-wide snapshot
// describes the whole account, while a per-model snapshot describes the exact route
// being scheduled. Cooldown state is deliberately not consulted here; blocked
// credentials and blocked models are already removed by isAuthBlockedForModel before
// banding runs.
func routeQuotaBand(auth *Auth, selectionModel string) (int, bool) {
	if auth == nil {
		return quotaBandUnknown, false
	}
	provider := strings.ToLower(strings.TrimSpace(auth.Provider))
	if state := existingModelState(auth, selectionModel); state != nil {
		if remaining, ok := RemainingQuotaFraction(provider, selectionModel, state.Quota.Signals); ok {
			return quotaBandFromRemaining(remaining), true
		}
	}
	if remaining, ok := RemainingQuotaFraction(provider, selectionModel, auth.Quota.Signals); ok {
		return quotaBandFromRemaining(remaining), true
	}
	return quotaBandUnknown, false
}

// narrowToBestQuotaBand keeps only the candidates sitting in the highest observed
// remaining-quota band.
//
// It applies only when the requested route resolves to more than one concrete upstream
// model, i.e. when the caller asked for an alias/tier pool. An explicit concrete model
// request, or a pool where every credential serves the same upstream model, is returned
// untouched so existing credential round-robin semantics are preserved.
//
// Candidates are returned in their input order, so the caller's later priority bucketing
// and round-robin rotation stay deterministic.
func (m *Manager) narrowToBestQuotaBand(candidates []*Auth, routeModel string) []*Auth {
	if m == nil || len(candidates) < 2 || strings.TrimSpace(routeModel) == "" {
		return candidates
	}

	bands := make([]int, len(candidates))
	distinctModels := make(map[string]struct{}, len(candidates))
	bestBand := 0
	bestCount := 0
	anyKnown := false
	for i, candidate := range candidates {
		selectionModel := m.selectionModelForAuth(candidate, routeModel)
		if key := canonicalModelKey(selectionModel); key != "" {
			distinctModels[key] = struct{}{}
		}
		band, known := routeQuotaBand(candidate, selectionModel)
		anyKnown = anyKnown || known
		bands[i] = band
		switch {
		case bestCount == 0 || band > bestBand:
			bestBand = band
			bestCount = 1
		case band == bestBand:
			bestCount++
		}
	}
	if !anyKnown || len(distinctModels) < 2 || bestCount == len(candidates) {
		return candidates
	}

	out := make([]*Auth, 0, bestCount)
	for i, candidate := range candidates {
		if bands[i] == bestBand {
			out = append(out, candidate)
		}
	}
	return out
}
