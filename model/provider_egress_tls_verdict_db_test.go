// What a URL TLS finding does to its provider (provider_egress_tls_verdict.go).
// These tests reach the judge only through provider_egress_tls_verdict_seam_test.go.
package model

import (
	"context"
	"testing"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/qualityprobe/egresshealth"
)

// A synthetic site at target.
func testingTlsSite(name string, target string) egresshealth.Destination {
	return egresshealth.Destination{Name: name, Class: egresshealth.ClassSite, Url: target}
}

// A provider on a network of its own.
func testingTlsProvider(ctx context.Context) server.Id {
	clientId := server.NewId()
	Testing_CreateDevice(ctx, server.NewId(), server.NewId(), clientId, "", "")
	return clientId
}

// One URL turn of clientId on site at `at`, admitted to cycleStartedAt (zero
// for no cycle): a certificate failure on its only hop when tlsFailure, else
// a verified response with real content.
func testingTlsUrlResult(clientId server.Id, cycleStartedAt time.Time, site egresshealth.Destination, at time.Time, tlsFailure bool) *ProviderEgressHealth {
	evidence := fp2TestUrlProbeEvidence(at, !tlsFailure)
	evidence.Destination = site
	evidence.Security = []egresshealth.UrlProbeSecurityEvent{{Destination: site, MeasuredAt: at, TlsFailure: tlsFailure, TlsAuthenticated: !tlsFailure}}
	ok := 1
	if tlsFailure {
		// the handshake failed, so nothing came back
		ok = 0
		evidence.ContentClassification, evidence.PerformanceClassification, evidence.FailureStage = "", "", "request"
		evidence.StatusCode, evidence.ByteCount, evidence.WireByteCount, evidence.WireSampleByteCount = 0, 0, 0, 0
		evidence.BodyComplete, evidence.RequestWritten, evidence.FirstByteReceived = false, false, false
		evidence.TtfbMillis, evidence.BodyMillis, evidence.BodyBitsPerSecond = 0, 0, 0
	}
	return &ProviderEgressHealth{
		RunId: server.NewId(), ClientId: clientId, CycleStartedAt: cycleStartedAt, MeasuredAt: at,
		OKCount: ok, Total: 1, TLSAuthenticationFailure: tlsFailure, UrlProbeEvidence: evidence,
	}
}

// Which of clientIds are hard-excluded. The score export's set and the request
// path's read-through must agree on every one of them.
func testingTlsExcluded(t testing.TB, ctx context.Context, clientIds []server.Id) map[server.Id]bool {
	t.Helper()
	exported := GetAllProviderEgressTLSAuthenticationFailedClientIds(ctx)
	requested, err := readProviderHardExclusions(ctx, clientIds)
	if err != nil {
		t.Fatal(err)
	}
	excluded := map[server.Id]bool{}
	for _, clientId := range clientIds {
		if exported[clientId] != requested[clientId] {
			t.Fatalf("the score export and the request path disagree about %s: export=%t request=%t", clientId, exported[clientId], requested[clientId])
		}
		if exported[clientId] {
			excluded[clientId] = true
		}
	}
	return excluded
}

// Inserts a URL cycle for each provider, started at startedAt and due now.
func testingTlsCycles(ctx context.Context, startedAt time.Time, clientIds ...server.Id) {
	server.Tx(ctx, func(tx server.PgTx) {
		for _, clientId := range clientIds {
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO provider_egress_probe_cycle (client_id,cycle_started_at,next_attempt_at,eligible)
				VALUES($1,$2,$2,true)`, clientId, startedAt))
		}
	})
}

// The provider's next URL turn.
func testingTlsNextAttempt(t testing.TB, ctx context.Context, clientId server.Id) time.Time {
	t.Helper()
	var next time.Time
	server.Db(ctx, func(conn server.PgConn) {
		server.Raise(conn.QueryRow(ctx, `SELECT next_attempt_at FROM provider_egress_probe_cycle WHERE client_id=$1`, clientId).Scan(&next))
	})
	return next
}

// A provider that fails TLS on a URL which verifies directly from the server,
// with nothing common-mode or place-level to explain it, is intercepting:
// it is excluded, from the score export's set and the request path alike,
// while the providers that verify the URL are not. Its finding excludes
// nothing before that corroboration, holds while it keeps failing, and an
// authenticated response from the same URL clears it.
func TestTlsFindingSpecificToItsProviderStillExcludesIt(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		now := server.NowUtc().Truncate(time.Microsecond)
		site := testingTlsSite("intercepted-site", "https://intercepted.example/")
		intercepting := testingTlsProvider(ctx)
		verifying := []server.Id{testingTlsProvider(ctx), testingTlsProvider(ctx), testingTlsProvider(ctx)}
		all := append([]server.Id{intercepting}, verifying...)
		SetProviderEgressHealth(ctx, testingTlsUrlResult(intercepting, time.Time{}, site, now, true))
		for i, clientId := range verifying {
			SetProviderEgressHealth(ctx, testingTlsUrlResult(clientId, time.Time{}, site, now.Add(time.Duration(i+1)*time.Second), false))
		}

		if excluded := testingTlsExcluded(t, ctx, all); len(excluded) != 0 {
			t.Fatalf("a TLS finding excluded its provider before anything corroborated it: %v", excluded)
		}
		// an inconclusive direct check corroborates nothing
		testingJudgeTlsFindings(t, ctx, now.Add(time.Minute), []string{site.Name}, nil)
		if excluded := testingTlsExcluded(t, ctx, all); len(excluded) != 0 {
			t.Fatalf("an uncorroborated TLS finding excluded its provider: %v", excluded)
		}
		if verdict := testingTlsVerdict(t, ctx, intercepting, site.Url); verdict != "pending" {
			t.Fatalf("an uncorroborated finding was judged %q", verdict)
		}

		// the URL verifies directly from the server, and nothing else fails it
		testingJudgeTlsFindings(t, ctx, now.Add(2*time.Minute), []string{site.Name}, map[string]string{"intercepted.example": "verified"})
		excluded := testingTlsExcluded(t, ctx, all)
		if !excluded[intercepting] || len(excluded) != 1 {
			t.Fatalf("a provider failing TLS where the URL verifies was not the one excluded: %v", excluded)
		}
		if verdict := testingTlsVerdict(t, ctx, intercepting, site.Url); verdict != "provider" {
			t.Fatalf("a provider-specific finding was judged %q", verdict)
		}

		SetProviderEgressHealth(ctx, testingTlsUrlResult(intercepting, time.Time{}, site, now.Add(3*time.Minute), true))
		if !testingTlsExcluded(t, ctx, all)[intercepting] {
			t.Fatal("a repeated failure lapsed a judged finding out of quarantine")
		}
		SetProviderEgressHealth(ctx, testingTlsUrlResult(intercepting, time.Time{}, site, now.Add(4*time.Minute), false))
		if excluded := testingTlsExcluded(t, ctx, all); len(excluded) != 0 {
			t.Fatalf("an authenticated response from the same URL did not clear its finding: %v", excluded)
		}
	})
}

// When providers on many networks fail TLS on one URL within the hour, the
// fault is the site's or the prober's, never theirs: none of them is
// excluded, even while the URL verifies directly from this server (a prober
// host whose clock is wrong), and each finding is recorded as a site or
// prober fault. A URL that fails direct verification from the server is a
// site or prober fault on a single finding.
func TestTlsFailureAcrossManyNetworksIsASiteFaultAndExcludesNone(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		now := server.NowUtc().Truncate(time.Microsecond)
		broken := testingTlsSite("broken-site", "https://broken.example/")
		expired := testingTlsSite("expired-site", "https://expired.example/")
		failing := []server.Id{}
		for i := range testingTlsCommonModeNetworkCount {
			clientId := testingTlsProvider(ctx)
			failing = append(failing, clientId)
			SetProviderEgressHealth(ctx, testingTlsUrlResult(clientId, time.Time{}, broken, now.Add(time.Duration(i)*time.Minute), true))
		}
		lone := testingTlsProvider(ctx)
		SetProviderEgressHealth(ctx, testingTlsUrlResult(lone, time.Time{}, expired, now, true))
		all := append(append([]server.Id{}, failing...), lone)

		judgedAt := now.Add(time.Duration(testingTlsCommonModeNetworkCount) * time.Minute)
		testingJudgeTlsFindings(t, ctx, judgedAt, []string{broken.Name, expired.Name},
			map[string]string{"broken.example": "verified", "expired.example": "failed"})
		if excluded := testingTlsExcluded(t, ctx, all); len(excluded) != 0 {
			t.Fatalf("a site or prober TLS fault excluded providers: %d of %d", len(excluded), len(all))
		}
		for _, clientId := range failing {
			if verdict := testingTlsVerdict(t, ctx, clientId, broken.Url); verdict != "site" {
				t.Fatalf("a failure shared by %d networks was judged %q", testingTlsCommonModeNetworkCount, verdict)
			}
		}
		if verdict := testingTlsVerdict(t, ctx, lone, expired.Url); verdict != "site" {
			t.Fatalf("a failure the server's own check shares was judged %q", verdict)
		}

		// the fault recorded, a later run keeps it
		testingJudgeTlsFindings(t, ctx, judgedAt.Add(5*time.Minute), []string{broken.Name, expired.Name},
			map[string]string{"broken.example": "verified", "expired.example": "verified"})
		if excluded := testingTlsExcluded(t, ctx, all); len(excluded) != 0 {
			t.Fatalf("a recorded site fault excluded providers on a later run: %v", excluded)
		}
	})
}

// A finding whose destination the pool stops serving can never be failed or
// cleared by a drawn turn while it stays out, so once it has stayed out for a
// whole refresh interval the finding is retired: it stops excluding its
// provider, the claim no longer offers its stored destination for recheck,
// and a provider whose quota is met is no longer turned for it. One judge
// run that reads the destination out of the pool changes nothing, and a
// retired finding whose destination is served again is rechecked again, so a
// provider that still intercepts is excluded again on its next failure.
func TestTlsFindingForAnUnservedUrlIsRetired(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		city := egressTestCity(ctx, "Synthetic City", "Synthetic Region", "Synthetic Country", "zz")
		provider := egressTestConnect(ctx, t, city, egressTestUnsampled, nil, nil)
		testingInsertProviderReliabilityWitness(ctx, provider.clientId)
		now := server.NowUtc()
		UpdateClientLocationReliabilities(ctx, now.Add(-time.Hour), now)
		now = server.NowUtc().Add(time.Second).Truncate(time.Microsecond)
		initial := ClaimProviderUrlProbeDue(ctx, now, 1, 0, 1)
		if len(initial) != 1 {
			t.Fatalf("missing the initial URL admission: %+v", initial)
		}
		for range ProviderUrlProbeRunTarget {
			testingSetUrlProbeHealth(ctx, &ProviderEgressHealth{RunId: server.NewId(), ClientId: provider.clientId,
				CycleStartedAt: initial[0].CycleStartedAt, MeasuredAt: now, OKCount: 1, Total: 1})
		}
		removed := testingTlsSite("removed-site", "https://removed.example/")
		SetProviderEgressHealth(ctx, testingTlsUrlResult(provider.clientId, time.Time{}, removed, now, true))
		judge := testingNewTlsJudgeChain()
		judge.judge(t, ctx, now.Add(time.Minute), []string{removed.Name}, map[string]string{"removed.example": "verified"})
		clients := []server.Id{provider.clientId}
		if !testingTlsExcluded(t, ctx, clients)[provider.clientId] {
			t.Fatal("the precondition failed: a corroborated finding on a served URL did not exclude")
		}
		reclaimAt := func(at time.Time) []ProviderUrlProbeDue {
			server.Tx(ctx, func(tx server.PgTx) {
				server.RaisePgResult(tx.Exec(ctx, `UPDATE provider_egress_probe_cycle SET next_attempt_at=$2 WHERE client_id=$1`, provider.clientId, at))
			})
			return ClaimProviderUrlProbeDue(ctx, at, 1, 0, 1)
		}
		offered := func(due []ProviderUrlProbeDue) bool {
			return len(due) == 1 && len(due[0].SecurityDestinations) == 1 && due[0].SecurityDestinations[0].Url == removed.Url
		}
		if due := reclaimAt(now.Add(2 * time.Minute)); !offered(due) {
			t.Fatalf("the precondition failed: a quota-full provider with a served finding was not offered its recheck: %+v", due)
		}

		// One judge run reads the destination out of the pool: a pool read, a
		// release in flight, or a refresh holding the site back for a while.
		leftAt := now.Add(3 * time.Minute)
		judge.judge(t, ctx, leftAt, []string{}, nil)
		if !testingTlsExcluded(t, ctx, clients)[provider.clientId] {
			t.Fatal("a finding whose URL was out of the pool for one judge run stopped excluding its provider")
		}
		if due := reclaimAt(now.Add(4 * time.Minute)); !offered(due) {
			t.Fatalf("a finding whose URL was out of the pool for one judge run is no longer rechecked: %+v", due)
		}
		// out of the pool on every run until just short of a refresh interval
		judge.judge(t, ctx, leftAt.Add(testingTlsRetireAfter()-5*time.Minute), []string{}, nil)
		if verdict := testingTlsVerdict(t, ctx, provider.clientId, removed.Url); verdict != "provider" {
			t.Fatalf("a finding whose URL has been out of the pool for less than a refresh interval was judged %q", verdict)
		}

		// out for a whole refresh interval: retired
		judge.judge(t, ctx, leftAt.Add(testingTlsRetireAfter()), []string{}, nil)
		if testingTlsExcluded(t, ctx, clients)[provider.clientId] {
			t.Fatal("a finding on a URL the pool no longer serves still excludes its provider")
		}
		if verdict := testingTlsVerdict(t, ctx, provider.clientId, removed.Url); verdict != "retired" {
			t.Fatalf("a finding on an unserved URL was judged %q", verdict)
		}
		if due := reclaimAt(now.Add(5 * time.Minute)); len(due) != 0 {
			t.Fatalf("a retired finding still drives rechecks of a quota-full provider: %+v", due)
		}

		// The pool serves the destination again: the finding is rechecked
		// again, and the provider, which still intercepts, fails the recheck
		// and is judged its own on that failure.
		servedAgainAt := leftAt.Add(testingTlsRetireAfter() + 5*time.Minute)
		judge.judge(t, ctx, servedAgainAt, []string{removed.Name}, map[string]string{"removed.example": "verified"})
		if verdict := testingTlsVerdict(t, ctx, provider.clientId, removed.Url); verdict != "pending" {
			t.Fatalf("a retired finding whose URL is served again was judged %q", verdict)
		}
		if due := reclaimAt(now.Add(6 * time.Minute)); !offered(due) {
			t.Fatalf("a retired finding whose URL is served again is not rechecked: %+v", due)
		}
		// The recheck fails, and a run soon after judges that fresh failure.
		// (Receipts stay at the test's own clock, inside the stored windows; a
		// judge run stores no time of its own, only the failure it decides.)
		SetProviderEgressHealth(ctx, testingTlsUrlResult(provider.clientId, time.Time{}, removed, now.Add(7*time.Minute), true))
		testingJudgeTlsFindings(t, ctx, now.Add(8*time.Minute), []string{removed.Name}, map[string]string{"removed.example": "verified"})
		if !testingTlsExcluded(t, ctx, clients)[provider.clientId] {
			t.Fatal("a provider that still intercepts once its URL is served again was not excluded again")
		}
	})
}

// A measured outcome on a URL the provider has an open finding for is a
// security recheck: evidence about that URL alone. Passing or failing, it
// stays out of the eight-hour ratio and the rolling quota, so a provider is
// not penalized twice for one finding. The finding's own first failure, a
// normal draw, counts, and once a recheck clears the finding, turns on that
// URL count again.
func TestTlsRecheckOutcomesStayOutOfTheRatio(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		// every turn in the past, inside the window the ratio reads now
		now := server.NowUtc().Add(-time.Minute).Truncate(time.Microsecond)
		clientId := testingTlsProvider(ctx)
		cycleStartedAt := now.Add(-time.Hour)
		testingTlsCycles(ctx, cycleStartedAt, clientId)
		rechecked := testingTlsSite("rechecked-site", "https://rechecked.example/")
		other := testingTlsSite("other-site", "https://other.example/")

		// a normal draw fails TLS and opens the finding: counted
		SetProviderEgressHealth(ctx, testingTlsUrlResult(clientId, cycleStartedAt, rechecked, now, true))
		// two failed rechecks, then one that authenticates and clears it
		SetProviderEgressHealth(ctx, testingTlsUrlResult(clientId, cycleStartedAt, rechecked, now.Add(time.Second), true))
		SetProviderEgressHealth(ctx, testingTlsUrlResult(clientId, cycleStartedAt, rechecked, now.Add(2*time.Second), true))
		SetProviderEgressHealth(ctx, testingTlsUrlResult(clientId, cycleStartedAt, rechecked, now.Add(3*time.Second), false))
		// with the finding cleared, both count
		SetProviderEgressHealth(ctx, testingTlsUrlResult(clientId, cycleStartedAt, other, now.Add(4*time.Second), false))
		SetProviderEgressHealth(ctx, testingTlsUrlResult(clientId, cycleStartedAt, rechecked, now.Add(5*time.Second), false))

		counts, ok := GetAllProviderEgressHealthCounts(ctx)[clientId]
		if !ok || counts.OKCount != 2 || counts.Total != 3 {
			t.Fatalf("recheck outcomes entered the eight-hour ratio: ok=%d total=%d, want 2/3 (the first failure and the two later turns)", counts.OKCount, counts.Total)
		}
		var runs int
		server.Db(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(ctx, `SELECT run_count FROM (`+providerUrlProbeRunWindowSql("$1", "$2")+`) AS recent`,
				clientId, now.Add(time.Minute)).Scan(&runs))
		})
		if runs != 3 {
			t.Fatalf("recheck outcomes filled the rolling quota: runs=%d, want 3", runs)
		}
	})
}

// A provider with an open finding is turned no faster than a fully measured
// provider: its next turn is the renewal spacing (24 minutes) after each
// receipt, failed or not, never the one-minute failure pace at which a site
// fault leaving a finding on every provider that probed the site multiplied
// fleet probe load. A provider with no finding keeps the failure pace.
func TestTlsRecheckPaceIsCapped(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		now := server.NowUtc().Truncate(time.Microsecond)
		finding := testingTlsProvider(ctx)
		control := testingTlsProvider(ctx)
		cycleStartedAt := now.Add(-time.Hour)
		testingTlsCycles(ctx, cycleStartedAt, finding, control)
		site := testingTlsSite("capped-site", "https://capped.example/")
		assertPace := func(clientId server.Id, at time.Time, want time.Duration, what string) {
			t.Helper()
			pace := testingTlsNextAttempt(t, ctx, clientId).Sub(at)
			if pace < want*9/10 || want*11/10 < pace {
				t.Fatalf("%s: next turn %s after the receipt, want %s +/-10%%", what, pace, want)
			}
		}

		SetProviderEgressHealth(ctx, testingTlsUrlResult(finding, cycleStartedAt, site, now, true))
		assertPace(finding, now, ProviderUrlProbeRenewalSpacing, "the failure that opened a finding")
		recheckAt := now.Add(ProviderUrlProbeRenewalSpacing)
		SetProviderEgressHealth(ctx, testingTlsUrlResult(finding, cycleStartedAt, site, recheckAt, true))
		assertPace(finding, recheckAt, ProviderUrlProbeRenewalSpacing, "a failed recheck")

		// an ordinary failure with no finding keeps the one-minute pace
		failed := &ProviderEgressHealth{RunId: server.NewId(), ClientId: control, CycleStartedAt: cycleStartedAt,
			MeasuredAt: now, OKCount: 0, Total: 1, UrlProbeEvidence: fp2TestUrlProbeEvidence(now, false)}
		SetProviderEgressHealth(ctx, failed)
		assertPace(control, now, time.Minute, "an ordinary failure")
	})
}

// Where the site does not work from the provider's country (its scored loads
// there fail at the pool refresh's incompatibility share, while the country's
// exits carry its other sites), a TLS failure on it says nothing about the
// provider: a national block answering with its own certificate. It excludes
// nobody and is recorded as a place fault. The same failure from a country
// where the site works is the provider's own.
func TestTlsFailureWhereTheSiteDoesNotWorkFromTheCountryIsAPlaceFault(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		blockedCity := egressTestCity(ctx, "Blocked City", "Blocked Region", "Blocked Country", "zz")
		openCity := egressTestCity(ctx, "Open City", "Open Region", "Open Country", "zy")
		inBlocked := egressTestConnect(ctx, t, blockedCity, egressTestUnsampled, nil, nil)
		inOpen := egressTestConnect(ctx, t, openCity, egressTestUnsampled, nil, nil)
		now := server.NowUtc()
		UpdateClientLocationReliabilities(ctx, now.Add(-time.Hour), now)
		now = server.NowUtc().Truncate(time.Microsecond)

		site := testingTlsSite("blocked-site", "https://blocked.example/")
		tally := func(countryCode string, siteFailures int) {
			loads := []ProviderEgressSiteLoad{}
			for i := range 40 {
				loads = append(loads, ProviderEgressSiteLoad{Name: site.Name, Ok: siteFailures <= i})
			}
			for range 200 {
				loads = append(loads, ProviderEgressSiteLoad{Name: "working-site", Ok: true})
			}
			AddProviderEgressRunTally(ctx, now, ProviderEgressRunTally{Place: ProviderEgressPlace{CountryCode: countryCode}}, loads)
		}
		tally("zz", 39)
		tally("zy", 2)

		SetProviderEgressHealth(ctx, testingTlsUrlResult(inBlocked.clientId, time.Time{}, site, now, true))
		SetProviderEgressHealth(ctx, testingTlsUrlResult(inOpen.clientId, time.Time{}, site, now, true))
		testingJudgeTlsFindings(t, ctx, now.Add(time.Minute), []string{site.Name, "working-site"}, map[string]string{"blocked.example": "verified"})

		excluded := testingTlsExcluded(t, ctx, []server.Id{inBlocked.clientId, inOpen.clientId})
		if excluded[inBlocked.clientId] {
			t.Fatal("a TLS failure on a site that fails from the whole country excluded the provider")
		}
		if verdict := testingTlsVerdict(t, ctx, inBlocked.clientId, site.Url); verdict != "place" {
			t.Fatalf("a failure where the site does not work from the country was judged %q", verdict)
		}
		if !excluded[inOpen.clientId] {
			t.Fatal("a TLS failure where the site works from the country did not exclude the provider")
		}
	})
}
