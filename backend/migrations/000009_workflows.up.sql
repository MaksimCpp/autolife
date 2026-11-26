CREATE TABLE platform.workflows (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    owner_id uuid NOT NULL REFERENCES platform.users(id) ON DELETE CASCADE,
    name text NOT NULL CHECK (char_length(btrim(name)) BETWEEN 1 AND 120),
    description text NOT NULL DEFAULT '' CHECK (char_length(description) <= 2000),
    status text NOT NULL DEFAULT 'draft' CHECK (status IN ('draft','active','inactive','archived')),
    active_version_id uuid,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (owner_id,id),
    CHECK (status <> 'active' OR active_version_id IS NOT NULL)
);
CREATE INDEX workflows_owner_cursor ON platform.workflows(owner_id,created_at DESC,id DESC);

CREATE TABLE platform.workflow_drafts (
    workflow_id uuid PRIMARY KEY,
    owner_id uuid NOT NULL,
    revision bigint NOT NULL DEFAULT 1 CHECK (revision >= 1),
    definition jsonb NOT NULL CHECK (jsonb_typeof(definition)='object' AND octet_length(definition::text)<=524288),
    updated_at timestamptz NOT NULL DEFAULT now(),
    FOREIGN KEY (owner_id,workflow_id) REFERENCES platform.workflows(owner_id,id) ON DELETE CASCADE
);
CREATE TABLE platform.workflow_versions (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    owner_id uuid NOT NULL,
    workflow_id uuid NOT NULL,
    version bigint NOT NULL CHECK (version >= 1),
    revision bigint NOT NULL CHECK (revision >= 1),
    schema_version integer NOT NULL CHECK (schema_version >= 1),
    definition jsonb NOT NULL CHECK (jsonb_typeof(definition)='object' AND octet_length(definition::text)<=524288),
    CHECK (definition->'schema_version' IS NOT DISTINCT FROM to_jsonb(schema_version)),
    created_at timestamptz NOT NULL DEFAULT now(),
    FOREIGN KEY (owner_id,workflow_id) REFERENCES platform.workflows(owner_id,id) ON DELETE CASCADE,
    UNIQUE (owner_id,workflow_id,id),
    UNIQUE (workflow_id,version),
    UNIQUE (workflow_id,revision)
);
ALTER TABLE platform.workflows ADD CONSTRAINT workflows_active_version_fk
    FOREIGN KEY (owner_id,id,active_version_id) REFERENCES platform.workflow_versions(owner_id,workflow_id,id)
    DEFERRABLE INITIALLY DEFERRED;

CREATE TABLE platform.draft_execution_snapshots (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    owner_id uuid NOT NULL,
    workflow_id uuid NOT NULL,
    revision bigint NOT NULL CHECK (revision >= 1),
    schema_version integer NOT NULL CHECK (schema_version >= 1),
    definition jsonb NOT NULL CHECK (jsonb_typeof(definition)='object' AND octet_length(definition::text)<=524288),
    CHECK (definition->'schema_version' IS NOT DISTINCT FROM to_jsonb(schema_version)),
    created_at timestamptz NOT NULL DEFAULT now(),
    FOREIGN KEY (owner_id,workflow_id) REFERENCES platform.workflows(owner_id,id) ON DELETE CASCADE,
    UNIQUE (owner_id,workflow_id,id)
);

CREATE FUNCTION platform.reject_workflow_history_update() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'workflow history is immutable' USING ERRCODE='55000';
END;
$$;
CREATE TRIGGER workflow_version_immutable BEFORE UPDATE ON platform.workflow_versions
    FOR EACH ROW EXECUTE FUNCTION platform.reject_workflow_history_update();
CREATE TRIGGER workflow_snapshot_immutable BEFORE UPDATE ON platform.draft_execution_snapshots
    FOR EACH ROW EXECUTE FUNCTION platform.reject_workflow_history_update();

DO $$
DECLARE table_name text;
BEGIN
    FOREACH table_name IN ARRAY ARRAY['workflows','workflow_drafts','workflow_versions','draft_execution_snapshots']
    LOOP
        EXECUTE format('ALTER TABLE platform.%I ENABLE ROW LEVEL SECURITY',table_name);
        EXECUTE format('CREATE POLICY workflow_api_owner ON platform.%I TO autolife_api USING (owner_id = nullif(current_setting(''autolife.workflow_owner'', true), '''')::uuid)',table_name);
        EXECUTE format('CREATE POLICY workflow_worker_read ON platform.%I FOR SELECT TO autolife_worker USING (true)',table_name);
        EXECUTE format('GRANT SELECT, INSERT ON platform.%I TO autolife_api',table_name);
        EXECUTE format('GRANT SELECT ON platform.%I TO autolife_worker',table_name);
    END LOOP;
END;
$$;
GRANT UPDATE (name,description,status,active_version_id,updated_at) ON platform.workflows TO autolife_api;
GRANT UPDATE (definition,revision,updated_at) ON platform.workflow_drafts TO autolife_api;
UPDATE platform.schema_stamp SET version=9 WHERE singleton;
