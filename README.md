# Ultimate Proxy

A fast, self-hosted LLM gateway that speaks the [Open Responses](https://www.openresponses.org/) API
and meters every request by **tenant**, **application** and **end-user email**.

See [docs/DESIGN.md](docs/DESIGN.md) for the full design and roadmap.

## What works today (phase 1)

- `POST /v1/responses`: JSON and SSE streaming, passing the Open Responses HTTP compliance tests.
- Providers:
  - `openai`: any upstream that already speaks Open Responses (OpenAI's Responses API and compatible
    servers). Events are relayed as-is.
  - `anthropic`: translated to and from the Messages API, covering text, images, PDFs, function tools,
    `tool_choice`, reasoning (extended thinking, replayable across turns) and prompt-cache usage.
- API keys per application. Each key belongs to one application and one tenant.
- End-user attribution from the `X-Proxy-User-Email` header, `metadata.user_email` or `safety_identifier`,
  accepted only from applications allowed to name their users.
- Usage events (tokens in/out, cached, cache writes, reasoning, latency, time to first token) written to
  Postgres in batches, off the request path.
- `GET /admin/usage` to answer "who used how many tokens", grouped by tenant, application, email, model
  or provider, optionally bucketed by hour or day.
- Prometheus metrics at `/metrics`.

Not built yet: WebSocket transport, `/v1/responses/compact`, `previous_response_id` for non-OpenAI
upstreams, background responses, cache-miss insights, budgets and rate limits. They are later phases in
the design doc.

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
| `group_by` | Comma-separated: `tenant`, `application`, `email`, `model`, `provider`, or `none`. Default `tenant`. |
| `from`, `to` | RFC 3339 times. Default: the last 24 hours. |
| `granularity` | `hour` or `day`. Default: one row per group. |
| `tenant_id`, `application_id`, `email`, `model`, `provider` | Filters. |

Revoke a key with `DELETE /admin/keys/<key id>`.

## Develop

```sh
go test ./...                                         # unit and integration tests (fake upstream)
TEST_DATABASE_URL=postgres://… go test ./internal/store  # also run the store tests against Postgres
go test -run x -bench Overhead ./internal/server      # proxy overhead vs. calling the upstream directly
scripts/compliance.sh <openresponses checkout>        # official compliance suite, both adapters
```

`cmd/fake-upstream` is a deterministic fake of the Anthropic Messages and OpenAI Responses APIs, so tests
and CI need no provider credentials.
