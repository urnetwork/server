// One fenced main coordinator prepares a single control, admits qualified hosts
// and runs one existing worker per physical slot. Result persistence and sealing
// remain in the main store; agents never receive its credentials or API tokens.
package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/competitionagent"
)

// Each child owns only its host. An unavailable host does not cancel healthy
// siblings, and the existing global pending-job check prevents early sealing.
func runCompetitionDistributedCoordinator(ctx context.Context, settings *Settings, store competitionSupervisorWorkerStore, workerId string) error {
	if settings == nil || settings.DistributedEvaluationPolicy == nil || settings.distributedCoordinator == nil || settings.automationLease == nil || settings.distributedRoundEpoch < 2 ||
		settings.DistributedEvaluationPolicy.CoordinatorId != settings.distributedCoordinator.Config.CoordinatorId {
		return errors.New("distributed execution requires the explicit frozen coordinator role")
	}
	if err := validateCompetitionDistributedPolicy(settings.DistributedEvaluationPolicy); err != nil {
		return err
	}
	bootstrapSettings := *settings
	bootstrapSettings.executorHostId, bootstrapSettings.executorSlotId = "", 0
	bootstrapSettings.distributedPreparationOnly = true
	bootstrap, err := NewWorker(&bootstrapSettings, store, CommandEvaluator{}, competitionCoordinatorWorkerId(workerId, "baseline"))
	if err != nil {
		return err
	}
	hostCheck, err := bootstrap.preflight(ctx)
	if err != nil {
		return err
	}
	if err := validateCompetitionDistributedBaselineHost(&bootstrapSettings, hostCheck); err != nil {
		return err
	}
	var baseline *roundBaselineRecord
	for {
		finished, err := bootstrap.finishEpoch(ctx)
		if err != nil && !errors.Is(err, ErrAutomationHeld) {
			return err
		}
		if finished {
			return nil
		}
		baseline, err = store.PostgresStore.distributedRoundBaseline(ctx, settings, store.roundId)
		if err != nil {
			return err
		}
		if baseline != nil {
			break
		}
		if settings.securityToolchainException != nil {
			return errors.New("security toolchain exception requires the existing baseline; it cannot prepare a replacement")
		}
		if _, err := bootstrap.claimWithPreparedRoundBaseline(ctx, hostCheck); err != nil && !errors.Is(err, ErrAutomationHeld) {
			return err
		}
		if err := store.RegisterHost(ctx, &bootstrapSettings, hostCheck); err != nil {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(settings.distributedCoordinator.Config.Settings.PollInterval):
		}
	}
	round, err := store.GetRound(ctx, settings, store.roundId)
	if err != nil {
		return err
	}
	if err := validateCompetitionSecurityToolchainScope(settings, round, baseline); err != nil {
		return err
	}
	if err := store.PostgresStore.recordCompetitionSecurityToolchainException(ctx, settings, round, baseline); err != nil {
		return err
	}
	if err := qualifyCompetitionDistributedHosts(ctx, settings, store, baseline); err != nil {
		return err
	}
	type childReturn struct {
		hostId string
		err    error
	}
	done := make(chan childReturn, len(settings.DistributedEvaluationPolicy.Hosts))
	type executorChild struct {
		hostId    string
		worker    *Worker
		evaluator *competitionAgentEvaluator
	}
	children := []executorChild{}
	for _, host := range settings.DistributedEvaluationPolicy.Hosts {
		host = competitionExecutorControlHost(settings, host)
		childSettings := *settings
		childSettings.executorHostId, childSettings.executorSlotId = host.HostId, host.SlotId
		childSettings.roundBaselinePreparation, childSettings.distributedPreparationOnly = nil, false
		transport, err := competitionTransportForHost(&childSettings, host)
		if err != nil {
			return err
		}
		evaluator := &competitionAgentEvaluator{host: host, transport: transport}
		worker, err := NewWorker(&childSettings, store, evaluator, competitionCoordinatorWorkerId(workerId, host.HostId))
		if err != nil {
			return err
		}
		children = append(children, executorChild{hostId: host.HostId, worker: worker, evaluator: evaluator})
	}
	for _, child := range children {
		go func() {
			done <- childReturn{hostId: child.hostId, err: runCompetitionExecutorWorker(ctx, child.worker, child.evaluator)}
		}()
	}
	var childErrors []error
	for range settings.DistributedEvaluationPolicy.Hosts {
		result := <-done
		if result.err != nil {
			childErrors = append(childErrors, fmt.Errorf("executor %s: %w", result.hostId, result.err))
		}
	}
	return errors.Join(childErrors...)
}

// The separately pinned, no-candidate baseline producer has the global frozen
// qualification. Its physical host must be local, but that fingerprint is not
// the private executor's fingerprint. Every executor still passes its own
// signed check and retained full-R9 screen before any candidate may be claimed.
func validateCompetitionDistributedBaselineHost(settings *Settings, check HostSelfCheck) error {
	if settings == nil || settings.DistributedEvaluationPolicy == nil || settings.executorHostId != "" || !check.Eligible(settings) {
		return errors.New("baseline producer did not pass its independent frozen qualification")
	}
	for _, host := range settings.DistributedEvaluationPolicy.Hosts {
		if host.Transport == "local" {
			if host.HostId != check.HostId {
				return errors.New("baseline producer is not the frozen local physical host")
			}
			return nil
		}
	}
	return errors.New("baseline producer has no frozen local physical host")
}

// A failed preflight or acknowledgement cannot permanently strand an expired
// remote run. Only already-owned, deadline-expired work can bypass host health;
// fresh FIFO work still requires the genuine agent self-check.
func runCompetitionExecutorWorker(ctx context.Context, worker *Worker, evaluator *competitionAgentEvaluator) error {
	integrityRejected := false
	for {
		finished, err := worker.finishEpoch(ctx)
		if finished {
			return nil
		}
		// A grading pause is an idle worker state, not a reason to skip the
		// preflight and heartbeat owner on a held coordinator restart.
		if (err == nil || errors.Is(err, ErrAutomationHeld)) && !integrityRejected {
			err = worker.Run(ctx)
			if err == nil {
				return nil
			}
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if errors.Is(err, errCompetitionExecutorIntegrityRejected) || errors.Is(err, competitionagent.ErrIntegrity) {
			integrityRejected = true
		}
		competitionExecutorTransportFailures.WithLabelValues(evaluator.host.HostId).Inc()
		expiredSettings := *worker.settings
		expiredSettings.executorReconcileExpiredOnly = true
		job, claimErr := worker.store.Claim(ctx, &expiredSettings, worker.workerId, worker.workerImageDigest)
		if claimErr == nil && job != nil {
			if job.ExecutionDeadline == nil || server.NowUtc().Before(*job.ExecutionDeadline) {
				return errors.New("executor expired-only claim returned unexpired work")
			}
			evaluator.cancelExpiredRun(worker.settings, job, competitionagent.RunId(job.JobId.String(), job.EvaluationCycle, job.AttemptCount))
			outcome := EvaluationOutcome{Error: terminalInfrastructureError("evaluation_time_budget_exhausted", "submission exhausted its total evaluation time budget")}
			lease := time.Time{}
			if job.LeaseExpiresAt != nil {
				lease = *job.LeaseExpiresAt
			}
			_, _ = worker.completeWithRetry(withCompetitionJobAuthority(ctx, job), job, lease, outcome)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(worker.settings.distributedCoordinator.Config.Settings.PollInterval):
		}
	}
}

// A bounded hash retains stable ownership without overflowing worker-id limits.
func competitionCoordinatorWorkerId(coordinatorId, hostId string) string {
	digest := sha256.Sum256([]byte(coordinatorId + "\x00" + hostId))
	return "executor-" + hex.EncodeToString(digest[:])
}

// Every read validates the control against the same frozen round before use.
func (self PostgresStore) distributedRoundBaseline(ctx context.Context, settings *Settings, roundId server.Id) (baseline *roundBaselineRecord, resultErr error) {
	round, err := self.GetRound(ctx, settings, roundId)
	if err != nil {
		return nil, err
	}
	resultErr = captureDatabaseError(func() {
		server.Db(ctx, func(conn server.PgConn) {
			var err error
			baseline, err = scanRoundBaseline(conn.QueryRow(ctx, `SELECT round_id,baseline_json,baseline_sha256,source_job_id,source_attempt,
 source_artifact_manifest_sha256,base_sha,providers_sha256,evaluator_image_digest,scorer_version,hardware_id,host_qualification_sha256,created_at
 FROM competition_round_baseline WHERE round_id=$1`, roundId))
			if errors.Is(err, pgx.ErrNoRows) {
				baseline = nil
				return
			}
			server.Raise(err)
			server.Raise(validateRoundBaselineRecord(settings, round, baseline))
		})
	})
	return baseline, resultErr
}
