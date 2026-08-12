package controller

// Control frames in one pack are independent operations, and the transfer
// layer acks the pack on delivery — a frame the server skips is never resent.
// See ConnectControlFrames.

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/connect/protocol"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

func controlFrameFailureCount(message string, cause string) float64 {
	return testutil.ToFloat64(controlFrameFailureCounter.WithLabelValues(message, cause))
}

// TestControlFrameFailureLabelsAreBounded pins both label sets of
// urnetwork_connect_control_frame_failures_total. These labels are attached
// to client-supplied input, so they must come from fixed switches — deriving
// either from message contents would be a metric-cardinality attack, and an
// unclassified value must fall into `other` rather than pass through.
func TestControlFrameFailureLabelsAreBounded(t *testing.T) {
	messages := []struct {
		message any
		want    string
	}{
		{&protocol.CreateContract{}, "create_contract"},
		{&protocol.CloseContract{}, "close_contract"},
		{&protocol.Provide{}, "provide"},
		{&protocol.EncryptedKey{}, "encrypted_key"},
		{&protocol.ClientKey{}, "client_key"},
		{&protocol.ControlPing{}, "control_ping"},
		{&protocol.ProvidePing{}, "provide_ping"},
		// an undecodable frame reports no message
		{nil, "other"},
		// any message outside the dispatch collapses to one bucket
		{&protocol.Pack{}, "other"},
	}
	for _, test := range messages {
		if got := controlFrameMessageLabel(test.message); got != test.want {
			t.Fatalf("controlFrameMessageLabel(%T) = %q, want %q", test.message, got, test.want)
		}
	}

	causes := []struct {
		err  error
		want string
	}{
		{fmt.Errorf("Contract already closed with outcome settled: a b c->d"), "contract_already_closed"},
		{fmt.Errorf("Contract not found: a"), "contract_not_found"},
		{fmt.Errorf("Client is not a party to the contract: a b c->d"), "not_a_party"},
		{fmt.Errorf("Contract in dispute: a b c->d"), "contract_in_dispute"},
		{fmt.Errorf("Cannot handle oob control message: *protocol.Pack"), "unhandled_message"},
		{fmt.Errorf("control frame *protocol.CloseContract panicked: boom"), "panic"},
		{fmt.Errorf("postgres unavailable"), "other"},
	}
	for _, test := range causes {
		if got := controlFrameErrorClass(test.err); got != test.want {
			t.Fatalf("controlFrameErrorClass(%q) = %q, want %q", test.err, got, test.want)
		}
	}
}

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
		defer returnConnectControlFrames(frames)
		// the duplicate close is the exact failure that flooded the resident
		// log; it must land in the counter, which is now the only
		// default-level signal for it
		beforeDuplicate := controlFrameFailureCount("close_contract", "contract_already_closed")
		outFrames, err := ConnectControlFrames(ctx, sourceId, frames, connect.DefaultContractManagerSettings())
		defer returnConnectControlFrames(outFrames)
		// the duplicate is reported...
		if err == nil {
			t.Fatal("expected the duplicate close to be reported")
		}
		if after := controlFrameFailureCount("close_contract", "contract_already_closed"); after != beforeDuplicate+1 {
			t.Fatalf("counter{close_contract,contract_already_closed} = %v, want %v", after, beforeDuplicate+1)
		}
		// ...and the frame behind it was still applied
		open := model.GetOpenContractIdsWithNoPartialClose(ctx, sourceId, destinationId)
		if _, stillOpen := open[secondContractId]; stillOpen {
			t.Fatal("the control frame behind a failing one was skipped: it is never resent, so the operation is lost")
		}
	})
}

// TestConnectControlFramesCanceledContextPanicPropagates covers the other half
// of the batch's error handling: which failures may be converted to errors.
//
// The model layer raises db failures as panics. A panic under a canceled caller
// context belongs to that caller's lifecycle boundary and must propagate. A
// live-context panic is instead reported as that frame's error, so one poison
// frame cannot kill its siblings; that path is covered by the batch test above,
// which relies on siblings surviving a failure. Resident controllers keep
// their context live until admitted Client callbacks join, and Client isolates
// registered application callback panics; this unit contract is not a transfer
// ACK or resend signal.
//
// Without the ctx check cancellation is misclassified as an ordinary live
// control-frame failure.
func TestConnectControlFramesCanceledContextPanicPropagates(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		cancelCtx, cancel := context.WithCancel(ctx)
		cancel()

		// any frame will do: the db call under a cancelled context raises
		frames := []*protocol.Frame{closeContractFrame(t, server.NewId())}
		defer returnConnectControlFrames(frames)

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
			t.Fatal("a canceled-context panic was converted to an ordinary control-frame error")
		}
	})
}

// TestConnectControlFramesHandlesKeepAlivePings covers the keep-alive
// messages older clients send to the control destination. ControlPing and
// ProvidePing expect nothing back except the transfer-level ack, so the
// dispatch must treat them as handled no-ops. Before the explicit cases they
// fell through to the "Cannot handle oob control message" error, which
// flooded the resident log at thousands of lines per second once the
// credential-migration gates re-admitted the legacy fleet (2026-08-10).
func TestConnectControlFramesHandlesKeepAlivePings(t *testing.T) {
	ctx := context.Background()

	controlPing, err := connect.ToFrame(&protocol.ControlPing{}, connect.DefaultProtocolVersion)
	connect.AssertEqual(t, err, nil)
	providePing, err := connect.ToFrame(&protocol.ProvidePing{}, connect.DefaultProtocolVersion)
	connect.AssertEqual(t, err, nil)
	defer returnConnectControlFrames([]*protocol.Frame{controlPing, providePing})

	outFrames, err := ConnectControlFrames(
		ctx,
		server.NewId(),
		[]*protocol.Frame{controlPing, providePing},
		connect.DefaultContractManagerSettings(),
	)
	defer returnConnectControlFrames(outFrames)
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, len(outFrames), 0)
}

// keep the linter honest about the import used only in a message
var _ = strings.TrimSpace
