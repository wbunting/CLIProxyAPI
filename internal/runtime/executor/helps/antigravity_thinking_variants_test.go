package helps

import (
	"testing"

	"github.com/tidwall/gjson"
)

func TestAntigravityThinkingVariant(t *testing.T) {
	tests := []struct {
		name      string
		model     string
		payload   string
		wantModel string
		// wantLevel is the expected thinkingLevel; "-" means the field is absent.
		wantLevel string
	}{
		{name: "level picks variant", model: "gemini-4-neon", payload: `{"request":{"generationConfig":{"thinkingConfig":{"thinkingLevel":"low","includeThoughts":true}}}}`, wantModel: "gemini-4-neon-low", wantLevel: "low"},
		{name: "xhigh drops thinkingLevel", model: "gemini-4-neon", payload: `{"request":{"generationConfig":{"thinkingConfig":{"thinkingLevel":"xhigh","includeThoughts":true}}}}`, wantModel: "gemini-4-neon-xhigh", wantLevel: "-"},
		{name: "budget converts to level", model: "gemini-4-argon", payload: `{"request":{"generationConfig":{"thinkingConfig":{"thinkingBudget":8192}}}}`, wantModel: "gemini-4-argon-medium", wantLevel: "medium"},
		{name: "unsupported level falls back to high", model: "gemini-4-argon", payload: `{"request":{"generationConfig":{"thinkingConfig":{"thinkingLevel":"xhigh"}}}}`, wantModel: "gemini-4-argon-high", wantLevel: "high"},
		{name: "no thinking config uses high", model: "gemini-4-carbon", payload: `{"request":{"contents":[]}}`, wantModel: "gemini-4-carbon-high", wantLevel: "-"},
		{name: "uppercase level is normalized", model: "gemini-4-neon", payload: `{"request":{"generationConfig":{"thinkingConfig":{"thinkingLevel":"MEDIUM"}}}}`, wantModel: "gemini-4-neon-medium", wantLevel: "medium"},
		{name: "invalid negative budget falls back to high", model: "gemini-4-argon", payload: `{"request":{"generationConfig":{"thinkingConfig":{"thinkingBudget":-2}}}}`, wantModel: "gemini-4-argon-high", wantLevel: "high"},
		{name: "disabled thinking uses lowest", model: "gemini-4-hydrogen", payload: `{"request":{"generationConfig":{"thinkingConfig":{"thinkingBudget":0}}}}`, wantModel: "gemini-4-hydrogen-low", wantLevel: "low"},
		{name: "shared id resolves through antigravity catalog", model: "gemini-3.8-flash", payload: `{"request":{"generationConfig":{"thinkingConfig":{"thinkingLevel":"medium"}}}}`, wantModel: "gemini-3.8-flash-medium", wantLevel: "medium"},
		{name: "claude group", model: "claude-opus-5-5-antigravity", payload: `{"request":{"generationConfig":{"thinkingConfig":{"thinkingLevel":"high"}}}}`, wantModel: "claude-opus-5-5-high", wantLevel: "high"},
		{name: "variant id is unchanged", model: "gemini-4-neon-low", payload: `{"request":{"generationConfig":{"thinkingConfig":{"thinkingLevel":"high"}}}}`, wantModel: "gemini-4-neon-low", wantLevel: "high"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			model, payload := AntigravityThinkingVariant(tc.model, []byte(tc.payload))
			if model != tc.wantModel {
				t.Fatalf("model = %q, want %q", model, tc.wantModel)
			}
			level := gjson.GetBytes(payload, "request.generationConfig.thinkingConfig.thinkingLevel")
			switch {
			case tc.wantLevel == "-" && level.Exists():
				t.Fatalf("thinkingLevel = %q, want absent; payload %s", level.String(), payload)
			case tc.wantLevel != "-" && level.String() != tc.wantLevel:
				t.Fatalf("thinkingLevel = %q, want %q; payload %s", level.String(), tc.wantLevel, payload)
			}
			if gjson.GetBytes(payload, "request.generationConfig.thinkingConfig.thinkingBudget").Exists() {
				t.Fatalf("thinkingBudget must be removed for grouped models; payload %s", payload)
			}
		})
	}
}

func TestAntigravityThinkingVariantKeepsIncludeThoughts(t *testing.T) {
	_, payload := AntigravityThinkingVariant("gemini-4-neon", []byte(`{"request":{"generationConfig":{"thinkingConfig":{"thinkingLevel":"xhigh","includeThoughts":true}}}}`))
	if !gjson.GetBytes(payload, "request.generationConfig.thinkingConfig.includeThoughts").Bool() {
		t.Fatalf("includeThoughts was dropped: %s", payload)
	}
}
