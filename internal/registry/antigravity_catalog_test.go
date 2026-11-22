package registry

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestEmbeddedAntigravityCatalogRefresh(t *testing.T) {
	var catalog staticModelsJSON
	if err := json.Unmarshal(embeddedModelsJSON, &catalog); err != nil {
		t.Fatal(err)
	}
	models := make(map[string]*ModelInfo, len(catalog.Antigravity))
	for _, model := range catalog.Antigravity {
		if model == nil || model.ID == "" {
			t.Fatal("Antigravity catalog contains an invalid model")
		}
		if models[model.ID] != nil {
			t.Fatalf("duplicate model %q", model.ID)
		}
		models[model.ID] = model
	}

	for _, id := range []string{
		"gemini-4-hydrogen-sonar",
		"gemini-4-neon-low", "gemini-4-neon-medium", "gemini-4-neon-high", "gemini-4-neon-xhigh",
		"gemini-4-argon-low", "gemini-4-argon-medium", "gemini-4-argon-high",
		"gemini-4-carbon-low", "gemini-4-carbon-medium", "gemini-4-carbon-high",
		"gemini-4-hydrogen-low", "gemini-4-hydrogen-medium", "gemini-4-hydrogen-high", "gemini-4-hydrogen-xhigh",
		"gemini-4-hydrogen-tiered-high", "gemini-4-hydrogen-tiered-xhigh",
		"gemini-3.8-flash-low", "gemini-3.8-flash-medium", "gemini-3.8-flash-high", "gemini-3.8-flash-tiered",
		"claude-opus-5-5-low", "claude-opus-5-5-medium", "claude-opus-5-5-high",
		"claude-sonnet-5-5-low", "claude-sonnet-5-5-medium", "claude-sonnet-5-5-high",
	} {
		t.Run(id, func(t *testing.T) {
			model := models[id]
			if model == nil {
				t.Fatalf("missing model %q", id)
			}
			wantContext, wantOutput := 1048576, 65536
			if strings.HasPrefix(id, "claude") {
				wantContext, wantOutput = 1000000, 128000
			} else if strings.HasPrefix(id, "gemini-4-") && id != "gemini-4-hydrogen-sonar" && !strings.HasPrefix(id, "gemini-4-argon-") {
				wantOutput = 262144
			}
			if model.ContextLength != wantContext || model.MaxCompletionTokens != wantOutput {
				t.Fatalf("limits = %d/%d, want %d/%d", model.ContextLength, model.MaxCompletionTokens, wantContext, wantOutput)
			}
			if model.Thinking == nil || len(model.Thinking.Levels) == 0 {
				t.Fatal("missing thinking support")
			}
			if strings.HasSuffix(id, "-xhigh") {
				found := false
				for _, level := range model.Thinking.Levels {
					found = found || level == "xhigh"
				}
				if !found {
					t.Fatal("missing xhigh thinking support")
				}
			}
			if model.DisplayName == "" || model.OwnedBy != "antigravity" || model.Type != "antigravity" {
				t.Fatalf("invalid model metadata: %#v", model)
			}
		})
	}

	for _, id := range []string{"chat_20706", "chat_23310", "tab_flash_lite_preview", "tab_jump_flash_lite_preview"} {
		if models[id] != nil {
			t.Errorf("internal completion model %q must not be in the chat catalog", id)
		}
	}
	imageModel := models["gemini-3.1-flash-image"]
	if imageModel == nil || imageModel.ContextLength != 65536 || imageModel.MaxCompletionTokens != 65536 {
		t.Fatal("image model must retain the upstream token limits")
	}
	oss := models["gpt-oss-120b-medium"]
	if oss == nil || oss.ContextLength != 131072 || oss.Thinking == nil {
		t.Fatal("GPT-OSS must use the upstream context limit and thinking support")
	}
}
