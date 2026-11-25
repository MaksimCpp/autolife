DROP TABLE platform.privacy_jobs, platform.audit_events, platform.outbox_consumptions,
    platform.outbox_events, platform.auth_tokens, platform.sessions, platform.user_settings, platform.users;
UPDATE platform.schema_stamp SET version = 1 WHERE singleton;
