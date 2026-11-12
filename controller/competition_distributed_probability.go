// CPU-dispatched transcendental arithmetic may round the same Welch probability
// differently. Representation tolerance never changes an admission decision.
package controller

import "math"

// Four representable steps cover the observed FMA/non-FMA math.Exp difference.
// Endpoints and the frozen significance classification remain exact; every
// other retained statistic, sample, identity and object digest is unchanged.
func competitionHostProbabilityRoundoffMatches(expected, retained float64) bool {
	if math.IsNaN(expected) || math.IsInf(expected, 0) || expected < 0 || expected > 1 ||
		math.IsNaN(retained) || math.IsInf(retained, 0) || retained < 0 || retained > 1 {
		return false
	}
	expectedBits, retainedBits := math.Float64bits(expected), math.Float64bits(retained)
	if expectedBits == retainedBits {
		return true
	}
	if expected == 0 || expected == 1 || retained == 0 || retained == 1 || (expected <= 0.05) != (retained <= 0.05) {
		return false
	}
	if expectedBits < retainedBits {
		expectedBits, retainedBits = retainedBits, expectedBits
	}
	return expectedBits-retainedBits <= 4
}
