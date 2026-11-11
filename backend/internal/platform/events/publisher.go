package events

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/Joakim-animate90/myjobfyt/backend/internal/config"
	"github.com/Joakim-animate90/myjobfyt/backend/internal/platform/telemetry"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

// Publisher publishes envelopes to JetStream.
type Publisher interface {
	Publish(ctx context.Context, eventType string, version int, payload any) error
	PublishWithMeta(ctx context.Context, eventType string, version int, meta Meta, payload any) error
}

// Bus is the JetStream-backed Publisher plus the handle consumers are built from.
type Bus struct {
	conn *nats.Conn
	js   jetstream.JetStream
	log  *slog.Logger
}

// Connect establishes the NATS connection, initialises JetStream and ensures streams.
func Connect(ctx context.Context, cfg config.NATSConfig, log *slog.Logger) (*Bus, error) {
	conn, err := nats.Connect(cfg.URL,
		nats.Name(cfg.Name),
		nats.MaxReconnects(cfg.MaxReconnects),
		nats.ReconnectWait(cfg.ReconnectWait),
		// Reconnect buffering lets a publish during a brief outage succeed rather than
		// failing the request that triggered it.
		nats.ReconnectBufSize(8*1024*1024),
		nats.DisconnectErrHandler(func(_ *nats.Conn, err error) {
			log.Error("nats disconnected", slog.Any("error", err))
		}),
		nats.ReconnectHandler(func(c *nats.Conn) {
			log.Info("nats reconnected", slog.String("url", c.ConnectedUrl()))
		}),
		nats.ErrorHandler(func(_ *nats.Conn, sub *nats.Subscription, err error) {
			subject := ""
			if sub != nil {
				subject = sub.Subject
			}
			log.Error("nats async error", slog.String("subject", subject), slog.Any("error", err))
		}),
	)
	if err != nil {
		return nil, fmt.Errorf("connect nats: %w", err)
	}

	js, err := jetstream.New(conn)
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("init jetstream: %w", err)
	}

	if err := EnsureStreams(ctx, js, cfg.StreamReplicas); err != nil {
		conn.Close()
		return nil, err
	}

	return &Bus{conn: conn, js: js, log: log}, nil
}

// JetStream exposes the JetStream context for consumer construction.
func (b *Bus) JetStream() jetstream.JetStream { return b.js }

// Close drains and closes the connection. Drain rather than Close so in-flight
// publishes and acks complete instead of being discarded at shutdown.
func (b *Bus) Close() error {
	if err := b.conn.Drain(); err != nil {
		b.conn.Close()
		return err
	}
	return nil
}

// Healthy reports whether the connection is usable, for readiness probes.
func (b *Bus) Healthy() bool { return b.conn.IsConnected() }

// Publish publishes an event, deriving envelope metadata from the context.
func (b *Bus) Publish(ctx context.Context, eventType string, version int, payload any) error {
	return b.PublishWithMeta(ctx, eventType, version, MetaFromContext(ctx), payload)
}

// PublishWithMeta publishes an event with explicit envelope metadata. Handlers use
// this with Envelope.DerivedMeta() so the correlation chain is carried forward.
func (b *Bus) PublishWithMeta(ctx context.Context, eventType string, version int, meta Meta, payload any) error {
	env, err := NewEnvelope(eventType, version, meta, payload)
	if err != nil {
		return err
	}

	body, err := json.Marshal(env)
	if err != nil {
		return fmt.Errorf("marshal envelope for %s: %w", eventType, err)
	}

	// Publishing is bounded independently of the caller's context so a slow broker
	// cannot hold an HTTP request open past its own deadline.
	pubCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()

	// MsgID enables JetStream's own duplicate window as a cheap first line of defence.
	// It does not replace handler idempotency, which must survive longer than the
	// two-minute window and across stream restarts.
	_, err = b.js.Publish(pubCtx, env.Subject(), body, jetstream.WithMsgID(env.EventID))
	if err != nil {
		eventsPublishFailures.WithLabelValues(eventType).Inc()
		return fmt.Errorf("publish %s: %w", eventType, err)
	}

	eventsPublished.WithLabelValues(eventType).Inc()
	b.log.DebugContext(ctx, "event published",
		slog.String("event_type", eventType),
		slog.String("event_id", env.EventID),
		slog.String("correlation_id", env.CorrelationID),
	)
	return nil
}

// MetaFromContext builds envelope metadata from the ambient request context, so an
// event published from a handler is automatically joined to that request's workflow.
func MetaFromContext(ctx context.Context) Meta {
	return Meta{
		CorrelationID: telemetry.CorrelationID(ctx),
		UserID:        telemetry.UserID(ctx),
		Trace:         traceHeadersFromContext(ctx),
	}
}
