package proxy

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
)

// TestProxyClientNotificationInitialSyncEmpty covers the readiness gate for a
// host/block with no proxy clients: the first successful (empty) read of
// every watch stream completes the initial sync (PROXYDRAIN1.md §3.1).
func TestProxyClientNotificationInitialSyncEmpty(t *testing.T) {
	if testing.Short() {
		return
	}
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		setProxyTestEnv()
		// a host with no proxy clients; no dot, so the fq-host watch runs too
		// and both watches must complete
		os.Setenv("WARP_HOST", fmt.Sprintf("initialsync%d", time.Now().UnixNano()%1000000))
		defer os.Unsetenv("WARP_HOST")

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		settings := DefaultProxySettings()
		settings.NotificationTimeout = 500 * time.Millisecond

		notif := NewProxyClientNotification(ctx, settings)
		select {
		case <-notif.InitialSyncDone():
		case <-time.After(15 * time.Second):
			t.Fatalf("initial sync did not complete for an empty host/block")
		}
	})
}

// TestProxyClientNotificationInitialSyncDelivery covers the non-empty path:
// the initial sync completes only once the pending clients have actually been
// DELIVERED (wg peers applied), not merely read. A delivery that fails —
// here, because no callback is registered yet, the startup race the
// notification already retries — must hold the gate.
func TestProxyClientNotificationInitialSyncDelivery(t *testing.T) {
	if testing.Short() {
		return
	}
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		setProxyTestEnv()

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		// a proxy client, created the way auth-client does
		networkId := server.NewId()
		userId := server.NewId()
		deviceId := server.NewId()
		clientId := server.NewId()
		model.Testing_CreateNetwork(ctx, networkId, fmt.Sprintf("notiftest-%s", networkId), userId)
		model.Testing_CreateDevice(ctx, networkId, deviceId, clientId, "notiftest", "notiftest")

		proxyDeviceConfig := &model.ProxyDeviceConfig{
			ProxyDeviceConnection: model.ProxyDeviceConnection{
				ClientId: clientId,
			},
			ProxyDeviceMode: model.ProxyDeviceModeDevice,
		}
		if err := model.CreateProxyDeviceConfig(ctx, proxyDeviceConfig); err != nil {
			t.Fatalf("create proxy device config: %v", err)
		}
		proxyClient, err := model.CreateProxyClient(
			ctx,
			proxyDeviceConfig.ProxyId,
			clientId,
			proxyDeviceConfig.InstanceId,
			model.CreateProxyClientOptions{},
		)
		if err != nil {
			t.Fatalf("create proxy client: %v", err)
		}

		// watch exactly the (host, block) the client was assigned
		os.Setenv("WARP_HOST", proxyClient.ProxyHost)
		os.Setenv("WARP_BLOCK", proxyClient.Block)
		defer os.Unsetenv("WARP_HOST")
		defer os.Setenv("WARP_BLOCK", "test")

		settings := DefaultProxySettings()
		settings.NotificationTimeout = 500 * time.Millisecond

		notif := NewProxyClientNotification(ctx, settings)

		// no callback registered: the delivery fails and retries, so the
		// initial sync must NOT complete
		select {
		case <-notif.InitialSyncDone():
			t.Fatalf("initial sync completed without a delivery")
		case <-time.After(3 * time.Second):
		}

		// register the callback; the retried delivery now succeeds and the
		// initial sync completes
		delivered := make(chan server.Id, 16)
		sub := notif.AddProxyClientsCallback(func(proxyClients []*model.ProxyClient) error {
			for _, proxyClient := range proxyClients {
				select {
				case delivered <- proxyClient.ProxyId:
				default:
				}
			}
			return nil
		})
		defer sub()

		select {
		case <-notif.InitialSyncDone():
		case <-time.After(15 * time.Second):
			t.Fatalf("initial sync did not complete after the callback was registered")
		}

		// and the delivery carried the pending client
		waitFor(t, 5*time.Second, "created client delivered", func() bool {
			for {
				select {
				case proxyId := <-delivered:
					if proxyId == proxyClient.ProxyId {
						return true
					}
				default:
					return false
				}
			}
		})
	})
}
