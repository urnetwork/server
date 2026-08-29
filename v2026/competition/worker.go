package competition

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"time"

	"github.com/urnetwork/glog/v2026"
	"github.com/urnetwork/server/v2026"
)

var workerIdPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

type Worker struct {
	settings          *Settings
	store             Store
	evaluator         Evaluator
	workerId          string
	workerImageDigest string
	pollEvery         time.Duration
}

func NewWorker(settings *Settings, store Store, evaluator Evaluator, workerId string) (*Worker, error) {
	workerImageDigest, err := runtimeImageDigest()
	if err != nil {
		return nil, err
	}
	return newWorkerWithImageDigest(settings, store, evaluator, workerId, workerImageDigest)
}

func newWorkerWithImageDigest(
	settings *Settings,
	store Store,
	evaluator Evaluator,
	workerId string,
	workerImageDigest string,
) (*Worker, error) {
	if err := settings.Validate(); err != nil {
		return nil, err
	}
	if store == nil || evaluator == nil {
		return nil, errors.New("competition worker requires a store and evaluator")
	}
	if !workerIdPattern.MatchString(workerId) {
		return nil, errors.New("worker id must match [A-Za-z0-9][A-Za-z0-9._-]{0,127}")
	}
	if _, err := validateRuntimeImageDigest(workerImageDigest); err != nil {
		return nil, err
	}
	return &Worker{
		settings: settings, store: store, evaluator: evaluator, workerId: workerId,
		workerImageDigest: workerImageDigest, pollEvery: time.Second,
	}, nil
}

func (w *Worker) Run(ctx context.Context) error {
	hostCheck, err := w.evaluator.SelfCheck(ctx, w.settings)
	if err != nil {
		// Register a failed report when the command returned a parseable report;
		// eligibility is recomputed in the store and therefore cannot be forged
		// by merely setting an `eligible` field.
		if hostCheck.HostId != "" {
			_ = w.store.RegisterHost(context.WithoutCancel(ctx), w.settings, hostCheck)
		}
		return fmt.Errorf("competition evaluator self-check: %w", err)
	}
	if err := w.store.RegisterHost(ctx, w.settings, hostCheck); err != nil {
		return fmt.Errorf("register evaluator host: %w", err)
	}
	hostTicker := time.NewTicker(time.Duration(w.settings.WorkerHeartbeatSeconds) * time.Second)
	defer hostTicker.Stop()
	pollTicker := time.NewTicker(w.pollEvery)
	defer pollTicker.Stop()

	for {
		finished, err := w.finishEpoch(ctx)
		if err != nil {
			return fmt.Errorf("finish competition epoch: %w", err)
		}
		if finished {
			return nil
		}
		job, err := w.store.Claim(ctx, w.settings, w.workerId, w.workerImageDigest)
		if err != nil {
			return fmt.Errorf("claim competition job: %w", err)
		}
		if job != nil {
			if err := w.evaluateOne(ctx, job, hostCheck); err != nil {
				return err
			}
			continue
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-pollTicker.C:
		case <-hostTicker.C:
			fresh, checkErr := w.evaluator.SelfCheck(ctx, w.settings)
			if checkErr != nil {
				if fresh.HostId != "" {
					_ = w.store.RegisterHost(context.WithoutCancel(ctx), w.settings, fresh)
				}
				return fmt.Errorf("competition evaluator lost self-check: %w", checkErr)
			}
			if err := w.store.RegisterHost(ctx, w.settings, fresh); err != nil {
				return fmt.Errorf("refresh evaluator host: %w", err)
			}
			hostCheck = fresh
		}
	}
}

// Seals the one round owned by this process after admission closes and the
// immediate FIFO drains, including work that extends beyond closes_at. A
// significant candidate is left embargoed for the operator-controlled honesty
// review gate; the worker never selects or publishes a winner by itself.
func (w *Worker) finishEpoch(ctx context.Context) (bool, error) {
	latest, err := w.store.CurrentRound(ctx, w.settings)
	if err != nil {
		return false, err
	}
	if latest == nil {
		return false, nil
	}
	if latest.FinalizedAt != nil {
		return true, nil
	}
	state, err := w.store.PrepareCandidateReview(ctx, w.settings, latest.Epoch)
	if err != nil {
		return false, err
	}
	if state.Status == "pending_review" {
		glog.Infof(
			"[competition]epoch %d sealed round=%s candidate=%s rank=%d awaiting_honesty_review=true\n",
			state.Epoch,
			state.RoundId,
			state.Candidate.JobId,
			state.Candidate.Rank,
		)
		return true, nil
	}
	if state.Status == "finalized" {
		winner := "none"
		if state.WinnerJobId != nil {
			winner = state.WinnerJobId.String()
		}
		glog.Infof(
			"[competition]epoch %d finalized round=%s winner=%s\n",
			state.Epoch,
			state.RoundId,
			winner,
		)
		return true, nil
	}
	return false, nil
}

func (w *Worker) evaluateOne(parent context.Context, job *queuedJob, hostCheck HostSelfCheck) error {
	startedAt := time.Now()
	metricOutcome := "infrastructure_failed"
	defer func() {
		competitionEvaluationSeconds.Observe(time.Since(startedAt).Seconds())
		competitionEvaluations.WithLabelValues(metricOutcome).Inc()
	}()
	if !hostCheck.RebaselinePassed || hostCheck.RebaselineRoundId == nil || *hostCheck.RebaselineRoundId != job.RoundId {
		metricOutcome = "rebaseline_mismatch"
		_ = w.handBack(job.JobId, "round_rebaseline_mismatch")
		return fmt.Errorf("competition evaluator host is not re-baselined for round %s", job.RoundId)
	}
	if job.StartedAt == nil {
		_ = w.handBack(job.JobId, "missing_execution_start")
		return fmt.Errorf("competition job %s has no execution start", job.JobId)
	}
	executionDeadline := job.StartedAt.Add(
		time.Duration(w.settings.EvaluationPolicy.ScoreTimeoutSeconds) * time.Second,
	)
	if !server.NowUtc().Before(executionDeadline) {
		outcome := EvaluationOutcome{
			Error: infrastructureError(
				"evaluation_time_budget_exhausted",
				"submission exhausted its total evaluation time budget",
			),
			Infrastructure: false,
		}
		retry, err := w.store.Complete(
			context.WithoutCancel(parent),
			w.settings,
			w.workerId,
			job.JobId,
			outcome,
		)
		if err != nil {
			return fmt.Errorf("complete expired competition job %s: %w", job.JobId, err)
		}
		if retry {
			return fmt.Errorf("expired competition job %s was retained for retry", job.JobId)
		}
		return nil
	}
	evalCtx, cancel := context.WithDeadline(parent, executionDeadline)
	defer cancel()
	type evalReturn struct{ outcome EvaluationOutcome }
	done := make(chan evalReturn, 1)
	go func() {
		done <- evalReturn{outcome: w.evaluator.Evaluate(evalCtx, w.settings, job)}
	}()
	heartbeatTicker := time.NewTicker(time.Duration(w.settings.WorkerHeartbeatSeconds) * time.Second)
	defer heartbeatTicker.Stop()
	for {
		select {
		case result := <-done:
			cancel()
			if result.outcome.Score != nil {
				if err := validateScore(result.outcome.Score); err != nil {
					result.outcome = EvaluationOutcome{
						Error:          infrastructureError("score_result_invalid", "pinned scorer returned an invalid result"),
						Infrastructure: true,
					}
				}
			}
			retry, err := w.store.Complete(context.WithoutCancel(parent), w.settings, w.workerId, job.JobId, result.outcome)
			if err != nil {
				return fmt.Errorf("complete competition job %s: %w", job.JobId, err)
			}
			if retry {
				metricOutcome = "infrastructure_retry"
				glog.Infof("[competition]job %s retained for infrastructure retry\n", job.JobId)
			} else if result.outcome.Score != nil && result.outcome.Error == nil {
				metricOutcome = "succeeded"
			} else if result.outcome.Error != nil && result.outcome.Error.Kind == "submission" {
				metricOutcome = "submission_failed"
			}
			return nil
		case <-heartbeatTicker.C:
			if err := w.store.Heartbeat(parent, w.settings, w.workerId, job.JobId); err != nil {
				cancel()
				<-done
				_ = w.handBack(job.JobId, "heartbeat_failed")
				return fmt.Errorf("heartbeat competition job %s: %w", job.JobId, err)
			}
			// A long evaluation must not make a healthy box disappear from the
			// authoritative-host readiness set. The per-job security result remains the
			// authoritative live containment proof; this refresh only extends
			// the already pinned, successful host attestation.
			if err := w.store.RegisterHost(parent, w.settings, hostCheck); err != nil {
				cancel()
				<-done
				_ = w.handBack(job.JobId, "host_heartbeat_failed")
				return fmt.Errorf("refresh evaluator host during job %s: %w", job.JobId, err)
			}
		case <-parent.Done():
			cancel()
			<-done
			if err := w.handBack(job.JobId, "worker_shutdown"); err != nil {
				return fmt.Errorf("hand back competition job %s: %w", job.JobId, err)
			}
			return parent.Err()
		}
	}
}

func (w *Worker) handBack(jobId server.Id, reason string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	return w.store.HandBack(ctx, w.workerId, jobId, reason)
}
