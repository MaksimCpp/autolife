package accounts

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"autolife/backend/internal/platform/httpserver"
	"github.com/jackc/pgx/v5/pgxpool"
)

func BenchmarkPassword(b *testing.B) {
	for b.Loop() {
		if _, err := hashPassword(b.Context(), "benchmark-password"); err != nil {
			b.Fatal(err)
		}
	}
}

func TestRegistrationAndVerification(t *testing.T) {
	apiURL, migrationURL := os.Getenv("AUTOLIFE_API_DATABASE_URL"), os.Getenv("AUTOLIFE_MIGRATION_DATABASE_URL")
	if apiURL == "" || migrationURL == "" {
		t.Skip("requires an isolated migrated PostgreSQL database")
	}
	api, err := pgxpool.New(t.Context(), apiURL)
	if err != nil {
		t.Fatal("cannot configure API pool")
	}
	defer api.Close()
	owner, err := pgxpool.New(t.Context(), migrationURL)
	if err != nil {
		t.Fatal("cannot configure migration pool")
	}
	defer owner.Close()

	const origin = "http://127.0.0.1:8080"
	logger := slog.New(slog.DiscardHandler)
	h, err := NewHTTP(NewStore(api), logger, origin, "mail:1025", strings.Repeat("ab", 32))
	if err != nil {
		t.Fatal(err)
	}
	var verification, resetToken string
	h.mail = func(_ context.Context, _ string, action, token string) error {
		if action == "verify" {
			verification = token
		} else if action == "reset" {
			resetToken = token
		}
		return nil
	}
	router := httpserver.Handler(logger, 8192, func(context.Context) error { return nil }, h.Routes)
	csrf := httptest.NewRecorder()
	router.ServeHTTP(csrf, httptest.NewRequest(http.MethodGet, "/api/v1/auth/csrf", nil))
	var data map[string]string
	if err := json.Unmarshal(csrf.Body.Bytes(), &data); err != nil || csrf.Code != 200 || len(csrf.Result().Cookies()) != 1 {
		t.Fatalf("CSRF issuance failed (%d): %v", csrf.Code, err)
	}
	cookie := csrf.Result().Cookies()[0]
	unique := uint32(time.Now().UnixNano())
	source := fmt.Sprintf("127.%d.%d.%d:1234", byte(unique>>16), byte(unique>>8), byte(unique))
	email := fmt.Sprintf("verify-%d@example.test", time.Now().UnixNano())
	defer func() {
		ctx := context.Background()
		_, _ = owner.Exec(ctx, `DELETE FROM platform.outbox_events WHERE logical_key IN
			(SELECT id::text FROM platform.users WHERE normalized_email=$1)`, email)
		_, _ = owner.Exec(ctx, "DELETE FROM platform.users WHERE normalized_email=$1", email)
	}()

	post := func(path, requestOrigin, csrfHeader string, payload map[string]string) *httptest.ResponseRecorder {
		t.Helper()
		body, err := json.Marshal(payload)
		if err != nil {
			t.Fatal(err)
		}
		r := httptest.NewRequest(http.MethodPost, "/api/v1/auth/"+path, bytes.NewReader(body))
		r.AddCookie(cookie)
		r.RemoteAddr = source
		r.Header.Set("Origin", requestOrigin)
		r.Header.Set("X-CSRF-Token", csrfHeader)
		w := httptest.NewRecorder()
		router.ServeHTTP(w, r)
		return w
	}
	input := map[string]string{"email": email, "password": "fixture-password"}
	if w := post("register", "http://not-local.example", data["csrf_token"], input); w.Code != 403 {
		t.Fatalf("invalid Origin: %d", w.Code)
	}
	if w := post("register", origin, "invalid", input); w.Code != 403 {
		t.Fatalf("invalid CSRF: %d", w.Code)
	}
	first := post("register", origin, data["csrf_token"], input)
	if first.Code != 202 || !validToken(verification) {
		t.Fatalf("registration failed: %d", first.Code)
	}
	original := verification
	if w := post("register", origin, data["csrf_token"], input); w.Code != first.Code || w.Body.String() != first.Body.String() {
		t.Fatalf("duplicate registration response differs: %d", w.Code)
	}
	if _, err := owner.Exec(t.Context(), `UPDATE platform.auth_tokens SET created_at=now()-interval '2 hours',expires_at=now()-interval '1 hour' WHERE token_hash=$1`, hashToken(original)); err != nil {
		t.Fatal(err)
	}
	if w := post("verify", origin, data["csrf_token"], map[string]string{"token": original}); w.Code != 422 {
		t.Fatalf("expired token: %d", w.Code)
	}
	input = map[string]string{"token": verification}
	if w := post("verify", origin, data["csrf_token"], input); w.Code != 204 {
		t.Fatalf("verification: %d", w.Code)
	}
	if w := post("verify", origin, data["csrf_token"], input); w.Code != 422 {
		t.Fatalf("replayed token: %d", w.Code)
	}
	password := "fixture-password"
	if w := post("login", origin, data["csrf_token"], map[string]string{"email": email, "password": "incorrect-password"}); w.Code != 401 {
		t.Fatalf("incorrect credentials: %d", w.Code)
	}
	login := post("login", origin, data["csrf_token"], map[string]string{"email": email, "password": password})
	if login.Code != 200 {
		t.Fatalf("login: %d %s", login.Code, login.Body)
	}
	var authCookie *http.Cookie
	for _, c := range login.Result().Cookies() {
		if c.Name == h.cookieName("session") {
			authCookie = c
		}
	}
	if authCookie == nil || !authCookie.HttpOnly || authCookie.SameSite != http.SameSiteStrictMode || !validToken(authCookie.Value) {
		t.Fatal("invalid session cookie")
	}
	logout := func(token string) int {
		r := httptest.NewRequest(http.MethodPost, "/api/v1/auth/logout", nil)
		r.AddCookie(authCookie)
		r.Header.Set("Origin", origin)
		r.Header.Set("X-CSRF-Token", token)
		w := httptest.NewRecorder()
		router.ServeHTTP(w, r)
		return w.Code
	}
	if code := logout(data["csrf_token"]); code != 403 {
		t.Fatalf("pre-auth token accepted for a session: %d", code)
	}
	if code := logout(csrfValue("session", authCookie.Value)); code != 204 {
		t.Fatalf("logout: %d", code)
	}
	if code := logout(csrfValue("session", authCookie.Value)); code != 401 {
		t.Fatalf("revoked session: %d", code)
	}
	login = post("login", origin, data["csrf_token"], map[string]string{"email": email, "password": password})
	if login.Code != 200 {
		t.Fatalf("second login: %d", login.Code)
	}
	authCookie = login.Result().Cookies()[0]
	if _, err := owner.Exec(t.Context(), `UPDATE platform.sessions SET created_at=now()-interval '2 hours',expires_at=now()-interval '1 hour' WHERE token_hash=$1`, hashToken(authCookie.Value)); err != nil {
		t.Fatal(err)
	}
	if code := logout(csrfValue("session", authCookie.Value)); code != 401 {
		t.Fatalf("expired session: %d", code)
	}
	loginSession := func(password string) *http.Cookie {
		t.Helper()
		w := post("login", origin, data["csrf_token"], map[string]string{"email": email, "password": password})
		if w.Code != 200 {
			t.Fatalf("login: %d %s", w.Code, w.Body)
		}
		return w.Result().Cookies()[0]
	}
	oldFirst := loginSession(password)
	oldSecond := loginSession(password)
	got := post("password/forgot", origin, data["csrf_token"], map[string]string{"email": email})
	missing := post("password/forgot", origin, data["csrf_token"], map[string]string{"email": "missing-" + email})
	if got.Code != 202 || missing.Code != got.Code || missing.Body.String() != got.Body.String() || !validToken(resetToken) {
		t.Fatal("forgot response or mail delivery incorrect")
	}
	newPassword := "tested-reset-password"
	resetPayload := map[string]string{"token": resetToken, "new_password": newPassword}
	if w := post("password/reset", origin, data["csrf_token"], resetPayload); w.Code != 204 {
		t.Fatalf("reset: %d %s", w.Code, w.Body)
	}
	if w := post("password/reset", origin, data["csrf_token"], resetPayload); w.Code != 422 {
		t.Fatalf("reset replay: %d", w.Code)
	}
	for _, old := range []*http.Cookie{oldFirst, oldSecond} {
		authCookie = old
		if logout(csrfValue("session", authCookie.Value)) != 401 {
			t.Fatal("reset left an active session")
		}
	}
	oldFirst = loginSession(newPassword)
	oldSecond = loginSession(newPassword)
	r := httptest.NewRequest(http.MethodPost, "/api/v1/auth/password/change",
		strings.NewReader(`{"current_password":"tested-reset-password","new_password":"tested-change-password"}`))
	r.AddCookie(oldFirst)
	r.Header.Set("Origin", origin)
	r.Header.Set("X-CSRF-Token", csrfValue("session", oldFirst.Value))
	change := httptest.NewRecorder()
	router.ServeHTTP(change, r)
	if change.Code != 204 || len(change.Result().Cookies()) != 1 {
		t.Fatalf("change: %d %s", change.Code, change.Body)
	}
	for _, old := range []*http.Cookie{oldFirst, oldSecond} {
		authCookie = old
		if logout(csrfValue("session", authCookie.Value)) != 401 {
			t.Fatal("password change left an old session active")
		}
	}
	authCookie = change.Result().Cookies()[0]
	if logout(csrfValue("session", authCookie.Value)) != 204 {
		t.Fatal("password change failed to issue the rotated session")
	}
	for range 12 {
		w := post("login", origin, data["csrf_token"], map[string]string{"email": email, "password": "incorrect-password"})
		if w.Code != 401 && w.Code != 429 {
			t.Fatalf("unexpected login response: %d", w.Code)
		}
	}
	if w := post("login", origin, data["csrf_token"], map[string]string{"email": email, "password": password}); w.Code != 429 {
		t.Fatalf("rate limit did not reject request: %d", w.Code)
	}

}

func TestProfileAndSessionOwnership(t *testing.T) {
	apiURL, migrationURL := os.Getenv("AUTOLIFE_API_DATABASE_URL"), os.Getenv("AUTOLIFE_MIGRATION_DATABASE_URL")
	if apiURL == "" || migrationURL == "" {
		t.Skip("requires an isolated migrated PostgreSQL database")
	}
	ctx := t.Context()
	api, err := pgxpool.New(ctx, apiURL)
	if err != nil {
		t.Fatal("cannot configure API pool")
	}
	t.Cleanup(api.Close)
	owner, err := pgxpool.New(ctx, migrationURL)
	if err != nil {
		t.Fatal("cannot configure migration pool")
	}
	t.Cleanup(owner.Close)
	store := NewStore(api)
	fixture := func() (session, string) {
		t.Helper()
		email := fmt.Sprintf("profile-%d@example.test", time.Now().UnixNano())
		token, err := newToken()
		if err != nil {
			t.Fatal(err)
		}
		created, err := store.register(ctx, email, "fixture", hashToken(token))
		if err != nil || !created {
			t.Fatalf("fixture account: %v", err)
		}
		if err := store.verify(ctx, hashToken(token), "test"); err != nil {
			t.Fatal(err)
		}
		user, err := store.findAccount(ctx, email)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			cleanup := context.Background()
			_, _ = owner.Exec(cleanup, "DELETE FROM platform.users WHERE id=$1", user.ID)
			_, _ = owner.Exec(cleanup, "DELETE FROM platform.outbox_events WHERE payload->>'owner_id'=$1", user.ID)
		})
		token, err = newToken()
		if err != nil {
			t.Fatal(err)
		}
		if err := store.login(ctx, user, hashToken(token), nil); err != nil {
			t.Fatal(err)
		}
		current, err := store.getSession(ctx, hashToken(token))
		if err != nil {
			t.Fatal(err)
		}
		return current, token
	}
	first, token := fixture()
	second, _ := fixture()
	const origin = "http://127.0.0.1:8080"
	logger := slog.New(slog.DiscardHandler)
	h, err := NewHTTP(store, logger, origin, "mail:1025", strings.Repeat("ab", 32))
	if err != nil {
		t.Fatal(err)
	}
	router := httpserver.Handler(logger, 8192, func(context.Context) error { return nil }, h.Routes)
	request := func(method, path, body string) *httptest.ResponseRecorder {
		t.Helper()
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		r.AddCookie(&http.Cookie{Name: h.cookieName("session"), Value: token})
		r.Header.Set("Origin", origin)
		r.Header.Set("X-CSRF-Token", csrfValue("session", token))
		w := httptest.NewRecorder()
		router.ServeHTTP(w, r)
		return w
	}
	read := request(http.MethodGet, "/api/v1/me", "")
	if read.Code != 200 || !strings.Contains(read.Body.String(), `"analytics_consent":false`) {
		t.Fatalf("profile defaults: %d %s", read.Code, read.Body)
	}
	if w := request(http.MethodPatch, "/api/v1/me", `{"timezone":"bogus/zone"}`); w.Code != 422 {
		t.Fatalf("invalid timezone: %d", w.Code)
	}
	if w := request(http.MethodPatch, "/api/v1/me", `{"owner_id":"other","locale":"en"}`); w.Code != 400 {
		t.Fatalf("client supplied owner: %d", w.Code)
	}
	updated := request(http.MethodPatch, "/api/v1/me", `{"locale":"en","timezone":"Europe/Moscow","theme":"dark","analytics_consent":true}`)
	if updated.Code != 200 || !strings.Contains(updated.Body.String(), `"analytics_consent":true`) {
		t.Fatalf("profile update: %d %s", updated.Code, updated.Body)
	}
	persisted := request(http.MethodGet, "/api/v1/me", "")
	if persisted.Code != 200 || !strings.Contains(persisted.Body.String(), `"timezone":"Europe/Moscow"`) {
		t.Fatalf("profile not persisted: %d %s", persisted.Code, persisted.Body)
	}
	sessions := request(http.MethodGet, "/api/v1/me/sessions", "")
	if sessions.Code != 200 || !strings.Contains(sessions.Body.String(), first.ID) || strings.Contains(sessions.Body.String(), second.ID) {
		t.Fatalf("owner-scoped sessions: %d %s", sessions.Code, sessions.Body)
	}
	if w := request(http.MethodDelete, "/api/v1/me/sessions/"+second.ID, ""); w.Code != 404 {
		t.Fatalf("other owner's session: %d", w.Code)
	}
	if w := request(http.MethodDelete, "/api/v1/me/sessions/"+first.ID, ""); w.Code != 204 {
		t.Fatalf("revoking own session: %d", w.Code)
	}
	if w := request(http.MethodGet, "/api/v1/me", ""); w.Code != 401 {
		t.Fatalf("revoked session still authorized: %d", w.Code)
	}
}
