package work

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/urnetwork/server/qualityprobe/fleetprobe"
	"github.com/urnetwork/server/qualityprobe/ingest"
	"github.com/urnetwork/server/qualityprobe/prober"
)

func testBlackholeProgressWork(check fleetprobe.BlackholeChecker, submit func(context.Context, []ingest.BlackholeCheck) error) *providerEgressProbePass {
	return &providerEgressProbePass{
		blackholeOptions:      fleetprobe.BlackholeOptions{CheckOne: check},
		runBlackhole:          fleetprobe.RunBlackhole,
		submitBlackholeChecks: submit,
	}
}

func testBlackholeProgressResult(provider prober.Provider) fleetprobe.BlackholeResult {
	return fleetprobe.BlackholeResult{Check: ingest.BlackholeCheck{ClientId: provider.ClientId, Ok: true}}
}

func testBlackholeProgressState(t *testing.T, want [4]int64) {
	t.Helper()
	if got := egressProbeBlackholeProgress.snapshot().states; got != want {
		t.Errorf("active/queued/running/completed_buffered=%v, want %v", got, want)
	}
}

func TestEgressBlackholeProgressActualOwnerSeparatesBufferAndSubmission(t *testing.T) {
	args := providerEgressProbeArgs(testProviderEgressProbeSettings(1), 0)
	synctest.Test(t, func(t *testing.T) {
		releaseTail, releaseSubmit := make(chan struct{}), make(chan struct{})
		done := make(chan struct{})
		var submits atomic.Int32
		pass := testBlackholeProgressWork(func(_ context.Context, provider prober.Provider) fleetprobe.BlackholeResult {
			if provider.ClientId == "tail.example" {
				<-releaseTail
			}
			return testBlackholeProgressResult(provider)
		}, func(context.Context, []ingest.BlackholeCheck) error {
			submits.Add(1)
			<-releaseSubmit
			return nil
		})
		go func() {
			defer close(done)
			if _, _, err := pass.runBlackholeBatch(context.Background(), args, nil, nil, 2, fleetprobe.ProvidersFromClientIds([]string{"fast.example", "tail.example"})); err != nil {
				t.Error(err)
			}
		}()
		synctest.Wait()
		testBlackholeProgressState(t, [4]int64{1, 0, 1, 1})
		if submits.Load() != 0 {
			t.Error("buffered completion bypassed batch guard/barrier")
		}
		close(releaseTail)
		synctest.Wait()
		testBlackholeProgressState(t, [4]int64{1, 0, 0, 2})
		if submits.Load() != 1 {
			t.Error("batch did not reach submit boundary")
		}
		close(releaseSubmit)
		<-done
		testBlackholeProgressState(t, [4]int64{})
	})
}

func TestEgressBlackholeProgressConcurrentOwnersRetireIndependently(t *testing.T) {
	args := providerEgressProbeArgs(testProviderEgressProbeSettings(1), 0)
	synctest.Test(t, func(t *testing.T) {
		releaseA, releaseB := make(chan struct{}), make(chan struct{})
		doneA, doneB := make(chan struct{}), make(chan struct{})
		start := func(release <-chan struct{}, done chan<- struct{}) {
			defer close(done)
			pass := testBlackholeProgressWork(func(_ context.Context, provider prober.Provider) fleetprobe.BlackholeResult {
				<-release
				return testBlackholeProgressResult(provider)
			}, func(context.Context, []ingest.BlackholeCheck) error { return nil })
			if _, _, err := pass.runBlackholeBatch(context.Background(), args, nil, nil, 1, fleetprobe.ProvidersFromClientIds([]string{"owner.example"})); err != nil {
				t.Error(err)
			}
		}
		go start(releaseA, doneA)
		go start(releaseB, doneB)
		synctest.Wait()
		testBlackholeProgressState(t, [4]int64{2, 0, 2, 0})
		close(releaseA)
		<-doneA
		testBlackholeProgressState(t, [4]int64{1, 0, 1, 0})
		close(releaseB)
		<-doneB
		testBlackholeProgressState(t, [4]int64{})
	})
}

func TestEgressBlackholeProgressCancellationRetiresQueuedWithoutCompletion(t *testing.T) {
	args := providerEgressProbeArgs(testProviderEgressProbeSettings(1), 0)
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		before := egressProbeBlackholeProgress.snapshot()
		done := make(chan struct{})
		pass := testBlackholeProgressWork(func(ctx context.Context, provider prober.Provider) fleetprobe.BlackholeResult {
			<-ctx.Done()
			return testBlackholeProgressResult(provider)
		}, func(context.Context, []ingest.BlackholeCheck) error { t.Error("canceled result submitted"); return nil })
		go func() {
			defer close(done)
			if _, _, err := pass.runBlackholeBatch(ctx, args, nil, nil, 1, fleetprobe.ProvidersFromClientIds([]string{"active.example", "queued.example"})); !errors.Is(err, context.Canceled) {
				t.Errorf("cancel error=%v", err)
			}
		}()
		synctest.Wait()
		testBlackholeProgressState(t, [4]int64{1, 1, 1, 0})
		cancel()
		<-done
		testBlackholeProgressState(t, [4]int64{})
		after := egressProbeBlackholeProgress.snapshot()
		if after.events[0]-before.events[0] != 1 || after.events[1] != before.events[1] || after.events[2]-before.events[2] != 1 {
			t.Errorf("canceled lifecycle event delta: before=%v after=%v", before.events, after.events)
		}
	})
}

func TestEgressBlackholeProgressGuardedUnknownRemainsBufferedNotMeasured(t *testing.T) {
	args := providerEgressProbeArgs(testProviderEgressProbeSettings(1), 0)
	args.DarkBatchGuardMinChecks = 1
	var submitted []ingest.BlackholeCheck
	pass := testBlackholeProgressWork(func(_ context.Context, provider prober.Provider) fleetprobe.BlackholeResult {
		return fleetprobe.BlackholeResult{Check: ingest.BlackholeCheck{ClientId: provider.ClientId, Failure: "all_destinations_failed"}, Dark: true}
	}, func(_ context.Context, checks []ingest.BlackholeCheck) error {
		testBlackholeProgressState(t, [4]int64{1, 0, 0, 1})
		submitted = append(submitted, checks...)
		return nil
	})
	summary, guarded, err := pass.runBlackholeBatch(context.Background(), args, nil, nil, 1, fleetprobe.ProvidersFromClientIds([]string{"guard.example"}))
	if err != nil || !guarded || summary.NotMeasured != 1 || len(submitted) != 1 || !submitted[0].NotMeasured {
		t.Fatalf("guarded buffer changed ordinary negative policy: guarded=%t summary=%+v err=%v", guarded, summary, err)
	}
	testBlackholeProgressState(t, [4]int64{})
}

func TestEgressBlackholeProgressAdmissionZeroRetiresCleanly(t *testing.T) {
	args := providerEgressProbeArgs(testProviderEgressProbeSettings(1), 0)
	stop := make(chan struct{})
	close(stop)
	before := egressProbeBlackholeProgress.snapshot()
	pass := testBlackholeProgressWork(func(_ context.Context, provider prober.Provider) fleetprobe.BlackholeResult {
		t.Error("closed admission started work")
		return testBlackholeProgressResult(provider)
	}, func(context.Context, []ingest.BlackholeCheck) error { t.Error("empty result submitted"); return nil })
	pass.blackholeOptions.AdmissionDone = stop
	_, _, err := pass.runBlackholeBatch(context.Background(), args, nil, nil, 1, fleetprobe.ProvidersFromClientIds([]string{"queued.example"}))
	if !errors.Is(err, errProviderEgressBlackholeAdmissionBudget) {
		t.Fatalf("admission error=%v", err)
	}
	testBlackholeProgressState(t, [4]int64{})
	if egressProbeBlackholeProgress.snapshot().events != before.events {
		t.Fatal("closed admission invented worker events")
	}
}

type testBlackholeProgressIngest struct {
	egressProbeIngest
	submit func(context.Context, []ingest.BlackholeCheck) error
}

func (self *testBlackholeProgressIngest) SubmitBlackholeChecks(ctx context.Context, checks []ingest.BlackholeCheck) error {
	return self.submit(ctx, checks)
}

func TestEgressBlackholeProgressSubmissionIsPostCallAndPreservesError(t *testing.T) {
	for index, returned := range []error{nil, context.Canceled, errors.New("private.example/query?token=synthetic")} {
		before := egressProbeBlackholeProgress.snapshot()
		calls := 0
		reporter := newEgressProbeMetricsReporter(&testBlackholeProgressIngest{submit: func(context.Context, []ingest.BlackholeCheck) error {
			calls++
			if egressProbeBlackholeProgress.snapshot().submissions != before.submissions {
				t.Error("acknowledgement preceded returned outcome")
			}
			return returned
		}}, nil)
		err := reporter.SubmitBlackholeChecks(context.Background(), []ingest.BlackholeCheck{{ClientId: "private-provider.example", NotMeasured: true, Failure: "not_measured"}})
		after := egressProbeBlackholeProgress.snapshot()
		want := before.submissions
		want[index]++
		if err != returned || calls != 1 || after.submissions != want {
			t.Fatalf("post-call outcome changed forwarding: calls=%d before=%v after=%v err=%v", calls, before.submissions, after.submissions, err)
		}
	}
}

func TestEgressBlackholeProgressConcurrentCancelDoesNotRelabelAck(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	before := egressProbeBlackholeProgress.snapshot()
	reporter := newEgressProbeMetricsReporter(&testBlackholeProgressIngest{submit: func(context.Context, []ingest.BlackholeCheck) error { cancel(); return nil }}, nil)
	if err := reporter.SubmitBlackholeChecks(ctx, []ingest.BlackholeCheck{{ClientId: "ack.example", Ok: true}}); err != nil {
		t.Fatal(err)
	}
	after := egressProbeBlackholeProgress.snapshot()
	if ctx.Err() == nil || after.submissions[0] != before.submissions[0]+1 || after.submissions[1] != before.submissions[1] {
		t.Fatal("context race relabeled returned acknowledgment")
	}
}

func TestEgressBlackholeProgressEmptySubmissionHasNoAcknowledgement(t *testing.T) {
	before := egressProbeBlackholeProgress.snapshot()
	calls := 0
	reporter := newEgressProbeMetricsReporter(&testBlackholeProgressIngest{submit: func(context.Context, []ingest.BlackholeCheck) error { calls++; return nil }}, nil)
	if err := reporter.SubmitBlackholeChecks(context.Background(), nil); err != nil || calls != 1 {
		t.Fatal("empty forwarding changed")
	}
	if egressProbeBlackholeProgress.snapshot().submissions != before.submissions {
		t.Fatal("empty no-request call gained acknowledgement")
	}
}

func TestEgressBlackholeProgressFixedExpositionAndIdempotentOwnerCleanup(t *testing.T) {
	metrics := newProviderEgressBlackholeProgressMetrics()
	owner := metrics.begin(2)
	owner.observe(fleetprobe.BlackholeStarted)
	owner.observe(fleetprobe.BlackholeCompleted)
	owner.close()
	owner.close()
	if metrics.snapshot().states != [4]int64{} {
		t.Fatal("repeated close underflowed aggregate ownership")
	}
	registry := prometheus.NewPedanticRegistry()
	registry.MustRegister(metrics)
	families, err := registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	counts := map[string]int{}
	total := 0
	for _, family := range families {
		counts[family.GetName()] = len(family.Metric)
		for _, metric := range family.Metric {
			total++
			for _, label := range metric.Label {
				if label.GetName() != "state" && label.GetName() != "event" && label.GetName() != "outcome" {
					t.Fatal("identity dimension entered progress metrics")
				}
				if strings.Contains(label.GetValue(), "example") {
					t.Fatal("private identity entered metric")
				}
			}
		}
	}
	want := map[string]int{
		"urnetwork_egress_probe_blackhole_inflight":                  4,
		"urnetwork_egress_probe_blackhole_worker_events_total":       4,
		"urnetwork_egress_probe_blackhole_submission_outcomes_total": 3,
		"urnetwork_egress_probe_blackhole_progress_enabled":          1,
	}
	if total != 12 || !reflect.DeepEqual(counts, want) {
		t.Fatalf("fixed preinitialized progress domain changed: %v", counts)
	}
}
