// The two exact distributed returned-rollback classes share a finite job-only
// allowance. No legacy, host, supervisor or incomplete transaction borrows it.
package controller

import (
	"context"
	"testing"
	"testing/synctest"
	"time"

	"github.com/urnetwork/server"
)

// Three whole distributed transactions can return acknowledged rollbacks
// before the owner spends its fourth call, without refreshing any deadline.
func TestCompetitionHeartbeatDistributedReturnedRollbackFourthAttemptUsesOriginalReserve(t *testing.T) {
	for _, historicalCap := range []bool{true, false} {
		for _, mixedPrefix := range []bool{false, true} {
			synctest.Test(t, func(t *testing.T) {
				settings := defaultCompetitionHeartbeatSettings()
				settings.LeaseRetry.Now = time.Now
				settings.AcquireRetry.AttemptTimeout = time.Second
				if historicalCap {
					settings.DistributedReturnedRollbackMaximumAttempts = settings.LeaseRetry.MaximumAttempts
				}
				started, calls := time.Now(), 0
				ack, err := retryCompetitionJobHeartbeatOperation(t.Context(), started.Add(time.Minute), started.Add(time.Minute), settings, func(attempt context.Context) error {
					calls++
					if calls <= 3 {
						<-attempt.Done()
						if mixedPrefix && calls < 3 {
							return competitionHeartbeatPrefixFailure(8)
						}
						return competitionHeartbeatDistributedSlotFailure()
					}
					return nil
				})
				if historicalCap {
					if err == nil || calls != 3 || !ack.IsZero() || time.Since(started) >= settings.AcquireRetry.Timeout {
						t.Fatal("historical cap did not reproduce premature final-slot handback", calls, err)
					}
				} else if err != nil || calls != 4 || ack.IsZero() || time.Since(started) >= settings.AcquireRetry.Timeout {
					t.Fatal("fourth returned-rollback call lost its original reserve", calls, err)
				}
			})
		}
	}
}

// Alternating graphs cannot mint a new counter or use a different class's
// allowance to reset the original total deadline or known authority.
func TestCompetitionHeartbeatDistributedReturnedRollbackSharesCumulativeCap(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		settings := defaultCompetitionHeartbeatSettings()
		settings.LeaseRetry.Now = time.Now
		settings.AcquireRetry.AttemptTimeout = time.Second / 2
		started, calls := time.Now(), 0
		ack, err := retryCompetitionJobHeartbeatOperation(t.Context(), started.Add(time.Minute), started.Add(time.Minute), settings, func(attempt context.Context) error {
			calls++
			<-attempt.Done()
			if calls%2 != 0 {
				return competitionHeartbeatPrefixFailure(8)
			}
			return competitionHeartbeatDistributedSlotFailure()
		})
		if err == nil || !ack.IsZero() || calls != settings.DistributedReturnedRollbackMaximumAttempts || time.Since(started) > settings.AcquireRetry.Timeout {
			t.Fatal("alternating source proofs created extra calls or a fresh owner", calls, err)
		}
	})
}

// A larger finite call setting cannot refresh the twenty-second lifetime or
// borrow time from still-live confirmed leases after that lifetime expires.
func TestCompetitionHeartbeatDistributedReturnedRollbackKeepsOriginalTotalDeadline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		settings := defaultCompetitionHeartbeatSettings()
		settings.LeaseRetry.Now = time.Now
		settings.DistributedReturnedRollbackMaximumAttempts = 50
		started, calls := time.Now(), 0
		ack, err := retryCompetitionJobHeartbeatOperation(t.Context(), started.Add(time.Minute), started.Add(time.Minute), settings, func(attempt context.Context) error {
			calls++
			<-attempt.Done()
			return competitionHeartbeatDistributedSlotFailure()
		})
		if err == nil || !ack.IsZero() || calls != 3 || time.Since(started) != settings.AcquireRetry.Timeout || t.Context().Err() != nil {
			t.Fatal("returned rollback refreshed the original total deadline or shared parent", calls, time.Since(started), err)
		}
	})
}

// Disabling the optional extension does not remove the already-supported
// three-call final-slot continuation or broaden any other policy class.
func TestCompetitionHeartbeatDistributedReturnedRollbackDisabledKeepsHistoricalCap(t *testing.T) {
	for _, maximum := range []int{-1, 0, 3} {
		synctest.Test(t, func(t *testing.T) {
			settings := defaultCompetitionHeartbeatSettings()
			settings.LeaseRetry.Now = time.Now
			settings.AcquireRetry.AttemptTimeout = time.Second
			settings.DistributedReturnedRollbackMaximumAttempts = maximum
			started, calls := time.Now(), 0
			ack, err := retryCompetitionJobHeartbeatOperation(t.Context(), started.Add(time.Minute), started.Add(time.Minute), settings, func(attempt context.Context) error {
				calls++
				<-attempt.Done()
				return competitionHeartbeatDistributedSlotFailure()
			})
			if err == nil || !ack.IsZero() || calls != settings.LeaseRetry.MaximumAttempts {
				t.Fatal("disabled distributed extension changed historical final-slot continuation", maximum, calls, err)
			}
		})
	}
}

// Failed or pending rollback, destruction transfer, manual statements and a
// COMMIT attempt remain outside the stronger optional job-only extension.
func TestCompetitionHeartbeatDistributedReturnedRollbackRefusesAmbiguousTransactions(t *testing.T) {
	for index, mutate := range []func(*competitionHeartbeatDatabaseError){
		func(p *competitionHeartbeatDatabaseError) { p.snapshot.Transaction.RollbackAcknowledged = 0 },
		func(p *competitionHeartbeatDatabaseError) { p.snapshot.Transaction.LastRollback.Returned = false },
		func(p *competitionHeartbeatDatabaseError) { p.snapshot.Transaction.ReleaseReturned = 0 },
		func(p *competitionHeartbeatDatabaseError) {
			p.snapshot.Transaction.LastRelease.Disposition = server.DbTxReleaseDestroyTransferred
		},
		func(p *competitionHeartbeatDatabaseError) { p.snapshot.Transaction.CommitAttempts++ },
		func(p *competitionHeartbeatDatabaseError) { p.snapshot.UnknownStatements++ },
		func(p *competitionHeartbeatDatabaseError) { p.snapshot.Statements[4].Succeeded = false },
		func(p *competitionHeartbeatDatabaseError) { p.snapshot.SqlShapeValid = false },
	} {
		synctest.Test(t, func(t *testing.T) {
			settings := defaultCompetitionHeartbeatSettings()
			settings.LeaseRetry.Now = time.Now
			settings.AcquireRetry.AttemptTimeout = time.Second
			started, calls := time.Now(), 0
			ack, err := retryCompetitionJobHeartbeatOperation(t.Context(), started.Add(time.Minute), started.Add(time.Minute), settings, func(attempt context.Context) error {
				calls++
				<-attempt.Done()
				proof := competitionHeartbeatDistributedSlotFailure()
				mutate(&proof)
				return proof
			})
			if err == nil || !ack.IsZero() || calls != 1 {
				t.Fatal("incomplete transaction borrowed optional distributed reserve", index, calls, err)
			}
		})
	}
}

// The expanded strong cap remains separate from the historical legacy class,
// including its acknowledged rollback and its owned-destruction alternative.
func TestCompetitionHeartbeatDistributedReturnedRollbackKeepsLegacyThreeCallPolicy(t *testing.T) {
	for _, disposition := range []server.DbTxReleaseDisposition{server.DbTxReleaseIdle, server.DbTxReleaseDestroyTransferred} {
		synctest.Test(t, func(t *testing.T) {
			settings := defaultCompetitionHeartbeatSettings()
			settings.LeaseRetry.Now = time.Now
			settings.AcquireRetry.AttemptTimeout = time.Second
			started, calls := time.Now(), 0
			ack, err := retryCompetitionJobHeartbeatOperation(t.Context(), started.Add(time.Minute), started.Add(time.Minute), settings, func(attempt context.Context) error {
				calls++
				<-attempt.Done()
				return competitionHeartbeatSlotFailure(context.DeadlineExceeded, disposition)
			})
			if err == nil || !ack.IsZero() || calls != settings.LeaseRetry.MaximumAttempts {
				t.Fatal("legacy proof borrowed stronger distributed transaction allowance", calls, err)
			}
		})
	}
}

// A real final-slot UPDATE follows an actually successful job UPDATE. Each
// canceled statement/rollback must restore both leases and the original run.
func TestCompetitionHeartbeatDistributedReturnedRollbackPgThreeFinalSlotFailuresThenFourthSuccess(t *testing.T) {
	testCompetitionHeartbeatReturnedRollbackPg(t, competitionHeartbeatDatabaseSlotUpdate)
}
