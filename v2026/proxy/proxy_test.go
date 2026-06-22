package proxy

// Integration test for the proxy server, mirroring connect/connect_test in
// spirit. It stands up, in-process:
//
//   - a real connect server (exchange + connect handler) on a plain ws port
//   - a real api server (the full api.Routes()) on a plain http port
//   - a local provider (sdk.DeviceLocal in provide mode) that egresses to the
//     real internet
//   - the real proxy Http, Https, Socks and Wireguard servers
//
// A proxy device (the resident sdk.DeviceLocal created by the proxy) connects
// through the local api+connect servers and is pinned to the local provider via
// the real find-providers2 discovery path. The test then drives the proxy with
// Go network libraries (http/https/socks) and an in-process userspace WireGuard
// client, loading https://ur.io through each path.
//
// The SDK is pointed at the local servers via sdk.Testing_NewNetworkSpaceWithUrls.
// The default platform transport mode (auto) uses H1 (plain websocket), so no
// TLS/quic is required between the SDK and the local servers.
//
// NOTE: like connect_test, this expects the standard local test environment
// (WARP_ENV=local plus the local postgres/redis and vault, e.g. via test.sh)
// and real outbound internet access (it loads https://ur.io for real).

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"strconv"
	"sync"
	"testing"
	"time"

	xproxy "golang.org/x/net/proxy"

	"gvisor.dev/gvisor/pkg/buffer"
	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/adapters/gonet"
	"gvisor.dev/gvisor/pkg/tcpip/header"
	"gvisor.dev/gvisor/pkg/tcpip/link/channel"
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv4"
	"gvisor.dev/gvisor/pkg/tcpip/stack"
	"gvisor.dev/gvisor/pkg/tcpip/transport/icmp"
	"gvisor.dev/gvisor/pkg/tcpip/transport/tcp"
	"gvisor.dev/gvisor/pkg/tcpip/transport/udp"

	"github.com/urnetwork/userwireguard/v2026/conn"
	uwgdevice "github.com/urnetwork/userwireguard/v2026/device"
	"github.com/urnetwork/userwireguard/v2026/logger"
	"github.com/urnetwork/userwireguard/v2026/tun/tuntest"
	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"

	"github.com/urnetwork/connect/v2026"
	"github.com/urnetwork/connect/v2026/protocol"
	"github.com/urnetwork/sdk/v2026"
	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/api"
	connectserver "github.com/urnetwork/server/v2026/connect"
	"github.com/urnetwork/server/v2026/jwt"
	"github.com/urnetwork/server/v2026/model"
	"github.com/urnetwork/server/v2026/router"
)

const (
	testConnectClientPort  = 7200
	testConnectServicePort = 7300
	testApiPort            = 7400

	testTargetUrl = "https://api.bringyour.com/hello"

	// number of times the full leg sequence is run within a single setup, to
	// flush out intermittent failures (provider return-path / contract races)
	// explicitly.
	testProxyIterations = 16

	testInitialBalance = model.ByteCount(1024) * model.ByteCount(1024) * model.ByteCount(1024) * model.ByteCount(1024)
)

// proxyTestOptions parameterizes setupProxyTestWithOptions. The defaults
// reproduce the original setupProxyTest behavior.
type proxyTestOptions struct {
	// initial transfer balance redeemed for the proxy device network
	pdInitialBalance model.ByteCount
	// initial transfer balance redeemed for the provider network
	providerInitialBalance model.ByteCount
	// when true, the proxy device and the provider allow all packet
	// destinations. The production policies filter loopback/private
	// destinations for public provide relationships, which would block the
	// local target servers the contract/load tests egress to.
	disableSecurityPolicies bool
}

func defaultProxyTestOptions() *proxyTestOptions {
	return &proxyTestOptions{
		pdInitialBalance:       testInitialBalance,
		providerInitialBalance: testInitialBalance,
	}
}

// proxyTestHarness holds the handles a test needs after setup.
type proxyTestHarness struct {
	ctx           context.Context
	cancel        context.CancelFunc
	signedProxyId string
	proxyClient   *model.ProxyClient

	// the wg server runs under its own child context so a test can stop it
	// (simulating an instance restart) without tearing down the harness
	proxySettings      *ProxySettings
	proxyDeviceManager *ProxyDeviceManager
	wg                 *wgServer
	wgCancel           context.CancelFunc

	// network/client identities for contract and balance assertions
	pdNetworkId       server.Id
	pdClientId        server.Id
	providerNetworkId server.Id
	providerClientId  server.Id
	proxyId           server.Id
}

func setProxyTestEnv() {
	// WARP_ENV must already be "local" (asserted by DefaultTestEnv before setup).
	setIfEmpty := func(k, v string) {
		if os.Getenv(k) == "" {
			os.Setenv(k, v)
		}
	}
	setIfEmpty("WARP_SERVICE", "test")
	setIfEmpty("WARP_BLOCK", "test")
	setIfEmpty("WARP_VERSION", "0.0.0")
	setIfEmpty("WARP_DOMAIN", "bringyour.com")
	// the wg server reads RequireFwMark(); 0 disables the (linux-only) SO_MARK
	setIfEmpty("WARP_FWMARK", "0")
}

// setupProxyTest wires up the full local environment and returns a harness.
// It must be called inside DefaultTestEnv().Run (db + redis + migrations ready).
func setupProxyTest(t testing.TB) *proxyTestHarness {
	return setupProxyTestWithOptions(t, defaultProxyTestOptions())
}

func setupProxyTestWithOptions(t testing.TB, opts *proxyTestOptions) *proxyTestHarness {
	setProxyTestEnv()

	// the proxy servers bind fixed ports; when several tests in this package
	// each run their own setup sequentially in one process, wait for the
	// previous teardown to release them
	waitFor(t, 15*time.Second, "proxy ports released", func() bool {
		for _, port := range []int{InternalSocksPort, InternalHttpPort, InternalHttpsPort, InternalApiPort} {
			l, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
			if err != nil {
				return false
			}
			l.Close()
		}
		pc, err := net.ListenPacket("udp4", fmt.Sprintf("0.0.0.0:%d", InternalWgPort))
		if err != nil {
			return false
		}
		pc.Close()
		return true
	})

	ctx, cancel := context.WithCancel(context.Background())

	// ---- local connect server (plain ws, in-process) -------------------------
	connectHost := "proxytest"
	service := "connect"
	block := "test"
	routes := map[string]string{connectHost: "127.0.0.1"}
	hostToServicePorts := map[int]int{testConnectServicePort: testConnectServicePort}

	exchangeSettings := connectserver.DefaultExchangeSettings()
	exchange := connectserver.NewExchange(ctx, connectHost, service, block, hostToServicePorts, routes, exchangeSettings)

	connectHandlerSettings := connectserver.DefaultConnectHandlerSettings()
	connectHandlerSettings.ConnectionAnnounceTimeout = 0
	connectHandler := connectserver.NewConnectHandler(ctx, server.NewId(), exchange, connectHandlerSettings)

	connectRoutes := []*router.Route{
		router.NewRoute("GET", "/status", router.WarpStatus),
		router.NewRoute("GET", "/", connectHandler.Connect),
	}
	connectHttp := &http.Server{
		Addr:    fmt.Sprintf("127.0.0.1:%d", testConnectClientPort),
		Handler: router.NewRouter(ctx, connectRoutes),
	}
	go connectHttp.ListenAndServe()

	// ---- local api server (plain http, full route set, in-process) -----------
	apiHttp := &http.Server{
		Addr:    fmt.Sprintf("127.0.0.1:%d", testApiPort),
		Handler: router.NewRouter(ctx, api.Routes()),
	}
	go apiHttp.ListenAndServe()

	go func() {
		<-ctx.Done()
		connectHttp.Close()
		apiHttp.Close()
		exchange.Close()
	}()

	// give the listeners a moment to bind
	select {
	case <-time.After(1 * time.Second):
	}

	// ---- the test network space pointing the SDK at the local servers --------
	connectSettings := connect.DefaultConnectSettings()
	connectSettings.DisableIpv6 = true
	networkSpace := sdk.Testing_NewNetworkSpaceWithUrls(
		ctx,
		fmt.Sprintf("http://127.0.0.1:%d", testApiPort),
		fmt.Sprintf("ws://127.0.0.1:%d", testConnectClientPort),
		connectSettings,
	)

	// ---- a local provider (sdk.DeviceLocal in provide mode) ------------------
	providerNetworkId := server.NewId()
	providerUserId := server.NewId()
	providerNetworkName := fmt.Sprintf("provider-%s", providerNetworkId)
	providerDeviceId := server.NewId()
	providerClientId := server.NewId()
	providerInstanceId := server.NewId()

	model.Testing_CreateNetwork(ctx, providerNetworkId, providerNetworkName, providerUserId)
	model.Testing_CreateDevice(ctx, providerNetworkId, providerDeviceId, providerClientId, "provider", "provider")
	redeemBalance(t, ctx, providerNetworkId, opts.providerInitialBalance)

	providerByJwt := jwt.NewByJwt(providerNetworkId, providerUserId, providerNetworkName, false, false).
		Client(providerDeviceId, providerClientId).Sign()

	apiUrl := fmt.Sprintf("http://127.0.0.1:%d", testApiPort)
	platformUrl := fmt.Sprintf("ws://127.0.0.1:%d", testConnectClientPort)

	// The sdk's NewPlatformDeviceLocal hardcodes allowProvider=false (it's for
	// embedded source devices that reach providers via the multi-client
	// generator). A real provider needs its own client + platform transport +
	// egress NAT, so build it directly from connect primitives.
	providerStrategySettings := connect.DefaultClientStrategySettings()
	providerStrategySettings.EnableResilient = false
	providerClientStrategy := connect.NewClientStrategy(ctx, providerStrategySettings)

	providerOob := connect.NewApiOutOfBandControl(ctx, providerClientStrategy, providerByJwt, apiUrl)
	providerClient := connect.NewClient(ctx, connect.Id(providerClientId), providerOob, connect.DefaultClientSettings())
	go func() {
		<-ctx.Done()
		providerClient.Close()
	}()

	providerAuth := &connect.ClientAuth{
		ByJwt:      providerByJwt,
		InstanceId: connect.Id(providerInstanceId),
		AppVersion: server.RequireVersion(),
	}
	providerTransport := connect.NewPlatformTransportWithDefaults(
		providerClient.Ctx(),
		providerClientStrategy,
		providerClient.RouteManager(),
		platformUrl,
		providerAuth,
	)
	go func() {
		<-ctx.Done()
		providerTransport.Close()
	}()

	// egress to the real internet via a user-space NAT
	providerLocalUserNat := connect.NewLocalUserNatWithDefaults(providerClient.Ctx(), providerClientId.String())
	providerNatSettings := connect.DefaultRemoteUserNatProviderSettings()
	if opts.disableSecurityPolicies {
		providerNatSettings.EgressSecurityPolicyGenerator = connect.DisableSecurityPolicyWithStats
	}
	providerRemoteNat := connect.NewRemoteUserNatProvider(providerClient, providerLocalUserNat, providerNatSettings)
	go func() {
		<-ctx.Done()
		providerRemoteNat.Close()
		providerLocalUserNat.Close()
	}()

	// provide public, with return-traffic stream so the source can open both the
	// forward and companion contracts
	providerClient.ContractManager().SetProvideModesWithReturnTraffic(map[protocol.ProvideMode]bool{
		protocol.ProvideMode_Public:  true,
		protocol.ProvideMode_Network: true,
	})

	// wait for the provider's provide to register on the platform before the
	// proxy device tries to open contracts to it
	waitFor(t, 30*time.Second, "provider provide registered", func() bool {
		modes, err := model.GetProvideModes(ctx, providerClientId)
		return err == nil && len(modes) > 0
	})
	fmt.Printf("[progress]provider provide registered\n")

	// ---- the proxy device's network/device/client + balance ------------------
	pdNetworkId := server.NewId()
	pdUserId := server.NewId()
	pdNetworkName := fmt.Sprintf("proxydev-%s", pdNetworkId)
	pdDeviceId := server.NewId()
	pdClientId := server.NewId()

	model.Testing_CreateNetwork(ctx, pdNetworkId, pdNetworkName, pdUserId)
	model.Testing_CreateDevice(ctx, pdNetworkId, pdDeviceId, pdClientId, "proxydevice", "proxydevice")
	redeemBalance(t, ctx, pdNetworkId, opts.pdInitialBalance)

	// the proxy device connects "by location" pinned directly to the provider
	// client id, which the real find-providers2 path resolves.
	location := &sdk.ConnectLocation{
		ConnectLocationId: &sdk.ConnectLocationId{
			ClientId: server.ToSdkId(providerClientId),
		},
	}

	proxyDeviceConfig := &model.ProxyDeviceConfig{
		ProxyDeviceConnection: model.ProxyDeviceConnection{
			ClientId: pdClientId,
		},
		ProxyDeviceMode: model.ProxyDeviceModeDevice,
		InitialDeviceState: &model.ProxyDeviceState{
			Location: location,
		},
	}
	if err := model.CreateProxyDeviceConfig(ctx, proxyDeviceConfig); err != nil {
		t.Fatalf("create proxy device config: %v", err)
	}
	proxyId := proxyDeviceConfig.ProxyId

	// seed a few high-sequence proxy_client_ipv4 rows so CreateProxyClient(wg)
	// can allocate a client ip (avoids the 10M-row ResetProxyClientIpv4).
	seedProxyClientIpv4(t, ctx)

	proxyClient, err := model.CreateProxyClient(
		ctx,
		proxyId,
		pdClientId,
		proxyDeviceConfig.InstanceId,
		model.CreateProxyClientOptions{EnableWg: true},
	)
	if err != nil {
		t.Fatalf("create proxy client: %v", err)
	}
	signedProxyId := proxyClient.AuthToken

	// ---- the real proxy servers (socks/http/https/wg/api) --------------------
	proxySettings := DefaultProxySettings()

	transportTls := server.NewTransportTls(
		map[string]bool{},
		&server.TransportTlsSettings{EnableSelfSign: true, DefaultHostName: "127.0.0.1"},
	)

	pdmSettings := DefaultProxyDeviceManagerSettings()
	pdmSettings.NetworkSpace = networkSpace
	if opts.disableSecurityPolicies {
		pdmSettings.IngressSecurityPolicyGenerator = connect.DisableSecurityPolicyWithStats
		pdmSettings.EgressSecurityPolicyGenerator = connect.DisableSecurityPolicyWithStats
	}
	proxyDeviceManager := NewProxyDeviceManager(ctx, pdmSettings)
	go func() {
		<-ctx.Done()
		proxyDeviceManager.Close()
	}()

	NewSocks5Server(ctx, cancel, proxyDeviceManager, transportTls, proxySettings)
	NewHttpServer(ctx, cancel, proxyDeviceManager, transportTls, proxySettings)
	wgCtx, wgCancel := context.WithCancel(ctx)
	wg := NewWgServer(wgCtx, wgCancel, proxyDeviceManager, proxySettings)
	NewApiServer(ctx, cancel, proxyDeviceManager, transportTls, nil, InternalApiPort, proxySettings)

	// give the proxy listeners a moment to bind
	select {
	case <-time.After(1 * time.Second):
	}

	// register the wg client with the wg server (in production this is driven by
	// the proxy client notification / warmup callback)
	if err := wg.AddProxyClients(proxyClient); err != nil {
		t.Fatalf("wg add proxy client: %v", err)
	}

	// warm up the proxy device: open it and wait until it has a usable path to
	// the provider before we start driving traffic through it.
	pd, err := proxyDeviceManager.OpenProxyDevice(proxyId)
	if err != nil {
		t.Fatalf("open proxy device: %v", err)
	}
	if ready := pd.WaitForReady(ctx, 60*time.Second); !ready {
		t.Fatalf("proxy device did not become ready (provider not reachable)")
	}
	fmt.Printf("[progress]proxy device ready\n")

	return &proxyTestHarness{
		ctx:                ctx,
		cancel:             cancel,
		signedProxyId:      signedProxyId,
		proxyClient:        proxyClient,
		proxySettings:      proxySettings,
		proxyDeviceManager: proxyDeviceManager,
		wg:                 wg,
		wgCancel:           wgCancel,
		pdNetworkId:        pdNetworkId,
		pdClientId:         pdClientId,
		providerNetworkId:  providerNetworkId,
		providerClientId:   providerClientId,
		proxyId:            proxyId,
	}
}

func redeemBalance(t testing.TB, ctx context.Context, networkId server.Id, initialTransferBalance model.ByteCount) {
	balanceCode, err := model.CreateBalanceCode(
		ctx,
		initialTransferBalance,
		365*24*time.Hour,
		0,
		// unique per call so a network can be topped up repeatedly
		fmt.Sprintf("test-%s-%s", networkId, server.NewId()),
		"",
		"",
	)
	if err != nil {
		t.Fatalf("create balance code: %v", err)
	}
	result, err := model.RedeemBalanceCode(
		&model.RedeemBalanceCodeArgs{
			Secret:    balanceCode.Secret,
			NetworkId: networkId,
		},
		ctx,
	)
	if err != nil {
		t.Fatalf("redeem balance code: %v", err)
	}
	if result.Error != nil {
		t.Fatalf("redeem balance code: %v", result.Error.Message)
	}
}

// seedProxyClientIpv4 inserts a small block of high-sequence rows. CreateProxyClient
// selects a row with sequence_id >= rand(0, ~31/32*ProxyClientIpv4Count), so the
// rows must sit near the top of the sequence space.
func seedProxyClientIpv4(t testing.TB, ctx context.Context) {
	server.Tx(ctx, func(tx server.PgTx) {
		base := int64(model.ProxyClientIpv4Count - 64)
		for i := 0; i < 64; i += 1 {
			seq := base + int64(i)
			// arbitrary distinct ipv4 (10.b.c.d), avoiding 10.0.0.0
			ipv4 := int64(0x0A000000) + seq + 1
			server.RaisePgResult(tx.Exec(
				ctx,
				`
				INSERT INTO proxy_client_ipv4 (sequence_id, client_ipv4)
				VALUES ($1, $2)
				`,
				seq,
				ipv4,
			))
		}
	})
}

func TestProxy(t *testing.T) {
	if testing.Short() {
		return
	}
	// The proxy servers bind fixed ports (8080-8084), which cannot be rebound on
	// a rerun in the same process, so disable reruns and set up exactly once.
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		fmt.Printf("[progress]start TestProxy\n")
		h := setupProxyTest(t)
		defer h.cancel()

		// run the full leg sequence repeatedly within one setup to flush out
		// intermittent failures (e.g. return-path / contract races). The shared
		// proxy device re-claims its mode on each call — tun via DialContext,
		// wg via activateClient — so the modes can interleave within an iteration.
		for i := 0; i < testProxyIterations; i += 1 {
			fmt.Printf("[progress]iteration %d/%d\n", i+1, testProxyIterations)
			testProxyHttp(t, h)
			testProxySocks(t, h)
			testProxyHttps(t, h)
			testProxyWireguard(t, h)
		}
	})
}

// TestProxyWgRestartReconnect simulates a proxy instance restart for the wg
// path. The wg server is stopped (losing all in-memory wireguard state:
// sessions, peer table, learned endpoints) and a new one is started on the
// same port with the same server key, with the peers restored the way the
// startup full sync does. The SAME wireguard client — still holding the now
// dead session, never reconfigured or restarted — must detect the dead session
// through its own timers, re-handshake, and resume traffic.
func TestProxyWgRestartReconnect(t *testing.T) {
	if testing.Short() {
		return
	}
	// see TestProxy: fixed ports cannot be rebound on a rerun in the same process
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		fmt.Printf("[progress]start TestProxyWgRestartReconnect\n")
		h := setupProxyTest(t)
		defer h.cancel()

		// a long-lived wg client + netstack that survive the server restart
		wgCtx, wgCancel := context.WithCancel(h.ctx)
		defer wgCancel()
		transport, closeWgClient := startWgClient(t, wgCtx, h.proxyClient.WgConfig)
		defer closeWgClient()

		// establish the session and confirm traffic
		requireProxyGet(t, "wg-before-restart", transport)

		// stop the wg server, releasing its socket and all in-memory state
		fmt.Printf("[progress]restarting wg server\n")
		h.wgCancel()
		waitFor(t, 15*time.Second, "wg port release", func() bool {
			pc, err := net.ListenPacket("udp4", fmt.Sprintf("0.0.0.0:%d", InternalWgPort))
			if err != nil {
				return false
			}
			pc.Close()
			return true
		})
		// drop pooled tcp connections through the dead tunnel so the first
		// post-restart attempt dials fresh (the dial's syns are the client
		// traffic that triggers the wg re-handshake)
		transport.CloseIdleConnections()

		// new wg server: same vault key, same port, empty peer table
		wg2Ctx, wg2Cancel := context.WithCancel(h.ctx)
		defer wg2Cancel()
		wg2 := NewWgServer(wg2Ctx, wg2Cancel, h.proxyDeviceManager, h.proxySettings)

		// restore the peers the way the notification startup full sync does
		if err := wg2.SyncProxyClients([]*model.ProxyClient{h.proxyClient}, server.NowUtc()); err != nil {
			t.Fatalf("wg: restore clients after restart: %v", err)
		}

		// the client's first sends go into the dead session; its new-handshake
		// timer (~15s of unanswered data) then re-initiates, the restored peer
		// answers, and traffic resumes
		restartTime := time.Now()
		requireProxyGet(t, "wg-after-restart", transport)
		fmt.Printf("[progress]wg reconnected %v after restart\n", time.Since(restartTime).Round(time.Second))
	})
}

// TestProxyIdleDeviceRecreate exercises the idle device lifecycle in the device
// manager: a proxy id whose device goes idle (canceled via CancelIfIdle) is
// cleaned up (Run() exits on the canceled tun ctx, the cleanup closes the
// DeviceLocal and removes the proxy id from the manager), and a subsequent
// request for the same proxy id must create a brand new, working device rather
// than hand back the dead one.
func TestProxyIdleDeviceRecreate(t *testing.T) {
	if testing.Short() {
		return
	}
	// see TestProxy: fixed ports cannot be rebound on a rerun in the same process
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		fmt.Printf("[progress]start TestProxyIdleDeviceRecreate\n")
		h := setupProxyTest(t)
		defer h.cancel()

		// the device opened during setup, reachable for the proxy id
		pd1, err := h.proxyDeviceManager.OpenProxyDevice(h.proxyId)
		if err != nil {
			t.Fatalf("open proxy device: %v", err)
		}
		// confirm the device serves https before we idle it
		testProxyHttps(t, h)

		// ---- idle the device via CancelIfIdle ----
		// push last activity past the idle timeout (under the state lock, the same
		// way UpdateActivity/CancelIfIdle touch it) so CancelIfIdle cancels the
		// device, exactly as the manager's background idle checker would.
		func() {
			pd1.stateLock.Lock()
			defer pd1.stateLock.Unlock()
			pd1.lastActivityTime = pd1.lastActivityTime.Add(-2 * pd1.settings.ProxyDeviceIdleTimeout)
		}()
		if !pd1.CancelIfIdle() {
			t.Fatalf("CancelIfIdle did not cancel the idle device")
		}

		// ---- wait for the idle device to be cleaned up ----
		// Run() exits on the canceled tun ctx; the cleanup closes the DeviceLocal
		// and removes the proxy id from the manager.
		waitFor(t, 15*time.Second, "idle device removed from manager", func() bool {
			h.proxyDeviceManager.stateLock.Lock()
			defer h.proxyDeviceManager.stateLock.Unlock()
			_, ok := h.proxyDeviceManager.proxyDevices[h.proxyId]
			return !ok
		})
		// the cleanup closes the DeviceLocal before removing the map entry, so by
		// now the old device is fully torn down
		if !pd1.deviceLocal.GetDone() {
			t.Fatalf("idle device's DeviceLocal was not closed by cleanup")
		}

		// ---- a new request must create a new, working device ----
		pd2, err := h.proxyDeviceManager.OpenProxyDevice(h.proxyId)
		if err != nil {
			t.Fatalf("re-open proxy device after idle cleanup: %v", err)
		}
		if pd2 == pd1 {
			t.Fatalf("expected a new proxy device after idle cleanup, got the dead instance back")
		}
		if pd2.deviceLocal.GetDone() {
			t.Fatalf("recreated proxy device is already closed")
		}
		if ready := pd2.WaitForReady(h.ctx, 60*time.Second); !ready {
			t.Fatalf("recreated proxy device did not become ready")
		}
		// and it must actually serve traffic again
		testProxyHttps(t, h)
		fmt.Printf("[progress]idle device recreated and serving\n")
	})
}

// TestProxyDeadDeviceRecreate covers the case the idle path does not: the
// device's egress dies while its context stays live. In production this is the
// resident moving / the connection idling out and the egress window collapsing —
// none of which cancel the proxy device context, so UpdateActivity (a context-only
// check) keeps reporting the device active. OpenProxyDevice must instead notice
// the device can no longer serve and recreate it.
//
// The egress death is simulated by closing the DeviceLocal directly, which leaves
// the proxy device context live but collapses the egress window
// (GetWindowStatus().MinSatisfied == false) — the same observable a real resident
// move / idle collapse produces.
func TestProxyDeadDeviceRecreate(t *testing.T) {
	if testing.Short() {
		return
	}
	// see TestProxy: fixed ports cannot be rebound on a rerun in the same process
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		fmt.Printf("[progress]start TestProxyDeadDeviceRecreate\n")
		h := setupProxyTest(t)
		defer h.cancel()

		pd1, err := h.proxyDeviceManager.OpenProxyDevice(h.proxyId)
		if err != nil {
			t.Fatalf("open proxy device: %v", err)
		}
		if ready := pd1.WaitForReady(h.ctx, 60*time.Second); !ready {
			t.Fatalf("proxy device did not become ready")
		}
		// confirm it serves https, which also marks it everReady via the reuse gate
		testProxyHttps(t, h)

		// ---- kill the egress without canceling the device context ----
		pd1.deviceLocal.Close()

		// precondition: the old context-only check still reports the dead device
		// as active (this is the bug), but its egress window is gone
		if !pd1.UpdateActivity() {
			t.Fatalf("precondition: expected pd1 context still live after deviceLocal.Close()")
		}
		if pd1.deviceLocal.GetWindowStatus().MinSatisfied {
			t.Fatalf("precondition: expected egress window collapsed after deviceLocal.Close()")
		}

		// ---- a new request must recreate the device, not hand back the dead one ----
		pd2, err := h.proxyDeviceManager.OpenProxyDevice(h.proxyId)
		if err != nil {
			t.Fatalf("re-open proxy device after egress died: %v", err)
		}
		if pd2 == pd1 {
			t.Fatalf("expected a new proxy device after the egress died, got the dead instance back")
		}
		if ready := pd2.WaitForReady(h.ctx, 60*time.Second); !ready {
			t.Fatalf("recreated proxy device did not become ready")
		}
		// and it must actually serve traffic again
		testProxyHttps(t, h)
		fmt.Printf("[progress]dead device recreated and serving\n")
	})
}

func waitFor(t testing.TB, timeout time.Duration, desc string, cond func() bool) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		select {
		case <-time.After(500 * time.Millisecond):
		}
	}
	t.Fatalf("timed out waiting for %s", desc)
}

// testProxyHttp drives the http proxy (CONNECT to an https target).
func testProxyHttp(t testing.TB, h *proxyTestHarness) {
	fmt.Printf("[progress]http leg\n")
	proxyUrl, err := url.Parse(fmt.Sprintf("http://%s:x@127.0.0.1:%d", h.signedProxyId, InternalHttpPort))
	if err != nil {
		t.Fatalf("http: parse proxy url: %v", err)
	}
	transport := &http.Transport{
		Proxy: http.ProxyURL(proxyUrl),
	}
	requireProxyGet(t, "http", transport)
}

// testProxyHttps drives the https proxy (the proxy connection itself is TLS).
func testProxyHttps(t testing.TB, h *proxyTestHarness) {
	fmt.Printf("[progress]https leg\n")
	proxyUrl, err := url.Parse(fmt.Sprintf("https://%s:x@127.0.0.1:%d", h.signedProxyId, InternalHttpsPort))
	if err != nil {
		t.Fatalf("https: parse proxy url: %v", err)
	}
	transport := &http.Transport{
		Proxy: http.ProxyURL(proxyUrl),
		// the proxy presents a self-signed cert; this also relaxes the target's
		// tls (acceptable for the https-proxy leg).
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
	}
	requireProxyGet(t, "https", transport)
}

// testProxySocks drives the socks5 proxy.
func testProxySocks(t testing.TB, h *proxyTestHarness) {
	fmt.Printf("[progress]socks leg\n")
	dialer, err := xproxy.SOCKS5(
		"tcp",
		fmt.Sprintf("127.0.0.1:%d", InternalSocksPort),
		&xproxy.Auth{User: h.signedProxyId, Password: "x"},
		xproxy.Direct,
	)
	if err != nil {
		t.Fatalf("socks: dialer: %v", err)
	}
	contextDialer, ok := dialer.(xproxy.ContextDialer)
	if !ok {
		t.Fatalf("socks: dialer is not a ContextDialer")
	}
	transport := &http.Transport{
		DialContext: contextDialer.DialContext,
	}
	requireProxyGet(t, "socks", transport)
}

// testProxyWireguard drives the wg server with an in-process userspace
// WireGuard client and an HTTP GET routed over the tunnel.
func testProxyWireguard(t testing.TB, h *proxyTestHarness) {
	fmt.Printf("[progress]wireguard leg\n")

	// per-call ctx so the netstack bridge goroutines are torn down when this leg
	// returns (important when the leg sequence runs in a loop).
	wgCtx, wgCancel := context.WithCancel(h.ctx)
	defer wgCancel()

	transport, closeWgClient := startWgClient(t, wgCtx, h.proxyClient.WgConfig)
	// close synchronously at leg end: each leg connects as the same peer (same
	// key/ip), and a lingering previous client device would flap the server
	// peer's endpoint into the next leg
	defer closeWgClient()
	requireProxyGet(t, "wireguard", transport)
}

// startWgClient brings up an in-process userspace WireGuard client and a
// netstack bound to the client's tunnel address, returning an http transport
// that dials through the tunnel and a close function for the client device
// (also invoked if ctx is canceled first).
func startWgClient(t testing.TB, ctx context.Context, wgConfig *model.WgConfig) (*http.Transport, func()) {
	if wgConfig == nil {
		t.Fatalf("wg: proxy client has no wg config")
	}

	clientPrivate, err := wgtypes.ParseKey(wgConfig.ClientPrivateKey)
	if err != nil {
		t.Fatalf("wg: parse client private key: %v", err)
	}
	serverPublic, err := wgtypes.ParseKey(wgConfig.ProxyPublicKey)
	if err != nil {
		t.Fatalf("wg: parse server public key: %v", err)
	}

	mtu := 1420

	clientTun := tuntest.NewChannelTUN()
	clientDevice := uwgdevice.NewDevice(
		clientTun.TUN(),
		conn.NewDefaultBind(),
		logger.NewLogger(logger.LogLevelError, "wgclient: "),
	)
	var closeOnce sync.Once
	closeWgClient := func() {
		closeOnce.Do(clientDevice.Close)
	}
	go func() {
		<-ctx.Done()
		closeWgClient()
	}()

	zeroPort := 0
	wgClientConfig := wgtypes.Config{
		PrivateKey:   &clientPrivate,
		ListenPort:   &zeroPort,
		ReplacePeers: true,
		Peers: []wgtypes.PeerConfig{
			{
				PublicKey: serverPublic,
				Endpoint: &net.UDPAddr{
					IP:   net.IPv4(127, 0, 0, 1),
					Port: InternalWgPort,
				},
				ReplaceAllowedIPs: true,
				AllowedIPs: []net.IPNet{
					{IP: net.IPv4zero.To4(), Mask: net.CIDRMask(0, 32)},
				},
			},
		},
	}
	if err := clientDevice.IpcSet(&wgClientConfig); err != nil {
		t.Fatalf("wg: configure client device: %v", err)
	}
	if err := clientDevice.Up(); err != nil {
		t.Fatalf("wg: bring client up: %v", err)
	}

	wgStack, err := newWgClientStack(ctx, wgConfig.ClientIpv4, mtu, clientTun)
	if err != nil {
		t.Fatalf("wg: create client netstack: %v", err)
	}

	return &http.Transport{
		DialContext: wgStack.DialContext,
	}, closeWgClient
}

// requireProxyGet issues a GET to the real target through the given transport,
// retrying while the proxy device establishes its path to the provider.
func requireProxyGet(t testing.TB, leg string, transport *http.Transport) {
	client := &http.Client{
		Transport: transport,
		Timeout:   60 * time.Second,
	}
	defer client.CloseIdleConnections()

	deadline := time.Now().Add(120 * time.Second)
	var lastErr error
	for time.Now().Before(deadline) {
		req, err := http.NewRequest("GET", testTargetUrl, nil)
		if err != nil {
			t.Fatalf("%s: new request: %v", leg, err)
		}
		resp, err := client.Do(req)
		if err != nil {
			lastErr = err
			fmt.Printf("[progress][%s]retry err=%v\n", leg, err)
			select {
			case <-time.After(2 * time.Second):
			}
			continue
		}
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		resp.Body.Close()
		if resp.StatusCode < 200 || 400 <= resp.StatusCode {
			lastErr = fmt.Errorf("status %d", resp.StatusCode)
			fmt.Printf("[progress][%s]retry status=%d\n", leg, resp.StatusCode)
			select {
			case <-time.After(2 * time.Second):
			}
			continue
		}
		fmt.Printf("[progress][%s]OK status=%d bytes=%d\n", leg, resp.StatusCode, len(body))
		return
	}
	t.Fatalf("%s: could not load %s through proxy: %v", leg, testTargetUrl, lastErr)
}

// ---- in-process userspace WireGuard client netstack -------------------------

// wgClientStack is a gVisor netstack bound to the wg client's assigned ipv4,
// bridged to a userspace WireGuard device via a channel TUN. Outbound packets
// from the netstack are fed to the wg device (encrypted, sent to the wg server);
// inbound decrypted packets are injected back into the netstack.
type wgClientStack struct {
	stack *stack.Stack
	nicId tcpip.NICID
}

func newWgClientStack(ctx context.Context, clientIPv4 netip.Addr, mtu int, tunDev *tuntest.ChannelTUN) (*wgClientStack, error) {
	s := stack.New(stack.Options{
		NetworkProtocols: []stack.NetworkProtocolFactory{
			ipv4.NewProtocolWithOptions(ipv4.Options{AllowExternalLoopbackTraffic: true}),
		},
		TransportProtocols: []stack.TransportProtocolFactory{
			tcp.NewProtocol,
			udp.NewProtocol,
			icmp.NewProtocol4,
		},
		HandleLocal: false,
	})

	nicId := tcpip.NICID(1)
	ep := channel.New(512, uint32(mtu), "")

	if tcpipErr := s.CreateNIC(nicId, ep); tcpipErr != nil {
		return nil, fmt.Errorf("create nic: %v", tcpipErr)
	}
	protoAddr := tcpip.ProtocolAddress{
		Protocol:          ipv4.ProtocolNumber,
		AddressWithPrefix: tcpip.AddrFromSlice(clientIPv4.AsSlice()).WithPrefix(),
	}
	if tcpipErr := s.AddProtocolAddress(nicId, protoAddr, stack.AddressProperties{}); tcpipErr != nil {
		return nil, fmt.Errorf("add address: %v", tcpipErr)
	}
	s.AddRoute(tcpip.Route{Destination: header.IPv4EmptySubnet, NIC: nicId})

	// netstack -> wg device (app sends): drain ep, write to the tun outbound.
	go func() {
		for {
			pkt := ep.ReadContext(ctx)
			if pkt == nil {
				return
			}
			b := append([]byte(nil), pkt.ToView().AsSlice()...)
			pkt.DecRef()
			select {
			case tunDev.Outbound <- b:
			case <-ctx.Done():
				return
			}
		}
	}()

	// wg device -> netstack (app receives): inject decrypted packets.
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case p := <-tunDev.Inbound:
				pkb := stack.NewPacketBuffer(stack.PacketBufferOptions{
					Payload: buffer.MakeWithData(p),
				})
				ep.InjectInbound(header.IPv4ProtocolNumber, pkb)
				pkb.DecRef()
			}
		}
	}()

	return &wgClientStack{stack: s, nicId: nicId}, nil
}

func (self *wgClientStack) DialContext(ctx context.Context, network string, address string) (net.Conn, error) {
	host, portStr, err := net.SplitHostPort(address)
	if err != nil {
		return nil, err
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		return nil, err
	}

	ip := net.ParseIP(host)
	if ip == nil {
		// the netstack does not resolve dns; resolve the target ipv4 locally and
		// dial it over the tunnel (the provider egresses to it).
		ips, err := net.DefaultResolver.LookupIP(ctx, "ip4", host)
		if err != nil {
			return nil, err
		}
		if len(ips) == 0 {
			return nil, fmt.Errorf("no ipv4 for %s", host)
		}
		ip = ips[0]
	}
	ip4 := ip.To4()
	if ip4 == nil {
		return nil, fmt.Errorf("not an ipv4 address: %s", ip)
	}

	fa := tcpip.FullAddress{
		NIC:  self.nicId,
		Addr: tcpip.AddrFromSlice(ip4),
		Port: uint16(port),
	}
	return gonet.DialContextTCP(ctx, self.stack, fa, ipv4.ProtocolNumber)
}
