// Test support for a whole packet fixture, including context.Background escapes.
package server

import (
	"sync"
	"testing"
)

// Arms a process-scoped acquisition tripwire in a serial native test. Install
// after durable fixture setup, join every packet owner before stop, and stop
// before durable fixture cleanup. Independent setup with permitted PostgreSQL
// belongs in a different isolation test, not inside this guarded scope.
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
