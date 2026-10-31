// Intake preserves source/archive protection while separately reporting real
// host availability and exact initial or calibration-maintenance hold authority.
package controller

import (
	"context"
	"errors"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
)

// Scope-specific latest revisions contain no private explanation or actor data.
type competitionAutomationHoldStatus struct {
	Admission, Evaluation, RewardDelivery bool
	EvaluationHold, RewardDeliveryHold    *model.CompetitionAutomationHold
}

// Missing support or a failed hold read cannot produce a green v2 projection.
type competitionAutomationHoldStatusStore interface {
	AutomationHoldStatus(context.Context, *Settings) (competitionAutomationHoldStatus, error)
}

// One bounded read owns its connection; callers never retain another resource.
func (self PostgresStore) AutomationHoldStatus(ctx context.Context, settings *Settings) (status competitionAutomationHoldStatus, err error) {
	err = captureDatabaseError(func() {
		server.Db(ctx, func(conn server.PgConn) {
			status, err = readCompetitionIntakeHolds(ctx, conn, settings)
			server.Raise(err)
		})
	})
	return status, err
}

// Ordinary holds never waive readiness; only an exact intake approval can
// distinguish intake from an offline host. Canonical PublicInfo stays unchanged.
func (self *Service) competitionRuntimeStatus(ctx context.Context, settings *Settings, round *roundRecord, checks map[string]bool) (*model.CompetitionRuntimeStatus, error) {
	if !settings.AutomationPolicy.Enabled {
		return nil, nil
	}
	store, ok := self.store.(competitionAutomationHoldStatusStore)
	if !ok {
		return nil, errors.New("durable competition hold status is unavailable")
	}
	holds, err := store.AutomationHoldStatus(ctx, settings)
	if err != nil {
		return nil, err
	}
	strictReady := allChecks(checks) && !holds.Admission
	intakeReady := strictReady
	if !intakeReady {
		intakeReady, err = self.approvedIntakeReadiness(ctx, settings, round, checks, holds)
		if err != nil {
			return nil, err
		}
	}
	return &model.CompetitionRuntimeStatus{IntakeReady: intakeReady, GradingReady: strictReady && !holds.Evaluation,
		PublicationHeld: holds.Evaluation}, nil
}

// Public scores, winners and round secrets share the grading hold. A payout
// hold remains independent and does not suppress historical integrity notices.
func (self *Service) competitionPublicationHeld(ctx context.Context, settings *Settings, round *roundRecord) (bool, error) {
	if !competitionRoundUsesV2(round) {
		return false, nil
	}
	policy, err := decodeRoundPolicySnapshot(round.PolicyJson)
	if err != nil || policy.CompetitionId != settings.CompetitionId {
		return true, errors.New("frozen hold scope does not match the requested competition")
	}
	scoped := *settings
	scoped.AutomationPolicy = *policy.AutomationPolicy
	store, ok := self.store.(competitionAutomationHoldStatusStore)
	if !ok {
		return true, errors.New("durable competition hold status is unavailable")
	}
	holds, err := store.AutomationHoldStatus(ctx, &scoped)
	if err != nil {
		return true, err
	}
	return holds.Evaluation, nil
}
