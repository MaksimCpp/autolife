CREATE TABLE platform.users (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    normalized_email text NOT NULL UNIQUE CHECK (
        length(normalized_email) BETWEEN 3 AND 254
        AND normalized_email = lower(btrim(normalized_email))
    ),
    password_hash text NOT NULL CHECK (length(password_hash) BETWEEN 1 AND 512),
    email_verified_at timestamptz,
    status text NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'deleting')),
    deleting_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT now(),
    CHECK ((status = 'deleting') = (deleting_at IS NOT NULL))
);
CREATE TABLE platform.user_settings (
    user_id uuid PRIMARY KEY REFERENCES platform.users ON DELETE CASCADE,
    locale text NOT NULL DEFAULT 'ru' CHECK (locale IN ('ru', 'en')),
    timezone text NOT NULL DEFAULT 'UTC' CHECK (length(timezone) BETWEEN 1 AND 128),
    theme text NOT NULL DEFAULT 'system' CHECK (theme IN ('system', 'light', 'dark')),
    notifications boolean NOT NULL DEFAULT true,
    analytics_consent boolean NOT NULL DEFAULT false,
    analytics_consent_updated_at timestamptz
);
CREATE TABLE platform.sessions (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id uuid NOT NULL REFERENCES platform.users ON DELETE CASCADE,
    token_hash bytea NOT NULL UNIQUE CHECK (octet_length(token_hash) = 32),
    created_at timestamptz NOT NULL DEFAULT now(),
    last_authenticated_at timestamptz NOT NULL DEFAULT now(),
    expires_at timestamptz NOT NULL,
    revoked_at timestamptz,
    CHECK (expires_at > created_at),
    CHECK (last_authenticated_at >= created_at)
);
CREATE INDEX sessions_owner_idx ON platform.sessions (user_id, created_at DESC, id DESC);
CREATE TABLE platform.auth_tokens (
    token_hash bytea PRIMARY KEY CHECK (octet_length(token_hash) = 32),
    user_id uuid NOT NULL REFERENCES platform.users ON DELETE CASCADE,
    purpose text NOT NULL CHECK (purpose IN ('verify', 'reset')),
    created_at timestamptz NOT NULL DEFAULT now(),
    expires_at timestamptz NOT NULL,
    used_at timestamptz,
    CHECK (expires_at > created_at),
    CHECK (used_at IS NULL OR used_at >= created_at)
);
CREATE INDEX auth_tokens_owner_idx ON platform.auth_tokens (user_id, purpose);
CREATE TABLE platform.outbox_events (
    event_id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    type text NOT NULL CHECK (length(type) BETWEEN 1 AND 120),
    schema_version integer NOT NULL DEFAULT 1 CHECK (schema_version > 0),
    logical_key text NOT NULL CHECK (length(logical_key) BETWEEN 1 AND 200),
    occurred_at timestamptz NOT NULL DEFAULT now(),
    payload jsonb NOT NULL CHECK (jsonb_typeof(payload) = 'object'),
    UNIQUE (type, logical_key)
);
CREATE TABLE platform.outbox_consumptions (
    consumer text NOT NULL CHECK (length(consumer) BETWEEN 1 AND 120),
    event_id uuid NOT NULL REFERENCES platform.outbox_events ON DELETE CASCADE,
    processed_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (consumer, event_id)
);
CREATE TABLE platform.audit_events (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    actor uuid,
    action text NOT NULL CHECK (length(action) BETWEEN 1 AND 120),
    resource_ref uuid,
    outcome text NOT NULL CHECK (outcome IN ('accepted', 'succeeded', 'failed', 'denied')),
    occurred_at timestamptz NOT NULL DEFAULT now(),
    request_id text CHECK (request_id IS NULL OR length(request_id) <= 64)
);
CREATE TABLE platform.privacy_jobs (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    owner_id uuid NOT NULL,
    type text NOT NULL CHECK (type IN ('export', 'delete')),
    state text NOT NULL,
    export_owner_id uuid GENERATED ALWAYS AS (CASE WHEN type = 'export' THEN owner_id END) STORED
        REFERENCES platform.users ON DELETE CASCADE,
    idempotency_key text CHECK (idempotency_key IS NULL OR length(idempotency_key) BETWEEN 1 AND 128),
    requested_at timestamptz NOT NULL DEFAULT now(),
    completed_at timestamptz,
    expires_at timestamptz,
    result jsonb CHECK (result IS NULL OR jsonb_typeof(result) = 'object'),
    safe_error text,
    CHECK (
        (type = 'export' AND state IN ('requested', 'running', 'ready', 'failed', 'expired') AND idempotency_key IS NOT NULL)
        OR (type = 'delete' AND state IN ('requested', 'running', 'succeeded', 'failed') AND idempotency_key IS NULL AND result IS NULL AND expires_at IS NULL)
    ),
    CHECK ((state = 'ready') = (result IS NOT NULL AND expires_at IS NOT NULL)),
    CHECK (expires_at IS NULL OR completed_at IS NOT NULL AND expires_at > completed_at)
);
CREATE UNIQUE INDEX privacy_delete_once ON platform.privacy_jobs (owner_id) WHERE type = 'delete';
CREATE INDEX privacy_jobs_owner_idx ON platform.privacy_jobs (owner_id, type, requested_at DESC);

GRANT SELECT, INSERT, UPDATE ON platform.users, platform.user_settings, platform.sessions, platform.auth_tokens, platform.privacy_jobs TO autolife_api;
GRANT INSERT ON platform.outbox_events, platform.audit_events TO autolife_api;
GRANT SELECT, UPDATE ON platform.privacy_jobs TO autolife_worker;
GRANT SELECT, DELETE ON platform.users, platform.outbox_events TO autolife_worker;
GRANT SELECT ON platform.user_settings, platform.sessions TO autolife_worker;
GRANT INSERT ON platform.audit_events TO autolife_worker;
UPDATE platform.schema_stamp SET version = 2 WHERE singleton;
