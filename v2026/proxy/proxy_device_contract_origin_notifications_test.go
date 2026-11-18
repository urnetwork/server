// The manager's one contract origin notification owner (server CPUOPT6.md
// B #2), which the local authority of every device it opens borrows instead of
// building its own. Synthetic identities only; no watch is registered, so no
// subscription is opened.
package proxy

import (
	"context"
	"testing"
	"time"

	"github.com/urnetwork/sdk/v2026"
	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
)

// Two devices of one manager share its notifications, so a refusal recorded
// through one replays for an identical request through the other, and the
// manager closes them once every device worker has stopped. On f43488da1 each
// device's authority built its own.
func TestProxyDeviceManagerSharesOneContractOriginNotifications(t *testing.T) {
	popConfig := server.Config.PushSimpleResource("proxy.yml", []byte("{}\n"))
	defer popConfig()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	manager := budgetTestManager(t, ctx)
	closed := false
	defer func() {
		if !closed {
			manager.CloseAndWait(context.Background())
		}
	}()

	first := manager.proxyDeviceSettings().authoritySettings()
	second := manager.proxyDeviceSettings().authoritySettings()
	if first.ContractOriginNotifications == nil {
		t.Fatal("a device's authority gets no notifications from its manager, so it builds its own")
	}
	if first.ContractOriginNotifications != second.ContractOriginNotifications {
		t.Fatal("two devices of one manager do not share its notifications")
	}
	notifications := first.ContractOriginNotifications
	sourceId, destinationId := server.NewId(), server.NewId()
	rejection := model.ContractRouteRejection{
		Cause:          model.ContractRouteMissingOrigin,
		ResolutionPath: "test",
	}
	if !notifications.RecordRouteRejection(sourceId, destinationId, true, rejection, nil) {
		t.Fatal("the refusal was not recorded")
	}
	if _, ok := second.ContractOriginNotifications.RouteRejection(sourceId, destinationId, true); !ok {
		t.Fatal("a refusal recorded through one device does not replay through another")
	}

	if err := manager.CloseAndWait(ctx); err != nil {
		t.Fatal(err)
	}
	closed = true
	if _, ok := notifications.RouteRejection(sourceId, destinationId, true); ok {
		t.Fatal("the manager's notifications replay a refusal after the manager closed")
	}
	if notifications.RecordRouteRejection(sourceId, destinationId, true, rejection, nil) {
		t.Fatal("the manager's notifications record after the manager closed")
	}
}

// The default shares an owner with the api's per-process budgets, which keep
// the route refusal timeouts of 60 s and, for terminal refusals, 6 h. Settings
// built without them share none, and each device's authority builds its own.
func TestProxyDeviceManagerContractOriginNotificationDefaults(t *testing.T) {
	popConfig := server.Config.PushSimpleResource("proxy.yml", []byte("{}\n"))
	defer popConfig()
	settings := DefaultProxyDeviceManagerSettings()
	if settings.ContractOriginNotificationSettings != model.DefaultContractOriginNotificationSettings() {
		t.Fatalf("default notification budgets %+v, want the api's %+v", settings.ContractOriginNotificationSettings, model.DefaultContractOriginNotificationSettings())
	}
	if settings.ContractOriginNotificationSettings.RouteRejectionTimeout != 60*time.Second {
		t.Fatalf("route refusal timeout %s, want 60s", settings.ContractOriginNotificationSettings.RouteRejectionTimeout)
	}
	if settings.ContractOriginNotificationSettings.TerminalRouteRejectionTimeout != 6*time.Hour {
		t.Fatalf("terminal route refusal timeout %s, want 6h", settings.ContractOriginNotificationSettings.TerminalRouteRejectionTimeout)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	manager := NewProxyDeviceManager(ctx, &ProxyDeviceManagerSettings{NetworkSpace: &sdk.NetworkSpace{}})
	defer manager.CloseAndWait(context.Background())
	if manager.proxyDeviceSettings().authoritySettings().ContractOriginNotifications != nil {
		t.Fatal("a manager whose settings share no owner hands its devices one")
	}
}
