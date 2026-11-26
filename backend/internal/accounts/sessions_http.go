package accounts

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"errors"
	"net"
	"net/http"

	"autolife/backend/internal/platform/httpserver"
	"github.com/jackc/pgx/v5"
)

type sessionKey struct{}

func currentSession(r *http.Request) session {
	return r.Context().Value(sessionKey{}).(session)
}

func (h *HTTP) session(r *http.Request) (session, string, error) {
	cookie, err := r.Cookie(h.cookieName("session"))
	if err != nil || !validToken(cookie.Value) {
		return session{}, "", pgx.ErrNoRows
	}
	current, err := h.store.getSession(r.Context(), hashToken(cookie.Value))
	return current, cookie.Value, err
}

func (h *HTTP) require(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		current, _, err := h.session(r)
		if errors.Is(err, pgx.ErrNoRows) {
			unauthenticated(w, r)
			return
		}
		if err != nil {
			h.fail(w, r)
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), sessionKey{}, current)))
	})
}

func unauthenticated(w http.ResponseWriter, r *http.Request) {
	httpserver.WriteError(w, r, http.StatusUnauthorized, "UNAUTHENTICATED", "Authentication required")
}

func (h *HTTP) allow(w http.ResponseWriter, r *http.Request, account string) bool {
	client, _, _ := net.SplitHostPort(r.RemoteAddr)
	if forwarded := r.Header.Get("X-Autolife-Client-IP"); h.TrustProxy && net.ParseIP(forwarded) != nil {
		client = forwarded
	}
	attempt := func(scope, value string, limit int) bool {
		mac := hmac.New(sha256.New, h.key)
		mac.Write([]byte("rate:" + scope + ":" + value))
		allowed, err := h.store.rate(r.Context(), scope, mac.Sum(nil), limit)
		if err != nil {
			h.fail(w, r)
			return false
		}
		if !allowed {
			w.Header().Set("Retry-After", "900")
			httpserver.WriteError(w, r, http.StatusTooManyRequests, "RATE_LIMITED", "Too many requests")
		}
		return allowed
	}
	return attempt("source", client, 100) && (account == "" || attempt("account", account, 12))
}

const dummyPasswordHash = "$argon2id$v=19$m=19456,t=2,p=1$AAAAAAAAAAAAAAAAAAAAAA$AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"

func (h *HTTP) login(w http.ResponseWriter, r *http.Request) {
	var input struct{ Email, Password string }
	if !decode(w, r, &input) {
		return
	}
	email, err := normalizeEmail(input.Email)
	if err != nil || !validPassword(input.Password) {
		unauthenticated(w, r)
		return
	}
	if !h.allow(w, r, email) {
		return
	}
	ctx := r.Context()
	user, err := h.store.findAccount(ctx, email)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		h.fail(w, r)
		return
	}
	passwordHash := user.PasswordHash
	if passwordHash == "" {
		passwordHash = dummyPasswordHash
	}
	matches, err := passwordMatches(ctx, input.Password, passwordHash)
	if err != nil {
		h.fail(w, r)
		return
	}
	if !matches || user.ID == "" {
		unauthenticated(w, r)
		return
	}
	if !user.Verified {
		httpserver.WriteError(w, r, http.StatusForbidden, "EMAIL_NOT_VERIFIED", "Verify your email before signing in")
		return
	}
	token, err := newToken()
	if err != nil {
		h.fail(w, r)
		return
	}
	var previous []byte
	if cookie, err := r.Cookie(h.cookieName("session")); err == nil && validToken(cookie.Value) {
		previous = hashToken(cookie.Value)
	}
	if err := h.store.login(ctx, user, hashToken(token), previous); errors.Is(err, pgx.ErrNoRows) {
		unauthenticated(w, r)
		return
	} else if err != nil {
		h.fail(w, r)
		return
	}
	h.cookie(w, "session", token, sessionTTL)
	h.cookie(w, "pre", "", 0)
	httpserver.WriteJSON(w, http.StatusOK, map[string]string{"id": user.ID, "email": user.Email})
}

func (h *HTTP) logout(w http.ResponseWriter, r *http.Request) {
	current := currentSession(r)
	err := h.store.revoke(r.Context(), current, current.ID, httpserver.RequestID(r))
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		h.fail(w, r)
		return
	}
	h.cookie(w, "session", "", 0)
	w.WriteHeader(http.StatusNoContent)
}

func (h *HTTP) RequireSession(next http.Handler) http.Handler {
	return h.require(next)
}

func (h *HTTP) ProtectSession(next http.Handler) http.Handler {
	return h.protect("session")(next)
}

func SessionOwner(r *http.Request) string {
	return currentSession(r).OwnerID
}
