package workflowhttp

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"autolife/backend/internal/platform/httpserver"
	"autolife/backend/internal/workflow"
	"autolife/backend/internal/workflow/postgres"
	"github.com/go-chi/chi/v5"
)

type API struct {
	Store    *postgres.Store
	Registry *workflow.Registry
}

type request struct {
	w     http.ResponseWriter
	r     *http.Request
	owner string
	id    string
}

type data struct {
	Name        *string `json:"name"`
	Description *string `json:"description"`
}

func (a API) Routes(r chi.Router, auth, csrf func(http.Handler) http.Handler, owner func(*http.Request) string) {
	bind := func(handle func(request)) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
			defer cancel()
			handle(request{w, r.WithContext(ctx), owner(r), chi.URLParam(r, "id")})
		}
	}
	r.With(auth).Get("/api/v1/actions", bind(func(q request) { q.write(200, map[string]any{"actions": a.Registry.Actions()}) }))
	r.With(auth).Get("/api/v1/triggers", bind(func(q request) { q.write(200, map[string]any{"triggers": a.Registry.Triggers()}) }))
	r.Route("/api/v1/workflows", func(r chi.Router) {
		r.Use(auth)
		r.Get("/", bind(a.list))
		r.With(csrf).Post("/", bind(a.create))
		r.Get("/{id}", bind(a.get))
		r.With(csrf).Patch("/{id}", bind(a.patch))
		r.With(csrf).Delete("/{id}", bind(a.archive))
		r.With(csrf).Post("/{id}/copy", bind(a.copy))
		r.With(csrf).Put("/{id}/draft", bind(a.saveDraft))
		r.With(csrf).Post("/{id}/validate", bind(a.validate))
		r.With(csrf).Post("/{id}/versions", bind(a.publish))
		r.Get("/{id}/versions", bind(a.versions))
	})
}

func (q request) write(status int, value any) { httpserver.WriteJSON(q.w, status, value) }
func (q request) ctx() context.Context        { return q.r.Context() }
func (q request) result(status int, value any, err error) {
	if err != nil {
		q.err(err)
		return
	}
	q.write(status, value)
}
func (q request) err(err error) {
	status, code, message := 503, "WORKFLOW_UNAVAILABLE", "Workflow service unavailable"
	var invalid *workflow.Invalid
	switch {
	case errors.As(err, &invalid):
		q.write(422, map[string]any{"error": map[string]any{"code": "VALIDATION_ERROR", "message": "Workflow validation failed", "fields": invalid.Fields, "request_id": httpserver.RequestID(q.r)}})
		return
	case errors.Is(err, workflow.ErrNotFound):
		status, code, message = 404, "NOT_FOUND", "Workflow not found"
	case errors.Is(err, workflow.ErrConflict):
		status, code, message = 409, "REVISION_CONFLICT", "Draft revision is stale or already published"
	case errors.Is(err, workflow.ErrArchived):
		status, code, message = 409, "WORKFLOW_ARCHIVED", "Workflow is archived"
	}
	httpserver.WriteError(q.w, q.r, status, code, message)
}

func (q request) decode(out any, optional bool) bool {
	q.r.Body = http.MaxBytesReader(q.w, q.r.Body, workflow.MaxDefinitionBytes+8192)
	dec := json.NewDecoder(q.r.Body)
	var raw json.RawMessage
	err := dec.Decode(&raw)
	if err == io.EOF && optional {
		return true
	}
	if err == nil {
		err = dec.Decode(new(any))
		if err == io.EOF && len(raw) > 0 && raw[0] == '{' {
			payload := json.NewDecoder(bytes.NewReader(raw))
			payload.DisallowUnknownFields()
			if payload.Decode(out) == nil {
				return true
			}
		}
	}
	var tooLarge *http.MaxBytesError
	if errors.As(err, &tooLarge) {
		httpserver.WriteError(q.w, q.r, 413, "PAYLOAD_TOO_LARGE", "Request body exceeds the limit")
		return false
	}
	q.bad("INVALID_REQUEST", "Expected a single JSON object")
	return false
}

func (q request) bad(code, message string) { httpserver.WriteError(q.w, q.r, 400, code, message) }

func normalizeText(p *string, min, max int) bool {
	if p == nil {
		return min == 0
	}
	value := strings.TrimSpace(*p)
	for _, ch := range value {
		if ch == 0 || (unicode.IsControl(ch) && (min > 0 || ch != '\n' && ch != '\t')) {
			return false
		}
	}
	length := utf8.RuneCountInString(value)
	if length < min || length > max {
		return false
	}
	*p = value
	return true
}

func (a API) create(q request) {
	var input data
	if !q.decode(&input, false) {
		return
	}
	if !normalizeText(input.Name, 1, 120) || !normalizeText(input.Description, 0, 2000) {
		q.bad("INVALID_METADATA", "Invalid name or description")
		return
	}
	about := ""
	if input.Description != nil {
		about = *input.Description
	}
	value, err := a.Store.Create(q.ctx(), q.owner, *input.Name, about)
	q.result(201, value, err)
}

func (a API) get(q request) { v, err := a.Store.Get(q.ctx(), q.owner, q.id); q.result(200, v, err) }

func (a API) patch(q request) {
	var input data
	if !q.decode(&input, false) {
		return
	}
	if input.Name == nil && input.Description == nil || input.Name != nil && !normalizeText(input.Name, 1, 120) || !normalizeText(input.Description, 0, 2000) {
		q.bad("INVALID_METADATA", "Invalid name or description")
		return
	}
	v, err := a.Store.Update(q.ctx(), q.owner, q.id, input.Name, input.Description)
	q.result(200, v, err)
}

func (a API) archive(q request) {
	if err := a.Store.Archive(q.ctx(), q.owner, q.id); err != nil {
		q.err(err)
		return
	}
	q.w.WriteHeader(204)
}

func (a API) copy(q request) {
	var input struct {
		Name *string `json:"name"`
	}
	if !q.decode(&input, true) {
		return
	}
	if input.Name != nil && !normalizeText(input.Name, 1, 120) {
		q.bad("INVALID_METADATA", "Invalid name")
		return
	}
	v, err := a.Store.Copy(q.ctx(), q.owner, q.id, input.Name)
	q.result(201, v, err)
}

func (a API) saveDraft(q request) {
	var input struct {
		Revision   int64           `json:"expected_revision"`
		Definition json.RawMessage `json:"definition"`
	}
	if !q.decode(&input, false) {
		return
	}
	if input.Revision < 1 {
		q.bad("INVALID_REVISION", "expected_revision must be positive")
		return
	}
	v, err := a.Store.SaveDraft(q.ctx(), q.owner, q.id, input.Revision, input.Definition)
	q.result(200, v, err)
}

func (a API) validate(q request) {
	var input struct {
		Definition json.RawMessage `json:"definition"`
	}
	if !q.decode(&input, true) {
		return
	}
	if input.Definition != nil && len(input.Definition) > workflow.MaxDefinitionBytes {
		httpserver.WriteError(q.w, q.r, 413, "PAYLOAD_TOO_LARGE", "Definition exceeds the limit")
		return
	}
	fields, err := a.Store.Validate(q.ctx(), q.owner, q.id, input.Definition)
	if fields == nil {
		fields = []workflow.Problem{}
	}
	q.result(200, map[string]any{"valid": len(fields) == 0, "errors": fields, "warnings": []workflow.Problem{}}, err)
}

func (a API) publish(q request) {
	var input struct {
		Revision int64 `json:"expected_revision"`
	}
	if !q.decode(&input, false) {
		return
	}
	if input.Revision < 1 {
		q.bad("INVALID_REVISION", "expected_revision must be positive")
		return
	}
	v, err := a.Store.Publish(q.ctx(), q.owner, q.id, input.Revision)
	q.result(201, v, err)
}

type cursor struct {
	Scope  string     `json:"s"`
	At     *time.Time `json:"t,omitempty"`
	ID     string     `json:"i,omitempty"`
	Number *int64     `json:"v,omitempty"`
}

func (c cursor) encode() string {
	raw, _ := json.Marshal(c)
	return base64.RawURLEncoding.EncodeToString(raw)
}

func (q request) page(scope string) (int, cursor, bool) {
	value := q.r.URL.Query()
	limit := 20
	if text := value.Get("limit"); text != "" {
		n, err := strconv.Atoi(text)
		if err != nil || n < 1 || n > 100 {
			q.bad("INVALID_LIMIT", "limit must be between 1 and 100")
			return 0, cursor{}, false
		}
		limit = n
	}
	var c cursor
	if text := value.Get("cursor"); text != "" {
		if len(text) > 512 {
			q.bad("INVALID_CURSOR", "Invalid cursor")
			return 0, cursor{}, false
		}
		raw, err := base64.RawURLEncoding.DecodeString(text)
		if err != nil || json.Unmarshal(raw, &c) != nil || c.Scope != scope {
			q.bad("INVALID_CURSOR", "Invalid cursor")
			return 0, cursor{}, false
		}
	}
	return limit, c, true
}

func (a API) list(q request) {
	status := q.r.URL.Query().Get("status")
	switch status {
	case "", "draft", "active", "inactive", "archived":
	default:
		q.bad("INVALID_STATUS", "Invalid workflow status")
		return
	}
	scope := q.owner + ":workflows:" + status
	limit, c, ok := q.page(scope)
	if !ok {
		return
	}
	if c.Scope != "" && (c.At == nil || !workflow.ValidID(c.ID) || c.Number != nil) {
		q.bad("INVALID_CURSOR", "Invalid cursor")
		return
	}
	rows, err := a.Store.List(q.ctx(), q.owner, status, c.At, c.ID, limit+1)
	if err != nil {
		q.err(err)
		return
	}
	next := ""
	if len(rows) > limit {
		rows = rows[:limit]
		last := rows[limit-1]
		next = cursor{Scope: scope, At: &last.CreatedAt, ID: last.ID}.encode()
	}
	q.write(200, map[string]any{"workflows": rows, "next_cursor": next})
}

func (a API) versions(q request) {
	if !workflow.ValidID(q.id) {
		q.err(workflow.ErrNotFound)
		return
	}
	scope := q.owner + ":versions:" + q.id
	limit, c, ok := q.page(scope)
	if !ok {
		return
	}
	if c.Scope != "" && (c.Number == nil || *c.Number < 1 || c.At != nil || c.ID != "") {
		q.bad("INVALID_CURSOR", "Invalid cursor")
		return
	}
	rows, err := a.Store.Versions(q.ctx(), q.owner, q.id, c.Number, limit+1)
	if err != nil {
		q.err(err)
		return
	}
	next := ""
	if len(rows) > limit {
		rows = rows[:limit]
		n := rows[limit-1].Number
		next = cursor{Scope: scope, Number: &n}.encode()
	}
	q.write(200, map[string]any{"versions": rows, "next_cursor": next})
}
