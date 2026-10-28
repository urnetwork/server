// Intake readiness preserves every real source/host/archive check while
// independently reporting grading and publication holds. No gate is fabricated.
package controller

import (
	"context"
	"errors"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// Scope-specific latest revisions contain no private explanation or actor data.
type competitionAutomationHoldStatus struct {
	Admission, Evaluation, RewardDelivery bool
}

// Missing support or a failed hold read cannot produce a green v2 projection.
type competitionAutomationHoldStatusStore interface {
	AutomationHoldStatus(context.Context, *Settings) (competitionAutomationHoldStatus, error)
}

// One bounded read owns its connection; callers never retain another resource.
func (self PostgresStore) AutomationHoldStatus(ctx context.Context, settings *Settings) (status competitionAutomationHoldStatus, err error) {
	err = captureDatabaseError(func() {
		server.Db(ctx, func(conn server.PgConn) {
			rows, queryErr := conn.Query(ctx, `SELECT capability.kind,COALESCE((SELECT active FROM competition_automation_hold
 WHERE environment=$1 AND competition_id=$2 AND season_id=$3 AND kind=capability.kind
 ORDER BY revision DESC LIMIT 1),false)
 FROM (VALUES ('admission'),('evaluation'),('reward_delivery')) AS capability(kind)`,
				settings.AutomationPolicy.Environment, settings.CompetitionId, settings.AutomationPolicy.SeasonId)
			server.WithPgResult(rows, queryErr, func() {
				for rows.Next() {
					var kind string
					var active bool
					server.Raise(rows.Scan(&kind, &active))
					switch kind {
					case "admission":
						status.Admission = active
					case "evaluation":
						status.Evaluation = active
					case "reward_delivery":
						status.RewardDelivery = active
					}
				}
			})
		})
	})
	return status, err
}

// An evaluation hold does not weaken or add to admission's containment checks.
// PublicInfo leaves this pointer nil, preserving canonical policy digests.
func (self *Service) competitionRuntimeStatus(ctx context.Context, settings *Settings, checks map[string]bool) (*model.CompetitionRuntimeStatus, error) {
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
	intakeReady := allChecks(checks) && !holds.Admission
	return &model.CompetitionRuntimeStatus{IntakeReady: intakeReady, GradingReady: intakeReady && !holds.Evaluation,
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
