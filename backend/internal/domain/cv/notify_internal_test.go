package cv

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"

	"github.com/Joakim-animate90/myjobfyt/backend/internal/apperr"
	"github.com/Joakim-animate90/myjobfyt/backend/internal/platform/events"
	"github.com/Joakim-animate90/myjobfyt/backend/internal/platform/mailer"
	"github.com/google/uuid"
)

// notifyRepo implements only CandidateEmail. Anything else panics, so a method this
// handler is not expected to call cannot pass unnoticed.
type notifyRepo struct {
	Repository
	email string
	err   error
	calls int
}

func (r *notifyRepo) CandidateEmail(context.Context, uuid.UUID) (string, error) {
	r.calls++
	if r.err != nil {
		return "", r.err
	}
	return r.email, nil
}

type recordingNotifier struct {
	mu      sync.Mutex
	sent    []mailer.ParseFailureNotice
	failErr error
}

func (n *recordingNotifier) SendParseFailedEmail(_ context.Context, notice mailer.ParseFailureNotice) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.failErr != nil {
		return n.failErr
	}
	n.sent = append(n.sent, notice)
	return nil
}

func (n *recordingNotifier) count() int {
	n.mu.Lock()
	defer n.mu.Unlock()
	return len(n.sent)
}

func parseFailedEnvelope(t *testing.T, userID uuid.UUID, reason ParseFailureReason, stage string) *events.Envelope {
	t.Helper()
	env, err := events.NewEnvelope(events.TypeCVParseFailed, 1, events.Meta{UserID: userID.String()},
		map[string]any{
			"cv_id":      uuid.New().String(),
			"version_id": uuid.New().String(),
			"user_id":    userID.String(),
			"reason":     string(reason),
			"stage":      stage,
		})
	if err != nil {
		t.Fatalf("NewEnvelope: %v", err)
	}
	return env
}

func newNotifyWorker(repo Repository, notifier Notifier) *Worker {
	return &Worker{
		repo:     repo,
		notifier: notifier,
		log:      slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
}

func TestParseFailureNoticeExplainsTheReasonToTheCandidate(t *testing.T) {
	// The whole point of classifying failures: "this looks like a scanned image" is the
	// difference between a candidate fixing it in a minute and giving up. A generic
	// error would make the classification pointless.
	userID := uuid.New()
	repo := &notifyRepo{email: "candidate@example.com"}
	notifier := &recordingNotifier{}
	w := newNotifyWorker(repo, notifier)

	env := parseFailedEnvelope(t, userID, ReasonNoExtractableText, "text_extract")
	if err := w.handleParseFailedNotification(context.Background(), env); err != nil {
		t.Fatalf("handleParseFailedNotification: %v", err)
	}

	if notifier.count() != 1 {
		t.Fatalf("sent %d notices, want 1", notifier.count())
	}
	notice := notifier.sent[0]
	if notice.To != "candidate@example.com" {
		t.Errorf("To = %q", notice.To)
	}
	if notice.Explanation != ReasonNoExtractableText.Message() {
		t.Errorf("Explanation = %q, want the reason's own copy", notice.Explanation)
	}
	if !strings.Contains(strings.ToLower(notice.Explanation), "scanned image") {
		t.Errorf("the explanation is not specific to the reason: %q", notice.Explanation)
	}
	if notice.Retryable {
		t.Error("a scanned image was marked retryable; it will never parse on a retry")
	}
}

func TestARetryableFailureTellsTheCandidateToWait(t *testing.T) {
	// An AI outage is not the candidate's problem to fix. Telling them to upload a
	// different file would have them replace a perfectly good CV.
	repo := &notifyRepo{email: "candidate@example.com"}
	notifier := &recordingNotifier{}
	w := newNotifyWorker(repo, notifier)

	env := parseFailedEnvelope(t, uuid.New(), ReasonAIUnavailable, "structure")
	if err := w.handleParseFailedNotification(context.Background(), env); err != nil {
		t.Fatalf("handleParseFailedNotification: %v", err)
	}

	if notifier.count() != 1 {
		t.Fatalf("sent %d notices, want 1", notifier.count())
	}
	if !notifier.sent[0].Retryable {
		t.Error("an AI outage was not marked retryable, so the candidate is told to fix it themselves")
	}
}

func TestNoNoticeWithoutAVerifiedAddress(t *testing.T) {
	// Mail to an unverified address goes to someone who may not own it, and a deleted
	// account should not be contacted at all. Neither is an error to retry.
	repo := &notifyRepo{err: apperr.NotFound("candidate")}
	notifier := &recordingNotifier{}
	w := newNotifyWorker(repo, notifier)

	env := parseFailedEnvelope(t, uuid.New(), ReasonCorrupt, "text_extract")
	if err := w.handleParseFailedNotification(context.Background(), env); err != nil {
		t.Fatalf("err = %v, want nil; a missing address is not a failure to retry", err)
	}
	if notifier.count() != 0 {
		t.Errorf("sent %d notices without a verified address", notifier.count())
	}
}

func TestAMailerFailureIsRetryable(t *testing.T) {
	// A refused SMTP connection must not consume the candidate's only notification.
	repo := &notifyRepo{email: "candidate@example.com"}
	notifier := &recordingNotifier{failErr: errors.New("connection refused")}
	w := newNotifyWorker(repo, notifier)

	env := parseFailedEnvelope(t, uuid.New(), ReasonCorrupt, "text_extract")
	err := w.handleParseFailedNotification(context.Background(), env)
	if apperr.KindOf(err) != apperr.KindDependency {
		t.Fatalf("err = %v (kind %v), want a dependency failure so it is retried",
			err, apperr.KindOf(err))
	}
}

func TestNotificationIsSkippedCleanlyWithoutANotifier(t *testing.T) {
	// Running without a mail sender is a supported development mode. The handler must
	// acknowledge rather than dead-letter every parse failure.
	repo := &notifyRepo{email: "candidate@example.com"}
	w := newNotifyWorker(repo, nil)

	env := parseFailedEnvelope(t, uuid.New(), ReasonCorrupt, "text_extract")
	if err := w.handleParseFailedNotification(context.Background(), env); err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if repo.calls != 0 {
		t.Errorf("looked up the candidate's address %d times with no notifier", repo.calls)
	}
}

func TestAnUndecodablePayloadIsTerminalNotRetried(t *testing.T) {
	// Retrying a message that cannot be decoded produces the same result four times and
	// then dead-letters anyway. Failing terminally gets it in front of an operator now.
	repo := &notifyRepo{email: "candidate@example.com"}
	w := newNotifyWorker(repo, &recordingNotifier{})

	env := &events.Envelope{
		Type:    events.TypeCVParseFailed,
		Version: 1,
		Payload: json.RawMessage(`"not an object"`),
	}
	err := w.handleParseFailedNotification(context.Background(), env)
	if apperr.KindOf(err) != apperr.KindUnprocessable {
		t.Fatalf("err = %v (kind %v), want unprocessable", err, apperr.KindOf(err))
	}
}

func TestAnInvalidUserIDIsTerminalNotRetried(t *testing.T) {
	repo := &notifyRepo{email: "candidate@example.com"}
	w := newNotifyWorker(repo, &recordingNotifier{})

	env, err := events.NewEnvelope(events.TypeCVParseFailed, 1, events.Meta{}, map[string]any{
		"version_id": uuid.New().String(),
		"user_id":    "not-a-uuid",
		"reason":     string(ReasonCorrupt),
		"stage":      "text_extract",
	})
	if err != nil {
		t.Fatalf("NewEnvelope: %v", err)
	}

	if got := apperr.KindOf(w.handleParseFailedNotification(context.Background(), env)); got != apperr.KindUnprocessable {
		t.Fatalf("kind = %v, want unprocessable", got)
	}
}

func TestTheNoticeCarriesNothingFromTheDocument(t *testing.T) {
	// A CV is personal data and email is the least controlled channel it could travel
	// through. The notice must say what went wrong and what to do, and nothing about
	// what the document contained.
	repo := &notifyRepo{email: "candidate@example.com"}
	notifier := &recordingNotifier{}
	w := newNotifyWorker(repo, notifier)

	for _, reason := range []ParseFailureReason{
		ReasonNoExtractableText, ReasonEncrypted, ReasonCorrupt, ReasonTooLong,
		ReasonAIUnavailable, ReasonSchemaViolation, ReasonInfected,
	} {
		env := parseFailedEnvelope(t, uuid.New(), reason, "text_extract")
		if err := w.handleParseFailedNotification(context.Background(), env); err != nil {
			t.Fatalf("%s: %v", reason, err)
		}
	}

	if notifier.count() != 7 {
		t.Fatalf("sent %d notices, want 7", notifier.count())
	}
	for _, notice := range notifier.sent {
		// The explanation must be the domain's prose, never an internal identifier that
		// happened to be in scope.
		if notice.Explanation == "" {
			t.Error("a notice had no explanation")
		}
		for _, leak := range []string{"storage_key", "tenants/", ".pdf", "raw_text", "cv_version_id"} {
			if strings.Contains(notice.Explanation, leak) {
				t.Errorf("the explanation contains %q: %s", leak, notice.Explanation)
			}
		}
	}
}

func TestEveryFailureReasonHasCandidateFacingCopy(t *testing.T) {
	// A reason with no message falls through to the generic default, which silently
	// undoes the classification for whichever failure was added last.
	generic := ParseFailureReason("something-unmapped").Message()

	for _, reason := range []ParseFailureReason{
		ReasonNoExtractableText, ReasonEncrypted, ReasonCorrupt, ReasonTooLong,
		ReasonAIUnavailable, ReasonSchemaViolation, ReasonInfected,
	} {
		msg := reason.Message()
		if msg == "" {
			t.Errorf("%s has no message", reason)
			continue
		}
		if msg == generic {
			t.Errorf("%s falls through to the generic message, so its classification is wasted", reason)
		}
		if !strings.HasSuffix(strings.TrimSpace(msg), ".") {
			t.Errorf("%s: message is not a sentence: %q", reason, msg)
		}
	}
}
