// This file pins direct-P2P platform suppression at the Pack and route levels.
package perfvar

import (
	"context"
	"testing"
	"time"

	clientconnect "github.com/urnetwork/connect"
	"github.com/urnetwork/connect/protocol"
)

// One decoded route item retains the destination and application message
// types needed to distinguish test traffic from concurrent control work.
type platformRouteFrame struct {
	destinationId clientconnect.Id
	messageTypes  []protocol.MessageType
}

// Draining decodes both current direct Packs and legacy TransferPack frames,
// then releases every pooled wire byte slice.
func drainPlatformRouteFrames(
	t *testing.T,
	route clientconnect.Route,
) []platformRouteFrame {
	t.Helper()
	frames := []platformRouteFrame{}
	for {
		select {
		case transferFrameBytes := <-route:
			var transferFrame protocol.TransferFrame
			if err := clientconnect.ProtoUnmarshal(transferFrameBytes, &transferFrame); err != nil {
				clientconnect.MessagePoolReturn(transferFrameBytes)
				t.Fatalf("decode platform transfer frame: %v", err)
			}
			path, err := clientconnect.TransferPathFromProtobuf(transferFrame.TransferPath)
			if err != nil {
				clientconnect.MessagePoolReturn(transferFrameBytes)
				t.Fatalf("decode platform transfer path: %v", err)
			}
			pack := transferFrame.Pack
			if pack == nil && transferFrame.Frame != nil &&
				transferFrame.Frame.MessageType == protocol.MessageType_TransferPack {
				pack = &protocol.Pack{}
				if err := clientconnect.ProtoUnmarshal(transferFrame.Frame.MessageBytes, pack); err != nil {
					clientconnect.MessagePoolReturn(transferFrameBytes)
					t.Fatalf("decode legacy platform Pack: %v", err)
				}
			}
			messageTypes := []protocol.MessageType{}
			if pack != nil {
				for _, frame := range pack.Frames {
					messageTypes = append(messageTypes, frame.MessageType)
				}
			} else if transferFrame.Frame != nil {
				messageTypes = append(messageTypes, transferFrame.Frame.MessageType)
			}
			clientconnect.MessagePoolReturn(transferFrameBytes)
			frames = append(frames, platformRouteFrame{
				destinationId: path.DestinationId,
				messageTypes:  messageTypes,
			})
		default:
			return frames
		}
	}
}

// Exact application identity requires both the routed destination and one
// decoded message type; destination-only matching can select startup control.
func hasPlatformRouteMessage(
	frames []platformRouteFrame,
	destinationId clientconnect.Id,
	messageType protocol.MessageType,
) bool {
	for _, frame := range frames {
		if frame.destinationId != destinationId {
			continue
		}
		for _, candidate := range frame.messageTypes {
			if candidate == messageType {
				return true
			}
		}
	}
	return false
}

// Direct P2P suppresses only provider payload on the exchange route. A
// ControlId Pack still reaches a terminal platform write, while a provider
// Pack reaches only the already-live P2P route.
func TestDirectP2pSuppressionPreservesControlAndExcludesPayloadExchange(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	clientId := clientconnect.NewId()
	providerId := clientconnect.NewId()
	streamId := clientconnect.NewId()
	noAckSends := newNoAckSendTracker()
	defer noAckSends.close()
	settings := clientconnect.DefaultClientSettings()
	settings.ControlPingTimeout = 0
	settings.ContractManagerSettings.NetworkEventTimeEnableContracts = time.Now().Add(time.Hour)
	settings.EncryptionSettings.Mode = clientconnect.EncryptionModeOff
	settings.SendBufferSettings.NoAckSendObserver = noAckSends.newObserver()
	client := clientconnect.NewClient(
		ctx,
		clientId,
		clientconnect.NewNoContractClientOob(),
		settings,
	)
	defer client.Cancel()
	client.ContractManager().AddNoContractPeer(providerId)
	client.ContractManager().AddNoContractPeer(clientconnect.ControlId)

	controller := newPlatformSendRouteController(providerId)
	defer closePlatformSendRouteController(t, controller)
	controller.setRouteManager(client.RouteManager())
	platformTransport, _ := controller.newTransportPair()
	platformRoute := make(clientconnect.Route, 64)
	controller.observe(platformTransport, platformRoute, true)
	p2pTransport := clientconnect.NewSendClientTransport(clientconnect.DestinationId(providerId))
	p2pRoute := make(clientconnect.Route, 8)
	client.RouteManager().UpdateTransport(p2pTransport, []clientconnect.Route{p2pRoute})
	controller.observeP2pRoute(clientconnect.P2pRouteState{
		PeerId:    providerId,
		StreamId:  streamId,
		Send:      true,
		Connected: true,
	})
	controller.setDisabled(true)
	defer controller.setDisabled(false)

	controlFrame, err := clientconnect.ToFrame(&protocol.ControlPing{}, settings.ProtocolVersion)
	if err != nil {
		t.Fatal(err)
	}
	controlFailureCountBefore := noAckSends.failures()
	if !client.SendControlWithTimeout(controlFrame, nil, time.Second, clientconnect.NoAck()) {
		clientconnect.MessagePoolReturn(controlFrame.MessageBytes)
		t.Fatal("suppressed direct-P2P client rejected ControlId Pack")
	}
	controlBoundary, ok := noAckSends.boundary(ctx)
	if !ok || !noAckSends.waitThrough(ctx, controlBoundary) {
		t.Fatalf("ControlId Pack did not complete its first platform write: %v", ctx.Err())
	}
	if failureCount := noAckSends.failures() - controlFailureCountBefore; failureCount != 0 {
		t.Fatalf("ControlId Pack had %d first-write failures", failureCount)
	}
	platformFrames := drainPlatformRouteFrames(t, platformRoute)
	if !hasPlatformRouteMessage(
		platformFrames,
		clientconnect.ControlId,
		protocol.MessageType_TransferControlPing,
	) {
		t.Fatalf("ControlId Pack did not use platform route: frames=%v", platformFrames)
	}
	select {
	case transferFrameBytes := <-p2pRoute:
		clientconnect.MessagePoolReturn(transferFrameBytes)
		t.Fatal("ControlId Pack leaked onto provider P2P route")
	default:
	}
	providerWriter := client.RouteManager().OpenMultiRouteWriter(
		clientconnect.DestinationId(providerId),
	)
	defer client.RouteManager().CloseMultiRouteWriter(providerWriter)
	if routes := providerWriter.GetActiveRoutes(); len(routes) != 1 {
		t.Fatalf("forced provider routes=%d before payload, want P2P only", len(routes))
	}

	providerFrame, err := clientconnect.ToFrame(&protocol.IpPing{}, settings.ProtocolVersion)
	if err != nil {
		t.Fatal(err)
	}
	providerFailureCountBefore := noAckSends.failures()
	if !client.SendWithTimeout(providerFrame, providerId, nil, time.Second, clientconnect.NoAck()) {
		clientconnect.MessagePoolReturn(providerFrame.MessageBytes)
		t.Fatal("suppressed direct-P2P client rejected provider Pack")
	}
	providerBoundary, ok := noAckSends.boundary(ctx)
	if !ok || !noAckSends.waitThrough(ctx, providerBoundary) {
		t.Fatalf("provider Pack did not complete its first P2P write: %v", ctx.Err())
	}
	if failureCount := noAckSends.failures() - providerFailureCountBefore; failureCount != 0 {
		t.Fatalf("provider Pack had %d first-write failures", failureCount)
	}
	select {
	case transferFrameBytes := <-p2pRoute:
		var transferFrame protocol.TransferFrame
		if err := clientconnect.ProtoUnmarshal(transferFrameBytes, &transferFrame); err != nil {
			clientconnect.MessagePoolReturn(transferFrameBytes)
			t.Fatalf("decode provider P2P transfer frame: %v", err)
		}
		path, err := clientconnect.TransferPathFromProtobuf(transferFrame.TransferPath)
		if err != nil || path.DestinationId != providerId {
			clientconnect.MessagePoolReturn(transferFrameBytes)
			t.Fatalf("provider P2P path=%+v err=%v", path, err)
		}
		pack := transferFrame.Pack
		if pack == nil || len(pack.Frames) != 1 ||
			pack.Frames[0].MessageType != protocol.MessageType_IpIpPing {
			clientconnect.MessagePoolReturn(transferFrameBytes)
			t.Fatalf("provider P2P Pack=%+v", pack)
		}
		clientconnect.MessagePoolReturn(transferFrameBytes)
	case <-ctx.Done():
		t.Fatalf("provider Pack did not use P2P route: %v", ctx.Err())
	}
	for _, frame := range drainPlatformRouteFrames(t, platformRoute) {
		if frame.destinationId == providerId {
			t.Fatal("provider Pack leaked onto exchange route")
		}
	}

	controlWriter := client.RouteManager().OpenMultiRouteWriter(
		clientconnect.DestinationId(clientconnect.ControlId),
	)
	defer client.RouteManager().CloseMultiRouteWriter(controlWriter)
	controller.observeP2pRoute(clientconnect.P2pRouteState{
		PeerId:    providerId,
		StreamId:  streamId,
		Send:      true,
		Connected: false,
	})
	client.RouteManager().RemoveTransport(p2pTransport)
	if routes := providerWriter.GetActiveRoutes(); len(routes) != 0 {
		t.Fatalf("provider platform fallback routes=%d after P2P disconnect, want 0", len(routes))
	}
	if routes := controlWriter.GetActiveRoutes(); len(routes) != 1 {
		t.Fatalf("ControlId platform routes=%d after P2P disconnect, want 1", len(routes))
	}
	written, writeErr := providerWriter.WriteDetailed(ctx, []byte("no exchange fallback"), 0)
	if writeErr != nil || written {
		t.Fatalf("disconnected forced provider write=(%t, %v), want rejected", written, writeErr)
	}
	for _, frame := range drainPlatformRouteFrames(t, platformRoute) {
		if frame.destinationId == providerId {
			t.Fatal("disconnected provider payload used exchange route")
		}
	}
	if violationCount := controller.fallbackViolationCount.Load(); violationCount != 0 {
		t.Fatalf("forced provider fallback policy violations=%d", violationCount)
	}
}
