package pdfgen

// Font is one of the PDF base-14 fonts.
//
// Deliberately limited to the base fonts rather than embedding a TrueType file. Every
// PDF reader has them, nothing has to be bundled or licensed, and — the reason that
// matters here — text set in a base font extracts cleanly, which is the whole point of
// generating a CV an applicant tracking system can read.
type Font string

const (
	// FontRegular is Helvetica, the body font.
	FontRegular Font = "Helvetica"
	// FontBold is Helvetica-Bold, for names and headings.
	FontBold Font = "Helvetica-Bold"
	// FontItalic is Helvetica-Oblique, used sparingly: italics extract fine but read
	// poorly at small sizes.
	FontItalic Font = "Helvetica-Oblique"
)

// resourceName is the name this font is given in a page's resource dictionary.
func (f Font) resourceName() string {
	switch f {
	case FontBold:
		return "F2"
	case FontItalic:
		return "F3"
	default:
		return "F1"
	}
}

// allFonts is every font a document declares, in a fixed order so the output is
// byte-identical between runs.
var allFonts = []Font{FontRegular, FontBold, FontItalic}

// defaultWidth is used for any character outside the measured range.
//
// Only affects line wrapping, never correctness: a mis-measured character makes a line
// slightly short or long, it does not corrupt the document.
const defaultWidth = 556

// helveticaWidths holds glyph widths in 1/1000 em for ASCII, from the Adobe metrics.
//
// Needed because wrapping has to know how wide a line is. Approximating would produce
// lines that overflow the page margin, which looks careless on a document someone is
// judged by.
var helveticaWidths = map[rune]int{
	' ': 278, '!': 278, '"': 355, '#': 556, '$': 556, '%': 889, '&': 667, '\'': 191,
	'(': 333, ')': 333, '*': 389, '+': 584, ',': 278, '-': 333, '.': 278, '/': 278,
	'0': 556, '1': 556, '2': 556, '3': 556, '4': 556, '5': 556, '6': 556, '7': 556,
	'8': 556, '9': 556, ':': 278, ';': 278, '<': 584, '=': 584, '>': 584, '?': 556,
	'@': 1015, 'A': 667, 'B': 667, 'C': 722, 'D': 722, 'E': 667, 'F': 611, 'G': 778,
	'H': 722, 'I': 278, 'J': 500, 'K': 667, 'L': 556, 'M': 833, 'N': 722, 'O': 778,
	'P': 667, 'Q': 778, 'R': 722, 'S': 667, 'T': 611, 'U': 722, 'V': 667, 'W': 944,
	'X': 667, 'Y': 667, 'Z': 611, '[': 278, '\\': 278, ']': 278, '^': 469, '_': 556,
	'`': 333, 'a': 556, 'b': 556, 'c': 500, 'd': 556, 'e': 556, 'f': 278, 'g': 556,
	'h': 556, 'i': 222, 'j': 222, 'k': 500, 'l': 222, 'm': 833, 'n': 556, 'o': 556,
	'p': 556, 'q': 556, 'r': 333, 's': 500, 't': 278, 'u': 556, 'v': 500, 'w': 722,
	'x': 500, 'y': 500, 'z': 500, '{': 334, '|': 260, '}': 334, '~': 584,
}

// helveticaBoldWidths holds the same for Helvetica-Bold.
var helveticaBoldWidths = map[rune]int{
	' ': 278, '!': 333, '"': 474, '#': 556, '$': 556, '%': 889, '&': 722, '\'': 238,
	'(': 333, ')': 333, '*': 389, '+': 584, ',': 278, '-': 333, '.': 278, '/': 278,
	'0': 556, '1': 556, '2': 556, '3': 556, '4': 556, '5': 556, '6': 556, '7': 556,
	'8': 556, '9': 556, ':': 333, ';': 333, '<': 584, '=': 584, '>': 584, '?': 611,
	'@': 975, 'A': 722, 'B': 722, 'C': 722, 'D': 722, 'E': 667, 'F': 611, 'G': 778,
	'H': 722, 'I': 278, 'J': 556, 'K': 722, 'L': 611, 'M': 833, 'N': 722, 'O': 778,
	'P': 667, 'Q': 778, 'R': 722, 'S': 667, 'T': 611, 'U': 722, 'V': 667, 'W': 944,
	'X': 667, 'Y': 667, 'Z': 611, '[': 333, '\\': 278, ']': 333, '^': 584, '_': 556,
	'`': 333, 'a': 556, 'b': 611, 'c': 556, 'd': 611, 'e': 556, 'f': 333, 'g': 611,
	'h': 611, 'i': 278, 'j': 278, 'k': 556, 'l': 278, 'm': 889, 'n': 611, 'o': 611,
	'p': 611, 'q': 611, 'r': 389, 's': 556, 't': 333, 'u': 611, 'v': 556, 'w': 778,
	'x': 556, 'y': 556, 'z': 500, '{': 389, '|': 280, '}': 389, '~': 584,
}

// widthOf returns a glyph's width in 1/1000 em.
func widthOf(f Font, r rune) int {
	table := helveticaWidths
	if f == FontBold {
		table = helveticaBoldWidths
	}
	if w, ok := table[r]; ok {
		return w
	}
	return defaultWidth
}

// TextWidth measures a string at a given size, in points.
func TextWidth(f Font, size float64, s string) float64 {
	var total int
	for _, r := range s {
		total += widthOf(f, r)
	}
	return float64(total) * size / 1000
}
