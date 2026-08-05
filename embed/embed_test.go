package embed

import (
	"context"
	"errors"
	"math"
	"testing"
)

func TestNoopEmbedder(t *testing.T) {
	idx := NewNoop()

	if idx.Enabled() {
		t.Error("the no-op embedder reports itself enabled")
	}

	if idx.Model() != "" {
		t.Errorf("the no-op embedder reports model %q, want empty", idx.Model())
	}

	if _, err := idx.Embed(context.Background(), []string{"anything"}); !errors.Is(err, ErrDisabled) {
		t.Errorf("Embed on the no-op: got %v, want ErrDisabled", err)
	}
}

func TestCosine(t *testing.T) {
	tests := []struct {
		name string
		a    Vector
		b    Vector
		want float64
	}{
		{name: "identical", a: Vector{1, 0, 0}, b: Vector{1, 0, 0}, want: 1},
		{name: "orthogonal", a: Vector{1, 0}, b: Vector{0, 1}, want: 0},
		{name: "opposite", a: Vector{1, 0}, b: Vector{-1, 0}, want: -1},

		// Direction is what matters, not length: an unnormalised vector must score the same as its
		// unit form, or a model that does not normalise its output would rank by magnitude.
		{name: "scale invariant", a: Vector{3, 4}, b: Vector{6, 8}, want: 1},
		{name: "partial", a: Vector{1, 1}, b: Vector{1, 0}, want: math.Sqrt2 / 2},

		// A similarity that cannot be computed must be 0, never NaN - a NaN compares false against
		// everything including itself and would corrupt any sort it reached.
		{name: "mismatched lengths", a: Vector{1, 2, 3}, b: Vector{1, 2}, want: 0},
		{name: "zero vector", a: Vector{0, 0}, b: Vector{1, 1}, want: 0},
		{name: "both zero", a: Vector{0, 0}, b: Vector{0, 0}, want: 0},
		{name: "empty", a: Vector{}, b: Vector{}, want: 0},
		{name: "nil", a: nil, b: nil, want: 0},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := Cosine(test.a, test.b)

			if math.IsNaN(got) {
				t.Fatalf("Cosine(%v, %v) returned NaN", test.a, test.b)
			}

			if math.Abs(got-test.want) > 1e-6 {
				t.Errorf("Cosine(%v, %v) = %f, want %f", test.a, test.b, got, test.want)
			}
		})
	}
}

// Cosine is the comparison a brute-force search would sort on, so it must actually order things:
// the nearer vector has to score higher than the further one.
func TestCosineOrdersByCloseness(t *testing.T) {
	query := Vector{1, 0}
	near := Vector{0.9, 0.1}
	far := Vector{0.1, 0.9}

	if Cosine(query, near) <= Cosine(query, far) {
		t.Errorf("the nearer vector scored %f, not above the further one's %f",
			Cosine(query, near), Cosine(query, far))
	}
}
