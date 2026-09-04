// This file records comparable client-to-client H1 exchange, H3 exchange,
// legacy P2P, and fast P2P performance through a real server topology. P2P
// uses the production direct-IP NoAck semantics; exchange retains Transfer
// acknowledgement and retry.
package connect

import (
	"context"
	"encoding/binary"
	"os"
	"runtime"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	clientconnect "github.com/urnetwork/connect"
	"github.com/urnetwork/connect/protocol"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

const (
	streamRoutePerformancePayloadByteCount = 1380
	streamRoutePerformancePacketCount      = 16 * 1024
	streamRoutePerformanceRunCount         = 5
	streamRoutePerformanceMinimumFastGain  = 1.5
	// Direct IP has an inner congestion window. The synthetic raw-frame source
	// needs the same bounded in-flight shape or it can outpace every receive
	// queue and measure intentional overload drops instead of throughput.
	streamRoutePerformanceMaximumInFlight = 64
)

// One immutable run is published to the nonblocking receive callback. The
// marker prevents a late packet from an earlier run entering the next result.
type streamRoutePerformanceReceiveRun struct {
	markerByte           byte
	targetPacketCount    int64
	receivedPacketCount  atomic.Int64
	receivedByteCount    atomic.Int64
	duplicatePacketCount atomic.Int64
	invalidIndexCount    atomic.Int64
	receivedIndexWords   []atomic.Uint64
	done                 chan struct{}
	credit               chan struct{}
}

// One receiver is attached for a phase and swaps run counters without
// replacing the client's callback.
type streamRoutePerformanceReceiver struct {
	sourceClientId clientconnect.Id
	currentRun     atomic.Pointer[streamRoutePerformanceReceiveRun]
}

// The callback only performs bounded parsing and atomics; it never waits on
// the measurement goroutine.
func newStreamRoutePerformanceReceiver(
	client *clientconnect.Client,
	sourceClientId server.Id,
) (*streamRoutePerformanceReceiver, func()) {
	receiver := &streamRoutePerformanceReceiver{
		sourceClientId: clientconnect.Id(sourceClientId),
	}
	unsub := client.AddReceiveCallback(receiver.receive)
	return receiver, unsub
}

// Only raw performance frames from the expected peer enter the active run.
func (self *streamRoutePerformanceReceiver) receive(
	source clientconnect.TransferPath,
	frames []*protocol.Frame,
	peer clientconnect.Peer,
) {
	_ = peer
	if source.SourceId != self.sourceClientId {
		return
	}
	receiveRun := self.currentRun.Load()
	if receiveRun == nil {
		return
	}
	for _, frame := range frames {
		if frame.MessageType != protocol.MessageType_TestSimpleMessage ||
			len(frame.MessageBytes) != streamRoutePerformancePayloadByteCount ||
			frame.MessageBytes[0] != receiveRun.markerByte {
			continue
		}
		packetIndex := binary.BigEndian.Uint64(frame.MessageBytes[1:9])
		if uint64(receiveRun.targetPacketCount) <= packetIndex {
			receiveRun.invalidIndexCount.Add(1)
			continue
		}
		wordIndex := packetIndex / 64
		bit := uint64(1) << (packetIndex % 64)
		if receiveRun.receivedIndexWords[wordIndex].Or(bit)&bit != 0 {
			receiveRun.duplicatePacketCount.Add(1)
			continue
		}
		receiveRun.receivedByteCount.Add(int64(len(frame.MessageBytes)))
		if receiveRun.credit != nil {
			select {
			case receiveRun.credit <- struct{}{}:
			default:
			}
		}
		if receiveRun.receivedPacketCount.Add(1) == receiveRun.targetPacketCount {
			close(receiveRun.done)
		}
	}
}

// A fresh marker and counters isolate one warmup or measured transfer.
func (self *streamRoutePerformanceReceiver) begin(
	markerByte byte,
	targetPacketCount int,
	maximumInFlightCount int,
) *streamRoutePerformanceReceiveRun {
	receiveRun := &streamRoutePerformanceReceiveRun{
		markerByte:         markerByte,
		targetPacketCount:  int64(targetPacketCount),
		receivedIndexWords: make([]atomic.Uint64, (targetPacketCount+63)/64),
		done:               make(chan struct{}),
	}
	if 0 < maximumInFlightCount {
		receiveRun.credit = make(chan struct{}, maximumInFlightCount)
		for range maximumInFlightCount {
			receiveRun.credit <- struct{}{}
		}
	}
	if !self.currentRun.CompareAndSwap(nil, receiveRun) {
		panic("stream route performance receive run already active")
	}
	return receiveRun
}

// The exact packet and byte totals make loss or duplication a correctness
// failure instead of allowing it to inflate the throughput result.
func (self *streamRoutePerformanceReceiver) finish(
	t testing.TB,
	receiveRun *streamRoutePerformanceReceiveRun,
) (int64, int64) {
	t.Helper()
	select {
	case <-receiveRun.done:
	case <-time.After(2 * time.Minute):
		t.Fatalf(
			"route performance receive timeout: packets=%d/%d bytes=%d",
			receiveRun.receivedPacketCount.Load(),
			receiveRun.targetPacketCount,
			receiveRun.receivedByteCount.Load(),
		)
	}
	if !self.currentRun.CompareAndSwap(receiveRun, nil) {
		t.Fatal("route performance receive run changed before completion")
	}
	packetCount := receiveRun.receivedPacketCount.Load()
	byteCount := receiveRun.receivedByteCount.Load()
	if packetCount != receiveRun.targetPacketCount {
		t.Fatalf(
			"route performance packets=%d want=%d",
			packetCount,
			receiveRun.targetPacketCount,
		)
	}
	if duplicatePacketCount := receiveRun.duplicatePacketCount.Load(); duplicatePacketCount != 0 {
		t.Fatalf("route performance duplicate packets=%d", duplicatePacketCount)
	}
	if invalidIndexCount := receiveRun.invalidIndexCount.Load(); invalidIndexCount != 0 {
		t.Fatalf("route performance invalid packet indexes=%d", invalidIndexCount)
	}
	expectedByteCount := receiveRun.targetPacketCount *
		streamRoutePerformancePayloadByteCount
	if byteCount != expectedByteCount {
		t.Fatalf(
			"route performance bytes=%d want=%d",
			byteCount,
			expectedByteCount,
		)
	}
	return packetCount, byteCount
}

// A duplicate cannot substitute for a missing encoded packet index in the
// exact-delivery gate used by every measured route.
func TestStreamRoutePerformanceReceiverTracksUniquePackets(t *testing.T) {
	sourceClientId := clientconnect.NewId()
	receiver := &streamRoutePerformanceReceiver{sourceClientId: sourceClientId}
	receiveRun := receiver.begin(7, 2, 0)
	makeFrame := func(packetIndex uint64) *protocol.Frame {
		messageBytes := make([]byte, streamRoutePerformancePayloadByteCount)
		messageBytes[0] = 7
		binary.BigEndian.PutUint64(messageBytes[1:9], packetIndex)
		return &protocol.Frame{
			MessageType:  protocol.MessageType_TestSimpleMessage,
			MessageBytes: messageBytes,
			Raw:          true,
		}
	}
	source := clientconnect.SourceId(sourceClientId)
	receiver.receive(
		source,
		[]*protocol.Frame{makeFrame(0), makeFrame(0), makeFrame(2)},
		clientconnect.Peer{},
	)
	if receiveRun.receivedPacketCount.Load() != 1 ||
		receiveRun.duplicatePacketCount.Load() != 1 ||
		receiveRun.invalidIndexCount.Load() != 1 {
		t.Fatalf(
			"unique=%d duplicate=%d invalid=%d want=1/1/1",
			receiveRun.receivedPacketCount.Load(),
			receiveRun.duplicatePacketCount.Load(),
			receiveRun.invalidIndexCount.Load(),
		)
	}
	select {
	case <-receiveRun.done:
		t.Fatal("duplicate completed the exact-delivery gate")
	default:
	}
	receiver.receive(source, []*protocol.Frame{makeFrame(1)}, clientconnect.Peer{})
	select {
	case <-receiveRun.done:
	case <-time.After(time.Second):
		t.Fatal("second unique packet did not complete the gate")
	}
	if !receiver.currentRun.CompareAndSwap(receiveRun, nil) {
		t.Fatal("test receive run changed unexpectedly")
	}
}

// Each result measures useful payload from the first send until exact delivery
// and records process-wide allocation deltas for regression comparison.
type streamRoutePerformanceResult struct {
	duration           time.Duration
	packetCount        int64
	byteCount          int64
	allocatedByteCount uint64
	allocationCount    uint64
}

// The aggregate keeps machine-readable values for the fast-versus-legacy
// assertion while each individual run remains visible in the test log.
type streamRoutePerformanceSummary struct {
	medianThroughput float64
	worstThroughput  float64
}

// Every send uses the production Transfer, route, carrier, and receive path.
// The supplied options select the route's production recovery policy. Exact
// receive totals make loss or duplication a correctness failure instead of a
// favorable performance artifact.
func measureStreamRoutePerformanceRun(
	t testing.TB,
	sender *clientconnect.Client,
	destinationClientId server.Id,
	receiver *streamRoutePerformanceReceiver,
	markerByte byte,
	packetCount int,
	transferOptions []any,
) streamRoutePerformanceResult {
	t.Helper()
	maximumInFlightCount := 0
	if 0 < len(transferOptions) {
		maximumInFlightCount = streamRoutePerformanceMaximumInFlight
	}
	receiveRun := receiver.begin(
		markerByte,
		packetCount,
		maximumInFlightCount,
	)
	creditTimer := time.NewTimer(0)
	if !creditTimer.Stop() {
		select {
		case <-creditTimer.C:
		default:
		}
	}
	defer creditTimer.Stop()

	var memoryBefore runtime.MemStats
	runtime.ReadMemStats(&memoryBefore)
	startTime := time.Now()
	for packetIndex := range packetCount {
		if receiveRun.credit != nil {
			select {
			case <-receiveRun.credit:
			default:
				creditTimer.Reset(2 * time.Minute)
				select {
				case <-receiveRun.credit:
					if !creditTimer.Stop() {
						select {
						case <-creditTimer.C:
						default:
						}
					}
				case <-creditTimer.C:
					t.Fatalf(
						"route performance credit timeout: send=%d/%d receive=%d",
						packetIndex,
						packetCount,
						receiveRun.receivedPacketCount.Load(),
					)
				}
			}
		}
		messageBytes := clientconnect.MessagePoolGet(
			streamRoutePerformancePayloadByteCount,
		)
		messageBytes[0] = markerByte
		binary.BigEndian.PutUint64(messageBytes[1:9], uint64(packetIndex))
		clear(messageBytes[9:])
		frame := &protocol.Frame{
			MessageType:  protocol.MessageType_TestSimpleMessage,
			MessageBytes: messageBytes,
			Raw:          true,
		}
		sent := sender.SendWithTimeout(
			frame,
			clientconnect.Id(destinationClientId),
			nil,
			60*time.Second,
			transferOptions...,
		)
		if !sent {
			clientconnect.MessagePoolReturn(messageBytes)
			t.Fatalf("route performance send %d/%d failed", packetIndex, packetCount)
		}
	}
	receivedPacketCount, receivedByteCount := receiver.finish(t, receiveRun)
	duration := time.Since(startTime)
	var memoryAfter runtime.MemStats
	runtime.ReadMemStats(&memoryAfter)

	return streamRoutePerformanceResult{
		duration:           duration,
		packetCount:        receivedPacketCount,
		byteCount:          receivedByteCount,
		allocatedByteCount: memoryAfter.TotalAlloc - memoryBefore.TotalAlloc,
		allocationCount:    memoryAfter.Mallocs - memoryBefore.Mallocs,
	}
}

// Five runs report a median and worst-of-five throughput while retaining the
// allocation evidence for every individual run in the test log.
func measureStreamRoutePerformance(
	t testing.TB,
	routeName string,
	sender *clientconnect.Client,
	destinationClientId server.Id,
	receiver *streamRoutePerformanceReceiver,
	markerByte *byte,
	packetCount int,
	runCount int,
	transferOptions []any,
) streamRoutePerformanceSummary {
	t.Helper()
	warmupPacketCount := min(packetCount, 256)
	measureStreamRoutePerformanceRun(
		t,
		sender,
		destinationClientId,
		receiver,
		*markerByte,
		warmupPacketCount,
		transferOptions,
	)
	*markerByte += 1

	throughputs := make([]float64, 0, runCount)
	for runIndex := range runCount {
		result := measureStreamRoutePerformanceRun(
			t,
			sender,
			destinationClientId,
			receiver,
			*markerByte,
			packetCount,
			transferOptions,
		)
		*markerByte += 1
		throughput := float64(result.byteCount) /
			result.duration.Seconds() /
			1_000_000
		throughputs = append(throughputs, throughput)
		t.Logf(
			"[stream-route-perf] route=%s run=%d useful=%.2f MB/s packets=%d bytes=%d duration=%s allocs/packet=%.2f allocated-bytes/packet=%.2f",
			routeName,
			runIndex+1,
			throughput,
			result.packetCount,
			result.byteCount,
			result.duration,
			float64(result.allocationCount)/float64(result.packetCount),
			float64(result.allocatedByteCount)/float64(result.packetCount),
		)
	}
	slices.Sort(throughputs)
	medianThroughput := throughputs[len(throughputs)/2]
	worstThroughput := throughputs[0]
	t.Logf(
		"[stream-route-perf] route=%s median=%.2f MB/s worst=%.2f MB/s runs=%d",
		routeName,
		medianThroughput,
		worstThroughput,
		len(throughputs),
	)
	return streamRoutePerformanceSummary{
		medianThroughput: medianThroughput,
		worstThroughput:  worstThroughput,
	}
}

// newStreamRoutePerformanceClient creates a hermetic native client with one
// observable, forced P2P data plane shared by all of its stream hops.
func newStreamRoutePerformanceClient(
	env *peerDiscoveryEnv,
	clientId server.Id,
	dataPlaneMode clientconnect.P2pDataPlaneMode,
) (*clientconnect.Client, *clientconnect.P2pDataPlaneStats) {
	settings := newPeerDiscoveryClientSettings()
	stats := &clientconnect.P2pDataPlaneStats{}
	p2pSettings := settings.StreamManagerSettings.
		StreamBufferSettings.
		P2pTransportSettings
	p2pSettings.DataPlaneMode = dataPlaneMode
	p2pSettings.DataPlaneStats = stats
	client := clientconnect.NewClient(
		env.ctx,
		clientconnect.Id(clientId),
		Testing_NewControllerOutOfBandControl(
			env.ctx,
			clientId,
			settings.ContractManagerSettings,
		),
		settings,
	)
	return client, stats
}

// assertStreamRoutePerformanceDataPlane rejects fallback or a mislabeled
// forced result by examining both directions of both endpoint clients.
func assertStreamRoutePerformanceDataPlane(
	t testing.TB,
	mode clientconnect.P2pDataPlaneMode,
	sourceStats *clientconnect.P2pDataPlaneStats,
	destinationStats *clientconnect.P2pDataPlaneStats,
) {
	t.Helper()
	source := sourceStats.Snapshot()
	destination := destinationStats.Snapshot()
	if source.FastFallbackCount != 0 || destination.FastFallbackCount != 0 {
		t.Fatalf(
			"forced P2P route fell back: source=%+v destination=%+v",
			source,
			destination,
		)
	}
	switch mode {
	case clientconnect.P2pDataPlaneModeLegacyOnly:
		if source.LegacySendMessageCount == 0 ||
			destination.LegacyReceiveMessageCount == 0 ||
			source.FastSendMessageCount != 0 ||
			destination.FastReceiveMessageCount != 0 {
			t.Fatalf(
				"forced legacy route used the wrong carrier: source=%+v destination=%+v",
				source,
				destination,
			)
		}
	case clientconnect.P2pDataPlaneModeFastOnly:
		if source.FastSendMessageCount == 0 ||
			destination.FastReceiveMessageCount == 0 ||
			source.LegacySendMessageCount != 0 ||
			destination.LegacyReceiveMessageCount != 0 ||
			source.FastDropCount != 0 ||
			destination.FastDropCount != 0 {
			t.Fatalf(
				"forced fast route used the wrong carrier: source=%+v destination=%+v",
				source,
				destination,
			)
		}
	default:
		t.Fatalf("unexpected forced data-plane mode %d", mode)
	}
}

// measureStreamRouteP2p establishes a fresh real stream, removes its platform
// fallback, measures payload through Transfer and WebRTC, and verifies the
// carrier used in both directions.
func measureStreamRouteP2p(
	t testing.TB,
	env *peerDiscoveryEnv,
	mode clientconnect.P2pDataPlaneMode,
	routeName string,
	markerByte *byte,
	packetCount int,
	runCount int,
) streamRoutePerformanceSummary {
	t.Helper()
	ctx := env.ctx
	sourceClientId, sourceJwt := env.authClient(&model.AuthNetworkClientArgs{
		Description: routeName + " source",
	})
	destinationClientId, destinationJwt := env.authClient(&model.AuthNetworkClientArgs{
		Description: routeName + " destination",
	})
	source, sourceStats := newStreamRoutePerformanceClient(env, sourceClientId, mode)
	destination, destinationStats := newStreamRoutePerformanceClient(
		env,
		destinationClientId,
		mode,
	)
	defer source.Close()
	defer destination.Close()
	sourceTransport := env.newTransport(
		sourceJwt,
		server.NewId(),
		source.RouteManager(),
	)
	destinationTransport := env.newTransport(
		destinationJwt,
		server.NewId(),
		destination.RouteManager(),
	)
	defer sourceTransport.Close()
	defer destinationTransport.Close()
	env.setProvideModes(source, map[protocol.ProvideMode]bool{
		protocol.ProvideMode_Network: true,
	})
	env.setProvideModes(destination, map[protocol.ProvideMode]bool{
		protocol.ProvideMode_Network: true,
	})
	waitForStreamRoutePerformanceP2p(
		t,
		source,
		destination,
		sourceClientId,
		destinationClientId,
	)
	writer := source.RouteManager().OpenMultiRouteWriter(
		clientconnect.DestinationId(clientconnect.Id(destinationClientId)),
	)
	defer source.RouteManager().CloseMultiRouteWriter(writer)
	waitForActiveRoutes(ctx, t, writer, 2)
	source.ContractManager().AddNoContractPeer(clientconnect.Id(destinationClientId))
	destination.ContractManager().AddNoContractPeer(clientconnect.Id(sourceClientId))
	sourceTransport.Close()
	destinationTransport.Close()
	waitForActiveRoutes(ctx, t, writer, 1)
	receiver, receiverUnsub := newStreamRoutePerformanceReceiver(
		destination,
		sourceClientId,
	)
	defer receiverUnsub()
	summary := measureStreamRoutePerformance(
		t,
		routeName,
		source,
		destinationClientId,
		receiver,
		markerByte,
		packetCount,
		runCount,
		[]any{clientconnect.NoAck()},
	)
	assertStreamRoutePerformanceDataPlane(
		t,
		mode,
		sourceStats,
		destinationStats,
	)
	t.Logf(
		"[stream-route-perf] route=%s source-data-plane=%+v destination-data-plane=%+v",
		routeName,
		sourceStats.Snapshot(),
		destinationStats.Snapshot(),
	)
	return summary
}

// measureStreamRouteExchange measures a forced real client carrier, handler,
// resident, exchange, and destination path with no P2P stream present.
func measureStreamRouteExchange(
	t testing.TB,
	env *peerDiscoveryEnv,
	mode clientconnect.TransportMode,
	routeName string,
	markerByte *byte,
	packetCount int,
	runCount int,
) streamRoutePerformanceSummary {
	t.Helper()
	sourceClientId, sourceJwt := env.authClient(&model.AuthNetworkClientArgs{
		Description: "exchange performance source",
	})
	destinationClientId, destinationJwt := env.authClient(&model.AuthNetworkClientArgs{
		Description: "exchange performance destination",
	})
	source := env.newClient(sourceClientId)
	destination := env.newClient(destinationClientId)
	defer source.Close()
	defer destination.Close()
	sourceTransport := env.newTransportWithMode(
		sourceJwt,
		server.NewId(),
		source.RouteManager(),
		mode,
	)
	destinationTransport := env.newTransportWithMode(
		destinationJwt,
		server.NewId(),
		destination.RouteManager(),
		mode,
	)
	defer sourceTransport.Close()
	defer destinationTransport.Close()
	waitForStreamRoutePerformanceTransport(t, env.ctx, sourceTransport)
	waitForStreamRoutePerformanceTransport(t, env.ctx, destinationTransport)
	// The exchange resident enforces active contracts. Establish the real
	// Network relationship instead of marking the peers no-contract locally;
	// the latter makes sends appear accepted while the resident correctly drops
	// every frame before it can reach the destination.
	env.setProvideModes(source, map[protocol.ProvideMode]bool{
		protocol.ProvideMode_Network: true,
	})
	env.setProvideModes(destination, map[protocol.ProvideMode]bool{
		protocol.ProvideMode_Network: true,
	})
	writer := source.RouteManager().OpenMultiRouteWriter(
		clientconnect.DestinationId(clientconnect.Id(destinationClientId)),
	)
	defer source.RouteManager().CloseMultiRouteWriter(writer)
	waitForActiveRoutes(env.ctx, t, writer, 1)
	receiver, receiverUnsub := newStreamRoutePerformanceReceiver(
		destination,
		sourceClientId,
	)
	defer receiverUnsub()
	return measureStreamRoutePerformance(
		t,
		routeName,
		source,
		destinationClientId,
		receiver,
		markerByte,
		packetCount,
		runCount,
		nil,
	)
}

// Exchange measurement starts only after each resident has registered. A
// source route alone does not prove the destination can receive yet.
func waitForStreamRoutePerformanceTransport(
	t testing.TB,
	ctx context.Context,
	transport *clientconnect.PlatformTransport,
) {
	t.Helper()
	endTime := time.Now().Add(60 * time.Second)
	for {
		notify := transport.ConnectedNotify()
		if transport.IsConnected() {
			return
		}
		if endTime.Before(time.Now()) {
			t.Fatal("timeout waiting for stream route platform transport")
		}
		select {
		case <-ctx.Done():
			t.Fatal("stream route environment closed before platform transport connected")
		case <-notify:
		case <-time.After(time.Second):
		}
	}
}

// A one-shot callback confirms stream setup without remaining on the measured
// receive path.
func waitForStreamRoutePerformanceP2p(
	t testing.TB,
	sender *clientconnect.Client,
	receiver *clientconnect.Client,
	sourceClientId server.Id,
	destinationClientId server.Id,
) {
	t.Helper()
	received := make(chan struct{}, 1)
	unsub := receiver.AddReceiveCallback(func(
		source clientconnect.TransferPath,
		frames []*protocol.Frame,
		peer clientconnect.Peer,
	) {
		_ = peer
		if source.SourceId != clientconnect.Id(sourceClientId) {
			return
		}
		for _, frame := range frames {
			if frame.MessageType == protocol.MessageType_TestSimpleMessage {
				select {
				case received <- struct{}{}:
				default:
				}
			}
		}
	})
	defer unsub()

	sendSimpleMessage(t, sender, destinationClientId, clientconnect.ForceStream())
	select {
	case <-received:
	case <-time.After(60 * time.Second):
		t.Fatal("timeout waiting for stream setup message")
	}
}

// TestStreamRouteDataPlaneSelection is the always-on, small end-to-end
// correctness gate for both exchange carriers and both P2P data planes.
func TestStreamRouteDataPlaneSelection(t *testing.T) {
	if testing.Short() {
		return
	}
	testEnv := &server.TestEnv{
		ApplyDbMigrations: true,
		RerunCount:        0,
	}
	testEnv.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		env := testing_newPeerDiscoveryEnv(ctx, t)
		defer env.Close()
		markerByte := byte(1)
		measureStreamRouteExchange(
			t,
			env,
			clientconnect.TransportModeH1,
			"exchange-h1",
			&markerByte,
			64,
			1,
		)
		measureStreamRouteExchange(
			t,
			env,
			clientconnect.TransportModeH3,
			"exchange-h3",
			&markerByte,
			64,
			1,
		)
		measureStreamRouteP2p(
			t,
			env,
			clientconnect.P2pDataPlaneModeLegacyOnly,
			"p2p-legacy",
			&markerByte,
			64,
			1,
		)
		measureStreamRouteP2p(
			t,
			env,
			clientconnect.P2pDataPlaneModeFastOnly,
			"p2p-fast",
			&markerByte,
			64,
			1,
		)
	})
}

// TestStreamRoutePerformanceComparison records five real-topology runs for
// both exchange carriers, forced legacy P2P, and forced fast P2P. It is opt-in
// because it measures performance rather than ordinary correctness.
func TestStreamRoutePerformanceComparison(t *testing.T) {
	if testing.Short() || os.Getenv("CONNECT_STREAM_ROUTE_PERFORMANCE_MEASURE") == "" {
		t.Skip("set CONNECT_STREAM_ROUTE_PERFORMANCE_MEASURE=1")
	}
	testEnv := &server.TestEnv{
		ApplyDbMigrations: true,
		RerunCount:        0,
	}
	testEnv.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		env := testing_newPeerDiscoveryEnv(ctx, t)
		defer env.Close()
		markerByte := byte(1)
		selectedRoute := os.Getenv("CONNECT_STREAM_ROUTE_PERFORMANCE_ROUTE")
		var legacySummary streamRoutePerformanceSummary
		var fastSummary streamRoutePerformanceSummary
		legacyMeasured := false
		fastMeasured := false
		if selectedRoute == "" || selectedRoute == "exchange-h1" {
			measureStreamRouteExchange(
				t,
				env,
				clientconnect.TransportModeH1,
				"exchange-h1",
				&markerByte,
				streamRoutePerformancePacketCount,
				streamRoutePerformanceRunCount,
			)
		}
		if selectedRoute == "" || selectedRoute == "exchange-h3" {
			measureStreamRouteExchange(
				t,
				env,
				clientconnect.TransportModeH3,
				"exchange-h3",
				&markerByte,
				streamRoutePerformancePacketCount,
				streamRoutePerformanceRunCount,
			)
		}
		if selectedRoute == "" || selectedRoute == "p2p-legacy" {
			legacySummary = measureStreamRouteP2p(
				t,
				env,
				clientconnect.P2pDataPlaneModeLegacyOnly,
				"p2p-legacy",
				&markerByte,
				streamRoutePerformancePacketCount,
				streamRoutePerformanceRunCount,
			)
			legacyMeasured = true
		}
		if selectedRoute == "" || selectedRoute == "p2p-fast" {
			fastSummary = measureStreamRouteP2p(
				t,
				env,
				clientconnect.P2pDataPlaneModeFastOnly,
				"p2p-fast",
				&markerByte,
				streamRoutePerformancePacketCount,
				streamRoutePerformanceRunCount,
			)
			fastMeasured = true
		}
		if selectedRoute != "" &&
			selectedRoute != "exchange-h1" &&
			selectedRoute != "exchange-h3" &&
			selectedRoute != "p2p-legacy" &&
			selectedRoute != "p2p-fast" {
			t.Fatalf("unknown stream route performance route %q", selectedRoute)
		}
		if legacyMeasured && fastMeasured {
			gain := fastSummary.medianThroughput / legacySummary.medianThroughput
			t.Logf(
				"[stream-route-perf] p2p-fast/legacy median gain=%.2fx worst-fast=%.2f MB/s",
				gain,
				fastSummary.worstThroughput,
			)
			if gain < streamRoutePerformanceMinimumFastGain {
				t.Fatalf(
					"P2P fast median gain=%.2fx want>=%.2fx",
					gain,
					streamRoutePerformanceMinimumFastGain,
				)
			}
		}
	})
}
