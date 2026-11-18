package work

import (
	"context"
	"errors"
	"math"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	"github.com/urnetwork/server/v2026/model"
	"github.com/urnetwork/server/v2026/qualityprobe/egresshealth"
	"github.com/urnetwork/server/v2026/qualityprobe/fleetprobe"
	"github.com/urnetwork/server/v2026/qualityprobe/prober"
)

type timingPublicationIngest struct {
	egressProbeIngest
	health func(context.Context, string, *egresshealth.Result) error
}

func (self *timingPublicationIngest) SubmitEgressHealth(ctx context.Context, id string, value *egresshealth.Result) error {
	return self.health(ctx, id, value)
}

func (self *timingPublicationIngest) ReportAttempt(ctx context.Context, id, failure string) error {
	time.Sleep(time.Second)
	return self.egressProbeIngest.ReportAttempt(ctx, id, failure)
}

func TestUrlProbeStageCompletionWaitsForCloseAndPublication(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		args := providerEgressProbeArgs(testProviderEgressProbeSettings(1), 0)
		args.Full.Limit, args.Full.Concurrency = 1, 1
		closeEntered, closeRelease := make(chan struct{}), make(chan struct{})
		publishEntered, publishRelease := make(chan struct{}), make(chan struct{})
		completed := make(chan struct{})
		var closeOnce, publishOnce sync.Once
		t.Cleanup(func() {
			closeOnce.Do(func() { close(closeRelease) })
			publishOnce.Do(func() { close(publishRelease) })
			<-completed
		})
		inner := newRecordingEgressProbeIngest()
		var observed atomic.Int32
		before := urlProbeStageMetrics.snapshot()
		pass := &providerEgressProbePass{
			urlProbes: true,
			fullSink: testFullBatchSink(&timingPublicationIngest{
				egressProbeIngest: inner,
				health: func(ctx context.Context, id string, value *egresshealth.Result) error {
					close(publishEntered)
					<-publishRelease
					return inner.SubmitEgressHealth(ctx, id, value)
				},
			}),
			fullOptions: fleetprobe.FullOptions{ObserveTiming: func(prober.ProbeTiming) { observed.Add(1) }},
			recordTally: func(context.Context, time.Time, model.ProviderEgressRunTally, []model.ProviderEgressSiteLoad) {
				time.Sleep(3 * time.Second)
			},
			runFull: func(ctx context.Context, providers []prober.Provider, options fleetprobe.FullOptions) (prober.Summary, error) {
				scheduler := &prober.Scheduler{Concurrency: options.Concurrency, ObserveProgress: options.ObserveProgress,
					Prober: &prober.Prober{
						ObserveTiming: options.ObserveTiming,
						Open: func(context.Context, string) (*http.Client, func() error, error) {
							time.Sleep(2 * time.Second)
							return &http.Client{}, func() error { close(closeEntered); <-closeRelease; return nil }, nil
						},
						Health: func(context.Context, *http.Client, egresshealth.Place) (*egresshealth.Result, error) {
							time.Sleep(3 * time.Second)
							return testHealthRun(1, 1), nil
						},
						Submit: options.Submit, Attempts: options.Attempts, HealthResults: options.HealthResults,
					},
				}
				return scheduler.Run(ctx, providers), nil
			},
		}
		done := make(chan providerEgressFullOutcome, 1)
		go func() {
			defer close(completed)
			done <- pass.runFullBatch(t.Context(), args, nil, nil, testDueProviders("synthetic"))
		}()
		<-closeEntered
		time.Sleep(7 * time.Second)
		synctest.Wait()
		if got := urlProbeStageMetrics.snapshot(); got != before || observed.Load() != 0 {
			t.Fatal("unfinished cleanup advanced completed-turn evidence")
		}
		if states := egressProbeFullProgress.snapshot().states; states[2] != 1 || states[3] != 0 {
			t.Fatalf("timing changed running ownership: %v", states)
		}
		closeOnce.Do(func() { close(closeRelease) })
		<-publishEntered
		time.Sleep(4 * time.Second)
		synctest.Wait()
		if got := urlProbeStageMetrics.snapshot(); got != before || observed.Load() != 1 {
			t.Fatal("ProbeOne return prematurely published the durable-release cohort")
		}
		if states := egressProbeFullProgress.snapshot().states; states[2] != 0 || states[3] != 1 {
			t.Fatalf("publication boundary changed worker ownership: %v", states)
		}
		publishOnce.Do(func() { close(publishRelease) })
		outcome := <-done
		after := urlProbeStageMetrics.snapshot()
		if outcome.err != nil || outcome.summary.Submitted != 1 || after.timed != before.timed+1 || after.unobserved != before.unobserved {
			t.Fatalf("completed result changed: %+v before=%+v after=%+v", outcome, before, after)
		}
		want := [8]float64{0, 2, 3, 7, 0, 8, 0, 20}
		for i, value := range want {
			// Cumulative float counters can already contain fractional seconds
			// from another completed test turn. Their subtraction can round by
			// an ulp; the underlying synthetic Duration partition is exact.
			if math.Abs((after.seconds[i]-before.seconds[i])-value) > time.Nanosecond.Seconds() {
				t.Errorf("stage %s got=%g want=%g", urlProbeStageNames[i], after.seconds[i]-before.seconds[i], value)
			}
		}
	})
}

func TestUrlProbeStageMissingRunnerEvidenceRemainsUnobserved(t *testing.T) {
	before := urlProbeStageMetrics.snapshot()
	args := providerEgressProbeArgs(testProviderEgressProbeSettings(1), 0)
	want := errors.New("synthetic source failure")
	pass := &providerEgressProbePass{urlProbes: true,
		runFull: func(context.Context, []prober.Provider, fleetprobe.FullOptions) (prober.Summary, error) {
			return prober.Summary{}, want
		}}
	got := pass.runFullBatch(t.Context(), args, nil, nil, testDueProviders("synthetic"))
	after := urlProbeStageMetrics.snapshot()
	if !errors.Is(got.err, want) || after.seconds != before.seconds || after.timed != before.timed || after.unobserved != before.unobserved+1 {
		t.Fatalf("unsupported runner became zero-latency work: before=%+v after=%+v err=%v", before, after, got.err)
	}
}

func TestUrlProbeStageRejectsAmbiguousAndInvalidTiming(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		metrics := newProviderUrlProbeStageMetrics()
		for _, test := range []struct {
			count int
			value prober.ProbeTiming
		}{
			{0, prober.ProbeTiming{}},
			{2, prober.ProbeTiming{}},
			{1, prober.ProbeTiming{Open: -time.Second}},
			{1, prober.ProbeTiming{Open: time.Second, Total: 2 * time.Second}},
			{1, prober.ProbeTiming{Open: 3 * time.Second, Total: 3 * time.Second}},
		} {
			owner := newProviderUrlProbeStageOwner(metrics)
			time.Sleep(time.Second)
			for range test.count {
				owner.observe(test.value)
			}
			owner.finish()
		}
		if got := metrics.snapshot(); got.timed != 0 || got.unobserved != 5 || got.seconds != [8]float64{} {
			t.Fatalf("invalid interval entered valid timing cohort: %+v", got)
		}
	})
}

func TestUrlProbeStageCollectorScrapeIsOneGeneration(t *testing.T) {
	metrics := newProviderUrlProbeStageMetrics()
	metrics.values = providerUrlProbeStageSnapshot{seconds: [8]float64{1, 1, 1, 1, 1, 1, 1, 7}, timed: 1}
	ch := make(chan prometheus.Metric)
	go func() { metrics.Collect(ch); close(ch) }()
	first := <-ch
	metrics.mu.Lock()
	metrics.values = providerUrlProbeStageSnapshot{seconds: [8]float64{2, 2, 2, 2, 2, 2, 2, 14}, timed: 2}
	metrics.mu.Unlock()
	seen := 0
	check := func(metric prometheus.Metric) {
		if strings.Contains(metric.Desc().String(), "urnetwork_url_probe_completed_setup_") {
			return
		}
		var value dto.Metric
		if err := metric.Write(&value); err != nil {
			t.Fatal(err)
		}
		want := float64(1)
		for _, label := range value.Label {
			if label.GetName() == "stage" && label.GetValue() == "total" {
				want = 7
			}
			if label.GetName() == "coverage" && label.GetValue() == "unobserved" {
				want = 0
			}
		}
		got := value.GetCounter().GetValue()
		if value.Gauge != nil {
			got = value.GetGauge().GetValue()
		}
		if got != want {
			t.Fatalf("scrape mixed completion generations: got=%g want=%g", got, want)
		}
		seen++
	}
	check(first)
	for metric := range ch {
		check(metric)
	}
	if seen != 11 {
		t.Fatalf("wrong finite metric count: %d", seen)
	}
}

func TestUrlProbeStageConcurrentCompletionsRemainCoherent(t *testing.T) {
	metrics := newProviderUrlProbeStageMetrics()
	var wg sync.WaitGroup
	for range 64 {
		wg.Go(func() {
			owner := newProviderUrlProbeStageOwner(metrics)
			owner.observe(prober.ProbeTiming{})
			owner.finish()
		})
	}
	wg.Wait()
	got := metrics.snapshot()
	if got.timed != 64 || got.unobserved != 0 {
		t.Fatalf("lost or ambiguous completions: %+v", got)
	}
	var total float64
	for _, value := range got.seconds[:7] {
		total += value
	}
	if total != got.seconds[7] {
		t.Fatal("stage sums no longer share a denominator")
	}
}

func TestUrlProbeStageCapabilityHasFiniteZeroSeries(t *testing.T) {
	registry := prometheus.NewPedanticRegistry()
	registry.MustRegister(newProviderUrlProbeStageMetrics())
	families, err := registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	if len(families) != 13 {
		t.Fatalf("wrong metric families: %d", len(families))
	}
	allowed := map[string]bool{}
	for _, stage := range urlProbeStageNames {
		allowed[stage] = true
	}
	count := 0
	for _, family := range families {
		if strings.HasPrefix(family.GetName(), "urnetwork_url_probe_completed_setup_") {
			continue
		}
		for _, metric := range family.Metric {
			count++
			for _, label := range metric.Label {
				if label.GetName() == "stage" && allowed[label.GetValue()] {
					continue
				}
				if label.GetName() == "coverage" && (label.GetValue() == "timed" || label.GetValue() == "unobserved") {
					continue
				}
				t.Fatal("unexpected identity or label cardinality")
			}
			if metric.Counter != nil && metric.GetCounter().GetValue() != 0 {
				t.Fatal("idle collector invented activity")
			}
			if metric.Gauge != nil && metric.GetGauge().GetValue() != 1 {
				t.Fatal("missing timing capability")
			}
		}
	}
	if count != 11 {
		t.Fatalf("missing explicit zero series: %d", count)
	}
}
