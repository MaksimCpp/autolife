package postgres

import (
	"context"
	"encoding/json"

	"github.com/jackc/pgx/v5"
)

// ExportDefinitions is called with the privacy worker's transaction and exact
// owner. Execution snapshots are exported as private data but are not listed
// as published versions.
func ExportDefinitions(ctx context.Context, tx pgx.Tx, owner string) (json.RawMessage, error) {
	var out json.RawMessage
	err := tx.QueryRow(ctx, `SELECT coalesce(jsonb_agg(jsonb_build_object(
  'id',w.id,'name',w.name,'description',w.description,'status',w.status,
  'active_version_id',w.active_version_id,'created_at',w.created_at,'updated_at',w.updated_at,
  'draft',jsonb_build_object('revision',d.revision,'definition',d.definition,'updated_at',d.updated_at),
  'versions',coalesce((SELECT jsonb_agg(to_jsonb(v)-'owner_id'-'workflow_id' ORDER BY v.version)
   FROM platform.workflow_versions v WHERE v.owner_id=w.owner_id AND v.workflow_id=w.id),'[]'::jsonb),
  'draft_snapshots',coalesce((SELECT jsonb_agg(to_jsonb(s)-'owner_id'-'workflow_id' ORDER BY s.created_at,s.id)
   FROM platform.draft_execution_snapshots s WHERE s.owner_id=w.owner_id AND s.workflow_id=w.id),'[]'::jsonb)
  ) ORDER BY w.created_at,w.id),'[]'::jsonb)
  FROM platform.workflows w JOIN platform.workflow_drafts d ON d.owner_id=w.owner_id AND d.workflow_id=w.id
  WHERE w.owner_id=$1`, owner).Scan(&out)
	return out, err
}
