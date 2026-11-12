// Exact distributed SQL and returned rollback evidence are separate from the
// legacy singleton proof. Virtual time preserves every original owner bound.
package controller

import (
	"context"
	"errors"
	"slices"
	"testing"
	"testing/synctest"
	"time"

	"github.com/urnetwork/server"
)

// Synthetic distributed authority has three executor-fence statements and a
// successful job UPDATE before the failed slot UPDATE; COMMIT was never tried.
func competitionHeartbeatDistributedSlotFailure() competitionHeartbeatDatabaseError {
	proof := competitionHeartbeatSlotFailure(context.DeadlineExceeded, server.DbTxReleaseIdle)
	proof.snapshot.SqlShape = competitionHeartbeatDistributedSlotShape
	expected := competitionHeartbeatDistributedSlotStatements()
	proof.snapshot.SqlStatements = uint64(len(expected))
	proof.snapshot.Statements = nil
	ordinals := map[competitionHeartbeatDatabaseStep]uint8{}
	for index, want := range expected {
		ordinals[want.step]++
		returned := proof.snapshot.AtEntry
		if index == len(expected)-1 {
			returned = proof.snapshot.AtReturn
		}
		proof.snapshot.Statements = append(proof.snapshot.Statements, competitionHeartbeatStatementObservation{Step: want.step, Ordinal: ordinals[want.step], Kind: want.kind, Count: 1, Entered: proof.snapshot.AtEntry, Returned: true, Succeeded: index != len(expected)-1, AtReturn: &returned})
	}
	// Capture emits step eight before nine, despite SQL updating nine first.
	slices.SortFunc(proof.snapshot.Statements, func(left, right competitionHeartbeatStatementObservation) int {
		if left.Step != right.Step {
			return int(left.Step) - int(right.Step)
		}
		return int(left.Ordinal) - int(right.Ordinal)
	})
	return proof
}

func TestCompetitionHeartbeatDistributedSlotProofUsesRealStepOrderedDiagnostics(t *testing.T) {
	proof := competitionHeartbeatDistributedSlotFailure()
	oldIndexAccepted := true
	for index, expected := range competitionHeartbeatDistributedSlotStatements() {
		if proof.snapshot.Statements[index].Step != expected.step {
			oldIndexAccepted = false
			break
		}
	}
	if oldIndexAccepted || proof.snapshot.Statements[10].Step != competitionHeartbeatDatabaseSlotUpdate || proof.snapshot.Statements[10].Succeeded || proof.snapshot.Statements[11].Step != competitionHeartbeatDatabaseJobUpdate || !proof.snapshot.Statements[11].Succeeded {
		t.Fatal("fixture did not reproduce the actual step/SQL order mismatch")
	}
	if !competitionHeartbeatDistributedSlotUpdateDeadline(proof) {
		t.Fatal("exact SQL proof rejected canonical step-ordered diagnostics")
	}
}

func TestCompetitionHeartbeatDistributedSlotRetryResumesInsideOriginalReserve(t *testing.T) {
	for _, historical := range []bool{true, false} {
		synctest.Test(t, func(t *testing.T) {
			started := time.Now()
			settings := defaultCompetitionHeartbeatSettings()
			settings.LeaseRetry.Now = time.Now
			calls := 0
			operation := func(attempt context.Context) error {
				calls++
				if calls == 1 {
					<-attempt.Done()
					return competitionHeartbeatDistributedSlotFailure()
				}
				return nil
			}
			var ack time.Time
			var err error
			if historical {
				ack, err = retryCompetitionHeartbeatWithProof(t.Context(), started.Add(time.Minute), settings, operation, func(err error) bool {
					return competitionHeartbeatUnadmittedDeadline(err) || competitionHeartbeatSlotUpdateDeadline(err) || competitionHeartbeatJobLockDeadline(err)
				})
			} else {
				ack, err = retryCompetitionJobHeartbeatOperation(t.Context(), started.Add(time.Minute), started.Add(time.Minute), settings, operation)
			}
			if historical {
				if err == nil || calls != 1 || !ack.IsZero() {
					t.Fatal("old singleton-only proof did not reproduce lost distributed custody", calls, err)
				}
			} else if err != nil || calls != 2 || ack.Sub(started) != settings.AcquireRetry.AttemptTimeout+settings.LeaseRetry.Interval {
				t.Fatal("distributed rollback could not continue within its same reserve", calls, ack, err)
			}
		})
	}
}

func TestCompetitionHeartbeatDistributedSlotRetryRequiresBothLeasesAndKeepsThreeCallCap(t *testing.T) {
	for _, mode := range []string{"known", "unknown-job", "unknown-host", "expired-job", "expired-host", "cancel"} {
		synctest.Test(t, func(t *testing.T) {
			settings := defaultCompetitionHeartbeatSettings()
			settings.LeaseRetry.Now = time.Now
			settings.AcquireRetry.AttemptTimeout = time.Second
			started := time.Now()
			job, host := started.Add(time.Minute), started.Add(time.Minute)
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
				if mode == "cancel" {
					cancel()
					return nil
				}
				<-attempt.Done()
				return competitionHeartbeatDistributedSlotFailure()
			})
			maximum := 1
			if mode == "known" {
				maximum = settings.LeaseRetry.MaximumAttempts
			}
			if mode == "expired-job" || mode == "expired-host" {
				maximum = 0
			}
			if err == nil || !ack.IsZero() || calls != maximum || time.Since(started) > settings.AcquireRetry.Timeout {
				t.Fatal("distributed rollback borrowed unknown, expired or extra call authority", mode, calls, err)
			}
		})
	}
}

func TestCompetitionHeartbeatDistributedSlotProofRejectsAmbiguousOrChangedWork(t *testing.T) {
	valid := competitionHeartbeatDistributedSlotFailure()
	if !competitionHeartbeatDistributedSlotUpdateDeadline(valid) || competitionHeartbeatSlotUpdateDeadline(valid) || competitionHeartbeatScopeLockTimeout(valid) {
		t.Fatal("distributed proof was rejected or mislabeled as another SQL graph")
	}
	for index, change := range []func(*competitionHeartbeatDatabaseError){
		func(p *competitionHeartbeatDatabaseError) { p.snapshot.SqlShape = competitionHeartbeatLegacySlotShape },
		func(p *competitionHeartbeatDatabaseError) { p.snapshot.SqlShapeValid = false },
		func(p *competitionHeartbeatDatabaseError) { p.snapshot.SqlStatements-- },
		func(p *competitionHeartbeatDatabaseError) { p.snapshot.CallbackEntries++ },
		func(p *competitionHeartbeatDatabaseError) { p.snapshot.UnknownStatements++ },
		func(p *competitionHeartbeatDatabaseError) { p.snapshot.Step = competitionHeartbeatDatabaseJobUpdate },
		func(p *competitionHeartbeatDatabaseError) { p.snapshot.Transaction.CommitAttempts++ },
		func(p *competitionHeartbeatDatabaseError) { p.snapshot.Transaction.LastCommit.Returned = true },
		func(p *competitionHeartbeatDatabaseError) { p.snapshot.Transaction.RollbackAcknowledged = 0 },
		func(p *competitionHeartbeatDatabaseError) { p.snapshot.Transaction.LastRollback.Returned = false },
		func(p *competitionHeartbeatDatabaseError) { p.snapshot.Transaction.ReleaseReturned = 0 },
		func(p *competitionHeartbeatDatabaseError) {
			p.snapshot.Transaction.LastRelease.Disposition = server.DbTxReleaseDestroyTransferred
		},
		func(p *competitionHeartbeatDatabaseError) { p.snapshot.AtReturn = p.snapshot.AtEntry },
		func(p *competitionHeartbeatDatabaseError) {
			p.snapshot.Statements = p.snapshot.Statements[:len(p.snapshot.Statements)-1]
		},
		func(p *competitionHeartbeatDatabaseError) { p.snapshot.Statements[4].Ordinal = 1 },
		func(p *competitionHeartbeatDatabaseError) { p.snapshot.Statements[4].Succeeded = false },
		func(p *competitionHeartbeatDatabaseError) {
			p.snapshot.Statements[10].Step = competitionHeartbeatDatabaseJobUpdate
		},
		func(p *competitionHeartbeatDatabaseError) { p.snapshot.Statements[10].Succeeded = true },
		func(p *competitionHeartbeatDatabaseError) { p.snapshot.Statements[11].AtReturn = nil },
		func(p *competitionHeartbeatDatabaseError) {
			p.cause = competitionDatabasePhaseError{cause: context.DeadlineExceeded, phase: server.DbOperationCommit}
		},
		func(p *competitionHeartbeatDatabaseError) {
			p.cause = competitionDatabasePhaseError{cause: errors.Join(context.DeadlineExceeded, ErrLeaseLost), phase: server.DbOperationCallback}
		},
	} {
		proof := competitionHeartbeatDistributedSlotFailure()
		change(&proof)
		if competitionHeartbeatDistributedSlotUpdateDeadline(proof) {
			t.Fatal("ambiguous distributed graph authorized continuation", index)
		}
	}
}

func TestCompetitionHeartbeatDistributedSqlProofAuthenticatesExecutorAndUpdateOrder(t *testing.T) {
	for _, mode := range []string{"exact", "executor", "update-order", "extra"} {
		observation := &competitionHeartbeatDatabaseObservation{sqlShape: competitionHeartbeatDistributedSlotShape, sqlValid: true}
		previous := competitionHeartbeatDatabaseUnknown
		for index, want := range competitionHeartbeatDistributedSlotStatements() {
			if previous != want.step {
				observation.enterStep(want.step)
				previous = want.step
			}
			statement := observation.beginStatement(t.Context(), want.kind)
			sql := want.sql
			if mode == "executor" && index == 4 {
				sql = "SELECT 1"
			}
			if mode == "update-order" && index == 10 {
				sql = competitionHeartbeatDistributedSlotStatements()[11].sql
			}
			observation.observeSql(statement, sql)
		}
		if mode == "extra" {
			observation.observeSql(observation.beginStatement(t.Context(), "exec"), "COMMIT")
		}
		if observation.sqlValid != (mode == "exact") {
			t.Fatal("distributed graph accepted altered authority/update order", mode)
		}
	}
}
