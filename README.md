# Omni Proxy

[![CI](https://github.com/omni-proxy/omni-proxy/actions/workflows/ci.yml/badge.svg)](https://github.com/omni-proxy/omni-proxy/actions/workflows/ci.yml)

A fast, self-hosted LLM gateway: a pass-through proxy for OpenAI-compatible APIs
([Responses](https://www.openresponses.org/) and Chat Completions) that controls and meters every
request by **tenant**, **application** and **end-user email**.

Omni Proxy is open source under the MIT license. It is before 1.0, so a minor version may change the
config or the admin API; [CHANGELOG.md](CHANGELOG.md) says what changed and how to upgrade.

## What it does

The proxy passes requests through to OpenAI-compatible providers unchanged, and adds control
(who may call, which models, how much) and accounting (tokens, cost, cache health) on the way.
Each provider in the config is reached under its own name:

| Endpoint | |
| --- | --- |
| `POST /<provider>/v1/responses` | The Responses API, JSON and SSE streaming. Passes the Open Responses HTTP compliance tests. |
| `POST /<provider>/v1/chat/completions` | Chat Completions, JSON and SSE streaming. |
| `GET /<provider>/v1/models` | The provider's own model list. |

So an OpenAI SDK only needs its base URL set to `http://proxy:8080/openai/v1`. The model in the body
goes to the provider as it is. Any provider that speaks the OpenAI API works: OpenAI, vLLM, Ollama,
llama.cpp, Groq, Together, OpenRouter and others. The proxy does not translate between APIs, retry,
or fall back to other models: an upstream error reaches the caller as the upstream sent it.

- API keys that expire and are limited to some models, and short-lived tokens for agents on users'
  devices.
- Rate limits per tenant, application or end user, in requests or tokens per minute, and spend or
  token budgets per day, week or month, shared across replicas through Valkey (see "Rate limits").
- Callers authenticate with the proxy's own API keys (one per application and tenant), with access
  tokens from your own identity provider (see "Using your own identity provider"), or both.
- End-user attribution from the `X-Proxy-User-Email` header, `metadata.user_email` or `safety_identifier`,
  accepted only from applications allowed to name their users.
- Usage events (tokens in/out, cached, cache writes, reasoning, latency, time to first token) written to
  ClickHouse in batches, off the request path. Tenants, keys, prices and insights stay in Postgres.
- `GET /admin/usage` to answer "who used how many tokens", grouped by tenant, application, email, model,
  provider or your own request tags (`X-Proxy-Tags`), optionally bucketed by hour or day.
- Prompt-cache diagnosis on every Responses request: whether the cache hit, if not why, and what the
  miss cost (see below). Chat Completions requests record cached tokens but are not diagnosed yet.
- Insights: problems the proxy notices in live traffic, per application and model, listed at
  `GET /admin/insights` and posted to a webhook or Slack when they open and resolve.
- Cost in USD from a versioned prices table, on every usage event and in `/admin/usage`.
- Prometheus metrics at `/metrics`.

Not built yet: WebSocket transport, `/responses/compact` and background responses.

## Run it

Everything runs with Docker Compose: the proxy, Postgres, ClickHouse, Valkey, the dashboard, and a free
fake provider (`fake`, any model name) to try things without a provider key.

```sh
cp .env.example .env        # optional: change the admin token, add OPENAI_API_KEY
docker compose up -d --build
scripts/demo-traffic.sh     # optional: sample tenants, traffic and cache problems
```

| What | Where |
| --- | --- |
| Dashboard | http://localhost:3000, sign in with your identity provider (see [Signing in](#signing-in)) or `PROXY_ADMIN_TOKEN` (default `dev-admin-token`) |
| Proxy API | http://localhost:8080, demo key `op_demo_key` for the `demo/playground` application |

That builds the images from your checkout. To run a published release instead, set
`OMNI_PROXY_VERSION` in `.env` to a version from the [releases](https://github.com/omni-proxy/omni-proxy/releases)
(such as `0.1.0`), then `docker compose pull && docker compose up -d`. The images are
`ghcr.io/omni-proxy/omni-proxy` (the proxy, and the fake provider) and `ghcr.io/omni-proxy/dashboard`,
for linux/amd64 and linux/arm64; `omni-proxy -version` prints the version.

The proxy config for the stack is `deploy/compose/config.yaml`: providers `fake` (free, at
`/fake/v1/...`) and `openai` (at `/openai/v1/...`, once `OPENAI_API_KEY` is set). Tenants, keys and prices live in the `pgdata` volume and the
request log in the `chdata` volume; `docker compose down -v` wipes both. `POSTGRES_PASSWORD` and
`CLICKHOUSE_PASSWORD` only apply when their volume is first created.

Without Docker: `go run ./cmd/omni-proxy -config config.yaml` (see `config.example.yaml`; without
`database_url` everything is kept in memory, and without `clickhouse_url` usage stays in Postgres), and `cd web && npm install && npm run dev` for the
dashboard on http://localhost:5173, which forwards `/admin` to `localhost:8080`.

### Where data lives

| Data | Store |
| --- | --- |
| Tenants, applications, API keys, prices, insights | Postgres (`database_url`) |
| Request log: one usage event per request (90 days by default) and hourly totals (forever), behind every `/admin/usage` query | ClickHouse (`clickhouse_url`) |
| Rate limit and budget counters | Valkey, Redis or Dragonfly (`redis_url`) |

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

### Using your own Redis or Dragonfly

The counters need any server that speaks the Redis protocol with Lua scripts and Redis 7 commands:
Valkey 7.2 or later, Redis 7 or later, or Dragonfly. Valkey 8, Redis 7.0 and Dragonfly 2.0 are tested.
Cluster mode is not supported. To use one you already run instead of the bundled Valkey, add this to
`.env`:

```sh
REDIS_URL=redis://:password@redis.internal:6379/3   # rediss:// for TLS; host.docker.internal for this machine
COMPOSE_FILE=docker-compose.yml:deploy/compose/external-redis.yml
```

`COMPOSE_FILE` adds `deploy/compose/external-redis.yml`, which stops compose from starting the bundled
Valkey. Every key starts with `rl/`, so a database number of its own (`/3` above) keeps them apart from
your other data. Nothing in it needs a backup: the proxy rebuilds budgets from the usage log.

### Using your own Postgres or ClickHouse

If your company already runs Postgres (RDS, Aurora, Cloud SQL, your own) or ClickHouse (ClickHouse Cloud or
your own server), the proxy can use them instead of the bundled ones, each on its own. It only needs a
schema or database of its own and a user with rights there; it creates and upgrades its tables itself
on start, and several replicas starting together take turns. Add to `.env` the URL of each you bring,
and its compose file to `COMPOSE_FILE` (files joined with `:`), which stops compose from starting the
bundled one:

```sh
DATABASE_URL=postgres://proxy:password@db.internal:5432/company?search_path=proxy&sslmode=require
CLICKHOUSE_URL=https://proxy:password@abc123.eu-west-1.aws.clickhouse.cloud:8443/proxy
COMPOSE_FILE=docker-compose.yml:deploy/compose/external-postgres.yml:deploy/compose/external-clickhouse.yml
```

**Postgres** 13 or later (13 and 16 are tested). To share a database with other services, give the proxy
a schema it owns and name it in `search_path`; nothing else in the database is touched:

```sql
CREATE ROLE proxy LOGIN PASSWORD '...';
CREATE SCHEMA proxy AUTHORIZATION proxy;
```

The URL takes every [libpq option](https://www.postgresql.org/docs/current/libpq-connect.html#LIBPQ-PARAMKEYWORDS)
the pgx driver supports. For TLS, `sslmode=require` encrypts; `sslmode=verify-full` also checks the
server's certificate, and with RDS or Aurora needs AWS's CA bundle: put
[`global-bundle.pem`](https://truststore.pki.rds.amazonaws.com/global/global-bundle.pem) in
`deploy/compose/certs/` and add `&sslrootcert=/etc/omni-proxy/certs/global-bundle.pem`. Behind
PgBouncer in transaction mode (or RDS Proxy), add `&default_query_exec_mode=exec`, since prepared
statements do not survive between transactions there.

**ClickHouse** 24.8 or later on a single server, or ClickHouse Cloud (24.8 and 25.8 are tested). Use `https://`
and port 8443 for TLS; for a certificate from a private CA, put the CA in `deploy/compose/certs/` and
add `?sslrootcert=/etc/omni-proxy/certs/ca.pem`. The proxy creates the database if it is missing,
but a user who may not create databases can use one made for it, with these rights:

```sql
CREATE DATABASE proxy;
CREATE USER proxy IDENTIFIED BY '...';
GRANT SELECT, INSERT, ALTER, CREATE TABLE, CREATE VIEW, DROP VIEW ON proxy.* TO proxy;
```

The tables are plain `MergeTree` tables made on the server the URL points at (ClickHouse Cloud turns
them into replicated ones by itself). A self-hosted cluster of several servers is not supported: the
proxy does not make `ON CLUSTER` or `Replicated` tables, so point it at one server.

## Dashboard

The dashboard (`web/`: React, Tailwind CSS and shadcn/ui components, light theme, square edges, monospace type) is a front end for the admin API. `npx shadcn add <component>` works there to add more components.

- **Overview**: requests, tokens, cost and cache hit rate over 24 hours, 7 days or 30 days, with the
  top applications and users.
- **Usage**: group by tenant, application, email, model, provider, cache status or a request tag,
  filter, export CSV.
- **Prompt cache**: each application's hit rate and why its requests miss.
- **Insights**: open and resolved problems with their cause and fix.
- **Prices**: prices in effect and their history; add a new price.
- **Tenants & keys**: create tenants, applications and keys; revoke keys.
- **Audit log**: who changed what, and every sign-in.

### Audit log

Every request to the admin API that can change something (`POST`, `PATCH`, `DELETE`) is recorded in
Postgres after it is served: when, who (the signed-in email, or the admin token), the action (such as
`key.create` or `limit.update`), the object's id, the JSON body that was sent, and the HTTP status, so
refused and failed attempts show too. Sign-ins are recorded, and so are people the identity provider
vouched for but the allow lists refused. Wrong admin tokens are only logged, since anyone can send
them. Reads are not recorded, and replies never are: a new key's secret stays out of the log. Read it
on the dashboard's Audit log tab or at `GET /admin/audit?limit=100&before=<RFC 3339 time>`, newest
first. Entries are kept until you delete them from the `audit_log` table.

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
  OIDC_ALLOWED_DOMAINS=example.com             # admins; and/or OIDC_ALLOWED_EMAILS, OIDC_ALLOWED_GROUPS
  OIDC_VIEWER_GROUPS=finance                   # read-only; and/or OIDC_VIEWER_EMAILS, OIDC_VIEWER_DOMAINS
  ADMIN_SESSION_SECRET=$(openssl rand -base64 32)
  ```

  Only people on an allow list get in: an email in `allowed_emails`, an email at a domain in
  `allowed_domains`, or a group in `allowed_groups` (read from the ID token's `groups` claim, or
  `groups_claim`; some providers need an extra scope in `OIDC_SCOPES` to send it). An email the
  provider marks unverified is ignored. The proxy uses the authorization code flow with PKCE, checks
  state and nonce, and verifies the ID token against the provider's keys.

  A sign-in becomes a signed, HttpOnly session cookie that lasts `session_ttl` (12 hours by
  default). The allow lists and roles are checked on every request, so taking someone off a list and
  restarting the proxy ends their session. Set `session_secret` so sessions survive restarts and
  work across several proxies.

- **The break-glass token**, `admin.token` (`PROXY_ADMIN_TOKEN`): a bearer token for scripts and
  for when the provider is down. The dashboard offers it under the SSO button. Set it empty to turn
  it off once single sign-on works. It always has full admin rights.

### Roles

People on the `allowed_*` lists are full admins. `roles` lets more people in with less:

```yaml
admin:
  oidc:
    allowed_groups: [platform]           # full admins
    roles:
      - role: viewer                     # read everything, change nothing
        groups: [finance]
      - role: admin                      # run their own tenant: apps, keys, limits
        groups: [acme-leads]
        tenants: [acme]
      - role: viewer                     # read their own tenant
        domains: [globex.com]
        tenants: [globex]
```

| Role | Reads | Changes |
| --- | --- | --- |
| admin, every tenant | everything, including the audit log | everything: tenants, prices, tokens |
| viewer, every tenant | everything, including the audit log | nothing |
| admin of some tenants | those tenants' usage, insights, keys and limits; prices | those tenants' applications, keys and limits |
| viewer of some tenants | those tenants' usage, insights, keys and limits; prices | nothing |

Tenants are named by name. Someone matching several entries gets all of them. The proxy checks every
admin request, and the dashboard hides what the role can't use. Only full admins create tenants, add
prices or revoke minted tokens, since those belong to no single tenant.

## Using your own identity provider

API callers can send an access token (a JWT) from the identity provider the organization already
runs, instead of a proxy API key: Keycloak, Microsoft Entra ID, Okta, Auth0, Authentik and others.
Tenant, application and end user are read from the token's claims, and appear in the dashboard the
first time a token names them. Nothing has to be created in the proxy first.

```yaml
auth:
  api_keys: true          # keep accepting op_ keys too (the default)
  jwt:
    - issuer: https://keycloak.example.com/realms/acme   # must equal the tokens' iss exactly
      audience: omni-proxy   # required: tokens issued for other services are refused
      tenant: acme               # a fixed tenant for this issuer, or:
      claims:
        tenant: org_id           # the claim naming the tenant (wins over the fixed one)
        app: azp                 # default: azp, then client_id
        user: email              # default: email
        groups: groups           # default: groups
        models: llm_models       # optional: a claim listing the models the caller may use
      group_models:              # optional: models per group, for providers without a custom claim
        ml-team: [gpt-5, vllm/*]
```

The proxy fetches the issuer's signing keys from its discovery document (or `jwks_url`) and checks
the signature, issuer, audience and expiry of every token. Only asymmetric algorithms (RS, PS, ES,
EdDSA) are accepted. A verified token is cached until it expires, for at most five minutes, so
only the first request with a token pays for the signature check. List several issuers to serve
several organizations, for example one per Keycloak realm.

Allowed models are model names (any provider) or `<provider>/<model>`, optionally ending in `*`. A
request for any other model gets `403 model_not_allowed`. When neither `claims.models` nor
`group_models` is set, every model is allowed.

With Keycloak, add an *Audience* mapper to the client so its access tokens carry
`aud: omni-proxy`, and a *Group Membership* mapper (with "Full group path" off) for `groups`.

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
  curl -s localhost:8080/v1/tokens -H "Authorization: Bearer op_…" \
    -d '{"user":"alice@acme.com","models":["gpt-5-mini"],"expires_in":900}'
  # {"id":"tok_…","token":"opt_…","expires_at":"…","user":"alice@acme.com","models":["gpt-5-mini"]}
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
  -d '{"expires_in": 2592000, "allowed_models": ["gpt-5-mini", "vllm/*"]}'   # or "expires_at": "2027-01-01T00:00:00Z"
```

An expired key gets `401 api_key_expired`, on time even though keys are cached for 30 seconds. A
model outside the list gets `403 model_not_allowed`.

Call the proxy with any OpenAI client, with the base URL set to `http://localhost:8080/<provider>/v1`:

```sh
curl localhost:8080/openai/v1/responses \
  -H "Authorization: Bearer op_…" \
  -H "X-Proxy-User-Email: alice@acme.com" \
  -d '{"model":"gpt-5-mini","input":"Hello!","stream":true}'

curl localhost:8080/openai/v1/chat/completions \
  -H "Authorization: Bearer op_…" \
  -d '{"model":"gpt-5-mini","messages":[{"role":"user","content":"Hello!"}]}'
```

For streamed Chat Completions the proxy asks the provider for usage
(`stream_options.include_usage`) so it can count tokens, and leaves that last chunk out unless the
client asked for it too. Nothing else in a request or reply is changed.

Ask who used what:

```sh
curl -s "localhost:8080/admin/usage?group_by=tenant,application,email&granularity=day" -H "$ADMIN"
```

| Parameter | Meaning |
| --- | --- |
| `group_by` | Comma-separated: `tenant`, `application`, `email`, `model`, `provider`, `cache`, `tag:<key>`, or `none`. Default `tenant`. |
| `from`, `to` | RFC 3339 times. Default: the last 24 hours. |
| `granularity` | `hour` or `day`. Default: one row per group. |
| `tenant_id`, `application_id`, `email`, `model`, `provider`, `cache_status`, `tag:<key>` | Filters. |

Each row also carries `cost_usd` when the model had a price at the time of the request.

### Request tags

To see which feature or environment the money goes to, label requests with the `X-Proxy-Tags`
header. The proxy stores the tags with the usage event and doesn't send them to the provider.

```sh
curl localhost:8080/openai/v1/responses -H "Authorization: Bearer op_…" \
  -H "X-Proxy-Tags: feature=search,env=prod" -d '{"model":"gpt-5-mini","input":"Hello!"}'

curl -s "localhost:8080/admin/usage?group_by=tag:feature&tag:env=prod" -H "$ADMIN"
```

A request carries up to 10 tags. Keys are up to 40 characters of `a-z`, `0-9`, `_`, `-` and `.`.
Values are up to 100 characters of letters, digits and `_ - . : / @ +`. A malformed header is refused
with `400 invalid_tags`. The hourly rollup keeps one row per distinct set of tags, so use tags for a
handful of values each (features, environments, teams), not for request or user IDs.

### Tracing

The proxy can send an OpenTelemetry span for each model call to any backend that takes OTLP over
HTTP (an OpenTelemetry Collector, Grafana Tempo, Jaeger, Honeycomb, Langfuse, Datadog). It's off
until an endpoint is set:

```yaml
tracing:
  endpoint: http://otel-collector:4318   # /v1/traces is added when the URL has no path
  headers: {Authorization: "Bearer …"}   # optional
  sample_ratio: 1                        # share of calls traced, 0 to 1
  include_user: false                    # add user.email to spans
```

In the compose stack, set `TRACING_ENDPOINT` (and `TRACING_AUTHORIZATION` if the backend needs a
key) in `.env`.

When the caller sends a W3C `traceparent` header, the proxy's span joins the caller's trace, and the
caller's sampling decision wins over `sample_ratio`. Spans are named `chat <model>` and carry the
OpenTelemetry GenAI attributes (`gen_ai.provider.name`, `gen_ai.request.model`,
`gen_ai.usage.input_tokens`, `gen_ai.usage.output_tokens`, `gen_ai.usage.cache_read.input_tokens`,
…) plus `omni_proxy.*` ones: tenant, application, status, cost in USD, cache status, money lost
to a cache miss, the request ID (as in `X-Proxy-Request-Id`), time to first token, and each request tag as `omni_proxy.tag.<key>`.

Spans never contain prompts or outputs. The end user's email is left out unless `include_user` is
on, since tracing backends are often readable by more people than the dashboard.

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

Counters live in Valkey when `redis_url` is set (the compose stack runs one; Redis or Dragonfly work
too, see [Using your own Redis or Dragonfly](#using-your-own-redis-or-dragonfly)), and every replica shares
them with one round trip per request. Without it each proxy counts on its own, so set it when running
more than one. If Valkey is unreachable, requests are let through and
`omni_proxy_limiter_errors_total` counts it; `limits.fail_closed: true` refuses them instead.
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

`model` matches a model name from any provider, or `<provider>/<model>` for one provider only.
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

### Money lost to cache misses

A miss that could have hit is priced: the tokens that should have come from cache, times the
difference between the model's input and cached-input prices. For `miss_unexpected` those are the
tokens of the prefix sent recently. For the misses the application causes (`miss_instructions_*`,
`miss_tools_*`, `miss_history_rewritten`), the proxy estimates them as the length of the application's
previous prompt, when that was sent within `cache_ttl`. Each usage event carries `missed_cost_usd`,
`/admin/usage` sums it, and the dashboard shows it on the Prompt cache page (in total, split between
"app changes its prompt" and "provider missed", and per application) and on the Overview. The two
cache insights quote it too. It is an estimate: it needs a price for the model, and providers decide
for themselves what they cache.

`/admin/usage?group_by=application,cache` shows the mix per application. Every minute, rules look at
the last `window` of traffic per application and model and open an insight when a rate crosses its
threshold, then resolve it when the rate falls below half of it:

| Insight | Fires when |
| --- | --- |
| `cache_prefix_unstable` | Too many requests miss because the application keeps changing its prompt prefix. The insight says which change dominates, how to fix it, and what it cost. |
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
`omni_proxy_cache_requests_total{status}`, `omni_proxy_cache_missed_tokens_total`,
`omni_proxy_cost_usd_total` and `omni_proxy_insights_open{kind}`.

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

`cmd/fake-upstream` is a deterministic fake of the OpenAI Responses and Chat Completions APIs, so tests
and CI need no provider credentials.

See [CONTRIBUTING.md](CONTRIBUTING.md) for how to send changes and how releases are made.

## Security

Please report vulnerabilities privately, as [SECURITY.md](SECURITY.md) describes.

## License

MIT. See [LICENSE](LICENSE).
