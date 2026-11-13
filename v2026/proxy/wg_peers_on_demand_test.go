package proxy

// The wg server registers its configured clients without device peers: a
// client's peer is created on its first handshake initiation and removed
// after the idle bound, so peers, goroutines and queues follow the clients
// that actually use WireGuard (proxy repo wg.go, PeersOnDemand).

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"runtime"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	proxyconnect "github.com/urnetwork/proxy/v2026"
	uwgdevice "github.com/urnetwork/userwireguard/v2026/device"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
)

func TestWgPeersOnDemandIsTheDefault(t *testing.T) {
	popConfig := server.Config.PushSimpleResource("proxy.yml", []byte("{}\n"))
	defer popConfig()
	if !proxyWgPeersOnDemandFromConfig() {
		t.Fatal("on-demand wg peers are off without configuration")
	}
}

func TestWgPeersOnDemandCanBeTurnedOff(t *testing.T) {
	popConfig := server.Config.PushSimpleResource("proxy.yml", []byte("wg_peers_on_demand: false\n"))
	defer popConfig()
	if proxyWgPeersOnDemandFromConfig() {
		t.Fatal("wg_peers_on_demand: false left on-demand peers on")
	}
}

func TestWgPeersOnDemandIgnoresAnInvalidValue(t *testing.T) {
	popConfig := server.Config.PushSimpleResource("proxy.yml", []byte("wg_peers_on_demand: sometimes\n"))
	defer popConfig()
	if !proxyWgPeersOnDemandFromConfig() {
		t.Fatal("an invalid wg_peers_on_demand turned on-demand peers off")
	}
}

func TestUpdateWgPeerMetricsPublishesPeersMissesAndEvictions(t *testing.T) {
	t.Cleanup(func() {
		updateWgPeerMetrics(proxyconnect.WgPeerStats{})
	})
	updateWgPeerMetrics(proxyconnect.WgPeerStats{
		PeerCount: 3,
		PeerMiss: uwgdevice.PeerMissStats{
			Accepted:          11,
			Refused:           7,
			NegativeCacheHits: 5,
			Coalesced:         4,
			RateLimited:       3,
			HandlerFailures:   2,
			HandlerTimeouts:   1,
			CreateFailures:    6,
		},
		EvictedPeerCount: 9,
	})
	if got := testutil.ToFloat64(wgDevicePeersGauge); got != 3 {
		t.Fatalf("wg device peers = %v, want 3", got)
	}
	if got := testutil.ToFloat64(wgPeerEvictionsGauge); got != 9 {
		t.Fatalf("wg peer evictions = %v, want 9", got)
	}
	for outcome, want := range map[string]float64{
		"accepted":           11,
		"refused":            7,
		"negative_cache_hit": 5,
		"coalesced":          4,
		"rate_limited":       3,
		"handler_failure":    2,
		"handler_timeout":    1,
		"create_failure":     6,
	} {
		if got := testutil.ToFloat64(wgPeerMissGauge.WithLabelValues(outcome)); got != want {
			t.Fatalf("wg peer miss %s = %v, want %v", outcome, got, want)
		}
	}
}

// Registering many configured clients creates no device peers and no
// goroutines; before on-demand peers each one started two.
func TestWgServerConfiguredClientsDoNotDriveGoroutines(t *testing.T) {
	if testing.Short() {
		return
	}
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		setProxyTestEnv()

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		proxyDeviceManager := NewProxyDeviceManagerWithDefaults(ctx)
		defer func() {
			_ = proxyDeviceManager.CloseAndWait(context.Background())
		}()
		wgCtx, wgCancel := context.WithCancel(ctx)
		defer wgCancel()
		ports, releasePorts := reserveProxyTestPorts(t)
		settings := DefaultProxySettings()
		settings.WgPort = ports.wg
		releasePorts()
		wg := NewWgServer(wgCtx, wgCancel, proxyDeviceManager, settings)
		// peers registered eagerly start their goroutines only on a device
		// that is up, so wait for the device to bind its port
		waitFor(t, 10*time.Second, "the wg device to bind its port", func() bool {
			return udpPortBound(ports.wg)
		})

		const configured = 2000
		proxyClients := make([]*model.ProxyClient, configured)
		for i := range proxyClients {
			proxyClients[i] = newTestProxyClient(t, netip.AddrFrom4([4]byte{10, 20, byte(i >> 8), byte(i)}))
		}
		goroutinesBefore := settledGoroutines()
		if err := wg.AddProxyClients(proxyClients...); err != nil {
			t.Fatalf("add proxy clients: %v", err)
		}
		goroutinesAfter := settledGoroutines()
		fmt.Printf("[progress]%d configured wg clients: %d device peers, %+d goroutines\n", configured, wg.wgProxy.PeerCount(), goroutinesAfter-goroutinesBefore)
		if count := wg.wgProxy.ClientCount(); count != configured {
			t.Fatalf("ClientCount = %d, want %d", count, configured)
		}
		if count := wg.wgProxy.PeerCount(); count != 0 {
			t.Fatalf("configured clients created %d device peers, want 0", count)
		}
		if delta := goroutinesAfter - goroutinesBefore; 20 < delta {
			t.Fatalf("configured clients added %d goroutines, want at most 20", delta)
		}
	})
}

// A hosted WireGuard session is established and carries traffic without the
// client's peer being registered up front: the peer appears with the first
// handshake.
func TestProxyWgHostedSessionWithoutPeerPreRegistration(t *testing.T) {
	if testing.Short() {
		return
	}
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		fmt.Printf("[progress]start TestProxyWgHostedSessionWithoutPeerPreRegistration\n")
		h := setupProxyTargetTest(t)
		defer h.close(t)

		if count := h.wg.wgProxy.ClientCount(); count != 1 {
			t.Fatalf("ClientCount = %d, want the harness client", count)
		}
		if count := h.wg.wgProxy.PeerCount(); count != 0 {
			t.Fatalf("the registered client has %d device peers before it handshook, want 0", count)
		}

		wgCtx, wgCancel := context.WithCancel(h.ctx)
		defer wgCancel()
		transport, closeWgClient := startWgClient(t, wgCtx, h.proxyClient.WgConfig, h.wgPort)
		defer closeWgClient()
		requireProxyGet(t, h, "wg-on-demand", transport)

		stats := h.wg.wgProxy.PeerStats()
		if stats.PeerCount != 1 || stats.PeerMiss.Accepted != 1 {
			t.Fatalf("peer stats = %+v, want the client's one peer from one accepted miss", stats)
		}
		statuses, err := h.wg.wgProxy.PeerStatuses()
		if err != nil {
			t.Fatalf("peer statuses: %v", err)
		}
		status := statuses[h.proxyClient.WgConfig.ClientIpv4]
		if status == nil || status.LastHandshake.IsZero() || status.Endpoint == nil {
			t.Fatalf("session status = %+v, want a handshake and an endpoint", status)
		}
	})
}

func settledGoroutines() int {
	runtime.GC()
	time.Sleep(100 * time.Millisecond)
	return runtime.NumGoroutine()
}

func udpPortBound(port int) bool {
	pc, err := net.ListenPacket("udp4", fmt.Sprintf("0.0.0.0:%d", port))
	if err != nil {
		return true
	}
	pc.Close()
	return false
}
