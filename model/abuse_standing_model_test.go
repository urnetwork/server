//go:build sidecar_pending

package model

// §21.7's standing on MG5's network_abuse_state (ABUSE-M5-07), table by state, through .stack/server-test.sh: a 6-day
// network on probation and an 8-day one standard; a kill making a network restricted for 30 days, a lift as wrong
// clearing it, the 30 days on a moved clock ending it and the pass removing the row; QD-194's order, probation before
// restricted and suspended before everything; the no-strike actions and a burner label's halt restricting nothing;
// and NewAbuseGate over Postgres reading what the rows say. The actions are recorded and re-derived in one
// transaction, as controller.ApplyAbuseAction and LiftAbuseAction do.

import (
	"context"
	"testing"
	"time"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/server"
)

// A network created age before now, with an admin user and no sign-in, its id answered.
func testAbuseStandingNetwork(t testing.TB, ctx context.Context, now time.Time, age time.Duration) server.Id {
	t.Helper()
	networkId := server.NewId()
	adminUserId := server.NewId()
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(
			ctx,
			`INSERT INTO network_user (user_id, user_name, auth_type) VALUES ($1, $2, $3)`,
			adminUserId,
			"standing",
			AuthTypeGuest,
		))
		server.RaisePgResult(tx.Exec(
			ctx,
			`
				INSERT INTO network (network_id, network_name, admin_user_id, create_time)
				VALUES ($1, $2, $3, $4)
			`,
			networkId,
			"standing-"+networkId.String(),
			adminUserId,
			now.Add(-age).UTC(),
		))
	})
	return networkId
}

// Opens a case of category on the network's host in its own transaction.
func testAbuseStandingCase(t testing.TB, ctx context.Context, networkId server.Id, category AbuseCategory, barred bool, now time.Time) server.Id {
	t.Helper()
	host := "shop-" + server.NewId().String() + ".root.example"
	var caseId server.Id
	var err error
	server.Tx(ctx, func(tx server.PgTx) {
		caseId, _, err = OpenAbuseCaseInTx(ctx, tx, &AbuseCaseArgs{
			NetworkId:     &networkId,
			Category:      category,
			Target:        AbuseTarget{Kind: AbuseTargetKindHost, Name: host},
			ReporterClass: AbuseReporterClassStaff,
			JoinKind:      AbuseJoinKindHost,
			JoinKey:       host,
			Barred:        barred,
		}, now, DefaultAbuseSettings())
	})
	if err != nil {
		t.Fatal(err)
	}
	return caseId
}

// Records a machine's action of kind under the case at now and re-derives the network's row in the same transaction,
// as ApplyAbuseAction does, answering the action's id.
func testAbuseStandingAction(t testing.TB, ctx context.Context, caseId server.Id, kind AbuseActionKind, now time.Time) server.Id {
	t.Helper()
	actionId := server.NewId()
	settings := DefaultAbuseSettings()
	var err error
	server.Tx(ctx, func(tx server.PgTx) {
		var action *AbuseAction
		action, _, _, err = RecordAbuseActionInTx(ctx, tx, &AbuseActionArgs{
			ActionId: actionId,
			CaseId:   caseId,
			Kind:     kind,
			Target:   "shop.root.example",
		}, now, settings)
		if err == nil && action.NetworkId != nil {
			ReevaluateNetworkAbuseStateInTx(ctx, tx, *action.NetworkId, now, settings)
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	return actionId
}

// Lifts the action at now, as wrong or not, and re-derives in the same transaction, as LiftAbuseAction does.
func testAbuseStandingLift(t testing.TB, ctx context.Context, caseId server.Id, actionId server.Id, reviewState AbuseReviewState, now time.Time) {
	t.Helper()
	reviewerId := server.NewId()
	settings := DefaultAbuseSettings()
	var err error
	server.Tx(ctx, func(tx server.PgTx) {
		var lift *AbuseAction
		lift, _, _, _, err = RecordAbuseLiftInTx(ctx, tx, &AbuseLiftArgs{
			ActionId:       server.NewId(),
			CaseId:         caseId,
			LiftedActionId: actionId,
			ReviewState:    reviewState,
			ReviewerId:     &reviewerId,
		}, now)
		if err == nil && lift.NetworkId != nil {
			ReevaluateNetworkAbuseStateInTx(ctx, tx, *lift.NetworkId, now, settings)
		}
	})
	if err != nil {
		t.Fatal(err)
	}
}

// The network's standing at now, which must exist.
func testAbuseStandingAt(t testing.TB, ctx context.Context, networkId server.Id, now time.Time) *NetworkAbuseStanding {
	t.Helper()
	standing, found := GetNetworkAbuseStanding(ctx, networkId, now)
	if !found {
		t.Fatalf("network %s not found", networkId)
	}
	return standing
}

// network_abuse_state's row for the network, false with none.
func testAbuseStandingRow(t testing.TB, ctx context.Context, networkId server.Id) (row networkAbuseRow, found bool) {
	t.Helper()
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`SELECT state, case_id, until_time FROM network_abuse_state WHERE network_id = $1`,
			networkId,
		)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				server.Raise(result.Scan(&row.state, &row.caseId, &row.untilTime))
				found = true
			}
		})
	})
	return
}

func testAbuseStandingRun(t *testing.T, run func(t testing.TB, ctx context.Context, now time.Time)) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		if !server.SidecarMg5Linked() {
			t.Fatal("a pending build does not link MG5")
		}
		run(t, context.Background(), server.NowUtc())
	})
}

// Under 7 days old is probation, ending at the seventh day; 7 or more with no row is standard; a network that does not
// exist is not found.
func TestNetworkAbuseStandingByAge(t *testing.T) {
	testAbuseStandingRun(t, func(t testing.TB, ctx context.Context, now time.Time) {
		young := testAbuseStandingNetwork(t, ctx, now, 6*24*time.Hour)
		old := testAbuseStandingNetwork(t, ctx, now, 8*24*time.Hour)
		standing := testAbuseStandingAt(t, ctx, young, now)
		if standing.Standing != AbuseStandingProbation || standing.ChangeTime == nil || standing.CaseId != nil {
			t.Fatalf("6 days old: %+v", standing)
		}
		if !standing.ChangeTime.Equal(now.Add(24 * time.Hour)) {
			t.Fatalf("probation ends at %s", standing.ChangeTime)
		}
		if standing := testAbuseStandingAt(t, ctx, young, now.Add(24*time.Hour)); standing.Standing != AbuseStandingStandard {
			t.Fatalf("7 days old: %+v", standing)
		}
		if standing := testAbuseStandingAt(t, ctx, old, now); standing.Standing != AbuseStandingStandard || standing.ChangeTime != nil {
			t.Fatalf("8 days old: %+v", standing)
		}
		if _, found := GetNetworkAbuseStanding(ctx, server.NewId(), now); found {
			t.Fatal("a network that does not exist was found")
		}
	})
}

// A kill restricts its network for 30 days from the kill, under its case; past them the network reads standard with
// no writer, and the pass removes the row.
func TestNetworkAbuseStandingKillRestrictsFor30Days(t *testing.T) {
	testAbuseStandingRun(t, func(t testing.TB, ctx context.Context, now time.Time) {
		settings := DefaultAbuseSettings()
		networkId := testAbuseStandingNetwork(t, ctx, now, 60*24*time.Hour)
		caseId := testAbuseStandingCase(t, ctx, networkId, AbuseCategoryPhishing, false, now)
		testAbuseStandingAction(t, ctx, caseId, AbuseActionKindHostKill, now)

		row, found := testAbuseStandingRow(t, ctx, networkId)
		if !found || row.state != networkAbuseStateRestricted || row.caseId != caseId || row.untilTime == nil ||
			!row.untilTime.Equal(now.Add(settings.RestrictedWindow)) {
			t.Fatalf("the row: %+v %t", row, found)
		}
		standing := testAbuseStandingAt(t, ctx, networkId, now)
		if standing.Standing != AbuseStandingRestricted || standing.CaseId == nil || *standing.CaseId != caseId || standing.Barred {
			t.Fatalf("after the kill: %+v", standing)
		}
		if standing := testAbuseStandingAt(t, ctx, networkId, now.Add(settings.RestrictedWindow-time.Second)); standing.Standing != AbuseStandingRestricted {
			t.Fatalf("a second before the 30 days end: %+v", standing)
		}
		later := now.Add(settings.RestrictedWindow + time.Second)
		if standing := testAbuseStandingAt(t, ctx, networkId, later); standing.Standing != AbuseStandingStandard {
			t.Fatalf("past the 30 days: %+v", standing)
		}

		// the pass before the end changes nothing, and after it removes the row
		if result := RunAbuseStandingPass(ctx, now, settings); result.RemovedCount != 0 || result.ExtendedCount != 0 {
			t.Fatalf("an early pass: %+v", result)
		}
		result := RunAbuseStandingPass(ctx, later, settings)
		if result.RemovedCount < 1 {
			t.Fatalf("the pass: %+v", result)
		}
		if _, found := testAbuseStandingRow(t, ctx, networkId); found {
			t.Fatal("the row outlived its window")
		}
	})
}

// A lift as wrong takes the kill out of the window, and the network back to standard; a lift that is not, being still
// a strike, leaves it restricted.
func TestNetworkAbuseStandingLiftAsWrongClears(t *testing.T) {
	testAbuseStandingRun(t, func(t testing.TB, ctx context.Context, now time.Time) {
		wrong := testAbuseStandingNetwork(t, ctx, now, 60*24*time.Hour)
		wrongCaseId := testAbuseStandingCase(t, ctx, wrong, AbuseCategoryMalware, false, now)
		wrongKillId := testAbuseStandingAction(t, ctx, wrongCaseId, AbuseActionKindHostKill, now)
		testAbuseStandingLift(t, ctx, wrongCaseId, wrongKillId, AbuseReviewStateLiftedWrong, now.Add(time.Hour))
		if standing := testAbuseStandingAt(t, ctx, wrong, now.Add(time.Hour)); standing.Standing != AbuseStandingStandard {
			t.Fatalf("lifted as wrong: %+v", standing)
		}
		if _, found := testAbuseStandingRow(t, ctx, wrong); found {
			t.Fatal("a lift as wrong left the row")
		}

		fixed := testAbuseStandingNetwork(t, ctx, now, 60*24*time.Hour)
		fixedCaseId := testAbuseStandingCase(t, ctx, fixed, AbuseCategoryMalware, false, now)
		fixedKillId := testAbuseStandingAction(t, ctx, fixedCaseId, AbuseActionKindHostKill, now)
		testAbuseStandingLift(t, ctx, fixedCaseId, fixedKillId, AbuseReviewStateLifted, now.Add(time.Hour))
		if standing := testAbuseStandingAt(t, ctx, fixed, now.Add(time.Hour)); standing.Standing != AbuseStandingRestricted {
			t.Fatalf("lifted, still a strike: %+v", standing)
		}
	})
}

// A network halt and a label kill restrict; a protective kill, a compromise's halt, a burner label's halt and a
// revocation do not (plan-ABUSE-small-readings #4).
func TestNetworkAbuseStandingWhatRestricts(t *testing.T) {
	testAbuseStandingRun(t, func(t testing.TB, ctx context.Context, now time.Time) {
		for _, c := range []struct {
			category  AbuseCategory
			kind      AbuseActionKind
			restricts bool
		}{
			{category: AbuseCategorySpam, kind: AbuseActionKindMailHalt, restricts: true},
			{category: AbuseCategoryPhishing, kind: AbuseActionKindLabelKill, restricts: true},
			{category: AbuseCategoryPhishing, kind: AbuseActionKindZoneSuspend, restricts: true},
			{category: AbuseCategoryCopyright, kind: AbuseActionKindHostKill, restricts: true},
			{category: AbuseCategoryExposedPanel, kind: AbuseActionKindHostKill, restricts: false},
			{category: AbuseCategoryCompromise, kind: AbuseActionKindMailHalt, restricts: false},
			{category: AbuseCategorySpam, kind: AbuseActionKindMailLabelHalt, restricts: false},
			{category: AbuseCategoryPhishing, kind: AbuseActionKindCertRevoke, restricts: false},
			{category: AbuseCategoryCopyright, kind: AbuseActionKindNoticeForward, restricts: false},
		} {
			networkId := testAbuseStandingNetwork(t, ctx, now, 60*24*time.Hour)
			caseId := testAbuseStandingCase(t, ctx, networkId, c.category, false, now)
			testAbuseStandingAction(t, ctx, caseId, c.kind, now)
			standing := testAbuseStandingAt(t, ctx, networkId, now)
			if restricts := standing.Standing == AbuseStandingRestricted; restricts != c.restricts {
				t.Errorf("%s %s: %+v", c.category, c.kind, standing)
			}
		}
	})
}

// QD-194: probation reads before restricted, and suspended before everything. A suspended row is the effects': a
// re-derivation leaves it, and once the suspension ends the network is restricted as its whole history says.
func TestNetworkAbuseStandingOrder(t *testing.T) {
	testAbuseStandingRun(t, func(t testing.TB, ctx context.Context, now time.Time) {
		settings := DefaultAbuseSettings()
		young := testAbuseStandingNetwork(t, ctx, now, 3*24*time.Hour)
		youngCaseId := testAbuseStandingCase(t, ctx, young, AbuseCategoryPhishing, false, now)
		testAbuseStandingAction(t, ctx, youngCaseId, AbuseActionKindHostKill, now)
		if standing := testAbuseStandingAt(t, ctx, young, now); standing.Standing != AbuseStandingProbation {
			t.Fatalf("a kill on probation: %+v", standing)
		}
		if standing := testAbuseStandingAt(t, ctx, young, now.Add(5*24*time.Hour)); standing.Standing != AbuseStandingRestricted {
			t.Fatalf("off probation, inside the window: %+v", standing)
		}
		if err := SetNetworkAbuseSuspended(ctx, young, youngCaseId, now); err != nil {
			t.Fatal(err)
		}
		if standing := testAbuseStandingAt(t, ctx, young, now); standing.Standing != AbuseStandingSuspended || standing.CaseId == nil {
			t.Fatalf("suspended on probation: %+v", standing)
		}
		if _, changed := ReevaluateNetworkAbuseState(ctx, young, now); changed {
			t.Fatal("a re-derivation changed a suspended row")
		}
		if standing := testAbuseStandingAt(t, ctx, young, now); standing.Standing != AbuseStandingSuspended {
			t.Fatalf("after a re-derivation: %+v", standing)
		}

		// an older network: a kill, then a suspension under another case lifted as wrong, as the effects end it
		old := testAbuseStandingNetwork(t, ctx, now, 400*24*time.Hour)
		killCaseId := testAbuseStandingCase(t, ctx, old, AbuseCategoryPhishing, false, now.Add(-10*24*time.Hour))
		testAbuseStandingAction(t, ctx, killCaseId, AbuseActionKindHostKill, now.Add(-10*24*time.Hour))
		suspendCaseId := testAbuseStandingCase(t, ctx, old, AbuseCategoryThreatToLife, false, now)
		suspendId := testAbuseStandingAction(t, ctx, suspendCaseId, AbuseActionKindNetworkSuspend, now)
		if err := SetNetworkAbuseSuspended(ctx, old, suspendCaseId, now); err != nil {
			t.Fatal(err)
		}
		if standing := testAbuseStandingAt(t, ctx, old, now); standing.Standing != AbuseStandingSuspended || *standing.CaseId != suspendCaseId {
			t.Fatalf("suspended: %+v", standing)
		}
		testAbuseStandingLift(t, ctx, suspendCaseId, suspendId, AbuseReviewStateLiftedWrong, now.Add(time.Hour))
		if _, err := EndNetworkAbuseSuspended(ctx, old, nil, now.Add(time.Hour)); err != nil {
			t.Fatal(err)
		}
		ReevaluateNetworkAbuseState(ctx, old, now.Add(time.Hour))
		standing := testAbuseStandingAt(t, ctx, old, now.Add(time.Hour))
		if standing.Standing != AbuseStandingRestricted || *standing.CaseId != killCaseId {
			t.Fatalf("the suspension lifted as wrong, the kill standing: %+v", standing)
		}
		if row, _ := testAbuseStandingRow(t, ctx, old); row.untilTime == nil || !row.untilTime.Equal(now.Add(-10*24*time.Hour).Add(settings.RestrictedWindow)) {
			t.Fatalf("the restriction ends at %v", row.untilTime)
		}
	})
}

// A restriction under a barred case says so, so GET /abuse/status shows the standing and never why.
func TestNetworkAbuseStandingBarredCase(t *testing.T) {
	testAbuseStandingRun(t, func(t testing.TB, ctx context.Context, now time.Time) {
		networkId := testAbuseStandingNetwork(t, ctx, now, 60*24*time.Hour)
		caseId := testAbuseStandingCase(t, ctx, networkId, AbuseCategoryPhishing, true, now)
		testAbuseStandingAction(t, ctx, caseId, AbuseActionKindHostKill, now)
		if standing := testAbuseStandingAt(t, ctx, networkId, now); standing.Standing != AbuseStandingRestricted || !standing.Barred {
			t.Fatalf("a barred case: %+v", standing)
		}
	})
}

// The pass re-derives only the restricted rows past their until_time: one a newer action keeps is extended, one with
// nothing left is removed, and a suspended row or one still ahead is not read.
func TestAbuseStandingPassReachesOnlyExpiredRows(t *testing.T) {
	testAbuseStandingRun(t, func(t testing.TB, ctx context.Context, now time.Time) {
		settings := DefaultAbuseSettings()
		// rows written as a dump restored from before an action, or a pass that did not run, leave them
		writeRow := func(networkId server.Id, state string, caseId server.Id, untilTime *time.Time) {
			server.Tx(ctx, func(tx server.PgTx) {
				server.RaisePgResult(tx.Exec(
					ctx,
					`
						INSERT INTO network_abuse_state (network_id, state, case_id, until_time, update_time)
						VALUES ($1, $2, $3, $4, $5)
						ON CONFLICT (network_id) DO UPDATE SET state = EXCLUDED.state, case_id = EXCLUDED.case_id, until_time = EXCLUDED.until_time
					`,
					networkId, state, caseId, untilTime, now,
				))
			})
		}
		past := now.Add(-time.Hour)
		ahead := now.Add(24 * time.Hour)

		extended := testAbuseStandingNetwork(t, ctx, now, 60*24*time.Hour)
		extendedCaseId := testAbuseStandingCase(t, ctx, extended, AbuseCategorySpam, false, now.Add(-24*time.Hour))
		testAbuseStandingAction(t, ctx, extendedCaseId, AbuseActionKindMailHalt, now.Add(-24*time.Hour))
		writeRow(extended, networkAbuseStateRestricted, extendedCaseId, &past)

		removed := testAbuseStandingNetwork(t, ctx, now, 60*24*time.Hour)
		writeRow(removed, networkAbuseStateRestricted, server.NewId(), &past)

		suspended := testAbuseStandingNetwork(t, ctx, now, 60*24*time.Hour)
		writeRow(suspended, networkAbuseStateSuspended, server.NewId(), nil)

		current := testAbuseStandingNetwork(t, ctx, now, 60*24*time.Hour)
		writeRow(current, networkAbuseStateRestricted, server.NewId(), &ahead)

		result := RunAbuseStandingPass(ctx, now, settings)
		if result.ExtendedCount < 1 || result.RemovedCount < 1 {
			t.Fatalf("the pass: %+v", result)
		}
		if row, found := testAbuseStandingRow(t, ctx, extended); !found || row.untilTime == nil || !row.untilTime.Equal(now.Add(-24*time.Hour).Add(settings.RestrictedWindow)) {
			t.Fatalf("extended: %+v %t", row, found)
		}
		if _, found := testAbuseStandingRow(t, ctx, removed); found {
			t.Fatal("an expired row with nothing behind it stayed")
		}
		if row, found := testAbuseStandingRow(t, ctx, suspended); !found || row.state != networkAbuseStateSuspended {
			t.Fatal("the pass changed a suspended row")
		}
		if row, found := testAbuseStandingRow(t, ctx, current); !found || !row.untilTime.Equal(ahead) {
			t.Fatal("the pass changed a current row")
		}
	})
}

// NewAbuseGate over Postgres: a suspension from the snapshot, a restriction, probation and network_verified from
// one load, each answered from memory once loaded.
func TestNewAbuseGateReadsPostgres(t *testing.T) {
	testAbuseStandingRun(t, func(t testing.TB, ctx context.Context, now time.Time) {
		suspended := testAbuseStandingNetwork(t, ctx, now, 60*24*time.Hour)
		suspendCaseId := testAbuseStandingCase(t, ctx, suspended, AbuseCategoryCsam, true, now)
		if err := SetNetworkAbuseSuspended(ctx, suspended, suspendCaseId, now); err != nil {
			t.Fatal(err)
		}
		restricted := testAbuseStandingNetwork(t, ctx, now, 60*24*time.Hour)
		testAbuseStandingAction(t, ctx, testAbuseStandingCase(t, ctx, restricted, AbuseCategoryPhishing, false, now), AbuseActionKindHostKill, now)
		young := testAbuseStandingNetwork(t, ctx, now, 2*24*time.Hour)
		verified := testAbuseStandingNetwork(t, ctx, now, 60*24*time.Hour)
		testAddPaidTransferBalance(t, ctx, verified, now.Add(-time.Hour), now.Add(time.Hour), true)

		snapshotLoads := make(chan error, 1)
		snapshotSettings := DefaultAbuseHardDenySettings()
		snapshotSettings.Interval = 0
		snapshotSettings.OnLoad = func(count int, err error) {
			snapshotLoads <- err
		}
		snapshot := NewAbuseHardDenySnapshot(ctx, snapshotSettings)
		defer snapshot.Close()
		if err := <-snapshotLoads; err != nil {
			t.Fatal(err)
		}
		loads := make(chan testAbuseGateLoaded, 64)
		settings := DefaultAbuseGateSettings()
		settings.OnLoad = func(networkIds []server.Id, err error) {
			loads <- testAbuseGateLoaded{networkIds: networkIds, err: err}
		}
		gate := NewAbuseGate(ctx, snapshot, settings)
		defer gate.Close()

		networkIds := []server.Id{suspended, restricted, young, verified}
		for _, networkId := range networkIds {
			gate.Verified(connect.Id(networkId))
		}
		if err := testAwaitAbuseGateLoad(t, loads, networkIds...); err != nil {
			t.Fatal(err)
		}
		for _, c := range []struct {
			networkId server.Id
			standing  AbuseStanding
			verified  bool
		}{
			{networkId: suspended, standing: AbuseStandingSuspended},
			{networkId: restricted, standing: AbuseStandingRestricted},
			{networkId: young, standing: AbuseStandingProbation},
			{networkId: verified, standing: AbuseStandingStandard, verified: true},
		} {
			if standing, ok := gate.Standing(connect.Id(c.networkId)); !ok || standing != c.standing {
				t.Errorf("%s: %q %t", c.standing, standing, ok)
			}
			if verified, ok := gate.Verified(connect.Id(c.networkId)); !ok || verified != c.verified {
				t.Errorf("%s verified: %t %t", c.standing, verified, ok)
			}
		}
		if standing, denied, ok := snapshot.Denied(suspended); !ok || !denied || standing != AbuseStandingSuspended {
			t.Fatalf("the snapshot: %q %t %t", standing, denied, ok)
		}
	})
}
