package stats

import (
	"slices"
	"testing"
)

// A stat's reservoir draws from the top-level math/rand/v2 generator
// (RANDUPDATE.md S5). Every stat drew from its own Go 1 source seeded with 1,
// so any two stats fed the same values kept the same reservoir. Two stats fed
// one stream past the reservoir's cap now keep different reservoirs.
func TestStatReservoirIsNotSeededWithAFixedValue(t *testing.T) {
	reservoir := func() []float64 {
		s := newStat()
		for i := range 2 * reservoirCap {
			s.add(float64(i))
		}
		return s.res
	}
	if first, second := reservoir(), reservoir(); slices.Equal(first, second) {
		t.Fatal("two stats fed one stream kept the same reservoir")
	}
}
