package model

// §21.4's actions and lifts (abuse_action, §2.8 api-14). Each row has the id its
// caller chose, and a second insert of that id adds nothing (ON CONFLICT DO
// NOTHING), which is what lets the action pass retry one and §19.10's restore
// re-apply one from the abuse mailbox idempotently (§21.10). A lift is a row of
// its own, kind lift, its target the lifted action's id
// (plan-ABUSE-small-readings #11), and reaches an action only through the case
// that holds both.
//
// Who acted is a staff member's opaque reviewer id, or no one, which is a
// machine. A machine's action holds until a person reviews it, so it starts
// pending and sets the case's review clock (§21.8). A person's own action is
// their review, so it starts upheld, with no clock. No row is signed: a server
// originates only denials (I-4).
//
// applied_time and mailed_time record the two steps after commit, the effect and
// §21.10's off-site record, so a crash between commit and either leaves the row
// owed for the action pass (controller/abuse_action_controller.go).
//
// Nothing here reads or writes the table unless the binary links MG5
// (server.SidecarMg5Linked).

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/urnetwork/server"
)

// The width of abuse_action's reason column.
const abuseReviewReasonMaxLength = 2048

// Arguments no row may hold. The message names no target (I-13).
var ErrAbuseActionInvalid = errors.New("abuse action invalid")

// The action id already names a row that is not this action, of another case,
// kind or target.
var ErrAbuseActionConflict = errors.New("abuse action id conflict")

// No action of the case has the id.
var ErrAbuseActionNotFound = errors.New("abuse action not found")

// The action was lifted already, by another lift.
var ErrAbuseActionLifted = errors.New("abuse action already lifted")

// One action to record.
type AbuseActionArgs struct {
	// the caller's, so a retry or a restore records the same row once
	ActionId server.Id
	CaseId   server.Id
	Kind     AbuseActionKind
	// the unit it acts on: a host, label, sending domain or network
	Target string
	// when the action ends by itself; nil when only a lift ends it
	UntilTime *time.Time
	// the staff member who acted; nil is a machine
	ReviewerId   *server.Id
	ReviewReason string
	// when a machine action's review falls due (AbuseReviewDue). Zero takes the
	// short clock, MachineReview from now, since every doubt reviews early.
	ReviewDueTime time.Time
}

// One lift to record.
type AbuseLiftArgs struct {
	// the lift's own id, the caller's as for any action
	ActionId server.Id
	CaseId   server.Id
	// the action lifted, which must be the case's and no lift
	LiftedActionId server.Id
	// AbuseReviewStateLifted, or AbuseReviewStateLiftedWrong when review found
	// the action wrong, which takes it out of every count (readings 4, 5)
	ReviewState AbuseReviewState
	// the staff member who lifted; nil is a machine
	ReviewerId   *server.Id
	ReviewReason string
}

// One action or lift as abuse_action holds it.
type AbuseAction struct {
	ActionId     server.Id
	CaseId       server.Id
	NetworkId    *server.Id
	Category     AbuseCategory
	Kind         AbuseActionKind
	Target       string
	UntilTime    *time.Time
	ByMachine    bool
	ReviewState  AbuseReviewState
	ReviewerId   *server.Id
	ReviewReason string
	ReviewTime   *time.Time
	AppliedTime  *time.Time
	MailedTime   *time.Time
	CreateTime   time.Time
}

// Whether the kind is one a row records: every kind but none and §14.8's
// ladder, which smtpedge runs and reports as halts.
func abuseActionKindRecorded(kind AbuseActionKind) bool {
	switch kind {
	case AbuseActionKindNoticeForward, AbuseActionKindHostKill, AbuseActionKindLabelKill,
		AbuseActionKindCertRevoke, AbuseActionKindZoneSuspend, AbuseActionKindHostSuspend,
		AbuseActionKindNetworkSuspend, AbuseActionKindMailHalt, AbuseActionKindMailLabelHalt:
		return true
	}
	return false
}

// Every column of an action, in scanAbuseAction's order.
const abuseActionColumns = `
	abuse_action.action_id,
	abuse_action.case_id,
	abuse_action.network_id,
	abuse_action.category,
	abuse_action.kind,
	abuse_action.target,
	abuse_action.until_time,
	abuse_action.by_machine,
	abuse_action.review_state,
	abuse_action.reviewer_id,
	COALESCE(abuse_action.review_reason, ''),
	abuse_action.review_time,
	abuse_action.applied_time,
	abuse_action.mailed_time,
	abuse_action.create_time
`

// One row of abuseActionColumns.
func scanAbuseAction(result server.PgResult) *AbuseAction {
	action := &AbuseAction{}
	var category, kind, reviewState string
	server.Raise(result.Scan(
		&action.ActionId,
		&action.CaseId,
		&action.NetworkId,
		&category,
		&kind,
		&action.Target,
		&action.UntilTime,
		&action.ByMachine,
		&reviewState,
		&action.ReviewerId,
		&action.ReviewReason,
		&action.ReviewTime,
		&action.AppliedTime,
		&action.MailedTime,
		&action.CreateTime,
	))
	action.Category = AbuseCategory(category)
	action.Kind = AbuseActionKind(kind)
	action.ReviewState = AbuseReviewState(reviewState)
	return action
}

// The action, read in the caller's transaction and locked to its end when
// forUpdate, or nil.
func getAbuseActionInTx(ctx context.Context, tx server.PgTx, actionId server.Id, forUpdate bool) *AbuseAction {
	sql := `SELECT ` + abuseActionColumns + ` FROM abuse_action WHERE abuse_action.action_id = $1`
	if forUpdate {
		sql += ` FOR UPDATE`
	}
	var action *AbuseAction
	result, err := tx.Query(ctx, sql, actionId)
	server.WithPgResult(result, err, func() {
		if result.Next() {
			action = scanAbuseAction(result)
		}
	})
	return action
}

// The action, or nil, including nil in a binary that does not link MG5.
func GetAbuseAction(ctx context.Context, actionId server.Id) (action *AbuseAction) {
	if !server.SidecarMg5Linked() {
		return nil
	}
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`SELECT `+abuseActionColumns+` FROM abuse_action WHERE abuse_action.action_id = $1`,
			actionId,
		)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				action = scanAbuseAction(result)
			}
		})
	})
	return
}

// The case's actions and lifts, oldest first.
func GetAbuseCaseActions(ctx context.Context, caseId server.Id) (actions []*AbuseAction) {
	actions = []*AbuseAction{}
	if !server.SidecarMg5Linked() {
		return
	}
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
				SELECT `+abuseActionColumns+`
				FROM abuse_action
				WHERE abuse_action.case_id = $1
				ORDER BY abuse_action.create_time, abuse_action.action_id
			`,
			caseId,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				actions = append(actions, scanAbuseAction(result))
			}
		})
	})
	return
}

// Inserts a row unless its id exists, and reads back what the id names.
// inserted is false when the row was there already.
func insertAbuseActionInTx(ctx context.Context, tx server.PgTx, action *AbuseAction) (stored *AbuseAction, inserted bool) {
	tag := server.RaisePgResult(tx.Exec(
		ctx,
		`
			INSERT INTO abuse_action (
				action_id,
				case_id,
				network_id,
				category,
				kind,
				target,
				until_time,
				by_machine,
				review_state,
				reviewer_id,
				review_reason,
				review_time,
				create_time
			) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, NULLIF($11, ''), $12, $13)
			ON CONFLICT (action_id) DO NOTHING
		`,
		action.ActionId,
		action.CaseId,
		action.NetworkId,
		string(action.Category),
		string(action.Kind),
		action.Target,
		action.UntilTime,
		action.ByMachine,
		string(action.ReviewState),
		action.ReviewerId,
		action.ReviewReason,
		action.ReviewTime,
		action.CreateTime,
	))
	return getAbuseActionInTx(ctx, tx, action.ActionId, false), tag.RowsAffected() == 1
}

// Records an action under its case in the caller's transaction, and for a
// machine's sets the case's review clock no later than it was. The case row is
// locked to the end of the transaction, so a case's actions and lifts apply in
// one order. A second call with the same id adds nothing and returns the row
// already there, inserted false, or ErrAbuseActionConflict when that row is
// another action.
func RecordAbuseActionInTx(
	ctx context.Context,
	tx server.PgTx,
	args *AbuseActionArgs,
	now time.Time,
	settings *AbuseSettings,
) (action *AbuseAction, abuseCase *AbuseCase, inserted bool, err error) {
	if !server.SidecarMg5Linked() {
		return nil, nil, false, ErrAbuseUnlinked
	}
	if args.ActionId == (server.Id{}) {
		return nil, nil, false, fmt.Errorf("%w: no action id", ErrAbuseActionInvalid)
	}
	if !abuseActionKindRecorded(args.Kind) {
		return nil, nil, false, fmt.Errorf("%w: kind %q records no row", ErrAbuseActionInvalid, args.Kind)
	}
	if args.Target == "" || abuseTargetMaxLength < len(args.Target) {
		return nil, nil, false, fmt.Errorf("%w: a target of %d bytes", ErrAbuseActionInvalid, len(args.Target))
	}
	if abuseReviewReasonMaxLength < len(args.ReviewReason) {
		return nil, nil, false, fmt.Errorf("%w: a reason of %d bytes", ErrAbuseActionInvalid, len(args.ReviewReason))
	}
	abuseCase = getAbuseCaseInTx(ctx, tx, args.CaseId, true)
	if abuseCase == nil {
		return nil, nil, false, ErrAbuseCaseNotFound
	}

	now = now.UTC()
	row := &AbuseAction{
		ActionId:     args.ActionId,
		CaseId:       args.CaseId,
		NetworkId:    abuseCase.NetworkId,
		Category:     abuseCase.Category,
		Kind:         args.Kind,
		Target:       args.Target,
		UntilTime:    args.UntilTime,
		ByMachine:    args.ReviewerId == nil,
		ReviewState:  AbuseReviewStatePending,
		ReviewerId:   args.ReviewerId,
		ReviewReason: args.ReviewReason,
		CreateTime:   now,
	}
	if !row.ByMachine {
		row.ReviewState = AbuseReviewStateUpheld
		row.ReviewTime = &now
	}
	action, inserted = insertAbuseActionInTx(ctx, tx, row)
	if !inserted {
		if action.CaseId != args.CaseId || action.Kind != args.Kind || action.Target != args.Target {
			return nil, nil, false, ErrAbuseActionConflict
		}
		return action, abuseCase, false, nil
	}

	if row.ByMachine {
		reviewDueTime := args.ReviewDueTime
		if reviewDueTime.IsZero() {
			reviewDueTime = now.Add(settings.MachineReview)
		}
		server.RaisePgResult(tx.Exec(
			ctx,
			`
				UPDATE abuse_case
				SET review_due_time = LEAST(COALESCE(abuse_case.review_due_time, $2), $2)
				WHERE abuse_case.case_id = $1
			`,
			abuseCase.CaseId,
			reviewDueTime.UTC(),
		))
	}
	return action, abuseCase, true, nil
}

// Records a lift under its case in the caller's transaction: the lift's own row,
// and the lifted action's review. Only an action of the same case, not itself a
// lift and not lifted already, may be lifted. Once every action of the case is
// lifted or upheld, the case closes (plan-ABUSE-small-readings #9). A second
// call with the lift's id adds nothing and returns what is there.
func RecordAbuseLiftInTx(
	ctx context.Context,
	tx server.PgTx,
	args *AbuseLiftArgs,
	now time.Time,
) (lift *AbuseAction, lifted *AbuseAction, abuseCase *AbuseCase, inserted bool, err error) {
	if !server.SidecarMg5Linked() {
		return nil, nil, nil, false, ErrAbuseUnlinked
	}
	if args.ActionId == (server.Id{}) {
		return nil, nil, nil, false, fmt.Errorf("%w: no action id", ErrAbuseActionInvalid)
	}
	if args.ReviewState != AbuseReviewStateLifted && args.ReviewState != AbuseReviewStateLiftedWrong {
		return nil, nil, nil, false, fmt.Errorf("%w: a lift reviews to %q", ErrAbuseActionInvalid, args.ReviewState)
	}
	if abuseReviewReasonMaxLength < len(args.ReviewReason) {
		return nil, nil, nil, false, fmt.Errorf("%w: a reason of %d bytes", ErrAbuseActionInvalid, len(args.ReviewReason))
	}
	abuseCase = getAbuseCaseInTx(ctx, tx, args.CaseId, true)
	if abuseCase == nil {
		return nil, nil, nil, false, ErrAbuseCaseNotFound
	}
	lifted = getAbuseActionInTx(ctx, tx, args.LiftedActionId, true)
	if lifted == nil || lifted.CaseId != args.CaseId || lifted.Kind == AbuseActionKindLift {
		return nil, nil, nil, false, ErrAbuseActionNotFound
	}

	now = now.UTC()
	row := &AbuseAction{
		ActionId:     args.ActionId,
		CaseId:       args.CaseId,
		NetworkId:    abuseCase.NetworkId,
		Category:     abuseCase.Category,
		Kind:         AbuseActionKindLift,
		Target:       args.LiftedActionId.String(),
		ByMachine:    args.ReviewerId == nil,
		ReviewState:  AbuseReviewStatePending,
		ReviewerId:   args.ReviewerId,
		ReviewReason: args.ReviewReason,
		CreateTime:   now,
	}
	if !row.ByMachine {
		row.ReviewState = AbuseReviewStateUpheld
		row.ReviewTime = &now
	}
	if existing := getAbuseActionInTx(ctx, tx, args.ActionId, false); existing != nil {
		if existing.CaseId != args.CaseId || existing.Kind != AbuseActionKindLift || existing.Target != row.Target {
			return nil, nil, nil, false, ErrAbuseActionConflict
		}
		return existing, lifted, abuseCase, false, nil
	}
	if lifted.ReviewState == AbuseReviewStateLifted || lifted.ReviewState == AbuseReviewStateLiftedWrong {
		return nil, nil, nil, false, ErrAbuseActionLifted
	}
	lift, inserted = insertAbuseActionInTx(ctx, tx, row)
	if !inserted {
		// another transaction took the id between the read and the insert
		return nil, nil, nil, false, ErrAbuseActionConflict
	}

	server.RaisePgResult(tx.Exec(
		ctx,
		`
			UPDATE abuse_action
			SET
				review_state = $2,
				reviewer_id = $3,
				review_reason = NULLIF($4, ''),
				review_time = $5
			WHERE abuse_action.action_id = $1
		`,
		lifted.ActionId,
		string(args.ReviewState),
		args.ReviewerId,
		args.ReviewReason,
		now,
	))
	lifted = getAbuseActionInTx(ctx, tx, lifted.ActionId, false)

	// the case closes once no action of it waits on review or stands unreviewed
	server.RaisePgResult(tx.Exec(
		ctx,
		`
			UPDATE abuse_case
			SET state = 'closed', close_time = $2
			WHERE
				abuse_case.case_id = $1 AND
				abuse_case.state = 'open' AND
				NOT EXISTS (
					SELECT 1
					FROM abuse_action
					WHERE
						abuse_action.case_id = $1 AND
						abuse_action.kind <> 'lift' AND
						abuse_action.review_state NOT IN ('upheld', 'lifted', 'lifted_wrong')
				)
		`,
		abuseCase.CaseId,
		now,
	))
	abuseCase = getAbuseCaseInTx(ctx, tx, abuseCase.CaseId, false)
	return lift, lifted, abuseCase, true, nil
}

// Marks the action's effect applied, once: a later call keeps the first time.
func SetAbuseActionApplied(ctx context.Context, actionId server.Id, appliedTime time.Time) {
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(
			ctx,
			`
				UPDATE abuse_action
				SET applied_time = $2
				WHERE abuse_action.action_id = $1 AND abuse_action.applied_time IS NULL
			`,
			actionId,
			appliedTime.UTC(),
		))
	})
}

// Marks the action's record mailed, once: a later call keeps the first time.
func SetAbuseActionMailed(ctx context.Context, actionId server.Id, mailedTime time.Time) {
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(
			ctx,
			`
				UPDATE abuse_action
				SET mailed_time = $2
				WHERE abuse_action.action_id = $1 AND abuse_action.mailed_time IS NULL
			`,
			actionId,
			mailedTime.UTC(),
		))
	})
}

// Where a pass over owed actions stands: the last row it took, in create time
// then id order. The zero value starts at the oldest.
type AbuseActionCursor struct {
	CreateTime time.Time
	ActionId   server.Id
}

// Up to limit actions whose effect or record is owed, created before
// createdBefore, past cursor in create time then id order, oldest first, and
// the cursor after them.
func ListOwedAbuseActions(
	ctx context.Context,
	createdBefore time.Time,
	cursor AbuseActionCursor,
	limit int,
) (actions []*AbuseAction, next AbuseActionCursor) {
	actions = []*AbuseAction{}
	next = cursor
	if !server.SidecarMg5Linked() || limit <= 0 {
		return
	}
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
				SELECT `+abuseActionColumns+`
				FROM abuse_action
				WHERE
					(abuse_action.applied_time IS NULL OR abuse_action.mailed_time IS NULL) AND
					abuse_action.create_time < $1 AND
					(abuse_action.create_time, abuse_action.action_id) > ($2, $3)
				ORDER BY abuse_action.create_time, abuse_action.action_id
				LIMIT $4
			`,
			createdBefore.UTC(),
			cursor.CreateTime.UTC(),
			cursor.ActionId,
			limit,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				actions = append(actions, scanAbuseAction(result))
			}
		})
	})
	if 0 < len(actions) {
		last := actions[len(actions)-1]
		next = AbuseActionCursor{CreateTime: last.CreateTime, ActionId: last.ActionId}
	}
	return
}

// The email addresses that get an action's notice: one for each Owner and
// Admin of the network. Until TEAM-M6-02 names Owners, that is the network's
// admin (plan-ABUSE-small-readings #19), at its login when that is an email,
// else its first verified email sign-in, else an Apple or Google sign-in's
// email. A phone number receives no notice, since only account security goes
// by SMS, and an admin with no email has none.
func AbuseNoticeRecipientsInTx(ctx context.Context, tx server.PgTx, networkId server.Id) []string {
	userAuths := []string{}
	result, err := tx.Query(
		ctx,
		`
			SELECT network_user.user_auth, 1 AS rank
			FROM network
			INNER JOIN network_user ON network_user.user_id = network.admin_user_id
			WHERE network.network_id = $1 AND network_user.user_auth IS NOT NULL
			UNION ALL
			SELECT network_user_auth_password.user_auth, 2 AS rank
			FROM network
			INNER JOIN network_user_auth_password ON network_user_auth_password.user_id = network.admin_user_id
			WHERE
				network.network_id = $1 AND
				network_user_auth_password.auth_type = 'email' AND
				network_user_auth_password.verified
			UNION ALL
			SELECT network_user_auth_sso.user_auth, 3 AS rank
			FROM network
			INNER JOIN network_user_auth_sso ON network_user_auth_sso.user_id = network.admin_user_id
			WHERE network.network_id = $1
			ORDER BY rank, user_auth
		`,
		networkId,
	)
	server.WithPgResult(result, err, func() {
		for result.Next() {
			var userAuth string
			var rank int
			server.Raise(result.Scan(&userAuth, &rank))
			userAuths = append(userAuths, userAuth)
		}
	})
	for _, userAuth := range userAuths {
		if normalUserAuth, userAuthType := NormalUserAuth(userAuth); userAuthType == UserAuthTypeEmail && normalUserAuth != "" {
			// one Owner or Admin today, so one address
			return []string{normalUserAuth}
		}
	}
	return []string{}
}
