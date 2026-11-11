package events

// These tests exercise the delivery protocol directly: the idempotency claim, the
// retry/dead-letter decision, and the in-flight ambiguity policy. They are the unit
// tests for the single most correctness-critical code path in the platform, so they run
// against fakes rather than a live broker and stay fast enough to run on every change.

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/Joakim-animate90/myjobfyt/backend/internal/apperr"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

// ---------------------------------------------------------------------------
// fakes
// ---------------------------------------------------------------------------

type fakeMsg struct {
	data       []byte
	subject    string
	deliveries uint64

	mu            sync.Mutex
	acked         int
	naked         int
	nakDelay      time.Duration
	termed        int
	inProgressHit int
}

func newFakeMsg(t *testing.T, env *Envelope, deliveries uint64) *fakeMsg {
	t.Helper()
	body, err := json.Marshal(env)
	if err != nil {
		t.Fatalf("marshal envelope: %v", err)
	}
	return &fakeMsg{data: body, subject: env.Subject(), deliveries: deliveries}
}

func (m *fakeMsg) Metadata() (*jetstream.MsgMetadata, error) {
	return &jetstream.MsgMetadata{NumDelivered: m.deliveries}, nil
}
func (m *fakeMsg) Data() []byte         { return m.data }
func (m *fakeMsg) Headers() nats.Header { return nil }
func (m *fakeMsg) Subject() string      { return m.subject }
func (m *fakeMsg) Reply() string        { return "" }

func (m *fakeMsg) Ack() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.acked++
	return nil
}
func (m *fakeMsg) DoubleAck(context.Context) error { return m.Ack() }
func (m *fakeMsg) Nak() error                      { return m.NakWithDelay(0) }
func (m *fakeMsg) NakWithDelay(d time.Duration) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.naked++
	m.nakDelay = d
	return nil
}
func (m *fakeMsg) InProgress() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.inProgressHit++
	return nil
}
func (m *fakeMsg) Term() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.termed++
	return nil
}
func (m *fakeMsg) TermWithReason(string) error { return m.Term() }

func (m *fakeMsg) counts() (acked, naked, termed int, delay time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.acked, m.naked, m.termed, m.nakDelay
}

// fakeStore is an in-memory IdempotencyStore.
type fakeStore struct {
	mu       sync.Mutex
	states   map[string]ClaimState
	claimErr error

	claims    int
	completes int
	releases  int
}

func newFakeStore() *fakeStore {
	return &fakeStore{states: map[string]ClaimState{}}
}

func (s *fakeStore) Claim(_ context.Context, key, _ string, _ time.Duration) (ClaimState, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.claims++
	if s.claimErr != nil {
		return "", s.claimErr
	}
	if st, ok := s.states[key]; ok {
		return st, nil
	}
	s.states[key] = ClaimInFlight
	return ClaimNew, nil
}

func (s *fakeStore) Complete(_ context.Context, key string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.completes++
	s.states[key] = ClaimDone
	return nil
}

func (s *fakeStore) Release(_ context.Context, key string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.releases++
	if s.states[key] == ClaimInFlight {
		delete(s.states, key)
	}
	return nil
}

func (s *fakeStore) stats() (claims, completes, releases int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.claims, s.completes, s.releases
}

// ---------------------------------------------------------------------------
// harness
// ---------------------------------------------------------------------------

type harness struct {
	rt        *Runtime
	store     *fakeStore
	deadLetrs []deadLetterCapture
	mu        sync.Mutex
}

type deadLetterCapture struct {
	subject string
	body    map[string]any
}

func newHarness(store IdempotencyStore) *harness {
	h := &harness{}
	if fs, ok := store.(*fakeStore); ok {
		h.store = fs
	}
	// Discard logs: these tests assert on behaviour, not on log output.
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	rt := &Runtime{
		idempotency:       store,
		log:               log,
		tracer:            noopTracer(),
		defaultAckWait:    time.Minute,
		defaultMaxDeliver: 5,
	}
	rt.publishRaw = func(_ context.Context, subject string, body []byte) error {
		var decoded map[string]any
		_ = json.Unmarshal(body, &decoded)
		h.mu.Lock()
		defer h.mu.Unlock()
		h.deadLetrs = append(h.deadLetrs, deadLetterCapture{subject: subject, body: decoded})
		return nil
	}
	h.rt = rt
	return h
}

func (h *harness) deadLetters() []deadLetterCapture {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]deadLetterCapture(nil), h.deadLetrs...)
}

func testRegistration(handler Handler) Registration {
	reg := Registration{
		Name:     "test-consumer",
		Group:    "cv",
		Stream:   "CV",
		Subjects: []string{"myjobfyt.cv.uploaded"},
		Handler:  handler,
		IdempotencyKey: func(env *Envelope) string {
			return "cv.parse:" + env.EventID
		},
	}
	reg.withDefaults(time.Minute, 5)
	return reg
}

func testEnvelope(t *testing.T) *Envelope {
	t.Helper()
	env, err := NewEnvelope(TypeCVUploaded, 1, Meta{CorrelationID: "corr-1", UserID: "user-1"},
		map[string]string{"cv_id": "cv-1"})
	if err != nil {
		t.Fatalf("NewEnvelope: %v", err)
	}
	return env
}

// ---------------------------------------------------------------------------
// tests
// ---------------------------------------------------------------------------

func TestSuccessfulHandlerAcksAndCompletesTheClaim(t *testing.T) {
	h := newHarness(newFakeStore())
	called := 0
	reg := testRegistration(func(context.Context, *Envelope) error {
		called++
		return nil
	})
	env := testEnvelope(t)
	msg := newFakeMsg(t, env, 1)

	outcome := h.rt.run(context.Background(), reg, env, msg, h.rt.log, 1)

	if outcome != "ok" {
		t.Fatalf("outcome = %q, want ok", outcome)
	}
	if called != 1 {
		t.Fatalf("handler called %d times, want 1", called)
	}
	acked, naked, termed, _ := msg.counts()
	if acked != 1 || naked != 0 || termed != 0 {
		t.Fatalf("acked=%d naked=%d termed=%d, want 1/0/0", acked, naked, termed)
	}
	_, completes, _ := h.store.stats()
	if completes != 1 {
		t.Fatalf("completes = %d, want 1", completes)
	}
}

func TestRedeliveryOfCompletedWorkIsDroppedWithoutRunningTheHandler(t *testing.T) {
	// This is the core at-least-once guarantee: JetStream redelivers, and the second
	// delivery must not repeat the work.
	store := newFakeStore()
	h := newHarness(store)
	called := 0
	reg := testRegistration(func(context.Context, *Envelope) error {
		called++
		return nil
	})
	env := testEnvelope(t)

	first := newFakeMsg(t, env, 1)
	if got := h.rt.run(context.Background(), reg, env, first, h.rt.log, 1); got != "ok" {
		t.Fatalf("first outcome = %q, want ok", got)
	}

	second := newFakeMsg(t, env, 2)
	if got := h.rt.run(context.Background(), reg, env, second, h.rt.log, 2); got != "duplicate" {
		t.Fatalf("second outcome = %q, want duplicate", got)
	}

	if called != 1 {
		t.Fatalf("handler ran %d times across two deliveries, want 1", called)
	}
	acked, _, _, _ := second.counts()
	if acked != 1 {
		t.Fatalf("duplicate was not acknowledged (acked=%d); it would redeliver forever", acked)
	}
}

func TestRetryableFailureNaksWithBackoffAndReleasesTheClaim(t *testing.T) {
	store := newFakeStore()
	h := newHarness(store)
	reg := testRegistration(func(context.Context, *Envelope) error {
		return apperr.Dependency("anthropic", errors.New("503"))
	})
	env := testEnvelope(t)
	msg := newFakeMsg(t, env, 1)

	outcome := h.rt.run(context.Background(), reg, env, msg, h.rt.log, 1)

	if outcome != "retry" {
		t.Fatalf("outcome = %q, want retry", outcome)
	}
	acked, naked, termed, delay := msg.counts()
	if acked != 0 || naked != 1 || termed != 0 {
		t.Fatalf("acked=%d naked=%d termed=%d, want 0/1/0", acked, naked, termed)
	}
	// Plain Nak() ignores the consumer backoff and redelivers instantly, turning a
	// failing dependency into a hot loop. The delay must be non-zero.
	if delay <= 0 {
		t.Fatalf("nak delay = %v, want a positive backoff", delay)
	}
	// The claim must be released, or our own in-flight record blocks the retry.
	_, _, releases := store.stats()
	if releases != 1 {
		t.Fatalf("releases = %d, want 1", releases)
	}
}

func TestReleasedClaimAllowsTheRetryToRunTheHandlerAgain(t *testing.T) {
	store := newFakeStore()
	h := newHarness(store)
	attempts := 0
	reg := testRegistration(func(context.Context, *Envelope) error {
		attempts++
		if attempts == 1 {
			return apperr.Dependency("anthropic", errors.New("timeout"))
		}
		return nil
	})
	env := testEnvelope(t)

	if got := h.rt.run(context.Background(), reg, env, newFakeMsg(t, env, 1), h.rt.log, 1); got != "retry" {
		t.Fatalf("first outcome = %q, want retry", got)
	}
	if got := h.rt.run(context.Background(), reg, env, newFakeMsg(t, env, 2), h.rt.log, 2); got != "ok" {
		t.Fatalf("second outcome = %q, want ok", got)
	}
	if attempts != 2 {
		t.Fatalf("handler ran %d times, want 2", attempts)
	}
}

func TestNonRetryableFailureDeadLettersImmediately(t *testing.T) {
	// Burning five redelivery attempts on a malformed input that will never succeed
	// wastes capacity and delays the dead-letter alert that tells us about it.
	h := newHarness(newFakeStore())
	reg := testRegistration(func(context.Context, *Envelope) error {
		return apperr.Unprocessable("no_extractable_text", "the document contains no extractable text")
	})
	env := testEnvelope(t)
	msg := newFakeMsg(t, env, 1)

	outcome := h.rt.run(context.Background(), reg, env, msg, h.rt.log, 1)

	if outcome != "dead_lettered" {
		t.Fatalf("outcome = %q, want dead_lettered", outcome)
	}
	acked, naked, termed, _ := msg.counts()
	if termed != 1 || naked != 0 || acked != 0 {
		t.Fatalf("acked=%d naked=%d termed=%d, want 0/0/1", acked, naked, termed)
	}

	dls := h.deadLetters()
	if len(dls) != 1 {
		t.Fatalf("dead letters = %d, want 1", len(dls))
	}
	if dls[0].subject != "myjobfyt.dlq.cv" {
		t.Errorf("dlq subject = %q, want myjobfyt.dlq.cv", dls[0].subject)
	}
	if dls[0].body["reason"] != "terminal" {
		t.Errorf("reason = %v, want terminal", dls[0].body["reason"])
	}
	if dls[0].body["last_error"] == nil {
		t.Error("dead letter does not record the error; it would be undiagnosable")
	}
	if dls[0].body["envelope"] == nil {
		t.Error("dead letter does not carry the envelope; requeue would be impossible")
	}
}

func TestRetryableFailureOnTheLastAttemptDeadLetters(t *testing.T) {
	h := newHarness(newFakeStore())
	reg := testRegistration(func(context.Context, *Envelope) error {
		return apperr.Dependency("adzuna", errors.New("503"))
	})
	reg.MaxDeliver = 3
	env := testEnvelope(t)
	msg := newFakeMsg(t, env, 3)

	outcome := h.rt.run(context.Background(), reg, env, msg, h.rt.log, 3)

	if outcome != "dead_lettered" {
		t.Fatalf("outcome = %q, want dead_lettered on the final attempt", outcome)
	}
	dls := h.deadLetters()
	if len(dls) != 1 || dls[0].body["reason"] != "max_deliver_exhausted" {
		t.Fatalf("dead letter reason = %v, want max_deliver_exhausted", dls)
	}
}

func TestInFlightWithUnknownPolicyRefusesToRepeatTheSideEffect(t *testing.T) {
	// The scenario: a submit worker claimed the key, dispatched an application, then
	// crashed before acking. On redelivery we cannot tell whether the employer already
	// received it. Repeating it would apply twice to the same job, so we must stop.
	store := newFakeStore()
	store.states["submit:app-1"] = ClaimInFlight

	h := newHarness(store)
	called := 0
	reg := Registration{
		Name:     "submit-application",
		Group:    "applications",
		Stream:   "APPLICATIONS",
		Handler:  func(context.Context, *Envelope) error { called++; return nil },
		InFlight: InFlightUnknown,
		IdempotencyKey: func(*Envelope) string {
			return "submit:app-1"
		},
	}
	reg.withDefaults(time.Minute, 5)

	env, err := NewEnvelope(TypeApplicationAuthorised, 1, Meta{}, map[string]string{"application_id": "app-1"})
	if err != nil {
		t.Fatalf("NewEnvelope: %v", err)
	}
	msg := newFakeMsg(t, env, 2)

	outcome := h.rt.run(context.Background(), reg, env, msg, h.rt.log, 2)

	if outcome != "outcome_unknown" {
		t.Fatalf("outcome = %q, want outcome_unknown", outcome)
	}
	if called != 0 {
		t.Fatalf("handler ran %d times; it must never repeat an irreversible side effect", called)
	}
	dls := h.deadLetters()
	if len(dls) != 1 {
		t.Fatalf("dead letters = %d, want 1", len(dls))
	}
	if dls[0].body["reason"] != "outcome_unknown" {
		t.Errorf("reason = %v, want outcome_unknown", dls[0].body["reason"])
	}
	if dls[0].subject != "myjobfyt.dlq.applications" {
		t.Errorf("dlq subject = %q, want myjobfyt.dlq.applications", dls[0].subject)
	}
	// Nothing retries it: the whole point is to surface the ambiguity to a human.
	_, naked, termed, _ := msg.counts()
	if naked != 0 || termed != 1 {
		t.Fatalf("naked=%d termed=%d, want 0/1", naked, termed)
	}
}

func TestInFlightWithRetryPolicyRunsTheHandlerAgain(t *testing.T) {
	// For internally idempotent work (parsing, scoring) redoing it is harmless, and
	// refusing would strand the message.
	store := newFakeStore()
	env := testEnvelope(t)
	store.states["cv.parse:"+env.EventID] = ClaimInFlight

	h := newHarness(store)
	called := 0
	reg := testRegistration(func(context.Context, *Envelope) error { called++; return nil })
	reg.InFlight = InFlightRetry

	outcome := h.rt.run(context.Background(), reg, env, newFakeMsg(t, env, 2), h.rt.log, 2)

	if outcome != "ok" {
		t.Fatalf("outcome = %q, want ok", outcome)
	}
	if called != 1 {
		t.Fatalf("handler ran %d times, want 1", called)
	}
}

func TestClaimFailureRetriesRatherThanGuessing(t *testing.T) {
	// If we cannot read the idempotency record we do not know whether the work is
	// safe to perform, so the only correct move is to retry the claim later.
	store := newFakeStore()
	store.claimErr = errors.New("connection refused")
	h := newHarness(store)

	called := 0
	reg := testRegistration(func(context.Context, *Envelope) error { called++; return nil })
	env := testEnvelope(t)
	msg := newFakeMsg(t, env, 1)

	outcome := h.rt.run(context.Background(), reg, env, msg, h.rt.log, 1)

	if outcome != "claim_failed" {
		t.Fatalf("outcome = %q, want claim_failed", outcome)
	}
	if called != 0 {
		t.Fatalf("handler ran %d times despite an unknown claim state, want 0", called)
	}
	_, naked, termed, _ := msg.counts()
	if naked != 1 || termed != 0 {
		t.Fatalf("naked=%d termed=%d, want 1/0", naked, termed)
	}
}

func TestHandlerSuccessIsAckedEvenIfCompleteFails(t *testing.T) {
	// The work is done. NAKing would redeliver work that already happened, which is
	// strictly worse than a stale in_flight row that lapses on its own.
	store := &failingCompleteStore{inner: newFakeStore()}
	h := newHarness(store)
	reg := testRegistration(func(context.Context, *Envelope) error { return nil })
	env := testEnvelope(t)
	msg := newFakeMsg(t, env, 1)

	outcome := h.rt.run(context.Background(), reg, env, msg, h.rt.log, 1)

	if outcome != "ok" {
		t.Fatalf("outcome = %q, want ok", outcome)
	}
	acked, naked, _, _ := msg.counts()
	if acked != 1 || naked != 0 {
		t.Fatalf("acked=%d naked=%d, want 1/0", acked, naked)
	}
}

type failingCompleteStore struct{ inner *fakeStore }

func (s *failingCompleteStore) Claim(ctx context.Context, k, c string, d time.Duration) (ClaimState, error) {
	return s.inner.Claim(ctx, k, c, d)
}
func (s *failingCompleteStore) Complete(context.Context, string) error {
	return errors.New("postgres unavailable")
}
func (s *failingCompleteStore) Release(ctx context.Context, k string) error {
	return s.inner.Release(ctx, k)
}

func TestHandlerWithoutAnIdempotencyKeySkipsTheStore(t *testing.T) {
	store := newFakeStore()
	h := newHarness(store)
	reg := testRegistration(func(context.Context, *Envelope) error { return nil })
	reg.IdempotencyKey = nil
	env := testEnvelope(t)

	if got := h.rt.run(context.Background(), reg, env, newFakeMsg(t, env, 1), h.rt.log, 1); got != "ok" {
		t.Fatalf("outcome = %q, want ok", got)
	}
	claims, completes, _ := store.stats()
	if claims != 0 || completes != 0 {
		t.Fatalf("store was used despite no key: claims=%d completes=%d", claims, completes)
	}
}

func TestBackoffIsMonotonicAndBounded(t *testing.T) {
	var previous time.Duration
	for deliveries := uint64(1); deliveries <= 10; deliveries++ {
		d := backoffFor(deliveries)
		if d <= 0 {
			t.Fatalf("backoffFor(%d) = %v, want positive", deliveries, d)
		}
		if d < previous {
			t.Fatalf("backoffFor(%d) = %v decreased from %v", deliveries, d, previous)
		}
		previous = d
	}
	// Past the end of the schedule it must clamp, not index out of range or grow
	// without bound.
	if backoffFor(1000) != redeliveryBackoff[len(redeliveryBackoff)-1] {
		t.Fatalf("backoffFor(1000) = %v, want the final schedule entry", backoffFor(1000))
	}
	// Zero would be a programming error upstream; it must not panic.
	if backoffFor(0) != redeliveryBackoff[0] {
		t.Fatalf("backoffFor(0) = %v, want the first schedule entry", backoffFor(0))
	}
}

func TestRegistrationDefaultsLeaveHeadroomUnderAckWait(t *testing.T) {
	// A handler timeout equal to AckWait means JetStream redelivers at the exact
	// moment the handler gives up, so the NAK races the redelivery.
	reg := Registration{Name: "x", Handler: func(context.Context, *Envelope) error { return nil }}
	reg.withDefaults(2*time.Minute, 5)

	if reg.AckWait != 2*time.Minute {
		t.Errorf("AckWait = %v, want the default", reg.AckWait)
	}
	if reg.MaxDeliver != 5 {
		t.Errorf("MaxDeliver = %d, want the default", reg.MaxDeliver)
	}
	if reg.MaxAckPending <= 0 {
		t.Error("MaxAckPending must default to a positive bound for backpressure")
	}
	if reg.Timeout >= reg.AckWait {
		t.Errorf("Timeout %v must be shorter than AckWait %v", reg.Timeout, reg.AckWait)
	}
}

func TestUndecodableMessageIsDeadLetteredWithItsRawBody(t *testing.T) {
	h := newHarness(newFakeStore())
	called := 0
	reg := testRegistration(func(context.Context, *Envelope) error { called++; return nil })

	msg := &fakeMsg{data: []byte("{not json"), subject: "myjobfyt.cv.uploaded", deliveries: 1}
	h.rt.handle(context.Background(), reg, msg)

	if called != 0 {
		t.Fatalf("handler ran on an undecodable message")
	}
	_, _, termed, _ := msg.counts()
	if termed != 1 {
		t.Fatalf("termed = %d, want 1; an undecodable message can never succeed", termed)
	}
	dls := h.deadLetters()
	if len(dls) != 1 {
		t.Fatalf("dead letters = %d, want 1", len(dls))
	}
	if dls[0].body["raw"] != "{not json" {
		t.Errorf("raw body = %v, want the original payload preserved for diagnosis", dls[0].body["raw"])
	}
	if dls[0].subject != "myjobfyt.dlq.unknown" {
		t.Errorf("subject = %q, want myjobfyt.dlq.unknown", dls[0].subject)
	}
}

func TestBackoffScheduleSatisfiesJetStreamsConstraint(t *testing.T) {
	// JetStream rejects a consumer whose BackOff length is at least MaxDeliver, with
	// "max deliver is required to be > length of backoff values". Getting this wrong
	// means the worker fails to start, so the runtime trims the schedule rather than
	// requiring every registration author to know the rule.
	for maxDeliver := 1; maxDeliver <= 12; maxDeliver++ {
		schedule := backoffSchedule(maxDeliver)
		if len(schedule) >= maxDeliver {
			t.Errorf("MaxDeliver=%d produced %d backoff entries; JetStream requires fewer than MaxDeliver",
				maxDeliver, len(schedule))
		}
	}
}

func TestBackoffScheduleKeepsTheFullCurveWhenItFits(t *testing.T) {
	// With generous redelivery the whole curve should be used, so a long outage backs
	// off to five minutes rather than retrying every second.
	schedule := backoffSchedule(10)
	if len(schedule) != len(redeliveryBackoff) {
		t.Fatalf("got %d entries, want the full %d", len(schedule), len(redeliveryBackoff))
	}
}

func TestBackoffScheduleTrimsFromTheFront(t *testing.T) {
	// A trimmed schedule must keep the SHORT delays, not the long ones: a consumer with
	// few attempts should use them quickly rather than waiting five minutes for its
	// only retry.
	schedule := backoffSchedule(3)
	if len(schedule) != 2 {
		t.Fatalf("got %d entries, want 2", len(schedule))
	}
	if schedule[0] != redeliveryBackoff[0] || schedule[1] != redeliveryBackoff[1] {
		t.Fatalf("schedule = %v, want the first two entries %v", schedule, redeliveryBackoff[:2])
	}
}

func TestSingleDeliveryHasNoBackoff(t *testing.T) {
	if schedule := backoffSchedule(1); len(schedule) != 0 {
		t.Fatalf("MaxDeliver=1 produced %v, want no backoff entries", schedule)
	}
	if schedule := backoffSchedule(0); len(schedule) != 0 {
		t.Fatalf("MaxDeliver=0 produced %v, want no backoff entries", schedule)
	}
}
