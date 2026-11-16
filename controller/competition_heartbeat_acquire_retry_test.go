// Owned virtual deadlines and exact cumulative observations exercise only
// pre-admission continuation. No database, evaluator or wall-time race runs.
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
	"github.com/urnetwork/server"
)

// Production-shaped immutable proof; private PG tests separately authenticate
// the actual pool/store observation path instead of trusting these fixtures.
func competitionHeartbeatAcquireFailure(cause error) competitionHeartbeatDatabaseError {
	return competitionHeartbeatDatabaseError{cause: competitionDatabasePhaseError{cause: cause, phase: server.DbOperationAcquire},
		snapshot: competitionHeartbeatDatabaseSnapshot{Acquire: server.DbAcquireObservation{Attempt: 1, Returned: true,
			BeforeAcquire: server.DbContextBudget{State: "live", HasDeadline: true, RemainingNs: int64(time.Second)},
			AtReturn:      server.DbContextBudget{State: "deadline", HasDeadline: true}}, Transaction: server.DbTxObservationSnapshot{AcquireAttempts: 1}}}
}

// A Db callback has its own actual cumulative read census, not a fabricated Tx.
func competitionHostAcquireFailure(cause error) competitionHostHeartbeatAcquireError {
	read := server.DbReadObservationSnapshot{Phase: server.DbReadError, Finished: true}
	read.PhaseCounts[server.DbReadAcquireBegin], read.PhaseCounts[server.DbReadAcquireDone] = 1, 1
	return competitionHostHeartbeatAcquireError{cause: competitionDatabasePhaseError{cause: cause, phase: server.DbOperationAcquire},
		acquire: competitionHeartbeatAcquireFailure(cause).snapshot.Acquire, read: read}
}

// Observations neither change diagnostic bytes nor lose original Is/As causes.
func TestCompetitionHeartbeatAcquireProofAcceptsOnlyWholeReturnedDeadline(t *testing.T) {
	cause := errors.Join(server.DbContextDoneError, fmt.Errorf("synthetic acquisition: %w", context.DeadlineExceeded))
	for _, proof := range []error{competitionHeartbeatAcquireFailure(cause), competitionHostAcquireFailure(cause)} {
		if proof.Error() != cause.Error() || !errors.Is(proof, context.DeadlineExceeded) || !errors.Is(proof, server.DbContextDoneError) {
			t.Fatal("proof changed original cause identity")
		}
	}
	if !competitionHeartbeatUnadmittedDeadline(competitionHeartbeatAcquireFailure(cause)) || !competitionHostHeartbeatUnadmittedDeadline(competitionHostAcquireFailure(cause)) {
		t.Fatal("complete returned no-admission deadline was not recognized")
	}
}

// Last-phase acquisition cannot erase any admitted earlier transaction/SQL.
func TestCompetitionHeartbeatAcquireProofRejectsEarlierOrIncompleteAdmission(t *testing.T) {
	for _, mutate := range []func(*competitionHeartbeatDatabaseError){
		func(p *competitionHeartbeatDatabaseError) {
			p.cause = competitionDatabasePhaseError{cause: context.DeadlineExceeded, phase: server.DbOperationCallback}
		},
		func(p *competitionHeartbeatDatabaseError) { p.snapshot.CallbackEntries = 1 },
		func(p *competitionHeartbeatDatabaseError) { p.snapshot.Step = competitionHeartbeatDatabaseSlotLock },
		func(p *competitionHeartbeatDatabaseError) { p.snapshot.UnknownStatements = 1 },
		func(p *competitionHeartbeatDatabaseError) {
			p.snapshot.Statements = []competitionHeartbeatStatementObservation{{Count: 1}}
		},
		func(p *competitionHeartbeatDatabaseError) { p.snapshot.Acquire.Returned = false },
		func(p *competitionHeartbeatDatabaseError) { p.snapshot.Acquire.Admitted = true },
		func(p *competitionHeartbeatDatabaseError) { p.snapshot.Acquire.Attempt = 0 },
		func(p *competitionHeartbeatDatabaseError) { p.snapshot.Acquire.BeforeAcquire.State = "deadline" },
		func(p *competitionHeartbeatDatabaseError) { p.snapshot.Acquire.BeforeAcquire.HasDeadline = false },
		func(p *competitionHeartbeatDatabaseError) { p.snapshot.Acquire.BeforeAcquire.RemainingNs = 0 },
		func(p *competitionHeartbeatDatabaseError) { p.snapshot.Acquire.AtReturn.State = "live" },
		func(p *competitionHeartbeatDatabaseError) { p.snapshot.Acquire.AtReturn.HasDeadline = false },
		func(p *competitionHeartbeatDatabaseError) { p.snapshot.Acquire.AtReturn.RemainingNs = 1 },
		func(p *competitionHeartbeatDatabaseError) { p.snapshot.Transaction.AcquireAttempts = 0 },
		func(p *competitionHeartbeatDatabaseError) { p.snapshot.Transaction.AcquireAttempts = 2 },
		func(p *competitionHeartbeatDatabaseError) { p.snapshot.Transaction.AcquireSucceeded = 1 },
		func(p *competitionHeartbeatDatabaseError) { p.snapshot.Transaction.BeginAttempts = 1 },
		func(p *competitionHeartbeatDatabaseError) { p.snapshot.Transaction.BeginSucceeded = 1 },
		func(p *competitionHeartbeatDatabaseError) { p.snapshot.Transaction.CallbackEntered = 1 },
		func(p *competitionHeartbeatDatabaseError) { p.snapshot.Transaction.CommitAttempts = 1 },
		func(p *competitionHeartbeatDatabaseError) { p.snapshot.Transaction.CommitAcknowledged = 1 },
		func(p *competitionHeartbeatDatabaseError) { p.snapshot.Transaction.RollbackAttempts = 1 },
		func(p *competitionHeartbeatDatabaseError) { p.snapshot.Transaction.RollbackAcknowledged = 1 },
		func(p *competitionHeartbeatDatabaseError) { p.snapshot.Transaction.LastBegin.Returned = true },
	} {
		proof := competitionHeartbeatAcquireFailure(context.DeadlineExceeded)
		mutate(&proof)
		if competitionHeartbeatUnadmittedDeadline(proof) {
			t.Fatal("incomplete/previously admitted invocation authenticated continuation", proof.snapshot)
		}
	}
}

// Db host registration has no extra authority from a nearby successful job ack.
func TestCompetitionHostAcquireProofRetainsItsOwnCumulativeAdmission(t *testing.T) {
	for _, mutate := range []func(*competitionHostHeartbeatAcquireError){
		func(p *competitionHostHeartbeatAcquireError) { p.callbackEntered = true },
		func(p *competitionHostHeartbeatAcquireError) { p.acquire.Admitted = true },
		func(p *competitionHostHeartbeatAcquireError) { p.acquire.Returned = false },
		func(p *competitionHostHeartbeatAcquireError) { p.read.Finished = false },
		func(p *competitionHostHeartbeatAcquireError) { p.read.Phase = server.DbReadComplete },
		func(p *competitionHostHeartbeatAcquireError) { p.read.AcquireSucceeded = 1 },
		func(p *competitionHostHeartbeatAcquireError) { p.read.PhaseCounts[server.DbReadAcquireBegin] = 2 },
		func(p *competitionHostHeartbeatAcquireError) { p.read.PhaseCounts[server.DbReadAcquireDone] = 0 },
		func(p *competitionHostHeartbeatAcquireError) { p.read.PhaseCounts[server.DbReadQueryBegin] = 1 },
		func(p *competitionHostHeartbeatAcquireError) { p.read.QuerySucceeded = 1 },
		func(p *competitionHostHeartbeatAcquireError) { p.read.Rows = 1 },
	} {
		proof := competitionHostAcquireFailure(context.DeadlineExceeded)
		mutate(&proof)
		if competitionHostHeartbeatUnadmittedDeadline(proof) {
			t.Fatal("host proof forgot earlier admission or callback", proof)
		}
	}
	observation := newCompetitionHostHeartbeatObservation()
	err := observation.capture(func() {
		observation.read.BeginAcquire()
		observation.read.FinishAcquire(true)
		observation.callbackEntered = true
		observation.read.BeginAcquire()
		observation.read.FinishAcquire(false)
		panic(context.DeadlineExceeded)
	})
	if competitionHostHeartbeatUnadmittedDeadline(err) {
		t.Fatal("real observation capture erased the earlier admitted inner attempt")
	}
}

// A private proof must enclose the entire error; arbitrary Is/As or an outer
// Join branch cannot authenticate a differently sourced deadline or SQL error.
func TestCompetitionHeartbeatAcquireProofRejectsMixedAndUnknownCauses(t *testing.T) {
	for _, cause := range []error{context.Canceled, ErrLeaseLost, errors.New("Done"),
		errors.Join(context.DeadlineExceeded, context.Canceled), errors.Join(context.DeadlineExceeded, &pgconn.PgError{Code: "55P03"}),
		&net.OpError{Op: "read", Net: "synthetic", Err: context.DeadlineExceeded},
		competitionSupervisorUnknownDeadline{cause: context.DeadlineExceeded},
		errors.Join(context.DeadlineExceeded, &competitionLeaseRetryCycleError{}),
		server.NewErrorCauseBatch([]error{context.DeadlineExceeded, nil}),
	} {
		if competitionHeartbeatUnadmittedDeadline(competitionHeartbeatAcquireFailure(cause)) || competitionHostHeartbeatUnadmittedDeadline(competitionHostAcquireFailure(cause)) {
			t.Fatalf("mixed/unknown acquisition graph passed: %T", cause)
		}
	}
	proof := competitionHeartbeatAcquireFailure(context.DeadlineExceeded)
	for _, outside := range []error{context.DeadlineExceeded, fmt.Errorf("synthetic wrapper: %w", proof), errors.Join(proof, context.DeadlineExceeded), errors.Join(proof, proof)} {
		if competitionHeartbeatUnadmittedDeadline(outside) {
			t.Fatalf("unenclosed proof supplied authority: %T", outside)
		}
	}
}

// The successor gets its full original per-call budget after the first owned
// deadline; neither the evaluator's parent nor confirmed lease is renewed.
func TestCompetitionHeartbeatAcquireRetryRecoversWithFreshFullAttemptBudget(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		settings := defaultCompetitionHeartbeatSettings()
		started := server.NowUtc()
		parent, cancel := context.WithTimeout(t.Context(), time.Minute)
		defer cancel()
		calls := 0
		var first context.Context
		ack, err := retryCompetitionHeartbeatOperation(parent, started.Add(time.Minute), settings, false, func(ctx context.Context) error {
			calls++
			deadline, ok := ctx.Deadline()
			if !ok || deadline.Sub(server.NowUtc()) != settings.AcquireRetry.AttemptTimeout {
				t.Fatal("healthy attempt lost its full independent budget")
			}
			if calls == 1 {
				first = ctx
				<-ctx.Done()
				return competitionHeartbeatAcquireFailure(ctx.Err())
			}
			if ctx == first || ctx.Err() != nil || first.Err() != context.DeadlineExceeded {
				t.Fatal("recovery reused expired acquisition ownership")
			}
			time.Sleep(settings.AcquireRetry.AttemptTimeout - time.Nanosecond)
			return nil
		})
		if err != nil || calls != 2 || !ack.Equal(started.Add(settings.AcquireRetry.AttemptTimeout+settings.LeaseRetry.Interval)) || parent.Err() != nil {
			t.Fatal("wholly unadmitted deadline failed recovery", ack, calls, err)
		}
	})
}

// Three calls cannot each restart the total operation reserve.
func TestCompetitionHeartbeatAcquireRetryKeepsOneOuterDeadlineAndCallCap(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		settings := defaultCompetitionHeartbeatSettings()
		started := server.NowUtc()
		calls := 0
		ack, err := retryCompetitionHeartbeatOperation(t.Context(), started.Add(time.Minute), settings, false, func(ctx context.Context) error {
			calls++
			deadline, _ := ctx.Deadline()
			if deadline.After(started.Add(settings.AcquireRetry.Timeout)) {
				t.Fatal("attempt extended the original outer reserve")
			}
			<-ctx.Done()
			return competitionHeartbeatAcquireFailure(ctx.Err())
		})
		if !errors.Is(err, context.DeadlineExceeded) || calls != settings.LeaseRetry.MaximumAttempts || !ack.IsZero() || !server.NowUtc().Equal(started.Add(settings.AcquireRetry.Timeout)) {
			t.Fatal("failed acquisitions restarted their reserve or acknowledged a lease", calls, ack, err)
		}
	})
}

// Known-expired authority does not dispatch; an unknown expiry permits only
// the historical first call. Neither can fund another eight-second attempt.
func TestCompetitionHeartbeatAcquireRetryRejectsExpiredOrUnknownAuthority(t *testing.T) {
	for _, unknown := range []bool{false, true} {
		synctest.Test(t, func(t *testing.T) {
			settings := defaultCompetitionHeartbeatSettings()
			expires := server.NowUtc()
			if unknown {
				expires = time.Time{}
			}
			calls := 0
			ack, err := retryCompetitionHeartbeatOperation(t.Context(), expires, settings, false, func(ctx context.Context) error {
				calls++
				<-ctx.Done()
				return competitionHeartbeatAcquireFailure(ctx.Err())
			})
			if !ack.IsZero() || (!unknown && (!errors.Is(err, ErrLeaseLost) || calls != 0)) || (unknown && (!errors.Is(err, context.DeadlineExceeded) || calls != 1)) {
				t.Fatal("unknown/expired authority was invented", unknown, calls, ack, err)
			}
		})
	}
}

// Failed call cleanup may outlast its lease; no late success or deadline
// observation can be adopted as an acknowledged extension after that boundary.
func TestCompetitionHeartbeatAcquireRetryRejectsLateCleanupAndAcknowledgement(t *testing.T) {
	for _, success := range []bool{false, true} {
		synctest.Test(t, func(t *testing.T) {
			settings := defaultCompetitionHeartbeatSettings()
			started := server.NowUtc()
			calls := 0
			ack, err := retryCompetitionHeartbeatOperation(t.Context(), started.Add(10*time.Second), settings, false, func(ctx context.Context) error {
				calls++
				<-ctx.Done()
				time.Sleep(3 * time.Second)
				if success {
					return nil
				}
				return competitionHeartbeatAcquireFailure(ctx.Err())
			})
			if !errors.Is(err, ErrLeaseLost) || calls != 1 || !ack.IsZero() {
				t.Fatal("late cleanup/result extended expired authority", success, calls, ack, err)
			}
		})
	}
}

// Parent cancellation is checked after acknowledged backoff and before any
// successor dispatch. It never leaks into a fresh independent parent lifetime.
func TestCompetitionHeartbeatAcquireRetryStopsAtParentCancellation(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		settings := defaultCompetitionHeartbeatSettings()
		settings.LeaseRetry.Wait = func(wait context.Context, _ time.Duration) error {
			cancel()
			<-wait.Done()
			return wait.Err()
		}
		calls := 0
		ack, err := retryCompetitionHeartbeatOperation(ctx, server.NowUtc().Add(time.Minute), settings, false, func(attempt context.Context) error {
			calls++
			<-attempt.Done()
			return competitionHeartbeatAcquireFailure(attempt.Err())
		})
		if !errors.Is(err, context.Canceled) || calls != 1 || !ack.IsZero() {
			t.Fatal("canceled heartbeat dispatched a successor", calls, ack, err)
		}
	})
}

// Callback/commit admission is not recoverable even if the last error/phase
// otherwise resembles the observed production acquisition failure.
func TestCompetitionHeartbeatAcquireRetryDoesNotBroadenAdmittedDeadlines(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		settings := defaultCompetitionHeartbeatSettings()
		calls := 0
		_, err := retryCompetitionHeartbeatOperation(t.Context(), server.NowUtc().Add(time.Minute), settings, false, func(ctx context.Context) error {
			calls++
			<-ctx.Done()
			proof := competitionHeartbeatAcquireFailure(ctx.Err())
			proof.snapshot.Transaction.AcquireSucceeded = 1
			return proof
		})
		if !errors.Is(err, context.DeadlineExceeded) || calls != 1 {
			t.Fatal("admitted or unknown operation acquired continuation", calls, err)
		}
	})
}

// The original scoring deadline still clamps a shorter recovery successor.
func TestCompetitionHeartbeatAcquireRetryRetainsOriginalEvaluationDeadline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		settings := defaultCompetitionHeartbeatSettings()
		started := server.NowUtc()
		ctx, cancel := context.WithDeadline(t.Context(), started.Add(10*time.Second))
		defer cancel()
		calls := 0
		ack, err := retryCompetitionHeartbeatOperation(ctx, started.Add(time.Minute), settings, false, func(attempt context.Context) error {
			calls++
			deadline, _ := attempt.Deadline()
			if deadline.After(started.Add(10 * time.Second)) {
				t.Fatal("acquisition moved the original evaluation deadline")
			}
			<-attempt.Done()
			return competitionHeartbeatAcquireFailure(attempt.Err())
		})
		if !errors.Is(err, context.DeadlineExceeded) || calls != 2 || !ack.IsZero() || !server.NowUtc().Equal(started.Add(10*time.Second)) {
			t.Fatal("evaluation deadline was not the actual outer stop", calls, ack, err)
		}
	})
}

// Recovery still consults the actual store fence; a new no-admission allowance
// cannot turn the old owner's next authority refusal into an acknowledgement.
func TestCompetitionHeartbeatAcquireRetryPreservesSuccessorFenceRefusal(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		settings := defaultCompetitionHeartbeatSettings()
		calls := 0
		ack, err := retryCompetitionHeartbeatOperation(t.Context(), server.NowUtc().Add(time.Minute), settings, false, func(ctx context.Context) error {
			calls++
			if calls == 1 {
				<-ctx.Done()
				return competitionHeartbeatAcquireFailure(ctx.Err())
			}
			return ErrLeaseLost
		})
		if !errors.Is(err, ErrLeaseLost) || calls != 2 || !ack.IsZero() {
			t.Fatal("acquisition continuation ignored actual successor fencing", calls, ack, err)
		}
	})
}
