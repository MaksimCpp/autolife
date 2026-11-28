package fileflow

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"image"
	_ "image/jpeg"
	"io"
	"time"

	"autolife/backend/internal/sandbox"
	"github.com/jackc/pgx/v5/pgtype"
)

type Previewer interface {
	Transform(context.Context, string, []byte, io.Reader, int64) (io.ReadCloser, int64, error)
}

const MaxPreviewBytes = 1 << 20
const previewEdge = 512

func (s *Service) preview(ctx context.Context, owner, id string) ([]byte, error) {
	if !validID(id) {
		return nil, ErrNotFound
	}
	ctx, cancel := context.WithTimeout(ctx, 25*time.Second)
	defer cancel()
	file, err := s.Store.GetReady(ctx, owner, id)
	if err != nil {
		return nil, err
	}
	if file.MIME == nil || file.Bytes == nil {
		return nil, ErrNotFound
	}
	switch *file.MIME {
	case "image/jpeg", "image/png", "image/webp":
	default:
		return nil, ErrType
	}
	objects, ok := s.Objects.(interface {
		Open(context.Context, string) (io.ReadCloser, int64, error)
	})
	if s.Previewer == nil || !ok {
		return nil, ErrUnavailable
	}
	if !s.Acquire() {
		return nil, ErrQuota
	}
	defer s.Release()
	in, size, err := objects.Open(ctx, file.ObjectKey)
	if err != nil {
		return nil, ErrUnavailable
	}
	defer in.Close()
	if size != *file.Bytes {
		return nil, ErrInvalid
	}
	idBytes := [16]byte{}
	if _, err := rand.Read(idBytes[:]); err != nil {
		return nil, ErrUnavailable
	}
	idBytes[6] = (idBytes[6] & 0x0f) | 0x40
	idBytes[8] = (idBytes[8] & 0x3f) | 0x80
	job := (pgtype.UUID{Bytes: idBytes, Valid: true}).String()
	header := make([]byte, sandbox.ImageHeaderBytes)
	copy(header, "ALI1")
	header[4], header[5], header[6] = 1, 80, 8|2
	header[7], header[8], header[9] = 255, 255, 255
	binary.BigEndian.PutUint32(header[10:], previewEdge)
	binary.BigEndian.PutUint32(header[14:], previewEdge)
	out, length, err := s.Previewer.Transform(ctx, job, header, in, size)
	if err != nil {
		return nil, ErrUnavailable
	}
	defer out.Close()
	if length < 1 || length > MaxPreviewBytes {
		return nil, ErrInvalid
	}
	data, err := io.ReadAll(io.LimitReader(out, length+1))
	if err != nil || int64(len(data)) != length {
		return nil, ErrInvalid
	}
	cfg, format, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil || format != "jpeg" || cfg.Width < 1 || cfg.Height < 1 || cfg.Width > previewEdge || cfg.Height > previewEdge {
		return nil, ErrInvalid
	}
	// Recheck expiry/deletion after rendering, before releasing the derivative.
	if _, err := s.Store.GetReady(ctx, owner, id); err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil, ErrNotFound
		}
		return nil, ErrUnavailable
	}
	return data, nil
}
