package work

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/qualityprobe/egresshealth"
	"github.com/urnetwork/server/session"
)

// A security recheck is aimed at a URL the provider already failed, so it is
// no evidence of how the site does at the provider's place: it stays out of
// the site tally the TLS judge's place rule reads, while a drawn turn is
// tallied as before. Otherwise an intercepting provider's own rechecks would
// fill its place's tally with failures until the site read as broken there.
func TestUrlTurnSecurityRecheckIsLeftOutOfTheSiteTally(t *testing.T) {
	inner := newRecordingEgressProbeIngest()
	batch := newProviderEgressFullBatch(testFullBatchSink(inner))
	run := func(source string) *egresshealth.Result {
		return &egresshealth.Result{UrlSource: source, OkCount: 0, Total: 1,
			Checks: []egresshealth.CheckResult{{Name: "rechecked-site", Class: egresshealth.ClassSite, StatusCode: 403, ByteCount: 1}}}
	}
	ctx := context.Background()
	for provider, source := range map[string]string{"drawn-provider": "general_country_unavailable", "rechecking-provider": "security_recheck"} {
		if err := batch.SubmitEgressHealth(ctx, provider, run(source)); err != nil {
			t.Fatal(err)
		}
		if err := batch.ReportAttempt(ctx, provider, ""); err != nil {
			t.Fatal(err)
		}
	}
	tallied := map[string]int{}
	var tallyLock sync.Mutex
	places := map[string]model.ProviderEgressPlace{
		"drawn-provider":      {CountryCode: "zz"},
		"rechecking-provider": {CountryCode: "zy"},
	}
	batch.release(ctx, false, places, nil, 0.9, func(_ context.Context, _ time.Time, run model.ProviderEgressRunTally, loads []model.ProviderEgressSiteLoad) {
		tallyLock.Lock()
		defer tallyLock.Unlock()
		tallied[run.Place.CountryCode] += len(loads)
	})
	if tallied["zz"] != 1 {
		t.Fatalf("a drawn URL turn was not tallied: %v", tallied)
	}
	if tallied["zy"] != 0 {
		t.Fatalf("a security recheck was tallied as a load of its site at the provider's place: %v", tallied)
	}
}

// A direct check calls a certificate the system roots cannot verify a
// failure, as the probe does, and anything that never reaches a verdict
// (a refused dial, a peer that hangs up mid-handshake) inconclusive.
func TestProviderEgressTlsDirectCheckClassifiesOnlyVerificationFailures(t *testing.T) {
	ctx := context.Background()
	untrusted := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer untrusted.Close()
	if outcome := observeProviderEgressTlsDirectAt(ctx, "127.0.0.1", untrusted.Listener.Addr().String()); outcome != model.ProviderEgressTlsDirectFailed {
		t.Errorf("an unverifiable certificate was %s", outcome)
	}

	closed, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	refusedAddr := closed.Addr().String()
	closed.Close()
	if outcome := observeProviderEgressTlsDirectAt(ctx, "127.0.0.1", refusedAddr); outcome != model.ProviderEgressTlsDirectInconclusive {
		t.Errorf("a refused dial was %s", outcome)
	}

	hangup, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer hangup.Close()
	go func() {
		for {
			conn, err := hangup.Accept()
			if err != nil {
				return
			}
			conn.Close()
		}
	}()
	if outcome := observeProviderEgressTlsDirectAt(ctx, "127.0.0.1", hangup.Addr().String()); outcome != model.ProviderEgressTlsDirectInconclusive {
		t.Errorf("a peer that hung up mid-handshake was %s", outcome)
	}
	if outcome := observeProviderEgressTlsDirect(ctx, ""); outcome != model.ProviderEgressTlsDirectInconclusive {
		t.Errorf("an empty host was %s", outcome)
	}
}

// A direct check dials a host only at a public address a probe target may
// have: a literal or a name that is only inward is inconclusive and never
// dialed from the taskworker.
func TestProviderEgressTlsDirectCheckDialsOnlyPublicAddresses(t *testing.T) {
	ctx := context.Background()
	for _, host := range []string{"127.0.0.1", "10.0.0.1", "192.168.1.1", "100.64.0.1", "169.254.169.254", "::1", "fc00::1", "localhost", ""} {
		if addr, ok := providerEgressTlsDirectAddress(ctx, host); ok {
			t.Errorf("%q dials %s", host, addr)
		}
		if outcome := observeProviderEgressTlsDirect(ctx, host); outcome != model.ProviderEgressTlsDirectInconclusive {
			t.Errorf("%q was %s", host, outcome)
		}
	}
	for _, host := range []string{"1.1.1.1", "2606:4700:4700::1111"} {
		if addr, ok := providerEgressTlsDirectAddress(ctx, host); !ok || addr.String() != host {
			t.Errorf("public literal %q resolved to %s %t", host, addr, ok)
		}
	}
}

// With no served pool the judge changes nothing: retiring every finding, or
// keeping a stale served set, would both be guesses. It reads no finding,
// carries the clock on as it was, and is no judged run.
func TestProviderEgressTlsJudgeWithoutAPoolChangesNothing(t *testing.T) {
	before := testutil.ToFloat64(providerEgressTlsJudgeRunsTotal.WithLabelValues("pool_unavailable"))
	lastJudged := testutil.ToFloat64(providerEgressTlsJudgeLastJudgedTimestamp)
	leftAt := time.Date(2026, time.November, 7, 9, 0, 0, 0, time.UTC)
	args := &JudgeProviderEgressTlsFindingsArgs{UnservedSince: map[string]time.Time{"left-site": leftAt}}
	result, err := judgeProviderEgressTlsFindings(context.Background(), time.Now(), args, &providerEgressTlsJudgeSources{
		served: func(context.Context) (map[string]bool, error) { return nil, errors.New("synthetic pool refusal") },
		siteSettings: func() (*model.ProviderEgressSiteSettings, error) {
			t.Fatal("the judge read its settings without a pool")
			return nil, nil
		},
		direct: func(context.Context, string) model.ProviderEgressTlsDirectOutcome {
			t.Fatal("the judge checked a host without a pool")
			return model.ProviderEgressTlsDirectInconclusive
		},
	})
	if err != nil || result.Skipped != "pool_unavailable" || result.Findings != 0 {
		t.Fatalf("judge without a pool: %+v %v", result, err)
	}
	if len(result.UnservedSince) != 1 || !result.UnservedSince["left-site"].Equal(leftAt) {
		t.Fatalf("a run without a pool did not carry the clock on: %v", result.UnservedSince)
	}
	if after := testutil.ToFloat64(providerEgressTlsJudgeRunsTotal.WithLabelValues("pool_unavailable")); after != before+1 {
		t.Fatalf("pool_unavailable runs %v -> %v", before, after)
	}
	if after := testutil.ToFloat64(providerEgressTlsJudgeLastJudgedTimestamp); after != lastJudged {
		t.Fatalf("a run without a pool moved the last judged run %v -> %v", lastJudged, after)
	}
}

// The next run carries the clock in its arguments, and the startup schedule,
// which merges into the pending run, keeps them: a deploy restarts nobody's
// count.
func TestProviderEgressTlsJudgePostCarriesTheClock(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		clientSession := session.NewLocalClientSession(ctx, "0.0.0.0:0", nil)
		defer clientSession.Cancel()
		leftAt := time.Date(2026, time.November, 7, 9, 0, 0, 0, time.UTC)
		readArgs := func() (*JudgeProviderEgressTlsFindingsArgs, time.Time) {
			var argsJson string
			var runAt time.Time
			server.Db(ctx, func(conn server.PgConn) {
				server.Raise(conn.QueryRow(ctx, `SELECT args_json, run_at FROM pending_task WHERE run_once_key = '["judge_provider_egress_tls_findings"]'`).Scan(&argsJson, &runAt))
			})
			args := &JudgeProviderEgressTlsFindingsArgs{}
			server.Raise(json.Unmarshal([]byte(argsJson), args))
			return args, runAt
		}
		before := server.NowUtc()
		server.Tx(ctx, func(tx server.PgTx) {
			// the evaluator deletes its completed RunOnce row before Post
			server.RaisePgResult(tx.Exec(ctx, `DELETE FROM pending_task WHERE run_once_key = '["judge_provider_egress_tls_findings"]'`))
			server.Raise(JudgeProviderEgressTlsFindingsPost(&JudgeProviderEgressTlsFindingsArgs{},
				&JudgeProviderEgressTlsFindingsResult{UnservedSince: map[string]time.Time{"left-site": leftAt}}, clientSession, tx))
		})
		args, runAt := readArgs()
		if len(args.UnservedSince) != 1 || !args.UnservedSince["left-site"].Equal(leftAt) {
			t.Fatalf("the next run's arguments lost the clock: %+v", args)
		}
		if delay := runAt.Sub(before); delay < providerEgressTlsJudgeInterval || providerEgressTlsJudgeInterval+time.Minute < delay {
			t.Fatalf("the next run is %s out, want %s", delay, providerEgressTlsJudgeInterval)
		}

		server.Tx(ctx, func(tx server.PgTx) {
			ScheduleJudgeProviderEgressTlsFindings(clientSession, tx)
		})
		args, rescheduledAt := readArgs()
		if len(args.UnservedSince) != 1 || !args.UnservedSince["left-site"].Equal(leftAt) {
			t.Fatalf("the startup schedule reset the clock: %+v", args)
		}
		if !rescheduledAt.Before(runAt) {
			t.Fatalf("the startup schedule did not bring the run forward: %s, was %s", rescheduledAt, runAt)
		}
	})
}

// One run of the task: the hosts it checks are only those of findings still
// to be judged and recent enough for a check now to be close to them; a
// finding whose destination one run reads out of the pool is judged as if it
// were served, and retired without a check only once the carried clock shows
// it out for a whole refresh interval; and every stored verdict and check is
// counted.
func TestProviderEgressTlsJudgeChecksOnlyFindingsItCanJudge(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		now := server.NowUtc().Truncate(time.Microsecond)
		fail := func(clientId server.Id, name string, target string, at time.Time) {
			destination := egresshealth.Destination{Name: name, Class: egresshealth.ClassSite, Url: target}
			model.SetProviderEgressHealth(ctx, &model.ProviderEgressHealth{
				RunId: server.NewId(), ClientId: clientId, MeasuredAt: at, Total: 1, TLSAuthenticationFailure: true,
				UrlProbeEvidence: &egresshealth.UrlProbeEvidence{
					PolicyVersion: egresshealth.UrlProbePolicyVersion, Policy: egresshealth.DefaultUrlProbePolicy(),
					Destination: destination, MeasuredAt: at, ContentMatcherVersion: 1, FailureStage: "request",
					Security: []egresshealth.UrlProbeSecurityEvent{{Destination: destination, MeasuredAt: at, TlsFailure: true}},
				},
			})
		}
		report := func(name string, target string, at time.Time) server.Id {
			clientId := server.NewId()
			model.Testing_CreateDevice(ctx, server.NewId(), server.NewId(), clientId, "", "")
			fail(clientId, name, target, at)
			return clientId
		}
		fresh := report("fresh-site", "https://fresh.example/", now.Add(-time.Minute))
		unserved := report("unserved-site", "https://unserved.example/", now.Add(-time.Minute))
		old := report("old-site", "https://old.example/", now.Add(-2*time.Hour))

		verdicts := func(verdict model.ProviderEgressTlsVerdict) float64 {
			return testutil.ToFloat64(providerEgressTlsVerdictsTotal.WithLabelValues(verdict.String()))
		}
		providerBefore, retiredBefore := verdicts(model.ProviderEgressTlsVerdictProvider), verdicts(model.ProviderEgressTlsVerdictRetired)
		verifiedBefore := testutil.ToFloat64(providerEgressTlsDirectChecksTotal.WithLabelValues("verified"))
		checked := []string{}
		var checkedLock sync.Mutex
		sources := &providerEgressTlsJudgeSources{
			served: func(context.Context) (map[string]bool, error) {
				return map[string]bool{"fresh-site": true, "old-site": true}, nil
			},
			siteSettings: func() (*model.ProviderEgressSiteSettings, error) {
				return model.DefaultProviderEgressSiteSettings(), nil
			},
			direct: func(_ context.Context, host string) model.ProviderEgressTlsDirectOutcome {
				checkedLock.Lock()
				defer checkedLock.Unlock()
				checked = append(checked, host)
				return model.ProviderEgressTlsDirectVerified
			},
		}
		result, err := judgeProviderEgressTlsFindings(ctx, now, &JudgeProviderEgressTlsFindingsArgs{}, sources)
		if err != nil || result.Skipped != "" || result.Findings != 3 || result.Changed != 2 {
			t.Fatalf("judge run: %+v %v", result, err)
		}
		// the checks run concurrently, so in no fixed order
		slices.Sort(checked)
		if !slices.Equal(checked, []string{"fresh.example", "unserved.example"}) {
			t.Fatalf("checked hosts %v, want the two fresh findings'", checked)
		}
		excluded := model.GetAllProviderEgressTLSAuthenticationFailedClientIds(ctx)
		if !excluded[fresh] || !excluded[unserved] || excluded[old] {
			t.Fatalf("excluded fresh=%t unserved=%t old=%t, want the two corroborated fresh findings", excluded[fresh], excluded[unserved], excluded[old])
		}
		if result.Verdicts["provider"] != 2 || result.Verdicts["retired"] != 0 || result.Verdicts["pending"] != 1 {
			t.Fatalf("run verdicts %v", result.Verdicts)
		}
		if len(result.UnservedSince) != 1 || !result.UnservedSince["unserved-site"].Equal(now) {
			t.Fatalf("the run did not start the clock of the name it read out of the pool: %v", result.UnservedSince)
		}
		if verdicts(model.ProviderEgressTlsVerdictProvider) != providerBefore+2 || verdicts(model.ProviderEgressTlsVerdictRetired) != retiredBefore {
			t.Fatal("stored verdicts were not counted")
		}
		if testutil.ToFloat64(providerEgressTlsDirectChecksTotal.WithLabelValues("verified")) != verifiedBefore+2 {
			t.Fatal("the direct checks were not counted")
		}

		// The judged provider fails again: the newer failure is judged on its
		// own evidence and decided provider again, which is stored but moves no
		// verdict, so nothing is counted as moved. It stays excluded throughout.
		fail(fresh, "fresh-site", "https://fresh.example/", now.Add(time.Minute))
		if !model.GetAllProviderEgressTLSAuthenticationFailedClientIds(ctx)[fresh] {
			t.Fatal("a repeated failure lapsed the judged provider out of quarantine")
		}
		checked = []string{}
		result, err = judgeProviderEgressTlsFindings(ctx, now.Add(2*time.Minute), &JudgeProviderEgressTlsFindingsArgs{UnservedSince: result.UnservedSince}, sources)
		if err != nil || result.Changed != 1 || result.Verdicts["provider"] != 2 || !slices.Equal(checked, []string{"fresh.example"}) {
			t.Fatalf("re-judging a newer failure: %+v %v checked=%v", result, err, checked)
		}
		if verdicts(model.ProviderEgressTlsVerdictProvider) != providerBefore+2 {
			t.Fatal("a re-decided provider verdict was counted as moved")
		}
		if !model.GetAllProviderEgressTLSAuthenticationFailedClientIds(ctx)[fresh] {
			t.Fatal("re-judging the newer failure released the provider")
		}
		// Each failure is judged once: a third run with no newer failure checks
		// nothing and stores nothing.
		checked = []string{}
		result, err = judgeProviderEgressTlsFindings(ctx, now.Add(3*time.Minute), &JudgeProviderEgressTlsFindingsArgs{UnservedSince: result.UnservedSince}, sources)
		if err != nil || result.Changed != 0 || len(checked) != 0 {
			t.Fatalf("a decided failure was judged again: %+v %v checked=%v", result, err, checked)
		}

		// The destination has stayed out of the pool on every run, as the
		// carried clock says, for a whole refresh interval: its finding is
		// retired without a check, and stops excluding.
		retireAt := result.UnservedSince["unserved-site"].Add(model.DefaultProviderEgressSiteSettings().SiteRefreshInterval())
		checked = []string{}
		result, err = judgeProviderEgressTlsFindings(ctx, retireAt, &JudgeProviderEgressTlsFindingsArgs{UnservedSince: result.UnservedSince}, sources)
		if err != nil || result.Changed != 1 || result.Verdicts["retired"] != 1 || result.Verdicts["provider"] != 1 || len(checked) != 0 {
			t.Fatalf("retiring a finding out of the pool for a refresh interval: %+v %v checked=%v", result, err, checked)
		}
		if verdicts(model.ProviderEgressTlsVerdictRetired) != retiredBefore+1 {
			t.Fatal("the retirement was not counted")
		}
		if excluded := model.GetAllProviderEgressTLSAuthenticationFailedClientIds(ctx); !excluded[fresh] || excluded[unserved] {
			t.Fatalf("after the retirement excluded fresh=%t unserved=%t, want only the served provider", excluded[fresh], excluded[unserved])
		}
	})
}

// The run counts the URL and country pairs whose place evidence it withheld,
// once per pair however many findings share it, by reason.
func TestProviderEgressTlsJudgeCountsWithheldPairsOnce(t *testing.T) {
	russia, turkey := server.NewId(), server.NewId()
	judgement := func(urlKey string, country server.Id, reason string) *model.ProviderEgressTlsJudgement {
		return &model.ProviderEgressTlsJudgement{
			Finding:       &model.ProviderEgressTlsFinding{ClientId: server.NewId(), UrlKey: urlKey, CountryLocationId: &country},
			PlaceWithheld: reason,
		}
	}
	counts := providerEgressTlsPlaceWithheldPairCounts([]*model.ProviderEgressTlsJudgement{
		judgement("bbc", russia, model.ProviderEgressTlsPlaceWithheldBudget),
		judgement("bbc", russia, model.ProviderEgressTlsPlaceWithheldBudget),
		judgement("bbc", turkey, model.ProviderEgressTlsPlaceWithheldError),
		judgement("line", russia, model.ProviderEgressTlsPlaceWithheldLimit),
		judgement("discord", turkey, ""),
	})
	if counts["budget"] != 1 || counts["error"] != 1 || counts["limit"] != 1 || len(counts) != 3 {
		t.Fatalf("withheld pairs %v, want one of each reason", counts)
	}
}
