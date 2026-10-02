// Command eval runs the stable tier-auto evaluation set against JEV.
// Usage: VERCEL_AI_GATEWAY_KEY=... go run ./eval.go
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
)

const instructions = "You are routing one coding-agent task to a model tier. Read the request and choose the cheapest tier that can complete it well. Judge the work the request actually implies, not how politely or verbosely it is written. A long paste with a small ask is still a small task; a short sentence asking for a redesign is not. When the request is ambiguous about scope, prefer the lower tier — the user can escalate, and over-routing every task defeats the purpose."

var criteria = map[string]string{
	"fast": "Trivial or mechanical work: a short factual question, a rename, a one-line edit, reading or summarising a file, running a simple command. No design judgement is required.",
	"balanced": "Ordinary development work: implement a well-specified change, fix a clearly-described bug, write focused tests, review a small diff. Some judgement is needed but the approach is evident.",
	"deep": "Multi-step work that needs sustained reasoning: debug a failure whose cause is unclear, design or refactor across several files, trace behaviour through unfamiliar code, or weigh competing approaches.",
	"frontier": "The hardest work: novel architecture, subtle concurrency or correctness bugs, security-sensitive reasoning, large ambiguous refactors, or anything where a wrong answer is expensive to discover later.",
}

func main() {
	key := strings.TrimSpace(os.Getenv("VERCEL_AI_GATEWAY_KEY"))
	if key == "" { key = strings.TrimSpace(os.Getenv("AI_GATEWAY_API_KEY")) }
	if key == "" { panic("VERCEL_AI_GATEWAY_KEY or AI_GATEWAY_API_KEY is required") }
	cases := []struct { prompt, want string }{
		{"what does the `cn` helper in lib/utils do?", "fast"},
		{"rename the variable `foo` to `bar` in src/app.ts", "fast"},
		{"add a unit test for the clampPrompt helper covering the truncation branch", "balanced"},
		{"reconnect loop drops the first frame after a relay failover, can't repro locally", "deep"},
		{"design the migration to a multi-region projector, no downtime, keep checkpoint ordering", "frontier"},
		{"review this auth token minting code for privilege escalation and replay attacks", "frontier"},
		{"I would be enormously grateful if, at your earliest convenience, you could kindly make the very small mechanical adjustment of changing only the copyright year in our footer, taking great care not to modify anything else whatsoever.", "fast"},
	}
	failed := false
	for _, testCase := range cases {
		choice, confidence, err := evaluate(key, testCase.prompt)
		if err != nil { fmt.Printf("ERROR %q: %v\n", testCase.prompt, err); failed = true; continue }
		status := "PASS"
		if choice != testCase.want { status = "FAIL"; failed = true }
		fmt.Printf("%s want=%-8s got=%-8s confidence=%s  %s\n", status, testCase.want, choice, confidence, testCase.prompt)
	}
	if failed { os.Exit(1) }
}

func evaluate(key, prompt string) (string, string, error) {
	payload := map[string]any{"state": map[string]any{"request": prompt, "requestTruncated": false}, "questions": map[string]any{"tier": map[string]any{"type": "choice", "instructions": instructions, "criteria": criteria}}}
	body, _ := json.Marshal(payload)
	req, err := http.NewRequest(http.MethodPost, "https://ai-gateway.vercel.sh/v4/ai/evaluation-model", bytes.NewReader(body))
	if err != nil { return "", "", err }
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("ai-gateway-protocol-version", "0.0.1")
	req.Header.Set("ai-gateway-auth-method", "api-key")
	req.Header.Set("ai-evaluation-model-specification-version", "4")
	req.Header.Set("ai-model-id", "typesafe-ai/jev")
	response, err := http.DefaultClient.Do(req)
	if err != nil { return "", "", err }
	defer func() { _ = response.Body.Close() }()
	raw, err := io.ReadAll(response.Body)
	if err != nil { return "", "", err }
	if response.StatusCode < 200 || response.StatusCode > 299 { return "", "", fmt.Errorf("HTTP %d: %s", response.StatusCode, raw) }
	var parsed struct { Answers map[string]struct { Choice string `json:"choice"` } `json:"answers"`; ProviderMetadata struct { Typesafe struct { Confidence map[string]float64 `json:"confidence"` } `json:"typesafe"` } `json:"providerMetadata"` }
	if err := json.Unmarshal(raw, &parsed); err != nil { return "", "", err }
	return parsed.Answers["tier"].Choice, fmt.Sprintf("%.2f", parsed.ProviderMetadata.Typesafe.Confidence["tier"]), nil
}
