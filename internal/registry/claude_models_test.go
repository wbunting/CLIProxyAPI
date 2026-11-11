package registry

import (
	"reflect"
	"testing"
)

func TestClaudeFable52Builtin(t *testing.T) {
	original := getModels()
	t.Cleanup(func() {
		modelsCatalogStore.mu.Lock()
		modelsCatalogStore.data = original
		modelsCatalogStore.mu.Unlock()
	})

	for _, catalog := range []string{
		`{"claude":[{"id":"claude-fable-5-1"}]}`,
		`{"claude":[{"id":"claude-fable-5-2","context_length":1}]}`,
	} {
		t.Run(catalog, func(t *testing.T) {
			if err := loadModelsFromBytes([]byte(catalog), "test"); err != nil {
				t.Fatal(err)
			}
			count := 0
			for _, model := range GetClaudeModels() {
				if model.ID == "claude-fable-5-2" {
					count++
				}
			}
			if count != 1 {
				t.Fatalf("Fable 5.2 catalog entries = %d, want 1", count)
			}

			for _, model := range []*ModelInfo{
				LookupStaticModelInfo("claude-fable-5-2"),
				LookupStaticModelInfoByChannel("claude-fable-5-2", "claude"),
			} {
				if model == nil {
					t.Fatal("Fable 5.2 missing from static lookup")
				}
				if model.DisplayName != "Claude Fable 5.2" || model.Type != "claude" || model.OwnedBy != "anthropic" || model.Object != "model" {
					t.Fatalf("unexpected model identity: %+v", model)
				}
				if model.ContextLength != 1000000 || model.MaxCompletionTokens != 128000 {
					t.Fatalf("unexpected model limits: %+v", model)
				}
				wantThinking := &ThinkingSupport{
					Min: 1024, Max: 128000, ZeroAllowed: true,
					Levels: []string{"low", "medium", "high", "xhigh", "max"},
				}
				if !reflect.DeepEqual(model.Thinking, wantThinking) {
					t.Fatalf("thinking = %+v, want %+v", model.Thinking, wantThinking)
				}
				if !reflect.DeepEqual(model.SupportedInputModalities, []string{"text", "image"}) ||
					!reflect.DeepEqual(model.SupportedOutputModalities, []string{"text"}) {
					t.Fatalf("unexpected modalities: %+v", model)
				}

				model.Thinking.Levels[0] = "changed"
				model.SupportedInputModalities[0] = "changed"
				fresh := LookupStaticModelInfo("claude-fable-5-2")
				if fresh.Thinking.Levels[0] != "low" || fresh.SupportedInputModalities[0] != "text" {
					t.Fatal("mutating a lookup result modified the builtin")
				}
			}
		})
	}
}
