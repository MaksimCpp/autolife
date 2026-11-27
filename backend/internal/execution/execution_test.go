package execution

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"autolife/backend/internal/workflow"
	workflowdb "autolife/backend/internal/workflow/postgres"
	"autolife/backend/internal/workflow/workflowhttp"
	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
	"github.com/riverqueue/river/rivermigrate"
)

func TestStateTransitions(t *testing.T) {
	all := []State{Queued, Running, Retrying, CancelRequested, Succeeded, Failed, Cancelled, TimedOut}
	allowed := map[State][]State{
		Queued:          {Running, CancelRequested, TimedOut},
		Running:         {Retrying, CancelRequested, Succeeded, Failed, TimedOut},
		Retrying:        {Queued, CancelRequested, TimedOut},
		CancelRequested: {Cancelled},
	}
	for _, from := range all {
		for _, to := range all {
			expected := false
			for _, possible := range allowed[from] {
				if possible == to {
					expected = true
				}
			}
			if from.CanBecome(to) != expected {
				t.Errorf("%s -> %s", from, to)
			}
		}
	}
	now := time.Now()
	if got := (Execution{StartDeadline: now}).Deadline(); !got.Equal(now) {
		t.Fatal(got)
	}
	later := now.Add(time.Minute)
	if got := (Execution{StartDeadline: now, RunDeadline: &later}).Deadline(); !got.Equal(later) {
		t.Fatal(got)
	}
	for n := 1; n <= 3; n++ {
		delay := retryDelay(n)
		if delay < 5*time.Second || delay > 30*time.Second {
			t.Fatal(delay)
		}
	}
}

func TestExecutionLifecycle(t *testing.T) {
	ctx := t.Context()
	connect := func(name string) *pgxpool.Pool {
		t.Helper()
		dsn := os.Getenv(name)
		if dsn == "" {
			t.Skip("requires isolated migrated PostgreSQL for API, worker, migrate")
		}
		pool, err := pgxpool.New(ctx, dsn)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(pool.Close)
		return pool
	}
	api, worker, admin := connect("AUTOLIFE_API_DATABASE_URL"), connect("AUTOLIFE_WORKER_DATABASE_URL"), connect("AUTOLIFE_MIGRATION_DATABASE_URL")
	migrator, err := rivermigrate.New(riverpgxv5.New(admin), &rivermigrate.Config{Schema: "queue"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := migrator.Migrate(ctx, rivermigrate.DirectionUp, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := admin.Exec(ctx, "GRANT UPDATE(kind) ON queue.river_job TO autolife_api"); err != nil {
		t.Fatal(err)
	}
	fixture := func() string {
		t.Helper()
		var id string
		if err := admin.QueryRow(ctx, `INSERT INTO platform.users(normalized_email,password_hash,email_verified_at)
    VALUES(gen_random_uuid()::text||'@execution.example.test','fixture',now()) RETURNING id::text`).Scan(&id); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _, _ = admin.Exec(context.Background(), "DELETE FROM platform.users WHERE id=$1", id) })
		return id
	}
	owner, other := fixture(), fixture()
	registry := workflow.NewRegistry()
	var calls atomic.Int64
	var firstCalls atomic.Int64
	staged := make(chan string, 1)
	readStage := make(chan struct{}, 1)
	releaseStage := make(chan struct{})
	var proof atomic.Bool
	var invokedNative atomic.Bool
	for _, kind := range []string{"fixture.first", "fixture.second", "fixture.block", "fixture.stage", "fixture.read_stage", "fixture.native", "fixture.unknown"} {
		descriptor := workflow.Descriptor{Key: workflow.Key{Type: kind, Version: 1}, MaxAttempts: 3, Outputs: workflow.Fields{"label": {Kind: "string"}}}
		if kind == "fixture.second" {
			descriptor.Inputs = workflow.Fields{"label": {Kind: "string"}}
		}
		if kind == "fixture.stage" {
			descriptor.Outputs = workflow.Fields{"file": {Kind: "file", Formats: []string{"image/png"}}}
		}
		if kind == "fixture.read_stage" {
			descriptor.Inputs = workflow.Fields{"file": {Kind: "file", Formats: []string{"image/png"}}}
		}
		var confirm func(context.Context, string) (bool, error)
		if kind == "fixture.native" {
			descriptor.ResourceProfile = "image"
			confirm = func(_ context.Context, id string) (bool, error) { return workflow.ValidID(id) && proof.Load(), nil }
		}
		err := registry.RegisterAction(workflow.ActionDefinition{Descriptor: descriptor, SafeReplay: true, ConfirmStopped: confirm,
			Handler: func(ctx context.Context, _ workflow.Values, in workflow.Values) (workflow.Values, error) {
				switch kind {
				case "fixture.first":
					firstCalls.Add(1)
				case "fixture.second":
					if string(in["label"]) != `"fixture"` {
						return nil, errors.New("checkpoint not propagated")
					}
					if calls.Add(1) == 1 {
						return nil, ErrTemporary
					}
				case "fixture.block":
					<-ctx.Done()
					return nil, ctx.Err()
				case "fixture.stage":
					content := []byte("fixture")
					id, err := StageFile(ctx, VerifiedFile{Name: "fixture.png", MIME: "image/png", Bytes: int64(len(content)), SHA256: sha256.Sum256(content)},
						func(_ context.Context, key string) error {
							if len(key) != 42 {
								return errors.New("unexpected storage key")
							}
							return nil
						})
					if err != nil {
						return nil, err
					}
					staged <- id
					return workflow.Values{"file": json.RawMessage(`"` + id + `"`)}, nil
				case "fixture.read_stage":
					var file string
					_ = json.Unmarshal(in["file"], &file)
					if key, err := ObjectKey(ctx, file); err != nil || key != "files/"+file {
						return nil, errors.New("private checkpoint unavailable")
					}
					readStage <- struct{}{}
					select {
					case <-releaseStage:
					case <-ctx.Done():
						return nil, ctx.Err()
					}
				case "fixture.native":
					invokedNative.Store(true)
					<-ctx.Done()
					return nil, ctx.Err()
				case "fixture.unknown":
					return nil, ErrUnknownOutcome
				}
				return workflow.Values{"label": json.RawMessage(`"fixture"`)}, nil
			}})
		if err != nil {
			t.Fatal(err)
		}
	}
	apiStore, err := New(api, registry, false)
	if err != nil {
		t.Fatal(err)
	}
	workerStore, err := New(worker, registry, true)
	if err != nil {
		t.Fatal(err)
	}
	definitions := workflowdb.New(api, registry)
	create := func(d string) workflow.Details {
		t.Helper()
		w, err := definitions.Create(ctx, owner, "Workflow", "")
		if err != nil {
			t.Fatal(err)
		}
		draft, err := definitions.SaveDraft(ctx, owner, w.ID, 1, json.RawMessage(d))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := definitions.Publish(ctx, owner, w.ID, draft.Revision); err != nil {
			t.Fatal(err)
		}
		return w
	}
	w := create(`{"schema_version":1,"trigger":{"type":"core.manual","version":1},"steps":[
  {"id":"first","action_type":"fixture.first","action_version":1},
  {"id":"second","action_type":"fixture.second","action_version":1,"input_bindings":{"label":{"source":"first","output":"label"}}}]}`)
	var inputID string
	if err := admin.QueryRow(ctx, `INSERT INTO platform.files(owner_id,display_name,mime,bytes,sha256,state,ready_at,expires_at)
  VALUES($1,'execution-input.png','image/png',7,$2,'ready',now(),now()+interval '1 hour') RETURNING id::text`, owner, make([]byte, 32)).Scan(&inputID); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = admin.Exec(context.Background(), "DELETE FROM platform.files WHERE id=$1", inputID) })
	inputFiles := []string{inputID}
	initial := Start{Files: &inputFiles}
	failedStore := &Store{Pool: api, Registry: registry}
	if _, err := failedStore.Start(ctx, owner, w.ID, "rolled-back", initial, false); err == nil {
		t.Fatal("expected enqueue failure")
	}
	for _, table := range []string{"executions", "execution_reservations", "execution_idempotency", "file_pins"} {
		var count int
		if err := admin.QueryRow(ctx, "SELECT count(*) FROM platform."+table+" WHERE owner_id=$1", owner).Scan(&count); err != nil || count != 0 {
			t.Fatalf("rollback %s: %d %v", table, count, err)
		}
	}
	router := chi.NewRouter()
	pass := func(h http.Handler) http.Handler { return h }
	identity := func(*http.Request) string { return owner }
	(workflowhttp.API{Store: definitions, Registry: registry}).Routes(router, pass, pass, identity)
	apiStore.Routes(router, pass, pass, identity)
	body, err := json.Marshal(initial)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/api/v1/workflows/"+w.ID+"/executions", strings.NewReader(string(body)))
	request.Header.Set("Idempotency-Key", "run")
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	if response.Code != 202 {
		t.Fatalf("start HTTP: %d %s", response.Code, response.Body)
	}
	var receipt struct{ ID string }
	if err := json.Unmarshal(response.Body.Bytes(), &receipt); err != nil {
		t.Fatal(err)
	}
	detail, err := apiStore.Details(ctx, owner, receipt.ID)
	if err != nil {
		t.Fatal(err)
	}
	started := detail.Execution
	again, err := apiStore.Start(ctx, owner, w.ID, "run", initial, false)
	if err != nil || started.ID != again.ID {
		t.Fatalf("idempotency %v", err)
	}
	if _, err := apiStore.Start(ctx, owner, w.ID, "run", Start{VersionID: "different"}, false); !errors.Is(err, ErrIdempotency) {
		t.Fatalf("key conflict: %v", err)
	}
	snapshot, err := apiStore.Start(ctx, owner, w.ID, "test", Start{ExpectedRevision: 2}, true)
	if err != nil || snapshot.SnapshotID == nil || snapshot.VersionID != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if _, err := apiStore.Start(ctx, owner, w.ID, "over-quota", Start{}, false); !errors.Is(err, ErrQuota) {
		t.Fatalf("quota %v", err)
	}
	if _, err := apiStore.Details(ctx, other, started.ID); !errors.Is(err, workflow.ErrNotFound) {
		t.Fatalf("foreign read: %v", err)
	}
	if _, err := apiStore.Cancel(ctx, owner, snapshot.ID); err != nil {
		t.Fatal(err)
	}
	if err := workerStore.Queue.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := workerStore.Stop(); err != nil {
			t.Error(err)
		}
	})
	wait := func(id string, status State) Details {
		t.Helper()
		deadline := time.Now().Add(18 * time.Second)
		for time.Now().Before(deadline) {
			detail, err := apiStore.Details(ctx, owner, id)
			if err != nil {
				t.Fatal(err)
			}
			if detail.Status == status {
				return detail
			}
			if detail.Status.Terminal() {
				t.Fatalf("unexpected terminal %s: %v", detail.Status, detail.ErrorCode)
			}
			time.Sleep(50 * time.Millisecond)
		}
		var statusText, latest string
		_ = admin.QueryRow(ctx, "SELECT status,coalesce(error_code,'') FROM platform.executions WHERE id=$1", id).Scan(&statusText, &latest)
		t.Fatalf("waiting for %s: status=%s code=%s", status, statusText, latest)
		return Details{}
	}
	wait(snapshot.ID, Cancelled)
	result := wait(started.ID, Succeeded)
	if firstCalls.Load() != 1 || calls.Load() != 2 || len(result.Steps[1].Attempts) != 2 {
		t.Fatal("checkpoint or attempt accounting failed")
	}
	if got := string(result.Outputs["second"]["label"]); got != `"fixture"` {
		t.Fatal(got)
	}
	var pins int
	if err := admin.QueryRow(ctx, "SELECT count(*) FROM platform.file_pins WHERE file_id=$1", inputID).Scan(&pins); err != nil || pins != 0 {
		t.Fatalf("released pins: %d %v", pins, err)
	}
	retry, err := apiStore.Retry(ctx, owner, started.ID, "retry", Start{})
	if err != nil || retry.ID == started.ID || *retry.VersionID != *started.VersionID {
		t.Fatalf("historical retry: %v", err)
	}
	wait(retry.ID, Succeeded)
	if _, err := admin.Exec(ctx, "UPDATE platform.files SET ready_at=now()-interval '2 hours',expires_at=now()-interval '1 hour' WHERE id=$1", inputID); err != nil {
		t.Fatal(err)
	}
	if _, err := apiStore.Retry(ctx, owner, started.ID, "stale", Start{}); !errors.Is(err, ErrInputs) {
		t.Fatalf("expired input retry: %v", err)
	}
	if usage, err := apiStore.Usage(ctx, owner); err != nil || usage.Active != 0 {
		t.Fatalf("reservations: %+v %v", usage, err)
	}
	blocked := create(`{"schema_version":1,"trigger":{"type":"core.manual","version":1},"steps":[{"id":"block","action_type":"fixture.block","action_version":1}]}`)
	active, err := apiStore.Start(ctx, owner, blocked.ID, "active", Start{}, false)
	if err != nil {
		t.Fatal(err)
	}
	wait(active.ID, Running)
	if _, err := apiStore.Cancel(ctx, owner, active.ID); err != nil {
		t.Fatal(err)
	}
	wait(active.ID, Cancelled)
	timed, err := apiStore.Start(ctx, owner, blocked.ID, "run-deadline", Start{}, false)
	if err != nil {
		t.Fatal(err)
	}
	wait(timed.ID, Running)
	if _, err := admin.Exec(ctx, "UPDATE platform.executions SET run_deadline=clock_timestamp() WHERE id=$1", timed.ID); err != nil {
		t.Fatal(err)
	}
	wait(timed.ID, TimedOut)
	artifacts := create(`{"schema_version":1,"trigger":{"type":"core.manual","version":1},"steps":[
	 {"id":"stage","action_type":"fixture.stage","action_version":1},
	 {"id":"read","action_type":"fixture.read_stage","action_version":1,"input_bindings":{"file":{"source":"stage","output":"file"}}}]}`)
	pending, err := apiStore.Start(ctx, owner, artifacts.ID, "artifacts", Start{}, false)
	if err != nil {
		t.Fatal(err)
	}
	var fileID string
	select {
	case fileID = <-staged:
	case <-time.After(10 * time.Second):
		t.Fatal("stage not created")
	}
	select {
	case <-readStage:
	case <-time.After(10 * time.Second):
		t.Fatal("stage not available as checkpoint")
	}
	var state string
	if err := admin.QueryRow(ctx, "SELECT state FROM platform.files WHERE id=$1", fileID).Scan(&state); err != nil || state != "uploading" {
		t.Fatalf("premature exposure %s %v", state, err)
	}
	close(releaseStage)
	completed := wait(pending.ID, Succeeded)
	if len(completed.Files) != 1 || completed.Files[0].Expired || completed.Files[0].ID != fileID {
		t.Fatalf("published artifact %+v", completed.Files)
	}
	if err := admin.QueryRow(ctx, "SELECT state FROM platform.files WHERE id=$1", fileID).Scan(&state); err != nil || state != "ready" {
		t.Fatalf("artifact not promoted %s %v", state, err)
	}
	if _, err := admin.Exec(ctx, "DELETE FROM platform.files WHERE id=$1", fileID); err != nil {
		t.Fatal(err)
	}
	expired, err := apiStore.Details(ctx, owner, pending.ID)
	if err != nil || len(expired.Files) != 1 || !expired.Files[0].Expired {
		t.Fatalf("missing expiry tombstone: %+v %v", expired.Files, err)
	}

	unknown := create(`{"schema_version":1,"trigger":{"type":"core.manual","version":1},"steps":[{"id":"unknown","action_type":"fixture.unknown","action_version":1}]}`)
	u, err := apiStore.Start(ctx, owner, unknown.ID, "unknown", Start{}, false)
	if err != nil {
		t.Fatal(err)
	}
	failed := wait(u.ID, Failed)
	if len(failed.Steps[0].Attempts) != 1 || failed.ErrorCode == nil || *failed.ErrorCode != "UNKNOWN_OUTCOME" {
		t.Fatalf("unknown failure retry: %+v", failed)
	}

	native := create(`{"schema_version":1,"trigger":{"type":"core.manual","version":1},"steps":[{"id":"native","action_type":"fixture.native","action_version":1}]}`)
	nativeRun, err := apiStore.Start(ctx, owner, native.ID, "native", Start{}, false)
	if err != nil {
		t.Fatal(err)
	}
	for end := time.Now().Add(5 * time.Second); !invokedNative.Load() && time.Now().Before(end); {
		time.Sleep(10 * time.Millisecond)
	}
	if !invokedNative.Load() {
		t.Fatal("isolated test action did not start")
	}
	if _, err := apiStore.Cancel(ctx, owner, nativeRun.ID); err != nil {
		t.Fatal(err)
	}
	time.Sleep(1300 * time.Millisecond)
	wait(nativeRun.ID, CancelRequested)
	if usage, err := apiStore.Usage(ctx, owner); err != nil || usage.Active != 1 {
		t.Fatalf("released before stopped proof: %+v %v", usage, err)
	}
	proof.Store(true)
	if _, err := admin.Exec(ctx, "UPDATE platform.executions SET lease_until=clock_timestamp()-interval '1 second' WHERE id=$1", nativeRun.ID); err != nil {
		t.Fatal(err)
	}
	if err := workerStore.run(ctx, nativeRun.ID); err != nil {
		t.Fatal(err)
	}
	wait(nativeRun.ID, Cancelled)

	if err := workerStore.Stop(); err != nil {
		t.Fatal(err)
	}
	// A durable queued job can be picked up by a new worker, including when old requests race the quota.
	var contenders sync.WaitGroup
	var success, denied atomic.Int64
	for i := range 5 {
		contenders.Go(func() {
			_, err := apiStore.Start(ctx, owner, w.ID, "race-"+string(rune('a'+i)), Start{}, false)
			switch {
			case err == nil:
				success.Add(1)
			case errors.Is(err, ErrQuota):
				denied.Add(1)
			default:
				t.Error(err)
			}
		})
	}
	contenders.Wait()
	if success.Load() != 2 || denied.Load() != 3 {
		t.Fatalf("concurrent quota: %d / %d", success.Load(), denied.Load())
	}
	queued, err := apiStore.List(ctx, owner, w.ID, string(Queued), "", nil, 10)
	if err != nil || len(queued) != 2 {
		t.Fatalf("queued list: %d %v", len(queued), err)
	}
	old := queued[0]
	if _, err := admin.Exec(ctx, "DELETE FROM queue.river_job WHERE args->>'execution_id'=$1", old.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := admin.Exec(ctx, "UPDATE platform.executions SET queued_at=clock_timestamp()-interval '30 seconds' WHERE id=$1", old.ID); err != nil {
		t.Fatal(err)
	}
	if err := workerStore.reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	var jobs int
	if err := admin.QueryRow(ctx, "SELECT count(*) FROM queue.river_job WHERE args->>'execution_id'=$1", old.ID).Scan(&jobs); err != nil || jobs != 1 {
		t.Fatalf("lost delivery recovery: %d %v", jobs, err)
	}
	if _, err := admin.Exec(ctx, `UPDATE platform.executions SET start_deadline=clock_timestamp() WHERE id=$1`, old.ID); err != nil {
		t.Fatal(err)
	}
	if err := workerStore.run(ctx, old.ID); err != nil {
		t.Fatal(err)
	}
	wait(old.ID, TimedOut)
	crashed := queued[1]
	child := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestCrashFixtureProcess$")
	child.Env = append(os.Environ(), "AUTOLIFE_CRASH_FIXTURE_EXECUTION="+crashed.ID)
	diagnostics, err := child.CombinedOutput()
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 23 {
		t.Fatalf("child did not crash at the expected point: %v %s", err, diagnostics)
	}
	interrupted, err := apiStore.Details(ctx, owner, crashed.ID)
	if err != nil || interrupted.Status != Running || interrupted.Steps[0].Status != "succeeded" ||
		len(interrupted.Steps[1].Attempts) != 1 || interrupted.Steps[1].Attempts[0].State != "running" {
		t.Fatalf("crash did not preserve checkpoint and attempt: %+v %v", interrupted, err)
	}
	if _, err := admin.Exec(ctx, "UPDATE platform.executions SET lease_until=clock_timestamp()-interval '1 second' WHERE id=$1", crashed.ID); err != nil {
		t.Fatal(err)
	}
	firstBefore := firstCalls.Load()
	if err := workerStore.run(ctx, crashed.ID); err != nil {
		t.Fatal(err)
	}
	recovered := wait(crashed.ID, Succeeded)
	if firstCalls.Load() != firstBefore || len(recovered.Steps[1].Attempts) != 2 || recovered.Steps[1].Attempts[0].State != "interrupted" {
		t.Fatal("crash did not resume correctly")
	}
	oldFence := crashed
	oldFence.Fence = 1
	if _, err := workerStore.beginAttempt(ctx, worker, oldFence, 1, nil); !errors.Is(err, ErrLease) {
		t.Fatalf("stale fence: %v", err)
	}
	if err := definitions.Archive(ctx, owner, w.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := apiStore.Retry(ctx, owner, started.ID, "archived-retry", Start{}); !errors.Is(err, workflow.ErrArchived) {
		t.Fatalf("archive retry: %v", err)
	}

	if err := workerStore.consume(ctx); err != nil {
		t.Fatal(err)
	}
	var before, after int64
	if err := admin.QueryRow(ctx, "SELECT sum(count) FROM platform.execution_event_totals").Scan(&before); err != nil {
		t.Fatal(err)
	}
	if err := workerStore.consume(ctx); err != nil {
		t.Fatal(err)
	}
	if err := admin.QueryRow(ctx, "SELECT sum(count) FROM platform.execution_event_totals").Scan(&after); err != nil || before != after {
		t.Fatalf("double consumption: %d vs %d %v", before, after, err)
	}
}

func TestCrashFixtureProcess(t *testing.T) {
	id := os.Getenv("AUTOLIFE_CRASH_FIXTURE_EXECUTION")
	if id == "" {
		t.Skip("child subprocess for TestExecutionLifecycle")
	}
	pool, err := pgxpool.New(t.Context(), os.Getenv("AUTOLIFE_WORKER_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	registry := workflow.NewRegistry()
	for _, typeName := range []string{"fixture.first", "fixture.second"} {
		desc := workflow.Descriptor{Key: workflow.Key{Type: typeName, Version: 1}, MaxAttempts: 3, Outputs: workflow.Fields{"label": {Kind: "string"}}}
		if typeName == "fixture.second" {
			desc.Inputs = workflow.Fields{"label": {Kind: "string"}}
		}
		if err := registry.RegisterAction(workflow.ActionDefinition{Descriptor: desc, SafeReplay: true,
			Handler: func(context.Context, workflow.Values, workflow.Values) (workflow.Values, error) {
				if typeName == "fixture.second" {
					os.Exit(23)
				}
				return workflow.Values{"label": json.RawMessage(`"fixture"`)}, nil
			},
		}); err != nil {
			t.Fatal(err)
		}
	}
	store, err := New(pool, registry, true)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.run(t.Context(), id); err != nil {
		t.Fatal(err)
	}
	t.Fatal("child did not reach the abrupt exit")
}
