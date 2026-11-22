// An authorized application message crosses three real native P2P associations
// with its original source identity after the exchange transports are joined.
package perfvar

import (
	"bytes"
	"context"
	"fmt"
	"hash/crc64"
	"sync/atomic"
	"testing"
	"time"

	clientconnect "github.com/urnetwork/connect/v2026"
	"github.com/urnetwork/connect/v2026/protocol"
	"github.com/urnetwork/server/v2026"
)

// Two observed receive-loop entries enclose the source decision for one exact
// application frame. A separate callback proves end-to-end delivery.
func TestProductionThreeHopStreamDeliversOriginOverP2p(t *testing.T) {
	if testing.Short() {
		return
	}
	testEnvironment := &server.TestEnv{ApplyDbMigrations: true, RerunCount: 0}
	testEnvironment.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()

		const hopCount = 3
		const payloadByteCount = 1200
		profile := initialNetworkProfiles(3301)["clean-lan"]
		environment := newRouteEnvironmentWithNetworkPeers(ctx, t, profile, false)
		streamNetwork, err := newStreamP2pNetwork(profile, hopCount)
		if err != nil {
			environment.close()
			t.Fatalf("create three-hop P2P network: %v", err)
		}
		defer func() {
			environment.close()
			streamNetwork.close()
		}()

		payload := make([]byte, payloadByteCount)
		for byteIndex := 8; byteIndex < len(payload); byteIndex += 1 {
			payload[byteIndex] = byte(byteIndex % 251)
		}
		barrierPayload := []byte("next application message")
		checksumTable := crc64.MakeTable(crc64.ECMA)
		type wireFingerprint struct {
			wireHash     uint64
			byteCount    int
			transferPath clientconnect.TransferPath
		}
		// A retry can carry a different head encoding. Keep a bounded set of
		// sender-proven fingerprints without retaining the observer's bytes.
		var wireFingerprints [8]atomic.Pointer[wireFingerprint]
		observeSend := func(observation clientconnect.TransferWireMessageObservation) {
			var frame protocol.TransferFrame
			if err := clientconnect.ProtoUnmarshal(observation.TransferFrameBytes, &frame); err != nil {
				return
			}
			for _, app := range frame.GetPack().GetFrames() {
				if app.GetMessageType() != protocol.MessageType_TestSimpleMessage ||
					!bytes.Equal(app.GetMessageBytes(), payload) {
					continue
				}
				transferPath, err := clientconnect.TransferPathFromProtobuf(frame.GetTransferPath())
				if err != nil {
					return
				}
				fingerprint := wireFingerprint{
					wireHash:     crc64.Checksum(observation.WireMessageBytes, checksumTable),
					byteCount:    len(observation.WireMessageBytes),
					transferPath: transferPath,
				}
				for i := range wireFingerprints {
					previous := wireFingerprints[i].Load()
					if previous == nil && wireFingerprints[i].CompareAndSwap(nil, &fingerprint) {
						return
					}
					if previous != nil && *previous == fingerprint {
						return
					}
				}
				return
			}
		}

		type wireReceipt struct {
			event        clientconnect.TransferProgressEvent
			transferPath clientconnect.TransferPath
		}
		var receivingClient atomic.Pointer[clientconnect.Client]
		wireArrived := make(chan wireReceipt, 1)
		sourceDecision := make(chan uint64, 1)
		// Only the client's single route reader emits receive_wire. Compare its
		// forged-source count across consecutive reads, before either read is
		// processed. Thus no other frame can contribute to this delta.
		var arrived, decided bool
		var dropCountBefore uint64
		observeReceive := func(event clientconnect.TransferProgressEvent) {
			if event.Stage != "receive_wire" || decided {
				return
			}
			client := receivingClient.Load()
			if client == nil {
				return
			}
			if arrived {
				decided = true
				sourceDecision <- client.ReceiveStats().ForgedSourceDropCount - dropCountBefore
				return
			}
			for i := range wireFingerprints {
				fingerprint := wireFingerprints[i].Load()
				if fingerprint != nil && fingerprint.wireHash == event.WireHash &&
					fingerprint.byteCount == event.ByteCount {
					dropCountBefore = client.ReceiveStats().ForgedSourceDropCount
					arrived = true
					wireArrived <- wireReceipt{event: event, transferPath: fingerprint.transferPath}
					return
				}
			}
		}

		clients := make([]*routeClient, hopCount+1)
		for nodeIndex := range clients {
			var configure func(*clientconnect.ClientSettings)
			switch nodeIndex {
			case 0:
				configure = func(settings *clientconnect.ClientSettings) {
					settings.SendBufferSettings.TransferWireMessageObserver = observeSend
				}
			case hopCount:
				configure = func(settings *clientconnect.ClientSettings) {
					settings.ReceiveBufferSettings.ProgressObserver = observeReceive
				}
			}
			clients[nodeIndex] = environment.newClientWithSettings(
				fmt.Sprintf("stream regression node %d", nodeIndex),
				clientconnect.P2pDataPlaneModeFastOnly,
				streamNetwork.nets[nodeIndex],
				false,
				configure,
			)
			environment.connectPlatform(clients[nodeIndex], clientconnect.TransportModeH1)
		}
		source, destination := clients[0], clients[hopCount]
		receivingClient.Store(destination.client)
		for nodeIndex, client := range clients {
			if !waitForPlatform(ctx, client.transport) {
				t.Fatalf("stream node %d platform did not connect", nodeIndex)
			}
			if err := setProductionStreamProvide(ctx, client.client); err != nil {
				t.Fatalf("stream node %d provide: %v", nodeIndex, err)
			}
		}

		sourceId := clientconnect.Id(source.clientId)
		destinationId := clientconnect.Id(destination.clientId)
		path, err := clientconnect.NewMultiHopId(
			clientconnect.Id(clients[1].clientId),
			clientconnect.Id(clients[2].clientId),
			destinationId,
		)
		if err != nil {
			t.Fatalf("create three-hop path: %v", err)
		}
		streamId, err := waitForProductionStreamP2p(ctx, source, destination, path, clients)
		if err != nil {
			t.Fatalf("establish three-hop stream: %v", err)
		}
		writer := source.client.RouteManager().OpenMultiRouteWriter(clientconnect.DestinationId(destinationId))
		defer source.client.RouteManager().CloseMultiRouteWriter(writer)
		routeObserver := clientconnect.TestingObserveMultiRouteWriterRouteState(writer)
		defer routeObserver.Close()
		for hopIndex := range hopCount {
			leftId := clientconnect.Id(clients[hopIndex].clientId)
			rightId := clientconnect.Id(clients[hopIndex+1].clientId)
			clients[hopIndex].client.ContractManager().AddNoContractPeer(rightId)
			clients[hopIndex+1].client.ContractManager().AddNoContractPeer(leftId)
		}
		if source.client.ContractManager().SendNoContract(destinationId) ||
			destination.client.ContractManager().ReceiveNoContract(sourceId) {
			t.Fatal("logical endpoints must use their verified stream contract")
		}
		platformBarrier := routeObserver.Snapshot()
		if platformBarrier.ActiveRouteCount != 2 {
			t.Fatalf("expected stream and platform before forced route: %+v", platformBarrier)
		}
		if err := closeRouteClientTransportsAndWait(ctx, routeClientLifecycles(clients)); err != nil {
			t.Fatalf("join all three-hop platform transports: %v", err)
		}
		if _, err := waitForRouteCountAfter(ctx, routeObserver, platformBarrier, 1); err != nil {
			t.Fatalf("wait for the three-hop stream as the only source route: %v", err)
		}

		type receipt struct {
			sourceId      clientconnect.Id
			transportType clientconnect.TransportType
		}
		delivered := make(chan receipt, 1)
		unsub := destination.client.AddReceiveCallback(func(
			transferPath clientconnect.TransferPath,
			frames []*protocol.Frame,
			peer clientconnect.Peer,
		) {
			for _, frame := range frames {
				if frame.MessageType == protocol.MessageType_TestSimpleMessage &&
					bytes.Equal(frame.MessageBytes, payload) {
					select {
					case delivered <- receipt{
						sourceId:      transferPath.SourceId,
						transportType: peer.TransportType,
					}:
					default:
					}
				}
			}
		})
		defer unsub()
		send := func(messageBytes []byte) {
			frame := &protocol.Frame{
				MessageType:  protocol.MessageType_TestSimpleMessage,
				MessageBytes: clientconnect.MessagePoolCopy(messageBytes),
				Raw:          true,
			}
			// Reliable sends use the existing per-message wire observer. The
			// full topology test separately exercises its ordinary NoAck sends.
			if !source.client.SendMultiHopWithTimeout(
				frame, path, nil, 60*time.Second, clientconnect.ForceStream(),
			) {
				clientconnect.MessagePoolReturn(frame.MessageBytes)
				t.Fatal("three-hop application send was not admitted")
			}
		}

		send(payload)
		select {
		case receipt := <-wireArrived:
			if receipt.transferPath.SourceId != sourceId || receipt.transferPath.DestinationId != destinationId {
				t.Fatalf("application wire path=%+v, want %s to %s", receipt.transferPath, sourceId, destinationId)
			}
			if receipt.event.ClientId != destinationId || receipt.event.TransportType != clientconnect.TransportTypeP2p {
				t.Fatalf("application wire reached the wrong endpoint or carrier: %+v", receipt.event)
			}
		case <-ctx.Done():
			t.Fatalf("application wire did not reach the three-hop destination: %v", ctx.Err())
		}

		// The second legal send can advance the reader even when the first
		// frame never reaches application delivery.
		send(barrierPayload)
		select {
		case dropCount := <-sourceDecision:
			if dropCount != 0 {
				t.Fatalf("authorized three-hop origin was rejected after matching P2P wire arrival: stream=%s forged_source_drops=%d", streamId, dropCount)
			}
		case <-ctx.Done():
			t.Fatalf("destination did not advance to its next wire read: %v", ctx.Err())
		}
		select {
		case receipt := <-delivered:
			if receipt.sourceId != sourceId || receipt.transportType != clientconnect.TransportTypeP2p {
				t.Fatalf("three-hop application receipt=%+v, want source=%s via P2P", receipt, sourceId)
			}
		case <-ctx.Done():
			t.Fatalf("authorized three-hop application message did not reach its callback: %v", ctx.Err())
		}
	})
}
