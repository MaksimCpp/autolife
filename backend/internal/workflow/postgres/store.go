package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"autolife/backend/internal/workflow"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

type Store struct {
	pool     *pgxpool.Pool
	registry *workflow.Registry
}

func New(pool *pgxpool.Pool, r *workflow.Registry) *Store { return &Store{pool: pool, registry: r} }

const workflowColumns = "id::text,name,description,status,active_version_id::text,created_at,updated_at"
const versionColumns = "id::text,version,revision,schema_version,definition,created_at"

func readWorkflow(r pgx.Row, w *workflow.Workflow) error {
	return notFound(r.Scan(&w.ID, &w.Name, &w.Description, &w.Status, &w.ActiveVersionID, &w.CreatedAt, &w.UpdatedAt))
}
func readDraft(r pgx.Row, d *workflow.Draft) error {
	return notFound(r.Scan(&d.Revision, &d.Definition, &d.UpdatedAt))
}
func readVersion(r pgx.Row, v *workflow.Version) error {
	return notFound(r.Scan(&v.ID, &v.Number, &v.Revision, &v.SchemaVersion, &v.Definition, &v.CreatedAt))
}
func notFound(err error) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return workflow.ErrNotFound
	}
	return err
}

func (s *Store) owned(ctx context.Context, owner string, mutating bool, run func(pgx.Tx) error) error {
	if !workflow.ValidID(owner) {
		return workflow.ErrNotFound
	}
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, "SELECT set_config('autolife.workflow_owner',$1,true), set_config('autolife.file_owner',$1,true)", owner); err != nil {
			return err
		}
		query := "SELECT id::text FROM platform.users WHERE id=$1 AND status='active'"
		if mutating {
			query += " FOR UPDATE"
		}
		var id string
		if err := tx.QueryRow(ctx, query, owner).Scan(&id); err != nil {
			return notFound(err)
		}
		return run(tx)
	})
}

func load(ctx context.Context, tx pgx.Tx, owner, id string, lock bool) (workflow.Details, error) {
	var d workflow.Details
	if !workflow.ValidID(id) {
		return d, workflow.ErrNotFound
	}
	query := "SELECT " + workflowColumns + " FROM platform.workflows WHERE owner_id=$1 AND id=$2"
	if lock {
		query += " FOR UPDATE"
	}
	if err := readWorkflow(tx.QueryRow(ctx, query, owner, id), &d.Workflow); err != nil {
		return d, err
	}
	err := readDraft(tx.QueryRow(ctx, "SELECT revision,definition,updated_at FROM platform.workflow_drafts WHERE owner_id=$1 AND workflow_id=$2", owner, id), &d.Draft)
	return d, err
}

func (s *Store) Get(ctx context.Context, owner, id string) (workflow.Details, error) {
	var w workflow.Details
	err := s.owned(ctx, owner, false, func(tx pgx.Tx) error {
		// The read is one snapshot so the active version and draft are consistent.
		query := `SELECT w.id::text,w.name,w.description,w.status,w.active_version_id::text,w.created_at,w.updated_at,d.revision,d.definition,d.updated_at
    FROM platform.workflows w JOIN platform.workflow_drafts d ON d.owner_id=w.owner_id AND d.workflow_id=w.id
    WHERE w.owner_id=$1 AND w.id=$2`
		if !workflow.ValidID(id) {
			return workflow.ErrNotFound
		}
		return notFound(tx.QueryRow(ctx, query, owner, id).Scan(&w.ID, &w.Name, &w.Description, &w.Status, &w.ActiveVersionID, &w.CreatedAt, &w.UpdatedAt, &w.Draft.Revision, &w.Draft.Definition, &w.Draft.UpdatedAt))
	})
	return w, err
}

func (s *Store) List(ctx context.Context, owner string, status string, before *time.Time, beforeID string, limit int) ([]workflow.Workflow, error) {
	result := make([]workflow.Workflow, 0, limit)
	err := s.owned(ctx, owner, false, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, "SELECT "+workflowColumns+` FROM platform.workflows WHERE owner_id=$1
   AND ($2='' OR status=$2) AND ($3::timestamptz IS NULL OR (created_at,id)<($3::timestamptz,nullif($4,'')::uuid))
   ORDER BY created_at DESC,id DESC LIMIT $5`, owner, status, before, beforeID, limit)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var w workflow.Workflow
			if err := readWorkflow(rows, &w); err != nil {
				return err
			}
			result = append(result, w)
		}
		return rows.Err()
	})
	return result, err
}

func (s *Store) Create(ctx context.Context, owner, name, description string) (workflow.Details, error) {
	var d workflow.Details
	err := s.owned(ctx, owner, true, func(tx pgx.Tx) error {
		var err error
		d, err = create(ctx, tx, owner, name, description, workflow.EmptyDraft())
		return err
	})
	return d, err
}

func create(ctx context.Context, tx pgx.Tx, owner, name, description string, definition json.RawMessage) (workflow.Details, error) {
	var d workflow.Details
	if err := readWorkflow(tx.QueryRow(ctx, "INSERT INTO platform.workflows(owner_id,name,description) VALUES($1,$2,$3) RETURNING "+workflowColumns, owner, name, description), &d.Workflow); err != nil {
		return d, err
	}
	if err := readDraft(tx.QueryRow(ctx, `INSERT INTO platform.workflow_drafts(owner_id,workflow_id,definition) VALUES($1,$2,$3)
  RETURNING revision,definition,updated_at`, owner, d.ID, definition), &d.Draft); err != nil {
		return d, err
	}
	return d, event(ctx, tx, "workflow.created", owner, d.ID, d.ID, d.Draft.Revision, "")
}

func (s *Store) Copy(ctx context.Context, owner, id string, name *string) (workflow.Details, error) {
	var copied workflow.Details
	err := s.owned(ctx, owner, true, func(tx pgx.Tx) error {
		source, err := load(ctx, tx, owner, id, true)
		if err != nil {
			return err
		}
		copyName := source.Name
		if name != nil {
			copyName = *name
		}
		copied, err = create(ctx, tx, owner, copyName, source.Description, source.Draft.Definition)
		return err
	})
	return copied, err
}

func (s *Store) Update(ctx context.Context, owner, id string, name, description *string) (workflow.Details, error) {
	var d workflow.Details
	err := s.owned(ctx, owner, true, func(tx pgx.Tx) error {
		var err error
		d, err = load(ctx, tx, owner, id, true)
		if err != nil {
			return err
		}
		if d.Status == "archived" {
			return workflow.ErrArchived
		}
		if (name == nil || *name == d.Name) && (description == nil || *description == d.Description) {
			return nil
		}
		if err := readWorkflow(tx.QueryRow(ctx, `UPDATE platform.workflows SET name=coalesce($3,name),description=coalesce($4,description),
    updated_at=statement_timestamp() WHERE owner_id=$1 AND id=$2 RETURNING `+workflowColumns, owner, id, name, description), &d.Workflow); err != nil {
			return err
		}
		return event(ctx, tx, "workflow.updated", owner, id, "", d.Draft.Revision, "")
	})
	return d, err
}

func (s *Store) Archive(ctx context.Context, owner, id string) error {
	return s.owned(ctx, owner, true, func(tx pgx.Tx) error {
		w, err := load(ctx, tx, owner, id, true)
		if err != nil {
			return err
		}
		if w.Status == "archived" {
			return nil
		}
		if _, err := tx.Exec(ctx, "UPDATE platform.workflows SET status='archived',updated_at=statement_timestamp() WHERE owner_id=$1 AND id=$2", owner, id); err != nil {
			return err
		}
		return event(ctx, tx, "workflow.updated", owner, id, id+":archive", w.Draft.Revision, "")
	})
}

func (s *Store) SaveDraft(ctx context.Context, owner, id string, expected int64, definition json.RawMessage) (workflow.Draft, error) {
	var draft workflow.Draft
	invalidDefinition := &workflow.Invalid{Fields: []workflow.Problem{{Code: "INVALID_DEFINITION", Path: "/definition"}}}
	if !workflow.ValidDraft(definition) {
		return draft, invalidDefinition
	}
	err := s.owned(ctx, owner, true, func(tx pgx.Tx) error {
		d, err := load(ctx, tx, owner, id, true)
		if err != nil {
			return err
		}
		if d.Status == "archived" {
			return workflow.ErrArchived
		}
		if d.Draft.Revision != expected {
			return workflow.ErrConflict
		}
		if err := readDraft(tx.QueryRow(ctx, `UPDATE platform.workflow_drafts SET definition=$3,revision=revision+1,updated_at=statement_timestamp()
   WHERE owner_id=$1 AND workflow_id=$2 RETURNING revision,definition,updated_at`, owner, id, definition), &draft); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, "UPDATE platform.workflows SET updated_at=statement_timestamp() WHERE owner_id=$1 AND id=$2", owner, id); err != nil {
			return err
		}
		return event(ctx, tx, "workflow.updated", owner, id, fmt.Sprintf("%s:draft:%d", id, draft.Revision), draft.Revision, "")
	})
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && (pgErr.Code == "22P05" || pgErr.Code == "22021" || pgErr.Code == "22P02" || (pgErr.Code == "23514" && pgErr.TableName == "workflow_drafts")) {
		return workflow.Draft{}, invalidDefinition
	}
	return draft, err
}

func (s *Store) Validate(ctx context.Context, owner, id string, override json.RawMessage) ([]workflow.Problem, error) {
	var problems []workflow.Problem
	err := s.owned(ctx, owner, false, func(tx pgx.Tx) error {
		d, err := load(ctx, tx, owner, id, false)
		if err != nil {
			return err
		}
		if override == nil {
			override = d.Draft.Definition
		}
		_, problems, err = s.validate(ctx, tx, owner, override)
		return err
	})
	return problems, err
}

func (s *Store) validate(ctx context.Context, tx pgx.Tx, owner string, raw json.RawMessage) (workflow.Definition, []workflow.Problem, error) {
	d, err := workflow.DecodeDefinition(raw)
	if err != nil {
		var invalid *workflow.Invalid
		if errors.As(err, &invalid) {
			return d, invalid.Fields, nil
		}
		return d, nil, err
	}
	found := make(map[string]string)
	lookup := func(ctx context.Context, id string) (string, error) {
		if mime, ok := found[id]; ok {
			return mime, nil
		}
		if len(found) >= 50 {
			return "", nil
		}
		var mime string
		err := tx.QueryRow(ctx, `SELECT mime FROM platform.files WHERE owner_id=$1 AND id=$2 AND state='ready'
   AND expires_at>clock_timestamp() AND bytes BETWEEN 1 AND 100000000`, owner, id).Scan(&mime)
		if errors.Is(err, pgx.ErrNoRows) {
			err = nil
		}
		if err == nil {
			found[id] = mime
		}
		return mime, err
	}
	fields, err := s.registry.Validate(ctx, d, lookup)
	return d, fields, err
}

func (s *Store) Publish(ctx context.Context, owner, id string, expected int64) (workflow.Version, error) {
	var v workflow.Version
	err := s.owned(ctx, owner, true, func(tx pgx.Tx) error {
		d, err := load(ctx, tx, owner, id, true)
		if err != nil {
			return err
		}
		if d.Status == "archived" {
			return workflow.ErrArchived
		}
		if d.Draft.Revision != expected {
			return workflow.ErrConflict
		}
		var already bool
		if err := tx.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM platform.workflow_versions WHERE owner_id=$1 AND workflow_id=$2 AND revision=$3)", owner, id, expected).Scan(&already); err != nil {
			return err
		}
		if already {
			return workflow.ErrConflict
		}
		def, fields, err := s.validate(ctx, tx, owner, d.Draft.Definition)
		if err != nil {
			return err
		}
		if len(fields) != 0 {
			return &workflow.Invalid{Fields: fields}
		}
		if err := readVersion(tx.QueryRow(ctx, `INSERT INTO platform.workflow_versions(owner_id,workflow_id,version,revision,schema_version,definition)
   SELECT $1,$2,coalesce(max(version),0)+1,$3,$4,$5 FROM platform.workflow_versions WHERE owner_id=$1 AND workflow_id=$2
   RETURNING `+versionColumns, owner, id, expected, def.SchemaVersion, d.Draft.Definition), &v); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, "UPDATE platform.workflows SET status='active',active_version_id=$3,updated_at=statement_timestamp() WHERE owner_id=$1 AND id=$2", owner, id, v.ID); err != nil {
			return err
		}
		return event(ctx, tx, "workflow.activated", owner, id, v.ID, expected, v.ID)
	})
	return v, err
}

func (s *Store) Versions(ctx context.Context, owner, id string, before *int64, limit int) ([]workflow.Version, error) {
	list := make([]workflow.Version, 0, limit)
	err := s.owned(ctx, owner, false, func(tx pgx.Tx) error {
		if _, err := load(ctx, tx, owner, id, false); err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `SELECT id::text,version,revision,schema_version,NULL::jsonb,created_at FROM platform.workflow_versions
   WHERE owner_id=$1 AND workflow_id=$2 AND ($3::bigint IS NULL OR version<$3) ORDER BY version DESC LIMIT $4`, owner, id, before, limit)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var v workflow.Version
			if err := readVersion(rows, &v); err != nil {
				return err
			}
			list = append(list, v)
		}
		return rows.Err()
	})
	return list, err
}

func event(ctx context.Context, tx pgx.Tx, kind, owner, id, key string, revision int64, versionID string) error {
	payload := map[string]any{"owner_id": owner, "workflow_id": id, "revision": revision}
	if versionID != "" {
		payload["version_id"] = versionID
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `INSERT INTO platform.outbox_events(type,logical_key,payload) VALUES($1,coalesce(nullif($2,''),gen_random_uuid()::text),$3)`, kind, key, raw)
	return err
}

// RunnableVersion selects an exact historical version or the current active
// version. The execution stage must recheck these invariants inside its enqueue transaction.
func (s *Store) RunnableVersion(ctx context.Context, owner, id, versionID string) (workflow.Version, error) {
	var v workflow.Version
	err := s.owned(ctx, owner, false, func(tx pgx.Tx) error {
		d, err := load(ctx, tx, owner, id, false)
		if err != nil {
			return err
		}
		if d.Status == "archived" {
			return workflow.ErrArchived
		}
		if versionID == "" && d.ActiveVersionID != nil {
			versionID = *d.ActiveVersionID
		}
		if !workflow.ValidID(versionID) {
			return workflow.ErrNotFound
		}
		if err := readVersion(tx.QueryRow(ctx, "SELECT "+versionColumns+" FROM platform.workflow_versions WHERE owner_id=$1 AND workflow_id=$2 AND id=$3", owner, id, versionID), &v); err != nil {
			return err
		}
		_, fields, err := s.validate(ctx, tx, owner, v.Definition)
		if err != nil {
			return err
		}
		if len(fields) > 0 {
			return &workflow.Invalid{Fields: fields}
		}
		return nil
	})
	return v, err
}

type Snapshot struct {
	ID            string
	Revision      int64
	SchemaVersion int
	Definition    json.RawMessage
}

func (s *Store) CreateSnapshot(ctx context.Context, owner, id string, expected int64) (Snapshot, error) {
	var snapshot Snapshot
	err := s.owned(ctx, owner, true, func(tx pgx.Tx) error {
		d, err := load(ctx, tx, owner, id, true)
		if err != nil {
			return err
		}
		if d.Status == "archived" {
			return workflow.ErrArchived
		}
		if d.Draft.Revision != expected {
			return workflow.ErrConflict
		}
		def, fields, err := s.validate(ctx, tx, owner, d.Draft.Definition)
		if err != nil {
			return err
		}
		if len(fields) > 0 {
			return &workflow.Invalid{Fields: fields}
		}
		return tx.QueryRow(ctx, `INSERT INTO platform.draft_execution_snapshots(owner_id,workflow_id,revision,schema_version,definition)
    VALUES($1,$2,$3,$4,$5) RETURNING id::text,revision,schema_version,definition`, owner, id, expected, def.SchemaVersion, d.Draft.Definition).
			Scan(&snapshot.ID, &snapshot.Revision, &snapshot.SchemaVersion, &snapshot.Definition)
	})
	return snapshot, err
}
