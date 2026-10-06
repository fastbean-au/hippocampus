package hippocampus

import (
	"fmt"
	"math"
	"testing"
)

// TestDecayIsMonotoneForEveryMethod (TODO-3 item 167): days_until_forgotten and the decay curve are
// found by bisecting calculateValue (explain.go), which is only sound if every method's value never
// rises with age. Nothing checked that across the six methods. It also pins the two other properties
// the consolidation decision leans on: the value is finite (a NaN fails every comparison, which
// corrupts eviction's sort) and never falls as significance rises.
//
// The grid covers the accepted ranges: aggressiveness above 0 (above 1/e for method 3, which startup
// refuses otherwise), significance from 1 up, and ages from a fraction of a unit to far past any
// configured horizon.
func TestDecayIsMonotoneForEveryMethod(t *testing.T) {
	aggressivenesses := []float64{0.05, 0.37, 0.5, 1, 1.5, 2, 5, 10, 50}
	significances := []float64{1, 2, 10, 100, 1e4, 1e6}
	ages := []float64{1e-6, 0.001, 0.1, 0.5, 1, 1.0001, 2, 10, 100, 1000, 1e5, 1e7}

	for method := 1; method <= 6; method++ {
		for _, aggressiveness := range aggressivenesses {
			if method == 3 && aggressiveness <= math.Exp(-1) {
				continue
			}

			s := &Server{consolidation: Consolidation{method: method, aggressiveness: aggressiveness}}

			t.Run(fmt.Sprintf("method %d aggressiveness %g", method, aggressiveness), func(t *testing.T) {
				for _, significance := range significances {
					previous := math.Inf(1)

					for _, age := range ages {
						value := s.calculateValue(significance, age)

						if math.IsNaN(value) || math.IsInf(value, 0) {
							t.Fatalf("value(significance %g, age %g) = %v, not finite", significance, age, value)
						}

						if value > previous {
							t.Fatalf("value rose with age at significance %g: %g at the previous age, %g at age %g", significance, previous, value, age)
						}

						previous = value
					}
				}

				for _, age := range ages {
					previous := math.Inf(-1)

					for _, significance := range significances {
						value := s.calculateValue(significance, age)

						if value < previous {
							t.Fatalf("value fell as significance rose at age %g: %g then %g at significance %g", age, previous, value, significance)
						}

						previous = value
					}
				}
			})
		}
	}
}
