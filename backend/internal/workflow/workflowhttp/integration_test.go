package workflowhttp_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"

	"autolife/backend/internal/accounts"
	"autolife/backend/internal/platform/httpserver"
	"autolife/backend/internal/workflow"
	"autolife/backend/internal/workflow/postgres"
	"autolife/backend/internal/workflow/workflowhttp"
	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestWorkflowAPIAndPersistence(t *testing.T) {
	ctx := t.Context()
	connect := func(key string) *pgxpool.Pool {
		t.Helper()
		dsn := os.Getenv(key)
		if dsn == "" {
			t.Skip("requires a migrated PostgreSQL 17 with API and migration roles")
		}
		p, err := pgxpool.New(ctx, dsn)
		if err != nil {
			t.Fatal("invalid test database configuration")
		}
		t.Cleanup(p.Close)
		return p
	}
	api, admin := connect("AUTOLIFE_API_DATABASE_URL"), connect("AUTOLIFE_MIGRATION_DATABASE_URL")
	type identity struct{ id, token, csrf string }
	makeIdentity := func() identity {
		t.Helper()
		var user identity
		if err := admin.QueryRow(ctx, `INSERT INTO platform.users(normalized_email,password_hash,email_verified_at)
   VALUES(gen_random_uuid()::text||'@workflow.example.test','fixture',now()) RETURNING id::text`).Scan(&user.id); err != nil {
			t.Fatal(err)
		}
		var token [32]byte
		if _, err := rand.Read(token[:]); err != nil {
			t.Fatal(err)
		}
		user.token = base64.RawURLEncoding.EncodeToString(token[:])
		digest := sha256.Sum256([]byte(user.token))
		csrf := sha256.Sum256([]byte("autolife-csrf:session:" + user.token))
		user.csrf = base64.RawURLEncoding.EncodeToString(csrf[:])
		if _, err := admin.Exec(ctx, "INSERT INTO platform.sessions(user_id,token_hash,expires_at) VALUES($1,$2,now()+interval '1 hour')", user.id, digest[:]); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			for _, query := range []string{"DELETE FROM platform.outbox_events WHERE payload->>'owner_id'=$1", "DELETE FROM platform.files WHERE owner_id=$1", "DELETE FROM platform.users WHERE id=$1"} {
				if _, err := admin.Exec(context.Background(), query, user.id); err != nil {
					t.Error(err)
				}
			}
		})
		return user
	}
	owner, other := makeIdentity(), makeIdentity()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	auth, err := accounts.NewHTTP(accounts.NewStore(api), logger, "http://127.0.0.1:8080", "127.0.0.1:1025", strings.Repeat("2a", 32))
	if err != nil {
		t.Fatal(err)
	}
	registry := workflow.NewRegistry()
	enabled := true
	key := workflow.Key{Type: "fixture.summarize", Version: 1}
	if err := registry.RegisterAction(workflow.ActionDefinition{
		Descriptor: workflow.Descriptor{Key: key, Inputs: workflow.Fields{"source": {Kind: "file", Formats: []string{"image/png"}}}, Outputs: workflow.Fields{"label": {Kind: "string"}}},
		Available:  func() bool { return enabled },
		Handler: func(context.Context, workflow.Values, workflow.Values) (workflow.Values, error) {
			return workflow.Values{"label": json.RawMessage(`"fixture"`)}, nil
		},
	}); err != nil {
		t.Fatal(err)
	}
	store := postgres.New(api, registry)
	routes := workflowhttp.API{Store: store, Registry: registry}
	handler := httpserver.Handler(logger, 1<<20, func(context.Context) error { return nil }, func(r chi.Router) {
		routes.Routes(r, auth.RequireSession, auth.ProtectSession, accounts.SessionOwner)
	})
	request := func(user identity, hasCSRF bool, method, path, body string, status int) []byte {
		t.Helper()
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		if user.token != "" {
			req.AddCookie(&http.Cookie{Name: "autolife-session", Value: user.token})
		}
		req.Header.Set("Origin", "http://127.0.0.1:8080")
		if hasCSRF {
			req.Header.Set("X-CSRF-Token", user.csrf)
		}
		res := httptest.NewRecorder()
		handler.ServeHTTP(res, req)
		if res.Code != status {
			t.Fatalf("%s %s: expected %d, got %d %s", method, path, status, res.Code, res.Body)
		}
		return res.Body.Bytes()
	}
	decode := func(body []byte, into any) {
		t.Helper()
		if err := json.Unmarshal(body, into); err != nil {
			t.Fatal(err)
		}
	}
	base := "/api/v1/workflows"
	request(identity{}, false, "GET", base, "", 401)
	request(owner, false, "POST", base, `{"name":"no csrf"}`, 403)
	if body := request(owner, false, "GET", "/api/v1/triggers", "", 200); !bytes.Contains(body, []byte("core.manual")) {
		t.Fatal("manual trigger missing")
	}
	if body := request(owner, false, "GET", "/api/v1/actions", "", 200); !bytes.Contains(body, []byte(key.Type)) {
		t.Fatal("fixture registry missing")
	}
	var created workflow.Details
	decode(request(owner, true, "POST", base, `{"name":"Proof","description":"a private description"}`, 201), &created)
	if created.Draft.Revision != 1 || created.ActiveVersionID != nil {
		t.Fatalf("invalid initial draft: %+v", created)
	}
	path := base + "/" + created.ID
	request(other, false, "GET", path, "", 404)
	for _, variant := range []struct{ method, suffix, body string }{
		{"POST", "/copy", "{}"}, {"POST", "/validate", "{}"}, {"POST", "/versions", `{"expected_revision":1}`},
		{"PUT", "/draft", `{"expected_revision":1,"definition":{}}`}, {"PATCH", "", `{"name":"foreign"}`}, {"DELETE", "", ""},
	} {
		request(other, true, variant.method, path+variant.suffix, variant.body, 404)
	}
	request(other, false, "GET", path+"/versions", "", 404)
	request(owner, true, "PATCH", path, `{"name":"Revised"}`, 200)
	request(owner, true, "PUT", path+"/draft", `{"expected_revision":1,"definition":{"steps":[{}]}}`, 200)
	request(owner, true, "PUT", path+"/draft", `{"expected_revision":1,"definition":{}}`, 409)
	request(owner, true, "POST", path+"/versions", `{"expected_revision":2}`, 422)
	var validation struct {
		Valid  bool
		Errors []workflow.Problem
	}
	decode(request(owner, true, "POST", path+"/validate", "", 200), &validation)
	if validation.Valid || len(validation.Errors) == 0 {
		t.Fatal("partial definition validated")
	}
	makeFile := func(user identity) string {
		t.Helper()
		var id string
		err := admin.QueryRow(ctx, `INSERT INTO platform.files(owner_id,display_name,mime,bytes,sha256,state,ready_at,expires_at)
   VALUES($1,'fixture.png','image/png',10,$2,'ready',now(),now()+interval '1 hour') RETURNING id::text`, user.id, make([]byte, 32)).Scan(&id)
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	def := func(file string) json.RawMessage {
		return json.RawMessage(`{"schema_version":1,"trigger":{"type":"core.manual","version":1,"config":{}},"steps":[
    {"id":"a","action_type":"fixture.summarize","action_version":1,"input_bindings":{"source":{"source":"constant","value":"` + file + `"}}}]}`)
	}
	foreign := makeFile(other)
	decode(request(owner, true, "POST", path+"/validate", `{"definition":`+string(def(foreign))+"}", 200), &validation)
	if validation.Valid || !strings.Contains(validation.Errors[0].Code, "FILE_UNAVAILABLE") {
		t.Fatalf("foreign file binding: %+v", validation)
	}
	safe := makeFile(owner)
	validDef := def(safe)
	var draft workflow.Draft
	decode(request(owner, true, "PUT", path+"/draft", `{"expected_revision":2,"definition":`+string(validDef)+"}", 200), &draft)
	if draft.Revision != 3 {
		t.Fatal(draft.Revision)
	}
	snapshot, err := store.CreateSnapshot(ctx, owner.id, created.ID, draft.Revision)
	if err != nil {
		t.Fatal(err)
	}
	f, err := store.Get(ctx, owner.id, created.ID)
	if err != nil || f.ActiveVersionID != nil {
		t.Fatal("creating a snapshot activated a version")
	}
	assertDBError := func(err error, code string) {
		t.Helper()
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != code {
			t.Fatalf("expected SQLSTATE %s: %v", code, err)
		}
	}
	_, err = admin.Exec(ctx, "UPDATE platform.draft_execution_snapshots SET revision=revision+1 WHERE id=$1", snapshot.ID)
	assertDBError(err, "55000")
	_, err = admin.Exec(ctx, `INSERT INTO platform.draft_execution_snapshots(owner_id,workflow_id,revision,schema_version,definition)
  VALUES($1,$2,1,1,'{"schema_version":1}')`, other.id, created.ID)
	assertDBError(err, "23503")

	// Only one editor can publish a particular draft revision.
	type outcome struct {
		v   workflow.Version
		err error
	}
	channel := make(chan outcome, 2)
	start := make(chan struct{})
	var wait sync.WaitGroup
	for range 2 {
		wait.Go(func() { <-start; v, err := store.Publish(ctx, owner.id, created.ID, 3); channel <- outcome{v, err} })
	}
	close(start)
	wait.Wait()
	first, second := <-channel, <-channel
	if first.err != nil {
		first, second = second, first
	}
	if first.err != nil || !errors.Is(second.err, workflow.ErrConflict) || first.v.Number != 1 {
		t.Fatalf("publication race: %v %v", first.err, second.err)
	}
	request(owner, true, "POST", path+"/versions", `{"expected_revision":3}`, 409)
	_, err = admin.Exec(ctx, "UPDATE platform.workflow_versions SET definition='{}' WHERE id=$1", first.v.ID)
	assertDBError(err, "55000")
	// A second workflow cannot adopt the first workflow's active version.
	var copied workflow.Details
	decode(request(owner, true, "POST", path+"/copy", `{"name":"Copy"}`, 201), &copied)
	if copied.ID == created.ID || copied.ActiveVersionID != nil || copied.Draft.Revision != 1 {
		t.Fatal("copy shares history")
	}
	_, err = admin.Exec(ctx, "UPDATE platform.workflows SET active_version_id=$1 WHERE id=$2", first.v.ID, copied.ID)
	assertDBError(err, "23503")
	err = pgx.BeginFunc(ctx, api, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, "SELECT set_config('autolife.workflow_owner',$1,true)", other.id); err != nil {
			return err
		}
		var count int
		if err := tx.QueryRow(ctx, "SELECT count(*) FROM platform.workflow_versions WHERE id=$1", first.v.ID).Scan(&count); err != nil {
			return err
		}
		if count != 0 {
			t.Error("RLS exposed the version")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	request(owner, true, "PUT", base+"/"+copied.ID+"/draft", `{"expected_revision":1,"definition":{}}`, 200)
	current, err := store.Get(ctx, owner.id, created.ID)
	if err != nil || current.Draft.Revision != 3 {
		t.Fatal("copy modified the source")
	}
	replacement := makeFile(owner)
	newDef := def(replacement)
	request(owner, true, "PUT", path+"/draft", `{"expected_revision":3,"definition":`+string(newDef)+"}", 200)
	var latest workflow.Version
	decode(request(owner, true, "POST", path+"/versions", `{"expected_revision":4}`, 201), &latest)
	if latest.Number != 2 {
		t.Fatalf("version sequence: %d", latest.Number)
	}
	historical, err := store.RunnableVersion(ctx, owner.id, created.ID, first.v.ID)
	if err != nil || !bytes.Contains(historical.Definition, []byte(safe)) || bytes.Contains(historical.Definition, []byte(replacement)) {
		t.Fatal("historical version changed")
	}
	var versions struct {
		Versions []workflow.Version
		Next     string `json:"next_cursor"`
	}
	decode(request(owner, false, "GET", path+"/versions?limit=1", "", 200), &versions)
	if len(versions.Versions) != 1 || versions.Versions[0].Number != 2 || versions.Next == "" {
		t.Fatal("version pagination")
	}
	decode(request(owner, false, "GET", path+"/versions?limit=1&cursor="+versions.Next, "", 200), &versions)
	if len(versions.Versions) != 1 || versions.Versions[0].ID != first.v.ID || versions.Next != "" {
		t.Fatal("version history includes snapshot or wrong cursor")
	}
	var page struct {
		Workflows []workflow.Workflow
		Next      string `json:"next_cursor"`
	}
	decode(request(owner, false, "GET", base+"?limit=1", "", 200), &page)
	if len(page.Workflows) != 1 || page.Next == "" {
		t.Fatal("workflow list did not paginate")
	}
	request(other, false, "GET", base+"?limit=1&cursor="+page.Next, "", 400)
	enabled = false
	if _, err := store.RunnableVersion(ctx, owner.id, created.ID, first.v.ID); err == nil {
		t.Fatal("unavailable historical action was substituted")
	}
	enabled = true
	request(owner, true, "DELETE", path, "", 204)
	request(owner, true, "DELETE", path, "", 204)
	request(owner, true, "PUT", path+"/draft", `{"expected_revision":4,"definition":{}}`, 409)
	if _, err := store.RunnableVersion(ctx, owner.id, created.ID, first.v.ID); !errors.Is(err, workflow.ErrArchived) {
		t.Fatalf("archived runnable: %v", err)
	}
	if _, err := store.CreateSnapshot(ctx, owner.id, created.ID, 4); !errors.Is(err, workflow.ErrArchived) {
		t.Fatalf("archived snapshot: %v", err)
	}
	var activated, leaked int
	if err := admin.QueryRow(ctx, `SELECT count(*) FILTER(WHERE type='workflow.activated'),count(*) FILTER(WHERE payload ? 'name' OR payload ? 'definition' OR payload ? 'config')
  FROM platform.outbox_events WHERE payload->>'workflow_id'=$1`, created.ID).Scan(&activated, &leaked); err != nil || activated != 2 || leaked != 0 {
		t.Fatalf("outbox activated=%d unsafe=%d %v", activated, leaked, err)
	}
	// Account deletion cascades through draft, versions, snapshot and active FK.
	if _, err := admin.Exec(ctx, "DELETE FROM platform.files WHERE owner_id=$1", owner.id); err != nil {
		t.Fatal(err)
	}
	if _, err := admin.Exec(ctx, "DELETE FROM platform.users WHERE id=$1", owner.id); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := admin.QueryRow(ctx, `SELECT (SELECT count(*) FROM platform.workflows WHERE owner_id=$1)
  +(SELECT count(*) FROM platform.workflow_drafts WHERE owner_id=$1)
  +(SELECT count(*) FROM platform.workflow_versions WHERE owner_id=$1)
  +(SELECT count(*) FROM platform.draft_execution_snapshots WHERE owner_id=$1)`, owner.id).Scan(&count); err != nil || count != 0 {
		t.Fatalf("cascade: %d %v", count, err)
	}
}

func TestProductionRegistryHasNoFixtureActions(t *testing.T) {
	r := workflow.NewRegistry()
	if got := r.Actions(); len(got) != 0 {
		t.Fatalf("early or fixture handlers: %+v", got)
	}
	if got := r.Triggers(); len(got) != 1 || got[0].Type != "core.manual" {
		t.Fatalf("triggers: %+v", got)
	}
}
