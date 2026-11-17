package connect

import (
	"context"
	"testing"
	"testing/synctest"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
)

// Neither delivery mode silently falls back to the other. Each mode sets an
// exchange's switches through localDeliveryTestMode.apply and the production
// constructor NewExchange uses, then runs a same-process transport and a
// same-process forward through their real loops: in in_process, both attach
// in memory (a link each, exchange_local counts their frames) and neither
// dials; in tcp, each dials its exchange socket once, exchange_io counts the
// frames, and nothing attaches. Catches a mode whose settings miss either
// switch or ceiling, and a branch that dials despite an allowing switch.
func TestLocalDeliveryModesAttachOrDial(t *testing.T) {
	warmLocalTransportPool()
	const messageCount = 50
	buildSwitches := func(exchange *Exchange, ctx context.Context, settings *ExchangeSettings) {
		exchange.localTransport = newExchangeLocalTransport(ctx, settings, exchange.block, ExchangeOpTransport)
		exchange.localForward = newExchangeLocalTransport(ctx, settings, exchange.block, ExchangeOpForward)
	}
	for _, mode := range localDeliveryTestModes {
		// a transport whose resident is in its exchange
		synctest.Test(t, func(t *testing.T) {
			f := newLocalTransportComposedFixture(t, false, mode.apply)
			defer f.close()
			buildSwitches(f.exchange, f.ctx, f.settings)
			mode.requireExchange(t, f.exchange)
			attachmentsBefore := testutil.ToFloat64(exchangeLocalAttachments[exchangeIOOpTransport])
			localBefore := snapshotLocalTransportIO("received", "data")
			socketBefore := snapshotExchangeIOOp("transport", "received", "data")
			f.run()
			synctest.Wait()
			attached := testutil.ToFloat64(exchangeLocalAttachments[exchangeIOOpTransport]) - attachmentsBefore
			script := f.exchangeMessages(t, 0, messageCount, f.recordResident)
			waitCtx, waitCancel := context.WithTimeout(f.ctx, time.Minute)
			defer waitCancel()
			deviceReceived := f.deviceReceived.wait(t, waitCtx, messageCount, "device")
			residentReceived := f.received(f.recordResident).wait(t, waitCtx, messageCount, "resident")
			for index := range messageCount {
				if deviceReceived[index] != script[index] || residentReceived[index] != script[index] {
					t.Fatalf("mode %s: transport message %d out of order", mode.name, index)
				}
			}
			localFrames := snapshotLocalTransportIO("received", "data").frames - localBefore.frames
			socketFrames := snapshotExchangeIOOp("transport", "received", "data").frames - socketBefore.frames
			t.Logf("mode %s transport: dials=%d attached=%v local_frames=%v socket_frames=%v", mode.name, f.dials.Load(), attached, localFrames, socketFrames)
			if mode.local {
				// the transfer layer packs messages into frames, so frames are
				// counted, not matched to messages
				if f.dials.Load() != 0 || attached != 1 || f.exchange.localTransport.linkCount() != 1 || localFrames == 0 || socketFrames != 0 {
					t.Errorf("mode %s: a same-process transport did not attach in memory: dials=%d attached=%v local_frames=%v socket_frames=%v", mode.name, f.dials.Load(), attached, localFrames, socketFrames)
				}
			} else {
				if f.dials.Load() != 1 || attached != 0 || localFrames != 0 || socketFrames == 0 {
					t.Errorf("mode %s: a same-process transport did not dial: dials=%d attached=%v local_frames=%v socket_frames=%v", mode.name, f.dials.Load(), attached, localFrames, socketFrames)
				}
			}
		})

		// a forward whose destination's resident is in its exchange
		synctest.Test(t, func(t *testing.T) {
			f := newLocalForwardComposedFixture(t, false, mode.apply)
			defer f.close()
			buildSwitches(f.exchange, f.ctx, f.settings)
			mode.requireExchange(t, f.exchange)
			attachmentsBefore := testutil.ToFloat64(exchangeLocalAttachments[exchangeIOOpForward])
			localBefore := snapshotLocalForwardIO("received", "data")
			socketBefore := snapshotExchangeIOOp("forward", "received", "data")
			f.run()
			f.send(0, messageCount)
			waitCtx, waitCancel := context.WithTimeout(f.ctx, time.Minute)
			defer waitCancel()
			requireLocalForwardIndices(t, f.receivedBy(f.recordResident).wait(t, waitCtx, messageCount, "resident"), 0, "resident")
			synctest.Wait()
			attached := testutil.ToFloat64(exchangeLocalAttachments[exchangeIOOpForward]) - attachmentsBefore
			localFrames := snapshotLocalForwardIO("received", "data").frames - localBefore.frames
			socketFrames := snapshotExchangeIOOp("forward", "received", "data").frames - socketBefore.frames
			t.Logf("mode %s forward: dials=%d attached=%v local_frames=%v socket_frames=%v", mode.name, f.dials.Load(), attached, localFrames, socketFrames)
			if mode.local {
				if f.dials.Load() != 0 || attached != 1 || f.exchange.localForward.linkCount() != 1 || localFrames != messageCount || socketFrames != 0 {
					t.Errorf("mode %s: a same-process forward did not attach in memory: dials=%d attached=%v local_frames=%v socket_frames=%v", mode.name, f.dials.Load(), attached, localFrames, socketFrames)
				}
			} else {
				if f.dials.Load() != 1 || attached != 0 || localFrames != 0 || socketFrames != messageCount {
					t.Errorf("mode %s: a same-process forward did not dial: dials=%d attached=%v local_frames=%v socket_frames=%v", mode.name, f.dials.Load(), attached, localFrames, socketFrames)
				}
			}
		})
	}
}
