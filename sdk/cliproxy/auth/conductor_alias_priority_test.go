package auth

import (
	"context"
	"testing"
	"time"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

func intPointer(value int) *int { return &value }

func TestManagerAliasPriorityPrefersRouteWithoutChangingExplicitModels(t *testing.T) {
	const (
		alias         = "tier-frontier"
		preferredID   = "tier-priority-claude"
		fallbackID    = "tier-priority-codex"
		preferredName = "claude-fable-5-1"
		fallbackName  = "gpt-6-astra"
	)

	manager := NewManager(nil, &RoundRobinSelector{}, nil)
	manager.RegisterExecutor(&aliasRoutingExecutor{id: "claude"})
	manager.RegisterExecutor(&aliasRoutingExecutor{id: "codex"})
	manager.SetOAuthModelAlias(map[string][]internalconfig.OAuthModelAlias{
		"claude": {{Name: preferredName, Alias: alias, Fork: true, Priority: intPointer(100)}},
		"codex":  {{Name: fallbackName, Alias: alias, Fork: true, Priority: intPointer(50)}},
	})

	preferredAuth := &Auth{ID: preferredID, Provider: "claude", Status: StatusActive}
	fallbackAuth := &Auth{ID: fallbackID, Provider: "codex", Status: StatusActive}
	for _, auth := range []*Auth{preferredAuth, fallbackAuth} {
		if _, errRegister := manager.Register(context.Background(), auth); errRegister != nil {
			t.Fatalf("register %s: %v", auth.ID, errRegister)
		}
	}

	reg := registry.GetGlobalRegistry()
	reg.RegisterClient(preferredID, "claude", []*registry.ModelInfo{{ID: preferredName}, {ID: alias}})
	reg.RegisterClient(fallbackID, "codex", []*registry.ModelInfo{{ID: fallbackName}, {ID: alias}})
	t.Cleanup(func() {
		reg.UnregisterClient(preferredID)
		reg.UnregisterClient(fallbackID)
	})
	manager.RefreshSchedulerEntry(preferredID)
	manager.RefreshSchedulerEntry(fallbackID)

	response, errExecute := manager.Execute(context.Background(), []string{"claude", "codex"}, cliproxyexecutor.Request{Model: alias}, cliproxyexecutor.Options{})
	if errExecute != nil {
		t.Fatalf("execute tier alias: %v", errExecute)
	}
	if string(response.Payload) != preferredName {
		t.Fatalf("tier response = %q, want preferred model %q", response.Payload, preferredName)
	}

	explicitResponse, errExplicit := manager.Execute(context.Background(), []string{"codex"}, cliproxyexecutor.Request{Model: fallbackName}, cliproxyexecutor.Options{})
	if errExplicit != nil {
		t.Fatalf("execute explicit model: %v", errExplicit)
	}
	if string(explicitResponse.Payload) != fallbackName {
		t.Fatalf("explicit response = %q, want %q", explicitResponse.Payload, fallbackName)
	}
}

func TestManagerAliasPriorityFallsBackDuringCooldownAndReturnsAfterReset(t *testing.T) {
	const (
		alias         = "tier-frontier-reset"
		preferredID   = "tier-reset-claude"
		fallbackID    = "tier-reset-codex"
		preferredName = "claude-fable-5-1"
		fallbackName  = "gpt-6-astra"
	)

	manager := NewManager(nil, &RoundRobinSelector{}, nil)
	manager.RegisterExecutor(&aliasRoutingExecutor{id: "claude"})
	manager.RegisterExecutor(&aliasRoutingExecutor{id: "codex"})
	manager.SetOAuthModelAlias(map[string][]internalconfig.OAuthModelAlias{
		"claude": {{Name: preferredName, Alias: alias, Fork: true, Priority: intPointer(100)}},
		"codex":  {{Name: fallbackName, Alias: alias, Fork: true, Priority: intPointer(50)}},
	})

	resetAt := time.Now().Add(time.Hour)
	preferredAuth := &Auth{
		ID:       preferredID,
		Provider: "claude",
		Status:   StatusActive,
		ModelStates: map[string]*ModelState{
			preferredName: {
				Status:         StatusError,
				Unavailable:    true,
				NextRetryAfter: resetAt,
				Quota: QuotaState{
					Exceeded:      true,
					Reason:        "quota",
					NextRecoverAt: resetAt,
				},
			},
		},
	}
	fallbackAuth := &Auth{ID: fallbackID, Provider: "codex", Status: StatusActive}
	for _, auth := range []*Auth{preferredAuth, fallbackAuth} {
		if _, errRegister := manager.Register(context.Background(), auth); errRegister != nil {
			t.Fatalf("register %s: %v", auth.ID, errRegister)
		}
	}

	reg := registry.GetGlobalRegistry()
	reg.RegisterClient(preferredID, "claude", []*registry.ModelInfo{{ID: preferredName}, {ID: alias}})
	reg.RegisterClient(fallbackID, "codex", []*registry.ModelInfo{{ID: fallbackName}, {ID: alias}})
	t.Cleanup(func() {
		reg.UnregisterClient(preferredID)
		reg.UnregisterClient(fallbackID)
	})
	manager.RefreshSchedulerEntry(preferredID)
	manager.RefreshSchedulerEntry(fallbackID)

	response, errExecute := manager.Execute(context.Background(), []string{"claude", "codex"}, cliproxyexecutor.Request{Model: alias}, cliproxyexecutor.Options{})
	if errExecute != nil {
		t.Fatalf("execute fallback: %v", errExecute)
	}
	if string(response.Payload) != fallbackName {
		t.Fatalf("fallback response = %q, want %q", response.Payload, fallbackName)
	}

	updated, ok := manager.GetByID(preferredID)
	if !ok || updated == nil {
		t.Fatalf("preferred auth missing")
	}
	state := updated.ModelStates[preferredName]
	state.Status = StatusActive
	state.Unavailable = false
	state.NextRetryAfter = time.Time{}
	state.Quota = QuotaState{}
	updated.UpdatedAt = time.Now().Add(time.Second)
	if _, errUpdate := manager.Update(context.Background(), updated); errUpdate != nil {
		t.Fatalf("update preferred auth: %v", errUpdate)
	}
	manager.RefreshSchedulerEntry(preferredID)

	response, errExecute = manager.Execute(context.Background(), []string{"claude", "codex"}, cliproxyexecutor.Request{Model: alias}, cliproxyexecutor.Options{})
	if errExecute != nil {
		t.Fatalf("execute after reset: %v", errExecute)
	}
	if string(response.Payload) != preferredName {
		t.Fatalf("post-reset response = %q, want %q", response.Payload, preferredName)
	}
}

func TestPerAuthAliasPriorityOverridesGlobalPriority(t *testing.T) {
	globalPriority := 100
	perAuthPriority := 25
	manager := NewManager(nil, &RoundRobinSelector{}, nil)
	manager.SetOAuthModelAlias(map[string][]internalconfig.OAuthModelAlias{
		"codex": {{Name: "gpt-6-astra", Alias: "tier-frontier", Priority: &globalPriority}},
	})
	auth := &Auth{ID: "priority-override", Provider: "codex"}
	SetOAuthModelAliasesAttribute(auth, []internalconfig.OAuthModelAlias{
		{Name: "gpt-6-astra", Alias: "tier-frontier", Priority: &perAuthPriority},
	})
	if got := manager.oauthModelAliasPriority(auth, "tier-frontier(high)"); got != perAuthPriority {
		t.Fatalf("alias priority = %d, want per-auth priority %d", got, perAuthPriority)
	}
}

func TestOAuthAliasPrioritySurvivesSanitization(t *testing.T) {
	priority := 100
	cfg := &internalconfig.Config{
		OAuthModelAlias: map[string][]internalconfig.OAuthModelAlias{
			" Claude ": {{Name: " claude-fable-5-1 ", Alias: " tier-frontier ", Priority: &priority}},
		},
	}
	cfg.SanitizeOAuthModelAlias()
	aliases := cfg.OAuthModelAlias["claude"]
	if len(aliases) != 1 || aliases[0].Priority == nil || *aliases[0].Priority != priority {
		t.Fatalf("sanitized aliases = %#v, want priority %d", aliases, priority)
	}
}
