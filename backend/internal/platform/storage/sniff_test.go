package storage_test

import (
	"bytes"
	"testing"

	"github.com/Joakim-animate90/myjobfyt/backend/internal/platform/storage"
)

// Realistic file headers. These are the actual leading bytes of each format, because a
// detector tested only against synthetic input is a detector that works only on
// synthetic input.
func pdfHeader() []byte {
	return []byte("%PDF-1.7\n%\xe2\xe3\xcf\xd3\n1 0 obj\n<< /Type /Catalog >>\nendobj\n")
}

func docxHeader() []byte {
	// A real DOCX begins with a ZIP local-file header for [Content_Types].xml, with
	// the word/ directory entries appearing shortly after.
	h := []byte{0x50, 0x4B, 0x03, 0x04, 0x14, 0x00, 0x06, 0x00, 0x08, 0x00}
	h = append(h, []byte("[Content_Types].xml")...)
	h = append(h, bytes.Repeat([]byte{0x00}, 40)...)
	h = append(h, []byte("word/document.xml")...)
	h = append(h, []byte("wordprocessingml")...)
	return h
}

func plainZipHeader() []byte {
	// An ordinary archive: correct ZIP signature, no OOXML word markers.
	h := []byte{0x50, 0x4B, 0x03, 0x04, 0x14, 0x00, 0x00, 0x00, 0x08, 0x00}
	h = append(h, []byte("holiday-photos/img001.jpg")...)
	h = append(h, bytes.Repeat([]byte{0xFF}, 60)...)
	return h
}

func TestDetectContentTypeAcceptsRealFormats(t *testing.T) {
	if ct, ok := storage.DetectContentType(pdfHeader()); !ok || ct != storage.ContentTypePDF {
		t.Errorf("PDF detected as (%q, %v), want application/pdf", ct, ok)
	}
	if ct, ok := storage.DetectContentType(docxHeader()); !ok || ct != storage.ContentTypeDOCX {
		t.Errorf("DOCX detected as (%q, %v), want the OOXML type", ct, ok)
	}
}

func TestDetectContentTypeRejectsAPlainZipRenamedAsDocx(t *testing.T) {
	// The attack this check exists for: a ZIP signature is shared by every OOXML
	// format, every JAR, every APK and every ordinary archive, so the signature alone
	// is not evidence of a DOCX. A ZIP bomb named CV.docx must not reach the parser.
	if ct, ok := storage.DetectContentType(plainZipHeader()); ok {
		t.Fatalf("a plain ZIP was accepted as %q", ct)
	}
}

func TestDetectContentTypeRejectsHostileAndIrrelevantInput(t *testing.T) {
	cases := map[string][]byte{
		"empty":                  {},
		"nil":                    nil,
		"plain text":             []byte("Dear hiring manager, I am writing to apply..."),
		"html":                   []byte("<!doctype html><html><body>CV</body></html>"),
		"png":                    {0x89, 0x50, 0x4E, 0x47, 0x0D, 0x0A, 0x1A, 0x0A},
		"jpeg":                   {0xFF, 0xD8, 0xFF, 0xE0},
		"gzip":                   {0x1F, 0x8B, 0x08},
		"elf executable":         {0x7F, 0x45, 0x4C, 0x46, 0x02, 0x01, 0x01},
		"windows executable":     {0x4D, 0x5A, 0x90, 0x00},
		"shell script":           []byte("#!/bin/sh\nrm -rf /\n"),
		"empty zip archive":      {0x50, 0x4B, 0x05, 0x06},
		"spanned zip archive":    {0x50, 0x4B, 0x07, 0x08},
		"rtf":                    []byte(`{\rtf1\ansi`),
		"old doc":                {0xD0, 0xCF, 0x11, 0xE0, 0xA1, 0xB1, 0x1A, 0xE1},
		"pdf marker buried deep": append(bytes.Repeat([]byte{0x41}, 200), pdfHeader()...),
	}
	for name, header := range cases {
		t.Run(name, func(t *testing.T) {
			if ct, ok := storage.DetectContentType(header); ok {
				t.Fatalf("%s was accepted as %q", name, ct)
			}
		})
	}
}

func TestDetectContentTypeToleratesALeadingBOMOrWhitespace(t *testing.T) {
	// Real PDFs in the wild sometimes carry a few leading bytes before the signature,
	// and readers accept them. Rejecting those would fail files that parse perfectly
	// well downstream.
	for _, prefix := range [][]byte{
		{0xEF, 0xBB, 0xBF}, // UTF-8 BOM
		[]byte("\n\n"),     // stray newlines
		[]byte("   "),      // whitespace
	} {
		header := append(append([]byte{}, prefix...), pdfHeader()...)
		if ct, ok := storage.DetectContentType(header); !ok || ct != storage.ContentTypePDF {
			t.Errorf("PDF with a %d-byte prefix detected as (%q, %v)", len(prefix), ct, ok)
		}
	}
}

func TestContentTypeExtensions(t *testing.T) {
	if got := storage.ContentTypePDF.Extension(); got != "pdf" {
		t.Errorf("PDF extension = %q", got)
	}
	if got := storage.ContentTypeDOCX.Extension(); got != "docx" {
		t.Errorf("DOCX extension = %q", got)
	}
	if got := storage.ContentType("application/octet-stream").Extension(); got != "bin" {
		t.Errorf("unknown extension = %q, want bin", got)
	}
}
