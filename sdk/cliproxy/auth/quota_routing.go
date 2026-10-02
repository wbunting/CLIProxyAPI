package auth

import (
	"strconv"
	"strings"
	"time"
)

// quotaHealthyThreshold is deliberately conservative: preference remains the
// primary policy while a route has comfortable headroom. Quota and reset timing
// override preference only when the preferred route is becoming risky.
const quotaHealthyThreshold = 0.25

type quotaRouteObservation struct {
	remaining  float64
	known      bool
	resetAt    time.Time
	resetKnown bool
}

// RemainingQuotaFraction reports the remaining quota fraction in [0,1] implied by
// a provider's observed quota signals, and whether any usable signal was present.
// The most constrained window wins because that window can block the route first.
func RemainingQuotaFraction(provider, model string, signals map[string]string) (float64, bool) {
	observation := quotaObservation(provider, model, signals, time.Time{})
	return observation.remaining, observation.known
}

func quotaObservation(provider, model string, signals map[string]string, observedAt time.Time) quotaRouteObservation {
	if len(signals) == 0 {
		return quotaRouteObservation{}
	}
	switch strings.ToLower(strings.TrimSpace(provider)) {
	case "claude":
		windows := []string{"5h", "7d"}
		if strings.Contains(strings.ToLower(model), "fable") {
			windows = append(windows, "7d_oi")
		}
		var best quotaRouteObservation
		for _, window := range windows {
			value, ok := quotaSignalFloat(signals, "Anthropic-Ratelimit-Unified-"+window+"-Utilization")
			if !ok {
				continue
			}
			candidate := quotaRouteObservation{
				remaining:  clampQuotaFraction(1 - value),
				known:      true,
				resetAt:    quotaSignalTime(signals, "Anthropic-Ratelimit-Unified-"+window+"-Reset", observedAt),
				resetKnown: false,
			}
			candidate.resetKnown = !candidate.resetAt.IsZero()
			if !best.known || candidate.remaining < best.remaining {
				best = candidate
			}
		}
		if !best.resetKnown {
			best.resetAt = quotaSignalTime(signals, "Anthropic-Ratelimit-Unified-Reset", observedAt)
			best.resetKnown = !best.resetAt.IsZero()
		}
		return best
	case "codex":
		var best quotaRouteObservation
		for key, value := range signals {
			lowerKey := strings.ToLower(strings.TrimSpace(key))
			if !strings.HasSuffix(lowerKey, "-used-percent") {
				continue
			}
			used, errParse := strconv.ParseFloat(strings.TrimSpace(value), 64)
			if errParse != nil {
				continue
			}
			prefix := strings.TrimSuffix(key, "-Used-Percent")
			candidate := quotaRouteObservation{
				remaining: clampQuotaFraction(1 - used/100),
				known:     true,
			}
			candidate.resetAt, candidate.resetKnown = quotaSignalTimeForPrefix(signals, prefix, observedAt)
			if !best.known || candidate.remaining < best.remaining {
				best = candidate
			}
		}
		return best
	default:
		return quotaRouteObservation{}
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

func quotaSignalTime(signals map[string]string, name string, observedAt time.Time) time.Time {
	for key, value := range signals {
		if strings.EqualFold(strings.TrimSpace(key), name) {
			return parseQuotaRoutingTime(value, observedAt)
		}
	}
	return time.Time{}
}

func quotaSignalTimeForPrefix(signals map[string]string, prefix string, observedAt time.Time) (time.Time, bool) {
	if reset := quotaSignalTime(signals, prefix+"-Reset-At", observedAt); !reset.IsZero() {
		return reset, true
	}
	if resetAfter, ok := quotaSignalFloat(signals, prefix+"-Reset-After-Seconds"); ok && resetAfter >= 0 {
		base := observedAt
		if base.IsZero() {
			base = time.Now()
		}
		return base.Add(time.Duration(resetAfter * float64(time.Second))), true
	}
	return time.Time{}, false
}

func parseQuotaRoutingTime(value string, observedAt time.Time) time.Time {
	value = strings.TrimSpace(value)
	if seconds, errParse := strconv.ParseInt(value, 10, 64); errParse == nil && seconds > 0 {
		return time.Unix(seconds, 0)
	}
	if parsed, errParse := time.Parse(time.RFC3339, value); errParse == nil {
		return parsed
	}
	return time.Time{}
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

func routeQuotaObservation(auth *Auth, selectionModel string) quotaRouteObservation {
	if auth == nil {
		return quotaRouteObservation{}
	}
	provider := strings.ToLower(strings.TrimSpace(auth.Provider))
	if state := existingModelState(auth, selectionModel); state != nil {
		observation := quotaObservation(provider, selectionModel, state.Quota.Signals, state.Quota.ObservedAt)
		if observation.known || observation.resetKnown {
			return observation
		}
	}
	return quotaObservation(provider, selectionModel, auth.Quota.Signals, auth.Quota.ObservedAt)
}

// narrowToSaferQuotaCandidates keeps the configured preference as the default
// policy. It only overrides preference when every highest-priority candidate is
// known to be low on quota. Healthy alternatives then win; if every alternative
// is also low, the route with the earliest known reset wins, followed by the
// route with the most remaining quota. Missing reset information is treated as
// riskier than a known reset because it cannot be safely scheduled around.
func (m *Manager) narrowToSaferQuotaCandidates(candidates []*Auth, routeModel string, now time.Time) []*Auth {
	if m == nil || len(candidates) < 2 || strings.TrimSpace(routeModel) == "" {
		return candidates
	}

	observations := make([]quotaRouteObservation, len(candidates))
	maxPriority := 0
	for i, candidate := range candidates {
		observations[i] = routeQuotaObservation(candidate, m.selectionModelForAuth(candidate, routeModel))
		priority := authPriority(candidate)
		if i == 0 || priority > maxPriority {
			maxPriority = priority
		}
	}

	// Preference wins whenever the preferred route is healthy or its quota is
	// unknown. Unknown telemetry is not evidence that the route is exhausted.
	preferredLow := false
	for i, candidate := range candidates {
		if authPriority(candidate) != maxPriority {
			continue
		}
		observation := observations[i]
		if !observation.known || observation.remaining >= quotaHealthyThreshold {
			return candidates
		}
		preferredLow = true
	}
	if !preferredLow {
		return candidates
	}

	// Prefer known healthy alternatives over a low preferred route.
	healthy := make([]*Auth, 0, len(candidates))
	for i, candidate := range candidates {
		if observations[i].known && observations[i].remaining >= quotaHealthyThreshold {
			healthy = append(healthy, candidate)
		}
	}
	if len(healthy) > 0 {
		return healthy
	}

	// All known alternatives are low. Keep only candidates with the best reset
	// horizon, then let normal model priority and round-robin resolve ties.
	best := make([]*Auth, 0, len(candidates))
	var bestObservation quotaRouteObservation
	for i, candidate := range candidates {
		observation := observations[i]
		if !observation.known {
			continue
		}
		if len(best) == 0 || saferLowQuota(observation, bestObservation, now) {
			best = best[:0]
			bestObservation = observation
		}
		if sameLowQuotaRisk(observation, bestObservation, now) {
			best = append(best, candidate)
		}
	}
	if len(best) == 0 {
		return candidates
	}
	return best
}

func saferLowQuota(candidate, current quotaRouteObservation, now time.Time) bool {
	if candidate.resetKnown != current.resetKnown {
		return candidate.resetKnown
	}
	if candidate.resetKnown && !candidate.resetAt.Equal(current.resetAt) {
		return candidate.resetAt.Before(current.resetAt)
	}
	return candidate.remaining > current.remaining
}

func sameLowQuotaRisk(a, b quotaRouteObservation, now time.Time) bool {
	if a.resetKnown != b.resetKnown {
		return false
	}
	if a.resetKnown && !a.resetAt.Equal(b.resetAt) {
		return false
	}
	_ = now
	return a.remaining == b.remaining
}
