// BEGIN may outlive its caller's stop only by a finite bound. The pooler below
// never answers, and the test supplies a zero bound, so the proof never waits
// on wall time.
package server

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
)

// A BEGIN that cannot get a server after its caller stops is interrupted at
// the bound. The caller then sees its own stop and no callback work runs.
func TestTxStopDuringBeginIsBounded(t *testing.T) {
	beginArrived := make(chan struct{})
	var arrivedOnce sync.Once
	release := make(chan struct{})
	_, pool := newPgPoolWireFixture(t, func(_ int, query string) bool {
		if strings.HasPrefix(query, "begin") {
			arrivedOnce.Do(func() { close(beginArrived) })
			<-release
			return false
		}
		return true
	}, neverPingTestPool)
	// Registered after the fixture, so it runs first and frees its worker.
	t.Cleanup(func() { close(release) })
	ctx, stop := context.WithCancel(t.Context())
	defer stop()
	callbacks := 0
	done := make(chan any, 1)
	go func() {
		done <- captureDbErrorPanic(func() {
			txWithPool(ctx, pool, func(PgTx) { callbacks++ }, txBeginTimeout(0), OptNoRetry())
		})
	}()
	<-beginArrived
	stop()
	recovered := <-done
	if err, _ := recovered.(error); !errors.Is(err, context.Canceled) || callbacks != 0 {
		t.Fatal("bounded BEGIN lost its caller's stop or ran callback work", recovered, callbacks)
	}
}
