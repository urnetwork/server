// Real supervisor reconciliation retains a running worker across transient
// renewals and joins it on terminal failure, using explicit owned-clock edges.
package controller

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/urnetwork/server/v2026/model"
)

// Public driver decoration keeps renewal failures on the real Reconcile path.
type competitionLeaseRetrySupervisorDriver struct {
	*competitionSupervisorFixture
	clock                                *competitionLeaseRetryClock
	started, canceled, release, finished chan struct{}
	renew                                func(int) error
	renewWithContext                     func(context.Context, int) error
	renewCalls                           int
	joined                               bool
}

// Exact current owner/token and expiry are rechecked on every synthetic call.
func (self *competitionLeaseRetrySupervisorDriver) Renew(ctx context.Context, lease model.CompetitionSupervisorLease, ttl time.Duration) (*model.CompetitionSupervisorLease, error) {
	self.renewCalls++
	if self.renewWithContext != nil {
		if err := self.renewWithContext(ctx, self.renewCalls); err != nil {
			return nil, err
		}
	}
	if self.renew != nil {
		if err := self.renew(self.renewCalls); err != nil {
			return nil, err
		}
	}
	if lease.OwnerId != self.lease.OwnerId || lease.FencingToken != self.lease.FencingToken || !self.clock.Now().Before(self.lease.LeaseExpiresAt) {
		return nil, ErrLeaseLost
	}
	self.lease.LeaseExpiresAt = self.clock.Now().Add(ttl)
	copy := self.lease
	return &copy, nil
}

// A terminal renewal must cancel and then join this explicit cleanup barrier.
func (self *competitionLeaseRetrySupervisorDriver) RunWorker(ctx context.Context, _ model.CompetitionSupervisorLease, _ model.CompetitionRoundResult) error {
	close(self.started)
	select {
	case <-ctx.Done():
		close(self.canceled)
		<-self.release
		self.joined = true
		return ctx.Err()
	case <-self.finished:
		self.joined = true
		return nil
	}
}

// A test watchdog is distinct from the owner's otherwise unchanged lifetime.
type competitionLeaseRetrySupervisorFixture struct {
	clock      *competitionLeaseRetryClock
	supervisor *CompetitionSupervisor
	driver     *competitionLeaseRetrySupervisorDriver
	settings   *competitionSupervisorRenewalSettings
	watchdog   context.Context
	ticks      chan time.Time
	accepted   chan struct{}
	done       chan struct {
		state string
		err   error
	}
	release, finish sync.Once
}

// The real driver snapshot selects an open, unsealed round and its worker hook.
func newCompetitionLeaseRetrySupervisorFixture(t *testing.T) *competitionLeaseRetrySupervisorFixture {
	t.Helper()
	clock := &competitionLeaseRetryClock{now: time.Now()}
	base := newCompetitionSupervisorFixture()
	base.now = clock.Now()
	base.snapshot.Seal = nil
	base.snapshot.Round.Status, base.snapshot.Round.ClosesAt = "open", clock.Now().Add(time.Hour)
	driver := &competitionLeaseRetrySupervisorDriver{competitionSupervisorFixture: base, clock: clock,
		started: make(chan struct{}), canceled: make(chan struct{}), release: make(chan struct{}), finished: make(chan struct{})}
	settings := defaultCompetitionSupervisorRenewalSettings()
	settings.LockRetry.Now = clock.Now
	settings.LockRetry.Wait = func(_ context.Context, delay time.Duration) error { clock.Advance(delay); return nil }
	watchdog, stopWatchdog := context.WithTimeout(t.Context(), 5*time.Second)
	fixture := &competitionLeaseRetrySupervisorFixture{clock: clock, driver: driver, settings: &settings, watchdog: watchdog,
		ticks: make(chan time.Time, 1), accepted: make(chan struct{}, 2), done: make(chan struct {
			state string
			err   error
		}, 1)}
	fixture.supervisor = &CompetitionSupervisor{Enabled: true, OwnerId: "synthetic-lease-retry-supervisor", Driver: driver,
		Now: clock.Now, LeaseDuration: 5 * time.Minute, leaseRetrySettings: &settings, renewTickForTest: fixture.ticks,
		renewedForTest: func() { fixture.accepted <- struct{}{} }}
	t.Cleanup(func() {
		stopWatchdog()
		fixture.release.Do(func() { close(driver.release) })
		fixture.finish.Do(func() { close(driver.finished) })
	})
	return fixture
}

// Every renewal is forced only once the real supervisor has launched its worker.
func (self *competitionLeaseRetrySupervisorFixture) Start(t *testing.T) {
	t.Helper()
	go func() {
		state, err := self.supervisor.Reconcile(t.Context())
		self.done <- struct {
			state string
			err   error
		}{state: state, err: err}
	}()
	select {
	case <-self.driver.started:
	case <-self.watchdog.Done():
		t.Fatal("supervisor did not reach its worker barrier")
	}
}

// Terminal paths cannot finish reconciliation until its worker cleanup joins.
func (self *competitionLeaseRetrySupervisorFixture) RequireFailure(t *testing.T, cause error, calls int) {
	t.Helper()
	select {
	case <-self.driver.canceled:
		if self.driver.joined {
			t.Fatal("supervisor worker bypassed its cleanup barrier")
		}
	case <-self.watchdog.Done():
		t.Fatal("terminal renewal did not cancel the worker")
	}
	self.release.Do(func() { close(self.driver.release) })
	select {
	case result := <-self.done:
		if result.state != "lease_lost" || !errors.Is(result.err, cause) || !self.driver.joined || self.driver.renewCalls != calls {
			t.Fatalf("terminal renewal lost its fence/cleanup result: state=%s calls=%d, %v", result.state, self.driver.renewCalls, result.err)
		}
	case <-self.watchdog.Done():
		t.Fatal("supervisor did not join the released worker")
	}
}

// One lock timeout does not tear down an ongoing worker. The next renewal uses
// the returned expiry, even after the original acquired lease's expiry passes.
func TestCompetitionSupervisorLeaseRetryRecoversWithRollingAuthority(t *testing.T) {
	fixture := newCompetitionLeaseRetrySupervisorFixture(t)
	fixture.settings.Timeout, fixture.settings.AttemptTimeout = 2*time.Minute, 2*time.Minute
	fixture.settings.LockRetry.Timeout, fixture.settings.LockRetry.Interval = 2*time.Minute, time.Minute
	fixture.driver.renew = func(call int) error {
		if call == 1 {
			return &pgconn.PgError{Code: "55P03"}
		}
		return nil
	}
	fixture.Start(t)
	for expected := 0; expected < 2; expected++ {
		fixture.ticks <- fixture.clock.Now()
		select {
		case <-fixture.accepted:
		case <-fixture.driver.canceled:
			t.Fatal("transient renewal canceled the worker instead of retrying")
		case <-fixture.watchdog.Done():
			t.Fatal("transient renewal did not recover without canceling its worker")
		}
		if expected == 0 {
			fixture.clock.Advance(4*time.Minute + 30*time.Second)
		}
	}
	fixture.finish.Do(func() { close(fixture.driver.finished) })
	select {
	case result := <-fixture.done:
		if result.err != nil || result.state != "evaluating" || fixture.driver.renewCalls != 3 || !fixture.driver.joined {
			t.Fatalf("recovered renewal changed reconciliation: state=%s calls=%d, %v", result.state, fixture.driver.renewCalls, result.err)
		}
	case <-fixture.watchdog.Done():
		t.Fatal("recovered supervisor did not finish its worker")
	}
}

// Bounded failure retains the existing lease_lost state and joined worker stop.
func TestCompetitionSupervisorLeaseRetryExhaustionCancelsAndJoins(t *testing.T) {
	fixture := newCompetitionLeaseRetrySupervisorFixture(t)
	fixture.settings.LockRetry.MaximumAttempts = 2
	cause := &pgconn.PgError{Code: "55P03"}
	fixture.driver.renew = func(int) error { return cause }
	fixture.Start(t)
	fixture.ticks <- fixture.clock.Now()
	fixture.RequireFailure(t, cause, 2)
}

// A changed token after one refused transaction is terminal on its next check,
// never permission to renew using another owner or restart the job budget.
func TestCompetitionSupervisorLeaseRetryStopsAfterFenceChange(t *testing.T) {
	fixture := newCompetitionLeaseRetrySupervisorFixture(t)
	fixture.driver.renew = func(call int) error {
		if call == 1 {
			return &pgconn.PgError{Code: "55P03"}
		}
		return nil
	}
	fixture.settings.LockRetry.Wait = func(_ context.Context, delay time.Duration) error {
		fixture.clock.Advance(delay)
		fixture.driver.lease.FencingToken++
		return nil
	}
	fixture.Start(t)
	fixture.ticks <- fixture.clock.Now()
	fixture.RequireFailure(t, ErrLeaseLost, 2)
}

// Already-expired confirmed authority is rejected before any renewal dispatch.
func TestCompetitionSupervisorLeaseRetryRejectsExpiredBeforeRenew(t *testing.T) {
	fixture := newCompetitionLeaseRetrySupervisorFixture(t)
	fixture.Start(t)
	fixture.clock.Advance(fixture.supervisor.LeaseDuration)
	fixture.ticks <- fixture.clock.Now()
	fixture.RequireFailure(t, ErrLeaseLost, 0)
}
