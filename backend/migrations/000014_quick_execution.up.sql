CREATE TABLE platform.quick_execution_snapshots (
 owner_id uuid NOT NULL REFERENCES platform.users(id) ON DELETE CASCADE,
 id uuid NOT NULL DEFAULT gen_random_uuid(),
 schema_version integer NOT NULL CHECK(schema_version>=1),
 definition jsonb NOT NULL,
 created_at timestamptz NOT NULL DEFAULT now(),
 PRIMARY KEY(owner_id,id),
 CHECK(jsonb_typeof(definition)='object' AND octet_length(definition::text)<=524288),
 CHECK(definition->'schema_version' IS NOT DISTINCT FROM to_jsonb(schema_version)),
 CHECK(jsonb_typeof(definition->'steps') IS NOT DISTINCT FROM 'array' AND jsonb_array_length(definition->'steps')=1)
);
ALTER TABLE platform.quick_execution_snapshots ENABLE ROW LEVEL SECURITY;
CREATE POLICY quick_snapshot_api_owner ON platform.quick_execution_snapshots TO autolife_api
 USING(owner_id=nullif(current_setting('autolife.execution_owner',true),'')::uuid);
CREATE POLICY quick_snapshot_worker_read ON platform.quick_execution_snapshots FOR SELECT TO autolife_worker USING(true);
GRANT SELECT,INSERT ON platform.quick_execution_snapshots TO autolife_api;
GRANT SELECT,DELETE ON platform.quick_execution_snapshots TO autolife_worker;
CREATE POLICY quick_snapshot_worker_delete ON platform.quick_execution_snapshots FOR DELETE TO autolife_worker USING(true);
CREATE TRIGGER quick_execution_snapshot_immutable BEFORE UPDATE ON platform.quick_execution_snapshots
 FOR EACH ROW EXECUTE FUNCTION platform.reject_workflow_history_update();

ALTER TABLE platform.executions DROP CONSTRAINT executions_check,
 ALTER COLUMN workflow_id DROP NOT NULL,
 ADD COLUMN quick_snapshot_id uuid,
 ADD CONSTRAINT execution_quick_snapshot_fk FOREIGN KEY(owner_id,quick_snapshot_id)
  REFERENCES platform.quick_execution_snapshots(owner_id,id) DEFERRABLE INITIALLY IMMEDIATE,
 ADD CONSTRAINT execution_definition_source_check CHECK (
  num_nonnulls(version_id,snapshot_id,quick_snapshot_id)=1 AND (workflow_id IS NULL)=(quick_snapshot_id IS NOT NULL)
 );
CREATE INDEX execution_quick_snapshot_idx ON platform.executions(owner_id,quick_snapshot_id) WHERE quick_snapshot_id IS NOT NULL;
UPDATE platform.schema_stamp SET version=14 WHERE singleton;
