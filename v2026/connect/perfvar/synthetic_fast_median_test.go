// Three independent five-window means supply one throughput verdict. Every
// iteration must return joined owners and valid evidence before the next starts.
package perfvar

import (
	"errors"
	"fmt"
	"math"
	"sort"
)

const syntheticFastMedianIterationCount = 3

// A callback returns only scalars after its fixture and TestEnv have closed.
// Full per-iteration reports are logged before returning, not retained here.
type syntheticFastIterationSample struct {
	Mean              syntheticFastMeanReport
	OwnersJoined      bool
	EnvironmentClosed bool
}

// A below-minimum sample retains Mean.Valid=false while EvidenceValid=true.
type syntheticFastIterationRecord struct {
	Index             int                     `json:"index"`
	State             string                  `json:"state"`
	Mean              syntheticFastMeanReport `json:"mean"`
	MeanObserved      bool                    `json:"mean_observed"`
	OwnersJoined      bool                    `json:"owners_joined"`
	EnvironmentClosed bool                    `json:"environment_closed"`
	EvidenceValid     bool                    `json:"evidence_valid"`
	MeetsMinimum      bool                    `json:"meets_minimum"`
	Error             string                  `json:"error,omitempty"`
	NotRunReason      string                  `json:"not_run_reason,omitempty"`
}

// Iteration order is preserved; only a three-element index copy is sorted.
type syntheticFastMedianReport struct {
	Schema              string                          `json:"schema"`
	ConsumerTarget      int64                           `json:"consumer_target_bytes"`
	ExpectedIterations  int                             `json:"expected_iterations"`
	AttemptedIterations int                             `json:"attempted_iterations"`
	Iterations          [3]syntheticFastIterationRecord `json:"iterations"`
	MedianMeanMbps      float64                         `json:"median_arithmetic_mean_whole_run_mbps"`
	MinimumMedianMbps   float64                         `json:"minimum_median_mbps"`
	SelectedIteration   int                             `json:"selected_iteration"`
	EvidenceValid       bool                            `json:"evidence_valid"`
	Valid               bool                            `json:"valid"`
	Stop                string                          `json:"stop"`
	Error               string                          `json:"error,omitempty"`
}

// Validate the callback's actual five-window scalar and numeric-error boundary.
// A joined numeric+correctness error is deliberately a hard failure.
func syntheticFastIterationEvidenceError(sample syntheticFastIterationSample, sampleErr error) error {
	if !sample.OwnersJoined || !sample.EnvironmentClosed {
		return errors.Join(sampleErr, errors.New("synthetic Fast iteration owners or environment did not close"))
	}
	mean := sample.Mean
	if mean.MinimumMeanMbps != syntheticFastMinimumMeanMbps {
		return errors.Join(sampleErr, errors.New("synthetic Fast iteration changed its five-window minimum"))
	}
	var expected float64
	for _, rate := range mean.WindowMbps {
		if math.IsNaN(rate) || math.IsInf(rate, 0) || rate <= 0 {
			return errors.Join(sampleErr, errors.New("synthetic Fast iteration omitted a finite positive window"))
		}
		expected += rate / 5
	}
	if math.IsNaN(mean.ArithmeticMeanMbps) || math.IsInf(mean.ArithmeticMeanMbps, 0) || mean.ArithmeticMeanMbps != expected {
		return errors.Join(sampleErr, errors.New("synthetic Fast iteration arithmetic does not match all five windows"))
	}
	if mean.ArithmeticMeanMbps >= syntheticFastMinimumMeanMbps {
		if !mean.Valid || sampleErr != nil {
			return errors.Join(sampleErr, errors.New("synthetic Fast passing iteration has invalid evidence"))
		}
		return nil
	}
	minimumErr, ok := sampleErr.(*syntheticFastMeanMinimumError)
	if !ok || minimumErr == nil || minimumErr.MeanMbps != mean.ArithmeticMeanMbps || mean.Valid {
		return errors.Join(sampleErr, errors.New("synthetic Fast below-minimum iteration is not a numeric-only result"))
	}
	return nil
}

// Execute exactly three fresh iterations, stopping immediately on invalid
// evidence. This is sampling, with no retry, filtering, or fourth replacement.
func collectSyntheticFastMedian(report *syntheticFastMedianReport, execute func(int) (syntheticFastIterationSample, error)) (resultErr error) {
	*report = syntheticFastMedianReport{Schema: "perfvar-public-native-fast-median-v1", ExpectedIterations: syntheticFastMedianIterationCount,
		MinimumMedianMbps: syntheticFastMinimumMeanMbps, Stop: "incomplete"}
	for index := range report.Iterations {
		report.Iterations[index] = syntheticFastIterationRecord{Index: index + 1, State: "NOTRUN", NotRunReason: "earlier_iteration_not_completed"}
	}
	active := -1
	defer func() {
		if active >= 0 {
			row := &report.Iterations[active]
			row.State, row.Error = "invalid", "synthetic Fast iteration did not return after cleanup"
			report.Stop, report.Error = "invalid_iteration", row.Error
		}
	}()
	for index := range report.Iterations {
		active = index
		row := &report.Iterations[index]
		row.State, row.NotRunReason = "running", ""
		report.AttemptedIterations++
		sample, sampleErr := execute(index + 1)
		row.OwnersJoined, row.EnvironmentClosed = sample.OwnersJoined, sample.EnvironmentClosed
		finite := !math.IsNaN(sample.Mean.ArithmeticMeanMbps) && !math.IsInf(sample.Mean.ArithmeticMeanMbps, 0) &&
			!math.IsNaN(sample.Mean.MinimumMeanMbps) && !math.IsInf(sample.Mean.MinimumMeanMbps, 0)
		for _, rate := range sample.Mean.WindowMbps {
			finite = finite && !math.IsNaN(rate) && !math.IsInf(rate, 0)
		}
		if finite {
			row.Mean, row.MeanObserved = sample.Mean, true
		}
		if err := syntheticFastIterationEvidenceError(sample, sampleErr); err != nil {
			row.State, row.Error = "invalid", err.Error()
			report.Stop, report.Error = "invalid_iteration", row.Error
			active = -1
			return err
		}
		row.Mean, row.State, row.EvidenceValid = sample.Mean, "valid", true
		row.MeetsMinimum = sample.Mean.Valid
		if sampleErr != nil {
			row.Error = sampleErr.Error()
		}
		active = -1
	}
	order := []int{0, 1, 2}
	sort.SliceStable(order, func(a, b int) bool {
		return report.Iterations[order[a]].Mean.ArithmeticMeanMbps < report.Iterations[order[b]].Mean.ArithmeticMeanMbps
	})
	report.SelectedIteration = order[1] + 1
	report.MedianMeanMbps = report.Iterations[order[1]].Mean.ArithmeticMeanMbps
	report.EvidenceValid, report.Stop = true, "completed"
	if report.MedianMeanMbps < syntheticFastMinimumMeanMbps {
		resultErr = fmt.Errorf("synthetic Fast median of three five-window arithmetic means %.6f Mbps is below %.0f Mbps", report.MedianMeanMbps, syntheticFastMinimumMeanMbps)
		report.Error = resultErr.Error()
		return resultErr
	}
	report.Valid = true
	return nil
}
