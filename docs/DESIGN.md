# Ultimate Proxy: Design

Status: phases 1 (core proxy) and 2 (cache insights, cost accounting) built; later phases proposed. Spec target: Open Responses 2026-04-24.

Scope note (2026-09-29): upstreams are limited to providers that speak Open Responses (OpenAI and
compatible servers) for now. The Anthropic adapter described below was built in phase 1 and then
removed; git history keeps it if Anthropic support comes back.

## 1. What it is

A self-hosted gateway that speaks the [Open Responses](https://www.openresponses.org/) API to clients and
talks to any upstream model provider (OpenAI, Anthropic, Gemini, Bedrock, Azure, vLLM/Ollama).
Every request is attributed to a **tenant**, an **application** and an **end-user email**, metered in
tokens and dollars, and inspected for problems such as prompt-cache prefix misses.

```
 clients (SDKs, agents)                        upstreams
 ───────────────────────►┌──────────────────┐─────────────► OpenAI Responses (passthrough)
  POST /v1/responses     │  ultimate-proxy  │─────────────► Anthropic Messages
  SSE / WebSocket        │  (stateless Go)  │─────────────► Gemini / Bedrock / Azure
  POST /v1/responses/    └──┬───────┬───────┘─────────────► OpenAI-compatible (vLLM, Ollama)
       compact              │       │ async, batched
                     Redis (hot)   Postgres (config, usage, response store)
```

## 2. Defaults I'm picking

| Choice | Pick | Why |
| --- | --- | --- |
| Language | **Go** | Cheap goroutine-per-stream, `net/http` streaming without buffering, single static binary, predictable GC at gateway loads. Rust would shave a little more latency but slows iteration; the upstream model dominates latency anyway. |
| Config + usage storage | **Postgres** | Tenants, apps, keys, prices, and a time-partitioned `usage_events` table with rollups. One dependency to run. |
| Hot state | **Redis** | Rate-limit/budget counters, cache-affinity map, `previous_response_id` state with TTL. Optional in single-node mode (in-memory fallback). |
| Analytics at scale | **ClickHouse later** | Same event schema; swap the sink when Postgres rollups stop being enough. Not in v1. |
| Deploy | Docker image + docker-compose (proxy, Postgres, Redis) | Helm chart later. |

## 3. Requirements mapping

### 3.1 Open Responses support

- `POST /v1/responses` non-streaming (JSON) and streaming (SSE, `event:` equals `type`, terminal `[DONE]`).
- `POST /v1/responses/compact`.
- WebSocket transport on `/v1/responses` (sequential turns, connection-local `previous_response_id` cache,
  eviction on failed continuation, 60-minute limit, `previous_response_not_found` and
  `websocket_connection_limit_reached` errors).
- `previous_response_id` for **every** provider: the proxy owns the response store, because most
  upstreams (Anthropic, Gemini, vLLM) have no server-side state. `store=false` never touches disk.
- Spec error envelope and types (`invalid_request`, `not_found`, `too_many_requests`, `server_error`, `model_error`).
- **Adapters** translate Open Responses ⇄ provider-native APIs, including semantic streaming events
  (`response.output_item.added`, `response.output_text.delta`, `response.function_call_arguments.delta`,
  reasoning events, `response.completed`, ...). OpenAI Responses is a near-passthrough.
- The official compliance suite (`bun run test:compliance --base-url ...`) runs in CI against the proxy
  with a mock upstream and against real providers nightly.

### 3.2 Tenant / application / email usage tracking

Identity model:

- **Tenant**: the billing/organizational unit. Owns applications, budgets, provider keys (BYOK optional).
- **Application**: identified by a proxy-issued **virtual key** (`up_live_...`, stored as a hash). Every key
  belongs to exactly one tenant and one application, so tenant and app come from the key alone.
- **Email (end user)**: resolved in this order:
  1. Verified JWT (OIDC) passed in `X-Proxy-User-Token`: email claim.
  2. `X-Proxy-User-Email` header, **only** if the app is flagged `can_assert_users` (a trusted backend).
  3. Open Responses `safety_identifier` or `metadata.user_email`, same trust rule.
  4. Otherwise `unknown`.

Every request produces one `usage_event`:

```
ts, request_id, tenant_id, app_id, user_email, model_alias, provider, provider_model, upstream_key_id,
input_tokens, cached_input_tokens, cache_write_tokens, output_tokens, reasoning_tokens,
cost_usd, status, error_type, ttft_ms, latency_ms, stream, prefix_hash, prompt_cache_key, tags(jsonb)
```

Tokens come from the upstream's own usage report (terminal `response.completed` event or JSON body),
never estimated when the provider reports them. If a stream is cut off, the proxy records what it saw
and marks the event `partial`.

Queries it answers, via `GET /admin/usage?group_by=tenant|app|email|model&from=&to=&granularity=`:
tokens and cost by email, by tenant, by application, by model, over any time window. Minute/hour/day
rollups are maintained incrementally so dashboards stay cheap.

### 3.3 Fast

Budget: **< 1 ms p50 / < 3 ms p99 added latency** on the hot path, excluding upstream time, and no added
time-to-first-token for streams.

- No blocking I/O before the upstream call except what's unavoidable:
  - Virtual-key lookup from an in-process cache (refreshed via Postgres `LISTEN/NOTIFY`).
  - Rate limits and budgets checked against locally pre-allocated token buckets that sync with Redis in
    the background (bounded over-spend, zero round-trips on the common path).
- Streaming is event-by-event translation with flush after every event; never buffer a full response.
- Usage events go into a lock-free channel and are batch-inserted (`COPY`) every 250 ms or 1,000 events.
  If Postgres is down, events spill to a local disk queue instead of slowing requests.
- Pooled HTTP/2 connections per upstream, tuned keep-alives, `jsoniter`/`sonic`-class JSON with
  zero-copy passthrough when the adapter is the identity (OpenAI Responses).
- Stateless proxy nodes; scale horizontally behind any L4/L7 load balancer.
- Benchmarked in CI (k6 against a mock upstream) so regressions fail the build.

### 3.4 Surfacing problems like prefix-cache misses

The proxy sees both the prompt and the provider's `cached_tokens`, so it can tell when caching *should*
have worked and didn't, and point at why.

**Prefix fingerprinting.** For each request the proxy computes rolling hashes at item boundaries over the
canonicalized prefix: `instructions`, then `tools` (as sent, order preserved), then each input item.
Result: `[h_instr, h_tools, h_item0, h_item1, ...]`. Only hashes are stored, never content.

**Expected-hit detection.** Keep a short-lived map (Redis, TTL = provider cache TTL, e.g. 5 min for
Anthropic default, ~5-10 min for OpenAI) of `(app, provider, model, prefix_hash_at_depth_k) → last_seen`.
If a request's prefix of at least the provider's minimum cacheable size (e.g. 1,024 tokens) was seen
within the TTL, a hit is **expected**. If the upstream reports `cached_tokens == 0` (or far below the
matched prefix), it's an **unexpected miss**.

**Diagnosis**, attached to each miss and aggregated per app:

| Signal | Likely cause shown to the user |
| --- | --- |
| Hashes diverge at `h_instr` between consecutive calls | System prompt changes per request (timestamp, request id, user name in the prompt) |
| Diverge at `h_tools` but the tool set is the same | Tool order or JSON key order is non-deterministic |
| Diverge at an early `h_item` | History is being rewritten/truncated at the front |
| Same prefix, different upstream key/region | Load balancing broke cache affinity |
| Same prefix, gap > TTL | Traffic too sparse for the cache TTL; consider longer TTL or warmers |
| Prefix below provider minimum | Prompt too short to be cached |
| Anthropic with no `cache_control` breakpoints | Caching not enabled; proxy can auto-insert breakpoints |

**Insights and alerts.** A rules engine evaluates rolling windows per app/tenant/model and opens
"insights" (deduplicated, with first-seen/last-seen and examples):

- Cache hit ratio below threshold, or unexpected-miss rate above threshold.
- Error-rate spike by type, upstream 429s, upstream 5xx per provider.
- TTFT or latency p95 regression versus the trailing 7-day baseline.
- `incomplete` responses from `max_output_tokens`, truncation events, context-overflow errors.
- Malformed tool-call arguments (JSON that fails the tool's schema).
- Cost or token spike versus baseline; budget at 80% / 100%.

Delivered via webhook, Slack, and email, plus Prometheus metrics for existing alerting.

**As built in phase 2.** The expected-hit map lives in each proxy process rather than Redis; rates per
replica are good enough, and a shared store can be added when cache-affinity routing (phase 3) needs
it. Built rules: unstable prefix (with the dominant cause), unexpected miss (with missed tokens and
dollars), error rate and truncation, over a sliding window with open/resolve hysteresis (open at the
threshold, resolve below half of it, or when traffic stops). Latency and cost-spike baselines, the
key/region affinity signal, and email delivery are not built yet. Prices are an append-only
`model_prices` table managed through `/admin/prices` rather than a bundled catalog or config file:
each row has an `effective_from`, and each request is costed with the price in effect when it ran.

## 4. Additional features I'd propose

Grouped by value. **Bold** ones I'd consider near-essential for a proxy people will call "ultimate".

**Control and cost**
1. **Budgets and quotas** per tenant / app / email, in tokens or dollars, soft (alert) and hard (block) limits.
2. **Rate limits** (RPM, TPM, concurrent streams) at each level.
3. **Cost accounting** from a versioned price table, including cache-read/cache-write and reasoning pricing.
4. Chargeback / showback exports (CSV, S3, webhook) per tenant per month.

**Routing and reliability**
5. **Model aliases** (`fast`, `smart`, `tenant-default`) mapped to provider models, per tenant.
6. **Fallback chains, retries with backoff, and circuit breakers** per upstream.
7. **Cache-affinity routing**: pin a `prompt_cache_key` / prefix hash to the same upstream key and region.
8. Load balancing across multiple provider keys/accounts and regions; data-residency constraints per tenant.
9. Canary / A-B routing and shadow traffic to evaluate a new model on real prompts.

**Compatibility**
10. **Extra front doors**: accept OpenAI Chat Completions and Anthropic Messages too, so existing SDKs work
    unchanged and still get tracked.
11. `background: true` jobs with a queue, and batch APIs routed to providers' discounted batch endpoints.
12. MCP / hosted tools gateway (proxy-executed tools following the spec's internally-hosted tool model).

**Security and governance**
13. **Provider-key vault** (encrypted at rest, BYOK per tenant, rotation without redeploys).
14. OIDC/SSO for the admin UI, RBAC (tenant admin, app owner, viewer), audit log of admin actions.
15. Per-tenant model allowlists and parameter caps (max tokens, allowed tools).
16. Guardrails hooks: PII redaction, prompt-injection detection, content policy, pluggable pre/post filters.
17. Request/response logging that is opt-in per app, honors `store=false` / zero data retention, with retention policies.

**Visibility**
18. **OpenTelemetry traces** using the GenAI semantic conventions, Prometheus metrics, structured logs.
19. **Admin UI**: usage by tenant/app/email, cost, cache health, insights, key management.
20. Request explorer with replay against another model.

**Performance extras**
21. Exact-match response cache (opt-in, for deterministic requests) and optional semantic cache.
22. Automatic Anthropic `cache_control` breakpoint insertion.

## 5. Proposed build order

| Phase | Scope |
| --- | --- |
| **1. Core proxy** | Go service, `/v1/responses` JSON + SSE, adapters for OpenAI Responses and Anthropic Messages, virtual keys, tenant/app/email attribution, usage events to Postgres, usage API, Prometheus metrics, compliance suite in CI. |
| **2. Cache insights** | Prefix fingerprinting, expected-hit/unexpected-miss detection, diagnosis, insights + webhook/Slack alerts, cost accounting. |
| **3. Control** | Budgets, rate limits, model aliases, fallbacks/retries/circuit breakers, cache-affinity routing. |
| **4. Full spec + UI** | `previous_response_id` store, `/responses/compact`, WebSocket transport, admin UI, Chat Completions / Messages front doors. |
| **5. Governance** | Key vault/BYOK, SSO/RBAC/audit, guardrails, logging policies. |

## 6. Repository layout (planned)

```
cmd/ultimate-proxy/      main
internal/api/            HTTP, SSE, WebSocket handlers, spec types
internal/adapters/       openai, anthropic, gemini, bedrock, openaicompat
internal/identity/       virtual keys, JWT, user resolution
internal/meter/          usage events, batch writer, rollups
internal/insights/       prefix hashing, miss detection, rules, notifiers
internal/limits/         rate limits, budgets
internal/router/         aliases, fallbacks, affinity
internal/store/          postgres, redis
migrations/
docker-compose.yml  (proxy, Postgres, fake upstream, dashboard)
web/               (dashboard)
```
