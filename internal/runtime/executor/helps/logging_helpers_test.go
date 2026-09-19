package helps

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/logging"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

func TestRequestLoggingDoesNotMarkUpstreamAttempt(t *testing.T) {
	tests := []struct {
		name   string
		record func(context.Context)
	}{
		{
			name: "HTTP",
			record: func(ctx context.Context) {
				RecordAPIRequest(ctx, &config.Config{}, UpstreamRequestLog{URL: "https://api.example.com", Method: http.MethodPost})
			},
		},
		{
			name: "websocket",
			record: func(ctx context.Context) {
				RecordAPIWebsocketRequest(ctx, &config.Config{}, UpstreamRequestLog{URL: "wss://api.example.com", Method: "WEBSOCKET"})
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ctx := cliproxyexecutor.WithUpstreamAttemptTracker(context.Background())
			test.record(ctx)
			if cliproxyexecutor.UpstreamAttempted(ctx) {
				t.Fatal("request logging marked an upstream attempt before transport")
			}
		})
	}
}

func TestRecordAPIRequestClonesDeferredBodyWhenRequestLogDisabled(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	ginCtx, _ := gin.CreateTestContext(recorder)
	ctx := context.WithValue(context.Background(), "gin", ginCtx)
	body := []byte(`{"model":"original"}`)

	RecordAPIRequest(ctx, &config.Config{}, UpstreamRequestLog{
		URL:    "https://api.example.com/v1/responses",
		Method: http.MethodPost,
		Body:   body,
	})
	body[10] = 'X'

	value, exists := ginCtx.Get(logging.DeferredAPIRequestContextKey)
	if !exists {
		t.Fatal("deferred API request was not captured")
	}
	requests, ok := value.([]logging.DeferredAPIRequest)
	if !ok || len(requests) != 1 {
		t.Fatalf("deferred API requests = %#v, want one request", value)
	}
	captured := string(requests[0]())
	if !strings.Contains(captured, `{"model":"original"}`) {
		t.Fatalf("captured API request = %q, want original body", captured)
	}
}

func TestUpstreamRequestResponseLogsRedactCredentials(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	ginCtx, _ := gin.CreateTestContext(recorder)
	ctx := context.WithValue(context.Background(), "gin", ginCtx)
	cfg := &config.Config{SDKConfig: config.SDKConfig{RequestLog: true}}

	RecordAPIRequest(ctx, cfg, UpstreamRequestLog{
		URL:    "https://api.example.com/v1/responses",
		Method: http.MethodPost,
		Headers: http.Header{
			"aUtHoRiZaTiOn":       {"Bearer upstream-secret"},
			"pRoXy-AuThOrIzAtIoN": {"Basic proxy-secret"},
			"cOoKiE":              {"session=one", "refresh=two"},
			"X-Diagnostic":        {"kept-request"},
		},
		Body:      []byte(`{"full":"request-body"}`),
		AuthType:  "custom",
		AuthValue: "auth-metadata-secret",
	})
	RecordAPIResponseMetadata(ctx, cfg, http.StatusUnauthorized, http.Header{
		"SeT-cOoKiE":         {"response-session=one", "response-refresh=two"},
		"CoOkIe":             {"response-cookie=three"},
		"X-Upstream-Request": {"kept-response"},
	})
	AppendAPIResponseChunk(ctx, cfg, []byte(`{"full":"response-body"}`))

	requestValue, exists := ginCtx.Get(apiRequestKey)
	if !exists {
		t.Fatal("API_REQUEST was not captured")
	}
	responseValue, exists := ginCtx.Get(apiResponseKey)
	if !exists {
		t.Fatal("API_RESPONSE was not captured")
	}
	logText := string(requestValue.([]byte)) + string(responseValue.([]byte))

	for _, want := range []string{
		"aUtHoRiZaTiOn: [REDACTED]",
		"pRoXy-AuThOrIzAtIoN: [REDACTED]",
		"cOoKiE: [REDACTED]",
		"SeT-cOoKiE: [REDACTED]",
		"CoOkIe: [REDACTED]",
		"X-Diagnostic: kept-request",
		"X-Upstream-Request: kept-response",
		"Auth: type=custom",
		`{"full":"request-body"}`,
		`{"full":"response-body"}`,
	} {
		if !strings.Contains(logText, want) {
			t.Fatalf("upstream log missing %q:\n%s", want, logText)
		}
	}
	requestText := string(requestValue.([]byte))
	if got := strings.Count(requestText, "cOoKiE: [REDACTED]"); got != 2 {
		t.Fatalf("redacted request cookie values = %d, want 2:\n%s", got, requestText)
	}
	if got := strings.Count(logText, "SeT-cOoKiE: [REDACTED]"); got != 2 {
		t.Fatalf("redacted response Set-Cookie values = %d, want 2:\n%s", got, logText)
	}
	for _, secret := range []string{
		"upstream-secret",
		"proxy-secret",
		"session=one",
		"refresh=two",
		"response-session=one",
		"response-refresh=two",
		"response-cookie=three",
		"auth-metadata-secret",
	} {
		if strings.Contains(logText, secret) {
			t.Fatalf("upstream log leaked %q:\n%s", secret, logText)
		}
	}
}

func TestRecordAPIResponseMetadataStoresHeadersWhenRequestLogDisabled(t *testing.T) {
	ctx := logging.WithResponseHeadersHolder(context.Background())
	headers := http.Header{}
	headers.Add("X-Upstream-Request-Id", "upstream-req-1")

	RecordAPIResponseMetadata(ctx, &config.Config{}, http.StatusOK, headers)
	headers.Set("X-Upstream-Request-Id", "mutated")

	got := logging.GetResponseHeaders(ctx)
	if got.Get("X-Upstream-Request-Id") != "upstream-req-1" {
		t.Fatalf("response header = %q, want %q", got.Get("X-Upstream-Request-Id"), "upstream-req-1")
	}
}

func TestAPIResponseAttemptsAreSeparated(t *testing.T) {
	gin.SetMode(gin.TestMode)

	tests := []struct {
		name              string
		fileBacked        bool
		firstResponseBody []byte
	}{
		{name: "memory backed error"},
		{name: "file backed error", fileBacked: true},
		{name: "memory backed partial body", firstResponseBody: []byte("partial")},
		{name: "file backed partial body", fileBacked: true, firstResponseBody: []byte("partial")},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			ginCtx, _ := gin.CreateTestContext(recorder)
			var responseSource *logging.FileBodySource
			if tt.fileBacked {
				var errSource error
				responseSource, errSource = logging.NewFileBodySourceInDir(t.TempDir(), "api-response")
				if errSource != nil {
					t.Fatalf("NewFileBodySourceInDir: %v", errSource)
				}
				t.Cleanup(func() {
					if errCleanup := responseSource.Cleanup(); errCleanup != nil {
						t.Errorf("Cleanup: %v", errCleanup)
					}
				})
				ginCtx.Set(logging.APIResponseSourceContextKey, responseSource)
			}

			ctx := context.WithValue(context.Background(), "gin", ginCtx)
			cfg := &config.Config{SDKConfig: config.SDKConfig{RequestLog: true}}
			RecordAPIRequest(ctx, cfg, UpstreamRequestLog{URL: "https://api.example.com/first", Method: http.MethodPost})
			if len(tt.firstResponseBody) > 0 {
				AppendAPIResponseChunk(ctx, cfg, tt.firstResponseBody)
			} else {
				RecordAPIResponseError(ctx, cfg, errors.New("EOF"))
			}
			RecordAPIRequest(ctx, cfg, UpstreamRequestLog{URL: "https://api.example.com/second", Method: http.MethodPost})
			RecordAPIResponseError(ctx, cfg, errors.New("retry failed"))

			var response []byte
			if responseSource != nil {
				var errBytes error
				response, errBytes = responseSource.Bytes()
				if errBytes != nil {
					t.Fatalf("responseSource.Bytes: %v", errBytes)
				}
			} else {
				value, exists := ginCtx.Get(apiResponseKey)
				if !exists {
					t.Fatal("API_RESPONSE was not captured")
				}
				response, _ = value.([]byte)
			}

			previousEnd := "Error: EOF"
			if len(tt.firstResponseBody) > 0 {
				previousEnd = string(tt.firstResponseBody)
			}
			wantBoundary := previousEnd + "\n\n=== API RESPONSE 2 ==="
			if !strings.Contains(string(response), wantBoundary) {
				t.Fatalf("API response attempts are not separated by one blank line:\n%q\nwant boundary %q", response, wantBoundary)
			}
		})
	}
}
