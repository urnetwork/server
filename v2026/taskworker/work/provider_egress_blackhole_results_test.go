// Real callback/publication boundaries with synthetic checks and explicit
// barriers. Row events are never interpreted as distinct durable providers.
package work

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/urnetwork/server/v2026/model"
	"github.com/urnetwork/server/v2026/qualityprobe/fleetprobe"
	"github.com/urnetwork/server/v2026/qualityprobe/ingest"
	"github.com/urnetwork/server/v2026/qualityprobe/prober"
)

// Read the public metric contract so the pre-instrumentation source fails at
// its actual missing evidence boundary, not because a new symbol is absent.
func blackholePublicationRowsForTest(t testing.TB) map[string]float64 {
	t.Helper()
	families, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		t.Fatal(err)
	}
	values := map[string]float64{}
	for _, family := range families {
		if family.GetName() != "urnetwork_egress_probe_blackhole_publication_rows_total" {
			continue
		}
		for _, metric := range family.Metric {
			labels := map[string]string{}
			for _, label := range metric.Label {
				labels[label.GetName()] = label.GetValue()
			}
			values[labels["result"]+"/"+labels["outcome"]] = metric.GetCounter().GetValue()
		}
	}
	return values
}

func TestBlackholePublicationRowAcknowledgmentWaitsForInnerReturn(t *testing.T) {
	before := blackholePublicationRowsForTest(t)
	entered, release, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
	reporter := newEgressProbeMetricsReporter(&testBlackholeProgressIngest{submit: func(context.Context, []ingest.BlackholeCheck) error {
		close(entered)
		<-release
		return nil
	}}, nil)
	checks := []ingest.BlackholeCheck{
		{ClientId: "synthetic-pass", Ok: true, CheckedAt: time.Now()},
		{ClientId: "synthetic-negative", Failure: "all_destinations_failed", CheckedAt: time.Now()},
		{ClientId: "synthetic-tls", Failure: "tls_authentication_failed", CheckedAt: time.Now()},
		{ClientId: "synthetic-unknown", Failure: "not_measured", NotMeasured: true, CheckedAt: time.Now()},
	}
	go func() {
		defer close(done)
		if err := reporter.SubmitBlackholeChecks(t.Context(), checks); err != nil {
			t.Error(err)
		}
	}()
	<-entered
	pending := blackholePublicationRowsForTest(t)
	close(release)
	<-done
	after := blackholePublicationRowsForTest(t)
	for _, result := range []string{"ok", "ordinary_negative", "tls_authentication_failed", "not_measured"} {
		if pending[result+"/attempted"]-before[result+"/attempted"] != 1 || pending[result+"/acknowledged"] != before[result+"/acknowledged"] {
			t.Errorf("%s payload was not distinguished from a returned acknowledgment", result)
		}
		if after[result+"/acknowledged"]-before[result+"/acknowledged"] != 1 {
			t.Errorf("%s returned acknowledgment has no row-result evidence", result)
		}
	}
}

// A failed early write can be retried at finalization. Count both payload
// attempts, but only the actual successful return can contribute acknowledged
// rows. Neither implies two unique providers or proves row replacement.
func TestBlackholePublicationRowsFailureThenRetryNeverInventsAck(t *testing.T) {
	before := egressProbeBlackholeResults.snapshot()
	returned := errors.New("synthetic publication failure")
	calls := 0
	reporter := newEgressProbeMetricsReporter(&testBlackholeProgressIngest{submit: func(context.Context, []ingest.BlackholeCheck) error {
		calls++
		if calls == 1 {
			return returned
		}
		return nil
	}}, nil)
	checks := []ingest.BlackholeCheck{{ClientId: "synthetic-replayed-row", Ok: true}}
	if err := reporter.SubmitBlackholeChecks(t.Context(), checks); err != returned {
		t.Fatal("publication error identity changed")
	}
	failed := egressProbeBlackholeResults.snapshot()
	if failed.publications[blackholeResultOk][blackholePublicationAcknowledged] != before.publications[blackholeResultOk][blackholePublicationAcknowledged] {
		t.Error("failed inner submit gained acknowledged rows")
	}
	if err := reporter.SubmitBlackholeChecks(t.Context(), checks); err != nil {
		t.Fatal(err)
	}
	after := egressProbeBlackholeResults.snapshot()
	want := before.publications
	want[blackholeResultOk][blackholePublicationAttempted] += 2
	want[blackholeResultOk][blackholePublicationError]++
	want[blackholeResultOk][blackholePublicationAcknowledged]++
	if calls != 2 || after.publications != want {
		t.Errorf("retry erased attempts or duplicated acknowledgment: calls=%d before=%v after=%v", calls, before.publications, after.publications)
	}
}

// The returned error, not a racing context observation, owns the outcome.
// Empty calls retain the inner forwarding behavior but count no rows.
func TestBlackholePublicationRowsCancellationAndEmptyCallBoundaries(t *testing.T) {
	before := egressProbeBlackholeResults.snapshot()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	calls := 0
	reporter := newEgressProbeMetricsReporter(&testBlackholeProgressIngest{submit: func(context.Context, []ingest.BlackholeCheck) error {
		calls++
		switch calls {
		case 1:
			return context.Canceled
		case 2:
			return context.DeadlineExceeded
		default:
			cancel()
			return nil
		}
	}}, nil)
	checks := []ingest.BlackholeCheck{{ClientId: "synthetic-unknown", NotMeasured: true, Failure: "not_measured"}}
	for _, wantErr := range []error{context.Canceled, context.DeadlineExceeded, nil} {
		if err := reporter.SubmitBlackholeChecks(ctx, checks); err != wantErr {
			t.Errorf("returned outcome changed: got=%v want=%v", err, wantErr)
		}
	}
	if err := reporter.SubmitBlackholeChecks(ctx, nil); err != nil {
		t.Fatal(err)
	}
	want := before.publications
	want[blackholeResultNotMeasured][blackholePublicationAttempted] += 3
	want[blackholeResultNotMeasured][blackholePublicationCanceled] += 2
	want[blackholeResultNotMeasured][blackholePublicationAcknowledged]++
	if ctx.Err() == nil || calls != 4 || egressProbeBlackholeResults.snapshot().publications != want {
		t.Error("cancellation raced a successful return or an empty call gained row events")
	}
}

// A panic is not a returned publication result. Diagnostics must neither
// swallow the panic nor manufacture an acknowledgment for its payload.
func TestBlackholePublicationRowsPanicKeepsOnlyAttempt(t *testing.T) {
	before := egressProbeBlackholeResults.snapshot()
	reporter := newEgressProbeMetricsReporter(&testBlackholeProgressIngest{submit: func(context.Context, []ingest.BlackholeCheck) error {
		panic("synthetic publication panic")
	}}, nil)
	panicked := false
	func() {
		defer func() { panicked = recover() != nil }()
		_ = reporter.SubmitBlackholeChecks(t.Context(), []ingest.BlackholeCheck{{ClientId: "synthetic-panic", Ok: true}})
	}()
	want := before.publications
	want[blackholeResultOk][blackholePublicationAttempted]++
	if !panicked || egressProbeBlackholeResults.snapshot().publications != want {
		t.Error("publication panic was swallowed or counted as a returned row")
	}
}

// Late closure or a sibling cohort cannot erase another owner's pending
// negative evidence, and a completed pass never enters that pending gauge.
func TestBlackholeNegativeOwnersRetireIndependently(t *testing.T) {
	metrics := newProviderEgressBlackholeResultMetrics()
	a, b := metrics.begin(), metrics.begin()
	a.observe(ingest.BlackholeCheck{Failure: "synthetic-negative"})
	a.observe(ingest.BlackholeCheck{Ok: true})
	b.observe(ingest.BlackholeCheck{Failure: "synthetic-negative"})
	b.observe(ingest.BlackholeCheck{Failure: "synthetic-negative"})
	if metrics.snapshot().pending != 3 {
		t.Fatal("ordinary-negative ownership counted passing evidence or lost a sibling")
	}
	a.resolve(blackholeNegativeEligible)
	a.close()
	a.close()
	a.observe(ingest.BlackholeCheck{Failure: "late-synthetic-negative"})
	if metrics.snapshot().pending != 2 {
		t.Fatal("resolved/closed owner erased or revived pending evidence")
	}
	b.close()
	got := metrics.snapshot()
	if got.pending != 0 || got.completed != [4]uint64{1, 0, 3, 0} || got.dispositions != [6]uint64{1, 0, 0, 0, 0, 2} {
		t.Errorf("owner accounting did not retire exactly once: %+v", got)
	}
}

// Exercise the actual fleet completion callbacks, safe early publisher and
// dark guard. Buffered passes may already be ACKed while only ordinary
// negatives remain pending; guard conversion must not appear as an ACKed dark.
func TestBlackholeResultBoundariesSeparateEarlyAckFromGuardedNegatives(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		before := egressProbeBlackholeResults.snapshot()
		providers := testBlackholeEarlyProviders(22)
		releaseTail, done := make(chan struct{}), make(chan struct{})
		reporter := newEgressProbeMetricsReporter(&testBlackholeProgressIngest{submit: func(context.Context, []ingest.BlackholeCheck) error { return nil }}, nil)
		pass := testBlackholeEarlyPass(func(_ context.Context, provider prober.Provider) fleetprobe.BlackholeResult {
			if provider.ClientId == providers[21].ClientId {
				<-releaseTail
				return testBlackholeAdmissionTls(provider)
			}
			if provider.ClientId < providers[16].ClientId {
				return testBlackholeAdmissionPass(provider)
			}
			return testBlackholeAdmissionDark(provider)
		}, reporter.SubmitBlackholeChecks)
		args := providerEgressProbeArgs(testProviderEgressProbeSettings(1), 0)
		go func() {
			defer close(done)
			if _, tripped, err := pass.runBlackholeBatch(t.Context(), args, nil, nil, 22, providers); err != nil || !tripped {
				t.Errorf("guard changed: tripped=%t error=%v", tripped, err)
			}
		}()
		synctest.Wait()
		pending := egressProbeBlackholeResults.snapshot()
		close(releaseTail)
		<-done
		after := egressProbeBlackholeResults.snapshot()
		if pending.pending-before.pending != 5 || pending.completed[blackholeResultNegative]-before.completed[blackholeResultNegative] != 5 || pending.publications[blackholeResultOk][blackholePublicationAcknowledged]-before.publications[blackholeResultOk][blackholePublicationAcknowledged] != 16 {
			t.Error("ordinary negatives and already-ACKed safe completions were conflated")
		}
		if after.pending != before.pending || after.dispositions[blackholeNegativeDarkGuard]-before.dispositions[blackholeNegativeDarkGuard] != 5 || after.publications[blackholeResultNotMeasured][blackholePublicationAcknowledged]-before.publications[blackholeResultNotMeasured][blackholePublicationAcknowledged] != 5 || after.publications[blackholeResultNegative][blackholePublicationAcknowledged] != before.publications[blackholeResultNegative][blackholePublicationAcknowledged] || after.publications[blackholeResultTls][blackholePublicationAcknowledged]-before.publications[blackholeResultTls][blackholePublicationAcknowledged] != 1 {
			t.Error("guarded negatives were lost, retained forever, or counted as acknowledged provider failures")
		}
	})
}

// Both the final guard and incomplete-sample guard preserve the same result
// fields. This fixture ends admission after two completions, without canceling
// their owner, so the lone negative is unknown rather than a small-tail pass.
func TestBlackholeResultIncompleteSampleHasDistinctDisposition(t *testing.T) {
	before := egressProbeBlackholeResults.snapshot()
	providers := testBlackholeEarlyProviders(4)
	stop := make(chan struct{})
	completed := 0
	pass := testBlackholeProgressWork(func(_ context.Context, provider prober.Provider) fleetprobe.BlackholeResult {
		if provider.ClientId == providers[0].ClientId {
			return testBlackholeAdmissionDark(provider)
		}
		return testBlackholeAdmissionPass(provider)
	}, func(context.Context, []ingest.BlackholeCheck) error { return nil })
	pass.blackholeOptions.AdmissionDone = stop
	pass.blackholeOptions.OnCompleted = func(fleetprobe.BlackholeResult) {
		completed++
		if completed == 2 {
			close(stop)
		}
	}
	args := providerEgressProbeArgs(testProviderEgressProbeSettings(1), 0)
	args.DarkBatchGuardMinChecks = 3
	summary, tripped, err := pass.runBlackholeBatch(t.Context(), args, nil, nil, 1, providers)
	after := egressProbeBlackholeResults.snapshot()
	if err != nil || tripped || len(summary.Checks) != 2 || summary.NotMeasured != 1 || after.pending != before.pending || after.dispositions[blackholeNegativeIncomplete]-before.dispositions[blackholeNegativeIncomplete] != 1 {
		t.Errorf("incomplete sample changed verdict/ownership: completed=%d unknown=%d tripped=%t error=%v", len(summary.Checks), summary.NotMeasured, tripped, err)
	}
}

// The readiness recheck can veto only ordinary negatives. Its disposition is
// neither a provider failure nor an attempted unknown publication.
func TestBlackholeResultReadinessLossIsNotPublicationLoss(t *testing.T) {
	before := egressProbeBlackholeResults.snapshot()
	reads := 0
	pass := testBlackholeProgressWork(func(_ context.Context, provider prober.Provider) fleetprobe.BlackholeResult {
		return testBlackholeAdmissionDark(provider)
	}, func(context.Context, []ingest.BlackholeCheck) error {
		t.Error("withheld negative was submitted")
		return nil
	})
	pass.readiness = &providerEgressProbeReadiness{minimum: 1, available: func(context.Context) (model.ByteCount, error) {
		reads++
		if reads == 1 {
			return 1, nil
		}
		return 0, nil
	}}
	args := providerEgressProbeArgs(testProviderEgressProbeSettings(1), 0)
	summary, _, err := pass.runBlackholeBatch(t.Context(), args, nil, nil, 1, testBlackholeEarlyProviders(1))
	after := egressProbeBlackholeResults.snapshot()
	if !errors.Is(err, errProviderEgressProbeUnfunded) || len(summary.Checks) != 0 || after.pending != before.pending || after.dispositions[blackholeNegativeReadiness]-before.dispositions[blackholeNegativeReadiness] != 1 || after.publications != before.publications {
		t.Error("readiness hold became a provider negative or failed publication")
	}
}

// Joined completed evidence can still be withheld by task cancellation or a
// runner failure. Neither path may retain pending counts or invent a submit.
func TestBlackholeResultFailedOwnersKeepDistinctWithheldReasons(t *testing.T) {
	for _, testCase := range []struct {
		cancel      bool
		err         error
		disposition providerEgressBlackholeNegativeDisposition
	}{
		{cancel: true, disposition: blackholeNegativeCanceled},
		{err: errors.New("synthetic joined runner failure"), disposition: blackholeNegativeOwnerExit},
	} {
		before := egressProbeBlackholeResults.snapshot()
		ctx, cancel := context.WithCancel(t.Context())
		pass := testBlackholeProgressWork(func(_ context.Context, provider prober.Provider) fleetprobe.BlackholeResult {
			return testBlackholeAdmissionDark(provider)
		}, func(context.Context, []ingest.BlackholeCheck) error {
			t.Error("withheld negative was submitted")
			return nil
		})
		pass.runBlackhole = func(ctx context.Context, providers []prober.Provider, options fleetprobe.BlackholeOptions) (fleetprobe.BlackholeSummary, error) {
			summary, err := fleetprobe.RunBlackhole(ctx, providers, options)
			if err != nil {
				t.Error(err)
			}
			if testCase.cancel {
				cancel()
			}
			return summary, testCase.err
		}
		args := providerEgressProbeArgs(testProviderEgressProbeSettings(1), 0)
		summary, _, err := pass.runBlackholeBatch(ctx, args, nil, nil, 1, testBlackholeEarlyProviders(1))
		cancel()
		after := egressProbeBlackholeResults.snapshot()
		if err == nil || len(summary.Checks) != 0 || after.pending != before.pending || after.dispositions[testCase.disposition]-before.dispositions[testCase.disposition] != 1 || after.publications != before.publications {
			t.Errorf("failed owner boundary changed: canceled=%t error=%v", testCase.cancel, err)
		}
	}
}

// A small complete due tail remains measurable under the existing guard
// minimum; telemetry must not convert its negative into unknown or dark.
func TestBlackholeResultEligibleNegativeKeepsOriginalPublication(t *testing.T) {
	before := egressProbeBlackholeResults.snapshot()
	reporter := newEgressProbeMetricsReporter(&testBlackholeProgressIngest{submit: func(_ context.Context, checks []ingest.BlackholeCheck) error {
		if len(checks) != 1 || checks[0].Ok || checks[0].NotMeasured || checks[0].Failure != "all_destinations_failed" {
			t.Error("telemetry changed the eligible negative's fields")
		}
		return nil
	}}, nil)
	pass := testBlackholeProgressWork(func(_ context.Context, provider prober.Provider) fleetprobe.BlackholeResult {
		return testBlackholeAdmissionDark(provider)
	}, reporter.SubmitBlackholeChecks)
	args := providerEgressProbeArgs(testProviderEgressProbeSettings(1), 0)
	args.DarkBatchGuardMinChecks = 2
	summary, tripped, err := pass.runBlackholeBatch(t.Context(), args, nil, nil, 1, testBlackholeEarlyProviders(1))
	after := egressProbeBlackholeResults.snapshot()
	if err != nil || tripped || summary.Dark != 1 || summary.NotMeasured != 0 || after.pending != before.pending || after.dispositions[blackholeNegativeEligible]-before.dispositions[blackholeNegativeEligible] != 1 || after.publications[blackholeResultNegative][blackholePublicationAcknowledged]-before.publications[blackholeResultNegative][blackholePublicationAcknowledged] != 1 {
		t.Errorf("telemetry changed original small-tail evidence: tripped=%t dark=%d unknown=%d error=%v", tripped, summary.Dark, summary.NotMeasured, err)
	}
}

// All values are fixed, including unknown failure text. A scrape must never
// acquire a provider, URL, or error-string dimension.
func TestBlackholeResultMetricsHaveFixedZeroDomain(t *testing.T) {
	metrics := newProviderEgressBlackholeResultMetrics()
	owner := metrics.begin()
	owner.observe(ingest.BlackholeCheck{ClientId: "synthetic-private", Failure: "https://private.example/synthetic?token=never-a-label"})
	owner.close()
	registry := prometheus.NewPedanticRegistry()
	registry.MustRegister(metrics)
	families, err := registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]int{
		"urnetwork_egress_probe_blackhole_completed_results_total":     4,
		"urnetwork_egress_probe_blackhole_publication_rows_total":      16,
		"urnetwork_egress_probe_blackhole_negative_dispositions_total": 6,
		"urnetwork_egress_probe_blackhole_negative_pending":            1,
		"urnetwork_egress_probe_blackhole_result_observation_enabled":  1,
	}
	got := map[string]int{}
	for _, family := range families {
		got[family.GetName()] = len(family.Metric)
		for _, metric := range family.Metric {
			for _, label := range metric.Label {
				if label.GetName() != "result" && label.GetName() != "outcome" && label.GetName() != "disposition" || strings.Contains(label.GetValue(), "private") || strings.Contains(label.GetValue(), "example") {
					t.Fatal("a nonfixed/private diagnostic label escaped")
				}
			}
		}
	}
	if !reflect.DeepEqual(got, want) || metrics.snapshot().pending != 0 {
		t.Errorf("bounded zero-domain metric contract changed: %v", got)
	}
}
