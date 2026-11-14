// Every failed-last statement in the exact distributed source graph shares
// the same returned rollback proof. Later callback/commit failures never do.
package controller

import (
	"context"
	"errors"
	"slices"
	"testing"
	"testing/synctest"
	"time"

	"github.com/urnetwork/server/v2026"
)

// Build canonical step-ordered diagnostics for any known source prefix. No
// production identity, argument, timestamp or private failure is retained.
func competitionHeartbeatDistributedStatementFailure(count int) competitionHeartbeatDatabaseError {
	proof := competitionHeartbeatDistributedSlotFailure()
	expected := competitionHeartbeatDistributedSlotStatements()[:count]
	proof.snapshot.SqlStatements = uint64(count)
	proof.snapshot.Step = expected[count-1].step
	proof.snapshot.Statements = nil
	ordinals := map[competitionHeartbeatDatabaseStep]uint8{}
	for index, statement := range expected {
		ordinals[statement.step]++
		returned := proof.snapshot.AtEntry
		if index == count-1 {
			returned = proof.snapshot.AtReturn
		}
		proof.snapshot.Statements = append(proof.snapshot.Statements, competitionHeartbeatStatementObservation{Step: statement.step,
			Ordinal: ordinals[statement.step], Kind: statement.kind, Count: 1, Entered: proof.snapshot.AtEntry,
			Returned: true, Succeeded: index != count-1, AtReturn: &returned})
	}
	slices.SortFunc(proof.snapshot.Statements, func(left, right competitionHeartbeatStatementObservation) int {
		if left.Step != right.Step {
			return int(left.Step) - int(right.Step)
		}
		return int(left.Ordinal) - int(right.Ordinal)
	})
	return proof
}

// The compatibility helpers retain their narrower meaning while the job-only
// continuation authenticates the entire known failed-last source graph.
func TestCompetitionHeartbeatDistributedAllStatementsProofAcceptsEveryKnownFailedLastPosition(t *testing.T) {
	for count := 1; count <= len(competitionHeartbeatDistributedSlotStatements()); count++ {
		proof := competitionHeartbeatDistributedStatementFailure(count)
		if !competitionHeartbeatDistributedReturnedRollbackDeadline(proof) || competitionHeartbeatDistributedPreBusinessDeadline(proof) != (count <= len(competitionHeartbeatDistributedPreBusinessStatements())) ||
			competitionHeartbeatDistributedSlotUpdateDeadline(proof) != (count == len(competitionHeartbeatDistributedSlotStatements())) {
			t.Fatal("known failed-last position was rejected or relabeled", count)
		}
	}
	job := competitionHeartbeatDistributedStatementFailure(11)
	if job.snapshot.Step != competitionHeartbeatDatabaseJobUpdate || job.snapshot.Statements[10].Succeeded || len(job.snapshot.Statements) != 11 {
		t.Fatal("fixture did not exercise failed job UPDATE before any slot UPDATE")
	}
}

// Both the missing position class and the old cumulative three-call ceiling
// lose live custody before the same original reserve can reach call four.
func TestCompetitionHeartbeatDistributedAllStatementsJobUpdateFourthAttempt(t *testing.T) {
	for _, mode := range []string{"old-proof", "old-cap", "fixed"} {
		synctest.Test(t, func(t *testing.T) {
			settings := defaultCompetitionHeartbeatSettings()
			settings.LeaseRetry.Now = time.Now
			settings.AcquireRetry.AttemptTimeout = time.Second
			if mode == "old-cap" {
				settings.DistributedReturnedRollbackMaximumAttempts = settings.LeaseRetry.MaximumAttempts
			}
			started, calls := time.Now(), 0
			operation := func(attempt context.Context) error {
				calls++
				if calls <= 3 {
					<-attempt.Done()
					return competitionHeartbeatDistributedStatementFailure(11)
				}
				return nil
			}
			var ack time.Time
			var err error
			if mode == "old-proof" {
				ack, err = retryCompetitionHeartbeatWithProof(t.Context(), started.Add(time.Minute), settings, operation, func(err error) bool {
					return competitionHeartbeatDistributedPreBusinessDeadline(err) || competitionHeartbeatDistributedSlotUpdateDeadline(err)
				})
			} else {
				ack, err = retryCompetitionJobHeartbeatOperation(t.Context(), started.Add(time.Minute), started.Add(time.Minute), settings, operation)
			}
			if mode == "fixed" {
				if err != nil || calls != 4 || ack.IsZero() || time.Since(started) >= settings.AcquireRetry.Timeout {
					t.Fatal("fourth job UPDATE renewal lost its original reserve", calls, err)
				}
			} else {
				maximum := 1
				if mode == "old-cap" {
					maximum = settings.LeaseRetry.MaximumAttempts
				}
				if err == nil || calls != maximum || !ack.IsZero() || time.Since(started) >= settings.AcquireRetry.Timeout {
					t.Fatal("historical missing-position/cap did not reproduce premature handback", mode, calls, err)
				}
			}
		})
	}
}

// The new generic recognizer does not alter flag-zero historical full-slot
// behavior or manufacture either of the required confirmed authorities.
func TestCompetitionHeartbeatDistributedAllStatementsRequiresEnabledConfirmedAuthority(t *testing.T) {
	for _, mode := range []string{"disabled-job", "disabled-slot", "unknown-job", "unknown-host", "expired-job", "expired-host", "late-ack", "parent-cancel"} {
		synctest.Test(t, func(t *testing.T) {
			settings := defaultCompetitionHeartbeatSettings()
			settings.LeaseRetry.Now = time.Now
			settings.AcquireRetry.AttemptTimeout = time.Second
			started := time.Now()
			job, host := started.Add(time.Minute), started.Add(time.Minute)
			if mode == "disabled-job" || mode == "disabled-slot" {
				settings.DistributedReturnedRollbackMaximumAttempts = 0
			}
			if mode == "unknown-job" {
				job = time.Time{}
			}
			if mode == "unknown-host" {
				host = time.Time{}
			}
			if mode == "expired-job" {
				job = started
			}
			if mode == "expired-host" {
				host = started
			}
			parent, cancel := context.WithCancel(t.Context())
			defer cancel()
			calls := 0
			ack, err := retryCompetitionJobHeartbeatOperation(parent, job, host, settings, func(attempt context.Context) error {
				calls++
				if mode == "parent-cancel" {
					cancel()
					return competitionHeartbeatDistributedStatementFailure(11)
				}
				<-attempt.Done()
				if mode == "late-ack" {
					return nil
				}
				if mode == "disabled-slot" {
					return competitionHeartbeatDistributedStatementFailure(12)
				}
				return competitionHeartbeatDistributedStatementFailure(11)
			})
			maximum := 1
			if mode == "disabled-slot" {
				maximum = settings.LeaseRetry.MaximumAttempts
			}
			if mode == "expired-job" || mode == "expired-host" {
				maximum = 0
			}
			if err == nil || !ack.IsZero() || calls != maximum {
				t.Fatal("known source proof borrowed disabled, expired, unknown or late authority", mode, calls, err)
			}
		})
	}
}

// One failed last statement is essential. An all-success prefix followed by
// another callback error, manual SQL, nested work or any commit stays refused.
func TestCompetitionHeartbeatDistributedAllStatementsProofRefusesOtherFailureBoundaries(t *testing.T) {
	for index, mutate := range []func(*competitionHeartbeatDatabaseError){
		func(p *competitionHeartbeatDatabaseError) { p.snapshot.SqlStatements = 0 },
		func(p *competitionHeartbeatDatabaseError) {
			p.snapshot.SqlStatements = uint64(len(competitionHeartbeatDistributedSlotStatements()) + 1)
		},
		func(p *competitionHeartbeatDatabaseError) { p.snapshot.SqlShape = competitionHeartbeatLegacySlotShape },
		func(p *competitionHeartbeatDatabaseError) { p.snapshot.SqlShapeValid = false },
		func(p *competitionHeartbeatDatabaseError) {
			p.snapshot.Statements[10].Succeeded = true
			p.snapshot.Statements[10].AtReturn = &p.snapshot.AtEntry
		},
		func(p *competitionHeartbeatDatabaseError) { p.snapshot.Statements[9].Succeeded = false },
		func(p *competitionHeartbeatDatabaseError) { p.snapshot.Statements[10].Returned = false },
		func(p *competitionHeartbeatDatabaseError) { p.snapshot.Statements[10].AtReturn = nil },
		func(p *competitionHeartbeatDatabaseError) { p.snapshot.Statements[10].Ordinal++ },
		func(p *competitionHeartbeatDatabaseError) { p.snapshot.UnknownStatements++ },
		func(p *competitionHeartbeatDatabaseError) { p.snapshot.CallbackEntries++ },
		func(p *competitionHeartbeatDatabaseError) { p.snapshot.Transaction.CallbackEntered++ },
		func(p *competitionHeartbeatDatabaseError) { p.snapshot.Transaction.BeginAttempts++ },
		func(p *competitionHeartbeatDatabaseError) { p.snapshot.Transaction.CommitAttempts++ },
		func(p *competitionHeartbeatDatabaseError) { p.snapshot.Transaction.LastCommit.Returned = true },
		func(p *competitionHeartbeatDatabaseError) { p.snapshot.Transaction.RollbackAcknowledged = 0 },
		func(p *competitionHeartbeatDatabaseError) { p.snapshot.Transaction.ReleaseReturned = 0 },
		func(p *competitionHeartbeatDatabaseError) {
			p.snapshot.Transaction.LastRelease.Disposition = server.DbTxReleaseDestroyTransferred
		},
		func(p *competitionHeartbeatDatabaseError) {
			p.cause = competitionDatabasePhaseError{cause: context.DeadlineExceeded, phase: server.DbOperationCommit}
		},
		func(p *competitionHeartbeatDatabaseError) {
			p.cause = competitionDatabasePhaseError{cause: errors.Join(context.DeadlineExceeded, context.Canceled), phase: server.DbOperationCallback}
		},
	} {
		proof := competitionHeartbeatDistributedStatementFailure(11)
		mutate(&proof)
		if competitionHeartbeatDistributedReturnedRollbackDeadline(proof) {
			t.Fatal("incomplete or different failure boundary authorized generic continuation", index)
		}
	}
	proof := competitionHeartbeatDistributedStatementFailure(11)
	if competitionHeartbeatDistributedReturnedRollbackDeadline(errors.Join(proof, context.DeadlineExceeded)) || competitionHeartbeatDistributedReturnedRollbackDeadline(&proof) ||
		competitionHeartbeatDistributedReturnedRollbackDeadline(competitionHeartbeatSlotFailure(context.DeadlineExceeded, server.DbTxReleaseIdle)) ||
		competitionHostHeartbeatUnadmittedDeadline(proof) || competitionSupervisorUnadmittedDeadline(proof) {
		t.Fatal("generic proof widened root identity, legacy, host or supervisor authority")
	}
}

// Actual job UPDATE cancellation precedes any slot UPDATE and returns three
// acknowledged rollbacks before a fourth renewal of the original claim.
func TestCompetitionHeartbeatDistributedAllStatementsPgThreeJobUpdateFailuresThenFourthSuccess(t *testing.T) {
	testCompetitionHeartbeatReturnedRollbackPg(t, competitionHeartbeatDatabaseJobUpdate)
}
