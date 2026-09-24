// Tests for the two batch guards of connect/GEOMAP.md §11.3, and for how a
// full batch's runs are scored, tallied and released once its guard has
// judged it.
package work

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/urnetwork/operator-proxy/egresshealth"
	"github.com/urnetwork/operator-proxy/fleetprobe"
	"github.com/urnetwork/operator-proxy/ingest"
	"github.com/urnetwork/operator-proxy/prober"

	"github.com/urnetwork/server/model"
)

// A blackhole batch with the given numbers of passing, dark, TLS-failed and
// not-measured checks, counted the way the prober counts them.
func testBlackholeSummary(passes int, dark int, tlsFailures int, notMeasured int) fleetprobe.BlackholeSummary {
	checkedAt := time.Date(2026, time.September, 1, 12, 0, 0, 0, time.UTC)
	summary := fleetprobe.BlackholeSummary{
		Dark:        dark + tlsFailures,
		NotMeasured: notMeasured,
	}
	add := func(prefix string, count int, check ingest.BlackholeCheck) {
		for i := range count {
			check.ClientId = fmt.Sprintf("%s-%d", prefix, i)
			check.CheckedAt = checkedAt
			summary.Checks = append(summary.Checks, check)
		}
	}
	add("pass", passes, ingest.BlackholeCheck{Ok: true})
	add("dark", dark, ingest.BlackholeCheck{Failure: egresshealth.FailureAllDestinationsFailed})
	add("tls", tlsFailures, ingest.BlackholeCheck{Failure: egresshealth.FailureTlsAuthentication})
	add("unmeasured", notMeasured, ingest.BlackholeCheck{Failure: egresshealth.FailureNotMeasured, NotMeasured: true})
	return summary
}

// Three dark checks in ten is the prober's fault until proven otherwise: the
// two ordinary negatives go back as not measured, while the passes and the
// forged certificate stand.
func TestProviderEgressBlackholeGuardDiscardsTheNegativesOfADarkBatch(t *testing.T) {
	rules := model.DefaultProviderEgressRules()
	summary := testBlackholeSummary(7, 2, 1, 0)

	guarded, share, tripped := providerEgressBlackholeGuard(summary, rules)
	if !tripped || share != 0.3 {
		t.Fatalf("guard = share %v tripped %t, want 0.3 tripped", share, tripped)
	}
	if len(guarded.Checks) != 10 || guarded.Dark != 1 || guarded.NotMeasured != 2 {
		t.Fatalf("guarded summary = %d checks, %d dark, %d not measured; want 10, 1, 2", len(guarded.Checks), guarded.Dark, guarded.NotMeasured)
	}
	for i, check := range guarded.Checks {
		original := summary.Checks[i]
		if check.ClientId != original.ClientId || !check.CheckedAt.Equal(original.CheckedAt) {
			t.Fatalf("check %d changed provider or time: %+v from %+v", i, check, original)
		}
		switch {
		case original.Ok:
			if check != original {
				t.Errorf("a pass was changed by the guard: %+v", check)
			}
		case original.Failure == egresshealth.FailureTlsAuthentication:
			if check != original {
				t.Errorf("a TLS-authentication failure was discarded: %+v", check)
			}
		default:
			if check.Ok || !check.NotMeasured || check.Failure != egresshealth.FailureNotMeasured {
				t.Errorf("a discarded negative is not a check that measured nothing: %+v", check)
			}
		}
	}
	if summary.Checks[7].NotMeasured {
		t.Fatal("the guard rewrote the batch it was given")
	}
}

// One dark check in ten is a provider, and the batch goes on as measured.
func TestProviderEgressBlackholeGuardKeepsABatchUnderTheShare(t *testing.T) {
	summary := testBlackholeSummary(9, 1, 0, 0)
	guarded, share, tripped := providerEgressBlackholeGuard(summary, model.DefaultProviderEgressRules())
	if tripped || share != 0.1 {
		t.Fatalf("guard = share %v tripped %t, want 0.1 kept", share, tripped)
	}
	if !slices.Equal(guarded.Checks, summary.Checks) || guarded.Dark != 1 || guarded.NotMeasured != 0 {
		t.Fatalf("a kept batch was changed: %+v", guarded)
	}
}

// The share is over measured checks only, the line itself does not trip, and
// a batch under the minimum is not judged.
func TestProviderEgressBlackholeGuardJudgesOnlyMeasuredChecks(t *testing.T) {
	rules := model.DefaultProviderEgressRules()
	for _, test := range []struct {
		name                                   string
		passes, dark, tlsFailures, notMeasured int
		share                                  float64
		tripped                                bool
	}{
		{name: "on the line", passes: 8, dark: 2, share: 0.2, tripped: false},
		{name: "not measured beside a kept batch", passes: 8, dark: 2, notMeasured: 20, share: 0.2, tripped: false},
		{name: "not measured beside a dark batch", passes: 7, dark: 3, notMeasured: 20, share: 0.3, tripped: true},
		{name: "tls failures count as dark", passes: 7, tlsFailures: 3, share: 0.3, tripped: true},
		{name: "under the minimum", dark: 3, share: 1, tripped: false},
		{name: "nothing measured", notMeasured: 12, share: 0, tripped: false},
	} {
		summary := testBlackholeSummary(test.passes, test.dark, test.tlsFailures, test.notMeasured)
		guarded, share, tripped := providerEgressBlackholeGuard(summary, rules)
		if share != test.share || tripped != test.tripped {
			t.Errorf("%s: share %v tripped %t, want %v %t", test.name, share, tripped, test.share, test.tripped)
		}
		if !tripped && !slices.Equal(guarded.Checks, summary.Checks) {
			t.Errorf("%s: a batch the guard kept was changed", test.name)
		}
	}
}

// Through the batch runner: what reaches the server from a tripped batch is
// the guarded checks, and the result counts what was submitted.
func TestProviderEgressProbeBlackholeBatchSubmitsTheGuardedChecks(t *testing.T) {
	args := providerEgressProbeArgs(testProviderEgressProbeSettings(1), 0)
	summary := testBlackholeSummary(7, 3, 0, 0)
	submitted := []ingest.BlackholeCheck{}
	pass := &providerEgressProbePass{
		runBlackhole: func(context.Context, []prober.Provider, fleetprobe.BlackholeOptions) (fleetprobe.BlackholeSummary, error) {
			return summary, nil
		},
		submitBlackholeChecks: func(_ context.Context, checks []ingest.BlackholeCheck) error {
			submitted = append(submitted, checks...)
			return nil
		},
	}
	got, tripped, err := pass.runBlackholeBatch(context.Background(), args, nil, nil, 1, fleetprobe.ProvidersFromClientIds([]string{"pass-0"}))
	if err != nil || !tripped {
		t.Fatalf("batch = tripped %t error %v, want tripped", tripped, err)
	}
	if got.Dark != 0 || got.NotMeasured != 3 || len(submitted) != 10 {
		t.Fatalf("guarded batch = %d dark, %d not measured, %d submitted; want 0, 3, 10", got.Dark, got.NotMeasured, len(submitted))
	}
	for _, check := range submitted {
		if !check.Ok && !check.NotMeasured {
			t.Errorf("a negative of a tripped batch reached the server: %+v", check)
		}
	}
}

// A run with the given scored loads and passes.
func testHealthRun(total int, ok int) *egresshealth.Result {
	return &egresshealth.Result{Total: total, OkCount: ok}
}

// The run guard is over the scored loads of the batch, with the same line
// and minimum rules as the dark guard.
func TestProviderEgressRunGuardJudgesTheScoredLoads(t *testing.T) {
	rules := model.DefaultProviderEgressRules()
	for _, test := range []struct {
		name    string
		runs    []*egresshealth.Result
		share   float64
		tripped bool
	}{
		{name: "on the line", runs: []*egresshealth.Result{testHealthRun(10, 7), testHealthRun(10, 7), testHealthRun(10, 7)}, share: 0.3, tripped: false},
		{name: "above the line", runs: []*egresshealth.Result{testHealthRun(10, 6), testHealthRun(10, 6), testHealthRun(10, 6)}, share: 0.4, tripped: true},
		{name: "under the minimum", runs: []*egresshealth.Result{testHealthRun(10, 0), testHealthRun(10, 0)}, share: 1, tripped: false},
		{name: "runs that measured nothing are not runs", runs: []*egresshealth.Result{testHealthRun(10, 0), testHealthRun(10, 0), testHealthRun(0, 0), nil}, share: 1, tripped: false},
		{name: "loads weigh, not runs", runs: []*egresshealth.Result{testHealthRun(40, 40), testHealthRun(5, 0), testHealthRun(5, 0)}, share: 0.2, tripped: false},
		{name: "nothing", runs: nil, share: 0, tripped: false},
	} {
		share, tripped := providerEgressRunGuard(test.runs, rules)
		if share != test.share || tripped != test.tripped {
			t.Errorf("%s: share %v tripped %t, want %v %t", test.name, share, tripped, test.share, test.tripped)
		}
	}
}

// Scoring over a pool that holds one scored site, one on probation and one
// blocked in China.
func testHealthScoring() *model.ProviderEgressHealthScoring {
	return model.NewProviderEgressHealthScoring([]*model.ProviderEgressDestination{
		{Name: "scored-site", Class: "site", Active: true},
		{Name: "probation-site", Class: "site", Active: true, Probation: true},
		{Name: "blocked-site", Class: "site", Active: true, Incompatible: []model.ProviderEgressDestinationPlace{{Country: "cn"}}},
	})
}

// A run over that pool: the scored site passes, the probationary and blocked
// sites fail, a built-in site the pool does not hold passes, and a canary and
// a load whose tunnel went away are in no count.
func testScoringRun() *egresshealth.Result {
	return &egresshealth.Result{
		Checks: []egresshealth.CheckResult{
			{Name: "scored-site", Class: "site", Ok: true},
			{Name: "probation-site", Class: "site"},
			{Name: "blocked-site", Class: "site"},
			{Name: "builtin-site", Class: "cdn", Ok: true},
			{Name: "canary-site", Class: "site", Ok: true, Canary: true},
			{Name: "gone-site", Class: "site", NotMeasured: true},
		},
		Total:       4,
		OkCount:     2,
		NotMeasured: 1,
		ByClass: map[egresshealth.Class]egresshealth.ClassSummary{
			"site": {Total: 3, Ok: 1},
			"cdn":  {Total: 1, Ok: 1},
		},
	}
}

// The names of a run's checks, in order.
func testCheckNames(res *egresshealth.Result) []string {
	names := []string{}
	for _, check := range res.Checks {
		names = append(names, check.Name)
	}
	return names
}

// A probationary load never counts, a blocked load does not count from the
// place it is blocked in, and everything else is left as measured.
func TestScoreEgressHealthResultTakesOutProbationAndIncompatibleLoads(t *testing.T) {
	scoring := testHealthScoring()

	run := testScoringRun()
	scored := scoreEgressHealthResult(run, model.ProviderEgressPlace{CountryCode: "cn", Region: "Beijing"}, scoring)
	if scored.Total != 2 || scored.OkCount != 2 || scored.NotMeasured != 1 {
		t.Fatalf("scored from cn = total %d ok %d not measured %d, want 2 2 1", scored.Total, scored.OkCount, scored.NotMeasured)
	}
	if got := scored.ByClass["site"]; got.Total != 1 || got.Ok != 1 {
		t.Errorf("scored site class from cn = %+v, want 1 of 1", got)
	}
	if got := scored.ByClass["cdn"]; got.Total != 1 || got.Ok != 1 {
		t.Errorf("scored cdn class from cn = %+v, want 1 of 1", got)
	}
	if names := testCheckNames(scored); !slices.Equal(names, []string{"scored-site", "builtin-site", "canary-site", "gone-site"}) {
		t.Errorf("scored checks from cn = %v", names)
	}

	scored = scoreEgressHealthResult(run, model.ProviderEgressPlace{CountryCode: "de"}, scoring)
	if scored.Total != 3 || scored.OkCount != 2 {
		t.Fatalf("scored from de = total %d ok %d, want 3 2", scored.Total, scored.OkCount)
	}
	if got := scored.ByClass["site"]; got.Total != 2 || got.Ok != 1 {
		t.Errorf("scored site class from de = %+v, want 1 of 2", got)
	}

	// the measured run is what the per-destination metrics are drawn from
	if run.Total != 4 || run.OkCount != 2 || run.ByClass["site"].Total != 3 || len(run.Checks) != 6 {
		t.Fatalf("scoring changed the measured run: %+v", run)
	}
}

// A class left with no scored load is dropped rather than reported as zero
// of zero, and without scoring the run is its own score.
func TestScoreEgressHealthResultDropsEmptiedClasses(t *testing.T) {
	run := &egresshealth.Result{
		Checks:  []egresshealth.CheckResult{{Name: "probation-site", Class: "site"}, {Name: "builtin-site", Class: "cdn", Ok: true}},
		Total:   2,
		OkCount: 1,
		ByClass: map[egresshealth.Class]egresshealth.ClassSummary{"site": {Total: 1}, "cdn": {Total: 1, Ok: 1}},
	}
	scored := scoreEgressHealthResult(run, model.ProviderEgressPlace{CountryCode: "de"}, testHealthScoring())
	if _, ok := scored.ByClass["site"]; ok || len(scored.ByClass) != 1 {
		t.Fatalf("an emptied class is still reported: %+v", scored.ByClass)
	}
	if scoreEgressHealthResult(run, model.ProviderEgressPlace{}, nil) != run {
		t.Fatal("a run without scoring was replaced")
	}
	if scoreEgressHealthResult(nil, model.ProviderEgressPlace{}, testHealthScoring()) != nil {
		t.Fatal("a missing run gained a score")
	}
}

// A run of twenty scored sites plus a probationary load, a canary and a load
// that was not measured; failing names the scored sites that fail.
func testSiteLoadsRun(failing ...string) *egresshealth.Result {
	res := &egresshealth.Result{}
	for i := range 20 {
		name := fmt.Sprintf("site-%d", i)
		res.Checks = append(res.Checks, egresshealth.CheckResult{Name: name, Class: "site", Ok: !slices.Contains(failing, name)})
	}
	res.Checks = append(res.Checks,
		egresshealth.CheckResult{Name: "probation-site", Class: "site"},
		egresshealth.CheckResult{Name: "canary-site", Class: "site", Ok: true, Canary: true},
		egresshealth.CheckResult{Name: "gone-site", Class: "site", NotMeasured: true},
	)
	return res
}

// Each load's exit is judged on the other scored sites of its run, so a site
// that fails a healthy exit is tallied as failing a healthy exit.
func TestEgressHealthSiteLoadsJudgesEachLoadOnTheOtherSites(t *testing.T) {
	scoring := testHealthScoring()
	place := model.ProviderEgressPlace{CountryCode: "de"}

	loads, runHealthy := egressHealthSiteLoads(testSiteLoadsRun("site-0"), place, scoring, 0.9)
	if !runHealthy {
		t.Fatal("a run passing 19 of 20 scored sites is not healthy")
	}
	if len(loads) != 22 {
		t.Fatalf("tallied loads = %d, want the 20 scored, the probationary and the canary", len(loads))
	}
	byName := map[string]model.ProviderEgressSiteLoad{}
	for _, load := range loads {
		byName[load.Name] = load
	}
	if load := byName["site-0"]; load.Ok || !load.Healthy || load.Canary {
		t.Errorf("the failing site on an exit passing all the others = %+v, want a failure on a healthy exit", load)
	}
	if load := byName["site-1"]; !load.Ok || !load.Healthy {
		t.Errorf("a passing site beside 18 of 19 others passing = %+v, want healthy", load)
	}
	if load := byName["probation-site"]; load.Ok || !load.Healthy {
		t.Errorf("the probationary load = %+v, want a failure judged on all 20 scored sites", load)
	}
	if load := byName["canary-site"]; !load.Canary || !load.Ok || load.Healthy {
		t.Errorf("the canary = %+v, want a passing canary", load)
	}
	if _, ok := byName["gone-site"]; ok {
		t.Error("a load that was not measured was tallied")
	}

	// three failures in twenty leave every exit under the line
	loads, runHealthy = egressHealthSiteLoads(testSiteLoadsRun("site-0", "site-1", "site-2"), place, scoring, 0.9)
	if runHealthy {
		t.Fatal("a run passing 17 of 20 scored sites is healthy")
	}
	for _, load := range loads {
		if load.Healthy {
			t.Errorf("a load of an unhealthy exit was tallied healthy: %+v", load)
		}
	}
}

// An ingest that records every call in order, and refuses the location
// submissions of the named providers.
type recordingEgressProbeIngest struct {
	calls        []string
	health       map[string]*egresshealth.Result
	refuseSubmit map[string]bool
}

// A recording ingest that refuses the location submissions of refused.
func newRecordingEgressProbeIngest(refused ...string) *recordingEgressProbeIngest {
	ingest := &recordingEgressProbeIngest{
		health:       map[string]*egresshealth.Result{},
		refuseSubmit: map[string]bool{},
	}
	for _, providerClientId := range refused {
		ingest.refuseSubmit[providerClientId] = true
	}
	return ingest
}

// Implements prober.Submitter.
func (self *recordingEgressProbeIngest) Submit(_ context.Context, providerClientId string, exitIp string, _ time.Time) error {
	self.calls = append(self.calls, "submit "+providerClientId+" "+exitIp)
	if self.refuseSubmit[providerClientId] {
		return errors.New("synthetic refused submission")
	}
	return nil
}

// Implements prober.AttemptReporter.
func (self *recordingEgressProbeIngest) ReportAttempt(_ context.Context, providerClientId string, probeFailure string) error {
	self.calls = append(self.calls, "attempt "+providerClientId+" "+probeFailure)
	return nil
}

// Implements prober.HealthReporter.
func (self *recordingEgressProbeIngest) SubmitEgressHealth(_ context.Context, providerClientId string, res *egresshealth.Result) error {
	self.calls = append(self.calls, "health "+providerClientId)
	self.health[providerClientId] = res
	return nil
}

// Records nothing; bandwidth is not judged by the guard.
func (self *recordingEgressProbeIngest) ReserveBandwidth(context.Context, string, int64) error {
	return nil
}

// Records nothing; bandwidth is not judged by the guard.
func (self *recordingEgressProbeIngest) SubmitBandwidth(context.Context, string, string, float64, int64) error {
	return nil
}

// Records the checks as one call.
func (self *recordingEgressProbeIngest) SubmitBlackholeChecks(_ context.Context, checks []ingest.BlackholeCheck) error {
	self.calls = append(self.calls, fmt.Sprintf("blackhole %d", len(checks)))
	return nil
}

// A metrics reporter over inner that resolves no exit, so no test reads the
// GeoLite2 database.
func testFullBatchSink(inner egressProbeIngest) *egressProbeMetricsReporter {
	sink := newEgressProbeMetricsReporter(inner, nil)
	sink.resolveExit = nil
	return sink
}

// Fills a batch the way the prober's reporters would: a provider with a
// health run, an exit and an attempt; one whose tunnel failed; and one whose
// exit the server will refuse.
func testFillFullBatch(t *testing.T, batch *providerEgressFullBatch) {
	t.Helper()
	ctx := context.Background()
	observedAt := time.Date(2026, time.September, 1, 12, 0, 0, 0, time.UTC)
	run := testScoringRun()
	run.ExitIp = "203.0.113.7"
	for _, err := range []error{
		batch.SubmitEgressHealth(ctx, "provider-a", run),
		batch.Submit(ctx, "provider-a", "203.0.113.7", observedAt),
		batch.ReportAttempt(ctx, "provider-a", ""),
		batch.ReportAttempt(ctx, "provider-b", prober.FailureTunnel),
		batch.Submit(ctx, "provider-c", "203.0.113.9", observedAt),
		batch.ReportAttempt(ctx, "provider-c", ""),
	} {
		if err != nil {
			t.Fatalf("the batch refused a call: %v", err)
		}
	}
}

// A released batch goes on provider by provider in the order it was measured:
// the health run scored and tallied, then the exit, then the attempt, which a
// refused exit turns into submit_failed.
func TestProviderEgressFullBatchReleaseSubmitsInOrderAndTallies(t *testing.T) {
	inner := newRecordingEgressProbeIngest("provider-c")
	batch := newProviderEgressFullBatch(testFullBatchSink(inner))
	testFillFullBatch(t, batch)
	if len(inner.calls) != 0 {
		t.Fatalf("the batch passed calls on before its release: %v", inner.calls)
	}

	// one recorded run with its loads
	type tally struct {
		run   model.ProviderEgressRunTally
		loads []model.ProviderEgressSiteLoad
	}
	tallies := []tally{}
	places := map[string]model.ProviderEgressPlace{"provider-a": {CountryCode: "cn"}}
	submitFailures := batch.release(context.Background(), false, places, testHealthScoring(), 0.9,
		func(_ context.Context, _ time.Time, run model.ProviderEgressRunTally, loads []model.ProviderEgressSiteLoad) {
			tallies = append(tallies, tally{run: run, loads: loads})
		},
	)
	if submitFailures != 1 {
		t.Fatalf("submit failures = %d, want the one refused exit", submitFailures)
	}
	want := []string{
		"health provider-a",
		"submit provider-a 203.0.113.7",
		"attempt provider-a ",
		"attempt provider-b " + prober.FailureTunnel,
		"submit provider-c 203.0.113.9",
		"attempt provider-c " + prober.FailureSubmit,
	}
	if !slices.Equal(inner.calls, want) {
		t.Fatalf("released calls = %q, want %q", inner.calls, want)
	}
	if scored := inner.health["provider-a"]; scored.Total != 2 || scored.OkCount != 2 {
		t.Fatalf("health submitted from cn = total %d ok %d, want the run as it may count", scored.Total, scored.OkCount)
	}
	if len(tallies) != 1 || tallies[0].run.Place.CountryCode != "cn" || tallies[0].run.EchoFailed {
		t.Fatalf("tallies = %+v, want provider-a's run at cn with its echo", tallies)
	}
	if len(tallies[0].loads) != 5 {
		t.Fatalf("tallied loads = %+v, want every measured load but none that was not", tallies[0].loads)
	}
}

// A batch the run guard held back submits nothing but its attempts, each as
// the guard's class, so its providers come round again after the backoff.
func TestProviderEgressFullBatchReleaseRequeuesATrippedBatch(t *testing.T) {
	inner := newRecordingEgressProbeIngest()
	batch := newProviderEgressFullBatch(testFullBatchSink(inner))
	testFillFullBatch(t, batch)

	tallied := false
	submitFailures := batch.release(context.Background(), true, nil, nil, 0.9,
		func(context.Context, time.Time, model.ProviderEgressRunTally, []model.ProviderEgressSiteLoad) {
			tallied = true
		},
	)
	want := []string{
		"attempt provider-a " + model.ProbeRunBatchGuardClass,
		"attempt provider-b " + model.ProbeRunBatchGuardClass,
		"attempt provider-c " + model.ProbeRunBatchGuardClass,
	}
	if submitFailures != 0 || tallied || !slices.Equal(inner.calls, want) {
		t.Fatalf("tripped release = %q, failures %d, tallied %t; want only %q", inner.calls, submitFailures, tallied, want)
	}
}

// Through the batch runner: a batch whose runs fail above the line reaches
// the server as re-queued attempts only, and its result says so.
func TestProviderEgressProbeFullBatchGuardTripSubmitsNothing(t *testing.T) {
	args := providerEgressProbeArgs(testProviderEgressProbeSettings(1), 0)
	inner := newRecordingEgressProbeIngest()
	pass := &providerEgressProbePass{
		fullSink: testFullBatchSink(inner),
		runFull: func(ctx context.Context, providers []prober.Provider, options fleetprobe.FullOptions) (prober.Summary, error) {
			for _, provider := range providers {
				run := testHealthRun(10, 5)
				run.ExitIp = "203.0.113.7"
				_ = options.HealthResults.SubmitEgressHealth(ctx, provider.ClientId, run)
				_ = options.Submit.Submit(ctx, provider.ClientId, run.ExitIp, time.Now())
				_ = options.Attempts.ReportAttempt(ctx, provider.ClientId, "")
			}
			return prober.Summary{Attempted: len(providers), Submitted: len(providers)}, nil
		},
	}
	outcome := pass.runFullBatch(context.Background(), args, nil, nil, testDueProviders("provider-a", "provider-b", "provider-c"))
	if outcome.err != nil || !outcome.guardTripped {
		t.Fatalf("outcome = tripped %t error %v, want a tripped guard", outcome.guardTripped, outcome.err)
	}
	if outcome.summary.Submitted != 0 || outcome.summary.Failed != 3 {
		t.Fatalf("tripped summary = %+v, want nothing submitted", outcome.summary)
	}
	for _, call := range inner.calls {
		if call != "attempt provider-a "+model.ProbeRunBatchGuardClass &&
			call != "attempt provider-b "+model.ProbeRunBatchGuardClass &&
			call != "attempt provider-c "+model.ProbeRunBatchGuardClass {
			t.Errorf("a tripped batch submitted %q", call)
		}
	}
	if len(inner.calls) != 3 {
		t.Fatalf("tripped calls = %q, want three re-queued attempts", inner.calls)
	}
}

// The same batch under the line is submitted whole.
func TestProviderEgressProbeFullBatchUnderTheLineIsSubmitted(t *testing.T) {
	args := providerEgressProbeArgs(testProviderEgressProbeSettings(1), 0)
	inner := newRecordingEgressProbeIngest()
	pass := &providerEgressProbePass{
		fullSink: testFullBatchSink(inner),
		runFull: func(ctx context.Context, providers []prober.Provider, options fleetprobe.FullOptions) (prober.Summary, error) {
			for _, provider := range providers {
				run := testHealthRun(10, 9)
				run.ExitIp = "203.0.113.7"
				_ = options.HealthResults.SubmitEgressHealth(ctx, provider.ClientId, run)
				_ = options.Submit.Submit(ctx, provider.ClientId, run.ExitIp, time.Now())
				_ = options.Attempts.ReportAttempt(ctx, provider.ClientId, "")
			}
			return prober.Summary{Attempted: len(providers), Submitted: len(providers)}, nil
		},
	}
	outcome := pass.runFullBatch(context.Background(), args, nil, nil, testDueProviders("provider-a", "provider-b", "provider-c"))
	if outcome.err != nil || outcome.guardTripped || outcome.summary.Submitted != 3 {
		t.Fatalf("outcome = %+v, want three submitted runs", outcome)
	}
	if len(inner.calls) != 9 {
		t.Fatalf("calls = %q, want health, exit and attempt of each provider", inner.calls)
	}
}

// A run whose every load was left out of the counts measured nothing that may
// count: it is not submitted over the provider's last real run, and its loads
// still reach the tally that judges the sites left out.
func TestProviderEgressFullBatchReleaseSkipsARunWithNothingScored(t *testing.T) {
	inner := newRecordingEgressProbeIngest()
	batch := newProviderEgressFullBatch(testFullBatchSink(inner))
	run := &egresshealth.Result{
		Checks:  []egresshealth.CheckResult{{Name: "probation-site", Class: "site"}},
		Total:   1,
		ByClass: map[egresshealth.Class]egresshealth.ClassSummary{"site": {Total: 1}},
		ExitIp:  "203.0.113.7",
	}
	ctx := context.Background()
	if err := batch.SubmitEgressHealth(ctx, "provider-a", run); err != nil {
		t.Fatal(err)
	}
	if err := batch.ReportAttempt(ctx, "provider-a", ""); err != nil {
		t.Fatal(err)
	}
	tallied := 0
	batch.release(ctx, false, nil, testHealthScoring(), 0.9,
		func(_ context.Context, _ time.Time, _ model.ProviderEgressRunTally, loads []model.ProviderEgressSiteLoad) {
			tallied += len(loads)
		},
	)
	if !slices.Equal(inner.calls, []string{"attempt provider-a "}) {
		t.Fatalf("released calls = %q, want the attempt alone", inner.calls)
	}
	if tallied != 1 {
		t.Fatalf("tallied loads = %d, want the probationary load", tallied)
	}
}
