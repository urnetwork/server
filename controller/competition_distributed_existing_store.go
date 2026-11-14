// Existing custody snapshots never claim, renew or start an execution. Native
// observation occurs only after this database connection has been released.
package controller

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/urnetwork/server"
)

// The occupied slot, not FIFO or request input, selects the sole existing run.
// A later normal claim callback independently locks and compares the snapshot.
func (self PostgresStore) existingCompetitionJob(ctx context.Context, settings *Settings) (job *queuedJob, resultErr error) {
	if !competitionDistributedEnabled(settings) || !competitionExistingControlEnabled(settings) || settings.automationLease == nil || settings.automationRoundId == (server.Id{}) {
		return nil, ErrLeaseLost
	}
	resultErr = captureDatabaseError(func() {
		server.Tx(ctx, func(tx server.PgTx) {
			automationScopeLockInTx(ctx, tx, settings)
			server.Raise(self.requireAutomationLeaseInTx(ctx, tx, settings, *settings.automationLease))
			var err error
			job, err = scanJob(tx.QueryRow(ctx, jobSelect+` WHERE j.round_id=$1 AND j.executor_host_id=$2 AND j.executor_slot_id=$3 AND j.state='running'
 AND EXISTS(SELECT 1 FROM competition_execution_slot s WHERE s.slot_id=j.executor_slot_id AND s.host_id=j.executor_host_id
 AND s.job_id=j.job_id AND s.round_id=j.round_id AND s.worker_id=j.lease_owner AND s.fencing_token=j.executor_fencing_token
 AND s.lease_expires_at=j.lease_expires_at)`, settings.automationRoundId, settings.executorHostId, settings.executorSlotId), true, self.nowUtc())
			if errors.Is(err, pgx.ErrNoRows) {
				job = nil
				return
			}
			server.Raise(err)
			if job.Round.Canceled || job.Round.FinalizedAt != nil || job.Round.AutomationSealedAt != nil {
				panic(ErrLeaseLost)
			}
			_, err = settingsForFrozenRound(settings, &job.Round)
			server.Raise(err)
			server.Raise(ensureAutomationRoundValidInTx(ctx, tx, job.RoundId))
			baseline, err := scanRoundBaseline(tx.QueryRow(ctx, `SELECT round_id,baseline_json,baseline_sha256,source_job_id,source_attempt,
 source_artifact_manifest_sha256,base_sha,providers_sha256,evaluator_image_digest,scorer_version,hardware_id,host_qualification_sha256,created_at
 FROM competition_round_baseline WHERE round_id=$1`, job.RoundId))
			server.Raise(err)
			server.Raise(validateRoundBaselineRecord(settings, &job.Round, baseline))
			job.RoundBaseline = baseline
		})
	})
	if resultErr != nil {
		return nil, resultErr
	}
	return job, nil
}
