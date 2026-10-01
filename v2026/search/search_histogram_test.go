package search

import (
	"strings"
	"testing"
)

// The legacy predicate is a lower bound, not an edit-distance replacement.
// Preserve its choice of the larger distinct-rune set, including equal-size
// direction, repeated characters, raw Unicode and malformed UTF-8.
func legacyHistoDistanceForTest(a, b string, distance int) bool {
	counts := func(s string) map[rune]int {
		h := map[rune]int{}
		for _, r := range s {
			h[r]++
		}
		return h
	}
	x, y := counts(a), counts(b)
	if len(x) < len(y) {
		x, y = y, x
	}
	d := 0
	for r, n := range x {
		if y[r] < n {
			d += n - y[r]
			if distance < d {
				return false
			}
		}
	}
	return true
}

func TestCompactHistogramPreservesLegacyPredicate(t *testing.T) {
	values := []string{""}
	level := []string{""}
	for range 4 {
		next := []string{}
		for _, prefix := range level {
			for _, r := range []rune{'a', 'b', 'é'} {
				next = append(next, prefix+string(r))
			}
		}
		values = append(values, next...)
		level = next
	}
	values = append(values, "\xff", "\xffa", "界a界", "a\u0301", strings.Repeat("a", 1024))
	for _, a := range values {
		for _, b := range values {
			x, y := createHisto(a), createHisto(b)
			for distance := -1; distance <= 6; distance++ {
				if got, want := minHistoDistance(x, y, distance), legacyHistoDistanceForTest(a, b, distance); got != want {
					t.Fatalf("histogram parity changed for %q/%q at distance%d", a, b, distance)
				}
			}
		}
	}
}
