package connect

import (
	"context"
	"encoding/hex"
	"fmt"
	"net/http"
	"os"
	"runtime"
	"runtime/debug"
	"testing"
	"time"

	mathrand "math/rand"

	"github.com/urnetwork/connect/v2026"
	"github.com/urnetwork/connect/v2026/protocol"
	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/jwt"
	"github.com/urnetwork/server/v2026/model"
	"github.com/urnetwork/server/v2026/router"
)

// TestExchangeRelayPoolBalance drives real client-to-client traffic through a real
// exchange (the server relay: transport -> resident -> forward -> resident ->
// transport) and asserts that after teardown every connect message-pool buffer is
// returned and the relay's goroutines return to baseline.
//
// This is the server-side counterpart to the connect library's
// TestMultiClientLifecyclePoolBalance / TestClientClosePoolBalance. The relay's
// forward path (resident.handleClientForward) hands each frame off across goroutines
// and the exchange connection, so a lost MessagePoolReturn there degrades the
// process-wide pool reuse under sustained relaying -- invisible to heap checks
// because the GC collects the orphaned buffers. taken-returned returning to baseline
// after a load+teardown cycle is the signal.
//
// Requires the test DB env (WARP_ENV=local + postgres/redis/vault), like the rest of
// this package; skipped under -short.
func TestExchangeRelayPoolBalance(t *testing.T) {
	if testing.Short() {
		return
	}
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		testExchangeRelayPoolBalance(t)
	})
}

func testExchangeRelayPoolBalance(t testing.TB) {
	os.Setenv("WARP_SERVICE", "test")
	os.Setenv("WARP_BLOCK", "test")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	const (
		service = "connect"
		block   = "test"
		host    = "host0"
		port    = 8080

		messageContentSize = ByteCount(1024)
		warmupMessages     = 8
		loadMessages       = 200

		goroutineBaselineTolerance = 12
		poolTolerance              = 16
	)

	framerMaxMessageLen := max(
		2*int(messageContentSize),
		int(connect.DefaultClientSettings().MinimumMessageLenLimit()),
	)

	routes := map[string]string{host: "127.0.0.1"}

	// --- exchange + connect handler + http server ---
	exchangeSettings := DefaultExchangeSettings()
	exchangeSettings.ExchangeBufferSize = 0
	exchangeSettings.ForwardBufferSize = 0
	exchangeSettings.ForwardTimeout = exchangeSettings.WriteTimeout // blocking forward: no silent drops
	exchangeSettings.ExchangeResidentTtl = 5 * time.Second
	exchangeSettings.ForwardIdleTimeout = 5 * time.Second
	exchangeSettings.FramerSettings.MaxMessageLen = framerMaxMessageLen
	exchangeSettings.ForwardEnforceActiveContracts = false

	hostToServicePorts := map[int]int{9000: 9000}
	exchange := NewExchange(ctx, host, service, block, hostToServicePorts, routes, exchangeSettings)

	handlerSettings := DefaultConnectHandlerSettings()
	handlerSettings.ListenH3Port = port + 443
	handlerSettings.ListenDnsPort = port + 53
	handlerSettings.EnableProxyProtocol = false
	handlerSettings.FramerSettings.MaxMessageLen = framerMaxMessageLen
	handlerSettings.TransportTlsSettings.EnableSelfSign = true
	handlerSettings.TransportTlsSettings.DefaultHostName = "127.0.0.1"
	handlerSettings.ConnectionAnnounceTimeout = 0
	handlerSettings.ConnectionRateLimitSettings.BurstConnectionCount = 1000
	connectHandler := NewConnectHandler(ctx, server.NewId(), exchange, handlerSettings)

	httpRoutes := []*router.Route{
		router.NewRoute("GET", "/status", router.WarpStatus),
		router.NewRoute("GET", "/", connectHandler.Connect),
	}
	httpServer := &http.Server{
		Addr:    fmt.Sprintf(":%d", port),
		Handler: router.NewRouter(ctx, httpRoutes),
	}
	go httpServer.ListenAndServe()
	select {
	case <-time.After(2 * time.Second):
	}

	// --- two clients, no-contract peers (avoids balance-code / provide-mode setup;
	// the relay forward path is exercised the same way) ---
	newClientSettings := func() *connect.ClientSettings {
		s := connect.DefaultClientSettings()
		s.SendBufferSettings.SequenceBufferSize = 0
		s.SendBufferSettings.AckBufferSize = 0
		s.ReceiveBufferSettings.SequenceBufferSize = 0
		s.ReceiveBufferSettings.AckCompressTimeout = 0
		s.ForwardBufferSettings.SequenceBufferSize = 0
		s.ContractManagerSettings = connect.DefaultContractManagerSettingsNoNetworkEvents()
		s.ControlPingTimeout = 30 * time.Second
		return s
	}

	clientIdA := server.NewId()
	clientIdB := server.NewId()
	clientAInstanceId := server.NewId()
	clientBInstanceId := server.NewId()

	clientSettingsA := newClientSettings()
	clientSettingsB := newClientSettings()

	clientA := connect.NewClient(ctx, connect.Id(clientIdA), Testing_NewControllerOutOfBandControl(ctx, clientIdA, clientSettingsA.ContractManagerSettings), clientSettingsA)
	clientB := connect.NewClient(ctx, connect.Id(clientIdB), Testing_NewControllerOutOfBandControl(ctx, clientIdB, clientSettingsB.ContractManagerSettings), clientSettingsB)

	// networks + devices so the transport auth validates
	networkIdA := server.NewId()
	userIdA := server.NewId()
	deviceIdA := server.NewId()
	model.Testing_CreateNetwork(ctx, networkIdA, "poolBalanceA", userIdA)
	model.Testing_CreateDevice(ctx, networkIdA, deviceIdA, clientIdA, "a", "a")

	networkIdB := server.NewId()
	userIdB := server.NewId()
	deviceIdB := server.NewId()
	model.Testing_CreateNetwork(ctx, networkIdB, "poolBalanceB", userIdB)
	model.Testing_CreateDevice(ctx, networkIdB, deviceIdB, clientIdB, "b", "b")

	receiveA := make(chan struct{}, 1<<16)
	receiveB := make(chan struct{}, 1<<16)
	clientA.AddReceiveCallback(func(source connect.TransferPath, frames []*protocol.Frame, provideMode protocol.ProvideMode) {
		for range frames {
			receiveA <- struct{}{}
		}
	})
	clientB.AddReceiveCallback(func(source connect.TransferPath, frames []*protocol.Frame, provideMode protocol.ProvideMode) {
		for range frames {
			receiveB <- struct{}{}
		}
	})

	clientA.ContractManager().AddNoContractPeer(connect.Id(clientIdB))
	clientB.ContractManager().AddNoContractPeer(connect.Id(clientIdA))

	newTransport := func(clientStrategy *connect.ClientStrategy, routeManager *connect.RouteManager, networkId server.Id, userId server.Id, deviceId server.Id, clientId server.Id, instanceId server.Id) *connect.PlatformTransport {
		byJwt := jwt.NewByJwt(networkId, userId, "poolBalance", false, false).Client(deviceId, clientId)
		auth := &connect.ClientAuth{
			ByJwt:      byJwt.Sign(),
			InstanceId: connect.Id(instanceId),
			AppVersion: "0.0.0",
		}
		settings := connect.DefaultPlatformTransportSettings()
		settings.QuicTlsConfig.InsecureSkipVerify = true
		settings.H3Port = port + 443
		settings.DnsPort = port + 53
		settings.FramerSettings.MaxMessageLen = framerMaxMessageLen
		return connect.NewPlatformTransportWithTargetMode(
			ctx,
			clientStrategy,
			routeManager,
			fmt.Sprintf("ws://127.0.0.1:%d", port),
			auth,
			connect.TransportModeH1,
			settings,
		)
	}

	clientStrategyA := connect.NewClientStrategyWithDefaults(ctx)
	clientStrategyB := connect.NewClientStrategyWithDefaults(ctx)
	transportA := newTransport(clientStrategyA, clientA.RouteManager(), networkIdA, userIdA, deviceIdA, clientIdA, clientAInstanceId)
	transportB := newTransport(clientStrategyB, clientB.RouteManager(), networkIdB, userIdB, deviceIdB, clientIdB, clientBInstanceId)

	messageContentBytes := make([]byte, messageContentSize/2)
	mathrand.Read(messageContentBytes)
	messageContent := hex.EncodeToString(messageContentBytes)

	// send one message and wait for its receipt (round-trips the relay)
	sendAndWait := func(from *connect.Client, toClientId server.Id, recv chan struct{}) bool {
		frame, err := connect.ToFrame(&protocol.SimpleMessage{Content: messageContent}, connect.DefaultProtocolVersion)
		if err != nil {
			t.Fatalf("to frame: %v", err)
		}
		if !from.SendWithTimeout(frame, connect.DestinationId(connect.Id(toClientId)), func(err error) {}, 30*time.Second) {
			return false
		}
		select {
		case <-recv:
			return true
		case <-time.After(30 * time.Second):
			return false
		}
	}

	// warmup: establish the residents + forward path + reach pool steady state
	for i := 0; i < warmupMessages; i += 1 {
		sendAndWait(clientA, clientIdB, receiveB)
		sendAndWait(clientB, clientIdA, receiveA)
	}

	baseGoroutines, basePool := relaySampleStable()
	t.Logf("baseline: goroutines=%d pool-outstanding=%d", baseGoroutines, basePool)

	// load: relay many messages both directions
	delivered := 0
	for i := 0; i < loadMessages; i += 1 {
		if sendAndWait(clientA, clientIdB, receiveB) {
			delivered += 1
		}
		if sendAndWait(clientB, clientIdA, receiveA) {
			delivered += 1
		}
	}
	t.Logf("relayed %d/%d messages", delivered, loadMessages*2)
	if delivered == 0 {
		t.Fatal("no messages were relayed; the exchange relay did not work")
	}

	// --- teardown, then assert return-to-baseline ---
	clientA.Flush()
	clientB.Flush()
	select {
	case <-time.After(2 * time.Second):
	}
	clientA.Close()
	clientB.Close()
	transportA.Close()
	transportB.Close()
	httpServer.Close()
	exchange.Close()
	cancel()

	finalGoroutines, finalPool := relaySampleStable()
	t.Logf("post-teardown: goroutines=%d pool-outstanding=%d", finalGoroutines, finalPool)

	if finalPool > basePool+poolTolerance {
		t.Errorf("message pool buffers not returned after relay teardown: outstanding %d -> %d (+%d, tol +%d)",
			basePool, finalPool, finalPool-basePool, poolTolerance)
	}
	if finalGoroutines > baseGoroutines+goroutineBaselineTolerance {
		t.Errorf("goroutines did not return to baseline after relay teardown: %d -> %d (tol +%d)",
			baseGoroutines, finalGoroutines, goroutineBaselineTolerance)
	}
}

// relaySampleStable forces a collection, waits for the goroutine count to settle,
// and returns the settled goroutine count and the outstanding message-pool buffer
// count (taken - returned). Outstanding buffers are returned asynchronously as
// sequences/goroutines wind down after close, so it polls to settle rather than
// sleeping a fixed duration.
func relaySampleStable() (goroutines int, poolOutstanding int64) {
	outstanding := func() int64 {
		taken, returned, _ := connect.MessagePoolCounts()
		return int64(taken) - int64(returned)
	}

	const (
		settleInterval = 100 * time.Millisecond
		settleChecks   = 6
		settleTimeout  = 30 * time.Second
	)

	runtime.GC()
	debug.FreeOSMemory()

	deadline := time.Now().Add(settleTimeout)
	prevG := runtime.NumGoroutine()
	prevP := outstanding()
	stable := 0
	for {
		select {
		case <-time.After(settleInterval):
		}
		runtime.GC()
		g := runtime.NumGoroutine()
		p := outstanding()
		if g == prevG && p == prevP {
			stable += 1
			if stable >= settleChecks {
				break
			}
		} else {
			stable = 0
			prevG = g
			prevP = p
		}
		if time.Now().After(deadline) {
			break
		}
	}

	runtime.GC()
	debug.FreeOSMemory()
	return runtime.NumGoroutine(), outstanding()
}
