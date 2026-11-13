package perfvar

import (
	"context"
	"testing"
	"time"

	clientconnect "github.com/urnetwork/connect/v2026"
)

// P2P scenarios use a separate, real H1 platform connection before promotion.
// The suppression wrapper may change matching, never that carrier's identity.
func TestPlatformSendRouteControllerPreservesH1CarrierType(t *testing.T) {
	controller := newPlatformSendRouteController(clientconnect.NewId())
	defer closePlatformSendRouteController(t, controller)
	send, receive := controller.newTransportPair()
	for name, transport := range map[string]clientconnect.Transport{"send": send, "receive": receive} {
		typed, ok := transport.(interface {
			TransportType() clientconnect.TransportType
		})
		if !ok || typed.TransportType() != clientconnect.TransportTypeH1 {
			t.Errorf("%s platform carrier=%v, want H1 before P2P promotion", name, transportTypeForH1TypingTest(transport))
		}
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	manager := clientconnect.NewRouteManager(ctx, "typed platform controller")
	controller.setRouteManager(manager)
	route := make(clientconnect.Route, 1)
	controller.observe(send, route, true)
	if !controller.waitForIdle() {
		t.Fatal("controller did not publish its H1 route")
	}
	writer := manager.OpenMultiRouteWriter(clientconnect.DestinationId(clientconnect.NewId()))
	defer manager.CloseMultiRouteWriter(writer)
	detailed, ok := writer.(interface {
		WriteDetailedWithTransport(context.Context, []byte, time.Duration) (bool, clientconnect.TransportType, error)
	})
	if !ok {
		t.Fatal("writer lacks actual-carrier disposition")
	}
	accepted, kind, err := detailed.WriteDetailedWithTransport(ctx, []byte("type-only fixture"), 0)
	if !accepted || err != nil || kind != clientconnect.TransportTypeH1 {
		t.Fatalf("actual platform writer disposition=%t/%s/%v, want accepted H1", accepted, kind, err)
	}
	<-route
}

func transportTypeForH1TypingTest(transport clientconnect.Transport) clientconnect.TransportType {
	if typed, ok := transport.(interface {
		TransportType() clientconnect.TransportType
	}); ok {
		return typed.TransportType()
	}
	return clientconnect.TransportTypeUnknown
}
