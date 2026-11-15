//go:build sidecar_pending

package model

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/server"
)

// §21.8's appeals and a network's own view of its cases and actions
// (abuse_appeal_model.go; ABUSE-M5-11) on MG5's tables.

// A network of its own.
func testAbuseCustomerNetwork(t testing.TB, ctx context.Context) server.Id {
	t.Helper()
	networkId := server.NewId()
	Testing_CreateNetwork(ctx, networkId, "abuse"+strings.ReplaceAll(networkId.String()[24:], "-", ""), server.NewId())
	return networkId
}

// A case of networkId on host with a person's host kill at actionTime, which a person's action starts upheld,
// so the network was told of it.
func testNoticedAbuseCase(t testing.TB, ctx context.Context, networkId server.Id, host string, actionTime time.Time) (caseId server.Id, actionId server.Id) {
	t.Helper()
	caseId, _ = testOpenAbuseCase(t, ctx, testAbuseCaseArgs(networkId, host), actionTime)
	reviewerId := server.NewId()
	actionId = server.NewId()
	if _, _, err := testRecordAbuseAction(t, ctx, &AbuseActionArgs{
		ActionId:   actionId,
		CaseId:     caseId,
		Kind:       AbuseActionKindHostKill,
		Target:     host,
		ReviewerId: &reviewerId,
	}, actionTime); err != nil {
		t.Fatal(err)
	}
	return
}

// Runs a statement of the test's own.
func testAbuseExec(t testing.TB, ctx context.Context, sql string, args ...any) {
	t.Helper()
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(ctx, sql, args...))
	})
}

// The case's state, close time and review clock.
func testAbuseCaseState(t testing.TB, ctx context.Context, caseId server.Id) (state string, closeTime *time.Time, reviewDueTime *time.Time) {
	t.Helper()
	server.Db(ctx, func(conn server.PgConn) {
		server.Raise(conn.QueryRow(
			ctx,
			`SELECT state, close_time, review_due_time FROM abuse_case WHERE case_id = $1`,
			caseId,
		).Scan(&state, &closeTime, &reviewDueTime))
	})
	return
}

// How many appeals the case has.
func testAbuseAppealCount(t testing.TB, ctx context.Context, caseId server.Id) (count int) {
	t.Helper()
	server.Db(ctx, func(conn server.PgConn) {
		server.Raise(conn.QueryRow(ctx, `SELECT COUNT(*) FROM abuse_appeal WHERE case_id = $1`, caseId).Scan(&count))
	})
	return
}

// A closed case's appeal is filed once, reopens the case and clears its spent clock; filing again while it waits
// answers the same appeal.
func TestCreateAbuseAppealFilesOnce(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		settings := DefaultAbuseCustomerSettings()
		now := server.NowUtc().Truncate(time.Millisecond)
		networkId := testAbuseCustomerNetwork(t, ctx)
		userId := server.NewId()
		caseId, _ := testNoticedAbuseCase(t, ctx, networkId, "shop.appeal.example", now.Add(-time.Hour))
		testAbuseExec(t, ctx, `UPDATE abuse_case SET state = 'closed', close_time = $2, review_due_time = $2 WHERE case_id = $1`, caseId, now.Add(-time.Minute))

		appeal, created, err := CreateAbuseAppeal(ctx, networkId, userId, &AbuseAppealArgs{CaseId: caseId, Reason: "the host was fixed"}, now, settings)
		if err != nil || !created {
			t.Fatalf("the first appeal: %t, %v", created, err)
		}
		if appeal.State != AbuseAppealStatePending || appeal.UserId != userId || appeal.NetworkId != networkId || !appeal.CreateTime.Equal(now) {
			t.Fatalf("the appeal: %+v", appeal)
		}
		state, closeTime, reviewDueTime := testAbuseCaseState(t, ctx, caseId)
		if state != string(AbuseCaseStateOpen) || closeTime != nil || reviewDueTime != nil {
			t.Fatalf("the case after an appeal: %s, closed %v, due %v", state, closeTime, reviewDueTime)
		}

		again, created, err := CreateAbuseAppeal(ctx, networkId, userId, &AbuseAppealArgs{CaseId: caseId, Reason: "a second filing"}, now.Add(time.Minute), settings)
		if err != nil || created || again.AppealId != appeal.AppealId || again.Reason != "the host was fixed" {
			t.Fatalf("the second filing: %+v, %t, %v", again, created, err)
		}
		if count := testAbuseAppealCount(t, ctx, caseId); count != 1 {
			t.Fatalf("%d appeals", count)
		}
		if result := appeal.Result(); result.AppealId != appeal.AppealId || result.CaseId != caseId || result.CreateTimeMs != now.UnixMilli() {
			t.Fatalf("the answer: %+v", result)
		}
	})
}

// An unknown case, another network's, a barred one and one with no action answer the same error, and none
// writes a row.
func TestCreateAbuseAppealAnswersUnknownOtherBarredAndUnnoticedAlike(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		settings := DefaultAbuseCustomerSettings()
		now := server.NowUtc()
		networkId := testAbuseCustomerNetwork(t, ctx)
		otherNetworkId := testAbuseCustomerNetwork(t, ctx)
		otherCaseId, _ := testNoticedAbuseCase(t, ctx, otherNetworkId, "shop.other.example", now.Add(-time.Hour))
		barredCaseId, _ := testNoticedAbuseCase(t, ctx, networkId, "shop.barred.example", now.Add(-time.Hour))
		testAbuseExec(t, ctx, `UPDATE abuse_case SET barred = true WHERE case_id = $1`, barredCaseId)
		unnoticedCaseId, _ := testOpenAbuseCase(t, ctx, testAbuseCaseArgs(networkId, "shop.unnoticed.example"), now.Add(-time.Hour))

		for name, caseId := range map[string]server.Id{
			"unknown":   server.NewId(),
			"other":     otherCaseId,
			"barred":    barredCaseId,
			"unnoticed": unnoticedCaseId,
		} {
			appeal, created, err := CreateAbuseAppeal(ctx, networkId, server.NewId(), &AbuseAppealArgs{CaseId: caseId, Reason: "why"}, now, settings)
			if !errors.Is(err, ErrAbuseCaseNotFound) || appeal != nil || created {
				t.Errorf("%s: %+v, %t, %v", name, appeal, created, err)
			}
			if count := testAbuseAppealCount(t, ctx, caseId); count != 0 {
				t.Errorf("%s: %d appeals written", name, count)
			}
		}
	})
}

// An appeal may be filed until abuse.yml's window has passed since the case's newest notice, and not at its end.
func TestCreateAbuseAppealWindow(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		settings := DefaultAbuseCustomerSettings()
		settings.AppealWindowMonths = 7
		actionTime := server.NowUtc().Truncate(time.Millisecond).AddDate(-1, 0, 0)
		windowEnd := actionTime.AddDate(0, 7, 0)
		networkId := testAbuseCustomerNetwork(t, ctx)
		lateCaseId, _ := testNoticedAbuseCase(t, ctx, networkId, "shop.late.example", actionTime)
		inTimeCaseId, _ := testNoticedAbuseCase(t, ctx, networkId, "shop.intime.example", actionTime)

		if _, _, err := CreateAbuseAppeal(ctx, networkId, server.NewId(), &AbuseAppealArgs{CaseId: lateCaseId, Reason: "late"}, windowEnd, settings); !errors.Is(err, ErrAbuseAppealWindowClosed) {
			t.Fatalf("at the window's end: %v", err)
		}
		if _, created, err := CreateAbuseAppeal(ctx, networkId, server.NewId(), &AbuseAppealArgs{CaseId: inTimeCaseId, Reason: "in time"}, windowEnd.Add(-time.Millisecond), settings); err != nil || !created {
			t.Fatalf("inside the window: %t, %v", created, err)
		}

		// a newer action opens the window again, since its notice is the newest
		reviewerId := server.NewId()
		if _, _, err := testRecordAbuseAction(t, ctx, &AbuseActionArgs{
			ActionId:   server.NewId(),
			CaseId:     lateCaseId,
			Kind:       AbuseActionKindLabelKill,
			Target:     "appeal.example",
			ReviewerId: &reviewerId,
		}, windowEnd); err != nil {
			t.Fatal(err)
		}
		if _, created, err := CreateAbuseAppeal(ctx, networkId, server.NewId(), &AbuseAppealArgs{CaseId: lateCaseId, Reason: "after a new action"}, windowEnd.Add(time.Hour), settings); err != nil || !created {
			t.Fatalf("after a newer action: %t, %v", created, err)
		}
	})
}

// A decided appeal takes no second one until an action comes after its decision.
func TestCreateAbuseAppealAfterItsDecision(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		settings := DefaultAbuseCustomerSettings()
		now := server.NowUtc().Truncate(time.Millisecond)
		networkId := testAbuseCustomerNetwork(t, ctx)
		caseId, _ := testNoticedAbuseCase(t, ctx, networkId, "shop.decided.example", now.Add(-3*time.Hour))
		appeal, _, err := CreateAbuseAppeal(ctx, networkId, server.NewId(), &AbuseAppealArgs{CaseId: caseId, Reason: "first"}, now.Add(-2*time.Hour), settings)
		if err != nil {
			t.Fatal(err)
		}
		testAbuseExec(
			t, ctx,
			`UPDATE abuse_appeal SET state = 'denied', reviewer_id = $2, decision_reason = 'upheld', decide_time = $3 WHERE appeal_id = $1`,
			appeal.AppealId, server.NewId(), now.Add(-time.Hour),
		)
		if _, _, err := CreateAbuseAppeal(ctx, networkId, server.NewId(), &AbuseAppealArgs{CaseId: caseId, Reason: "again"}, now, settings); !errors.Is(err, ErrAbuseAppealDecided) {
			t.Fatalf("after the decision: %v", err)
		}

		reviewerId := server.NewId()
		if _, _, err := testRecordAbuseAction(t, ctx, &AbuseActionArgs{
			ActionId:   server.NewId(),
			CaseId:     caseId,
			Kind:       AbuseActionKindLabelKill,
			Target:     "decided.example",
			ReviewerId: &reviewerId,
		}, now.Add(-time.Minute)); err != nil {
			t.Fatal(err)
		}
		second, created, err := CreateAbuseAppeal(ctx, networkId, server.NewId(), &AbuseAppealArgs{CaseId: caseId, Reason: "the new action"}, now, settings)
		if err != nil || !created || second.AppealId == appeal.AppealId {
			t.Fatalf("after a newer action: %+v, %t, %v", second, created, err)
		}
	})
}

// A reason that is empty, blank, too long or not text is refused, and nothing is written.
func TestCreateAbuseAppealRefusesWhatNoRowHolds(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		settings := DefaultAbuseCustomerSettings()
		now := server.NowUtc()
		networkId := testAbuseCustomerNetwork(t, ctx)
		caseId, _ := testNoticedAbuseCase(t, ctx, networkId, "shop.reasons.example", now.Add(-time.Hour))
		for _, reason := range []string{
			"",
			"   \n",
			strings.Repeat("r", settings.AppealReasonMaxByteCount+1),
			"a\x00b",
			"\xff\xfe",
		} {
			if _, _, err := CreateAbuseAppeal(ctx, networkId, server.NewId(), &AbuseAppealArgs{CaseId: caseId, Reason: reason}, now, settings); !errors.Is(err, ErrAbuseAppealInvalid) {
				t.Errorf("a reason of %d bytes: %v", len(reason), err)
			}
		}
		if count := testAbuseAppealCount(t, ctx, caseId); count != 0 {
			t.Fatalf("%d appeals written", count)
		}
		if _, created, err := CreateAbuseAppeal(ctx, networkId, server.NewId(), &AbuseAppealArgs{CaseId: caseId, Reason: strings.Repeat("r", settings.AppealReasonMaxByteCount)}, now, settings); err != nil || !created {
			t.Fatalf("a reason at the bound: %t, %v", created, err)
		}
	})
}

// The network lists the cases it was told of, newest first, with each case's actions (a lifted one by its state,
// no lift row) and newest appeal, and none of another network's, a barred case or a case with no action.
func TestListNetworkAbuseCasesShowsWhatTheNetworkWasTold(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		settings := DefaultAbuseCustomerSettings()
		settings.AppealUrl = "https://appeal.example/abuse"
		now := server.NowUtc().Truncate(time.Millisecond)
		networkId := testAbuseCustomerNetwork(t, ctx)
		otherNetworkId := testAbuseCustomerNetwork(t, ctx)

		olderCaseId, olderActionId := testNoticedAbuseCase(t, ctx, networkId, "shop.older.example", now.Add(-3*time.Hour))
		if _, _, _, err := testRecordAbuseLift(t, ctx, &AbuseLiftArgs{
			ActionId:       server.NewId(),
			CaseId:         olderCaseId,
			LiftedActionId: olderActionId,
			ReviewState:    AbuseReviewStateLifted,
		}, now.Add(-2*time.Hour)); err != nil {
			t.Fatal(err)
		}
		newerCaseId, newerActionId := testNoticedAbuseCase(t, ctx, networkId, "shop.newer.example", now.Add(-time.Hour))
		appeal, _, err := CreateAbuseAppeal(ctx, networkId, server.NewId(), &AbuseAppealArgs{CaseId: newerCaseId, Reason: "fixed"}, now, settings)
		if err != nil {
			t.Fatal(err)
		}
		testOpenAbuseCase(t, ctx, testAbuseCaseArgs(networkId, "shop.unnoticed.example"), now)
		barredCaseId, _ := testNoticedAbuseCase(t, ctx, networkId, "shop.barred.example", now)
		testAbuseExec(t, ctx, `UPDATE abuse_case SET barred = true WHERE case_id = $1`, barredCaseId)
		testNoticedAbuseCase(t, ctx, otherNetworkId, "shop.elsewhere.example", now)

		cases := ListNetworkAbuseCases(ctx, networkId, settings).Cases
		if len(cases) != 2 || cases[0].CaseId != newerCaseId || cases[1].CaseId != olderCaseId {
			t.Fatalf("the cases: %d", len(cases))
		}
		newer := cases[0]
		if newer.Category != AbuseCategoryPhishing || newer.TargetKind != AbuseTargetKindHost || newer.Target != "shop.newer.example" || newer.State != AbuseCaseStateOpen || newer.CloseTimeMs != 0 {
			t.Fatalf("the newer case: %+v", newer)
		}
		if len(newer.Actions) != 1 || newer.Actions[0].ActionId != newerActionId || newer.Actions[0].ReviewState != AbuseReviewStateUpheld {
			t.Fatalf("the newer case's actions: %+v", newer.Actions)
		}
		if newer.Appeal == nil || newer.Appeal.AppealId != appeal.AppealId || newer.Appeal.State != AbuseAppealStatePending || newer.Appeal.DecideTimeMs != 0 {
			t.Fatalf("the newer case's appeal: %+v", newer.Appeal)
		}
		if newer.AppealUrl != "https://appeal.example/abuse?case_id="+newerCaseId.String() {
			t.Fatalf("the appeal link: %q", newer.AppealUrl)
		}
		older := cases[1]
		if older.State != AbuseCaseStateClosed || older.CloseTimeMs == 0 || older.Appeal != nil {
			t.Fatalf("the older case: %+v", older)
		}
		if len(older.Actions) != 1 || older.Actions[0].ActionId != olderActionId || older.Actions[0].ReviewState != AbuseReviewStateLifted {
			t.Fatalf("the older case's actions: %+v", older.Actions)
		}

		settings.CaseListLimit = 1
		if cases := ListNetworkAbuseCases(ctx, networkId, settings).Cases; len(cases) != 1 || cases[0].CaseId != newerCaseId {
			t.Fatalf("under a limit of one: %d", len(cases))
		}
	})
}

// Status lists the actions that stand, oldest first: never a lifted one, one past its own end, a lift, or one of a
// barred case, and a barred case's suspension shows as the standing alone.
func TestGetNetworkAbuseStatusListsWhatStands(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		settings := DefaultAbuseCustomerSettings()
		now := server.NowUtc().Truncate(time.Millisecond)
		networkId := testAbuseCustomerNetwork(t, ctx)

		standingCaseId, standingActionId := testNoticedAbuseCase(t, ctx, networkId, "shop.standing.example", now.Add(-4*time.Hour))
		liftedCaseId, liftedActionId := testNoticedAbuseCase(t, ctx, networkId, "shop.lifted.example", now.Add(-3*time.Hour))
		if _, _, _, err := testRecordAbuseLift(t, ctx, &AbuseLiftArgs{
			ActionId:       server.NewId(),
			CaseId:         liftedCaseId,
			LiftedActionId: liftedActionId,
			ReviewState:    AbuseReviewStateLiftedWrong,
		}, now.Add(-2*time.Hour)); err != nil {
			t.Fatal(err)
		}
		_, endedActionId := testNoticedAbuseCase(t, ctx, networkId, "shop.ended.example", now.Add(-3*time.Hour))
		testAbuseExec(t, ctx, `UPDATE abuse_action SET until_time = $2 WHERE action_id = $1`, endedActionId, now.Add(-time.Minute))
		barredCaseId, _ := testNoticedAbuseCase(t, ctx, networkId, "shop.barred.example", now.Add(-time.Hour))
		testAbuseExec(t, ctx, `UPDATE abuse_case SET barred = true WHERE case_id = $1`, barredCaseId)
		laterCaseId, laterActionId := testNoticedAbuseCase(t, ctx, networkId, "shop.later.example", now.Add(-30*time.Minute))

		status, found := GetNetworkAbuseStatus(ctx, networkId, now, settings)
		if !found {
			t.Fatal("no status")
		}
		// Testing_CreateNetwork's owner signs in with a verified email
		if status.Standing != AbuseStandingProbation || !status.Verified {
			t.Fatalf("a new network's status: %s, verified %t", status.Standing, status.Verified)
		}
		if len(status.Actions) != 2 {
			t.Fatalf("%d actions", len(status.Actions))
		}
		first, second := status.Actions[0], status.Actions[1]
		if server.Id(first.ActionId) != standingActionId || server.Id(first.CaseId) != standingCaseId || server.Id(second.ActionId) != laterActionId || server.Id(second.CaseId) != laterCaseId {
			t.Fatalf("the actions are not the two standing ones, oldest first")
		}
		if first.Kind != string(AbuseActionKindHostKill) || first.Category != string(AbuseCategoryPhishing) || first.Target != "shop.standing.example" || first.ReviewState != string(AbuseReviewStateUpheld) || first.UntilTimeMs != 0 {
			t.Fatalf("the first action: %+v", first)
		}

		settings.StatusActionLimit = 1
		status, _ = GetNetworkAbuseStatus(ctx, networkId, now, settings)
		if len(status.Actions) != 1 || server.Id(status.Actions[0].ActionId) != laterActionId {
			t.Fatal("a limit of one does not keep the newest")
		}

		if _, found := GetNetworkAbuseStatus(ctx, server.NewId(), now, settings); found {
			t.Fatal("a network that does not exist has a status")
		}
	})
}

// A network with a sealed hold reads byte for byte as its no-hold twin does, ids aside, and as it did before the
// hold: no field of cases or status says a hold exists (D-ABUSE-8), and the hold's barred case shows nothing.
func TestAbuseCustomerReadsAreTheSameForAHeldNetwork(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		settings := DefaultAbuseCustomerSettings()
		settings.AppealUrl = "https://appeal.example/abuse"
		now := server.NowUtc().Truncate(time.Millisecond)
		actionTime := now.Add(-time.Hour)

		read := func(networkId server.Id) string {
			cases, err := json.Marshal(ListNetworkAbuseCases(ctx, networkId, settings))
			if err != nil {
				t.Fatal(err)
			}
			status, found := GetNetworkAbuseStatus(ctx, networkId, now, settings)
			if !found {
				t.Fatal("no status")
			}
			statusJson, err := json.Marshal(status)
			if err != nil {
				t.Fatal(err)
			}
			return string(cases) + "\n" + string(statusJson)
		}

		heldNetworkId := testAbuseCustomerNetwork(t, ctx)
		twinNetworkId := testAbuseCustomerNetwork(t, ctx)
		heldCaseId, heldActionId := testNoticedAbuseCase(t, ctx, heldNetworkId, "shop.twin.example", actionTime)
		twinCaseId, twinActionId := testNoticedAbuseCase(t, ctx, twinNetworkId, "shop.twin.example", actionTime)
		before := read(heldNetworkId)

		// a sealed order's case, its hold and its legal request
		holdCaseId, _ := testOpenAbuseCase(t, ctx, &AbuseCaseArgs{
			NetworkId:     &heldNetworkId,
			Category:      AbuseCategoryOther,
			Target:        AbuseTarget{Kind: AbuseTargetKindNetwork, Name: heldNetworkId.String()},
			ReporterClass: AbuseReporterClassLawEnforcement,
			JoinKind:      AbuseJoinKindNetwork,
			JoinKey:       heldNetworkId.String(),
			Barred:        true,
		}, now.Add(-time.Minute))
		testAbuseExec(
			t, ctx,
			`
				INSERT INTO abuse_legal_hold (network_id, case_id, part, kind, scope, place_time, expire_time, copy_state)
				VALUES ($1, $2, 0, 'preserve_2703f', '"network"'::jsonb, $3, $4, 'copied')
			`,
			heldNetworkId, holdCaseId, now.Add(-time.Minute), now.AddDate(0, 0, 90),
		)
		testAbuseExec(
			t, ctx,
			`
				INSERT INTO legal_request (request_id, case_id, instrument, country, network_ids, sealed, seal_until, production_state, create_time)
				VALUES ($1, $2, 'preservation', 'US', ARRAY[$3::uuid], true, $4, 'none', $5)
			`,
			server.NewId(), holdCaseId, heldNetworkId, now.AddDate(1, 0, 0), now.Add(-time.Minute),
		)

		after := read(heldNetworkId)
		if after != before {
			t.Fatalf("a hold changed the network's reads:\n%s\n%s", before, after)
		}
		twin := read(twinNetworkId)
		mapped := strings.NewReplacer(heldCaseId.String(), twinCaseId.String(), heldActionId.String(), twinActionId.String()).Replace(after)
		if mapped != twin {
			t.Fatalf("the held network does not read as its twin:\n%s\n%s", mapped, twin)
		}
	})
}
