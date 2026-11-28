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
	"autolife/backend/internal/fileflow/officeflow"
	"autolife/backend/internal/fileflow/tableflow"
	"autolife/backend/internal/tableproto"
	"autolife/backend/internal/workflow"
	workflowdb "autolife/backend/internal/workflow/postgres"
	workflowhttp "autolife/backend/internal/workflow/workflowhttp"
	"errors"
	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
	"github.com/riverqueue/river/rivermigrate"
	"github.com/xuri/excelize/v2"
	"image/jpeg"
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
	_ = execution.ReportSandboxUsage(ctx, 20_000_000, 32<<20)
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
	expanded := os.Getenv("AUTOLIFE_TABLE_TEST_TAG") != ""
	native := &fixtureLauncher{image: pdfTag, memory: "768m", disk: "768m", entered: make(chan struct{}, 1)}
	if err := RegisterOperations(reg, objects, native, expanded); err != nil {
		t.Fatal(err)
	}
	imageNative := &fixtureLauncher{image: imageTag, memory: "512m", disk: "384m"}
	if err := imageflow.RegisterFormats(reg, objects, imageNative, expanded); err != nil {
		t.Fatal(err)
	}
	officeTag := os.Getenv("AUTOLIFE_OFFICE_TEST_TAG")
	var officeNative *fixtureLauncher
	if officeTag != "" {
		officeNative = &fixtureLauncher{image: officeTag, memory: "768m", disk: "384m"}
		if err := officeflow.Register(reg, objects, officeNative); err != nil {
			t.Fatal(err)
		}
	}
	if expanded {
		if err := tableflow.Register(reg, objects, &fixtureLauncher{image: os.Getenv("AUTOLIFE_TABLE_TEST_TAG"), memory: "512m", disk: "384m"}); err != nil {
			t.Fatal(err)
		}
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
	files := &fileflow.Service{Store: fileflow.NewStore(api), Objects: objects, Previewer: imageNative, Slots: make(chan struct{}, 1)}
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
	if officeNative != nil {
		t.Run("office_text_batch", func(t *testing.T) {
			protected := excelize.NewFile()
			defer protected.Close()
			var encrypted bytes.Buffer
			if err := protected.Write(&encrypted, excelize.Options{Password: "office-fixture"}); err != nil {
				t.Fatal(err)
			}
			_, _, protectedErr := officeNative.Transform(ctx, "", []byte{'I'}, bytes.NewReader(encrypted.Bytes()), int64(encrypted.Len()))
			if !errors.Is(protectedErr, execution.ErrInputs) {
				t.Fatalf("protected workbook accepted or invalid runtime: %v", protectedErr)
			}
			files.StorageBytes, files.ConcurrentUploads = 1_000_000_000, 2
			files.Inspector = officeFixtureInspector{officeNative}
			var inputIDs []string
			for _, text := range []string{"Первый файл", "Второй файл"} {
				file, err := files.Upload(ctx, owner, "source.txt", "text/plain", strings.NewReader(text), func() error { return nil })
				if err != nil || file.MIME == nil || *file.MIME != "text/plain" {
					t.Fatalf("Office upload: %v (%+v)", err, file)
				}
				inputIDs = append(inputIDs, file.ID)
			}
			officeID := fixture(`[
 {"id":"convert","action_type":"file.text.to_pdf","action_version":1,"config":{"page_size":"Letter","orientation":"landscape","margin_mm":15},"input_bindings":{"files":{"source":"trigger","output":"files"}}},
 {"id":"merge","action_type":"file.pdf.merge","action_version":1,"input_bindings":{"pdfs":{"source":"convert","output":"files"}}}]`)
			id := start(officeID, "office-batch", inputIDs)
			done := wait(id, execution.Succeeded)
			var out []string
			if err := json.Unmarshal(done.Outputs["convert"]["files"], &out); err != nil || len(out) != 2 || done.Outputs["merge"]["pdf"] == nil {
				t.Fatalf("office results: %v (%v)", err, done.Outputs)
			}
			var reports int
			if err := admin.QueryRow(ctx, `SELECT count(*) FROM platform.outbox_events WHERE type='file.action_completed'
 AND payload->>'execution_id'=$1 AND payload->>'input_format'='txt' AND payload->>'output_count'='2'`, id).Scan(&reports); err != nil || reports != 1 {
				t.Fatalf("Office outbox: %d %v", reports, err)
			}
			invalid := seed("text/plain", []byte{0})
			failed := wait(start(officeID, "office-invalid", []string{inputIDs[0], invalid}), execution.Failed)
			if len(failed.Outputs) != 0 || failed.Steps[1].Status != "skipped" {
				t.Fatal("office batch exposed a partial output")
			}
		})
	}
	if expanded {
		t.Run("additional_formats_and_quick", func(t *testing.T) {
			quick := func(action, key string, cfg map[string]any, input []string, expected int) string {
				t.Helper()
				var r struct{ ID string }
				request("POST", "/api/v1/executions", key, map[string]any{"action_type": action, "action_version": 1,
					"config": cfg, "files": input}, expected, &r)
				return r.ID
			}
			ids := func(run execution.Details, step string, count int) []string {
				t.Helper()
				var list []string
				if err := json.Unmarshal(run.Outputs[step]["files"], &list); err != nil || len(list) != count {
					t.Fatalf("unexpected refs: %v %v", list, err)
				}
				return list
			}
			rasterConfig := map[string]any{"format": "png", "dpi": 72, "alpha": true, "ranges": []string{"2", "1"}}
			rasterID := quick("file.pdf.rasterize", "quick-raster", rasterConfig, []string{pdfA}, 202)
			if repeated := quick("file.pdf.rasterize", "quick-raster", rasterConfig, []string{pdfA}, 202); repeated != rasterID {
				t.Fatal("idempotent quick run changed ID")
			}
			quick("file.pdf.rasterize", "quick-raster", rasterConfig, []string{pdfB}, 409)
			quick("file.pdf.rasterize", "invalid-pair", rasterConfig, []string{pngID}, 422)
			raster := wait(rasterID, execution.Succeeded)
			if raster.WorkflowID != nil || raster.QuickSnapshotID == nil {
				t.Fatal("wrong quick snapshot source")
			}
			pages := ids(raster, "convert", 2)
			var positions []struct {
				Input int `json:"input_index"`
				Page  int
			}
			if raw := raster.Outputs["convert"]["pages"]; json.Unmarshal(raw, &positions) != nil ||
				len(positions) != 2 || positions[0].Input != 1 || positions[1].Input != 1 || positions[0].Page != 1 || positions[1].Page != 2 {
				t.Fatalf("wrong PDF page provenance: %s", raw)
			}
			for index, page := range pages {
				f, err := files.Store.GetReady(ctx, owner, page)
				if err != nil {
					t.Fatal(err)
				}
				reader, _, err := objects.Open(ctx, f.ObjectKey)
				if err != nil {
					t.Fatal(err)
				}
				raster, err := png.Decode(reader)
				reader.Close()
				if err != nil || raster.Bounds().Dx() != 100+20*index || raster.Bounds().Dy() != 300 {
					t.Fatalf("raster bounds/order: %v", err)
				}
				_, _, _, alpha := raster.At(0, 0).RGBA()
				if alpha != 0 {
					t.Fatal("transparent PDF background lost")
				}
			}
			req := httptest.NewRequest("GET", "/api/v1/files/"+pages[0]+"/preview", nil)
			req.AddCookie(&http.Cookie{Name: "autolife-session", Value: token})
			response := httptest.NewRecorder()
			route.ServeHTTP(response, req)
			preview, err := jpeg.Decode(response.Body)
			if err != nil || response.Code != 200 || response.Header().Get("Cache-Control") != "private, no-store" || preview.Bounds().Dx() != 100 {
				t.Fatalf("invalid preview: %d %v", response.Code, err)
			}
			var other string
			if err := admin.QueryRow(ctx, `INSERT INTO platform.users(normalized_email,password_hash,email_verified_at)
 VALUES(gen_random_uuid()::text||'@quick.example.test','fixture',now()) RETURNING id::text`).Scan(&other); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _, _ = admin.Exec(context.Background(), "DELETE FROM platform.users WHERE id=$1", other) })
			if _, err := execAPI.Details(ctx, other, rasterID); !errors.Is(err, workflow.ErrNotFound) {
				t.Fatalf("quick execution crossed owners: %v", err)
			}
			if _, err := files.Store.GetReady(ctx, other, pages[0]); !errors.Is(err, fileflow.ErrNotFound) {
				t.Fatalf("quick output crossed owners: %v", err)
			}
			badRange := map[string]any{"format": "png", "ranges": []string{"3"}}
			if failed := wait(quick("file.pdf.rasterize", "invalid-range", badRange, []string{pdfA}, 202), execution.Failed); len(failed.Outputs) != 0 {
				t.Fatal("invalid page range was published")
			}
			huge := seed("application/pdf", makePDF(10000))
			if failed := wait(quick("file.pdf.rasterize", "pixel-limit", map[string]any{"format": "png", "dpi": 300}, []string{huge}, 202), execution.Failed); len(failed.Outputs) != 0 {
				t.Fatal("oversized raster was published")
			}
			var retry struct{ ID string }
			request("POST", "/api/v1/executions/"+rasterID+"/retry", "quick-retry", map[string]any{}, 202, &retry)
			if rerun := wait(retry.ID, execution.Succeeded); rerun.QuickSnapshotID == nil || *rerun.QuickSnapshotID != *raster.QuickSnapshotID {
				t.Fatal("retry changed immutable snapshot")
			}
			rasterChain := fixture(`[
 {"id":"render","action_type":"file.pdf.rasterize","action_version":1,"config":{"format":"png","dpi":72},"input_bindings":{"pdfs":{"source":"trigger","output":"files"}}},
 {"id":"compress","action_type":"file.image.compress","action_version":1,"input_bindings":{"files":{"source":"render","output":"files"}}}]`)
			ids(wait(start(rasterChain, "raster-chain", []string{pdfA}), execution.Succeeded), "compress", 2)
			svg := seed("image/svg+xml", []byte(`<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 24 12"><rect width="12" height="12" fill="#ff0000"/></svg>`))
			rasterResult := func(id string) []byte {
				t.Helper()
				f, err := files.Store.GetReady(ctx, owner, id)
				if err != nil {
					t.Fatal(err)
				}
				reader, _, err := objects.Open(ctx, f.ObjectKey)
				if err != nil {
					t.Fatal(err)
				}
				defer reader.Close()
				data, err := io.ReadAll(reader)
				if err != nil {
					t.Fatal(err)
				}
				return data
			}
			img := wait(quick("file.image.convert", "svg-png", map[string]any{"format": "png"}, []string{svg}, 202), execution.Succeeded)
			pngImage, err := png.Decode(bytes.NewReader(rasterResult(ids(img, "convert", 1)[0])))
			if err != nil || pngImage.Bounds().Dx() != 24 || pngImage.Bounds().Dy() != 12 {
				t.Fatalf("SVG viewBox: %v", err)
			}
			if _, _, _, alpha := pngImage.At(23, 6).RGBA(); alpha != 0 {
				t.Fatal("SVG PNG lost transparency")
			}
			jpg := wait(quick("file.image.convert", "svg-jpeg", map[string]any{"format": "jpeg", "background": "#0022dd"}, []string{svg}, 202), execution.Succeeded)
			jpegImage, err := jpeg.Decode(bytes.NewReader(rasterResult(ids(jpg, "convert", 1)[0])))
			if err != nil {
				t.Fatal(err)
			}
			r, g, b, _ := jpegImage.At(23, 6).RGBA()
			if b < 45_000 || r > 10_000 || g > 20_000 {
				t.Fatalf("SVG JPEG background: %d %d %d", r, g, b)
			}
			rotate := seed("application/pdf", makePDF(-100))
			rotated := wait(quick("file.pdf.rasterize", "pdf-rotated", map[string]any{"format": "jpeg", "dpi": 72}, []string{rotate}, 202), execution.Succeeded)
			jpegImage, err = jpeg.Decode(bytes.NewReader(rasterResult(ids(rotated, "convert", 1)[0])))
			if err != nil || jpegImage.Bounds().Dx() != 300 || jpegImage.Bounds().Dy() != 100 {
				t.Fatalf("PDF rotated geometry: %v", err)
			}
			quick("file.image.convert", "svg-webp", map[string]any{"format": "webp"}, []string{svg}, 422)
			csv := seed(tableproto.CSV, []byte("001;Привет;=1+1\n"))
			book := ids(wait(quick("file.table.convert", "csv-xlsx", map[string]any{"format": "xlsx", "delimiter": ";"}, []string{csv}, 202), execution.Succeeded), "convert", 1)
			table := wait(quick("file.table.convert", "xlsx-csv", map[string]any{"format": "csv", "delimiter": ";"}, book, 202), execution.Succeeded)
			if string(table.Outputs["convert"]["escaped_values"]) != "1" {
				t.Fatal("formula-safe CSV warning missing")
			}
			ids(table, "convert", 1)
			var completed, attempts int
			if err := admin.QueryRow(ctx, `SELECT count(*) FILTER(WHERE type='file.action_completed'),
 count(*) FILTER(WHERE type='file.action_attempted' AND payload->>'sampled_jobs'='1' AND payload->>'cpu_ns'='20000000' AND payload->>'storage_gets'='1' AND payload->>'storage_puts'='2') FROM platform.outbox_events
 WHERE payload->>'execution_id'=$1 AND payload->>'schema_version'='1'`, rasterID).Scan(&completed, &attempts); err != nil || completed != 1 || attempts != 1 {
				t.Fatalf("versioned outcome/attempt: %d/%d %v", completed, attempts, err)
			}
		})
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

type officeFixtureInspector struct{ native *fixtureLauncher }

func (i officeFixtureInspector) Inspect(ctx context.Context, id, profile string, body io.Reader, size int64) (string, error) {
	if profile != "office" {
		return "", fileflow.ErrInvalid
	}
	result, _, err := i.native.Transform(ctx, id, []byte{'I'}, body, size)
	if err != nil {
		return "", err
	}
	defer result.Close()
	var metadata struct{ MIME string }
	if err := json.NewDecoder(result).Decode(&metadata); err != nil {
		return "", err
	}
	return metadata.MIME, nil
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
		rotate := 0
		if w < 0 {
			w, rotate = -w, 90
		}
		add("<< /Type /Page /Parent 2 0 R /MediaBox [0 0 %d 300] /Rotate %d /Resources << >> >>", w, rotate)
	}
	xref := b.Len()
	fmt.Fprintf(&b, "xref\n0 %d\n0000000000 65535 f \n", len(offsets))
	for _, n := range offsets[1:] {
		fmt.Fprintf(&b, "%010d 00000 n \n", n)
	}
	fmt.Fprintf(&b, "trailer\n<< /Root 1 0 R /Size %d >>\nstartxref\n%d\n%%%%EOF\n", len(offsets), xref)
	return b.Bytes()
}
