// Synthetic receipts distinguish harmless CPU-dispatched probability rounding
// from changed samples, evidence, uncertainty or admission classifications.
package controller

import (
	"encoding/json"
	"math"
	"testing"
)

// The representation bound is symmetric and cannot cross a statistical gate,
// change exact endpoints or admit a nonfinite/out-of-range probability.
func TestCompetitionHostProbabilityRoundoffBoundary(t *testing.T) {
	for _, value := range []float64{0.01, 0.25, 0.75} {
		for _, direction := range []float64{0, 1} {
			rounded := value
			for range 4 {
				rounded = math.Nextafter(rounded, direction)
			}
			if !competitionHostProbabilityRoundoffMatches(value, rounded) || !competitionHostProbabilityRoundoffMatches(rounded, value) {
				t.Errorf("four representable steps were rejected: %g toward %g", value, direction)
			}
			rounded = math.Nextafter(rounded, direction)
			if competitionHostProbabilityRoundoffMatches(value, rounded) || competitionHostProbabilityRoundoffMatches(rounded, value) {
				t.Errorf("adjacent step outside the bound was accepted: %g toward %g", value, direction)
			}
		}
	}
	for _, values := range [][2]float64{
		{0.05, math.Nextafter(0.05, 1)},
		{math.Nextafter(0.05, 1), 0.05},
		{0, math.SmallestNonzeroFloat64},
		{0, math.Copysign(0, -1)},
		{1, math.Nextafter(1, 0)},
		{math.NaN(), 0.5}, {0.5, math.NaN()},
		{math.Inf(1), math.Inf(1)}, {math.Inf(-1), 0.5},
		{-math.SmallestNonzeroFloat64, 0}, {1, math.Nextafter(1, math.Inf(1))},
	} {
		if competitionHostProbabilityRoundoffMatches(values[0], values[1]) {
			t.Errorf("invalid or classification-changing probability accepted: %v", values)
		}
	}
	for _, value := range []float64{0, 0.05, 1} {
		if !competitionHostProbabilityRoundoffMatches(value, value) {
			t.Errorf("unchanged exact probability rejected: %g", value)
		}
	}
}

// One synthetic raw-score shift produces a nontrivial passing probability;
// the independent throughput exception, when requested, stays visibly failed.
func competitionProbabilityTestReceipt(t *testing.T, excepted bool) (*competitionNoopScreenFixture, *competitionExecutorEquivalenceReceipt) {
	t.Helper()
	var fixture *competitionNoopScreenFixture
	var receipt *competitionExecutorEquivalenceReceipt
	if excepted {
		fixture, receipt = admissionExceptionTestReceipt(t)
	} else {
		fixture = newCompetitionNoopScreenFixture(t)
		var err error
		receipt, err = screenCompetitionExecutorNoop(fixture.settings, fixture.job, fixture.host, fixture.directory, fixture.response, fixture.outcome)
		if err != nil {
			t.Fatal(err)
		}
	}
	metric := receipt.Metrics[0]
	values := append([]float64(nil), metric.CandidateValues...)
	for index := range values {
		values[index]++
	}
	recomputed, err := compareCompetitionHostMetric(metric.Metric, metric.BaselineValues, values)
	if err != nil || recomputed.SignificantShift || recomputed.TwoSidedPValue <= 0 || recomputed.TwoSidedPValue >= 1 {
		t.Fatal("synthetic sample did not produce a nontrivial passing probability", err)
	}
	recomputed.BaselineWithinRunSe, recomputed.CandidateWithinRunSe = metric.BaselineWithinRunSe, metric.CandidateWithinRunSe
	receipt.Metrics[0] = *recomputed
	receipt.RawScoreRelativeDifference = math.Abs(recomputed.Difference) / recomputed.BaselineMean
	sealCompetitionNoopScreenReceipt(t, fixture.settings, receipt)
	return fixture, receipt
}

// The same Go amd64 binary dispatches math.Exp differently on FMA-capable
// CPUs. Four representable probability steps reproduce the retained rejection
// without copying a production identity, sample or diagnostic into this test.
func TestCompetitionDistributedReceiptAcceptsFourUlpProbabilityRoundoff(t *testing.T) {
	for _, excepted := range []bool{false, true} {
		fixture, receipt := competitionProbabilityTestReceipt(t, excepted)
		for range 4 {
			receipt.Metrics[0].TwoSidedPValue = math.Nextafter(receipt.Metrics[0].TwoSidedPValue, 1)
		}
		sealCompetitionNoopScreenReceipt(t, fixture.settings, receipt)
		before, err := json.Marshal(receipt)
		if err != nil {
			t.Fatal(err)
		}
		exceptionDocument := ""
		if excepted {
			exceptionDocument = receipt.Retention.Sha256
		}
		if err := validateCompetitionExecutorReceiptWithException(fixture.settings, &fixture.job.Round, fixture.job.RoundBaseline, fixture.host, receipt, exceptionDocument); err != nil {
			t.Fatalf("retained probability roundoff rejected excepted=%t: %v", excepted, err)
		}
		after, err := json.Marshal(receipt)
		if err != nil || string(before) != string(after) || receipt.Passed == excepted {
			t.Fatalf("validation changed retained evidence or failed judgement excepted=%t: %v", excepted, err)
		}
	}
}

// Rehashing altered metadata cannot expand numerical comparison to samples,
// other diagnostics, evidence identities or an admission outcome.
func TestCompetitionDistributedReceiptRejectsAdjacentProbabilityAndOtherChanges(t *testing.T) {
	for _, change := range []string{"five-ulp", "mean", "variance", "interval", "baseline-sample", "classification", "probability-negative", "probability-over-one", "evidence-digest"} {
		fixture, receipt := competitionProbabilityTestReceipt(t, true)
		switch change {
		case "five-ulp":
			for range 5 {
				receipt.Metrics[0].TwoSidedPValue = math.Nextafter(receipt.Metrics[0].TwoSidedPValue, 1)
			}
		case "mean":
			receipt.Metrics[0].BaselineMean = math.Nextafter(receipt.Metrics[0].BaselineMean, math.Inf(1))
		case "variance":
			receipt.Metrics[0].BaselineSampleVariance = math.Nextafter(receipt.Metrics[0].BaselineSampleVariance, math.Inf(1))
		case "interval":
			receipt.Metrics[0].DifferenceCiHigh = math.Nextafter(receipt.Metrics[0].DifferenceCiHigh, math.Inf(1))
		case "baseline-sample":
			receipt.Metrics[0].BaselineValues[0] = math.Nextafter(receipt.Metrics[0].BaselineValues[0], math.Inf(1))
		case "classification":
			receipt.Metrics[0].SignificantShift = !receipt.Metrics[0].SignificantShift
		case "probability-negative":
			receipt.Metrics[0].TwoSidedPValue = -math.SmallestNonzeroFloat64
		case "probability-over-one":
			receipt.Metrics[0].TwoSidedPValue = math.Nextafter(1, math.Inf(1))
		}
		if change != "evidence-digest" {
			sealCompetitionNoopScreenReceipt(t, fixture.settings, receipt)
		} else {
			receipt.Metrics[0].TwoSidedPValue = math.Nextafter(receipt.Metrics[0].TwoSidedPValue, 1)
		}
		if err := validateCompetitionExecutorReceiptWithException(fixture.settings, &fixture.job.Round, fixture.job.RoundBaseline, fixture.host, receipt, receipt.Retention.Sha256); err == nil {
			t.Errorf("changed retained evidence was accepted: %s", change)
		}
	}
}
