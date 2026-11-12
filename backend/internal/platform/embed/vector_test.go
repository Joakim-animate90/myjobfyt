package embed_test

import (
	"errors"
	"math"
	"testing"

	"github.com/Joakim-animate90/myjobfyt/backend/internal/platform/embed"
)

func TestCosineAgainstKnownVectors(t *testing.T) {
	// Hand-computed cases. Cosine similarity is the arithmetic the whole semantic
	// dimension rests on, so it is checked against values that can be verified by
	// inspection rather than against whatever the implementation happens to return.
	cases := []struct {
		name string
		a, b embed.Vector
		want float64
	}{
		{"identical", embed.Vector{1, 0, 0}, embed.Vector{1, 0, 0}, 1},
		{"orthogonal", embed.Vector{1, 0, 0}, embed.Vector{0, 1, 0}, 0},
		{"opposite", embed.Vector{1, 0, 0}, embed.Vector{-1, 0, 0}, -1},
		// Direction is what matters, not magnitude: a vector and its scaled self are
		// the same direction.
		{"same direction, different magnitude", embed.Vector{1, 1, 0}, embed.Vector{5, 5, 0}, 1},
		{"45 degrees", embed.Vector{1, 0}, embed.Vector{1, 1}, math.Sqrt2 / 2},
		{"60 degrees", embed.Vector{1, 0}, embed.Vector{1, float32(math.Sqrt(3))}, 0.5},
		{"negative components", embed.Vector{-1, -1}, embed.Vector{-1, -1}, 1},
		{"mixed", embed.Vector{3, 4}, embed.Vector{4, 3}, 24.0 / 25.0},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := embed.Cosine(tc.a, tc.b)
			if err != nil {
				t.Fatalf("Cosine: %v", err)
			}
			if math.Abs(got-tc.want) > 1e-6 {
				t.Fatalf("Cosine(%v, %v) = %.6f, want %.6f", tc.a, tc.b, got, tc.want)
			}
		})
	}
}

func TestCosineIsSymmetric(t *testing.T) {
	a := embed.Vector{0.1, -0.5, 0.3, 0.9, -0.2}
	b := embed.Vector{0.4, 0.2, -0.7, 0.1, 0.6}

	forward, err := embed.Cosine(a, b)
	if err != nil {
		t.Fatalf("Cosine(a, b): %v", err)
	}
	reverse, err := embed.Cosine(b, a)
	if err != nil {
		t.Fatalf("Cosine(b, a): %v", err)
	}
	if math.Abs(forward-reverse) > 1e-12 {
		t.Fatalf("Cosine is not symmetric: %.12f vs %.12f", forward, reverse)
	}
}

func TestCosineStaysWithinBounds(t *testing.T) {
	// Floating-point accumulation can push an identical pair a hair past 1.0, which
	// then fails a `<= 1` assertion downstream for no real reason.
	large := make(embed.Vector, 1536)
	for i := range large {
		large[i] = float32(math.Sin(float64(i)))
	}

	got, err := embed.Cosine(large, large)
	if err != nil {
		t.Fatalf("Cosine: %v", err)
	}
	if got > 1 || got < -1 {
		t.Fatalf("Cosine of a vector with itself = %.17f, outside [-1, 1]", got)
	}
	if math.Abs(got-1) > 1e-9 {
		t.Fatalf("Cosine of a vector with itself = %.12f, want 1", got)
	}
}

func TestCosineRejectsADimensionMismatch(t *testing.T) {
	// Almost always a model change. Comparing vectors from two models produces a number
	// that looks valid and means nothing, so it must be an error rather than a score.
	_, err := embed.Cosine(embed.Vector{1, 0}, embed.Vector{1, 0, 0})
	if !errors.Is(err, embed.ErrDimensionMismatch) {
		t.Fatalf("err = %v, want ErrDimensionMismatch", err)
	}
}

func TestCosineRejectsDegenerateVectors(t *testing.T) {
	cases := map[string]embed.Vector{
		"empty":        {},
		"all zeros":    {0, 0, 0},
		"denormal":     {1e-30, 1e-30},
		"contains NaN": {1, float32(math.NaN()), 0},
		"contains Inf": {1, float32(math.Inf(1)), 0},
	}
	for name, v := range cases {
		t.Run(name, func(t *testing.T) {
			// A NaN would propagate silently through the similarity and produce a score
			// that compares false against every threshold, dropping the match for a
			// reason nobody could see.
			if _, err := embed.Cosine(v, v); err == nil {
				t.Fatalf("Cosine accepted a %s vector", name)
			}
		})
	}
}

func TestValidate(t *testing.T) {
	if err := (embed.Vector{0.1, 0.2}).Validate(); err != nil {
		t.Errorf("a valid vector was rejected: %v", err)
	}
	if err := (embed.Vector{}).Validate(); !errors.Is(err, embed.ErrEmptyVector) {
		t.Errorf("empty vector = %v, want ErrEmptyVector", err)
	}
	if err := (embed.Vector{0, 0}).Validate(); !errors.Is(err, embed.ErrZeroVector) {
		t.Errorf("zero vector = %v, want ErrZeroVector", err)
	}
}

func TestMagnitudeAccumulatesInFloat64(t *testing.T) {
	// Summing a thousand squared float32 values in float32 loses meaningful precision,
	// and the norm is the denominator of every similarity computation.
	v := make(embed.Vector, 2048)
	for i := range v {
		v[i] = 0.01
	}
	// Each component contributes 1e-4; 2048 of them gives sqrt(0.2048).
	want := math.Sqrt(2048 * 0.0001)
	if got := v.Magnitude(); math.Abs(got-want) > 1e-6 {
		t.Fatalf("Magnitude = %.9f, want %.9f", got, want)
	}
}

func TestClampUnitFoldsNegativesToZero(t *testing.T) {
	// For matching, "opposite" and "unrelated" are both "not a match". The distinction
	// matters to the similarity function, not to a score the candidate reads.
	cases := map[float64]float64{
		1.0: 1.0, 0.5: 0.5, 0.0: 0.0,
		-0.5: 0.0, -1.0: 0.0,
		1.5: 1.0, -1.5: 0.0,
	}
	for input, want := range cases {
		if got := embed.ClampUnit(input); got != want {
			t.Errorf("ClampUnit(%v) = %v, want %v", input, got, want)
		}
	}
	// A NaN reaching a score would compare false against every threshold and silently
	// drop the match.
	if got := embed.ClampUnit(math.NaN()); got != 0 {
		t.Errorf("ClampUnit(NaN) = %v, want 0", got)
	}
}

func TestKindValidation(t *testing.T) {
	// An unknown kind is rejected by the database CHECK, so it is caught before the
	// call that would produce it.
	for _, k := range []embed.Kind{embed.KindTitle, embed.KindSkills, embed.KindSummary, embed.KindFull} {
		if !k.Valid() {
			t.Errorf("Kind(%q).Valid() = false", k)
		}
	}
	for _, k := range []embed.Kind{"", "experience", "TITLE", "whole"} {
		if embed.Kind(k).Valid() {
			t.Errorf("Kind(%q).Valid() = true, want false", k)
		}
	}
}

func TestMethodDegraded(t *testing.T) {
	if embed.MethodSemantic.Degraded() {
		t.Error("a semantic score was reported degraded")
	}
	if !embed.MethodLexical.Degraded() {
		t.Error("a lexical score was not reported degraded")
	}
}
