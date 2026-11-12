package mailer

import (
	"context"
	"fmt"
	"html"
	"log/slog"
	"strings"
	"time"
)

// CVMailer composes the CV domain's transactional messages.
type CVMailer struct {
	sender  Sender
	baseURL string
	log     *slog.Logger
}

// NewCVMailer builds the CV mailer.
func NewCVMailer(sender Sender, baseURL string, log *slog.Logger) *CVMailer {
	return &CVMailer{sender: sender, baseURL: strings.TrimRight(baseURL, "/"), log: log}
}

// ParseFailureNotice is what the candidate needs to be told.
//
// The explanation is composed by the CV domain, which owns the failure reasons. This
// package deliberately does not import that domain: a platform package that depended on
// a domain would invert the layering, and the copy is the domain's business anyway.
type ParseFailureNotice struct {
	To string
	// Explanation is the candidate-facing sentence for the failure, already chosen by
	// the domain. Plain prose, never an internal error string.
	Explanation string
	// Retryable reports whether the platform will try again by itself. It changes the
	// instruction from "upload a different file" to "you need do nothing", which is the
	// difference between a useful message and an alarming one.
	Retryable bool
}

// SendParseFailedEmail tells a candidate their CV could not be processed, and what to do.
//
// Deliberately contains nothing extracted from the document: not a filename, not a
// snippet, not a page count. A CV is personal data and an email is the least controlled
// channel it could travel through, so this message says only what went wrong and what
// to do about it.
func (m *CVMailer) SendParseFailedEmail(ctx context.Context, notice ParseFailureNotice) error {
	explanation := strings.TrimSpace(notice.Explanation)
	if explanation == "" {
		explanation = "We could not process this CV."
	}

	var nextStep, nextStepHTML string
	if notice.Retryable {
		nextStep = "You do not need to do anything — we will try again automatically. " +
			"If you would rather not wait, you can enter your details manually."
		nextStepHTML = nextStep
	} else {
		uploadURL := m.baseURL + "/cv"
		nextStep = fmt.Sprintf("You can upload a different file here:\n\n%s", uploadURL)
		nextStepHTML = fmt.Sprintf(
			`<a href="%s" style="display:inline-block;padding:10px 18px;background:#4f46e5;color:#fff;border-radius:6px;text-decoration:none">Upload a different CV</a>`,
			html.EscapeString(uploadURL))
	}

	text := fmt.Sprintf(`We could not process your CV.

%s

%s

Your other CVs and your profile are unaffected.
`, explanation, nextStep)

	htmlBody := fmt.Sprintf(`<!doctype html>
<html><body style="font-family:system-ui,-apple-system,sans-serif;line-height:1.5;color:#1f2937">
<h2 style="margin:0 0 16px">We could not process your CV</h2>
<p>%s</p>
<p>%s</p>
<p style="color:#6b7280;font-size:14px">Your other CVs and your profile are unaffected.</p>
</body></html>`, html.EscapeString(explanation), nextStepHTML)

	sendCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	return m.sender.Send(sendCtx, Message{
		To:       notice.To,
		Subject:  "We could not process your CV",
		TextBody: text,
		HTMLBody: htmlBody,
	})
}
