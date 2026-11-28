package fileflow

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type inspectFunc func(context.Context, string, string, io.Reader, int64) (string, error)

func (f inspectFunc) Inspect(ctx context.Context, id, profile string, in io.Reader, bytes int64) (string, error) {
	return f(ctx, id, profile, in, bytes)
}

type testObjects struct {
	puts, deletes       int
	failPut, failDelete bool
}

func (o *testObjects) Put(_ context.Context, _ string, b io.ReadSeeker, n int64, _ string, sum [sha256.Size]byte) error {
	o.puts++
	h := sha256.New()
	count, err := io.Copy(h, b)
	if err != nil || count != n || !bytes.Equal(h.Sum(nil), sum[:]) || o.failPut {
		return ErrUnavailable
	}
	return nil
}
func (o *testObjects) Delete(context.Context, string) error {
	o.deletes++
	if o.failDelete {
		return ErrUnavailable
	}
	return nil
}
func (*testObjects) PresignDownload(_ context.Context, _, _ string, expiry time.Time) (string, error) {
	if !expiry.After(time.Now()) {
		return "", ErrNotFound
	}
	return "https://storage.invalid/signed", nil
}

type zeros struct{}

func (zeros) Read(p []byte) (int, error) { clear(p); return len(p), nil }

func TestFileLifecycle(t *testing.T) {
	ctx := t.Context()
	pool := func(key string) *pgxpool.Pool {
		t.Helper()
		dsn := os.Getenv(key)
		if dsn == "" {
			t.Skip("requires migrated PostgreSQL with test API, worker and migration roles")
		}
		p, err := pgxpool.New(ctx, dsn)
		if err != nil {
			t.Fatal("database configuration failed")
		}
		t.Cleanup(p.Close)
		return p
	}
	api, worker, admin := pool("AUTOLIFE_API_DATABASE_URL"), pool("AUTOLIFE_WORKER_DATABASE_URL"), pool("AUTOLIFE_MIGRATION_DATABASE_URL")
	fixture := func() string {
		t.Helper()
		var id string
		if err := admin.QueryRow(ctx, `INSERT INTO platform.users(normalized_email,password_hash)
   VALUES (gen_random_uuid()::text || '@example.test','fixture') RETURNING id::text`).Scan(&id); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			clean, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			for _, query := range []string{
				"DELETE FROM platform.file_pins WHERE owner_id=$1",
				"DELETE FROM platform.privacy_jobs WHERE owner_id=$1",
				"DELETE FROM platform.files WHERE owner_id=$1",
				"DELETE FROM platform.outbox_events WHERE payload->>'owner_id'=$1",
				"DELETE FROM platform.users WHERE id=$1",
			} {
				if _, err := admin.Exec(clean, query, id); err != nil {
					t.Error(err)
				}
			}
		})
		return id
	}
	owner, other := fixture(), fixture()
	objects := &testObjects{}
	service := &Service{Store: NewStore(api), Objects: objects, StorageBytes: MaxFileBytes, ConcurrentUploads: 1, Slots: make(chan struct{}, 1)}
	cleaner := &Service{Store: NewStore(worker), Objects: objects}
	const pngMagic = "\x89PNG\r\n\x1a\n"
	accepted := inspectFunc(func(_ context.Context, _ string, profile string, r io.Reader, n int64) (string, error) {
		count, err := io.Copy(io.Discard, r)
		if err != nil || profile != "image" || count != n {
			t.Fatal("invalid inspection input")
		}
		return "image/png", nil
	})
	input := func(n int64) io.Reader {
		return io.MultiReader(strings.NewReader(pngMagic), io.LimitReader(zeros{}, n-int64(len(pngMagic))))
	}
	upload := func(declared string, n int64, finish func() error) (File, error) {
		return service.Upload(ctx, owner, "fixture.png", declared, input(n), finish)
	}
	finish := func() error { return nil }
	cleanup := func() {
		t.Helper()
		if found, err := cleaner.cleanupOne(ctx); err != nil || !found {
			t.Fatalf("cleanup: found=%t err=%v", found, err)
		}
	}
	if _, err := upload("image/png", 12, finish); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("nil inspector: %v", err)
	}
	service.Inspector = inspectFunc(func(context.Context, string, string, io.Reader, int64) (string, error) { return "", ErrUnavailable })
	if _, err := upload("image/png", 12, finish); !errors.Is(err, ErrUnavailable) || objects.puts != 0 {
		t.Fatalf("failed inspector: %v", err)
	}
	cleanup()
	service.Inspector = accepted
	if _, err := upload("image/jpeg", 12, finish); !errors.Is(err, ErrType) {
		t.Fatalf("mismatched MIME: %v", err)
	}
	cleanup()
	if _, err := upload("image/png", MaxFileBytes+1, finish); !errors.Is(err, ErrSize) {
		t.Fatalf("oversize: %v", err)
	}
	cleanup()
	if _, err := upload("image/png", 12, func() error { return ErrInvalid }); !errors.Is(err, ErrInvalid) {
		t.Fatalf("broken multipart: %v", err)
	}
	cleanup()
	service.Inspector = inspectFunc(func(_ context.Context, _ string, profile string, _ io.Reader, _ int64) (string, error) {
		if profile != "office" {
			t.Fatalf("Office inspection dispatched to %s", profile)
		}
		return "application/msword", nil
	})
	if _, err := service.Upload(ctx, owner, "fixture.doc", "application/msword", strings.NewReader("PK\x03\x04payload"), finish); !errors.Is(err, ErrInvalid) || objects.puts != 0 {
		t.Fatalf("ZIP must not be published as an OLE document: %v", err)
	}
	cleanup()
	service.Inspector = accepted
	objects.failPut = true
	if _, err := upload("image/png", 12, finish); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("storage failure: %v", err)
	}
	objects.failPut = false
	objects.failDelete = true
	if found, err := cleaner.cleanupOne(ctx); !found || err == nil {
		t.Fatalf("expected retriable object removal: %t %v", found, err)
	}
	objects.failDelete = false
	cleanup()
	service.Inspector = inspectFunc(func(ctx context.Context, id, profile string, r io.Reader, n int64) (string, error) {
		if _, err := service.Store.GetReady(ctx, owner, id); !errors.Is(err, ErrNotFound) {
			t.Fatalf("visible before inspection: %v", err)
		}
		if _, err := service.Store.reserve(ctx, owner, "second.png", MaxFileBytes, 1); !errors.Is(err, ErrQuota) {
			t.Fatalf("concurrent quota: %v", err)
		}
		return accepted(ctx, id, profile, r, n)
	})
	ready, err := upload("image/png", MaxFileBytes, finish)
	if err != nil || ready.Bytes == nil || *ready.Bytes != MaxFileBytes {
		t.Fatalf("inclusive limit: %v", err)
	}
	if _, err := service.Store.GetReady(ctx, other, ready.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("foreign read: %v", err)
	}
	if err := service.Store.deleteReady(ctx, other, ready.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("foreign delete: %v", err)
	}
	if _, err := service.download(ctx, other, ready.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("foreign download: %v", err)
	}
	if _, err := service.download(ctx, owner, ready.ID); err != nil {
		t.Fatal(err)
	}
	var uploadEvents, downloadEvents, completed int
	if err := admin.QueryRow(ctx, `SELECT count(*) FILTER(WHERE type='file.upload_completed'),
  count(*) FILTER(WHERE type='download_link_issued'),count(*) FILTER(WHERE type='download_completed')
  FROM platform.outbox_events WHERE payload->>'owner_id'=$1`, owner).Scan(&uploadEvents, &downloadEvents, &completed); err != nil ||
		uploadEvents != 1 || downloadEvents != 1 || completed != 0 {
		t.Fatalf("events: %d/%d/%d %v", uploadEvents, downloadEvents, completed, err)
	}
	if _, err := admin.Exec(ctx, "INSERT INTO platform.file_pins(owner_id,file_id,expires_at) VALUES ($1,$2,now()+interval '1 hour')", owner, ready.ID); err != nil {
		t.Fatal(err)
	}
	if err := service.Store.deleteReady(ctx, owner, ready.ID); !errors.Is(err, ErrPinned) {
		t.Fatalf("pinned delete: %v", err)
	}
	if _, err := admin.Exec(ctx, "UPDATE platform.files SET ready_at=now()-interval '2 hours',expires_at=now()-interval '1 hour' WHERE id=$1", ready.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := service.download(ctx, owner, ready.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expired download: %v", err)
	}
	if found, err := cleaner.cleanupOne(ctx); found || err != nil {
		t.Fatalf("expired pinned object removed: %t %v", found, err)
	}
	if _, err := admin.Exec(ctx, "DELETE FROM platform.file_pins WHERE file_id=$1", ready.ID); err != nil {
		t.Fatal(err)
	}
	cleanup()
	if found, err := cleaner.cleanupOne(ctx); found || err != nil {
		t.Fatalf("cleanup repeated incorrectly: %t %v", found, err)
	}

	// Exercise two actual concurrent reservations against the same account.
	var writers sync.WaitGroup
	start := make(chan struct{})
	results := make(chan error, 2)
	for range 2 {
		writers.Go(func() {
			<-start
			_, err := service.Store.reserve(ctx, owner, "competing.png", MaxFileBytes, 1)
			results <- err
		})
	}
	close(start)
	writers.Wait()
	a, b := <-results, <-results
	if !((a == nil && errors.Is(b, ErrQuota)) || (b == nil && errors.Is(a, ErrQuota))) {
		t.Fatalf("competing reservations: %v, %v", a, b)
	}
	if _, err := admin.Exec(ctx, "UPDATE platform.files SET state='deleting' WHERE owner_id=$1", owner); err != nil {
		t.Fatal(err)
	}
	cleanup()

	service.Inspector = accepted
	router := chi.NewRouter()
	pass := func(h http.Handler) http.Handler { return h }
	service.Routes(router, pass, pass, func(*http.Request) string { return owner })
	multipartUpload := func(filename string, closeBody bool) *httptest.ResponseRecorder {
		var body bytes.Buffer
		w := multipart.NewWriter(&body)
		h := textproto.MIMEHeader{"Content-Disposition": {`form-data; name="file"; filename="` + filename + `"`}, "Content-Type": {"image/png"}}
		part, err := w.CreatePart(h)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = io.Copy(part, input(12)); err != nil {
			t.Fatal(err)
		}
		if closeBody {
			if err := w.Close(); err != nil {
				t.Fatal(err)
			}
		}
		req := httptest.NewRequest(http.MethodPost, "/api/v1/files", &body)
		req.ContentLength = -1
		req.Header.Set("Content-Type", w.FormDataContentType())
		out := httptest.NewRecorder()
		router.ServeHTTP(out, req)
		return out
	}
	if out := multipartUpload("../fixture.png", true); out.Code != 422 {
		t.Fatalf("traversal: %d", out.Code)
	}
	if out := multipartUpload("fixture.png", false); out.Code != 422 {
		t.Fatalf("truncated body: %d %s", out.Code, out.Body)
	}
	cleanup()
	if out := multipartUpload("fixture.png", true); out.Code != 201 {
		t.Fatalf("multipart: %d %s", out.Code, out.Body)
	}
	files, _, err := service.Store.list(ctx, owner, "", 1)
	if err != nil || len(files) != 1 {
		t.Fatalf("list: %d %v", len(files), err)
	}
	if _, err := admin.Exec(ctx, "UPDATE platform.users SET status='deleting',deleting_at=now() WHERE id=$1", owner); err != nil {
		t.Fatal(err)
	}
	if _, err := admin.Exec(ctx, `INSERT INTO platform.privacy_jobs(owner_id,type,state)
	 VALUES ($1,'delete','requested')`, owner); err != nil {
		t.Fatal(err)
	}
	if found, err := cleaner.cleanupOne(ctx); found || err != nil {
		t.Fatalf("cleanup started before durable journal confirmation: %t %v", found, err)
	}
	if _, err := admin.Exec(ctx, "UPDATE platform.privacy_jobs SET state='running' WHERE owner_id=$1 AND type='delete'", owner); err != nil {
		t.Fatal(err)
	}
	cleanup()
}

func TestFileRoutesRejectMissingInspector(t *testing.T) {
	service := &Service{}
	router := chi.NewRouter()
	passthrough := func(h http.Handler) http.Handler { return h }
	service.Routes(router, passthrough, passthrough, func(*http.Request) string { return "" })
	w := httptest.NewRecorder()
	router.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/api/v1/files", strings.NewReader("file")))
	if w.Code != 503 {
		t.Fatalf("expected fail-closed route, got %d", w.Code)
	}
}
