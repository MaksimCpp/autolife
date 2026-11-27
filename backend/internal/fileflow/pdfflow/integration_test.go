package pdfflow

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"image"
	"image/png"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"autolife/backend/internal/accounts"
	"autolife/backend/internal/execution"
	"autolife/backend/internal/fileflow"
	"autolife/backend/internal/fileflow/imageflow"
	"autolife/backend/internal/workflow"
	workflowdb "autolife/backend/internal/workflow/postgres"
	workflowhttp "autolife/backend/internal/workflow/workflowhttp"
	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
	"github.com/riverqueue/river/rivermigrate"
)

type fixtureLauncher struct {
	image, memory, disk string
	block               atomic.Bool
	entered             chan struct{}
}

func (*fixtureLauncher) Available() bool                                      { return true }
func (*fixtureLauncher) ConfirmStopped(context.Context, string) (bool, error) { return true, nil }
func (l *fixtureLauncher) Transform(ctx context.Context, _ string, header []byte, r io.Reader, _ int64) (io.ReadCloser, int64, error) {
	if l.block.Load() {
		select {
		case l.entered <- struct{}{}:
		default:
		}
		<-ctx.Done()
		return nil, 0, ctx.Err()
	}
	cmd := exec.CommandContext(ctx, "docker", "run", "--rm", "-i", "--network", "none", "--read-only", "--cap-drop", "ALL",
		"--security-opt", "no-new-privileges", "--ipc", "none", "--user", "65534:65534", "--cpus", "1", "--memory", l.memory, "--memory-swap", l.memory,
		"--pids-limit", "32", "--tmpfs", "/work:rw,noexec,nosuid,nodev,size="+l.disk+",mode=1777", "--env", "TMPDIR=/work", l.image)
	cmd.Stdin = io.MultiReader(bytes.NewReader(header), r)
	raw, err := cmd.Output()
	if err != nil {
		if e, ok := err.(*exec.ExitError); ok && e.ExitCode() == 20 {
			return nil, 0, execution.ErrInputs
		}
		return nil, 0, err
	}
	return io.NopCloser(bytes.NewReader(raw)), int64(len(raw)), nil
}

func TestPDFWorkflowAPI(t *testing.T) {
	ctx := t.Context()
	pdfTag, imageTag, endpoint := os.Getenv("AUTOLIFE_PDF_TEST_TAG"), os.Getenv("AUTOLIFE_IMAGE_TEST_TAG"), os.Getenv("AUTOLIFE_TEST_S3_ENDPOINT")
	if pdfTag == "" || imageTag == "" || endpoint == "" {
		t.Skip("requires built test profiles and private local S3")
	}
	connect := func(key string) *pgxpool.Pool {
		t.Helper()
		if os.Getenv(key) == "" {
			t.Skip("requires dedicated PostgreSQL roles")
		}
		pool, err := pgxpool.New(ctx, os.Getenv(key))
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
	objects, err := fileflow.NewS3(fileflow.S3Config{Endpoint: endpoint, Bucket: "autolife", Region: "us-east-1", AllowHTTP: true,
		AccessKey: os.Getenv("AUTOLIFE_S3_ACCESS_KEY"), SecretKey: os.Getenv("AUTOLIFE_S3_SECRET_KEY")})
	if err != nil {
		t.Fatal(err)
	}
	var owner string
	if err := admin.QueryRow(ctx, `INSERT INTO platform.users(normalized_email,password_hash,email_verified_at)
 VALUES(gen_random_uuid()::text||'@pdf.example.test','fixture',now()) RETURNING id::text`).Scan(&owner); err != nil {
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
	token := base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{41}, 32))
	hash := sha256.Sum256([]byte(token))
	if _, err := admin.Exec(ctx, `INSERT INTO platform.sessions(user_id,token_hash,expires_at)
 VALUES($1,$2,now()+interval '1 hour')`, owner, hash[:]); err != nil {
		t.Fatal(err)
	}
	auth, err := accounts.NewHTTP(accounts.NewStore(api), slog.New(slog.DiscardHandler), "http://127.0.0.1:8080", "mail:1025", strings.Repeat("ab", 32))
	if err != nil {
		t.Fatal(err)
	}
	reg := workflow.NewRegistry()
	native := &fixtureLauncher{image: pdfTag, memory: "768m", disk: "768m", entered: make(chan struct{}, 1)}
	if err := Register(reg, objects, native); err != nil {
		t.Fatal(err)
	}
	if err := imageflow.Register(reg, objects, &fixtureLauncher{image: imageTag, memory: "512m", disk: "384m"}); err != nil {
		t.Fatal(err)
	}
	execAPI, err := execution.New(api, reg, false)
	if err != nil {
		t.Fatal(err)
	}
	execWorker, err := execution.New(worker, reg, true)
	if err != nil {
		t.Fatal(err)
	}
	if err := execWorker.Queue.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := execWorker.Stop(); err != nil {
			t.Error(err)
		}
	})
	files := &fileflow.Service{Store: fileflow.NewStore(api), Objects: objects}
	route := chi.NewRouter()
	auth.Routes(route)
	(workflowhttp.API{Store: workflowdb.New(api, reg), Registry: reg}).Routes(route, auth.RequireSession, auth.ProtectSession, accounts.SessionOwner)
	execAPI.Routes(route, auth.RequireSession, auth.ProtectSession, accounts.SessionOwner)
	files.Routes(route, auth.RequireSession, auth.ProtectSession, accounts.SessionOwner)
	var csrf string
	request := func(method, path, key string, body any, expected int, result any) {
		t.Helper()
		var reader io.Reader
		if body != nil {
			b, err := json.Marshal(body)
			if err != nil {
				t.Fatal(err)
			}
			reader = bytes.NewReader(b)
		}
		req := httptest.NewRequest(method, path, reader)
		req.AddCookie(&http.Cookie{Name: "autolife-session", Value: token})
		req.Header.Set("Origin", "http://127.0.0.1:8080")
		req.Header.Set("X-CSRF-Token", csrf)
		req.Header.Set("Idempotency-Key", key)
		response := httptest.NewRecorder()
		route.ServeHTTP(response, req)
		if response.Code != expected {
			t.Fatalf("%s %s: %d, expected %d: %s", method, path, response.Code, expected, response.Body)
		}
		if result != nil && json.Unmarshal(response.Body.Bytes(), result) != nil {
			t.Fatal("invalid response")
		}
	}
	var tokenResult struct {
		Token string `json:"csrf_token"`
	}
	request("GET", "/api/v1/auth/csrf", "", nil, 200, &tokenResult)
	csrf = tokenResult.Token
	if csrf == "" {
		t.Fatal("CSRF missing")
	}
	seed := func(mime string, data []byte) string {
		t.Helper()
		var id, key string
		sum := sha256.Sum256(data)
		if err := admin.QueryRow(ctx, `INSERT INTO platform.files(owner_id,display_name,mime,bytes,sha256,state,ready_at,expires_at)
 VALUES($1,'fixture',$2,$3,$4,'ready',now(),now()+interval '1 hour') RETURNING id::text,object_key`, owner, mime, len(data), sum[:]).Scan(&id, &key); err != nil {
			t.Fatal(err)
		}
		if err := objects.Put(ctx, key, bytes.NewReader(data), int64(len(data)), mime, sum); err != nil {
			t.Fatal(err)
		}
		return id
	}
	var pngFixture bytes.Buffer
	if err := png.Encode(&pngFixture, image.NewNRGBA(image.Rect(0, 0, 70, 40))); err != nil {
		t.Fatal(err)
	}
	pngID := seed("image/png", pngFixture.Bytes())
	pdfA := seed("application/pdf", makePDF(100, 120))
	pdfB := seed("application/pdf", makePDF(130))
	bad := seed("application/pdf", []byte("%PDF-damaged fixture"))
	fixture := func(steps string) string {
		t.Helper()
		var w workflow.Details
		request("POST", "/api/v1/workflows", "", map[string]string{"name": "pdf fixture"}, 201, &w)
		path := "/api/v1/workflows/" + w.ID
		request("PUT", path+"/draft", "", map[string]any{"expected_revision": 1,
			"definition": json.RawMessage(`{"schema_version":1,"trigger":{"type":"core.manual","version":1},"steps":` + steps + `}`)}, 200, nil)
		request("POST", path+"/versions", "", map[string]any{"expected_revision": 2}, 201, nil)
		return w.ID
	}
	start := func(w, key string, ids []string) string {
		t.Helper()
		var r struct{ ID string }
		request("POST", "/api/v1/workflows/"+w+"/executions", key, map[string]any{"files": ids}, 202, &r)
		return r.ID
	}
	wait := func(id string, want execution.State) execution.Details {
		t.Helper()
		for end := time.Now().Add(25 * time.Second); time.Now().Before(end); {
			var d execution.Details
			request("GET", "/api/v1/executions/"+id, "", nil, 200, &d)
			if d.Status == want {
				return d
			}
			if d.Status.Terminal() {
				t.Fatalf("unexpected status %s, want %s (%+v)", d.Status, want, d.Steps)
			}
			time.Sleep(50 * time.Millisecond)
		}
		t.Fatalf("execution did not reach %s", want)
		return execution.Details{}
	}
	chain := fixture(`[
 {"id":"resize","action_type":"file.image.resize","action_version":1,"config":{"max_width":30,"max_height":20},"input_bindings":{"files":{"source":"trigger","output":"files"}}},
 {"id":"create","action_type":"file.pdf.create","action_version":1,"config":{"page_size":"A4","orientation":"landscape"},"input_bindings":{"images":{"source":"resize","output":"files"}}}]`)
	created := wait(start(chain, "create", []string{pngID}), execution.Succeeded)
	var result string
	if err := json.Unmarshal(created.Outputs["create"]["pdf"], &result); err != nil || result == "" {
		t.Fatal("create output missing")
	}
	var link struct{ URL string }
	request("POST", "/api/v1/files/"+result+"/download", "", nil, 200, &link)
	response, err := http.Get(link.URL)
	if err != nil {
		t.Fatal(err)
	}
	actual, err := io.ReadAll(response.Body)
	response.Body.Close()
	if err != nil || response.StatusCode != 200 || !bytes.HasPrefix(actual, []byte("%PDF-")) ||
		!strings.HasPrefix(response.Header.Get("Content-Disposition"), "attachment") {
		t.Fatalf("private download: %d %v", response.StatusCode, err)
	}
	workflowID := fixture(`[
 {"id":"merge","action_type":"file.pdf.merge","action_version":1,"input_bindings":{"pdfs":{"source":"trigger","output":"files"}}},
 {"id":"split","action_type":"file.pdf.split","action_version":1,"config":{"ranges":["3","1-2"]},"input_bindings":{"pdf":{"source":"merge","output":"pdf"}}},
 {"id":"extract","action_type":"file.pdf.extract","action_version":1,"config":{"ranges":["2"]},"input_bindings":{"pdf":{"source":"merge","output":"pdf"}}}]`)
	id := start(workflowID, "merge-split", []string{pdfA, pdfB})
	complete := wait(id, execution.Succeeded)
	var split []string
	if err := json.Unmarshal(complete.Outputs["split"]["files"], &split); err != nil || len(split) != 2 || complete.Outputs["extract"]["pdf"] == nil {
		t.Fatal("invalid split/extract results")
	}
	var events int
	if err := admin.QueryRow(ctx, `SELECT count(*) FROM platform.outbox_events WHERE type='file.action_completed'
 AND payload->>'execution_id'=$1 AND payload->>'output_format'='pdf'`, id).Scan(&events); err != nil || events != 3 {
		t.Fatalf("outbox: %d %v", events, err)
	}
	failed := wait(start(workflowID, "damaged", []string{pdfA, bad}), execution.Failed)
	if len(failed.Outputs) != 0 || failed.Steps[1].Status != "skipped" {
		t.Fatal("partial result exposed")
	}
	native.block.Store(true)
	cancelID := start(workflowID, "cancel", []string{pdfA, pdfB})
	select {
	case <-native.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("cancel fixture did not start")
	}
	request("POST", "/api/v1/executions/"+cancelID+"/cancel", "", nil, 202, nil)
	if result := wait(cancelID, execution.Cancelled); len(result.Outputs) != 0 {
		t.Fatal("cancelled outputs exposed")
	}
}

func makePDF(widths ...int) []byte {
	var b bytes.Buffer
	b.WriteString("%PDF-1.4\n")
	offsets := []int{0}
	add := func(format string, args ...any) {
		offsets = append(offsets, b.Len())
		fmt.Fprintf(&b, "%d 0 obj\n", len(offsets)-1)
		fmt.Fprintf(&b, format, args...)
		b.WriteString("\nendobj\n")
	}
	add("<< /Type /Catalog /Pages 2 0 R >>")
	var kids strings.Builder
	for i := range widths {
		fmt.Fprintf(&kids, "%d 0 R ", i+3)
	}
	add("<< /Type /Pages /Kids [%s] /Count %d >>", kids.String(), len(widths))
	for _, w := range widths {
		add("<< /Type /Page /Parent 2 0 R /MediaBox [0 0 %d 300] /Resources << >> >>", w)
	}
	xref := b.Len()
	fmt.Fprintf(&b, "xref\n0 %d\n0000000000 65535 f \n", len(offsets))
	for _, n := range offsets[1:] {
		fmt.Fprintf(&b, "%010d 00000 n \n", n)
	}
	fmt.Fprintf(&b, "trailer\n<< /Root 1 0 R /Size %d >>\nstartxref\n%d\n%%%%EOF\n", len(offsets), xref)
	return b.Bytes()
}
