package controller

// §21.7's tightening as the staff route and the effects run it (ABUSE-M5-19; D-ABUSE-1, D-QUOTA-7, FS-47). A
// tightening is an action of kind limit_tighten on §21's one path (abuse_action_controller.go): recorded under its
// case, its notice owed unless the case is barred, its effect writing the network_sidecar_limit row that cites the
// case (model.SetAbuseLimitRow), its record mailed. A lift through the case, by LiftNetworkLimit or the staff
// route's lift and review kinds alike, runs the lift's effect, which clears the row while it still cites the case.
// model.RecordAbuseActionInTx refuses a tightening ValidateAbuseLimitTighten refuses before anything is recorded:
// a key §21 does not tighten, a fixed key, a value under the floor.
//
// POST /abuse/action's limit_tighten kind is the reviewer's: a tightening follows review, never a machine (§21.7,
// plan-ABUSE-small-readings #17).

import (
	"context"
	"fmt"
	"strings"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// Tightens one of the network's limits under a case for a reviewer: the key and value pass
// model.ValidateAbuseLimitTighten, then the action rides ApplyAbuseAction, whose effect writes the row. The case
// must name its network.
func TightenNetworkLimit(
	ctx context.Context,
	reviewerId server.Id,
	caseId server.Id,
	actionId server.Id,
	limitKey string,
	limitValue int64,
	reason string,
) (*AbuseActionResult, error) {
	if err := model.ValidateAbuseLimitTighten(limitKey, limitValue, caseId); err != nil {
		return nil, err
	}
	return ApplyAbuseAction(ctx, &model.AbuseActionArgs{
		ActionId:     actionId,
		CaseId:       caseId,
		Kind:         model.AbuseActionKindLimitTighten,
		Target:       model.AbuseLimitTightenTarget(limitKey, limitValue),
		ReviewerId:   &reviewerId,
		ReviewReason: reason,
	})
}

// Lifts a tightening through its case for a reviewer: LiftAbuseAction with the lift's own id, whose effect clears
// the row. wrong takes the tightening out of every count.
func LiftNetworkLimit(
	ctx context.Context,
	reviewerId server.Id,
	caseId server.Id,
	tightenActionId server.Id,
	liftId server.Id,
	wrong bool,
	reason string,
) (*AbuseActionResult, error) {
	tighten := model.GetAbuseAction(ctx, tightenActionId)
	if tighten == nil || tighten.CaseId != caseId || tighten.Kind != model.AbuseActionKindLimitTighten {
		return nil, model.ErrAbuseActionNotFound
	}
	reviewState := model.AbuseReviewStateLifted
	if wrong {
		reviewState = model.AbuseReviewStateLiftedWrong
	}
	return LiftAbuseAction(ctx, &model.AbuseLiftArgs{
		ActionId:       liftId,
		CaseId:         caseId,
		LiftedActionId: tightenActionId,
		ReviewState:    reviewState,
		ReviewerId:     &reviewerId,
		ReviewReason:   reason,
	})
}

// The effect of a limit_tighten action: its row. Idempotent, as every effect is.
func applyAbuseLimitTightenEffect(ctx context.Context, action *model.AbuseAction) error {
	if action.NetworkId == nil {
		return fmt.Errorf("%w: a tightening with no network", ErrAbuseEffectTarget)
	}
	return model.SetAbuseLimitRow(ctx, *action.NetworkId, action.CaseId, action.Target)
}

// The effect of a tightening's lift: its row goes, while it still cites the case.
func liftAbuseLimitTightenEffect(ctx context.Context, lifted *model.AbuseAction) error {
	if lifted.NetworkId == nil {
		return fmt.Errorf("%w: a tightening with no network", ErrAbuseEffectTarget)
	}
	_, err := model.ClearAbuseLimitRow(ctx, *lifted.NetworkId, lifted.CaseId, lifted.Target)
	return err
}

// limit_tighten's body.
type abuseStaffLimitTightenArgs struct {
	Kind   string    `json:"kind"`
	CaseId server.Id `json:"case_id"`
	// the caller's id for the action, so a retry records it once; absent draws one
	ActionId   *server.Id `json:"action_id"`
	LimitKey   string     `json:"limit_key"`
	LimitValue int64      `json:"limit_value"`
	Reason     string     `json:"reason"`
}

func init() {
	RegisterAbuseStaffKind(&AbuseStaffKind{
		Name:  "limit_tighten",
		Roles: []AbuseStaffRole{AbuseStaffRoleReviewer},
		Run: func(ctx context.Context, staff *AbuseStaff, body []byte) (any, error) {
			args := &abuseStaffLimitTightenArgs{}
			if err := decodeAbuseStaffArgs(body, args); err != nil {
				return nil, err
			}
			if err := validateAbuseStaffReason(args.Reason, abuseStaffDeps.settings()); err != nil {
				return nil, err
			}
			if strings.TrimSpace(args.LimitKey) == "" {
				return nil, fmt.Errorf("%w: no limit key", ErrAbuseStaffRequestInvalid)
			}
			actionId := server.NewId()
			if args.ActionId != nil {
				actionId = *args.ActionId
			}
			result, err := TightenNetworkLimit(ctx, staff.ReviewerId, args.CaseId, actionId, args.LimitKey, args.LimitValue, args.Reason)
			if err != nil {
				return nil, err
			}
			return abuseStaffActionResultOf(result), nil
		},
	})
}
