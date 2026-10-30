// A live P2P fixture keeps exchange fallback, so peer-local no-contract policy
// must not retire the real contract that owns the signaling/relay permission.
package perfvar

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	clientconnect "github.com/urnetwork/connect/v2026"
	"github.com/urnetwork/connect/v2026/protocol"
	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
)

// The actual constructor and a real acknowledged application send expose the
// policy flip before any outage or cache-expiration timing can conceal it.
func TestLiveP2pRouteRetainsRelayContractOwnership(t *testing.T) {
	if testing.Short() {
		return
	}
	testEnvironment := &server.TestEnv{ApplyDbMigrations: true, RerunCount: 0}
	testEnvironment.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		profile := initialNetworkProfiles(4310)["clean-lan"]
		environment := newRouteEnvironment(ctx, t, profile)
		defer environment.close()
		fixture := newLiveP2pRoute(t, environment)
		defer fixture.close()
		source := fixture.source.client
		destination := fixture.destination.client
		sourceId := source.ClientId()
		destinationId := destination.ClientId()
		before := source.ContractManager().LocalStats()

		const content = "synthetic live P2P contract ownership marker"
		received := make(chan clientconnect.TransferKey, 1)
		acked := make(chan struct {
			err   error
			stats *clientconnect.ContractManagerStats
		}, 1)
		var receiveCount atomic.Int32
		var ackCount atomic.Int32
		unsub := destination.AddReceiveCallback(func(path clientconnect.TransferPath, frames []*protocol.Frame, peer clientconnect.Peer) {
			if path.SourceId != sourceId {
				return
			}
			for _, frame := range frames {
				if frame.MessageType != protocol.MessageType_TestSimpleMessage {
					continue
				}
				message, err := clientconnect.FromFrame(frame)
				if err != nil {
					continue
				}
				if simple, ok := message.(*protocol.SimpleMessage); ok && simple.Content == content {
					receiveCount.Add(1)
					select {
					case received <- peer.TransferKey:
					default:
					}
				}
			}
		})
		defer unsub()
		frame := clientconnect.RequireToFrameWithDefaultProtocolVersion(&protocol.SimpleMessage{Content: content})
		if !source.SendWithTimeout(frame, destinationId, func(err error) {
			ackCount.Add(1)
			// Snapshot on the sequence's ACK boundary, before it can idle or
			// rotate again while the test goroutine waits to be scheduled.
			stats := source.ContractManager().LocalStats()
			select {
			case acked <- struct {
				err   error
				stats *clientconnect.ContractManagerStats
			}{err: err, stats: stats}:
			default:
			}
		}, time.Second, clientconnect.ForceStream()) {
			clientconnect.MessagePoolReturn(frame.MessageBytes)
			t.Fatal("contract-policy marker was not admitted")
		}
		select {
		case key := <-received:
			if !key.ForceStream || key.CompanionContract || key.LogicalLane != 0 {
				t.Fatalf("contract-policy marker used a different logical lane: %+v", key)
			}
		case <-ctx.Done():
			t.Fatalf("contract-policy marker was not delivered: %v", ctx.Err())
		}
		var after *clientconnect.ContractManagerStats
		select {
		case observation := <-acked:
			if observation.err != nil {
				t.Fatalf("contract-policy marker did not receive a successful ACK: %v", observation.err)
			}
			after = observation.stats
		case <-ctx.Done():
			t.Fatalf("contract-policy marker did not complete: %v", ctx.Err())
		}
		if receiveCount.Load() != 1 || ackCount.Load() != 1 {
			t.Errorf("contract-policy marker receive/ACK counts=%d/%d, want 1/1", receiveCount.Load(), ackCount.Load())
		}

		// The no-contract branch retires the active contract before this send;
		// ACK accounting also runs before the callback. No fake ACK or lease
		// substitutes for those boundaries, and rollover may change the ID.
		ownedContractCount := 0
		for _, key := range after.ContractOpenKeys {
			if key.Destination.DestinationId == destinationId && key.ForceStream &&
				!key.CompanionContract && !key.EncryptionCompanion &&
				key.EncryptionRole == 0 && key.LogicalLane == 0 {
				ownedContractCount += 1
			}
		}
		if ownedContractCount == 0 {
			t.Errorf("live fixture lacks real ForceStream contract ownership at ACK: open=%d/%d close=%d/%d", before.ContractOpenCount, after.ContractOpenCount, before.ContractCloseCount, after.ContractCloseCount)
		}
		for _, pair := range []struct {
			name   string
			client *clientconnect.Client
			peerId clientconnect.Id
		}{
			{name: "source", client: source, peerId: destinationId},
			{name: "destination", client: destination, peerId: sourceId},
		} {
			manager := pair.client.ContractManager()
			if manager.SendNoContract(pair.peerId) || manager.ReceiveNoContract(pair.peerId) {
				t.Errorf("%s disabled contracts while exchange fallback remains selectable", pair.name)
			}
		}
		status, validUntil, err := model.ReadContractHoleLease(ctx, fixture.source.clientId, fixture.destination.clientId)
		if err != nil || status != model.ContractHolePositive || !time.Now().Before(validUntil) {
			t.Errorf("acknowledged live fixture lacks fresh relay permission: status=%v deadline=%v error=%v", status, validUntil, err)
		}
	})
}
