package management

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
)

func TestClaudeCodeHarnessQuotaManagement(t *testing.T) {
	gin.SetMode(gin.TestMode)
	var receivedAuthorization string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedAuthorization = r.Header.Get("Authorization")
		if r.URL.Path != claudeCodeHarnessUsagePath {
			t.Fatalf("path = %q, want %q", r.URL.Path, claudeCodeHarnessUsagePath)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"usage":{"five_hour":{"utilization":5,"resets_at":"2026-09-16T11:00:00Z"}},"profile":{"account":{"has_claude_max":true}}}`))
	}))
	defer upstream.Close()

	handler := NewHandlerWithoutConfigFilePath(&config.Config{
		OpenAICompatibility: []config.OpenAICompatibility{{
			Name:              "claude-code-harness",
			BaseURL:           upstream.URL + "/v1",
			APIKeyEntries:     []config.OpenAICompatibilityAPIKey{{APIKey: "internal-secret"}},
			ClaudeCodeHarness: &config.ClaudeCodeHarnessConfig{DisplayName: "Will - Claude Max"},
		}},
	}, nil)

	listRecorder := httptest.NewRecorder()
	listContext, _ := gin.CreateTestContext(listRecorder)
	listContext.Request = httptest.NewRequest(http.MethodGet, "/v0/management/claude-code-harnesses", nil)
	handler.ListClaudeCodeHarnesses(listContext)
	if listRecorder.Code != http.StatusOK {
		t.Fatalf("list status = %d, body = %s", listRecorder.Code, listRecorder.Body.String())
	}
	var listPayload struct {
		Harnesses []struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"harnesses"`
	}
	if err := json.Unmarshal(listRecorder.Body.Bytes(), &listPayload); err != nil {
		t.Fatal(err)
	}
	if len(listPayload.Harnesses) != 1 || listPayload.Harnesses[0].Name != "Will - Claude Max" {
		t.Fatalf("unexpected harness list: %+v", listPayload.Harnesses)
	}

	quotaRecorder := httptest.NewRecorder()
	quotaContext, _ := gin.CreateTestContext(quotaRecorder)
	quotaContext.Params = gin.Params{{Key: "id", Value: listPayload.Harnesses[0].ID}}
	quotaContext.Request = httptest.NewRequest(http.MethodPost, "/v0/management/claude-code-harnesses/id/quota", strings.NewReader(""))
	handler.FetchClaudeCodeHarnessQuota(quotaContext)
	if quotaRecorder.Code != http.StatusOK {
		t.Fatalf("quota status = %d, body = %s", quotaRecorder.Code, quotaRecorder.Body.String())
	}
	if receivedAuthorization != "Bearer internal-secret" {
		t.Fatalf("authorization = %q", receivedAuthorization)
	}
	if strings.Contains(quotaRecorder.Body.String(), "internal-secret") {
		t.Fatal("quota response leaked the adapter API key")
	}
}
