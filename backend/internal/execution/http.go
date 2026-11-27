package execution

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"autolife/backend/internal/platform/httpserver"
	"autolife/backend/internal/workflow"
	"github.com/go-chi/chi/v5"
)

func (s *Store) Routes(r chi.Router, auth, csrf func(http.Handler) http.Handler, owner func(*http.Request) string) {
	for _, test := range []bool{false, true} {
		path := "/api/v1/workflows/{id}/executions"
		if test {
			path = "/api/v1/workflows/{id}/test-executions"
		}
		r.With(auth, csrf).Post(path, func(w http.ResponseWriter, r *http.Request) {
			var req Start
			key, ok := decodeStart(w, r, &req)
			if !ok {
				return
			}
			if test && (req.ExpectedRevision < 1 || req.VersionID != "") || !test && req.ExpectedRevision != 0 {
				httpserver.WriteError(w, r, 400, "INVALID_REQUEST", "Invalid execution definition selector")
				return
			}
			e, err := s.Start(r.Context(), owner(r), chi.URLParam(r, "id"), key, req, test)
			accepted(w, r, e, err)
		})
	}
	r.With(auth, csrf).Post("/api/v1/executions/{id}/retry", func(w http.ResponseWriter, r *http.Request) {
		var req Start
		key, ok := decodeStart(w, r, &req)
		if !ok {
			return
		}
		if req.VersionID != "" || req.ExpectedRevision != 0 {
			httpserver.WriteError(w, r, 400, "INVALID_REQUEST", "Retry uses the historical definition")
			return
		}
		e, err := s.Retry(r.Context(), owner(r), chi.URLParam(r, "id"), key, req)
		accepted(w, r, e, err)
	})
	r.With(auth, csrf).Post("/api/v1/executions/{id}/cancel", func(w http.ResponseWriter, r *http.Request) {
		e, err := s.Cancel(r.Context(), owner(r), chi.URLParam(r, "id"))
		if err != nil {
			writeError(w, r, err)
			return
		}
		code := 202
		if e.Status.Terminal() {
			code = 200
		}
		httpserver.WriteJSON(w, code, map[string]any{"id": e.ID, "status": e.Status})
	})
	r.With(auth).Get("/api/v1/executions/{id}", func(w http.ResponseWriter, r *http.Request) {
		e, err := s.Details(r.Context(), owner(r), chi.URLParam(r, "id"))
		if err != nil {
			writeError(w, r, err)
			return
		}
		httpserver.WriteJSON(w, 200, e)
	})
	r.With(auth).Get("/api/v1/executions", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		limit := 20
		if q.Get("limit") != "" {
			var err error
			limit, err = strconv.Atoi(q.Get("limit"))
			if err != nil || limit < 1 || limit > 100 {
				httpserver.WriteError(w, r, 400, "INVALID_LIMIT", "Invalid page size")
				return
			}
		}
		wid, status := q.Get("workflow_id"), State(q.Get("status"))
		if wid != "" && !workflow.ValidID(wid) || status != "" && !status.Terminal() && status != Queued && status != Running && status != Retrying && status != CancelRequested {
			httpserver.WriteError(w, r, 400, "INVALID_FILTER", "Invalid execution filters")
			return
		}
		type cursor struct {
			Scope, ID string
			At        time.Time
		}
		scope := owner(r) + ":" + wid + ":" + string(status)
		var c cursor
		var at *time.Time
		if raw := q.Get("cursor"); raw != "" {
			if len(raw) > 512 {
				httpserver.WriteError(w, r, 400, "INVALID_CURSOR", "Invalid cursor")
				return
			}
			data, err := base64.RawURLEncoding.DecodeString(raw)
			if err != nil || json.Unmarshal(data, &c) != nil || c.Scope != scope || !workflow.ValidID(c.ID) || c.At.IsZero() {
				httpserver.WriteError(w, r, 400, "INVALID_CURSOR", "Invalid cursor")
				return
			}
			at = &c.At
		}
		rows, err := s.List(r.Context(), owner(r), wid, string(status), c.ID, at, limit+1)
		if err != nil {
			writeError(w, r, err)
			return
		}
		next := ""
		if len(rows) > limit {
			rows = rows[:limit]
			last := rows[limit-1]
			data, _ := json.Marshal(cursor{scope, last.ID, last.QueuedAt})
			next = base64.RawURLEncoding.EncodeToString(data)
		}
		httpserver.WriteJSON(w, 200, map[string]any{"executions": rows, "next_cursor": next})
	})
	r.With(auth).Get("/api/v1/usage", func(w http.ResponseWriter, r *http.Request) {
		u, err := s.Usage(r.Context(), owner(r))
		if err != nil {
			writeError(w, r, err)
			return
		}
		httpserver.WriteJSON(w, 200, u)
	})
}
func decodeStart(w http.ResponseWriter, r *http.Request, input *Start) (string, bool) {
	key := r.Header.Get("Idempotency-Key")
	if len(key) == 0 || len(key) > 128 || strings.IndexFunc(key, func(ch rune) bool { return ch < 33 || ch > 126 }) != -1 {
		httpserver.WriteError(w, r, 400, "IDEMPOTENCY_KEY_REQUIRED", "Use a valid Idempotency-Key")
		return "", false
	}
	d := json.NewDecoder(http.MaxBytesReader(w, r.Body, 256<<10))
	d.DisallowUnknownFields()
	err := d.Decode(input)
	if err == io.EOF || err == nil && d.Decode(new(any)) == io.EOF {
		return key, true
	}
	httpserver.WriteError(w, r, 400, "INVALID_REQUEST", "Invalid execution request")
	return "", false
}
func accepted(w http.ResponseWriter, r *http.Request, e Execution, err error) {
	if err != nil {
		writeError(w, r, err)
		return
	}
	httpserver.WriteJSON(w, 202, map[string]string{"id": e.ID})
}
func writeError(w http.ResponseWriter, r *http.Request, err error) {
	status, code, message := 503, "EXECUTION_UNAVAILABLE", "Execution service unavailable"
	var invalid *workflow.Invalid
	switch {
	case errors.As(err, &invalid):
		httpserver.WriteJSON(w, 422, map[string]any{"error": map[string]any{"code": "VALIDATION_ERROR", "message": "Execution input is invalid", "fields": invalid.Fields, "request_id": httpserver.RequestID(r)}})
		return
	case errors.Is(err, workflow.ErrNotFound):
		status, code, message = 404, "NOT_FOUND", "Resource not found"
	case errors.Is(err, workflow.ErrArchived):
		status, code, message = 409, "WORKFLOW_ARCHIVED", "Workflow is archived"
	case errors.Is(err, workflow.ErrConflict):
		status, code, message = 409, "REVISION_CONFLICT", "Draft revision changed"
	case errors.Is(err, ErrIdempotency):
		status, code, message = 409, "IDEMPOTENCY_CONFLICT", "Key used for another request"
	case errors.Is(err, ErrState):
		status, code, message = 409, "EXECUTION_STATE_CONFLICT", "Execution cannot be retried"
	case errors.Is(err, ErrQuota):
		status, code, message = 429, "QUOTA_EXCEEDED", "Execution limit reached"
	case errors.Is(err, ErrInputs):
		status, code, message = 422, "INPUT_UNAVAILABLE", "Inputs are unavailable or exceed limits"
	}
	httpserver.WriteError(w, r, status, code, message)
}
