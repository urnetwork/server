package connect

import (
	"context"
	"net/netip"
	"testing"
	"time"

	"github.com/urnetwork/connect/v2026"
	"github.com/urnetwork/connect/v2026/protocol"
	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
)

// A connect resident's in-band control path carries move result reports
// (connect/MOVE.md §5.1): accepted only for the client's own move intent,
// which it then removes, and never an error for a report of another move.
func TestResidentControllerAcceptsMoveResultForTheIntent(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		clientId := server.NewId()
		intent := &model.ResidentMoveIntent{
			MoveId:         server.NewId(),
			Kind:           model.ResidentMoveKindAdvisory,
			FromResidentId: server.NewId(),
			FromExchangeId: server.NewId(),
			TargetHost:     "alpha-edge-1",
			Targets:        []netip.Addr{netip.MustParseAddr("198.51.100.21")},
		}
		connect.AssertEqual(t, nil, model.SetResidentMoveIntent(ctx, clientId, intent, time.Minute))

		controller := newResidentController(ctx, clientId, nil, DefaultExchangeSettings())
		defer controller.Close()
		report := func(moveId server.Id) error {
			frame, err := connect.ToFrame(&protocol.ResidentMoveResult{
				MoveId:   moveId.Bytes(),
				Outcome:  protocol.ResidentMoveOutcome_ResidentMoveOutcomeFailed,
				Reason:   protocol.ResidentMoveReason_ResidentMoveReasonConnectTimeout,
				IpFamily: 4,
			}, connect.DefaultProtocolVersion)
			connect.AssertEqual(t, nil, err)
			defer connect.MessagePoolReturn(frame.MessageBytes)
			return controller.HandleControlFrames([]*protocol.Frame{frame})
		}

		connect.AssertEqual(t, nil, report(server.NewId()))
		kept, err := model.GetResidentMoveIntent(ctx, clientId)
		connect.AssertEqual(t, nil, err)
		connect.AssertEqual(t, intent.MoveId, kept.MoveId)

		connect.AssertEqual(t, nil, report(intent.MoveId))
		remaining, err := model.GetResidentMoveIntent(ctx, clientId)
		connect.AssertEqual(t, nil, err)
		connect.AssertEqual(t, true, remaining == nil)
	})
}
