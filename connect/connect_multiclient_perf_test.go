package connect_test

// Full user-stack performance test, combining the connect_perf harness with
// the ip layer (see ../connect/ip_test.go). It stands up, in-process:
//
//   - two real exchange hosts + connect handlers on plain ws ports
//   - a real api server (the full api.Routes())
//   - a provider: connect.Client + platform transport + LocalUserNat +
//     RemoteUserNatProvider, egressing over real OS sockets
//   - a device: RemoteUserNatMultiClient with the real ApiMultiClientGenerator
//     (find-providers2 pinned to the provider client id, per-destination
//     clients created through the real auth-client api)
//   - a local udp echo server as the egress target
//
// IP packets flow: device multi client -> ws -> exchange -> provider client
// -> user nat -> os socket -> echo -> back. The device and provider share a
// network so the provide relationship is ProvideMode_Network, which the
// egress security policy requires for loopback destinations.
//
// Phases, each reported with "[mcperf]" lines:
//
//   1. first packet: stack cold start (find provider, auth clients, ws
//      connects, contracts) to first echoed packet
//   2. packet echo RTT percentiles at a small payload
//   3. udp throughput: timed flood at a packet-like payload, reporting send
//      and echo rates, delivery, and under-load RTT percentiles
//   4. download blast: a local server blasts a fixed volume of udp packets
//      back to the device (the provider -> device direction), paced slightly
//      above the expected stack ceiling so the path stays saturated; reports
//      the delivered goodput, which is the sustainable download rate of the
//      full stack
//
// Run with the standard local test env (see test.sh), without -race:
//
//	go test -run TestConnectMultiClientPerformance -count 1 -timeout 30m \
//	  -args -v 0 -logtostderr true

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"runtime/pprof"
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
	mcWsPort0       = 7630
	mcWsPort1       = 7631
	mcApiPort       = 7640
	mcExchangePort0 = 7730
	mcExchangePort1 = 7731

	mcPingPongCount  = 300
	mcPingPongWarmup = 20
	mcBlastDuration  = 6 * time.Second
	// each measured perf phase is run this many times and the best (max) result
	// is taken, to ride out host scheduling noise on the build server.
	mcPerfRunCount = 3
	// minimum acceptable udp echo delivery fraction (best of the runs). Set low
	// for now: the flood far outruns the stack, so this is a collapse-detector
	// (catches a path that drops ~everything) rather than a real delivery gate.
	// Raise once the flood is bounded to the stack's sustainable rate.
	mcUdpDeliveryMinFraction = 0.01
	// seq (8) + send nanos (8)
	mcPayloadHeaderByteCount = 16

	// download blast volume and pacing. The pace is set above the expected
	// stack ceiling so the path stays saturated; the kernel drops the excess
	// at the nat ingress socket, and the delivered goodput measures the
	// sustainable download rate of the full stack.
	mcDownloadTotalByteCount   = 100 * 1024 * 1024
	mcDownloadPayloadByteCount = 1400
	// pace in 64 KiB chunks with a 1ms sleep: ~62 MiB/s
	mcDownloadPaceChunkByteCount = 64 * 1024

	// tcp stream test ports (distinct from the udp test so both can run)
	mcTcpWsPort0       = 7650
	mcTcpWsPort1       = 7651
	mcTcpApiPort       = 7660
	mcTcpExchangePort0 = 7750
	mcTcpExchangePort1 = 7751

	mcTcpStreamByteCount = 100 * 1024 * 1024
	// minimum acceptable max tcp goodput. Set low for now; raise as the stack
	// is optimized.
	mcTcpStreamMinGoodput = 0.5
)

func TestConnectMultiClientPerformance(t *testing.T) {
	perfTestEnv().Run(t, func(t testing.TB) {
		testConnectMultiClientPerformance(t)
	})
}

func TestConnectMultiClientTcpPerformance(t *testing.T) {
	perfTestEnv().Run(t, func(t testing.TB) {
		testConnectMultiClientTcpPerformance(t)
	})
}

// testConnectMultiClientTcpPerformance drives a real TCP stream through the
// full user stack: a userspace tun TCP stack on the device, bridged to the
// RemoteUserNatMultiClient, through the exchange to the provider, out the
// provider LocalUserNat to a real OS-socket TCP echo server. It measures the
// single-client TCP stream throughput (the optimization target) and profiles
// the per-packet TCP path.
func testConnectMultiClientTcpPerformance(t testing.TB) {
	if testing.Short() {
		return
	}

	os.Setenv("WARP_SERVICE", "test")
	os.Setenv("WARP_BLOCK", "test")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	fmt.Printf("[mctcp]go=%s cpus=%d gomaxprocs=%d\n", runtime.Version(), runtime.NumCPU(), runtime.GOMAXPROCS(0))

	stack, cleanup := setupMcStack(ctx, "mctcp", mcTcpWsPort0, mcTcpWsPort1, mcTcpApiPort, mcTcpExchangePort0, mcTcpExchangePort1)
	defer cleanup()

	// ---- real tcp echo server (os sockets) -----------------------------------

	echoListener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		panic(err)
	}
	defer echoListener.Close()
	echoAddr := echoListener.Addr().String()
	go func() {
		for {
			conn, err := echoListener.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				io.Copy(conn, conn)
			}()
		}
	}()
	fmt.Printf("[mctcp]tcp echo server %s\n", echoAddr)

	// ---- device: tun bridged to the multi client -----------------------------

	tun, err := connect.CreateTunWithDefaults(ctx)
	if err != nil {
		panic(err)
	}
	defer tun.Close()

	deviceStrategySettings := connect.DefaultClientStrategySettings()
	deviceStrategySettings.EnableResilient = false
	deviceStrategy := connect.NewClientStrategy(ctx, deviceStrategySettings)

	providerClientIdConnect := connect.Id(stack.providerClientId)
	deviceClientIdConnect := connect.Id(stack.deviceClientId)
	specs := []*connect.ProviderSpec{
		{ClientId: &providerClientIdConnect},
	}
	generator := connect.NewApiMultiClientGeneratorWithDefaults(
		ctx,
		specs,
		deviceStrategy,
		nil,
		stack.apiUrl,
		stack.deviceByJwt,
		stack.devicePlatformUrl,
		"mctcp",
		"mctcp",
		"0.0.0",
		&deviceClientIdConnect,
	)

	// received packets from providers are written back into the tun stack
	receivePacket := func(source connect.TransferPath, provideMode protocol.ProvideMode, ipPath *connect.IpPath, packet []byte) {
		tun.Write(packet)
	}

	multiClient := connect.NewRemoteUserNatMultiClientWithDefaults(
		ctx,
		generator,
		receivePacket,
		protocol.ProvideMode_Network,
	)
	defer multiClient.Close()

	deviceSource := connect.SourceId(deviceClientIdConnect)

	// bridge tun -> multi client (egress); receive callback above bridges back
	go func() {
		packets := make([][]byte, 64)
		for {
			n, err := tun.ReadBatch(packets)
			if err != nil {
				return
			}
			for i := 0; i < n; i += 1 {
				multiClient.SendPacket(deviceSource, protocol.ProvideMode_Network, packets[i], -1)
			}
		}
	}()

	// ---- profiling -----------------------------------------------------------

	profileDir := "profile"
	os.MkdirAll(profileDir, 0755)

	// ---- phase 1: connect + first byte (cold start) --------------------------

	startTime := time.Now()
	dialCtx, dialCancel := context.WithTimeout(ctx, 60*time.Second)
	conn, err := tun.DialContext(dialCtx, "tcp", echoAddr)
	dialCancel()
	if err != nil {
		panic(fmt.Errorf("tcp dial through stack: %w", err))
	}
	defer conn.Close()
	// one round trip to confirm the stream
	probe := []byte("probe-0123456789")
	conn.SetWriteDeadline(time.Now().Add(30 * time.Second))
	if _, err := conn.Write(probe); err != nil {
		panic(fmt.Errorf("tcp probe write: %w", err))
	}
	probeEcho := make([]byte, len(probe))
	conn.SetReadDeadline(time.Now().Add(30 * time.Second))
	if _, err := io.ReadFull(conn, probeEcho); err != nil {
		panic(fmt.Errorf("tcp probe read: %w", err))
	}
	fmt.Printf("[mctcp]connect + first byte rtt = %s\n", time.Since(startTime))

	// ---- phase 2: stream throughput ------------------------------------------
	// write a fixed volume and read it back concurrently, measuring goodput.
	// the measurement is repeated and the max is taken, since goodput is
	// sensitive to host scheduling noise and we care about the achievable
	// ceiling rather than the worst-case sample.

	streamByteCount := int64(mcTcpStreamByteCount)
	chunk := make([]byte, 64*1024)
	for i := range chunk {
		chunk[i] = byte(i)
	}

	cpuProfilePath := filepath.Join(profileDir, "mctcp_stream_cpu.pprof")
	cpuProfileFile, _ := os.Create(cpuProfilePath)
	cpuProfileActive := pprof.StartCPUProfile(cpuProfileFile) == nil

	// runStream writes streamByteCount through the conn and concurrently reads
	// it back, returning the measured goodput in MiB/s.
	runStream := func() float64 {
		var memStatsStart runtime.MemStats
		runtime.ReadMemStats(&memStatsStart)

		streamStart := time.Now()
		var readErr atomic.Pointer[error]
		var readByteCount int64
		readDone := make(chan struct{})
		go func() {
			defer close(readDone)
			buffer := make([]byte, 256*1024)
			remaining := streamByteCount
			for 0 < remaining {
				conn.SetReadDeadline(time.Now().Add(60 * time.Second))
				n, err := conn.Read(buffer)
				if 0 < n {
					atomic.AddInt64(&readByteCount, int64(n))
					remaining -= int64(n)
				}
				if err != nil {
					readErr.Store(&err)
					return
				}
			}
		}()

		written := int64(0)
		for written < streamByteCount {
			n := min(int64(len(chunk)), streamByteCount-written)
			conn.SetWriteDeadline(time.Now().Add(60 * time.Second))
			if _, err := conn.Write(chunk[:n]); err != nil {
				panic(fmt.Errorf("tcp stream write: %w", err))
			}
			written += n
		}

		select {
		case <-readDone:
		case <-time.After(120 * time.Second):
			panic(fmt.Errorf("tcp stream read timeout: read %d/%d", atomic.LoadInt64(&readByteCount), streamByteCount))
		}
		elapsed := time.Since(streamStart)

		if errp := readErr.Load(); errp != nil {
			panic(fmt.Errorf("tcp stream read error: %w", *errp))
		}
		delivered := atomic.LoadInt64(&readByteCount)
		goodput := float64(delivered) / (1024 * 1024) / elapsed.Seconds()

		var memStatsEnd runtime.MemStats
		runtime.ReadMemStats(&memStatsEnd)

		fmt.Printf(
			"[mctcp]stream bytes=%dMiB elapsed=%.2fs goodput=%.2f MiB/s allocs/MiB=%dKB gc=%d pause=%s\n",
			streamByteCount/(1024*1024),
			elapsed.Seconds(),
			goodput,
			int64(memStatsEnd.TotalAlloc-memStatsStart.TotalAlloc)/max(streamByteCount/(1024*1024), 1)/1024,
			memStatsEnd.NumGC-memStatsStart.NumGC,
			time.Duration(memStatsEnd.PauseTotalNs-memStatsStart.PauseTotalNs),
		)
		return goodput
	}

	goodput := 0.0
	for run := 0; run < mcPerfRunCount; run += 1 {
		goodput = max(goodput, runStream())
	}

	if cpuProfileActive {
		pprof.StopCPUProfile()
		cpuProfileFile.Close()
	}
	if writeProfile := pprof.Lookup("allocs"); writeProfile != nil {
		if f, ferr := os.Create(filepath.Join(profileDir, "mctcp_stream_allocs.pprof")); ferr == nil {
			writeProfile.WriteTo(f, 0)
			f.Close()
		}
	}

	fmt.Printf("[mctcp]==== summary ====\n")
	fmt.Printf("[mctcp]tcp stream goodput=%.2f MiB/s (max of %d runs) (cpu profile %s)\n", goodput, mcPerfRunCount, cpuProfilePath)

	if goodput < mcTcpStreamMinGoodput {
		panic(fmt.Errorf("tcp stream goodput too low: %.2f MiB/s", goodput))
	}
}

// mcStack holds the shared full-stack handles a multi-client perf test needs:
// two exchange hosts + connect handlers, a real api server, and a provider
// (client + transport + LocalUserNat + RemoteUserNatProvider) egressing over
// real OS sockets. The device and provider share a network_id, so traffic is
// ProvideMode_Network (loopback egress allowed, network-mode return path).
type mcStack struct {
	apiUrl            string
	devicePlatformUrl string
	deviceByJwt       string
	deviceClientId    server.Id
	providerClientId  server.Id
	waitFor           func(timeout time.Duration, what string, condition func() bool)
}

func setupMcStack(ctx context.Context, label string, wsPort0, wsPort1, apiPort, exchangePort0, exchangePort1 int) (*mcStack, func()) {
	service := "connect"
	block := "test"
	routes := map[string]string{
		label + "0": "127.0.0.1",
		label + "1": "127.0.0.1",
	}

	hostConfigs := []struct {
		host         string
		wsPort       int
		exchangePort int
	}{
		{label + "0", wsPort0, exchangePort0},
		{label + "1", wsPort1, exchangePort1},
	}

	exchanges := []*connectserver.Exchange{}
	httpServers := []*http.Server{}
	for _, hostConfig := range hostConfigs {
		settings := connectserver.DefaultExchangeSettings()
		settings.ConnectionAnnounceTimeout = 0
		settings.ConnectionRateLimitSettings.BurstConnectionCount = 1000
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
		Addr:    fmt.Sprintf("127.0.0.1:%d", apiPort),
		Handler: router.NewRouter(ctx, api.Routes()),
	}
	httpServers = append(httpServers, apiServer)
	go apiServer.ListenAndServe()

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

	apiUrl := fmt.Sprintf("http://127.0.0.1:%d", apiPort)
	devicePlatformUrl := fmt.Sprintf("ws://127.0.0.1:%d", wsPort0)
	providerPlatformUrl := fmt.Sprintf("ws://127.0.0.1:%d", wsPort1)

	// ---- one network: device + provider --------------------------------------

	networkId := server.NewId()
	networkName := label
	userId := server.NewId()
	model.Testing_CreateNetwork(ctx, networkId, networkName, userId)

	providerDeviceId := server.NewId()
	providerClientId := server.NewId()
	providerInstanceId := server.NewId()
	model.Testing_CreateDevice(ctx, networkId, providerDeviceId, providerClientId, "mcprovider", "mcprovider")

	deviceDeviceId := server.NewId()
	deviceClientId := server.NewId()
	model.Testing_CreateDevice(ctx, networkId, deviceDeviceId, deviceClientId, "mcdevice", "mcdevice")

	initialTransferBalance := model.ByteCount(1024) * model.ByteCount(1024) * model.ByteCount(1024) * model.ByteCount(1024)
	balanceCode, err := model.CreateBalanceCode(ctx, initialTransferBalance, 365*24*time.Hour, 0, label+"-1", "", "")
	if err != nil {
		panic(err)
	}
	redeemResult, err := model.RedeemBalanceCode(
		&model.RedeemBalanceCodeArgs{
			Secret:    balanceCode.Secret,
			NetworkId: networkId,
		},
		ctx,
	)
	if err != nil {
		panic(err)
	}
	if redeemResult.Error != nil {
		panic(fmt.Errorf("redeem balance code error = %s", redeemResult.Error.Message))
	}

	providerByJwt := jwt.NewByJwt(networkId, userId, networkName, false, false).Client(providerDeviceId, providerClientId).Sign()
	deviceByJwt := jwt.NewByJwt(networkId, userId, networkName, false, false).Client(deviceDeviceId, deviceClientId).Sign()

	// ---- provider stack ------------------------------------------------------

	providerStrategySettings := connect.DefaultClientStrategySettings()
	providerStrategySettings.EnableResilient = false
	providerStrategy := connect.NewClientStrategy(ctx, providerStrategySettings)

	providerOob := connect.NewApiOutOfBandControl(ctx, providerStrategy, providerByJwt, apiUrl)
	providerClient := connect.NewClient(ctx, connect.Id(providerClientId), providerOob, connect.DefaultClientSettings())

	providerAuth := &connect.ClientAuth{
		ByJwt:      providerByJwt,
		InstanceId: connect.Id(providerInstanceId),
		AppVersion: "0.0.0",
	}
	providerTransport := connect.NewPlatformTransportWithTargetMode(
		ctx,
		providerStrategy,
		providerClient.RouteManager(),
		providerPlatformUrl,
		providerAuth,
		connect.TransportModeH1,
		connect.DefaultPlatformTransportSettings(),
	)

	providerLocalNat := connect.NewLocalUserNatWithDefaults(providerClient.Ctx(), providerClientId.String())
	providerRemoteNat := connect.NewRemoteUserNatProviderWithDefaults(providerClient, providerLocalNat)

	providerClient.ContractManager().SetProvideModesWithReturnTraffic(map[protocol.ProvideMode]bool{
		protocol.ProvideMode_Network: true,
		protocol.ProvideMode_Public:  true,
	})

	waitFor(30*time.Second, "provider provide registered", func() bool {
		modes, err := model.GetProvideModes(ctx, providerClientId)
		return err == nil && 0 < len(modes)
	})
	fmt.Printf("[mcperf]provider provide registered\n")

	cleanup := func() {
		providerRemoteNat.Close()
		providerLocalNat.Close()
		providerTransport.Close()
		providerClient.Close()
		for _, httpServer := range httpServers {
			httpServer.Close()
		}
		for _, exchange := range exchanges {
			exchange.Close()
		}
	}

	return &mcStack{
		apiUrl:            apiUrl,
		devicePlatformUrl: devicePlatformUrl,
		deviceByJwt:       deviceByJwt,
		deviceClientId:    deviceClientId,
		providerClientId:  providerClientId,
		waitFor:           waitFor,
	}, cleanup
}

func testConnectMultiClientPerformance(t testing.TB) {
	if testing.Short() {
		return
	}

	os.Setenv("WARP_SERVICE", "test")
	os.Setenv("WARP_BLOCK", "test")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	fmt.Printf("[mcperf]go=%s cpus=%d gomaxprocs=%d\n", runtime.Version(), runtime.NumCPU(), runtime.GOMAXPROCS(0))

	stack, cleanup := setupMcStack(ctx, "mcperf", mcWsPort0, mcWsPort1, mcApiPort, mcExchangePort0, mcExchangePort1)
	defer cleanup()
	apiUrl := stack.apiUrl
	devicePlatformUrl := stack.devicePlatformUrl
	deviceByJwt := stack.deviceByJwt
	deviceClientId := stack.deviceClientId
	providerClientId := stack.providerClientId

	// ---- local udp echo server --------------------------------------------------

	// the device and provider share a network_id, so traffic is
	// ProvideMode_Network and the provider echoes that mode on the return
	// path, which bypasses the device ingress public rules (including the
	// p2p source-port guard). An ephemeral server port is therefore fine.
	echoConn, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		panic(err)
	}
	defer echoConn.Close()
	echoPort := echoConn.LocalAddr().(*net.UDPAddr).Port
	var echoServerRecvCount int64
	go func() {
		buffer := make([]byte, 4096)
		for {
			n, addr, err := echoConn.ReadFrom(buffer)
			if err != nil {
				return
			}
			atomic.AddInt64(&echoServerRecvCount, 1)
			echoConn.WriteTo(buffer[0:n], addr)
		}
	}()
	echoIp := net.IPv4(127, 0, 0, 1)
	fmt.Printf("[mcperf]udp echo server 127.0.0.1:%d\n", echoPort)

	// ---- local udp blast server ---------------------------------------------------
	// a request to this server triggers a paced blast of udp packets back to
	// the requester: the download direction of the user stack

	blastConn, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		panic(err)
	}
	defer blastConn.Close()
	blastPort := blastConn.LocalAddr().(*net.UDPAddr).Port
	var blastRequestCount int64
	go func() {
		request := make([]byte, 64)
		payload := make([]byte, mcDownloadPayloadByteCount)
		for {
			_, addr, err := blastConn.ReadFrom(request)
			if err != nil {
				return
			}
			// only the first request triggers a blast; later requests (the
			// device retries until the download starts) are drained below
			if atomic.AddInt64(&blastRequestCount, 1) != 1 {
				continue
			}
			packetCount := mcDownloadTotalByteCount / mcDownloadPayloadByteCount
			chunkByteCount := 0
			for i := 0; i < packetCount; i += 1 {
				binary.BigEndian.PutUint64(payload[0:8], uint64(i))
				binary.BigEndian.PutUint64(payload[8:16], uint64(time.Now().UnixNano()))
				if _, err := blastConn.WriteTo(payload, addr); err != nil {
					break
				}
				chunkByteCount += len(payload)
				if mcDownloadPaceChunkByteCount <= chunkByteCount {
					chunkByteCount = 0
					time.Sleep(1 * time.Millisecond)
				}
			}
		}
	}()
	fmt.Printf("[mcperf]udp blast server 127.0.0.1:%d\n", blastPort)

	// ---- device multi client ----------------------------------------------------

	deviceStrategySettings := connect.DefaultClientStrategySettings()
	deviceStrategySettings.EnableResilient = false
	deviceStrategy := connect.NewClientStrategy(ctx, deviceStrategySettings)

	providerClientIdConnect := connect.Id(providerClientId)
	deviceClientIdConnect := connect.Id(deviceClientId)
	specs := []*connect.ProviderSpec{
		{ClientId: &providerClientIdConnect},
	}
	generator := connect.NewApiMultiClientGeneratorWithDefaults(
		ctx,
		specs,
		deviceStrategy,
		nil,
		apiUrl,
		deviceByJwt,
		devicePlatformUrl,
		"mcperf",
		"mcperf",
		"0.0.0",
		&deviceClientIdConnect,
	)

	var echoCount int64
	var echoByteCount int64
	var lastEchoNanos int64
	echoRtts := make(chan time.Duration, 1<<16)
	echoSeqs := make(chan uint64, 1<<16)

	var downloadCount int64
	var downloadByteCount int64
	var downloadFirstNanos int64
	var downloadLastNanos int64

	receivePacket := func(source connect.TransferPath, provideMode protocol.ProvideMode, ipPath *connect.IpPath, packet []byte) {
		payload, payloadOk := mcUdp4Payload(packet)
		if !payloadOk || len(payload) < mcPayloadHeaderByteCount {
			return
		}
		// the multi client reverses ipPath before this callback, so the blast
		// server port can land in either field; match both
		if ipPath != nil && (ipPath.SourcePort == blastPort || ipPath.DestinationPort == blastPort) {
			now := time.Now().UnixNano()
			atomic.CompareAndSwapInt64(&downloadFirstNanos, 0, now)
			atomic.StoreInt64(&downloadLastNanos, now)
			atomic.AddInt64(&downloadCount, 1)
			atomic.AddInt64(&downloadByteCount, int64(len(payload)))
			return
		}
		seq := binary.BigEndian.Uint64(payload[0:8])
		sendNanos := int64(binary.BigEndian.Uint64(payload[8:16]))
		atomic.AddInt64(&echoCount, 1)
		atomic.AddInt64(&echoByteCount, int64(len(payload)))
		atomic.StoreInt64(&lastEchoNanos, time.Now().UnixNano())
		select {
		case echoRtts <- time.Duration(time.Now().UnixNano() - sendNanos):
		default:
		}
		select {
		case echoSeqs <- seq:
		default:
		}
	}

	multiClient := connect.NewRemoteUserNatMultiClientWithDefaults(
		ctx,
		generator,
		receivePacket,
		protocol.ProvideMode_Network,
	)
	defer multiClient.Close()

	deviceSource := connect.SourceId(deviceClientIdConnect)
	deviceIp := net.IPv4(72, 0, 0, 1)

	var sendSeq uint64
	sendPacketDetailed := func(sourcePort int, payloadByteCount int) (uint64, bool) {
		seq := sendSeq
		sendSeq += 1
		payload := make([]byte, max(payloadByteCount, mcPayloadHeaderByteCount))
		binary.BigEndian.PutUint64(payload[0:8], seq)
		binary.BigEndian.PutUint64(payload[8:16], uint64(time.Now().UnixNano()))
		packet := mcUdp4Packet(deviceIp, sourcePort, echoIp, echoPort, payload)
		success := multiClient.SendPacket(deviceSource, protocol.ProvideMode_Network, packet, -1)
		return seq, success
	}
	sendPacket := func(sourcePort int, payloadByteCount int) uint64 {
		seq, _ := sendPacketDetailed(sourcePort, payloadByteCount)
		return seq
	}

	drainEchoes := func() {
		for {
			select {
			case <-echoRtts:
			case <-echoSeqs:
			default:
				return
			}
		}
	}

	// ---- phase 1: first packet (stack cold start) -------------------------------

	startTime := time.Now()
	firstEcho := func() time.Duration {
		deadline := time.Now().Add(120 * time.Second)
		for {
			_, sendOk := sendPacketDetailed(40000, 64)
			if !sendOk {
				fmt.Printf("[mcperf]bootstrap send dropped (no window yet)\n")
			}
			select {
			case <-ctx.Done():
				panic(fmt.Errorf("done waiting for first echo"))
			case <-echoSeqs:
				return time.Since(startTime)
			case <-time.After(500 * time.Millisecond):
			}
			if deadline.Before(time.Now()) {
				panic(fmt.Errorf(
					"timeout waiting for first echo (echo server received %d packets)",
					atomic.LoadInt64(&echoServerRecvCount),
				))
			}
		}
	}
	firstEchoTime := firstEcho()
	fmt.Printf("[mcperf]first echoed packet = %s (multi client cold start)\n", firstEchoTime)
	// let stragglers from the bootstrap probes settle
	select {
	case <-time.After(1 * time.Second):
	}
	drainEchoes()

	// ---- phase 2: packet echo rtt ------------------------------------------------

	rtts := make([]time.Duration, 0, mcPingPongCount)
	for i := 0; i < mcPingPongWarmup+mcPingPongCount; i += 1 {
		sendPacket(40001, 64)
		select {
		case rtt := <-echoRtts:
			if mcPingPongWarmup <= i {
				rtts = append(rtts, rtt)
			}
		case <-time.After(30 * time.Second):
			panic(fmt.Errorf("timeout waiting for echo %d", i))
		}
	}
	drainEchoes()
	fmt.Printf(
		"[mcperf]echo rtt n=%d p50=%s p90=%s p99=%s max=%s\n",
		len(rtts),
		perfPercentile(rtts, 0.50),
		perfPercentile(rtts, 0.90),
		perfPercentile(rtts, 0.99),
		perfPercentile(rtts, 1.0),
	)

	// ---- phase 3: udp throughput --------------------------------------------------
	// the blast is run several times and the run with the best delivery is
	// taken, to ride out host scheduling noise on the build server.

	payloadByteCount := 1400
	sourcePorts := []int{41000, 41001, 41002, 41003}

	// profile the single-client send/receive path across the throughput runs
	profileDir := "profile"
	os.MkdirAll(profileDir, 0755)
	cpuProfilePath := filepath.Join(profileDir, "mcperf_tput_cpu.pprof")
	cpuProfileFile, err := os.Create(cpuProfilePath)
	if err != nil {
		panic(err)
	}
	cpuProfileActive := pprof.StartCPUProfile(cpuProfileFile) == nil
	runtime.SetBlockProfileRate(10 * 1000) // sample blocking >= 10us
	runtime.SetMutexProfileFraction(5)

	type mcUdpThroughputResult struct {
		delivered         int64
		deliveredFraction float64
		elapsed           time.Duration
	}

	// runUdpThroughput floods the echo path for mcBlastDuration, waits for the
	// echoes to settle, and reports the delivered fraction and goodput.
	runUdpThroughput := func() mcUdpThroughputResult {
		echoCountStart := atomic.LoadInt64(&echoCount)
		var memStatsStart runtime.MemStats
		runtime.ReadMemStats(&memStatsStart)

		blastRtts := make([]time.Duration, 0, 1<<18)
		blastStart := time.Now()
		sent := int64(0)
		for time.Since(blastStart) < mcBlastDuration {
			sendPacket(sourcePorts[int(sent)%len(sourcePorts)], payloadByteCount)
			sent += 1
			// collect rtts opportunistically to keep the channel from overflowing
			for {
				select {
				case rtt := <-echoRtts:
					blastRtts = append(blastRtts, rtt)
					continue
				default:
				}
				break
			}
		}
		sendElapsed := time.Since(blastStart)

		var memStatsEnd runtime.MemStats
		runtime.ReadMemStats(&memStatsEnd)

		// wait for echoes to settle
		stableCount := 0
		lastEchoCount := atomic.LoadInt64(&echoCount)
		for stableCount < 8 {
			select {
			case <-time.After(250 * time.Millisecond):
			}
			for {
				select {
				case rtt := <-echoRtts:
					blastRtts = append(blastRtts, rtt)
					continue
				default:
				}
				break
			}
			currentEchoCount := atomic.LoadInt64(&echoCount)
			if currentEchoCount == lastEchoCount {
				stableCount += 1
			} else {
				stableCount = 0
				lastEchoCount = currentEchoCount
			}
		}

		delivered := atomic.LoadInt64(&echoCount) - echoCountStart
		elapsed := time.Unix(0, atomic.LoadInt64(&lastEchoNanos)).Sub(blastStart)
		if elapsed <= 0 {
			elapsed = sendElapsed
		}
		deliveredFraction := float64(delivered) / float64(max(sent, 1))
		fmt.Printf(
			"[mcperf]udp throughput payload=%d sent=%d (%.0f pkt/s) echoed=%d (%.1f%%) elapsed=%.2fs goodput=%.2f MiB/s rtt p50=%s p90=%s p99=%s max=%s\n",
			payloadByteCount,
			sent,
			float64(sent)/sendElapsed.Seconds(),
			delivered,
			100*deliveredFraction,
			elapsed.Seconds(),
			(float64(delivered)*float64(payloadByteCount))/(1024*1024)/elapsed.Seconds(),
			perfPercentile(blastRtts, 0.50),
			perfPercentile(blastRtts, 0.90),
			perfPercentile(blastRtts, 0.99),
			perfPercentile(blastRtts, 1.0),
		)
		fmt.Printf(
			"[mcperf]tput allocs/pkt=%dB gc=%d pause=%s (cpu profile %s)\n",
			int64(memStatsEnd.TotalAlloc-memStatsStart.TotalAlloc)/max(sent, 1),
			memStatsEnd.NumGC-memStatsStart.NumGC,
			time.Duration(memStatsEnd.PauseTotalNs-memStatsStart.PauseTotalNs),
			cpuProfilePath,
		)
		return mcUdpThroughputResult{
			delivered:         delivered,
			deliveredFraction: deliveredFraction,
			elapsed:           elapsed,
		}
	}

	var throughput mcUdpThroughputResult
	for run := 0; run < mcPerfRunCount; run += 1 {
		r := runUdpThroughput()
		if run == 0 || throughput.deliveredFraction < r.deliveredFraction {
			throughput = r
		}
	}

	if cpuProfileActive {
		pprof.StopCPUProfile()
		cpuProfileFile.Close()
	}
	for _, name := range []string{"allocs", "mutex", "block"} {
		if writeProfile := pprof.Lookup(name); writeProfile != nil {
			if f, ferr := os.Create(filepath.Join(profileDir, fmt.Sprintf("mcperf_tput_%s.pprof", name))); ferr == nil {
				writeProfile.WriteTo(f, 0)
				f.Close()
			}
		}
	}
	runtime.SetBlockProfileRate(0)
	runtime.SetMutexProfileFraction(0)

	// the flood outruns the stack, so most packets drop; this floor only catches
	// a path that drops ~everything (a relay/nat collapse), not a perf regression.
	if throughput.deliveredFraction < mcUdpDeliveryMinFraction {
		panic(fmt.Errorf("udp delivery too low: %.1f%%", 100*throughput.deliveredFraction))
	}

	// ---- phase 4: download blast ---------------------------------------------------
	// run several times and take the max goodput, to ride out host scheduling
	// noise on the build server.

	// runDownloadBlast resets the per-run download counters, requests a fresh
	// blast (retrying until it starts), waits for it to settle, and reports the
	// delivered download goodput in MiB/s. The blast server fires once per
	// request burst, so resetting blastRequestCount re-arms it for the next run.
	runDownloadBlast := func() float64 {
		atomic.StoreInt64(&blastRequestCount, 0)
		atomic.StoreInt64(&downloadCount, 0)
		atomic.StoreInt64(&downloadByteCount, 0)
		atomic.StoreInt64(&downloadFirstNanos, 0)
		atomic.StoreInt64(&downloadLastNanos, 0)

		// request the blast, retrying until it starts (a single request packet can
		// be dropped while the path drains the prior flood)
		requestPayload := make([]byte, mcPayloadHeaderByteCount)
		blastDeadline := time.Now().Add(30 * time.Second)
		for atomic.LoadInt64(&downloadCount) == 0 {
			if blastDeadline.Before(time.Now()) {
				panic(fmt.Errorf(
					"timeout waiting for download blast start (blast server received %d requests)",
					atomic.LoadInt64(&blastRequestCount),
				))
			}
			requestPacket := mcUdp4Packet(deviceIp, 10000, echoIp, blastPort, requestPayload)
			multiClient.SendPacket(deviceSource, protocol.ProvideMode_Network, requestPacket, -1)
			select {
			case <-time.After(500 * time.Millisecond):
			}
		}

		// wait for the blast to settle
		stableCount := 0
		lastDownloadCount := atomic.LoadInt64(&downloadCount)
		for stableCount < 8 {
			select {
			case <-time.After(250 * time.Millisecond):
			}
			currentDownloadCount := atomic.LoadInt64(&downloadCount)
			if currentDownloadCount == lastDownloadCount {
				stableCount += 1
			} else {
				stableCount = 0
				lastDownloadCount = currentDownloadCount
			}
		}

		downloadReceived := atomic.LoadInt64(&downloadCount)
		downloadReceivedByteCount := atomic.LoadInt64(&downloadByteCount)
		downloadElapsed := time.Duration(atomic.LoadInt64(&downloadLastNanos) - atomic.LoadInt64(&downloadFirstNanos))
		if downloadElapsed <= 0 {
			downloadElapsed = time.Millisecond
		}
		downloadExpectedCount := int64(mcDownloadTotalByteCount / mcDownloadPayloadByteCount)
		downloadGoodput := float64(downloadReceivedByteCount) / (1024 * 1024) / downloadElapsed.Seconds()
		fmt.Printf(
			"[mcperf]download blast total=%dMiB payload=%d received=%d/%d (%.1f%%) elapsed=%.2fs goodput=%.2f MiB/s\n",
			mcDownloadTotalByteCount/(1024*1024),
			mcDownloadPayloadByteCount,
			downloadReceived,
			downloadExpectedCount,
			100*float64(downloadReceived)/float64(downloadExpectedCount),
			downloadElapsed.Seconds(),
			downloadGoodput,
		)
		return downloadGoodput
	}

	downloadGoodput := 0.0
	for run := 0; run < mcPerfRunCount; run += 1 {
		downloadGoodput = max(downloadGoodput, runDownloadBlast())
	}
	// sanity: the stack should sustain at least a modest download rate.
	// loss against the paced blast is expected (the kernel sheds the excess
	// at the nat ingress socket); the goodput is the measurement.
	if downloadGoodput < 1 {
		panic(fmt.Errorf("download goodput too low: %.2f MiB/s", downloadGoodput))
	}

	fmt.Printf("[mcperf]==== summary ====\n")
	fmt.Printf("[mcperf]first packet = %s\n", firstEchoTime)
	fmt.Printf(
		"[mcperf]echo rtt p50=%s p99=%s\n",
		perfPercentile(rtts, 0.50),
		perfPercentile(rtts, 0.99),
	)
	fmt.Printf(
		"[mcperf]udp echo goodput=%.2f MiB/s delivered=%.1f%%\n",
		(float64(throughput.delivered)*float64(payloadByteCount))/(1024*1024)/throughput.elapsed.Seconds(),
		100*throughput.deliveredFraction,
	)
	fmt.Printf("[mcperf]download goodput=%.2f MiB/s\n", downloadGoodput)
}

// mcUdp4Packet serializes an ipv4+udp packet. The udp checksum is left unset
// (0), which is valid for udp over ipv4.
func mcUdp4Packet(sourceIp net.IP, sourcePort int, destinationIp net.IP, destinationPort int, payload []byte) []byte {
	udpByteCount := 8 + len(payload)
	totalByteCount := 20 + udpByteCount
	packet := make([]byte, totalByteCount)

	// ipv4 header
	packet[0] = 0x45
	binary.BigEndian.PutUint16(packet[2:4], uint16(totalByteCount))
	packet[8] = 64
	packet[9] = 17
	copy(packet[12:16], sourceIp.To4())
	copy(packet[16:20], destinationIp.To4())
	var sum uint32
	for i := 0; i < 20; i += 2 {
		sum += uint32(binary.BigEndian.Uint16(packet[i : i+2]))
	}
	for 0xffff < sum {
		sum = (sum & 0xffff) + (sum >> 16)
	}
	binary.BigEndian.PutUint16(packet[10:12], ^uint16(sum))

	// udp header
	binary.BigEndian.PutUint16(packet[20:22], uint16(sourcePort))
	binary.BigEndian.PutUint16(packet[22:24], uint16(destinationPort))
	binary.BigEndian.PutUint16(packet[24:26], uint16(udpByteCount))
	copy(packet[28:], payload)
	return packet
}

// mcUdp4Payload extracts the udp payload from an ipv4 packet
func mcUdp4Payload(packet []byte) ([]byte, bool) {
	if len(packet) < 20 || packet[0]>>4 != 4 {
		return nil, false
	}
	headerByteCount := int(packet[0]&0xf) * 4
	if packet[9] != 17 || len(packet) < headerByteCount+8 {
		return nil, false
	}
	udpByteCount := int(binary.BigEndian.Uint16(packet[headerByteCount+4 : headerByteCount+6]))
	if udpByteCount < 8 || len(packet) < headerByteCount+udpByteCount {
		return nil, false
	}
	return packet[headerByteCount+8 : headerByteCount+udpByteCount], true
}
