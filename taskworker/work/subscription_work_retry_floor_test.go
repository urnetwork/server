// A failed retention run must not restart at once.
package work

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/urnetwork/server/task"
)

// A run canceled at its MaxTime, or failed by a statement, has no Post to
// re-arm the chain's 30-minute gap. The registered target waits that gap
// before it retries, where ordinary backoff restarts it within seconds. A drain
// still hands the run to the next worker at once.
func TestRemoveCompletedContractsFailureWaitsChainGap(t *testing.T) {
	target := NewRemoveCompletedContractsTaskTarget()
	if got := target.TargetFunctionName(); got != task.NewTaskTarget(RemoveCompletedContracts).TargetFunctionName() {
		t.Fatalf("the registered target renamed its function: %s", got)
	}
	for _, err := range []error{
		errors.Join(errors.New("Timeout"), context.Canceled),
		&pgconn.PgError{Code: "40P01"},
	} {
		for _, errorCount := range []int{0, 1, 4} {
			if delay := task.Testing_TargetErrorRetryDelay(target, err, errorCount); delay < removeCompletedContractsErrorRetryFloor {
				t.Errorf("%v at count %d retries after %s, inside the chain's %s gap", err, errorCount, delay, removeCompletedContractsErrorRetryFloor)
			}
		}
	}
	if delay := task.Testing_TargetErrorRetryDelay(target, task.ErrDrained, 0); time.Minute <= delay {
		t.Errorf("a drained run waits %s before the next worker takes it", delay)
	}
}
