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
