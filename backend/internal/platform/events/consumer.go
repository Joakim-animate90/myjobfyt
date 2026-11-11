package events

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/Joakim-animate90/myjobfyt/backend/internal/apperr"
	"github.com/Joakim-animate90/myjobfyt/backend/internal/platform/telemetry"
	"github.com/nats-io/nats.go/jetstream"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

// Handler processes one event. It must be idempotent.
//
// A handler returning a retryable error (per apperr.IsRetryable) is NAK'd and
// redelivered with backoff. Any other error is terminal: the message is dead-lettered
// immediately rather than burning redelivery attempts on an input that will never
// succeed.
type Handler func(ctx context.Context, env *Envelope) error

// InFlightPolicy decides what a handler does when it finds its own idempotency key
// already in flight from an attempt that never resolved.
type InFlightPolicy int

const (
	// InFlightRetry assumes the previous attempt died before completing and retries.
	// Correct for handlers whose work is internally idempotent — parsing, scoring,
	// normalisation: redoing them is harmless.
	InFlightRetry InFlightPolicy = iota

	// InFlightUnknown refuses to repeat the work and dead-letters with
	// ErrOutcomeUnknown. Required for handlers with irreversible external side
	// effects — submitting an application, sending an email — where repeating the work
	// could mean a candidate applies twice to the same job.
	InFlightUnknown
)

// Registration describes one durable consumer.
type Registration struct {
	// Name is the durable consumer name; it must be stable across deploys or the
	// consumer restarts from scratch and reprocesses the stream.
	Name string
	// Group selects which worker process runs this consumer (cv, jobs, matching, ...).
	Group string
	// Stream is the JetStream stream to consume.
	Stream string
	// Subjects filters which subjects this consumer receives.
	Subjects []string
	// Handler does the work.
	Handler Handler
	// IdempotencyKey derives the deterministic key for a message. Returning "" opts
	// out of idempotency tracking, which is acceptable only for handlers with no side
	// effects at all.
	IdempotencyKey func(env *Envelope) string
	// InFlight selects the ambiguity policy. Defaults to InFlightRetry.
	InFlight InFlightPolicy
	// AckWait must exceed the handler's worst-case duration, or JetStream will
	// redeliver a message that is still being processed.
	AckWait time.Duration
	// MaxDeliver bounds redelivery before dead-lettering.
	MaxDeliver int
	// MaxAckPending bounds in-flight messages for this consumer; it is the
	// backpressure control that stops one consumer from exhausting a shared resource.
	MaxAckPending int
	// Timeout bounds a single handler invocation.
	Timeout time.Duration
}

func (r *Registration) withDefaults(ackWait time.Duration, maxDeliver int) {
	if r.AckWait <= 0 {
		r.AckWait = ackWait
	}
	if r.MaxDeliver <= 0 {
		r.MaxDeliver = maxDeliver
	}
	if r.MaxAckPending <= 0 {
		r.MaxAckPending = 64
	}
	if r.Timeout <= 0 {
		// Leave headroom under AckWait so a handler that hits its own timeout still
		// has time to NAK before JetStream redelivers behind its back.
		r.Timeout = r.AckWait - (r.AckWait / 5)
	}
}

// Runtime runs a set of registered consumers.
type Runtime struct {
	bus         *Bus
	idempotency IdempotencyStore
	log         *slog.Logger
	tracer      trace.Tracer

	// publishRaw sends a pre-encoded body to a subject. It exists as a field rather
	// than a direct call on bus so the dead-letter path can be exercised in unit
	// tests without a live NATS server.
	publishRaw func(ctx context.Context, subject string, body []byte) error

	defaultAckWait    time.Duration
	defaultMaxDeliver int

	mu       sync.Mutex
	contexts []jetstream.ConsumeContext
}

// NewRuntime builds a consumer runtime.
func NewRuntime(bus *Bus, store IdempotencyStore, log *slog.Logger, defaultAckWait time.Duration, defaultMaxDeliver int) *Runtime {
	rt := &Runtime{
		bus:               bus,
		idempotency:       store,
		log:               log,
		tracer:            otel.Tracer("myjobfyt/events"),
		defaultAckWait:    defaultAckWait,
		defaultMaxDeliver: defaultMaxDeliver,
	}
	if bus != nil {
		rt.publishRaw = func(ctx context.Context, subject string, body []byte) error {
			_, err := bus.js.Publish(ctx, subject, body)
			return err
		}
	}
	return rt
}

// Start creates every registration whose group is enabled and begins consuming.
func (rt *Runtime) Start(ctx context.Context, regs []Registration, enabledGroups []string) error {
	enabled := map[string]bool{}
	for _, g := range enabledGroups {
		enabled[strings.TrimSpace(g)] = true
	}

	started := 0
	for i := range regs {
		reg := regs[i]
		if !enabled[reg.Group] {
			continue
		}
		reg.withDefaults(rt.defaultAckWait, rt.defaultMaxDeliver)

		if err := rt.startOne(ctx, reg); err != nil {
			return fmt.Errorf("start consumer %s: %w", reg.Name, err)
		}
		started++
	}

	rt.log.Info("consumer runtime started",
		slog.Int("consumers", started),
		slog.String("groups", strings.Join(enabledGroups, ",")),
	)
	if started == 0 {
		// A worker with no consumers would sit idle looking healthy, which is a
		// confusing way to discover a typo in WORKER_GROUPS.
		return fmt.Errorf("no consumers matched the enabled groups %v", enabledGroups)
	}
	return nil
}

func (rt *Runtime) startOne(ctx context.Context, reg Registration) error {
	stream, err := rt.bus.js.Stream(ctx, reg.Stream)
	if err != nil {
		return fmt.Errorf("lookup stream %s: %w", reg.Stream, err)
	}

	cfg := jetstream.ConsumerConfig{
		Durable:        reg.Name,
		FilterSubjects: reg.Subjects,
		AckPolicy:      jetstream.AckExplicitPolicy,
		AckWait:        reg.AckWait,
		MaxDeliver:     reg.MaxDeliver,
		MaxAckPending:  reg.MaxAckPending,
		DeliverPolicy:  jetstream.DeliverAllPolicy,
		// Explicit backoff rather than a flat redelivery interval: a dependency that
		// is briefly unavailable recovers on the second attempt, while one that is
		// genuinely down is not hammered every AckWait seconds.
		// JetStream requires MaxDeliver to exceed the number of backoff entries, so the
		// schedule is trimmed to fit rather than making every registration author know
		// that constraint. A consumer with MaxDeliver=4 gets the first three delays.
		BackOff: backoffSchedule(reg.MaxDeliver),
	}

	consumer, err := stream.CreateOrUpdateConsumer(ctx, cfg)
	if err != nil {
		return fmt.Errorf("create consumer: %w", err)
	}

	consumeCtx, err := consumer.Consume(func(msg jetstream.Msg) {
		rt.handle(ctx, reg, msg)
	})
	if err != nil {
		return fmt.Errorf("begin consuming: %w", err)
	}

	rt.mu.Lock()
	rt.contexts = append(rt.contexts, consumeCtx)
	rt.mu.Unlock()

	rt.log.Info("consumer registered",
		slog.String("consumer", reg.Name),
		slog.String("group", reg.Group),
		slog.String("stream", reg.Stream),
		slog.Any("subjects", reg.Subjects),
		slog.Duration("ack_wait", reg.AckWait),
		slog.Int("max_deliver", reg.MaxDeliver),
	)
	return nil
}

// handle processes one delivery: decode, trace, claim, run, ack.
func (rt *Runtime) handle(parent context.Context, reg Registration, msg jetstream.Msg) {
	var env Envelope
	if err := json.Unmarshal(msg.Data(), &env); err != nil {
		// An undecodable message can never succeed, so retrying is pure waste.
		rt.log.Error("undecodable event; dead-lettering",
			slog.String("consumer", reg.Name),
			slog.String("subject", msg.Subject()),
			slog.Any("error", err),
		)
		rt.deadLetter(parent, reg, msg, nil, "undecodable", err)
		return
	}

	meta, _ := msg.Metadata()
	deliveries := uint64(1)
	if meta != nil {
		deliveries = meta.NumDelivered
	}

	// Restore the publishing request's trace context so the whole asynchronous chain
	// is one trace, then layer this consumer's span on top.
	ctx := ContextWithTrace(parent, env.Trace)
	ctx = telemetry.WithCorrelationID(ctx, env.CorrelationID)
	if env.UserID != "" {
		ctx = telemetry.WithUserID(ctx, env.UserID)
	}
	ctx = telemetry.WithRequestID(ctx, env.EventID)

	ctx, span := rt.tracer.Start(ctx, "consume "+env.Type,
		trace.WithSpanKind(trace.SpanKindConsumer),
		trace.WithAttributes(
			attribute.String("messaging.system", "nats"),
			attribute.String("messaging.destination.name", msg.Subject()),
			attribute.String("myjobfyt.consumer", reg.Name),
			attribute.String("myjobfyt.event_type", env.Type),
			attribute.String("myjobfyt.event_id", env.EventID),
			attribute.Int64("messaging.redelivery_count", int64(deliveries-1)),
		),
	)
	defer span.End()

	log := rt.log.With(
		slog.String("consumer", reg.Name),
		slog.String("event_type", env.Type),
		slog.String("event_id", env.EventID),
		slog.Uint64("delivery", deliveries),
	)

	start := time.Now()
	outcome := rt.run(ctx, reg, &env, msg, log, deliveries)
	elapsed := time.Since(start)

	eventsProcessed.WithLabelValues(reg.Name, env.Type, outcome).Inc()
	eventProcessingDuration.WithLabelValues(reg.Name, env.Type).Observe(elapsed.Seconds())

	span.SetAttributes(attribute.String("myjobfyt.outcome", outcome))
}

// run executes the idempotency protocol and the handler, returning an outcome label.
func (rt *Runtime) run(ctx context.Context, reg Registration, env *Envelope, msg jetstream.Msg, log *slog.Logger, deliveries uint64) string {
	key := ""
	if reg.IdempotencyKey != nil {
		key = reg.IdempotencyKey(env)
	}

	if key != "" && rt.idempotency != nil {
		state, err := rt.idempotency.Claim(ctx, key, reg.Name, reg.AckWait*2)
		if err != nil {
			// The claim itself failed, so we do not know whether the work is safe to
			// perform. Retrying the claim is the only safe move.
			log.Error("idempotency claim failed; will retry", slog.Any("error", err))
			rt.nak(ctx, msg, deliveries, log)
			return "claim_failed"
		}

		switch state {
		case ClaimDone:
			eventsDuplicatesDropped.WithLabelValues(reg.Name, env.Type).Inc()
			log.Debug("duplicate delivery of completed work; acknowledging")
			rt.ack(ctx, msg, log)
			return "duplicate"

		case ClaimInFlight:
			if reg.InFlight == InFlightUnknown {
				// The previous attempt may already have submitted an application or
				// sent an email. Repeating that is worse than stopping, so surface the
				// ambiguity instead of resolving it by guessing.
				log.Error("previous attempt unresolved; refusing to repeat a side effect",
					slog.String("idempotency_key", key))
				rt.deadLetter(ctx, reg, msg, env, "outcome_unknown", ErrOutcomeUnknown)
				return "outcome_unknown"
			}
			log.Warn("previous attempt unresolved; retrying idempotent work",
				slog.String("idempotency_key", key))
		}
	}

	handlerCtx, cancel := context.WithTimeout(ctx, reg.Timeout)
	defer cancel()

	err := reg.Handler(handlerCtx, env)
	if err == nil {
		if key != "" && rt.idempotency != nil {
			if cerr := rt.idempotency.Complete(ctx, key); cerr != nil {
				// The work succeeded but we failed to record it. Acking anyway is
				// correct: NAKing would redeliver work that has already been done,
				// and the stale in_flight row will be reclaimed when its lease lapses.
				log.Error("work completed but idempotency record not updated",
					slog.Any("error", cerr), slog.String("idempotency_key", key))
			}
		}
		rt.ack(ctx, msg, log)
		return "ok"
	}

	retryable := apperr.IsRetryable(err) && !errors.Is(err, context.Canceled)
	lastAttempt := reg.MaxDeliver > 0 && deliveries >= uint64(reg.MaxDeliver)

	if retryable && !lastAttempt {
		// Release the claim so the retry is not blocked by our own in-flight record.
		// Safe only because a retryable error means no side effect was committed.
		if key != "" && rt.idempotency != nil {
			if rerr := rt.idempotency.Release(ctx, key); rerr != nil {
				log.Error("failed to release idempotency claim before retry", slog.Any("error", rerr))
			}
		}
		log.Warn("handler failed; will retry",
			slog.Any("error", err),
			slog.Duration("retry_in", backoffFor(deliveries)),
		)
		rt.nak(ctx, msg, deliveries, log)
		return "retry"
	}

	reason := "terminal"
	if retryable {
		reason = "max_deliver_exhausted"
	}
	log.Error("handler failed terminally; dead-lettering",
		slog.Any("error", err),
		slog.String("reason", reason),
	)
	rt.deadLetter(ctx, reg, msg, env, reason, err)
	return "dead_lettered"
}

func (rt *Runtime) ack(ctx context.Context, msg jetstream.Msg, log *slog.Logger) {
	if err := msg.Ack(); err != nil {
		// A failed ack means the message will be redelivered; idempotency covers it.
		log.Error("ack failed; message will be redelivered", slog.Any("error", err))
	}
	_ = ctx
}

// redeliveryBackoff is the redelivery schedule. It is also set as the consumer's
// BackOff so a message whose AckWait simply lapses follows the same curve as one that
// was explicitly NAK'd.
var redeliveryBackoff = []time.Duration{
	1 * time.Second,
	5 * time.Second,
	15 * time.Second,
	1 * time.Minute,
	5 * time.Minute,
}

// backoffSchedule returns the backoff entries valid for a given MaxDeliver.
//
// JetStream rejects a consumer whose BackOff is at least as long as MaxDeliver, because
// the last entry could never be used. Trimming here keeps the constraint in one place
// instead of in every registration.
func backoffSchedule(maxDeliver int) []time.Duration {
	if maxDeliver <= 1 {
		// A single delivery never backs off, and an empty schedule is valid.
		return nil
	}
	if maxDeliver-1 < len(redeliveryBackoff) {
		return redeliveryBackoff[:maxDeliver-1]
	}
	return redeliveryBackoff
}

// backoffFor returns the delay before the next delivery attempt. deliveries is
// 1-based, so the first failure uses the first entry.
func backoffFor(deliveries uint64) time.Duration {
	if deliveries < 1 {
		deliveries = 1
	}
	i := int(deliveries) - 1
	if i >= len(redeliveryBackoff) {
		i = len(redeliveryBackoff) - 1
	}
	return redeliveryBackoff[i]
}

// nak schedules redelivery after an explicit delay.
//
// Plain Nak() is documented to ignore the consumer's BackOff and redeliver instantly,
// which turns a failing dependency into a hot loop that hammers it until MaxDeliver is
// exhausted. NakWithDelay is the only form that actually backs off.
func (rt *Runtime) nak(ctx context.Context, msg jetstream.Msg, deliveries uint64, log *slog.Logger) {
	if err := msg.NakWithDelay(backoffFor(deliveries)); err != nil {
		log.Error("nak failed", slog.Any("error", err))
	}
	_ = ctx
}

// deadLetter republishes the message to its domain's dead-letter subject and
// terminates the delivery so JetStream stops retrying it.
func (rt *Runtime) deadLetter(ctx context.Context, reg Registration, msg jetstream.Msg, env *Envelope, reason string, cause error) {
	eventType := "unknown"
	if env != nil {
		eventType = env.Type
	}
	eventsDeadLettered.WithLabelValues(reg.Name, eventType, reason).Inc()

	deliveries := uint64(0)
	if meta, err := msg.Metadata(); err == nil && meta != nil {
		deliveries = meta.NumDelivered
	}

	record := map[string]any{
		"original_subject": msg.Subject(),
		"consumer":         reg.Name,
		"reason":           reason,
		"delivery_count":   deliveries,
		"failed_at":        time.Now().UTC(),
		"last_error":       cause.Error(),
	}
	if env != nil {
		record["envelope"] = env
	} else {
		// Undecodable payloads are preserved as a string so an operator can see what
		// actually arrived; dropping it would make the bug undiagnosable.
		record["raw"] = string(msg.Data())
	}

	subject := DeadLetterSubject(eventType)
	if env == nil {
		subject = subjectPrefix + ".dlq.unknown"
	}

	body, err := json.Marshal(record)
	switch {
	case err != nil:
		rt.log.Error("failed to marshal dead letter record", slog.Any("error", err))
	case rt.publishRaw == nil:
		rt.log.Error("no publisher configured; dead letter not recorded",
			slog.String("subject", subject))
	default:
		pubCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		if perr := rt.publishRaw(pubCtx, subject, body); perr != nil {
			rt.log.Error("failed to publish dead letter",
				slog.String("subject", subject), slog.Any("error", perr))
		}
	}

	// Term rather than Ack: it records that the message was abandoned deliberately,
	// which is visible in JetStream's own consumer stats.
	if terr := msg.Term(); terr != nil {
		rt.log.Error("failed to terminate dead-lettered message", slog.Any("error", terr))
	}
}

// Stop drains every consumer, waiting for in-flight handlers up to the timeout.
func (rt *Runtime) Stop(timeout time.Duration) {
	rt.mu.Lock()
	contexts := rt.contexts
	rt.contexts = nil
	rt.mu.Unlock()

	done := make(chan struct{})
	go func() {
		for _, c := range contexts {
			// Drain stops new fetches and lets in-flight messages finish, which is
			// what makes a rolling deploy not abandon work mid-flight.
			c.Drain()
		}
		close(done)
	}()

	select {
	case <-done:
		rt.log.Info("consumer runtime drained", slog.Int("consumers", len(contexts)))
	case <-time.After(timeout):
		rt.log.Warn("consumer drain timed out; stopping anyway",
			slog.Duration("timeout", timeout),
			slog.Int("consumers", len(contexts)),
		)
		for _, c := range contexts {
			c.Stop()
		}
	}
}
