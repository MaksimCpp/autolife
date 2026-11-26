CREATE TABLE platform.files (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    owner_id uuid NOT NULL REFERENCES platform.users (id),
    object_key text GENERATED ALWAYS AS ('files/' || id::text) STORED UNIQUE,
    display_name text NOT NULL CHECK (length(display_name) BETWEEN 1 AND 255),
    mime text CHECK (mime IN ('image/jpeg','image/png','image/webp','application/pdf')),
    bytes bigint CHECK (bytes BETWEEN 1 AND 100000000),
    sha256 bytea CHECK (octet_length(sha256) = 32),
    state text NOT NULL DEFAULT 'uploading' CHECK (state IN ('uploading','ready','deleting')),
    created_at timestamptz NOT NULL DEFAULT now(),
    ready_at timestamptz,
    expires_at timestamptz,
    UNIQUE (owner_id, id),
    CHECK ((ready_at IS NULL) = (expires_at IS NULL)),
    CHECK (expires_at IS NULL OR expires_at > ready_at),
    CHECK (state <> 'uploading' OR ready_at IS NULL),
    CHECK (state <> 'ready' OR (ready_at IS NOT NULL AND mime IS NOT NULL AND bytes IS NOT NULL AND sha256 IS NOT NULL)),
    CHECK ((mime IS NULL) = (bytes IS NULL) AND (bytes IS NULL) = (sha256 IS NULL))
);
CREATE INDEX files_owner_listing ON platform.files (owner_id, created_at DESC, id DESC);
CREATE INDEX files_expiry ON platform.files (expires_at, id) WHERE state = 'ready';

CREATE TABLE platform.file_pins (
    owner_id uuid NOT NULL,
    file_id uuid NOT NULL,
    id uuid NOT NULL DEFAULT gen_random_uuid(),
    created_at timestamptz NOT NULL DEFAULT now(),
    expires_at timestamptz NOT NULL CHECK (expires_at > created_at),
    PRIMARY KEY (owner_id, file_id, id),
    FOREIGN KEY (owner_id, file_id) REFERENCES platform.files (owner_id, id)
);

CREATE TABLE platform.quota_limits (
    owner_id uuid NOT NULL REFERENCES platform.users (id) ON DELETE CASCADE,
    resource_type text NOT NULL CHECK (resource_type IN ('storage_bytes','concurrent_uploads')),
    limit_value bigint NOT NULL CHECK (limit_value > 0),
    PRIMARY KEY (owner_id, resource_type)
);
CREATE TABLE platform.quota_reservations (
    owner_id uuid NOT NULL,
    file_id uuid NOT NULL,
    reserved_bytes bigint NOT NULL CHECK (reserved_bytes BETWEEN 1 AND 100000000),
    created_at timestamptz NOT NULL DEFAULT now(),
    expires_at timestamptz NOT NULL CHECK (expires_at > created_at),
    PRIMARY KEY (owner_id, file_id),
    FOREIGN KEY (owner_id, file_id) REFERENCES platform.files (owner_id, id) ON DELETE CASCADE
);

ALTER TABLE platform.files ENABLE ROW LEVEL SECURITY;
CREATE POLICY files_api_owner ON platform.files TO autolife_api
    USING (owner_id = nullif(current_setting('autolife.file_owner', true), '')::uuid)
    WITH CHECK (owner_id = nullif(current_setting('autolife.file_owner', true), '')::uuid);
GRANT SELECT ON platform.files TO autolife_api;
GRANT INSERT (owner_id, display_name) ON platform.files TO autolife_api;
UPDATE platform.schema_stamp SET version = 7 WHERE singleton;
