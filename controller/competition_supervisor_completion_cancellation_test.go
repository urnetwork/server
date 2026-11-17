// Finishing a worker cancels its renewal owner for cleanup, not because the
// lease failed. That cancellation must never overwrite the actual worker error.
package controller

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// The worker returns its failure only at the acknowledged test boundary.
type competitionSupervisorWorkerExitDriver struct {
	*competitionLeaseRetrySupervisorDriver
	failure error
}

// The ordinary reconciliation path still joins this exact single worker.
func (self *competitionSupervisorWorkerExitDriver) RunWorker(ctx context.Context, _ model.CompetitionSupervisorLease, _ model.CompetitionRoundResult) error {
	close(self.started)
	select {
	case <-self.finished:
		self.joined = true
		return self.failure
	case <-ctx.Done():
		self.joined = true
		return ctx.Err()
	}
}

// Without the cleanup-cancellation distinction, the real Reconcile defer
// changes evaluation_blocked/worker failure into lease_lost/context.Canceled.
func TestCompetitionSupervisorWorkerExitKeepsFailureDuringRenewalCleanup(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		fixture := newCompetitionLeaseRetrySupervisorFixture(t)
		cause := errors.New("synthetic worker failure with diagnostic report")
		fixture.supervisor.Driver = &competitionSupervisorWorkerExitDriver{competitionLeaseRetrySupervisorDriver: fixture.driver, failure: cause}
		entered, canceled := make(chan struct{}), make(chan struct{})
		fixture.driver.renewWithContext = func(ctx context.Context, _ int) error {
			close(entered)
			<-ctx.Done()
			close(canceled)
			return ctx.Err()
		}
		fixture.Start(t)
		fixture.ticks <- time.Now()
		<-entered
		fixture.finish.Do(func() { close(fixture.driver.finished) })
		result := <-fixture.done
		<-canceled
		if result.state != "evaluation_blocked" || result.err != cause || !fixture.driver.joined || fixture.driver.renewCalls != 1 {
			t.Fatal("renewal cleanup discarded the actual worker failure", result.state, result.err)
		}
	})
}

// A real fenced refusal can wrap ErrLeaseLost while retaining transaction
// diagnostics. The watchdog's plain sentinel must not erase that root proof.
func TestCompetitionSupervisorWorkerExitPreservesFencedDatabaseDiagnostic(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		fixture := newCompetitionLeaseRetrySupervisorFixture(t)
		proof := competitionSupervisorReadLockFailure()
		proof.cause = competitionDatabasePhaseError{cause: ErrLeaseLost, phase: server.DbOperationCallback}
		proof.database = &CompetitionSupervisorRenewalDatabaseFailure{Schema: 1, Kind: "competition-supervisor-renewal-database/v1",
			DatabasePhase: server.DbOperationCallback, Step: proof.step, CallbackEntries: 1, Transaction: proof.transaction}
		fixture.driver.renew = func(int) error { return proof }
		fixture.Start(t)
		fixture.ticks <- fixture.clock.Now()
		select {
		case <-fixture.driver.canceled:
		case <-fixture.watchdog.Done():
			t.Fatal("fenced refusal did not cancel the original worker")
		}
		fixture.release.Do(func() { close(fixture.driver.release) })
		result := <-fixture.done
		if result.state != "lease_lost" || !errors.Is(result.err, ErrLeaseLost) || CompetitionSupervisorRenewalFailureDetails(result.err) == nil || !fixture.driver.joined {
			t.Fatal("plain watchdog sentinel erased an authenticated database refusal", result.state, result.err)
		}
	})
}
