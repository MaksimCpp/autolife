package imageflow

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"image"
	"image/color"
	_ "image/jpeg"
	"image/png"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"autolife/backend/internal/execution"
	"autolife/backend/internal/fileflow"
	"autolife/backend/internal/workflow"
	workflowdb "autolife/backend/internal/workflow/postgres"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
	"github.com/riverqueue/river/rivermigrate"
)

type testLauncher struct {
	native        string
	nativeEnabled atomic.Bool
	fixture       []byte
	calls         atomic.Int64
	rejectAt      atomic.Int64
	retryAt       atomic.Int64
	block         atomic.Bool
	blocked       chan struct{}
}

func (*testLauncher) Available() bool                                      { return true }
func (*testLauncher) ConfirmStopped(context.Context, string) (bool, error) { return true, nil }

func (l *testLauncher) Transform(ctx context.Context, _ string, header []byte, input io.Reader, _ int64) (io.ReadCloser, int64, error) {
	count := l.calls.Add(1)
	if l.block.Load() {
		select {
		case l.blocked <- struct{}{}:
		default:
		}
		<-ctx.Done()
		return nil, 0, ctx.Err()
	}
	if l.retryAt.Load() == count {
		return nil, 0, execution.ErrTemporary
	}
	if l.rejectAt.Load() == count {
		return nil, 0, execution.ErrInputs
	}
	if l.nativeEnabled.Load() {
		command := exec.CommandContext(ctx, "docker", "run", "--rm", "-i", "--network", "none", "--read-only", "--cap-drop", "ALL",
			"--security-opt", "no-new-privileges", "--ipc", "none", "--user", "65534:65534", "--cpus", "1", "--memory", "512m", "--memory-swap", "512m",
			"--pids-limit", "32", "--tmpfs", "/work:rw,noexec,nosuid,nodev,size=384m,mode=1777", "--env", "TMPDIR=/work", l.native)
		command.Stdin = io.MultiReader(bytes.NewReader(header), input)
		data, err := command.Output()
		if err != nil {
			return nil, 0, err
		}
		return io.NopCloser(bytes.NewReader(data)), int64(len(data)), nil
	}
	_, err := io.Copy(io.Discard, input)
	if err != nil {
		return nil, 0, err
	}
	return io.NopCloser(bytes.NewReader(l.fixture)), int64(len(l.fixture)), nil
}

func TestImageWorkflows(t *testing.T) {
	ctx := t.Context()
	endpoint := os.Getenv("AUTOLIFE_TEST_S3_ENDPOINT")
	if endpoint == "" {
		t.Skip("requires a private local test bucket and PostgreSQL")
	}
	pool := func(key string) *pgxpool.Pool {
		t.Helper()
		dsn := os.Getenv(key)
		if dsn == "" {
			t.Skip("requires API, worker and migration DSN")
		}
		p, err := pgxpool.New(ctx, dsn)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(p.Close)
		return p
	}
	api, worker, admin := pool("AUTOLIFE_API_DATABASE_URL"), pool("AUTOLIFE_WORKER_DATABASE_URL"), pool("AUTOLIFE_MIGRATION_DATABASE_URL")
	migration, err := rivermigrate.New(riverpgxv5.New(admin), &rivermigrate.Config{Schema: "queue"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := migration.Migrate(ctx, rivermigrate.DirectionUp, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := admin.Exec(ctx, "GRANT UPDATE(kind) ON queue.river_job TO autolife_api"); err != nil {
		t.Fatal(err)
	}
	objects, err := fileflow.NewS3(fileflow.S3Config{Endpoint: endpoint,
		Bucket: "autolife", Region: "us-east-1", AllowHTTP: true, AccessKey: os.Getenv("AUTOLIFE_S3_ACCESS_KEY"), SecretKey: os.Getenv("AUTOLIFE_S3_SECRET_KEY")})
	if err != nil {
		t.Fatal(err)
	}
	var owner string
	if err := admin.QueryRow(ctx, `INSERT INTO platform.users(normalized_email,password_hash,email_verified_at)
 VALUES(gen_random_uuid()::text||'@images.example.test','fixture',now()) RETURNING id::text`).Scan(&owner); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		clean, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		rows, err := admin.Query(clean, "SELECT object_key FROM platform.files WHERE owner_id=$1", owner)
		if err != nil {
			t.Error(err)
			return
		}
		keys := []string{}
		for rows.Next() {
			var key string
			if rows.Scan(&key) == nil {
				keys = append(keys, key)
			}
		}
		rows.Close()
		for _, key := range keys {
			if err := objects.Delete(clean, key); err != nil {
				t.Error(err)
			}
		}
		for _, sql := range []string{"DELETE FROM platform.executions WHERE owner_id=$1", "DELETE FROM platform.file_pins WHERE owner_id=$1", "DELETE FROM platform.files WHERE owner_id=$1", "DELETE FROM platform.users WHERE id=$1", "DELETE FROM platform.outbox_events WHERE payload->>'owner_id'=$1"} {
			if _, err := admin.Exec(clean, sql, owner); err != nil {
				t.Error(err)
			}
		}
	})
	makeFixture := func(width int) []byte {
		t.Helper()
		im := image.NewNRGBA(image.Rect(0, 0, width, 20))
		for y := 0; y < 20; y++ {
			for x := 0; x < width; x++ {
				im.SetNRGBA(x, y, color.NRGBA{R: 240, G: 30, B: 12, A: 180})
			}
		}
		var buf bytes.Buffer
		if err := png.Encode(&buf, im); err != nil {
			t.Fatal(err)
		}
		return buf.Bytes()
	}
	storeFixture := func(width int) string {
		t.Helper()
		data := makeFixture(width)
		sum := sha256.Sum256(data)
		var id, key string
		err := admin.QueryRow(ctx, `INSERT INTO platform.files(owner_id,display_name,mime,bytes,sha256,state,ready_at,expires_at)
   VALUES($1,'fixture.png','image/png',$2,$3,'ready',now(),now()+interval '1 hour') RETURNING id::text,object_key`, owner, len(data), sum[:]).Scan(&id, &key)
		if err != nil {
			t.Fatal(err)
		}
		if err := objects.Put(ctx, key, bytes.NewReader(data), int64(len(data)), "image/png", sum); err != nil {
			t.Fatal(err)
		}
		return id
	}
	source := []string{storeFixture(60), storeFixture(40), storeFixture(20)}
	launcher := &testLauncher{native: os.Getenv("AUTOLIFE_IMAGE_TEST_TAG"), fixture: makeFixture(20), blocked: make(chan struct{}, 1)}
	launcher.nativeEnabled.Store(launcher.native != "")
	registry := workflow.NewRegistry()
	if err := Register(registry, objects, launcher); err != nil {
		t.Fatal(err)
	}
	if len(registry.Actions()) != 3 {
		t.Fatal("baseline image action catalog missing")
	}
	enqueue, err := execution.New(api, registry, false)
	if err != nil {
		t.Fatal(err)
	}
	executor, err := execution.New(worker, registry, true)
	if err != nil {
		t.Fatal(err)
	}
	if err := executor.Queue.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := executor.Stop(); err != nil {
			t.Error(err)
		}
	})
	definitions := workflowdb.New(api, registry)
	flow := func(steps string) string {
		t.Helper()
		result, err := definitions.Create(ctx, owner, "images fixture", "")
		if err != nil {
			t.Fatal(err)
		}
		raw := json.RawMessage(`{"schema_version":1,"trigger":{"type":"core.manual","version":1},"steps":` + steps + `}`)
		draft, err := definitions.SaveDraft(ctx, owner, result.ID, 1, raw)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := definitions.Publish(ctx, owner, result.ID, draft.Revision); err != nil {
			t.Fatal(err)
		}
		return result.ID
	}
	start := func(wid, key string, inputs []string) execution.Execution {
		t.Helper()
		run, err := enqueue.Start(ctx, owner, wid, key, execution.Start{Files: &inputs}, false)
		if err != nil {
			t.Fatal(err)
		}
		return run
	}
	wait := func(run execution.Execution, want execution.State) execution.Details {
		t.Helper()
		for until := time.Now().Add(40 * time.Second); time.Now().Before(until); {
			d, err := enqueue.Details(ctx, owner, run.ID)
			if err != nil {
				t.Fatal(err)
			}
			if d.Status == want {
				return d
			}
			if d.Status.Terminal() {
				t.Fatalf("expected %s, got %s: %+v", want, d.Status, d.Steps)
			}
			time.Sleep(50 * time.Millisecond)
		}
		t.Fatalf("waiting for %s", want)
		return execution.Details{}
	}
	if launcher.native != "" {
		chain := flow(`[
   {"id":"convert","action_type":"file.image.convert","action_version":1,"config":{"format":"jpeg","background":"#102030"},"input_bindings":{"files":{"source":"trigger","output":"files"}}},
   {"id":"resize","action_type":"file.image.resize","action_version":1,"config":{"max_width":30,"max_height":25},"input_bindings":{"files":{"source":"convert","output":"files"}}},
   {"id":"compress","action_type":"file.image.compress","action_version":1,"config":{"quality":65},"input_bindings":{"files":{"source":"resize","output":"files"}}}]`)
		run := start(chain, "native-chain", source)
		result := wait(run, execution.Succeeded)
		var ids []string
		if err := json.Unmarshal(result.Outputs["compress"]["files"], &ids); err != nil || len(ids) != 3 {
			t.Fatalf("chain output: %v", err)
		}
		for i, id := range ids {
			f, err := fileflow.NewStore(api).GetReady(ctx, owner, id)
			if err != nil || f.MIME == nil || *f.MIME != "image/jpeg" {
				t.Fatalf("output metadata: %v", err)
			}
			body, _, err := objects.Open(ctx, f.ObjectKey)
			if err != nil {
				t.Fatal(err)
			}
			info, format, err := image.DecodeConfig(body)
			body.Close()
			if err != nil || format != "jpeg" || info.Width != []int{30, 30, 20}[i] || info.Height != []int{10, 15, 20}[i] {
				t.Fatalf("unexpected output and order: %+v %s %v", info, format, err)
			}
		}
		var count int
		if err := admin.QueryRow(ctx, `SELECT count(*) FROM platform.outbox_events WHERE type='file.action_completed' AND payload->>'execution_id'=$1
   AND (payload->>'input_count')::int=3 AND (payload->>'output_count')::int=3`, run.ID).Scan(&count); err != nil || count != 3 {
			t.Fatalf("logical events: %d %v", count, err)
		}
		// Subsequent state-machine scenarios use a test transformer without launching native processes.
		launcher.nativeEnabled.Store(false)
	}
	conversion := flow(`[{"id":"image","action_type":"file.image.convert","action_version":1,"config":{"format":"png"},"input_bindings":{"files":{"source":"trigger","output":"files"}}}]`)
	fifty := append([]string(nil), source...)
	for len(fifty) < 50 {
		fifty = append(fifty, storeFixture(20))
	}
	run := start(conversion, "fifty", fifty)
	batch := wait(run, execution.Succeeded)
	var ids []string
	if err := json.Unmarshal(batch.Outputs["image"]["files"], &ids); err != nil || len(ids) != 50 {
		t.Fatalf("fifty outputs: %d %v", len(ids), err)
	}
	if again := start(conversion, "fifty", fifty); again.ID != run.ID {
		t.Fatal("idempotency changed a batch")
	}
	launcher.retryAt.Store(launcher.calls.Load() + 2)
	retried := wait(start(conversion, "retry-image", source), execution.Succeeded)
	var logical int
	if err := admin.QueryRow(ctx, `SELECT count(*) FROM platform.outbox_events
	  WHERE type='file.action_completed' AND payload->>'execution_id'=$1`, retried.ID).Scan(&logical); err != nil ||
		logical != 1 || len(retried.Steps[0].Attempts) != 2 {
		t.Fatalf("logical action vs retries: %d %v", logical, err)
	}
	launcher.rejectAt.Store(launcher.calls.Load() + 2)
	rejected := wait(start(conversion, "bad-item", source), execution.Failed)
	if len(rejected.Outputs) != 0 || len(rejected.Steps[0].Attempts) != 1 {
		t.Fatal("partial result exposed or non-retryable input retried")
	}
	attempt := rejected.Steps[0].Attempts[0]
	if attempt.FailedIndex == nil || *attempt.FailedIndex != 2 {
		t.Fatalf("wrong failed input: %+v", attempt)
	}
	var leaked int
	if err := admin.QueryRow(ctx, "SELECT count(*) FROM platform.execution_staged_files WHERE execution_id=$1", rejected.ID).Scan(&leaked); err != nil || leaked != 0 {
		t.Fatalf("staging not cleared: %d %v", leaked, err)
	}
	launcher.block.Store(true)
	cancelled := start(conversion, "cancel", source)
	select {
	case <-launcher.blocked:
	case <-time.After(5 * time.Second):
		t.Fatal("cancel fixture did not start")
	}
	if _, err := enqueue.Cancel(ctx, owner, cancelled.ID); err != nil {
		t.Fatal(err)
	}
	wait(cancelled, execution.Cancelled)
	timed := flow(`[{"id":"image","action_type":"file.image.convert","action_version":1,"timeout_seconds":1,"config":{"format":"png"},"input_bindings":{"files":{"source":"trigger","output":"files"}}}]`)
	expired := wait(start(timed, "timeout", source), execution.Failed)
	if expired.ErrorCode == nil || !strings.Contains(*expired.ErrorCode, "ACTION_TIMEOUT") {
		t.Fatalf("step deadline: %+v", expired.ErrorCode)
	}
	if val, err := enqueue.Usage(ctx, owner); err != nil || val.Active != 0 {
		t.Fatalf("quota release: %+v %v", val, err)
	}
	if _, err := enqueue.Details(ctx, "11111111-1111-1111-1111-111111111111", run.ID); !errors.Is(err, workflow.ErrNotFound) {
		t.Fatalf("foreign results: %v", err)
	}
}
