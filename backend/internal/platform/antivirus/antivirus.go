// Package antivirus scans uploaded documents.
//
// Mandatory for an endpoint that accepts arbitrary files from the internet. The scan is
// not primarily about protecting our own workers — they never execute the file — but
// about not becoming a distribution point: a CV is downloaded by the candidate, handed
// to employers, and in the email channel attached to outbound mail. Forwarding malware
// to an employer under a candidate's name would be unforgivable.
package antivirus

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"strings"
	"time"
)

// Verdict is the outcome of a scan.
type Verdict string

const (
	// VerdictClean means the scanner examined the content and found nothing.
	VerdictClean Verdict = "clean"
	// VerdictInfected means a signature matched.
	VerdictInfected Verdict = "infected"
	// VerdictError means the scan could not be completed. Deliberately distinct from
	// clean: an unscannable file is not a safe file, and conflating them is how a
	// scanner outage turns into a silent bypass.
	VerdictError Verdict = "error"
)

// Result carries the verdict and, when infected, the signature that matched.
type Result struct {
	Verdict   Verdict
	Signature string
}

// Scanner scans content.
type Scanner interface {
	// Scan examines the content and returns a verdict. An error means the scan could
	// not be performed; the caller must not treat that as clean.
	Scan(ctx context.Context, r io.Reader) (Result, error)
	// Ping reports whether the scanner is reachable, for the readiness probe.
	Ping(ctx context.Context) error
	// Name identifies the implementation, so logs and the admin UI can show which
	// scanner produced a verdict.
	Name() string
}

// ErrScannerUnavailable means the scanner could not be reached.
var ErrScannerUnavailable = errors.New("antivirus scanner unavailable")

// ClamAVScanner talks to clamd over TCP using the INSTREAM command.
type ClamAVScanner struct {
	addr      string
	timeout   time.Duration
	chunkSize int
	maxBytes  int64
}

// ClamAVConfig configures the scanner.
type ClamAVConfig struct {
	Addr    string
	Timeout time.Duration
	// MaxBytes must not exceed clamd's own StreamMaxLength, or clamd aborts the stream
	// mid-scan and the result is an error rather than a verdict.
	MaxBytes int64
}

// NewClamAV builds a clamd client.
func NewClamAV(cfg ClamAVConfig) *ClamAVScanner {
	if cfg.Timeout <= 0 {
		cfg.Timeout = 60 * time.Second
	}
	if cfg.MaxBytes <= 0 {
		cfg.MaxBytes = 25 << 20
	}
	return &ClamAVScanner{
		addr:    cfg.Addr,
		timeout: cfg.Timeout,
		// clamd's INSTREAM protocol caps a chunk at 4GB but recommends small chunks;
		// 64KiB keeps memory flat regardless of document size.
		chunkSize: 64 << 10,
		maxBytes:  cfg.MaxBytes,
	}
}

// Name identifies the scanner.
func (s *ClamAVScanner) Name() string { return "clamav" }

// Scan streams the content to clamd and interprets the response.
func (s *ClamAVScanner) Scan(ctx context.Context, r io.Reader) (Result, error) {
	conn, err := s.dial(ctx)
	if err != nil {
		return Result{Verdict: VerdictError}, err
	}
	defer func() { _ = conn.Close() }()

	if _, err := conn.Write([]byte("zINSTREAM\x00")); err != nil {
		return Result{Verdict: VerdictError}, fmt.Errorf("send INSTREAM: %w", err)
	}

	buf := make([]byte, s.chunkSize)
	var sent int64

	for {
		n, rerr := r.Read(buf)
		if n > 0 {
			sent += int64(n)
			if sent > s.maxBytes {
				// Abort cleanly rather than letting clamd terminate the connection:
				// a zero-length chunk ends the stream, and the distinction keeps the
				// failure diagnosable.
				_, _ = conn.Write([]byte{0, 0, 0, 0})
				return Result{Verdict: VerdictError},
					fmt.Errorf("content exceeds the scanner limit of %d bytes", s.maxBytes)
			}

			// Each chunk is a 4-byte big-endian length followed by the data.
			header := []byte{
				byte(n >> 24), byte(n >> 16), byte(n >> 8), byte(n),
			}
			if _, werr := conn.Write(header); werr != nil {
				return Result{Verdict: VerdictError}, fmt.Errorf("send chunk header: %w", werr)
			}
			if _, werr := conn.Write(buf[:n]); werr != nil {
				return Result{Verdict: VerdictError}, fmt.Errorf("send chunk: %w", werr)
			}
		}

		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			return Result{Verdict: VerdictError}, fmt.Errorf("read content: %w", rerr)
		}
	}

	// A zero-length chunk terminates the stream.
	if _, err := conn.Write([]byte{0, 0, 0, 0}); err != nil {
		return Result{Verdict: VerdictError}, fmt.Errorf("terminate stream: %w", err)
	}

	response, err := bufio.NewReader(conn).ReadString(0)
	if err != nil && !errors.Is(err, io.EOF) {
		return Result{Verdict: VerdictError}, fmt.Errorf("read scanner response: %w", err)
	}

	return interpretClamAVResponse(response)
}

// interpretClamAVResponse parses clamd's reply.
//
// The response is terminated by a NUL and takes one of three shapes: "stream: OK",
// "stream: <signature> FOUND", or an error containing "ERROR". Anything unrecognised is
// an error, not a pass — guessing in this function would be a silent bypass.
func interpretClamAVResponse(response string) (Result, error) {
	// NUL first, then whitespace. The other order leaves trailing spaces in place,
	// because a NUL is not whitespace, so TrimSpace stops at it — and "stream: OK  "
	// then fails the suffix check and a clean file is reported as a scanner error.
	cleaned := strings.TrimSpace(strings.Trim(response, "\x00"))

	switch {
	case strings.HasSuffix(cleaned, "OK"):
		return Result{Verdict: VerdictClean}, nil

	case strings.HasSuffix(cleaned, "FOUND"):
		signature := strings.TrimSpace(strings.TrimSuffix(cleaned, "FOUND"))
		signature = strings.TrimPrefix(signature, "stream:")
		return Result{Verdict: VerdictInfected, Signature: strings.TrimSpace(signature)}, nil

	case strings.Contains(cleaned, "ERROR"):
		return Result{Verdict: VerdictError}, fmt.Errorf("scanner reported: %s", cleaned)

	case cleaned == "":
		return Result{Verdict: VerdictError}, errors.New("scanner returned an empty response")

	default:
		return Result{Verdict: VerdictError},
			fmt.Errorf("unrecognised scanner response: %q", cleaned)
	}
}

// Ping checks that clamd answers.
func (s *ClamAVScanner) Ping(ctx context.Context) error {
	conn, err := s.dial(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()

	if _, err := conn.Write([]byte("zPING\x00")); err != nil {
		return fmt.Errorf("send PING: %w", err)
	}

	response, err := bufio.NewReader(conn).ReadString(0)
	if err != nil && !errors.Is(err, io.EOF) {
		return fmt.Errorf("read PONG: %w", err)
	}
	if !strings.Contains(response, "PONG") {
		return fmt.Errorf("unexpected PING response: %q", strings.TrimSpace(response))
	}
	return nil
}

func (s *ClamAVScanner) dial(ctx context.Context) (net.Conn, error) {
	dialer := &net.Dialer{Timeout: s.timeout}
	conn, err := dialer.DialContext(ctx, "tcp", s.addr)
	if err != nil {
		return nil, fmt.Errorf("%w: dial %s: %v", ErrScannerUnavailable, s.addr, err)
	}
	// A deadline covering the whole exchange: clamd can accept a connection and then
	// stall while loading its signature database, which would otherwise hold a worker
	// indefinitely.
	if s.timeout > 0 {
		_ = conn.SetDeadline(time.Now().Add(s.timeout))
	}
	return conn, nil
}

// DevelopmentScanner passes everything, loudly.
//
// Exists so the platform runs end to end without a 300-second ClamAV signature
// download on every developer's machine. It is refused in production by configuration
// validation, because an unconditional pass is indistinguishable from having no scanner
// at all — and it logs on every scan so nobody can forget which one is running.
type DevelopmentScanner struct{ log *slog.Logger }

// NewDevelopmentScanner builds the pass-through scanner.
func NewDevelopmentScanner(log *slog.Logger) *DevelopmentScanner {
	log.Warn("antivirus scanning is DISABLED; every upload will be marked clean without being examined")
	return &DevelopmentScanner{log: log}
}

// Name identifies the scanner. Deliberately explicit, so a verdict recorded by this
// scanner is obviously not a real one when read back later.
func (s *DevelopmentScanner) Name() string { return "development-no-scan" }

// Scan drains the content and returns clean.
func (s *DevelopmentScanner) Scan(ctx context.Context, r io.Reader) (Result, error) {
	// The content is still drained: a caller may be streaming from storage, and leaving
	// the body unread would make the development path behave differently from the real
	// one in a way that hides bugs.
	n, err := io.Copy(io.Discard, r)
	if err != nil {
		return Result{Verdict: VerdictError}, fmt.Errorf("read content: %w", err)
	}

	s.log.WarnContext(ctx, "upload marked clean without scanning (development scanner)",
		slog.Int64("bytes", n))
	return Result{Verdict: VerdictClean}, nil
}

// Ping always succeeds.
func (s *DevelopmentScanner) Ping(context.Context) error { return nil }
