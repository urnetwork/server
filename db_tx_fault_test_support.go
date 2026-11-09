// Test support for §22.8's pg-commit fault on T3 (sidecart3): a process-scoped hook every Tx and
// MaintenanceTx commit consults, nil in production, so a fault schedule can lose a commit's answer after
// the commit took (ApplyThenDrop), fail it before it reaches the database (FailBeforeApply), or end the
// committing caller on either side of it. It sits beside DenyPostgresForTest, in a file a test of any
// package can reach, and is installed the same way: serially, for one test, and off at its end.
package server

import (
	"context"
	"sync/atomic"
	"testing"
)

// What a test decides for one commit. ctx is the transaction's caller's, so a hook can tell whose commit
// it is. commit sends the real COMMIT and returns its answer; rollback ends the transaction without one.
// The hook calls at most one of them, and what it returns is what the commit returns: an error that is
// not a *pgconn.PgError reads as an answer that never came, so Tx raises it and reruns nothing, as it
// does for a connection lost at commit.
type PgCommitFaultFunction func(ctx context.Context, commit func() error, rollback func()) error

// nil outside a test that armed it
var pgCommitFaultHook atomic.Pointer[PgCommitFaultFunction]

// Routes every commit in this process through hook until stop or the end of the test. A second hook
// while one is armed fails the test, as the commits of two tests would land in one.
func SetPgCommitFaultForTest(t testing.TB, hook PgCommitFaultFunction) (stop func()) {
	t.Helper()
	if hook == nil {
		t.Fatal("no pg-commit fault hook to arm")
	}
	if !pgCommitFaultHook.CompareAndSwap(nil, &hook) {
		t.Fatal("overlapping pg-commit fault hooks")
	}
	stopped := atomic.Bool{}
	stop = func() {
		if stopped.CompareAndSwap(false, true) {
			pgCommitFaultHook.Store(nil)
		}
	}
	t.Cleanup(stop)
	return stop
}

// The commit, through the armed hook if a test armed one: commitObservedTx otherwise, which is all
// production ever runs.
func commitObservedTxWithFault(ctx context.Context, commitCtx context.Context, tx *postCommitPgTx) error {
	hook := pgCommitFaultHook.Load()
	if hook == nil {
		return commitObservedTx(commitCtx, tx)
	}
	return (*hook)(
		ctx,
		func() error {
			return commitObservedTx(commitCtx, tx)
		},
		func() {
			rollbackTx(commitCtx, tx)
		},
	)
}
