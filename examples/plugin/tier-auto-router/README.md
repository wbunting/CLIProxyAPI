# JEV `tier-auto` Router (RPC process plugin)

`tier-auto` is a virtual model alias. On the first request of a conversation, this RPC process plugin calls JEV through the Vercel AI Gateway and selects the cheapest available `tier-*` alias. It keeps that decision for the session-affinity TTL, including the transition from a first-turn message hash to the later hash that includes the first assistant reply.

The plugin uses the process RPC transport (`.rpc` executable), not Go `.so` plugins. The plugin process is isolated from the server Go toolchain and remains stable across dependency upgrades.

## Build and install

```bash
cd examples/plugin/tier-auto-router/go
go build -o ../../../../plugins/tier-auto-router.rpc .
chmod +x ../../../../plugins/tier-auto-router.rpc
```

Configure the gateway credential in the service environment (preferred), using `VERCEL_AI_GATEWAY_KEY`; `AI_GATEWAY_API_KEY` is accepted as a fallback. Do not place credentials in the plugin YAML.

```yaml
plugins:
  enabled: true
  dir: /absolute/path/to/plugins
  configs:
    tier-auto-router:
      enabled: true
      priority: 100
      model: typesafe-ai/jev
      gateway_base_url: https://ai-gateway.vercel.sh/v4/ai
      timeout: 3s
      ttl: 2h
      fallback_tier: tier-balanced
      instructions: >-
        You are routing one coding-agent task to a model tier. Read the request and choose the cheapest tier that can complete it well. Judge the work the request actually implies, not how politely or verbosely it is written. A long paste with a small ask is still a small task; a short sentence asking for a redesign is not. When the request is ambiguous about scope, prefer the lower tier — the user can escalate, and over-routing every task defeats the purpose.
      criteria:
        fast: Trivial or mechanical work: a short factual question, a rename, a one-line edit, reading or summarising a file, running a simple command. No design judgement is required.
        balanced: Ordinary development work: implement a well-specified change, fix a clearly-described bug, write focused tests, review a small diff. Some judgement is needed but the approach is evident.
        deep: Multi-step work that needs sustained reasoning: debug a failure whose cause is unclear, design or refactor across several files, trace behaviour through unfamiliar code, or weigh competing approaches.
        frontier: The hardest work: novel architecture, subtle concurrency or correctness bugs, security-sensitive reasoning, large ambiguous refactors, or anything where a wrong answer is expensive to discover later.
```

The policy lives in plugin configuration so tuning it does not need a binary rebuild. Only `tier-*` aliases currently visible from live credentials are offered to JEV. If the gateway, credential, response, or candidate set is unusable, the plugin fails open to `tier-balanced` when that alias exists.

## Catalog registration

Add `tier-auto` as an alias wherever the tier aliases are exposed. For example, under each configured provider that should advertise it, create a `tier-auto` alias that maps to a harmless existing tier model. The router replaces it before normal model resolution, so its underlying mapping is never executed for a correctly loaded plugin.

## Observability

Responses selected by the router expose:

- `X-CPA-Resolved-Model`
- `X-CPA-Route-Confidence` (when supplied by JEV)
- `X-CPA-Route-Cache-Hit`

The server exposes these through CORS. Router logs include a truncated session identity, chosen tier, cache outcome, JEV latency, confidence, and gateway cost.

## Tests and live evaluation

```bash
cd examples/plugin/tier-auto-router/go
go test ./...

# Load ~/.local/src/slateo/.env only for this manual, live evaluation.
set -a; source ~/.local/src/slateo/.env; set +a
cd .. && go run ./eval.go
```

The eval set includes the deliberately polite copyright-year padding case.
