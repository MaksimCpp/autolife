package accounts

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"autolife/backend/internal/platform/httpserver"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestPrivacyLifecycle(t *testing.T) {
	dsns := []string{os.Getenv("AUTOLIFE_API_DATABASE_URL"), os.Getenv("AUTOLIFE_WORKER_DATABASE_URL"), os.Getenv("AUTOLIFE_MIGRATION_DATABASE_URL")}
	ctx := t.Context()
	pools := make([]*pgxpool.Pool, len(dsns))
	for i, dsn := range dsns {
		if dsn == "" {
			t.Skip("requires API, worker and migration connections to isolated PostgreSQL")
		}
		pool, err := pgxpool.New(ctx, dsn)
		if err != nil {
			t.Fatal("cannot configure test connection")
		}
		pools[i] = pool
		t.Cleanup(pool.Close)
	}
	api, worker, owner := NewStore(pools[0]), NewStore(pools[1]), pools[2]
	fixture := func() (account, string) {
		t.Helper()
		email := fmt.Sprintf("privacy-%d@example.test", time.Now().UnixNano())
		token, err := newToken()
		if err != nil {
			t.Fatal(err)
		}
		created, err := api.register(ctx, email, "fixture-password-hash", hashToken(token))
		if err != nil || !created {
			t.Fatalf("create account: %v", err)
		}
		if err := api.verify(ctx, hashToken(token), "test"); err != nil {
			t.Fatal(err)
		}
		user, err := api.findAccount(ctx, email)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			cleanup := context.Background()
			_, _ = owner.Exec(cleanup, "DELETE FROM platform.privacy_jobs WHERE owner_id=$1", user.ID)
			_, _ = owner.Exec(cleanup, "DELETE FROM platform.outbox_events WHERE payload->>'owner_id'=$1", user.ID)
			_, _ = owner.Exec(cleanup, "DELETE FROM platform.audit_events WHERE actor=$1", user.ID)
			_, _ = owner.Exec(cleanup, "DELETE FROM platform.users WHERE id=$1", user.ID)
		})
		token, err = newToken()
		if err != nil {
			t.Fatal(err)
		}
		if err := api.login(ctx, user, hashToken(token), nil); err != nil {
			t.Fatal(err)
		}
		return user, token
	}
	user, token := fixture()
	otherAccount, other := fixture()
	const origin = "http://127.0.0.1:8080"
	logger := slog.New(slog.DiscardHandler)
	h, err := NewHTTP(api, logger, origin, "mail:1025", strings.Repeat("ab", 32))
	if err != nil {
		t.Fatal(err)
	}
	journal, err := NewJournal(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	h.UseJournal(journal)
	router := httpserver.Handler(logger, 8192, func(context.Context) error { return nil }, h.Routes)
	request := func(method, path, credential, body string) *httptest.ResponseRecorder {
		t.Helper()
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		r.AddCookie(&http.Cookie{Name: h.cookieName("session"), Value: credential})
		r.Header.Set("Origin", origin)
		r.Header.Set("X-CSRF-Token", csrfValue("session", credential))
		r.Header.Set("Idempotency-Key", "privacy-test")
		w := httptest.NewRecorder()
		router.ServeHTTP(w, r)
		return w
	}
	field := func(w *httptest.ResponseRecorder, key string, status int) string {
		t.Helper()
		var data map[string]any
		if w.Code != status || json.Unmarshal(w.Body.Bytes(), &data) != nil {
			t.Fatalf("expected %d: %d %s", status, w.Code, w.Body)
		}
		value, _ := data[key].(string)
		if value == "" {
			t.Fatalf("missing %s: %s", key, w.Body)
		}
		return value
	}
	advance := func(id, want string) {
		t.Helper()
		for range 16 {
			var state string
			if err := owner.QueryRow(ctx, "SELECT state FROM platform.privacy_jobs WHERE id=$1", id).Scan(&state); err != nil {
				t.Fatal(err)
			}
			if state == want {
				return
			}
			if processed, err := worker.processPrivacy(ctx, journal); !processed || err != nil {
				t.Fatalf("waiting for privacy state %s: processed=%v, err=%v", want, processed, err)
			}
		}
		t.Fatalf("privacy job did not reach %s", want)
	}
	for _, item := range []struct{ accountID, name string }{
		{user.ID, "owned-workflow-fixture"}, {otherAccount.ID, "other-workflow-fixture"},
	} {
		var id string
		if err := owner.QueryRow(ctx, "INSERT INTO platform.workflows (owner_id,name) VALUES ($1,$2) RETURNING id::text", item.accountID, item.name).Scan(&id); err != nil {
			t.Fatal(err)
		}
		if _, err := owner.Exec(ctx, "INSERT INTO platform.workflow_drafts (owner_id,workflow_id,definition) VALUES ($1,$2,'{}')", item.accountID, id); err != nil {
			t.Fatal(err)
		}
	}
	id := field(request("POST", "/api/v1/me/export", token, "{}"), "id", 202)
	if again := field(request("POST", "/api/v1/me/export", token, "{}"), "id", 202); again != id {
		t.Fatal("export idempotency lost")
	}
	path := "/api/v1/me/exports/" + id
	if w := request("GET", path, other, ""); w.Code != 404 {
		t.Fatalf("foreign export: %d", w.Code)
	}
	advance(id, "ready")
	if status := field(request("GET", path, token, ""), "state", 200); status != "ready" {
		t.Fatalf("export status %s", status)
	}
	link := field(request("POST", path+"/download", token, ""), "url", 200)
	var issued int
	if err := owner.QueryRow(ctx, `SELECT count(*) FROM platform.outbox_events
		WHERE type='download_link_issued' AND purpose='operational' AND payload->>'export_id'=$1`, id).Scan(&issued); err != nil || issued != 1 {
		t.Fatalf("expected one operational link issuance: %d %v", issued, err)
	}
	data := request("GET", link, token, "")
	if data.Code != 200 || !strings.Contains(data.Body.String(), user.Email) || strings.Contains(data.Body.String(), user.PasswordHash) || strings.Contains(data.Body.String(), "token_hash") {
		t.Fatalf("unexpected export: %d", data.Code)
	}
	if !strings.Contains(data.Body.String(), "owned-workflow-fixture") || strings.Contains(data.Body.String(), "other-workflow-fixture") {
		t.Fatal("workflow export did not preserve owner scope")
	}
	if w := request("GET", link, other, ""); w.Code != 404 {
		t.Fatalf("foreign download: %d", w.Code)
	}
	if _, err := owner.Exec(ctx, "UPDATE platform.privacy_jobs SET completed_at=now()-interval '2 hours',expires_at=now()-interval '1 hour' WHERE id=$1", id); err != nil {
		t.Fatal(err)
	}
	if w := request("GET", link, token, ""); w.Code != 404 {
		t.Fatalf("expired download: %d", w.Code)
	}
	if err := worker.sweepPrivacy(ctx); err != nil {
		t.Fatal(err)
	}

	if _, err := owner.Exec(ctx, "UPDATE platform.sessions SET created_at=now()-interval '20 minutes',last_authenticated_at=now()-interval '20 minutes' WHERE user_id=$1", user.ID); err != nil {
		t.Fatal(err)
	}
	deleteBody := `{"confirm":true}`
	if w := request("DELETE", "/api/v1/me", token, deleteBody); w.Code != 401 {
		t.Fatalf("accepted without recent credentials: %d", w.Code)
	}
	if _, err := owner.Exec(ctx, "UPDATE platform.sessions SET last_authenticated_at=now() WHERE user_id=$1", user.ID); err != nil {
		t.Fatal(err)
	}
	h.UseJournal(Journal{})
	if w := request("DELETE", "/api/v1/me", token, deleteBody); w.Code != 503 {
		t.Fatalf("accepted before journal commit: %d", w.Code)
	}
	if w := request("GET", "/api/v1/me", token, ""); w.Code != 401 {
		t.Fatalf("deleting account remained active: %d", w.Code)
	}
	h.UseJournal(journal)
	deletion := field(request("DELETE", "/api/v1/me", token, deleteBody), "id", 202)
	if _, err := os.Stat(filepath.Join(journal.directory, deletion+".json")); err != nil {
		t.Fatal(err)
	}
	var fileID string
	if err := owner.QueryRow(ctx, "INSERT INTO platform.files (owner_id,display_name) VALUES ($1,'privacy-fixture.png') RETURNING id::text", user.ID).Scan(&fileID); err != nil {
		t.Fatal(err)
	}
	defer owner.Exec(ctx, "DELETE FROM platform.files WHERE id=$1", fileID)
	advance(deletion, "running")
	if _, err := owner.Exec(ctx, "DELETE FROM platform.files WHERE id=$1", fileID); err != nil {
		t.Fatal(err)
	}
	if _, err := owner.Exec(ctx, "UPDATE platform.privacy_jobs SET next_attempt_at=now() WHERE id=$1", deletion); err != nil {
		t.Fatal(err)
	}
	advance(deletion, "succeeded")
	if again := field(request("DELETE", "/api/v1/me", token, deleteBody), "id", 202); again != deletion {
		t.Fatal("deletion receipt changed")
	}
	var remaining, audited int
	if err := owner.QueryRow(ctx, `SELECT
		(SELECT count(*) FROM platform.users WHERE id=$1)
		+(SELECT count(*) FROM platform.workflows WHERE owner_id=$1)
		+(SELECT count(*) FROM platform.user_settings WHERE user_id=$1)
		+(SELECT count(*) FROM platform.sessions WHERE user_id=$1)
		+(SELECT count(*) FROM platform.auth_tokens WHERE user_id=$1)
		+(SELECT count(*) FROM platform.privacy_jobs WHERE owner_id=$1 AND type='export')
		+(SELECT count(*) FROM platform.outbox_events WHERE payload->>'owner_id'=$1::text),
		(SELECT count(*) FROM platform.audit_events WHERE actor=$1 AND action='privacy.delete')`, user.ID).Scan(&remaining, &audited); err != nil || remaining != 0 || audited != 2 {
		t.Fatalf("deletion cleanup: rows=%d audit=%d err=%v", remaining, audited, err)
	}
}
