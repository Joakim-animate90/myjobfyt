// Package pdfgen renders a document to PDF from structured content.
//
// The design answers MJF-012's security requirements by construction rather than by
// sandboxing. An HTML-to-PDF engine is a browser: it resolves URLs, loads stylesheets
// and images, and understands file:// — so rendering candidate-controlled content with
// one means defending against SSRF and local file inclusion, and the defence is a
// container that has to stay locked down for ever.
//
// This renderer has no concept of a URL, a file path or a stylesheet. It takes typed
// content and writes PDF operators. There is no fetch to block, no file to read and
// nothing for an injected string to reference, so "the renderer has no network access
// and cannot read local files" is a property of the code rather than of its deployment.
//
// The trade-off is real and worth stating: templates are Go code, so a designer cannot
// contribute one without an engineer. That is the price of removing the attack surface
// entirely, and for a document whose whole job is to be parsed as text it is a price
// worth paying.
package pdfgen

import (
	"bytes"
	"fmt"
	"strings"
)

// Page geometry in PostScript points, A4.
const (
	pageWidth  = 595.28
	pageHeight = 841.89

	// marginLeft and friends keep text clear of the edges. Generous, because an ATS
	// that crops margins loses less, and a human reader needs the whitespace.
	marginLeft   = 56.0
	marginRight  = 56.0
	marginTop    = 56.0
	marginBottom = 56.0
)

// contentWidth is the usable width for text.
const contentWidth = pageWidth - marginLeft - marginRight

// Block is one piece of document content.
//
// A small closed set rather than arbitrary markup. Every block lays out as a full-width
// run of text in a single column, which is what makes the output ATS-parseable: a
// parser reads it top to bottom in the order a human does.
type Block struct {
	Text string
	Font Font
	Size float64
	// SpaceBefore is extra leading above this block, in points.
	SpaceBefore float64
	// Bullet renders a leading marker and indents wrapped lines to match.
	Bullet bool
	// Rule draws a horizontal line below the block, for section headings.
	Rule bool
}

// Document is the content to render.
type Document struct {
	// Title appears in the PDF's document information. Kept out of the page content.
	Title  string
	Blocks []Block
}

// ErrUnrenderable reports content the base fonts cannot represent.
type ErrUnrenderable struct {
	// Sample is the first offending character, for a message the candidate can act on.
	Sample rune
}

func (e *ErrUnrenderable) Error() string {
	return fmt.Sprintf("the character %q cannot be rendered: this builder supports Latin "+
		"characters only, so a CV containing other scripts must be uploaded as a file instead",
		string(e.Sample))
}

// Render writes the document as a PDF.
//
// Deterministic: the same Document always produces byte-identical output. No creation
// timestamp, no document id, no map iteration — which is what lets a preview and a final
// render be compared byte for byte rather than merely looked at.
func Render(doc Document) ([]byte, error) {
	if err := checkRenderable(doc); err != nil {
		return nil, err
	}

	pages := layout(doc)
	if len(pages) == 0 {
		// An empty document is still a valid PDF. A reader given zero pages errors, and
		// a corrupt file is a worse outcome than a blank one.
		pages = []string{""}
	}

	return assemble(pages), nil
}

// checkRenderable rejects characters the base fonts cannot encode.
//
// Failing loudly rather than substituting. A CV is the document a candidate is judged
// by, and silently replacing their name with question marks is worse than telling them
// this builder cannot set it.
func checkRenderable(doc Document) error {
	for _, block := range doc.Blocks {
		// Asked of the encoder itself rather than guessed from a rune range, so the
		// check and the write can never disagree about what is representable.
		if bad, ok := encodable(block.Text); !ok {
			return &ErrUnrenderable{Sample: bad}
		}
	}
	return nil
}

// layout flows the blocks into pages, returning each page's content stream.
func layout(doc Document) []string {
	var (
		pages   []string
		current strings.Builder
		y       = pageHeight - marginTop
	)

	newPage := func() {
		pages = append(pages, current.String())
		current.Reset()
		y = pageHeight - marginTop
	}

	for _, block := range doc.Blocks {
		size := block.Size
		if size <= 0 {
			size = 10
		}
		font := block.Font
		if font == "" {
			font = FontRegular
		}
		leading := size * 1.35

		y -= block.SpaceBefore

		indent := 0.0
		prefix := ""
		if block.Bullet {
			prefix = "•   "
			indent = TextWidth(font, size, prefix)
		}

		lines := wrap(font, size, block.Text, contentWidth-indent)
		for i, line := range lines {
			if y-leading < marginBottom {
				newPage()
			}
			y -= leading

			x := marginLeft
			text := line
			if block.Bullet {
				if i == 0 {
					text = prefix + line
				} else {
					// Wrapped bullet lines align under the first line's text, not under
					// the marker.
					x += indent
				}
			}
			writeText(&current, font, size, x, y, text)
		}

		if block.Rule {
			if y-6 < marginBottom {
				newPage()
			} else {
				y -= 6
				fmt.Fprintf(&current, "0.6 w 0.4 0.4 0.4 RG %.2f %.2f m %.2f %.2f l S\n",
					marginLeft, y, pageWidth-marginRight, y)
			}
		}
	}

	pages = append(pages, current.String())
	return pages
}

// writeText emits one line of text at a position.
func writeText(b *strings.Builder, font Font, size, x, y float64, text string) {
	if strings.TrimSpace(text) == "" {
		return
	}
	fmt.Fprintf(b, "BT /%s %.2f Tf 1 0 0 1 %.2f %.2f Tm (%s) Tj ET\n",
		font.resourceName(), size, x, y, escape(text))
}

// escape encodes a string and makes it safe inside a PDF literal.
func escape(s string) string {
	// Encoded to WinAnsi first: a PDF string is a byte string read through the font's
	// encoding, so writing UTF-8 renders each byte as its own character — a bullet
	// becomes "â€¢" and "Müller" becomes "MÃ¼ller". Render has already refused anything
	// the encoding cannot represent, so the fallback here is unreachable in practice
	// and exists only so this function has no error path.
	encoded, _, ok := encodeWinAnsi(s)
	if !ok {
		encoded = s
	}

	// Backslash first: escaping it after the parentheses would double-escape the
	// backslashes this function itself introduced.
	encoded = strings.ReplaceAll(encoded, "\\", "\\\\")
	encoded = strings.ReplaceAll(encoded, "(", "\\(")
	encoded = strings.ReplaceAll(encoded, ")", "\\)")
	encoded = strings.ReplaceAll(encoded, "\r", " ")
	return encoded
}

// wrap breaks text into lines that fit a width.
//
// Word wrapping on spaces. A word longer than the line is emitted on its own and
// allowed to overflow rather than being broken mid-word: a hyphenated URL or identifier
// is harder to read, and harder for an ATS to match, than one that runs long.
func wrap(font Font, size float64, text string, width float64) []string {
	var lines []string
	for _, paragraph := range strings.Split(text, "\n") {
		words := strings.Fields(paragraph)
		if len(words) == 0 {
			continue
		}

		line := words[0]
		for _, word := range words[1:] {
			candidate := line + " " + word
			if TextWidth(font, size, candidate) <= width {
				line = candidate
				continue
			}
			lines = append(lines, line)
			line = word
		}
		lines = append(lines, line)
	}
	return lines
}

// assemble writes the PDF file structure around the page content streams.
func assemble(pages []string) []byte {
	var out bytes.Buffer
	// Byte offset of each indirect object, for the cross-reference table. A reader
	// seeks by these, so an off-by-one here produces a file that opens nowhere.
	offsets := []int{}

	object := func(body string) {
		offsets = append(offsets, out.Len())
		fmt.Fprintf(&out, "%d 0 obj\n%s\nendobj\n", len(offsets), body)
	}

	out.WriteString("%PDF-1.7\n")
	// A binary comment marks the file as binary for tools that sniff it.
	out.Write([]byte{'%', 0xE2, 0xE3, 0xCF, 0xD3, '\n'})

	// Object numbering is fixed: 1 catalog, 2 page tree, 3..n+2 pages,
	// then one content stream per page, then the fonts.
	const catalogObj, pagesObj = 1, 2
	firstPageObj := 3
	firstContentObj := firstPageObj + len(pages)
	firstFontObj := firstContentObj + len(pages)

	object(fmt.Sprintf("<< /Type /Catalog /Pages %d 0 R >>", pagesObj))

	kids := make([]string, 0, len(pages))
	for i := range pages {
		kids = append(kids, fmt.Sprintf("%d 0 R", firstPageObj+i))
	}
	object(fmt.Sprintf("<< /Type /Pages /Kids [%s] /Count %d >>",
		strings.Join(kids, " "), len(pages)))

	fontRefs := make([]string, 0, len(allFonts))
	for i, font := range allFonts {
		fontRefs = append(fontRefs, fmt.Sprintf("/%s %d 0 R", font.resourceName(), firstFontObj+i))
	}
	resources := fmt.Sprintf("<< /Font << %s >> >>", strings.Join(fontRefs, " "))

	for i := range pages {
		object(fmt.Sprintf(
			"<< /Type /Page /Parent %d 0 R /MediaBox [0 0 %.2f %.2f] /Resources %s /Contents %d 0 R >>",
			pagesObj, pageWidth, pageHeight, resources, firstContentObj+i))
	}

	for _, content := range pages {
		object(fmt.Sprintf("<< /Length %d >>\nstream\n%s\nendstream", len(content), content))
	}

	for _, font := range allFonts {
		object(fmt.Sprintf(
			"<< /Type /Font /Subtype /Type1 /BaseFont /%s /Encoding /WinAnsiEncoding >>", font))
	}

	// No /Info dictionary. It is where a creation date would live, and a timestamp is
	// the usual reason two renders of identical content differ.
	xrefOffset := out.Len()
	fmt.Fprintf(&out, "xref\n0 %d\n", len(offsets)+1)
	out.WriteString("0000000000 65535 f \n")
	for _, offset := range offsets {
		fmt.Fprintf(&out, "%010d 00000 n \n", offset)
	}
	fmt.Fprintf(&out, "trailer\n<< /Size %d /Root %d 0 R >>\nstartxref\n%d\n%%%%EOF\n",
		len(offsets)+1, catalogObj, xrefOffset)

	return out.Bytes()
}
