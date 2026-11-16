// Pending validation is read-only and has no operator import or source/reward
// capability. The native adapter supplies independently authenticated readers.
package main

import (
	"context"
	"errors"
	"reflect"

	"github.com/urnetwork/server/controller"
	"github.com/urnetwork/server/model"
)

type winnerHandoffPendingAuthority struct {
	Read            func(context.Context) (*model.CompetitionReviewHandoff, []model.CompetitionReviewHandoffCall, *model.CompetitionReviewHandoffCompletion, error)
	State           func(context.Context) (*controller.CompetitionWinnerReviewState, error)
	Provenance      func(context.Context) (winnerHandoffProvenance, error)
	ReloadProtected func(context.Context, []model.CompetitionReviewHandoffCall) error
}

// The exact durable pending record, current decision, both original model
// findings and retrievable protected bytes are all independently necessary.
func validateWinnerHandoffPendingEvidence(ctx context.Context, owner *model.CompetitionReviewHandoff, plan *winnerMultipassPlan, bundle winnerMultipassBundle, evidence winnerMultipassEvidence, custody winnerHandoffCustodyManifest, completion *model.CompetitionReviewHandoffCompletion, authority winnerHandoffPendingAuthority) error {
	if authority.Read == nil || authority.State == nil || authority.Provenance == nil || authority.ReloadProtected == nil {
		return errors.New("pending validation lacks independent durable/protected readers")
	}
	if err := validateWinnerHandoffCompletion(owner, completion, plan, bundle, evidence, custody); err != nil {
		return err
	}
	actual, calls, terminal, err := authority.Read(ctx)
	if err != nil || !reflect.DeepEqual(actual, owner) || !reflect.DeepEqual(terminal, completion) {
		return errors.New("pending findings differ from the exact current durable handoff")
	}
	if err := validateWinnerHandoffJournal(owner, plan, evidence, calls); err != nil {
		return err
	}
	state, err := authority.State(ctx)
	if err != nil || state == nil || state.CurrentDecision.DecisionSha256 != owner.Args.DecisionSha256 || state.CurrentDecision.Revision != owner.Args.RewardRevision {
		return errors.New("pending findings belong to a superseded elected decision")
	}
	provenance, err := authority.Provenance(ctx)
	if err != nil || provenance != custody.Provenance {
		return errors.New("pending findings changed their qualified release or exact capture admission")
	}
	return authority.ReloadProtected(ctx, calls)
}
