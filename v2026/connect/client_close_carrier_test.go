// Carriers below the control coalescer's supported envelope leave close
// requests pending instead of consuming work that the transport cannot carry.
package connect

import (
	"testing"

	connectlib "github.com/urnetwork/connect/v2026"
	"github.com/urnetwork/connect/v2026/protocol"
	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
)

// A one-KiB carrier cannot safely coalesce individually fitting close frames.
// Refusal must happen before SPOP and preserve both kinds of pending request.
func TestClientCloseUnsupportedCarrierLeavesRequestsPending(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		clientId := server.NewId()
		contractIds := make([]server.Id, 32)
		peerIds := make([]server.Id, 33)
		pendingContractKVs := map[server.Id]bool{}
		pendingPeerKVs := map[server.Id]bool{}
		for i := range contractIds {
			contractIds[i] = server.NewId()
			pendingContractKVs[contractIds[i]] = true
		}
		for i := range peerIds {
			peerIds[i] = server.NewId()
			pendingPeerKVs[peerIds[i]] = true
		}
		model.AddClientCloseRequests(ctx, clientId, contractIds, peerIds)

		settings := DefaultExchangeSettings()
		settings.FramerSettings = connectlib.DefaultFramerSettings(1024)
		sentFrameCount := 0
		listener := newClientCloseListener(ctx, clientId, func(frame *protocol.Frame) {
			sentFrameCount++
			connectlib.MessagePoolReturn(frame.MessageBytes)
		}, settings)
		defer listener.CloseAndWait()
		listener.deliver()
		listener.CloseAndWait()

		requests := model.TakeClientCloseRequests(ctx, clientId, len(contractIds)+len(peerIds)+1)
		if sentFrameCount != 0 {
			t.Fatalf("unsupported carrier emitted %d close frames", sentFrameCount)
		}
		for _, contractId := range requests.ContractIds {
			if !pendingContractKVs[contractId] {
				t.Fatalf("unexpected pending contract %s", contractId)
			}
			delete(pendingContractKVs, contractId)
		}
		for _, peerId := range requests.PeerIds {
			if !pendingPeerKVs[peerId] {
				t.Fatalf("unexpected pending peer %s", peerId)
			}
			delete(pendingPeerKVs, peerId)
		}
		if len(pendingContractKVs)+len(pendingPeerKVs) != 0 {
			t.Fatalf("unsupported carrier consumed %d contract and %d peer requests", len(pendingContractKVs), len(pendingPeerKVs))
		}
	})
}
