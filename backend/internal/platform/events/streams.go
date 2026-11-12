package events

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/nats-io/nats.go/jetstream"
)

// dlqStreamName is the dead-letter stream. Named once so the topology and anything
// observing it cannot drift apart.
const dlqStreamName = "DLQ"

// StreamSpec describes one stream's desired configuration.
type StreamSpec struct {
	Name      string
	Subjects  []string
	Retention jetstream.RetentionPolicy
	MaxAge    time.Duration
}

// Streams returns the desired stream topology.
//
// Every domain stream uses interest retention, because these are fan-out event streams
// rather than work queues. A work-queue stream delivers each message to exactly one
// consumer and therefore forbids two consumers from filtering the same subject, which
// is incompatible with the pipeline: cv.parsed is consumed independently by skill
// normalisation, by embedding and (later) by matching, and each of those needs its own
// copy. Choosing WorkQueuePolicy here failed at startup with
//
//	filtered consumer not unique on workqueue stream
//
// as soon as the second cv.parsed consumer was registered. Interest retention gives
// each consumer its own copy while still reclaiming storage once every registered
// consumer has acked, so it keeps the footprint of a work queue without the exclusivity.
//
// Concurrency between workers competing for the same message is a consumer-level
// property, not a stream-level one: a durable consumer shared by several worker
// replicas already distributes messages among them.
//
// MaxAge is chosen per domain by how reconstructible the data is, not uniformly. JOBS
// and MATCHING are high-volume and fully replayable from raw_job_payloads, so three
// days is ample. APPLICATIONS and OUTREACH carry consent-relevant history, and BILLING
// must outlive a payment dispute, so they are kept far longer.
func Streams() []StreamSpec {
	return []StreamSpec{
		{
			Name:      "IDENTITY",
			Subjects:  []string{subjectPrefix + ".identity.>"},
			Retention: jetstream.InterestPolicy,
			MaxAge:    7 * 24 * time.Hour,
		},
		{
			Name:      "CV",
			Subjects:  []string{subjectPrefix + ".cv.>"},
			Retention: jetstream.InterestPolicy,
			MaxAge:    7 * 24 * time.Hour,
		},
		{
			Name:      "JOBS",
			Subjects:  []string{subjectPrefix + ".jobs.>"},
			Retention: jetstream.InterestPolicy,
			MaxAge:    3 * 24 * time.Hour,
		},
		{
			Name:      "MATCHING",
			Subjects:  []string{subjectPrefix + ".matching.>"},
			Retention: jetstream.InterestPolicy,
			MaxAge:    3 * 24 * time.Hour,
		},
		{
			Name:      "APPLICATIONS",
			Subjects:  []string{subjectPrefix + ".applications.>"},
			Retention: jetstream.InterestPolicy,
			MaxAge:    30 * 24 * time.Hour,
		},
		{
			Name:      "OUTREACH",
			Subjects:  []string{subjectPrefix + ".outreach.>"},
			Retention: jetstream.InterestPolicy,
			MaxAge:    30 * 24 * time.Hour,
		},
		{
			Name:      "BILLING",
			Subjects:  []string{subjectPrefix + ".billing.>"},
			Retention: jetstream.InterestPolicy,
			MaxAge:    90 * 24 * time.Hour,
		},
		{
			// Dead letters use limits retention so they persist until an operator has
			// dealt with them. A work-queue policy would delete the evidence as soon
			// as it was read, which is the opposite of what a dead letter is for.
			Name:      dlqStreamName,
			Subjects:  []string{subjectPrefix + ".dlq.>"},
			Retention: jetstream.LimitsPolicy,
			MaxAge:    90 * 24 * time.Hour,
		},
	}
}

// ErrRetentionMismatch reports a stream whose retention policy differs from the
// desired topology. JetStream cannot change retention in place, and correcting it means
// deleting the stream, so this is surfaced rather than repaired.
type ErrRetentionMismatch struct {
	Stream string
	Have   jetstream.RetentionPolicy
	Want   jetstream.RetentionPolicy
}

func (e *ErrRetentionMismatch) Error() string {
	return fmt.Sprintf(
		"stream %s has %s retention but the topology expects %s; "+
			"JetStream cannot change retention in place, so this stream must be deleted "+
			"and recreated (`nats stream rm %s`). Deleting it discards any messages not "+
			"yet acked by every consumer, so drain the workers first",
		e.Stream, retentionName(e.Have), retentionName(e.Want), e.Stream,
	)
}

// retentionName renders a retention policy for operators. The jetstream constants
// stringify as integers, which tells nobody anything.
func retentionName(p jetstream.RetentionPolicy) string {
	switch p {
	case jetstream.LimitsPolicy:
		return "limits"
	case jetstream.InterestPolicy:
		return "interest"
	case jetstream.WorkQueuePolicy:
		return "workqueue"
	default:
		return fmt.Sprintf("unknown(%d)", int(p))
	}
}

// EnsureStreams creates or updates every stream to match the desired topology.
// Idempotent, so it is safe to run on every process start.
//
// A retention change is reported as an error rather than applied. JetStream rejects an
// in-place retention update, and the only way to apply one is to delete the stream,
// which would discard every message that has not yet been acked by all of its
// consumers. For a CV pipeline that means losing a candidate's upload mid-flight, so
// the decision belongs to an operator who can drain the workers first.
func EnsureStreams(ctx context.Context, js jetstream.JetStream, replicas int) error {
	if replicas < 1 {
		replicas = 1
	}

	for _, spec := range Streams() {
		if err := checkRetention(ctx, js, spec); err != nil {
			return err
		}

		cfg := jetstream.StreamConfig{
			Name:      spec.Name,
			Subjects:  spec.Subjects,
			Retention: spec.Retention,
			MaxAge:    spec.MaxAge,
			Storage:   jetstream.FileStorage,
			Replicas:  replicas,
			// Discard oldest on overflow: for a pipeline, losing the oldest unprocessed
			// message is strictly better than refusing new publishes and stalling the
			// producing request path.
			Discard:    jetstream.DiscardOld,
			Duplicates: 2 * time.Minute,
		}

		if _, err := js.CreateOrUpdateStream(ctx, cfg); err != nil {
			return fmt.Errorf("ensure stream %s: %w", spec.Name, err)
		}
	}
	return nil
}

// checkRetention reports a mismatch between an existing stream's retention and the
// desired topology. A stream that does not exist yet is not a mismatch: it is about to
// be created with the right policy.
func checkRetention(ctx context.Context, js jetstream.JetStream, spec StreamSpec) error {
	stream, err := js.Stream(ctx, spec.Name)
	if errors.Is(err, jetstream.ErrStreamNotFound) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect stream %s: %w", spec.Name, err)
	}

	info, err := stream.Info(ctx)
	if err != nil {
		return fmt.Errorf("stream info %s: %w", spec.Name, err)
	}
	if info.Config.Retention != spec.Retention {
		return &ErrRetentionMismatch{
			Stream: spec.Name,
			Have:   info.Config.Retention,
			Want:   spec.Retention,
		}
	}
	return nil
}
