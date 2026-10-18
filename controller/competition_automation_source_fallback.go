// Fallback abandons only an explicitly verified unpublished promotion, never a live source.
package controller

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// The caller must first prove every remote ref still equals this intent's expected old ref.
// Once any ref advances, recovery must retry that immutable publication rather than abandon it.
func (self PostgresStore) AbandonSourceIntent(ctx context.Context, settings *Settings, lease model.CompetitionSupervisorLease, generation int64, reason, unpublishedProofSha256 string) error {
	if !sha256Pattern.MatchString(unpublishedProofSha256) || (reason != "publication_failed" && reason != "readiness_failed" && reason != "build_failed" && reason != "source_approval_revoked") {
		return ErrConflict
	}
	return captureDatabaseError(func() {
		server.Tx(ctx, func(tx server.PgTx) {
			automationScopeLockInTx(ctx, tx, settings)
			server.Raise(self.requireAutomationLeaseInTx(ctx, tx, settings, lease))
			intent, err := sourceActivationInTx(ctx, tx, settings, generation)
			server.Raise(err)
			var existing []byte
			err = tx.QueryRow(ctx, `SELECT abandonment_json FROM competition_source_abandonment WHERE environment=$1 AND competition_id=$2 AND season_id=$3 AND generation=$4`,
				intent.Environment, intent.CompetitionId, intent.SeasonId, generation).Scan(&existing)
			if err == nil {
				var abandonment model.CompetitionSourceAbandonment
				server.Raise(json.Unmarshal(existing, &abandonment))
				if abandonment.Reason != reason || abandonment.UnpublishedProofSha256 != unpublishedProofSha256 || abandonment.IntentSha256 != intent.IntentSha256 || intent.AbandonedAt == nil {
					panic(ErrConflict)
				}
				return
			}
			if !errors.Is(err, pgx.ErrNoRows) {
				server.Raise(err)
			}
			if intent.Kind != "reviewed_source_promotion" || intent.ActivatedAt != nil || intent.AbandonedAt != nil {
				panic(ErrConflict)
			}
			var unsafe bool
			server.Raise(tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM competition_source_readiness WHERE environment=$1 AND competition_id=$2 AND season_id=$3 AND generation=$4)
			 OR EXISTS(SELECT 1 FROM competition_source_pointer WHERE environment=$1 AND competition_id=$2 AND season_id=$3 AND generation=$4)
			 OR EXISTS(SELECT 1 FROM competition_round WHERE competition_id=$2 AND staging=$6 AND epoch_number=$5
			 AND policy_json->'automation_policy'->>'environment'=$1 AND policy_json->'automation_policy'->>'season_id'=$3)`,
				intent.Environment, intent.CompetitionId, intent.SeasonId, generation, intent.TargetEpoch, intent.Staging).Scan(&unsafe))
			if unsafe {
				panic(ErrConflict)
			}
			now := self.nowUtc()
			abandonment := model.CompetitionSourceAbandonment{Environment: intent.Environment, CompetitionId: intent.CompetitionId, SeasonId: intent.SeasonId,
				Generation: generation, IntentSha256: intent.IntentSha256, ActorId: lease.OwnerId, FencingToken: lease.FencingToken, Reason: reason, UnpublishedProofSha256: unpublishedProofSha256, CreatedAt: now}
			content, err := json.Marshal(abandonment)
			server.Raise(err)
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO competition_source_abandonment(environment,competition_id,season_id,generation,abandonment_json,created_at) VALUES($1,$2,$3,$4,$5::jsonb,$6)`,
				intent.Environment, intent.CompetitionId, intent.SeasonId, generation, string(content), now))
			server.RaisePgResult(tx.Exec(ctx, `UPDATE competition_source_activation SET abandoned_at=$5 WHERE environment=$1 AND competition_id=$2 AND season_id=$3 AND generation=$4`,
				intent.Environment, intent.CompetitionId, intent.SeasonId, generation, now))
		})
	})
}

// Activation generations include failed attempts; reviewed source generations do not.
func (self PostgresStore) NextSourceActivationGeneration(ctx context.Context, settings *Settings) (generation int64, err error) {
	err = captureDatabaseError(func() {
		server.Db(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(ctx, `SELECT COALESCE(max(generation),0)+1 FROM competition_source_activation WHERE environment=$1 AND competition_id=$2 AND season_id=$3`,
				settings.AutomationPolicy.Environment, settings.CompetitionId, settings.AutomationPolicy.SeasonId).Scan(&generation))
		})
	})
	return generation, err
}

// The sole non-abandoned intent is the recovery authority for this target epoch.
func (self PostgresStore) SourceActivationForTarget(ctx context.Context, settings *Settings, targetEpoch int) (result *model.CompetitionSourceActivation, err error) {
	err = captureDatabaseError(func() {
		server.Tx(ctx, func(tx server.PgTx) {
			result = nil
			var generation int64
			err := tx.QueryRow(ctx, `SELECT generation FROM competition_source_activation WHERE environment=$1 AND competition_id=$2 AND season_id=$3 AND target_epoch=$4 AND abandoned_at IS NULL`,
				settings.AutomationPolicy.Environment, settings.CompetitionId, settings.AutomationPolicy.SeasonId, targetEpoch).Scan(&generation)
			if errors.Is(err, pgx.ErrNoRows) {
				panic(ErrNotFound)
			}
			server.Raise(err)
			result, err = sourceActivationInTx(ctx, tx, settings, generation)
			server.Raise(err)
		})
	})
	return result, err
}
