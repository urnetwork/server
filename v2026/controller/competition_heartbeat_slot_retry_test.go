// Source-owned synthetic proofs exercise the exact gate and virtual retry
// clocks. Separate real-PG tests authenticate the actual store/SQL wiring.
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

// No incident identity, query argument or private error is fixture authority.
func competitionHeartbeatSlotFailure(cause error, disposition server.DbTxReleaseDisposition) competitionHeartbeatDatabaseError {
	live := server.DbContextBudget{State: "live", HasDeadline: true, RemainingNs: int64(time.Second)}
	deadline := server.DbContextBudget{State: "deadline", HasDeadline: true, RemainingNs: -int64(5 * time.Second)}
	rollbackAcknowledged := uint64(0)
	if disposition == server.DbTxReleaseIdle {
		rollbackAcknowledged = 1
	}
	snapshot := competitionHeartbeatDatabaseSnapshot{AtEntry: live, AtReturn: deadline, Step: competitionHeartbeatDatabaseSlotUpdate,
		CallbackEntries: 1, SqlShape: competitionHeartbeatLegacySlotShape, SqlStatements: uint64(len(competitionHeartbeatLegacySlotStatements())), SqlShapeValid: true,
		Acquire: server.DbAcquireObservation{Attempt: 1, Returned: true, Admitted: true, BeforeAcquire: live, AtReturn: live},
		Transaction: server.DbTxObservationSnapshot{AcquireAttempts: 1, AcquireSucceeded: 1, BeginAttempts: 1, BeginSucceeded: 1, CallbackEntered: 1,
			RollbackAttempts: 1, RollbackAcknowledged: rollbackAcknowledged, ReleaseAttempts: 1, ReleaseReturned: 1,
			LastBegin:    server.DbTxBoundaryObservation{Before: live, AtReturn: live, Returned: true},
			LastRollback: server.DbTxBoundaryObservation{Before: live, AtReturn: live, Returned: true},
			LastRelease:  server.DbTxReleaseObservation{Disposition: disposition}}}
	ordinals := map[competitionHeartbeatDatabaseStep]uint8{}
	for index, expected := range competitionHeartbeatLegacySlotStatements() {
		ordinals[expected.step]++
		returned := live
		if index == len(competitionHeartbeatLegacySlotStatements())-1 {
			returned = deadline
		}
		snapshot.Statements = append(snapshot.Statements, competitionHeartbeatStatementObservation{
			Step: expected.step, Ordinal: ordinals[expected.step], Kind: expected.kind, Count: 1, Entered: live,
			Returned: true, Succeeded: index != len(competitionHeartbeatLegacySlotStatements())-1, AtReturn: &returned})
	}
	return competitionHeartbeatDatabaseError{cause: competitionDatabasePhaseError{cause: cause, phase: server.DbOperationCallback}, snapshot: snapshot}
}

// Pending pool-owned destruction is intentionally not a claimed physical join.
func TestCompetitionHeartbeatSlotProofAcceptsAckRollbackOrOwnedDestroy(t *testing.T) {
	cause := errors.Join(server.DbContextDoneError, fmt.Errorf("synthetic deadline: %w", context.DeadlineExceeded))
	for _, disposition := range []server.DbTxReleaseDisposition{server.DbTxReleaseIdle, server.DbTxReleaseDestroyTransferred} {
		proof := competitionHeartbeatSlotFailure(cause, disposition)
		if !competitionHeartbeatSlotUpdateDeadline(proof) || competitionHeartbeatUnadmittedDeadline(proof) || proof.snapshot.Transaction.LastRelease.CleanupComplete ||
			!errors.Is(proof, context.DeadlineExceeded) || proof.Error() != cause.Error() {
			t.Fatal("complete no-COMMIT transfer was rejected or mislabeled unadmitted/joined", proof.snapshot)
		}
	}
}

// Earlier/manual work, incomplete release or even one COMMIT attempt refuses.
func TestCompetitionHeartbeatSlotProofRejectsIncompleteOrAmbiguousHistory(t *testing.T) {
	for _, change := range []func(*competitionHeartbeatDatabaseError){
		func(p *competitionHeartbeatDatabaseError) { p.snapshot.SqlShape = "" },
		func(p *competitionHeartbeatDatabaseError) { p.snapshot.SqlShapeValid = false },
		func(p *competitionHeartbeatDatabaseError) { p.snapshot.SqlStatements++ },
		func(p *competitionHeartbeatDatabaseError) { p.snapshot.CallbackEntries++ },
		func(p *competitionHeartbeatDatabaseError) { p.snapshot.UnknownStatements++ },
		func(p *competitionHeartbeatDatabaseError) { p.snapshot.Step = competitionHeartbeatDatabaseJobUpdate },
		func(p *competitionHeartbeatDatabaseError) { p.snapshot.Transaction.AcquireAttempts++ },
		func(p *competitionHeartbeatDatabaseError) { p.snapshot.Transaction.BeginAttempts++ },
		func(p *competitionHeartbeatDatabaseError) { p.snapshot.Transaction.BeginSucceeded = 0 },
		func(p *competitionHeartbeatDatabaseError) { p.snapshot.Transaction.CallbackEntered++ },
		func(p *competitionHeartbeatDatabaseError) { p.snapshot.Transaction.CommitAttempts = 1 },
		func(p *competitionHeartbeatDatabaseError) { p.snapshot.Transaction.CommitAcknowledged = 1 },
		func(p *competitionHeartbeatDatabaseError) { p.snapshot.Transaction.LastCommit.Returned = true },
		func(p *competitionHeartbeatDatabaseError) { p.snapshot.Transaction.RollbackAttempts++ },
		func(p *competitionHeartbeatDatabaseError) { p.snapshot.Transaction.LastRollback.Returned = false },
		func(p *competitionHeartbeatDatabaseError) { p.snapshot.Transaction.ReleaseAttempts++ },
		func(p *competitionHeartbeatDatabaseError) { p.snapshot.Transaction.ReleaseReturned = 0 },
		func(p *competitionHeartbeatDatabaseError) {
			p.snapshot.Transaction.LastRelease.Disposition = server.DbTxReleaseUnknown
		},
		func(p *competitionHeartbeatDatabaseError) {
			p.snapshot.Transaction.LastRelease.Disposition = server.DbTxReleaseIdle
		},
		func(p *competitionHeartbeatDatabaseError) { p.snapshot.AtEntry.State = "deadline" },
		func(p *competitionHeartbeatDatabaseError) { p.snapshot.AtReturn.State = "live" },
		func(p *competitionHeartbeatDatabaseError) { p.snapshot.Acquire.Admitted = false },
		func(p *competitionHeartbeatDatabaseError) { p.snapshot.Transaction.LastBegin.Returned = false },
		func(p *competitionHeartbeatDatabaseError) {
			p.snapshot.Transaction.LastRollback.Before.HasDeadline = false
		},
		func(p *competitionHeartbeatDatabaseError) { p.snapshot.Statements = p.snapshot.Statements[:8] },
		func(p *competitionHeartbeatDatabaseError) { p.snapshot.Statements[0].Count++ },
		func(p *competitionHeartbeatDatabaseError) { p.snapshot.Statements[0].Returned = false },
		func(p *competitionHeartbeatDatabaseError) { p.snapshot.Statements[0].Succeeded = false },
		func(p *competitionHeartbeatDatabaseError) { p.snapshot.Statements[0].Ordinal++ },
		func(p *competitionHeartbeatDatabaseError) { p.snapshot.Statements[0].Kind = "query" },
		func(p *competitionHeartbeatDatabaseError) { p.snapshot.Statements[8].Succeeded = true },
		func(p *competitionHeartbeatDatabaseError) { p.snapshot.Statements[8].AtReturn = nil },
		func(p *competitionHeartbeatDatabaseError) {
			p.cause = competitionDatabasePhaseError{cause: context.DeadlineExceeded, phase: server.DbOperationCommit}
		},
	} {
		proof := competitionHeartbeatSlotFailure(context.DeadlineExceeded, server.DbTxReleaseDestroyTransferred)
		change(&proof)
		if competitionHeartbeatSlotUpdateDeadline(proof) {
			t.Fatal("incomplete no-COMMIT evidence authenticated continuation", proof.snapshot)
		}
	}
}

// One valid branch cannot authenticate unrelated cancellation/SQL/network work.
func TestCompetitionHeartbeatSlotProofRejectsEveryMixedOrUnknownCause(t *testing.T) {
	for _, cause := range []error{context.Canceled, ErrLeaseLost, errors.New("synthetic unrelated failure"),
		errors.Join(context.DeadlineExceeded, context.Canceled), errors.Join(context.DeadlineExceeded, ErrLeaseLost),
		errors.Join(context.DeadlineExceeded, &pgconn.PgError{Code: "55P03"}),
		&net.OpError{Op: "read", Net: "synthetic", Err: context.DeadlineExceeded},
		competitionSupervisorUnknownDeadline{cause: context.DeadlineExceeded},
		errors.Join(context.DeadlineExceeded, &competitionLeaseRetryCycleError{}),
		server.NewErrorCauseBatch([]error{context.DeadlineExceeded, nil}),
	} {
		if competitionHeartbeatSlotUpdateDeadline(competitionHeartbeatSlotFailure(cause, server.DbTxReleaseDestroyTransferred)) {
			t.Fatalf("unknown cause graph authenticated a no-COMMIT continuation: %T", cause)
		}
	}
	proof := competitionHeartbeatSlotFailure(context.DeadlineExceeded, server.DbTxReleaseDestroyTransferred)
	if competitionHeartbeatSlotUpdateDeadline(errors.Join(proof, context.DeadlineExceeded)) ||
		competitionHostHeartbeatUnadmittedDeadline(proof) || competitionSupervisorUnadmittedDeadline(proof) {
		t.Fatal("another root/authority borrowed the job-only proof")
	}
}

// The first eight-second call owns five seconds of existing cleanup. Its
// successor receives only the original remainder, not another twenty seconds.
func TestCompetitionHeartbeatSlotContinuationSharesOriginalTotalAndCallCap(t *testing.T) {
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
				return competitionHeartbeatSlotFailure(attempt.Err(), server.DbTxReleaseDestroyTransferred)
			}
			deadline, ok := attempt.Deadline()
			if !ok || !deadline.Equal(started.Add(settings.AcquireRetry.Timeout)) || time.Until(deadline) >= settings.AcquireRetry.AttemptTimeout {
				t.Fatal("continuation reset total lifetime instead of using the remainder", deadline)
			}
			return nil
		})
		if err != nil || calls != 2 || ack.Sub(started) != 13*time.Second+settings.LeaseRetry.Interval {
			t.Fatal("known no-COMMIT heartbeat did not continue within its original owner", calls, ack.Sub(started), err)
		}
	})
}

// A valid immediate predecessor proof never replenishes the shared call cap.
func TestCompetitionHeartbeatSlotContinuationRetainsThreeCallMaximum(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		settings := defaultCompetitionHeartbeatSettings()
		settings.LeaseRetry.Now = time.Now
		settings.AcquireRetry.AttemptTimeout = time.Second
		expiry, calls := time.Now().Add(time.Minute), 0
		_, err := retryCompetitionJobHeartbeatOperation(t.Context(), expiry, expiry, settings, func(attempt context.Context) error {
			calls++
			<-attempt.Done()
			return competitionHeartbeatSlotFailure(attempt.Err(), server.DbTxReleaseDestroyTransferred)
		})
		if err == nil || calls != settings.LeaseRetry.MaximumAttempts || calls != 3 {
			t.Fatal("known no-COMMIT proof replenished its shared call cap", calls, err)
		}
	})
}

// Neither host/supervisor entrypoints nor unknown host/job bounds can opt into
// an admitted transaction continuation through the generic acquisition helper.
func TestCompetitionHeartbeatSlotContinuationRemainsJobOnlyAndRequiresBothBounds(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		for _, mode := range []string{"host", "acquire-only", "unknown-host", "unknown-job"} {
			settings := defaultCompetitionHeartbeatSettings()
			settings.LeaseRetry.Now = time.Now
			expiry := time.Now().Add(time.Minute)
			calls := 0
			operation := func(attempt context.Context) error {
				calls++
				<-attempt.Done()
				return competitionHeartbeatSlotFailure(attempt.Err(), server.DbTxReleaseDestroyTransferred)
			}
			var err error
			switch mode {
			case "host":
				_, err = retryCompetitionHeartbeatOperation(t.Context(), expiry, settings, true, operation)
			case "acquire-only":
				_, err = retryCompetitionHeartbeatOperation(t.Context(), expiry, settings, false, operation)
			case "unknown-host":
				_, err = retryCompetitionJobHeartbeatOperation(t.Context(), expiry, time.Time{}, settings, operation)
			case "unknown-job":
				_, err = retryCompetitionJobHeartbeatOperation(t.Context(), time.Time{}, expiry, settings, operation)
			}
			if err == nil || calls != 1 {
				t.Fatal("another authority acquired admitted deadline continuation", mode, calls, err)
			}
		}
	})
}

// Both UTC authority samples and actual parent deadlines win after cleanup;
// no proof can revive authority which ended during the original owned call.
func TestCompetitionHeartbeatSlotContinuationStopsAtHostJobAndParentExpiry(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		for _, mode := range []string{"host", "job", "execution", "canceled", "total"} {
			started := time.Now()
			settings := defaultCompetitionHeartbeatSettings()
			settings.LeaseRetry.Now = time.Now
			job, host := started.Add(time.Minute), started.Add(time.Minute)
			parent, cancel := context.WithCancel(t.Context())
			if mode == "host" {
				host = started.Add(13*time.Second + settings.LeaseRetry.Interval/2)
			} else if mode == "job" {
				job = started.Add(13*time.Second + settings.LeaseRetry.Interval/2)
			} else if mode == "execution" {
				cancel()
				parent, cancel = context.WithDeadline(t.Context(), started.Add(10*time.Second))
			}
			calls := 0
			_, err := retryCompetitionJobHeartbeatOperation(parent, job, host, settings, func(attempt context.Context) error {
				calls++
				<-attempt.Done()
				if mode == "canceled" {
					cancel()
				}
				if mode == "total" {
					time.Sleep(settings.AcquireRetry.Timeout)
				} else {
					time.Sleep(5 * time.Second)
				}
				return competitionHeartbeatSlotFailure(context.DeadlineExceeded, server.DbTxReleaseDestroyTransferred)
			})
			cancel()
			if err == nil || calls != 1 {
				t.Fatal("expired/canceled original authority dispatched another call", mode, calls, err)
			}
		}
	})
}

// A COMMIT which may finish after the child deadline never authorizes replay,
// even when its caller sees only a deadline and the physical reply is lost.
func TestCompetitionHeartbeatSlotContinuationRejectsLateOrUnacknowledgedCommit(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		for _, acknowledged := range []uint64{0, 1} {
			settings := defaultCompetitionHeartbeatSettings()
			settings.LeaseRetry.Now = time.Now
			expiry, calls := time.Now().Add(time.Minute), 0
			_, err := retryCompetitionJobHeartbeatOperation(t.Context(), expiry, expiry, settings, func(attempt context.Context) error {
				calls++
				<-attempt.Done()
				proof := competitionHeartbeatSlotFailure(context.DeadlineExceeded, server.DbTxReleaseDestroyTransferred)
				proof.snapshot.Transaction.CommitAttempts, proof.snapshot.Transaction.CommitAcknowledged = 1, acknowledged
				return proof
			})
			if err == nil || calls != 1 {
				t.Fatal("a possible completed COMMIT granted a second transaction", acknowledged, calls, err)
			}
		}
	})
}
