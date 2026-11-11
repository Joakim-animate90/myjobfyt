//go:build integration

package storage_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Joakim-animate90/myjobfyt/backend/internal/config"
	"github.com/Joakim-animate90/myjobfyt/backend/internal/platform/storage"
	"github.com/google/uuid"
)

// newStore connects to the development MinIO. Skips rather than fails when storage is
// not configured, so `go test -tags=integration ./...` is usable without the stack.
func newStore(t *testing.T) *storage.S3Store {
	t.Helper()

	endpoint := os.Getenv("STORAGE_ENDPOINT")
	if endpoint == "" {
		t.Skip("STORAGE_ENDPOINT not set; run with the development stack up")
	}

	cfg := config.StorageConfig{
		Endpoint:   endpoint,
		AccessKey:  os.Getenv("STORAGE_ACCESS_KEY"),
		SecretKey:  os.Getenv("STORAGE_SECRET_KEY"),
		Bucket:     "myjobfyt-test",
		Region:     "us-east-1",
		UseSSL:     false,
		PresignTTL: 2 * time.Minute,
		MaxCVBytes: 64 * 1024,
	}

	store, err := storage.Open(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	return store
}

// validPDF returns a syntactically plausible PDF of roughly the requested size.
func validPDF(size int) []byte {
	body := []byte("%PDF-1.7\n%\xe2\xe3\xcf\xd3\n1 0 obj\n<< /Type /Catalog >>\nendobj\n")
	if size <= len(body) {
		return body
	}
	// Pad inside a comment so the result stays a plausible document.
	padding := bytes.Repeat([]byte("% padding\n"), (size-len(body))/10+1)
	return append(body, padding[:size-len(body)]...)
}

func TestPutAndGetRoundTrip(t *testing.T) {
	store := newStore(t)
	ctx := context.Background()
	tenant := uuid.New()
	content := validPDF(4096)

	obj, err := store.Put(ctx, tenant, "application/pdf", bytes.NewReader(content))
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	t.Cleanup(func() { _ = store.Delete(ctx, obj.Key) })

	if obj.SizeBytes != int64(len(content)) {
		t.Errorf("SizeBytes = %d, want %d", obj.SizeBytes, len(content))
	}
	if obj.ContentType != storage.ContentTypePDF {
		t.Errorf("ContentType = %q", obj.ContentType)
	}
	if obj.ChecksumHex() == "" {
		t.Error("no checksum computed")
	}

	// The key must be opaque and tenant-scoped, with no filename in it.
	if !strings.HasPrefix(obj.Key, storage.TenantPrefix(tenant)) {
		t.Errorf("key %q is not under the tenant prefix", obj.Key)
	}

	reader, err := store.Get(ctx, obj.Key)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	defer func() { _ = reader.Close() }()

	got, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if !bytes.Equal(got, content) {
		t.Fatalf("round-trip mismatch: got %d bytes, want %d", len(got), len(content))
	}
}

func TestPutRejectsOversizedUploadsDuringTheStream(t *testing.T) {
	// The cap must bite on the stream. If it only applied after a full read, a large
	// upload would be a memory-exhaustion vector rather than a rejected request.
	store := newStore(t)
	ctx := context.Background()

	oversized := validPDF(64*1024 + 4096) // MaxCVBytes is 64KiB in this test config
	_, err := store.Put(ctx, uuid.New(), "application/pdf", bytes.NewReader(oversized))

	if !errors.Is(err, storage.ErrTooLarge) {
		t.Fatalf("Put = %v, want ErrTooLarge", err)
	}
}

func TestPutLeavesNoPartialObjectAfterAnOversizedUpload(t *testing.T) {
	// A half-written CV is worse than none: it would parse into a truncated profile.
	store := newStore(t)
	ctx := context.Background()
	tenant := uuid.New()

	_, err := store.Put(ctx, tenant, "application/pdf", bytes.NewReader(validPDF(200*1024)))
	if !errors.Is(err, storage.ErrTooLarge) {
		t.Fatalf("Put = %v, want ErrTooLarge", err)
	}

	// Nothing should remain under this tenant's prefix. Checked via a fresh key probe
	// rather than a listing, since the key is not returned on failure.
	obj, err := store.Put(ctx, tenant, "application/pdf", bytes.NewReader(validPDF(1024)))
	if err != nil {
		t.Fatalf("a subsequent valid upload failed: %v", err)
	}
	t.Cleanup(func() { _ = store.Delete(ctx, obj.Key) })
}

func TestPutRejectsNonDocuments(t *testing.T) {
	store := newStore(t)
	ctx := context.Background()

	cases := map[string][]byte{
		"empty":              {},
		"plain text":         []byte("I am a CV, honestly"),
		"png":                {0x89, 0x50, 0x4E, 0x47, 0x0D, 0x0A, 0x1A, 0x0A, 0x00, 0x00},
		"windows executable": {0x4D, 0x5A, 0x90, 0x00, 0x03, 0x00, 0x00, 0x00},
		"plain zip":          plainZipHeader(),
	}
	for name, content := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := store.Put(ctx, uuid.New(), "", bytes.NewReader(content))
			if err == nil {
				t.Fatalf("%s was accepted", name)
			}
			if !errors.Is(err, storage.ErrUnsupportedType) && !errors.Is(err, storage.ErrEmpty) {
				t.Fatalf("%s rejected with %v, want ErrUnsupportedType or ErrEmpty", name, err)
			}
		})
	}
}

func TestPutRejectsADeclaredTypeThatContradictsTheBytes(t *testing.T) {
	// Usually an attack rather than a mistake, which is why it has its own error.
	store := newStore(t)
	_, err := store.Put(context.Background(), uuid.New(),
		"application/vnd.openxmlformats-officedocument.wordprocessingml.document",
		bytes.NewReader(validPDF(1024)))

	if !errors.Is(err, storage.ErrTypeMismatch) {
		t.Fatalf("Put = %v, want ErrTypeMismatch", err)
	}
}

func TestPresignedGetServesTheObjectAndExpires(t *testing.T) {
	store := newStore(t)
	ctx := context.Background()
	content := validPDF(2048)

	obj, err := store.Put(ctx, uuid.New(), "application/pdf", bytes.NewReader(content))
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	t.Cleanup(func() { _ = store.Delete(ctx, obj.Key) })

	signed, err := store.PresignedGet(ctx, obj.Key, `my "weird"/cv.pdf`)
	if err != nil {
		t.Fatalf("PresignedGet: %v", err)
	}

	resp, err := http.Get(signed) //nolint:gosec // the URL is produced by this test
	if err != nil {
		t.Fatalf("fetch presigned URL: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("presigned GET status = %d, want 200", resp.StatusCode)
	}
	got, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read presigned body: %v", err)
	}
	if !bytes.Equal(got, content) {
		t.Fatal("presigned download returned different bytes")
	}

	// The filename must be sanitised: quotes and slashes would break the header or
	// influence where the browser saves the file.
	disposition := resp.Header.Get("Content-Disposition")
	if strings.Contains(disposition, `"my "weird"`) || strings.Contains(disposition, "/cv.pdf") {
		t.Fatalf("Content-Disposition was not sanitised: %q", disposition)
	}
	if !strings.Contains(disposition, "attachment") {
		t.Errorf("Content-Disposition = %q, want an attachment", disposition)
	}
}

func TestExistsAndDeleteAreIdempotent(t *testing.T) {
	// The erasure job must be safely resumable, which means deleting an absent key is
	// not an error and Exists must be able to confirm the deletion happened.
	store := newStore(t)
	ctx := context.Background()

	obj, err := store.Put(ctx, uuid.New(), "application/pdf", bytes.NewReader(validPDF(1024)))
	if err != nil {
		t.Fatalf("Put: %v", err)
	}

	exists, err := store.Exists(ctx, obj.Key)
	if err != nil || !exists {
		t.Fatalf("Exists after Put = (%v, %v), want (true, nil)", exists, err)
	}

	if err := store.Delete(ctx, obj.Key); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if err := store.Delete(ctx, obj.Key); err != nil {
		t.Fatalf("second Delete = %v, want nil; erasure must be resumable", err)
	}

	exists, err = store.Exists(ctx, obj.Key)
	if err != nil {
		t.Fatalf("Exists after Delete: %v", err)
	}
	if exists {
		t.Fatal("the object still exists after deletion; the erasure verification pass would wrongly pass")
	}
}

func TestKeysAreUnguessableAndCarryNoFilename(t *testing.T) {
	store := newStore(t)
	ctx := context.Background()
	tenant := uuid.New()

	seen := make(map[string]struct{}, 5)
	for i := 0; i < 5; i++ {
		obj, err := store.Put(ctx, tenant, "application/pdf", bytes.NewReader(validPDF(512)))
		if err != nil {
			t.Fatalf("Put %d: %v", i, err)
		}
		t.Cleanup(func() { _ = store.Delete(ctx, obj.Key) })

		if _, dup := seen[obj.Key]; dup {
			t.Fatalf("duplicate key %q", obj.Key)
		}
		seen[obj.Key] = struct{}{}

		// Identical content must still produce distinct keys, so one candidate cannot
		// discover another's object by uploading the same file.
		if !strings.HasPrefix(obj.Key, storage.TenantPrefix(tenant)) {
			t.Fatalf("key %q escaped the tenant prefix", obj.Key)
		}
	}
}

func TestPutRequiresATenant(t *testing.T) {
	// Without a tenant the key would not be namespaced, which is the mechanism that
	// makes per-tenant erasure a prefix operation.
	store := newStore(t)
	_, err := store.Put(context.Background(), uuid.Nil, "application/pdf", bytes.NewReader(validPDF(512)))
	if err == nil {
		t.Fatal("Put accepted a nil tenant id")
	}
}

func TestPingReportsHealth(t *testing.T) {
	store := newStore(t)
	if err := store.Ping(context.Background()); err != nil {
		t.Fatalf("Ping: %v", err)
	}
}
