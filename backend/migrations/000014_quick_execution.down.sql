DROP INDEX platform.execution_quick_snapshot_idx;
ALTER TABLE platform.executions ALTER COLUMN workflow_id SET NOT NULL,
 DROP CONSTRAINT execution_definition_source_check, DROP CONSTRAINT execution_quick_snapshot_fk,
 DROP COLUMN quick_snapshot_id, ADD CONSTRAINT executions_check CHECK(num_nonnulls(version_id,snapshot_id)=1);
DROP TABLE platform.quick_execution_snapshots;
UPDATE platform.schema_stamp SET version=13 WHERE singleton;
