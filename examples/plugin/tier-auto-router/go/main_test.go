package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func configureTest(t *testing.T, config string) {
	t.Helper()
	payload, err := json.Marshal(lifecycleRequest{ConfigYAML: []byte(config)})
	if err != nil {
		t.Fatal(err)
	}
	if err := configure(payload); err != nil {
		t.Fatal(err)
	}
	decisions = newTierCache()
}

func routeTest(t *testing.T, body string, models []string) pluginapi.ModelRouteResponse {
	t.Helper()
	payload, err := json.Marshal(rpcModelRouteRequest{ModelRouteRequest: pluginapi.ModelRouteRequest{
		RequestedModel: "tier-auto", Body: []byte(body), AvailableModels: models,
	}})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := routeModel(payload)
	if err != nil {
		t.Fatal(err)
	}
	var env envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatal(err)
	}
	var response pluginapi.ModelRouteResponse
	if err := json.Unmarshal(env.Result, &response); err != nil {
		t.Fatal(err)
	}
	return response
}

func TestTurnOneAndTwoReuseDecision(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		_, _ = w.Write([]byte(`{"answers":{"tier":{"type":"choice","choice":"deep"}},"providerMetadata":{"typesafe":{"confidence":{"tier":0.94}}}}`))
	}))
	defer server.Close()
	t.Setenv("VERCEL_AI_GATEWAY_KEY", "test-key")
	configureTest(t, "gateway_base_url: "+server.URL+"\nttl: 1h\n")
	first := routeTest(t, `{"messages":[{"role":"system","content":"system"},{"role":"user","content":"debug a hard failure"}]}`, []string{"tier-balanced", "tier-deep"})
	if first.TargetModel != "tier-deep" || first.Reason == "" {
		t.Fatalf("first route = %#v", first)
	}
	second := routeTest(t, `{"messages":[{"role":"system","content":"system"},{"role":"user","content":"debug a hard failure"},{"role":"assistant","content":"I will investigate"},{"role":"user","content":"any update?"}]}`, []string{"tier-balanced", "tier-deep"})
	if second.TargetModel != "tier-deep" || !contains(second.Reason, "fallback_cache_hit") {
		t.Fatalf("second route = %#v", second)
	}
	if calls != 1 {
		t.Fatalf("gateway calls = %d, want 1", calls)
	}
}

func TestGatewayErrorFailsOpen(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusBadGateway) }))
	defer server.Close()
	t.Setenv("VERCEL_AI_GATEWAY_KEY", "test-key")
	configureTest(t, "gateway_base_url: "+server.URL+"\n")
	response := routeTest(t, `{"messages":[{"role":"user","content":"do work"}]}`, []string{"tier-balanced", "tier-deep"})
	if response.TargetModel != "tier-balanced" || !contains(response.Reason, "gateway_error") {
		t.Fatalf("route = %#v", response)
	}
}

func TestUnavailableTierFailsOpen(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"answers":{"tier":{"type":"choice","choice":"frontier"}}}`))
	}))
	defer server.Close()
	t.Setenv("VERCEL_AI_GATEWAY_KEY", "test-key")
	configureTest(t, "gateway_base_url: "+server.URL+"\n")
	response := routeTest(t, `{"messages":[{"role":"user","content":"do work"}]}`, []string{"tier-balanced", "tier-deep"})
	if response.TargetModel != "tier-balanced" || !contains(response.Reason, "unavailable_choice") {
		t.Fatalf("route = %#v", response)
	}
}

func TestSingleCandidateSkipsGateway(t *testing.T) {
	t.Setenv("VERCEL_AI_GATEWAY_KEY", "")
	configureTest(t, "gateway_base_url: http://127.0.0.1:1\n")
	response := routeTest(t, `{"messages":[{"role":"user","content":"do work"}]}`, []string{"tier-deep"})
	if response.TargetModel != "tier-deep" || !contains(response.Reason, "single_candidate") {
		t.Fatalf("route = %#v", response)
	}
}

func TestCacheHitSkipsGateway(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		_, _ = w.Write([]byte(`{"answers":{"tier":{"type":"choice","choice":"balanced"}}}`))
	}))
	defer server.Close()
	t.Setenv("VERCEL_AI_GATEWAY_KEY", "test-key")
	configureTest(t, "gateway_base_url: "+server.URL+"\nttl: 1h\n")
	body := `{"messages":[{"role":"user","content":"add a focused test"}]}`
	_ = routeTest(t, body, []string{"tier-balanced", "tier-deep"})
	response := routeTest(t, body, []string{"tier-balanced", "tier-deep"})
	if !contains(response.Reason, "cache_hit") || calls != 1 {
		t.Fatalf("response=%#v calls=%d", response, calls)
	}
}

func TestClamp(t *testing.T) {
	text := make([]byte, 5001)
	for index := range text {
		text[index] = 'x'
	}
	clamped, truncated := clamp(string(text))
	if !truncated || len(clamped) <= 4000 {
		t.Fatalf("clamp = %d chars, truncated=%v", len(clamped), truncated)
	}
}

func TestCacheExpires(t *testing.T) {
	cache := newTierCache()
	cache.set("session", "", cacheEntry{Tier: "tier-deep", ExpiresAt: time.Now().Add(-time.Second)})
	if _, ok := cache.get("session", time.Now()); ok {
		t.Fatal("expired cache entry hit")
	}
}

func contains(value, needle string) bool {
	for index := 0; index+len(needle) <= len(value); index++ {
		if value[index:index+len(needle)] == needle {
			return true
		}
	}
	return false
}
