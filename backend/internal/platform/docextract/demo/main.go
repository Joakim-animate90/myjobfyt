//go:build demo

// Command demo prints what docextract recovers from a document, so extraction quality
// can be eyeballed against a real CV rather than only asserted in tests.
//
// Behind a build tag so it is never compiled into a release binary.
//
//	go run -tags=demo ./internal/platform/docextract/demo <file> [content-type]
package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/Joakim-animate90/myjobfyt/backend/internal/platform/docextract"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: demo <file> [content-type]")
		os.Exit(2)
	}

	path := os.Args[1]
	contentType := ""
	if len(os.Args) > 2 {
		contentType = os.Args[2]
	} else {
		switch strings.ToLower(filepath.Ext(path)) {
		case ".pdf":
			contentType = "application/pdf"
		case ".docx":
			contentType = "application/vnd.openxmlformats-officedocument.wordprocessingml.document"
		}
	}

	content, err := os.ReadFile(path)
	if err != nil {
		fmt.Fprintln(os.Stderr, "read:", err)
		os.Exit(1)
	}

	result, err := docextract.Extract(content, contentType, docextract.DefaultLimits())
	if err != nil {
		fmt.Printf("FAILED: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("%d chars, %d pages\n%s\n%s\n", result.CharCount, result.PageCount,
		strings.Repeat("-", 60), result.Text)
}
