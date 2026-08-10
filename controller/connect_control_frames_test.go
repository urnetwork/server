package controller

// Control frames in one pack are independent operations, and the transfer
// layer acks the pack on delivery — a frame the server skips is never resent.
// See ConnectControlFrames.

import (
	"context"
	"strings"
	"testing"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/connect/protocol"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// closeContractFrame builds a CloseContract control frame for a contract id.
func closeContractFrame(t testing.TB, contractId server.Id) *protocol.Frame {
	frame, err := connect.ToFrame(&protocol.CloseContract{
		ContractId:     contractId.Bytes(),
		AckedByteCount: 0,
	}, connect.DefaultProtocolVersion)
	connect.AssertEqual(t, err, nil)
	return frame
}

// TestConnectControlFramesProcessesEveryFrame is the regression test for
// silently dropped control operations.
//
// The loop used to abort the whole batch on the first error. The send side
// coalesces queued control messages into one pack and the transfer layer acks
// on delivery, so every frame after the failing one was discarded with no
// retry ever coming. One benign duplicate close — routine when ControlSync
// re-sends under transport churn — therefore leaked every later contract in
// the pack until the straggler reaper.
//
// Without the fix the second close is never applied and this fails.
func TestConnectControlFramesProcessesEveryFrame(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		networkId := server.NewId()
		userId := server.NewId()
		model.Testing_CreateNetwork(ctx, networkId, "ccf", userId)

		sourceId := server.NewId()
		destinationId := server.NewId()
		model.Testing_CreateDevice(ctx, networkId, server.NewId(), sourceId, "s", "s")
		model.Testing_CreateDevice(ctx, networkId, server.NewId(), destinationId, "d", "d")

		balanceCode, err := model.CreateBalanceCode(
			ctx, 1024*1024*1024, 365*24*60*60*1000000000, 0, "ccf-batch", "", "")
		connect.AssertEqual(t, err, nil)
		_, err = model.RedeemBalanceCode(&model.RedeemBalanceCodeArgs{
			Secret:    balanceCode.Secret,
			NetworkId: networkId,
		}, ctx)
		connect.AssertEqual(t, err, nil)

		// two independent contracts, both open
		firstContractId, _, err := model.CreateContract(
			ctx, networkId, sourceId, networkId, destinationId, 4*1024)
		connect.AssertEqual(t, err, nil)
		secondContractId, _, err := model.CreateContract(
			ctx, networkId, sourceId, networkId, destinationId, 4*1024)
		connect.AssertEqual(t, err, nil)

		// close the first one, so a second close of it is a duplicate that
		// errors — exactly what a ControlSync re-send produces
		connect.AssertEqual(t, model.CloseContract(ctx, firstContractId, sourceId, 0, false), nil)
		connect.AssertEqual(t, model.CloseContract(ctx, firstContractId, destinationId, 0, false), nil)

		// one pack: a poison duplicate followed by a live close
		frames := []*protocol.Frame{
			closeContractFrame(t, firstContractId),
			closeContractFrame(t, secondContractId),
		}
		_, err = ConnectControlFrames(ctx, sourceId, frames, connect.DefaultContractManagerSettings())
		// the duplicate is reported...
		if err == nil {
			t.Fatal("expected the duplicate close to be reported")
		}
		// ...and the frame behind it was still applied
		open := model.GetOpenContractIdsWithNoPartialClose(ctx, sourceId, destinationId)
		if _, stillOpen := open[secondContractId]; stillOpen {
			t.Fatal("the control frame behind a failing one was skipped: it is never resent, so the operation is lost")
		}
	})
}

// TestConnectControlFramesTeardownPanicPropagates covers the other half of the
// batch's error handling: WHICH failures may be swallowed.
//
// The model layer raises db failures as panics. A panic while this resident's
// context is cancelled is a teardown race — the frame must stay UN-acked so
// the sender's resend re-applies it against a healthy resident, so the panic
// must propagate. (A live-context panic is instead reported as that frame's
// error, so one poison frame cannot kill its siblings; that path is covered by
// the batch test above, which relies on siblings surviving a failure.)
//
// Without the ctx check the teardown panic is converted to an error, the pack
// is acked, and the operation is lost for good.
func TestConnectControlFramesTeardownPanicPropagates(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		cancelCtx, cancel := context.WithCancel(ctx)
		cancel()

		// any frame will do: the db call under a cancelled context raises
		frames := []*protocol.Frame{closeContractFrame(t, server.NewId())}

		panicked := false
		func() {
			defer func() {
				if r := recover(); r != nil {
					panicked = true
				}
			}()
			ConnectControlFrames(cancelCtx, server.NewId(), frames, connect.DefaultContractManagerSettings())
		}()
		if !panicked {
			t.Fatal("a teardown-race panic must propagate so the pack stays un-acked and the sender resends; swallowing it loses the operation")
		}
	})
}

// keep the linter honest about the import used only in a message
var _ = strings.TrimSpace
