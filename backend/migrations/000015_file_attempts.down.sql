DROP TRIGGER file_attempt_outbox ON platform.execution_attempts;
DROP FUNCTION platform.record_file_attempt();
DROP INDEX platform.execution_file_ref_position_idx;
ALTER TABLE platform.execution_file_refs DROP COLUMN position;
ALTER TABLE platform.execution_event_totals DROP COLUMN sampled_jobs,DROP COLUMN cpu_ns,DROP COLUMN peak_memory_bytes,
 DROP COLUMN storage_gets,DROP COLUMN storage_puts,DROP COLUMN storage_read_bytes,DROP COLUMN storage_write_bytes;
ALTER TABLE platform.execution_attempts DROP COLUMN sampled_jobs,DROP COLUMN cpu_ns,DROP COLUMN peak_memory_bytes,
 DROP COLUMN storage_gets,DROP COLUMN storage_puts,DROP COLUMN storage_read_bytes,DROP COLUMN storage_write_bytes;
UPDATE platform.schema_stamp SET version=14 WHERE singleton;
