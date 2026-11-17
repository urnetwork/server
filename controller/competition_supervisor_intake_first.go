// An explicitly approved source-only lifecycle can open intake before grading
// preparation. Historical drivers and frozen rounds retain their old contract.
package controller

import (
	"context"
	"errors"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// Optional capability avoids silently teaching an old/mock driver that API
// publication means qualified execution. Concrete runtime integration is real.
type competitionSupervisorIntakeFirstDriver interface {
	IntakeFirst(*model.CompetitionSourceActivation) bool
	PrepareIntake(context.Context, model.CompetitionSupervisorLease, *model.CompetitionSourceActivation) error
	OpenIntake(context.Context, model.CompetitionSupervisorLease, *model.CompetitionSourceActivation) (*model.CompetitionRoundResult, error)
}

type competitionSupervisorRoundGradingDriver interface {
	PrepareRoundGrading(context.Context, model.CompetitionSupervisorLease, model.CompetitionRoundResult) error
}

func (self *PostgresCompetitionSupervisorDriver) IntakeFirst(intent *model.CompetitionSourceActivation) bool {
	return competitionIntakeFirstApplies(self.Settings, intent)
}

// An unconfigured adapter cannot manufacture readiness. One genuine recorded
// API acknowledgment is sufficient only for the separately fenced intake grant.
func (self *PostgresCompetitionSupervisorDriver) PrepareIntake(ctx context.Context, lease model.CompetitionSupervisorLease, intent *model.CompetitionSourceActivation) error {
	if !self.IntakeFirst(intent) || self.Hooks.PrepareIntake == nil {
		return ErrReviewNotReady
	}
	if err := self.Hooks.PrepareIntake(ctx, lease, intent); err != nil {
		return err
	}
	api, err := self.Store.sourcePreparationReceipts(ctx, self.Settings, intent.Generation)
	if err != nil {
		return err
	}
	matched := false
	for _, receipt := range api {
		if receipt.Role == "api" {
			if matched {
				return ErrConflict
			}
			if _, err := competitionIntakeFirstApiReceiptDigest(intent, receipt); err != nil {
				return err
			}
			matched = true
		}
	}
	if !matched {
		return ErrReviewNotReady
	}
	return nil
}

// The grant and actual opening are separately resumable. Neither starts a job
// clock, advances the grading source pointer or records worker acknowledgment.
func (self *PostgresCompetitionSupervisorDriver) OpenIntake(ctx context.Context, lease model.CompetitionSupervisorLease, intent *model.CompetitionSourceActivation) (*model.CompetitionRoundResult, error) {
	if !self.IntakeFirst(intent) {
		return nil, ErrConflict
	}
	if _, err := self.Store.ActivateSourceIntake(ctx, self.Settings, lease, intent.Generation); err != nil {
		return nil, err
	}
	return self.Store.CreateIntakeFirstRound(ctx, self.Settings, lease, intent.Generation)
}

// This is runtime activation for trusted control preparation, not permission
// to grade candidates. The latter is independently gated by the immutable R9
// baseline and all configured host admission receipts at the SQL claim edge.
func (self *PostgresCompetitionSupervisorDriver) PrepareRoundGrading(ctx context.Context, lease model.CompetitionSupervisorLease, round model.CompetitionRoundResult) error {
	if self.Settings.IntakeFirstPolicy == nil || round.Staging || round.Epoch < self.Settings.IntakeFirstPolicy.FromEpoch {
		return nil
	}
	stored, err := self.Store.GetRound(ctx, self.Settings, round.RoundId)
	if err != nil {
		return err
	}
	policy, err := decodeRoundPolicySnapshot(stored.PolicyJson)
	if err != nil || policy.AutomationPolicy == nil {
		return ErrConflict
	}
	generation := policy.AutomationPolicy.SourceActivationGeneration
	grant, err := self.Store.SourceIntakeActivation(ctx, self.Settings, generation)
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	if err != nil || grant.TargetRoundId != round.RoundId {
		if err != nil {
			return err
		}
		return ErrConflict
	}
	intent, err := self.Store.SourceActivation(ctx, self.Settings, generation)
	if err != nil {
		return err
	}
	if intent.ActivatedAt == nil {
		if err := self.PrepareSource(ctx, lease, intent); err != nil {
			return err
		}
		if err := self.ActivateSource(ctx, lease, intent); err != nil {
			return err
		}
	}
	status, err := self.Store.SourceLifecycleStatus(ctx, self.Settings, generation)
	if err != nil {
		return err
	}
	if status == nil || !status.GradingRuntimeActivated || !status.ApiAcknowledged || !status.WorkerAcknowledged {
		return errCompetitionIntakeFirstPending
	}
	return nil
}

// The job has not begun until its ordinary claim. Reserved workload/source
// identities are not public request clocks or synthetic runtime receipts.
func competitionIntakeFirstRoundIdentity(round *model.CompetitionRoundResult, intent *model.CompetitionSourceActivation) bool {
	return round != nil && intent != nil && round.RoundId != (server.Id{}) && round.RoundId == intent.TargetRoundId && round.Epoch == intent.TargetEpoch && !round.Staging &&
		!round.OpensAt.IsZero() && round.ClosesAt.Sub(round.OpensAt).Seconds() == 604800
}
