package events

import (
	"context"
	"fmt"
	"time"

	"github.com/nats-io/nats.go/jetstream"
)

// StreamSpec describes one stream's desired configuration.
type StreamSpec struct {
	Name      string
	Subjects  []string
	Retention jetstream.RetentionPolicy
	MaxAge    time.Duration
}

// Streams returns the desired stream topology.
//
// Retention is chosen per domain by how reconstructible the data is, not uniformly.
// JOBS and MATCHING are high-volume and fully replayable from raw_job_payloads, so
// three days is ample. APPLICATIONS and OUTREACH carry consent-relevant history, and
// BILLING must outlive a payment dispute, so they are kept far longer.
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
			Retention: jetstream.WorkQueuePolicy,
			MaxAge:    7 * 24 * time.Hour,
		},
		{
			Name:      "JOBS",
			Subjects:  []string{subjectPrefix + ".jobs.>"},
			Retention: jetstream.WorkQueuePolicy,
			MaxAge:    3 * 24 * time.Hour,
		},
		{
			Name:      "MATCHING",
			Subjects:  []string{subjectPrefix + ".matching.>"},
			Retention: jetstream.WorkQueuePolicy,
			MaxAge:    3 * 24 * time.Hour,
		},
		{
			Name:      "APPLICATIONS",
			Subjects:  []string{subjectPrefix + ".applications.>"},
			Retention: jetstream.WorkQueuePolicy,
			MaxAge:    30 * 24 * time.Hour,
		},
		{
			Name:      "OUTREACH",
			Subjects:  []string{subjectPrefix + ".outreach.>"},
			Retention: jetstream.WorkQueuePolicy,
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
			Name:      "DLQ",
			Subjects:  []string{subjectPrefix + ".dlq.>"},
			Retention: jetstream.LimitsPolicy,
			MaxAge:    90 * 24 * time.Hour,
		},
	}
}

// EnsureStreams creates or updates every stream to match the desired topology.
// Idempotent, so it is safe to run on every process start.
func EnsureStreams(ctx context.Context, js jetstream.JetStream, replicas int) error {
	if replicas < 1 {
		replicas = 1
	}

	for _, spec := range Streams() {
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
