package helps

import (
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/thinking"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

const antigravityThinkingConfigPath = "request.generationConfig.thinkingConfig"

// antigravityVariantLevels orders the effort levels Antigravity serves as
// separate models, from lowest to highest.
var antigravityVariantLevels = []string{"low", "medium", "high", "xhigh"}

// AntigravityThinkingVariant resolves a grouped Antigravity model, such as
// gemini-4-neon, to the effort-specific model the Antigravity API serves, such
// as gemini-4-neon-xhigh. Antigravity lists every effort level as its own
// model; grouping lets clients choose one model plus a thinking level.
//
// The level comes from the translated thinkingConfig. Without one the group
// uses "high", "none" uses the lowest variant, and "max" the highest. Models
// without thinking variants are returned unchanged.
//
// thinkingLevel is kept for low, medium, and high, matching what the variants
// accept. It is removed for xhigh, which has no documented thinkingLevel value;
// the xhigh variant applies its own effort.
func AntigravityThinkingVariant(model string, payload []byte) (string, []byte) {
	variants := antigravityThinkingVariants(model)
	if len(variants) == 0 {
		return model, payload
	}
	level := pickAntigravityVariantLevel(variants, requestedAntigravityThinkingLevel(payload))
	if level == "" {
		return model, payload
	}

	if gjson.GetBytes(payload, antigravityThinkingConfigPath).IsObject() {
		payload, _ = sjson.DeleteBytes(payload, antigravityThinkingConfigPath+".thinkingBudget")
		payload, _ = sjson.DeleteBytes(payload, antigravityThinkingConfigPath+".thinking_budget")
		payload, _ = sjson.DeleteBytes(payload, antigravityThinkingConfigPath+".thinking_level")
		if level == "xhigh" {
			payload, _ = sjson.DeleteBytes(payload, antigravityThinkingConfigPath+".thinkingLevel")
		} else {
			payload, _ = sjson.SetBytes(payload, antigravityThinkingConfigPath+".thinkingLevel", level)
		}
		if config := gjson.GetBytes(payload, antigravityThinkingConfigPath); config.IsObject() && len(config.Map()) == 0 {
			payload, _ = sjson.DeleteBytes(payload, antigravityThinkingConfigPath)
		}
	}
	return variants[level], payload
}

// antigravityThinkingVariants reads the variant map for a grouped model. The
// registered Antigravity entry wins; the bundled Antigravity catalog is the
// fallback so a shared ID such as gemini-3.8-flash never resolves through
// another provider's catalog.
func antigravityThinkingVariants(model string) map[string]string {
	model = strings.TrimSpace(model)
	if model == "" {
		return nil
	}
	if info := registry.LookupModelInfo(model, "antigravity"); info != nil && info.Config != nil && len(info.Config.ThinkingVariants) > 0 {
		return info.Config.ThinkingVariants
	}
	for _, info := range registry.GetAntigravityModels() {
		if info != nil && info.ID == model && info.Config != nil {
			return info.Config.ThinkingVariants
		}
	}
	return nil
}

func requestedAntigravityThinkingLevel(payload []byte) string {
	config := gjson.GetBytes(payload, antigravityThinkingConfigPath)
	if !config.IsObject() {
		return ""
	}
	for _, key := range []string{"thinkingLevel", "thinking_level"} {
		if level := strings.ToLower(strings.TrimSpace(config.Get(key).String())); level != "" {
			return level
		}
	}
	for _, key := range []string{"thinkingBudget", "thinking_budget"} {
		budget := config.Get(key)
		if budget.Type != gjson.Number {
			continue
		}
		switch value := int(budget.Int()); {
		case value == 0:
			return "none"
		case value < 0:
			return "auto"
		default:
			if level, ok := thinking.ConvertBudgetToLevel(value); ok {
				return level
			}
		}
	}
	return ""
}

func pickAntigravityVariantLevel(variants map[string]string, requested string) string {
	if variants[requested] != "" {
		return requested
	}
	available := make([]string, 0, len(antigravityVariantLevels))
	for _, level := range antigravityVariantLevels {
		if variants[level] != "" {
			available = append(available, level)
		}
	}
	if len(available) == 0 {
		return ""
	}
	switch requested {
	case "none", "minimal":
		return available[0]
	case "max":
		return available[len(available)-1]
	}
	if variants["high"] != "" {
		return "high"
	}
	return available[len(available)-1]
}
