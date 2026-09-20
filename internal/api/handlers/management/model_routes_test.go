package management

import (
	"encoding/base64"
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/redisqueue"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

func modelRoutePriority(value int) *int { return &value }

func TestGetModelRoutesOrdersMembersAndAggregatesUsage(t *testing.T) {
	withManagementUsageQueue(t, func() {
		usagePayload, _ := json.Marshal(map[string]any{
			"timestamp": "2026-09-20T10:00:00Z",
			"provider":  "codex",
			"model":     "gpt-5.6-sol",
			"alias":     "role-implementer",
			"tokens": map[string]any{
				"input_tokens":  100,
				"output_tokens": 20,
			},
		})
		envelope, _ := json.Marshal(map[string]any{"payload": base64.StdEncoding.EncodeToString(usagePayload)})
		redisqueue.Enqueue(envelope)

		manager := coreauth.NewManager(nil, nil, nil)
		auth := &coreauth.Auth{
			ID:       "codex-auth",
			Provider: "codex",
			Status:   coreauth.StatusActive,
			Quota: coreauth.QuotaState{Signals: map[string]string{
				"X-Codex-Primary-Used-Percent": "25",
			}},
		}
		if _, errRegister := manager.Register(t.Context(), auth); errRegister != nil {
			t.Fatal(errRegister)
		}

		cfg := &config.Config{OAuthModelAlias: map[string][]config.OAuthModelAlias{
			"codex": {
				{Name: "gpt-5.6-sol", Alias: "role-implementer", DisplayName: "Implementer Role", Priority: modelRoutePriority(100)},
				{Name: "gpt-5.6-luna", Alias: "role-implementer", DisplayName: "Implementer Role", Priority: modelRoutePriority(50)},
			},
		}}
		handler := NewHandlerWithoutConfigFilePath(cfg, manager)
		recorder := httptest.NewRecorder()
		ctx, _ := gin.CreateTestContext(recorder)
		ctx.Request = httptest.NewRequest(http.MethodGet, "/v0/management/model-routes", nil)
		handler.GetModelRoutes(ctx)
		if recorder.Code != http.StatusOK {
			t.Fatalf("status = %d body=%s", recorder.Code, recorder.Body.String())
		}
		var payload struct {
			Routes []modelRouteView `json:"routes"`
		}
		if errDecode := json.Unmarshal(recorder.Body.Bytes(), &payload); errDecode != nil {
			t.Fatal(errDecode)
		}
		if len(payload.Routes) != 1 || len(payload.Routes[0].Members) != 2 {
			t.Fatalf("routes = %#v", payload.Routes)
		}
		route := payload.Routes[0]
		if route.Active == nil || route.Active.Model != "gpt-5.6-sol" || route.Next == nil || route.Next.Model != "gpt-5.6-luna" {
			t.Fatalf("active/next = %#v / %#v", route.Active, route.Next)
		}
		if !route.Active.QuotaKnown || route.Active.QuotaRemaining != 0.75 {
			t.Fatalf("active quota = %#v", route.Active)
		}
		if route.Active.Requests != 1 || route.Active.InputTokens != 100 || route.Active.OutputTokens != 20 {
			t.Fatalf("active usage = %#v", route.Active)
		}
	})
}

func TestModelRouteQuotaWeightsClaudeMaxPlansByCapacity(t *testing.T) {
	manager := coreauth.NewManager(nil, nil, nil)
	for _, auth := range []*coreauth.Auth{
		{
			ID:         "claude-max-5x",
			Provider:   "claude",
			Status:     coreauth.StatusActive,
			Attributes: map[string]string{"rate_limit_tier": "default_claude_max_5x"},
			Quota: coreauth.QuotaState{Signals: map[string]string{
				"Anthropic-Ratelimit-Unified-5h-Utilization": "0.20",
				"Anthropic-Ratelimit-Unified-7d-Utilization": "0.27",
			}},
		},
		{
			ID:         "claude-max-20x",
			Provider:   "claude",
			Status:     coreauth.StatusActive,
			Attributes: map[string]string{"rate_limit_tier": "default_claude_max_20x"},
			Quota: coreauth.QuotaState{Signals: map[string]string{
				"Anthropic-Ratelimit-Unified-5h-Utilization": "0.05",
				"Anthropic-Ratelimit-Unified-7d-Utilization": "0.71",
			}},
		},
	} {
		if _, errRegister := manager.Register(t.Context(), auth); errRegister != nil {
			t.Fatal(errRegister)
		}
	}
	cfg := &config.Config{OAuthModelAlias: map[string][]config.OAuthModelAlias{
		"claude": {{Name: "claude-opus-5", Alias: "tier-deep", Priority: modelRoutePriority(100)}},
	}}
	routes := buildModelRouteViews(cfg, manager.List(), nil, time.Now())
	if len(routes) != 1 || len(routes[0].Members) != 1 {
		t.Fatalf("routes = %#v", routes)
	}
	member := routes[0].Members[0]
	// The 5x account has 73%% remaining and the 20x account has 29%% remaining.
	// Capacity weighting is (5*0.73 + 20*0.29) / 25 = 0.378, not 0.51.
	if math.Abs(member.QuotaRemaining-0.378) > 0.000001 {
		t.Fatalf("member quota = %f, want 0.378", member.QuotaRemaining)
	}
	if member.QuotaCapacity != 25 {
		t.Fatalf("member capacity = %f, want 25", member.QuotaCapacity)
	}
	if math.Abs(routes[0].AggregateRemaining-0.378) > 0.000001 {
		t.Fatalf("aggregate quota = %f, want 0.378", routes[0].AggregateRemaining)
	}
}

func TestGetModelRoutesSkipsUnavailablePreferredMember(t *testing.T) {
	manager := coreauth.NewManager(nil, nil, nil)
	next := time.Now().Add(time.Hour)
	for _, auth := range []*coreauth.Auth{
		{ID: "claude-auth", Provider: "claude", Status: coreauth.StatusActive, ModelStates: map[string]*coreauth.ModelState{
			"claude-fable-5-1": {Unavailable: true, NextRetryAfter: next, Quota: coreauth.QuotaState{Exceeded: true, NextRecoverAt: next}},
		}},
		{ID: "codex-auth", Provider: "codex", Status: coreauth.StatusActive},
	} {
		if _, errRegister := manager.Register(t.Context(), auth); errRegister != nil {
			t.Fatal(errRegister)
		}
	}
	cfg := &config.Config{OAuthModelAlias: map[string][]config.OAuthModelAlias{
		"claude": {{Name: "claude-fable-5-1", Alias: "tier-frontier", Priority: modelRoutePriority(100)}},
		"codex":  {{Name: "gpt-6-astra", Alias: "tier-frontier", Priority: modelRoutePriority(50)}},
	}}
	routes := buildModelRouteViews(cfg, manager.List(), nil, time.Now())
	if len(routes) != 1 || routes[0].Active == nil || routes[0].Active.Model != "gpt-6-astra" {
		t.Fatalf("routes = %#v", routes)
	}
}
