// This file measures live path changes and bounded recovery without replacing
// any production transport or route object.
package perfvar

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	clientconnect "github.com/urnetwork/connect"
	"github.com/urnetwork/connect/protocol"
	"github.com/urnetwork/server"
)

// Recovery observations identify the actual outage boundary and first useful
// application progress after restoration.
type perfvarRecoveryResult struct {
	Workload       workloadResult
	OutageDuration time.Duration
	RecoveryTime   time.Duration
	BytesAtOutage  int64
}

// Live-profile observations retain acknowledged event boundaries and link
// state while one exact application stream remains active.
type perfvarLiveProfileResult struct {
	Workload          workloadResult
	ChangeUpdates     []networkProfileUpdateResult
	RestoreUpdates    []networkProfileUpdateResult
	BytesAtChange     int64
	BytesAtRestore    int64
	BeforeLinks       map[string]directionalLinkSnapshot
	ImpairedLinks     map[string]directionalLinkSnapshot
	ImpairedProfiles  map[string]linkProfile
	AfterRestoreLinks map[string]directionalLinkSnapshot
}

// Direct routes blackhole native Pion packets; exchange routes update every
// userspace client-edge scheduler below the production carrier.
func setFullTunBlackhole(ctx context.Context, path *fullTunPath, blackhole bool) error {
	if path.p2pNetwork != nil {
		return path.p2pNetwork.setBlackhole(blackhole, blackhole)
	}
	return path.environment.network.setBlackhole(ctx, blackhole)
}

// Selects the next bounded segment without restarting a final partial segment
// at the beginning of the repeated deterministic payload.
func pacedDeterministicWorkloadChunk(
	payload []byte,
	sentByteCount int64,
	remainingByteCount int64,
	maximumByteCount int,
) []byte {
	offset := int(sentByteCount % int64(len(payload)))
	chunk := payload[offset:min(len(payload), offset+maximumByteCount)]
	if remainingByteCount < int64(len(chunk)) {
		chunk = chunk[:remainingByteCount]
	}
	return chunk
}

// Owns one paced writer and makes cancellation join the writer even when it is
// blocked in the connection. Wait reports the writer result without closing a
// successful stream; CloseAndWait is the cleanup path for every early return.
type pacedEventSender struct {
	cancel     context.CancelFunc
	connection net.Conn
	done       <-chan error
	waitOnce   sync.Once
	waitErr    error
}

// Starts one cancellation-owned writer whose result can be joined once.
func startPacedEventSender(
	ctx context.Context,
	connection net.Conn,
	byteCount int64,
	offeredBitRate int64,
	startTime time.Time,
) *pacedEventSender {
	senderCtx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	sender := &pacedEventSender{
		cancel:     cancel,
		connection: connection,
		done:       done,
	}
	go func() {
		payload := deterministicPayload()
		const writeByteCount = 4 * 1024
		var sentByteCount int64
		for remaining := byteCount; 0 < remaining; {
			chunk := pacedDeterministicWorkloadChunk(
				payload,
				sentByteCount,
				remaining,
				writeByteCount,
			)
			if writeErr := writeFullTunAll(connection, chunk); writeErr != nil {
				done <- writeErr
				return
			}
			remaining -= int64(len(chunk))
			sentByteCount += int64(len(chunk))
			// Pace below the simulated link rate so a short outage drops a
			// bounded amount of active-flow data instead of filling every
			// socket and route queue before the impairment starts.
			targetTime := startTime.Add(time.Duration(
				float64(time.Second) * float64(sentByteCount*8) / float64(offeredBitRate),
			))
			if wait := time.Until(targetTime); 0 < wait {
				timer := time.NewTimer(wait)
				select {
				case <-timer.C:
				case <-senderCtx.Done():
					timer.Stop()
					done <- senderCtx.Err()
					return
				}
			}
		}
		done <- nil
	}()
	return sender
}

// Joins the writer and preserves its single terminal result for later callers.
func (self *pacedEventSender) Wait() error {
	self.waitOnce.Do(func() {
		self.waitErr = <-self.done
	})
	return self.waitErr
}

// Interrupts a blocked socket write before joining the writer.
func (self *pacedEventSender) CloseAndWait() error {
	self.cancel()
	_ = self.connection.Close()
	return self.Wait()
}

type pacedEventSenderBarrierConnection struct {
	net.Conn
	writeEntered chan struct{}
	writeExited  chan struct{}
	enterOnce    sync.Once
	exitOnce     sync.Once
}

// Exposes deterministic barriers on both sides of the wrapped socket write.
func (self *pacedEventSenderBarrierConnection) Write(payload []byte) (int, error) {
	self.enterOnce.Do(func() {
		close(self.writeEntered)
	})
	written, err := self.Conn.Write(payload)
	self.exitOnce.Do(func() {
		close(self.writeExited)
	})
	return written, err
}

// A measurement returning while its writer is blocked must close the socket
// and join that writer. The entered/exited barriers make the lifecycle event
// driven; the timers only backstop a broken join.
func TestPacedEventSenderCloseAndWaitJoinsBlockedWrite(t *testing.T) {
	client, server := net.Pipe()
	defer server.Close()
	connection := &pacedEventSenderBarrierConnection{
		Conn:         client,
		writeEntered: make(chan struct{}),
		writeExited:  make(chan struct{}),
	}
	sender := startPacedEventSender(
		context.Background(),
		connection,
		4*1024,
		2_000_000,
		time.Now(),
	)
	select {
	case <-connection.writeEntered:
	case <-time.After(time.Second):
		t.Fatal("paced event sender did not enter its blocked write")
	}
	joined := make(chan error, 1)
	go func() {
		joined <- sender.CloseAndWait()
	}()
	select {
	case err := <-joined:
		if err == nil {
			t.Fatal("closed blocked write returned no error")
		}
	case <-time.After(time.Second):
		t.Fatal("paced event sender was not joined after close")
	}
	select {
	case <-connection.writeExited:
	default:
		t.Fatal("paced event sender joined before its blocked write exited")
	}
	if err := sender.CloseAndWait(); err == nil {
		t.Fatal("second sender join did not preserve the writer result")
	}
}

// A short tail after one full paced write must continue at its current payload
// offset so its digest matches the exact workload stream.
func TestPacedDeterministicWorkloadChunkPreservesFinalOffset(t *testing.T) {
	const writeByteCount = 4 * 1024
	const byteCount = writeByteCount + 3
	payload := deterministicPayload()
	hash := sha256.New()
	var sentByteCount int64
	for sentByteCount < byteCount {
		chunk := pacedDeterministicWorkloadChunk(
			payload,
			sentByteCount,
			byteCount-sentByteCount,
			writeByteCount,
		)
		_, _ = hash.Write(chunk)
		sentByteCount += int64(len(chunk))
	}
	actualHash := hex.EncodeToString(hash.Sum(nil))
	expectedHash := deterministicPayloadHash(byteCount)
	if actualHash != expectedHash {
		t.Fatalf("paced payload hash=%s, want=%s", actualHash, expectedHash)
	}
}

// A long exact TCP flow remains active while both path directions disappear.
func measureFullTunOutageRecovery(
	ctx context.Context,
	path *fullTunPath,
	byteCount int64,
	outageDuration time.Duration,
) (perfvarRecoveryResult, error) {
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		return perfvarRecoveryResult{}, err
	}
	const flowId = logicalTCPFlowId(0)
	var receivedByteCount atomic.Int64
	progress := make(chan struct{}, 1)
	var actualHash []byte
	flowServer := newLogicalTCPFlowServer(
		ctx,
		listener,
		1,
		func(receivedFlowId logicalTCPFlowId, connection net.Conn) error {
			if receivedFlowId != flowId {
				return fmt.Errorf("outage recovery flow id=%d, want=%d", receivedFlowId, flowId)
			}
			if deadlineErr := connection.SetDeadline(time.Now().Add(fullTunWorkloadTimeout(path, byteCount))); deadlineErr != nil {
				return fmt.Errorf("set outage recovery server deadline: %w", deadlineErr)
			}
			hash := sha256.New()
			buffer := make([]byte, 16*1024)
			for receivedByteCount.Load() < byteCount {
				remaining := byteCount - receivedByteCount.Load()
				readBuffer := buffer
				if remaining < int64(len(readBuffer)) {
					readBuffer = buffer[:remaining]
				}
				readCount, readErr := connection.Read(readBuffer)
				if 0 < readCount {
					_, _ = hash.Write(readBuffer[:readCount])
					receivedByteCount.Add(int64(readCount))
					select {
					case progress <- struct{}{}:
					default:
					}
				}
				if readErr != nil {
					return fmt.Errorf("read outage recovery payload: %w", readErr)
				}
			}
			actualHash = hash.Sum(nil)
			return nil
		},
		nil,
	)
	defer flowServer.CloseAndWait()
	dialCtx, dialCancel := context.WithTimeout(ctx, 90*time.Second)
	connection, err := path.appTun.DialContext(dialCtx, "tcp", listener.Addr().String())
	dialCancel()
	if err != nil {
		return perfvarRecoveryResult{}, err
	}
	defer connection.Close()
	if err := writeLogicalTCPFlowPreface(connection, flowId); err != nil {
		return perfvarRecoveryResult{}, err
	}
	if err := flowServer.WaitReady(ctx); err != nil {
		return perfvarRecoveryResult{}, err
	}
	if err := connection.SetDeadline(time.Now().Add(fullTunWorkloadTimeout(path, byteCount))); err != nil {
		return perfvarRecoveryResult{}, err
	}
	startTime := time.Now()
	sender := startPacedEventSender(ctx, connection, byteCount, 2_000_000, startTime)
	defer sender.CloseAndWait()
	progressTarget := min(byteCount/4, int64(64*1024))
	for receivedByteCount.Load() < progressTarget {
		select {
		case <-flowServer.Done():
			if serverErr := flowServer.Wait(); serverErr != nil {
				return perfvarRecoveryResult{}, serverErr
			}
			if progressTarget <= receivedByteCount.Load() {
				continue
			}
			return perfvarRecoveryResult{}, fmt.Errorf(
				"outage recovery flow completed at %d bytes before progress target %d",
				receivedByteCount.Load(),
				progressTarget,
			)
		case <-progress:
		case <-ctx.Done():
			return perfvarRecoveryResult{}, ctx.Err()
		}
	}
	if receivedByteCount.Load() == byteCount {
		return perfvarRecoveryResult{}, fmt.Errorf("outage workload completed before impairment")
	}
	if err := setFullTunBlackhole(ctx, path, true); err != nil {
		return perfvarRecoveryResult{}, err
	}
	bytesAtOutage := receivedByteCount.Load()
	time.Sleep(outageDuration)
	restoreTime := time.Now()
	if err := setFullTunBlackhole(ctx, path, false); err != nil {
		return perfvarRecoveryResult{}, err
	}
	for receivedByteCount.Load() <= bytesAtOutage {
		select {
		case <-flowServer.Done():
			if serverErr := flowServer.Wait(); serverErr != nil {
				return perfvarRecoveryResult{}, serverErr
			}
			if bytesAtOutage < receivedByteCount.Load() {
				continue
			}
			return perfvarRecoveryResult{}, fmt.Errorf(
				"outage recovery flow completed without post-restore progress after %d bytes",
				bytesAtOutage,
			)
		case <-progress:
		case <-ctx.Done():
			return perfvarRecoveryResult{}, ctx.Err()
		}
	}
	recoveryTime := time.Since(restoreTime)
	if err := sender.Wait(); err != nil {
		return perfvarRecoveryResult{}, err
	}
	if err := flowServer.Wait(); err != nil {
		return perfvarRecoveryResult{}, err
	}
	expectedHash, err := hex.DecodeString(deterministicPayloadHash(byteCount))
	if err != nil {
		return perfvarRecoveryResult{}, err
	}
	if !bytes.Equal(actualHash, expectedHash) {
		return perfvarRecoveryResult{}, fmt.Errorf("outage recovery content hash mismatch")
	}
	return perfvarRecoveryResult{
		Workload: finishWorkloadResult(workloadResult{
			UsefulByteCount: byteCount,
			Duration:        time.Since(startTime),
			ContentHash:     fmt.Sprintf("%x", actualHash),
		}),
		OutageDuration: outageDuration,
		RecoveryTime:   recoveryTime,
		BytesAtOutage:  bytesAtOutage,
	}, nil
}

// A paced exact stream crosses a scheduled rate, delay, jitter, and loss
// change, then crosses a second scheduled restoration on the same connection.
func measureFullTunLiveProfileChange(
	ctx context.Context,
	path *fullTunPath,
	byteCount int64,
	change func(linkProfile) linkProfile,
	restore func(linkProfile) linkProfile,
) (perfvarLiveProfileResult, error) {
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		return perfvarLiveProfileResult{}, err
	}
	const flowId = logicalTCPFlowId(0)
	var receivedByteCount atomic.Int64
	progress := make(chan struct{}, 1)
	var actualHash []byte
	flowServer := newLogicalTCPFlowServer(
		ctx,
		listener,
		1,
		func(receivedFlowId logicalTCPFlowId, connection net.Conn) error {
			if receivedFlowId != flowId {
				return fmt.Errorf("live profile flow id=%d, want=%d", receivedFlowId, flowId)
			}
			if deadlineErr := connection.SetDeadline(time.Now().Add(fullTunWorkloadTimeout(path, byteCount))); deadlineErr != nil {
				return fmt.Errorf("set live profile server deadline: %w", deadlineErr)
			}
			hash := sha256.New()
			buffer := make([]byte, 16*1024)
			for receivedByteCount.Load() < byteCount {
				remaining := byteCount - receivedByteCount.Load()
				readBuffer := buffer
				if remaining < int64(len(readBuffer)) {
					readBuffer = buffer[:remaining]
				}
				readCount, readErr := connection.Read(readBuffer)
				if 0 < readCount {
					_, _ = hash.Write(readBuffer[:readCount])
					receivedByteCount.Add(int64(readCount))
					select {
					case progress <- struct{}{}:
					default:
					}
				}
				if readErr != nil {
					return fmt.Errorf("read live profile payload: %w", readErr)
				}
			}
			actualHash = hash.Sum(nil)
			return nil
		},
		nil,
	)
	defer flowServer.CloseAndWait()
	dialCtx, dialCancel := context.WithTimeout(ctx, 90*time.Second)
	connection, err := path.appTun.DialContext(dialCtx, "tcp", listener.Addr().String())
	dialCancel()
	if err != nil {
		return perfvarLiveProfileResult{}, err
	}
	defer connection.Close()
	if err := writeLogicalTCPFlowPreface(connection, flowId); err != nil {
		return perfvarLiveProfileResult{}, err
	}
	if err := flowServer.WaitReady(ctx); err != nil {
		return perfvarLiveProfileResult{}, err
	}
	if err := connection.SetDeadline(time.Now().Add(fullTunWorkloadTimeout(path, byteCount))); err != nil {
		return perfvarLiveProfileResult{}, err
	}
	startTime := time.Now()
	sender := startPacedEventSender(ctx, connection, byteCount, 4_000_000, startTime)
	defer sender.CloseAndWait()
	waitForProgress := func(targetByteCount int64) error {
		for receivedByteCount.Load() < targetByteCount {
			select {
			case <-flowServer.Done():
				if serverErr := flowServer.Wait(); serverErr != nil {
					return serverErr
				}
				if receivedByteCount.Load() < targetByteCount {
					return fmt.Errorf(
						"live profile flow completed at %d bytes before progress target %d",
						receivedByteCount.Load(),
						targetByteCount,
					)
				}
			case <-progress:
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		return nil
	}
	changeTarget := min(byteCount/4, int64(64*1024))
	if err := waitForProgress(changeTarget); err != nil {
		return perfvarLiveProfileResult{}, err
	}
	beforeLinks := path.environment.network.snapshotLinks()
	changeScheduledTime := time.Now().Add(20 * time.Millisecond)
	changeUpdates, err := path.environment.network.updateProfiles(
		ctx,
		"live-impairment",
		changeScheduledTime,
		change,
	)
	if err != nil {
		return perfvarLiveProfileResult{}, err
	}
	bytesAtChange := receivedByteCount.Load()
	restoreTarget := min(byteCount-1, bytesAtChange+128*1024)
	if err := waitForProgress(restoreTarget); err != nil {
		return perfvarLiveProfileResult{}, err
	}
	impairedLinks := path.environment.network.snapshotLinks()
	impairedProfiles := path.environment.network.snapshotProfiles()
	restoreScheduledTime := time.Now().Add(20 * time.Millisecond)
	restoreUpdates, err := path.environment.network.updateProfiles(
		ctx,
		"live-restore",
		restoreScheduledTime,
		restore,
	)
	if err != nil {
		return perfvarLiveProfileResult{}, err
	}
	bytesAtRestore := receivedByteCount.Load()
	if err := sender.Wait(); err != nil {
		return perfvarLiveProfileResult{}, err
	}
	if err := flowServer.Wait(); err != nil {
		return perfvarLiveProfileResult{}, err
	}
	expectedHash, err := hex.DecodeString(deterministicPayloadHash(byteCount))
	if err != nil {
		return perfvarLiveProfileResult{}, err
	}
	if !bytes.Equal(actualHash, expectedHash) {
		return perfvarLiveProfileResult{}, fmt.Errorf("live profile content hash mismatch")
	}
	return perfvarLiveProfileResult{
		Workload: finishWorkloadResult(workloadResult{
			UsefulByteCount: byteCount,
			Duration:        time.Since(startTime),
			ContentHash:     fmt.Sprintf("%x", actualHash),
		}),
		ChangeUpdates:     changeUpdates,
		RestoreUpdates:    restoreUpdates,
		BytesAtChange:     bytesAtChange,
		BytesAtRestore:    bytesAtRestore,
		BeforeLinks:       beforeLinks,
		ImpairedLinks:     impairedLinks,
		ImpairedProfiles:  impairedProfiles,
		AfterRestoreLinks: path.environment.network.snapshotLinks(),
	}, nil
}

// Platform state waits subscribe before reading so a fast kick/reconnect edge
// cannot be lost between the state check and monitor registration.
func waitForPlatformState(
	ctx context.Context,
	transport interface {
		IsConnected() bool
		ConnectedNotify() <-chan struct{}
	},
	connected bool,
) bool {
	deadline := time.NewTimer(30 * time.Second)
	defer deadline.Stop()
	for {
		notify := transport.ConnectedNotify()
		if transport.IsConnected() == connected {
			return true
		}
		select {
		case <-ctx.Done():
			return false
		case <-deadline.C:
			return false
		case <-notify:
		}
	}
}

// A route-count transition proves that a network-change worker retired the
// old direct route after the caller's generation barrier. Every intervening
// publication is retained, so a fast 2 -> 1 -> 2 cannot disappear.
func waitForRouteCountChange(
	ctx context.Context,
	observer *clientconnect.TestingMultiRouteWriterRouteStateObserver,
	barrier clientconnect.TestingMultiRouteWriterRouteState,
	previousCount int,
) (clientconnect.TestingMultiRouteWriterRouteState, error) {
	waitCtx, waitCancel := context.WithTimeout(ctx, 90*time.Second)
	defer waitCancel()
	generation := barrier.Generation
	for {
		state, err := observer.WaitAfter(waitCtx, generation)
		if err != nil {
			return clientconnect.TestingMultiRouteWriterRouteState{}, fmt.Errorf(
				"route count did not change from %d after generation %d: %w",
				previousCount,
				barrier.Generation,
				err,
			)
		}
		if state.ActiveRouteCount != previousCount {
			return state, nil
		}
		generation = state.Generation
	}
}

// A live direct-route fixture retains its platform fallback so path changes
// can retire and rebuild Pion without replacing either transfer client.
type liveP2pRoute struct {
	environment        *routeEnvironment
	network            *p2pNetwork
	source             *routeClient
	destination        *routeClient
	writer             clientconnect.MultiRouteWriter
	routeStateObserver *clientconnect.TestingMultiRouteWriterRouteStateObserver
}

// Construction completes signaling and promotion but leaves both carriers up.
func newLiveP2pRoute(t testing.TB, environment *routeEnvironment) *liveP2pRoute {
	network, err := newP2pNetwork(environment.profile)
	if err != nil {
		t.Fatalf("create live P2P network: %v", err)
	}
	source := environment.newClient(
		"live P2P source",
		clientconnect.P2pDataPlaneModeFastOnly,
		network.left,
		false,
	)
	destination := environment.newClient(
		"live P2P destination",
		clientconnect.P2pDataPlaneModeFastOnly,
		network.right,
		false,
	)
	environment.connectPlatform(source, clientconnect.TransportModeH1)
	environment.connectPlatform(destination, clientconnect.TransportModeH1)
	if !waitForPlatform(environment.ctx, source.transport) ||
		!waitForPlatform(environment.ctx, destination.transport) {
		network.close()
		t.Fatal("live P2P signaling platform did not connect")
	}
	if err := setRouteProvide(environment.ctx, source.client); err != nil {
		network.close()
		t.Fatalf("live P2P source provide: %v", err)
	}
	if err := setRouteProvide(environment.ctx, destination.client); err != nil {
		network.close()
		t.Fatalf("live P2P destination provide: %v", err)
	}
	writer := source.client.RouteManager().OpenMultiRouteWriter(
		clientconnect.DestinationId(clientconnect.Id(destination.clientId)),
	)
	routeStateObserver := clientconnect.TestingObserveMultiRouteWriterRouteState(writer)
	if err := waitForP2pRoute(environment.ctx, source, destination); err != nil {
		routeStateObserver.Close()
		source.client.RouteManager().CloseMultiRouteWriter(writer)
		network.close()
		t.Fatal(err)
	}
	if err := waitForRouteCount(environment.ctx, routeStateObserver, 2); err != nil {
		routeStateObserver.Close()
		source.client.RouteManager().CloseMultiRouteWriter(writer)
		network.close()
		t.Fatalf("wait for live P2P promotion: %v", err)
	}
	source.client.ContractManager().AddNoContractPeer(clientconnect.Id(destination.clientId))
	destination.client.ContractManager().AddNoContractPeer(clientconnect.Id(source.clientId))
	return &liveP2pRoute{
		environment:        environment,
		network:            network,
		source:             source,
		destination:        destination,
		writer:             writer,
		routeStateObserver: routeStateObserver,
	}
}

// Route-writer ownership ends before the Pion router and clients are closed.
func (self *liveP2pRoute) close() {
	self.routeStateObserver.Close()
	self.source.client.RouteManager().CloseMultiRouteWriter(self.writer)
	self.network.close()
}

// Exact traffic plus data-plane counters prove the preferred direct route.
func (self *liveP2pRoute) measureDirect(packetCount int) (workloadResult, error) {
	before := self.source.stats.Snapshot()
	result, err := measureProductionRoute(
		self.environment.ctx,
		self.source,
		self.destination,
		packetCount,
		clientconnect.NoAck(),
	)
	if err != nil {
		return workloadResult{}, err
	}
	after := self.source.stats.Snapshot()
	if after.FastSendMessageCount <= before.FastSendMessageCount || after.FastFallbackCount != 0 {
		return workloadResult{}, fmt.Errorf("direct fast P2P counters before=%+v after=%+v", before, after)
	}
	return result, nil
}

// A scheduled multi-axis impairment is applied during one real exchange H3
// inner TCP stream, then restored without rebuilding the route.
func TestFullTunLiveProfileChangeCorrectness(t *testing.T) {
	if testing.Short() {
		return
	}
	testEnvironment := &server.TestEnv{ApplyDbMigrations: true, RerunCount: 0}
	testEnvironment.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		cleanProfile := initialNetworkProfiles(4300)["clean-lan"]
		environment := newRouteEnvironmentWithNetworkPeers(ctx, t, cleanProfile, false)
		defer environment.close()
		path := newFullTunPath(ctx, t, environment, fullTunRouteExchangeH3)
		defer path.close()
		change := func(profile linkProfile) linkProfile {
			profile.RateBitsPerSecond = 2_000_000
			profile.BurstByteCount = 16 * 1024
			profile.BaseDelay = 10 * time.Millisecond
			profile.ProcessingDelay = 2 * time.Millisecond
			profile.Jitter = 2 * time.Millisecond
			profile.LossModel = lossModelEveryN
			profile.LossProbability = 0
			profile.DropEveryPacketCount = 97
			profile.BurstLoss = nil
			return profile
		}
		restore := func(_ linkProfile) linkProfile {
			return cleanProfile.Forward
		}
		result, err := measureFullTunLiveProfileChange(
			ctx,
			path,
			1024*1024,
			change,
			restore,
		)
		if err != nil {
			t.Fatalf("full-TUN live profile change: %v", err)
		}
		if result.BytesAtChange <= 0 || result.BytesAtChange >= result.BytesAtRestore ||
			result.BytesAtRestore >= result.Workload.UsefulByteCount {
			t.Fatalf("live profile progress=%d/%d/%d", result.BytesAtChange, result.BytesAtRestore, result.Workload.UsefulByteCount)
		}
		validateUpdates := func(eventName string, updates []networkProfileUpdateResult) {
			if len(updates) == 0 || len(updates) != len(result.BeforeLinks) {
				t.Fatalf("%s update count=%d links=%d", eventName, len(updates), len(result.BeforeLinks))
			}
			for updateIndex, update := range updates {
				if update.EventName != eventName || update.ActualTime.Before(update.ScheduledTime) {
					t.Fatalf("%s update=%+v", eventName, update)
				}
				if time.Second < update.ActualTime.Sub(update.ScheduledTime) {
					t.Fatalf("%s update applied late: %+v", eventName, update)
				}
				if 0 < updateIndex && update.LinkName <= updates[updateIndex-1].LinkName {
					t.Fatalf("%s updates are not in stable link order: %+v", eventName, updates)
				}
			}
		}
		validateUpdates("live-impairment", result.ChangeUpdates)
		validateUpdates("live-restore", result.RestoreUpdates)
		var lossDropPacketCount uint64
		for linkName, profile := range result.ImpairedProfiles {
			if profile.RateBitsPerSecond != 2_000_000 ||
				profile.BaseDelay != 10*time.Millisecond ||
				profile.ProcessingDelay != 2*time.Millisecond ||
				profile.Jitter != 2*time.Millisecond ||
				profile.LossModel != lossModelEveryN ||
				profile.DropEveryPacketCount != 97 {
				t.Fatalf("impaired %s profile=%+v", linkName, profile)
			}
			before := result.BeforeLinks[linkName]
			impaired := result.ImpairedLinks[linkName]
			after := result.AfterRestoreLinks[linkName]
			lossDropPacketCount += impaired.LossDropPacketCount - before.LossDropPacketCount
			if after.ProfileUpdateCount != before.ProfileUpdateCount+2 {
				t.Fatalf("%s profile updates before=%d after=%d", linkName, before.ProfileUpdateCount, after.ProfileUpdateCount)
			}
		}
		if lossDropPacketCount == 0 {
			t.Fatal("live loss profile did not drop a packet during the impaired interval")
		}
		if err := path.verifyRoute(); err != nil {
			t.Fatal(err)
		}
		t.Logf(
			"[perfvar] live-profile route=%s duration=%s change-bytes=%d restore-bytes=%d drops=%d",
			path.route,
			result.Workload.Duration,
			result.BytesAtChange,
			result.BytesAtRestore,
			lossDropPacketCount,
		)
	})
}

// The application-facing network-change seam closes a live platform socket,
// survives an unreachable interval, and re-dials the same full route.
func TestFullTunPlatformNetworkChangeRecovery(t *testing.T) {
	if testing.Short() {
		return
	}
	testEnvironment := &server.TestEnv{ApplyDbMigrations: true, RerunCount: 0}
	testEnvironment.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		profile := initialNetworkProfiles(4301)["clean-lan"]
		environment := newRouteEnvironmentWithNetworkPeers(ctx, t, profile, false)
		defer environment.close()
		path := newFullTunPath(ctx, t, environment, fullTunRouteExchangeH1)
		defer path.close()
		if err := environment.network.setBlackhole(ctx, true); err != nil {
			t.Fatal(err)
		}
		path.multiClient.NotifyNetworkChanged()
		if !waitForPlatformState(ctx, path.providerTransport, false) {
			t.Fatal("provider platform did not close after network change")
		}
		restoreTime := time.Now()
		if err := environment.network.setBlackhole(ctx, false); err != nil {
			t.Fatal(err)
		}
		path.multiClient.NotifyNetworkChanged()
		if !waitForPlatformState(ctx, path.providerTransport, true) {
			t.Fatal("provider platform did not reconnect after network restoration")
		}
		reconnectDuration := time.Since(restoreTime)
		result, err := measureFullTunUpload(ctx, path, 128*1024)
		if err != nil {
			t.Fatalf("full-TUN platform recovery upload: %v", err)
		}
		if err := path.verifyRoute(); err != nil {
			t.Fatal(err)
		}
		t.Logf(
			"[perfvar] platform-network-change route=%s reconnect=%s goodput=%.6fGbps",
			path.route,
			reconnectDuration,
			result.GoodputGigabits,
		)
	})
}

// Blocking the preferred direct path and firing the production network-change
// hook forces platform delivery, then restoration rebuilds direct P2P.
func TestP2pNetworkChangeFallbackAndRestore(t *testing.T) {
	if testing.Short() {
		return
	}
	testEnvironment := &server.TestEnv{ApplyDbMigrations: true, RerunCount: 0}
	testEnvironment.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		profile := initialNetworkProfiles(4302)["clean-lan"]
		environment := newRouteEnvironment(ctx, t, profile)
		defer environment.close()
		fixture := newLiveP2pRoute(t, environment)
		defer fixture.close()
		if _, err := fixture.measureDirect(16); err != nil {
			t.Fatalf("baseline direct P2P: %v", err)
		}
		beforeBlock := fixture.network.snapshot()
		if err := fixture.network.setBlackhole(true, true); err != nil {
			t.Fatalf("blackhole direct P2P: %v", err)
		}
		forwardSubmissionBefore := fixture.network.forwardLink.submittedPackets.Load()
		frame, err := clientconnect.ToFrame(
			&protocol.SimpleMessage{Content: "perfvar blocked direct packet"},
			clientconnect.DefaultProtocolVersion,
		)
		if err != nil {
			t.Fatal(err)
		}
		if !fixture.source.client.SendWithTimeout(
			frame,
			clientconnect.Id(fixture.destination.clientId),
			nil,
			time.Second,
			clientconnect.NoAck(),
		) {
			clientconnect.MessagePoolReturn(frame.MessageBytes)
			t.Fatal("blocked direct P2P packet was not accepted by the sender")
		}
		if !fixture.network.forwardLink.waitForSubmissionCount(ctx, forwardSubmissionBefore+1) {
			t.Fatalf("blocked direct packet did not reach its physical source link: %v", ctx.Err())
		}
		if !fixture.network.waitForTerminalIdle(ctx) {
			t.Fatalf("blocked direct packet did not reach a terminal disposition: %v", ctx.Err())
		}
		afterBlock := fixture.network.snapshot()
		if afterBlock.ForwardDropCount+afterBlock.ReverseDropCount <=
			beforeBlock.ForwardDropCount+beforeBlock.ReverseDropCount {
			t.Fatalf("direct P2P blackhole did not terminally drop its exact packet: before=%+v after=%+v", beforeBlock, afterBlock)
		}
		fallbackBarrier := fixture.routeStateObserver.Snapshot()
		if fallbackBarrier.ActiveRouteCount != 2 {
			t.Fatalf(
				"network-change fallback started from route state=%+v, want two live routes",
				fallbackBarrier,
			)
		}
		clientconnect.NetworkChanged()
		if _, err := waitForRouteCountAfter(
			ctx,
			fixture.routeStateObserver,
			fallbackBarrier,
			1,
		); err != nil {
			t.Fatal(err)
		}
		if !waitForPlatformState(ctx, fixture.source.transport, true) ||
			!waitForPlatformState(ctx, fixture.destination.transport, true) {
			t.Fatal("platform fallback did not reconnect")
		}
		beforeFallback := fixture.source.stats.Snapshot()
		fallback, err := measureProductionRoute(ctx, fixture.source, fixture.destination, 16)
		if err != nil {
			t.Fatalf("platform fallback traffic: %v", err)
		}
		afterFallback := fixture.source.stats.Snapshot()
		if afterFallback.FastSendMessageCount != beforeFallback.FastSendMessageCount {
			t.Fatalf("platform fallback used fast P2P before=%+v after=%+v", beforeFallback, afterFallback)
		}
		if err := fixture.network.setBlackhole(false, false); err != nil {
			t.Fatalf("restore direct P2P: %v", err)
		}
		restoreTime := time.Now()
		restoreBarrier := fixture.routeStateObserver.Snapshot()
		if restoreBarrier.ActiveRouteCount != 1 {
			t.Fatalf(
				"network-change restoration started from route state=%+v, want platform only",
				restoreBarrier,
			)
		}
		clientconnect.NetworkChanged()
		if _, err := waitForRouteCountAfter(
			ctx,
			fixture.routeStateObserver,
			restoreBarrier,
			2,
		); err != nil {
			t.Fatal(err)
		}
		restoreDuration := time.Since(restoreTime)
		direct, err := fixture.measureDirect(32)
		if err != nil {
			t.Fatalf("restored direct P2P traffic: %v", err)
		}
		t.Logf(
			"[perfvar] P2P fallback=%s restore=%s fallback-goodput=%.6fGbps direct-goodput=%.6fGbps",
			fullTunRouteExchangeH1,
			restoreDuration,
			fallback.GoodputGigabits,
			direct.GoodputGigabits,
		)
	})
}

// A live vnet address replacement plus the production network-change hook
// retires old ICE sockets and carries new direct traffic from the new address.
func TestP2pAddressMigrationRebuildsDirectRoute(t *testing.T) {
	if testing.Short() {
		return
	}
	testEnvironment := &server.TestEnv{ApplyDbMigrations: true, RerunCount: 0}
	testEnvironment.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		profile := initialNetworkProfiles(4303)["clean-lan"]
		environment := newRouteEnvironment(ctx, t, profile)
		defer environment.close()
		fixture := newLiveP2pRoute(t, environment)
		defer fixture.close()
		if _, err := fixture.measureDirect(16); err != nil {
			t.Fatalf("baseline direct P2P: %v", err)
		}
		before := fixture.network.addressSnapshot()
		if before.RightAddress != "10.240.0.2" || before.LastReverseSourceAddress != before.RightAddress {
			t.Fatalf("baseline P2P addresses=%+v", before)
		}
		if err := fixture.network.migrateRightAddress(net.ParseIP("10.240.0.3")); err != nil {
			t.Fatal(err)
		}
		migrationTime := time.Now()
		migrationBarrier := fixture.routeStateObserver.Snapshot()
		if migrationBarrier.ActiveRouteCount != 2 {
			t.Fatalf(
				"address migration started from route state=%+v, want two live routes",
				migrationBarrier,
			)
		}
		clientconnect.NetworkChanged()
		withdrawnState, err := waitForRouteCountChange(
			ctx,
			fixture.routeStateObserver,
			migrationBarrier,
			2,
		)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := waitForRouteCountAfter(
			ctx,
			fixture.routeStateObserver,
			withdrawnState,
			2,
		); err != nil {
			t.Fatalf("wait for migrated direct P2P: %v", err)
		}
		migrationDuration := time.Since(migrationTime)
		if _, err := fixture.measureDirect(32); err != nil {
			t.Fatalf("migrated direct P2P traffic: %v", err)
		}
		after := fixture.network.addressSnapshot()
		if after.RightAddress != "10.240.0.3" ||
			after.LastReverseSourceAddress != after.RightAddress ||
			after.AddressMigrationCount != 1 ||
			after.LastReverseSourcePort == 0 {
			t.Fatalf("migrated P2P addresses=%+v", after)
		}
		t.Logf(
			"[perfvar] P2P address-migration duration=%s before=%+v after=%+v",
			migrationDuration,
			before,
			after,
		)
	})
}

// Direct fast P2P and exchange H3 both recover exact inner TCP after a live
// bidirectional outage.
func TestFullTunOutageRecoveryCorrectness(t *testing.T) {
	if testing.Short() {
		return
	}
	testEnvironment := &server.TestEnv{ApplyDbMigrations: true, RerunCount: 0}
	testEnvironment.Run(t, func(t testing.TB) {
		for routeIndex, route := range []fullTunRoute{fullTunRouteP2pFast, fullTunRouteExchangeH3} {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
			profile := allNetworkProfiles(4100 + int64(routeIndex))["rate-10mbps"]
			enableNetworkPeers := route == fullTunRouteP2pFast
			environment := newRouteEnvironmentWithNetworkPeers(ctx, t, profile, enableNetworkPeers)
			path := newFullTunPath(ctx, t, environment, route)
			result, err := measureFullTunOutageRecovery(ctx, path, 512*1024, 300*time.Millisecond)
			if err != nil {
				path.close()
				environment.close()
				cancel()
				t.Fatalf("full-TUN %s outage recovery: %v", route, err)
			}
			if result.RecoveryTime <= 0 || 30*time.Second < result.RecoveryTime {
				t.Fatalf("full-TUN %s recovery=%s", route, result.RecoveryTime)
			}
			t.Logf(
				"[perfvar] outage route=%s duration=%s recovery=%s bytes-at-outage=%d goodput=%.6fGbps",
				route,
				result.OutageDuration,
				result.RecoveryTime,
				result.BytesAtOutage,
				result.Workload.GoodputGigabits,
			)
			verifyErr := path.verifyRoute()
			path.close()
			environment.close()
			cancel()
			if verifyErr != nil {
				t.Fatal(verifyErr)
			}
		}
	})
}

// One fresh impairment fixture keeps failures attributable to a route/profile.
func testFullTunImpairmentCorrectness(
	t *testing.T,
	route fullTunRoute,
	profileName string,
	seed int64,
) {
	if testing.Short() {
		return
	}
	testEnvironment := &server.TestEnv{ApplyDbMigrations: true, RerunCount: 0}
	testEnvironment.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		profile := allNetworkProfiles(seed)[profileName]
		enableNetworkPeers := route == fullTunRouteP2pFast
		environment := newRouteEnvironmentWithNetworkPeers(ctx, t, profile, enableNetworkPeers)
		if route == fullTunRouteP2pFast && profile.Forward.OuterMtu < 1500 {
			cleanProfile := allNetworkProfiles(seed)["clean-lan"]
			environment.accessProfile = cleanProfile
			environment.providerAccessProfile = cleanProfile
			environment.deviceAccessProfile = cleanProfile
		}
		path := newFullTunPath(ctx, t, environment, route)
		result, err := measureFullTunUpload(ctx, path, 128*1024)
		verifyErr := path.verifyRoute()
		path.close()
		environment.close()
		cancel()
		if err != nil {
			t.Fatalf("full-TUN %s/%s: %v", route, profileName, err)
		}
		if verifyErr != nil {
			t.Fatal(verifyErr)
		}
		if result.UsefulByteCount != 128*1024 || result.ContentHash == "" {
			t.Fatalf("full-TUN %s/%s result=%+v", route, profileName, result)
		}
	})
}

// Fast P2P retains exact inner TCP under the LTE loss model.
func TestFullTunP2pFastLossCorrectness(t *testing.T) {
	testFullTunImpairmentCorrectness(t, fullTunRouteP2pFast, "lte", 4200)
}

// Exchange H3 retains exact inner TCP under seeded independent loss.
func TestFullTunExchangeH3LossCorrectness(t *testing.T) {
	testFullTunImpairmentCorrectness(t, fullTunRouteExchangeH3, "loss-10bp", 4201)
}

// Exchange H3 discovers a 1,280-byte outer path without corrupting inner TCP.
func TestFullTunExchangeH3MtuCorrectness(t *testing.T) {
	testFullTunImpairmentCorrectness(t, fullTunRouteExchangeH3, "mtu-1280", 4203)
}

// One exact fast-P2P MTU fixture requires both inner TCP delivery and direct
// carrier evidence that no submitted datagram exceeded the selected path.
func testFullTunP2pFastMtuCorrectness(
	t *testing.T,
	profileName string,
	seed int64,
) {
	testEnvironment := &server.TestEnv{ApplyDbMigrations: true, RerunCount: 0}
	testEnvironment.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(
			context.Background(),
			fullTunP2pFastMtuCorrectnessTimeout(
				fullTunRaceInstrumentationAllowance(),
				fullTunMinimumDirectionalWorkloadTimeout(),
			),
		)
		profile := allNetworkProfiles(seed)[profileName]
		environment := newRouteEnvironmentWithNetworkPeers(ctx, t, profile, true)
		cleanProfile := allNetworkProfiles(seed)["clean-lan"]
		environment.accessProfile = cleanProfile
		environment.providerAccessProfile = cleanProfile
		environment.deviceAccessProfile = cleanProfile
		path := newFullTunPath(ctx, t, environment, fullTunRouteP2pFast)
		result, transferErr := measureFullTunUpload(ctx, path, 128*1024)
		snapshot := path.p2pNetwork.snapshot()
		verifyErr := path.verifyRoute()
		path.close()
		environment.close()
		cancel()
		if transferErr != nil {
			t.Fatalf("P2P fast %s path: %v", profileName, transferErr)
		}
		if verifyErr != nil {
			t.Fatal(verifyErr)
		}
		if result.UsefulByteCount != 128*1024 || result.ContentHash == "" {
			t.Fatalf("P2P fast %s result=%+v", profileName, result)
		}
		if snapshot.MtuDropCount != 0 ||
			uint64(profile.Forward.OuterMtu) < snapshot.MaximumPacketByteCount {
			t.Fatalf(
				"P2P fast carrier exceeded %d-byte path: %+v",
				profile.Forward.OuterMtu,
				snapshot,
			)
		}
	})
}

// The full-TUN race fixture gives route construction a larger allowance than
// production. Its outer context must cover that allowance plus one directional
// workload boundary; otherwise a healthy no-drop P2P route is canceled by the
// fixture's former fixed 90-second deadline before the MTU assertion runs.
func fullTunP2pFastMtuCorrectnessTimeout(
	routeConstructionAllowance time.Duration,
	workloadAllowance time.Duration,
) time.Duration {
	if routeConstructionAllowance == 0 {
		return 90 * time.Second
	}
	return routeConstructionAllowance + workloadAllowance
}

func TestFullTunP2pFastMtuCorrectnessTimeoutCoversRaceRouteAndWorkload(t *testing.T) {
	const routeConstructionAllowance = 4 * time.Minute
	const workloadAllowance = 2 * time.Minute
	if got := fullTunP2pFastMtuCorrectnessTimeout(
		routeConstructionAllowance,
		workloadAllowance,
	); got != 6*time.Minute {
		t.Fatalf("race correctness timeout=%s, want 6m", got)
	}
	if got := fullTunP2pFastMtuCorrectnessTimeout(0, 30*time.Second); got != 90*time.Second {
		t.Fatalf("ordinary correctness timeout=%s, want 90s", got)
	}
}

// Fast P2P retains exact inner TCP on an ordinary 1,500-byte outer path.
func TestFullTunP2pFastMtuCorrectness(t *testing.T) {
	testFullTunP2pFastMtuCorrectness(t, "mtu-1500", 4202)
}

// The serial PERFVAR regression retains exact inner TCP on the 1,400-byte
// outer path that exposed the oversized 1,472-byte carrier fragment.
func TestFullTunP2pFastMtu1400Correctness(t *testing.T) {
	testFullTunP2pFastMtuCorrectness(t, "mtu-1400", 4205)
}

// Fast P2P fragments below IPv6's minimum outer MTU, so a 1,280-byte path
// carries exact inner TCP without a carrier drop.
func TestFullTunP2pFastIpv6MinimumMtuCorrectness(t *testing.T) {
	testFullTunP2pFastMtuCorrectness(t, "mtu-1280", 4204)
}
