package fileflow

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"
)

func TestS3PrivateObject(t *testing.T) {
	ctx := t.Context()
	endpoint := os.Getenv("AUTOLIFE_TEST_S3_ENDPOINT")
	if endpoint == "" {
		t.Skip("requires a private test S3 bucket")
	}
	storage, err := NewS3(S3Config{
		Endpoint: endpoint, Bucket: "autolife", Region: "us-east-1", AllowHTTP: true,
		AccessKey: os.Getenv("AUTOLIFE_S3_ACCESS_KEY"), SecretKey: os.Getenv("AUTOLIFE_S3_SECRET_KEY"),
	})
	if err != nil {
		t.Fatal(err)
	}
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		t.Fatal(err)
	}
	key := "smoke/" + hex.EncodeToString(id[:])
	data := []byte("AutoLife private object fixture")
	client := &http.Client{Timeout: 5 * time.Second}
	defer func() {
		if err := storage.Delete(ctx, key); err != nil {
			t.Error("test object cleanup failed")
		}
	}()
	if err := storage.Put(ctx, key, bytes.NewReader(data), int64(len(data)), "application/octet-stream", sha256.Sum256(data)); err != nil {
		t.Fatalf("private object upload failed: %T", err)
	}
	get := func(address string) (int, string, []byte) {
		t.Helper()
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, address, nil)
		if err != nil {
			t.Fatal("cannot construct test request")
		}
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal("S3 test request failed")
		}
		defer resp.Body.Close()
		payload, err := io.ReadAll(io.LimitReader(resp.Body, 8192))
		if err != nil {
			t.Fatal("cannot read test object")
		}
		return resp.StatusCode, resp.Header.Get("Content-Disposition"), payload
	}
	if status, _, _ := get(strings.TrimRight(endpoint, "/") + "/autolife/" + key); status != http.StatusForbidden {
		t.Fatalf("anonymous access status: %d", status)
	}
	link, err := storage.PresignDownload(ctx, key, "fixture.bin", time.Now().Add(4*time.Second))
	if err != nil {
		t.Fatal("cannot sign private download")
	}
	if status, disposition, payload := get(link); status != http.StatusOK || !bytes.Equal(payload, data) || !strings.HasPrefix(disposition, "attachment") {
		t.Fatalf("unexpected private download: %d, disposition %q", status, disposition)
	}
	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	select {
	case <-timer.C:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	if status, _, _ := get(link); status != http.StatusForbidden {
		t.Fatalf("expired link status: %d", status)
	}
}

func TestS3OrphanListing(t *testing.T) {
	endpoint := os.Getenv("AUTOLIFE_TEST_S3_ENDPOINT")
	if endpoint == "" {
		t.Skip("requires a private test S3 bucket")
	}
	ctx := t.Context()
	storage, err := NewS3(S3Config{Endpoint: endpoint, Bucket: "autolife", Region: "us-east-1", AllowHTTP: true,
		AccessKey: os.Getenv("AUTOLIFE_S3_ACCESS_KEY"), SecretKey: os.Getenv("AUTOLIFE_S3_SECRET_KEY")})
	if err != nil {
		t.Fatal(err)
	}
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		t.Fatal(err)
	}
	h := hex.EncodeToString(raw[:])
	id := h[:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:]
	key := "files/" + id
	data := []byte("synthetic unreferenced object fixture")
	if err := storage.Put(ctx, key, bytes.NewReader(data), int64(len(data)), "application/octet-stream", sha256.Sum256(data)); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := storage.Delete(ctx, key); err != nil {
			t.Error(err)
		}
	}()
	find := func(cutoff time.Time) bool {
		var cursor *string
		for range 20 {
			found := false
			next, err := storage.orphanPage(ctx, cursor, cutoff, func(candidate, candidateKey string) error {
				if candidate == id && candidateKey == key {
					found = true
				}
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			if found || next == nil {
				return found
			}
			cursor = next
		}
		t.Fatal("orphan pagination did not finish")
		return false
	}
	if find(time.Now().Add(-time.Hour)) {
		t.Fatal("fresh object eligible for orphan deletion")
	}
	if !find(time.Now().Add(time.Hour)) {
		t.Fatal("server key missing from orphan listing")
	}
}
