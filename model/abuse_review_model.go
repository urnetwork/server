package model

// §21.8's reviews and appeal decisions by the abuse role (ABUSE-M5-12; D-ABUSE-5, D-ABUSE-7, I-13; QD-126), and
// §12.6's restore flags the role clears (DNS-M5-15, §21.10's check).
//
// A review settles a machine's action within its clock (§21.8): RecordAbuseReview upholds it, or, for a case with
// no action, reviews the case itself, which closes it (plan-ABUSE-small-readings #9). A lift is no review here: it
// runs through controller.LiftAbuseAction with the reviewer and the reason, on §21's one path, so it is recorded,
// undone, noticed and mailed like any action. Each review keeps its reviewer's opaque id and reasons on the action
// (plan-ABUSE-metadata-rows #6). A case closes once no action of it waits on review and no appeal of it waits on a
// decision.
//
// DecideAbuseAppeal is the second reviewer (§21.8, DSA Art. 17): a reviewer who reviewed or took any of the case's
// actions may not decide its appeal, every action must be reviewed first, and a decision states its reasons, which
// the appellant reads in GET /abuse/cases. A denial upholds the case and closes it again; a grant answers the actions
// still standing, each for controller.LiftAbuseAction as lifted_wrong, review having found them wrong, which takes
// them out of every count. The decision commits before the lifts, so a grant whose lifts are cut short is decided
// again by the same decision, which answers the actions still standing.
//
// Nothing here reads or writes the tables unless the binary links MG5 (server.SidecarMg5Linked).

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/urnetwork/sidecar/dnsservice"

	"github.com/urnetwork/server"
)

// A review or decision no row may hold: an unknown decision, no reason, or one past the settings' bound.
var ErrAbuseReviewInvalid = errors.New("abuse review invalid")

// A case with actions has no review of its own: each action takes its own.
var ErrAbuseReviewHasActions = errors.New("abuse review of a case with actions")

// The appeal's decider reviewed or took one of the case's actions.
var ErrAbuseAppealSameReviewer = errors.New("abuse appeal decided by its action's reviewer")

// An action of the appeal's case still waits on its first review.
var ErrAbuseAppealActionsPending = errors.New("abuse appeal with an unreviewed action")

// Refuses reasons no row may hold: none, past maxByteCount (AbuseSettings.ReviewReasonMaxByteCount), invalid UTF-8,
// or a NUL.
func validateAbuseReviewReason(reason string, maxByteCount int) error {
	switch {
	case strings.TrimSpace(reason) == "":
		return fmt.Errorf("%w: no reason", ErrAbuseReviewInvalid)
	case maxByteCount < len(reason):
		return fmt.Errorf("%w: a reason of %d bytes", ErrAbuseReviewInvalid, len(reason))
	case !utf8.ValidString(reason) || strings.ContainsRune(reason, 0):
		return fmt.Errorf("%w: a reason that is not text", ErrAbuseReviewInvalid)
	}
	return nil
}

// Closes the case at now in the caller's transaction once nothing of it is left to settle: no action waiting on
// review and no appeal waiting on a decision. A case closed already takes now as its last close, from which its
// retention runs (plan-ABUSE-small-readings #9).
func closeSettledAbuseCaseInTx(ctx context.Context, tx server.PgTx, caseId server.Id, now time.Time) {
	server.RaisePgResult(tx.Exec(
		ctx,
		`
			UPDATE abuse_case
			SET state = 'closed', close_time = $2
			WHERE
				abuse_case.case_id = $1 AND
				NOT EXISTS (
					SELECT 1
					FROM abuse_action
					WHERE
						abuse_action.case_id = $1 AND
						abuse_action.kind <> 'lift' AND
						abuse_action.review_state NOT IN ('upheld', 'lifted', 'lifted_wrong')
				) AND
				NOT EXISTS (
					SELECT 1
					FROM abuse_appeal
					WHERE abuse_appeal.case_id = $1 AND abuse_appeal.state = 'pending'
				)
		`,
		caseId,
		now.UTC(),
	))
}

// One review: an action of the case upheld, or a case with no action reviewed.
type AbuseReviewArgs struct {
	CaseId server.Id
	// the action reviewed; nil reviews a case that has none, which closes it
	ActionId   *server.Id
	ReviewerId server.Id
	Reason     string
}

// What a review did.
type AbuseReviewResult struct {
	// the action as it stands after the review; nil for a case's own review
	Action *AbuseAction
	Case   *AbuseCase
	// false when the action was upheld already, so this changed nothing
	Recorded bool
}

// Records a review at now, the file header's. ErrAbuseCaseNotFound and ErrAbuseActionNotFound for a case or an
// action of the case that does not exist, a lift counting as none; ErrAbuseActionLifted for an action already
// lifted; ErrAbuseReviewHasActions for a case's own review while it has actions.
func RecordAbuseReview(ctx context.Context, args *AbuseReviewArgs, now time.Time, settings *AbuseSettings) (result *AbuseReviewResult, returnErr error) {
	if !server.SidecarMg5Linked() {
		return nil, ErrAbuseUnlinked
	}
	if args.ReviewerId == (server.Id{}) {
		return nil, fmt.Errorf("%w: no reviewer", ErrAbuseReviewInvalid)
	}
	if err := validateAbuseReviewReason(args.Reason, settings.ReviewReasonMaxByteCount); err != nil {
		return nil, err
	}
	now = now.UTC()
	server.Tx(ctx, func(tx server.PgTx) {
		result = nil
		returnErr = nil
		abuseCase := getAbuseCaseInTx(ctx, tx, args.CaseId, true)
		if abuseCase == nil {
			returnErr = ErrAbuseCaseNotFound
			return
		}

		if args.ActionId == nil {
			var actionCount int
			server.Raise(tx.QueryRow(
				ctx,
				`SELECT COUNT(*) FROM abuse_action WHERE abuse_action.case_id = $1 AND abuse_action.kind <> 'lift'`,
				args.CaseId,
			).Scan(&actionCount))
			if 0 < actionCount {
				returnErr = ErrAbuseReviewHasActions
				return
			}
			closeSettledAbuseCaseInTx(ctx, tx, args.CaseId, now)
			result = &AbuseReviewResult{
				Case:     getAbuseCaseInTx(ctx, tx, args.CaseId, false),
				Recorded: abuseCase.State == AbuseCaseStateOpen,
			}
			return
		}

		action := getAbuseActionInTx(ctx, tx, *args.ActionId, true)
		if action == nil || action.CaseId != args.CaseId || action.Kind == AbuseActionKindLift {
			returnErr = ErrAbuseActionNotFound
			return
		}
		switch action.ReviewState {
		case AbuseReviewStateLifted, AbuseReviewStateLiftedWrong:
			returnErr = ErrAbuseActionLifted
			return
		case AbuseReviewStateUpheld:
			result = &AbuseReviewResult{Action: action, Case: abuseCase}
			return
		}
		server.RaisePgResult(tx.Exec(
			ctx,
			`
				UPDATE abuse_action
				SET review_state = $2, reviewer_id = $3, review_reason = $4, review_time = $5
				WHERE abuse_action.action_id = $1
			`,
			action.ActionId,
			string(AbuseReviewStateUpheld),
			args.ReviewerId,
			args.Reason,
			now,
		))
		closeSettledAbuseCaseInTx(ctx, tx, args.CaseId, now)
		result = &AbuseReviewResult{
			Action:   getAbuseActionInTx(ctx, tx, action.ActionId, false),
			Case:     getAbuseCaseInTx(ctx, tx, args.CaseId, false),
			Recorded: true,
		}
	})
	return
}

// One appeal decision.
type AbuseAppealDecisionArgs struct {
	AppealId   server.Id
	ReviewerId server.Id
	// AbuseAppealStateGranted or AbuseAppealStateDenied
	State  AbuseAppealState
	Reason string
}

// What a decision did.
type AbuseAppealDecision struct {
	Appeal *AbuseAppeal
	Case   *AbuseCase
	// for a grant, the case's actions still standing, each for controller.LiftAbuseAction as lifted_wrong
	LiftActionIds []server.Id
	// false when the same decision was recorded already, so this changed nothing
	Recorded bool
}

// The case's actions neither lifted nor lifts themselves, oldest first.
func getStandingAbuseCaseActionIdsInTx(ctx context.Context, tx server.PgTx, caseId server.Id) []server.Id {
	actionIds := []server.Id{}
	result, err := tx.Query(
		ctx,
		`
			SELECT abuse_action.action_id
			FROM abuse_action
			WHERE
				abuse_action.case_id = $1 AND
				abuse_action.kind <> 'lift' AND
				abuse_action.review_state NOT IN ('lifted', 'lifted_wrong')
			ORDER BY abuse_action.create_time, abuse_action.action_id
		`,
		caseId,
	)
	server.WithPgResult(result, err, func() {
		for result.Next() {
			var actionId server.Id
			server.Raise(result.Scan(&actionId))
			actionIds = append(actionIds, actionId)
		}
	})
	return actionIds
}

// Decides an appeal at now as its second reviewer, the file header's. ErrAbuseAppealNotFound for no such appeal;
// ErrAbuseAppealDecided for one decided the other way; ErrAbuseAppealSameReviewer when the decider reviewed or took
// one of the case's actions; ErrAbuseAppealActionsPending while an action waits on its first review.
func DecideAbuseAppeal(ctx context.Context, args *AbuseAppealDecisionArgs, now time.Time, settings *AbuseSettings) (decision *AbuseAppealDecision, returnErr error) {
	if !server.SidecarMg5Linked() {
		return nil, ErrAbuseUnlinked
	}
	if args.ReviewerId == (server.Id{}) {
		return nil, fmt.Errorf("%w: no reviewer", ErrAbuseReviewInvalid)
	}
	if args.State != AbuseAppealStateGranted && args.State != AbuseAppealStateDenied {
		return nil, fmt.Errorf("%w: an appeal decided to %q", ErrAbuseReviewInvalid, args.State)
	}
	if err := validateAbuseReviewReason(args.Reason, settings.ReviewReasonMaxByteCount); err != nil {
		return nil, err
	}
	now = now.UTC()
	server.Tx(ctx, func(tx server.PgTx) {
		decision = nil
		returnErr = nil
		var appeal *AbuseAppeal
		result, err := tx.Query(
			ctx,
			`SELECT `+abuseAppealColumns+` FROM abuse_appeal WHERE abuse_appeal.appeal_id = $1 FOR UPDATE`,
			args.AppealId,
		)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				appeal = scanAbuseAppeal(result)
			}
		})
		if appeal == nil {
			returnErr = ErrAbuseAppealNotFound
			return
		}
		abuseCase := getAbuseCaseInTx(ctx, tx, appeal.CaseId, true)
		if abuseCase == nil {
			returnErr = ErrAbuseCaseNotFound
			return
		}

		if appeal.State != AbuseAppealStatePending {
			if appeal.State != args.State {
				returnErr = ErrAbuseAppealDecided
				return
			}
			// the same decision again: a grant answers what still stands, for lifts a cut-short run left
			decision = &AbuseAppealDecision{Appeal: appeal, Case: abuseCase, LiftActionIds: []server.Id{}}
			if appeal.State == AbuseAppealStateGranted {
				decision.LiftActionIds = getStandingAbuseCaseActionIdsInTx(ctx, tx, appeal.CaseId)
			}
			return
		}

		reviewed := false
		pending := false
		result, err = tx.Query(
			ctx,
			`
				SELECT abuse_action.reviewer_id, abuse_action.review_state
				FROM abuse_action
				WHERE abuse_action.case_id = $1 AND abuse_action.kind <> 'lift'
			`,
			appeal.CaseId,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var reviewerId *server.Id
				var reviewState string
				server.Raise(result.Scan(&reviewerId, &reviewState))
				reviewed = reviewed || (reviewerId != nil && *reviewerId == args.ReviewerId)
				pending = pending || AbuseReviewState(reviewState) == AbuseReviewStatePending
			}
		})
		if reviewed {
			returnErr = ErrAbuseAppealSameReviewer
			return
		}
		if pending {
			returnErr = ErrAbuseAppealActionsPending
			return
		}

		server.RaisePgResult(tx.Exec(
			ctx,
			`
				UPDATE abuse_appeal
				SET state = $2, reviewer_id = $3, decision_reason = $4, decide_time = $5
				WHERE abuse_appeal.appeal_id = $1
			`,
			appeal.AppealId,
			string(args.State),
			args.ReviewerId,
			args.Reason,
			now,
		))
		liftActionIds := []server.Id{}
		if args.State == AbuseAppealStateGranted {
			liftActionIds = getStandingAbuseCaseActionIdsInTx(ctx, tx, appeal.CaseId)
		}
		if len(liftActionIds) == 0 {
			// nothing left for a lift to close the case on
			closeSettledAbuseCaseInTx(ctx, tx, appeal.CaseId, now)
		}

		decidedAppeal := *appeal
		decidedAppeal.State = args.State
		decidedAppeal.ReviewerId = &args.ReviewerId
		decidedAppeal.DecisionReason = args.Reason
		decidedAppeal.DecideTime = &now
		decision = &AbuseAppealDecision{
			Appeal:        &decidedAppeal,
			Case:          getAbuseCaseInTx(ctx, tx, appeal.CaseId, false),
			LiftActionIds: liftActionIds,
			Recorded:      true,
		}
	})
	return
}

// Clears the restore flags of one network's entries in an incident of root at now (§12.6's check, §21.10): each of
// its flagged RRsets and custom hosts the incident listed, once the abuse role has looked at them, takes now as its
// flag's clear time. Takes root's lister lock first, as every lister of a root does (dns_restore_model.go), so a
// listing and a clear never write over each other. Returns how many flags it cleared, 0 for none left, so a second
// call is harmless. dnsservice.ErrDnsRootUnknown for a root dns_root does not hold.
func ClearDnsRestoreFlag(ctx context.Context, root string, incidentId server.Id, networkId server.Id, now time.Time) (clearedCount int, returnErr error) {
	if !server.SidecarMg5Linked() {
		return 0, ErrDnsUnlinked
	}
	clearTime := now.UTC().Truncate(time.Microsecond)
	server.Tx(ctx, func(tx server.PgTx) {
		clearedCount = 0
		returnErr = nil
		lockDnsRestoreListsInTx(ctx, tx, root)
		listRows, _, found := getDnsRestoreRowsInTx(ctx, tx, root)
		if !found {
			returnErr = fmt.Errorf("%w: %s", dnsservice.ErrDnsRootUnknown, root)
			return
		}
		for _, row := range listRows {
			if row.IncidentId != incidentId || row.NetworkId == nil || *row.NetworkId != networkId || !row.Flagged || row.FlagClearTime != nil {
				continue
			}
			row.FlagClearTime = &clearTime
			clearedCount += 1
		}
		if 0 < clearedCount {
			server.RaisePgResult(tx.Exec(
				ctx,
				`UPDATE dns_root SET restore_lists = $2::jsonb WHERE root = $1`,
				root,
				dnsJsonArray(listRows),
			))
		}
	}, server.TxReadCommitted)
	return
}
