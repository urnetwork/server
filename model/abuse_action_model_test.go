//go:build sidecar_pending

package model

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/urnetwork/server"
)

// §21.4's actions and lifts on MG5's abuse_action.

// Records an action in its own transaction.
func testRecordAbuseAction(t testing.TB, ctx context.Context, args *AbuseActionArgs, now time.Time) (*AbuseAction, bool, error) {
	t.Helper()
	var action *AbuseAction
	var inserted bool
	var err error
	server.Tx(ctx, func(tx server.PgTx) {
		action, _, inserted, err = RecordAbuseActionInTx(ctx, tx, args, now, DefaultAbuseSettings())
	})
	return action, inserted, err
}

// Records a lift in its own transaction.
func testRecordAbuseLift(t testing.TB, ctx context.Context, args *AbuseLiftArgs, now time.Time) (*AbuseAction, *AbuseCase, bool, error) {
	t.Helper()
	var lift *AbuseAction
	var abuseCase *AbuseCase
	var inserted bool
	var err error
	server.Tx(ctx, func(tx server.PgTx) {
		lift, _, abuseCase, inserted, err = RecordAbuseLiftInTx(ctx, tx, args, now)
	})
	return lift, abuseCase, inserted, err
}

// A machine's host kill on a fresh case.
func testHostKill(t testing.TB, ctx context.Context, now time.Time) (*AbuseActionArgs, server.Id) {
	t.Helper()
	caseId, _ := testOpenAbuseCase(t, ctx, testAbuseCaseArgs(server.NewId(), "shop.root.example"), now)
	return &AbuseActionArgs{
		ActionId: server.NewId(),
		CaseId:   caseId,
		Kind:     AbuseActionKindHostKill,
		Target:   "shop.root.example",
	}, caseId
}

// The same action id twice is one row, and the second call says so. The same
// id for another action is refused.
func TestAbuseActionIdIsIdempotent(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		now := server.NowUtc()
		args, caseId := testHostKill(t, ctx, now)
		first, inserted, err := testRecordAbuseAction(t, ctx, args, now)
		if err != nil || !inserted {
			t.Fatalf("the first record: %t, %v", inserted, err)
		}
		again, inserted, err := testRecordAbuseAction(t, ctx, args, now.Add(time.Minute))
		if err != nil || inserted {
			t.Fatalf("the second record: %t, %v", inserted, err)
		}
		if again.ActionId != first.ActionId || !again.CreateTime.Equal(first.CreateTime) {
			t.Fatalf("the second record read another row: %+v", again)
		}
		if actions := GetAbuseCaseActions(ctx, caseId); len(actions) != 1 {
			t.Fatalf("%d rows", len(actions))
		}

		other := *args
		other.Kind = AbuseActionKindLabelKill
		if _, _, err := testRecordAbuseAction(t, ctx, &other, now); !errors.Is(err, ErrAbuseActionConflict) {
			t.Fatalf("another action under the id: %v", err)
		}
	})
}

// A machine's action is pending and sets the case's clock, never later than it
// was; a person's is upheld, by them, and sets none.
func TestAbuseActionReviewClock(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		now := server.NowUtc().Truncate(time.Second)
		args, caseId := testHostKill(t, ctx, now)
		args.ReviewDueTime = now.Add(24 * time.Hour)
		machine, _, err := testRecordAbuseAction(t, ctx, args, now)
		if err != nil {
			t.Fatal(err)
		}
		if !machine.ByMachine || machine.ReviewState != AbuseReviewStatePending || machine.ReviewerId != nil {
			t.Fatalf("a machine's action reads %+v", machine)
		}
		if machine.NetworkId == nil || machine.Category != AbuseCategoryPhishing {
			t.Fatalf("the action did not take its case's network and category: %+v", machine)
		}

		reviewerId := server.NewId()
		person := &AbuseActionArgs{
			ActionId:      server.NewId(),
			CaseId:        caseId,
			Kind:          AbuseActionKindLabelKill,
			Target:        "shop.root.example",
			ReviewerId:    &reviewerId,
			ReviewReason:  "the second strike",
			ReviewDueTime: now.Add(time.Hour),
		}
		personAction, _, err := testRecordAbuseAction(t, ctx, person, now)
		if err != nil {
			t.Fatal(err)
		}
		if personAction.ByMachine || personAction.ReviewState != AbuseReviewStateUpheld || personAction.ReviewTime == nil ||
			personAction.ReviewerId == nil || *personAction.ReviewerId != reviewerId {
			t.Fatalf("a person's action reads %+v", personAction)
		}
		if abuseCase := GetAbuseCase(ctx, caseId); !abuseCase.ReviewDueTime.Equal(now.Add(24 * time.Hour)) {
			t.Fatalf("the case's clock is %v", abuseCase.ReviewDueTime)
		}

		// a machine action with no clock takes the short one, MachineReview
		later := &AbuseActionArgs{ActionId: server.NewId(), CaseId: caseId, Kind: AbuseActionKindMailHalt, Target: "mail.root.example"}
		if _, _, err := testRecordAbuseAction(t, ctx, later, now.Add(-time.Hour)); err != nil {
			t.Fatal(err)
		}
		if abuseCase := GetAbuseCase(ctx, caseId); !abuseCase.ReviewDueTime.Equal(now.Add(23 * time.Hour)) {
			t.Fatalf("the case's clock is %v", abuseCase.ReviewDueTime)
		}
	})
}

// No action without its case, and no kind a row does not record.
func TestAbuseActionRefusesWhatNoRowHolds(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		now := server.NowUtc()
		args, _ := testHostKill(t, ctx, now)
		for _, c := range []struct {
			name   string
			change func(args *AbuseActionArgs)
			want   error
		}{
			{name: "no case", change: func(args *AbuseActionArgs) { args.CaseId = server.NewId() }, want: ErrAbuseCaseNotFound},
			{name: "no id", change: func(args *AbuseActionArgs) { args.ActionId = server.Id{} }, want: ErrAbuseActionInvalid},
			{name: "the ladder", change: func(args *AbuseActionArgs) { args.Kind = AbuseActionKindMailLadder }, want: ErrAbuseActionInvalid},
			{name: "a lift", change: func(args *AbuseActionArgs) { args.Kind = AbuseActionKindLift }, want: ErrAbuseActionInvalid},
			{name: "no target", change: func(args *AbuseActionArgs) { args.Target = "" }, want: ErrAbuseActionInvalid},
		} {
			changed := *args
			changed.ActionId = server.NewId()
			c.change(&changed)
			if _, _, err := testRecordAbuseAction(t, ctx, &changed, now); !errors.Is(err, c.want) {
				t.Errorf("%s: %v", c.name, err)
			}
		}
	})
}

// A lift goes only through the case: its own row, kind lift, naming the lifted
// action, whose review it records; the case closes once every action is lifted
// or upheld. A second lift of one action, a lift of a lift, of another case's
// action or of an unknown one are refused, and the same lift id twice is one row.
func TestAbuseLiftThroughTheCase(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		now := server.NowUtc()
		args, caseId := testHostKill(t, ctx, now)
		action, _, err := testRecordAbuseAction(t, ctx, args, now)
		if err != nil {
			t.Fatal(err)
		}
		otherArgs, _ := testHostKill(t, ctx, now)
		otherAction, _, err := testRecordAbuseAction(t, ctx, otherArgs, now)
		if err != nil {
			t.Fatal(err)
		}

		reviewerId := server.NewId()
		liftArgs := &AbuseLiftArgs{
			ActionId:       server.NewId(),
			CaseId:         caseId,
			LiftedActionId: action.ActionId,
			ReviewState:    AbuseReviewStateLiftedWrong,
			ReviewerId:     &reviewerId,
			ReviewReason:   "the host was not phishing",
		}
		for _, c := range []struct {
			name   string
			change func(args *AbuseLiftArgs)
			want   error
		}{
			{name: "an unknown action", change: func(args *AbuseLiftArgs) { args.LiftedActionId = server.NewId() }, want: ErrAbuseActionNotFound},
			{name: "another case's action", change: func(args *AbuseLiftArgs) { args.LiftedActionId = otherAction.ActionId }, want: ErrAbuseActionNotFound},
			{name: "an unknown case", change: func(args *AbuseLiftArgs) { args.CaseId = server.NewId() }, want: ErrAbuseCaseNotFound},
			{name: "a review that lifts nothing", change: func(args *AbuseLiftArgs) { args.ReviewState = AbuseReviewStateUpheld }, want: ErrAbuseActionInvalid},
		} {
			changed := *liftArgs
			changed.ActionId = server.NewId()
			c.change(&changed)
			if _, _, _, err := testRecordAbuseLift(t, ctx, &changed, now); !errors.Is(err, c.want) {
				t.Errorf("%s: %v", c.name, err)
			}
		}

		lift, abuseCase, inserted, err := testRecordAbuseLift(t, ctx, liftArgs, now)
		if err != nil || !inserted {
			t.Fatalf("the lift: %t, %v", inserted, err)
		}
		if lift.Kind != AbuseActionKindLift || lift.Target != action.ActionId.String() || lift.ByMachine {
			t.Fatalf("the lift reads %+v", lift)
		}
		lifted := GetAbuseAction(ctx, action.ActionId)
		if lifted.ReviewState != AbuseReviewStateLiftedWrong || lifted.ReviewerId == nil || lifted.ReviewReason != liftArgs.ReviewReason {
			t.Fatalf("the lifted action reads %+v", lifted)
		}
		if abuseCase.State != AbuseCaseStateClosed || abuseCase.CloseTime == nil {
			t.Fatalf("the settled case reads %+v", abuseCase)
		}

		again, _, inserted, err := testRecordAbuseLift(t, ctx, liftArgs, now)
		if err != nil || inserted || again.ActionId != lift.ActionId {
			t.Fatalf("the same lift again: %t, %v", inserted, err)
		}
		second := *liftArgs
		second.ActionId = server.NewId()
		if _, _, _, err := testRecordAbuseLift(t, ctx, &second, now); !errors.Is(err, ErrAbuseActionLifted) {
			t.Fatalf("a second lift: %v", err)
		}
		ofALift := *liftArgs
		ofALift.ActionId = server.NewId()
		ofALift.LiftedActionId = lift.ActionId
		if _, _, _, err := testRecordAbuseLift(t, ctx, &ofALift, now); !errors.Is(err, ErrAbuseActionNotFound) {
			t.Fatalf("a lift of a lift: %v", err)
		}
	})
}

// A case with an action still pending stays open after another is lifted.
func TestAbuseLiftLeavesAnUnsettledCaseOpen(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		now := server.NowUtc()
		args, caseId := testHostKill(t, ctx, now)
		first, _, err := testRecordAbuseAction(t, ctx, args, now)
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err := testRecordAbuseAction(t, ctx, &AbuseActionArgs{
			ActionId: server.NewId(), CaseId: caseId, Kind: AbuseActionKindMailHalt, Target: "mail.root.example",
		}, now); err != nil {
			t.Fatal(err)
		}
		_, abuseCase, _, err := testRecordAbuseLift(t, ctx, &AbuseLiftArgs{
			ActionId: server.NewId(), CaseId: caseId, LiftedActionId: first.ActionId, ReviewState: AbuseReviewStateLifted,
		}, now)
		if err != nil {
			t.Fatal(err)
		}
		if abuseCase.State != AbuseCaseStateOpen {
			t.Fatalf("a case with a pending action closed: %+v", abuseCase)
		}
	})
}

// The owed list pages by its cursor, oldest first, and leaves out what is done
// and what is too new.
func TestOwedAbuseActionsPageByCursor(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		now := server.NowUtc()
		args, _ := testHostKill(t, ctx, now)
		actionIds := []server.Id{}
		for i := range 5 {
			changed := *args
			changed.ActionId = server.NewId()
			action, _, err := testRecordAbuseAction(t, ctx, &changed, now.Add(time.Duration(i)*time.Second))
			if err != nil {
				t.Fatal(err)
			}
			actionIds = append(actionIds, action.ActionId)
		}
		SetAbuseActionApplied(ctx, actionIds[1], now)
		SetAbuseActionMailed(ctx, actionIds[1], now)
		SetAbuseActionApplied(ctx, actionIds[2], now)
		// actionIds[4] is too new for a pass that lists before now+4s

		listed := []server.Id{}
		cursor := AbuseActionCursor{}
		for {
			actions, next := ListOwedAbuseActions(ctx, now.Add(4*time.Second), cursor, 2)
			if len(actions) == 0 {
				break
			}
			for _, action := range actions {
				listed = append(listed, action.ActionId)
			}
			cursor = next
		}
		want := []server.Id{actionIds[0], actionIds[2], actionIds[3]}
		if len(listed) != len(want) {
			t.Fatalf("listed %v, want %v", listed, want)
		}
		for i := range want {
			if listed[i] != want[i] {
				t.Fatalf("listed %v, want %v", listed, want)
			}
		}
	})
}
