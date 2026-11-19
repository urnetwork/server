// Public consumer regressions retain real controller/store/Tx/pgx boundaries
// without PostgreSQL, global production configuration, or wall-clock sleeps.
package server_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/controller"
	"github.com/urnetwork/server/v2026/model"
)

// Only acquire/snapshot/worker fixture state is synthetic. Renew executes the
// real PostgresStore, including its complete private retry proof.
type supervisorUpdateWireDriver struct {
	controller.CompetitionSupervisorDriver
	settings                                 *controller.Settings
	lease                                    model.CompetitionSupervisorLease
	started, canceled, finish, releaseWorker chan struct{}
	recovered                                chan struct{}
	calls                                    int
	first                                    *controller.CompetitionSupervisorRenewalDatabaseFailure
	joined                                   bool
	changeFence                              bool
	finishOnce, releaseOnce                  sync.Once
}

// Repeated cleanup cannot race a normal finish with a failing assertion.
func (self *supervisorUpdateWireDriver) finishWorker() {
	self.finishOnce.Do(func() { close(self.finish) })
}

// The worker cleanup barrier has exactly one releasing owner.
func (self *supervisorUpdateWireDriver) releaseJoinedWorker() {
	self.releaseOnce.Do(func() { close(self.releaseWorker) })
}

// The already-confirmed initial lease never changes during fixture acquisition.
func (self *supervisorUpdateWireDriver) Acquire(context.Context, string, time.Duration) (*model.CompetitionSupervisorLease, error) {
	copy := self.lease
	return &copy, nil
}

// A real first failure is passed unchanged to the controller's private policy.
func (self *supervisorUpdateWireDriver) Renew(ctx context.Context, lease model.CompetitionSupervisorLease, ttl time.Duration) (*model.CompetitionSupervisorLease, error) {
	self.calls++
	next, err := (controller.PostgresStore{}).RenewAutomationLease(ctx, self.settings, lease, ttl)
	if self.calls == 1 {
		self.first = controller.CompetitionSupervisorRenewalFailureDetails(err)
	}
	if err == nil {
		if self.changeFence {
			next.FencingToken++
		}
		self.recovered <- struct{}{}
	}
	return next, err
}

// Selects the actual open-round worker branch, not a fake renewal harness.
func (self *supervisorUpdateWireDriver) Snapshot(context.Context, model.CompetitionSupervisorLease) (*controller.CompetitionSupervisorSnapshot, error) {
	return &controller.CompetitionSupervisorSnapshot{Round: &model.CompetitionRoundResult{
		DecisionMode: model.CompetitionDecisionModeV2, Status: "open", ClosesAt: time.Now().Add(time.Hour),
	}}, nil
}

// Cancellation must join this explicit barrier before reconciliation returns.
func (self *supervisorUpdateWireDriver) RunWorker(ctx context.Context, _ model.CompetitionSupervisorLease, _ model.CompetitionRoundResult) error {
	close(self.started)
	select {
	case <-ctx.Done():
		close(self.canceled)
		<-self.releaseWorker
		self.joined = true
		return ctx.Err()
	case <-self.finish:
		self.joined = true
		return nil
	}
}

// Uses the real default TTL, renewal tick, retry budgets, and statement grace.
func newSupervisorUpdateWireDriver(expiry time.Time) *supervisorUpdateWireDriver {
	settings := &controller.Settings{CompetitionId: "synthetic-competition", AutomationPolicy: model.CompetitionAutomationPolicy{
		Enabled: true, Environment: "synthetic", SeasonId: "synthetic-season",
	}}
	return &supervisorUpdateWireDriver{settings: settings, lease: model.CompetitionSupervisorLease{
		Environment: settings.AutomationPolicy.Environment, CompetitionId: settings.CompetitionId, SeasonId: settings.AutomationPolicy.SeasonId,
		OwnerId: "synthetic-update-owner", FencingToken: 7, LeaseExpiresAt: expiry,
	}, started: make(chan struct{}), canceled: make(chan struct{}), finish: make(chan struct{}), releaseWorker: make(chan struct{}), recovered: make(chan struct{}, 1)}
}

// Only source-owned real transaction evidence can keep this worker running.
func TestCompetitionSupervisorUpdateComposedWireRecoversAfterActualRollback(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		started := time.Now()
		driver := newSupervisorUpdateWireDriver(started.Add(time.Minute))
		server.Testing_WithSupervisorUpdateWire(t, driver.lease.LeaseExpiresAt, nil, func(wire server.TestingSupervisorUpdateWire) {
			supervisor := &controller.CompetitionSupervisor{Enabled: true, OwnerId: driver.lease.OwnerId, Driver: driver}
			type result struct {
				state string
				err   error
			}
			done := make(chan result, 1)
			go func() { state, err := supervisor.Reconcile(t.Context()); done <- result{state: state, err: err} }()
			readResult := false
			defer func() {
				wire.Release(false)
				driver.finishWorker()
				driver.releaseJoinedWorker()
				if !readResult {
					<-done
				}
			}()
			<-driver.started
			<-wire.Arrived
			if time.Since(started) != 20*time.Second {
				t.Fatal("default renewal scheduling changed", time.Since(started))
			}
			time.Sleep(8 * time.Second)
			synctest.Wait()
			select {
			case <-driver.canceled:
				t.Fatal("owned attempt deadline canceled a still-authorized worker")
			default:
			}
			wire.Release(true)
			select {
			case <-driver.recovered:
			case <-driver.canceled:
				t.Fatal("complete actual UPDATE rollback was terminal instead of recovering")
			}
			synctest.Wait()
			first := driver.first
			if driver.calls != 2 || first == nil || first.Step != 4 || first.AtReturn.State != "deadline" ||
				first.Transaction.CommitAttempts != 0 || first.Transaction.RollbackAcknowledged != 1 ||
				!first.Transaction.LastRelease.PoolReturned || first.Transaction.LastRelease.CleanupComplete ||
				time.Since(started) != 28*time.Second+250*time.Millisecond {
				t.Fatal("actual UPDATE/rollback/physical-return did not recover on the original budgets", driver.calls, first, time.Since(started))
			}
			updates, rollbacks, commits := 0, 0, 0
			for _, sql := range wire.Queries() {
				switch {
				case strings.HasPrefix(sql, "UPDATE competition_supervisor_lease SET"):
					updates++
				case sql == "rollback":
					rollbacks++
				case sql == "commit":
					commits++
				}
			}
			if updates != 2 || rollbacks != 1 || commits != 1 {
				t.Fatal("wire history failed actual first-write rollback and second-write commit", updates, rollbacks, commits)
			}
			driver.finishWorker()
			finished := <-done
			readResult = true
			if finished.err != nil || finished.state != "evaluating" || !driver.joined {
				t.Fatal("recovered reconciliation lost its joined original worker", finished, driver.joined)
			}
		})
	})
}

// A lifetime-expired connection is idle at entry but is not returned to the
// physical pool; the old idle-only rule cannot authenticate this new writer.
func TestCompetitionSupervisorUpdateComposedWireRefusesLifetimeDestruction(t *testing.T) {
	supervisorUpdateComposedWireRefusal(t, func(config *pgxpool.Config) {
		config.MaxConnLifetime, config.MaxConnLifetimeJitter = time.Second, 0
	}, true, false, false)
}

// The first incident's no-ACK/destruction shape stays terminal, never a broad
// deadline retry or an attempt to start a competing cleanup owner.
func TestCompetitionSupervisorUpdateComposedWireRefusesUnacknowledgedRollback(t *testing.T) {
	supervisorUpdateComposedWireRefusal(t, nil, false, false, false)
}

// A newly returned owner/fence cannot borrow the original supervisor authority.
func TestCompetitionSupervisorUpdateComposedWireRefusesChangedFence(t *testing.T) {
	supervisorUpdateComposedWireRefusal(t, nil, true, true, false)
}

// Confirmed authority expires while a real statement still has its unchanged
// stop grace. Late rollback and a physical return cannot revive that window.
func TestCompetitionSupervisorUpdateComposedWireRefusesExpiredAuthority(t *testing.T) {
	supervisorUpdateComposedWireRefusal(t, nil, true, false, true)
}

// Shared fault construction leaves every positive assertion in a top-level
// case and joins the actual worker before retiring the private wire fixture.
func supervisorUpdateComposedWireRefusal(t *testing.T, configure func(*pgxpool.Config), reply, changeFence, nearExpiry bool) {
	t.Helper()
	synctest.Test(t, func(t *testing.T) {
		remaining := time.Minute
		if nearExpiry {
			remaining = 25 * time.Second
		}
		driver := newSupervisorUpdateWireDriver(time.Now().Add(remaining))
		driver.changeFence = changeFence
		server.Testing_WithSupervisorUpdateWire(t, driver.lease.LeaseExpiresAt, configure, func(wire server.TestingSupervisorUpdateWire) {
			supervisor := &controller.CompetitionSupervisor{Enabled: true, OwnerId: driver.lease.OwnerId, Driver: driver}
			type result struct {
				state string
				err   error
			}
			done := make(chan result, 1)
			go func() { state, err := supervisor.Reconcile(t.Context()); done <- result{state: state, err: err} }()
			readResult := false
			defer func() {
				wire.Release(false)
				driver.finishWorker()
				driver.releaseJoinedWorker()
				if !readResult {
					<-done
				}
			}()
			<-driver.started
			<-wire.Arrived
			delay := 8 * time.Second
			if nearExpiry {
				delay = 5 * time.Second
			}
			time.Sleep(delay)
			synctest.Wait()
			wire.Release(reply)
			if changeFence {
				<-driver.recovered
				<-driver.canceled
			} else {
				select {
				case <-driver.canceled:
				case <-driver.recovered:
					t.Fatal("incomplete or expired writer proof launched a healthy second renewal")
				}
			}
			synctest.Wait()
			select {
			case <-done:
				readResult = true
				t.Fatal("terminal renewal escaped its worker cleanup barrier")
			default:
			}
			driver.releaseJoinedWorker()
			finished := <-done
			readResult = true
			wantCalls := 1
			if changeFence {
				wantCalls = 2
			}
			if finished.state != "lease_lost" || finished.err == nil || driver.calls != wantCalls || !driver.joined || driver.first == nil {
				t.Fatal("unsafe update retry escaped original authority/worker custody", finished, driver.calls, driver.first)
			}
			if !reply && driver.first.Transaction.RollbackAcknowledged != 0 {
				t.Fatal("failed physical reply fabricated a rollback acknowledgement")
			}
			if configure != nil && (driver.first.Transaction.RollbackAcknowledged != 1 || driver.first.Transaction.LastRelease.PoolReturned) {
				t.Fatal("lifetime destruction borrowed physical idle return")
			}
			if (nearExpiry || changeFence) && !errors.Is(finished.err, controller.ErrLeaseLost) {
				t.Fatal("expired or changed authority did not retain lease-loss cause", finished.err)
			}
		})
	})
}
