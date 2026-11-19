package connect_test

// Performance test for the connect server data path, mirroring connect_test in
// setup but tuned for measurement instead of correctness chaos. It stands up,
// in-process:
//
//   - two real exchange hosts ("perf0", "perf1"), each with its own connect
//     handler on a plain ws port, so the A->B forward crosses a real exchange
//     connection between hosts (loopback TCP, same code path as production)
//   - a real api server (the full api.Routes()) used by the clients for
//     out-of-band control (contract setup) via connect.NewApiOutOfBandControl
//   - two real connect.Clients with production-default settings, one platform
//     transport each (H1+, the urnetwork-framer/1 upgrade, pinned, and checked
//     after the first message), client A homed on perf0 and B on perf1
//
// Phases, each reported with "[perf]" lines:
//
//   1. first-message: cold-path time from send to delivered echo (resident
//      nomination, contract bootstrap)
//   2. ping-pong: app-level round trip A->B->A and send->ack round trip,
//      percentiles over a steady run. Note the ack round trip includes the
//      receiver's ack coalescing window (ReceiveBufferSettings.
//      AckCompressTimeout), so it reads higher than the echo round trip by
//      design; echo rtt is the user-visible latency.
//   3. one-way throughput at several message sizes, acked: goodput, message
//      rate, ack latency percentiles, drain time of the in-flight backlog
//   4. one-way throughput with NoAck: isolates the ack-path cost and reports
//      delivery drop rate (nack messages are droppable by design)
//   5. bidirectional throughput: both directions at once
//
// A CPU profile is written per throughput phase, and mutex/block profiles are
// captured during the 1 KiB one-way phase, all under profile/.
//
// The carrier cases at the end of the file (TestConnectPerformanceCarriers*)
// measure each of connect's edge carriers on its own: the websocket and H1+
// pinned apart, raw QUIC and whodis, directly, through connect's lb_alt
// front, and through warp/lb's nginx build as the baseline (LBALT.md §9).
//
// Run with the standard local test env (see test.sh), without -race for
// meaningful numbers:
//
//	WARP_ENV=local WARP_SERVICE=test WARP_DOMAIN=bringyour.com \
//	WARP_BLOCK=test WARP_VERSION=0.0.0 \
//	BRINGYOUR_POSTGRES_HOSTNAME=local-pg.bringyour.com \
//	BRINGYOUR_REDIS_HOSTNAME=local-redis.bringyour.com \
//	go test -run TestConnectPerformance -count 1 -timeout 30m \
//	  -args -v 0 -logtostderr true

import (
	"context"
	cryptorand "crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"runtime/pprof"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/connect/protocol"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/api"
	connectserver "github.com/urnetwork/server/connect"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/router"
	"github.com/urnetwork/server/session"
)

const (
	perfPingPongCount  = 400
	perfPingPongWarmup = 20
	perfBlastDuration  = 6 * time.Second
	perfSettleTimeout  = 120 * time.Second
)

// production path: asymmetric contracts created through the real api
func TestConnectPerformance(t *testing.T) {
	perfTestEnv().Run(t, func(t testing.TB) {
		testConnectPerformance(t, true)
	})
}

// pure data plane: no contracts, forward contract enforcement off
func TestConnectPerformanceNoContract(t *testing.T) {
	perfTestEnv().Run(t, func(t testing.TB) {
		testConnectPerformance(t, false)
	})
}

func perfTestEnv() *server.TestEnv {
	// Measurement noise is ridden out inside each perf test (best-of-N runs,
	// with extra evidence runs only while a floor is unmet), so a failure here
	// is either real breakage or a cold-start/setup blip (provider registration,
	// ws bring-up, first-echo). One env-level rerun heals the blip without
	// letting repeated measurement retries mask a real floor regression.
	return &server.TestEnv{
		ApplyDbMigrations: true,
		RerunCount:        1,
	}
}

type perfReceivedMessage struct {
	sourceId   server.Id
	index      uint32
	contentLen int
}

type perfPeerStats struct {
	receiveCount   int64
	receiveBytes   int64
	lastRecvNanos  int64
	parseErrCount  int64
	otherTypeCount int64
}

func testConnectPerformance(t testing.TB, enableContracts bool) {
	if testing.Short() {
		return
	}

	modeName := "contract"
	if !enableContracts {
		modeName = "nocontract"
	}

	os.Setenv("WARP_SERVICE", "test")
	os.Setenv("WARP_BLOCK", "test")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	fmt.Printf("[perf]mode=%s go=%s cpus=%d gomaxprocs=%d\n", modeName, runtime.Version(), runtime.NumCPU(), runtime.GOMAXPROCS(0))

	// ---- servers: two exchange hosts + connect handlers, one api --------------

	service := "connect"
	block := "test"
	routes := map[string]string{
		"perf0": "127.0.0.1",
		"perf1": "127.0.0.1",
	}
	listenTcp := func() net.Listener {
		listener, err := net.Listen("tcp4", "127.0.0.1:0")
		if err != nil {
			panic(err)
		}
		return listener
	}
	wsListener0 := listenTcp()
	wsListener1 := listenTcp()
	apiListener := listenTcp()
	exchangeListener0 := listenTcp()
	exchangeListener1 := listenTcp()
	wsPort0 := wsListener0.Addr().(*net.TCPAddr).Port
	wsPort1 := wsListener1.Addr().(*net.TCPAddr).Port
	apiPort := apiListener.Addr().(*net.TCPAddr).Port

	hostConfigs := []struct {
		host             string
		wsPort           int
		exchangePort     int
		listener         net.Listener
		exchangeListener net.Listener
	}{
		{
			"perf0",
			wsPort0,
			exchangeListener0.Addr().(*net.TCPAddr).Port,
			wsListener0,
			exchangeListener0,
		},
		{
			"perf1",
			wsPort1,
			exchangeListener1.Addr().(*net.TCPAddr).Port,
			wsListener1,
			exchangeListener1,
		},
	}

	exchanges := []*connectserver.Exchange{}
	connectHandlers := []*connectserver.ConnectHandler{}
	httpServers := []*http.Server{}
	for _, hostConfig := range hostConfigs {
		settings := connectserver.DefaultExchangeSettings()
		settings.ConnectionAnnounceTimeout = 0
		settings.ConnectionRateLimitSettings.BurstConnectionCount = 1000
		settings.ForwardEnforceActiveContracts = enableContracts
		// disable the scheduled per-connection speed test. During a speed
		// test the client transport echoes all inbound messages upstream
		// instead of delivering them, which under load forces a full resend
		// window and floods the resident with mis-sourced echo frames.
		settings.ConnectionTestConfig = connectserver.V0TestConfig()
		settings.ListenH3Port = 0
		settings.ListenDnsPort = 0

		exchange := connectserver.NewExchangeWithListeners(
			ctx,
			hostConfig.host,
			service,
			block,
			map[int]int{hostConfig.exchangePort: hostConfig.exchangePort},
			routes,
			settings,
			map[int]net.Listener{hostConfig.exchangePort: hostConfig.exchangeListener},
		)
		exchanges = append(exchanges, exchange)

		connectHandler := connectserver.NewConnectHandler(ctx, server.NewId(), exchange, &settings.ConnectHandlerSettings)
		connectHandlers = append(connectHandlers, connectHandler)
		connectRoutes := []*router.Route{
			router.NewRoute("GET", "/status", router.WarpStatus),
			router.NewRoute("GET", "/", connectHandler.Connect),
		}
		httpServer := &http.Server{
			Handler: router.NewRouter(ctx, connectRoutes),
		}
		httpServers = append(httpServers, httpServer)
		go httpServer.Serve(hostConfig.listener)
	}

	apiServer := &http.Server{
		Handler: router.NewRouter(ctx, api.Routes()),
	}
	httpServers = append(httpServers, apiServer)
	go apiServer.Serve(apiListener)

	defer func() {
		for _, connectHandler := range connectHandlers {
			connectHandler.Close()
		}
		for _, httpServer := range httpServers {
			httpServer.Close()
		}
		for _, exchange := range exchanges {
			exchange.Close()
		}
		closeCtx, closeCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer closeCancel()
		for _, connectHandler := range connectHandlers {
			if !connectHandler.WaitForIdle(closeCtx) {
				panic("connect handlers did not finish during perf teardown")
			}
		}
	}()

	waitFor := func(timeout time.Duration, what string, condition func() bool) {
		deadline := time.Now().Add(timeout)
		for !condition() {
			if deadline.Before(time.Now()) {
				panic(fmt.Errorf("timeout waiting for %s", what))
			}
			select {
			case <-ctx.Done():
				panic(fmt.Errorf("done waiting for %s", what))
			case <-time.After(200 * time.Millisecond):
			}
		}
	}

	statusOk := func(port int) bool {
		statusClient := &http.Client{Timeout: 1 * time.Second}
		response, err := statusClient.Get(fmt.Sprintf("http://127.0.0.1:%d/status", port))
		if err != nil {
			return false
		}
		defer response.Body.Close()
		return response.StatusCode == 200
	}
	waitFor(15*time.Second, "ws server 0", func() bool { return statusOk(wsPort0) })
	waitFor(15*time.Second, "ws server 1", func() bool { return statusOk(wsPort1) })
	waitFor(15*time.Second, "api server", func() bool { return statusOk(apiPort) })

	// ---- networks, devices, balance -------------------------------------------

	apiUrl := fmt.Sprintf("http://127.0.0.1:%d", apiPort)

	type peer struct {
		networkId   server.Id
		networkName string
		userId      server.Id
		deviceId    server.Id
		clientId    server.Id
		instanceId  server.Id
		byJwt       string
	}

	newPeer := func(name string) *peer {
		p := &peer{
			networkId:   server.NewId(),
			networkName: fmt.Sprintf("perf-%s", name),
			userId:      server.NewId(),
			deviceId:    server.NewId(),
			clientId:    server.NewId(),
			instanceId:  server.NewId(),
		}
		model.Testing_CreateNetwork(ctx, p.networkId, p.networkName, p.userId)
		model.Testing_CreateDevice(ctx, p.networkId, p.deviceId, p.clientId, name, name)

		initialTransferBalance := model.ByteCount(1024) * model.ByteCount(1024) * model.ByteCount(1024) * model.ByteCount(1024)
		balanceCode, err := model.CreateBalanceCode(
			ctx,
			initialTransferBalance,
			365*24*time.Hour,
			0,
			fmt.Sprintf("perf-%s", name),
			"",
			"",
		)
		if err != nil {
			panic(err)
		}
		result, err := model.RedeemBalanceCode(
			&model.RedeemBalanceCodeArgs{
				Secret:    balanceCode.Secret,
				NetworkId: p.networkId,
			},
			ctx,
		)
		if err != nil {
			panic(err)
		}
		if result.Error != nil {
			panic(fmt.Errorf("redeem balance code error = %s", result.Error.Message))
		}

		p.byJwt = session.NewByJwt(
			p.networkId,
			p.userId,
			p.networkName,
			false,
			false,
		).Client(p.deviceId, p.clientId).Testing_Sign()
		return p
	}

	peerA := newPeer("a")
	peerB := newPeer("b")

	// ---- clients with the real api for out-of-band control --------------------

	newPeerClient := func(p *peer, wsPort int) (*connect.Client, *connect.PlatformTransport, *connect.H1ConnectionStats) {
		strategySettings := connect.DefaultClientStrategySettings()
		strategySettings.EnableResilient = false
		clientStrategy := connect.NewClientStrategy(ctx, strategySettings)

		oob := connect.NewApiOutOfBandControl(ctx, clientStrategy, p.byJwt, apiUrl)
		client := connect.NewClient(ctx, connect.Id(p.clientId), oob, newLocalPerformanceClientSettings())

		auth := &connect.ClientAuth{
			ByJwt:      p.byJwt,
			InstanceId: connect.Id(p.instanceId),
			AppVersion: "0.0.0",
		}
		// the phases measure H1+, pinned, which is the platform transport's
		// default. A refused upgrade falls back to the websocket unseen, so
		// the first message checks what each client negotiated. The carrier
		// cases below measure the websocket apart.
		h1Stats := &connect.H1ConnectionStats{}
		transportSettings := connect.DefaultPlatformTransportSettings()
		transportSettings.EnableH1Plus = true
		transportSettings.H1ConnectionStats = h1Stats
		transport := connect.NewPlatformTransportWithTargetMode(
			ctx,
			clientStrategy,
			client.RouteManager(),
			fmt.Sprintf("ws://127.0.0.1:%d", wsPort),
			auth,
			connect.TransportModeH1,
			transportSettings,
		)
		return client, transport, h1Stats
	}

	clientA, transportA, h1StatsA := newPeerClient(peerA, wsPort0)
	clientB, transportB, h1StatsB := newPeerClient(peerB, wsPort1)

	defer func() {
		clientA.Close()
		clientB.Close()
		transportA.Close()
		transportB.Close()
	}()

	// ---- receive plumbing ------------------------------------------------------

	statsA := &perfPeerStats{}
	statsB := &perfPeerStats{}

	// during the ping-pong phase, A's consumer forwards echoes here
	echoCollect := &atomic.Bool{}
	echoesToA := make(chan *perfReceivedMessage, 4096)

	// during the ping-pong phase, B echoes every received message back to A
	echoOn := &atomic.Bool{}
	echoContent := perfRandContent(32)

	destinationA := connect.Id(peerA.clientId)
	destinationB := connect.Id(peerB.clientId)

	send := func(client *connect.Client, destination connect.Id, frame *protocol.Frame, ackCallback func(error), opts ...any) {
		connectserver.TestingSendConnectFrame(client, frame, destination, ackCallback, opts...)
	}

	echoOpts := []any{}
	bToAOpts := []any{}
	if enableContracts {
		// b->a is the provider return direction
		echoOpts = append(echoOpts, connect.CompanionContract())
		bToAOpts = append(bToAOpts, connect.CompanionContract())
	}

	addConsumer := func(client *connect.Client, stats *perfPeerStats, onMessage func(m *perfReceivedMessage)) {
		receive := make(chan *perfReceivedMessage, 1<<16)
		client.AddReceiveCallback(func(source connect.TransferPath, frames []*protocol.Frame, peer connect.Peer) {
			for _, frame := range frames {
				m, err := connect.FromFrame(frame)
				if err != nil {
					atomic.AddInt64(&stats.parseErrCount, 1)
					continue
				}
				simpleMessage, ok := m.(*protocol.SimpleMessage)
				if !ok {
					atomic.AddInt64(&stats.otherTypeCount, 1)
					continue
				}
				select {
				case receive <- &perfReceivedMessage{
					sourceId:   server.Id(source.SourceId),
					index:      simpleMessage.MessageIndex,
					contentLen: len(simpleMessage.Content),
				}:
				default:
					panic(fmt.Errorf("receive overflow"))
				}
			}
		})
		go func() {
			for {
				select {
				case <-ctx.Done():
					return
				case m := <-receive:
					atomic.AddInt64(&stats.receiveCount, 1)
					atomic.AddInt64(&stats.receiveBytes, int64(m.contentLen))
					atomic.StoreInt64(&stats.lastRecvNanos, time.Now().UnixNano())
					if onMessage != nil {
						onMessage(m)
					}
				}
			}
		}()
	}

	addConsumer(clientA, statsA, func(m *perfReceivedMessage) {
		if echoCollect.Load() {
			select {
			case echoesToA <- m:
			default:
			}
		}
	})
	addConsumer(clientB, statsB, func(m *perfReceivedMessage) {
		if echoOn.Load() {
			frame := perfRequireFrame(m.index, 1, echoContent)
			send(clientB, destinationA, frame, func(err error) {}, echoOpts...)
		}
	})

	// ---- provide / contract setup ---------------------------------------------

	if enableContracts {
		setProvide := func(client *connect.Client, modes map[protocol.ProvideMode]bool) {
			ack := make(chan struct{})
			client.ContractManager().SetProvideModesWithReturnTrafficWithAckCallback(
				modes,
				func(err error) {
					close(ack)
				},
			)
			select {
			case <-ack:
			case <-time.After(30 * time.Second):
				panic(fmt.Errorf("timeout waiting for provide ack"))
			}
		}
		// a->b is provide, b->a is a companion (the client-provider relationship)
		setProvide(clientA, map[protocol.ProvideMode]bool{})
		setProvide(clientB, map[protocol.ProvideMode]bool{
			protocol.ProvideMode_Network: true,
			protocol.ProvideMode_Public:  true,
		})
		waitFor(30*time.Second, "provide registered", func() bool {
			modes, err := model.GetProvideModes(ctx, peerB.clientId)
			return err == nil && modes[model.ProvideModePublic]
		})
	} else {
		clientA.ContractManager().AddNoContractPeer(connect.Id(peerB.clientId))
		clientB.ContractManager().AddNoContractPeer(connect.Id(peerA.clientId))
	}

	// ---- profiling helpers -----------------------------------------------------

	profileDir := "profile"
	os.MkdirAll(profileDir, 0755)

	startCpuProfile := func(label string) func() {
		path := filepath.Join(profileDir, fmt.Sprintf("perf_%s_%s_cpu.pprof", modeName, label))
		f, err := os.Create(path)
		if err != nil {
			panic(err)
		}
		if err := pprof.StartCPUProfile(f); err != nil {
			// e.g. test invoked with -cpuprofile
			fmt.Printf("[perf]cpu profile unavailable (%s)\n", err)
			f.Close()
			return func() {}
		}
		return func() {
			pprof.StopCPUProfile()
			f.Close()
		}
	}

	writeProfile := func(name string, label string) {
		p := pprof.Lookup(name)
		if p == nil {
			return
		}
		path := filepath.Join(profileDir, fmt.Sprintf("perf_%s_%s_%s.pprof", modeName, label, name))
		f, err := os.Create(path)
		if err != nil {
			panic(err)
		}
		defer f.Close()
		p.WriteTo(f, 0)
	}

	// ---- phase 1: first message (cold path) ------------------------------------

	echoOn.Store(true)
	echoCollect.Store(true)

	pingPong := func(index uint32, content string, ackRtts *[]time.Duration, ackLock *sync.Mutex) time.Duration {
		startTime := time.Now()
		frame := perfRequireFrame(index, 1, content)
		send(clientA, destinationB, frame, func(err error) {
			if err != nil {
				return
			}
			if ackRtts != nil {
				d := time.Since(startTime)
				ackLock.Lock()
				*ackRtts = append(*ackRtts, d)
				ackLock.Unlock()
			}
		})
		for {
			select {
			case m := <-echoesToA:
				if m.index == index {
					return time.Since(startTime)
				}
				// stale echo from a previous iteration; keep waiting
			case <-time.After(120 * time.Second):
				panic(fmt.Errorf("timeout waiting for echo %d", index))
			}
		}
	}

	pingContent := perfRandContent(32)
	firstRtt := pingPong(0, pingContent, nil, nil)
	fmt.Printf("[perf]first message rtt = %s (includes contract bootstrap)\n", firstRtt)
	// each client carries the phases on the H1+ they pin, not the websocket
	for _, h1Stats := range []*connect.H1ConnectionStats{h1StatsA, h1StatsB} {
		if h1Snapshot := h1Stats.Snapshot(); h1Snapshot.H1PlusConnectionCount != 1 || h1Snapshot.WebSocketConnectionCount != 0 {
			t.Fatalf("[perf]a client negotiated %+v, want one H1+ connection", h1Snapshot)
		}
	}

	// ---- phase 2: ping-pong latency --------------------------------------------

	rtts := make([]time.Duration, 0, perfPingPongCount)
	ackRtts := make([]time.Duration, 0, perfPingPongCount)
	var ackLock sync.Mutex
	for i := 1; i < 1+perfPingPongWarmup+perfPingPongCount; i += 1 {
		rtt := pingPong(uint32(i), pingContent, &ackRtts, &ackLock)
		if perfPingPongWarmup < i {
			rtts = append(rtts, rtt)
		}
	}
	echoOn.Store(false)
	echoCollect.Store(false)
	perfDrain(echoesToA)

	fmt.Printf(
		"[perf]pingpong n=%d echo_rtt p50=%s p90=%s p99=%s max=%s\n",
		len(rtts),
		perfPercentile(rtts, 0.50),
		perfPercentile(rtts, 0.90),
		perfPercentile(rtts, 0.99),
		perfPercentile(rtts, 1.0),
	)
	ackLock.Lock()
	ackRttsCopy := slices.Clone(ackRtts)
	ackLock.Unlock()
	fmt.Printf(
		"[perf]pingpong n=%d ack_rtt p50=%s p90=%s p99=%s max=%s\n",
		len(ackRttsCopy),
		perfPercentile(ackRttsCopy, 0.50),
		perfPercentile(ackRttsCopy, 0.90),
		perfPercentile(ackRttsCopy, 0.99),
		perfPercentile(ackRttsCopy, 1.0),
	)

	// ---- throughput phases ------------------------------------------------------

	type blastResult struct {
		label        string
		contentSize  int
		sent         int64
		delivered    int64
		elapsed      time.Duration
		drain        time.Duration
		goodputMibS  float64
		messageRate  float64
		ackP50       time.Duration
		ackP90       time.Duration
		ackP99       time.Duration
		ackMax       time.Duration
		allocPerMsg  int64
		gcCount      uint32
		gcPauseTotal time.Duration
	}

	blastResults := []*blastResult{}
	// blast is run concurrently for the bidirectional test, so guard the append.
	var blastResultsLock sync.Mutex

	// blast sends messages of contentSize from client to destination for duration,
	// then waits for the in-flight backlog to settle. noAck measures the
	// fire-and-forget path using the receiver's counters.
	blast := func(
		label string,
		client *connect.Client,
		destination connect.Id,
		peerStats *perfPeerStats,
		contentSize int,
		duration time.Duration,
		noAck bool,
		opts ...any,
	) *blastResult {
		content := perfRandContent(contentSize)
		recvCountStart := atomic.LoadInt64(&peerStats.receiveCount)

		var acked int64
		var ackErrs int64
		var lastAckNanos int64
		ackLatencies := make([]time.Duration, 0, 1<<18)
		var ackLatencyLock sync.Mutex

		var memStatsStart runtime.MemStats
		runtime.ReadMemStats(&memStatsStart)

		sendOpts := opts
		if noAck {
			sendOpts = append([]any{connect.NoAck()}, opts...)
		}

		// watch for a wedged pipeline anywhere in the phase: if acks stop
		// making progress for a stretch, capture all goroutine stacks once to
		// locate where every component is parked
		watchCtx, watchCancel := context.WithCancel(ctx)
		defer watchCancel()
		if !noAck {
			go func() {
				stallAcked := int64(0)
				stallStart := time.Now()
				for {
					select {
					case <-watchCtx.Done():
						return
					case <-time.After(250 * time.Millisecond):
					}
					currentAcked := atomic.LoadInt64(&acked)
					if currentAcked != stallAcked {
						stallAcked = currentAcked
						stallStart = time.Now()
						continue
					}
					if 5*time.Second <= time.Since(stallStart) {
						stacks := make([]byte, 64*1024*1024)
						n := runtime.Stack(stacks, true)
						path := filepath.Join(profileDir, fmt.Sprintf("perf_%s_%s_freeze_stacks.txt", modeName, label))
						os.WriteFile(path, stacks[:n], 0644)
						fmt.Printf("[perf]%s acks stalled 5s (acked=%d), stacks written to %s\n", label, stallAcked, path)
						return
					}
				}
			}()
		}

		startTime := time.Now()
		sent := int64(0)
		for time.Since(startTime) < duration {
			sendTime := time.Now()
			var ackCallback func(error)
			if noAck {
				ackCallback = func(err error) {}
			} else {
				ackCallback = func(err error) {
					if err != nil {
						atomic.AddInt64(&ackErrs, 1)
						return
					}
					atomic.AddInt64(&acked, 1)
					atomic.StoreInt64(&lastAckNanos, time.Now().UnixNano())
					d := time.Since(sendTime)
					ackLatencyLock.Lock()
					ackLatencies = append(ackLatencies, d)
					ackLatencyLock.Unlock()
				}
			}
			frame := perfRequireFrame(uint32(sent), 1, content)
			send(client, destination, frame, ackCallback, sendOpts...)
			sent += 1
		}
		sendEndTime := time.Now()

		var delivered int64
		var elapsed time.Duration
		if noAck {
			// wait for the receiver count to stop moving
			if err := waitForPerformanceCountToSettle(ctx, func() int64 {
				return atomic.LoadInt64(&peerStats.receiveCount)
			}); err != nil {
				panic(fmt.Errorf("[%s]receive settle: %w", label, err))
			}
			delivered = atomic.LoadInt64(&peerStats.receiveCount) - recvCountStart
			lastRecv := time.Unix(0, atomic.LoadInt64(&peerStats.lastRecvNanos))
			elapsed = lastRecv.Sub(startTime)
		} else {
			err := connectserver.TestingWaitForConnectCondition(ctx, perfSettleTimeout, 50*time.Millisecond, func(context.Context) (bool, string) {
				ackCount := atomic.LoadInt64(&acked)
				ackErrorCount := atomic.LoadInt64(&ackErrs)
				return sent <= ackCount+ackErrorCount, fmt.Sprintf(
					"sent=%d acked=%d ackErrs=%d received=%d", sent, ackCount, ackErrorCount,
					atomic.LoadInt64(&peerStats.receiveCount)-recvCountStart,
				)
			})
			if err != nil {
				panic(fmt.Errorf("[%s]ack settle: %w", label, err))
			}
			if errs := atomic.LoadInt64(&ackErrs); 0 < errs {
				panic(fmt.Errorf("[%s]ack errors = %d", label, errs))
			}
			delivered = atomic.LoadInt64(&acked)
			elapsed = time.Unix(0, atomic.LoadInt64(&lastAckNanos)).Sub(startTime)
		}

		var memStatsEnd runtime.MemStats
		runtime.ReadMemStats(&memStatsEnd)

		if elapsed <= 0 {
			elapsed = sendEndTime.Sub(startTime)
		}

		ackLatencyLock.Lock()
		ackLatenciesCopy := slices.Clone(ackLatencies)
		ackLatencyLock.Unlock()

		result := &blastResult{
			label:        label,
			contentSize:  contentSize,
			sent:         sent,
			delivered:    delivered,
			elapsed:      elapsed,
			drain:        elapsed - sendEndTime.Sub(startTime),
			goodputMibS:  (float64(delivered) * float64(contentSize)) / (1024 * 1024) / elapsed.Seconds(),
			messageRate:  float64(delivered) / elapsed.Seconds(),
			ackP50:       perfPercentile(ackLatenciesCopy, 0.50),
			ackP90:       perfPercentile(ackLatenciesCopy, 0.90),
			ackP99:       perfPercentile(ackLatenciesCopy, 0.99),
			ackMax:       perfPercentile(ackLatenciesCopy, 1.0),
			allocPerMsg:  int64(memStatsEnd.TotalAlloc-memStatsStart.TotalAlloc) / max(sent, 1),
			gcCount:      memStatsEnd.NumGC - memStatsStart.NumGC,
			gcPauseTotal: time.Duration(memStatsEnd.PauseTotalNs - memStatsStart.PauseTotalNs),
		}
		func() {
			blastResultsLock.Lock()
			defer blastResultsLock.Unlock()
			blastResults = append(blastResults, result)
		}()
		fmt.Printf(
			"[perf]%s size=%d sent=%d delivered=%d (%.1f%%) elapsed=%.2fs goodput=%.2f MiB/s rate=%.0f msg/s drain=%s ack p50=%s p90=%s p99=%s max=%s allocs/msg=%dB gc=%d pause=%s\n",
			result.label,
			result.contentSize,
			result.sent,
			result.delivered,
			100*float64(result.delivered)/float64(max(result.sent, 1)),
			result.elapsed.Seconds(),
			result.goodputMibS,
			result.messageRate,
			result.drain,
			result.ackP50,
			result.ackP90,
			result.ackP99,
			result.ackMax,
			result.allocPerMsg,
			result.gcCount,
			result.gcPauseTotal,
		)
		return result
	}

	// one-way acked throughput at several sizes
	for _, contentSize := range []int{64, 1024, 2048} {
		label := fmt.Sprintf("tput%d", contentSize)
		profileMutex := contentSize == 1024
		if profileMutex {
			runtime.SetBlockProfileRate(100 * 1000) // sample blocking events >= 100us
			runtime.SetMutexProfileFraction(5)
		}
		stopCpu := startCpuProfile(label)
		blast(label, clientA, destinationB, statsB, contentSize, perfBlastDuration, false)
		stopCpu()
		if profileMutex {
			writeProfile("mutex", label)
			writeProfile("block", label)
			runtime.SetBlockProfileRate(0)
			runtime.SetMutexProfileFraction(0)
		}
	}

	// one-way fire-and-forget throughput: isolates the ack path cost
	{
		stopCpu := startCpuProfile("noack1024")
		blast("noack1024", clientA, destinationB, statsB, 1024, perfBlastDuration, true)
		stopCpu()
	}

	// bidirectional throughput
	{
		stopCpu := startCpuProfile("bidi1024")
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			blast("bidi1024-a2b", clientA, destinationB, statsB, 1024, perfBlastDuration, false)
		}()
		go func() {
			defer wg.Done()
			blast("bidi1024-b2a", clientB, destinationA, statsA, 1024, perfBlastDuration, false, bToAOpts...)
		}()
		wg.Wait()
		stopCpu()
	}

	writeProfile("allocs", "end")

	// ---- summary -----------------------------------------------------------------

	fmt.Printf("[perf]==== summary mode=%s ====\n", modeName)
	fmt.Printf("[perf]first message rtt = %s\n", firstRtt)
	fmt.Printf(
		"[perf]pingpong echo_rtt p50=%s p99=%s ack_rtt p50=%s p99=%s\n",
		perfPercentile(rtts, 0.50),
		perfPercentile(rtts, 0.99),
		perfPercentile(ackRttsCopy, 0.50),
		perfPercentile(ackRttsCopy, 0.99),
	)
	for _, result := range blastResults {
		fmt.Printf(
			"[perf]%s size=%d goodput=%.2f MiB/s rate=%.0f msg/s delivered=%.1f%% ack p50=%s p99=%s\n",
			result.label,
			result.contentSize,
			result.goodputMibS,
			result.messageRate,
			100*float64(result.delivered)/float64(max(result.sent, 1)),
			result.ackP50,
			result.ackP99,
		)
	}
	fmt.Printf(
		"[perf]parse errs a=%d b=%d other frames a=%d b=%d goroutines=%d\n",
		atomic.LoadInt64(&statsA.parseErrCount),
		atomic.LoadInt64(&statsB.parseErrCount),
		atomic.LoadInt64(&statsA.otherTypeCount),
		atomic.LoadInt64(&statsB.otherTypeCount),
		runtime.NumGoroutine(),
	)
}

func perfRequireFrame(index uint32, count uint32, content string) *protocol.Frame {
	frame, err := connect.ToFrame(&protocol.SimpleMessage{
		MessageIndex: index,
		MessageCount: count,
		Content:      content,
	}, connect.DefaultProtocolVersion)
	if err != nil {
		panic(err)
	}
	return frame
}

func perfRandContent(byteCount int) string {
	// hex encoding doubles the size
	contentBytes := make([]byte, byteCount/2)
	cryptorand.Read(contentBytes)
	return hex.EncodeToString(contentBytes)
}

func perfPercentile(durations []time.Duration, p float64) time.Duration {
	if len(durations) == 0 {
		return 0
	}
	sorted := slices.Clone(durations)
	slices.Sort(sorted)
	i := int(float64(len(sorted)-1) * p)
	return sorted[i]
}

func perfDrain(messages chan *perfReceivedMessage) {
	for {
		select {
		case <-messages:
		default:
			return
		}
	}
}

// ---- carriers (LBALT.md §9 "Performance") -----------------------------------
//
// Each carrier of connect's edge measured on its own, through each way a
// client reaches the exchange:
//
//   - direct: the websocket and the urnetwork-framer/1 upgrade (H1+) on
//     connect's plain H1 port, each pinned. The harness above runs H1+, the
//     platform transport's default, pins it and checks it.
//   - lb_alt: the websocket, H1+, raw QUIC and QUIC under whodis through the
//     public sockets of connect's lb_alt front (Testing_NewLbAltFront), TLS
//     terminated in connect.
//   - nginx: the same four through warp/lb's pinned nginx build, as an nginx
//     edge interface carries them today: H1 through its http proxy, and QUIC
//     and whodis through its udp stream with PROXY v2 to connect's trusted
//     listeners. The local build has no ssl module, so its H1 hop is
//     plaintext where production terminates TLS in nginx. The case skips
//     without the build (`make nginx_local` in warp/lb).
//
// Two clients, each with its own transport on the carrier under test, send
// to each other without contracts through one exchange. A run takes the echo
// round trip's percentiles and the one-way acked message rate at 1 KiB. Each
// carrier is run perfCarrierRunCount times and its best run is held to the
// carrier's floors, with up to mcPerfExtraRunCount more runs only while a
// floor is unmet. The floors are collapse detectors, not targets: a carrier
// that wedges or loses the exchange fails, a slow or loaded host does not.
// Whodis has floors of its own: the server can answer only the client's
// queries, so its rate is the client's query pace, about 20 messages a
// second at 1 KiB on an idle loopback through nginx and the front alike.

const (
	perfCarrierRunCount         = 3
	perfCarrierPingPongCount    = 100
	perfCarrierPingPongWarmup   = 10
	perfCarrierBlastDuration    = 3 * time.Second
	perfCarrierBlastContentSize = 1024
	// the first message crosses a cold path: both connects, the residents
	perfCarrierFirstMessageTimeout = 120 * time.Second
	// the best run's echo p50 at most, and its message rate at least: H1 and
	// QUIC measure thousands to tens of thousands of messages a second and a
	// p50 under 2 ms on loopback, whodis 18 to 20 a second and about 60 ms
	perfCarrierMaxEchoRttP50        = 500 * time.Millisecond
	perfCarrierMinMessageRate       = 100.0
	perfCarrierWhodisMaxEchoRttP50  = 2 * time.Second
	perfCarrierWhodisMinMessageRate = 5.0
)

// How one client reaches the exchange on a carrier: its platform url, and the
// settings of its strategy and its transport.
type perfCarrierDial struct {
	platformUrl       string
	strategySettings  *connect.ClientStrategySettings
	transportSettings *connect.PlatformTransportSettings
}

// One carrier of one path.
type perfCarrierCase struct {
	name string
	mode connect.TransportMode
	// on h1: the urnetwork-framer/1 upgrade rather than the websocket
	h1Plus bool
	// a new dial for each client
	dial func() *perfCarrierDial
}

// The floors of a carrier's best run: its echo p50 at most, and its message
// rate at least.
func (self *perfCarrierCase) floors() (time.Duration, float64) {
	if self.mode == connect.TransportModeH3Dns {
		return perfCarrierWhodisMaxEchoRttP50, perfCarrierWhodisMinMessageRate
	}
	return perfCarrierMaxEchoRttP50, perfCarrierMinMessageRate
}

// One measured run of a carrier.
type perfCarrierRun struct {
	echoRttP50  time.Duration
	echoRttP99  time.Duration
	messageRate float64
	goodputMibS float64
}

// One network with one client, for a carrier's measurement.
type perfCarrierPeer struct {
	clientId   server.Id
	instanceId server.Id
	byJwt      string
}

// Creates a network with one client.
func newPerfCarrierPeer(ctx context.Context, name string) *perfCarrierPeer {
	networkId := server.NewId()
	userId := server.NewId()
	deviceId := server.NewId()
	clientId := server.NewId()
	networkName := fmt.Sprintf("perf-%s-%s", name, networkId)
	model.Testing_CreateNetwork(ctx, networkId, networkName, userId)
	model.Testing_CreateDevice(ctx, networkId, deviceId, clientId, name, name)
	return &perfCarrierPeer{
		clientId:   clientId,
		instanceId: server.NewId(),
		byJwt: session.NewByJwt(
			networkId,
			userId,
			networkName,
			false,
			false,
		).Client(deviceId, clientId).Testing_Sign(),
	}
}

// A platform transport budget of the default size for one client, so the
// clients of a measurement never contend for the process's.
func newPerfCarrierTransportBudget() *connect.PlatformTransportBudget {
	budgetStats := connect.DefaultPlatformTransportBudget().Stats()
	return connect.NewPlatformTransportBudget(
		budgetStats.TotalByteCount,
		budgetStats.MaxTransportCount,
	)
}

// Measures one carrier of one path: two clients connect on it, the first
// message crosses cold, then each run takes the echo round trip and the
// one-way acked rate. The h1 carriers must negotiate the carrier they pin.
func testConnectCarrierPerformance(t testing.TB, ctx context.Context, pathName string, carrierCase *perfCarrierCase) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	label := fmt.Sprintf("path=%s carrier=%s", pathName, carrierCase.name)

	type perfCarrierClient struct {
		peer      *perfCarrierPeer
		client    *connect.Client
		h1Stats   *connect.H1ConnectionStats
		transport *connect.PlatformTransport
		strategy  *connect.ClientStrategy
	}
	newClient := func(name string) *perfCarrierClient {
		peer := newPerfCarrierPeer(ctx, pathName+"-"+carrierCase.name+"-"+name)
		client := connect.NewClient(ctx, connect.Id(peer.clientId), connect.NewNoContractClientOob(), newLocalPerformanceClientSettings())
		dial := carrierCase.dial()
		strategy := connect.NewClientStrategy(ctx, dial.strategySettings)
		h1Stats := &connect.H1ConnectionStats{}
		dial.transportSettings.H1ConnectionStats = h1Stats
		dial.transportSettings.EnableH1Plus = carrierCase.h1Plus
		transport := connect.NewPlatformTransportWithTargetMode(
			ctx,
			strategy,
			client.RouteManager(),
			dial.platformUrl,
			&connect.ClientAuth{
				ByJwt:      peer.byJwt,
				InstanceId: connect.Id(peer.instanceId),
				AppVersion: "0.0.0",
			},
			carrierCase.mode,
			dial.transportSettings,
		)
		return &perfCarrierClient{
			peer:      peer,
			client:    client,
			h1Stats:   h1Stats,
			transport: transport,
			strategy:  strategy,
		}
	}
	clientA := newClient("a")
	clientB := newClient("b")
	defer func() {
		closeCtx, closeCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer closeCancel()
		for _, perfClient := range []*perfCarrierClient{clientA, clientB} {
			perfClient.transport.Close()
			perfClient.client.Close()
			perfClient.transport.CloseAndWait(closeCtx)
			perfClient.client.CloseAndWait(closeCtx)
			perfClient.strategy.Close()
		}
	}()
	destinationA := connect.Id(clientA.peer.clientId)
	destinationB := connect.Id(clientB.peer.clientId)
	clientA.client.ContractManager().AddNoContractPeer(destinationB)
	clientB.client.ContractManager().AddNoContractPeer(destinationA)

	// b echoes every message back to a while echoing, and counts what it
	// receives
	echoOn := &atomic.Bool{}
	echoContent := perfRandContent(32)
	echoesToA := make(chan uint32, 4096)
	var receivedCountB atomic.Int64
	addReceive := func(client *connect.Client, onMessage func(index uint32)) {
		client.AddReceiveCallback(func(source connect.TransferPath, frames []*protocol.Frame, peer connect.Peer) {
			for _, frame := range frames {
				message, err := connect.FromFrame(frame)
				if err != nil {
					continue
				}
				if simpleMessage, ok := message.(*protocol.SimpleMessage); ok {
					onMessage(simpleMessage.MessageIndex)
				}
			}
		})
	}
	addReceive(clientA.client, func(index uint32) {
		select {
		case echoesToA <- index:
		default:
		}
	})
	addReceive(clientB.client, func(index uint32) {
		receivedCountB.Add(1)
		if echoOn.Load() {
			// a receive callback must not block on a send: the echo goes out
			// from its own goroutine
			go connectserver.TestingSendConnectFrame(clientB.client, perfRequireFrame(index, 1, echoContent), destinationA, func(error) {})
		}
	})

	echoOn.Store(true)
	pingContent := perfRandContent(32)
	pingPong := func(index uint32, timeout time.Duration) time.Duration {
		startTime := time.Now()
		connectserver.TestingSendConnectFrame(clientA.client, perfRequireFrame(index, 1, pingContent), destinationB, func(error) {})
		deadline := time.After(timeout)
		for {
			select {
			case echoIndex := <-echoesToA:
				if echoIndex == index {
					return time.Since(startTime)
				}
			case <-deadline:
				t.Fatalf("[perfcarrier]%s: no echo of %d within %s", label, index, timeout)
			}
		}
	}
	firstRtt := pingPong(0, perfCarrierFirstMessageTimeout)
	fmt.Printf("[perfcarrier]%s first message rtt = %s\n", label, firstRtt)

	// the carrier each client negotiated is the one pinned
	for _, perfClient := range []*perfCarrierClient{clientA, clientB} {
		h1Snapshot := perfClient.h1Stats.Snapshot()
		var webSocketCount, h1PlusCount int64
		switch {
		case carrierCase.mode != connect.TransportModeH1:
		case carrierCase.h1Plus:
			h1PlusCount = 1
		default:
			webSocketCount = 1
		}
		if h1Snapshot.WebSocketConnectionCount != webSocketCount || h1Snapshot.H1PlusConnectionCount != h1PlusCount {
			t.Fatalf("[perfcarrier]%s negotiated %+v", label, h1Snapshot)
		}
	}

	nextIndex := uint32(1)
	measure := func(run int) *perfCarrierRun {
		echoOn.Store(true)
		rtts := []time.Duration{}
		for i := range perfCarrierPingPongWarmup + perfCarrierPingPongCount {
			rtt := pingPong(nextIndex, perfSettleTimeout)
			nextIndex += 1
			if perfCarrierPingPongWarmup <= i {
				rtts = append(rtts, rtt)
			}
		}
		echoOn.Store(false)

		content := perfRandContent(perfCarrierBlastContentSize)
		var acked atomic.Int64
		var ackErrs atomic.Int64
		var lastAckNanos atomic.Int64
		startTime := time.Now()
		sent := int64(0)
		for time.Since(startTime) < perfCarrierBlastDuration {
			connectserver.TestingSendConnectFrame(clientA.client, perfRequireFrame(nextIndex, 1, content), destinationB, func(err error) {
				if err != nil {
					ackErrs.Add(1)
					return
				}
				acked.Add(1)
				lastAckNanos.Store(time.Now().UnixNano())
			})
			nextIndex += 1
			sent += 1
		}
		err := connectserver.TestingWaitForConnectCondition(ctx, perfSettleTimeout, 50*time.Millisecond, func(context.Context) (bool, string) {
			return sent <= acked.Load()+ackErrs.Load(), fmt.Sprintf("sent=%d acked=%d ackErrs=%d", sent, acked.Load(), ackErrs.Load())
		})
		if err != nil {
			t.Fatalf("[perfcarrier]%s run %d: %s", label, run, err)
		}
		if ackErrCount := ackErrs.Load(); 0 < ackErrCount {
			t.Fatalf("[perfcarrier]%s run %d: %d messages were not acknowledged", label, run, ackErrCount)
		}
		elapsed := time.Unix(0, lastAckNanos.Load()).Sub(startTime)
		if elapsed <= 0 {
			elapsed = time.Since(startTime)
		}
		measuredRun := &perfCarrierRun{
			echoRttP50:  perfPercentile(rtts, 0.50),
			echoRttP99:  perfPercentile(rtts, 0.99),
			messageRate: float64(acked.Load()) / elapsed.Seconds(),
			goodputMibS: float64(acked.Load()) * float64(len(content)) / (1024 * 1024) / elapsed.Seconds(),
		}
		fmt.Printf(
			"[perfcarrier]%s run=%d echo_rtt p50=%s p99=%s rate=%.0f msg/s goodput=%.2f MiB/s\n",
			label,
			run,
			measuredRun.echoRttP50,
			measuredRun.echoRttP99,
			measuredRun.messageRate,
			measuredRun.goodputMibS,
		)
		return measuredRun
	}

	maxEchoRttP50, minMessageRate := carrierCase.floors()
	var bestEchoRttP50 time.Duration
	bestMessageRate := 0.0
	floorsMet := func() bool {
		return 0 < bestEchoRttP50 && bestEchoRttP50 <= maxEchoRttP50 && minMessageRate <= bestMessageRate
	}
	for run := 0; run < perfCarrierRunCount || (!floorsMet() && run < perfCarrierRunCount+mcPerfExtraRunCount); run += 1 {
		measuredRun := measure(run)
		if bestEchoRttP50 == 0 || measuredRun.echoRttP50 < bestEchoRttP50 {
			bestEchoRttP50 = measuredRun.echoRttP50
		}
		bestMessageRate = max(bestMessageRate, measuredRun.messageRate)
	}
	fmt.Printf("[perfcarrier]%s best echo_rtt p50=%s rate=%.0f msg/s received_b=%d\n", label, bestEchoRttP50, bestMessageRate, receivedCountB.Load())
	if maxEchoRttP50 < bestEchoRttP50 {
		t.Fatalf("[perfcarrier]%s: best echo p50 %s over the %s ceiling", label, bestEchoRttP50, maxEchoRttP50)
	}
	if bestMessageRate < minMessageRate {
		t.Fatalf("[perfcarrier]%s: best rate %.0f msg/s under the %.0f floor", label, bestMessageRate, minMessageRate)
	}
}

// Connect for the direct and nginx paths: one exchange with its handler on a
// plain H1 port, as connect's private H1 serves nginx today, and with
// trustedUdp the handler's trusted h3 and h3dns listeners, which read nginx's
// PROXY v2 header, on loopback.
type perfCarrierConnect struct {
	h1Port  int
	h3Port  int
	dnsPort int
}

// Starts connect's side of the direct or nginx path; it closes with the test.
func newPerfCarrierConnect(t testing.TB, ctx context.Context, trustedUdp bool) *perfCarrierConnect {
	listenTcp := func() net.Listener {
		listener, err := net.Listen("tcp4", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		return listener
	}
	exchangeListener := listenTcp()
	exchangePort := exchangeListener.Addr().(*net.TCPAddr).Port
	settings := connectserver.DefaultExchangeSettings()
	settings.ConnectionAnnounceTimeout = 0
	settings.ConnectionRateLimitSettings.BurstConnectionCount = 1000
	settings.ForwardEnforceActiveContracts = false
	settings.ConnectionTestConfig = connectserver.V0TestConfig()
	settings.ListenH3Port = 0
	settings.ListenDnsPort = 0
	settings.ListenDnsCompatibilityPorts = nil
	packetConns := connectserver.ConnectHandlerPacketConns{}
	perfConnect := &perfCarrierConnect{}
	if trustedUdp {
		listenUdp := func() net.PacketConn {
			packetConn, err := net.ListenPacket("udp4", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			return packetConn
		}
		packetConns.H3 = listenUdp()
		packetConns.Dns = listenUdp()
		perfConnect.h3Port = packetConns.H3.LocalAddr().(*net.UDPAddr).Port
		perfConnect.dnsPort = packetConns.Dns.LocalAddr().(*net.UDPAddr).Port
		settings.ListenH3Port = perfConnect.h3Port
		settings.ListenDnsPort = perfConnect.dnsPort
		// what nginx streams to: the PROXY v2 reader, and connect's own
		// certificate, self-signed here
		settings.EnableProxyProtocol = true
		settings.TransportTlsSettings.EnableSelfSign = true
	}
	exchange := connectserver.NewExchangeWithListeners(
		ctx,
		"perfcarrier",
		"connect",
		"test",
		map[int]int{exchangePort: exchangePort},
		map[string]string{"perfcarrier": "127.0.0.1"},
		settings,
		map[int]net.Listener{exchangePort: exchangeListener},
	)
	connectHandler := connectserver.NewConnectHandlerWithPacketConns(ctx, server.NewId(), exchange, &settings.ConnectHandlerSettings, packetConns)
	h1Listener := listenTcp()
	perfConnect.h1Port = h1Listener.Addr().(*net.TCPAddr).Port
	h1Server := &http.Server{
		Handler: router.NewRouter(ctx, []*router.Route{
			router.NewRoute("GET", "/", connectHandler.Connect),
		}),
	}
	go h1Server.Serve(h1Listener)
	t.Cleanup(func() {
		connectHandler.Close()
		h1Server.Close()
		exchange.Close()
		closeCtx, closeCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer closeCancel()
		if !connectHandler.WaitForIdle(closeCtx) {
			t.Errorf("connect's handler did not finish")
		}
	})
	if trustedUdp {
		err := connectserver.TestingWaitForConnectCondition(ctx, perfSettleTimeout, 20*time.Millisecond, func(context.Context) (bool, string) {
			_, err := connectHandler.ListenerReadyUdpPorts()
			return err == nil, fmt.Sprintf("trusted listeners: %v", err)
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	return perfConnect
}

// The direct path's dial: a plain websocket url to connect's H1 port.
func (self *perfCarrierConnect) directDial() *perfCarrierDial {
	strategySettings := connect.DefaultClientStrategySettings()
	strategySettings.EnableResilient = false
	transportSettings := connect.DefaultPlatformTransportSettings()
	transportSettings.PlatformTransportBudget = newPerfCarrierTransportBudget()
	return &perfCarrierDial{
		platformUrl:       fmt.Sprintf("ws://127.0.0.1:%d", self.h1Port),
		strategySettings:  strategySettings,
		transportSettings: transportSettings,
	}
}

// The websocket and H1+ on connect's H1 port, each pinned.
func TestConnectPerformanceCarriersDirect(t *testing.T) {
	perfTestEnv().Run(t, func(t testing.TB) {
		if testing.Short() {
			return
		}
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		perfConnect := newPerfCarrierConnect(t, ctx, false)
		for _, carrierCase := range []*perfCarrierCase{
			{name: "websocket", mode: connect.TransportModeH1, h1Plus: false, dial: perfConnect.directDial},
			{name: "h1+", mode: connect.TransportModeH1, h1Plus: true, dial: perfConnect.directDial},
		} {
			testConnectCarrierPerformance(t, ctx, "direct", carrierCase)
		}
	})
}

// The websocket, H1+, raw QUIC and QUIC under whodis through the public
// sockets of connect's lb_alt front.
func TestConnectPerformanceCarriersLbAlt(t *testing.T) {
	perfTestEnv().Run(t, func(t testing.TB) {
		if testing.Short() {
			return
		}
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		front := connectserver.Testing_NewLbAltFront(t)
		frontDial := func(mode connect.TransportMode, h1Plus bool) func() *perfCarrierDial {
			return func() *perfCarrierDial {
				platformUrl, strategySettings, transportSettings := front.Dial(mode, h1Plus)
				return &perfCarrierDial{
					platformUrl:       platformUrl,
					strategySettings:  strategySettings,
					transportSettings: transportSettings,
				}
			}
		}
		for _, carrierCase := range []*perfCarrierCase{
			{name: "websocket", mode: connect.TransportModeH1, h1Plus: false, dial: frontDial(connect.TransportModeH1, false)},
			{name: "h1+", mode: connect.TransportModeH1, h1Plus: true, dial: frontDial(connect.TransportModeH1, true)},
			{name: "quic", mode: connect.TransportModeH3, h1Plus: false, dial: frontDial(connect.TransportModeH3, false)},
			{name: "whodis", mode: connect.TransportModeH3Dns, h1Plus: false, dial: frontDial(connect.TransportModeH3Dns, false)},
		} {
			testConnectCarrierPerformance(t, ctx, "lb_alt", carrierCase)
		}
	})
}

// warp/lb's pinned nginx build (`make nginx_local`): NGINX_UDP_PROXY_V2_BINARY
// when set, as warp/lb's own tests read it, then the build in the warp
// checkout beside the server's or under WARP_HOME. "" with the reason when
// there is none.
func perfNginxBinary() (string, string) {
	if configuredBinary := os.Getenv("NGINX_UDP_PROXY_V2_BINARY"); configuredBinary != "" {
		resolvedBinary, err := exec.LookPath(configuredBinary)
		if err != nil {
			return "", fmt.Sprintf("NGINX_UDP_PROXY_V2_BINARY=%q is not executable: %v", configuredBinary, err)
		}
		return resolvedBinary, ""
	}
	buildPath := filepath.Join("warp", "lb", "build", "nginx-local", "sbin", "nginx")
	candidates := []string{
		filepath.Join("..", "..", buildPath),
	}
	if warpHome := os.Getenv("WARP_HOME"); warpHome != "" {
		candidates = append(candidates, filepath.Join(warpHome, buildPath))
	}
	for _, candidate := range candidates {
		if resolvedBinary, err := exec.LookPath(candidate); err == nil {
			return resolvedBinary, ""
		}
	}
	return "", "the pinned nginx build was not found; run `make nginx_local` in warp/lb"
}

// Runs nginx with the edge's connect paths in front of perfConnect: an http
// proxy to connect's H1 port with the websocket upgrade, and udp streams with
// PROXY v2 to connect's trusted h3 and h3dns listeners, with the lb's
// proxy settings. Returns the ports nginx listens on, plain http first. nginx
// stops with the test.
func startPerfNginx(t testing.TB, nginxBinary string, perfConnect *perfCarrierConnect) (httpPort int, h3Port int, dnsPort int) {
	versionOutput, err := exec.Command(nginxBinary, "-V").CombinedOutput()
	if err != nil {
		t.Fatalf("read the nginx build configuration: %v: %s", err, versionOutput)
	}
	if !strings.Contains(string(versionOutput), "--with-stream") {
		t.Skipf("the nginx build has no stream module: %s", versionOutput)
	}
	freePort := func(network string) int {
		if network == "tcp" {
			listener, err := net.Listen("tcp4", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			defer listener.Close()
			return listener.Addr().(*net.TCPAddr).Port
		}
		packetConn, err := net.ListenPacket("udp4", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		defer packetConn.Close()
		return packetConn.LocalAddr().(*net.UDPAddr).Port
	}
	httpPort = freePort("tcp")
	h3Port = freePort("udp")
	dnsPort = freePort("udp")

	prefixDir := t.TempDir()
	configPath := filepath.Join(prefixDir, "nginx.conf")
	config := fmt.Sprintf(`
worker_processes 2;
pid nginx.pid;
error_log stderr warn;
daemon off;

events {
    worker_connections 1024;
}

http {
    access_log off;
    proxy_http_version 1.1;
    proxy_buffering off;

    server {
        listen 127.0.0.1:%d;
        location / {
            proxy_pass http://127.0.0.1:%d;
            proxy_set_header X-UR-Forwarded-For $remote_addr:$remote_port;
            proxy_set_header X-Forwarded-For "";
            proxy_set_header X-Forwarded-Source-Port "";
            proxy_set_header Host $host;
            proxy_set_header Upgrade $http_upgrade;
            proxy_set_header Connection 'upgrade';
            proxy_read_timeout 60s;
            proxy_send_timeout 60s;
        }
    }
}

stream {
    server {
        listen 127.0.0.1:%d udp reuseport;
        proxy_protocol v2;
        proxy_timeout 30s;
        proxy_requests 0;
        proxy_pass 127.0.0.1:%d;
    }

    server {
        listen 127.0.0.1:%d udp reuseport;
        proxy_protocol v2;
        proxy_timeout 30s;
        proxy_requests 0;
        proxy_pass 127.0.0.1:%d;
    }
}
`, httpPort, perfConnect.h1Port, h3Port, perfConnect.h3Port, dnsPort, perfConnect.dnsPort)
	if err := os.WriteFile(configPath, []byte(config), 0600); err != nil {
		t.Fatal(err)
	}
	outputPath := filepath.Join(prefixDir, "nginx-output.log")
	outputFile, err := os.Create(outputPath)
	if err != nil {
		t.Fatal(err)
	}
	nginxCmd := exec.Command(nginxBinary, "-p", prefixDir+string(os.PathSeparator), "-c", configPath)
	nginxCmd.Stdout = outputFile
	nginxCmd.Stderr = outputFile
	nginxCmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := nginxCmd.Start(); err != nil {
		outputFile.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		syscall.Kill(-nginxCmd.Process.Pid, syscall.SIGQUIT)
		waitDone := make(chan error, 1)
		go func() {
			waitDone <- nginxCmd.Wait()
		}()
		select {
		case <-waitDone:
		case <-time.After(10 * time.Second):
			syscall.Kill(-nginxCmd.Process.Pid, syscall.SIGKILL)
			<-waitDone
		}
		outputFile.Close()
		if t.Failed() {
			output, _ := os.ReadFile(outputPath)
			fmt.Printf("[perfcarrier]nginx output:\n%s\n", output)
		}
	})
	// ready once its http listener accepts
	readyDeadline := time.Now().Add(perfSettleTimeout)
	for {
		conn, err := net.Dial("tcp4", fmt.Sprintf("127.0.0.1:%d", httpPort))
		if err == nil {
			conn.Close()
			break
		}
		if readyDeadline.Before(time.Now()) {
			output, _ := os.ReadFile(outputPath)
			t.Fatalf("nginx did not become ready: %v\n%s", err, output)
		}
		select {
		case <-time.After(20 * time.Millisecond):
		}
	}
	return httpPort, h3Port, dnsPort
}

// The websocket, H1+, raw QUIC and QUIC under whodis through warp/lb's nginx
// build, as an nginx edge interface carries them: the baseline of the lb_alt
// cases. Skips without the build.
func TestConnectPerformanceCarriersNginx(t *testing.T) {
	nginxBinary, reason := perfNginxBinary()
	if nginxBinary == "" {
		t.Skip(reason)
	}
	perfTestEnv().Run(t, func(t testing.TB) {
		if testing.Short() {
			return
		}
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		perfConnect := newPerfCarrierConnect(t, ctx, true)
		httpPort, h3Port, dnsPort := startPerfNginx(t, nginxBinary, perfConnect)
		nginxDial := func(mode connect.TransportMode) func() *perfCarrierDial {
			return func() *perfCarrierDial {
				strategySettings := connect.DefaultClientStrategySettings()
				strategySettings.EnableResilient = false
				transportSettings := connect.DefaultPlatformTransportSettings()
				transportSettings.PlatformTransportBudget = newPerfCarrierTransportBudget()
				// connect's trusted listeners serve a self-signed certificate
				// here
				transportSettings.QuicTlsConfig = &tls.Config{
					InsecureSkipVerify: true,
				}
				platformUrl := fmt.Sprintf("ws://127.0.0.1:%d", httpPort)
				switch mode {
				case connect.TransportModeH3:
					platformUrl = "wss://connect.service.example"
					transportSettings.AltUrl = fmt.Sprintf("https://127.0.0.1:%d", h3Port)
				case connect.TransportModeH3Dns:
					// the trusted h3dns listener decodes with connect's
					// default tld, which the client's settings keep
					platformUrl = "wss://connect.service.example"
					transportSettings.AltUrl = fmt.Sprintf("https://127.0.0.1:%d", dnsPort)
				}
				return &perfCarrierDial{
					platformUrl:       platformUrl,
					strategySettings:  strategySettings,
					transportSettings: transportSettings,
				}
			}
		}
		for _, carrierCase := range []*perfCarrierCase{
			{name: "websocket", mode: connect.TransportModeH1, h1Plus: false, dial: nginxDial(connect.TransportModeH1)},
			{name: "h1+", mode: connect.TransportModeH1, h1Plus: true, dial: nginxDial(connect.TransportModeH1)},
			{name: "quic", mode: connect.TransportModeH3, h1Plus: false, dial: nginxDial(connect.TransportModeH3)},
			{name: "whodis", mode: connect.TransportModeH3Dns, h1Plus: false, dial: nginxDial(connect.TransportModeH3Dns)},
		} {
			testConnectCarrierPerformance(t, ctx, "nginx", carrierCase)
		}
	})
}
