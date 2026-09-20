package management

import (
	"encoding/base64"
	"encoding/json"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/redisqueue"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

const modelRouteUsageSnapshotLimit = 20000

type modelRouteMember struct {
	Provider       string  `json:"provider"`
	Model          string  `json:"model"`
	Priority       int     `json:"priority"`
	Available      bool    `json:"available"`
	QuotaRemaining float64 `json:"quota_remaining,omitempty"`
	QuotaKnown     bool    `json:"quota_known"`
	ResetAt        string  `json:"reset_at,omitempty"`
	LastUsedAt     string  `json:"last_used_at,omitempty"`
	Requests       int     `json:"requests"`
	Failures       int     `json:"failures"`
	InputTokens    int64   `json:"input_tokens"`
	OutputTokens   int64   `json:"output_tokens"`
}

type modelRouteView struct {
	Alias                   string             `json:"alias"`
	Kind                    string             `json:"kind"`
	DisplayName             string             `json:"display_name"`
	Members                 []modelRouteMember `json:"members"`
	Active                  *modelRouteMember  `json:"active,omitempty"`
	Next                    *modelRouteMember  `json:"next,omitempty"`
	AggregateRemaining      float64            `json:"aggregate_remaining,omitempty"`
	AggregateRemainingKnown bool               `json:"aggregate_remaining_known"`
	ObservedAt              string             `json:"observed_at"`
}

type routeUsageStats struct {
	LastUsedAt   time.Time
	Requests     int
	Failures     int
	InputTokens  int64
	OutputTokens int64
}

type persistedUsageEnvelope struct {
	Payload string `json:"payload"`
}

type routeUsageRecord struct {
	Timestamp string `json:"timestamp"`
	Provider  string `json:"provider"`
	Model     string `json:"model"`
	Alias     string `json:"alias"`
	Failed    bool   `json:"failed"`
	Tokens    struct {
		InputTokens  int64 `json:"input_tokens"`
		OutputTokens int64 `json:"output_tokens"`
	} `json:"tokens"`
}

// GetModelRoutes exposes role/tier aliases with their ordered candidates and
// best-effort live quota/usage state for the management dashboard.
func (h *Handler) GetModelRoutes(c *gin.Context) {
	if h == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "handler unavailable"})
		return
	}
	h.mu.Lock()
	var cfg *config.Config
	if h.cfg != nil {
		cfg = h.cfg.CloneForRuntime()
	}
	manager := h.authManager
	h.mu.Unlock()
	if cfg == nil {
		c.JSON(http.StatusOK, gin.H{"observed_at": time.Now().UTC(), "routes": []modelRouteView{}})
		return
	}

	now := time.Now().UTC()
	auths := []*coreauth.Auth(nil)
	if manager != nil {
		auths = manager.List()
	}
	usage := collectModelRouteUsage(redisqueue.SnapshotNewest(modelRouteUsageSnapshotLimit))
	routes := buildModelRouteViews(cfg, auths, usage, now)
	c.JSON(http.StatusOK, gin.H{"observed_at": now, "routes": routes})
}

func buildModelRouteViews(cfg *config.Config, auths []*coreauth.Auth, usage map[string]routeUsageStats, now time.Time) []modelRouteView {
	if cfg == nil {
		return nil
	}
	routes := make(map[string]*modelRouteView)
	add := func(provider, model, alias, display string, priority int) {
		alias = strings.TrimSpace(alias)
		if !isManagedModelRouteAlias(alias) {
			return
		}
		view := routes[alias]
		if view == nil {
			view = &modelRouteView{Alias: alias, Kind: managedModelRouteKind(alias), DisplayName: strings.TrimSpace(display), ObservedAt: now.Format(time.RFC3339)}
			routes[alias] = view
		}
		if view.DisplayName == "" {
			view.DisplayName = alias
		}
		view.Members = append(view.Members, buildModelRouteMember(provider, model, alias, priority, auths, usage, now))
	}

	for provider, aliases := range cfg.OAuthModelAlias {
		for _, entry := range aliases {
			priority := 0
			if entry.Priority != nil {
				priority = *entry.Priority
			}
			add(provider, entry.Name, entry.Alias, entry.DisplayName, priority)
		}
	}
	for _, compat := range cfg.OpenAICompatibility {
		if compat.Disabled {
			continue
		}
		provider := "openai-compatible-" + strings.ToLower(strings.TrimSpace(compat.Name))
		for _, entry := range compat.Models {
			priority := 0
			if entry.Priority != nil {
				priority = *entry.Priority
			}
			add(provider, entry.Name, entry.Alias, entry.DisplayName, priority)
		}
	}

	out := make([]modelRouteView, 0, len(routes))
	for _, view := range routes {
		sort.SliceStable(view.Members, func(i, j int) bool {
			if view.Members[i].Priority != view.Members[j].Priority {
				return view.Members[i].Priority > view.Members[j].Priority
			}
			if view.Members[i].Provider != view.Members[j].Provider {
				return view.Members[i].Provider < view.Members[j].Provider
			}
			return view.Members[i].Model < view.Members[j].Model
		})
		known := 0
		total := 0.0
		for i := range view.Members {
			member := view.Members[i]
			if member.QuotaKnown {
				known++
				total += member.QuotaRemaining
			}
			if view.Active == nil && member.Available {
				copyMember := member
				view.Active = &copyMember
				continue
			}
			if view.Active != nil && view.Next == nil && member.Available {
				copyMember := member
				view.Next = &copyMember
			}
		}
		if known > 0 {
			view.AggregateRemaining = total / float64(known)
			view.AggregateRemainingKnown = true
		}
		out = append(out, *view)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Kind != out[j].Kind {
			return out[i].Kind < out[j].Kind
		}
		return out[i].Alias < out[j].Alias
	})
	return out
}

func buildModelRouteMember(provider, model, alias string, priority int, auths []*coreauth.Auth, usage map[string]routeUsageStats, now time.Time) modelRouteMember {
	member := modelRouteMember{Provider: strings.ToLower(strings.TrimSpace(provider)), Model: strings.TrimSpace(model), Priority: priority}
	matching := matchingRouteAuths(member.Provider, auths)
	member.Available = false
	var remainingTotal float64
	var remainingCount int
	var resetAt time.Time
	for _, auth := range matching {
		if auth == nil || auth.Disabled || auth.Status == coreauth.StatusDisabled {
			continue
		}
		blocked := auth.Unavailable && auth.NextRetryAfter.After(now)
		if state := auth.ModelStates[member.Model]; state != nil {
			blocked = blocked || state.Unavailable && (state.NextRetryAfter.IsZero() || state.NextRetryAfter.After(now))
			if remaining, reset, ok := quotaRemainingForModel(member.Provider, member.Model, state.Quota.Signals); ok {
				remainingTotal += remaining
				remainingCount++
				if !reset.IsZero() && (resetAt.IsZero() || reset.Before(resetAt)) {
					resetAt = reset
				}
			}
		}
		if remaining, reset, ok := quotaRemainingForModel(member.Provider, member.Model, auth.Quota.Signals); ok {
			remainingTotal += remaining
			remainingCount++
			if !reset.IsZero() && (resetAt.IsZero() || reset.Before(resetAt)) {
				resetAt = reset
			}
		}
		if !blocked {
			member.Available = true
		}
	}
	member.QuotaKnown = remainingCount > 0
	if remainingCount > 0 {
		member.QuotaRemaining = remainingTotal / float64(remainingCount)
	}
	if !resetAt.IsZero() {
		member.ResetAt = resetAt.UTC().Format(time.RFC3339)
	}
	stats := usage[routeUsageKey(alias, member.Provider, member.Model)]
	member.Requests = stats.Requests
	member.Failures = stats.Failures
	member.InputTokens = stats.InputTokens
	member.OutputTokens = stats.OutputTokens
	if !stats.LastUsedAt.IsZero() {
		member.LastUsedAt = stats.LastUsedAt.UTC().Format(time.RFC3339)
	}
	return member
}

func matchingRouteAuths(provider string, auths []*coreauth.Auth) []*coreauth.Auth {
	provider = strings.ToLower(strings.TrimSpace(provider))
	out := make([]*coreauth.Auth, 0)
	for _, auth := range auths {
		if auth == nil {
			continue
		}
		authProvider := strings.ToLower(strings.TrimSpace(auth.Provider))
		if strings.HasPrefix(provider, "openai-compatible-") {
			name := strings.TrimPrefix(provider, "openai-compatible-")
			compatName := ""
			providerKey := ""
			if auth.Attributes != nil {
				compatName = strings.ToLower(strings.TrimSpace(auth.Attributes["compat_name"]))
				providerKey = strings.ToLower(strings.TrimSpace(auth.Attributes["provider_key"]))
			}
			if authProvider == provider || compatName == name || providerKey == provider || providerKey == name {
				out = append(out, auth)
			}
			continue
		}
		if authProvider == provider {
			out = append(out, auth)
		}
	}
	return out
}

func quotaRemainingForModel(provider, model string, signals map[string]string) (float64, time.Time, bool) {
	provider = strings.ToLower(strings.TrimSpace(provider))
	if provider == "claude" {
		remaining := []float64{}
		resets := []time.Time{}
		for _, window := range []string{"5h", "7d"} {
			if value, ok := signalFloat(signals, "Anthropic-Ratelimit-Unified-"+window+"-Utilization"); ok {
				remaining = append(remaining, clampRemaining(1-value))
			}
			if reset := signalTime(signals, "Anthropic-Ratelimit-Unified-"+window+"-Reset"); !reset.IsZero() {
				resets = append(resets, reset)
			}
		}
		if strings.Contains(strings.ToLower(model), "fable") {
			if value, ok := signalFloat(signals, "Anthropic-Ratelimit-Unified-7d_oi-Utilization"); ok {
				remaining = append(remaining, clampRemaining(1-value))
			}
			if reset := signalTime(signals, "Anthropic-Ratelimit-Unified-7d_oi-Reset"); !reset.IsZero() {
				resets = append(resets, reset)
			}
		}
		if len(remaining) > 0 {
			return minimum(remaining), earliestTime(resets), true
		}
	}
	if provider == "codex" {
		remaining := []float64{}
		resets := []time.Time{}
		for key, value := range signals {
			lower := strings.ToLower(key)
			if strings.HasSuffix(lower, "-used-percent") {
				if used, err := strconv.ParseFloat(strings.TrimSpace(value), 64); err == nil {
					remaining = append(remaining, clampRemaining(1-used/100))
				}
			}
			if strings.HasSuffix(lower, "-reset-at") {
				if reset := parseQuotaTime(value); !reset.IsZero() {
					resets = append(resets, reset)
				}
			}
		}
		if len(remaining) > 0 {
			return minimum(remaining), earliestTime(resets), true
		}
	}
	return 0, time.Time{}, false
}

func signalFloat(signals map[string]string, name string) (float64, bool) {
	for key, value := range signals {
		if strings.EqualFold(strings.TrimSpace(key), name) {
			parsed, err := strconv.ParseFloat(strings.TrimSpace(value), 64)
			return parsed, err == nil
		}
	}
	return 0, false
}

func signalTime(signals map[string]string, name string) time.Time {
	for key, value := range signals {
		if strings.EqualFold(strings.TrimSpace(key), name) {
			return parseQuotaTime(value)
		}
	}
	return time.Time{}
}

func parseQuotaTime(value string) time.Time {
	value = strings.TrimSpace(value)
	if seconds, err := strconv.ParseInt(value, 10, 64); err == nil && seconds > 0 {
		return time.Unix(seconds, 0)
	}
	if parsed, err := time.Parse(time.RFC3339, value); err == nil {
		return parsed
	}
	return time.Time{}
}

func collectModelRouteUsage(records [][]byte) map[string]routeUsageStats {
	out := make(map[string]routeUsageStats)
	for _, raw := range records {
		payload := raw
		var envelope persistedUsageEnvelope
		if json.Unmarshal(raw, &envelope) == nil && strings.TrimSpace(envelope.Payload) != "" {
			decoded, errDecode := base64.StdEncoding.DecodeString(envelope.Payload)
			if errDecode != nil {
				continue
			}
			payload = decoded
		}
		var record routeUsageRecord
		if json.Unmarshal(payload, &record) != nil || !isManagedModelRouteAlias(record.Alias) {
			continue
		}
		key := routeUsageKey(record.Alias, record.Provider, record.Model)
		stats := out[key]
		stats.Requests++
		if record.Failed {
			stats.Failures++
		}
		stats.InputTokens += record.Tokens.InputTokens
		stats.OutputTokens += record.Tokens.OutputTokens
		if observed, errTime := time.Parse(time.RFC3339Nano, record.Timestamp); errTime == nil && observed.After(stats.LastUsedAt) {
			stats.LastUsedAt = observed
		}
		out[key] = stats
	}
	return out
}

func routeUsageKey(alias, provider, model string) string {
	return strings.ToLower(strings.TrimSpace(alias)) + "|" + strings.ToLower(strings.TrimSpace(provider)) + "|" + strings.ToLower(strings.TrimSpace(model))
}

func isManagedModelRouteAlias(alias string) bool {
	alias = strings.ToLower(strings.TrimSpace(alias))
	return strings.HasPrefix(alias, "tier-") || strings.HasPrefix(alias, "role-")
}

func managedModelRouteKind(alias string) string {
	if strings.HasPrefix(strings.ToLower(strings.TrimSpace(alias)), "tier-") {
		return "tier"
	}
	return "role"
}

func clampRemaining(value float64) float64 {
	return math.Max(0, math.Min(1, value))
}

func minimum(values []float64) float64 {
	if len(values) == 0 {
		return 0
	}
	result := values[0]
	for _, value := range values[1:] {
		if value < result {
			result = value
		}
	}
	return result
}

func earliestTime(values []time.Time) time.Time {
	var result time.Time
	for _, value := range values {
		if value.IsZero() {
			continue
		}
		if result.IsZero() || value.Before(result) {
			result = value
		}
	}
	return result
}
