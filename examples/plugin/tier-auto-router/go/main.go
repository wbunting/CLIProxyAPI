package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
	"gopkg.in/yaml.v3"
)

const pluginIdentifier = "tier-auto-router"

const defaultInstructions = "You are routing one coding-agent task to a model tier. Read the request and choose the cheapest tier that can complete it well. Judge the work the request actually implies, not how politely or verbosely it is written. A long paste with a small ask is still a small task; a short sentence asking for a redesign is not. When the request is ambiguous about scope, prefer the lower tier — the user can escalate, and over-routing every task defeats the purpose."

var defaultCriteria = map[string]string{
	"fast":     "Trivial or mechanical work: a short factual question, a rename, a one-line edit, reading or summarising a file, running a simple command. No design judgement is required.",
	"balanced": "Ordinary development work: implement a well-specified change, fix a clearly-described bug, write focused tests, review a small diff. Some judgement is needed but the approach is evident.",
	"deep":     "Multi-step work that needs sustained reasoning: debug a failure whose cause is unclear, design or refactor across several files, trace behaviour through unfamiliar code, or weigh competing approaches.",
	"frontier": "The hardest work: novel architecture, subtle concurrency or correctness bugs, security-sensitive reasoning, large ambiguous refactors, or anything where a wrong answer is expensive to discover later.",
}

type envelope struct {
	OK     bool            `json:"ok"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *envelopeError  `json:"error,omitempty"`
}

type envelopeError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type processRequest struct {
	Method  string          `json:"method"`
	Request json.RawMessage `json:"request"`
}

type lifecycleRequest struct {
	ConfigYAML []byte `json:"config_yaml"`
}

type rpcModelRouteRequest struct {
	pluginapi.ModelRouteRequest
	HostCallbackID string `json:"host_callback_id,omitempty"`
}

type pluginConfig struct {
	Enabled        bool              `yaml:"enabled"`
	Model          string            `yaml:"model"`
	GatewayBaseURL string            `yaml:"gateway_base_url"`
	Timeout        time.Duration     `yaml:"timeout"`
	TTL            time.Duration     `yaml:"ttl"`
	FallbackTier   string            `yaml:"fallback_tier"`
	Instructions   string            `yaml:"instructions"`
	Criteria       map[string]string `yaml:"criteria"`
}

type registration struct {
	SchemaVersion uint32                 `json:"schema_version"`
	Metadata      pluginapi.Metadata     `json:"metadata"`
	Capabilities  registrationCapability `json:"capabilities"`
}

type registrationCapability struct {
	ModelRouter bool `json:"model_router"`
}

type cacheEntry struct {
	Tier       string
	Confidence *float64
	ExpiresAt  time.Time
}

type tierCache struct {
	mu      sync.Mutex
	entries map[string]cacheEntry
}

func newTierCache() *tierCache { return &tierCache{entries: make(map[string]cacheEntry)} }

func (c *tierCache) get(key string, now time.Time) (cacheEntry, bool) {
	if key == "" {
		return cacheEntry{}, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	entry, ok := c.entries[key]
	if !ok || !entry.ExpiresAt.After(now) {
		delete(c.entries, key)
		return cacheEntry{}, false
	}
	return entry, true
}

func (c *tierCache) set(primary, fallback string, entry cacheEntry) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if primary != "" {
		c.entries[primary] = entry
	}
	if fallback != "" && fallback != primary {
		c.entries[fallback] = entry
	}
}

var routerConfig = defaultConfig()
var configMu sync.RWMutex
var decisions = newTierCache()

func main() {
	scanner := bufio.NewScanner(os.Stdin)
	buffer := make([]byte, 0, 1<<20)
	scanner.Buffer(buffer, 8<<20)
	for scanner.Scan() {
		var call processRequest
		if err := json.Unmarshal(scanner.Bytes(), &call); err != nil {
			writeEnvelope(errorEnvelope("invalid_request", err.Error()))
			continue
		}
		response, errHandle := handleMethod(call.Method, call.Request)
		if errHandle != nil {
			writeEnvelope(errorEnvelope("plugin_error", errHandle.Error()))
			continue
		}
		writeEnvelope(response)
	}
}

func writeEnvelope(raw []byte) { _, _ = os.Stdout.Write(append(raw, '\n')) }

func handleMethod(method string, raw []byte) ([]byte, error) {
	switch method {
	case pluginabi.MethodPluginRegister, pluginabi.MethodPluginReconfigure:
		if errConfigure := configure(raw); errConfigure != nil {
			return nil, errConfigure
		}
		return okEnvelope(pluginRegistration())
	case pluginabi.MethodModelRoute:
		return routeModel(raw)
	case pluginabi.MethodPluginQuiesce:
		return okEnvelope(struct{}{})
	default:
		return errorEnvelope("unknown_method", "unknown method: "+method), nil
	}
}

func defaultConfig() pluginConfig {
	criteria := make(map[string]string, len(defaultCriteria))
	for tier, criterion := range defaultCriteria {
		criteria[tier] = criterion
	}
	return pluginConfig{
		Enabled: true, Model: "typesafe-ai/jev", GatewayBaseURL: "https://ai-gateway.vercel.sh/v4/ai",
		Timeout: 3 * time.Second, TTL: 2 * time.Hour, FallbackTier: "tier-balanced",
		Instructions: defaultInstructions, Criteria: criteria,
	}
}

func configure(raw []byte) error {
	var req lifecycleRequest
	if len(raw) > 0 {
		if errUnmarshal := json.Unmarshal(raw, &req); errUnmarshal != nil {
			return errUnmarshal
		}
	}
	cfg := defaultConfig()
	if len(req.ConfigYAML) > 0 {
		if errDecode := yaml.Unmarshal(req.ConfigYAML, &cfg); errDecode != nil {
			return errDecode
		}
	}
	cfg.Model = strings.TrimSpace(cfg.Model)
	cfg.GatewayBaseURL = strings.TrimRight(strings.TrimSpace(cfg.GatewayBaseURL), "/")
	cfg.FallbackTier = strings.TrimSpace(cfg.FallbackTier)
	cfg.Instructions = strings.TrimSpace(cfg.Instructions)
	if cfg.Model == "" {
		cfg.Model = defaultConfig().Model
	}
	if cfg.GatewayBaseURL == "" {
		cfg.GatewayBaseURL = defaultConfig().GatewayBaseURL
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = defaultConfig().Timeout
	}
	if cfg.TTL <= 0 {
		cfg.TTL = defaultConfig().TTL
	}
	if cfg.FallbackTier == "" {
		cfg.FallbackTier = defaultConfig().FallbackTier
	}
	if cfg.Instructions == "" {
		cfg.Instructions = defaultConfig().Instructions
	}
	if cfg.Criteria == nil {
		cfg.Criteria = defaultConfig().Criteria
	}
	configMu.Lock()
	routerConfig = cfg
	configMu.Unlock()
	return nil
}

func currentConfig() pluginConfig {
	configMu.RLock()
	defer configMu.RUnlock()
	return routerConfig
}

func pluginRegistration() registration {
	return registration{
		SchemaVersion: pluginabi.SchemaVersion,
		Metadata: pluginapi.Metadata{
			Name: "tier-auto-router", Version: "0.1.0", Author: "router-for-me",
			GitHubRepository: "https://github.com/router-for-me/CLIProxyAPI",
			ConfigFields: []pluginapi.ConfigField{
				{Name: "enabled", Type: pluginapi.ConfigFieldTypeBoolean, Description: "Enable tier-auto routing."},
				{Name: "model", Type: pluginapi.ConfigFieldTypeString, Description: "Vercel AI Gateway evaluation model ID."},
				{Name: "gateway_base_url", Type: pluginapi.ConfigFieldTypeString, Description: "Vercel AI Gateway evaluation API base URL."},
				{Name: "timeout", Type: pluginapi.ConfigFieldTypeString, Description: "Maximum time for one JEV decision."},
				{Name: "ttl", Type: pluginapi.ConfigFieldTypeString, Description: "Per-conversation routing decision TTL."},
				{Name: "fallback_tier", Type: pluginapi.ConfigFieldTypeString, Description: "Fail-open tier alias."},
				{Name: "instructions", Type: pluginapi.ConfigFieldTypeString, Description: "JEV choice question policy."},
				{Name: "criteria", Type: pluginapi.ConfigFieldTypeObject, Description: "JEV criteria keyed by tier name without tier-."},
			},
		},
		Capabilities: registrationCapability{ModelRouter: true},
	}
}

func routeModel(raw []byte) ([]byte, error) {
	var request rpcModelRouteRequest
	if errUnmarshal := json.Unmarshal(raw, &request); errUnmarshal != nil {
		return nil, errUnmarshal
	}
	cfg := currentConfig()
	if !cfg.Enabled || !isTierAuto(request.RequestedModel) {
		return okEnvelope(pluginapi.ModelRouteResponse{Handled: false})
	}

	candidates := tierCandidates(request.AvailableModels)
	if len(candidates) == 0 {
		return okEnvelope(failOpen(cfg, candidates, "no tier aliases are available"))
	}
	fallback := chooseFallback(cfg.FallbackTier, candidates)
	if len(candidates) == 1 {
		for _, tier := range candidates {
			return okEnvelope(routeResponse(tier, nil, true, "single_candidate"))
		}
	}

	primary, fallbackSession := cliproxyauth.ExtractSessionIDs(request.Headers, request.Body, request.Metadata)
	now := time.Now()
	if cached, hit := decisions.get(primary, now); hit {
		return okEnvelope(routeResponse(cached.Tier, cached.Confidence, true, "cache_hit"))
	}
	if cached, hit := decisions.get(fallbackSession, now); hit {
		decisions.set(primary, fallbackSession, cached)
		return okEnvelope(routeResponse(cached.Tier, cached.Confidence, true, "fallback_cache_hit"))
	}

	if primary == "" && fallbackSession == "" {
		return okEnvelope(failOpen(cfg, candidates, "no stable conversation session identity"))
	}
	key := gatewayKey()
	if key == "" {
		return okEnvelope(failOpen(cfg, candidates, "VERCEL_AI_GATEWAY_KEY is not configured"))
	}

	requestText, truncated := firstUserRequest(request.Body)
	if requestText == "" {
		return okEnvelope(failOpen(cfg, candidates, "no user message to classify"))
	}
	started := time.Now()
	tier, confidence, cost, errEvaluate := evaluate(context.Background(), cfg, key, requestText, truncated, candidates)
	latency := time.Since(started)
	if errEvaluate != nil {
		log.WithFields(log.Fields{"plugin": pluginIdentifier, "session": shortSession(primary), "tier": fallback, "cache_hit": false, "latency": latency.String(), "reason": errEvaluate.Error()}).Warn("tier-auto: JEV decision failed open")
		return okEnvelope(routeResponse(fallback, nil, false, "gateway_error"))
	}
	selectedTier, exists := candidates[tier]
	if !exists {
		log.WithFields(log.Fields{"plugin": pluginIdentifier, "session": shortSession(primary), "tier": fallback, "cache_hit": false, "latency": latency.String(), "gateway_choice": tier}).Warn("tier-auto: JEV returned unavailable tier; failing open")
		return okEnvelope(routeResponse(fallback, nil, false, "unavailable_choice"))
	}
	entry := cacheEntry{Tier: selectedTier, Confidence: confidence, ExpiresAt: now.Add(cfg.TTL)}
	decisions.set(primary, fallbackSession, entry)
	fields := log.Fields{"plugin": pluginIdentifier, "session": shortSession(primary), "tier": selectedTier, "cache_hit": false, "latency": latency.String()}
	if confidence != nil {
		fields["confidence"] = *confidence
	}
	if cost != nil {
		fields["gateway_cost"] = *cost
	}
	log.WithFields(fields).Info("tier-auto: JEV decision")
	return okEnvelope(routeResponse(selectedTier, confidence, false, "jev"))
}

func isTierAuto(model string) bool {
	model = strings.TrimSpace(model)
	if index := strings.LastIndex(model, "/"); index >= 0 {
		model = model[index+1:]
	}
	return strings.EqualFold(model, "tier-auto")
}

func tierCandidates(models []string) map[string]string {
	result := make(map[string]string)
	for _, alias := range models {
		alias = strings.TrimSpace(alias)
		if !strings.HasPrefix(alias, "tier-") || alias == "tier-auto" {
			continue
		}
		result[strings.TrimPrefix(alias, "tier-")] = alias
	}
	return result
}

func chooseFallback(configured string, candidates map[string]string) string {
	configured = strings.TrimSpace(configured)
	if configured != "" {
		if _, ok := candidates[strings.TrimPrefix(configured, "tier-")]; ok {
			return configured
		}
	}
	if tier, ok := candidates["balanced"]; ok {
		return tier
	}
	for _, tier := range candidates {
		return tier
	}
	return "tier-balanced"
}

func failOpen(cfg pluginConfig, candidates map[string]string, reason string) pluginapi.ModelRouteResponse {
	return routeResponse(chooseFallback(cfg.FallbackTier, candidates), nil, false, "fail_open:"+reason)
}

func routeResponse(tier string, confidence *float64, cacheHit bool, reason string) pluginapi.ModelRouteResponse {
	parts := []string{reason, "model=" + tier, fmt.Sprintf("cache_hit=%t", cacheHit)}
	if confidence != nil {
		parts = append(parts, fmt.Sprintf("confidence=%.4f", *confidence))
	}
	return pluginapi.ModelRouteResponse{Handled: true, TargetKind: pluginapi.ModelRouteTargetModel, TargetModel: tier, Reason: strings.Join(parts, ";")}
}

type evaluationRequest struct {
	State struct {
		Request          string `json:"request"`
		RequestTruncated bool   `json:"requestTruncated"`
	} `json:"state"`
	Questions map[string]choiceQuestion `json:"questions"`
}

type choiceQuestion struct {
	Type         string            `json:"type"`
	Instructions string            `json:"instructions"`
	Criteria     map[string]string `json:"criteria"`
}

type evaluationResponse struct {
	Answers map[string]struct {
		Type   string `json:"type"`
		Choice string `json:"choice"`
	} `json:"answers"`
	ProviderMetadata struct {
		Typesafe struct {
			Confidence map[string]float64 `json:"confidence"`
		} `json:"typesafe"`
		Gateway struct {
			Cost json.RawMessage `json:"cost"`
		} `json:"gateway"`
	} `json:"providerMetadata"`
}

func evaluate(ctx context.Context, cfg pluginConfig, key, text string, truncated bool, candidates map[string]string) (string, *float64, *float64, error) {
	criteria := make(map[string]string, len(candidates))
	for name := range candidates {
		criterion := strings.TrimSpace(cfg.Criteria[name])
		if criterion == "" {
			criterion = defaultCriteria[name]
		}
		if criterion != "" {
			criteria[name] = criterion
		}
	}
	if len(criteria) == 0 {
		return "", nil, nil, fmt.Errorf("no configured criteria for available tiers")
	}
	var body evaluationRequest
	body.State.Request = text
	body.State.RequestTruncated = truncated
	body.Questions = map[string]choiceQuestion{"tier": {Type: "choice", Instructions: cfg.Instructions, Criteria: criteria}}
	encoded, errMarshal := json.Marshal(body)
	if errMarshal != nil {
		return "", nil, nil, errMarshal
	}
	requestCtx, cancel := context.WithTimeout(ctx, cfg.Timeout)
	defer cancel()
	req, errRequest := http.NewRequestWithContext(requestCtx, http.MethodPost, cfg.GatewayBaseURL+"/evaluation-model", bytes.NewReader(encoded))
	if errRequest != nil {
		return "", nil, nil, errRequest
	}
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("ai-gateway-protocol-version", "0.0.1")
	req.Header.Set("ai-gateway-auth-method", "api-key")
	req.Header.Set("ai-evaluation-model-specification-version", "4")
	req.Header.Set("ai-model-id", cfg.Model)
	response, errDo := (&http.Client{}).Do(req)
	if errDo != nil {
		return "", nil, nil, errDo
	}
	defer func() { _ = response.Body.Close() }()
	responseBody, errRead := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if errRead != nil {
		return "", nil, nil, errRead
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return "", nil, nil, fmt.Errorf("gateway returned HTTP %d", response.StatusCode)
	}
	var parsed evaluationResponse
	if errDecode := json.Unmarshal(responseBody, &parsed); errDecode != nil {
		return "", nil, nil, fmt.Errorf("decode gateway response: %w", errDecode)
	}
	answer, ok := parsed.Answers["tier"]
	if !ok || answer.Type != "choice" || strings.TrimSpace(answer.Choice) == "" {
		return "", nil, nil, fmt.Errorf("gateway returned no tier choice")
	}
	choice := strings.TrimSpace(answer.Choice)
	confidence, hasConfidence := parsed.ProviderMetadata.Typesafe.Confidence["tier"]
	var confidencePtr *float64
	if hasConfidence {
		confidencePtr = &confidence
	}
	cost := parseCost(parsed.ProviderMetadata.Gateway.Cost)
	return choice, confidencePtr, cost, nil
}

func parseCost(raw json.RawMessage) *float64 {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	var number float64
	if err := json.Unmarshal(raw, &number); err == nil {
		return &number
	}
	var text string
	if err := json.Unmarshal(raw, &text); err != nil {
		return nil
	}
	if _, err := fmt.Sscan(text, &number); err != nil {
		return nil
	}
	return &number
}

func gatewayKey() string {
	if value := strings.TrimSpace(os.Getenv("VERCEL_AI_GATEWAY_KEY")); value != "" {
		return value
	}
	return strings.TrimSpace(os.Getenv("AI_GATEWAY_API_KEY"))
}

func firstUserRequest(payload []byte) (string, bool) {
	var text string
	messages := gjson.GetBytes(payload, "messages")
	if messages.Exists() && messages.IsArray() {
		messages.ForEach(func(_, item gjson.Result) bool {
			if item.Get("role").String() == "user" {
				text = messageText(item.Get("content"))
				return text == ""
			}
			return true
		})
	}
	if text == "" {
		contents := gjson.GetBytes(payload, "contents")
		contents.ForEach(func(_, item gjson.Result) bool {
			if item.Get("role").String() == "user" {
				item.Get("parts").ForEach(func(_, part gjson.Result) bool {
					if text == "" {
						text = part.Get("text").String()
					}
					return text == ""
				})
			}
			return text == ""
		})
	}
	if text == "" {
		input := gjson.GetBytes(payload, "input")
		input.ForEach(func(_, item gjson.Result) bool {
			if item.Get("role").String() == "user" {
				text = messageText(item.Get("content"))
			}
			return text == ""
		})
	}
	return clamp(text)
}

func messageText(content gjson.Result) string {
	if content.Type == gjson.String {
		return content.String()
	}
	var parts []string
	content.ForEach(func(_, part gjson.Result) bool {
		if value := part.Get("text").String(); value != "" {
			parts = append(parts, value)
		}
		return true
	})
	return strings.Join(parts, " ")
}

func clamp(text string) (string, bool) {
	const head, tail = 3000, 1000
	if len(text) <= head+tail {
		return text, false
	}
	return text[:head] + "\n[... request truncated ...]\n" + text[len(text)-tail:], true
}

func shortSession(session string) string {
	if len(session) <= 32 {
		return session
	}
	return session[:32]
}

func okEnvelope(value any) ([]byte, error) {
	result, errMarshal := json.Marshal(value)
	if errMarshal != nil {
		return nil, errMarshal
	}
	return json.Marshal(envelope{OK: true, Result: result})
}

func errorEnvelope(code, message string) []byte {
	result, _ := json.Marshal(envelope{OK: false, Error: &envelopeError{Code: code, Message: message}})
	return result
}
