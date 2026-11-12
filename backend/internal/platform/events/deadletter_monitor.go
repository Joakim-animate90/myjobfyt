package events

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/nats-io/nats.go/jetstream"
)

// DeadLetterMonitorConfig bounds the depth monitor.
type DeadLetterMonitorConfig struct {
	// Interval between readings. One stream-info call, so this is cheap.
	Interval time.Duration
}

func (c DeadLetterMonitorConfig) withDefaults() DeadLetterMonitorConfig {
	if c.Interval <= 0 {
		c.Interval = 30 * time.Second
	}
	return c
}

// DeadLetterMonitor publishes the dead-letter stream's depth as a gauge.
//
// A dead letter is a message the platform has given up on, which by definition nothing
// will process. The counter of arrivals cannot be alerted on usefully — it only ever
// increases, so a threshold on it fires for ever after one incident — and NATS's own
// metrics describe the stream without knowing which of our streams is the one holding
// abandoned candidate work. This reports the depth so "anything at all is waiting for a
// human" is a single expression.
type DeadLetterMonitor struct {
	js     jetstream.JetStream
	stream string
	log    *slog.Logger
}

// NewDeadLetterMonitor builds the monitor for the DLQ stream.
func NewDeadLetterMonitor(js jetstream.JetStream, log *slog.Logger) *DeadLetterMonitor {
	return &DeadLetterMonitor{js: js, stream: dlqStreamName, log: log}
}

// Run reports the depth until ctx is cancelled. Returns nil on cancellation.
//
// Blocks, so callers run it in a goroutine.
func (m *DeadLetterMonitor) Run(ctx context.Context, cfg DeadLetterMonitorConfig) error {
	cfg = cfg.withDefaults()

	// Report once immediately: waiting a full interval would leave the gauge absent at
	// startup, and an alert on an absent series behaves differently from one on zero.
	m.report(ctx)

	ticker := time.NewTicker(cfg.Interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			m.report(ctx)
		}
	}
}

// report reads the depth and updates the gauges.
func (m *DeadLetterMonitor) report(ctx context.Context) {
	depth, err := m.depth(ctx)
	if err != nil {
		if ctx.Err() != nil {
			return
		}
		// Marked stale rather than zeroed. Zeroing would assert "nothing is waiting",
		// which is precisely what is not known, and would silence the alert.
		eventsDeadLetterDepthStale.WithLabelValues(m.stream).Set(1)
		m.log.WarnContext(ctx, "could not read the dead-letter depth; the gauge is stale",
			slog.String("stream", m.stream), slog.Any("error", err))
		return
	}

	eventsDeadLetterDepthStale.WithLabelValues(m.stream).Set(0)
	eventsDeadLetterDepth.WithLabelValues(m.stream).Set(float64(depth))
}

// depth returns the number of messages currently in the dead-letter stream.
func (m *DeadLetterMonitor) depth(ctx context.Context) (uint64, error) {
	stream, err := m.js.Stream(ctx, m.stream)
	if errors.Is(err, jetstream.ErrStreamNotFound) {
		// Not yet created. Genuinely zero rather than unknown: no stream means no
		// dead letters.
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("inspect stream %s: %w", m.stream, err)
	}

	info, err := stream.Info(ctx)
	if err != nil {
		return 0, fmt.Errorf("stream info %s: %w", m.stream, err)
	}
	return info.State.Msgs, nil
}
