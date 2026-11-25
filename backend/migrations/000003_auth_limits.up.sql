CREATE TABLE platform.auth_rate_limits (
    scope text NOT NULL CHECK (scope IN ('source', 'account')),
    key_hash bytea NOT NULL CHECK (octet_length(key_hash) = 32),
    window_start timestamptz NOT NULL,
    attempts integer NOT NULL CHECK (attempts BETWEEN 1 AND 100),
    PRIMARY KEY (scope, key_hash, window_start)
);
CREATE INDEX auth_rate_expiry_idx ON platform.auth_rate_limits (window_start);
GRANT SELECT, INSERT, UPDATE ON platform.auth_rate_limits TO autolife_api;
GRANT SELECT, DELETE ON platform.auth_rate_limits TO autolife_worker;
UPDATE platform.schema_stamp SET version = 3 WHERE singleton;
