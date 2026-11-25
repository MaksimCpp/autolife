package accounts

import (
	"crypto/hmac"
	"crypto/sha256"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"

	"autolife/backend/internal/platform/httpserver"
	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
)

func (h *HTTP) privacyCredentials(w http.ResponseWriter, r *http.Request, current session, password string) (string, bool) {
	if password != "" {
		return h.confirmPassword(w, r, current, password)
	}
	if time.Since(current.AuthenticatedAt) > 10*time.Minute {
		httpserver.WriteError(w, r, http.StatusUnauthorized, "RECENT_AUTH_REQUIRED", "Enter your current password")
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
	return hash, true
}

func validKey(key string) bool {
	if len(key) == 0 || len(key) > 128 {
		return false
	}
	for _, char := range key {
		if char < 33 || char > 126 {
			return false
		}
	}
	return true
}

func (h *HTTP) requestExport(w http.ResponseWriter, r *http.Request) {
	key := r.Header.Get("Idempotency-Key")
	if !validKey(key) {
		invalid(w, r, "Provide a valid Idempotency-Key")
		return
	}
	var input struct {
		CurrentPassword string `json:"current_password"`
	}
	if (r.ContentLength != 0 || len(r.TransferEncoding) != 0) && !decode(w, r, &input) {
		return
	}
	current := currentSession(r)
	hash, ok := h.privacyCredentials(w, r, current, input.CurrentPassword)
	if !ok {
		return
	}
	job, err := h.store.requestExport(r.Context(), current, hash, key, httpserver.RequestID(r))
	if errors.Is(err, pgx.ErrNoRows) {
		unauthenticated(w, r)
	} else if err != nil {
		h.fail(w, r)
	} else {
		w.Header().Set("Location", "/api/v1/me/exports/"+job.ID)
		httpserver.WriteJSON(w, http.StatusAccepted, map[string]string{"id": job.ID})
	}
}

func (h *HTTP) findExport(w http.ResponseWriter, r *http.Request) (privacyJob, bool) {
	id := chi.URLParam(r, "id")
	if !validID(id) {
		notFound(w, r)
		return privacyJob{}, false
	}
	job, err := h.store.exportStatus(r.Context(), currentSession(r).OwnerID, id)
	if errors.Is(err, pgx.ErrNoRows) {
		h.denied(r, currentSession(r).OwnerID, "privacy.export.access", id)
		notFound(w, r)
		return privacyJob{}, false
	}
	if err != nil {
		h.fail(w, r)
		return privacyJob{}, false
	}
	return job, true
}

func (h *HTTP) exportStatus(w http.ResponseWriter, r *http.Request) {
	if job, ok := h.findExport(w, r); ok {
		httpserver.WriteJSON(w, http.StatusOK, job)
	}
}

func exportTicketScope(current session, exportID string) string {
	return "export:" + current.ID + ":" + exportID
}

func (h *HTTP) exportDownload(w http.ResponseWriter, r *http.Request) {
	job, ok := h.findExport(w, r)
	if !ok {
		return
	}
	if job.State != "ready" || job.ExpiresAt == nil {
		httpserver.WriteError(w, r, http.StatusConflict, "EXPORT_NOT_READY", "Export is unavailable")
		return
	}
	token, err := newToken()
	if err != nil {
		h.fail(w, r)
		return
	}
	expires := time.Now().Add(5 * time.Minute)
	if job.ExpiresAt.Before(expires) {
		expires = *job.ExpiresAt
	}
	current := currentSession(r)
	err = h.store.recordExportLink(r.Context(), current, job.ID)
	if errors.Is(err, pgx.ErrNoRows) {
		notFound(w, r)
		return
	}
	if err != nil {
		h.fail(w, r)
		return
	}
	ticket := h.sign(exportTicketScope(current, job.ID), token, expires)
	httpserver.WriteJSON(w, http.StatusOK, map[string]string{
		"url": "/api/v1/me/exports/" + job.ID + "/content?ticket=" + url.QueryEscape(ticket),
	})
}

func (h *HTTP) exportContent(w http.ResponseWriter, r *http.Request) {
	current := currentSession(r)
	id := chi.URLParam(r, "id")
	if !validID(id) || !h.validContext(exportTicketScope(current, id), r.URL.Query().Get("ticket")) {
		notFound(w, r)
		return
	}
	data, err := h.store.exportData(r.Context(), current.OwnerID, id)
	if errors.Is(err, pgx.ErrNoRows) {
		notFound(w, r)
	} else if err != nil {
		h.fail(w, r)
	} else {
		w.Header().Set("Content-Disposition", `attachment; filename="autolife-export.json"`)
		httpserver.WriteJSON(w, http.StatusOK, data)
	}
}

func (h *HTTP) deleteAccount(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Confirm         bool
		CurrentPassword string `json:"current_password"`
	}
	if !decode(w, r, &input) {
		return
	}
	if !input.Confirm {
		invalid(w, r, "Confirm account deletion")
		return
	}
	cookie, err := r.Cookie(h.cookieName("session"))
	if err != nil || !validToken(cookie.Value) {
		unauthenticated(w, r)
		return
	}
	mac := hmac.New(sha256.New, h.key)
	mac.Write([]byte("autolife-deletion-receipt:" + cookie.Value))
	receipt := mac.Sum(nil)
	ctx := r.Context()
	job, err := h.store.deleteReceipt(ctx, receipt)
	if errors.Is(err, pgx.ErrNoRows) {
		current, _, sessionErr := h.session(r)
		if errors.Is(sessionErr, pgx.ErrNoRows) {
			unauthenticated(w, r)
			return
		}
		if sessionErr != nil {
			h.fail(w, r)
			return
		}
		hash, ok := h.privacyCredentials(w, r, current, input.CurrentPassword)
		if !ok {
			return
		}
		job, err = h.store.requestDelete(ctx, current, hash, receipt, httpserver.RequestID(r))
		if errors.Is(err, pgx.ErrNoRows) {
			job, err = h.store.deleteReceipt(ctx, receipt)
		}
	}
	if errors.Is(err, pgx.ErrNoRows) {
		unauthenticated(w, r)
		return
	}
	if err != nil || strings.TrimSpace(job.ID) == "" {
		h.fail(w, r)
		return
	}
	if err := h.journal.append(job.ID, job.Owner, job.RequestedAt); err != nil {
		h.fail(w, r)
		return
	}
	httpserver.WriteJSON(w, http.StatusAccepted, map[string]string{"id": job.ID})
}
