package model

// §21.8's appeals, and a network's own view of the cases and actions it was told of (ABUSE-M5-11; D-ABUSE-5,
// R-ABUSE-6, I-6, I-13, D-TEAM-14): what GET /abuse/cases, POST /abuse/appeal and GET /abuse/status read and write
// for the caller's own network. These are customer reads, never the abuse role's: each takes the caller's network
// and reads nothing of another, so none writes abuse_access_log, which holds the role's reads alone
// (abuse_access_model.go).
//
// A network sees a case once it was told of it: a case with at least one action, whose notice went to its Owners and
// Admins as the action committed (§21.8; controller.ApplyAbuseAction). A case with no action, a report still
// waiting for review, is no case to the network yet, so a pending report never tips off the network it names. A
// barred case, under a sealing order or a CSAM case, shows the network its standing and nothing else
// (plan-ABUSE-small-readings #6): ListNetworkAbuseCases leaves it out, CreateAbuseAppeal answers
// ErrAbuseCaseNotFound for it exactly as for an unknown id or another network's case, from one query that matches
// none of the three, and GetNetworkAbuseStatus lists none of its actions.
//
// Nothing here reads abuse_legal_hold, legal_request or anything a hold writes, so no field, refusal or timing
// branch of these reads differs for a held network (D-ABUSE-8).
//
// An appeal's reason is the one free text kept, and it is the appellant's own (§21.8). An appeal may be filed while
// the case's newest notice is inside abuse.yml's appeal_window (plan-ABUSE-small-readings #9; DSA Art. 20). One
// waits at a time: a second filing answers the first. A decided appeal takes another only for an action newer than
// its decision. A filed appeal reopens a closed case, which closes again at its decision (DecideAbuseAppeal in
// abuse_review_model.go).
//
// Nothing here reads or writes the tables unless the binary links MG5 (server.SidecarMg5Linked): without it a
// network has no case and no action, and its status is its standing alone.

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/urnetwork/sidecar"

	"github.com/urnetwork/server"
)

// An appeal's state, as abuse_appeal stores it.
type AbuseAppealState string

const (
	// filed, waiting for a second reviewer
	AbuseAppealStatePending AbuseAppealState = "pending"
	// a second reviewer found the case's actions wrong and lifted them
	AbuseAppealStateGranted AbuseAppealState = "granted"
	// a second reviewer upheld the case's actions
	AbuseAppealStateDenied AbuseAppealState = "denied"
)

// Whether this build knows the state.
func (self AbuseAppealState) known() bool {
	switch self {
	case AbuseAppealStatePending, AbuseAppealStateGranted, AbuseAppealStateDenied:
		return true
	}
	return false
}

// An appeal no row may hold: no reason, one past AppealReasonMaxByteCount, or text Postgres cannot keep. The
// message names nothing of the reason.
var ErrAbuseAppealInvalid = errors.New("abuse appeal invalid")

// The case's newest notice is past abuse.yml's appeal_window.
var ErrAbuseAppealWindowClosed = errors.New("abuse appeal window closed")

// The case's appeal was decided, and no action came after the decision.
var ErrAbuseAppealDecided = errors.New("abuse appeal decided")

// No appeal has the id.
var ErrAbuseAppealNotFound = errors.New("abuse appeal not found")

// Tunables of the customer routes' reads and writes.
type AbuseCustomerSettings struct {
	// The most bytes of an appeal's reason: 2048, abuse_appeal.reason's width, so a longer one is refused rather
	// than cut.
	AppealReasonMaxByteCount int
	// How long after a case's newest notice an appeal may be filed, in calendar months: abuse.yml's appeal_window
	// (ApplyAbuseCustomerConfig), which the config reader never takes under DSA Art. 20's six
	// (server.AbuseAppealWindowMinMonths), the default. Non-positive refuses every appeal.
	AppealWindowMonths int
	// The appeal link a case carries: abuse.yml's appeal_url with the case id, "" when the file names none.
	AppealUrl string
	// The most cases GET /abuse/cases lists, newest first: 100, years of a busy network's history. Non-positive
	// lists none.
	CaseListLimit int
	// The most actions one listed case shows, oldest first: 64, past any ladder's rungs. Non-positive shows none.
	CaseActionLimit int
	// The most actions GET /abuse/status lists: the newest 100, oldest first among them, so a new action never
	// hides behind old ones. Non-positive lists none.
	StatusActionLimit int
}

// §21's numbers, with no appeal link until abuse.yml names one.
func DefaultAbuseCustomerSettings() *AbuseCustomerSettings {
	return &AbuseCustomerSettings{
		AppealReasonMaxByteCount: 2048,
		AppealWindowMonths:       server.AbuseAppealWindowMinMonths,
		CaseListLimit:            100,
		CaseActionLimit:          64,
		StatusActionLimit:        100,
	}
}

// Lays abuse.yml's appeal window and appeal link over settings.
func ApplyAbuseCustomerConfig(settings *AbuseCustomerSettings, config *server.AbuseConfig) {
	settings.AppealWindowMonths = config.AppealWindowMonths()
	settings.AppealUrl = config.AppealUrl()
}

// The appeal link of a case: appealUrl with the case id, an opaque id (I-13), or "" when appealUrl is.
func AbuseCaseAppealUrl(appealUrl string, caseId server.Id) string {
	if appealUrl == "" {
		return ""
	}
	separator := "?"
	if strings.Contains(appealUrl, "?") {
		separator = "&"
	}
	return appealUrl + separator + "case_id=" + caseId.String()
}

// One appeal as abuse_appeal holds it.
type AbuseAppeal struct {
	AppealId  server.Id
	CaseId    server.Id
	NetworkId server.Id
	// the Owner or Admin who filed, an opaque id
	UserId server.Id
	// the appellant's own words
	Reason string
	State  AbuseAppealState
	// the second reviewer, nil while pending
	ReviewerId     *server.Id
	DecisionReason string
	CreateTime     time.Time
	DecideTime     *time.Time
}

// Every column of an appeal, in scanAbuseAppeal's order.
const abuseAppealColumns = `
	abuse_appeal.appeal_id,
	abuse_appeal.case_id,
	abuse_appeal.network_id,
	abuse_appeal.user_id,
	abuse_appeal.reason,
	abuse_appeal.state,
	abuse_appeal.reviewer_id,
	COALESCE(abuse_appeal.decision_reason, ''),
	abuse_appeal.create_time,
	abuse_appeal.decide_time
`

// One row of abuseAppealColumns.
func scanAbuseAppeal(result server.PgResult) *AbuseAppeal {
	appeal := &AbuseAppeal{}
	var state string
	server.Raise(result.Scan(
		&appeal.AppealId,
		&appeal.CaseId,
		&appeal.NetworkId,
		&appeal.UserId,
		&appeal.Reason,
		&state,
		&appeal.ReviewerId,
		&appeal.DecisionReason,
		&appeal.CreateTime,
		&appeal.DecideTime,
	))
	appeal.State = AbuseAppealState(state)
	return appeal
}

// The case's newest appeal in the caller's transaction, or nil.
func getNewestAbuseAppealInTx(ctx context.Context, tx server.PgTx, caseId server.Id) *AbuseAppeal {
	var appeal *AbuseAppeal
	result, err := tx.Query(
		ctx,
		`
			SELECT `+abuseAppealColumns+`
			FROM abuse_appeal
			WHERE abuse_appeal.case_id = $1
			ORDER BY abuse_appeal.create_time DESC, abuse_appeal.appeal_id DESC
			LIMIT 1
		`,
		caseId,
	)
	server.WithPgResult(result, err, func() {
		if result.Next() {
			appeal = scanAbuseAppeal(result)
		}
	})
	return appeal
}

// When the case's newest notice went out: its newest action's creation, a lift's excluded, since a lift is no
// decision against the network. nil for a case with no action, which the network was never told of.
func getAbuseCaseNoticeTimeInTx(ctx context.Context, tx server.PgTx, caseId server.Id) *time.Time {
	var noticeTime *time.Time
	server.Raise(tx.QueryRow(
		ctx,
		`
			SELECT MAX(abuse_action.create_time)
			FROM abuse_action
			WHERE abuse_action.case_id = $1 AND abuse_action.kind <> $2
		`,
		caseId,
		string(AbuseActionKindLift),
	).Scan(&noticeTime))
	return noticeTime
}

// POST /abuse/appeal's body.
type AbuseAppealArgs struct {
	// the case the network was told of, by its notice or GET /abuse/cases
	CaseId server.Id `json:"case_id"`
	// the appellant's own words: why the action is wrong
	Reason string `json:"reason"`
}

// POST /abuse/appeal's answer: the appeal filed, or the one already waiting.
type AbuseAppealResult struct {
	AppealId     server.Id        `json:"appeal_id"`
	CaseId       server.Id        `json:"case_id"`
	State        AbuseAppealState `json:"state"`
	CreateTimeMs int64            `json:"create_time_ms"`
}

// The answer an appeal gives its appellant.
func (self *AbuseAppeal) Result() *AbuseAppealResult {
	return &AbuseAppealResult{
		AppealId:     self.AppealId,
		CaseId:       self.CaseId,
		State:        self.State,
		CreateTimeMs: self.CreateTime.UnixMilli(),
	}
}

// Refuses a reason no row may hold: none, one past the settings' bound, invalid UTF-8, or a NUL, which Postgres
// text cannot keep.
func validateAbuseAppealReason(reason string, maxByteCount int) error {
	switch {
	case strings.TrimSpace(reason) == "":
		return fmt.Errorf("%w: no reason", ErrAbuseAppealInvalid)
	case maxByteCount < len(reason):
		return fmt.Errorf("%w: a reason of %d bytes", ErrAbuseAppealInvalid, len(reason))
	case !utf8.ValidString(reason) || strings.ContainsRune(reason, 0):
		return fmt.Errorf("%w: a reason that is not text", ErrAbuseAppealInvalid)
	}
	return nil
}

// Files an Owner's or Admin's appeal of a case of networkId at now, the rules in the file header. created is false
// when an appeal of the case was already waiting, which is then the answer, so a retry files nothing twice. A case
// that is unknown, another network's, barred or never noticed answers ErrAbuseCaseNotFound, alike; a binary that
// does not link MG5 answers ErrAbuseUnlinked.
func CreateAbuseAppeal(
	ctx context.Context,
	networkId server.Id,
	userId server.Id,
	args *AbuseAppealArgs,
	now time.Time,
	settings *AbuseCustomerSettings,
) (appeal *AbuseAppeal, created bool, returnErr error) {
	if !server.SidecarMg5Linked() {
		return nil, false, ErrAbuseUnlinked
	}
	if err := validateAbuseAppealReason(args.Reason, settings.AppealReasonMaxByteCount); err != nil {
		return nil, false, err
	}
	now = now.UTC()
	server.Tx(ctx, func(tx server.PgTx) {
		appeal = nil
		created = false
		returnErr = nil

		// one statement matches neither an unknown case, another network's nor a barred one, and locks only a row
		// the caller may appeal
		var state string
		found := false
		result, err := tx.Query(
			ctx,
			`
				SELECT abuse_case.state
				FROM abuse_case
				WHERE
					abuse_case.case_id = $1 AND
					abuse_case.network_id = $2 AND
					NOT abuse_case.barred
				FOR UPDATE
			`,
			args.CaseId,
			networkId,
		)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				server.Raise(result.Scan(&state))
				found = true
			}
		})
		if !found {
			returnErr = ErrAbuseCaseNotFound
			return
		}
		noticeTime := getAbuseCaseNoticeTimeInTx(ctx, tx, args.CaseId)
		if noticeTime == nil {
			returnErr = ErrAbuseCaseNotFound
			return
		}
		if !now.Before(noticeTime.UTC().AddDate(0, settings.AppealWindowMonths, 0)) {
			returnErr = ErrAbuseAppealWindowClosed
			return
		}
		if newest := getNewestAbuseAppealInTx(ctx, tx, args.CaseId); newest != nil {
			if newest.State == AbuseAppealStatePending {
				appeal = newest
				return
			}
			if newest.DecideTime == nil || !noticeTime.After(*newest.DecideTime) {
				returnErr = ErrAbuseAppealDecided
				return
			}
		}

		appeal = &AbuseAppeal{
			AppealId:   server.NewId(),
			CaseId:     args.CaseId,
			NetworkId:  networkId,
			UserId:     userId,
			Reason:     args.Reason,
			State:      AbuseAppealStatePending,
			CreateTime: now,
		}
		server.RaisePgResult(tx.Exec(
			ctx,
			`
				INSERT INTO abuse_appeal (
					appeal_id,
					case_id,
					network_id,
					user_id,
					reason,
					state,
					create_time
				) VALUES ($1, $2, $3, $4, $5, $6, $7)
			`,
			appeal.AppealId,
			appeal.CaseId,
			appeal.NetworkId,
			appeal.UserId,
			appeal.Reason,
			string(appeal.State),
			appeal.CreateTime,
		))
		if AbuseCaseState(state) == AbuseCaseStateClosed {
			// a closed case's actions were all reviewed, so its clock is spent: cleared, so the reopened case
			// raises no overdue review it does not have
			server.RaisePgResult(tx.Exec(
				ctx,
				`
					UPDATE abuse_case
					SET state = 'open', close_time = NULL, review_due_time = NULL
					WHERE abuse_case.case_id = $1
				`,
				args.CaseId,
			))
		}
		created = true
	})
	return
}

// GET /abuse/cases's body: the network's cases it was told of, newest first.
type AbuseNetworkCasesResult struct {
	Cases []*AbuseNetworkCase `json:"cases"`
}

// One case as its network sees it: what its notice said, its actions and its newest appeal. Never a reporter, a
// join key, a reviewer or a review's reason, which stay with the abuse role.
type AbuseNetworkCase struct {
	CaseId server.Id `json:"case_id"`
	// §21.4's row
	Category AbuseCategory `json:"category"`
	// host, sending_domain, label or network
	TargetKind AbuseTargetKind `json:"target_kind"`
	Target     string          `json:"target"`
	// open or closed
	State        AbuseCaseState `json:"state"`
	CreateTimeMs int64          `json:"create_time_ms"`
	// 0 while open
	CloseTimeMs int64 `json:"close_time_ms"`
	// lifts excluded: a lifted action shows its review state
	Actions []*AbuseNetworkCaseAction `json:"actions"`
	// abuse.yml's appeal link with the case id, "" when the file names none
	AppealUrl string `json:"appeal_url"`
	// the case's newest appeal, absent for none
	Appeal *AbuseNetworkCaseAppeal `json:"appeal,omitempty"`
}

// One action of a listed case.
type AbuseNetworkCaseAction struct {
	ActionId server.Id       `json:"action_id"`
	Kind     AbuseActionKind `json:"kind"`
	// the unit it acts on: a host, a label, a sending domain or the network
	Target string `json:"target"`
	// pending, upheld, lifted or lifted_wrong
	ReviewState  AbuseReviewState `json:"review_state"`
	CreateTimeMs int64            `json:"create_time_ms"`
	// 0: only a lift ends it
	UntilTimeMs int64 `json:"until_time_ms"`
}

// A listed case's newest appeal.
type AbuseNetworkCaseAppeal struct {
	AppealId     server.Id        `json:"appeal_id"`
	State        AbuseAppealState `json:"state"`
	CreateTimeMs int64            `json:"create_time_ms"`
	// 0 while pending
	DecideTimeMs int64 `json:"decide_time_ms"`
	// the second reviewer's reasons, which the appellant is told (DSA Art. 20(5)); "" while pending
	DecisionReason string `json:"decision_reason"`
}

// Unix milliseconds of a time that may be absent, 0 for none.
func abuseOptionalTimeMs(t *time.Time) int64 {
	if t == nil {
		return 0
	}
	return t.UnixMilli()
}

// The cases networkId was told of, newest first, up to CaseListLimit, each with its actions and newest appeal; a
// barred case and a case with no action are left out (the file header). Empty in a binary that does not link MG5.
func ListNetworkAbuseCases(ctx context.Context, networkId server.Id, settings *AbuseCustomerSettings) *AbuseNetworkCasesResult {
	casesResult := &AbuseNetworkCasesResult{Cases: []*AbuseNetworkCase{}}
	if !server.SidecarMg5Linked() || settings.CaseListLimit <= 0 {
		return casesResult
	}
	server.Tx(ctx, func(tx server.PgTx) {
		casesResult.Cases = []*AbuseNetworkCase{}
		caseIdCases := map[server.Id]*AbuseNetworkCase{}
		caseIds := []server.Id{}
		// driven by the network's actions, whose index names the network, since abuse_case holds one only for its
		// open rows
		result, err := tx.Query(
			ctx,
			`
				SELECT `+abuseCaseColumns+`
				FROM abuse_case
				WHERE
					abuse_case.case_id IN (
						SELECT abuse_action.case_id
						FROM abuse_action
						WHERE abuse_action.network_id = $1 AND abuse_action.kind <> $2
					) AND
					abuse_case.network_id = $1 AND
					NOT abuse_case.barred
				ORDER BY abuse_case.create_time DESC, abuse_case.case_id DESC
				LIMIT $3
			`,
			networkId,
			string(AbuseActionKindLift),
			settings.CaseListLimit,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				abuseCase := scanAbuseCase(result)
				networkCase := &AbuseNetworkCase{
					CaseId:       abuseCase.CaseId,
					Category:     abuseCase.Category,
					TargetKind:   abuseCase.TargetKind,
					Target:       abuseCase.Target,
					State:        abuseCase.State,
					CreateTimeMs: abuseCase.CreateTime.UnixMilli(),
					CloseTimeMs:  abuseOptionalTimeMs(abuseCase.CloseTime),
					Actions:      []*AbuseNetworkCaseAction{},
					AppealUrl:    AbuseCaseAppealUrl(settings.AppealUrl, abuseCase.CaseId),
				}
				casesResult.Cases = append(casesResult.Cases, networkCase)
				caseIdCases[abuseCase.CaseId] = networkCase
				caseIds = append(caseIds, abuseCase.CaseId)
			}
		})
		if len(caseIds) == 0 {
			return
		}

		result, err = tx.Query(
			ctx,
			`
				SELECT `+abuseActionColumns+`
				FROM abuse_action
				WHERE abuse_action.case_id = ANY($1::uuid[]) AND abuse_action.kind <> $2
				ORDER BY abuse_action.case_id, abuse_action.create_time, abuse_action.action_id
			`,
			caseIds,
			string(AbuseActionKindLift),
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				action := scanAbuseAction(result)
				networkCase := caseIdCases[action.CaseId]
				if networkCase == nil || settings.CaseActionLimit <= len(networkCase.Actions) {
					continue
				}
				networkCase.Actions = append(networkCase.Actions, &AbuseNetworkCaseAction{
					ActionId:     action.ActionId,
					Kind:         action.Kind,
					Target:       action.Target,
					ReviewState:  action.ReviewState,
					CreateTimeMs: action.CreateTime.UnixMilli(),
					UntilTimeMs:  abuseOptionalTimeMs(action.UntilTime),
				})
			}
		})

		result, err = tx.Query(
			ctx,
			`
				SELECT DISTINCT ON (abuse_appeal.case_id) `+abuseAppealColumns+`
				FROM abuse_appeal
				WHERE abuse_appeal.case_id = ANY($1::uuid[])
				ORDER BY abuse_appeal.case_id, abuse_appeal.create_time DESC, abuse_appeal.appeal_id DESC
			`,
			caseIds,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				appeal := scanAbuseAppeal(result)
				networkCase := caseIdCases[appeal.CaseId]
				if networkCase == nil || !appeal.State.known() {
					continue
				}
				networkCase.Appeal = &AbuseNetworkCaseAppeal{
					AppealId:       appeal.AppealId,
					State:          appeal.State,
					CreateTimeMs:   appeal.CreateTime.UnixMilli(),
					DecideTimeMs:   abuseOptionalTimeMs(appeal.DecideTime),
					DecisionReason: appeal.DecisionReason,
				}
			}
		})
	})
	return casesResult
}

// GET /abuse/status's body for networkId at now (sidecar.AbuseStatus): its §21.7 standing, its network_verified
// and its actions that stand, the newest StatusActionLimit oldest first, a barred case's left out, so a barred case
// shows the network its standing alone. found is false for a network that does not exist.
func GetNetworkAbuseStatus(
	ctx context.Context,
	networkId server.Id,
	now time.Time,
	settings *AbuseCustomerSettings,
) (status *sidecar.AbuseStatus, found bool) {
	standing, found := GetNetworkAbuseStanding(ctx, networkId, now)
	if !found {
		return nil, false
	}
	status = &sidecar.AbuseStatus{
		Standing: standing.Standing,
		Verified: IsNetworkVerified(ctx, networkId),
		Actions:  []*sidecar.AbuseStatusAction{},
	}
	if !server.SidecarMg5Linked() || settings.StatusActionLimit <= 0 {
		return status, true
	}
	server.Db(ctx, func(conn server.PgConn) {
		status.Actions = []*sidecar.AbuseStatusAction{}
		result, err := conn.Query(
			ctx,
			`
				SELECT `+abuseActionColumns+`
				FROM abuse_action
				INNER JOIN abuse_case ON abuse_case.case_id = abuse_action.case_id
				WHERE
					abuse_action.network_id = $1 AND
					abuse_action.kind NOT IN ($2, $7) AND
					abuse_action.review_state NOT IN ($3, $4) AND
					(abuse_action.until_time IS NULL OR $5 < abuse_action.until_time) AND
					NOT abuse_case.barred
				ORDER BY abuse_action.create_time DESC, abuse_action.action_id DESC
				LIMIT $6
			`,
			networkId,
			string(AbuseActionKindLift),
			string(AbuseReviewStateLifted),
			string(AbuseReviewStateLiftedWrong),
			now.UTC(),
			settings.StatusActionLimit,
			// a reviewer's allow of a name withholds nothing, so the status lists no such action
			string(AbuseActionKindScreenAllow),
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				action := scanAbuseAction(result)
				status.Actions = append(status.Actions, &sidecar.AbuseStatusAction{
					ActionId:     sidecar.Id(action.ActionId),
					CaseId:       sidecar.Id(action.CaseId),
					Category:     string(action.Category),
					Kind:         string(action.Kind),
					Target:       action.Target,
					ReviewState:  string(action.ReviewState),
					CreateTimeMs: action.CreateTime.UnixMilli(),
					UntilTimeMs:  abuseOptionalTimeMs(action.UntilTime),
				})
			}
		})
		// read newest first under the bound, answered oldest first
		slices.Reverse(status.Actions)
	})
	return status, true
}
