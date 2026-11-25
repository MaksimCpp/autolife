DROP TABLE platform.auth_rate_limits;
UPDATE platform.schema_stamp SET version = 2 WHERE singleton;
