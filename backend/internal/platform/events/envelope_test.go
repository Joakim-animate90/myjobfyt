package events_test

import (
	"encoding/json"
	"github.com/nats-io/nats.go/jetstream"
	"strings"
	"testing"
	"time"

	"github.com/Joakim-animate90/myjobfyt/backend/internal/platform/events"
)

type cvUploaded struct {
	CVID       string `json:"cv_id"`
	StorageKey string `json:"storage_key"`
}

func TestNewEnvelopePopulatesIdentityAndTime(t *testing.T) {
	before := time.Now().UTC().Add(-time.Second)

	env, err := events.NewEnvelope(events.TypeCVUploaded, 1,
		events.Meta{CorrelationID: "corr-1", CausationID: "cause-1", UserID: "user-1"},
		cvUploaded{CVID: "cv-1", StorageKey: "tenants/user-1/abc"},
	)
	if err != nil {
		t.Fatalf("NewEnvelope: %v", err)
	}

	if env.EventID == "" {
		t.Error("EventID is empty")
	}
	if env.Type != events.TypeCVUploaded {
		t.Errorf("Type = %q", env.Type)
	}
	if env.Version != 1 {
		t.Errorf("Version = %d", env.Version)
	}
	if env.CorrelationID != "corr-1" || env.CausationID != "cause-1" || env.UserID != "user-1" {
		t.Errorf("metadata not carried: %+v", env)
	}
	if env.OccurredAt.Before(before) {
		t.Errorf("OccurredAt = %v, expected ~now", env.OccurredAt)
	}
	if env.OccurredAt.Location() != time.UTC {
		t.Errorf("OccurredAt is not UTC: %v", env.OccurredAt.Location())
	}
}

func TestNewEnvelopeFallsBackToEventIDForCorrelation(t *testing.T) {
	// A message published outside any request still needs a correlation id, or the
	// chain it starts cannot be queried later.
	env, err := events.NewEnvelope(events.TypeJobDiscovered, 1, events.Meta{}, map[string]string{"a": "b"})
	if err != nil {
		t.Fatalf("NewEnvelope: %v", err)
	}
	if env.CorrelationID != env.EventID {
		t.Fatalf("CorrelationID = %q, want it to default to EventID %q", env.CorrelationID, env.EventID)
	}
}

func TestEventIDsAreUnique(t *testing.T) {
	seen := make(map[string]struct{}, 500)
	for i := 0; i < 500; i++ {
		env, err := events.NewEnvelope(events.TypeJobDiscovered, 1, events.Meta{}, struct{}{})
		if err != nil {
			t.Fatalf("NewEnvelope: %v", err)
		}
		if _, dup := seen[env.EventID]; dup {
			t.Fatalf("duplicate event id %q at iteration %d", env.EventID, i)
		}
		seen[env.EventID] = struct{}{}
	}
}

func TestEnvelopeDecodeRoundTrip(t *testing.T) {
	in := cvUploaded{CVID: "cv-9", StorageKey: "tenants/u/9"}
	env, err := events.NewEnvelope(events.TypeCVUploaded, 1, events.Meta{}, in)
	if err != nil {
		t.Fatalf("NewEnvelope: %v", err)
	}

	// Through the wire, as a consumer would receive it.
	raw, err := json.Marshal(env)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var decoded events.Envelope
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	var out cvUploaded
	if err := decoded.Decode(&out); err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if out != in {
		t.Fatalf("payload = %+v, want %+v", out, in)
	}
}

func TestDecodeReportsTheEventTypeOnFailure(t *testing.T) {
	// A decode failure in a consumer is diagnosed from the log line alone, so the
	// error must name the event type and id.
	env, err := events.NewEnvelope(events.TypeCVUploaded, 1, events.Meta{}, cvUploaded{CVID: "x"})
	if err != nil {
		t.Fatalf("NewEnvelope: %v", err)
	}

	var wrongShape []string
	derr := env.Decode(&wrongShape)
	if derr == nil {
		t.Fatal("Decode accepted a mismatched target type")
	}
	if !contains(derr.Error(), events.TypeCVUploaded) || !contains(derr.Error(), env.EventID) {
		t.Fatalf("error %q does not identify the event", derr)
	}
}

func contains(haystack, needle string) bool {
	return len(needle) > 0 && len(haystack) >= len(needle) && (func() bool {
		for i := 0; i+len(needle) <= len(haystack); i++ {
			if haystack[i:i+len(needle)] == needle {
				return true
			}
		}
		return false
	})()
}

func TestDerivedMetaAdvancesTheCausationChain(t *testing.T) {
	// A follow-on event must keep the same correlation id but point its causation at
	// the event that produced it, otherwise the chain cannot be reconstructed.
	parent, err := events.NewEnvelope(events.TypeCVUploaded, 1,
		events.Meta{CorrelationID: "corr-7", UserID: "user-7", Trace: map[string]string{"traceparent": "00-abc-def-01"}},
		cvUploaded{CVID: "cv-7"},
	)
	if err != nil {
		t.Fatalf("NewEnvelope: %v", err)
	}

	meta := parent.DerivedMeta()
	if meta.CorrelationID != "corr-7" {
		t.Errorf("CorrelationID = %q, want corr-7", meta.CorrelationID)
	}
	if meta.CausationID != parent.EventID {
		t.Errorf("CausationID = %q, want the parent event id %q", meta.CausationID, parent.EventID)
	}
	if meta.UserID != "user-7" {
		t.Errorf("UserID = %q, want user-7", meta.UserID)
	}
	if meta.Trace["traceparent"] != "00-abc-def-01" {
		t.Errorf("trace context was dropped: %+v", meta.Trace)
	}
}

func TestSubjectAndDeadLetterRouting(t *testing.T) {
	cases := []struct {
		eventType string
		subject   string
		dlq       string
		domain    string
	}{
		{events.TypeCVUploaded, "myjobfyt.cv.uploaded", "myjobfyt.dlq.cv", "cv"},
		{events.TypeJobNormalised, "myjobfyt.jobs.normalised", "myjobfyt.dlq.jobs", "jobs"},
		{events.TypeApplicationSubmitted, "myjobfyt.applications.submitted", "myjobfyt.dlq.applications", "applications"},
		{events.TypeCandidateCreated, "myjobfyt.identity.candidate.created", "myjobfyt.dlq.identity", "identity"},
		{events.TypeOutreachBounced, "myjobfyt.outreach.bounced", "myjobfyt.dlq.outreach", "outreach"},
		{events.TypeQuotaExhausted, "myjobfyt.billing.quota.exhausted", "myjobfyt.dlq.billing", "billing"},
	}
	for _, tc := range cases {
		t.Run(tc.eventType, func(t *testing.T) {
			if got := events.SubjectFor(tc.eventType); got != tc.subject {
				t.Errorf("SubjectFor = %q, want %q", got, tc.subject)
			}
			if got := events.DeadLetterSubject(tc.eventType); got != tc.dlq {
				t.Errorf("DeadLetterSubject = %q, want %q", got, tc.dlq)
			}
			if got := events.DomainOf(tc.eventType); got != tc.domain {
				t.Errorf("DomainOf = %q, want %q", got, tc.domain)
			}
		})
	}
}

func TestEverySubjectIsCoveredByAStream(t *testing.T) {
	// A published event whose subject matches no stream is silently discarded by
	// JetStream. This test is the guard against adding an event type and forgetting
	// the stream, which would be invisible until the feature quietly did nothing.
	allTypes := []string{
		events.TypeCandidateCreated, events.TypeEmailVerified, events.TypeProfileUpdated,
		events.TypeAccountDeletionRequested, events.TypeAccountErased,
		events.TypeCVUploaded, events.TypeCVScanned, events.TypeCVTextExtracted,
		events.TypeCVParsed, events.TypeCVParseFailed, events.TypeCVAnalysisRequested,
		events.TypeCVAnalysisCompleted, events.TypeCVTailoringRequested, events.TypeCVTailored,
		events.TypeDiscoveryRequested, events.TypeJobDiscovered, events.TypeJobNormalised,
		events.TypeJobUpdated, events.TypeJobExpired, events.TypeSourceDegraded,
		events.TypeMatchingRequested, events.TypeJobMatched, events.TypeJobRejected,
		events.TypeBackfillRequested,
		events.TypeApplicationPreparationRequested, events.TypeApplicationMethodSelected,
		events.TypeApplicationPrepared, events.TypeApplicationQuestionsPending,
		events.TypeApplicationQuestionsAnswered, events.TypeApplicationAuthorised,
		events.TypeApplicationSubmitted, events.TypeApplicationFailed,
		events.TypeApplicationOutcomeUnknown, events.TypeApplicationStatusUpdated,
		events.TypeOutreachSendRequested, events.TypeOutreachSent, events.TypeOutreachOpened,
		events.TypeOutreachReplied, events.TypeOutreachBounced, events.TypeOutreachComplained,
		events.TypeSubscriptionActivated, events.TypeSubscriptionUpdated,
		events.TypeSubscriptionCanceled, events.TypePaymentFailed, events.TypeQuotaExhausted,
	}

	streams := events.Streams()
	for _, et := range allTypes {
		subject := events.SubjectFor(et)
		matched := false
		for _, s := range streams {
			for _, pattern := range s.Subjects {
				if subjectMatches(pattern, subject) {
					matched = true
				}
			}
		}
		if !matched {
			t.Errorf("event type %q (subject %q) matches no stream", et, subject)
		}

		// And its dead-letter subject must land in the DLQ stream.
		dlq := events.DeadLetterSubject(et)
		matchedDLQ := false
		for _, s := range streams {
			for _, pattern := range s.Subjects {
				if subjectMatches(pattern, dlq) {
					matchedDLQ = true
				}
			}
		}
		if !matchedDLQ {
			t.Errorf("dead-letter subject %q for %q matches no stream", dlq, et)
		}
	}
}

// subjectMatches implements the subset of NATS subject wildcard matching used by the
// stream specs: a trailing ">" matches one or more trailing tokens.
func subjectMatches(pattern, subject string) bool {
	if !hasSuffix(pattern, ".>") {
		return pattern == subject
	}
	prefix := pattern[:len(pattern)-1] // keep the trailing dot
	return len(subject) > len(prefix) && subject[:len(prefix)] == prefix
}

func hasSuffix(s, suffix string) bool {
	return len(s) >= len(suffix) && s[len(s)-len(suffix):] == suffix
}

func TestStreamTopologyHasNoOverlappingSubjects(t *testing.T) {
	// Two streams capturing the same subject would each store a copy, and a
	// work-queue stream refuses to be created at all if its subjects overlap another.
	streams := events.Streams()
	for i := range streams {
		for j := range streams {
			if i >= j {
				continue
			}
			for _, a := range streams[i].Subjects {
				for _, b := range streams[j].Subjects {
					if a == b {
						t.Errorf("streams %s and %s both claim subject %q",
							streams[i].Name, streams[j].Name, a)
					}
				}
			}
		}
	}
}

func TestStreamsHaveNonZeroRetention(t *testing.T) {
	for _, s := range events.Streams() {
		if s.MaxAge <= 0 {
			t.Errorf("stream %s has no MaxAge; it would grow without bound", s.Name)
		}
		if len(s.Subjects) == 0 {
			t.Errorf("stream %s has no subjects", s.Name)
		}
	}
}

func TestNoDomainStreamUsesWorkQueueRetention(t *testing.T) {
	// A work-queue stream delivers each message to exactly one consumer and so rejects
	// a second consumer filtering the same subject:
	//
	//	nats: API error: code=400 err_code=10100
	//	description=filtered consumer not unique on workqueue stream
	//
	// Every domain stream here is a fan-out event stream. cv.parsed alone is consumed
	// independently by skill normalisation, by embedding and later by matching. The
	// failure does not appear until the second consumer on a subject is registered, so
	// it would otherwise lie dormant in the topology until some unrelated feature added
	// one and the whole worker refused to start.
	for _, s := range events.Streams() {
		if s.Retention == jetstream.WorkQueuePolicy {
			t.Errorf("stream %s uses work-queue retention; a second consumer on any of "+
				"its subjects %v will fail to register", s.Name, s.Subjects)
		}
	}
}

func TestRetentionMismatchErrorTellsAnOperatorWhatToDo(t *testing.T) {
	// This error is read exactly once, by whoever is staring at a worker that will not
	// start. It has to name the stream and say what to run.
	err := &events.ErrRetentionMismatch{
		Stream: "CV",
		Have:   jetstream.WorkQueuePolicy,
		Want:   jetstream.InterestPolicy,
	}

	msg := err.Error()
	for _, want := range []string{"CV", "workqueue", "interest", "nats stream rm CV", "drain"} {
		if !strings.Contains(msg, want) {
			t.Errorf("the error does not mention %q: %s", want, msg)
		}
	}

	// It must not stringify the policies as the integers the jetstream constants are.
	if strings.Contains(msg, "has 2 retention") || strings.Contains(msg, "expects 1") {
		t.Errorf("retention policies rendered as integers: %s", msg)
	}
}
