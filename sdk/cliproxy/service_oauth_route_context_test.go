package cliproxy

import (
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
)

func findModel(t *testing.T, models []*ModelInfo, id string) *ModelInfo {
	t.Helper()
	for _, model := range models {
		if model != nil && model.ID == id {
			return model
		}
	}
	t.Fatalf("model %q not found in %+v", id, models)
	return nil
}

func TestApplyOAuthModelAliasEntries_SharedAliasUsesSmallestMemberWindow(t *testing.T) {
	aliases := []config.OAuthModelAlias{
		{Name: "gpt-5.6-sol", Alias: "tier-deep"},
		{Name: "gpt-6-sol", Alias: "tier-deep"},
	}
	models := []*ModelInfo{
		{ID: "gpt-5.6-sol", ContextLength: 872000},
		{ID: "gpt-6-sol", ContextLength: 272000},
	}

	out := applyOAuthModelAliasEntries(aliases, models)

	route := findModel(t, out, "tier-deep")
	if route.ContextLength != 272000 || route.MaxContextLength != 272000 {
		t.Errorf("tier-deep context = %d/%d, want 272000", route.ContextLength, route.MaxContextLength)
	}
	if route.MetadataModelID != "gpt-5.6-sol" {
		t.Errorf("tier-deep metadata = %q, want first member gpt-5.6-sol", route.MetadataModelID)
	}
	if models[0].ContextLength != 872000 || models[0].MaxContextLength != 0 {
		t.Errorf("input model was mutated: %+v", models[0])
	}
}

func TestApplyOAuthModelAliasAndSettings_SettingsShapeSharedAlias(t *testing.T) {
	cfg := &config.Config{
		OAuthModelAlias: map[string][]config.OAuthModelAlias{
			"codex": {
				{Name: "gpt-5.6-sol", Alias: "tier-deep"},
				{Name: "gpt-6-sol", Alias: "tier-deep"},
				{Name: "gpt-6-sol", Alias: "custom-sol", Fork: true},
			},
		},
		OAuthSettings: map[string][]config.OAuthModelSetting{
			"codex": {
				{Name: "gpt-5.6-sol", MaxContextLength: 872000},
				{Name: "gpt-6-sol", MaxContextLength: 600000},
			},
		},
	}
	models := []*ModelInfo{
		{ID: "gpt-5.6-sol", ContextLength: 272000},
		{ID: "gpt-6-sol", ContextLength: 272000},
	}

	out := applyOAuthModelAliasAndSettingsForAuth(cfg, "codex", "oauth", nil, models)
	out = applyOAuthSettingsAfterAliasForAuth(cfg, "codex", "oauth", out)

	// The post-alias pass must not raise tier-deep back to its first member's 872000.
	if route := findModel(t, out, "tier-deep"); route.ContextLength != 600000 || route.MaxContextLength != 600000 {
		t.Errorf("tier-deep context = %d/%d, want 600000", route.ContextLength, route.MaxContextLength)
	}
	if route := findModel(t, out, "custom-sol"); route.MaxContextLength != 600000 {
		t.Errorf("custom-sol MaxContextLength = %d, want inherited 600000", route.MaxContextLength)
	}
	if model := findModel(t, out, "gpt-6-sol"); model.MaxContextLength != 600000 {
		t.Errorf("gpt-6-sol MaxContextLength = %d, want 600000", model.MaxContextLength)
	}
}

func TestApplyOAuthSettingsAfterAlias_ExplicitAliasSettingWins(t *testing.T) {
	cfg := &config.Config{
		OAuthModelAlias: map[string][]config.OAuthModelAlias{
			"codex": {
				{Name: "gpt-5.6-sol", Alias: "tier-deep"},
				{Name: "gpt-6-sol", Alias: "tier-deep"},
			},
		},
		OAuthSettings: map[string][]config.OAuthModelSetting{
			"codex": {
				{Name: "gpt-5.6-sol", MaxContextLength: 872000},
				{Name: "tier-deep", MaxContextLength: 400000},
			},
		},
	}
	models := []*ModelInfo{
		{ID: "gpt-5.6-sol", ContextLength: 272000},
		{ID: "gpt-6-sol", ContextLength: 272000},
	}

	out := applyOAuthModelAliasAndSettingsForAuth(cfg, "codex", "oauth", nil, models)
	out = applyOAuthSettingsAfterAliasForAuth(cfg, "codex", "oauth", out)

	if route := findModel(t, out, "tier-deep"); route.MaxContextLength != 400000 {
		t.Errorf("tier-deep MaxContextLength = %d, want explicit 400000", route.MaxContextLength)
	}
}
