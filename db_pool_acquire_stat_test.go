package server

import (
	"context"
	"testing"
)

// The reading never opens a pool: an unused pool answers false.
func TestPgPoolAcquireStatAbsentUntilPoolIsUsed(t *testing.T) {
	pool := &safePgPool{}
	if _, ok := pool.acquireStat(); ok {
		t.Fatal("an unopened pool reported acquire counters")
	}
	if pool.pool != nil {
		t.Fatal("reading the acquire counters opened the pool")
	}
}

// Each successful acquire counts once, the wait never goes backwards, and the
// reading carries the pool's size.
func TestPgPoolAcquireStatCountsAcquires(t *testing.T) {
	_, pool := newPgPoolWireFixture(t, nil, nil)
	before, ok := pool.acquireStat()
	if !ok {
		t.Fatal("an open pool reported no acquire counters")
	}
	for range 3 {
		dbWithPool(context.Background(), pool, func(conn PgConn) {
			RaisePgResult(conn.Exec(context.Background(), "SELECT 1"))
		}, OptNoRetry())
	}
	after, ok := pool.acquireStat()
	if !ok {
		t.Fatal("an open pool reported no acquire counters")
	}
	if after.AcquireCount-before.AcquireCount != 3 {
		t.Fatalf("acquires counted %d, want 3", after.AcquireCount-before.AcquireCount)
	}
	if after.AcquireDuration < before.AcquireDuration {
		t.Fatalf("acquire wait went backwards: %s then %s", before.AcquireDuration, after.AcquireDuration)
	}
	if after.MaxConns != 1 || after.AcquiredConns != 0 {
		t.Fatalf("pool size acquired=%d max=%d, want 0 of 1", after.AcquiredConns, after.MaxConns)
	}
}
