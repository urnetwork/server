package work

import (
	"context"
	"net/http"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	"github.com/urnetwork/connect/v2026"
	"github.com/urnetwork/server/v2026/qualityprobe/egresshealth"
	"github.com/urnetwork/server/v2026/qualityprobe/fleetprobe"
	"github.com/urnetwork/server/v2026/qualityprobe/prober"
	"github.com/urnetwork/server/v2026/qualityprobe/providertunnel"
)

func urlSetupTestConfig() providertunnel.Config {
	return providertunnel.Config{ApiUrl: "http://127.0.0.1:0", PlatformUrl: "http://127.0.0.1:0", ByJwt: "synthetic", ClientId: connect.NewId()}
}

func TestUrlProbeSetupBatchWaitsForTerminalCloseAndPublication(t *testing.T) {
	args := providerEgressProbeArgs(testProviderEgressProbeSettings(1), 0)
	args.Full.Limit, args.Full.Concurrency = 1, 1
	closeEntered, closeRelease := make(chan struct{}), make(chan struct{})
	publishEntered, publishRelease := make(chan struct{}), make(chan struct{})
	var closeOnce, publishOnce sync.Once
	completed := make(chan struct{})
	t.Cleanup(func() {
		closeOnce.Do(func() { close(closeRelease) })
		publishOnce.Do(func() { close(publishRelease) })
		<-completed
	})
	inner := newRecordingEgressProbeIngest()
	before := urlProbeStageMetrics.snapshot()
	external := &providertunnel.SetupObservations{}
	var setup *providertunnel.SetupObservations
	pass := &providerEgressProbePass{urlProbes: true,
		fullOptions: fleetprobe.FullOptions{TunnelConfig: providertunnel.Config{SetupObservations: external}},
		fullSink: testFullBatchSink(&timingPublicationIngest{egressProbeIngest: inner, health: func(ctx context.Context, id string, value *egresshealth.Result) error {
			close(publishEntered)
			<-publishRelease
			return inner.SubmitEgressHealth(ctx, id, value)
		}}),
		runFull: func(ctx context.Context, providers []prober.Provider, options fleetprobe.FullOptions) (prober.Summary, error) {
			setup = options.TunnelConfig.SetupObservations
			if setup == nil || setup == external {
				t.Error("batch did not allocate its own setup owner")
			}
			scheduler := &prober.Scheduler{Concurrency: 1, ObserveProgress: options.ObserveProgress, Prober: &prober.Prober{
				ObserveTiming: options.ObserveTiming,
				Open: func(ctx context.Context, _ string) (*http.Client, func() error, error) {
					cfg := urlSetupTestConfig()
					cfg.SetupObservations = setup
					tunnel, err := providertunnel.Open(ctx, cfg, connect.NewId())
					if err != nil {
						return nil, nil, err
					}
					return &http.Client{}, func() error { close(closeEntered); <-closeRelease; return tunnel.Close() }, nil
				},
				Health: func(context.Context, *http.Client, egresshealth.Place) (*egresshealth.Result, error) {
					return testHealthRun(1, 1), nil
				},
				Submit: options.Submit, Attempts: options.Attempts, HealthResults: options.HealthResults,
			}}
			return scheduler.Run(ctx, providers), nil
		},
	}
	done := make(chan providerEgressFullOutcome, 1)
	go func() {
		defer close(completed)
		done <- pass.runFullBatch(t.Context(), args, nil, nil, testDueProviders("synthetic"))
	}()
	select {
	case <-closeEntered:
	case <-time.After(5 * time.Second):
		t.Fatal("Close was not reached")
	}
	if setup.Snapshot() != (providertunnel.SetupTiming{}) || urlProbeStageMetrics.snapshot() != before {
		t.Fatal("live tunnel advanced completed setup timing")
	}
	closeOnce.Do(func() { close(closeRelease) })
	select {
	case <-publishEntered:
	case <-time.After(5 * time.Second):
		t.Fatal("publication was not reached")
	}
	// Open launches background window enumeration. Even without application
	// traffic a credential wrapper can be pending at freeze; incomplete is
	// valid coverage for that case. This test owns the terminal/publication
	// boundary, while deterministic observer controls own route attribution.
	frozenSetup := setup.Snapshot()
	var frozenTunnels uint64
	for _, count := range frozenSetup.Tunnels {
		frozenTunnels += count
	}
	if frozenTunnels != 1 || urlProbeStageMetrics.snapshot() != before {
		t.Fatalf("terminal tunnel snapshot bypassed batch publication: setup=%+v before=%+v now=%+v", frozenSetup, before, urlProbeStageMetrics.snapshot())
	}
	if frozenSetup.Tunnels[providertunnel.SetupAdmissionNoConstructor]+frozenSetup.Tunnels[providertunnel.SetupAdmissionIncomplete] != 1 ||
		(frozenSetup.Tunnels[providertunnel.SetupAdmissionIncomplete] == 1 && frozenSetup.Pending == [2]uint64{}) {
		t.Fatalf("idle fixture acquired unexplained setup coverage: %+v", frozenSetup)
	}

	publishOnce.Do(func() { close(publishRelease) })
	outcome := <-done
	after := urlProbeStageMetrics.snapshot()
	expected := before
	expected.addSetup(frozenSetup, true)
	if outcome.err != nil || after.timed != before.timed+1 || after.setupObserved != expected.setupObserved ||
		after.setupUnobserved != expected.setupUnobserved || after.setup != expected.setup || setup.Snapshot() != frozenSetup || external.Snapshot() != (providertunnel.SetupTiming{}) {
		t.Fatalf("same-turn setup publication changed result or owner: outcome=%+v expected=%+v actual=%+v", outcome, expected, after)
	}
}

func TestUrlProbeSetupInvalidTurnAndRepeatedFinishAreExcluded(t *testing.T) {
	metrics := newProviderUrlProbeStageMetrics()
	owner := newProviderUrlProbeStageOwner(metrics)
	cfg := urlSetupTestConfig()
	cfg.SetupObservations = owner.setup
	tunnel, err := providertunnel.Open(t.Context(), cfg, connect.NewId())
	if err != nil {
		t.Fatal(err)
	}
	if err := tunnel.Close(); err != nil {
		t.Fatal(err)
	}
	owner.observe(prober.ProbeTiming{})
	owner.observe(prober.ProbeTiming{})
	owner.finish()
	got := metrics.snapshot()
	if got.unobserved != 1 || got.setupUnobserved != 1 || got.setup != (providertunnel.SetupTiming{}) {
		t.Fatalf("ambiguous turn published separate setup cohort: %+v", got)
	}
	owner.observe(prober.ProbeTiming{})
	owner.finish()
	if metrics.snapshot() != got {
		t.Fatal("late or repeated completion changed immutable publication")
	}
}

func TestUrlProbeSetupCollectorSharesGenerationAndFixedLabels(t *testing.T) {
	metrics := newProviderUrlProbeStageMetrics()
	first := providerUrlProbeStageSnapshot{timed: 1, setupObserved: 1}
	first.setup.Tunnels[providertunnel.SetupAdmissionMatched] = 1
	first.setup.Calls[0][0] = providertunnel.SetupCallTiming{Count: 1, Duration: time.Second}
	first.setup.Calls[1][0] = providertunnel.SetupCallTiming{Count: 1, Duration: 2 * time.Second}
	first.setup.RouteStages = [4]time.Duration{time.Second, 3 * time.Second, 2 * time.Second, 4 * time.Second}
	first.setup.EvaluationCount = 1
	first.setup.EvaluationToAdmission = time.Second
	metrics.values = first
	expectedChannel := make(chan prometheus.Metric, 43)
	metrics.Collect(expectedChannel)
	close(expectedChannel)
	var expected []*dto.Metric
	for metric := range expectedChannel {
		value := &dto.Metric{}
		if err := metric.Write(value); err != nil {
			t.Fatal(err)
		}
		expected = append(expected, value)
	}
	actual := make(chan prometheus.Metric)
	go func() { metrics.Collect(actual); close(actual) }()
	firstMetric := <-actual
	metrics.mu.Lock()
	metrics.values.addSetup(first.setup, true)
	metrics.values.timed++
	metrics.mu.Unlock()
	i := 0
	check := func(metric prometheus.Metric) {
		value := &dto.Metric{}
		if err := metric.Write(value); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(value, expected[i]) {
			t.Fatalf("scrape mixed setup/turn generations at metric %d", i)
		}
		i++
	}
	check(firstMetric)
	for metric := range actual {
		check(metric)
	}
	if i != 43 {
		t.Fatalf("metric cardinality changed: %d", i)
	}
	registry := prometheus.NewPedanticRegistry()
	registry.MustRegister(newProviderUrlProbeStageMetrics())
	families, err := registry.Gather()
	if err != nil || len(families) != 13 {
		t.Fatalf("fixed setup collector invalid: families=%d err=%v", len(families), err)
	}
	allowed := map[string]map[string]bool{"stage": {}, "outcome": {}, "coverage": {}}
	for _, v := range urlProbeStageNames {
		allowed["stage"][v] = true
	}
	for _, v := range urlProbeSetupRouteNames {
		allowed["stage"][v] = true
	}
	for _, v := range urlProbeSetupOutcomeNames {
		allowed["outcome"][v] = true
	}
	for _, v := range urlProbeSetupCoverageNames {
		allowed["coverage"][v] = true
	}
	for _, v := range []string{"timed", "unobserved", "observed"} {
		allowed["coverage"][v] = true
	}
	count := 0
	for _, family := range families {
		if !strings.HasPrefix(family.GetName(), "urnetwork_url_probe_completed_") {
			t.Fatal("unexpected metric family")
		}
		for _, metric := range family.Metric {
			count++
			for _, label := range metric.Label {
				if !allowed[label.GetName()][label.GetValue()] {
					t.Fatal("identity or variable-cardinality label escaped")
				}
			}
			if metric.Counter != nil && metric.Counter.GetValue() != 0 {
				t.Fatal("idle collector invented observations")
			}
			if metric.Gauge != nil && metric.Gauge.GetValue() != 1 {
				t.Fatal("setup capability missing")
			}
		}
	}
	if count != 43 {
		t.Fatalf("finite zero series missing: %d", count)
	}
}
