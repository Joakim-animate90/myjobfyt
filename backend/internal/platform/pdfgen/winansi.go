package pdfgen

import "strings"

// winAnsiSpecials maps the Unicode characters that WinAnsiEncoding places in 0x80–0x9F.
//
// Latin-1 and WinAnsi agree from 0xA0 upward, so only this window needs a table. It
// matters because a PDF string is a byte string interpreted through the font's encoding:
// writing UTF-8 into it renders the individual bytes as separate characters, so a
// bullet (U+2022) appears as "â€¢" and "Müller" as "MÃ¼ller". Observed in a real
// download before this existed.
var winAnsiSpecials = map[rune]byte{
	'€': 0x80, // euro
	'‚': 0x82, // single low quote
	'ƒ': 0x83, // florin
	'„': 0x84, // double low quote
	'…': 0x85, // ellipsis
	'†': 0x86, // dagger
	'‡': 0x87, // double dagger
	'ˆ': 0x88, // circumflex
	'‰': 0x89, // per mille
	'Š': 0x8A, // S caron
	'‹': 0x8B, // single left guillemet
	'Œ': 0x8C, // OE ligature
	'Ž': 0x8E, // Z caron
	'‘': 0x91, // left single quote
	'’': 0x92, // right single quote (the apostrophe word processors produce)
	'“': 0x93, // left double quote
	'”': 0x94, // right double quote
	'•': 0x95, // bullet
	'–': 0x96, // en dash
	'—': 0x97, // em dash
	'˜': 0x98, // small tilde
	'™': 0x99, // trade mark
	'š': 0x9A, // s caron
	'›': 0x9B, // single right guillemet
	'œ': 0x9C, // oe ligature
	'ž': 0x9E, // z caron
	'Ÿ': 0x9F, // Y diaeresis
}

// encodeWinAnsi converts a Go string to the bytes a WinAnsi-encoded PDF string needs.
//
// Reports false for any character the encoding cannot represent, so the caller can
// refuse rather than silently substituting. A CV is the document someone is judged by,
// and replacing a candidate's name with question marks is worse than saying it cannot
// be set.
func encodeWinAnsi(s string) (string, rune, bool) {
	var b strings.Builder
	b.Grow(len(s))

	for _, r := range s {
		switch {
		case r == '\t':
			b.WriteByte(' ')
		case r < 0x80:
			b.WriteByte(byte(r))
		case r >= 0xA0 && r <= 0xFF:
			// Latin-1 and WinAnsi coincide here.
			b.WriteByte(byte(r))
		default:
			if mapped, ok := winAnsiSpecials[r]; ok {
				b.WriteByte(mapped)
				continue
			}
			return "", r, false
		}
	}
	return b.String(), 0, true
}

// encodable reports whether every character in s can be written.
func encodable(s string) (rune, bool) {
	_, bad, ok := encodeWinAnsi(s)
	return bad, ok
}
