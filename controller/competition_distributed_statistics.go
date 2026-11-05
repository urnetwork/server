// Run-level Welch diagnostics for the operational host-admission screen. These
// numerical routines use the same Student-t/beta identities as the frozen scorer,
// without importing or changing its immutable competition-source main package.
package controller

import (
	"errors"
	"math"
)

// Independent run samples, never within-run request counts, determine error.
func compareCompetitionHostMetric(name string, baseline, candidate []float64) (*competitionHostMetricScreen, error) {
	if len(baseline) != 9 || len(candidate) != 9 {
		return nil, errors.New("host admission requires nine independent samples per side")
	}
	meanVariance := func(values []float64) (float64, float64, error) {
		mean := 0.0
		for _, value := range values {
			if math.IsNaN(value) || math.IsInf(value, 0) || value < 0 {
				return 0, 0, errors.New("host admission sample is not finite and nonnegative")
			}
			mean += value
		}
		mean /= float64(len(values))
		if math.IsNaN(mean) || math.IsInf(mean, 0) {
			return 0, 0, errors.New("host admission sample mean overflowed")
		}
		variance := 0.0
		nonconstant := false
		for _, value := range values {
			delta := value - mean
			nonconstant = nonconstant || delta != 0
			variance += delta * delta / float64(len(values)-1)
		}
		if math.IsNaN(variance) || math.IsInf(variance, 0) || nonconstant && variance == 0 {
			return 0, 0, errors.New("host admission sample variance overflowed or underflowed")
		}
		return mean, variance, nil
	}
	baselineMean, baselineVariance, err := meanVariance(baseline)
	if err != nil {
		return nil, err
	}
	candidateMean, candidateVariance, err := meanVariance(candidate)
	if err != nil {
		return nil, err
	}
	difference := candidateMean - baselineMean
	baselineError := baselineVariance / float64(len(baseline))
	candidateError := candidateVariance / float64(len(candidate))
	standardError := math.Sqrt(baselineError + candidateError)
	result := &competitionHostMetricScreen{Metric: name, BaselineValues: append([]float64(nil), baseline...), CandidateValues: append([]float64(nil), candidate...),
		BaselineMean: baselineMean, CandidateMean: candidateMean, BaselineSampleVariance: baselineVariance, CandidateSampleVariance: candidateVariance,
		Difference: difference, StandardError: standardError, DegreesOfFreedom: float64(len(baseline) + len(candidate) - 2),
		DifferenceCiLow: difference, DifferenceCiHigh: difference, TwoSidedPValue: 1}
	if standardError == 0 {
		if difference != 0 {
			result.TwoSidedPValue, result.SignificantShift = 0, true
		}
		return result, nil
	}
	varianceSum := baselineError + candidateError
	denominator := baselineError*baselineError/float64(len(baseline)-1) + candidateError*candidateError/float64(len(candidate)-1)
	result.DegreesOfFreedom = varianceSum * varianceSum / denominator
	if denominator <= 0 || math.IsNaN(result.DegreesOfFreedom) || math.IsInf(result.DegreesOfFreedom, 0) || result.DegreesOfFreedom <= 0 {
		return nil, errors.New("host admission degrees of freedom are unavailable")
	}
	t := math.Abs(difference) / standardError
	result.TwoSidedPValue = 2 * competitionHostStudentSurvival(t, result.DegreesOfFreedom)
	critical := competitionHostStudentCritical(0.025, result.DegreesOfFreedom)
	result.DifferenceCiLow = difference - critical*standardError
	result.DifferenceCiHigh = difference + critical*standardError
	if math.IsNaN(result.TwoSidedPValue) || math.IsInf(result.TwoSidedPValue, 0) || result.TwoSidedPValue < 0 || result.TwoSidedPValue > 1 ||
		math.IsNaN(critical) || math.IsInf(critical, 0) || math.IsNaN(result.DifferenceCiLow) || math.IsNaN(result.DifferenceCiHigh) || math.IsInf(result.DifferenceCiLow, 0) || math.IsInf(result.DifferenceCiHigh, 0) {
		return nil, errors.New("host admission statistical uncertainty did not converge to finite bounds")
	}
	result.SignificantShift = result.TwoSidedPValue <= 0.05
	return result, nil
}

// P(T >= t) for nonnegative t; symmetry is handled by the two-sided caller.
func competitionHostStudentSurvival(t, degreesOfFreedom float64) float64 {
	if math.IsNaN(t) || t < 0 || math.IsNaN(degreesOfFreedom) || math.IsInf(degreesOfFreedom, 0) || degreesOfFreedom <= 0 {
		return math.NaN()
	}
	if math.IsInf(t, 1) {
		return 0
	}
	x := degreesOfFreedom / (degreesOfFreedom + t*t)
	return 0.5 * competitionHostRegularizedBeta(degreesOfFreedom/2, 0.5, x)
}

// Bisection is bounded numerical work, not a retry or statistical policy knob.
func competitionHostStudentCritical(alpha, degreesOfFreedom float64) float64 {
	if math.IsNaN(alpha) || alpha <= 0 || alpha >= 0.5 || math.IsNaN(degreesOfFreedom) || math.IsInf(degreesOfFreedom, 0) || degreesOfFreedom <= 0 {
		return math.NaN()
	}
	low, high := 0.0, 1e6
	for range 200 {
		middle := (low + high) / 2
		p := competitionHostStudentSurvival(middle, degreesOfFreedom)
		if math.IsNaN(p) || p < 0 || p > 1 {
			return math.NaN()
		}
		if p > alpha {
			low = middle
		} else {
			high = middle
		}
		if high-low < 1e-10 {
			return (low + high) / 2
		}
	}
	return math.NaN()
}

// The complement identity chooses the quickly converging beta-fraction side.
func competitionHostRegularizedBeta(a, b, x float64) float64 {
	if math.IsNaN(a) || math.IsNaN(b) || math.IsNaN(x) || math.IsInf(a, 0) || math.IsInf(b, 0) || math.IsInf(x, 0) || a <= 0 || b <= 0 {
		return math.NaN()
	}
	if x <= 0 {
		return 0
	}
	if x >= 1 {
		return 1
	}
	abGamma, _ := math.Lgamma(a + b)
	aGamma, _ := math.Lgamma(a)
	bGamma, _ := math.Lgamma(b)
	front := math.Exp(abGamma - aGamma - bGamma + a*math.Log(x) + b*math.Log1p(-x))
	var result float64
	if x < (a+1)/(a+b+2) {
		result = front * competitionHostBetaFraction(a, b, x) / a
	} else {
		result = 1 - front*competitionHostBetaFraction(b, a, 1-x)/b
	}
	if math.IsNaN(result) || math.IsInf(result, 0) || result < 0 || result > 1 {
		return math.NaN()
	}
	return result
}

// Modified Lentz iteration; constants are mathematical convergence precision.
func competitionHostBetaFraction(a, b, x float64) float64 {
	const epsilon = 1e-14
	const tiny = 1e-300
	ab, aPlus, aMinus := a+b, a+1, a-1
	c := 1.0
	d := 1 - ab*x/aPlus
	if math.Abs(d) < tiny {
		d = tiny
	}
	d = 1 / d
	result := d
	for step := 1; step <= 500; step++ {
		m, twiceM := float64(step), float64(2*step)
		for _, numerator := range []float64{m * (b - m) * x / ((aMinus + twiceM) * (a + twiceM)), -(a + m) * (ab + m) * x / ((a + twiceM) * (aPlus + twiceM))} {
			d = 1 + numerator*d
			if math.Abs(d) < tiny {
				d = tiny
			}
			c = 1 + numerator/c
			if math.Abs(c) < tiny {
				c = tiny
			}
			d = 1 / d
			result *= d * c
		}
		if math.Abs(d*c-1) < epsilon {
			return result
		}
	}
	return math.NaN()
}
