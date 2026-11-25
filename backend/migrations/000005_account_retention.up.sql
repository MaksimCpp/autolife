CREATE FUNCTION platform.prune_account_history()
RETURNS void
LANGUAGE sql
SECURITY DEFINER
SET search_path = pg_catalog
AS $$
    DELETE FROM platform.audit_events WHERE occurred_at <= now() - interval '90 days';
    DELETE FROM platform.auth_tokens WHERE expires_at <= now() - interval '1 day';
    DELETE FROM platform.sessions
    WHERE least(expires_at, coalesce(revoked_at, expires_at)) <= now() - interval '30 days';
$$;
REVOKE EXECUTE ON FUNCTION platform.prune_account_history() FROM PUBLIC;
GRANT EXECUTE ON FUNCTION platform.prune_account_history() TO autolife_worker;
UPDATE platform.schema_stamp SET version = 5 WHERE singleton;
