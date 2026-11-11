// Package mailer sends transactional email.
//
// Deliberately separate from candidate outreach: the two have different reputation
// profiles, and a spam complaint against a candidate's job application must not
// jeopardise delivery of the password-reset emails everybody depends on.
package mailer

import (
	"context"
	"crypto/tls"
	"fmt"
	"log/slog"
	"net"
	"net/smtp"
	"net/url"
	"strings"
	"time"

	"github.com/Joakim-animate90/myjobfyt/backend/internal/config"
)

// Message is one outbound email.
type Message struct {
	To       string
	Subject  string
	TextBody string
	HTMLBody string
}

// Sender delivers a message.
type Sender interface {
	Send(ctx context.Context, msg Message) error
}

// SMTPSender delivers over SMTP.
type SMTPSender struct {
	cfg config.SMTPConfig
	log *slog.Logger
}

// NewSMTPSender builds the SMTP sender.
func NewSMTPSender(cfg config.SMTPConfig, log *slog.Logger) *SMTPSender {
	return &SMTPSender{cfg: cfg, log: log}
}

// Send delivers a message, bounded by the configured timeout.
func (s *SMTPSender) Send(ctx context.Context, msg Message) error {
	if err := validateRecipient(msg.To); err != nil {
		return err
	}

	addr := net.JoinHostPort(s.cfg.Host, fmt.Sprintf("%d", s.cfg.Port))

	dialer := &net.Dialer{Timeout: s.cfg.Timeout}
	conn, err := dialer.DialContext(ctx, "tcp", addr)
	if err != nil {
		return fmt.Errorf("dial smtp %s: %w", addr, err)
	}
	// A deadline on the connection as well as the dial: an SMTP server that accepts
	// the connection and then stalls would otherwise hold a worker indefinitely.
	if s.cfg.Timeout > 0 {
		_ = conn.SetDeadline(time.Now().Add(s.cfg.Timeout))
	}

	client, err := smtp.NewClient(conn, s.cfg.Host)
	if err != nil {
		_ = conn.Close()
		return fmt.Errorf("smtp handshake: %w", err)
	}
	defer func() { _ = client.Close() }()

	if s.cfg.UseTLS {
		ok, _ := client.Extension("STARTTLS")
		if !ok {
			// Configured to require TLS and the server cannot offer it: refuse rather
			// than silently downgrading to plaintext.
			return fmt.Errorf("smtp server %s does not support STARTTLS but TLS is required", addr)
		}
		if err := client.StartTLS(&tls.Config{ServerName: s.cfg.Host, MinVersion: tls.VersionTLS12}); err != nil {
			return fmt.Errorf("smtp starttls: %w", err)
		}
	}

	if s.cfg.Username != "" {
		if err := client.Auth(smtp.PlainAuth("", s.cfg.Username, s.cfg.Password, s.cfg.Host)); err != nil {
			return fmt.Errorf("smtp auth: %w", err)
		}
	}

	if err := client.Mail(s.cfg.From); err != nil {
		return fmt.Errorf("smtp MAIL FROM: %w", err)
	}
	if err := client.Rcpt(msg.To); err != nil {
		return fmt.Errorf("smtp RCPT TO: %w", err)
	}

	writer, err := client.Data()
	if err != nil {
		return fmt.Errorf("smtp DATA: %w", err)
	}
	if _, err := writer.Write([]byte(s.render(msg))); err != nil {
		return fmt.Errorf("write message body: %w", err)
	}
	if err := writer.Close(); err != nil {
		return fmt.Errorf("close message body: %w", err)
	}

	if err := client.Quit(); err != nil {
		// The message was accepted; a failed QUIT is cosmetic.
		s.log.DebugContext(ctx, "smtp quit failed after a successful send", slog.Any("error", err))
	}
	return nil
}

// validateRecipient rejects anything that could inject SMTP commands or extra headers.
//
// A newline in a recipient or subject is the classic email-header-injection vector: it
// lets an attacker add Bcc recipients or rewrite headers. Rejecting it here, once, is
// safer than trusting every call site to have escaped its input.
func validateRecipient(to string) error {
	if strings.TrimSpace(to) == "" {
		return fmt.Errorf("no recipient")
	}
	if strings.ContainsAny(to, "\r\n") {
		return fmt.Errorf("recipient contains a line break")
	}
	if !strings.Contains(to, "@") {
		return fmt.Errorf("recipient is not an email address")
	}
	return nil
}

// sanitiseHeader strips CR and LF from a header value, for the same reason.
func sanitiseHeader(v string) string {
	v = strings.ReplaceAll(v, "\r", " ")
	v = strings.ReplaceAll(v, "\n", " ")
	if len(v) > 500 {
		v = v[:500]
	}
	return v
}

func (s *SMTPSender) render(msg Message) string {
	boundary := "myjobfyt-boundary-7f3a9c2e"

	var b strings.Builder
	fmt.Fprintf(&b, "From: %s <%s>\r\n", sanitiseHeader(s.cfg.FromName), s.cfg.From)
	fmt.Fprintf(&b, "To: %s\r\n", sanitiseHeader(msg.To))
	fmt.Fprintf(&b, "Subject: %s\r\n", sanitiseHeader(msg.Subject))
	fmt.Fprintf(&b, "Date: %s\r\n", time.Now().UTC().Format(time.RFC1123Z))
	b.WriteString("MIME-Version: 1.0\r\n")
	// Transactional mail must not be auto-replied to or filed as a bulk campaign.
	b.WriteString("Auto-Submitted: auto-generated\r\n")

	if msg.HTMLBody == "" {
		b.WriteString("Content-Type: text/plain; charset=UTF-8\r\n\r\n")
		b.WriteString(msg.TextBody)
		return b.String()
	}

	fmt.Fprintf(&b, "Content-Type: multipart/alternative; boundary=%q\r\n\r\n", boundary)
	fmt.Fprintf(&b, "--%s\r\nContent-Type: text/plain; charset=UTF-8\r\n\r\n%s\r\n", boundary, msg.TextBody)
	fmt.Fprintf(&b, "--%s\r\nContent-Type: text/html; charset=UTF-8\r\n\r\n%s\r\n", boundary, msg.HTMLBody)
	fmt.Fprintf(&b, "--%s--\r\n", boundary)
	return b.String()
}

// IdentityMailer composes the identity domain's transactional messages.
type IdentityMailer struct {
	sender  Sender
	baseURL string
	log     *slog.Logger
}

// NewIdentityMailer builds the identity mailer.
func NewIdentityMailer(sender Sender, baseURL string, log *slog.Logger) *IdentityMailer {
	return &IdentityMailer{sender: sender, baseURL: strings.TrimRight(baseURL, "/"), log: log}
}

// SendVerificationEmail sends the email-verification link.
func (m *IdentityMailer) SendVerificationEmail(to, token string) error {
	// url.QueryEscape, not string concatenation: the token is base64url so it is safe
	// in practice, but a link built by concatenation is one format change away from
	// being broken or injectable.
	link := fmt.Sprintf("%s/auth/verify?token=%s", m.baseURL, url.QueryEscape(token))

	text := fmt.Sprintf(`Welcome to MyJobFyt.

Confirm your email address to finish setting up your account:

%s

This link is valid for 24 hours and can be used once.

If you did not create an account, you can ignore this message.
`, link)

	html := fmt.Sprintf(`<!doctype html>
<html><body style="font-family:system-ui,-apple-system,sans-serif;line-height:1.5;color:#1f2937">
<h2 style="margin:0 0 16px">Welcome to MyJobFyt</h2>
<p>Confirm your email address to finish setting up your account.</p>
<p><a href="%s" style="display:inline-block;padding:10px 18px;background:#4f46e5;color:#fff;border-radius:6px;text-decoration:none">Verify my email</a></p>
<p style="color:#6b7280;font-size:14px">This link is valid for 24 hours and can be used once. If you did not create an account, you can ignore this message.</p>
</body></html>`, link)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	return m.sender.Send(ctx, Message{
		To:       to,
		Subject:  "Confirm your MyJobFyt email address",
		TextBody: text,
		HTMLBody: html,
	})
}

// LoggingSender writes messages to the log instead of sending them.
//
// Used when no SMTP server is configured, so a developer can still complete the
// verification flow by copying the link out of the terminal.
type LoggingSender struct{ log *slog.Logger }

// NewLoggingSender builds the no-op sender.
func NewLoggingSender(log *slog.Logger) *LoggingSender { return &LoggingSender{log: log} }

// Send logs the message.
func (s *LoggingSender) Send(ctx context.Context, msg Message) error {
	s.log.InfoContext(ctx, "email not sent (no SMTP configured); body logged for development",
		slog.String("to", msg.To),
		slog.String("subject", msg.Subject),
		slog.String("text_body", msg.TextBody),
	)
	return nil
}
