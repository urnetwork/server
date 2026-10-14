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
	"testing"
	"testing/synctest"
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
