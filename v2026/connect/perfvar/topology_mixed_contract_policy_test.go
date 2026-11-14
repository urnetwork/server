// Mixed application fixtures retain relay payload routes, so their real
// endpoint contracts must survive promotion to either direct carrier.
package perfvar

import (
	"sync/atomic"
	"testing"
	"time"

	clientconnect "github.com/urnetwork/connect/v2026"
	"github.com/urnetwork/connect/v2026/protocol"
	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
)

// Native datagrams and the relay share one real contract-required fixture.
func TestFullTunMixedFastRetainsRelayContractOwnership(t *testing.T) {
	testFullTunMixedRelayContractOwnership(t, fullTunRouteP2pFastExchangeH1)
}

// The reliable direct carrier retains the same relay authorization policy.
func TestFullTunMixedLegacyRetainsRelayContractOwnership(t *testing.T) {
	testFullTunMixedRelayContractOwnership(t, fullTunRouteP2pLegacyExchangeH1)
}

// The current generated owner, real application probes and acknowledged marker
// expose the constructor's policy transition without waiting for lease expiry.
func testFullTunMixedRelayContractOwnership(t *testing.T, route fullTunRoute) {
	t.Helper()
	if testing.Short() {
		return
	}
	testEnvironment := &server.TestEnv{ApplyDbMigrations: true, RerunCount: 0}
	testEnvironment.Run(t, func(t testing.TB) {
		profiles := allNetworkProfiles(4311)
		direct := profiles["clean-lan"]
		relay := mixedRelayAccessProfileFor("clean-lan", 4311)
		fixture, err := newPerfvarCorrectnessFixture(
			t,
			route,
			direct,
			relay,
			direct,
			defaultTunResourceProfile(),
			10*time.Minute,
		)
		if err != nil {
			t.Fatalf("%s constructor: %v", route, err)
		}
		defer fixture.close()
		ctx := fixture.ctx
		path := fixture.path
		if !path.readinessAppFence.Load() {
			t.Fatal("mixed constructor did not complete its real application probes")
		}
		source, err := waitForCurrentGeneratedDeviceClient(ctx, path, path.deviceTransports)
		if err != nil {
			t.Fatalf("mixed generated-client owner fence: %v", err)
		}
		destination := path.providerClient
		sourceId := source.ClientId()
		destinationId := destination.ClientId()
		// One immutable owner snapshot also retains the constructor's client
		// if a legitimate replacement preceded the current-client fence.
		retainedTransports := path.deviceTransports.head.Load()
		deviceWriter := source.RouteManager().OpenMultiRouteWriter(clientconnect.DestinationId(destinationId))
		defer source.RouteManager().CloseMultiRouteWriter(deviceWriter)
		providerWriter := destination.RouteManager().OpenMultiRouteWriter(clientconnect.DestinationId(sourceId))
		defer destination.RouteManager().CloseMultiRouteWriter(providerWriter)
		deviceRoutes := clientconnect.TestingObserveMultiRouteWriterRouteState(deviceWriter)
		defer deviceRoutes.Close()
		providerRoutes := clientconnect.TestingObserveMultiRouteWriterRouteState(providerWriter)
		defer providerRoutes.Close()
		if deviceRoutes.Snapshot().ActiveRouteCount != 2 || providerRoutes.Snapshot().ActiveRouteCount != 2 ||
			path.deviceSendRoutes.isDisabled() || path.providerSendRoutes.isDisabled() {
			t.Fatalf("mixed constructor lacks selectable relay and direct routes: device=%+v provider=%+v", deviceRoutes.Snapshot(), providerRoutes.Snapshot())
		}

		const content = "synthetic mixed route contract ownership marker"
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
			// The send owner accounts contracts before this inline callback.
			// Snapshot here, without demanding an immortal earlier contract.
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
			t.Fatal("mixed contract-policy marker was not admitted")
		}
		select {
		case key := <-received:
			if !key.ForceStream || key.CompanionContract || key.LogicalLane != 0 {
				t.Fatalf("mixed contract-policy marker used a different logical lane: %+v", key)
			}
		case <-ctx.Done():
			t.Fatalf("mixed contract-policy marker was not delivered: %v", ctx.Err())
		}
		var atAck *clientconnect.ContractManagerStats
		select {
		case observation := <-acked:
			if observation.err != nil {
				t.Fatalf("mixed contract-policy marker was not acknowledged: %v", observation.err)
			}
			atAck = observation.stats
		case <-ctx.Done():
			t.Fatalf("mixed contract-policy marker did not complete: %v", ctx.Err())
		}
		if receiveCount.Load() != 1 || ackCount.Load() != 1 {
			t.Errorf("mixed contract-policy marker receive/ack counts=%d/%d, want 1/1", receiveCount.Load(), ackCount.Load())
		}
		if path.deviceClient.Load() != source {
			t.Fatal("generated client changed during the contract-policy marker")
		}
		ownedContractCount := 0
		for _, key := range atAck.ContractOpenKeys {
			if key.Destination.DestinationId == destinationId && key.ForceStream &&
				!key.CompanionContract && !key.EncryptionCompanion &&
				key.EncryptionRole == 0 && key.LogicalLane == 0 {
				ownedContractCount += 1
			}
		}
		if ownedContractCount == 0 {
			t.Errorf("mixed fixture lacks real ForceStream contract ownership at acknowledgment: open=%d close=%d", atAck.ContractOpenCount, atAck.ContractCloseCount)
		}

		// The append-only retained owner keeps earlier generated clients too.
		// A replacement must not conceal the constructor's forbidden policy.
		generatedClients := map[*clientconnect.Client]bool{source: true}
		for node := retainedTransports; node != nil; node = node.next {
			if node.client != nil {
				generatedClients[node.client] = true
			}
		}
		for generated := range generatedClients {
			manager := generated.ContractManager()
			if manager.SendNoContract(destinationId) || manager.ReceiveNoContract(destinationId) {
				t.Errorf("mixed generated client %s disabled contracts while relay remains selectable", generated.ClientId())
			}
			manager = destination.ContractManager()
			if manager.SendNoContract(generated.ClientId()) || manager.ReceiveNoContract(generated.ClientId()) {
				t.Errorf("mixed provider disabled contracts for generated client %s while relay remains selectable", generated.ClientId())
			}
		}
		status, validUntil, err := model.ReadContractHoleLease(ctx, server.Id(sourceId), server.Id(destinationId))
		if err != nil || status != model.ContractHolePositive || !time.Now().Before(validUntil) {
			t.Errorf("acknowledged mixed fixture lacks fresh relay permission: status=%v deadline=%v error=%v", status, validUntil, err)
		}
		if deviceRoutes.Snapshot().ActiveRouteCount != 2 || providerRoutes.Snapshot().ActiveRouteCount != 2 ||
			path.deviceSendRoutes.isDisabled() || path.providerSendRoutes.isDisabled() {
			t.Errorf("mixed marker lost a selectable carrier: device=%+v provider=%+v", deviceRoutes.Snapshot(), providerRoutes.Snapshot())
		}
		if err := path.verifyRoute(); err != nil {
			t.Errorf("mixed application route evidence: %v", err)
		}
	})
}
