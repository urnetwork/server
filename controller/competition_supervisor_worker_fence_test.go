// The actual worker-fence owner survives closed transient refusals but stops
// independently at confirmed expiry, including while database cleanup blocks.
package controller

import (
	"context"
	"errors"
	"sync"
	"testing"
	"testing/synctest"
	"time"
)

// An expired initial window cannot dispatch either a check or an evaluator.
func TestCompetitionSupervisorWorkerFenceRejectsExpiredInitialAuthority(t *testing.T) {
	readCalls, runCalls := 0, 0
	err := runCompetitionSupervisorWorkerFenced(t.Context(), time.Now(), defaultCompetitionSupervisorWorkerFenceSettings(),
		func(context.Context) (time.Time, error) { readCalls++; return time.Time{}, nil },
		func(context.Context) error { runCalls++; return nil })
	if !errors.Is(err, ErrLeaseLost) || readCalls != 0 || runCalls != 0 {
		t.Fatal("expired authority dispatched work", readCalls, runCalls, err)
	}
}

// A missing cadence cannot create an unbounded zero-delay liveness loop.
func TestCompetitionSupervisorWorkerFenceRejectsInvalidCadence(t *testing.T) {
	settings := defaultCompetitionSupervisorWorkerFenceSettings()
	settings.Interval = 0
	calls := 0
	err := runCompetitionSupervisorWorkerFenced(t.Context(), time.Now().Add(time.Minute), settings,
		func(context.Context) (time.Time, error) { calls++; return time.Time{}, nil },
		func(context.Context) error { calls++; return nil })
	if err == nil || calls != 0 {
		t.Fatal("invalid cadence dispatched an owner", calls, err)
	}
}

// A fully unwound acquisition refusal can recover while the original worker
// retains its immutable three-hour deadline. Only the real read moves expiry.
func TestCompetitionSupervisorWorkerFenceRecoversWithoutRestartOrClockReset(t *testing.T) {
	for _, readOnly := range []bool{false, true} {
		synctest.Test(t, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			settings := defaultCompetitionSupervisorWorkerFenceSettings()
			ticks, accepted := make(chan time.Time), make(chan struct{}, 1)
			settings.tickForTest, settings.confirmedForTest = ticks, func() { accepted <- struct{}{} }
			started := time.Now()
			originalDeadline := started.Add(3 * time.Hour)
			readCalls, runCalls := 0, 0
			workerContext := make(chan context.Context, 1)
			joined, done := make(chan struct{}), make(chan error, 1)
			go func() {
				done <- runCompetitionSupervisorWorkerFenced(ctx, started.Add(time.Minute), settings,
					func(attempt context.Context) (time.Time, error) {
						readCalls++
						if readCalls == 1 {
							<-attempt.Done()
							if readOnly {
								proof := competitionSupervisorReadLockFailure()
								proof.step = competitionSupervisorWorkerFenceRead
								proof.readPrefix = competitionSupervisorReadPrefixSnapshot{workerFence: true, valid: true, count: 1, returned: 1}
								return time.Time{}, proof
							}
							return time.Time{}, competitionSupervisorAcquireFailure(attempt.Err())
						}
						return time.Now().Add(time.Minute), nil
					}, func(parent context.Context) error {
						runCalls++
						evaluation, stop := context.WithDeadline(parent, originalDeadline)
						defer stop()
						workerContext <- evaluation
						<-evaluation.Done()
						close(joined)
						return evaluation.Err()
					})
			}()
			evaluation := <-workerContext
			ticks <- started
			select {
			case <-accepted:
			case <-evaluation.Done():
				t.Fatal("proven fence-read acquisition refusal restarted/canceled the worker")
			}
			// Advance beyond the request's original expiry, but not the independently
			// observed later expiry. No additional read or renewal is dispatched.
			<-time.After(started.Add(time.Minute + time.Second).Sub(time.Now()))
			deadline, present := evaluation.Deadline()
			if !present || !deadline.Equal(originalDeadline) || evaluation.Err() != nil || readCalls != 2 || runCalls != 1 {
				t.Fatal("read recovery altered worker count, original clock or returned expiry", readCalls, runCalls)
			}
			cancel()
			if err := <-done; !errors.Is(err, context.Canceled) {
				t.Fatal("ordinary parent cancellation lost its joined worker", err)
			}
			<-joined
		})
	}
}

// A reader blocked beyond expiry must not keep its evaluator alive. Returning
// later with a future expiry cannot revive it, and both owners must join.
func TestCompetitionSupervisorWorkerFenceExpiresBeforeBlockedReadReturns(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		settings := defaultCompetitionSupervisorWorkerFenceSettings()
		watchdog, stop := context.WithTimeout(t.Context(), time.Minute)
		defer stop()
		ticks := make(chan time.Time)
		settings.tickForTest = ticks
		started := time.Now()
		readEntered, releaseRead := make(chan struct{}), make(chan struct{})
		runEntered, runCanceled, releaseRun := make(chan struct{}), make(chan struct{}), make(chan struct{})
		var readOnce, runOnce sync.Once
		t.Cleanup(func() { readOnce.Do(func() { close(releaseRead) }); runOnce.Do(func() { close(releaseRun) }) })
		done := make(chan error, 1)
		go func() {
			done <- runCompetitionSupervisorWorkerFenced(watchdog, started.Add(10*time.Second), settings,
				func(context.Context) (time.Time, error) {
					close(readEntered)
					<-releaseRead
					return time.Now().Add(time.Hour), nil
				}, func(ctx context.Context) error {
					close(runEntered)
					<-ctx.Done()
					close(runCanceled)
					<-releaseRun
					return ctx.Err()
				})
		}()
		<-runEntered
		ticks <- started
		<-readEntered
		<-runCanceled
		if time.Since(started) != 10*time.Second {
			t.Fatal("worker cancellation did not match its original confirmed expiry", time.Since(started))
		}
		runOnce.Do(func() { close(releaseRun) })
		synctest.Wait()
		select {
		case err := <-done:
			t.Fatal("worker wrapper returned before the blocked reader joined", err)
		default:
		}
		readOnce.Do(func() { close(releaseRead) })
		if err := <-done; !errors.Is(err, ErrLeaseLost) {
			t.Fatal("late positive read revived expired authority or replaced its cause", err)
		}
	})
}

// Explicit negative authority is terminal immediately, not a transient read
// error. Its original cause must survive cancellation and joined cleanup.
func TestCompetitionSupervisorWorkerFenceStopsImmediatelyOnRevocation(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		settings := defaultCompetitionSupervisorWorkerFenceSettings()
		ticks := make(chan time.Time)
		settings.tickForTest = ticks
		started, calls := time.Now(), 0
		entered := make(chan struct{})
		done := make(chan error, 1)
		go func() {
			done <- runCompetitionSupervisorWorkerFenced(t.Context(), started.Add(time.Minute), settings,
				func(context.Context) (time.Time, error) { calls++; return time.Time{}, ErrLeaseLost },
				func(ctx context.Context) error { close(entered); <-ctx.Done(); return ctx.Err() })
		}()
		<-entered
		ticks <- started
		if err := <-done; !errors.Is(err, ErrLeaseLost) || calls != 1 || time.Since(started) != 0 {
			t.Fatal("explicit revocation was delayed, retried or lost", calls, time.Since(started), err)
		}
	})
}
