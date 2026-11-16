// Completion polling is read-only and never grants sealing authority. The
// final transaction independently rechecks close, hold, drain and source state.
package controller

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/urnetwork/server/v2026"
)

// Optional optimization: stores without it retain the final guarded SQL path.
// No caller may replace this fresh read with the launch request's round clock.
type competitionAutomationEpochPollStore interface {
	automationEpochSealReady(context.Context, *Settings, server.Id) (bool, error)
}

// One returned connection owns the entire poll. It holds no season/round lock
// and calls no other store method while the connection is checked out.
func (self PostgresStore) automationEpochSealReady(ctx context.Context, settings *Settings, roundId server.Id) (ready bool, resultErr error) {
	if settings == nil || !settings.AutomationPolicy.Enabled || roundId == (server.Id{}) {
		return false, ErrAutomationDisabled
	}
	var stateErr error
	resultErr = captureDatabaseError(func() {
		server.Db(ctx, func(conn server.PgConn) {
			ready, stateErr = false, nil
			var closesAt time.Time
			var sealedAt *time.Time
			var canceled, evaluationHeld bool
			err := conn.QueryRow(ctx, `SELECT r.closes_at,r.automation_sealed_at,r.canceled,
 COALESCE((SELECT h.active FROM competition_automation_hold h
 WHERE h.environment=$3 AND h.competition_id=$2 AND h.season_id=$4 AND h.kind='evaluation'
 ORDER BY h.revision DESC LIMIT 1),false)
 FROM competition_round r WHERE r.round_id=$1 AND r.competition_id=$2 AND r.staging=$5
 AND COALESCE((r.policy_json->'automation_policy'->>'enabled')::boolean,false)
 AND r.policy_json->'automation_policy'->>'environment'=$3
 AND r.policy_json->'automation_policy'->>'season_id'=$4
 AND COALESCE((r.policy_json->'automation_policy'->>'staging')::boolean,false)=$5`,
				roundId, settings.CompetitionId, settings.AutomationPolicy.Environment, settings.AutomationPolicy.SeasonId, settings.AutomationPolicy.Staging).
				Scan(&closesAt, &sealedAt, &canceled, &evaluationHeld)
			if errors.Is(err, pgx.ErrNoRows) {
				stateErr = ErrNotFound
				return
			}
			server.Raise(err)
			if evaluationHeld {
				stateErr = ErrAutomationHeld
				return
			}
			// Use this fresh durable close and the same current store clock as
			// SealAndElect, never stale request/configuration or cached Status.
			// Legacy staging manual closure never enters this automated path.
			ready = !canceled && (sealedAt != nil || !self.nowUtc().Before(closesAt))
		})
	})
	if resultErr == nil {
		resultErr = stateErr
	}
	return ready, resultErr
}
