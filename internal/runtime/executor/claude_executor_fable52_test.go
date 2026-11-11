package executor

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
	"github.com/tidwall/gjson"
)

func TestClaudeExecutor_Fable52(t *testing.T) {
	for _, stream := range []bool{false, true} {
		name := "nonstream"
		if stream {
			name = "stream"
		}
		t.Run(name, func(t *testing.T) {
			var seenBody []byte
			var seenHeaders http.Header
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				seenBody, _ = io.ReadAll(r.Body)
				seenHeaders = r.Header.Clone()
				if stream {
					w.Header().Set("Content-Type", "text/event-stream")
					_, _ = io.WriteString(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_1\",\"type\":\"message\",\"model\":\"claude-fable-5-2\",\"role\":\"assistant\",\"content\":[],\"usage\":{\"input_tokens\":1,\"output_tokens\":0}}}\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
					return
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `{"id":"msg_1","type":"message","model":"claude-fable-5-2","role":"assistant","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`)
			}))
			defer server.Close()

			auth := &cliproxyauth.Auth{
				ID:       "auth-fable52-" + name,
				Metadata: claudeOAuthTestMetadata(),
				Attributes: map[string]string{
					"api_key": "sk-ant-oat-fable52-test", "base_url": server.URL,
				},
			}
			executor := NewClaudeExecutor(&config.Config{})
			req := cliproxyexecutor.Request{
				Model:   "claude-fable-5-2(high)",
				Payload: []byte(`{"model":"claude-fable-5-2(high)","max_tokens":1024,"messages":[{"role":"user","content":"test"}]}`),
			}
			opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatClaude}
			if stream {
				result, err := executor.ExecuteStream(context.Background(), auth, req, opts)
				if err != nil {
					t.Fatal(err)
				}
				chunks := 0
				for chunk := range result.Chunks {
					if chunk.Err != nil {
						t.Fatal(chunk.Err)
					}
					chunks++
				}
				if chunks == 0 {
					t.Fatal("no streaming response chunks")
				}
			} else {
				result, err := executor.Execute(context.Background(), auth, req, opts)
				if err != nil {
					t.Fatal(err)
				}
				if gjson.GetBytes(result.Payload, "model").String() != "claude-fable-5-2" {
					t.Fatalf("unexpected response: %s", result.Payload)
				}
			}
			if got := gjson.GetBytes(seenBody, "model").String(); got != "claude-fable-5-2" {
				t.Fatalf("upstream model = %q", got)
			}
			if gjson.GetBytes(seenBody, "thinking.type").String() != "adaptive" || gjson.GetBytes(seenBody, "output_config.effort").String() != "high" {
				t.Fatalf("thinking suffix not applied: %s", seenBody)
			}
			// These additions were observed only for 5.1, not 5.2.
			if gjson.GetBytes(seenBody, "fallbacks").Exists() || hasFableReportingBlock(seenBody) || strings.Contains(seenHeaders.Get("Anthropic-Beta"), "server-side-fallback") {
				t.Fatalf("unexpected Fable 5.1 additions: %s", seenBody)
			}
		})
	}
}
