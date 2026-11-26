CREATE POLICY files_worker ON platform.files TO autolife_worker USING (true) WITH CHECK (true);
GRANT UPDATE (mime, bytes, sha256, state, ready_at, expires_at) ON platform.files TO autolife_api;
GRANT SELECT, DELETE, UPDATE (state) ON platform.files TO autolife_worker;
GRANT SELECT ON platform.quota_limits, platform.file_pins TO autolife_api;
GRANT SELECT, INSERT, DELETE ON platform.quota_reservations TO autolife_api;
GRANT SELECT, DELETE ON platform.file_pins TO autolife_worker;
ALTER TABLE platform.file_pins ENABLE ROW LEVEL SECURITY;
ALTER TABLE platform.quota_limits ENABLE ROW LEVEL SECURITY;
ALTER TABLE platform.quota_reservations ENABLE ROW LEVEL SECURITY;
CREATE POLICY pins_owner ON platform.file_pins TO autolife_api USING (owner_id = nullif(current_setting('autolife.file_owner',true),'')::uuid);
CREATE POLICY pins_worker ON platform.file_pins TO autolife_worker USING (true);
CREATE POLICY limits_owner ON platform.quota_limits TO autolife_api USING (owner_id = nullif(current_setting('autolife.file_owner',true),'')::uuid);
CREATE POLICY reservations_owner ON platform.quota_reservations TO autolife_api USING (owner_id = nullif(current_setting('autolife.file_owner',true),'')::uuid);
CREATE INDEX files_pending_cleanup ON platform.files (created_at,id) WHERE state IN ('uploading','deleting');

CREATE FUNCTION platform.check_file_pin() RETURNS trigger LANGUAGE plpgsql
SET search_path = pg_catalog, platform AS $$
BEGIN
  PERFORM 1 FROM platform.files WHERE id=NEW.file_id AND owner_id=NEW.owner_id
    AND state='ready' AND expires_at>clock_timestamp() FOR UPDATE;
  IF NOT FOUND THEN RAISE EXCEPTION 'file unavailable for pin' USING ERRCODE='23503'; END IF;
  RETURN NEW;
END;
$$;
CREATE TRIGGER check_file_pin BEFORE INSERT ON platform.file_pins FOR EACH ROW EXECUTE FUNCTION platform.check_file_pin();
UPDATE platform.schema_stamp SET version=8 WHERE singleton;
