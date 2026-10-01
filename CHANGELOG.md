# Changelog

All notable changes to Omni Proxy are listed here. The project follows
[Semantic Versioning](https://semver.org/). Until 1.0, a minor version (0.2, 0.3, ...) may change the
config file, the admin API or the database schema; the release notes say how to upgrade.

## [Unreleased]

## [0.1.0] - 2026-10-01

The first release.

### Proxy

- Pass-through for OpenAI-compatible APIs at `/<provider>/v1/responses`, `/<provider>/v1/chat/completions`
  and `/<provider>/v1/models`, with JSON and SSE streaming. Requests and responses reach the provider
  and the caller unchanged; the Responses endpoint passes the Open Responses HTTP compliance tests.
- Every request attributed to a tenant, an application and an end-user email, and written to a usage
  log with tokens (input, cached, cache writes, output, reasoning), cost in USD, latency and time to
  first token.

### Control

- API keys per application that can expire and be limited to some models.
- Access tokens from your own identity provider (any OIDC issuer with a JWKS), mapped to tenants,
  applications and allowed models by claims.
- Short-lived tokens for agents running on users' devices and in browsers.
- Rate limits in requests or tokens per minute, and spend or token budgets per day, week or month, per
  tenant, application or end user, shared across replicas through Valkey, Redis 7+ or Dragonfly.

### Accounting

- Request log in ClickHouse, with 90 days of raw events and hourly totals kept forever; Postgres for
  tenants, keys, prices and insights.
- `GET /admin/usage`, grouped by tenant, application, email, model, provider, cache status or your own
  request tags (`X-Proxy-Tags`), bucketed by hour or day.
- Model prices in a versioned table, editable from the admin API and the dashboard.
- Prompt-cache diagnosis on every Responses request: hit or miss, the likely cause of a miss, and the
  money a miss cost. Insights about problems in live traffic, posted to a webhook or Slack.
- Prometheus metrics at `/metrics` and opt-in OpenTelemetry tracing of model calls (metadata only).

### Dashboard

- Overview, usage, cache, insights, limits, access, prices and audit pages.
- Sign-in with your own OIDC provider, admin and viewer roles optionally limited to some tenants, and a
  break-glass admin token.
- Audit log of admin changes and sign-ins.

### Deployment

- The whole stack runs with `docker compose up`: proxy, dashboard, Postgres, ClickHouse, Valkey and a
  free fake provider for trying it out.
- Your own Postgres, ClickHouse, or Redis-compatible server can replace the bundled ones.
- Images for linux/amd64 and linux/arm64 at `ghcr.io/omni-proxy/omni-proxy` and
  `ghcr.io/omni-proxy/dashboard`.

[Unreleased]: https://github.com/omni-proxy/omni-proxy/compare/v0.1.0...HEAD
[0.1.0]: https://github.com/omni-proxy/omni-proxy/releases/tag/v0.1.0
