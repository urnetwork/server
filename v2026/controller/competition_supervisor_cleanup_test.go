// Fake time and explicit original-worker barriers exercise the public
// reconciler's bounded shutdown drain without live leases or runtime work.
package controller

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"
	"time"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
)

// The original worker and snapshot can be held independently; counters detect
// work started after cancellation even when a driver ignores its context.
type competitionSupervisorCleanupTestDriver struct {
	*competitionLeaseRetrySupervisorDriver
	workerCalls       int
	sealCalls         int
	workerReturnsNil  bool
	beforeSnapshotEnd func(context.Context)
}

// A canceled snapshot must not admit a new worker when its call finally joins.
func (self *competitionSupervisorCleanupTestDriver) Snapshot(ctx context.Context, lease model.CompetitionSupervisorLease) (*CompetitionSupervisorSnapshot, error) {
	snapshot, err := self.competitionLeaseRetrySupervisorDriver.Snapshot(ctx, lease)
	if self.beforeSnapshotEnd != nil {
		self.beforeSnapshotEnd(ctx)
	}
	return snapshot, err
}

// The public hook owns this exact child through its explicit cleanup release.
func (self *competitionSupervisorCleanupTestDriver) RunWorker(ctx context.Context, lease model.CompetitionSupervisorLease, round model.CompetitionRoundResult) error {
	self.workerCalls++
	err := self.competitionLeaseRetrySupervisorDriver.RunWorker(ctx, lease, round)
	if self.workerReturnsNil {
		return nil
	}
	return err
}

// Sealing is new work, not part of canceled child cleanup.
func (self *competitionSupervisorCleanupTestDriver) Seal(context.Context, model.CompetitionSupervisorLease, server.Id) error {
	self.sealCalls++
	return nil
}

// The watchdog is separate from the parent under test and every budget remains
// owned by this fixture. Renewal attempts cannot expire before the drain bound.
func newCompetitionSupervisorCleanupTestFixture(t *testing.T) (*competitionLeaseRetrySupervisorFixture, *competitionSupervisorCleanupTestDriver) {
	t.Helper()
	fixture := newCompetitionLeaseRetrySupervisorFixture(t)
	watchdog, cancel := context.WithTimeout(t.Context(), 10*time.Minute)
	t.Cleanup(cancel)
	fixture.watchdog = watchdog
	fixture.supervisor.CleanupGrace = 90 * time.Second
	fixture.settings.Timeout = 2 * time.Minute
	fixture.settings.AttemptTimeout = 2 * time.Minute
	fixture.settings.LockRetry.Timeout = 2 * time.Minute
	driver := &competitionSupervisorCleanupTestDriver{competitionLeaseRetrySupervisorDriver: fixture.driver}
	fixture.supervisor.Driver = driver
	return fixture, driver
}

// Each call remains the real public reconciliation; only its parent is varied.
func startCompetitionSupervisorCleanupTest(fixture *competitionLeaseRetrySupervisorFixture, parent context.Context) {
	go func() {
		state, err := fixture.supervisor.Reconcile(parent)
		fixture.done <- struct {
			state string
			err   error
		}{state: state, err: err}
	}()
}

// Requested cancellation keeps the exact live fence renewing while its child
// joins, leaves a healthy sibling live, and cannot seal even a nil child result.
func TestCompetitionSupervisorCleanupRenewsOnlyOriginalCanceledWorker(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		fixture, driver := newCompetitionSupervisorCleanupTestFixture(t)
		driver.workerReturnsNil = true
		driver.snapshot.Round.ClosesAt = fixture.clock.Now().Add(-time.Second)
		parent, cancel := context.WithCancel(t.Context())
		defer cancel()
		startCompetitionSupervisorCleanupTest(fixture, parent)
		<-driver.started
		owner, token := driver.lease.OwnerId, driver.lease.FencingToken
		sibling, siblingDriver := newCompetitionSupervisorCleanupTestFixture(t)
		sibling.Start(t)
		cancel()
		<-driver.canceled
		synctest.Wait()
		fixture.ticks <- fixture.clock.Now()
		select {
		case <-fixture.accepted:
		case <-fixture.watchdog.Done():
			t.Fatal("requested shutdown stopped parent renewal before child cleanup")
		}
		sibling.ticks <- sibling.clock.Now()
		<-sibling.accepted
		if driver.joined || siblingDriver.joined || driver.workerCalls != 1 || driver.sealCalls != 0 || driver.lease.OwnerId != owner || driver.lease.FencingToken != token {
			t.Fatal("drain restarted work, abandoned cleanup, or changed its fence")
		}
		fixture.release.Do(func() { close(driver.release) })
		result := <-fixture.done
		if result.state != "evaluation_blocked" || !errors.Is(result.err, context.Canceled) || !driver.joined || driver.sealCalls != 0 || driver.renewCalls != 1 {
			t.Fatal("canceled successful child escaped its original cleanup owner", result.state, result.err)
		}
		fixture.ticks <- fixture.clock.Now()
		synctest.Wait()
		if driver.renewCalls != 1 {
			t.Fatal("renewal outlived the original worker join")
		}
		sibling.finish.Do(func() { close(siblingDriver.finished) })
		result = <-sibling.done
		if result.err != nil || result.state != "evaluating" || !siblingDriver.joined || siblingDriver.workerCalls != 1 {
			t.Fatal("one canceled owner stopped or replaced its healthy sibling", result.state, result.err)
		}
	})
}

// A blocked renewal is canceled exactly at the owned fake-time bound, but
// reconciliation still cannot return until its original child actually joins.
func TestCompetitionSupervisorCleanupDrainHasOneFiniteBound(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		fixture, driver := newCompetitionSupervisorCleanupTestFixture(t)
		entered, canceled := make(chan struct{}), make(chan struct{})
		driver.renewWithContext = func(ctx context.Context, _ int) error {
			close(entered)
			<-ctx.Done()
			close(canceled)
			return ctx.Err()
		}
		parent, cancel := context.WithCancel(t.Context())
		defer cancel()
		startCompetitionSupervisorCleanupTest(fixture, parent)
		<-driver.started
		cancel()
		<-driver.canceled
		synctest.Wait()
		fixture.ticks <- fixture.clock.Now()
		<-entered
		synctest.Sleep(89 * time.Second)
		synctest.Wait()
		select {
		case <-canceled:
			t.Fatal("drain truncated the healthy original join allowance")
		default:
		}
		synctest.Sleep(time.Second)
		<-canceled
		synctest.Wait()
		select {
		case <-fixture.done:
			t.Fatal("grace expiry falsely joined the original child")
		default:
		}
		fixture.ticks <- fixture.clock.Now()
		synctest.Wait()
		if driver.renewCalls != 1 || driver.joined {
			t.Fatal("grace expiry restarted renewal or abandoned its child")
		}
		fixture.release.Do(func() { close(driver.release) })
		result := <-fixture.done
		if result.state != "evaluation_blocked" || !errors.Is(result.err, context.Canceled) || !driver.joined {
			t.Fatal("bounded requested shutdown lost its cancellation or join", result.state, result.err)
		}
	})
}

// A refusal that also unwraps cancellation is not an expected local stop. Its
// authenticated database diagnostic must survive already-canceled worker state.
func TestCompetitionSupervisorCleanupRetainsFenceLossDiagnostic(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		fixture, driver := newCompetitionSupervisorCleanupTestFixture(t)
		proof := competitionSupervisorReadLockFailure()
		proof.cause = competitionDatabasePhaseError{cause: errors.Join(context.Canceled, ErrLeaseLost), phase: server.DbOperationCallback}
		proof.database = &CompetitionSupervisorRenewalDatabaseFailure{Schema: 1, Kind: "competition-supervisor-renewal-database/v1",
			DatabasePhase: server.DbOperationCallback, Step: proof.step, CallbackEntries: 1, Transaction: proof.transaction}
		driver.renew = func(int) error { return proof }
		parent, cancel := context.WithCancel(t.Context())
		defer cancel()
		startCompetitionSupervisorCleanupTest(fixture, parent)
		<-driver.started
		cancel()
		<-driver.canceled
		fixture.ticks <- fixture.clock.Now()
		synctest.Wait()
		fixture.ticks <- fixture.clock.Now()
		synctest.Wait()
		if driver.renewCalls != 1 || driver.joined {
			t.Fatal("fence loss renewed again or abandoned the original child")
		}
		fixture.release.Do(func() { close(driver.release) })
		result := <-fixture.done
		if result.state != "lease_lost" || !errors.Is(result.err, ErrLeaseLost) || CompetitionSupervisorRenewalFailureDetails(result.err) == nil || !driver.joined {
			t.Fatal("local cancellation swallowed the confirmed fence refusal", result.state, result.err)
		}
	})
}

// Even inside requested drain, the last confirmed expiry refuses dispatch;
// no caller cancellation or remaining grace can revive expired authority.
func TestCompetitionSupervisorCleanupNeverRenewsExpiredAuthority(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		fixture, driver := newCompetitionSupervisorCleanupTestFixture(t)
		parent, cancel := context.WithCancel(t.Context())
		defer cancel()
		startCompetitionSupervisorCleanupTest(fixture, parent)
		<-driver.started
		cancel()
		<-driver.canceled
		fixture.clock.Advance(fixture.supervisor.LeaseDuration)
		fixture.ticks <- fixture.clock.Now()
		synctest.Wait()
		if driver.renewCalls != 0 || driver.joined {
			t.Fatal("expired parent was revived or its original child abandoned")
		}
		fixture.release.Do(func() { close(driver.release) })
		result := <-fixture.done
		if result.state != "lease_lost" || !errors.Is(result.err, ErrLeaseLost) || !driver.joined {
			t.Fatal("expired authority was hidden by earlier requested cancellation", result.state, result.err)
		}
	})
}

// A non-worker stage gets no shutdown grace, including a blocked renewal.
// A snapshot that ignores cancellation cannot later admit a worker or seal.
func TestCompetitionSupervisorCleanupNeverDrainsCanceledSnapshot(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		fixture, driver := newCompetitionSupervisorCleanupTestFixture(t)
		snapshotEntered, releaseSnapshot := make(chan struct{}), make(chan struct{})
		driver.beforeSnapshotEnd = func(ctx context.Context) {
			close(snapshotEntered)
			<-ctx.Done()
			<-releaseSnapshot
		}
		renewEntered, renewCanceled := make(chan struct{}), make(chan struct{})
		driver.renewWithContext = func(ctx context.Context, _ int) error {
			close(renewEntered)
			<-ctx.Done()
			close(renewCanceled)
			return ctx.Err()
		}
		parent, cancel := context.WithCancel(t.Context())
		defer cancel()
		startCompetitionSupervisorCleanupTest(fixture, parent)
		<-snapshotEntered
		fixture.ticks <- fixture.clock.Now()
		<-renewEntered
		canceledAt := time.Now()
		cancel()
		<-renewCanceled
		if !time.Now().Equal(canceledAt) {
			t.Fatal("non-worker cancellation spent a shutdown drain allowance")
		}
		close(releaseSnapshot)
		result := <-fixture.done
		if result.state != "state_unavailable" || !errors.Is(result.err, context.Canceled) || driver.workerCalls != 0 || driver.sealCalls != 0 || driver.renewCalls != 1 {
			t.Fatal("canceled snapshot launched new work or lost its local cancellation", result.state, result.err)
		}
	})
}

// A parent deadline is not a requested shutdown. The active worker still must
// join, but its renewal cannot gain an extra grace after that deadline.
func TestCompetitionSupervisorCleanupNeverDrainsParentDeadline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		fixture, driver := newCompetitionSupervisorCleanupTestFixture(t)
		entered, canceled := make(chan struct{}), make(chan struct{})
		driver.renewWithContext = func(ctx context.Context, _ int) error {
			close(entered)
			<-ctx.Done()
			close(canceled)
			return ctx.Err()
		}
		deadline := time.Now().Add(time.Second)
		parent, cancel := context.WithDeadline(t.Context(), deadline)
		defer cancel()
		startCompetitionSupervisorCleanupTest(fixture, parent)
		<-driver.started
		fixture.ticks <- fixture.clock.Now()
		<-entered
		<-driver.canceled
		<-canceled
		if !time.Now().Equal(deadline) || driver.joined {
			t.Fatal("parent deadline gained a renewal drain or abandoned its child")
		}
		fixture.release.Do(func() { close(driver.release) })
		result := <-fixture.done
		if result.state != "evaluation_blocked" || !errors.Is(result.err, context.DeadlineExceeded) || !driver.joined || driver.renewCalls != 1 {
			t.Fatal("deadline shutdown lost its original cause or cleanup join", result.state, result.err)
		}
	})
}
