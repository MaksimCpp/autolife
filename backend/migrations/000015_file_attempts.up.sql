ALTER TABLE platform.execution_attempts
 ADD COLUMN sampled_jobs integer NOT NULL DEFAULT 0 CHECK(sampled_jobs>=0),
 ADD COLUMN cpu_ns bigint CHECK(cpu_ns>=0),
 ADD COLUMN peak_memory_bytes bigint CHECK(peak_memory_bytes>0),
 ADD COLUMN storage_gets bigint NOT NULL DEFAULT 0 CHECK(storage_gets>=0),
 ADD COLUMN storage_puts bigint NOT NULL DEFAULT 0 CHECK(storage_puts>=0),
 ADD COLUMN storage_read_bytes bigint NOT NULL DEFAULT 0 CHECK(storage_read_bytes>=0),
 ADD COLUMN storage_write_bytes bigint NOT NULL DEFAULT 0 CHECK(storage_write_bytes>=0);
ALTER TABLE platform.execution_event_totals
 ADD COLUMN sampled_jobs bigint NOT NULL DEFAULT 0,
 ADD COLUMN cpu_ns bigint NOT NULL DEFAULT 0,
 ADD COLUMN peak_memory_bytes bigint NOT NULL DEFAULT 0,
 ADD COLUMN storage_gets bigint NOT NULL DEFAULT 0,
 ADD COLUMN storage_puts bigint NOT NULL DEFAULT 0,
 ADD COLUMN storage_read_bytes bigint NOT NULL DEFAULT 0,
 ADD COLUMN storage_write_bytes bigint NOT NULL DEFAULT 0;
ALTER TABLE platform.execution_file_refs ADD COLUMN position integer CHECK(position>=0);
CREATE UNIQUE INDEX execution_file_ref_position_idx ON platform.execution_file_refs(execution_id,role,position) WHERE position IS NOT NULL;

CREATE FUNCTION platform.record_file_attempt() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF OLD.ended_at IS NULL AND NEW.ended_at IS NOT NULL THEN
  INSERT INTO platform.outbox_events(type,logical_key,payload)
  SELECT 'file.action_attempted', e.id::text||':'||NEW.ordinal::text||':'||NEW.number::text,
   jsonb_build_object('schema_version',1,'execution_id',e.id::text,'owner_id',e.owner_id::text,
    'action_type',s.action_type,'action_version',s.action_version,'attempt',NEW.number,
    'outcome',NEW.state,'sampled_jobs',NEW.sampled_jobs,'cpu_ns',NEW.cpu_ns,'peak_memory_bytes',NEW.peak_memory_bytes,'duration_ms',greatest(0,(extract(epoch FROM (NEW.ended_at-NEW.started_at))*1000)::bigint),
    'storage_gets',NEW.storage_gets,'storage_puts',NEW.storage_puts,
    'storage_read_bytes',NEW.storage_read_bytes,'storage_write_bytes',NEW.storage_write_bytes,
    'input_bytes',coalesce((NEW.file_usage->>'input_bytes')::bigint,0),
    'output_bytes',coalesce((NEW.file_usage->>'output_bytes')::bigint,0))
  FROM platform.executions e JOIN platform.execution_steps s ON s.execution_id=e.id
  WHERE e.id=NEW.execution_id AND s.ordinal=NEW.ordinal AND s.action_type LIKE 'file.%'
  ON CONFLICT DO NOTHING;
 END IF;
 RETURN NEW;
END;
$$;
CREATE TRIGGER file_attempt_outbox AFTER UPDATE OF ended_at ON platform.execution_attempts
 FOR EACH ROW EXECUTE FUNCTION platform.record_file_attempt();
UPDATE platform.schema_stamp SET version=15 WHERE singleton;
