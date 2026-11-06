// Virtual time and exact callback generations reproduce expiry independently
// of database return, stale timers, late acknowledgments and wall-clock steps.
package controller

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
)

// An owner must be canceled at its confirmed boundary even if no database
// callback can return to the renewal loop. A later ack cannot revive it.
func TestCompetitionConfirmedAuthorityExpiresWithoutDatabaseReturn(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		started := time.Now()
		watchdog, stop := context.WithTimeout(t.Context(), time.Minute)
		defer stop()
		authority, err := newCompetitionConfirmedAuthority(watchdog, started.Add(time.Second), time.Now)
		if err != nil {
			t.Fatal(err)
		}
		defer authority.close()
		<-authority.ctx.Done()
		if !errors.Is(context.Cause(authority.ctx), ErrLeaseLost) || time.Since(started) != time.Second || authority.confirm(time.Now().Add(time.Minute)) == nil {
			t.Fatal("confirmed expiry waited for database return or accepted a late acknowledgment")
		}
	})
}

// Calling the old queued callback is the exact timer reset race, independent
// of scheduling luck. Only the currently acknowledged generation may expire.
func TestCompetitionConfirmedAuthorityIgnoresStaleQueuedTimer(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		started := time.Now()
		authority, err := newCompetitionConfirmedAuthority(t.Context(), started.Add(time.Second), time.Now)
		if err != nil {
			t.Fatal(err)
		}
		defer authority.close()
		if err := authority.confirm(started.Add(2 * time.Second)); err != nil {
			t.Fatal(err)
		}
		authority.expire(1)
		if authority.ctx.Err() != nil {
			t.Fatal("stale queued timer canceled a timely confirmed window")
		}
		<-authority.ctx.Done()
		if time.Since(started) != 2*time.Second || !errors.Is(context.Cause(authority.ctx), ErrLeaseLost) {
			t.Fatal("current confirmed generation did not expire at its own boundary")
		}
	})
}

// A delayed timer is modeled by its elapsed monotonic state. Moving the
// authority clock backward must not authorize adoption before it fires.
func TestCompetitionConfirmedAuthorityRejectsBackwardClockRevival(t *testing.T) {
	now := time.Now()
	authority, err := newCompetitionConfirmedAuthority(t.Context(), now.Add(time.Minute), func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	defer authority.close()
	func() {
		authority.stateLock.Lock()
		defer authority.stateLock.Unlock()
		authority.deadline = time.Now().Add(-time.Second)
	}()
	now = now.Add(-time.Hour)
	if err := authority.confirm(now.Add(time.Hour)); !errors.Is(err, ErrLeaseLost) {
		t.Fatal("backward authority clock revived an elapsed monotonic window", err)
	}
}

// Observing the same DB expiry after a backward clock step is not a renewal.
// The already-established monotonic boundary must remain exactly unchanged.
func TestCompetitionConfirmedAuthorityCannotGrowUnchangedExpiryOnClockRollback(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		started, now := time.Now(), time.Now()
		expiresAt := started.Add(time.Second)
		authority, err := newCompetitionConfirmedAuthority(t.Context(), expiresAt, func() time.Time { return now })
		if err != nil {
			t.Fatal(err)
		}
		defer authority.close()
		now = now.Add(-time.Hour)
		if err := authority.confirm(expiresAt); err != nil {
			t.Fatal(err)
		}
		<-authority.ctx.Done()
		if time.Since(started) != time.Second || !errors.Is(context.Cause(authority.ctx), ErrLeaseLost) {
			t.Fatal("unchanged read borrowed time from a wall-clock rollback")
		}
	})
}

// A virtual fractional microsecond makes the old truncated-clock timer late
// deterministically; database expiry must map no later than its actual instant.
func TestCompetitionConfirmedAuthorityDefaultClockDoesNotRoundExpiryUp(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		synctest.Sleep(500 * time.Nanosecond)
		expiresAt := time.Now().UTC().Truncate(time.Microsecond).Add(time.Second)
		authority, err := newCompetitionConfirmedAuthority(t.Context(), expiresAt, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer authority.close()
		<-authority.ctx.Done()
		if time.Now().UTC().After(expiresAt) || !errors.Is(context.Cause(authority.ctx), ErrLeaseLost) {
			t.Fatal("microsecond-truncated observation rounded confirmed authority up")
		}
	})
}

// Parent cancellation is the first cause; a concurrent expiry cannot replace
// its identity or allow a new acknowledgment after the lifetime has ended.
func TestCompetitionConfirmedAuthorityPreservesParentCancellation(t *testing.T) {
	cause := errors.New("synthetic parent shutdown")
	ctx, cancel := context.WithCancelCause(t.Context())
	authority, err := newCompetitionConfirmedAuthority(ctx, time.Now().Add(time.Minute), time.Now)
	if err != nil {
		t.Fatal(err)
	}
	defer authority.close()
	cancel(cause)
	authority.expire(1)
	if context.Cause(authority.ctx) != cause || authority.confirm(time.Now().Add(time.Hour)) == nil {
		t.Fatal("expiry replaced parent cancellation or admitted a new window")
	}
}

// Force the callback's closed-state publication before its out-of-lock cancel.
// Normal completion cannot overwrite a recorded expiry with context.Canceled.
func TestCompetitionConfirmedAuthorityClosePreservesPublishedExpiry(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		for _, parentFirst := range []bool{false, true} {
			parent, cancelParent := context.WithCancelCause(t.Context())
			authority, err := newCompetitionConfirmedAuthority(parent, time.Now().Add(time.Minute), time.Now)
			if err != nil {
				t.Fatal(err)
			}
			originalCancel := authority.cancel
			published, release, joined := make(chan struct{}), make(chan struct{}), make(chan struct{})
			var calls atomic.Int32
			authority.cancel = func(cause error) {
				if cause == ErrLeaseLost && calls.Add(1) == 1 {
					close(published)
					<-release
				}
				originalCancel(cause)
			}
			go func() { defer close(joined); authority.expire(1) }()
			<-published
			want := error(ErrLeaseLost)
			if parentFirst {
				want = errors.New("synthetic parent canceled before expiry publication completed")
				cancelParent(want)
			}
			authority.close()
			got := context.Cause(authority.ctx)
			close(release)
			<-joined
			cancelParent(nil)
			if got != want || context.Cause(authority.ctx) != want {
				t.Fatal("normal cleanup replaced published expiry or earlier parent cause", got, want)
			}
		}
	})
}

// The real reconciler starts one worker and blocks its renewal past confirmed
// expiry. Cancellation must precede DB release; completion still joins both.
func TestCompetitionSupervisorAuthorityExpiryCancelsBeforeRenewalUnwinds(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		fixture := newCompetitionLeaseRetrySupervisorFixture(t)
		watchdog, stopWatchdog := context.WithTimeout(t.Context(), 10*time.Minute)
		defer stopWatchdog()
		fixture.watchdog = watchdog
		fixture.settings.LockRetry.Now = time.Now
		fixture.clock.now = time.Now()
		renewEntered, releaseRenewal := make(chan struct{}), make(chan struct{})
		var releaseOnce sync.Once
		t.Cleanup(func() { releaseOnce.Do(func() { close(releaseRenewal) }) })
		fixture.driver.renewWithContext = func(context.Context, int) error {
			close(renewEntered)
			<-releaseRenewal
			return context.DeadlineExceeded
		}
		fixture.Start(t)
		fixture.ticks <- time.Now()
		<-renewEntered
		select {
		case <-fixture.driver.canceled:
		case <-watchdog.Done():
			t.Fatal("confirmed expiry waited for the blocked database return")
		}
		fixture.release.Do(func() { close(fixture.driver.release) })
		synctest.Wait()
		select {
		case result := <-fixture.done:
			t.Fatal("supervisor returned before joining its blocked renewal", result)
		default:
		}
		releaseOnce.Do(func() { close(releaseRenewal) })
		result := <-fixture.done
		if result.state != "lease_lost" || !errors.Is(result.err, ErrLeaseLost) || !fixture.driver.joined || fixture.driver.renewCalls != 1 {
			t.Fatal("confirmed expiry lost its fencing cause or joined ownership", result.state, result.err)
		}
	})
}
