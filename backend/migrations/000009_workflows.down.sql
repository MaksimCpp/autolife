DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM platform.workflows) THEN
        RAISE EXCEPTION 'workflow rollback requires an empty schema or a coordinated backup/forward fix';
    END IF;
END;
$$;
ALTER TABLE platform.workflows DROP CONSTRAINT workflows_active_version_fk;
DROP TABLE platform.draft_execution_snapshots,platform.workflow_versions,platform.workflow_drafts,platform.workflows;
DROP FUNCTION platform.reject_workflow_history_update();
UPDATE platform.schema_stamp SET version=8 WHERE singleton;
