// Package embed provides semantic similarity for matching, with a lexical fallback that
// keeps matching working when no embedding provider is available.
//
// The fallback is not a degraded afterthought — it is the always-available path. There
// is no embeddings endpoint on the Claude API, so embeddings come from a self-hosted
// model (see docs/INTEGRATION_ANALYSIS.md), which means "no provider configured" is the
// normal state in development and a real possibility in production. A matching engine
// that stops working without embeddings is a matching engine that stops working.
package embed

import (
	"errors"
	"fmt"
	"math"
)

// Vector is an embedding.
//
// float32 rather than float64: it halves storage and the precision loss is far below
// the noise floor of the similarity threshold. The database column is real[], which is
// float32, so using float64 here would convert on every read and write for nothing.
type Vector []float32

// Kind names what a vector represents.
//
// Separate vectors per aspect rather than one for the whole CV, because the dimensions
// are compared independently: a candidate's title should be compared to the posting's
// title, not to its benefits section. One blended vector would make every comparison
// mushier than the one before it.
type Kind string

const (
	// KindTitle embeds the candidate's current or most recent title.
	KindTitle Kind = "title"
	// KindSkills embeds the candidate's skill set as a whole.
	KindSkills Kind = "skills"
	// KindSummary embeds the CV's own summary section.
	KindSummary Kind = "summary"
	// KindFull embeds the whole CV, for coarse relevance.
	KindFull Kind = "full"
)

// Valid reports whether k is one of the kinds the database CHECK permits. An unknown
// kind would be rejected at write time, so it is caught before the call that produces it.
func (k Kind) Valid() bool {
	switch k {
	case KindTitle, KindSkills, KindSummary, KindFull:
		return true
	default:
		return false
	}
}

// Errors returned by this package.
var (
	// ErrDimensionMismatch means two vectors cannot be compared.
	//
	// Almost always a model change: vectors written by one model have different
	// dimensions from another's, and comparing them produces a number that looks valid
	// and means nothing. Hence an error rather than a zero score.
	ErrDimensionMismatch = errors.New("vectors have different dimensions")
	// ErrZeroVector means a vector has no magnitude, so its direction is undefined and
	// cosine similarity is not a meaningful question.
	ErrZeroVector = errors.New("vector has zero magnitude")
	// ErrEmptyVector means a vector has no components at all.
	ErrEmptyVector = errors.New("vector is empty")
	// ErrUnavailable means the embedding provider could not be reached. Callers fall
	// back to lexical similarity rather than failing.
	ErrUnavailable = errors.New("embedding provider unavailable")
)

// Dims returns the vector's dimensionality.
func (v Vector) Dims() int { return len(v) }

// Magnitude returns the Euclidean norm.
//
// Accumulated in float64 even though the components are float32: summing a thousand
// squared float32 values in float32 loses meaningful precision, and the norm is the
// denominator of every similarity computation.
func (v Vector) Magnitude() float64 {
	var sum float64
	for _, component := range v {
		sum += float64(component) * float64(component)
	}
	return math.Sqrt(sum)
}

// IsZero reports whether the vector has no magnitude.
//
// Compared against a small epsilon rather than exactly zero, because a vector of
// denormal values is numerically indistinguishable from zero for this purpose and
// would produce a similarity of ±inf.
func (v Vector) IsZero() bool { return v.Magnitude() < 1e-12 }

// Validate checks a vector is usable.
func (v Vector) Validate() error {
	if len(v) == 0 {
		return ErrEmptyVector
	}
	for i, component := range v {
		// A NaN or Inf propagates silently through cosine similarity and produces a
		// score that compares false against every threshold, so a match would be
		// dropped for a reason nobody could see.
		if math.IsNaN(float64(component)) || math.IsInf(float64(component), 0) {
			return fmt.Errorf("component %d is not a finite number (%v)", i, component)
		}
	}
	if v.IsZero() {
		return ErrZeroVector
	}
	return nil
}

// Cosine returns the cosine similarity of two vectors, in [-1, 1].
//
// Not normalised to [0, 1]: a negative similarity is a real signal — the texts point in
// opposite directions — and folding it into zero would make "unrelated" and "opposite"
// indistinguishable. Callers that need a 0-1 score clamp at the point of use, where the
// decision is visible.
func Cosine(a, b Vector) (float64, error) {
	if len(a) != len(b) {
		return 0, fmt.Errorf("%w: %d and %d", ErrDimensionMismatch, len(a), len(b))
	}
	if err := a.Validate(); err != nil {
		return 0, fmt.Errorf("first vector: %w", err)
	}
	if err := b.Validate(); err != nil {
		return 0, fmt.Errorf("second vector: %w", err)
	}

	var dot, normA, normB float64
	for i := range a {
		x, y := float64(a[i]), float64(b[i])
		dot += x * y
		normA += x * x
		normB += y * y
	}

	similarity := dot / (math.Sqrt(normA) * math.Sqrt(normB))

	// Floating-point accumulation can push an identical pair a hair past 1.0, which
	// then fails a `<= 1` assertion downstream for no real reason.
	switch {
	case similarity > 1:
		return 1, nil
	case similarity < -1:
		return -1, nil
	default:
		return similarity, nil
	}
}

// Similarity is a comparison result, carrying how it was produced.
//
// The method matters to the caller: a semantic score and a lexical score are not
// interchangeable, and a match scored lexically must be marked degraded so it can be
// rescored when the provider returns.
type Similarity struct {
	// Score is in [0, 1].
	Score float64
	// Method records how the score was produced.
	Method Method
}

// Method names how a similarity was computed.
type Method string

const (
	// MethodSemantic means embeddings were compared.
	MethodSemantic Method = "semantic"
	// MethodLexical means the fallback was used. A match scored this way is marked
	// degraded and rescored when embeddings become available.
	MethodLexical Method = "lexical"
)

// Degraded reports whether a score came from the fallback.
func (m Method) Degraded() bool { return m == MethodLexical }

// ClampUnit folds a cosine similarity into [0, 1] for use as a score component.
//
// Negative similarities become zero: for matching purposes "opposite" and "unrelated"
// are both "not a match", and the distinction only matters to the similarity function
// itself, not to a score the candidate reads.
func ClampUnit(similarity float64) float64 {
	switch {
	case math.IsNaN(similarity):
		// Should be impossible after Validate, but a NaN reaching a score would
		// compare false against every threshold and silently drop the match.
		return 0
	case similarity < 0:
		return 0
	case similarity > 1:
		return 1
	default:
		return similarity
	}
}
