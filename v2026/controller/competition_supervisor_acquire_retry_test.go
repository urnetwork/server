// Owned virtual deadlines and exact observation mutations force renewal
// admission, refusal and old-budget boundaries without a database or wall sleeps.
package controller

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"testing"
	"testing/synctest"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/urnetwork/server/v2026"
)

// Synthetic observations describe one returned, wholly unadmitted invocation.
// Separate deferred PG tests authenticate the real production observation path.
func competitionSupervisorAcquireFailure(cause error) competitionSupervisorRenewError {
	read := server.DbReadObservationSnapshot{Phase: server.DbReadError, Finished: true}
	read.PhaseCounts[server.DbReadAcquireBegin], read.PhaseCounts[server.DbReadAcquireDone] = 1, 1
	return competitionSupervisorRenewError{
		cause: competitionDatabasePhaseError{cause: cause, phase: server.DbOperationAcquire}, phase: server.DbOperationAcquire,
		acquire: server.DbAcquireObservation{Attempt: 1, Returned: true,
			BeforeAcquire: server.DbContextBudget{State: "live", HasDeadline: true, RemainingNs: int64(time.Second)},
			AtReturn:      server.DbContextBudget{State: "deadline", HasDeadline: true}}, read: read}
}

// Unknown nonleaf wrappers may carry independent meaning despite Unwrap.
type competitionSupervisorUnknownDeadline struct{ cause error }

// Fixed test-only diagnostics contain no environment identity.
func (self competitionSupervisorUnknownDeadline) Error() string { return "synthetic unknown wrapper" }

// Transparent shape alone must not authenticate an unknown wrapper's semantics.
func (self competitionSupervisorUnknownDeadline) Unwrap() error { return self.cause }

// Exact legacy Done identity is allowed only beside the real deadline sentinel.
func TestCompetitionSupervisorAcquireProofAcceptsCompleteDeadline(t *testing.T) {
	cause := errors.Join(server.DbContextDoneError, fmt.Errorf("synthetic acquired-from boundary: %w", context.DeadlineExceeded))
	proof := competitionSupervisorAcquireFailure(cause)
	if !competitionSupervisorUnadmittedDeadline(proof) || proof.Error() != cause.Error() || !errors.Is(proof, context.DeadlineExceeded) || !errors.Is(proof, server.DbContextDoneError) {
		t.Fatal("complete no-admission proof changed cause identity or refused its owned deadline")
	}
	phase := competitionHeartbeatFailureDetails(proof)
	if phase.DatabasePhase != server.DbOperationAcquire || phase.Category != "deadline_exceeded" {
		t.Fatal("renewal proof lost finite phase diagnostics", phase)
	}
	postgres := &pgconn.PgError{Code: "55P03"}
	wrapped := competitionSupervisorAcquireFailure(postgres)
	var actual *pgconn.PgError
	if !errors.As(wrapped, &actual) || actual != postgres || wrapped.Error() != postgres.Error() {
		t.Fatal("observational wrapper changed PostgreSQL error identity")
	}
}

// Last-phase Acquire cannot erase prior admission, callback or incomplete state.
func TestCompetitionSupervisorAcquireProofRejectsIncompleteInvocation(t *testing.T) {
	for _, check := range []struct {
		name   string
		mutate func(*competitionSupervisorRenewError)
	}{
		{name: "earlier admitted inner retry", mutate: func(p *competitionSupervisorRenewError) { p.read.AcquireSucceeded = 1 }},
		{name: "callback entered", mutate: func(p *competitionSupervisorRenewError) { p.callbackEntered = true }},
		{name: "unknown phase", mutate: func(p *competitionSupervisorRenewError) { p.phase = server.DbOperationUnknown }},
		{name: "begin phase", mutate: func(p *competitionSupervisorRenewError) { p.phase = server.DbOperationBegin }},
		{name: "callback phase", mutate: func(p *competitionSupervisorRenewError) { p.phase = server.DbOperationCallback }},
		{name: "commit phase", mutate: func(p *competitionSupervisorRenewError) { p.phase = server.DbOperationCommit }},
		{name: "acquire not returned", mutate: func(p *competitionSupervisorRenewError) { p.acquire.Returned = false }},
		{name: "last acquire admitted", mutate: func(p *competitionSupervisorRenewError) { p.acquire.Admitted = true }},
		{name: "no attempt", mutate: func(p *competitionSupervisorRenewError) { p.acquire.Attempt = 0 }},
		{name: "incomplete acquire census", mutate: func(p *competitionSupervisorRenewError) { p.read.PhaseCounts[server.DbReadAcquireDone] = 0 }},
		{name: "earlier attempt omitted", mutate: func(p *competitionSupervisorRenewError) { p.acquire.Attempt = 2 }},
		{name: "read not finished", mutate: func(p *competitionSupervisorRenewError) { p.read.Finished = false }},
		{name: "finished as success", mutate: func(p *competitionSupervisorRenewError) { p.read.Phase = server.DbReadComplete }},
		{name: "query entered", mutate: func(p *competitionSupervisorRenewError) { p.read.PhaseCounts[server.DbReadQueryBegin] = 1 }},
		{name: "query completed", mutate: func(p *competitionSupervisorRenewError) { p.read.QuerySucceeded = 1 }},
		{name: "row observed", mutate: func(p *competitionSupervisorRenewError) { p.read.Rows = 1 }},
		{name: "caller already stopped", mutate: func(p *competitionSupervisorRenewError) { p.acquire.BeforeAcquire.State = "deadline" }},
		{name: "no entry deadline", mutate: func(p *competitionSupervisorRenewError) { p.acquire.BeforeAcquire.HasDeadline = false }},
		{name: "entry allowance exhausted", mutate: func(p *competitionSupervisorRenewError) { p.acquire.BeforeAcquire.RemainingNs = 0 }},
		{name: "return still live", mutate: func(p *competitionSupervisorRenewError) { p.acquire.AtReturn.State = "live" }},
		{name: "return allowance remains", mutate: func(p *competitionSupervisorRenewError) { p.acquire.AtReturn.RemainingNs = 1 }},
		{name: "return deadline absent", mutate: func(p *competitionSupervisorRenewError) { p.acquire.AtReturn.HasDeadline = false }},
	} {
		proof := competitionSupervisorAcquireFailure(context.DeadlineExceeded)
		check.mutate(&proof)
		if competitionSupervisorUnadmittedDeadline(proof) {
			t.Errorf("%s authenticated an incomplete invocation", check.name)
		}
	}
}

// A known proof must enclose the entire cause; mixed leaves and unknown
// wrappers, including network wrappers around a deadline, remain terminal.
func TestCompetitionSupervisorAcquireProofRejectsMixedOrUnenclosedCauses(t *testing.T) {
	proof := competitionSupervisorAcquireFailure(context.DeadlineExceeded)
	for _, cause := range []error{
		context.Canceled, ErrLeaseLost, errors.New("Done"), errors.New("synthetic unknown"),
		errors.Join(context.DeadlineExceeded, context.Canceled),
		errors.Join(context.DeadlineExceeded, errors.New("Done")),
		errors.Join(context.DeadlineExceeded, &pgconn.PgError{Code: "55P03"}),
		errors.Join(context.DeadlineExceeded, &pgconn.PgError{Code: "08006"}),
		&net.OpError{Op: "read", Net: "synthetic", Err: context.DeadlineExceeded},
		&os.SyscallError{Syscall: "synthetic", Err: context.DeadlineExceeded},
		competitionSupervisorUnknownDeadline{cause: context.DeadlineExceeded},
		errors.Join(context.DeadlineExceeded, &competitionLeaseRetryCycleError{}),
		errors.Join(context.DeadlineExceeded, (*pgconn.PgError)(nil)),
		server.NewErrorCauseBatch([]error{context.DeadlineExceeded, nil}),
	} {
		if competitionSupervisorUnadmittedDeadline(competitionSupervisorAcquireFailure(cause)) {
			t.Errorf("mixed or unknown enclosed cause passed: %T", cause)
		}
	}
	for _, outside := range []error{context.DeadlineExceeded, fmt.Errorf("synthetic outer wrapper: %w", proof), errors.Join(proof, context.DeadlineExceeded), errors.Join(proof, proof)} {
		if competitionSupervisorUnadmittedDeadline(outside) {
			t.Errorf("a branch or external deadline proved the entire invocation: %T", outside)
		}
	}
}

// A successful inner acquisition remains in the cumulative observation after
// a later refused one; the final finite phase is deliberately misleading here.
func TestCompetitionSupervisorAcquireProofRetainsEarlierAdmission(t *testing.T) {
	observation := newCompetitionSupervisorRenewObservation()
	err := observation.capture(func() {
		observation.read.BeginAcquire()
		observation.read.FinishAcquire(true)
		observation.callbackEntered = true
		observation.read.BeginAcquire()
		observation.read.FinishAcquire(false)
		panic(context.DeadlineExceeded)
	})
	proof := err.(competitionSupervisorRenewError)
	last := competitionSupervisorAcquireFailure(context.DeadlineExceeded)
	proof.cause, proof.phase, proof.acquire = last.cause, last.phase, last.acquire
	proof.acquire.Attempt = 2
	if proof.read.AcquireSucceeded != 1 || !proof.callbackEntered || competitionSupervisorUnadmittedDeadline(proof) {
		t.Fatal("last refused acquisition erased admitted transaction history")
	}
}

// This is the root-cause positive regression: the first owned deadline must
// not cancel its still-authorized owner before a fresh acquisition can succeed.
func TestCompetitionSupervisorAcquireRetryRecoversAfterOwnedDeadline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		settings := defaultCompetitionSupervisorRenewalSettings()
		started := time.Now()
		calls := 0
		err := retryCompetitionSupervisorRenewal(t.Context(), started.Add(time.Minute), settings, func(ctx context.Context) error {
			calls++
			if calls == 1 {
				<-ctx.Done()
				return competitionSupervisorAcquireFailure(errors.Join(server.DbContextDoneError, ctx.Err()))
			}
			return nil
		})
		if err != nil || calls != 2 || time.Since(started) != settings.AttemptTimeout+settings.LockRetry.Interval {
			t.Fatal("completed unadmitted deadline canceled authority or did not recover", calls, err)
		}
	})
}

// A syntactically valid deadline proof cannot spend extra time if the owned
// attempt is still live; errors.Is(deadline) alone is not permission to retry.
func TestCompetitionSupervisorAcquireRetryRejectsUnexpiredAttempt(t *testing.T) {
	calls := 0
	cause := competitionSupervisorAcquireFailure(context.DeadlineExceeded)
	err := retryCompetitionSupervisorRenewal(t.Context(), time.Now().Add(time.Minute), defaultCompetitionSupervisorRenewalSettings(), func(context.Context) error { calls++; return cause })
	if calls != 1 || err != cause {
		t.Fatal("deadline-shaped error extended a live attempt", calls, err)
	}
}

// A forward clock step before the first dispatch must never report nil success
// with no returned lease (which would later dereference a nil driver result).
func TestCompetitionSupervisorAcquireRetryRejectsInitialClockJump(t *testing.T) {
	for _, atDispatch := range []bool{false, true} {
		for _, elapsed := range []time.Duration{8 * time.Second, 20 * time.Second} {
			settings := defaultCompetitionSupervisorRenewalSettings()
			started := time.Now()
			reads, calls := 0, 0
			settings.LockRetry.Now = func() time.Time {
				reads++
				if reads == 1 || atDispatch && reads == 2 {
					return started
				}
				return started.Add(elapsed)
			}
			err := retryCompetitionSupervisorRenewal(t.Context(), started.Add(time.Minute), settings, func(context.Context) error { calls++; return nil })
			if calls != 0 || err != context.DeadlineExceeded {
				t.Fatal("expired first dispatch fabricated successful renewal", atDispatch, elapsed, calls, err)
			}
		}
	}
}

// Parent cancellation can occur inside an owned clock seam after the earlier
// check. It must prevent dispatch and retain cancellation, not invent a timeout.
func TestCompetitionSupervisorAcquireRetryCancellationDuringDispatchKeepsCause(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	settings := defaultCompetitionSupervisorRenewalSettings()
	started := time.Now()
	reads, calls := 0, 0
	settings.LockRetry.Now = func() time.Time {
		reads++
		if reads == 3 {
			cancel()
		}
		return started
	}
	err := retryCompetitionSupervisorRenewal(ctx, started.Add(time.Minute), settings, func(context.Context) error { calls++; return nil })
	if calls != 0 || err != context.Canceled {
		t.Fatal("dispatch cancellation was lost or started database work", calls, err)
	}
}

// The same precedence holds while observing a returned success: cancellation
// acknowledged before acceptance cannot establish a new rolling lease.
func TestCompetitionSupervisorAcquireRetryCancellationDuringResultKeepsCause(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	settings := defaultCompetitionSupervisorRenewalSettings()
	started := time.Now()
	reads, calls := 0, 0
	settings.LockRetry.Now = func() time.Time {
		reads++
		if reads == 4 {
			cancel()
		}
		return started
	}
	err := retryCompetitionSupervisorRenewal(ctx, started.Add(time.Minute), settings, func(context.Context) error { calls++; return nil })
	if calls != 1 || err != context.Canceled {
		t.Fatal("returned success hid acknowledged parent cancellation", calls, err)
	}
}

// Even after acquisition recovery, PG55 cannot restart the original absolute
// lock retry budget and cannot dispatch a third database call after it expired.
func TestCompetitionSupervisorAcquireRetryDoesNotRestartLockBudget(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		calls := 0
		cause := &pgconn.PgError{Code: "55P03"}
		err := retryCompetitionSupervisorRenewal(t.Context(), time.Now().Add(time.Minute), defaultCompetitionSupervisorRenewalSettings(), func(ctx context.Context) error {
			calls++
			if calls == 1 {
				<-ctx.Done()
				return competitionSupervisorAcquireFailure(ctx.Err())
			}
			return cause
		})
		if calls != 2 || !errors.Is(err, cause) {
			t.Fatal("acquisition recovery restarted the exhausted PG55 budget", calls, err)
		}
	})
}

// UTC wall time can move backward. Ordinary retry attempts must retain the
// original monotonic lock context rather than converting that step into time.
func TestCompetitionSupervisorAcquireRetryBackwardClockKeepsOriginalLockContext(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		settings := defaultCompetitionSupervisorRenewalSettings()
		started, shift := time.Now(), time.Duration(0)
		settings.LockRetry.Now = func() time.Time { return time.Now().Add(shift).UTC() }
		calls := 0
		err := retryCompetitionSupervisorRenewal(t.Context(), started.Add(time.Minute), settings, func(ctx context.Context) error {
			calls++
			if calls == 1 {
				timer := time.NewTimer(2 * time.Second)
				<-timer.C
				shift = -time.Hour
				return &pgconn.PgError{Code: "55P03"}
			}
			deadline, ok := ctx.Deadline()
			if !ok || !deadline.Equal(started.Add(settings.LockRetry.Timeout)) {
				t.Fatal("backward UTC step reopened ordinary attempt deadline", deadline)
			}
			<-ctx.Done()
			return ctx.Err()
		})
		if calls != 2 || !errors.Is(err, context.DeadlineExceeded) || time.Since(started) != settings.LockRetry.Timeout {
			t.Fatal("backward UTC step extended the original monotonic lock owner", calls, err, time.Since(started))
		}
	})
}

// A long ordinary retry wait is also a child of the original lock context;
// clock rollback cannot preserve it until the larger manager deadline.
func TestCompetitionSupervisorAcquireRetryBackwardClockBoundsOrdinaryWait(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		settings := defaultCompetitionSupervisorRenewalSettings()
		started, shift := time.Now(), time.Duration(0)
		settings.LockRetry.Now = func() time.Time { return time.Now().Add(shift).UTC() }
		settings.LockRetry.Interval = time.Minute
		calls := 0
		err := retryCompetitionSupervisorRenewal(t.Context(), started.Add(time.Minute), settings, func(context.Context) error {
			calls++
			timer := time.NewTimer(2 * time.Second)
			<-timer.C
			shift = -time.Hour
			return &pgconn.PgError{Code: "55P03"}
		})
		if calls != 1 || !errors.Is(err, context.DeadlineExceeded) || time.Since(started) != settings.LockRetry.Timeout {
			t.Fatal("backward UTC step let ordinary backoff consume acquisition headroom", calls, err, time.Since(started))
		}
	})
}

// A lock retry still uses the first absolute deadline. Its later unadmitted
// deadline, not the old PG55, may separately authorize the final recovery call.
func TestCompetitionSupervisorAcquireRetryRetainsLockDeadlineAcrossCalls(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		settings := defaultCompetitionSupervisorRenewalSettings()
		started := time.Now()
		calls := 0
		err := retryCompetitionSupervisorRenewal(t.Context(), started.Add(time.Minute), settings, func(ctx context.Context) error {
			calls++
			if calls == 1 {
				return &pgconn.PgError{Code: "55P03"}
			}
			if calls == 2 {
				deadline, ok := ctx.Deadline()
				if !ok || !deadline.Equal(started.Add(settings.LockRetry.Timeout)) {
					t.Fatal("lock retry obtained a new eight-second deadline")
				}
				<-ctx.Done()
				return competitionSupervisorAcquireFailure(ctx.Err())
			}
			return nil
		})
		if calls != 3 || err != nil || time.Since(started) != settings.LockRetry.Timeout+settings.LockRetry.Interval {
			t.Fatal("fixed lock deadline lost separately proven acquisition recovery", calls, err)
		}
	})
}

// Every attempt shares one total deadline; neither a new child nor proof can
// enlarge it. Raising the test cap isolates the deadline from the attempt cap.
func TestCompetitionSupervisorAcquireRetryHasOneTotalBudget(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		settings := defaultCompetitionSupervisorRenewalSettings()
		settings.LockRetry.MaximumAttempts = 20
		started := time.Now()
		calls := 0
		err := retryCompetitionSupervisorRenewal(t.Context(), started.Add(time.Minute), settings, func(ctx context.Context) error {
			calls++
			<-ctx.Done()
			return competitionSupervisorAcquireFailure(ctx.Err())
		})
		if err == nil || calls != 3 || time.Since(started) != settings.Timeout {
			t.Fatal("acquisition retry reset or overran its original total", calls, err, time.Since(started))
		}
	})
}

// A short test attempt leaves ample total/lease time, so only the global call
// cap can stop this sequence; acquisition retries never obtain a separate cap.
func TestCompetitionSupervisorAcquireRetryHasOneAttemptCap(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		settings := defaultCompetitionSupervisorRenewalSettings()
		settings.AttemptTimeout, settings.LockRetry.Timeout = time.Second, time.Second
		calls := 0
		err := retryCompetitionSupervisorRenewal(t.Context(), time.Now().Add(time.Minute), settings, func(ctx context.Context) error {
			calls++
			<-ctx.Done()
			return competitionSupervisorAcquireFailure(ctx.Err())
		})
		if err == nil || calls != settings.LockRetry.MaximumAttempts {
			t.Fatal("acquisition recovery reset the shared call cap", calls, err)
		}
	})
}

// Parent cancellation remains authoritative even when the completed child has
// a valid proof and a later local total/lease would otherwise permit recovery.
func TestCompetitionSupervisorAcquireRetryPreservesParentCancellation(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		calls := 0
		err := retryCompetitionSupervisorRenewal(ctx, time.Now().Add(time.Minute), defaultCompetitionSupervisorRenewalSettings(), func(child context.Context) error {
			calls++
			<-child.Done()
			cancel()
			return competitionSupervisorAcquireFailure(child.Err())
		})
		if calls != 1 || err != context.Canceled {
			t.Fatal("completed child deadline overrode parent cancellation", calls, err)
		}
	})
}

// An earlier parent deadline reaches the actual call unchanged and does not
// turn into another local attempt when the owned operation returns.
func TestCompetitionSupervisorAcquireRetryPreservesParentDeadline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
		defer cancel()
		deadline, _ := ctx.Deadline()
		calls := 0
		err := retryCompetitionSupervisorRenewal(ctx, time.Now().Add(time.Minute), defaultCompetitionSupervisorRenewalSettings(), func(child context.Context) error {
			calls++
			actual, ok := child.Deadline()
			if !ok || !actual.Equal(deadline) {
				t.Fatal("renewal changed its parent's exact deadline")
			}
			<-child.Done()
			return competitionSupervisorAcquireFailure(child.Err())
		})
		if calls != 1 || err != context.DeadlineExceeded {
			t.Fatal("parent deadline granted another local allowance", calls, err)
		}
	})
}

// Cancellation of the sole owner during the acknowledged retry wait cannot
// dispatch the next call or keep its worker alive on a local retry allowance.
func TestCompetitionSupervisorAcquireRetryCancellationStopsBackoff(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		settings := defaultCompetitionSupervisorRenewalSettings()
		settings.LockRetry.Wait = func(context.Context, time.Duration) error { cancel(); return context.Canceled }
		calls := 0
		err := retryCompetitionSupervisorRenewal(ctx, time.Now().Add(time.Minute), settings, func(child context.Context) error {
			calls++
			<-child.Done()
			return competitionSupervisorAcquireFailure(child.Err())
		})
		if calls != 1 || err != context.Canceled {
			t.Fatal("retry wait outlived its canceled owner", calls, err)
		}
	})
}

// Deadline-clamped or unknown authority grants no recovery, and an apparently
// successful call after its attempt deadline cannot establish a fresh lease.
func TestCompetitionSupervisorAcquireRetryRejectsExpiredUnknownAndLateAuthority(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		for _, check := range []struct {
			name        string
			knownLease  bool
			leaseTtl    time.Duration
			lateSuccess bool
			wantedCalls int
			wantedError error
		}{
			{name: "unknown", wantedCalls: 1, wantedError: context.DeadlineExceeded},
			{name: "already expired", knownLease: true, leaseTtl: -time.Second, wantedCalls: 0, wantedError: ErrLeaseLost},
			{name: "expiry", knownLease: true, leaseTtl: time.Second, wantedCalls: 1, wantedError: ErrLeaseLost},
			{name: "late success", knownLease: true, leaseTtl: time.Minute, lateSuccess: true, wantedCalls: 1, wantedError: context.DeadlineExceeded},
		} {
			// Each earlier callback can advance this bubble's virtual clock.
			// A case owns a fresh relative lease, never a stale table timestamp.
			lease := time.Time{}
			if check.knownLease {
				lease = time.Now().Add(check.leaseTtl)
			}
			calls := 0
			err := retryCompetitionSupervisorRenewal(t.Context(), lease, defaultCompetitionSupervisorRenewalSettings(), func(ctx context.Context) error {
				calls++
				<-ctx.Done()
				if check.lateSuccess {
					return nil
				}
				return competitionSupervisorAcquireFailure(ctx.Err())
			})
			if calls != check.wantedCalls || !errors.Is(err, check.wantedError) {
				t.Errorf("%s extended unconfirmed or expired authority: calls=%d error=%v", check.name, calls, err)
			}
		}
	})
}

// The manager's distinct allowance must not change worker heartbeat/completion
// policies or their inherited exact lock-timeout contract.
func TestCompetitionSupervisorAcquireRetryKeepsRoleBudgetsSeparate(t *testing.T) {
	manager := defaultCompetitionSupervisorRenewalSettings()
	worker := defaultCompetitionHeartbeatSettings()
	if manager.Timeout != 20*time.Second || manager.AttemptTimeout != 8*time.Second || manager.LockRetry.Timeout != 8*time.Second ||
		manager.LockRetry.Interval != 250*time.Millisecond || manager.LockRetry.MaximumAttempts != 3 || worker.LeaseRetry.Timeout != 8*time.Second || worker.CompletionRetry.Timeout != 8*time.Second {
		t.Fatal("supervisor recovery changed another role or its original lock budget")
	}
}

// The real Reconcile loop keeps its running worker alive through the completed
// first refusal, accepts the next ack, then synchronously joins normal completion.
func TestCompetitionSupervisorAcquireRetryKeepsRunningWorker(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		fixture := newCompetitionLeaseRetrySupervisorFixture(t)
		watchdog, stop := context.WithTimeout(t.Context(), time.Minute)
		defer stop()
		fixture.watchdog = watchdog
		fixture.driver.renewWithContext = func(ctx context.Context, call int) error {
			if call == 1 {
				<-ctx.Done()
				fixture.clock.Advance(fixture.settings.AttemptTimeout)
				return competitionSupervisorAcquireFailure(ctx.Err())
			}
			return nil
		}
		fixture.Start(t)
		fixture.ticks <- fixture.clock.Now()
		select {
		case <-fixture.accepted:
		case <-fixture.driver.canceled:
			t.Fatal("completed pre-admission deadline canceled the live worker")
		case <-watchdog.Done():
			t.Fatal("supervisor did not acknowledge its recovered renewal")
		}
		fixture.finish.Do(func() { close(fixture.driver.finished) })
		select {
		case result := <-fixture.done:
			if result.err != nil || result.state != "evaluating" || fixture.driver.renewCalls != 2 || !fixture.driver.joined {
				t.Fatal("recovered supervisor lost its normal joined result", result, fixture.driver.renewCalls)
			}
		case <-watchdog.Done():
			t.Fatal("supervisor did not join its normally completed worker")
		}
	})
}

// The same composed path must still cancel and join if an earlier inner
// transaction was admitted, even though the last phase looks recoverable.
func TestCompetitionSupervisorAcquireRetryJoinsAfterEarlierAdmission(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		fixture := newCompetitionLeaseRetrySupervisorFixture(t)
		watchdog, stop := context.WithTimeout(t.Context(), time.Minute)
		defer stop()
		fixture.watchdog = watchdog
		fixture.driver.renewWithContext = func(ctx context.Context, _ int) error {
			<-ctx.Done()
			fixture.clock.Advance(fixture.settings.AttemptTimeout)
			proof := competitionSupervisorAcquireFailure(ctx.Err())
			proof.read.AcquireSucceeded = 1
			return proof
		}
		fixture.Start(t)
		fixture.ticks <- fixture.clock.Now()
		fixture.RequireFailure(t, context.DeadlineExceeded, 1)
	})
}
