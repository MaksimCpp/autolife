package accounts

import (
	"errors"
	"net/http"
	"time"

	"autolife/backend/internal/platform/httpserver"
	"github.com/jackc/pgx/v5"
)

func (h *HTTP) forgot(w http.ResponseWriter, r *http.Request) {
	var input struct{ Email string }
	if !decode(w, r, &input) {
		return
	}
	email, err := normalizeEmail(input.Email)
	if err != nil {
		invalid(w, r, "Enter a valid email")
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
	if user.Verified {
		token, err := newToken()
		if err != nil {
			h.fail(w, r)
			return
		}
		if err := h.store.issueToken(ctx, user, "reset", hashToken(token), 30*time.Minute); err == nil {
			h.deliver(r, email, "reset", token)
		} else if !errors.Is(err, pgx.ErrNoRows) {
			h.fail(w, r)
			return
		}
	}
	w.WriteHeader(http.StatusAccepted)
}

func (h *HTTP) reset(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Token       string
		NewPassword string `json:"new_password"`
	}
	if !decode(w, r, &input) {
		return
	}
	if !h.allow(w, r, "") {
		return
	}
	if !validPassword(input.NewPassword) || !validToken(input.Token) {
		invalid(w, r, "Enter a valid token and a password of at least 12 characters")
		return
	}
	hash, err := hashPassword(r.Context(), input.NewPassword)
	if err == nil {
		err = h.store.resetPassword(r.Context(), hashToken(input.Token), hash, httpserver.RequestID(r))
	}
	if errors.Is(err, pgx.ErrNoRows) {
		invalid(w, r, "Invalid or expired token")
		return
	}
	if err != nil {
		h.fail(w, r)
		return
	}
	h.cookie(w, "session", "", 0)
	w.WriteHeader(http.StatusNoContent)
}

func (h *HTTP) confirmPassword(w http.ResponseWriter, r *http.Request, current session, password string) (string, bool) {
	if !h.allow(w, r, current.OwnerID) {
		return "", false
	}
	if !validPassword(password) {
		unauthenticated(w, r)
		return "", false
	}
	hash, err := h.store.sessionPassword(r.Context(), current)
	if errors.Is(err, pgx.ErrNoRows) {
		unauthenticated(w, r)
		return "", false
	}
	if err != nil {
		h.fail(w, r)
		return "", false
	}
	matches, err := passwordMatches(r.Context(), password, hash)
	if err != nil {
		h.fail(w, r)
		return "", false
	}
	if !matches {
		unauthenticated(w, r)
		return "", false
	}
	return hash, true
}

func (h *HTTP) change(w http.ResponseWriter, r *http.Request) {
	var input struct {
		CurrentPassword string `json:"current_password"`
		NewPassword     string `json:"new_password"`
	}
	if !decode(w, r, &input) {
		return
	}
	if !validPassword(input.NewPassword) || input.NewPassword == input.CurrentPassword {
		invalid(w, r, "Enter a different password of at least 12 characters")
		return
	}
	current := currentSession(r)
	expected, ok := h.confirmPassword(w, r, current, input.CurrentPassword)
	if !ok {
		return
	}
	hash, err := hashPassword(r.Context(), input.NewPassword)
	if err != nil {
		h.fail(w, r)
		return
	}
	token, err := newToken()
	if err == nil {
		err = h.store.changePassword(r.Context(), current, expected, hash, hashToken(token), httpserver.RequestID(r))
	}
	if errors.Is(err, pgx.ErrNoRows) {
		unauthenticated(w, r)
		return
	}
	if err != nil {
		h.fail(w, r)
		return
	}
	h.cookie(w, "session", token, sessionTTL)
	w.WriteHeader(http.StatusNoContent)
}
