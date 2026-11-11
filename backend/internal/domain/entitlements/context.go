package entitlements

import (
	"context"

	"github.com/Joakim-animate90/myjobfyt/backend/internal/platform/telemetry"
	"github.com/google/uuid"
)

// userFromContext resolves the candidate an AI call is being made for.
//
// Read from the telemetry context rather than passed explicitly, because the AI layer's
// accounting interface is per-client and the candidate varies per call. The same value
// already rides every request and every event envelope for logging, so there is no
// separate plumbing to forget.
func userFromContext(ctx context.Context) (uuid.UUID, bool) {
	raw := telemetry.UserID(ctx)
	if raw == "" {
		return uuid.Nil, false
	}
	id, err := uuid.Parse(raw)
	if err != nil {
		return uuid.Nil, false
	}
	return id, true
}

type reservedKey struct{}

// WithReservedTokens records an estimate so Record can reconcile against it.
func WithReservedTokens(ctx context.Context, tokens int64) context.Context {
	return context.WithValue(ctx, reservedKey{}, tokens)
}

// reservedFromContext reads a recorded estimate.
func reservedFromContext(ctx context.Context) (int64, bool) {
	v, ok := ctx.Value(reservedKey{}).(int64)
	return v, ok
}
