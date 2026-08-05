// Package embed provides the optional text embedder that turns memory bodies and search queries
// into vectors, so memories can be found by meaning rather than by the words they happen to use.
//
// It is deliberately only half of semantic search. This package produces vectors and nothing else:
// it does not store them, index them, or search them. That separation is not tidiness - the two
// halves have genuinely different constraints. Embedding is a slow, fallible network call to a
// model server, best-effort and never on the critical path of a write; storing and searching
// vectors is a datastore concern whose right implementation depends on which store a deployment
// runs. Keeping them apart means the store can change without touching this, and an embedding can
// fail without the memory failing.
//
// Disabled by default: the no-op implementation reports Enabled() false and returns ErrDisabled,
// so a deployment that has not configured an embedder behaves exactly as it did before this
// existed.
package embed

import (
	"context"
	"errors"
	"math"
)

// ErrDisabled is returned by Embed when no embedder is configured.
var ErrDisabled = errors.New("embedding is not enabled (ollama.embedding.enabled is false)")

// Vector is one embedding: a dense vector of float32.
//
// float32 rather than float64 throughout, because that is what every embedding model actually
// produces and what every vector store accepts. Carrying float64 would double the memory and the
// wire size of a corpus of these for no precision that exists in the source data.
type Vector []float32

// Embedder turns text into vectors. Both stored memory bodies and the queries searched against
// them go through the same implementation, because a query vector and a document vector are only
// comparable when the same model produced them - which is also why Model() is part of the contract
// rather than a configuration detail the caller is trusted to remember.
type Embedder interface {
	// Embed returns one vector per input text, in the same order. An empty input returns no
	// vectors and no error. It must respect the context's deadline, and returns ErrDisabled when
	// no embedder is configured.
	Embed(ctx context.Context, texts []string) ([]Vector, error)

	// Model reports the model tag vectors are being produced with. Vectors from different models
	// are not comparable - the dimensions do not even necessarily match - so a caller that stores
	// vectors must store this alongside them and refuse to compare across a change.
	Model() string

	// Enabled reports whether a real embedder is configured; the no-op returns false.
	Enabled() bool
}

// noop is the disabled implementation.
type noop struct{}

// NewNoop returns the disabled embedder.
func NewNoop() Embedder {
	return noop{}
}

func (noop) Embed(ctx context.Context, texts []string) ([]Vector, error) {
	return nil, ErrDisabled
}

func (noop) Model() string {
	return ""
}

func (noop) Enabled() bool {
	return false
}

// Compile-time check that noop satisfies Embedder.
var _ Embedder = noop{}

// Cosine returns the cosine similarity of two vectors, in -1..1, with 1 meaning identical
// direction. It reports 0 for mismatched or empty vectors, and for a zero vector, rather than
// NaN: a similarity that cannot be computed must not poison a ranking with a value that compares
// false against everything including itself.
//
// Vectors from most embedding models are already unit length, for which this reduces to the dot
// product - but normalising here rather than assuming it keeps the function correct for models
// that do not, at the cost of two square roots.
func Cosine(a Vector, b Vector) float64 {
	if len(a) == 0 || len(a) != len(b) {
		return 0
	}

	var dot, normA, normB float64

	for i := range a {
		x, y := float64(a[i]), float64(b[i])

		dot += x * y
		normA += x * x
		normB += y * y
	}

	if normA == 0 || normB == 0 {
		return 0
	}

	return dot / (math.Sqrt(normA) * math.Sqrt(normB))
}
