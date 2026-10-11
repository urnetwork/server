// A URL turn is a run batch of one provider. A failure that every recent
// provider of a pass shares before any destination answers is held back from
// provider history; every other measured outcome is published as before.
package work

import (
	"context"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/qualityprobe/egresshealth"
	"github.com/urnetwork/server/qualityprobe/fleetprobe"
	"github.com/urnetwork/server/qualityprobe/ingest"
	"github.com/urnetwork/server/qualityprobe/prober"
)

var testUrlCommonModeMeasuredAt = time.Date(2026, time.October, 10, 23, 28, 0, 0, time.UTC)

// One measured URL load as fetchUrlProbe reports it, wrapped in the run and
// evidence the URL sampler builds around a single check.
func testUrlCommonModeRun(check egresshealth.CheckResult) *egresshealth.Result {
	destination := egresshealth.Destination{Name: "synthetic-site", Url: "https://site.example/", Class: egresshealth.ClassSite}
	check.Name, check.Class, check.Attempts = destination.Name, destination.Class, 1
	if check.UrlProbeSecurity == nil {
		check.UrlProbeSecurity = []egresshealth.UrlProbeSecurityEvent{}
	}
	for i := range check.UrlProbeSecurity {
		check.UrlProbeSecurity[i].Destination = destination
		check.UrlProbeSecurity[i].MeasuredAt = testUrlCommonModeMeasuredAt
	}
	okCount := 0
	if check.Ok {
		okCount = 1
	}
	return &egresshealth.Result{
		UrlSource: "general",
		UrlProbeEvidence: &egresshealth.UrlProbeEvidence{
			PolicyVersion: egresshealth.UrlProbePolicyVersion, Policy: egresshealth.DefaultUrlProbePolicy(),
			Destination: destination, MeasuredAt: testUrlCommonModeMeasuredAt, Security: check.UrlProbeSecurity,
			ContentClassification: check.ContentClassification, FailureStage: check.FailureStage,
			StatusCode: check.StatusCode, RequestWritten: check.RequestWritten, FirstByteReceived: check.FirstByteReceived,
		},
		Checks:                   []egresshealth.CheckResult{check},
		TlsAuthenticationFailure: check.TlsAuthenticationFailure,
		OkCount:                  okCount,
		Total:                    1,
		ByClass:                  map[egresshealth.Class]egresshealth.ClassSummary{destination.Class: {Ok: okCount, Total: 1}},
	}
}

// A URL that failed in stage before any destination answered: no status, no
// response byte, no TLS identity. dial_dns is what a tunnel whose first round
// trip outlives the five-second URL DNS phase reports.
func testUrlPreResponseFailure(stage string) *egresshealth.Result {
	return testUrlCommonModeRun(egresshealth.CheckResult{
		Err: "synthetic provider URL resolution timed out", FailureStage: stage, DnsMeasured: true,
	})
}

// A URL whose destination answered with a failing status: a real outcome of
// the provider's path to the site.
func testUrlAnsweredFailure(statusCode int) *egresshealth.Result {
	return testUrlCommonModeRun(egresshealth.CheckResult{
		StatusCode: statusCode, RequestWritten: true, FirstByteReceived: true, DnsMeasured: true,
		ContentClassification: "unexpected_status", FailureStage: "response_content",
		Err:              fmt.Sprintf("final URL status %d is not successful content", statusCode),
		UrlProbeSecurity: []egresshealth.UrlProbeSecurityEvent{{TlsAuthenticated: true}},
	})
}

// A URL whose peer presented a certificate that does not authenticate the
// site: independent integrity evidence that is never withheld.
func testUrlTlsAuthenticationFailure() *egresshealth.Result {
	return testUrlCommonModeRun(egresshealth.CheckResult{
		DnsMeasured: true, FailureStage: "tls", TlsAuthenticationFailure: true,
		Err:              "synthetic certificate is not valid for site.example",
		UrlProbeSecurity: []egresshealth.UrlProbeSecurityEvent{{TlsFailure: true}},
	})
}

// A URL that passed.
func testUrlPass() *egresshealth.Result {
	return testUrlCommonModeRun(egresshealth.CheckResult{
		Ok: true, StatusCode: http.StatusOK, RequestWritten: true, FirstByteReceived: true, DnsMeasured: true,
		ContentClassification: "content", ByteCount: 64 * 1024, WireByteCount: 64 * 1024,
		UrlProbeSecurity: []egresshealth.UrlProbeSecurityEvent{{TlsAuthenticated: true}},
	})
}

// One claimed URL turn: the provider and what its URL measured.
type testUrlCommonModeTurn struct {
	providerClientId string
	run              *egresshealth.Result
}

// Count distinct providers, prefix-first onwards, that each measure run.
func testUrlCommonModeTurns(prefix string, first int, count int, run *egresshealth.Result) []testUrlCommonModeTurn {
	turns := make([]testUrlCommonModeTurn, 0, count)
	for i := first; i < first+count; i++ {
		turns = append(turns, testUrlCommonModeTurn{providerClientId: fmt.Sprintf("%s-%02d", prefix, i), run: run})
	}
	return turns
}

// What one claimed turn's release acknowledged to the operator ingest.
type testUrlCommonModeRelease struct {
	providerClientId string
	claimOrdinal     int64
	probeFailure     string
	allowPacing      bool
	health           bool
}

// Runs turns through one production-shaped URL pass: the paced scheduler
// claims them in order, at most concurrency at once, each turn runs the real
// prober over a synthetic tunnel and its run reaches the operator only through
// the pass's buffered release. Claim ordinals follow the turn order, so the
// returned releases are in turn order whatever order concurrent turns finished.
func testRunUrlCommonModeTurns(t *testing.T, concurrency int, turns []testUrlCommonModeTurn) (*ProviderEgressProbeResult, []testUrlCommonModeRelease) {
	t.Helper()
	pass, args, inner := testUrlProbePass()
	args.UrlProbe.Limit, args.UrlProbe.Concurrency = concurrency, concurrency
	sink := &testingUrlCompletionIngest{recordingEgressProbeIngest: inner}
	pass.fullSink = testFullBatchSink(sink)
	claimed := 0
	pass.fullDue = func(_ context.Context, limit int) ([]ingest.DueProvider, error) {
		due := []ingest.DueProvider{}
		for ; claimed < len(turns) && len(due) < limit; claimed++ {
			due = append(due, ingest.DueProvider{
				ClientId:       turns[claimed].providerClientId,
				ClaimOrdinal:   int64(claimed + 1),
				ClaimedAt:      testUrlCommonModeMeasuredAt,
				CycleStartedAt: testUrlCommonModeMeasuredAt,
			})
		}
		return due, nil
	}
	pass.runFull = func(ctx context.Context, providers []prober.Provider, options fleetprobe.FullOptions) (prober.Summary, error) {
		if len(providers) != 1 || providers[0].ClaimOrdinal < 1 || int64(len(turns)) < providers[0].ClaimOrdinal {
			return prober.Summary{}, fmt.Errorf("a URL turn ran %d providers", len(providers))
		}
		run := turns[providers[0].ClaimOrdinal-1].run
		p := &prober.Prober{
			Open: func(context.Context, string) (*http.Client, func() error, error) {
				return &http.Client{}, func() error { return nil }, nil
			},
			Health: func(context.Context, *http.Client, egresshealth.Place) (*egresshealth.Result, error) {
				return run, nil
			},
			HealthResults: options.HealthResults,
			Attempts:      options.Attempts,
		}
		return (&prober.Scheduler{Prober: p, Concurrency: 1}).Run(ctx, providers), nil
	}

	result, err := pass.run(context.Background(), args)
	if err != nil {
		t.Fatalf("URL pass failed: %v", err)
	}

	// every joined turn has released; read the sink without contention
	providerClientIdHealthCounts := map[string]int{}
	for _, call := range inner.calls {
		var providerClientId string
		if _, err := fmt.Sscanf(call, "health %s", &providerClientId); err != nil {
			t.Fatalf("a URL turn used legacy ingest: %q", call)
		}
		providerClientIdHealthCounts[providerClientId]++
	}
	releases := make([]testUrlCommonModeRelease, len(turns))
	for _, completion := range sink.completions {
		index := completion.ClaimOrdinal - 1
		if index < 0 || int64(len(turns)) <= index || releases[index].claimOrdinal != 0 ||
			turns[index].providerClientId != completion.ClientId {
			t.Fatalf("completion lost or repeated its claim identity: %+v", completion)
		}
		releases[index] = testUrlCommonModeRelease{
			providerClientId: completion.ClientId,
			claimOrdinal:     completion.ClaimOrdinal,
			probeFailure:     completion.ProbeFailure,
			allowPacing:      completion.AllowPacing,
		}
	}
	for index, release := range releases {
		if release.claimOrdinal == 0 {
			t.Fatalf("turn %d of %s completed no claim", index, turns[index].providerClientId)
		}
	}
	// A provider's history receives a measured outcome only with its completed
	// claim; attribute health to the provider's published claims in order.
	for index := range releases {
		if releases[index].probeFailure == "" && 0 < providerClientIdHealthCounts[releases[index].providerClientId] {
			providerClientIdHealthCounts[releases[index].providerClientId]--
			releases[index].health = true
		}
	}
	for providerClientId, count := range providerClientIdHealthCounts {
		if count != 0 {
			t.Fatalf("%d health submissions of %s have no published claim", count, providerClientId)
		}
	}
	return result, releases
}

// Publishes the measured outcome as the provider's own: health, then its
// completed claim with no failure class.
func testUrlCommonModePublished(release testUrlCommonModeRelease) bool {
	return release.health && release.probeFailure == ""
}

// Held back as the run batch guard holds a batch: no health, and the claim
// completed as the guard's class with the prober's pacing permission, so the
// server re-offers it after a paced retry with no measured outcome.
func testUrlCommonModeHeld(release testUrlCommonModeRelease) bool {
	return !release.health && release.probeFailure == model.ProbeRunBatchGuardClass && release.allowPacing
}

// Counts published and held turns, and lists the turns that were neither or
// not as held says.
func testUrlCommonModeCount(releases []testUrlCommonModeRelease, held func(index int) bool) (publishedCount int, heldCount int, unexpected []int) {
	for index, release := range releases {
		switch {
		case testUrlCommonModePublished(release):
			publishedCount++
			if held != nil && held(index) {
				unexpected = append(unexpected, index)
			}
		case testUrlCommonModeHeld(release):
			heldCount++
			if held != nil && !held(index) {
				unexpected = append(unexpected, index)
			}
		default:
			unexpected = append(unexpected, index)
		}
	}
	return
}

// No turn is held back.
func testUrlCommonModeNoneHeld(int) bool {
	return false
}

// The defect of 2026-10-10: with the platform's contract setup on both tunnel
// legs consuming the URL DNS phase, every provider's turn failed in dial_dns
// before any destination answered, and the URL lane published every one as
// that provider's measured failure. Once the pass has seen sixteen distinct
// providers fail that way with no pass, the rest are held back; the fifteen
// turns before the window can judge are published as before.
func TestUrlProbeCommonModeHoldsBackAFleetWidePreResponseFailure(t *testing.T) {
	turns := testUrlCommonModeTurns("synthetic-provider", 0, 48, testUrlPreResponseFailure("dial_dns"))
	trips := testutil.ToFloat64(egressProbeBatchGuardTripsTotal.WithLabelValues("full"))
	result, releases := testRunUrlCommonModeTurns(t, 1, turns)
	publishedCount, heldCount, unexpected := testUrlCommonModeCount(releases, func(index int) bool { return 15 <= index })
	if publishedCount != 15 || heldCount != 33 || len(unexpected) != 0 {
		t.Fatalf("fleet-wide dial_dns failure: %d of 48 turns published as provider failures and %d held back (unexpected turns %v), want the 15 that fill the window published and 33 held",
			publishedCount, heldCount, unexpected)
	}
	if result.Attempted != 48 || result.Submitted != 15 || result.Failed != 33 || result.UrlNotMeasured != 0 {
		t.Fatalf("held turns were counted as accepted outcomes: %+v", result)
	}
	if tripped := testutil.ToFloat64(egressProbeBatchGuardTripsTotal.WithLabelValues("full")) - trips; tripped != 33 {
		t.Fatalf("batch guard trips = %v, want one per held turn", tripped)
	}
}

// The same judgment under a concurrent worker pool: the window records and
// judges each turn atomically, so whatever order concurrent turns finish in,
// exactly the first fifteen judged are published.
func TestUrlProbeCommonModeHoldsBackConcurrentTurns(t *testing.T) {
	turns := testUrlCommonModeTurns("synthetic-provider", 0, 64, testUrlPreResponseFailure("dial_dns"))
	result, releases := testRunUrlCommonModeTurns(t, 16, turns)
	publishedCount, heldCount, unexpected := testUrlCommonModeCount(releases, nil)
	if publishedCount != 15 || heldCount != 49 || len(unexpected) != 0 || result.Submitted != 15 || result.Failed != 49 {
		t.Fatalf("concurrent fleet-wide dial_dns failure: published %d held %d (unexpected turns %v) result %+v, want 15 and 49",
			publishedCount, heldCount, unexpected, result)
	}
}

// Every dial stage that ends before a destination answers is the same
// platform signature, whichever one each provider's turn reached.
func TestUrlProbeCommonModeHoldsBackEveryPreResponseStage(t *testing.T) {
	stages := []string{"dial_dns", "dial_dns_or_socket", "request_dns_timeout", "dial_tcp", "request_dial_timeout"}
	turns := []testUrlCommonModeTurn{}
	for i := range 32 {
		turns = append(turns, testUrlCommonModeTurn{
			providerClientId: fmt.Sprintf("synthetic-provider-%02d", i),
			run:              testUrlPreResponseFailure(stages[i%len(stages)]),
		})
	}
	_, releases := testRunUrlCommonModeTurns(t, 1, turns)
	publishedCount, heldCount, unexpected := testUrlCommonModeCount(releases, func(index int) bool { return 15 <= index })
	if publishedCount != 15 || heldCount != 17 || len(unexpected) != 0 {
		t.Fatalf("mixed pre-response stages: published %d held %d (unexpected turns %v), want 15 and 17", publishedCount, heldCount, unexpected)
	}
}

// Normal URL failure is 35-55% of turns. With 40% of providers passing, every
// measured outcome is published, the pre-response failures among them too.
func TestUrlProbeCommonModePublishesAMixedPass(t *testing.T) {
	turns := []testUrlCommonModeTurn{}
	for i := range 50 {
		run := testUrlPreResponseFailure("dial_dns")
		if i%5 < 2 {
			run = testUrlPass()
		}
		turns = append(turns, testUrlCommonModeTurn{providerClientId: fmt.Sprintf("synthetic-provider-%02d", i), run: run})
	}
	trips := testutil.ToFloat64(egressProbeBatchGuardTripsTotal.WithLabelValues("full"))
	result, releases := testRunUrlCommonModeTurns(t, 1, turns)
	publishedCount, heldCount, unexpected := testUrlCommonModeCount(releases, testUrlCommonModeNoneHeld)
	if publishedCount != 50 || len(unexpected) != 0 || result.Submitted != 50 || result.Failed != 0 {
		t.Fatalf("mixed pass: published %d held %d (unexpected turns %v) result %+v, want all 50 published", publishedCount, heldCount, unexpected, result)
	}
	if tripped := testutil.ToFloat64(egressProbeBatchGuardTripsTotal.WithLabelValues("full")) - trips; tripped != 0 {
		t.Fatalf("batch guard trips = %v in a mixed pass", tripped)
	}
}

// Every provider failing after a destination answered (a 403 or a 503) is the
// outcome of each provider's path to the site, not a platform that never
// reached it: all of it is published.
func TestUrlProbeCommonModePublishesAnsweredFailures(t *testing.T) {
	turns := []testUrlCommonModeTurn{}
	for i := range 48 {
		run := testUrlAnsweredFailure(http.StatusForbidden)
		if i%2 == 1 {
			run = testUrlAnsweredFailure(http.StatusServiceUnavailable)
		}
		turns = append(turns, testUrlCommonModeTurn{providerClientId: fmt.Sprintf("synthetic-provider-%02d", i), run: run})
	}
	result, releases := testRunUrlCommonModeTurns(t, 1, turns)
	publishedCount, heldCount, unexpected := testUrlCommonModeCount(releases, testUrlCommonModeNoneHeld)
	if publishedCount != 48 || len(unexpected) != 0 || result.Submitted != 48 || result.Failed != 0 {
		t.Fatalf("answered failures: published %d held %d (unexpected turns %v) result %+v, want all 48 published", publishedCount, heldCount, unexpected, result)
	}
}

// Fifteen providers are not the fleet, however often each fails: a provider
// claimed again after its failure pacing is still one provider in the window.
func TestUrlProbeCommonModePublishesFewerThanSixteenProviders(t *testing.T) {
	turns := []testUrlCommonModeTurn{}
	for range 3 {
		turns = append(turns, testUrlCommonModeTurns("synthetic-provider", 0, 15, testUrlPreResponseFailure("dial_dns"))...)
	}
	result, releases := testRunUrlCommonModeTurns(t, 1, turns)
	publishedCount, heldCount, unexpected := testUrlCommonModeCount(releases, testUrlCommonModeNoneHeld)
	if publishedCount != 45 || len(unexpected) != 0 || result.Submitted != 45 || result.Failed != 0 {
		t.Fatalf("fifteen providers: published %d held %d (unexpected turns %v) result %+v, want all 45 published", publishedCount, heldCount, unexpected, result)
	}
}

// A provider that passes, a destination that answers or a peer that fails TLS
// authentication proves the tunnel path works. That turn is published, and
// the pre-response failures after it are too until sixteen distinct newer
// providers have failed that way again.
func TestUrlProbeCommonModeEndsWhenAProviderReachesItsSite(t *testing.T) {
	for _, interruption := range []struct {
		name string
		run  *egresshealth.Result
	}{
		{name: "a pass", run: testUrlPass()},
		{name: "an answered failure", run: testUrlAnsweredFailure(http.StatusServiceUnavailable)},
		{name: "a tls authentication failure", run: testUrlTlsAuthenticationFailure()},
	} {
		failure := testUrlPreResponseFailure("dial_dns")
		turns := testUrlCommonModeTurns("synthetic-before", 0, 16, failure)
		turns = append(turns, testUrlCommonModeTurn{providerClientId: "synthetic-reached", run: interruption.run})
		turns = append(turns, testUrlCommonModeTurns("synthetic-after", 0, 16, failure)...)
		_, releases := testRunUrlCommonModeTurns(t, 1, turns)
		// held: the sixteenth failure before, and the sixteenth after, which
		// pushes the interruption out of the window
		publishedCount, heldCount, unexpected := testUrlCommonModeCount(releases, func(index int) bool { return index == 15 || index == 32 })
		if publishedCount != 31 || heldCount != 2 || len(unexpected) != 0 {
			t.Errorf("interrupted by %s: published %d held %d (unexpected turns %v), want turns 15 and 32 held and the other 31 published",
				interruption.name, publishedCount, heldCount, unexpected)
		}
	}
}

// Only a failure that no destination answered is the platform's signature. A
// response byte, a status, a redirect, a TLS verdict or security evidence
// means a destination answered; a pass or an unmeasured run is no failure.
func TestUrlProbePreResponseFailureNeedsNoAnswerFromAnyDestination(t *testing.T) {
	for _, stage := range []string{"dial_dns", "dial_dns_or_socket", "request_dns_timeout", "dial_tcp", "request_dial_timeout"} {
		if !providerUrlProbePreResponseFailure(testUrlPreResponseFailure(stage)) {
			t.Errorf("a %s failure was not pre-response", stage)
		}
	}
	for _, stage := range []string{
		"", "tls", "policy", "request_build", "request_tls_timeout", "request_connect_timeout",
		"request_write_timeout", "request_response_timeout", "request_timeout", "request_canceled",
		"request_eof", "request_unknown", "redirect_policy", "redirect_limit", "redirect_location",
		"response_encoding", "response_body", "response_content", "response_performance",
		"response_judgment", "tunnel_unavailable", "local_contract_acquisition", "unknown",
	} {
		if providerUrlProbePreResponseFailure(testUrlPreResponseFailure(stage)) {
			t.Errorf("a %q failure was pre-response", stage)
		}
	}

	withUnmeasured := testUrlPreResponseFailure("dial_dns")
	withUnmeasured.Checks = append(withUnmeasured.Checks,
		egresshealth.CheckResult{Name: "synthetic-unmeasured", Class: egresshealth.ClassSite, NotMeasured: true, FailureStage: "local_contract_acquisition"},
		egresshealth.CheckResult{Name: "synthetic-canary", Class: egresshealth.ClassSite, Canary: true, StatusCode: http.StatusOK, FirstByteReceived: true},
	)
	if !providerUrlProbePreResponseFailure(withUnmeasured) {
		t.Error("an unmeasured load or a canary beside the counted failure changed its verdict")
	}

	answered := func(answer func(run *egresshealth.Result)) *egresshealth.Result {
		run := testUrlPreResponseFailure("dial_dns")
		answer(run)
		return run
	}
	for _, c := range []struct {
		name string
		run  *egresshealth.Result
	}{
		{name: "a response byte", run: answered(func(run *egresshealth.Result) { run.Checks[0].FirstByteReceived = true })},
		{name: "a status", run: answered(func(run *egresshealth.Result) { run.Checks[0].StatusCode = http.StatusBadGateway })},
		{name: "an earlier redirect", run: answered(func(run *egresshealth.Result) { run.Checks[0].RedirectCount = 1 })},
		{name: "a forged certificate", run: answered(func(run *egresshealth.Result) { run.Checks[0].TlsAuthenticationFailure = true })},
		{name: "an authenticated hop", run: answered(func(run *egresshealth.Result) {
			run.Checks[0].UrlProbeSecurity = []egresshealth.UrlProbeSecurityEvent{{TlsAuthenticated: true}}
		})},
		{name: "a run tls authentication failure", run: answered(func(run *egresshealth.Result) { run.TlsAuthenticationFailure = true })},
		{name: "run security evidence", run: answered(func(run *egresshealth.Result) {
			run.UrlProbeEvidence.Security = []egresshealth.UrlProbeSecurityEvent{{TlsAuthenticated: true}}
		})},
		{name: "a pass", run: testUrlPass()},
		{name: "an answered failure", run: testUrlAnsweredFailure(http.StatusForbidden)},
		{name: "a tls authentication failure", run: testUrlTlsAuthenticationFailure()},
		{name: "nothing measured", run: answered(func(run *egresshealth.Result) {
			run.Checks[0].NotMeasured = true
			run.Total = 0
		})},
		{name: "a total its loads do not add up to", run: answered(func(run *egresshealth.Result) { run.Total = 2 })},
		{name: "no run", run: nil},
	} {
		if providerUrlProbePreResponseFailure(c.run) {
			t.Errorf("a failure with %s was pre-response", c.name)
		}
	}
}

// A batch of several providers is held only when everything it measured is a
// pre-response failure, even if its own later runs push its pass out of the
// window. The returned share is the window's.
func TestUrlProbeCommonModeNeverHoldsABatchWithAPass(t *testing.T) {
	commonMode := newProviderUrlProbeCommonMode(0, 1)
	failure := testUrlPreResponseFailure("dial_dns")
	batch := map[string]*egresshealth.Result{"synthetic-a-pass": testUrlPass()}
	for i := range providerUrlProbeCommonModeProviderCount {
		batch[fmt.Sprintf("synthetic-b-failure-%02d", i)] = failure
	}
	if share, held := commonMode.judge(batch); held || share != 1 {
		t.Fatalf("a batch with a pass: share %v held %t, want the window's share 1 and not held", share, held)
	}
	if share, held := commonMode.judge(map[string]*egresshealth.Result{"synthetic-c-failure": failure}); !held || share != 1 {
		t.Fatalf("the next pre-response failure: share %v held %t, want held", share, held)
	}
	if share, held := commonMode.judge(map[string]*egresshealth.Result{"synthetic-d-unmeasured": {}}); held || share != 1 {
		t.Fatalf("a turn that measured nothing: share %v held %t, want the window unchanged and not held", share, held)
	}
	if share, held := commonMode.judge(map[string]*egresshealth.Result{"synthetic-e-pass": testUrlPass()}); held || share != 15.0/16.0 {
		t.Fatalf("a pass: share %v held %t, want 15/16 and not held", share, held)
	}
}
