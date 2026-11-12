// Package events is the JetStream event backbone.
//
// The contract implemented here is specified in docs/EVENT_CATALOG.md: one envelope for
// every message, deterministic idempotency keys, bounded redelivery, and a dead-letter
// subject per domain. Delivery is at-least-once, so every handler must be idempotent —
// that is not a quality goal, it is a correctness requirement, and the Idempotency
// store exists to make it achievable without each handler reinventing it.
package events

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/oklog/ulid/v2"
)

// Envelope wraps every published message. Only Payload varies by event type.
type Envelope struct {
	EventID       string    `json:"event_id"`
	Type          string    `json:"type"`
	Version       int       `json:"version"`
	OccurredAt    time.Time `json:"occurred_at"`
	CorrelationID string    `json:"correlation_id"`
	CausationID   string    `json:"causation_id,omitempty"`
	UserID        string    `json:"user_id,omitempty"`
	// Trace carries W3C trace context explicitly. OpenTelemetry context does not
	// survive a message broker on its own; without this field the admin's end-to-end
	// trace would silently break at every asynchronous hop.
	Trace   map[string]string `json:"trace,omitempty"`
	Payload json.RawMessage   `json:"payload"`
}

// Meta describes the envelope fields a publisher supplies, excluding the payload.
type Meta struct {
	CorrelationID string
	CausationID   string
	UserID        string
	Trace         map[string]string
}

// NewEnvelope builds an envelope around a payload.
func NewEnvelope(eventType string, version int, meta Meta, payload any) (*Envelope, error) {
	raw, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("marshal payload for %s: %w", eventType, err)
	}

	eventID := ulid.Make().String()
	correlationID := meta.CorrelationID
	if correlationID == "" {
		// A message published outside any request still needs a correlation id;
		// falling back to the event id keeps every message joinable to a chain.
		correlationID = eventID
	}

	return &Envelope{
		EventID:       eventID,
		Type:          eventType,
		Version:       version,
		OccurredAt:    time.Now().UTC(),
		CorrelationID: correlationID,
		CausationID:   meta.CausationID,
		UserID:        meta.UserID,
		Trace:         meta.Trace,
		Payload:       raw,
	}, nil
}

// Decode unmarshals the payload into dst.
func (e *Envelope) Decode(dst any) error {
	if err := json.Unmarshal(e.Payload, dst); err != nil {
		return fmt.Errorf("decode payload of %s (%s): %w", e.Type, e.EventID, err)
	}
	return nil
}

// Subject returns the JetStream subject for this event type.
func (e *Envelope) Subject() string { return SubjectFor(e.Type) }

// DerivedMeta returns the Meta a handler should use when publishing a follow-on event,
// so the correlation chain and trace context are carried forward automatically rather
// than depending on every handler remembering to copy them.
func (e *Envelope) DerivedMeta() Meta {
	return Meta{
		CorrelationID: e.CorrelationID,
		CausationID:   e.EventID,
		UserID:        e.UserID,
		// Trace is deliberately left empty rather than copied from this envelope.
		//
		// The incoming headers name the span that published THIS event — for a CV, the
		// HTTP request that accepted the upload. Copying them forward made every stage
		// a direct child of that one span: the chain still shared a trace id, but it
		// appeared as a flat fan of siblings under a span that had already ended, so
		// the order of the pipeline and the time between stages were both invisible.
		//
		// PublishWithMeta fills this from the live context instead, where the active
		// span is the consumer currently running. That keeps the trace id (the consumer
		// extracted it from these very headers) while making each stage a child of the
		// stage that caused it.
	}
}

// ValidateShape reports whether this decoded value is actually an envelope.
//
// json.Unmarshal into a struct succeeds for any JSON object, so a message that is valid
// JSON but not an envelope — a producer with a stale schema, something hand-published
// during an incident — decodes into a struct with every field empty and no error. It
// then flows through the pipeline with no type, and the dead-letter subject derived
// from that empty type has an empty domain token, which matches no stream: the message
// is terminated and its dead letter silently dropped.
//
// Type is the field that must be present: it selects the subject, the domain and the
// dead-letter queue. EventID is required too, because it is the idempotency and
// causation identity — without it a redelivery cannot be recognised as one.
func (e *Envelope) ValidateShape() error {
	switch {
	case e.Type == "":
		return fmt.Errorf("envelope has no type; this is not a MyJobFyt event")
	case e.EventID == "":
		return fmt.Errorf("envelope %q has no event id", e.Type)
	}
	return nil
}
