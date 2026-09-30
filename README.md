# Ultimate Proxy

A fast, self-hosted LLM gateway that speaks the [Open Responses](https://www.openresponses.org/) API
and meters every request by **tenant**, **application** and **end-user email**.

See [docs/DESIGN.md](docs/DESIGN.md) for the full design and roadmap.

## What works today (phases 1 and 2)

- `POST /v1/responses`: JSON and SSE streaming, passing the Open Responses HTTP compliance tests.
- Upstreams: any provider that speaks Open Responses (OpenAI's Responses API and compatible servers),
  relayed as-is (the proxy only reads the final usage), and any server that only speaks Chat
  Completions (vLLM, Ollama, llama.cpp, LiteLLM, Groq, Together, OpenRouter and others), translated
  with provider `type: chat_completions`.
  Translation covers text, images, files as data, function tools and `tool_choice`, structured
  output (`text.format`), reasoning effort and streamed reasoning, and cached-token usage. It
  rejects with a 400 what Chat Completions cannot express: `previous_response_id`, hosted tools and
  item references. The output limit is sent as `max_tokens`, which every such server accepts.
- Several keys or servers per provider, retries, circuit breakers and fallback models, with each
  conversation kept on the deployment that has it cached (see "Retries, fallbacks and several keys").
- API keys that expire and are limited to some models, and short-lived tokens for agents on users'
  devices.
- Rate limits per tenant, application or end user, in requests or tokens per minute, shared across
  replicas through Valkey (see "Rate limits").
- Callers authenticate with the proxy's own API keys (one per application and tenant), with access
  tokens from your own identity provider (see "Using your own identity provider"), or both.
- End-user attribution from the `X-Proxy-User-Email` header, `metadata.user_email` or `safety_identifier`,
  accepted only from applications allowed to name their users.
- Usage events (tokens in/out, cached, cache writes, reasoning, latency, time to first token) written to
  ClickHouse in batches, off the request path. Tenants, keys, prices and insights stay in Postgres.
- `GET /admin/usage` to answer "who used how many tokens", grouped by tenant, application, email, model
  or provider, optionally bucketed by hour or day.
- Prompt-cache diagnosis on every request: whether the cache hit, and if not, why (see below).
- Insights: problems the proxy notices in live traffic, per application and model, listed at
  `GET /admin/insights` and posted to a webhook or Slack when they open and resolve.
- Cost in USD from a versioned prices table, on every usage event and in `/admin/usage`.
- Prometheus metrics at `/metrics`.

Not built yet: WebSocket transport, `/v1/responses/compact` and background responses. They are
later phases in the design doc.

## Run it

Everything runs with Docker Compose: the proxy, Postgres, ClickHouse, the dashboard, and a free fake model
(`fake-gpt`) to try things without a provider key.

```sh
cp .env.example .env        # optional: change the admin token, add OPENAI_API_KEY
docker compose up -d --build
scripts/demo-traffic.sh     # optional: sample tenants, traffic and cache problems
```

| What | Where |
| --- | --- |
| Dashboard | http://localhost:3000, sign in with your identity provider (see [Signing in](#signing-in)) or `PROXY_ADMIN_TOKEN` (default `dev-admin-token`) |
| Proxy API | http://localhost:8080, demo key `up_demo_key` for the `demo/playground` application |

The proxy config for the stack is `deploy/compose/config.yaml`: models `fake-gpt` (free), `smart` and
`fast` (OpenAI, once `OPENAI_API_KEY` is set). Tenants, keys and prices live in the `pgdata` volume and the
request log in the `chdata` volume; `docker compose down -v` wipes both. `POSTGRES_PASSWORD` and
`CLICKHOUSE_PASSWORD` only apply when their volume is first created.

Without Docker: `go run ./cmd/ultimate-proxy -config config.yaml` (see `config.example.yaml`; without
`database_url` everything is kept in memory, and without `clickhouse_url` usage stays in Postgres), and `cd web && npm install && npm run dev` for the
dashboard on http://localhost:5173, which forwards `/admin` to `localhost:8080`.

### Where data lives

| Data | Store |
| --- | --- |
| Tenants, applications, API keys, prices, insights | Postgres (`database_url`) |
| Request log: one usage event per request (90 days by default) and hourly totals (forever), behind every `/admin/usage` query | ClickHouse (`clickhouse_url`) |

In ClickHouse, each request's raw event is kept for `usage.retention_days` (default 90; 0 keeps them
forever) and then deleted. Every insert also adds to an hourly rollup (`usage_hourly`: requests, tokens
and cost per hour, tenant, application, email, model, provider and cache status), which is kept forever.
`/admin/usage` reads whole hours from the rollup and the partial hours at the ends of the range from raw
events, so results are exact while raw events exist, long ranges stay fast, and ranges older than the
retention still work (to the hour).

ClickHouse is optional: leave `clickhouse_url` empty and usage events go to Postgres as before, kept
forever with no rollup. To move events already in Postgres into ClickHouse, run this once in ClickHouse
(for example with `docker compose exec clickhouse clickhouse-client -u proxy --password clickhouse -d proxy`);
the two `usage_events` tables have the same columns in the same order. Events older than the retention
go into the hourly rollup only.

```sql
INSERT INTO usage_events
SELECT * FROM postgresql('postgres:5432', 'proxy', 'usage_events', 'postgres', '<POSTGRES_PASSWORD>');
```

## Dashboard

The dashboard (`web/`: React, Tailwind CSS and shadcn/ui components, light theme, square edges, monospace type) is a front end for the admin API. `npx shadcn add <component>` works there to add more components.

- **Overview**: requests, tokens, cost and cache hit rate over 24 hours, 7 days or 30 days, with the
  top applications and users.
- **Usage**: group by tenant, application, email, model, provider or cache status, filter, export CSV.
- **Prompt cache**: each application's hit rate and why its requests miss.
- **Insights**: open and resolved problems with their cause and fix.
- **Prices**: prices in effect and their history; add a new price.
- **Tenants & keys**: create tenants, applications and keys; revoke keys.

## Signing in

The dashboard and `/admin` accept two kinds of sign-in, set under `admin:` in the config (or the
`OIDC_*` variables in `.env` for the compose stack):

- **Single sign-on** with any OpenID Connect provider: Google, Microsoft Entra ID, Okta, Auth0,
  Keycloak, Authentik, Dex, GitLab and others. Register the proxy as a web application at the
  provider with the redirect URI `<dashboard origin>/admin/auth/callback` (for the compose stack,
  `http://localhost:3000/admin/auth/callback`), then set:

  ```sh
  OIDC_ISSUER=https://accounts.google.com      # the provider's issuer URL
  OIDC_CLIENT_ID=...
  OIDC_CLIENT_SECRET=...
  OIDC_REDIRECT_URL=https://proxy.example.com/admin/auth/callback
  OIDC_DISPLAY_NAME=Google                     # the button reads "Continue with Google"
  OIDC_ALLOWED_DOMAINS=example.com             # and/or OIDC_ALLOWED_EMAILS, OIDC_ALLOWED_GROUPS
  ADMIN_SESSION_SECRET=$(openssl rand -base64 32)
  ```

  Only people on an allow list get in: an email in `allowed_emails`, an email at a domain in
  `allowed_domains`, or a group in `allowed_groups` (read from the ID token's `groups` claim, or
  `groups_claim`; some providers need an extra scope in `OIDC_SCOPES` to send it). An email the
  provider marks unverified is ignored. The proxy uses the authorization code flow with PKCE, checks
  state and nonce, and verifies the ID token against the provider's keys.

  A sign-in becomes a signed, HttpOnly session cookie that lasts `session_ttl` (12 hours by
  default). The allow lists are checked on every request, so taking someone off a list and
  restarting the proxy ends their session. Set `session_secret` so sessions survive restarts and
  work across several proxies.

- **The break-glass token**, `admin.token` (`PROXY_ADMIN_TOKEN`): a bearer token for scripts and
  for when the provider is down. The dashboard offers it under the SSO button. Set it empty to turn
  it off once single sign-on works.

## Using your own identity provider

API callers can send an access token (a JWT) from the identity provider the organization already
runs, instead of a proxy API key: Keycloak, Microsoft Entra ID, Okta, Auth0, Authentik and others.
Tenant, application and end user are read from the token's claims, and appear in the dashboard the
first time a token names them. Nothing has to be created in the proxy first.

```yaml
auth:
  api_keys: true          # keep accepting up_ keys too (the default)
  jwt:
    - issuer: https://keycloak.example.com/realms/acme   # must equal the tokens' iss exactly
      audience: ultimate-proxy   # required: tokens issued for other services are refused
      tenant: acme               # a fixed tenant for this issuer, or:
      claims:
        tenant: org_id           # the claim naming the tenant (wins over the fixed one)
        app: azp                 # default: azp, then client_id
        user: email              # default: email
        groups: groups           # default: groups
        models: llm_models       # optional: a claim listing the models the caller may use
      group_models:              # optional: models per group, for providers without a custom claim
        ml-team: [smart, openai/*]
```

The proxy fetches the issuer's signing keys from its discovery document (or `jwks_url`) and checks
the signature, issuer, audience and expiry of every token. Only asymmetric algorithms (RS, PS, ES,
EdDSA) are accepted. A verified token is cached until it expires, for at most five minutes, so
only the first request with a token pays for the signature check. List several issuers to serve
several organizations, for example one per Keycloak realm.

Allowed models are names or `<provider>/<model>`, optionally ending in `*`. A request for any
other model gets `403 model_not_allowed`, and `GET /v1/models` lists only the allowed ones. When
neither `claims.models` nor `group_models` is set, every model is allowed.

With Keycloak, add an *Audience* mapper to the client so its access tokens carry
`aud: ultimate-proxy`, and a *Group Membership* mapper (with "Full group path" off) for `groups`.

Usage events record how each request authenticated (`auth_method`: `api_key`, `jwt` or
`proxy_token`) and the key ID, the token's `sub`, or the minted token's ID as `subject`.

## Agents on users' machines and in browsers

Code that runs on a user's device should never hold a provider key or a long-lived proxy key. Two
ways to give it a credential that expires in minutes:

- **The user's own access token.** A desktop or CLI agent signs the user in with your identity
  provider (authorization code with PKCE, or the device code flow for a CLI) and sends the access
  token to the proxy, which accepts it as described above.
- **A token minted by the proxy.** Your backend, holding an API key, or an agent holding the user's
  access token, asks for a short-lived token and hands it to the client:

  ```sh
  curl -s localhost:8080/v1/tokens -H "Authorization: Bearer up_…" \
    -d '{"user":"alice@acme.com","models":["fast"],"expires_in":900}'
  # {"id":"tok_…","token":"upt_…","expires_at":"…","user":"alice@acme.com","models":["fast"]}
  ```

  A minted token can only narrow what its minter may do: the same tenant and application, the
  minter's own user (or, from an API key allowed to name users, the one it names), a subset of its
  models, and at most `expires_in` seconds (60 to 86400, default 900), never past the minter's own
  token. The caller cannot change its user, and it cannot mint further tokens. Revoke one early
  with `DELETE /admin/tokens/<id>`; every proxy stops accepting it within 30 seconds, and the one
  that handled the call at once. Only a hash of the token is stored.

For pages that call the proxy straight from a browser, list their origins:

```yaml
auth:
  cors_origins: [https://app.example.com]   # or "*"; empty (the default) allows none
```

## Use it

Create a tenant, an application and a key:

```sh
ADMIN="Authorization: Bearer $PROXY_ADMIN_TOKEN"
curl -s localhost:8080/admin/tenants -H "$ADMIN" -d '{"name":"acme"}'
curl -s localhost:8080/admin/tenants/<tenant id>/applications -H "$ADMIN" -d '{"name":"support-bot"}'
curl -s -X POST localhost:8080/admin/applications/<app id>/keys -H "$ADMIN"   # returns the key once
```

A key can expire and be limited to some models. Both are optional, and can be changed later with
`PATCH /admin/keys/<key id>` (send `null` to clear one):

```sh
curl -s -X POST localhost:8080/admin/applications/<app id>/keys -H "$ADMIN" \
  -d '{"expires_in": 2592000, "allowed_models": ["smart", "openai/*"]}'   # or "expires_at": "2027-01-01T00:00:00Z"
```

An expired key gets `401 api_key_expired`, on time even though keys are cached for 30 seconds. A
model outside the list gets `403 model_not_allowed` and is left out of `GET /v1/models`.

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

Each row also carries `cost_usd` when the model had a price at the time of the request.

## Retries, fallbacks and several keys

A provider can have several `deployments`: API keys (for example different OpenAI projects) or
servers (for example vLLM replicas). New requests are spread by `weight`. A model can list
`fallbacks`, other aliases or `<provider>/<model>`, tried in order. See `config.example.yaml`.

Each request tries the picked deployment, then the provider's other healthy deployments, then each
fallback, up to `routing.max_attempts` calls:

| Upstream answer | What happens |
|---|---|
| 429 | The deployment rests for `Retry-After` (or `routing.cooldown`), and the next one is tried at once. |
| 5xx, connection error, header timeout | Backoff with jitter, then the next deployment (or the same one if it is the only one). Five in a row open the deployment's circuit breaker for `open_for`; then one request probes it. |
| 401, 403 | The deployment's key is logged as bad and kept out for `open_for`; the next one is tried. |
| 400, 404, anything after streaming started | Returned to the client, never retried. |

Retries only happen before the first byte reaches the client, so a stream is never replayed. A
request with `previous_response_id` or encrypted reasoning stays on its provider, because that
state lives with the upstream account. Fallbacks a credential may not use are skipped. The usage
log records the provider, upstream model and `deployment` that served each request and how many
`attempts` it took, and the cost uses the price of what served it. Health is per proxy process.
Metrics: `ultimate_proxy_upstream_attempts_total`, `ultimate_proxy_fallbacks_total` and
`ultimate_proxy_breaker_open`.

**Stickiness.** Prompt caches are per deployment: per vLLM or llama.cpp replica, per OpenAI project.
So when a provider has several deployments, each conversation keeps going to the one that served its
last turn, for the provider's `cache_ttl` (default `insights.cache_ttl`, 5 minutes; longer suits
vLLM). A conversation is, first match wins within an application and model: the
`X-Proxy-Session-Id` header, `prompt_cache_key`, or the instructions and input up to the first user
message. A request with `previous_response_id` goes to the deployment that produced that response.
If the sticky deployment is resting or its breaker is open, the conversation moves once and sticks
to the new one. `sticky: false` on a provider turns it off. The session map is per process for now.

## Rate limits

Rules live in the database and apply to a tenant, one of its applications, or end users: `user` is an
email, or `*` to give every user their own allowance. `rpm` counts requests and `tpm` counts input
plus output tokens, both over a sliding minute.

```sh
# 600 requests a minute for the whole tenant, and 20 a minute for each user of one application.
curl -s -X POST localhost:8080/admin/limits -H "$ADMIN" -d '{"tenant_id": "<tenant id>", "kind": "rpm", "amount": 600}'
curl -s -X POST localhost:8080/admin/limits -H "$ADMIN" \
  -d '{"tenant_id": "<tenant id>", "application_id": "<app id>", "user": "*", "kind": "rpm", "amount": 20}'
curl -s localhost:8080/admin/limits -H "$ADMIN"                       # list; PATCH or DELETE /admin/limits/<id>
curl -s "localhost:8080/admin/limits/status?user=ann@example.com" -H "$ADMIN"   # current use
```

A refused request gets `429 rate_limit_exceeded` with `Retry-After` and a message naming the scope,
and is logged in usage with status `rejected`. Every response carries OpenAI-style
`x-ratelimit-limit-requests`, `x-ratelimit-remaining-requests` and `x-ratelimit-reset-requests` (and
the `-tokens` set) for the tightest rule, so SDKs back off on their own. Token limits are checked
before a request and charged after it, so a request already in flight can overshoot by its own size.

Counters live in Valkey when `redis_url` is set (the compose stack runs one), and every replica shares
them with one round trip per request. Without it each proxy counts on its own, so set it when running
more than one. If Valkey is unreachable, requests are let through and
`ultimate_proxy_limiter_errors_total` counts it; `limits.fail_closed: true` refuses them instead.
Rules are cached in each proxy and reread every 10 seconds, so the database is never on the request
path.

### Budgets

Budgets are rules of kind `budget_usd` (spend, costed with the prices table) or `budget_tokens`
(input plus output tokens) over a calendar `period`: `day`, `week` (from Monday) or `month`, in UTC.
They apply to the same scopes as rate limits.

```sh
# $500 a month for the tenant; 200k tokens a day for each user of one application, alerting only.
curl -s -X POST localhost:8080/admin/limits -H "$ADMIN" \
  -d '{"tenant_id": "<tenant id>", "kind": "budget_usd", "amount": 500, "period": "month"}'
curl -s -X POST localhost:8080/admin/limits -H "$ADMIN" -d '{"tenant_id": "<tenant id>",
  "application_id": "<app id>", "user": "*", "kind": "budget_tokens", "amount": 200000, "period": "day", "enforcement": "soft"}'
```

A `hard` budget (the default) refuses requests once it is used up, with `429 budget_exceeded` and a
`Retry-After` that runs to the start of the next period. A `soft` budget never refuses. Both open a
`budget_threshold` insight when they pass 80%, raise it to critical at 100% (webhook event
`insight.escalated`), and resolve it when the period ends. Switch between them with
`PATCH /admin/limits/<id> {"enforcement": "soft"}`.

Like token limits, budgets are charged after each response, so requests already running can take a
budget a little past 100%. The counters in Valkey are only a cache: each proxy rebuilds them from the
usage log at startup, every hour, and when a budget is created, so a budget added mid-month starts from
what was already spent and a Valkey restart loses nothing.

## Prices

Prices are rows in the `model_prices` table, in USD per million tokens. They are never edited: when a
provider changes its price, add a new row, and it takes over from `effective_from` (default now).
Each request's cost is computed with the price in effect when it was made and stored on its usage
event, so past costs don't change.

```sh
curl -s localhost:8080/admin/prices -H "$ADMIN" \
  -d '{"model":"gpt-5","input":1.25,"cached_input":0.125,"output":10}'
curl -s "localhost:8080/admin/prices?current=true" -H "$ADMIN"   # prices in effect now
curl -s localhost:8080/admin/prices -H "$ADMIN"                  # full history
```

`model` matches a client-facing alias, an upstream model name, or `<provider>/<upstream model>`.
`cached_input` and `cache_write` default to `input`. Each replica reloads the table every 30 seconds.

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
| `rerouted` | The deployment that had the conversation cached was unavailable, so it moved (see below). |

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
`insights.slack_webhook_url` for a Slack message on every open and resolve (and when a budget
insight turns critical, `insight.escalated`). Prometheus gets
`ultimate_proxy_cache_requests_total{status}`, `ultimate_proxy_cache_missed_tokens_total`,
`ultimate_proxy_cost_usd_total` and `ultimate_proxy_insights_open{kind}`.

The fingerprint memory lives in each proxy process, so with several replicas each one judges only the
traffic it sees. That is fine for rates; a shared store can come later if needed.

## Develop

```sh
go test ./...                                         # unit and integration tests (fake upstream)
TEST_DATABASE_URL=postgres://… go test ./internal/store  # also run the store tests against Postgres
TEST_CLICKHOUSE_URL=http://user:pass@localhost:8123/proxy_test go test ./internal/store  # and ClickHouse
go test -run x -bench Overhead ./internal/server      # proxy overhead vs. calling the upstream directly
scripts/compliance.sh <openresponses checkout>        # official compliance suite
```

`cmd/fake-upstream` is a deterministic fake of the OpenAI Responses API, so tests
and CI need no provider credentials.

## License

MIT. See [LICENSE](LICENSE).
