// Test support for a whole packet fixture, including context.Background escapes.
package server

import (
	"context"
	"sync"
	"testing"
)

// Arms a process-scoped acquisition tripwire in a serial native test. Install
// after durable fixture setup, join every packet owner before stop, and stop
// before durable fixture cleanup. An explicitly authorized source check may use
// the single-acquisition test permit below; all other process calls still fail.
func DenyPostgresForTest(t testing.TB) (attempts func() uint64, stop func()) {
	t.Helper()
	guard := &packetPostgresGuard{}
	if !packetPostgresTestGuard.CompareAndSwap(nil, guard) {
		t.Fatal("overlapping PostgreSQL test tripwires")
	}
	var once sync.Once
	stop = func() {
		once.Do(func() { packetPostgresTestGuard.CompareAndSwap(guard, nil) })
	}
	t.Cleanup(stop)
	return guard.attempts.Load, stop
}

// Permits exactly one acquisition in the current process test oracle. This is
// not a production capability and cannot override WithoutPostgres. Scope the
// returned context only to the exact source helper and revoke it on return.
func PermitPostgresAcquisitionForTest(t testing.TB, ctx context.Context) (context.Context, func()) {
	t.Helper()
	guard := packetPostgresTestGuard.Load()
	if guard == nil {
		t.Fatal("single-acquisition permit requires an installed test oracle")
	}
	permit := &packetPostgresTestPermit{guard: guard}
	stop := func() { permit.state.Store(2) }
	t.Cleanup(stop)
	return context.WithValue(ctx, packetPostgresTestPermitKey{}, permit), stop
}
