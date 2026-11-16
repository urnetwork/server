// Deterministic controls use the same five-window sampler and outer collector
// as the native roots. A sample below the throughput bar stays visible.
package perfvar

import (
	"context"
	"errors"
	"fmt"
	"math"
	"reflect"
	"runtime"
	"testing"

	"github.com/urnetwork/sdk"
)

// All three original sequences run even when the first two would already
// determine the median. Close events precede every later fixture creation.
func TestSyntheticFastMedianRunsThreeJoinedFiveWindowSequences(t *testing.T) {
	for _, row := range []struct {
		name     string
		means    [3]float64
		median   float64
		selected int
		passed   bool
	}{
		{name: "one slow sample", means: [3]float64{100, 205, 210}, median: 205, selected: 2, passed: true},
		{name: "two slow samples", means: [3]float64{190, 195, 1000}, median: 195, selected: 2},
		{name: "inclusive boundary", means: [3]float64{100, 200, 300}, median: 200, selected: 2, passed: true},
		{name: "below boundary without rounding", means: [3]float64{199.998, 199.999, 200}, median: 199.999, selected: 2},
		{name: "two early passes still measure third", means: [3]float64{210, 220, 10}, median: 210, selected: 1, passed: true},
		{name: "two early misses still measure third", means: [3]float64{100, 150, 1000}, median: 150, selected: 2},
		{name: "original order survives selection", means: [3]float64{300, 100, 205}, median: 205, selected: 3, passed: true},
		{name: "stable equal means", means: [3]float64{210, 210, 100}, median: 210, selected: 1, passed: true},
	} {
		var report syntheticFastMedianReport
		var events []string
		alive, created, closed, windows, drains := false, 0, 0, 0, 0
		err := collectSyntheticFastMedian(&report, func(index int) (syntheticFastIterationSample, error) {
			if alive || index != created+1 || closed != created {
				t.Fatalf("%s: a later fixture started before its predecessor closed", row.name)
			}
			created++
			alive = true
			events = append(events, fmt.Sprintf("create%d", index))
			control := syntheticFastMeanControl([5]float64{row.means[index-1], row.means[index-1], row.means[index-1], row.means[index-1], row.means[index-1]})
			sequence, sequenceErr := measureSyntheticFastSequence(context.Background(), control.Runs[0].Workload.Settings, 5, "median-control",
				func(_ context.Context, window int) (syntheticFastReport, error) {
					if !alive || window != windows%5 {
						t.Fatalf("%s: original window order changed", row.name)
					}
					windows++
					events = append(events, fmt.Sprintf("window%d.%d", index, window+1))
					return control.Runs[window].Workload, nil
				}, func(_ context.Context, window int) error {
					drains++
					events = append(events, fmt.Sprintf("drain%d.%d", index, window+1))
					return nil
				}, func() (syntheticFastCumulative, any) { return syntheticFastCumulative{}, nil })
			if sequenceErr != nil {
				t.Fatalf("%s: original sampler failed its control: %v", row.name, sequenceErr)
			}
			mean, meanErr := syntheticFastFiveWindowMean(sequence)
			alive = false
			closed++
			events = append(events, fmt.Sprintf("close%d", index))
			return syntheticFastIterationSample{Mean: mean, OwnersJoined: true, EnvironmentClosed: true}, meanErr
		})
		if (err == nil) != row.passed || report.Valid != row.passed || !report.EvidenceValid || report.Stop != "completed" ||
			report.ExpectedIterations != 3 || report.AttemptedIterations != 3 || created != 3 || closed != 3 || alive || windows != 15 || drains != 15 ||
			math.Abs(report.MedianMeanMbps-row.median) > 1e-9 || report.SelectedIteration != row.selected || report.MinimumMedianMbps != 200 {
			t.Fatalf("%s: wrong complete median verdict: report=%+v err=%v created=%d closed=%d windows=%d drains=%d", row.name, report, err, created, closed, windows, drains)
		}
		var expected []string
		for index := 1; index <= 3; index++ {
			expected = append(expected, fmt.Sprintf("create%d", index))
			for window := 1; window <= 5; window++ {
				expected = append(expected, fmt.Sprintf("window%d.%d", index, window), fmt.Sprintf("drain%d.%d", index, window))
			}
			expected = append(expected, fmt.Sprintf("close%d", index))
		}
		if !reflect.DeepEqual(events, expected) {
			t.Fatalf("%s: measured lifetime/order changed: %v", row.name, events)
		}
		for index, iteration := range report.Iterations {
			meets := row.means[index] >= 200
			if iteration.Index != index+1 || iteration.State != "valid" || !iteration.MeanObserved || !iteration.EvidenceValid ||
				!iteration.OwnersJoined || !iteration.EnvironmentClosed || math.Abs(iteration.Mean.ArithmeticMeanMbps-row.means[index]) > 1e-9 ||
				iteration.MeetsMinimum != meets || iteration.Mean.Valid != meets || (iteration.Error == "") != meets {
				t.Fatalf("%s: sample %d was filtered or its numeric failure changed: %+v", row.name, index+1, iteration)
			}
		}
	}
}

// Missing or invalid evidence never becomes a replacement sample. In
// particular, a joined numeric+cleanup error remains a hard failure.
func TestSyntheticFastMedianStopsAtInvalidIteration(t *testing.T) {
	for _, row := range []struct {
		name   string
		mutate func(*syntheticFastSequenceReport, *syntheticFastIterationSample, *error)
		cause  bool
	}{
		{name: "missing window", mutate: func(s *syntheticFastSequenceReport, _ *syntheticFastIterationSample, _ *error) { s.Runs = s.Runs[:4] }},
		{name: "invalid workload", mutate: func(s *syntheticFastSequenceReport, _ *syntheticFastIterationSample, _ *error) {
			s.Runs[2].Workload.Valid = false
		}},
		{name: "unjoined workload", mutate: func(s *syntheticFastSequenceReport, _ *syntheticFastIterationSample, _ *error) {
			s.Runs[2].Workload.Joined = false
		}},
		{name: "unjoined requests", mutate: func(s *syntheticFastSequenceReport, _ *syntheticFastIterationSample, _ *error) {
			s.Runs[2].RequestsQuiescent = false
		}},
		{name: "nonfinite window", mutate: func(s *syntheticFastSequenceReport, _ *syntheticFastIterationSample, _ *error) {
			s.Runs[2].Workload.WholeRunMbps = math.Inf(1)
		}},
		{name: "probe reaches hard limit", mutate: func(s *syntheticFastSequenceReport, _ *syntheticFastIterationSample, _ *error) {
			s.Runs[2].Workload.LoadedLatencyNs[0] = int64(syntheticFastNavigationLimit)
		}},
		{name: "owners not joined", mutate: func(_ *syntheticFastSequenceReport, s *syntheticFastIterationSample, _ *error) {
			s.OwnersJoined = false
		}},
		{name: "environment not closed", mutate: func(_ *syntheticFastSequenceReport, s *syntheticFastIterationSample, _ *error) {
			s.EnvironmentClosed = false
		}},
		{name: "partial evidence and hard cause", cause: true, mutate: func(s *syntheticFastSequenceReport, _ *syntheticFastIterationSample, err *error) {
			s.Runs = s.Runs[:4]
			*err = errors.New("original partial-fixture failure")
		}},
		{name: "policy error", cause: true, mutate: func(_ *syntheticFastSequenceReport, _ *syntheticFastIterationSample, err *error) {
			*err = errors.New("window policy was not live")
		}},
		{name: "cleanup and numeric failure", cause: true, mutate: func(s *syntheticFastSequenceReport, _ *syntheticFastIterationSample, err *error) {
			for index := range s.Runs {
				s.Runs[index].Workload.WholeRunMbps = 100
			}
			*err = errors.New("fixture cleanup failed")
		}},
	} {
		var report syntheticFastMedianReport
		calls := 0
		var sourceErr error
		err := collectSyntheticFastMedian(&report, func(index int) (syntheticFastIterationSample, error) {
			calls++
			sequence := syntheticFastMeanControl([5]float64{210, 210, 210, 210, 210})
			sample := syntheticFastIterationSample{OwnersJoined: true, EnvironmentClosed: true}
			if index == 2 {
				row.mutate(&sequence, &sample, &sourceErr)
			}
			mean, meanErr := syntheticFastFiveWindowMean(sequence)
			sample.Mean = mean
			if sourceErr != nil {
				return sample, errors.Join(meanErr, sourceErr)
			}
			return sample, meanErr
		})
		if err == nil || calls != 2 || report.AttemptedIterations != 2 || report.Valid || report.EvidenceValid || report.Stop != "invalid_iteration" ||
			report.Iterations[0].State != "valid" || report.Iterations[1].State != "invalid" || report.Iterations[2].State != "NOTRUN" || report.Iterations[2].NotRunReason == "" {
			t.Fatalf("%s: invalid sample was retried or omitted: report=%+v err=%v calls=%d", row.name, report, err, calls)
		}
		if row.cause && !errors.Is(err, sourceErr) {
			t.Fatalf("%s: original hard-error cause was lost: %v", row.name, err)
		}
		if row.name == "owners not joined" && (!report.Iterations[1].MeanObserved || report.Iterations[1].Mean.ArithmeticMeanMbps != 210) {
			t.Fatalf("%s: finite invalid-sample evidence was lost", row.name)
		}
	}
}

// A callback cannot invent arithmetic, omit a window, change the bar, or turn a
// slow sample's existing failed scalar verdict into a passing one.
func TestSyntheticFastMedianRejectsMalformedScalarEvidence(t *testing.T) {
	for _, row := range []struct {
		name   string
		mutate func(*syntheticFastIterationSample, *error)
	}{
		{name: "wrong bar", mutate: func(s *syntheticFastIterationSample, _ *error) { s.Mean.MinimumMeanMbps = 180 }},
		{name: "false arithmetic", mutate: func(s *syntheticFastIterationSample, _ *error) { s.Mean.ArithmeticMeanMbps = 999 }},
		{name: "missing window", mutate: func(s *syntheticFastIterationSample, _ *error) { s.Mean.WindowMbps[4] = 0 }},
		{name: "nonfinite mean", mutate: func(s *syntheticFastIterationSample, _ *error) { s.Mean.ArithmeticMeanMbps = math.NaN() }},
		{name: "invalid passing scalar", mutate: func(s *syntheticFastIterationSample, _ *error) { s.Mean.Valid = false }},
		{name: "slow scalar masked as pass", mutate: func(s *syntheticFastIterationSample, err *error) {
			s.Mean, *err = syntheticFastFiveWindowMean(syntheticFastMeanControl([5]float64{100, 100, 100, 100, 100}))
			s.Mean.Valid = true
		}},
		{name: "slow scalar without owned error", mutate: func(s *syntheticFastIterationSample, err *error) {
			s.Mean, *err = syntheticFastFiveWindowMean(syntheticFastMeanControl([5]float64{100, 100, 100, 100, 100}))
			*err = nil
		}},
	} {
		var report syntheticFastMedianReport
		calls := 0
		err := collectSyntheticFastMedian(&report, func(int) (syntheticFastIterationSample, error) {
			calls++
			mean, sampleErr := syntheticFastFiveWindowMean(syntheticFastMeanControl([5]float64{210, 210, 210, 210, 210}))
			sample := syntheticFastIterationSample{Mean: mean, OwnersJoined: true, EnvironmentClosed: true}
			row.mutate(&sample, &sampleErr)
			return sample, sampleErr
		})
		if err == nil || calls != 1 || report.AttemptedIterations != 1 || report.Valid || report.EvidenceValid || report.Iterations[1].State != "NOTRUN" {
			t.Fatalf("%s: malformed callback was accepted: %+v err=%v calls=%d", row.name, report, err, calls)
		}
	}
}

// A native report must carry every original zero ledger and exact worker join.
// These are checked on each iteration, independently of throughput selection.
func TestSyntheticFastMedianRequiresEveryNativeJoinReceipt(t *testing.T) {
	for _, target := range []int64{32 * 1024 * 1024, 64 * 1024 * 1024} {
		for _, row := range []struct {
			name   string
			mutate func(*syntheticFastNativeIterationReport)
		}{
			{name: "complete"},
			{name: "missing client", mutate: func(r *syntheticFastNativeIterationReport) { r.ClientAfterJoin = nil }},
			{name: "client total", mutate: func(r *syntheticFastNativeIterationReport) { r.ClientAfterJoin.TotalByteCount = 1 }},
			{name: "provider total", mutate: func(r *syntheticFastNativeIterationReport) { r.ProviderAfterJoin[1].TotalByteCount = 1 }},
			{name: "missing provider", mutate: func(r *syntheticFastNativeIterationReport) {
				r.ProviderAfterJoin = r.ProviderAfterJoin[:len(r.ProviderAfterJoin)-1]
			}},
			{name: "nil provider", mutate: func(r *syntheticFastNativeIterationReport) { r.ProviderAfterJoin[1] = nil }},
			{name: "provider count", mutate: func(r *syntheticFastNativeIterationReport) { r.ProviderCount-- }},
			{name: "provider target", mutate: func(r *syntheticFastNativeIterationReport) { r.ProviderTarget-- }},
			{name: "worker not joined", mutate: func(r *syntheticFastNativeIterationReport) { r.WorkerFinalJoined[4] = false }},
			{name: "handler not joined", mutate: func(r *syntheticFastNativeIterationReport) { r.OriginHandlersFinished-- }},
			{name: "transfer accounting", mutate: func(r *syntheticFastNativeIterationReport) { r.ProviderAfterJoin[1].TransferRootReservedByteCount = 1 }},
			{name: "transport accounting", mutate: func(r *syntheticFastNativeIterationReport) { r.ClientAfterJoin.PlatformTransportReservedBytes = 1 }},
		} {
			var report syntheticFastMedianReport
			calls := 0
			err := collectSyntheticFastMedian(&report, func(index int) (syntheticFastIterationSample, error) {
				calls++
				providerCount := 6
				if target == 64*1024*1024 {
					providerCount = 12
				}
				native := syntheticFastNativeIterationReport{ConsumerTarget: target, ProviderTarget: 128 * 1024 * 1024, ProviderCount: providerCount,
					WorkerFinalJoined: [5]bool{true, true, true, true, true}, OriginHandlersStarted: 5, OriginHandlersFinished: 5,
					ClientAfterJoin: &sdk.DeviceLocalMemoryUsage{}, ProviderAfterJoin: make([]*sdk.DeviceLocalMemoryUsage, providerCount)}
				for p := range native.ProviderAfterJoin {
					native.ProviderAfterJoin[p] = &sdk.DeviceLocalMemoryUsage{}
				}
				if index == 2 && row.mutate != nil {
					row.mutate(&native)
				}
				joinErr := syntheticFastNativeIterationJoinError(native)
				mean, meanErr := syntheticFastFiveWindowMean(syntheticFastMeanControl([5]float64{210, 210, 210, 210, 210}))
				return syntheticFastIterationSample{Mean: mean, OwnersJoined: joinErr == nil, EnvironmentClosed: true}, errors.Join(meanErr, joinErr)
			})
			passed := row.mutate == nil
			if (err == nil) != passed || report.Valid != passed || (passed && calls != 3) || (!passed && (calls != 2 || report.Iterations[2].State != "NOTRUN")) {
				t.Fatalf("target%d %s: incomplete native ledger reached a later fixture: report=%+v err=%v", target, row.name, report, err)
			}
		}
	}
}

// Panic unwinding preserves already completed samples and explicit
// not-run successors, and never converts an incomplete callback into a sample.
func TestSyntheticFastMedianIncompleteCallbackRetainsEvidence(t *testing.T) {
	var report syntheticFastMedianReport
	cause := errors.New("incomplete iteration")
	calls := 0
	func() {
		defer func() {
			if recovered := recover(); recovered != cause {
				t.Fatalf("callback panic was swallowed or replaced: %v", recovered)
			}
		}()
		_ = collectSyntheticFastMedian(&report, func(index int) (syntheticFastIterationSample, error) {
			calls++
			if index == 2 {
				panic(cause)
			}
			mean, err := syntheticFastFiveWindowMean(syntheticFastMeanControl([5]float64{210, 210, 210, 210, 210}))
			return syntheticFastIterationSample{Mean: mean, OwnersJoined: true, EnvironmentClosed: true}, err
		})
	}()
	if calls != 2 || report.Valid || report.EvidenceValid || report.Stop != "invalid_iteration" || report.Iterations[0].State != "valid" ||
		report.Iterations[1].State != "invalid" || report.Iterations[2].State != "NOTRUN" {
		t.Fatalf("incomplete callback lost evidence or invoked replacement: %+v", report)
	}
}

// Goexit follows the same path as FailNow without marking this positive test
// failed. The completion channel is the exact ownership barrier, with no sleep.
func TestSyntheticFastMedianGoexitRetainsEvidence(t *testing.T) {
	var report syntheticFastMedianReport
	calls := 0
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = collectSyntheticFastMedian(&report, func(index int) (syntheticFastIterationSample, error) {
			calls++
			if index == 2 {
				runtime.Goexit()
			}
			mean, err := syntheticFastFiveWindowMean(syntheticFastMeanControl([5]float64{210, 210, 210, 210, 210}))
			return syntheticFastIterationSample{Mean: mean, OwnersJoined: true, EnvironmentClosed: true}, err
		})
	}()
	<-done
	if calls != 2 || report.Valid || report.EvidenceValid || report.Stop != "invalid_iteration" || report.Iterations[0].State != "valid" ||
		report.Iterations[1].State != "invalid" || report.Iterations[2].State != "NOTRUN" {
		t.Fatalf("Goexit lost evidence or invoked a replacement: %+v", report)
	}
}
