# Ultimate Proxy

A fast, self-hosted LLM gateway that speaks the [Open Responses](https://www.openresponses.org/) API
and meters every request by **tenant**, **application** and **end-user email**.

See [docs/DESIGN.md](docs/DESIGN.md) for the full design and roadmap.

## What works today (phases 1 and 2)

- `POST /v1/responses`: JSON and SSE streaming, passing the Open Responses HTTP compliance tests.
- Upstreams: any provider that speaks Open Responses (OpenAI's Responses API and compatible servers).
  Requests and events are relayed as-is; the proxy only reads the final usage.
- API keys per application. Each key belongs to one application and one tenant.
- End-user attribution from the `X-Proxy-User-Email` header, `metadata.user_email` or `safety_identifier`,
  accepted only from applications allowed to name their users.
- Usage events (tokens in/out, cached, cache writes, reasoning, latency, time to first token) written to
  Postgres in batches, off the request path.
- `GET /admin/usage` to answer "who used how many tokens", grouped by tenant, application, email, model
  or provider, optionally bucketed by hour or day.
- Prompt-cache diagnosis on every request: whether the cache hit, and if not, why (see below).
- Insights: problems the proxy notices in live traffic, per application and model, listed at
  `GET /admin/insights` and posted to a webhook or Slack when they open and resolve.
- Cost in USD from the prices in the config, on every usage event and in `/admin/usage`.
- Prometheus metrics at `/metrics`.

Not built yet: WebSocket transport, `/v1/responses/compact`, background responses, budgets and rate
limits. They are later phases in the design doc.

## Run it

```sh
cp config.example.yaml config.yaml    # edit providers and models
DATABASE_URL=postgres://… PROXY_ADMIN_TOKEN=secret go run ./cmd/ultimate-proxy -config config.yaml
```

Or `docker compose -f deploy/docker-compose.yml up` for the proxy plus Postgres.

Without `database_url`, the proxy keeps keys and usage in memory, which is handy for trying it out.

## Use it

Create a tenant, an application and a key:

```sh
ADMIN="Authorization: Bearer $PROXY_ADMIN_TOKEN"
curl -s localhost:8080/admin/tenants -H "$ADMIN" -d '{"name":"acme"}'
curl -s localhost:8080/admin/tenants/<tenant id>/applications -H "$ADMIN" -d '{"name":"support-bot"}'
curl -s -X POST localhost:8080/admin/applications/<app id>/keys -H "$ADMIN"   # returns the key once
```

Call the proxy with any Open Responses or OpenAI Responses client:

```sh
curl localhost:8080/v1/responses \
  -H "Authorization: Bearer up_…" \
  -H "X-Proxy-User-Email: alice@acme.com" \
  -d '{"model":"smart","input":"Hello!","stream":true}'
```

`model` is either an alias from the config or `<provider>/<upstream model>`.

Ask who used what:

```sh
curl -s "localhost:8080/admin/usage?group_by=tenant,application,email&granularity=day" -H "$ADMIN"
```

| Parameter | Meaning |
| --- | --- |
| `group_by` | Comma-separated: `tenant`, `application`, `email`, `model`, `provider`, `cache`, or `none`. Default `tenant`. |
| `from`, `to` | RFC 3339 times. Default: the last 24 hours. |
| `granularity` | `hour` or `day`. Default: one row per group. |
| `tenant_id`, `application_id`, `email`, `model`, `provider`, `cache_status` | Filters. |

Each row also carries `cost_usd` when the model has a price in the config.

Revoke a key with `DELETE /admin/keys/<key id>`.

## Prompt-cache insights

The proxy fingerprints the start of every prompt (instructions, tools, then each input item; only
hashes are kept) and compares it with what the same application sent recently. Each usage event gets a
`cache_status`:

| Status | Meaning |
| --- | --- |
| `hit` | The provider served part of the prompt from cache. |
| `miss_unexpected` | The same prefix was sent within `cache_ttl` but the provider still missed. |
| `miss_instructions_dynamic` | The instructions differ from the previous request only in numbers or ids (a timestamp, a date, a request id). |
| `miss_instructions_changed` | The instructions changed. |
| `miss_tools_reordered` | The same tools, in a different order or with keys in a different order. |
| `miss_tools_changed` | The tool list changed. |
| `miss_history_rewritten` | Earlier turns of the conversation changed (summarized, trimmed, re-serialized). |
| `miss_new_prefix` | Nothing similar was sent recently; a normal first request. |
| `miss_too_short` | The prompt is below the provider's minimum cacheable size. |
| `unknown` | The request uses `previous_response_id`, so the proxy can't see the prompt. |

`/admin/usage?group_by=application,cache` shows the mix per application. Every minute, rules look at
the last `window` of traffic per application and model and open an insight when a rate crosses its
threshold, then resolve it when the rate falls below half of it:

| Insight | Fires when |
| --- | --- |
| `cache_prefix_unstable` | Too many requests miss because the application keeps changing its prompt prefix. The insight says which change dominates and how to fix it. |
| `cache_unexpected_miss` | Too many repeated prefixes miss anyway, with the tokens and dollars that cost. |
| `error_rate` | Too many requests fail, with the most common error codes. |
| `truncation` | Too many responses end incomplete at `max_output_tokens`. |

```sh
curl -s "localhost:8080/admin/insights" -H "$ADMIN"                  # open insights
curl -s "localhost:8080/admin/insights?status=all" -H "$ADMIN"       # open and resolved
```

Set `insights.webhook_url` for a JSON POST (`{"event":"insight.opened","insight":{…}}`) or
`insights.slack_webhook_url` for a Slack message on every open and resolve. Prometheus gets
`ultimate_proxy_cache_requests_total{status}`, `ultimate_proxy_cache_missed_tokens_total`,
`ultimate_proxy_cost_usd_total` and `ultimate_proxy_insights_open{kind}`.

The fingerprint memory lives in each proxy process, so with several replicas each one judges only the
traffic it sees. That is fine for rates; a shared store can come later if needed.

## Develop

```sh
go test ./...                                         # unit and integration tests (fake upstream)
TEST_DATABASE_URL=postgres://… go test ./internal/store  # also run the store tests against Postgres
go test -run x -bench Overhead ./internal/server      # proxy overhead vs. calling the upstream directly
scripts/compliance.sh <openresponses checkout>        # official compliance suite
```

`cmd/fake-upstream` is a deterministic fake of the OpenAI Responses API, so tests
and CI need no provider credentials.
