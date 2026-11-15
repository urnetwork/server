//go:build sidecar_pending

package model

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/urnetwork/sidecar/dnsservice"

	"github.com/urnetwork/server"
)

// §21.8's reviews and appeal decisions on MG5's tables, and §12.6's restore flags (abuse_review_model.go;
// ABUSE-M5-12).

// A machine's host kill on a fresh case of networkId, pending review.
func testMachineAbuseKill(t testing.TB, ctx context.Context, networkId server.Id, host string, now time.Time) (caseId server.Id, actionId server.Id) {
	t.Helper()
	caseId, _ = testOpenAbuseCase(t, ctx, testAbuseCaseArgs(networkId, host), now)
	actionId = server.NewId()
	if _, _, err := testRecordAbuseAction(t, ctx, &AbuseActionArgs{
		ActionId: actionId,
		CaseId:   caseId,
		Kind:     AbuseActionKindHostKill,
		Target:   host,
	}, now); err != nil {
		t.Fatal(err)
	}
	return
}

// A review upholds a machine's action with its reviewer and reasons and closes the settled case; a second review
// of it changes nothing.
func TestRecordAbuseReviewUpholdsAndCloses(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		settings := DefaultAbuseSettings()
		now := server.NowUtc().Truncate(time.Millisecond)
		networkId := testAbuseCustomerNetwork(t, ctx)
		caseId, actionId := testMachineAbuseKill(t, ctx, networkId, "shop.review.example", now.Add(-time.Hour))
		reviewerId := server.NewId()

		review, err := RecordAbuseReview(ctx, &AbuseReviewArgs{CaseId: caseId, ActionId: &actionId, ReviewerId: reviewerId, Reason: "the page phishes"}, now, settings)
		if err != nil || !review.Recorded {
			t.Fatalf("the review: %+v, %v", review, err)
		}
		action := review.Action
		if action.ReviewState != AbuseReviewStateUpheld || action.ReviewerId == nil || *action.ReviewerId != reviewerId || action.ReviewReason != "the page phishes" || action.ReviewTime == nil || !action.ReviewTime.Equal(now) {
			t.Fatalf("the action after review: %+v", action)
		}
		if review.Case.State != AbuseCaseStateClosed || review.Case.CloseTime == nil {
			t.Fatalf("the settled case: %+v", review.Case)
		}
		again, err := RecordAbuseReview(ctx, &AbuseReviewArgs{CaseId: caseId, ActionId: &actionId, ReviewerId: server.NewId(), Reason: "again"}, now.Add(time.Minute), settings)
		if err != nil || again.Recorded || *again.Action.ReviewerId != reviewerId {
			t.Fatalf("a second review: %+v, %v", again, err)
		}
	})
}

// A case with no action takes a review of its own, which closes it; a case with an action does not.
func TestRecordAbuseReviewOfACaseWithNoAction(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		settings := DefaultAbuseSettings()
		now := server.NowUtc()
		networkId := testAbuseCustomerNetwork(t, ctx)
		emptyCaseId, _ := testOpenAbuseCase(t, ctx, testAbuseCaseArgs(networkId, "shop.empty.example"), now.Add(-time.Hour))
		review, err := RecordAbuseReview(ctx, &AbuseReviewArgs{CaseId: emptyCaseId, ReviewerId: server.NewId(), Reason: "nothing found"}, now, settings)
		if err != nil || !review.Recorded || review.Action != nil || review.Case.State != AbuseCaseStateClosed {
			t.Fatalf("a case's own review: %+v, %v", review, err)
		}

		actedCaseId, _ := testMachineAbuseKill(t, ctx, networkId, "shop.acted.example", now.Add(-time.Hour))
		if _, err := RecordAbuseReview(ctx, &AbuseReviewArgs{CaseId: actedCaseId, ReviewerId: server.NewId(), Reason: "whole"}, now, settings); !errors.Is(err, ErrAbuseReviewHasActions) {
			t.Fatalf("a case with an action reviewed whole: %v", err)
		}
	})
}

// A review refuses an action that was lifted, one of another case, a lift, and no reasons.
func TestRecordAbuseReviewRefuses(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		settings := DefaultAbuseSettings()
		now := server.NowUtc()
		networkId := testAbuseCustomerNetwork(t, ctx)
		caseId, actionId := testMachineAbuseKill(t, ctx, networkId, "shop.refused.example", now.Add(-time.Hour))
		otherCaseId, otherActionId := testMachineAbuseKill(t, ctx, networkId, "shop.otherrefused.example", now.Add(-time.Hour))
		liftId := server.NewId()
		if _, _, _, err := testRecordAbuseLift(t, ctx, &AbuseLiftArgs{ActionId: liftId, CaseId: otherCaseId, LiftedActionId: otherActionId, ReviewState: AbuseReviewStateLifted}, now); err != nil {
			t.Fatal(err)
		}

		for name, args := range map[string]*AbuseReviewArgs{
			"lifted":       {CaseId: otherCaseId, ActionId: &otherActionId, ReviewerId: server.NewId(), Reason: "why"},
			"other case's": {CaseId: caseId, ActionId: &otherActionId, ReviewerId: server.NewId(), Reason: "why"},
			"a lift":       {CaseId: otherCaseId, ActionId: &liftId, ReviewerId: server.NewId(), Reason: "why"},
			"no reasons":   {CaseId: caseId, ActionId: &actionId, ReviewerId: server.NewId(), Reason: " "},
			"no reviewer":  {CaseId: caseId, ActionId: &actionId, Reason: "why"},
		} {
			if _, err := RecordAbuseReview(ctx, args, now, settings); err == nil {
				t.Errorf("%s: reviewed", name)
			}
		}
		if _, err := RecordAbuseReview(ctx, &AbuseReviewArgs{CaseId: otherCaseId, ActionId: &otherActionId, ReviewerId: server.NewId(), Reason: "why"}, now, settings); !errors.Is(err, ErrAbuseActionLifted) {
			t.Fatalf("a lifted action: %v", err)
		}
		if _, err := RecordAbuseReview(ctx, &AbuseReviewArgs{CaseId: caseId, ActionId: &otherActionId, ReviewerId: server.NewId(), Reason: "why"}, now, settings); !errors.Is(err, ErrAbuseActionNotFound) {
			t.Fatalf("another case's action: %v", err)
		}
	})
}

// An appeal's decider must not have reviewed an action of its case, must give reasons, and decides once: a denial
// upholds the case and closes it again; the same denial again changes nothing; the other decision is refused.
func TestDecideAbuseAppealNeedsASecondReviewer(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		settings := DefaultAbuseSettings()
		now := server.NowUtc().Truncate(time.Millisecond)
		networkId := testAbuseCustomerNetwork(t, ctx)
		caseId, actionId := testMachineAbuseKill(t, ctx, networkId, "shop.second.example", now.Add(-3*time.Hour))
		firstReviewerId := server.NewId()
		if _, err := RecordAbuseReview(ctx, &AbuseReviewArgs{CaseId: caseId, ActionId: &actionId, ReviewerId: firstReviewerId, Reason: "upheld"}, now.Add(-2*time.Hour), settings); err != nil {
			t.Fatal(err)
		}
		appeal, _, err := CreateAbuseAppeal(ctx, networkId, server.NewId(), &AbuseAppealArgs{CaseId: caseId, Reason: "not phishing"}, now.Add(-time.Hour), DefaultAbuseCustomerSettings())
		if err != nil {
			t.Fatal(err)
		}

		if _, err := DecideAbuseAppeal(ctx, &AbuseAppealDecisionArgs{AppealId: appeal.AppealId, ReviewerId: firstReviewerId, State: AbuseAppealStateDenied, Reason: "still phishing"}, now, settings); !errors.Is(err, ErrAbuseAppealSameReviewer) {
			t.Fatalf("the action's reviewer decided: %v", err)
		}
		secondReviewerId := server.NewId()
		if _, err := DecideAbuseAppeal(ctx, &AbuseAppealDecisionArgs{AppealId: appeal.AppealId, ReviewerId: secondReviewerId, State: AbuseAppealStateDenied, Reason: ""}, now, settings); !errors.Is(err, ErrAbuseReviewInvalid) {
			t.Fatalf("a decision with no reasons: %v", err)
		}
		if _, err := DecideAbuseAppeal(ctx, &AbuseAppealDecisionArgs{AppealId: server.NewId(), ReviewerId: secondReviewerId, State: AbuseAppealStateDenied, Reason: "why"}, now, settings); !errors.Is(err, ErrAbuseAppealNotFound) {
			t.Fatalf("an unknown appeal: %v", err)
		}
		decision, err := DecideAbuseAppeal(ctx, &AbuseAppealDecisionArgs{AppealId: appeal.AppealId, ReviewerId: secondReviewerId, State: AbuseAppealStateDenied, Reason: "the login form is a copy"}, now, settings)
		if err != nil || !decision.Recorded || len(decision.LiftActionIds) != 0 {
			t.Fatalf("the denial: %+v, %v", decision, err)
		}
		if decision.Appeal.State != AbuseAppealStateDenied || *decision.Appeal.ReviewerId != secondReviewerId || decision.Appeal.DecisionReason != "the login form is a copy" || !decision.Appeal.DecideTime.Equal(now) {
			t.Fatalf("the decided appeal: %+v", decision.Appeal)
		}
		if decision.Case.State != AbuseCaseStateClosed {
			t.Fatalf("a denial left the case %s", decision.Case.State)
		}
		cases := ListNetworkAbuseCases(ctx, networkId, DefaultAbuseCustomerSettings()).Cases
		if len(cases) != 1 || cases[0].Appeal == nil || cases[0].Appeal.DecisionReason != "the login form is a copy" || cases[0].Appeal.State != AbuseAppealStateDenied {
			t.Fatalf("the network does not read the decision's reasons: %+v", cases)
		}

		again, err := DecideAbuseAppeal(ctx, &AbuseAppealDecisionArgs{AppealId: appeal.AppealId, ReviewerId: secondReviewerId, State: AbuseAppealStateDenied, Reason: "again"}, now.Add(time.Minute), settings)
		if err != nil || again.Recorded {
			t.Fatalf("the same denial again: %+v, %v", again, err)
		}
		if _, err := DecideAbuseAppeal(ctx, &AbuseAppealDecisionArgs{AppealId: appeal.AppealId, ReviewerId: secondReviewerId, State: AbuseAppealStateGranted, Reason: "changed my mind"}, now, settings); !errors.Is(err, ErrAbuseAppealDecided) {
			t.Fatalf("the other decision: %v", err)
		}
	})
}

// An appeal waits for its case's actions' first review; a grant answers each action still standing, a lifted one
// left out, and a lift during a pending appeal leaves the case open.
func TestDecideAbuseAppealGrantAnswersWhatStands(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		settings := DefaultAbuseSettings()
		now := server.NowUtc().Truncate(time.Millisecond)
		networkId := testAbuseCustomerNetwork(t, ctx)
		caseId, killId := testMachineAbuseKill(t, ctx, networkId, "shop.grant.example", now.Add(-4*time.Hour))
		appeal, _, err := CreateAbuseAppeal(ctx, networkId, server.NewId(), &AbuseAppealArgs{CaseId: caseId, Reason: "wrong host"}, now.Add(-3*time.Hour), DefaultAbuseCustomerSettings())
		if err != nil {
			t.Fatal(err)
		}
		if _, err := DecideAbuseAppeal(ctx, &AbuseAppealDecisionArgs{AppealId: appeal.AppealId, ReviewerId: server.NewId(), State: AbuseAppealStateGranted, Reason: "early"}, now.Add(-2*time.Hour), settings); !errors.Is(err, ErrAbuseAppealActionsPending) {
			t.Fatalf("a decision before the first review: %v", err)
		}

		reviewerId := server.NewId()
		if _, err := RecordAbuseReview(ctx, &AbuseReviewArgs{CaseId: caseId, ActionId: &killId, ReviewerId: reviewerId, Reason: "upheld"}, now.Add(-2*time.Hour), settings); err != nil {
			t.Fatal(err)
		}
		state, _, _ := testAbuseCaseState(t, ctx, caseId)
		if state != string(AbuseCaseStateOpen) {
			t.Fatalf("a review closed a case whose appeal waits: %s", state)
		}
		haltId := server.NewId()
		if _, _, err := testRecordAbuseAction(t, ctx, &AbuseActionArgs{ActionId: haltId, CaseId: caseId, Kind: AbuseActionKindMailHalt, Target: "grant.example", ReviewerId: &reviewerId}, now.Add(-90*time.Minute)); err != nil {
			t.Fatal(err)
		}
		labelId := server.NewId()
		if _, _, err := testRecordAbuseAction(t, ctx, &AbuseActionArgs{ActionId: labelId, CaseId: caseId, Kind: AbuseActionKindLabelKill, Target: "grant.example", ReviewerId: &reviewerId}, now.Add(-80*time.Minute)); err != nil {
			t.Fatal(err)
		}
		if _, _, _, err := testRecordAbuseLift(t, ctx, &AbuseLiftArgs{ActionId: server.NewId(), CaseId: caseId, LiftedActionId: haltId, ReviewState: AbuseReviewStateLifted, ReviewerId: &reviewerId}, now.Add(-time.Hour)); err != nil {
			t.Fatal(err)
		}
		if state, _, _ := testAbuseCaseState(t, ctx, caseId); state != string(AbuseCaseStateOpen) {
			t.Fatalf("a lift closed a case whose appeal waits: %s", state)
		}

		decision, err := DecideAbuseAppeal(ctx, &AbuseAppealDecisionArgs{AppealId: appeal.AppealId, ReviewerId: server.NewId(), State: AbuseAppealStateGranted, Reason: "the host was another's"}, now, settings)
		if err != nil || !decision.Recorded {
			t.Fatalf("the grant: %+v, %v", decision, err)
		}
		if len(decision.LiftActionIds) != 2 || decision.LiftActionIds[0] != killId || decision.LiftActionIds[1] != labelId {
			t.Fatalf("the actions a grant lifts: %v", decision.LiftActionIds)
		}
		if decision.Case.State != AbuseCaseStateOpen {
			t.Fatal("a grant closed the case before its lifts")
		}
		again, err := DecideAbuseAppeal(ctx, &AbuseAppealDecisionArgs{AppealId: appeal.AppealId, ReviewerId: server.NewId(), State: AbuseAppealStateGranted, Reason: "again"}, now, settings)
		if err != nil || again.Recorded || len(again.LiftActionIds) != 2 {
			t.Fatalf("the grant again, its lifts not run: %+v, %v", again, err)
		}
	})
}

// The abuse role clears one network's flags in an incident, each once, leaving another network's and another
// incident's; a root dns_root does not hold is refused.
func TestClearDnsRestoreFlag(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		now := server.NowUtc().Truncate(time.Microsecond)
		root := "flags.example."
		if err := EnsureDnsRoot(ctx, root); err != nil {
			t.Fatal(err)
		}
		networkId := server.NewId()
		otherNetworkId := server.NewId()
		incidentId := server.NewId()
		otherIncidentId := server.NewId()
		rows := []*dnsRestoreListRow{
			{IncidentId: incidentId, ListTime: now, NetworkId: &networkId, Owner: "a.n." + root, Type: 1, Flagged: true},
			{IncidentId: incidentId, ListTime: now, NetworkId: &networkId, Owner: "b.n." + root, Type: 16, Flagged: true},
			{IncidentId: incidentId, ListTime: now, NetworkId: &networkId, Owner: "c.n." + root, Type: 1},
			{IncidentId: incidentId, ListTime: now, NetworkId: &otherNetworkId, Owner: "a.o." + root, Type: 1, Flagged: true},
			{IncidentId: otherIncidentId, ListTime: now, NetworkId: &networkId, Owner: "a.n." + root, Type: 1, Flagged: true},
		}
		testAbuseExec(t, ctx, `UPDATE dns_root SET restore_lists = $2::jsonb WHERE root = $1`, root, dnsJsonArray(rows))

		clearedCount, err := ClearDnsRestoreFlag(ctx, root, incidentId, networkId, now)
		if err != nil || clearedCount != 2 {
			t.Fatalf("the clear: %d, %v", clearedCount, err)
		}
		if clearedCount, err := ClearDnsRestoreFlag(ctx, root, incidentId, networkId, now.Add(time.Minute)); err != nil || clearedCount != 0 {
			t.Fatalf("a second clear: %d, %v", clearedCount, err)
		}
		var stored []*dnsRestoreListRow
		server.Db(ctx, func(conn server.PgConn) {
			var listJson []byte
			server.Raise(conn.QueryRow(ctx, `SELECT restore_lists FROM dns_root WHERE root = $1`, root).Scan(&listJson))
			server.Raise(json.Unmarshal(listJson, &stored))
		})
		cleared := 0
		for _, row := range stored {
			if row.FlagClearTime == nil {
				continue
			}
			cleared += 1
			if row.IncidentId != incidentId || *row.NetworkId != networkId || !row.FlagClearTime.Equal(now) {
				t.Errorf("a cleared row: %+v", row)
			}
		}
		if cleared != 2 || len(stored) != len(rows) {
			t.Fatalf("%d of %d rows cleared", cleared, len(stored))
		}
		if _, err := ClearDnsRestoreFlag(ctx, "nothere.example.", incidentId, networkId, now); !errors.Is(err, dnsservice.ErrDnsRootUnknown) {
			t.Fatalf("an unknown root: %v", err)
		}
	})
}
