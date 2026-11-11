package storage

import "bytes"

// sniffLength is how many leading bytes are examined to identify a document. 512 covers
// every signature we accept, including the ZIP local-file header plus the OOXML content
// type marker that distinguishes a DOCX from any other ZIP.
const sniffLength = 512

// pdfMagic is the PDF file signature.
var pdfMagic = []byte("%PDF-")

// zipMagic is the ZIP local-file header, which a DOCX also begins with.
var zipMagic = []byte{0x50, 0x4B, 0x03, 0x04}

// zipEmptyMagic and zipSpannedMagic are the empty-archive and spanned-archive headers.
// A DOCX is never either, but recognising them lets us reject a ZIP explicitly rather
// than reporting an unidentifiable file.
var (
	zipEmptyMagic   = []byte{0x50, 0x4B, 0x05, 0x06}
	zipSpannedMagic = []byte{0x50, 0x4B, 0x07, 0x08}
)

// docxMarkers are strings that appear in the first bytes of a DOCX's ZIP directory.
//
// Checking for them is what stops any ZIP file — including one renamed to .docx, or a
// ZIP bomb — from being accepted as a document. A bare ZIP signature is not sufficient
// evidence, because the signature is shared by every OOXML format, every JAR, every APK
// and every ordinary archive.
var docxMarkers = [][]byte{
	[]byte("word/"),
	[]byte("wordprocessingml"),
}

// DetectContentType identifies a document from its leading bytes.
//
// Byte inspection rather than the file extension or the client-supplied MIME type: both
// of those are attacker-controlled, and trusting either is how a payload reaches a
// parser that was not expecting it.
func DetectContentType(header []byte) (ContentType, bool) {
	if len(header) == 0 {
		return "", false
	}

	// A PDF may carry leading whitespace or junk before the signature; readers tolerate
	// it, so a strict prefix check would reject files that parse fine downstream.
	if idx := bytes.Index(header, pdfMagic); idx >= 0 && idx < 32 {
		return ContentTypePDF, true
	}

	if bytes.HasPrefix(header, zipMagic) {
		for _, marker := range docxMarkers {
			if bytes.Contains(header, marker) {
				return ContentTypeDOCX, true
			}
		}
		// A ZIP without the OOXML word markers is not a DOCX. Rejecting it here is the
		// whole point of this check.
		return "", false
	}

	if bytes.HasPrefix(header, zipEmptyMagic) || bytes.HasPrefix(header, zipSpannedMagic) {
		return "", false
	}

	return "", false
}
