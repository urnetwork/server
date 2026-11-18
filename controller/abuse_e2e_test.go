package controller

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// §21's M5 gate (ABUSE-M5-21; §22.5's §21 row, §23.6's M5 exit; D-ABUSE-5, D-ABUSE-7, R-ABUSE-2, R-ABUSE-6, I-13):
// the composed path, and where each clause of the gate runs.
//
// The composed path, on §12's real label map and MG5's tables with recording effects and a recording record
// mailer (TestAbuseComposedPathFromTheFormToASecondReviewersGrant): a phishing report through the form, joined to
// its network by host, reaching review alone; a Web Risk hit on the same host joining the same case and killing the
// host at once through §21's one action path, applied, noticed and its record mailed; the abuse role reading the
// case, which writes its access-log row, and upholding the kill; the network's Owner appealing; the reviewer who
// upheld it refused as the appeal's decider; and a second reviewer granting it, which lifts the kill through the
// case as wrong, applied, noticed and mailed, takes the network's restricted row away and closes the case.
//
// The M5 report for §21, clause by clause (run with .stack/server-test.sh and -tags sidecar_pending, which links
// MG5's tables; without it the store cases skip):
//
//	TTL sweeps on an injected clock        model's TestAbuseRetentionTtlsAtTheirBoundaries, ...SkipsAHeldNetworkUntilItsHoldEnds,
//	                                       ...ResumesAStoppedPass; FS-48's TestFs48HoldsAcrossTheirDeadlinesOnPostgres
//	an access-log row per read             model's TestAbuseReaderLogsEachReadBeforeItAnswers, ...AnswersNothingWhenItCannotLog;
//	                                       this file's lookup; TestEveryAbuseReadLogs
//	holds pause only sweeps, GC and, from  sweeps: FS-48 here and model's retention cases; GC and the reconciler: sidecar's
//	M10, a drop's deletions (FS-92)        evsservice and gkmservice hold cases; a drop's deletions and FS-92 wait for M10's
//	                                       format 2 (D-PHASE-8), with FS-66's, FS-69's, FS-76's and FS-78's hold cases, which
//	                                       ABUSE-M10-01 reruns against api's real writes
//	each kind to its deadline (FS-48)      model's TestFs48HoldsAcrossTheirDeadlinesOnPostgres, TestFs48EraseAndNowReadNoHoldKind,
//	                                       TestLegalHoldDeadlinesFollowTheStatutes; TestLegalHoldDeadlinesEndOnThePass
//	abuse clocks across a weekend, every   model's TestFs49WeekendClocks and TestFs49WeekendClocksOnPostgres; the pages through
//	registrar or registry notice paging    pager.FakeSink: TestFileAbuseNoticePagesEveryClassOnASaturdayNight,
//	(FS-49)                                TestFileAbuseNoticeRegistrarAt0200PagesBothWithNoEscalationWait (FS-50's notice),
//	                                       TestAbuseClockPassCountsOverdueReviewsAndLiftsNothing
//	a held network's deletion copies what  model's TestRemoveNetworkUnderAHoldCopiesWhatItRemoves and the hold copy cases
//	it removes and reads as done
//	a forged, unMACed or MAC-failing       TestParseAbuseRecordsOpensAndVerifies, TestAbuseMailboxForgeryOpensButDoesNotVerify,
//	mailed record refused at restore       TestAbuseMailboxSealRefusesTampering
//	FS-39's case leg                       model's abuse_smtp_sink_test.go on the sink; the intake's T3 is cli/mxedge's
//	                                       TestReportIntakeCountsAnAggregateOnceOnPostgres and mxedgehost's T3
//	S7's SNI-less abuse@<root>             cli/mxedge's TestS7AbuseMailWithNoSniIsDroppedUnreadAndKeptNowhere, with the listener
//	                                       on its fakes in sidecar's mail_platform_scenario_test.go
//	CC3 and CC4 over abuse flows           abuse_privacy_test.go; the form's own canary case is
//	                                       TestAcceptAbuseReportKeepsNoThirdPartyData
//	the schema guard                       model's abuse_schema_test.go
//	FS-47's §21 case, FS-42's M5 step      QUOTA-M5-01's and OPS-M5-08's runs, reported there
//
// Not run here: the canaries through the whole block in one process with its glog lines (mxedgehost's child run
// scans those with the intake on fakes), a Web Risk hit from the worker's own scan (taskworker/work's
// abuse_webrisk_work_test.go; here the hit arrives as the scanner reports it), and anything on real hardware.

// A network with an Owner whose id the appeal needs, and a label in the test root.
func newTestAbuseE2eNetwork(t testing.TB, ctx context.Context) (networkId server.Id, ownerId server.Id, zone string) {
	t.Helper()
	networkId = server.NewId()
	ownerId = server.NewId()
	model.Testing_CreateNetwork(ctx, networkId, "e2e-"+networkId.String()[:8], ownerId)
	label, err := model.EnsureNetworkLabel(ctx, networkId, testAbuseIntakeRoot)
	if err != nil {
		t.Fatal(err)
	}
	return networkId, ownerId, label + "." + testAbuseIntakeRoot
}

// The network's §21.7 row: restricted or suspended while one stands, "" when none does. A new network's derived
// standing is probation whatever the row holds, so the row is what an action moves.
func testAbuseE2eStateRow(t testing.TB, ctx context.Context, networkId server.Id) (state string) {
	t.Helper()
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(ctx, `SELECT state FROM network_abuse_state WHERE network_id = $1`, networkId)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				server.Raise(result.Scan(&state))
			}
		})
	})
	return
}

// The access-log rows a reviewer's reads wrote.
func testAbuseE2eAccessLogCount(t testing.TB, ctx context.Context, reviewerId server.Id) (count int) {
	t.Helper()
	server.Db(ctx, func(conn server.PgConn) {
		server.Raise(conn.QueryRow(ctx, `SELECT COUNT(*) FROM abuse_access_log WHERE reviewer_id = $1`, reviewerId).Scan(&count))
	})
	return
}

// The composed path in the file header.
func TestAbuseComposedPathFromTheFormToASecondReviewersGrant(t *testing.T) {
	requireAbuseTables(t)
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		effects, mailer := newTestAbuseDeps(t)
		newTestAbuseIntake(t)
		defer server.Testing_PushAbuseConfig("protected_names: {roots: [" + testAbuseIntakeRoot + "], platform: [platform.example]}\n")()
		networkId, ownerId, zone := newTestAbuseE2eNetwork(t, ctx)
		host := "shop." + zone
		first := &AbuseStaff{ReviewerId: server.NewId(), Roles: []AbuseStaffRole{AbuseStaffRoleReviewer}}
		second := &AbuseStaff{ReviewerId: server.NewId(), Roles: []AbuseStaffRole{AbuseStaffRoleReviewer}}
		staffRun := func(staff *AbuseStaff, request map[string]any) (*AbuseStaffResult, error) {
			body, err := json.Marshal(request)
			if err != nil {
				t.Fatal(err)
			}
			return AbuseStaffAction(ctx, staff, body)
		}

		// a phishing report through the form: joined by its host, a case for review on §21.8's threat row, and no
		// machine acts on it
		report := testAcceptAbuseReport(t, ctx, &model.AbuseReport{
			Category:      model.AbuseCategoryPhishing,
			ReporterClass: model.AbuseReporterClassPublic,
			Url:           "https://" + host + "/login",
		})
		if !report.Joined || !report.Opened || len(report.Actions) != 0 || report.Signal != model.AbuseSignalThreatReport {
			t.Fatalf("the form's report: %+v, kinds %s", report, testAbuseIntakeKinds(report))
		}
		reportCase := model.GetAbuseCase(ctx, report.CaseId)
		if reportCase == nil || reportCase.NetworkId == nil || *reportCase.NetworkId != networkId || reportCase.JoinKey != host || reportCase.ReviewDueTime == nil {
			t.Fatalf("the report's case: %+v", reportCase)
		}
		if len(effects.applied()) != 0 || len(mailer.mailed()) != 0 {
			t.Fatal("a stranger's report acted")
		}

		// a Web Risk hit on the same host joins the same case and kills the host at once
		hit := testAcceptAbuseReport(t, ctx, &model.AbuseReport{
			Category:      model.AbuseCategoryPhishing,
			ReporterClass: model.AbuseReporterClassWebRisk,
			Url:           "https://" + host + "/login",
		})
		if !hit.Joined || hit.CaseId != report.CaseId || testAbuseIntakeKinds(hit) != "host_kill" {
			t.Fatalf("the Web Risk hit: %+v, kinds %s", hit, testAbuseIntakeKinds(hit))
		}
		kill := hit.Actions[0]
		if !kill.Applied || !kill.Mailed || kill.Action.Target != host || !kill.Action.ByMachine || kill.Action.ReviewState != model.AbuseReviewStatePending {
			t.Fatalf("the kill: %+v, action %+v", kill, kill.Action)
		}
		if applied := effects.applied(); len(applied) != 1 || applied[0].Action.ActionId != kill.Action.ActionId || applied[0].Lifted != nil {
			t.Fatalf("effects after the kill: %+v", applied)
		}
		if userAuths, _ := testAbuseNotices(t, ctx, networkId); len(userAuths) != 1 {
			t.Fatalf("%d notices after the kill", len(userAuths))
		}
		if state := testAbuseE2eStateRow(t, ctx, networkId); state != string(model.AbuseStandingRestricted) {
			t.Fatalf("the network's row after the kill: %q", state)
		}

		// the abuse role reads the case, which logs the read, and upholds the kill
		if _, err := staffRun(first, map[string]any{"kind": "lookup_case", "case_id": report.CaseId}); err != nil {
			t.Fatal(err)
		}
		if count := testAbuseE2eAccessLogCount(t, ctx, first.ReviewerId); count != 1 {
			t.Fatalf("%d access-log rows after one read", count)
		}
		if _, err := staffRun(first, map[string]any{
			"kind": "review", "case_id": report.CaseId, "action_id": kill.Action.ActionId, "decision": "uphold", "reason": "a login page imitating a bank",
		}); err != nil {
			t.Fatal(err)
		}
		if upheld := model.GetAbuseAction(ctx, kill.Action.ActionId); upheld.ReviewState != model.AbuseReviewStateUpheld || *upheld.ReviewerId != first.ReviewerId {
			t.Fatalf("the reviewed kill: %+v", upheld)
		}

		// the network's Owner appeals, and the reviewer who upheld the kill may not decide it
		appeal, created, err := model.CreateAbuseAppeal(ctx, networkId, ownerId, &model.AbuseAppealArgs{
			CaseId: report.CaseId,
			Reason: "the page is our own sign-in, and was never a bank's",
		}, server.NowUtc(), model.DefaultAbuseCustomerSettings())
		if err != nil || !created {
			t.Fatalf("the appeal: %t, %v", created, err)
		}
		decide := map[string]any{"kind": "decide_appeal", "appeal_id": appeal.AppealId, "decision": "granted", "reason": "the network's own sign-in page"}
		if _, err := staffRun(first, decide); !errors.Is(err, model.ErrAbuseAppealSameReviewer) {
			t.Fatalf("the upholding reviewer decided the appeal: %v", err)
		}

		// a second reviewer grants it: the kill is lifted through the case as wrong, applied, noticed and mailed
		decided, err := staffRun(second, decide)
		if err != nil {
			t.Fatal(err)
		}
		decision := decided.Result.(*AbuseStaffDecideAppealResult)
		if decision.Appeal.State != string(model.AbuseAppealStateGranted) || len(decision.Lifts) != 1 || decision.Lifts[0].Action.Target != kill.Action.ActionId.String() {
			t.Fatalf("the grant: %+v", decision)
		}
		lifted := model.GetAbuseAction(ctx, kill.Action.ActionId)
		if lifted.ReviewState != model.AbuseReviewStateLiftedWrong || *lifted.ReviewerId != second.ReviewerId {
			t.Fatalf("the lifted kill: %+v", lifted)
		}
		applied := effects.applied()
		if len(applied) != 2 || applied[1].Lifted == nil || applied[1].Lifted.ActionId != kill.Action.ActionId {
			t.Fatalf("effects after the grant: %d", len(applied))
		}
		if records := mailer.mailed(); len(records) != 2 {
			t.Fatalf("%d records mailed, want the kill's and the lift's", len(records))
		}
		if userAuths, _ := testAbuseNotices(t, ctx, networkId); len(userAuths) != 2 {
			t.Fatalf("%d notices after the grant", len(userAuths))
		}
		if state := testAbuseE2eStateRow(t, ctx, networkId); state != "" {
			t.Fatalf("a kill lifted as wrong still holds the network's row %q", state)
		}
		if abuseCase := model.GetAbuseCase(ctx, report.CaseId); abuseCase.State != model.AbuseCaseStateClosed {
			t.Fatalf("the case after the grant: %s", abuseCase.State)
		}
		// and every action and lift was mailed off site, owing nothing
		var owed int
		server.Db(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(ctx, `SELECT COUNT(*) FROM abuse_action WHERE applied_time IS NULL OR mailed_time IS NULL`).Scan(&owed))
		})
		if owed != 0 {
			t.Fatalf("%d actions owe an effect or a record", owed)
		}
	})
}
