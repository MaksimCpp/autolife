DO $$ BEGIN
 IF EXISTS(SELECT 1 FROM platform.executions) OR EXISTS(SELECT 1 FROM platform.quota_limits WHERE resource_type='concurrent_executions') THEN
  RAISE EXCEPTION 'execution rollback requires an empty schema or a coordinated backup/forward fix';
 END IF;
END $$;
DROP FUNCTION platform.active_execution_count();
DROP FUNCTION platform.lock_execution_storage_owner(uuid);
REVOKE INSERT(owner_id,display_name,mime,bytes,sha256),UPDATE(ready_at,expires_at) ON platform.files FROM autolife_worker;
REVOKE INSERT ON platform.file_pins FROM autolife_worker;
DROP POLICY workflow_output_pin ON platform.file_pins;
DROP POLICY execution_usage_worker ON platform.quota_limits;
DROP POLICY execution_reservations_worker ON platform.quota_reservations;
REVOKE SELECT ON platform.quota_limits,platform.quota_reservations FROM autolife_worker;
REVOKE INSERT ON platform.file_pins FROM autolife_api;
DROP POLICY execution_pins_api ON platform.file_pins;
REVOKE INSERT ON platform.outbox_events,platform.outbox_consumptions FROM autolife_worker;
REVOKE SELECT ON platform.outbox_consumptions FROM autolife_worker;
DROP TABLE platform.execution_event_totals,platform.execution_idempotency,platform.execution_reservations,platform.execution_file_refs,platform.execution_staged_files,platform.execution_attempts,platform.execution_steps,platform.executions;
DROP FUNCTION platform.validate_execution_transition();
ALTER TABLE platform.quota_limits DROP CONSTRAINT quota_limits_resource_type_check;
ALTER TABLE platform.quota_limits ADD CONSTRAINT quota_limits_resource_type_check
 CHECK(resource_type IN ('storage_bytes','concurrent_uploads'));
UPDATE platform.schema_stamp SET version=9 WHERE singleton;
