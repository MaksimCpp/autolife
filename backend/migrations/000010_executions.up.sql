ALTER TABLE platform.quota_limits DROP CONSTRAINT quota_limits_resource_type_check;
ALTER TABLE platform.quota_limits ADD CONSTRAINT quota_limits_resource_type_check
 CHECK(resource_type IN ('storage_bytes','concurrent_uploads','concurrent_executions'));
CREATE TABLE platform.executions (
 id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
 owner_id uuid NOT NULL REFERENCES platform.users(id) ON DELETE CASCADE,
 workflow_id uuid NOT NULL,
 version_id uuid,
 snapshot_id uuid,
 trigger_type text NOT NULL DEFAULT 'core.manual',
 trigger_values jsonb NOT NULL,
 user_inputs jsonb NOT NULL,
 status text NOT NULL DEFAULT 'queued' CHECK(status IN ('queued','running','retrying','cancel_requested','succeeded','failed','cancelled','timed_out')),
 queued_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 start_deadline timestamptz NOT NULL,
 started_at timestamptz,
 run_deadline timestamptz,
 terminal_at timestamptz,
 retry_at timestamptz,
 lease_until timestamptz,
 fence bigint NOT NULL DEFAULT 0 CHECK(fence>=0),
 error_code text,
 outputs jsonb,
 UNIQUE(owner_id,id),
 FOREIGN KEY(owner_id,workflow_id) REFERENCES platform.workflows(owner_id,id) ON DELETE CASCADE,
 FOREIGN KEY(owner_id,workflow_id,version_id) REFERENCES platform.workflow_versions(owner_id,workflow_id,id) ON DELETE CASCADE,
 FOREIGN KEY(owner_id,workflow_id,snapshot_id) REFERENCES platform.draft_execution_snapshots(owner_id,workflow_id,id) ON DELETE CASCADE,
 CHECK(num_nonnulls(version_id,snapshot_id)=1),
 CHECK((started_at IS NULL)=(run_deadline IS NULL)),
 CHECK((terminal_at IS NOT NULL)=(status IN ('succeeded','failed','cancelled','timed_out'))),
 CHECK((outputs IS NOT NULL)=(status='succeeded'))
);
CREATE FUNCTION platform.validate_execution_transition() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF OLD.status=NEW.status THEN RETURN NEW; END IF;
 IF NOT (CASE OLD.status
  WHEN 'queued' THEN NEW.status IN ('running','cancel_requested','timed_out')
  WHEN 'running' THEN NEW.status IN ('retrying','cancel_requested','succeeded','failed','timed_out')
  WHEN 'retrying' THEN NEW.status IN ('queued','cancel_requested','timed_out')
  WHEN 'cancel_requested' THEN NEW.status='cancelled'
  ELSE false END)
 THEN RAISE EXCEPTION 'invalid execution transition' USING ERRCODE='55000'; END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER execution_state_machine BEFORE UPDATE OF status ON platform.executions
 FOR EACH ROW EXECUTE FUNCTION platform.validate_execution_transition();
CREATE INDEX executions_owner_cursor ON platform.executions(owner_id,queued_at DESC,id DESC);
CREATE INDEX executions_pending ON platform.executions(queued_at,id) WHERE terminal_at IS NULL;
CREATE TABLE platform.execution_steps (
 execution_id uuid NOT NULL REFERENCES platform.executions(id) ON DELETE CASCADE,
 ordinal int NOT NULL CHECK(ordinal BETWEEN 0 AND 19),
 step_key text NOT NULL,
 action_type text NOT NULL,
 action_version int NOT NULL CHECK(action_version>0),
 status text NOT NULL DEFAULT 'pending' CHECK(status IN ('pending','running','retrying','succeeded','failed','skipped','cancelled','timed_out')),
 input_values jsonb,
 output_values jsonb,
 error_code text,
 started_at timestamptz,
 ended_at timestamptz,
 PRIMARY KEY(execution_id,ordinal),
 UNIQUE(execution_id,step_key)
);
CREATE TABLE platform.execution_attempts (
 execution_id uuid NOT NULL,
 ordinal int NOT NULL,
 number int NOT NULL CHECK(number BETWEEN 1 AND 3),
 fence bigint NOT NULL CHECK(fence>0),
 state text NOT NULL DEFAULT 'running' CHECK(state IN ('running','succeeded','retryable_failed','permanent_failed','interrupted','timed_out','cancelled','unknown_outcome')),
 started_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 ended_at timestamptz,
 error_code text,
 retry_at timestamptz,
 PRIMARY KEY(execution_id,ordinal,number),
 FOREIGN KEY(execution_id,ordinal) REFERENCES platform.execution_steps(execution_id,ordinal) ON DELETE CASCADE
);
CREATE TABLE platform.execution_file_refs (
 owner_id uuid NOT NULL,
 execution_id uuid NOT NULL,
 file_id uuid NOT NULL,
 live_file_id uuid,
 pin_id uuid,
 role text NOT NULL CHECK(role IN ('input','output')),
 PRIMARY KEY(execution_id,file_id,role),
 FOREIGN KEY(owner_id,execution_id) REFERENCES platform.executions(owner_id,id) ON DELETE CASCADE,
 CHECK(live_file_id IS NULL OR live_file_id=file_id),
 FOREIGN KEY(owner_id,live_file_id) REFERENCES platform.files(owner_id,id) ON DELETE SET NULL (live_file_id),
 FOREIGN KEY(owner_id,file_id,pin_id) REFERENCES platform.file_pins(owner_id,file_id,id)
);
CREATE TABLE platform.execution_staged_files (
 owner_id uuid NOT NULL,
 execution_id uuid NOT NULL,
 ordinal int NOT NULL,
 attempt int NOT NULL,
 fence bigint NOT NULL,
 file_id uuid PRIMARY KEY,
 uploaded boolean NOT NULL DEFAULT false,
 checkpoint boolean NOT NULL DEFAULT false,
 FOREIGN KEY(owner_id,execution_id) REFERENCES platform.executions(owner_id,id) ON DELETE CASCADE,
 FOREIGN KEY(execution_id,ordinal,attempt) REFERENCES platform.execution_attempts(execution_id,ordinal,number) ON DELETE CASCADE,
 FOREIGN KEY(owner_id,file_id) REFERENCES platform.files(owner_id,id) ON DELETE CASCADE
);
CREATE INDEX execution_staging_run ON platform.execution_staged_files(execution_id,ordinal,attempt);
GRANT SELECT,INSERT,UPDATE,DELETE ON platform.execution_staged_files TO autolife_worker;
GRANT INSERT(owner_id,display_name,mime,bytes,sha256),UPDATE(ready_at,expires_at) ON platform.files TO autolife_worker;
GRANT INSERT ON platform.file_pins TO autolife_worker;
CREATE POLICY workflow_output_pin ON platform.file_pins FOR INSERT TO autolife_worker WITH CHECK(true);
CREATE POLICY execution_usage_worker ON platform.quota_limits FOR SELECT TO autolife_worker USING(true);
CREATE POLICY execution_reservations_worker ON platform.quota_reservations FOR SELECT TO autolife_worker USING(true);
GRANT SELECT ON platform.quota_limits,platform.quota_reservations TO autolife_worker;
CREATE FUNCTION platform.lock_execution_storage_owner(request_owner uuid) RETURNS boolean LANGUAGE sql SECURITY DEFINER
 SET search_path=pg_catalog,platform AS $$
 SELECT EXISTS(SELECT 1 FROM platform.users WHERE id=request_owner AND status='active' FOR UPDATE)
 $$;
REVOKE ALL ON FUNCTION platform.lock_execution_storage_owner(uuid) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION platform.lock_execution_storage_owner(uuid) TO autolife_worker;
CREATE TABLE platform.execution_reservations (
 owner_id uuid NOT NULL,
 execution_id uuid PRIMARY KEY,
 input_bytes bigint NOT NULL CHECK(input_bytes BETWEEN 0 AND 250000000),
 FOREIGN KEY(owner_id,execution_id) REFERENCES platform.executions(owner_id,id) ON DELETE CASCADE
);
CREATE TABLE platform.execution_idempotency (
 owner_id uuid NOT NULL,
 operation text NOT NULL,
 key text NOT NULL CHECK(length(key) BETWEEN 1 AND 128),
 request_hash bytea NOT NULL CHECK(octet_length(request_hash)=32),
 execution_id uuid NOT NULL,
 expires_at timestamptz NOT NULL,
 PRIMARY KEY(owner_id,operation,key),
 FOREIGN KEY(owner_id,execution_id) REFERENCES platform.executions(owner_id,id) ON DELETE CASCADE
);
CREATE TABLE platform.execution_event_totals (
 type text NOT NULL,
 outcome text NOT NULL,
 count bigint NOT NULL,
 duration_ms bigint NOT NULL,
 PRIMARY KEY(type,outcome)
);
ALTER TABLE platform.executions ENABLE ROW LEVEL SECURITY;
CREATE POLICY execution_api_owner ON platform.executions TO autolife_api USING (owner_id=nullif(current_setting('autolife.execution_owner',true),'')::uuid);
CREATE POLICY execution_worker ON platform.executions TO autolife_worker USING (true);
DO $$
DECLARE relation text;
BEGIN
 FOREACH relation IN ARRAY ARRAY['execution_file_refs','execution_reservations','execution_idempotency']
 LOOP
  EXECUTE format('ALTER TABLE platform.%I ENABLE ROW LEVEL SECURITY',relation);
  EXECUTE format('CREATE POLICY execution_api_owner ON platform.%I TO autolife_api USING (owner_id=nullif(current_setting(''autolife.execution_owner'',true),'''')::uuid)',relation);
  EXECUTE format('CREATE POLICY execution_worker ON platform.%I TO autolife_worker USING (true)',relation);
 END LOOP;
 FOREACH relation IN ARRAY ARRAY['execution_steps','execution_attempts']
 LOOP
  EXECUTE format('ALTER TABLE platform.%I ENABLE ROW LEVEL SECURITY',relation);
  EXECUTE format('CREATE POLICY execution_api_owner ON platform.%I TO autolife_api USING (EXISTS(SELECT 1 FROM platform.executions e WHERE e.id=execution_id))',relation);
  EXECUTE format('CREATE POLICY execution_worker ON platform.%I TO autolife_worker USING (true)',relation);
 END LOOP;
END;
$$;
GRANT SELECT,INSERT,UPDATE(status) ON platform.executions TO autolife_api;
GRANT SELECT,INSERT ON platform.execution_steps,platform.execution_file_refs,platform.execution_reservations TO autolife_api;
GRANT SELECT ON platform.execution_attempts,platform.execution_event_totals TO autolife_api;
GRANT SELECT,INSERT,DELETE ON platform.execution_idempotency TO autolife_api;
GRANT SELECT,UPDATE,DELETE ON platform.executions TO autolife_worker;
GRANT SELECT,INSERT,UPDATE ON platform.execution_steps,platform.execution_attempts,platform.execution_file_refs TO autolife_worker;
GRANT SELECT,DELETE ON platform.execution_reservations TO autolife_worker;
GRANT SELECT,INSERT,UPDATE ON platform.execution_event_totals TO autolife_worker;
GRANT INSERT ON platform.outbox_events,platform.outbox_consumptions TO autolife_worker;
GRANT SELECT ON platform.outbox_consumptions TO autolife_worker;
CREATE POLICY execution_pins_api ON platform.file_pins FOR INSERT TO autolife_api
 WITH CHECK(owner_id=nullif(current_setting('autolife.file_owner',true),'')::uuid);
GRANT INSERT ON platform.file_pins TO autolife_api;
-- The global limit is checked through an aggregate without exposing other owners' rows.
CREATE FUNCTION platform.active_execution_count() RETURNS bigint LANGUAGE sql SECURITY DEFINER
 SET search_path=pg_catalog,platform AS $$ SELECT count(*) FROM platform.execution_reservations $$;
REVOKE ALL ON FUNCTION platform.active_execution_count() FROM PUBLIC;
GRANT EXECUTE ON FUNCTION platform.active_execution_count() TO autolife_api;
UPDATE platform.schema_stamp SET version=10 WHERE singleton;
