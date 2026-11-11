// Package docextract pulls plain text out of PDF and DOCX documents.
//
// Deterministic extraction runs before any AI call, for three reasons: it is free, it
// is reproducible, and it answers the one question that decides whether the expensive
// path is worth taking at all — does this document contain machine-readable text? A
// scanned CV has none, and sending it to a model produces an empty profile at full cost.
package docextract

import (
	"archive/zip"
	"bytes"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"strings"
	"unicode"

	"github.com/ledongthuc/pdf"
)

// Result is the outcome of extraction.
type Result struct {
	Text      string
	CharCount int
	PageCount int
}

// Errors returned by this package, each mapped to a distinct candidate-facing message
// because "we could not read your CV" is not actionable and "this looks like a scanned
// image, please export from Word" is.
var (
	// ErrNoText means the document opened but contains no machine-readable text,
	// almost always a scan.
	ErrNoText = errors.New("document contains no extractable text")
	// ErrEncrypted means the document is password-protected.
	ErrEncrypted = errors.New("document is encrypted")
	// ErrCorrupt means the document could not be opened.
	ErrCorrupt = errors.New("document could not be opened")
	// ErrTooManyPages means the document exceeds the page limit.
	ErrTooManyPages = errors.New("document has too many pages")
	// ErrUnsupported means the content type is not one we extract.
	ErrUnsupported = errors.New("unsupported document type")
)

// Limits bound extraction work.
type Limits struct {
	// MaxPages rejects a document longer than a CV plausibly is. A 500-page upload is
	// either not a CV or an attempt to exhaust the worker.
	MaxPages int
	// MaxChars truncates rather than rejects. A very long CV is unusual but legitimate,
	// and the AI call has its own token budget; truncating keeps one odd document from
	// consuming a month's allowance.
	MaxChars int
}

// DefaultLimits are the production limits.
func DefaultLimits() Limits {
	return Limits{MaxPages: 20, MaxChars: 200_000}
}

// Extract pulls text from a document, dispatching on content type.
func Extract(content []byte, contentType string, limits Limits) (Result, error) {
	if limits.MaxPages <= 0 {
		limits.MaxPages = DefaultLimits().MaxPages
	}
	if limits.MaxChars <= 0 {
		limits.MaxChars = DefaultLimits().MaxChars
	}

	switch {
	case strings.HasPrefix(contentType, "application/pdf"):
		return extractPDF(content, limits)
	case strings.Contains(contentType, "wordprocessingml.document"):
		return extractDOCX(content, limits)
	default:
		return Result{}, fmt.Errorf("%w: %s", ErrUnsupported, contentType)
	}
}

// extractPDF reads text from a PDF.
//
// The library panics on some malformed files rather than returning an error, so the
// call is wrapped. A panicking worker on a hostile upload would be a denial-of-service
// vector: anyone could restart a pod by uploading a crafted PDF.
func extractPDF(content []byte, limits Limits) (result Result, err error) {
	defer func() {
		if rec := recover(); rec != nil {
			result = Result{}
			err = fmt.Errorf("%w: %v", ErrCorrupt, rec)
		}
	}()

	reader, rerr := pdf.NewReader(bytes.NewReader(content), int64(len(content)))
	if rerr != nil {
		if isEncryptionError(rerr) {
			return Result{}, ErrEncrypted
		}
		return Result{}, fmt.Errorf("%w: %v", ErrCorrupt, rerr)
	}

	pages := reader.NumPage()
	if pages <= 0 {
		return Result{}, ErrCorrupt
	}
	if pages > limits.MaxPages {
		return Result{}, fmt.Errorf("%w: %d pages (limit %d)", ErrTooManyPages, pages, limits.MaxPages)
	}

	var builder strings.Builder
	extractedAny := false

	for i := 1; i <= pages; i++ {
		page := reader.Page(i)
		if page.V.IsNull() {
			continue
		}

		// Per-page recovery: one damaged page must not lose the rest of the document.
		// A CV whose third page fails is still mostly usable, and discarding all of it
		// would be a worse outcome than a partial profile the candidate can correct.
		text, perr := extractPageText(page)
		if perr != nil {
			continue
		}
		if strings.TrimSpace(text) != "" {
			extractedAny = true
		}
		builder.WriteString(text)
		builder.WriteString("\n")

		if builder.Len() > limits.MaxChars {
			break
		}
	}

	if !extractedAny {
		return Result{PageCount: pages}, ErrNoText
	}

	text := normaliseWhitespace(builder.String())
	if len(text) > limits.MaxChars {
		text = truncateAtRune(text, limits.MaxChars)
	}

	return Result{
		Text:      text,
		CharCount: len([]rune(text)),
		PageCount: pages,
	}, nil
}

func extractPageText(page pdf.Page) (text string, err error) {
	defer func() {
		if rec := recover(); rec != nil {
			text = ""
			err = fmt.Errorf("page extraction panicked: %v", rec)
		}
	}()

	// GetPlainText with a nil font map uses the page's own fonts, which preserves word
	// spacing better than reading the content stream directly.
	content, cerr := page.GetPlainText(nil)
	if cerr != nil {
		return "", cerr
	}
	return content, nil
}

func isEncryptionError(err error) bool {
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "encrypt") ||
		strings.Contains(msg, "password") ||
		strings.Contains(msg, "permission")
}

// docxBody mirrors the parts of word/document.xml that carry text.
//
// Hand-rolled rather than using a DOCX library: the structure needed is small, and a
// dependency that parses the full OOXML surface is a much larger attack surface for a
// file arriving from the internet.
type docxBody struct {
	Paragraphs []docxParagraph `xml:"body>p"`
}

type docxParagraph struct {
	Runs []docxRun `xml:"r"`
	// Text inside a table cell lives deeper in the tree. Many CVs are laid out as
	// tables, so ignoring these would silently lose most of the content.
	Tables []docxTable `xml:"tbl"`
}

type docxRun struct {
	Text []string `xml:"t"`
	// Tabs and breaks are structural, so they are captured to preserve the shape of a
	// bulleted list rather than running every line together.
	Breaks []struct{} `xml:"br"`
	Tabs   []struct{} `xml:"tab"`
}

type docxTable struct {
	Rows []struct {
		Cells []struct {
			Paragraphs []struct {
				Runs []docxRun `xml:"r"`
			} `xml:"p"`
		} `xml:"tc"`
	} `xml:"tr"`
}

// maxDOCXEntrySize bounds a single decompressed entry.
//
// A ZIP entry declares its uncompressed size, and a malicious one can declare a small
// compressed size and expand enormously. Bounding the read is what stops a zip bomb
// from exhausting the worker's memory.
const maxDOCXEntrySize = 32 << 20 // 32MiB

// extractDOCX reads text from a DOCX.
func extractDOCX(content []byte, limits Limits) (Result, error) {
	reader, err := zip.NewReader(bytes.NewReader(content), int64(len(content)))
	if err != nil {
		return Result{}, fmt.Errorf("%w: %v", ErrCorrupt, err)
	}

	var documentXML []byte
	for _, file := range reader.File {
		if file.Name != "word/document.xml" {
			continue
		}

		rc, oerr := file.Open()
		if oerr != nil {
			return Result{}, fmt.Errorf("%w: %v", ErrCorrupt, oerr)
		}

		// LimitReader rather than trusting the declared uncompressed size.
		documentXML, err = io.ReadAll(io.LimitReader(rc, maxDOCXEntrySize))
		_ = rc.Close()
		if err != nil {
			return Result{}, fmt.Errorf("%w: %v", ErrCorrupt, err)
		}
		break
	}

	if documentXML == nil {
		// A ZIP without word/document.xml is not a DOCX. The upload sniffer should
		// have caught this, so reaching here means either a crafted file or a format
		// we do not understand.
		return Result{}, fmt.Errorf("%w: no word/document.xml", ErrCorrupt)
	}

	var body docxBody
	decoder := xml.NewDecoder(bytes.NewReader(documentXML))
	// OOXML uses namespaces this struct does not declare; a permissive decoder matches
	// on local names instead of failing.
	decoder.Strict = false
	if err := decoder.Decode(&body); err != nil {
		return Result{}, fmt.Errorf("%w: %v", ErrCorrupt, err)
	}

	var builder strings.Builder
	for _, paragraph := range body.Paragraphs {
		writeRuns(&builder, paragraph.Runs)

		for _, table := range paragraph.Tables {
			for _, row := range table.Rows {
				for _, cell := range row.Cells {
					for _, p := range cell.Paragraphs {
						writeRuns(&builder, p.Runs)
						builder.WriteString("\t")
					}
				}
				builder.WriteString("\n")
			}
		}

		builder.WriteString("\n")
		if builder.Len() > limits.MaxChars*2 {
			break
		}
	}

	text := normaliseWhitespace(builder.String())
	if strings.TrimSpace(text) == "" {
		return Result{}, ErrNoText
	}
	if len(text) > limits.MaxChars {
		text = truncateAtRune(text, limits.MaxChars)
	}

	return Result{
		Text:      text,
		CharCount: len([]rune(text)),
		// DOCX has no reliable page count without rendering; the page concept does not
		// exist in the markup. Reported as zero rather than guessed, so a caller
		// cannot mistake a fabricated number for a real one.
		PageCount: 0,
	}, nil
}

func writeRuns(builder *strings.Builder, runs []docxRun) {
	for _, run := range runs {
		for _, t := range run.Text {
			builder.WriteString(t)
		}
		for range run.Tabs {
			builder.WriteString("\t")
		}
		for range run.Breaks {
			builder.WriteString("\n")
		}
	}
}

// normaliseWhitespace collapses runs of blank lines and trailing spaces.
//
// Worth doing before the text reaches a model: PDF extraction produces a great deal of
// incidental whitespace, and tokens spent on blank lines are tokens not spent on the
// candidate's experience.
func normaliseWhitespace(s string) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\r", "\n")

	// Strip characters that carry no meaning but do consume tokens and can confuse a
	// tokeniser: zero-width spaces, soft hyphens, replacement characters.
	s = strings.Map(func(r rune) rune {
		if isInvisible(r) {
			return -1
		}
		if unicode.IsControl(r) && r != '\n' && r != '\t' {
			return -1
		}
		return r
	}, s)

	lines := strings.Split(s, "\n")
	out := make([]string, 0, len(lines))
	blankRun := 0
	for _, line := range lines {
		trimmed := strings.TrimRight(line, " \t")
		if strings.TrimSpace(trimmed) == "" {
			blankRun++
			// Keep at most one blank line: paragraph structure is meaningful, a run of
			// twelve blank lines is not.
			if blankRun > 1 {
				continue
			}
			out = append(out, "")
			continue
		}
		blankRun = 0
		out = append(out, trimmed)
	}

	return strings.TrimSpace(strings.Join(out, "\n"))
}

// invisibleRunes carry no meaning but do consume tokens, and some of them confuse a
// tokeniser or a downstream renderer. Written as numeric values rather than literals
// because a literal byte order mark is not legal in Go source outside the first byte of
// a file.
var invisibleRunes = map[rune]struct{}{
	0x200B: {}, // zero-width space
	0x200C: {}, // zero-width non-joiner
	0x200D: {}, // zero-width joiner
	0x200E: {}, // left-to-right mark
	0x200F: {}, // right-to-left mark
	0x00AD: {}, // soft hyphen, which PDF extraction scatters through hyphenated words
	0xFEFF: {}, // byte order mark
	0xFFFD: {}, // replacement character, i.e. something already decoded badly
}

// isInvisible reports whether a rune should be stripped before the text is stored or
// sent to a model.
func isInvisible(r rune) bool {
	_, found := invisibleRunes[r]
	return found
}

// truncateAtRune cuts to at most n bytes without splitting a multi-byte character,
// which would otherwise leave an invalid UTF-8 sequence at the boundary.
func truncateAtRune(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !isRuneStart(s[n]) {
		n--
	}
	return s[:n]
}

func isRuneStart(b byte) bool { return b&0xC0 != 0x80 }
