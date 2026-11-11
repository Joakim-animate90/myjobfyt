package antivirus

// Internal tests: interpretClamAVResponse is the function where a mistake becomes a
// silent bypass, so it is tested directly rather than only through a live clamd.

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"
)

func TestInterpretClamAVResponse(t *testing.T) {
	cases := []struct {
		name          string
		response      string
		wantVerdict   Verdict
		wantSignature string
		wantErr       bool
	}{
		{"clean", "stream: OK\x00", VerdictClean, "", false},
		{"clean without nul", "stream: OK", VerdictClean, "", false},
		{"clean with whitespace", "  stream: OK  \x00", VerdictClean, "", false},

		{"infected", "stream: Eicar-Test-Signature FOUND\x00", VerdictInfected, "Eicar-Test-Signature", false},
		{"infected with a path-like name", "stream: Win.Test.EICAR_HDB-1 FOUND\x00", VerdictInfected, "Win.Test.EICAR_HDB-1", false},

		{"scanner error", "INSTREAM size limit exceeded. ERROR\x00", VerdictError, "", true},
		{"empty response", "", VerdictError, "", true},
		{"nul only", "\x00", VerdictError, "", true},

		// The important cases. Anything unrecognised must be an error, never a pass:
		// guessing here would be a silent bypass, and a response that merely contains
		// the word OK is not a clean verdict.
		{"unrecognised", "stream: something unexpected\x00", VerdictError, "", true},
		{"OK in the middle", "stream: OK-looking but FAILED somehow\x00", VerdictError, "", true},
		{"truncated FOUND", "stream: Eicar-Test-Signature FOU\x00", VerdictError, "", true},
		{"lowercase ok", "stream: ok\x00", VerdictError, "", true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			result, err := interpretClamAVResponse(tc.response)

			if result.Verdict != tc.wantVerdict {
				t.Errorf("Verdict = %q, want %q", result.Verdict, tc.wantVerdict)
			}
			if result.Signature != tc.wantSignature {
				t.Errorf("Signature = %q, want %q", result.Signature, tc.wantSignature)
			}
			if (err != nil) != tc.wantErr {
				t.Errorf("err = %v, wantErr = %v", err, tc.wantErr)
			}
		})
	}
}

func TestErrorVerdictIsNeverClean(t *testing.T) {
	// An unscannable file is not a safe file. Conflating the two is how a scanner
	// outage becomes a silent bypass, so this is asserted explicitly across every
	// response shape that is not a definite pass.
	for _, response := range []string{
		"", "\x00", "ERROR", "stream: ERROR", "garbage",
		"stream: unexpected", "connection reset",
	} {
		result, _ := interpretClamAVResponse(response)
		if result.Verdict == VerdictClean {
			t.Errorf("response %q produced a clean verdict", response)
		}
	}
}

func TestScanReturnsAnErrorVerdictWhenTheScannerIsUnreachable(t *testing.T) {
	// Port 1 on localhost is reserved and nothing listens there.
	scanner := NewClamAV(ClamAVConfig{Addr: "127.0.0.1:1", Timeout: time.Second})

	result, err := scanner.Scan(context.Background(), strings.NewReader("content"))
	if err == nil {
		t.Fatal("Scan succeeded against an unreachable scanner")
	}
	if result.Verdict != VerdictError {
		t.Fatalf("Verdict = %q, want error", result.Verdict)
	}
	// The caller distinguishes unavailability (retry) from a real verdict, so the
	// sentinel must survive wrapping.
	if !errors.Is(err, ErrScannerUnavailable) {
		t.Errorf("err = %v, want it to wrap ErrScannerUnavailable", err)
	}
}

func TestPingFailsWhenUnreachable(t *testing.T) {
	scanner := NewClamAV(ClamAVConfig{Addr: "127.0.0.1:1", Timeout: time.Second})
	if err := scanner.Ping(context.Background()); err == nil {
		t.Fatal("Ping succeeded against an unreachable scanner")
	}
}

func TestDevelopmentScannerDrainsTheContent(t *testing.T) {
	// The body must still be read: a caller may be streaming from object storage, and
	// leaving it unread would make the development path behave differently from the
	// real one in a way that hides bugs.
	var buf bytes.Buffer
	scanner := NewDevelopmentScanner(slog.New(slog.NewTextHandler(&buf, nil)))

	content := strings.Repeat("x", 4096)
	reader := strings.NewReader(content)

	result, err := scanner.Scan(context.Background(), reader)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if result.Verdict != VerdictClean {
		t.Fatalf("Verdict = %q, want clean", result.Verdict)
	}
	if remaining, _ := io.ReadAll(reader); len(remaining) != 0 {
		t.Fatalf("%d bytes left unread; the development path must drain like the real one", len(remaining))
	}
}

func TestDevelopmentScannerIsLoudAboutItself(t *testing.T) {
	// Nobody should be able to forget which scanner is running. It warns at
	// construction and on every scan, and its name makes a stored verdict obviously
	// not a real one when read back later.
	var buf bytes.Buffer
	scanner := NewDevelopmentScanner(slog.New(slog.NewTextHandler(&buf, nil)))

	if !strings.Contains(buf.String(), "DISABLED") {
		t.Errorf("construction did not warn: %q", buf.String())
	}

	buf.Reset()
	if _, err := scanner.Scan(context.Background(), strings.NewReader("x")); err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if !strings.Contains(buf.String(), "without scanning") {
		t.Errorf("scan did not warn: %q", buf.String())
	}
	if name := scanner.Name(); !strings.Contains(name, "no-scan") {
		t.Errorf("Name() = %q; it must make a stored verdict obviously unreal", name)
	}
}
