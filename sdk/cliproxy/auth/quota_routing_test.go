package auth

import (
	"context"
	"strconv"
	"testing"
	"time"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

// quotaRouteFixture wires a two-provider alias pool where the configured model
// preference favors Claude, so any Codex win must come from the quota signal.
type quotaRouteFixture struct {
	manager  *Manager
	alias    string
	claudeID string
	codexID  string
	claude   string
	codex    string
}

func newQuotaRouteFixture(t *testing.T, alias string, claudeAuth, codexAuth *Auth) *quotaRouteFixture {
	t.Helper()
	const (
		claudeModel = "claude-fable-5-1"
		codexModel  = "gpt-6-astra"
	)

	manager := NewManager(nil, &RoundRobinSelector{}, nil)
	manager.RegisterExecutor(&aliasRoutingExecutor{id: "claude"})
	manager.RegisterExecutor(&aliasRoutingExecutor{id: "codex"})
	manager.SetOAuthModelAlias(map[string][]internalconfig.OAuthModelAlias{
		"claude": {{Name: claudeModel, Alias: alias, Fork: true, Priority: intPointer(100)}},
		"codex":  {{Name: codexModel, Alias: alias, Fork: true, Priority: intPointer(50)}},
	})

	claudeAuth.Provider = "claude"
	claudeAuth.Status = StatusActive
	codexAuth.Provider = "codex"
	codexAuth.Status = StatusActive
	for _, auth := range []*Auth{claudeAuth, codexAuth} {
		if _, errRegister := manager.Register(context.Background(), auth); errRegister != nil {
			t.Fatalf("register %s: %v", auth.ID, errRegister)
		}
	}

	reg := registry.GetGlobalRegistry()
	reg.RegisterClient(claudeAuth.ID, "claude", []*registry.ModelInfo{{ID: claudeModel}, {ID: alias}})
	reg.RegisterClient(codexAuth.ID, "codex", []*registry.ModelInfo{{ID: codexModel}, {ID: alias}})
	t.Cleanup(func() {
		reg.UnregisterClient(claudeAuth.ID)
		reg.UnregisterClient(codexAuth.ID)
	})
	manager.RefreshSchedulerEntry(claudeAuth.ID)
	manager.RefreshSchedulerEntry(codexAuth.ID)

	return &quotaRouteFixture{
		manager:  manager,
		alias:    alias,
		claudeID: claudeAuth.ID,
		codexID:  codexAuth.ID,
		claude:   claudeModel,
		codex:    codexModel,
	}
}

func (f *quotaRouteFixture) executeAlias(t *testing.T) string {
	t.Helper()
	response, errExecute := f.manager.Execute(context.Background(), []string{"claude", "codex"}, cliproxyexecutor.Request{Model: f.alias}, cliproxyexecutor.Options{})
	if errExecute != nil {
		t.Fatalf("execute alias %q: %v", f.alias, errExecute)
	}
	return string(response.Payload)
}

func TestRemainingQuotaFractionUsesMostConstrainedWindow(t *testing.T) {
	cases := []struct {
		name      string
		provider  string
		model     string
		signals   map[string]string
		want      float64
		wantKnown bool
	}{
		{
			name:     "claude takes the worst unified window",
			provider: "claude",
			model:    "claude-opus-5",
			signals: map[string]string{
				"Anthropic-Ratelimit-Unified-5h-Utilization": "0.10",
				"Anthropic-Ratelimit-Unified-7d-Utilization": "0.80",
			},
			want:      0.20,
			wantKnown: true,
		},
		{
			name:     "claude fable also reads the opus-input window",
			provider: "claude",
			model:    "claude-fable-5-1",
			signals: map[string]string{
				"Anthropic-Ratelimit-Unified-5h-Utilization":    "0.10",
				"Anthropic-Ratelimit-Unified-7d_oi-Utilization": "0.95",
			},
			want:      0.05,
			wantKnown: true,
		},
		{
			name:     "codex takes the worst used-percent window",
			provider: "codex",
			model:    "gpt-6-astra",
			signals: map[string]string{
				"X-Codex-Primary-Used-Percent":   "25",
				"X-Codex-Secondary-Used-Percent": "60",
			},
			want:      0.40,
			wantKnown: true,
		},
		{
			name:      "unsupported provider reports no signal",
			provider:  "gemini",
			model:     "gemini-3-pro",
			signals:   map[string]string{"X-Ratelimit-Remaining": "5"},
			wantKnown: false,
		},
		{
			name:      "unparsable values report no signal",
			provider:  "codex",
			model:     "gpt-6-astra",
			signals:   map[string]string{"X-Codex-Primary-Used-Percent": "n/a"},
			wantKnown: false,
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			got, known := RemainingQuotaFraction(testCase.provider, testCase.model, testCase.signals)
			if known != testCase.wantKnown {
				t.Fatalf("known = %v, want %v", known, testCase.wantKnown)
			}
			if known && (got-testCase.want > 1e-9 || testCase.want-got > 1e-9) {
				t.Fatalf("remaining = %f, want %f", got, testCase.want)
			}
		})
	}
}

// TestQuotaBandBeatsModelPreferenceWithinTier asserts the primary ordering rule:
// inside one alias, a materially higher remaining quota outranks the configured
// model preference.
func TestQuotaBandBeatsModelPreferenceWithinTier(t *testing.T) {
	fixture := newQuotaRouteFixture(t, "tier-quota-band",
		&Auth{
			ID: "quota-band-claude",
			Quota: QuotaState{Signals: map[string]string{
				"Anthropic-Ratelimit-Unified-5h-Utilization": "0.95",
			}},
		},
		&Auth{
			ID: "quota-band-codex",
			Quota: QuotaState{Signals: map[string]string{
				"X-Codex-Primary-Used-Percent": "5",
			}},
		},
	)
	if got := fixture.executeAlias(t); got != fixture.codex {
		t.Fatalf("selected %q, want drained-quota fallback %q", got, fixture.codex)
	}
}

// TestModelPreferenceBreaksTiesWithinSameQuotaBand asserts the secondary ordering
// rule: comparable remaining quota leaves the configured priority in charge.
func TestModelPreferenceBreaksTiesWithinSameQuotaBand(t *testing.T) {
	fixture := newQuotaRouteFixture(t, "tier-quota-tie",
		&Auth{
			ID: "quota-tie-claude",
			Quota: QuotaState{Signals: map[string]string{
				"Anthropic-Ratelimit-Unified-5h-Utilization": "0.10",
			}},
		},
		&Auth{
			ID: "quota-tie-codex",
			Quota: QuotaState{Signals: map[string]string{
				"X-Codex-Primary-Used-Percent": "2",
			}},
		},
	)
	if got := fixture.executeAlias(t); got != fixture.claude {
		t.Fatalf("selected %q, want preferred model %q", got, fixture.claude)
	}
}

// TestHealthyPreferredModelBeatsHealthyAlternative asserts that quota is not
// needlessly used to override the configured preference while both routes are healthy.
func TestHealthyPreferredModelBeatsHealthyAlternative(t *testing.T) {
	fixture := newQuotaRouteFixture(t, "tier-quota-healthy-preference",
		&Auth{
			ID: "quota-healthy-claude",
			Quota: QuotaState{Signals: map[string]string{
				"Anthropic-Ratelimit-Unified-5h-Utilization": "0.10",
			}},
		},
		&Auth{
			ID: "quota-healthy-codex",
			Quota: QuotaState{Signals: map[string]string{
				"X-Codex-Primary-Used-Percent": "2",
			}},
		},
	)
	if got := fixture.executeAlias(t); got != fixture.claude {
		t.Fatalf("selected %q, want preferred healthy model %q", got, fixture.claude)
	}
}

// TestLowPreferredModelUsesAlternativeWithEarlierReset asserts that a low-quota
// preferred route yields to an alternative whose low quota recovers sooner.
func TestLowPreferredModelUsesAlternativeWithEarlierReset(t *testing.T) {
	now := time.Now()
	fixture := newQuotaRouteFixture(t, "tier-quota-reset-preference",
		&Auth{
			ID: "quota-reset-claude",
			Quota: QuotaState{ObservedAt: now, Signals: map[string]string{
				"Anthropic-Ratelimit-Unified-5h-Utilization": "0.95",
				"Anthropic-Ratelimit-Unified-5h-Reset":       strconv.FormatInt(now.Add(48*time.Hour).Unix(), 10),
			}},
		},
		&Auth{
			ID: "quota-reset-codex",
			Quota: QuotaState{ObservedAt: now, Signals: map[string]string{
				"X-Codex-Primary-Used-Percent": "80",
				"X-Codex-Primary-Reset-At":     strconv.FormatInt(now.Add(2*time.Hour).Unix(), 10),
			}},
		},
	)
	if got := fixture.executeAlias(t); got != fixture.codex {
		t.Fatalf("selected %q, want earlier-reset alternative %q", got, fixture.codex)
	}
}

// TestUnknownQuotaKeepsModelPreference guards the "do not claim telemetry we do not
// have" rule: a credential with no signal must not be demoted below one with a signal.
func TestUnknownQuotaKeepsModelPreference(t *testing.T) {
	fixture := newQuotaRouteFixture(t, "tier-quota-unknown",
		&Auth{ID: "quota-unknown-claude"},
		&Auth{
			ID: "quota-unknown-codex",
			Quota: QuotaState{Signals: map[string]string{
				"X-Codex-Primary-Used-Percent": "1",
			}},
		},
	)
	if got := fixture.executeAlias(t); got != fixture.claude {
		t.Fatalf("selected %q, want preferred model %q", got, fixture.claude)
	}
}

// TestModelLevelQuotaSignalOverridesCredentialSignal asserts that a per-model
// observation describes the scheduled route more precisely than the credential-wide one.
func TestModelLevelQuotaSignalOverridesCredentialSignal(t *testing.T) {
	fixture := newQuotaRouteFixture(t, "tier-quota-model-level",
		&Auth{
			ID: "quota-model-claude",
			// Credential-wide telemetry looks healthy.
			Quota: QuotaState{Signals: map[string]string{
				"Anthropic-Ratelimit-Unified-5h-Utilization": "0.05",
			}},
			// The specific route is nearly drained.
			ModelStates: map[string]*ModelState{
				"claude-fable-5-1": {
					Status: StatusActive,
					Quota: QuotaState{Signals: map[string]string{
						"Anthropic-Ratelimit-Unified-5h-Utilization": "0.98",
					}},
				},
			},
		},
		&Auth{
			ID: "quota-model-codex",
			Quota: QuotaState{Signals: map[string]string{
				"X-Codex-Primary-Used-Percent": "5",
			}},
		},
	)
	if got := fixture.executeAlias(t); got != fixture.codex {
		t.Fatalf("selected %q, want %q on model-level quota", got, fixture.codex)
	}
}

// TestCredentialWideCooldownRemovesRouteBeforeQuotaBanding asserts that a
// credential-wide quota cooldown drops the candidate outright, regardless of any
// remaining-quota watermark it still carries.
func TestCredentialWideCooldownRemovesRouteBeforeQuotaBanding(t *testing.T) {
	recoverAt := time.Now().Add(time.Hour)
	fixture := newQuotaRouteFixture(t, "tier-quota-credential-cooldown",
		&Auth{
			ID: "quota-credential-claude",
			Quota: QuotaState{
				Exceeded:      true,
				Reason:        "credential_quota",
				NextRecoverAt: recoverAt,
				// A stale healthy watermark must not resurrect a cooling credential.
				Signals: map[string]string{
					"Anthropic-Ratelimit-Unified-5h-Utilization": "0.01",
				},
			},
		},
		&Auth{
			ID: "quota-credential-codex",
			Quota: QuotaState{Signals: map[string]string{
				"X-Codex-Primary-Used-Percent": "90",
			}},
		},
	)
	if got := fixture.executeAlias(t); got != fixture.codex {
		t.Fatalf("selected %q, want cooldown fallback %q", got, fixture.codex)
	}
}

// TestModelLevelCooldownFallsBackWithinTier asserts that a model-level cooldown only
// removes that one route, leaving the credential usable for its other models.
func TestModelLevelCooldownFallsBackWithinTier(t *testing.T) {
	recoverAt := time.Now().Add(time.Hour)
	fixture := newQuotaRouteFixture(t, "tier-quota-model-cooldown",
		&Auth{
			ID: "quota-modelcool-claude",
			ModelStates: map[string]*ModelState{
				"claude-fable-5-1": {
					Status:         StatusError,
					Unavailable:    true,
					NextRetryAfter: recoverAt,
					Quota: QuotaState{
						Exceeded:      true,
						Reason:        "quota",
						NextRecoverAt: recoverAt,
						Signals: map[string]string{
							"Anthropic-Ratelimit-Unified-5h-Utilization": "0.02",
						},
					},
				},
			},
		},
		&Auth{
			ID: "quota-modelcool-codex",
			Quota: QuotaState{Signals: map[string]string{
				"X-Codex-Primary-Used-Percent": "85",
			}},
		},
	)
	if got := fixture.executeAlias(t); got != fixture.codex {
		t.Fatalf("selected %q, want model-cooldown fallback %q", got, fixture.codex)
	}
}

// TestQuotaBandingDoesNotAffectExplicitModelRequests asserts that quota banding never
// reshapes credential round-robin for a concrete model: both credentials serve the same
// upstream model, so neither may be filtered out.
func TestQuotaBandingDoesNotAffectExplicitModelRequests(t *testing.T) {
	manager := NewManager(nil, &RoundRobinSelector{}, nil)
	const model = "gpt-6-astra"
	drained := &Auth{
		ID:       "explicit-drained",
		Provider: "codex",
		Status:   StatusActive,
		Quota: QuotaState{Signals: map[string]string{
			"X-Codex-Primary-Used-Percent": "99",
		}},
	}
	fresh := &Auth{
		ID:       "explicit-fresh",
		Provider: "codex",
		Status:   StatusActive,
		Quota: QuotaState{Signals: map[string]string{
			"X-Codex-Primary-Used-Percent": "1",
		}},
	}
	for _, auth := range []*Auth{drained, fresh} {
		if _, errRegister := manager.Register(context.Background(), auth); errRegister != nil {
			t.Fatalf("register %s: %v", auth.ID, errRegister)
		}
	}

	available, errAvailable := manager.availableAuthsForRouteModel([]*Auth{drained, fresh}, "codex", model, time.Now())
	if errAvailable != nil {
		t.Fatalf("available auths: %v", errAvailable)
	}
	if len(available) != 2 {
		t.Fatalf("available = %d candidates, want both credentials retained for an explicit model", len(available))
	}
}

// TestQuotaBandingKeepsAllCandidatesWhenNoTelemetry asserts the no-telemetry
// deployment behaves exactly as before this change.
func TestQuotaBandingKeepsAllCandidatesWhenNoTelemetry(t *testing.T) {
	manager := NewManager(nil, &RoundRobinSelector{}, nil)
	manager.SetOAuthModelAlias(map[string][]internalconfig.OAuthModelAlias{
		"claude": {{Name: "claude-fable-5-1", Alias: "tier-no-telemetry", Fork: true}},
		"codex":  {{Name: "gpt-6-astra", Alias: "tier-no-telemetry", Fork: true}},
	})
	claudeAuth := &Auth{ID: "no-telemetry-claude", Provider: "claude", Status: StatusActive}
	codexAuth := &Auth{ID: "no-telemetry-codex", Provider: "codex", Status: StatusActive}
	for _, auth := range []*Auth{claudeAuth, codexAuth} {
		if _, errRegister := manager.Register(context.Background(), auth); errRegister != nil {
			t.Fatalf("register %s: %v", auth.ID, errRegister)
		}
	}

	available, errAvailable := manager.availableAuthsForRouteModel([]*Auth{claudeAuth, codexAuth}, "mixed", "tier-no-telemetry", time.Now())
	if errAvailable != nil {
		t.Fatalf("available auths: %v", errAvailable)
	}
	if len(available) != 2 {
		t.Fatalf("available = %d candidates, want both retained without quota telemetry", len(available))
	}
}
