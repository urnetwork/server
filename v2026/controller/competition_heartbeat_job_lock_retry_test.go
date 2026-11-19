// Exact source-owned prefix proofs and virtual clocks exercise the new
// job-only continuation without database access or native evaluator processes.
package controller

import (
	"context"
	"errors"
	"fmt"
	"net"
	"testing"
	"testing/synctest"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/urnetwork/server/v2026"
)

// The synthetic history has one failed acquire before its one admitted
// callback. No prior admission, statement, COMMIT or cleanup may disappear.
func competitionHeartbeatJobLockFailure(cause error) competitionHeartbeatDatabaseError {
	proof := competitionHeartbeatSlotFailure(cause, server.DbTxReleaseIdle)
	proof.snapshot.Step = competitionHeartbeatDatabaseJobLock
	proof.snapshot.SqlStatements = competitionHeartbeatJobLockPrefixLength
	proof.snapshot.Acquire.Attempt = 2
	proof.snapshot.Transaction.AcquireAttempts = 2
	proof.snapshot.Statements = proof.snapshot.Statements[:competitionHeartbeatJobLockPrefixLength]
	last := &proof.snapshot.Statements[competitionHeartbeatJobLockPrefixLength-1]
	deadline := proof.snapshot.AtReturn
	last.Succeeded, last.AtReturn = false, &deadline
	return proof
}

// A second failed acquisition is not a second admitted transaction. Counts
// must prove that distinction rather than rejecting all internal acquisition.
func TestCompetitionHeartbeatJobLockProofAcceptsEarlierUnadmittedAcquisitions(t *testing.T) {
	for _, attempts := range []uint64{1, 2, 3} {
		proof := competitionHeartbeatJobLockFailure(errors.Join(server.DbContextDoneError, fmt.Errorf("synthetic deadline: %w", context.DeadlineExceeded)))
		proof.snapshot.Acquire.Attempt, proof.snapshot.Transaction.AcquireAttempts = attempts, attempts
		if !competitionHeartbeatJobLockDeadline(proof) || competitionHeartbeatSlotUpdateDeadline(proof) || competitionHeartbeatUnadmittedDeadline(proof) ||
			proof.snapshot.Transaction.AcquireSucceeded != 1 || proof.snapshot.Transaction.RollbackAcknowledged != 1 {
			t.Fatal("one closed read-only callback was rejected or mislabeled", attempts, proof.snapshot)
		}
	}
}

// A complete prefix and idle acknowledged cleanup are mandatory. Earlier
// admitted work or any write/COMMIT/unknown/manual operation remains terminal.
func TestCompetitionHeartbeatJobLockProofRejectsInexactWorkAndCleanup(t *testing.T) {
	for _, change := range []func(*competitionHeartbeatDatabaseError){
		func(p *competitionHeartbeatDatabaseError) { p.snapshot.SqlShape = "" },
		func(p *competitionHeartbeatDatabaseError) { p.snapshot.SqlShapeValid = false },
		func(p *competitionHeartbeatDatabaseError) { p.snapshot.SqlStatements++ },
		func(p *competitionHeartbeatDatabaseError) { p.snapshot.CallbackEntries++ },
		func(p *competitionHeartbeatDatabaseError) { p.snapshot.UnknownStatements++ },
		func(p *competitionHeartbeatDatabaseError) { p.snapshot.Step = competitionHeartbeatDatabaseSlotUpdate },
		func(p *competitionHeartbeatDatabaseError) { p.snapshot.Transaction.AcquireAttempts = 0 },
		func(p *competitionHeartbeatDatabaseError) { p.snapshot.Acquire.Attempt++ },
		func(p *competitionHeartbeatDatabaseError) { p.snapshot.Transaction.AcquireSucceeded++ },
		func(p *competitionHeartbeatDatabaseError) { p.snapshot.Transaction.BeginAttempts++ },
		func(p *competitionHeartbeatDatabaseError) { p.snapshot.Transaction.BeginSucceeded = 0 },
		func(p *competitionHeartbeatDatabaseError) { p.snapshot.Transaction.CallbackEntered++ },
		func(p *competitionHeartbeatDatabaseError) { p.snapshot.Transaction.CommitAttempts = 1 },
		func(p *competitionHeartbeatDatabaseError) { p.snapshot.Transaction.CommitAcknowledged = 1 },
		func(p *competitionHeartbeatDatabaseError) { p.snapshot.Transaction.LastCommit.Returned = true },
		func(p *competitionHeartbeatDatabaseError) { p.snapshot.Transaction.RollbackAttempts++ },
		func(p *competitionHeartbeatDatabaseError) { p.snapshot.Transaction.RollbackAcknowledged = 0 },
		func(p *competitionHeartbeatDatabaseError) { p.snapshot.Transaction.LastRollback.Returned = false },
		func(p *competitionHeartbeatDatabaseError) { p.snapshot.Transaction.ReleaseAttempts++ },
		func(p *competitionHeartbeatDatabaseError) { p.snapshot.Transaction.ReleaseReturned = 0 },
		func(p *competitionHeartbeatDatabaseError) {
			p.snapshot.Transaction.LastRelease.Disposition = server.DbTxReleaseUnknown
		},
		func(p *competitionHeartbeatDatabaseError) {
			p.snapshot.Transaction.LastRelease.Disposition = server.DbTxReleaseDestroyTransferred
		},
		func(p *competitionHeartbeatDatabaseError) { p.snapshot.AtEntry.State = "deadline" },
		func(p *competitionHeartbeatDatabaseError) { p.snapshot.AtReturn.State = "live" },
		func(p *competitionHeartbeatDatabaseError) { p.snapshot.Acquire.Admitted = false },
		func(p *competitionHeartbeatDatabaseError) { p.snapshot.Acquire.Returned = false },
		func(p *competitionHeartbeatDatabaseError) { p.snapshot.Acquire.BeforeAcquire.HasDeadline = false },
		func(p *competitionHeartbeatDatabaseError) { p.snapshot.Acquire.AtReturn.State = "deadline" },
		func(p *competitionHeartbeatDatabaseError) { p.snapshot.Transaction.LastBegin.Returned = false },
		func(p *competitionHeartbeatDatabaseError) {
			p.snapshot.Transaction.LastBegin.AtReturn.State = "deadline"
		},
		func(p *competitionHeartbeatDatabaseError) {
			p.snapshot.Transaction.LastRollback.Before.HasDeadline = false
		},
		func(p *competitionHeartbeatDatabaseError) {
			p.snapshot.Transaction.LastRollback.AtReturn.State = "deadline"
		},
		func(p *competitionHeartbeatDatabaseError) { p.snapshot.Statements = p.snapshot.Statements[:2] },
		func(p *competitionHeartbeatDatabaseError) { p.snapshot.Statements[0].Count++ },
		func(p *competitionHeartbeatDatabaseError) { p.snapshot.Statements[0].Returned = false },
		func(p *competitionHeartbeatDatabaseError) { p.snapshot.Statements[0].Succeeded = false },
		func(p *competitionHeartbeatDatabaseError) { p.snapshot.Statements[1].Ordinal++ },
		func(p *competitionHeartbeatDatabaseError) { p.snapshot.Statements[1].Kind = "exec" },
		func(p *competitionHeartbeatDatabaseError) { p.snapshot.Statements[2].Succeeded = true },
		func(p *competitionHeartbeatDatabaseError) { p.snapshot.Statements[2].AtReturn = nil },
		func(p *competitionHeartbeatDatabaseError) { p.snapshot.Statements[2].Entered.State = "deadline" },
		func(p *competitionHeartbeatDatabaseError) {
			p.snapshot.Statements[2].AtReturn = &server.DbContextBudget{State: "live", HasDeadline: true, RemainingNs: 1}
		},
		func(p *competitionHeartbeatDatabaseError) {
			p.cause = competitionDatabasePhaseError{cause: context.DeadlineExceeded, phase: server.DbOperationCommit}
		},
		func(p *competitionHeartbeatDatabaseError) {
			// A later failed acquire may leave the earlier callback's Step3.
			p.snapshot.Acquire.Attempt++
			p.snapshot.Transaction.AcquireAttempts++
			p.snapshot.Acquire.Admitted = false
			p.cause = competitionDatabasePhaseError{cause: context.DeadlineExceeded, phase: server.DbOperationAcquire}
		},
	} {
		proof := competitionHeartbeatJobLockFailure(context.DeadlineExceeded)
		change(&proof)
		if competitionHeartbeatJobLockDeadline(proof) {
			t.Fatal("inexact read-only/no-COMMIT history authorized continuation", proof.snapshot)
		}
	}
}

// The exact returned private root must own every cause. A persisted taxonomy,
// network timeout, unknown wrapper or joined unrelated error is not a proof.
func TestCompetitionHeartbeatJobLockProofRejectsMixedUnknownAndOtherAuthorityCauses(t *testing.T) {
	for _, cause := range []error{context.Canceled, ErrLeaseLost, errors.New("synthetic unrelated failure"),
		errors.Join(context.DeadlineExceeded, context.Canceled), errors.Join(context.DeadlineExceeded, ErrLeaseLost),
		errors.Join(context.DeadlineExceeded, &pgconn.PgError{Code: "55P03"}),
		&net.OpError{Op: "read", Net: "synthetic", Err: context.DeadlineExceeded},
		competitionSupervisorUnknownDeadline{cause: context.DeadlineExceeded},
		errors.Join(context.DeadlineExceeded, &competitionLeaseRetryCycleError{}),
		server.NewErrorCauseBatch([]error{context.DeadlineExceeded, nil}),
	} {
		if competitionHeartbeatJobLockDeadline(competitionHeartbeatJobLockFailure(cause)) {
			t.Fatalf("another cause graph borrowed the job-lock proof: %T", cause)
		}
	}
	proof := competitionHeartbeatJobLockFailure(context.DeadlineExceeded)
	if competitionHeartbeatJobLockDeadline(errors.Join(proof, context.DeadlineExceeded)) || competitionHostHeartbeatUnadmittedDeadline(proof) || competitionSupervisorUnadmittedDeadline(proof) {
		t.Fatal("another root or authority borrowed the job-only proof")
	}
}

// Actual observer delegation must reject changed SQL before a valid semantic
// label could qualify. This also exercises manual/nested control invalidation.
func TestCompetitionHeartbeatJobLockSqlPrefixRejectsChangedReorderedAndManualWork(t *testing.T) {
	for _, mode := range []string{"exact", "changed", "reordered", "trailing commit", "manual commit", "raw connection"} {
		original := &competitionHeartbeatSurfaceTx{}
		observation := &competitionHeartbeatDatabaseObservation{sqlShape: competitionHeartbeatLegacySlotShape, sqlValid: true}
		wrapped := observation.beginCallback(original)
		for index, expected := range competitionHeartbeatLegacySlotStatements()[:competitionHeartbeatJobLockPrefixLength] {
			observation.enterStep(expected.step)
			sql := expected.sql
			if index == 2 {
				switch mode {
				case "changed":
					sql = "SELECT lease_expires_at FROM synthetic_other_job FOR UPDATE"
				case "reordered":
					sql = competitionHeartbeatLegacySlotStatements()[1].sql
				case "trailing commit":
					sql += "; COMMIT"
				}
			}
			if expected.kind == "exec" {
				_, _ = wrapped.Exec(t.Context(), sql)
			} else {
				_ = wrapped.QueryRow(t.Context(), sql).Scan()
			}
		}
		if mode == "manual commit" {
			_ = wrapped.Commit(t.Context())
		} else if mode == "raw connection" {
			_ = wrapped.Conn()
		}
		proof := competitionHeartbeatJobLockFailure(context.DeadlineExceeded)
		proof.snapshot.SqlShapeValid, proof.snapshot.SqlStatements, proof.snapshot.UnknownStatements = observation.sqlValid, observation.sqlCount, observation.unknown
		if competitionHeartbeatJobLockDeadline(proof) != (mode == "exact") || original.calls < 3 {
			t.Fatal("an unreviewed SQL/manual operation retained prefix authority", mode, observation.sqlValid, observation.unknown)
		}
	}
}

// Cleanup consumes the original reserve. No fresh total clock or fourth call
// is supplied when a prefix failure returns near its existing owner deadline.
func TestCompetitionHeartbeatJobLockContinuationKeepsOriginalTotalBudget(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		started := time.Now()
		settings := defaultCompetitionHeartbeatSettings()
		settings.LeaseRetry.Now = time.Now
		calls := 0
		ack, err := retryCompetitionJobHeartbeatOperation(t.Context(), started.Add(time.Minute), started.Add(time.Minute), settings, func(attempt context.Context) error {
			calls++
			if calls == 1 {
				<-attempt.Done()
				time.Sleep(5 * time.Second)
				return competitionHeartbeatJobLockFailure(attempt.Err())
			}
			deadline, ok := attempt.Deadline()
			if !ok || !deadline.Equal(started.Add(settings.AcquireRetry.Timeout)) || time.Until(deadline) >= settings.AcquireRetry.AttemptTimeout {
				t.Fatal("prefix continuation reset the original total budget", deadline)
			}
			return nil
		})
		if err != nil || calls != 2 || ack.Sub(started) != 13*time.Second+settings.LeaseRetry.Interval {
			t.Fatal("read-only prefix did not continue inside its original reserve", calls, ack.Sub(started), err)
		}
	})
}

// Every new continuation spends the same pre-existing shared call cap.
func TestCompetitionHeartbeatJobLockContinuationKeepsThreeCallMaximum(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		settings := defaultCompetitionHeartbeatSettings()
		settings.LeaseRetry.Now = time.Now
		settings.AcquireRetry.AttemptTimeout = time.Second
		expires, calls := time.Now().Add(time.Minute), 0
		_, err := retryCompetitionJobHeartbeatOperation(t.Context(), expires, expires, settings, func(attempt context.Context) error {
			calls++
			<-attempt.Done()
			return competitionHeartbeatJobLockFailure(attempt.Err())
		})
		if err == nil || calls != settings.LeaseRetry.MaximumAttempts || calls != 3 {
			t.Fatal("prefix continuation replenished the shared call cap", calls, err)
		}
	})
}

// Both confirmed authorities and the original parent/total lifetime win,
// including cleanup that crosses expiry and success which returns too late.
func TestCompetitionHeartbeatJobLockContinuationRejectsExpiredAuthorityAndLateSuccess(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		for _, mode := range []string{"host", "job", "parent", "cancel", "total", "late success"} {
			started := time.Now()
			settings := defaultCompetitionHeartbeatSettings()
			settings.LeaseRetry.Now = time.Now
			job, host := started.Add(time.Minute), started.Add(time.Minute)
			parent, cancel := context.WithCancel(t.Context())
			if mode == "host" || mode == "late success" {
				host = started.Add(10 * time.Second)
			} else if mode == "job" {
				job = started.Add(10 * time.Second)
			} else if mode == "parent" {
				cancel()
				parent, cancel = context.WithDeadline(t.Context(), started.Add(10*time.Second))
			}
			calls := 0
			ack, err := retryCompetitionJobHeartbeatOperation(parent, job, host, settings, func(attempt context.Context) error {
				calls++
				<-attempt.Done()
				if mode == "cancel" {
					cancel()
				}
				if mode == "total" {
					time.Sleep(settings.AcquireRetry.Timeout)
				} else {
					time.Sleep(5 * time.Second)
				}
				if mode == "late success" {
					return nil
				}
				return competitionHeartbeatJobLockFailure(context.DeadlineExceeded)
			})
			cancel()
			if err == nil || calls != 1 || !ack.IsZero() {
				t.Fatal("expired authority dispatched/adopted a successor", mode, calls, ack, err)
			}
		}
	})
}

// Unknown bounds and non-job entrypoints cannot opt into an admitted prefix.
func TestCompetitionHeartbeatJobLockContinuationRequiresBothJobAndHostAuthority(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		for _, mode := range []string{"host", "acquire-only", "unknown-host", "unknown-job"} {
			settings := defaultCompetitionHeartbeatSettings()
			settings.LeaseRetry.Now = time.Now
			expires := time.Now().Add(time.Minute)
			calls := 0
			operation := func(attempt context.Context) error {
				calls++
				<-attempt.Done()
				return competitionHeartbeatJobLockFailure(attempt.Err())
			}
			var err error
			switch mode {
			case "host":
				_, err = retryCompetitionHeartbeatOperation(t.Context(), expires, settings, true, operation)
			case "acquire-only":
				_, err = retryCompetitionHeartbeatOperation(t.Context(), expires, settings, false, operation)
			case "unknown-host":
				_, err = retryCompetitionJobHeartbeatOperation(t.Context(), expires, time.Time{}, settings, operation)
			case "unknown-job":
				_, err = retryCompetitionJobHeartbeatOperation(t.Context(), time.Time{}, expires, settings, operation)
			}
			if err == nil || calls != 1 {
				t.Fatal("another authority borrowed job-prefix continuation", mode, calls, err)
			}
		}
	})
}

// The real worker selects the new adapter without recreating its evaluator,
// claim, original deadline or recovery-cycle clock after internal acquisition.
func TestCompetitionWorkerJobLockContinuationKeepsEvaluatorAndOriginalClock(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		fixture := newCompetitionLeaseRetryWorkerFixture(t)
		watchdog, stopWatchdog := context.WithTimeout(t.Context(), time.Minute)
		defer stopWatchdog()
		fixture.watchdog = watchdog
		fixture.worker.confirmedHostLeaseExpiresAt = fixture.clock.Now().Add(time.Minute)
		originalStarted, originalAttempt := *fixture.job.StartedAt, fixture.job.AttemptCount
		acknowledged := make(chan struct{}, 1)
		fixture.worker.evaluationHeartbeatForTest = func() { acknowledged <- struct{}{} }
		fixture.store.heartbeat = func(ctx context.Context, call int) error {
			if call == 1 {
				<-ctx.Done()
				return competitionHeartbeatJobLockFailure(ctx.Err())
			}
			return nil
		}
		fixture.Start(t)
		fixture.ticks <- fixture.clock.Now()
		select {
		case <-acknowledged:
		case <-fixture.evaluator.canceled:
			t.Fatal("closed read-only prefix canceled the original evaluator")
		case <-watchdog.Done():
			t.Fatal("worker did not dispatch the exact job-prefix successor")
		}
		deadline, ok := fixture.evaluator.evaluationCtx.Deadline()
		if !ok || !deadline.Equal(originalStarted.Add(time.Duration(fixture.worker.settings.EvaluationPolicy.ScoreTimeoutSeconds)*time.Second)) ||
			fixture.evaluator.evaluationCtx.Err() != nil || fixture.evaluator.calls != 1 || fixture.store.heartbeatCalls != 2 || fixture.store.hostCalls != 1 ||
			fixture.job.AttemptCount != originalAttempt || !fixture.job.StartedAt.Equal(originalStarted) || fixture.store.reason != "" {
			t.Fatal("prefix continuation changed claim/evaluator/lifetime or handed back healthy work")
		}
		fixture.cancel()
		if err := fixture.Finish(t); !errors.Is(err, context.Canceled) || !fixture.store.joined || fixture.store.reason != "worker_shutdown" {
			t.Fatal("later shutdown did not join the original evaluator", err)
		}
	})
}

// Even an otherwise exact prefix cannot hide a possible COMMIT. The public
// worker must join its evaluator before returning any such authority to FIFO.
func TestCompetitionWorkerJobLockContinuationRejectsCommitAndJoinsBeforeHandback(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		fixture := newCompetitionLeaseRetryWorkerFixture(t)
		watchdog, stopWatchdog := context.WithTimeout(t.Context(), time.Minute)
		defer stopWatchdog()
		fixture.watchdog = watchdog
		fixture.worker.confirmedHostLeaseExpiresAt = fixture.clock.Now().Add(time.Minute)
		fixture.store.heartbeat = func(ctx context.Context, _ int) error {
			<-ctx.Done()
			proof := competitionHeartbeatJobLockFailure(ctx.Err())
			proof.snapshot.Transaction.CommitAttempts = 1
			return proof
		}
		fixture.Start(t)
		fixture.ticks <- fixture.clock.Now()
		select {
		case <-fixture.evaluator.canceled:
		case <-watchdog.Done():
			t.Fatal("possible COMMIT did not terminate the owning evaluator")
		}
		if fixture.store.reason != "" || fixture.evaluator.joined.Load() {
			t.Fatal("failure handback overtook explicit evaluator cleanup")
		}
		if err := fixture.Finish(t); err == nil || fixture.store.heartbeatCalls != 1 || !fixture.store.joined || fixture.store.reason != "heartbeat_failed" {
			t.Fatal("possible COMMIT replayed or bypassed joined handback", err)
		}
	})
}
