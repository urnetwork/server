package model

// What §19.10's abuse step writes (ABUSE-M5-18; D-ABUSE-7, D-OPS-14, FS-42, I-14): the rows of actions, lifts and
// holds the off-site mailboxes hold from after the sidecar dump, re-created under their own ids. None of it owes a
// notice, a record mail or a hint: the history already holds each one, and a second would read as a second action.
//
//	case    re-created under its id when the restored rows lack it: its category and network from the record, its
//	        target the network, reporter class restore and barred, so it shows the network nothing it was not shown
//	        already, since the record cannot say whether a sealing order covered it
//	action  under its action_id with its commit time, mailed_time set (the mailbox holds it), pending review with
//	        its case's clock due at once: its review history went with the primary, and every doubt reviews early
//	lift    under its own id, lifted, never lifted as wrong, which the record cannot say and which would take its
//	        action out of every count
//	hold    part 0 with its original placement and expiry, copy pending, and renew_time set for a renewed record,
//	        so a second renewal is refused as it was
//
// Each insert is idempotent by its id, so a second run of the step changes nothing.

import (
	"context"
	"fmt"
	"time"

	"github.com/urnetwork/sidecar"

	"github.com/urnetwork/server"
)

// Who a re-created case came from.
const AbuseReporterClassRestore AbuseReporterClass = "restore"

// What one restore needs of a mailed action or lift.
type AbuseRestoredAction struct {
	ActionId  server.Id
	CaseId    server.Id
	Category  AbuseCategory
	NetworkId *server.Id
	Kind      AbuseActionKind
	Target    string
	UntilTime *time.Time
	// its commit time, from the record
	Time time.Time
}

// Re-creates the action's case under its id when there is none, in the caller's transaction; created is false when
// the case was there. The case is open, barred, joined by its network, its target the network, and due for review
// at now.
func RestoreAbuseCaseInTx(ctx context.Context, tx server.PgTx, action *AbuseRestoredAction, now time.Time) (abuseCase *AbuseCase, created bool) {
	if abuseCase = getAbuseCaseInTx(ctx, tx, action.CaseId, true); abuseCase != nil {
		return abuseCase, false
	}
	target := action.CaseId.String()
	targetKind := AbuseTargetKindNetwork
	if action.NetworkId != nil {
		target = action.NetworkId.String()
	}
	category := action.Category
	if !abuseCategoryKnown(category) {
		category = AbuseCategoryOther
	}
	server.RaisePgResult(tx.Exec(
		ctx,
		`
			INSERT INTO abuse_case (
				case_id,
				network_id,
				category,
				target_kind,
				target,
				reporter_class,
				join_kind,
				join_key,
				state,
				barred,
				review_due_time,
				create_time
			) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, 'open', true, $9, $10)
			ON CONFLICT (case_id) DO NOTHING
		`,
		action.CaseId,
		action.NetworkId,
		string(category),
		string(targetKind),
		target,
		string(AbuseReporterClassRestore),
		string(AbuseJoinKindNetwork),
		target,
		now.UTC(),
		action.Time.UTC(),
	))
	return getAbuseCaseInTx(ctx, tx, action.CaseId, true), true
}

// Records a mailed action under its action_id, in the caller's transaction (see the file header); inserted is false
// when the id names a row already, which must be this action. Owes no notice and no mail.
func RestoreAbuseActionInTx(ctx context.Context, tx server.PgTx, action *AbuseRestoredAction, now time.Time) (stored *AbuseAction, inserted bool, err error) {
	if !abuseActionKindRecorded(action.Kind) {
		return nil, false, fmt.Errorf("%w: kind %q records no row", ErrAbuseActionInvalid, action.Kind)
	}
	if action.Target == "" || abuseTargetMaxLength < len(action.Target) {
		return nil, false, fmt.Errorf("%w: a target of %d bytes", ErrAbuseActionInvalid, len(action.Target))
	}
	abuseCase, _ := RestoreAbuseCaseInTx(ctx, tx, action, now)
	commitTime := action.Time.UTC()
	row := &AbuseAction{
		ActionId:    action.ActionId,
		CaseId:      action.CaseId,
		NetworkId:   abuseCase.NetworkId,
		Category:    abuseCase.Category,
		Kind:        action.Kind,
		Target:      action.Target,
		UntilTime:   action.UntilTime,
		ByMachine:   true,
		ReviewState: AbuseReviewStatePending,
		CreateTime:  commitTime,
	}
	stored, inserted = insertAbuseActionInTx(ctx, tx, row)
	if !inserted {
		if stored.CaseId != action.CaseId || stored.Kind != action.Kind || stored.Target != action.Target {
			return nil, false, ErrAbuseActionConflict
		}
		return stored, false, nil
	}
	server.RaisePgResult(tx.Exec(
		ctx,
		`
			UPDATE abuse_action SET mailed_time = $2 WHERE abuse_action.action_id = $1
		`,
		action.ActionId,
		now.UTC(),
	))
	server.RaisePgResult(tx.Exec(
		ctx,
		`
			UPDATE abuse_case
			SET review_due_time = LEAST(COALESCE(abuse_case.review_due_time, $2), $2), state = 'open', close_time = NULL
			WHERE abuse_case.case_id = $1
		`,
		action.CaseId,
		now.UTC(),
	))
	return getAbuseActionInTx(ctx, tx, action.ActionId, false), true, nil
}

// Records a mailed lift under its own id, in the caller's transaction: RecordAbuseLiftInTx at the lift's commit
// time, lifted (see the file header), mailed_time set. The lifted action must be the case's, restored or re-applied
// before it.
func RestoreAbuseLiftInTx(ctx context.Context, tx server.PgTx, lift *AbuseRestoredAction, now time.Time, settings *AbuseSettings) (stored *AbuseAction, lifted *AbuseAction, inserted bool, err error) {
	liftedActionId, err := server.ParseId(lift.Target)
	if err != nil {
		return nil, nil, false, fmt.Errorf("%w: a lift's target is no action id", ErrAbuseActionInvalid)
	}
	stored, lifted, _, inserted, err = RecordAbuseLiftInTx(ctx, tx, &AbuseLiftArgs{
		ActionId:       lift.ActionId,
		CaseId:         lift.CaseId,
		LiftedActionId: liftedActionId,
		ReviewState:    AbuseReviewStateLifted,
	}, lift.Time, settings)
	if err != nil {
		return nil, nil, false, err
	}
	if inserted {
		server.RaisePgResult(tx.Exec(
			ctx,
			`UPDATE abuse_action SET mailed_time = $2 WHERE abuse_action.action_id = $1`,
			lift.ActionId,
			now.UTC(),
		))
	}
	return stored, lifted, inserted, nil
}

// A hold to re-place from the legal-request mailbox: its record's terms.
type RestoredLegalHold struct {
	NetworkId  server.Id
	CaseId     server.Id
	Kind       sidecar.HoldKind
	Scope      sidecar.HoldScope
	PlaceTime  time.Time
	ExpireTime time.Time
	// the record was a renewal's
	Renewed bool
}

// Re-places a hold's part 0 with its original terms when the restored rows lack it, or raises a restored row's
// expiry to the record's, in the caller's transaction. Placement's holds_enabled gate does not apply: this restores
// what was placed. placed is false when the row stood with these terms already.
func ReplaceLegalHoldInTx(ctx context.Context, tx server.PgTx, restored *RestoredLegalHold) (hold *LegalHold, placed bool) {
	var renewTime *time.Time
	if restored.Renewed {
		// when the first period ended: the latest the renewal can have come
		renewed := legalHoldTime(restored.PlaceTime.UTC().AddDate(0, 0, LegalHoldPreserveDays))
		renewTime = &renewed
	}
	existing := getLegalHoldInTx(ctx, tx, restored.NetworkId, restored.CaseId, true)
	if existing == nil {
		tag := server.RaisePgResult(tx.Exec(
			ctx,
			`
				INSERT INTO abuse_legal_hold (
					network_id,
					case_id,
					part,
					kind,
					scope,
					place_time,
					renew_time,
					expire_time,
					copy_state
				) VALUES ($1, $2, 0, $3, $4::jsonb, $5, $6, $7, $8)
				ON CONFLICT (network_id, case_id, part) DO NOTHING
			`,
			restored.NetworkId,
			restored.CaseId,
			string(restored.Kind),
			legalHoldScopeJson(restored.Scope),
			legalHoldTime(restored.PlaceTime),
			renewTime,
			legalHoldTime(restored.ExpireTime),
			string(LegalHoldCopyStatePending),
		))
		return getLegalHoldInTx(ctx, tx, restored.NetworkId, restored.CaseId, false), tag.RowsAffected() == 1
	}
	if existing.ExpireTime.Before(legalHoldTime(restored.ExpireTime)) {
		// a renewal after the dump: the record's expiry stands
		server.RaisePgResult(tx.Exec(
			ctx,
			`
				UPDATE abuse_legal_hold
				SET expire_time = $3, renew_time = COALESCE(renew_time, $4)
				WHERE network_id = $1 AND case_id = $2 AND part = 0
			`,
			restored.NetworkId,
			restored.CaseId,
			legalHoldTime(restored.ExpireTime),
			renewTime,
		))
		return getLegalHoldInTx(ctx, tx, restored.NetworkId, restored.CaseId, false), true
	}
	return existing, false
}
