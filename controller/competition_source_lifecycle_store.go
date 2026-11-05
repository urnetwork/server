// Source-index and public readiness consume authenticated purpose authority,
// never an activation timestamp or a deployment command's success bit alone.
package controller

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// This bounded projection omits seed/provider/patch material. Genuine runtime
// receipts, immutable control and per-host admission remain independently
// authenticated; there is no callback that can mint a successful qualification.
func (self PostgresStore) SourceLifecycleStatus(ctx context.Context, settings *Settings, generation int64) (status *CompetitionSourceLifecycleStatus, resultErr error) {
	var round *roundRecord
	var baseline *roundBaselineRecord
	var frozen *Settings
	var stateErr error
	resultErr = captureDatabaseError(func() {
		server.Tx(ctx, func(tx server.PgTx) {
			status, round, baseline, frozen, stateErr = nil, nil, nil, nil, nil
			intent, err := sourceActivationInTx(ctx, tx, settings, generation)
			server.Raise(err)
			if !competitionIntakeFirstApplies(settings, intent) {
				stateErr = ErrUnsupportedDecisionMode
				return
			}
			frozen, _, err = competitionIntakeFirstFrozenSettings(settings, intent)
			server.Raise(err)
			status = &CompetitionSourceLifecycleStatus{Schema: 1, Capability: CompetitionSourceLifecycleCapability, Purpose: CompetitionSourceLifecyclePrepared, Intent: intent}
			rows, err := tx.Query(ctx, `SELECT receipt_json FROM competition_source_readiness WHERE environment=$1 AND competition_id=$2 AND season_id=$3 AND generation=$4`,
				intent.Environment, intent.CompetitionId, intent.SeasonId, generation)
			roles := []model.CompetitionSourceReadiness{}
			server.WithPgResult(rows, err, func() {
				for rows.Next() {
					var content []byte
					server.Raise(rows.Scan(&content))
					var receipt model.CompetitionSourceReadiness
					server.Raise(json.Unmarshal(content, &receipt))
					if !sourcePreparationReceiptMatches(intent, receipt) || receipt.RecordedAt.IsZero() {
						panic(ErrConflict)
					}
					roles = append(roles, receipt)
					status.ApiAcknowledged = status.ApiAcknowledged || receipt.Role == "api"
					status.WorkerAcknowledged = status.WorkerAcknowledged || receipt.Role == "worker"
				}
			})
			grant, err := sourceIntakeActivation(ctx, tx, settings, generation)
			if errors.Is(err, ErrNotFound) {
				return
			}
			server.Raise(err)
			round, err = competitionIntakeFirstRound(ctx, tx, settings, intent.TargetRoundId)
			server.Raise(err)
			if round == nil {
				return
			}
			api, err := sourceIntakeApiReceipt(ctx, tx, settings, intent)
			server.Raise(err)
			frozen, err = competitionIntakeFirstActivationMatches(settings, intent, round, api, grant)
			server.Raise(err)
			status.Intake, status.Purpose = grant, CompetitionSourceLifecycleIntake
			status.Round = &CompetitionSourceLifecycleRound{RoundId: round.RoundId, Epoch: round.Epoch, OpensAt: round.OpensAt, ClosesAt: round.ClosesAt, Canceled: round.Canceled}
			var currentGeneration int64
			server.Raise(tx.QueryRow(ctx, `SELECT generation FROM competition_source_pointer WHERE environment=$1 AND competition_id=$2 AND season_id=$3`, intent.Environment, intent.CompetitionId, intent.SeasonId).Scan(&currentGeneration))
			status.GradingRuntimeActivated = intent.ActivatedAt != nil && intent.AbandonedAt == nil && currentGeneration == generation && sourcePreparationReceiptsMatch(intent, roles) == nil
			if !status.GradingRuntimeActivated {
				return
			}
			baseline, err = scanRoundBaseline(tx.QueryRow(ctx, `SELECT round_id,baseline_json,baseline_sha256,source_job_id,source_attempt,
 source_artifact_manifest_sha256,base_sha,providers_sha256,evaluator_image_digest,scorer_version,hardware_id,host_qualification_sha256,created_at
 FROM competition_round_baseline WHERE round_id=$1`, round.RoundId))
			if errors.Is(err, pgx.ErrNoRows) {
				baseline = nil
				return
			}
			server.Raise(err)
			server.Raise(validateRoundBaselineRecord(frozen, round, baseline))
			status.BaselineSha256 = baseline.Sha256
			var ready bool
			server.Raise(tx.QueryRow(ctx, `SELECT competition_v2_intake_grading_ready($1)`, round.RoundId).Scan(&ready))
			status.GradingReady = ready && !round.Canceled
		})
	})
	if resultErr != nil {
		return nil, resultErr
	}
	if stateErr != nil {
		return nil, stateErr
	}
	if status == nil {
		return nil, ErrNotFound
	}
	if !status.GradingReady {
		return status, nil
	}
	// No connection is retained while exact retained host receipts are read.
	if err := LoadCompetitionRuntimeManifestContext(ctx, frozen, status.Intent); err != nil {
		return nil, err
	}
	if frozen.artifactArchive == nil {
		return nil, ErrReviewNotReady
	}
	if err := frozen.artifactArchive.Check(ctx); err != nil {
		return nil, err
	}
	checks, err := self.Readiness(ctx, frozen)
	if err != nil {
		return nil, err
	}
	status.HostAdmissionReady = secureEvaluatorChecksPass(checks)
	if policy := frozen.DistributedEvaluationPolicy; policy != nil && round.Epoch >= policy.FromEpoch {
		for _, host := range policy.Hosts {
			qualified, err := self.distributedExecutorQualified(ctx, frozen, round, baseline, host)
			if err != nil {
				return nil, err
			}
			status.HostAdmissionReady = status.HostAdmissionReady && qualified
		}
	}
	status.GradingReady = status.GradingReady && status.HostAdmissionReady
	if status.GradingReady {
		status.Purpose = CompetitionSourceLifecycleGrading
	}
	return status, nil
}
