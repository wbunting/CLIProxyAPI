package registry

import "testing"

func TestGetAvailableModelsAdvertisesSmallestSharedContextWindow(t *testing.T) {
	r := newTestModelRegistry()
	r.RegisterClient("codex-1", "codex", []*ModelInfo{{ID: "tier-deep", ContextLength: 272000, MaxContextLength: 872000}})
	r.RegisterClient("xai-1", "xai", []*ModelInfo{{ID: "tier-deep", ContextLength: 500000}})
	// Registered last, so Registration.Info alone would advertise 1M.
	r.RegisterClient("claude-1", "claude", []*ModelInfo{{ID: "tier-deep", ContextLength: 1000000}})

	models := r.GetAvailableModels("openai")
	if len(models) != 1 {
		t.Fatalf("expected 1 model, got %d", len(models))
	}
	if got := models[0]["context_length"]; got != 500000 {
		t.Errorf("context_length = %v, want 500000", got)
	}
	if got := models[0]["max_context_length"]; got != 500000 {
		t.Errorf("max_context_length = %v, want 500000", got)
	}

	infos := r.GetAvailableModelInfos()
	if len(infos) != 1 || EffectiveContextLength(infos[0]) != 500000 {
		t.Fatalf("GetAvailableModelInfos context = %+v, want 500000", infos)
	}

	r.UnregisterClient("xai-1")
	models = r.GetAvailableModels("openai")
	if got := models[0]["context_length"]; got != 872000 {
		t.Errorf("after unregistering the smallest member, context_length = %v, want 872000", got)
	}
}

func TestGetAvailableModelsKeepsSingleClientContextWindow(t *testing.T) {
	r := newTestModelRegistry()
	r.RegisterClient("codex-1", "codex", []*ModelInfo{{ID: "gpt-5.5", ContextLength: 272000}})

	models := r.GetAvailableModels("openai")
	if got := models[0]["context_length"]; got != 272000 {
		t.Errorf("context_length = %v, want 272000", got)
	}
	if _, ok := models[0]["max_context_length"]; ok {
		t.Errorf("max_context_length should stay unset for a single client: %v", models[0])
	}
}
