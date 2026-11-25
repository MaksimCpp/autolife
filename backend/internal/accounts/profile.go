package accounts

import (
	"context"
	"encoding/base64"
	"errors"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
	_ "time/tzdata"

	"autolife/backend/internal/platform/httpserver"
	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

type profile struct {
	ID                        string     `json:"id"`
	Email                     string     `json:"email"`
	EmailVerifiedAt           *time.Time `json:"email_verified_at"`
	Locale                    string     `json:"locale"`
	Timezone                  string     `json:"timezone"`
	Theme                     string     `json:"theme"`
	Notifications             bool       `json:"notifications"`
	AnalyticsConsent          bool       `json:"analytics_consent"`
	AnalyticsConsentUpdatedAt *time.Time `json:"analytics_consent_updated_at"`
}

type profilePatch struct {
	Locale           *string `json:"locale"`
	Timezone         *string `json:"timezone"`
	Theme            *string `json:"theme"`
	Notifications    *bool   `json:"notifications"`
	AnalyticsConsent *bool   `json:"analytics_consent"`
}

var timezoneName = regexp.MustCompile(`^[A-Za-z0-9_+-]+(/[A-Za-z0-9_+-]+)*$`)

func (p profilePatch) valid() bool {
	if p.Locale != nil && *p.Locale != "ru" && *p.Locale != "en" {
		return false
	}
	if p.Theme != nil && *p.Theme != "light" && *p.Theme != "dark" && *p.Theme != "system" {
		return false
	}
	if p.Timezone != nil {
		if len(*p.Timezone) > 128 || *p.Timezone == "Local" || !timezoneName.MatchString(*p.Timezone) {
			return false
		}
		if _, err := time.LoadLocation(*p.Timezone); err != nil {
			return false
		}
	}
	return p.Locale != nil || p.Timezone != nil || p.Theme != nil || p.Notifications != nil || p.AnalyticsConsent != nil
}

func readProfile(ctx context.Context, db interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}, owner string) (profile, error) {
	var p profile
	err := db.QueryRow(ctx, `SELECT u.id::text,u.normalized_email,u.email_verified_at,s.locale,s.timezone,
		s.theme,s.notifications,s.analytics_consent,s.analytics_consent_updated_at
		FROM platform.users u JOIN platform.user_settings s ON s.user_id=u.id WHERE u.id=$1 AND u.status='active'`, owner).
		Scan(&p.ID, &p.Email, &p.EmailVerifiedAt, &p.Locale, &p.Timezone, &p.Theme, &p.Notifications,
			&p.AnalyticsConsent, &p.AnalyticsConsentUpdatedAt)
	return p, err
}

func (s *Store) patchProfile(ctx context.Context, current session, patch profilePatch) (profile, error) {
	var p profile
	err := s.transaction(ctx, func(tx pgx.Tx) error {
		if _, err := lockSessionOwner(ctx, tx, current); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `UPDATE platform.user_settings SET locale=coalesce($2,locale),timezone=coalesce($3,timezone),
			theme=coalesce($4,theme),notifications=coalesce($5,notifications),
			analytics_consent_updated_at=CASE WHEN $6::boolean IS NOT NULL AND $6 IS DISTINCT FROM analytics_consent
				THEN now() ELSE analytics_consent_updated_at END,
			analytics_consent=coalesce($6,analytics_consent)
			WHERE user_id=$1`, current.OwnerID, patch.Locale, patch.Timezone, patch.Theme, patch.Notifications, patch.AnalyticsConsent)
		if err != nil {
			return err
		}
		p, err = readProfile(ctx, tx, current.OwnerID)
		return err
	})
	return p, err
}

type listedSession struct {
	ID        string    `json:"id"`
	CreatedAt time.Time `json:"created_at"`
	ExpiresAt time.Time `json:"expires_at"`
	Current   bool      `json:"current"`
}

func validID(value string) bool {
	var id pgtype.UUID
	return id.Scan(value) == nil && id.Valid
}

func (h *HTTP) getProfile(w http.ResponseWriter, r *http.Request) {
	p, err := readProfile(r.Context(), h.store.db, currentSession(r).OwnerID)
	h.profileResponse(w, r, p, err)
}

func (h *HTTP) patchProfile(w http.ResponseWriter, r *http.Request) {
	var patch profilePatch
	if !decode(w, r, &patch) {
		return
	}
	if !patch.valid() {
		invalid(w, r, "Provide valid account settings")
		return
	}
	p, err := h.store.patchProfile(r.Context(), currentSession(r), patch)
	h.profileResponse(w, r, p, err)
}

func (h *HTTP) profileResponse(w http.ResponseWriter, r *http.Request, p profile, err error) {
	if errors.Is(err, pgx.ErrNoRows) {
		unauthenticated(w, r)
	} else if err != nil {
		h.fail(w, r)
	} else {
		httpserver.WriteJSON(w, http.StatusOK, p)
	}
}

func (h *HTTP) listSessions(w http.ResponseWriter, r *http.Request) {
	current := currentSession(r)
	limit := 20
	if raw := r.URL.Query().Get("limit"); raw != "" {
		var err error
		limit, err = strconv.Atoi(raw)
		if err != nil || limit < 1 || limit > 100 {
			invalid(w, r, "Invalid limit")
			return
		}
	}
	var afterAt *time.Time
	var afterID *string
	if raw := r.URL.Query().Get("cursor"); raw != "" {
		decoded, err := base64.RawURLEncoding.DecodeString(raw)
		parts := strings.Split(string(decoded), "|")
		if err != nil || len(parts) != 3 || parts[0] != current.OwnerID || !validID(parts[2]) {
			invalid(w, r, "Invalid cursor")
			return
		}
		date, err := time.Parse(time.RFC3339Nano, parts[1])
		if err != nil {
			invalid(w, r, "Invalid cursor")
			return
		}
		afterAt, afterID = &date, &parts[2]
	}
	rows, err := h.store.db.Query(r.Context(), `SELECT id::text,created_at,expires_at,id=$2
		FROM platform.sessions WHERE user_id=$1 AND revoked_at IS NULL AND expires_at>now()
		AND ($3::timestamptz IS NULL OR (created_at,id)<($3,$4::uuid))
		ORDER BY created_at DESC,id DESC LIMIT $5`, current.OwnerID, current.ID, afterAt, afterID, limit+1)
	if err != nil {
		h.fail(w, r)
		return
	}
	items, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (listedSession, error) {
		var item listedSession
		err := row.Scan(&item.ID, &item.CreatedAt, &item.ExpiresAt, &item.Current)
		return item, err
	})
	if err != nil {
		h.fail(w, r)
		return
	}
	var cursor string
	if len(items) > limit {
		items = items[:limit]
		last := items[limit-1]
		cursor = base64.RawURLEncoding.EncodeToString([]byte(current.OwnerID + "|" + last.CreatedAt.Format(time.RFC3339Nano) + "|" + last.ID))
	}
	if items == nil {
		items = []listedSession{}
	}
	httpserver.WriteJSON(w, http.StatusOK, struct {
		Items []listedSession `json:"items"`
		Next  string          `json:"next_cursor,omitempty"`
	}{Items: items, Next: cursor})
}

func (h *HTTP) revokeSession(w http.ResponseWriter, r *http.Request) {
	current := currentSession(r)
	id := chi.URLParam(r, "id")
	if !validID(id) {
		notFound(w, r)
		return
	}
	err := h.store.revoke(r.Context(), current, id, httpserver.RequestID(r))
	if errors.Is(err, pgx.ErrNoRows) {
		h.denied(r, current.OwnerID, "session.revoke", id)
		notFound(w, r)
		return
	}
	if err != nil {
		h.fail(w, r)
		return
	}
	if id == current.ID {
		h.cookie(w, "session", "", 0)
	}
	w.WriteHeader(http.StatusNoContent)
}

func notFound(w http.ResponseWriter, r *http.Request) {
	httpserver.WriteError(w, r, http.StatusNotFound, "NOT_FOUND", "Resource not found")
}
