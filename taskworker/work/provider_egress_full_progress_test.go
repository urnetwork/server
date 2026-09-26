// Exercise the production batch/scheduler publication boundary with explicit
// barriers; a worker finish is not guard approval or acknowledged evidence.
package work

import (
	"context"
	"errors"
	"net/http"
	"reflect"
	"sync/atomic"
	"testing"
	"testing/synctest"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/urnetwork/server/qualityprobe/egresshealth"
	"github.com/urnetwork/server/qualityprobe/fleetprobe"
	"github.com/urnetwork/server/qualityprobe/prober"
)

type testFullProgressIngest struct {
	egressProbeIngest
	health func(context.Context, string, *egresshealth.Result) error
}

func (self *testFullProgressIngest) SubmitEgressHealth(ctx context.Context, id string, run *egresshealth.Result) error {
	return self.health(ctx, id, run)
}

func TestEgressFullProgressActualOwnerSeparatesWorkerAndPublication(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		args := providerEgressProbeArgs(testProviderEgressProbeSettings(1), 0)
		args.Full.Limit, args.Full.Concurrency = 2, 2
		tailRelease, publishRelease := make(chan struct{}), make(chan struct{})
		var publication, observed atomic.Int32
		before := egressProbeFullProgress.snapshot()
		inner := newRecordingEgressProbeIngest()
		pass := &providerEgressProbePass{
			fullSink: testFullBatchSink(&testFullProgressIngest{
				egressProbeIngest: inner,
				health: func(ctx context.Context, id string, run *egresshealth.Result) error {
					publication.Add(1)
					<-publishRelease
					return inner.SubmitEgressHealth(ctx, id, run)
				},
			}),
			fullOptions: fleetprobe.FullOptions{ObserveProgress: func(prober.Progress) { observed.Add(1) }},
			runFull: func(ctx context.Context, providers []prober.Provider, options fleetprobe.FullOptions) (prober.Summary, error) {
				scheduler := &prober.Scheduler{
					Concurrency: options.Concurrency, ObserveProgress: options.ObserveProgress,
					Prober: &prober.Prober{
						Open: func(_ context.Context, id string) (*http.Client, func() error, error) {
							if id == "synthetic-tail" {
								<-tailRelease
							}
							return &http.Client{}, func() error { return nil }, nil
						},
						Health: func(context.Context, *http.Client, egresshealth.Place) (*egresshealth.Result, error) {
							run := testHealthRun(10, 9)
							run.ExitIp = "203.0.113.7"
							return run, nil
						},
						Submit: options.Submit, Attempts: options.Attempts, HealthResults: options.HealthResults,
					},
				}
				return scheduler.Run(ctx, providers), nil
			},
		}
		done := make(chan providerEgressFullOutcome, 1)
		go func() {
			done <- pass.runFullBatch(context.Background(), args, nil, nil, testDueProviders("synthetic-fast", "synthetic-tail"))
		}()
		synctest.Wait()
		if got := egressProbeFullProgress.snapshot().states; got != [4]int64{1, 0, 1, 1} || publication.Load() != 0 {
			t.Errorf("active full worker invisible or released early: states=%v calls=%d", got, publication.Load())
		}
		close(tailRelease)
		synctest.Wait()
		if got := egressProbeFullProgress.snapshot().states; got != [4]int64{1, 0, 0, 2} || publication.Load() != 1 {
			t.Errorf("publication wait mislabeled: states=%v calls=%d", got, publication.Load())
		}
		close(publishRelease)
		outcome := <-done
		after := egressProbeFullProgress.snapshot()
		if outcome.err != nil || outcome.summary.Submitted != 2 || after.states != [4]int64{} || observed.Load() != 4 {
			t.Errorf("full owner did not retire/preserve observer: result=%+v states=%v observed=%d", outcome, after.states, observed.Load())
		}
		want := before.events
		want[0]++
		want[1]++
		want[2] += 2
		want[3] += 2
		if after.events != want {
			t.Errorf("wrong actual lifecycle: %v want=%v", after.events, want)
		}
	})
}

func TestEgressFullProgressSourceFailureHasNoProviderActivity(t *testing.T) {
	args := providerEgressProbeArgs(testProviderEgressProbeSettings(1), 0)
	want := errors.New("synthetic full source failure")
	before := egressProbeFullProgress.snapshot()
	pass := &providerEgressProbePass{runFull: func(context.Context, []prober.Provider, fleetprobe.FullOptions) (prober.Summary, error) {
		return prober.Summary{}, want
	}}
	outcome := pass.runFullBatch(context.Background(), args, nil, nil, testDueProviders("synthetic-unstarted"))
	after := egressProbeFullProgress.snapshot()
	if !errors.Is(outcome.err, want) || after.states != [4]int64{} || after.events[2] != before.events[2] || after.events[3] != before.events[3] || after.events[0] != before.events[0]+1 || after.events[1] != before.events[1]+1 {
		t.Fatalf("setup error fabricated worker completion: result=%+v before=%+v after=%+v", outcome, before, after)
	}
}

func TestEgressFullProgressOwnersRetireIndependently(t *testing.T) {
	metrics := newProviderEgressFullProgressMetrics()
	a, b := metrics.begin(2), metrics.begin(1)
	a.observe(prober.ProbeStarted)
	b.observe(prober.ProbeStarted)
	a.observe(prober.ProbeFinished)
	a.close()
	a.close()
	a.observe(prober.ProbeStarted)
	if got := metrics.snapshot().states; got != [4]int64{1, 0, 1, 0} {
		t.Fatalf("retiring owner erased sibling: %v", got)
	}
	b.observe(prober.ProbeFinished)
	b.close()
	if got := metrics.snapshot(); got.states != [4]int64{} || got.events != [4]uint64{2, 2, 2, 2} {
		t.Fatalf("ownership underflow/late event: %+v", got)
	}
}

func TestEgressFullProgressPreinitializesBoundedIdentityFreeSeries(t *testing.T) {
	metrics := newProviderEgressFullProgressMetrics()
	registry := prometheus.NewPedanticRegistry()
	registry.MustRegister(metrics)
	families, err := registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	counts := map[string]int{}
	allowed := map[string]map[string]bool{
		"state":   {"active_batches": true, "queued": true, "running": true, "finished_waiting": true},
		"event":   {"batch_started": true, "batch_finished": true, "provider_started": true, "provider_finished": true},
		"outcome": {"prefix_lookup": true, "prefix_advanced": true, "prefix_exhausted": true},
	}
	for _, family := range families {
		counts[family.GetName()] = len(family.Metric)
		for _, metric := range family.Metric {
			for _, label := range metric.Label {
				if !allowed[label.GetName()][label.GetValue()] {
					t.Fatal("unexpected identity/cardinality in progress")
				}
			}
			if family.GetName() == "urnetwork_egress_probe_full_progress_enabled" {
				if metric.GetGauge().GetValue() != 1 {
					t.Fatal("missing executable capability")
				}
			} else if metric.GetGauge().GetValue() != 0 || metric.GetCounter().GetValue() != 0 {
				t.Fatal("idle capability invented activity")
			}
		}
	}
	want := map[string]int{
		"urnetwork_egress_probe_full_inflight":            4,
		"urnetwork_egress_probe_full_worker_events_total": 4,
		"urnetwork_egress_probe_full_selection_total":     3,
		"urnetwork_egress_probe_full_progress_enabled":    1,
	}
	if !reflect.DeepEqual(counts, want) {
		t.Fatalf("fixed progress domain changed: %v", counts)
	}
}
