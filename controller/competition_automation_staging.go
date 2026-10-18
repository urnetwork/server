// Explicit staging authority is isolated from production and the historical staging era.
package controller

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// Production timing never derives from staging values, even on a staging-named environment.
func sourcePreparationInterval(intent *model.CompetitionSourceActivation) time.Duration {
	if intent.Staging {
		return time.Duration(intent.StagingPreparationSeconds) * time.Second
	}
	return competitionPreparationInterval
}

// A source cannot reinterpret a configured season as another execution mode.
func sourceModeMatches(settings *Settings, intent *model.CompetitionSourceActivation) bool {
	if settings == nil || intent == nil || !settings.AutomationPolicy.Enabled {
		return false
	}
	policy := settings.AutomationPolicy
	return intent.Staging == policy.Staging &&
		intent.StagingRoundWindowSeconds == policy.StagingRoundWindowSeconds && intent.StagingPreparationSeconds == policy.StagingPreparationSeconds &&
		((!intent.Staging && intent.StagingRoundWindowSeconds == 0 && intent.StagingPreparationSeconds == 0) ||
			(intent.Staging && model.ValidateCompetitionAutomationPolicy(policy) == nil && intent.Threshold == 0 && settings.EvaluationPolicy.TakeoverMargin == 0 && settings.SeasonPolicy.SubmissionFeeUsd == 0))
}

// Reads only the explicitly configured v2 season, never legacy staging or another mode.
func (self PostgresStore) CurrentAutomationRound(ctx context.Context, settings *Settings) (round *roundRecord, err error) {
	if settings == nil || !settings.AutomationPolicy.Enabled {
		return nil, ErrAutomationDisabled
	}
	err = captureDatabaseError(func() {
		server.Db(ctx, func(conn server.PgConn) {
			round, err = scanRound(conn.QueryRow(ctx, `SELECT round_id,competition_id,epoch_number,staging,workload_commitment,seed_nonce,
		 seed_ciphertext,providers_sha256,providers_path,policy_json,opens_at,closes_at,reveal_at,created_at,canceled,
		 finalized_at,winner_job_id,admission_closed_at,automation_sealed_at,automation_publication_at
		 FROM competition_round WHERE competition_id=$1 AND staging=$2 AND NOT canceled
		 AND COALESCE((policy_json->'automation_policy'->>'enabled')::boolean,false)
		 AND COALESCE((policy_json->'automation_policy'->>'staging')::boolean,false)=$2
		 AND policy_json->'automation_policy'->>'environment'=$3 AND policy_json->'automation_policy'->>'season_id'=$4
		 ORDER BY epoch_number DESC LIMIT 1`, settings.CompetitionId, settings.AutomationPolicy.Staging, settings.AutomationPolicy.Environment, settings.AutomationPolicy.SeasonId))
			if errors.Is(err, pgx.ErrNoRows) {
				round, err = nil, nil
			} else {
				server.Raise(err)
			}
		})
	})
	if err == nil && round != nil {
		setRoundStatus(round, self.nowUtc())
	}
	return round, err
}

// Creates a prospectively qualified v2 staging round without altering any legacy row.
// The database independently enforces the frozen schedule, source, predecessor and cap.
func (self PostgresStore) createAutomationStagingRound(ctx context.Context, settings *Settings, args GenerateRoundArgs) (*roundRecord, error) {
	if !settings.AutomationPolicy.Enabled || !settings.AutomationPolicy.Staging || settings.automationLease == nil {
		return nil, ErrAutomationDisabled
	}
	if err := settings.Validate(); err != nil {
		return nil, err
	}
	if !self.nowUtc().Before(settings.SeasonEndsAt) || settings.SeasonEndsAt.Before(args.ClosesAt) {
		return nil, ErrSeasonComplete
	}
	round, err := self.prepareRound(ctx, settings, args, true)
	if err != nil {
		return nil, err
	}
	err = captureDatabaseError(func() {
		server.Tx(ctx, func(tx server.PgTx) {
			automationScopeLockInTx(ctx, tx, settings)
			server.Raise(self.requireAutomationLeaseInTx(ctx, tx, settings, *settings.automationLease))
			server.Raise(ensureAutomationNotHeldInTx(ctx, tx, settings, "admission"))
			server.RaisePgResult(tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('competition-round-v1',0))`))
			var productionExists, activeStaging bool
			server.Raise(tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM competition_round WHERE competition_id=$1 AND NOT staging),
		 EXISTS(SELECT 1 FROM competition_round WHERE competition_id=$1 AND staging AND NOT canceled AND
		 CASE WHEN COALESCE((policy_json->'automation_policy'->>'enabled')::boolean,false) THEN automation_sealed_at IS NULL ELSE finalized_at IS NULL END)`, settings.CompetitionId).Scan(&productionExists, &activeStaging))
			if productionExists {
				panic(ErrSeasonStarted)
			}
			if activeStaging {
				panic(ErrPreviousEpochOpen)
			}
			intent, loadErr := currentSourceActivationInTx(ctx, tx, settings)
			server.Raise(loadErr)
			if !sourceModeMatches(settings, intent) || intent.TargetRoundId != round.RoundId || intent.Generation != settings.AutomationPolicy.SourceActivationGeneration || intent.ActivatedAt == nil {
				panic(ErrConflict)
			}
			round.Epoch = intent.TargetEpoch
			if round.Epoch < 1 || round.Epoch > 6 {
				panic(ErrSeasonComplete)
			}
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO competition_round(round_id,competition_id,epoch_number,staging,workload_commitment,seed_nonce,
		 seed_ciphertext,providers_sha256,providers_path,policy_json,opens_at,closes_at,reveal_at,created_at,canceled)
		 VALUES($1,$2,$3,true,$4,$5,$6,$7,$8,$9::jsonb,$10,$11,$12,$13,false)`, round.RoundId, round.CompetitionId, round.Epoch,
				round.WorkloadCommitment, round.SeedNonce, round.SeedCiphertext, round.ProvidersSha256, round.ProvidersPath,
				string(round.PolicyJson), round.OpensAt, round.ClosesAt, round.RevealAt, round.CreatedAt))
		})
	})
	if err != nil {
		return nil, err
	}
	setRoundStatus(round, self.nowUtc())
	competitionRoundEvents.WithLabelValues("staging_created").Inc()
	return round, nil
}
