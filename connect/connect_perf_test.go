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
//     transport each (H1 websocket), client A homed on perf0 and B on perf1
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
	"encoding/hex"
	"fmt"
	mathrand "math/rand"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"runtime/pprof"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/connect/protocol"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/api"
	connectserver "github.com/urnetwork/server/connect"
	"github.com/urnetwork/server/jwt"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/router"
)

const (
	perfWsPort0       = 7610
	perfWsPort1       = 7611
	perfApiPort       = 7620
	perfExchangePort0 = 7710
	perfExchangePort1 = 7711

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
	// a perf run should not be rerun on failure
	return &server.TestEnv{
		ApplyDbMigrations: true,
		RerunCount:        0,
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

	hostConfigs := []struct {
		host         string
		wsPort       int
		exchangePort int
	}{
		{"perf0", perfWsPort0, perfExchangePort0},
		{"perf1", perfWsPort1, perfExchangePort1},
	}

	exchanges := []*connectserver.Exchange{}
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

		exchange := connectserver.NewExchange(
			ctx,
			hostConfig.host,
			service,
			block,
			map[int]int{hostConfig.exchangePort: hostConfig.exchangePort},
			routes,
			settings,
		)
		exchanges = append(exchanges, exchange)

		connectHandler := connectserver.NewConnectHandler(ctx, server.NewId(), exchange, &settings.ConnectHandlerSettings)
		connectRoutes := []*router.Route{
			router.NewRoute("GET", "/status", router.WarpStatus),
			router.NewRoute("GET", "/", connectHandler.Connect),
		}
		httpServer := &http.Server{
			Addr:    fmt.Sprintf("127.0.0.1:%d", hostConfig.wsPort),
			Handler: router.NewRouter(ctx, connectRoutes),
		}
		httpServers = append(httpServers, httpServer)
		go httpServer.ListenAndServe()
	}

	apiServer := &http.Server{
		Addr:    fmt.Sprintf("127.0.0.1:%d", perfApiPort),
		Handler: router.NewRouter(ctx, api.Routes()),
	}
	httpServers = append(httpServers, apiServer)
	go apiServer.ListenAndServe()

	defer func() {
		for _, httpServer := range httpServers {
			httpServer.Close()
		}
		for _, exchange := range exchanges {
			exchange.Close()
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
	waitFor(15*time.Second, "ws server 0", func() bool { return statusOk(perfWsPort0) })
	waitFor(15*time.Second, "ws server 1", func() bool { return statusOk(perfWsPort1) })
	waitFor(15*time.Second, "api server", func() bool { return statusOk(perfApiPort) })

	// ---- networks, devices, balance -------------------------------------------

	apiUrl := fmt.Sprintf("http://127.0.0.1:%d", perfApiPort)

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

		p.byJwt = jwt.NewByJwt(
			p.networkId,
			p.userId,
			p.networkName,
			false,
			false,
		).Client(p.deviceId, p.clientId).Sign()
		return p
	}

	peerA := newPeer("a")
	peerB := newPeer("b")

	// ---- clients with the real api for out-of-band control --------------------

	newPeerClient := func(p *peer, wsPort int) (*connect.Client, *connect.PlatformTransport) {
		strategySettings := connect.DefaultClientStrategySettings()
		strategySettings.EnableResilient = false
		clientStrategy := connect.NewClientStrategy(ctx, strategySettings)

		oob := connect.NewApiOutOfBandControl(ctx, clientStrategy, p.byJwt, apiUrl)
		client := connect.NewClient(ctx, connect.Id(p.clientId), oob, connect.DefaultClientSettings())

		auth := &connect.ClientAuth{
			ByJwt:      p.byJwt,
			InstanceId: connect.Id(p.instanceId),
			AppVersion: "0.0.0",
		}
		transport := connect.NewPlatformTransportWithTargetMode(
			ctx,
			clientStrategy,
			client.RouteManager(),
			fmt.Sprintf("ws://127.0.0.1:%d", wsPort),
			auth,
			connect.TransportModeH1,
			connect.DefaultPlatformTransportSettings(),
		)
		return client, transport
	}

	clientA, transportA := newPeerClient(peerA, perfWsPort0)
	clientB, transportB := newPeerClient(peerB, perfWsPort1)

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

	destinationA := connect.DestinationId(connect.Id(peerA.clientId))
	destinationB := connect.DestinationId(connect.Id(peerB.clientId))

	send := func(client *connect.Client, destination connect.TransferPath, frame *protocol.Frame, ackCallback func(error), opts ...any) {
		_, err := client.SendWithTimeoutDetailed(frame, destination, ackCallback, -1, opts...)
		if err != nil && !server.IsDoneError(err) {
			panic(fmt.Errorf("send error = %v", err))
		}
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
		client.AddReceiveCallback(func(source connect.TransferPath, frames []*protocol.Frame, provideMode protocol.ProvideMode) {
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
		destination connect.TransferPath,
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
			stableCount := 0
			lastReceived := atomic.LoadInt64(&peerStats.receiveCount)
			for stableCount < 8 {
				select {
				case <-time.After(250 * time.Millisecond):
				}
				received := atomic.LoadInt64(&peerStats.receiveCount)
				if received == lastReceived {
					stableCount += 1
				} else {
					stableCount = 0
					lastReceived = received
				}
			}
			delivered = atomic.LoadInt64(&peerStats.receiveCount) - recvCountStart
			lastRecv := time.Unix(0, atomic.LoadInt64(&peerStats.lastRecvNanos))
			elapsed = lastRecv.Sub(startTime)
		} else {
			settleDeadline := time.Now().Add(perfSettleTimeout)
			for atomic.LoadInt64(&acked)+atomic.LoadInt64(&ackErrs) < sent {
				if settleDeadline.Before(time.Now()) {
					panic(fmt.Errorf(
						"[%s]settle timeout: sent=%d acked=%d ackErrs=%d received=%d",
						label,
						sent,
						atomic.LoadInt64(&acked),
						atomic.LoadInt64(&ackErrs),
						atomic.LoadInt64(&peerStats.receiveCount)-recvCountStart,
					))
				}
				select {
				case <-time.After(50 * time.Millisecond):
				}
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
	mathrand.Read(contentBytes)
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
