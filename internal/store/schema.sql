CREATE TABLE IF NOT EXISTS tenants (
    id         text PRIMARY KEY,
    name       text NOT NULL UNIQUE,
    created_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS applications (
    id               text PRIMARY KEY,
    tenant_id        text NOT NULL REFERENCES tenants (id),
    name             text NOT NULL,
    can_assert_users boolean NOT NULL DEFAULT true,
    created_at       timestamptz NOT NULL DEFAULT now(),
    UNIQUE (tenant_id, name)
);

CREATE TABLE IF NOT EXISTS api_keys (
    id         text PRIMARY KEY,
    app_id     text NOT NULL REFERENCES applications (id),
    key_hash   text NOT NULL UNIQUE,
    prefix     text NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    revoked_at timestamptz
);

CREATE TABLE IF NOT EXISTS usage_events (
    ts                  timestamptz NOT NULL,
    request_id          text NOT NULL,
    tenant_id           text NOT NULL,
    app_id              text NOT NULL,
    key_id              text NOT NULL,
    user_email          text NOT NULL,
    user_source         text NOT NULL,
    model               text NOT NULL,
    provider            text NOT NULL,
    upstream_model      text NOT NULL,
    stream              boolean NOT NULL,
    status              text NOT NULL,
    error_code          text NOT NULL,
    http_status         integer NOT NULL,
    input_tokens        integer NOT NULL,
    cached_input_tokens integer NOT NULL,
    cache_write_tokens  integer NOT NULL,
    output_tokens       integer NOT NULL,
    reasoning_tokens    integer NOT NULL,
    usage_reported      boolean NOT NULL,
    latency_ms          integer NOT NULL,
    ttft_ms             integer,
    prompt_cache_key    text NOT NULL
);

CREATE INDEX IF NOT EXISTS usage_events_ts_brin ON usage_events USING brin (ts);
CREATE INDEX IF NOT EXISTS usage_events_tenant_ts ON usage_events (tenant_id, ts);
CREATE INDEX IF NOT EXISTS usage_events_app_ts ON usage_events (app_id, ts);
CREATE INDEX IF NOT EXISTS usage_events_email_ts ON usage_events (user_email, ts);

ALTER TABLE usage_events ADD COLUMN IF NOT EXISTS cost_usd double precision NOT NULL DEFAULT 0;
ALTER TABLE usage_events ADD COLUMN IF NOT EXISTS cache_status text NOT NULL DEFAULT '';
ALTER TABLE usage_events ADD COLUMN IF NOT EXISTS expected_cached_tokens integer NOT NULL DEFAULT 0;
ALTER TABLE usage_events ADD COLUMN IF NOT EXISTS auth_method text NOT NULL DEFAULT '';
ALTER TABLE usage_events ADD COLUMN IF NOT EXISTS subject text NOT NULL DEFAULT '';
ALTER TABLE usage_events ADD COLUMN IF NOT EXISTS tags jsonb NOT NULL DEFAULT '{}';
ALTER TABLE usage_events ADD COLUMN IF NOT EXISTS missed_cost_usd double precision NOT NULL DEFAULT 0;

CREATE TABLE IF NOT EXISTS insights (
    id          text PRIMARY KEY,
    kind        text NOT NULL,
    severity    text NOT NULL,
    status      text NOT NULL,
    tenant_id   text NOT NULL,
    app_id      text NOT NULL,
    model       text NOT NULL,
    title       text NOT NULL,
    detail      text NOT NULL,
    evidence    jsonb NOT NULL,
    first_seen  timestamptz NOT NULL,
    last_seen   timestamptz NOT NULL,
    resolved_at timestamptz
);

CREATE INDEX IF NOT EXISTS insights_status_last_seen ON insights (status, last_seen DESC);

CREATE TABLE IF NOT EXISTS model_prices (
    id             text PRIMARY KEY,
    model          text NOT NULL,
    input          double precision NOT NULL,
    cached_input   double precision NOT NULL,
    cache_write    double precision NOT NULL,
    output         double precision NOT NULL,
    effective_from timestamptz NOT NULL,
    created_at     timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS model_prices_model_effective ON model_prices (model, effective_from);

CREATE TABLE IF NOT EXISTS proxy_tokens (
    id             text PRIMARY KEY,
    token_hash     text NOT NULL UNIQUE,
    app_id         text NOT NULL REFERENCES applications (id),
    user_email     text NOT NULL,
    allowed_models text[],
    minted_by      text NOT NULL,
    created_at     timestamptz NOT NULL DEFAULT now(),
    expires_at     timestamptz NOT NULL,
    revoked_at     timestamptz
);

CREATE INDEX IF NOT EXISTS proxy_tokens_expires_at ON proxy_tokens (expires_at);

ALTER TABLE api_keys ADD COLUMN IF NOT EXISTS expires_at timestamptz;
ALTER TABLE api_keys ADD COLUMN IF NOT EXISTS allowed_models text[];

-- Rate limits and budgets. A rule applies to a tenant, to one of its
-- applications, or to end users: user is an email, or * for each user on
-- their own. Counters live in Valkey (or in process), not here.
CREATE TABLE IF NOT EXISTS limits (
    id          text PRIMARY KEY,
    tenant_id   text NOT NULL REFERENCES tenants (id),
    app_id      text NOT NULL DEFAULT '',
    user_email  text NOT NULL DEFAULT '',
    kind        text NOT NULL,
    amount      double precision NOT NULL,
    period      text NOT NULL DEFAULT '',
    enforcement text NOT NULL DEFAULT 'hard',
    created_at  timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS audit_log (
    id           text PRIMARY KEY,
    ts           timestamptz NOT NULL,
    actor_method text NOT NULL,
    actor_email  text NOT NULL,
    actor_name   text NOT NULL,
    actor_role   text NOT NULL,
    action       text NOT NULL,
    method       text NOT NULL,
    path         text NOT NULL,
    target_id    text NOT NULL,
    status       integer NOT NULL,
    request      jsonb
);

CREATE INDEX IF NOT EXISTS audit_log_ts ON audit_log (ts DESC);
