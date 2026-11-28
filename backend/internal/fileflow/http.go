package fileflow

import (
	"context"
	"encoding/base64"
	"errors"
	"io"
	"mime"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"autolife/backend/internal/platform/httpserver"
	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

func (s *Service) Routes(r chi.Router, auth, csrf func(http.Handler) http.Handler, owner func(*http.Request) string) {
	r.Route("/api/v1/files", func(r chi.Router) {
		r.Use(auth)
		r.Get("/", func(w http.ResponseWriter, r *http.Request) {
			limit := 20
			if raw := r.URL.Query().Get("limit"); raw != "" {
				n, err := strconv.Atoi(raw)
				if err != nil || n < 1 || n > 100 {
					respondError(w, r, ErrInvalid)
					return
				}
				limit = n
			}
			var cursor string
			if raw := r.URL.Query().Get("cursor"); raw != "" {
				b, err := base64.RawURLEncoding.DecodeString(raw)
				if err != nil || !validID(string(b)) {
					respondError(w, r, ErrInvalid)
					return
				}
				cursor = string(b)
			}
			files, next, err := s.Store.list(r.Context(), owner(r), cursor, limit)
			if err != nil {
				respondError(w, r, err)
				return
			}
			httpserver.WriteJSON(w, 200, map[string]any{"files": files, "next_cursor": next})
		})
		r.With(csrf).Post("/", func(w http.ResponseWriter, r *http.Request) { s.uploadHTTP(w, r, owner(r)) })
		r.Get("/{id}", func(w http.ResponseWriter, r *http.Request) {
			id := chi.URLParam(r, "id")
			if !validID(id) {
				respondError(w, r, ErrNotFound)
				return
			}
			f, err := s.Store.GetReady(r.Context(), owner(r), id)
			if err != nil {
				respondError(w, r, err)
				return
			}
			httpserver.WriteJSON(w, 200, f)
		})
		r.Get("/{id}/preview", func(w http.ResponseWriter, r *http.Request) {
			data, err := s.preview(r.Context(), owner(r), chi.URLParam(r, "id"))
			if err != nil {
				respondError(w, r, err)
				return
			}
			w.Header().Set("Cache-Control", "private, no-store")
			w.Header().Set("X-Content-Type-Options", "nosniff")
			w.Header().Set("Cross-Origin-Resource-Policy", "same-origin")
			w.Header().Set("Content-Type", "image/jpeg")
			_, _ = w.Write(data)
		})
		r.With(csrf).Delete("/{id}", func(w http.ResponseWriter, r *http.Request) {
			id := chi.URLParam(r, "id")
			if !validID(id) {
				respondError(w, r, ErrNotFound)
				return
			}
			if err := s.Store.deleteReady(r.Context(), owner(r), id); err != nil {
				respondError(w, r, err)
				return
			}
			w.WriteHeader(204)
		})
		r.With(csrf).Post("/{id}/download", func(w http.ResponseWriter, r *http.Request) {
			id := chi.URLParam(r, "id")
			if !validID(id) {
				respondError(w, r, ErrNotFound)
				return
			}
			url, err := s.download(r.Context(), owner(r), id)
			if err != nil {
				respondError(w, r, err)
				return
			}
			w.Header().Set("Cache-Control", "no-store")
			httpserver.WriteJSON(w, 200, map[string]string{"url": url})
		})
	})
}

func validID(id string) bool {
	if len(id) != 36 || id[8] != '-' || id[13] != '-' || id[18] != '-' || id[23] != '-' {
		return false
	}
	var uuid pgtype.UUID
	return uuid.Scan(id) == nil && uuid.Valid
}

func (s *Service) uploadHTTP(w http.ResponseWriter, r *http.Request, owner string) {
	if s.Inspector == nil {
		respondError(w, r, ErrUnavailable)
		return
	}
	if !s.Acquire() {
		respondError(w, r, ErrQuota)
		return
	}
	defer s.Release()
	r.Body = http.MaxBytesReader(w, r.Body, MaxFileBytes+(64<<10))
	reader, err := r.MultipartReader()
	if err != nil {
		respondError(w, r, ErrInvalid)
		return
	}
	part, err := reader.NextRawPart()
	if err != nil {
		respondError(w, r, inputError(err))
		return
	}
	_, params, err := mime.ParseMediaType(part.Header.Get("Content-Disposition"))
	name := params["filename"]
	if err != nil || part.FormName() != "file" || !safeName(name) || part.Header.Get("Content-Transfer-Encoding") != "" {
		respondError(w, r, ErrInvalid)
		return
	}
	declared := part.Header.Get("Content-Type")
	if declared != "" {
		declared, _, err = mime.ParseMediaType(declared)
		if err != nil {
			respondError(w, r, ErrType)
			return
		}
	}
	finish := func() error {
		if _, err := reader.NextRawPart(); err != io.EOF {
			return inputError(err)
		}
		return nil
	}
	file, err := s.Upload(r.Context(), owner, name, declared, part, finish)
	if err != nil {
		respondError(w, r, err)
		return
	}
	httpserver.WriteJSON(w, 201, file)
}

func inputError(err error) error {
	var limit *http.MaxBytesError
	if errors.As(err, &limit) {
		return ErrSize
	}
	return ErrInvalid
}

func safeName(name string) bool {
	return name != "" && name != "." && name != ".." && name == strings.TrimSpace(name) && utf8.ValidString(name) &&
		utf8.RuneCountInString(name) <= 255 && !strings.ContainsAny(name, "/\\") &&
		strings.IndexFunc(name, func(r rune) bool { return unicode.IsControl(r) || unicode.Is(unicode.Cf, r) }) < 0
}

func respondError(w http.ResponseWriter, r *http.Request, err error) {
	status, code, message := 503, "FILE_UNAVAILABLE", "File service unavailable"
	switch {
	case errors.Is(err, ErrNotFound):
		status, code, message = 404, "NOT_FOUND", "File not found"
	case errors.Is(err, ErrInvalid):
		status, code, message = 422, "INVALID_INPUT", "Invalid file or request"
	case errors.Is(err, ErrType):
		status, code, message = 415, "UNSUPPORTED_FORMAT", "Unsupported or mismatched file type"
	case errors.Is(err, ErrSize):
		status, code, message = 413, "FILE_SIZE_LIMIT", "File size must be between 1 and 100000000 bytes"
	case errors.Is(err, ErrQuota):
		status, code, message = 429, "QUOTA_EXCEEDED", "File quota exceeded"
	case errors.Is(err, ErrPinned):
		status, code, message = 409, "FILE_IN_USE", "File is in use"
	}
	httpserver.WriteError(w, r, status, code, message)
}

func (s *Store) list(ctx context.Context, owner, cursor string, limit int) ([]File, string, error) {
	files := make([]File, 0, limit+1)
	err := s.asOwner(ctx, owner, func(tx pgx.Tx) error {
		var when time.Time
		var from any
		if cursor != "" {
			if err := tx.QueryRow(ctx, "SELECT created_at FROM platform.files WHERE owner_id=$1 AND id=$2 AND state='ready' AND expires_at>now()", owner, cursor).Scan(&when); err != nil {
				if errors.Is(err, pgx.ErrNoRows) {
					return ErrInvalid
				}
				return err
			}
			from = when
		}
		rows, err := tx.Query(ctx, "SELECT "+fileFields+` FROM platform.files WHERE owner_id=$1 AND state='ready' AND expires_at>now()
   AND ($2::timestamptz IS NULL OR (created_at,id)<($2::timestamptz,nullif($3,'')::uuid)) ORDER BY created_at DESC,id DESC LIMIT $4`, owner, from, cursor, limit+1)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var f File
			if err := readFile(rows, &f); err != nil {
				return err
			}
			files = append(files, f)
		}
		return rows.Err()
	})
	next := ""
	if len(files) > limit {
		files = files[:limit]
		next = base64.RawURLEncoding.EncodeToString([]byte(files[limit-1].ID))
	}
	return files, next, err
}

func (s *Store) deleteReady(ctx context.Context, owner, id string) error {
	return s.asOwner(ctx, owner, func(tx pgx.Tx) error {
		var f File
		if err := readFile(tx.QueryRow(ctx, "SELECT "+fileFields+" FROM platform.files WHERE owner_id=$1 AND id=$2 AND state='ready' AND expires_at>now() FOR UPDATE", owner, id), &f); err != nil {
			return err
		}
		var pinned bool
		if err := tx.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM platform.file_pins WHERE owner_id=$1 AND file_id=$2 AND expires_at>clock_timestamp())", owner, id).Scan(&pinned); err != nil {
			return err
		}
		if pinned {
			return ErrPinned
		}
		_, err := tx.Exec(ctx, "UPDATE platform.files SET state='deleting' WHERE owner_id=$1 AND id=$2", owner, id)
		return err
	})
}

func (s *Service) download(ctx context.Context, owner, id string) (string, error) {
	var link string
	err := s.Store.asOwner(ctx, owner, func(tx pgx.Tx) error {
		if err := lockActiveOwner(ctx, tx, owner); err != nil {
			return err
		}
		var f File
		if err := readFile(tx.QueryRow(ctx, "SELECT "+fileFields+" FROM platform.files WHERE owner_id=$1 AND id=$2 AND state='ready' AND expires_at>now() FOR UPDATE", owner, id), &f); err != nil {
			return err
		}
		var err error
		link, err = s.Objects.PresignDownload(ctx, f.ObjectKey, f.DisplayName, *f.ExpiresAt)
		if err != nil {
			return err
		}
		var event string
		if err := tx.QueryRow(ctx, "SELECT gen_random_uuid()::text").Scan(&event); err != nil {
			return err
		}
		return fileEvent(ctx, tx, "download_link_issued", event, map[string]any{"owner_id": owner, "file_id": id})
	})
	return link, err
}
