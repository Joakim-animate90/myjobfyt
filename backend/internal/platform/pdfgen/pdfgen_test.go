package pdfgen_test

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/Joakim-animate90/myjobfyt/backend/internal/platform/docextract"
	"github.com/Joakim-animate90/myjobfyt/backend/internal/platform/pdfgen"
)

func sampleDoc() pdfgen.Document {
	return pdfgen.Document{
		Title: "Joakim Candidate — CV",
		Blocks: []pdfgen.Block{
			{Text: "Joakim Candidate", Font: pdfgen.FontBold, Size: 20},
			{Text: "joakim@example.com | +254 700 000000 | Nairobi, Kenya", Size: 10, SpaceBefore: 4},
			{Text: "EXPERIENCE", Font: pdfgen.FontBold, Size: 11, SpaceBefore: 18, Rule: true},
			{Text: "Senior Backend Engineer, Acme Corporation", Font: pdfgen.FontBold, Size: 11, SpaceBefore: 10},
			{Text: "Jan 2021 - Present", Font: pdfgen.FontItalic, Size: 9},
			{Text: "Cut checkout latency by 35 percent across the payments platform.", Size: 10, Bullet: true},
			{Text: "Led a team of four engineers through a migration to Go.", Size: 10, Bullet: true},
		},
	}
}

// extractText reads the text back out of a generated PDF, which is the closest
// available stand-in for what an applicant tracking system does to it.
func extractText(t *testing.T, pdf []byte) string {
	t.Helper()
	result, err := docextract.Extract(pdf, "application/pdf", docextract.DefaultLimits())
	if err != nil {
		t.Fatalf("the generated PDF could not be parsed: %v", err)
	}
	return result.Text
}

func TestGeneratedTextExtractsBackOut(t *testing.T) {
	// The entire point of the builder. A CV that renders beautifully and extracts as
	// nothing is rejected by an ATS before a human sees it, so this is the acceptance
	// test that matters most.
	pdf, err := pdfgen.Render(sampleDoc())
	if err != nil {
		t.Fatalf("Render: %v", err)
	}

	text := extractText(t, pdf)
	for _, want := range []string{
		"Joakim Candidate",
		"joakim@example.com",
		"Senior Backend Engineer",
		"Acme Corporation",
		"Jan 2021",
		"checkout latency",
		"team of four engineers",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("extracted text is missing %q\n--- extracted ---\n%s", want, text)
		}
	}
}

func TestContentExtractsInReadingOrder(t *testing.T) {
	// Single column, top to bottom. A multi-column layout extracts interleaved, which
	// is how a CV ends up matched against the wrong employer for the wrong dates.
	pdf, err := pdfgen.Render(sampleDoc())
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	text := extractText(t, pdf)

	name := strings.Index(text, "Joakim Candidate")
	heading := strings.Index(text, "EXPERIENCE")
	role := strings.Index(text, "Senior Backend Engineer")
	bullet := strings.Index(text, "checkout latency")

	if name < 0 || heading < 0 || role < 0 || bullet < 0 {
		t.Fatalf("not all content was extracted: %q", text)
	}
	if !(name < heading && heading < role && role < bullet) {
		t.Errorf("content did not extract in reading order (name %d, heading %d, role %d, bullet %d)",
			name, heading, role, bullet)
	}
}

func TestRenderIsByteIdenticalForTheSameInput(t *testing.T) {
	// MJF-012 requires preview and final output to be byte-identical. That is only
	// possible if rendering is deterministic, which rules out a creation timestamp or a
	// document id — the usual reasons two renders of the same content differ.
	first, err := pdfgen.Render(sampleDoc())
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	for i := 0; i < 20; i++ {
		next, err := pdfgen.Render(sampleDoc())
		if err != nil {
			t.Fatalf("Render: %v", err)
		}
		if !bytes.Equal(first, next) {
			t.Fatalf("render %d differed from the first", i)
		}
	}
}

func TestTheOutputCarriesNoTimestamp(t *testing.T) {
	// Specifically: no /CreationDate anywhere. A timestamp is both a determinism
	// breaker and a small privacy leak about when a candidate was job hunting.
	pdf, err := pdfgen.Render(sampleDoc())
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	for _, forbidden := range []string{"/CreationDate", "/ModDate", "/Producer", "/Info"} {
		if bytes.Contains(pdf, []byte(forbidden)) {
			t.Errorf("the PDF contains %s, which breaks byte-identical rendering", forbidden)
		}
	}
}

func TestLongContentFlowsOntoFurtherPages(t *testing.T) {
	doc := pdfgen.Document{Title: "Long"}
	for i := 0; i < 120; i++ {
		doc.Blocks = append(doc.Blocks, pdfgen.Block{
			Text: "Delivered a substantial piece of work with a measurable outcome attached to it.",
			Size: 10, Bullet: true,
		})
	}

	pdf, err := pdfgen.Render(doc)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	result, err := docextract.Extract(pdf, "application/pdf", docextract.DefaultLimits())
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if result.PageCount < 2 {
		t.Errorf("120 bullets produced %d page(s); content is being written off the page",
			result.PageCount)
	}
}

func TestLongWordsDoNotOverflowSilently(t *testing.T) {
	// A long URL or identifier must still render and extract, even though it cannot be
	// wrapped.
	doc := pdfgen.Document{Blocks: []pdfgen.Block{
		{Text: "https://example.com/a/very/long/path/that/cannot/be/broken/into/pieces/at/all", Size: 10},
	}}
	pdf, err := pdfgen.Render(doc)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if !strings.Contains(extractText(t, pdf), "example.com") {
		t.Error("a long unbreakable word was lost")
	}
}

func TestParenthesesAndBackslashesSurviveEscaping(t *testing.T) {
	// Unescaped, a bracket closes the PDF string early and corrupts every object after
	// it. CVs contain brackets constantly — "(contract)", "C:\\builds" in a tools list.
	doc := pdfgen.Document{Blocks: []pdfgen.Block{
		{Text: `Senior Engineer (contract) \ Payments (EU) \\ and (nested (parens))`, Size: 10},
	}}

	pdf, err := pdfgen.Render(doc)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	text := extractText(t, pdf)
	if !strings.Contains(text, "(contract)") {
		t.Errorf("escaped parentheses did not survive: %q", text)
	}
	if !strings.Contains(text, "(nested (parens))") {
		t.Errorf("nested parentheses did not survive: %q", text)
	}
}

func TestAnEmptyDocumentIsStillAStructurallyValidPDF(t *testing.T) {
	// A reader given zero pages errors, and handing a candidate a corrupt file is worse
	// than handing them a blank one.
	//
	// Extraction still refuses it, but for the right reason: ErrNoText is the rule that
	// catches scanned images, and a blank page genuinely has no text. Reaching that
	// verdict at all means the file parsed, which is what this test is about.
	pdf, err := pdfgen.Render(pdfgen.Document{})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}

	if !bytes.HasPrefix(pdf, []byte("%PDF-1.7")) {
		t.Error("the output does not start with a PDF header")
	}
	if !bytes.Contains(pdf, []byte("xref")) || !bytes.HasSuffix(pdf, []byte("%%EOF\n")) {
		t.Error("the output has no cross-reference table or trailer")
	}

	_, err = docextract.Extract(pdf, "application/pdf", docextract.DefaultLimits())
	if !errors.Is(err, docextract.ErrNoText) {
		t.Fatalf("err = %v, want ErrNoText; anything else means the file did not parse", err)
	}
}

func TestUnrenderableCharactersFailLoudly(t *testing.T) {
	// Substituting would put a CV in front of an employer with the candidate's name
	// mangled. Telling them this builder cannot set their name is worse product and
	// better ethics; the error says what to do instead.
	doc := pdfgen.Document{Blocks: []pdfgen.Block{{Text: "候補者 太郎", Size: 12}}}

	_, err := pdfgen.Render(doc)
	if err == nil {
		t.Fatal("a CV containing non-Latin characters rendered anyway")
	}
	var unrenderable *pdfgen.ErrUnrenderable
	if !errorsAs(err, &unrenderable) {
		t.Fatalf("err = %v, want ErrUnrenderable", err)
	}
	if !strings.Contains(err.Error(), "upload") {
		t.Errorf("the error does not tell the candidate what to do instead: %q", err)
	}
}

func TestAccentedNamesSurviveExtraction(t *testing.T) {
	// Rejecting "Zoë Müller" would make the builder unusable across most of Europe, so
	// these characters must render — but asserting only that Render returned no error
	// is the weak test that let a real bug through: the text was written as UTF-8 into
	// a WinAnsi string, so "Müller" extracted as "MÃ¼ller". Found in a live download.
	// This asserts the characters come back out.
	names := "Zoë Müller, José Gutiérrez, Ana Gonçalves, Łukasz Jørgensen"
	doc := pdfgen.Document{Blocks: []pdfgen.Block{{Text: names, Size: 12}}}

	pdf, err := pdfgen.Render(doc)
	if err != nil {
		// Łukasz is outside WinAnsi, so this is expected to be refused — and refused
		// by naming the character, not by mangling the rest.
		var unrenderable *pdfgen.ErrUnrenderable
		if !errorsAs(err, &unrenderable) {
			t.Fatalf("err = %v, want ErrUnrenderable", err)
		}
		if unrenderable.Sample != 'Ł' {
			t.Errorf("the error names %q, want the actual unsupported character", string(unrenderable.Sample))
		}
		// Retry without it; the Latin-1 names must work.
		doc.Blocks[0].Text = "Zoë Müller, José Gutiérrez, Ana Gonçalves"
		pdf, err = pdfgen.Render(doc)
		if err != nil {
			t.Fatalf("Latin-1 accented names were rejected: %v", err)
		}
	}

	text := extractText(t, pdf)
	for _, want := range []string{"Zoë", "Müller", "José", "Gutiérrez", "Gonçalves"} {
		if !strings.Contains(text, want) {
			t.Errorf("%q did not survive rendering; extracted: %q", want, text)
		}
	}
	for _, mangled := range []string{"MÃ¼", "ZoÃ", "JosÃ"} {
		if strings.Contains(text, mangled) {
			t.Errorf("text was written as UTF-8 rather than WinAnsi: found %q", mangled)
		}
	}
}

func TestTypographicCharactersSurviveExtraction(t *testing.T) {
	// Word processors silently produce these: a curly apostrophe in "O'Brien", an en
	// dash in a date range, a bullet. They are in WinAnsi but not in Latin-1's lower
	// half, so they need the special-case table.
	doc := pdfgen.Document{Blocks: []pdfgen.Block{
		{Text: "O\u2019Brien \u2013 2019\u20132021 \u2022 \u201Cquoted\u201D \u2026", Size: 12},
	}}

	pdf, err := pdfgen.Render(doc)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}

	text := extractText(t, pdf)
	for _, mangled := range []string{"â€", "Ã¢"} {
		if strings.Contains(text, mangled) {
			t.Errorf("typographic characters were written as UTF-8: found %q in %q", mangled, text)
		}
	}
	if !strings.Contains(text, "Brien") || !strings.Contains(text, "quoted") {
		t.Errorf("content was lost around the typographic characters: %q", text)
	}
}

func TestBulletsExtractAsBulletsNotMojibake(t *testing.T) {
	// The bug exactly as it appeared in a real download: every bullet read "â€¢".
	doc := pdfgen.Document{Blocks: []pdfgen.Block{
		{Text: "Cut checkout latency by 35 percent", Size: 10, Bullet: true},
	}}

	pdf, err := pdfgen.Render(doc)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	text := extractText(t, pdf)
	if strings.Contains(text, "â€¢") {
		t.Errorf("the bullet marker is mojibake: %q", text)
	}
	if !strings.Contains(text, "Cut checkout latency") {
		t.Errorf("the bullet text was lost: %q", text)
	}
}

func TestTextWidthGrowsWithContent(t *testing.T) {
	// Wrapping depends on this. A measurement that ignored the string would wrap at a
	// fixed character count and overflow the margin on wide text.
	narrow := pdfgen.TextWidth(pdfgen.FontRegular, 10, "iiii")
	wide := pdfgen.TextWidth(pdfgen.FontRegular, 10, "WWWW")
	if !(narrow < wide) {
		t.Errorf("four narrow glyphs measured %.2f, four wide ones %.2f", narrow, wide)
	}
	if pdfgen.TextWidth(pdfgen.FontRegular, 20, "test") <= pdfgen.TextWidth(pdfgen.FontRegular, 10, "test") {
		t.Error("width does not scale with font size")
	}
}

// errorsAs is a tiny shim so the test reads without importing errors for one call.
func errorsAs(err error, target any) bool {
	switch t := target.(type) {
	case **pdfgen.ErrUnrenderable:
		if e, ok := err.(*pdfgen.ErrUnrenderable); ok {
			*t = e
			return true
		}
	}
	return false
}
