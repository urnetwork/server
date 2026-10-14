// A raise in a worker cancels only the owner whose lifetime the worker's
// failure decides. HandleError runs every cancel handler on every raise,
// including the cancellation raise of a narrower context, so each worker
// launcher here passes its owner's cancel only when that owner should end with
// the worker. Every test runs on the synctest clock and joins the recovery
// before it reads the owner's context.
package connect

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/urnetwork/server"
)

// A raise in one accepted connection, including the cancellation raise of the
// connection's own context, ends that connection and leaves the exchange
// serving the other residents on the host.
func TestExchangeConnectionWorkerRaiseKeepsExchange(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		for _, raise := range []any{errors.New("synthetic exchange connection fault"), context.Canceled} {
			ctx, cancel := context.WithCancel(t.Context())
			exchange := &Exchange{ctx: ctx, cancel: cancel}
			if !exchange.startConnectionWorker(func() {
				panic(raise)
			}) {
				t.Fatal("open exchange refused a connection worker")
			}
			synctest.Wait()
			if ctx.Err() != nil {
				t.Errorf("connection raise %v canceled the exchange", raise)
			}
			cancel()
		}
	})
}

// A raise in one forward, such as a failed resident lookup or the cancellation
// raise of a forward the idle reaper retired mid-lookup, ends that forward and
// leaves its resident serving the client.
func TestResidentForwardWorkerRaiseKeepsResident(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		for _, raise := range []any{errors.New("synthetic resident lookup failure"), context.Canceled} {
			ctx, cancel := context.WithCancel(t.Context())
			resident := &Resident{ctx: ctx, cancel: cancel}
			if !resident.startForwardWorker(nil, func() {
				panic(raise)
			}) {
				t.Fatal("open resident refused a forward worker")
			}
			synctest.Wait()
			if ctx.Err() != nil {
				t.Errorf("forward raise %v canceled the resident", raise)
			}
			cancel()
		}
	})
}

// A positive allowance's background refresh that raises releases its read and
// leaves the resident serving. The cached allowance keeps its own deadline.
func TestResidentContractRefreshRaiseKeepsResident(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		manager, _, source, destination := residentReadControl(t)
		var calls atomic.Int32
		manager.readContract = residentContractReadForTest(func(context.Context, server.Id, server.Id) bool {
			if calls.Add(1) == 1 {
				return true
			}
			panic(errors.New("synthetic contract refresh fault"))
		})
		if !manager.HasActiveContract(source, destination) {
			t.Fatal("initial positive result was refused")
		}
		time.Sleep(2500 * time.Millisecond)
		if !manager.HasActiveContract(source, destination) {
			t.Fatal("half-window refresh refused the cached allowance")
		}
		synctest.Wait()
		if calls.Load() != 2 {
			t.Fatalf("contract reads=%d, want 2 with one background refresh", calls.Load())
		}
		if manager.ctx.Err() != nil {
			t.Fatal("a raising refresh canceled the resident")
		}
		if len(manager.activeReads) != 0 {
			t.Fatal("a raising refresh kept its read")
		}
	})
}
