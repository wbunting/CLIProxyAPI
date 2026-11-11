package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
)

func TestModelsIncludesClaudeFable52(t *testing.T) {
	modelRegistry := registry.GetGlobalRegistry()
	const clientID = "test-fable52-model-list"
	modelRegistry.RegisterClient(clientID, "claude", registry.GetClaudeModels())
	t.Cleanup(func() { modelRegistry.UnregisterClient(clientID) })
	server := newTestServer(t)

	for _, anthropic := range []bool{false, true} {
		req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
		req.Header.Set("Authorization", "Bearer test-key")
		if anthropic {
			req.Header.Set("Anthropic-Version", "2023-06-01")
		}
		rr := httptest.NewRecorder()
		server.engine.ServeHTTP(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("status = %d: %s", rr.Code, rr.Body.String())
		}
		var response struct {
			Data []struct {
				ID string `json:"id"`
			} `json:"data"`
		}
		if err := json.Unmarshal(rr.Body.Bytes(), &response); err != nil {
			t.Fatal(err)
		}
		found := false
		for _, model := range response.Data {
			if model.ID == "claude-fable-5-2" {
				found = true
			}
		}
		if !found {
			t.Fatalf("Fable 5.2 missing (Anthropic format=%t): %s", anthropic, rr.Body.String())
		}
	}
}
