package accounts

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"autolife/backend/internal/platform/httpserver"
	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
)

type HTTP struct {
	store      *Store
	logger     *slog.Logger
	origin     string
	secure     bool
	key        []byte
	journal    Journal
	TrustProxy bool
	mail       func(context.Context, string, string, string) error
}

func NewHTTP(store *Store, logger *slog.Logger, origin, smtpAddress, secret string) (*HTTP, error) {
	u, err := url.Parse(origin)
	if err != nil || u.Host == "" || u.User != nil || u.RequestURI() != "/" || u.Fragment != "" ||
		(u.Scheme != "https" && !(u.Scheme == "http" && (u.Hostname() == "127.0.0.1" || u.Hostname() == "localhost"))) {
		return nil, errors.New("AUTOLIFE_PUBLIC_ORIGIN must be an HTTPS origin or a loopback development origin")
	}
	if host, port, err := net.SplitHostPort(smtpAddress); err != nil || host == "" || port == "" {
		return nil, errors.New("AUTOLIFE_DEV_SMTP_ADDR must be a host:port")
	}
	if origin != u.Scheme+"://"+u.Host {
		return nil, errors.New("AUTOLIFE_PUBLIC_ORIGIN must not include a path")
	}
	key, err := hex.DecodeString(secret)
	if err != nil || len(key) != 32 {
		return nil, errors.New("AUTOLIFE_AUTH_SECRET must be 32 random bytes encoded as hex")
	}
	mailer := devMailer{address: smtpAddress, origin: origin}
	return &HTTP{store: store, logger: logger, origin: origin, secure: u.Scheme == "https", key: key, mail: mailer.send}, nil
}

func (h *HTTP) UseJournal(journal Journal) {
	h.journal = journal
}

func (h *HTTP) Routes(router chi.Router) {
	router.Route("/api/v1/auth", func(r chi.Router) {
		r.Get("/csrf", h.csrf)
		r.With(h.protect("pre")).Post("/register", h.register)
		r.With(h.protect("pre")).Post("/verify", h.verify)
		r.With(h.protect("pre")).Post("/login", h.login)
		r.With(h.require, h.protect("session")).Post("/logout", h.logout)
		r.With(h.protect("pre")).Post("/password/forgot", h.forgot)
		r.With(h.protect("pre")).Post("/password/reset", h.reset)
		r.With(h.require, h.protect("session")).Post("/password/change", h.change)
	})
	router.Route("/api/v1/me", func(r chi.Router) {
		r.With(h.protect("session")).Delete("/", h.deleteAccount)
		r.Group(func(r chi.Router) {
			r.Use(h.require)
			r.Get("/", h.getProfile)
			r.With(h.protect("session")).Patch("/", h.patchProfile)
			r.Get("/sessions", h.listSessions)
			r.With(h.protect("session")).Delete("/sessions/{id}", h.revokeSession)
			r.With(h.protect("session")).Post("/export", h.requestExport)
			r.Get("/exports/{id}", h.exportStatus)
			r.With(h.protect("session")).Post("/exports/{id}/download", h.exportDownload)
			r.Get("/exports/{id}/content", h.exportContent)
		})
	})
}

func (h *HTTP) cookieName(kind string) string {
	if h.secure {
		return "__Host-autolife-" + kind
	}
	return "autolife-" + kind
}

func (h *HTTP) cookie(w http.ResponseWriter, kind, value string, lifetime time.Duration) {
	maxAge := int(lifetime / time.Second)
	if lifetime <= 0 {
		maxAge = -1
	}
	http.SetCookie(w, &http.Cookie{Name: h.cookieName(kind), Value: value, Path: "/",
		HttpOnly: true, Secure: h.secure, SameSite: http.SameSiteStrictMode, MaxAge: maxAge})
}

func csrfValue(kind, token string) string {
	return base64.RawURLEncoding.EncodeToString(hashToken("autolife-csrf:" + kind + ":" + token))
}

func (h *HTTP) sign(kind, token string, expires time.Time) string {
	value := token + "." + strconv.FormatInt(expires.Unix(), 10)
	mac := hmac.New(sha256.New, h.key)
	mac.Write([]byte(kind + ":" + value))
	return value + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func (h *HTTP) validContext(kind, value string) bool {
	if kind == "session" {
		return validToken(value)
	}
	parts := strings.Split(value, ".")
	if len(parts) != 3 || !validToken(parts[0]) {
		return false
	}
	expires, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil || !time.Unix(expires, 0).After(time.Now()) {
		return false
	}
	return hmac.Equal([]byte(value), []byte(h.sign(kind, parts[0], time.Unix(expires, 0))))
}

func (h *HTTP) protect(kind string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			cookie, err := r.Cookie(h.cookieName(kind))
			if r.Header.Get("Origin") != h.origin || err != nil || !h.validContext(kind, cookie.Value) ||
				subtle.ConstantTimeCompare([]byte(r.Header.Get("X-CSRF-Token")), []byte(csrfValue(kind, cookie.Value))) != 1 {
				httpserver.WriteError(w, r, http.StatusForbidden, "CSRF_REJECTED", "Request verification failed")
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

func (h *HTTP) csrf(w http.ResponseWriter, r *http.Request) {
	if origin := r.Header.Get("Origin"); origin != "" && origin != h.origin {
		httpserver.WriteError(w, r, http.StatusForbidden, "CSRF_REJECTED", "Request verification failed")
		return
	}
	current, sessionToken, err := h.session(r)
	if err == nil && current.ID != "" && r.URL.Query().Get("pre") != "1" {
		httpserver.WriteJSON(w, http.StatusOK, map[string]string{"csrf_token": csrfValue("session", sessionToken)})
		return
	}
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		h.fail(w, r)
		return
	}
	token, err := newToken()
	if err != nil {
		h.fail(w, r)
		return
	}
	const lifetime = 15 * time.Minute
	value := h.sign("pre", token, time.Now().Add(lifetime))
	h.cookie(w, "pre", value, lifetime)
	httpserver.WriteJSON(w, http.StatusOK, map[string]string{"csrf_token": csrfValue("pre", value)})
}

func decode(w http.ResponseWriter, r *http.Request, dest any) bool {
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<10))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(dest); err != nil || decoder.Decode(new(any)) != io.EOF {
		httpserver.WriteError(w, r, http.StatusBadRequest, "INVALID_JSON", "Invalid JSON request")
		return false
	}
	return true
}

func (h *HTTP) fail(w http.ResponseWriter, r *http.Request) {
	h.logger.Error("account operation failed", "request_id", httpserver.RequestID(r))
	httpserver.WriteError(w, r, http.StatusServiceUnavailable, "UNAVAILABLE", "Service temporarily unavailable")
}

func (h *HTTP) denied(r *http.Request, owner, action, id string) {
	err := h.store.transaction(r.Context(), func(tx pgx.Tx) error {
		return audit(r.Context(), tx, owner, action, id, "denied", httpserver.RequestID(r))
	})
	if err != nil {
		h.logger.Warn("cannot record denied account operation", "request_id", httpserver.RequestID(r))
	}
}

func invalid(w http.ResponseWriter, r *http.Request, message string) {
	httpserver.WriteError(w, r, http.StatusUnprocessableEntity, "VALIDATION_ERROR", message)
}

func (h *HTTP) register(w http.ResponseWriter, r *http.Request) {
	var input struct{ Email, Password string }
	if !decode(w, r, &input) {
		return
	}
	email, err := normalizeEmail(input.Email)
	if err != nil || !validPassword(input.Password) {
		invalid(w, r, "Enter a valid email and a password of at least 12 characters")
		return
	}
	if !h.allow(w, r, email) {
		return
	}
	ctx := r.Context()
	passwordHash, err := hashPassword(ctx, input.Password)
	if err != nil {
		h.fail(w, r)
		return
	}
	token, err := newToken()
	if err != nil {
		h.fail(w, r)
		return
	}
	created, err := h.store.register(ctx, email, passwordHash, hashToken(token))
	if err != nil {
		h.fail(w, r)
		return
	}
	if !created {
		user, err := h.store.findAccount(ctx, email)
		if err == nil && !user.Verified {
			matches, matchErr := passwordMatches(ctx, input.Password, user.PasswordHash)
			if matchErr == nil && matches {
				created = h.store.issueToken(ctx, user, "verify", hashToken(token), 24*time.Hour) == nil
			}
		}
	}
	if created {
		h.deliver(r, email, "verify", token)
	}
	w.WriteHeader(http.StatusAccepted)
}

func (h *HTTP) deliver(r *http.Request, email, action, token string) {
	if err := h.mail(r.Context(), email, action, token); err != nil {
		h.logger.Warn("account email delivery failed", "request_id", httpserver.RequestID(r))
	}
}

func (h *HTTP) verify(w http.ResponseWriter, r *http.Request) {
	var input struct{ Token string }
	if !decode(w, r, &input) {
		return
	}
	if !h.allow(w, r, "") {
		return
	}
	if !validToken(input.Token) {
		invalid(w, r, "Invalid or expired token")
		return
	}
	err := h.store.verify(r.Context(), hashToken(input.Token), httpserver.RequestID(r))
	if errors.Is(err, pgx.ErrNoRows) {
		invalid(w, r, "Invalid or expired token")
		return
	}
	if err != nil {
		h.fail(w, r)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
