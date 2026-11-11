// Package storage holds candidate documents in S3-compatible object storage.
//
// CVs do not belong in Postgres: they are megabyte-scale binaries that would bloat
// backups and vacuum cycles for no benefit. Postgres stores the key; the bytes live
// here, and downloads go directly from storage to the client via a presigned URL so the
// application is never on the data path.
//
// This is the highest-risk input surface in the product — it accepts arbitrary binary
// documents from the internet — so validation is layered: declared type, magic bytes,
// size enforced during the stream, and an antivirus scan before the file is usable.
package storage

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"net/url"
	"strings"
	"time"

	"github.com/Joakim-animate90/myjobfyt/backend/internal/config"
	"github.com/google/uuid"
	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

// ContentType is a permitted document type.
type ContentType string

const (
	ContentTypePDF  ContentType = "application/pdf"
	ContentTypeDOCX ContentType = "application/vnd.openxmlformats-officedocument.wordprocessingml.document"
)

// String returns the MIME type.
func (c ContentType) String() string { return string(c) }

// Extension returns the canonical file extension.
func (c ContentType) Extension() string {
	switch c {
	case ContentTypePDF:
		return "pdf"
	case ContentTypeDOCX:
		return "docx"
	default:
		return "bin"
	}
}

// Errors returned by this package. They are sentinel values so the HTTP layer can map
// each to a precise, actionable message rather than a generic upload failure.
var (
	// ErrUnsupportedType means the content is not a PDF or DOCX, by its actual bytes.
	ErrUnsupportedType = errors.New("unsupported document type")
	// ErrTooLarge means the stream exceeded the configured cap.
	ErrTooLarge = errors.New("document exceeds the maximum size")
	// ErrEmpty means the upload contained no bytes.
	ErrEmpty = errors.New("document is empty")
	// ErrTypeMismatch means the declared type and the actual bytes disagree. Treated
	// separately from ErrUnsupportedType because it usually indicates an attack rather
	// than a user mistake, and is worth logging differently.
	ErrTypeMismatch = errors.New("declared content type does not match the document")
)

// Object describes a stored document.
type Object struct {
	Key         string
	ContentType ContentType
	SizeBytes   int64
	Checksum    [32]byte
}

// ChecksumHex returns the SHA-256 checksum as hex.
func (o Object) ChecksumHex() string { return fmt.Sprintf("%x", o.Checksum) }

// Store is the object-storage contract.
type Store interface {
	Put(ctx context.Context, tenantID uuid.UUID, declared string, r io.Reader) (Object, error)
	Get(ctx context.Context, key string) (io.ReadCloser, error)
	PresignedGet(ctx context.Context, key, downloadName string) (string, error)
	Delete(ctx context.Context, key string) error
	Exists(ctx context.Context, key string) (bool, error)
	Ping(ctx context.Context) error
}

// S3Store is the S3-compatible implementation.
type S3Store struct {
	client     *minio.Client
	bucket     string
	presignTTL time.Duration
	maxBytes   int64
}

// Open connects to object storage, creating the bucket if absent.
//
// Bucket creation lives here rather than in an init container because the `mc` client
// image is not reachable from every environment, and because a missing bucket should be
// a recoverable startup condition rather than a silent 500 on first upload.
func Open(ctx context.Context, cfg config.StorageConfig) (*S3Store, error) {
	client, err := minio.New(cfg.Endpoint, &minio.Options{
		Creds:  credentials.NewStaticV4(cfg.AccessKey, cfg.SecretKey, ""),
		Secure: cfg.UseSSL,
		Region: cfg.Region,
	})
	if err != nil {
		return nil, fmt.Errorf("create object storage client: %w", err)
	}

	store := &S3Store{
		client:     client,
		bucket:     cfg.Bucket,
		presignTTL: cfg.PresignTTL,
		maxBytes:   cfg.MaxCVBytes,
	}

	checkCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	exists, err := client.BucketExists(checkCtx, cfg.Bucket)
	if err != nil {
		return nil, fmt.Errorf("check bucket %q: %w", cfg.Bucket, err)
	}
	if !exists {
		if err := client.MakeBucket(checkCtx, cfg.Bucket, minio.MakeBucketOptions{Region: cfg.Region}); err != nil {
			// Another replica may have won the race; re-check rather than failing.
			if ok, cerr := client.BucketExists(checkCtx, cfg.Bucket); cerr != nil || !ok {
				return nil, fmt.Errorf("create bucket %q: %w", cfg.Bucket, err)
			}
		}
	}

	return store, nil
}

// Put validates and stores a document, returning its key, size and checksum.
//
// Validation order matters. The size cap is applied to the stream itself, so an
// oversized upload is refused during transfer rather than after being read into memory
// — the difference between rejecting a 2GB upload and being killed by it.
func (s *S3Store) Put(ctx context.Context, tenantID uuid.UUID, declared string, r io.Reader) (Object, error) {
	if tenantID == uuid.Nil {
		return Object{}, errors.New("a tenant id is required to namespace the object key")
	}

	// Read just enough to identify the type. 512 bytes covers every signature we
	// accept and is small enough to buffer unconditionally.
	header := make([]byte, sniffLength)
	n, err := io.ReadFull(r, header)
	switch {
	case errors.Is(err, io.EOF):
		return Object{}, ErrEmpty
	case errors.Is(err, io.ErrUnexpectedEOF):
		// A short file is fine as long as it is still identifiable.
		header = header[:n]
	case err != nil:
		return Object{}, fmt.Errorf("read document header: %w", err)
	}

	if len(header) == 0 {
		return Object{}, ErrEmpty
	}

	actual, ok := DetectContentType(header)
	if !ok {
		return Object{}, ErrUnsupportedType
	}
	// The declared type is advisory — a client can claim anything — but a mismatch is
	// worth surfacing distinctly, because it is more often an attack than a mistake.
	//
	// A generic declaration is not a mismatch. Browsers and command-line clients send
	// application/octet-stream whenever they do not recognise an extension, which is
	// routine for .docx, so treating it as a contradiction would reject ordinary
	// uploads from ordinary clients.
	if isSpecificDeclaration(declared) && !strings.EqualFold(declared, actual.String()) {
		return Object{}, ErrTypeMismatch
	}

	key := objectKey(tenantID, actual)

	// Reassemble the stream: the sniffed header, then the rest, through a size limiter
	// and a hasher.
	body := io.MultiReader(bytes.NewReader(header), r)
	limited := &limitedReader{r: body, remaining: s.maxBytes + 1}
	hasher := sha256.New()
	counting := &countingReader{r: io.TeeReader(limited, hasher)}

	// Size -1 tells minio-go to stream with an unknown length, which is what lets the
	// cap be enforced by the reader rather than requiring the whole body up front.
	_, err = s.client.PutObject(ctx, s.bucket, key, counting, -1, minio.PutObjectOptions{
		ContentType: actual.String(),
		UserMetadata: map[string]string{
			// Tenant is recorded on the object too, so an object found in storage can
			// be attributed without consulting the database.
			"tenant": tenantID.String(),
		},
	})

	if limited.exceeded {
		// Remove the partial object: a half-written CV is worse than none, and the
		// orphan would otherwise linger until the reaper runs.
		s.deleteQuietly(ctx, key)
		return Object{}, ErrTooLarge
	}
	if err != nil {
		s.deleteQuietly(ctx, key)
		return Object{}, fmt.Errorf("store object: %w", err)
	}
	if counting.n == 0 {
		s.deleteQuietly(ctx, key)
		return Object{}, ErrEmpty
	}

	var checksum [32]byte
	copy(checksum[:], hasher.Sum(nil))

	return Object{
		Key:         key,
		ContentType: actual,
		SizeBytes:   counting.n,
		Checksum:    checksum,
	}, nil
}

// genericDeclarations are content types that carry no information about the payload, so
// they cannot contradict what the bytes say.
var genericDeclarations = map[string]struct{}{
	"":                         {},
	"application/octet-stream": {},
	"binary/octet-stream":      {},
	"application/x-binary":     {},
	"*/*":                      {},
}

// isSpecificDeclaration reports whether a declared content type is specific enough to
// be worth comparing against the sniffed type.
func isSpecificDeclaration(declared string) bool {
	normalised := strings.ToLower(strings.TrimSpace(declared))
	// Strip any parameters, e.g. "application/pdf; charset=binary".
	if i := strings.IndexByte(normalised, ';'); i >= 0 {
		normalised = strings.TrimSpace(normalised[:i])
	}
	_, generic := genericDeclarations[normalised]
	return !generic
}

// deleteQuietly removes an object on a cleanup path, where a failure must not mask the
// original error. Uses a detached context because the request context is often already
// cancelled by the time cleanup runs.
func (s *S3Store) deleteQuietly(ctx context.Context, key string) {
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	_ = s.client.RemoveObject(cleanupCtx, s.bucket, key, minio.RemoveObjectOptions{})
}

// Get opens a stored object for reading. Used by the parse worker; candidate downloads
// go through PresignedGet instead so the API never proxies the bytes.
func (s *S3Store) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	obj, err := s.client.GetObject(ctx, s.bucket, key, minio.GetObjectOptions{})
	if err != nil {
		return nil, fmt.Errorf("open object %q: %w", key, err)
	}
	// GetObject is lazy, so a missing key only surfaces on first read or Stat. Stat now
	// so the caller gets the error here rather than mid-stream.
	if _, err := obj.Stat(); err != nil {
		_ = obj.Close()
		return nil, fmt.Errorf("stat object %q: %w", key, err)
	}
	return obj, nil
}

// PresignedGet returns a short-lived download URL.
//
// These URLs are bearer credentials for personal data, so the TTL is minutes rather
// than hours. The download name is set via response-content-disposition so the
// candidate gets a sensible filename without the key having to contain one.
func (s *S3Store) PresignedGet(ctx context.Context, key, downloadName string) (string, error) {
	params := url.Values{}
	if downloadName != "" {
		params.Set("response-content-disposition",
			fmt.Sprintf("attachment; filename=%q", sanitiseFilename(downloadName)))
	}

	signed, err := s.client.PresignedGetObject(ctx, s.bucket, key, s.presignTTL, params)
	if err != nil {
		return "", fmt.Errorf("presign object %q: %w", key, err)
	}
	return signed.String(), nil
}

// Delete removes an object. Idempotent: deleting an absent key is not an error, which
// makes the erasure job safely resumable.
func (s *S3Store) Delete(ctx context.Context, key string) error {
	if err := s.client.RemoveObject(ctx, s.bucket, key, minio.RemoveObjectOptions{}); err != nil {
		return fmt.Errorf("delete object %q: %w", key, err)
	}
	return nil
}

// Exists reports whether an object is present. Used by the erasure verification pass,
// which must be able to assert that a deletion actually happened.
func (s *S3Store) Exists(ctx context.Context, key string) (bool, error) {
	_, err := s.client.StatObject(ctx, s.bucket, key, minio.StatObjectOptions{})
	if err == nil {
		return true, nil
	}
	if minio.ToErrorResponse(err).Code == "NoSuchKey" {
		return false, nil
	}
	return false, fmt.Errorf("stat object %q: %w", key, err)
}

// Ping verifies reachability for the readiness probe.
func (s *S3Store) Ping(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	if _, err := s.client.BucketExists(ctx, s.bucket); err != nil {
		return fmt.Errorf("object storage unreachable: %w", err)
	}
	return nil
}

// objectKey builds a per-tenant, opaque key.
//
// The candidate's original filename never appears. Filenames are attacker-controlled,
// often contain personal data, and would make keys guessable; a UUID under a tenant
// prefix is unguessable and makes per-tenant erasure a prefix operation.
func objectKey(tenantID uuid.UUID, ct ContentType) string {
	return fmt.Sprintf("tenants/%s/%s.%s", tenantID, uuid.NewString(), ct.Extension())
}

// TenantPrefix returns the key prefix for a tenant, for bulk erasure.
func TenantPrefix(tenantID uuid.UUID) string {
	return fmt.Sprintf("tenants/%s/", tenantID)
}

// sanitiseFilename strips anything that could break a Content-Disposition header or
// traverse a path when the browser saves the file.
func sanitiseFilename(name string) string {
	name = strings.ReplaceAll(name, "\\", "")
	name = strings.ReplaceAll(name, "\"", "")
	name = strings.ReplaceAll(name, "\r", "")
	name = strings.ReplaceAll(name, "\n", "")
	// Keep only the base name: a client-supplied path must not influence where the
	// file lands on the candidate's machine.
	if i := strings.LastIndexAny(name, "/"); i >= 0 {
		name = name[i+1:]
	}
	name = strings.TrimSpace(name)
	if name == "" {
		return "document"
	}
	if len(name) > 120 {
		name = name[:120]
	}
	return name
}

// limitedReader fails once more than `remaining` bytes have been read, recording that
// it happened so the caller can distinguish an oversized upload from an I/O error.
type limitedReader struct {
	r         io.Reader
	remaining int64
	exceeded  bool
}

func (l *limitedReader) Read(p []byte) (int, error) {
	if l.remaining <= 0 {
		l.exceeded = true
		return 0, ErrTooLarge
	}
	if int64(len(p)) > l.remaining {
		p = p[:l.remaining]
	}
	n, err := l.r.Read(p)
	l.remaining -= int64(n)
	if l.remaining <= 0 && err == nil {
		l.exceeded = true
		return n, ErrTooLarge
	}
	return n, err
}

// countingReader records how many bytes passed through.
type countingReader struct {
	r io.Reader
	n int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}
