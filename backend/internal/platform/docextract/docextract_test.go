package docextract_test

import (
	"errors"
	"os"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/Joakim-animate90/myjobfyt/backend/internal/platform/docextract"
)

const (
	pdfType  = "application/pdf"
	docxType = "application/vnd.openxmlformats-officedocument.wordprocessingml.document"
)

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	content, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	return content
}

func TestExtractPDFRecoversTheCVContent(t *testing.T) {
	result, err := docextract.Extract(fixture(t, "cv_single_page.pdf"), pdfType, docextract.DefaultLimits())
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}

	if result.PageCount != 1 {
		t.Errorf("PageCount = %d, want 1", result.PageCount)
	}
	if result.CharCount == 0 {
		t.Fatal("CharCount = 0")
	}

	// The specifics matter: these are the fields matching depends on. An extractor that
	// returns plausible-looking text but loses the employer and the skills produces a
	// profile that matches nothing, and the failure is invisible.
	for _, want := range []string{
		"Joakim Candidate",
		"Senior Backend Engineer",
		"Acme Corporation",
		"PostgreSQL",
		"Kubernetes",
		"University of Nairobi",
	} {
		if !strings.Contains(result.Text, want) {
			t.Errorf("extracted text is missing %q", want)
		}
	}
}

func TestExtractPDFCountsPagesAndKeepsAllOfThem(t *testing.T) {
	result, err := docextract.Extract(fixture(t, "cv_three_pages.pdf"), pdfType, docextract.DefaultLimits())
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if result.PageCount != 3 {
		t.Errorf("PageCount = %d, want 3", result.PageCount)
	}
	// Later pages must not be dropped: a CV's projects and references live there.
	if !strings.Contains(result.Text, "Page two") {
		t.Error("page 2 content is missing")
	}
	if !strings.Contains(result.Text, "references available") {
		t.Error("page 3 content is missing")
	}
}

func TestExtractPDFRejectsAScannedDocumentWithTheRightError(t *testing.T) {
	// The single most important failure case. A scanned CV yields no text, and silently
	// producing an empty profile would leave the candidate wondering why they match
	// nothing. The distinct error is what lets the UI say "export from Word instead".
	_, err := docextract.Extract(fixture(t, "scanned_no_text.pdf"), pdfType, docextract.DefaultLimits())
	if !errors.Is(err, docextract.ErrNoText) {
		t.Fatalf("Extract = %v, want ErrNoText", err)
	}
}

func TestExtractPDFRejectsADocumentOverThePageLimit(t *testing.T) {
	_, err := docextract.Extract(fixture(t, "cv_many_pages.pdf"), pdfType, docextract.DefaultLimits())
	if !errors.Is(err, docextract.ErrTooManyPages) {
		t.Fatalf("Extract = %v, want ErrTooManyPages", err)
	}
}

func TestExtractPDFRespectsACustomPageLimit(t *testing.T) {
	limits := docextract.DefaultLimits()
	limits.MaxPages = 2

	_, err := docextract.Extract(fixture(t, "cv_three_pages.pdf"), pdfType, limits)
	if !errors.Is(err, docextract.ErrTooManyPages) {
		t.Fatalf("Extract with MaxPages=2 on a 3-page document = %v, want ErrTooManyPages", err)
	}
}

func TestExtractDoesNotPanicOnHostileInput(t *testing.T) {
	// The PDF library panics on some malformed files rather than returning an error. A
	// panicking worker would be a denial-of-service vector: anyone could restart a pod
	// by uploading a crafted PDF.
	hostile := map[string][]byte{
		"corrupt fixture":      fixture(t, "corrupt.pdf"),
		"empty":                {},
		"header only":          []byte("%PDF-1.4\n"),
		"truncated mid-object": []byte("%PDF-1.4\n1 0 obj\n<< /Type /Catalog"),
		"random bytes":         {0x25, 0x50, 0x44, 0x46, 0xFF, 0xFE, 0x00, 0x01, 0x02},
		"huge declared length": []byte("%PDF-1.4\n1 0 obj\n<< /Length 999999999 >>\nstream\nx\nendstream\nendobj\n"),
		"recursive reference":  []byte("%PDF-1.4\n1 0 obj\n<< /Type /Catalog /Pages 1 0 R >>\nendobj\ntrailer\n<< /Root 1 0 R >>\n%%EOF"),
		"nul bytes":            append([]byte("%PDF-1.4\n"), make([]byte, 400)...),
	}

	for name, content := range hostile {
		t.Run(name, func(t *testing.T) {
			// The requirement is simply that it returns rather than panics.
			_, err := docextract.Extract(content, pdfType, docextract.DefaultLimits())
			if err == nil {
				t.Log("extraction unexpectedly succeeded; acceptable as long as it did not panic")
			}
		})
	}
}

func TestExtractDOCXRecoversTheCVContent(t *testing.T) {
	result, err := docextract.Extract(fixture(t, "cv_simple.docx"), docxType, docextract.DefaultLimits())
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}

	for _, want := range []string{
		"Joakim Candidate",
		"Senior Backend Engineer",
		"Acme Corporation",
		"PostgreSQL",
		"Terraform",
	} {
		if !strings.Contains(result.Text, want) {
			t.Errorf("extracted text is missing %q", want)
		}
	}
	// There is no reliable page count in OOXML without rendering, so it is reported as
	// zero rather than guessed — a caller must not mistake a fabricated number for a
	// real one.
	if result.PageCount != 0 {
		t.Errorf("PageCount = %d, want 0 for a DOCX", result.PageCount)
	}
}

func TestExtractDOCXReadsTableCells(t *testing.T) {
	// Many CVs are laid out entirely as tables. Ignoring table text would lose most of
	// the content while still appearing to succeed, which is the worst kind of bug:
	// the pipeline reports success and the candidate matches nothing.
	result, err := docextract.Extract(fixture(t, "cv_table.docx"), docxType, docextract.DefaultLimits())
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}

	for _, want := range []string{"Acme Corporation", "Globex Ltd", "2021-2024", "gRPC"} {
		if !strings.Contains(result.Text, want) {
			t.Errorf("table cell content %q was not extracted; text was:\n%s", want, result.Text)
		}
	}
}

func TestExtractDOCXRejectsAnEmptyDocument(t *testing.T) {
	_, err := docextract.Extract(fixture(t, "empty.docx"), docxType, docextract.DefaultLimits())
	if !errors.Is(err, docextract.ErrNoText) {
		t.Fatalf("Extract = %v, want ErrNoText", err)
	}
}

func TestExtractDOCXRejectsAZipWithoutADocumentPart(t *testing.T) {
	_, err := docextract.Extract(fixture(t, "not_a_docx.docx"), docxType, docextract.DefaultLimits())
	if !errors.Is(err, docextract.ErrCorrupt) {
		t.Fatalf("Extract = %v, want ErrCorrupt", err)
	}
}

func TestExtractDOCXRejectsCorruptArchives(t *testing.T) {
	hostile := map[string][]byte{
		"not a zip":          []byte("PK\x03\x04 but then nonsense"),
		"empty":              {},
		"truncated zip":      {0x50, 0x4B, 0x03, 0x04, 0x14, 0x00},
		"zip signature only": {0x50, 0x4B, 0x03, 0x04},
	}
	for name, content := range hostile {
		t.Run(name, func(t *testing.T) {
			if _, err := docextract.Extract(content, docxType, docextract.DefaultLimits()); err == nil {
				t.Fatal("corrupt archive was accepted")
			}
		})
	}
}

func TestExtractRejectsUnsupportedTypes(t *testing.T) {
	for _, contentType := range []string{"", "text/plain", "image/png", "application/zip"} {
		_, err := docextract.Extract([]byte("content"), contentType, docextract.DefaultLimits())
		if !errors.Is(err, docextract.ErrUnsupported) {
			t.Errorf("Extract(%q) = %v, want ErrUnsupported", contentType, err)
		}
	}
}

func TestExtractHandlesAContentTypeWithParameters(t *testing.T) {
	// Real uploads often carry "application/pdf; charset=binary".
	result, err := docextract.Extract(fixture(t, "cv_single_page.pdf"),
		"application/pdf; charset=binary", docextract.DefaultLimits())
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if result.CharCount == 0 {
		t.Fatal("no text extracted")
	}
}

func TestExtractedTextIsNormalised(t *testing.T) {
	result, err := docextract.Extract(fixture(t, "cv_single_page.pdf"), pdfType, docextract.DefaultLimits())
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}

	// Runs of blank lines waste tokens on every AI call that follows.
	if strings.Contains(result.Text, "\n\n\n") {
		t.Error("extracted text contains a run of three or more newlines")
	}
	if strings.HasPrefix(result.Text, " ") || strings.HasSuffix(result.Text, " ") {
		t.Error("extracted text is not trimmed")
	}
	if strings.Contains(result.Text, "\r") {
		t.Error("extracted text contains carriage returns")
	}
	// Invisible characters consume tokens and confuse tokenisers.
	for _, r := range []rune{0x200B, 0xFEFF, 0x00AD, 0xFFFD} {
		if strings.ContainsRune(result.Text, r) {
			t.Errorf("extracted text contains invisible rune U+%04X", r)
		}
	}
	// CharCount must count runes, not bytes, or a non-Latin CV reports the wrong size.
	if result.CharCount != len([]rune(result.Text)) {
		t.Errorf("CharCount = %d, want %d runes", result.CharCount, len([]rune(result.Text)))
	}
}

func TestExtractTruncatesRatherThanRejectingALongDocument(t *testing.T) {
	// A very long CV is unusual but legitimate. Truncating keeps one odd document from
	// consuming a month's AI allowance, while still producing a usable profile.
	limits := docextract.DefaultLimits()
	limits.MaxChars = 200

	result, err := docextract.Extract(fixture(t, "cv_single_page.pdf"), pdfType, limits)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if len(result.Text) > 200 {
		t.Fatalf("text is %d bytes, want at most 200", len(result.Text))
	}
	// Truncation must not split a multi-byte character, which would leave invalid UTF-8
	// for every consumer downstream — the database, the API response and the model.
	if !utf8.ValidString(result.Text) {
		t.Error("truncation produced invalid UTF-8")
	}
}

func TestZeroLimitsFallBackToDefaults(t *testing.T) {
	// A caller passing a zero-value Limits must get sane behaviour rather than a
	// document rejected for exceeding a limit of zero pages.
	result, err := docextract.Extract(fixture(t, "cv_single_page.pdf"), pdfType, docextract.Limits{})
	if err != nil {
		t.Fatalf("Extract with zero limits = %v, want the defaults to apply", err)
	}
	if result.CharCount == 0 {
		t.Fatal("no text extracted")
	}
}
