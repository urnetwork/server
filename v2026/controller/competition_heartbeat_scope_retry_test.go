// Virtual time and exact returned SQL evidence reproduce early custody loss
// without a production identity, scheduler race or elapsed-clock extension.
package controller

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"testing/synctest"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/urnetwork/server/v2026"
)

// A single admitted first statement failed; rollback and pool return are
// acknowledged. CleanupComplete intentionally stays false for an idle return.
func competitionHeartbeatScopeFailure() competitionHeartbeatDatabaseError {
	proof := competitionHeartbeatSlotFailure(&pgconn.PgError{Code: "55P03"}, server.DbTxReleaseIdle)
	live := server.DbContextBudget{State: "live", HasDeadline: true, RemainingNs: int64(time.Second)}
	proof.snapshot.AtReturn, proof.snapshot.Step = live, competitionHeartbeatDatabaseScopeLock
	proof.snapshot.SqlShape, proof.snapshot.SqlStatements = competitionHeartbeatScopeLockShape, 1
	proof.snapshot.Statements = proof.snapshot.Statements[:1]
	proof.snapshot.Statements[0].Succeeded, proof.snapshot.Statements[0].AtReturn = false, &live
	return proof
}

func TestCompetitionHeartbeatScopeRetryRecoversFourthAttemptInsideOriginalOwner(t *testing.T) {
	for _, historical := range []bool{true, false} {
		synctest.Test(t, func(t *testing.T) {
			started := time.Now()
			settings := defaultCompetitionHeartbeatSettings()
			settings.LeaseRetry.Now = time.Now
			if historical {
				settings.ScopeLockMaximumAttempts = 0
			}
			calls := 0
			ack, err := retryCompetitionJobHeartbeatOperation(t.Context(), started.Add(time.Minute), started.Add(time.Minute), settings, func(attempt context.Context) error {
				calls++
				deadline, ok := attempt.Deadline()
				if !ok || deadline.After(started.Add(settings.AcquireRetry.Timeout)) {
					t.Fatal("attempt created a new owner allowance", deadline)
				}
				if calls <= 3 {
					time.Sleep(2 * time.Second)
					return competitionHeartbeatScopeFailure()
				}
				return nil
			})
			if historical {
				if err == nil || calls != 3 || !ack.IsZero() || time.Since(started) != 6*time.Second+2*settings.LeaseRetry.Interval {
					t.Fatal("control did not reproduce original premature three-call refusal", calls, err)
				}
			} else if err != nil || calls != 4 || !ack.Equal(started.Add(6*time.Second+3*settings.LeaseRetry.Interval)) {
				t.Fatal("proved fourth heartbeat could not retain original custody", calls, ack, err)
			}
		})
	}
}

func TestCompetitionHeartbeatScopeRetryUsesOwnedReserveWithoutResettingDeadline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		started := time.Now()
		settings := defaultCompetitionHeartbeatSettings()
		settings.LeaseRetry.Now = time.Now
		calls := 0
		ack, err := retryCompetitionJobHeartbeatOperation(t.Context(), started.Add(time.Minute), started.Add(time.Minute), settings, func(attempt context.Context) error {
			calls++
			if calls <= 4 {
				time.Sleep(2 * time.Second)
				if attempt.Err() != nil {
					t.Fatal("exact scope proof retained the expired historical lock parent", calls, attempt.Err())
				}
				return competitionHeartbeatScopeFailure()
			}
			return nil
		})
		if err != nil || calls != 5 || ack.Sub(started) != 8*time.Second+4*settings.LeaseRetry.Interval || time.Since(started) >= settings.AcquireRetry.Timeout {
			t.Fatal("scoped reserve reset, lost or falsely acknowledged its owner", calls, ack, err)
		}
	})
}

func TestCompetitionHeartbeatScopeRetryKeepsFiniteTotalCallCapAndUnchangedOtherOwners(t *testing.T) {
	for _, mode := range []string{"job", "host", "plain", "unknown-host", "unknown-job"} {
		synctest.Test(t, func(t *testing.T) {
			settings := defaultCompetitionHeartbeatSettings()
			settings.LeaseRetry.Now = time.Now
			started, calls := time.Now(), 0
			expires := started.Add(time.Minute)
			operation := func(context.Context) error { calls++; return competitionHeartbeatScopeFailure() }
			var ack time.Time
			var err error
			if mode == "host" {
				ack, err = retryCompetitionHeartbeatOperation(t.Context(), expires, settings, true, operation)
			} else if mode == "plain" {
				ack, err = retryCompetitionLeaseOperation(t.Context(), expires, settings.LeaseRetry, operation)
			} else {
				job, host := expires, expires
				if mode == "unknown-host" {
					host = time.Time{}
				}
				if mode == "unknown-job" {
					job = time.Time{}
				}
				ack, err = retryCompetitionJobHeartbeatOperation(t.Context(), job, host, settings, operation)
			}
			maximum := settings.LeaseRetry.MaximumAttempts
			if mode == "job" {
				maximum = settings.ScopeLockMaximumAttempts
			}
			if mode == "unknown-job" {
				maximum = 1
			}
			if err == nil || !ack.IsZero() || calls != maximum || time.Since(started) != time.Duration(maximum-1)*settings.LeaseRetry.Interval {
				t.Fatal("scoped retries escaped a finite cap or changed another owner", mode, calls, maximum, err)
			}
		})
	}
}

func TestCompetitionHeartbeatScopeRetryHonorsConfirmedAuthorityCancellationAndLateAck(t *testing.T) {
	for _, mode := range []string{"expired-host", "expired-job", "host", "job", "parent", "cancel", "total", "late-ack"} {
		synctest.Test(t, func(t *testing.T) {
			started := time.Now()
			settings := defaultCompetitionHeartbeatSettings()
			settings.LeaseRetry.Now = time.Now
			settings.ScopeLockMaximumAttempts = 30
			job, host := started.Add(time.Minute), started.Add(time.Minute)
			parent, cancel := context.WithCancel(t.Context())
			defer cancel()
			if mode == "expired-host" {
				host = started
			}
			if mode == "expired-job" {
				job = started
			}
			if mode == "host" {
				host = started.Add(5 * time.Second)
			}
			if mode == "job" {
				job = started.Add(5 * time.Second)
			}
			if mode == "parent" {
				parent, cancel = context.WithTimeout(parent, 5*time.Second)
				defer cancel()
			}
			calls := 0
			ack, err := retryCompetitionJobHeartbeatOperation(parent, job, host, settings, func(attempt context.Context) error {
				calls++
				if mode == "cancel" {
					cancel()
					return nil
				}
				if mode == "late-ack" {
					<-attempt.Done()
					return nil
				}
				if mode == "total" && calls == 9 {
					<-attempt.Done()
					return competitionHeartbeatScopeFailure()
				}
				time.Sleep(2 * time.Second)
				return competitionHeartbeatScopeFailure()
			})
			if err == nil || !ack.IsZero() || time.Since(started) > settings.AcquireRetry.Timeout || calls > settings.ScopeLockMaximumAttempts {
				t.Fatal("scoped retry renewed an expired, canceled or late authority", mode, calls, err)
			}
			if (mode == "expired-host" || mode == "expired-job") && calls != 0 {
				t.Fatal("expired authority dispatched", mode, calls)
			}
			if mode == "total" && time.Since(started) != settings.AcquireRetry.Timeout {
				t.Fatal("total reserve was reset or abandoned", time.Since(started))
			}
		})
	}
}

func TestCompetitionHeartbeatScopeProofAuthenticatesEveryReturnAndRejectsAmbiguity(t *testing.T) {
	valid := competitionHeartbeatScopeFailure()
	if !competitionHeartbeatScopeLockTimeout(valid) || valid.snapshot.Transaction.LastRelease.CleanupComplete {
		t.Fatal("normal returned-idle pool custody was confused with physical cleanup")
	}
	if cloned, omitted := cloneCompetitionHeartbeatOwnerDatabase(&valid.snapshot); omitted || cloned == nil || cloned.SqlShape != competitionHeartbeatScopeLockShape {
		t.Fatal("new finite source shape was lost from owner diagnostics")
	}
	for index, change := range []func(*competitionHeartbeatDatabaseError){
		func(p *competitionHeartbeatDatabaseError) { p.snapshot.SqlShape = "" },
		func(p *competitionHeartbeatDatabaseError) { p.snapshot.SqlShapeValid = false },
		func(p *competitionHeartbeatDatabaseError) { p.snapshot.SqlStatements++ },
		func(p *competitionHeartbeatDatabaseError) { p.snapshot.CallbackEntries++ },
		func(p *competitionHeartbeatDatabaseError) { p.snapshot.Step = competitionHeartbeatDatabaseJobUpdate },
		func(p *competitionHeartbeatDatabaseError) { p.snapshot.UnknownStatements++ },
		func(p *competitionHeartbeatDatabaseError) { p.snapshot.AtReturn.State = "deadline" },
		func(p *competitionHeartbeatDatabaseError) { p.snapshot.Acquire.Admitted = false },
		func(p *competitionHeartbeatDatabaseError) { p.snapshot.Transaction.AcquireSucceeded++ },
		func(p *competitionHeartbeatDatabaseError) { p.snapshot.Transaction.BeginAttempts++ },
		func(p *competitionHeartbeatDatabaseError) { p.snapshot.Transaction.CallbackEntered++ },
		func(p *competitionHeartbeatDatabaseError) { p.snapshot.Transaction.CommitAttempts++ },
		func(p *competitionHeartbeatDatabaseError) { p.snapshot.Transaction.LastCommit.Returned = true },
		func(p *competitionHeartbeatDatabaseError) { p.snapshot.Transaction.RollbackAcknowledged = 0 },
		func(p *competitionHeartbeatDatabaseError) { p.snapshot.Transaction.LastRollback.Returned = false },
		func(p *competitionHeartbeatDatabaseError) { p.snapshot.Transaction.ReleaseReturned = 0 },
		func(p *competitionHeartbeatDatabaseError) {
			p.snapshot.Transaction.LastRelease.Disposition = server.DbTxReleaseUnknown
		},
		func(p *competitionHeartbeatDatabaseError) {
			p.snapshot.Transaction.LastRelease.Disposition = server.DbTxReleaseDestroyTransferred
		},
		func(p *competitionHeartbeatDatabaseError) { p.snapshot.Statements[0].Succeeded = true },
		func(p *competitionHeartbeatDatabaseError) { p.snapshot.Statements[0].Count++ },
		func(p *competitionHeartbeatDatabaseError) { p.snapshot.Statements[0].AtReturn = nil },
		func(p *competitionHeartbeatDatabaseError) { p.snapshot.Statements[0].Kind = "row_scan" },
		func(p *competitionHeartbeatDatabaseError) {
			p.cause = competitionDatabasePhaseError{cause: &pgconn.PgError{Code: "55P03"}, phase: server.DbOperationCommit}
		},
		func(p *competitionHeartbeatDatabaseError) {
			p.cause = competitionDatabasePhaseError{cause: context.DeadlineExceeded, phase: server.DbOperationCallback}
		},
		func(p *competitionHeartbeatDatabaseError) {
			p.cause = competitionDatabasePhaseError{cause: errors.Join(&pgconn.PgError{Code: "55P03"}, ErrLeaseLost), phase: server.DbOperationCallback}
		},
		func(p *competitionHeartbeatDatabaseError) {
			p.cause = competitionDatabasePhaseError{cause: competitionSupervisorUnknownDeadline{cause: &pgconn.PgError{Code: "55P03"}}, phase: server.DbOperationCallback}
		},
	} {
		proof := competitionHeartbeatScopeFailure()
		change(&proof)
		if competitionHeartbeatScopeLockTimeout(proof) {
			t.Fatal("inexact scope-lock proof authorized additional retry", index)
		}
	}
	valid.cause = competitionDatabasePhaseError{cause: fmt.Errorf("synthetic transparent wrapper: %w", &pgconn.PgError{Code: "55P03"}), phase: server.DbOperationCallback}
	if !competitionHeartbeatScopeLockTimeout(valid) {
		t.Fatal("standard transparent source cause lost exact lock classification")
	}
}

func TestCompetitionHeartbeatScopeSqlProofRejectsChangedAndLaterStatements(t *testing.T) {
	for _, mode := range []string{"exact", "changed", "later"} {
		observation := &competitionHeartbeatDatabaseObservation{sqlShape: competitionHeartbeatScopeLockShape, sqlValid: true}
		observation.enterStep(competitionHeartbeatDatabaseScopeLock)
		statement := observation.beginStatement(t.Context(), "exec")
		sql := competitionHeartbeatLegacySlotStatements()[0].sql
		if mode == "changed" {
			sql = "SELECT pg_advisory_xact_lock(hashtextextended($1,1))"
		}
		observation.observeSql(statement, sql)
		if mode == "later" {
			observation.enterStep(competitionHeartbeatDatabaseSlotLock)
			observation.observeSql(observation.beginStatement(t.Context(), "row_scan"), "SELECT 1")
		}
		if observation.sqlValid != (mode == "exact") {
			t.Fatal("scope proof did not authenticate exact first source statement", mode)
		}
	}
}
