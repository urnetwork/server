package localclient

import (
	"context"
	"encoding/base64"
	"net/netip"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/connect/protocol"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// The proxy's in-process control path carries move result reports
// (connect/MOVE.md §5.1): a hosted child's report is accepted only for the
// child's own move intent, which it then removes.
func TestAuthorityControlAcceptsMoveResultForTheChildIntent(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
		defer cancel()
		owner, _ := authorityMintTestOwner(t, ctx)
		minted, child := authorityTestMint(t, ctx, owner)

		setIntent := func(clientId server.Id) *model.ResidentMoveIntent {
			intent := &model.ResidentMoveIntent{
				MoveId:         server.NewId(),
				Kind:           model.ResidentMoveKindAdvisory,
				FromResidentId: server.NewId(),
				FromExchangeId: server.NewId(),
				TargetHost:     "alpha-edge-1",
				Targets:        []netip.Addr{netip.MustParseAddr("198.51.100.21")},
			}
			if err := model.SetResidentMoveIntent(ctx, clientId, intent, time.Minute); err != nil {
				t.Fatal(err)
			}
			return intent
		}
		report := func(moveId server.Id) {
			frame, err := connect.ToFrame(&protocol.ResidentMoveResult{
				MoveId:  moveId.Bytes(),
				Outcome: protocol.ResidentMoveOutcome_ResidentMoveOutcomeDeclined,
				Reason:  protocol.ResidentMoveReason_ResidentMoveReasonRecentMove,
			}, connect.DefaultProtocolVersion)
			if err != nil {
				t.Fatal(err)
			}
			defer connect.MessagePoolReturn(frame.MessageBytes)
			pack, err := proto.Marshal(&protocol.Pack{Frames: []*protocol.Frame{frame}})
			if err != nil {
				t.Fatal(err)
			}
			result, err := owner.ConnectControl(ctx, minted.ByClientJwt, &connect.ConnectControlArgs{Pack: base64.StdEncoding.EncodeToString(pack)})
			if err != nil || result == nil || result.Error != nil {
				t.Fatalf("hosted control report failed: %v %+v", err, result)
			}
		}
		intentOf := func(clientId server.Id) *model.ResidentMoveIntent {
			intent, err := model.GetResidentMoveIntent(ctx, clientId)
			if err != nil {
				t.Fatal(err)
			}
			return intent
		}

		// another client's move is not the child's
		otherClientId := server.NewId()
		otherIntent := setIntent(otherClientId)
		report(otherIntent.MoveId)
		if intentOf(otherClientId) == nil {
			t.Fatal("the child's report removed another client's intent")
		}

		intent := setIntent(*child.ClientId)
		// a report of another move leaves the child's intent
		report(server.NewId())
		if kept := intentOf(*child.ClientId); kept == nil || kept.MoveId != intent.MoveId {
			t.Fatal("a foreign move's report changed the child's intent")
		}
		report(intent.MoveId)
		if remaining := intentOf(*child.ClientId); remaining != nil {
			t.Fatalf("the accepted report left the intent: %+v", remaining)
		}
	})
}
