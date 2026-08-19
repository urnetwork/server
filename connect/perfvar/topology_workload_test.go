// This file adds route-level workloads that enter through the application TUN,
// cross the selected production carrier, and leave through provider NAT.
package perfvar

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptrace"
	"net/netip"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	quic "github.com/quic-go/quic-go"
	clientconnect "github.com/urnetwork/connect"
	"github.com/urnetwork/server"
)

// A PERFVAR UDP4 provider observation counts the full IPv4 and UDP packet,
// not only its application payload.
const fullTunUdp4HeaderByteCount = 20 + 8

// The fixed UDP4 fixture has no IPv4 options, so every packet contributes one
// minimum IPv4 header, one UDP header, and its exact application payload.
func fullTunUdp4ProviderReturnPacketByteCount(payloadByteCount int) clientconnect.ByteCount {
	return clientconnect.ByteCount(payloadByteCount + fullTunUdp4HeaderByteCount)
}

// The selected route determines how many impaired client-edge segments are
// traversed by one end-to-end exchange packet.
func fullTunOuterRoundTrip(path *fullTunPath) time.Duration {
	linkDelay := func(profile linkProfile) time.Duration {
		return profile.BaseDelay + profile.ProcessingDelay
	}
	profile := path.environment.profile
	if fullTunRouteIsExchange(path.route) {
		roundTrip := linkDelay(path.environment.deviceAccessProfile.Forward) +
			linkDelay(path.environment.providerAccessProfile.Reverse) +
			linkDelay(path.environment.providerAccessProfile.Forward) +
			linkDelay(path.environment.deviceAccessProfile.Reverse)
		if internal := path.environment.internalExchangeProfile; internal != nil {
			roundTrip += linkDelay(internal.Forward) + linkDelay(internal.Reverse)
		}
		return roundTrip
	}
	return time.Duration(max(1, path.p2pHopCount)) *
		(linkDelay(profile.Forward) + linkDelay(profile.Reverse))
}

// Multiple independent uploads share one established route and are timed as
// one application workload.
func measureFullTunParallelUploads(
	ctx context.Context,
	path *fullTunPath,
	flowCount int,
	byteCountPerFlow int64,
) (workloadResult, error) {
	result, err := measureFullTunParallelTCP(ctx, path, true, flowCount, byteCountPerFlow)
	if err == nil {
		err = path.waitForPostWorkloadBoundary(ctx)
	}
	return result, err
}

// Parallel downloads use the same aggregation and exact per-flow hashes.
func measureFullTunParallelDownloads(
	ctx context.Context,
	path *fullTunPath,
	flowCount int,
	byteCountPerFlow int64,
) (workloadResult, error) {
	result, err := measureFullTunParallelTCP(ctx, path, false, flowCount, byteCountPerFlow)
	if err == nil {
		err = path.waitForPostWorkloadBoundary(ctx)
	}
	return result, err
}

// One helper keeps the concurrent upload and download measurement boundaries
// identical.
func measureFullTunParallelTCP(
	ctx context.Context,
	path *fullTunPath,
	upload bool,
	flowCount int,
	byteCountPerFlow int64,
) (workloadResult, error) {
	if flowCount <= 0 {
		return workloadResult{}, fmt.Errorf("parallel upload flow count %d is not positive", flowCount)
	}
	results := make(chan workloadResult, flowCount)
	errors := make(chan error, flowCount)
	startTime := time.Now()
	var waitGroup sync.WaitGroup
	for range flowCount {
		waitGroup.Add(1)
		go func() {
			defer waitGroup.Done()
			var result workloadResult
			var err error
			deadlineByteCount := int64(flowCount) * byteCountPerFlow
			if upload {
				result, err = measureFullTunUploadWithDeadlineBytes(
					ctx,
					path,
					byteCountPerFlow,
					deadlineByteCount,
				)
			} else {
				result, err = measureFullTunDownloadWithDeadlineBytes(
					ctx,
					path,
					byteCountPerFlow,
					deadlineByteCount,
				)
			}
			if err != nil {
				errors <- err
				return
			}
			results <- result
		}()
	}
	waitGroup.Wait()
	close(results)
	close(errors)
	if err := <-errors; err != nil {
		return workloadResult{}, err
	}
	result := workloadResult{Duration: time.Since(startTime)}
	for flowResult := range results {
		result.UsefulByteCount += flowResult.UsefulByteCount
		result.AllocatedByteCount += flowResult.AllocatedByteCount
		result.AllocationCount += flowResult.AllocationCount
		result.GarbageCollectionCount += flowResult.GarbageCollectionCount
		result.GarbageCollectionPause += flowResult.GarbageCollectionPause
	}
	result.ContentHash = deterministicPayloadHash(byteCountPerFlow)
	return finishWorkloadResult(result), nil
}

// Sequence-numbered UDP gives exact delivery, corruption, duplication, and
// same-clock one-way latency observations through provider NAT.
func measureFullTunUDP(
	ctx context.Context,
	path *fullTunPath,
	duration time.Duration,
	offeredBitsPerSecond int64,
	payloadByteCount int,
) (workloadResult, error) {
	return measureFullTunUDPDirection(
		ctx,
		path,
		true,
		duration,
		offeredBitsPerSecond,
		payloadByteCount,
	)
}

// The first physical carrier leaving the application source is sufficient to
// prove that one marker has passed every hidden producer above the userspace
// scheduler. Exchange routes can select among several current access links, so
// they wait for any post-send carrier generation and then join all of them.
func fullTunUDPSourcePublicationLinks(path *fullTunPath, upload bool) []*directionalLink {
	if path.p2pNetwork != nil {
		if upload {
			return []*directionalLink{path.p2pNetwork.reverseLink}
		}
		return []*directionalLink{path.p2pNetwork.forwardLink}
	}
	if path.streamP2pNetwork != nil {
		if upload {
			return []*directionalLink{path.streamP2pNetwork.hopForwardLinks[0]}
		}
		lastHopIndex := len(path.streamP2pNetwork.hopReverseLinks) - 1
		return []*directionalLink{path.streamP2pNetwork.hopReverseLinks[lastHopIndex]}
	}
	return path.environment.network.directionalLinks()
}

// fullTunCarrierTerminalDropCount distinguishes a marker that reached a
// terminal modeled drop from one whose physical delivery is complete but whose
// application receiver has not yet published the positive receipt.
func fullTunCarrierTerminalDropCount(path *fullTunPath) uint64 {
	links := path.environment.network.directionalLinks()
	if path.p2pNetwork != nil {
		links = append(links, path.p2pNetwork.directionalLinks()...)
	}
	if path.streamP2pNetwork != nil {
		links = append(links, path.streamP2pNetwork.directionalLinks()...)
	}
	var dropCount uint64
	for _, link := range links {
		snapshot := link.snapshot()
		dropCount += snapshot.LossDropPacketCount +
			snapshot.MtuDropPacketCount +
			snapshot.QueueDropPacketCount +
			snapshot.OutageDropPacketCount +
			snapshot.ReceiverDropPacketCount +
			snapshot.CanceledDropPacketCount
	}
	return dropCount
}

// One waiter per eligible physical link removes polling and timing guesses.
// Cancellation joins every losing waiter before returning.
func waitForAnyDirectionalLinkSubmission(
	ctx context.Context,
	links []*directionalLink,
	before []uint64,
) bool {
	if len(links) == 0 || len(links) != len(before) {
		return false
	}
	waitCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	completed := make(chan struct{}, len(links))
	var waitGroup sync.WaitGroup
	for linkIndex, link := range links {
		waitGroup.Add(1)
		go func() {
			defer waitGroup.Done()
			if link.waitForSubmissionCount(waitCtx, before[linkIndex]+1) {
				select {
				case completed <- struct{}{}:
				default:
				}
			}
		}()
	}
	observed := false
	select {
	case <-ctx.Done():
	case <-completed:
		observed = true
	}
	cancel()
	waitGroup.Wait()
	return observed
}

// The expected provider-return flow is derived independently from the two
// application socket tuples rather than trusting the first observed packet.
func fullTunProviderReturnUdpFlowKey(
	destinationId clientconnect.Id,
	sourceAddress net.Addr,
	destinationAddress net.Addr,
) (clientconnect.RemoteUserNatProviderReturnFlowKey, error) {
	parseAddress := func(name string, address net.Addr) (netip.AddrPort, error) {
		if address == nil {
			return netip.AddrPort{}, fmt.Errorf("%s address is nil", name)
		}
		addressPort, err := netip.ParseAddrPort(address.String())
		if err != nil {
			return netip.AddrPort{}, fmt.Errorf("parse %s address %q: %w", name, address, err)
		}
		return netip.AddrPortFrom(addressPort.Addr().Unmap(), addressPort.Port()), nil
	}
	if destinationId == (clientconnect.Id{}) {
		return clientconnect.RemoteUserNatProviderReturnFlowKey{}, fmt.Errorf("provider-return destination ID is empty")
	}
	source, err := parseAddress("source", sourceAddress)
	if err != nil {
		return clientconnect.RemoteUserNatProviderReturnFlowKey{}, err
	}
	destination, err := parseAddress("destination", destinationAddress)
	if err != nil {
		return clientconnect.RemoteUserNatProviderReturnFlowKey{}, err
	}
	if source.Addr().Is4() != destination.Addr().Is4() {
		return clientconnect.RemoteUserNatProviderReturnFlowKey{}, fmt.Errorf(
			"provider-return flow mixes source %s and destination %s IP families",
			source,
			destination,
		)
	}
	flowKey := clientconnect.RemoteUserNatProviderReturnFlowKey{
		DestinationId:   destinationId,
		Protocol:        clientconnect.IpProtocolUdp,
		SourcePort:      source.Port(),
		DestinationPort: destination.Port(),
		Valid:           true,
	}
	if source.Addr().Is4() {
		sourceIp := source.Addr().As4()
		destinationIp := destination.Addr().As4()
		copy(flowKey.SourceIp[:], sourceIp[:])
		copy(flowKey.DestinationIp[:], destinationIp[:])
		flowKey.IpVersion = 4
	} else {
		sourceIp := source.Addr().As16()
		destinationIp := destination.Addr().As16()
		copy(flowKey.SourceIp[:], sourceIp[:])
		copy(flowKey.DestinationIp[:], destinationIp[:])
		flowKey.IpVersion = 6
	}
	return flowKey, nil
}

// Download mode opens provider NAT with one unmeasured registration datagram,
// then sends the measured sequence from host egress back to the application.
func measureFullTunUDPDirection(
	ctx context.Context,
	path *fullTunPath,
	upload bool,
	duration time.Duration,
	offeredBitsPerSecond int64,
	payloadByteCount int,
) (workloadResult, error) {
	if payloadByteCount < 24 {
		return workloadResult{}, fmt.Errorf("UDP payload %d is smaller than its 24-byte header", payloadByteCount)
	}
	if offeredBitsPerSecond <= 0 {
		return workloadResult{}, fmt.Errorf("UDP offered rate %d is not positive", offeredBitsPerSecond)
	}
	listener, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0})
	if err != nil {
		return workloadResult{}, err
	}
	defer listener.Close()
	connection, err := path.appTun.DialContext(ctx, "udp", listener.LocalAddr().String())
	if err != nil {
		return workloadResult{}, err
	}
	defer connection.Close()
	var providerAddress net.Addr
	if !upload {
		if _, err := connection.Write([]byte{1}); err != nil {
			return workloadResult{}, fmt.Errorf("open UDP provider NAT: %w", err)
		}
		registration := make([]byte, 1)
		if err := listener.SetReadDeadline(time.Now().Add(max(10*time.Second, 8*fullTunOuterRoundTrip(path)))); err != nil {
			return workloadResult{}, err
		}
		readByteCount, sourceAddress, err := listener.ReadFrom(registration)
		if err != nil {
			return workloadResult{}, fmt.Errorf("receive UDP provider registration: %w", err)
		}
		if readByteCount != 1 || registration[0] != 1 {
			return workloadResult{}, fmt.Errorf("UDP provider registration was corrupted")
		}
		providerAddress = sourceAddress
		if err := listener.SetReadDeadline(time.Time{}); err != nil {
			return workloadResult{}, err
		}
		registrationBoundary, ok := path.deviceNoAckSends.boundary(ctx)
		if !ok || !path.deviceNoAckSends.waitThrough(ctx, registrationBoundary) {
			return workloadResult{}, fmt.Errorf("join UDP provider registration source: %w", ctx.Err())
		}
		if err := path.waitForSetupBoundary(ctx); err != nil {
			return workloadResult{}, fmt.Errorf("join UDP provider registration: %w", err)
		}
		carrierMeasurementStart, err := beginPerfvarCarrierMeasurement(path)
		if err != nil {
			return workloadResult{}, err
		}
		path.setCarrierMeasurementStart(carrierMeasurementStart)
	}
	packetInterval := time.Duration(float64(time.Second) * float64(payloadByteCount*8) / float64(offeredBitsPerSecond))
	if packetInterval <= 0 {
		return workloadResult{}, fmt.Errorf("UDP offered rate %d is too high", offeredBitsPerSecond)
	}
	targetPacketCount := int64(duration / packetInterval)
	if targetPacketCount <= 0 {
		return workloadResult{}, fmt.Errorf("UDP duration %s produces no packets", duration)
	}

	receivedSequences := map[uint64]bool{}
	latencies := []time.Duration{}
	var stateLock sync.Mutex
	var deliveredPacketCount int64
	var duplicatePacketCount int64
	var reorderedPacketCount int64
	var corruptPacketCount int64
	var highestSequence uint64
	terminalSequence := uint64(targetPacketCount + 1)
	terminalReceived := make(chan struct{}, 1)
	receiverDone := make(chan struct{})
	go func() {
		defer close(receiverDone)
		packetBytes := make([]byte, 64*1024)
		for {
			var readByteCount int
			var readErr error
			if upload {
				readByteCount, _, readErr = listener.ReadFrom(packetBytes)
			} else {
				readByteCount, readErr = connection.Read(packetBytes)
			}
			if readErr != nil {
				return
			}
			stateLock.Lock()
			if readByteCount != payloadByteCount {
				corruptPacketCount += 1
				stateLock.Unlock()
				continue
			}
			sequence := binary.BigEndian.Uint64(packetBytes[0:8])
			sendUnixNano := int64(binary.BigEndian.Uint64(packetBytes[8:16]))
			checksum := binary.BigEndian.Uint64(packetBytes[16:24])
			if checksum != sequence^uint64(sendUnixNano) || sequence == 0 || terminalSequence < sequence {
				corruptPacketCount += 1
				stateLock.Unlock()
				continue
			}
			if sequence == terminalSequence {
				stateLock.Unlock()
				if path.beforeUdpTerminalReceiptForTest != nil {
					path.beforeUdpTerminalReceiptForTest(ctx, upload)
				}
				select {
				case terminalReceived <- struct{}{}:
				default:
				}
				if path.afterUdpTerminalReceiptForTest != nil {
					path.afterUdpTerminalReceiptForTest(upload)
				}
				continue
			}
			if receivedSequences[sequence] {
				duplicatePacketCount += 1
			} else {
				receivedSequences[sequence] = true
				deliveredPacketCount += 1
				if sequence < highestSequence {
					reorderedPacketCount += 1
				} else {
					highestSequence = sequence
				}
				latencies = append(latencies, time.Since(time.Unix(0, sendUnixNano)))
			}
			stateLock.Unlock()
		}
	}()

	payload := make([]byte, payloadByteCount)
	stampPayload := func(sequence uint64) {
		sendUnixNano := time.Now().UnixNano()
		binary.BigEndian.PutUint64(payload[0:8], sequence)
		binary.BigEndian.PutUint64(payload[8:16], uint64(sendUnixNano))
		binary.BigEndian.PutUint64(payload[16:24], sequence^uint64(sendUnixNano))
	}
	writePayload := func() error {
		if upload {
			_, err := connection.Write(payload)
			return err
		}
		_, err := listener.WriteTo(payload, providerAddress)
		return err
	}
	var providerReturnFlowKey clientconnect.RemoteUserNatProviderReturnFlowKey
	var measuredProviderReturnWindow providerReturnFlowWindow
	var measuredBridgeWindow fullTunBridgeFlowWindow
	if upload {
		bridgeFlowKey, bridgeFlowErr := fullTunBridgeUdpFlowKey(
			connection.LocalAddr(),
			listener.LocalAddr(),
		)
		if bridgeFlowErr != nil {
			return workloadResult{}, fmt.Errorf("derive measured bridge flow: %w", bridgeFlowErr)
		}
		var windowOk bool
		measuredBridgeWindow, windowOk = path.bridgeSends.beginFlowWindow(bridgeFlowKey)
		if !windowOk {
			return workloadResult{}, fmt.Errorf("begin measured bridge flow window for %+v", bridgeFlowKey)
		}
	} else {
		deviceClient := path.deviceClient.Load()
		if deviceClient == nil || deviceClient.ClientId() == (clientconnect.Id{}) {
			return workloadResult{}, fmt.Errorf("derive measured provider-return flow before the generated device client is ready")
		}
		providerReturnFlowKey, err = fullTunProviderReturnUdpFlowKey(
			deviceClient.ClientId(),
			listener.LocalAddr(),
			connection.LocalAddr(),
		)
		if err != nil {
			return workloadResult{}, fmt.Errorf("derive measured provider-return flow: %w", err)
		}
		var windowOk bool
		measuredProviderReturnWindow, windowOk = path.providerReturns.beginFlowWindow(
			ctx,
			providerReturnFlowKey,
		)
		if !windowOk {
			return workloadResult{}, fmt.Errorf(
				"begin measured provider-return flow window for %+v: %w",
				providerReturnFlowKey,
				ctx.Err(),
			)
		}
	}
	providerReturnFailureCountBefore := path.providerReturns.failures.Load()
	sourceTracker := path.deviceNoAckSends
	if !upload {
		sourceTracker = path.providerNoAckSends
	}
	sourceFailureCountBefore := sourceTracker.failures()
	startTime := time.Now()
	for packetIndex := int64(0); packetIndex < targetPacketCount; packetIndex += 1 {
		targetTime := startTime.Add(time.Duration(packetIndex) * packetInterval)
		if wait := time.Until(targetTime); 0 < wait {
			time.Sleep(wait)
		}
		sequence := uint64(packetIndex + 1)
		stampPayload(sequence)
		if err := writePayload(); err != nil {
			return workloadResult{}, err
		}
	}
	sendDuration := time.Since(startTime)
	// First prove that every measured application datagram reached its source
	// SendPack. No terminal marker has entered the path yet.
	if upload {
		targetBridgeByteCount := clientconnect.ByteCount(targetPacketCount) *
			fullTunUdp4ProviderReturnPacketByteCount(payloadByteCount)
		bridgeBoundary, bridgeBoundaryOk := path.bridgeSends.flowBoundary(
			ctx,
			measuredBridgeWindow,
			targetPacketCount,
			targetBridgeByteCount,
		)
		if !bridgeBoundaryOk || !path.bridgeSends.waitThrough(ctx, bridgeBoundary) {
			return workloadResult{}, fmt.Errorf(
				"application bridge did not complete measured flow %+v at %d packets and %d bytes: %w; failures=%d",
				measuredBridgeWindow.flowKey,
				targetPacketCount,
				targetBridgeByteCount,
				ctx.Err(),
				path.bridgeSends.failureCount.Load(),
			)
		}
	} else {
		targetProviderReturnByteCount := clientconnect.ByteCount(targetPacketCount) *
			fullTunUdp4ProviderReturnPacketByteCount(payloadByteCount)
		providerReturnBoundary, boundaryOk := path.providerReturns.flowBoundary(
			ctx,
			measuredProviderReturnWindow,
			targetPacketCount,
			targetProviderReturnByteCount,
		)
		if !boundaryOk || !path.providerReturns.waitThrough(ctx, providerReturnBoundary) {
			return workloadResult{}, fmt.Errorf(
				"provider return source did not complete flow %+v at %d packets and %d bytes: %w; observed_packets=%d observed_bytes=%d failures=%d congestion=%+v",
				providerReturnFlowKey,
				targetPacketCount,
				targetProviderReturnByteCount,
				ctx.Err(),
				path.providerReturns.startedPacketCount.Load(),
				path.providerReturns.startedByteCount.Load(),
				path.providerReturns.failures.Load()-providerReturnFailureCountBefore,
				path.providerRemoteNat.CongestionDropStats(),
			)
		}
	}
	sourceSerializationBoundary, boundaryOk := sourceTracker.boundary(ctx)
	if !boundaryOk {
		return workloadResult{}, fmt.Errorf("snapshot UDP source serialization boundary: %w", ctx.Err())
	}
	if !sourceTracker.waitThrough(ctx, sourceSerializationBoundary) {
		return workloadResult{}, fmt.Errorf(
			"UDP source serialization stopped at %d/%d: %w",
			sourceTracker.completedCount.Load(),
			len(sourceSerializationBoundary.entries),
			ctx.Err(),
		)
	}
	if failureCount := sourceTracker.failures() - sourceFailureCountBefore; failureCount != 0 {
		return workloadResult{}, fmt.Errorf("UDP source had %d asynchronous route-write failures", failureCount)
	}
	if err := path.waitForIntermediateWorkloadBoundary(ctx); err != nil {
		return workloadResult{}, fmt.Errorf("join UDP measured packets: %w", err)
	}
	markerAttemptCount := 0
	markerReceived := false
	for !markerReceived {
		markerAttemptCount += 1
		terminalDropCountBefore := fullTunCarrierTerminalDropCount(path)
		publicationLinks := fullTunUDPSourcePublicationLinks(path, upload)
		publicationBefore := make([]uint64, len(publicationLinks))
		for linkIndex, link := range publicationLinks {
			publicationBefore[linkIndex] = link.submittedPackets.Load()
		}
		if path.beforeUdpTerminalMarkerForTest != nil {
			if err := path.beforeUdpTerminalMarkerForTest(ctx, upload, markerAttemptCount); err != nil {
				return workloadResult{}, fmt.Errorf("before UDP terminal marker %d: %w", markerAttemptCount, err)
			}
		}
		stampPayload(terminalSequence)
		if err := writePayload(); err != nil {
			return workloadResult{}, fmt.Errorf("send UDP terminal marker %d: %w", markerAttemptCount, err)
		}
		if upload {
			targetBridgePacketCount := targetPacketCount + int64(markerAttemptCount)
			targetBridgeByteCount := clientconnect.ByteCount(targetBridgePacketCount) *
				fullTunUdp4ProviderReturnPacketByteCount(payloadByteCount)
			bridgeBoundary, bridgeBoundaryOk := path.bridgeSends.flowBoundary(
				ctx,
				measuredBridgeWindow,
				targetBridgePacketCount,
				targetBridgeByteCount,
			)
			if !bridgeBoundaryOk || !path.bridgeSends.waitThrough(ctx, bridgeBoundary) {
				return workloadResult{}, fmt.Errorf(
					"marker %d bridge source did not complete flow %+v at %d packets and %d bytes: %w; failures=%d",
					markerAttemptCount,
					measuredBridgeWindow.flowKey,
					targetBridgePacketCount,
					targetBridgeByteCount,
					ctx.Err(),
					path.bridgeSends.failureCount.Load(),
				)
			}
		} else {
			targetProviderReturnPacketCount := targetPacketCount + int64(markerAttemptCount)
			targetProviderReturnByteCount := clientconnect.ByteCount(targetProviderReturnPacketCount) *
				fullTunUdp4ProviderReturnPacketByteCount(payloadByteCount)
			providerReturnBoundary, providerBoundaryOk := path.providerReturns.flowBoundary(
				ctx,
				measuredProviderReturnWindow,
				targetProviderReturnPacketCount,
				targetProviderReturnByteCount,
			)
			if !providerBoundaryOk || !path.providerReturns.waitThrough(ctx, providerReturnBoundary) {
				return workloadResult{}, fmt.Errorf(
					"marker %d provider item did not complete flow %+v at %d packets and %d bytes: %w; failures=%d congestion=%+v",
					markerAttemptCount,
					providerReturnFlowKey,
					targetProviderReturnPacketCount,
					targetProviderReturnByteCount,
					ctx.Err(),
					path.providerReturns.failures.Load()-providerReturnFailureCountBefore,
					path.providerRemoteNat.CongestionDropStats(),
				)
			}
		}
		markerSerializationBoundary, boundaryOk := sourceTracker.boundary(ctx)
		if !boundaryOk || !sourceTracker.waitThrough(ctx, markerSerializationBoundary) {
			return workloadResult{}, fmt.Errorf(
				"wait for marker %d source serialization: %w",
				markerAttemptCount,
				ctx.Err(),
			)
		}
		if failureCount := sourceTracker.failures() - sourceFailureCountBefore; failureCount != 0 {
			return workloadResult{}, fmt.Errorf(
				"UDP source had %d measured or terminal route-write failures",
				failureCount,
			)
		}
		if !waitForAnyDirectionalLinkSubmission(ctx, publicationLinks, publicationBefore) {
			return workloadResult{}, fmt.Errorf(
				"marker %d never reached its source carrier: %w",
				markerAttemptCount,
				ctx.Err(),
			)
		}
		// The source carrier has accepted this attempt, so its terminal link
		// disposition can be joined before waiting for every reliable Pack that
		// the attempt may have triggered. This test-only seam lets a regression
		// restore an intentionally blackholed link at that exact ownership
		// boundary. Production still requires the all-Pack fixed point below.
		if !path.waitForCarrierQuiescent(ctx) {
			return workloadResult{}, fmt.Errorf(
				"join marker %d source carrier: %w",
				markerAttemptCount,
				ctx.Err(),
			)
		}
		markerReachedTerminalDrop := terminalDropCountBefore < fullTunCarrierTerminalDropCount(path)
		if path.afterUdpTerminalCarrierForTest != nil {
			if err := path.afterUdpTerminalCarrierForTest(ctx, upload, markerAttemptCount); err != nil {
				return workloadResult{}, fmt.Errorf("after UDP terminal carrier %d: %w", markerAttemptCount, err)
			}
		}
		if err := path.waitForIntermediateWorkloadBoundary(ctx); err != nil {
			return workloadResult{}, fmt.Errorf(
				"join marker %d: %w",
				markerAttemptCount,
				err,
			)
		}
		if path.afterUdpTerminalMarkerForTest != nil {
			if err := path.afterUdpTerminalMarkerForTest(ctx, upload, markerAttemptCount); err != nil {
				return workloadResult{}, fmt.Errorf("after UDP terminal marker %d: %w", markerAttemptCount, err)
			}
		}
		if markerReachedTerminalDrop {
			select {
			case <-terminalReceived:
				markerReceived = true
			default:
			}
			continue
		}
		select {
		case <-ctx.Done():
			return workloadResult{}, fmt.Errorf(
				"wait for UDP terminal marker receipt after %d attempts: %w",
				markerAttemptCount,
				ctx.Err(),
			)
		case <-receiverDone:
			return workloadResult{}, fmt.Errorf("UDP receiver ended before its terminal marker")
		case <-terminalReceived:
			markerReceived = true
		}
	}
	if upload {
		if !path.bridgeSends.finishFlowWindow(measuredBridgeWindow) {
			return workloadResult{}, fmt.Errorf(
				"finish bridge flow %+v after %d marker attempts",
				measuredBridgeWindow.flowKey,
				markerAttemptCount,
			)
		}
	} else {
		if !path.providerReturns.finishFlowWindow(ctx, measuredProviderReturnWindow) {
			return workloadResult{}, fmt.Errorf(
				"finish provider-return flow %+v after %d marker attempts: %w",
				providerReturnFlowKey,
				markerAttemptCount,
				ctx.Err(),
			)
		}
	}
	// The successful attempt has a positive application receipt, exact source
	// disposition, source-carrier publication, and route-wide terminal join.
	// Counting every attempt is conservative and prevents hidden Pion queues
	// from omitting measured carrier bytes.
	path.setCarrierMeasurementEnd(snapshotPerfvarCarrier(path), markerAttemptCount)
	_ = connection.Close()
	_ = listener.Close()
	select {
	case <-ctx.Done():
		return workloadResult{}, fmt.Errorf("join UDP receiver: %w", ctx.Err())
	case <-receiverDone:
	}
	stateLock.Lock()
	result := workloadResult{
		UsefulByteCount:            deliveredPacketCount * int64(payloadByteCount),
		OfferedPacketCount:         targetPacketCount,
		DeliveredPacketCount:       deliveredPacketCount,
		DuplicatePacketCount:       duplicatePacketCount,
		ReorderedPacketCount:       reorderedPacketCount,
		CorruptPacketCount:         corruptPacketCount,
		TerminalMarkerAttemptCount: markerAttemptCount,
		Duration:                   sendDuration,
		Latency:                    summarizeLatencies(latencies),
	}
	stateLock.Unlock()
	if err := path.waitForPostWorkloadBoundary(ctx); err != nil {
		return workloadResult{}, err
	}
	return finishWorkloadResult(result), nil
}

// Marker ordering, exact bridge disposition, and simulator terminal idle keep
// completion behind an explicitly held upload terminal receipt.
func TestFullTunUDPUploadTerminalBarriersWaitForHeldTerminalReceipt(t *testing.T) {
	testFullTunUDPTerminalBarriersWaitForHeldTerminalReceipt(t, true)
}

// The provider-to-application direction has the same exact completion
// contract and does not rely on its former fixed drain duration.
func TestFullTunUDPDownloadTerminalBarriersWaitForHeldTerminalReceipt(t *testing.T) {
	testFullTunUDPTerminalBarriersWaitForHeldTerminalReceipt(t, false)
}

// Both directions share one implementation so their completion assertions
// cannot accidentally diverge while retaining independent top-level tests.
func testFullTunUDPTerminalBarriersWaitForHeldTerminalReceipt(t *testing.T, upload bool) {
	t.Helper()
	if testing.Short() {
		return
	}
	testEnvironment := &server.TestEnv{ApplyDbMigrations: true, RerunCount: 0}
	testEnvironment.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		profile := initialNetworkProfiles(20260810)["clean-lan"]
		environment := newRouteEnvironmentWithNetworkPeers(ctx, t, profile, false)
		defer environment.close()
		path := newFullTunPath(ctx, t, environment, fullTunRouteExchangeH1)
		defer path.close()
		releaseReceipt := make(chan struct{})
		var releaseOnce sync.Once
		release := func() {
			releaseOnce.Do(func() { close(releaseReceipt) })
		}
		defer release()
		receiptHeld := make(chan struct{})
		var receiptHeldOnce sync.Once
		path.beforeUdpTerminalReceiptForTest = func(
			receiptCtx context.Context,
			receiptUpload bool,
		) {
			if receiptUpload != upload {
				return
			}
			receiptHeldOnce.Do(func() { close(receiptHeld) })
			select {
			case <-receiptCtx.Done():
			case <-releaseReceipt:
			}
		}
		completed := make(chan fullTunUDPTestCompletion, 1)
		go func() {
			result, err := measureFullTunUDPDirection(
				ctx,
				path,
				upload,
				time.Millisecond,
				8_000_000,
				1000,
			)
			completed <- fullTunUDPTestCompletion{result: result, err: err}
		}()
		select {
		case <-receiptHeld:
		case completion := <-completed:
			t.Fatalf("full-TUN UDP returned before its terminal delivery was held: %+v", completion)
		case <-ctx.Done():
			t.Fatalf("wait for held full-TUN UDP terminal delivery: %v", ctx.Err())
		}
		select {
		case completion := <-completed:
			t.Fatalf("full-TUN UDP returned through its held terminal delivery: %+v", completion)
		default:
		}
		release()
		var completion fullTunUDPTestCompletion
		select {
		case completion = <-completed:
		case <-ctx.Done():
			t.Fatalf("full-TUN UDP did not complete after terminal-delivery release: %v", ctx.Err())
		}
		if completion.err != nil {
			t.Fatal(completion.err)
		}
		result := completion.result
		direction := "download"
		if upload {
			direction = "upload"
		}
		if result.OfferedPacketCount != 1 || result.DeliveredPacketCount != result.OfferedPacketCount {
			t.Fatalf(
				"held-delivery full-TUN UDP %s delivery=%d/%d",
				direction,
				result.DeliveredPacketCount,
				result.OfferedPacketCount,
			)
		}
		if result.CorruptPacketCount != 0 {
			t.Fatalf("held-delivery full-TUN UDP %s result=%+v", direction, result)
		}
	})
}

// fullTunUDPTestCompletion carries one asynchronous regression result without
// calling testing methods from a workload goroutine.
type fullTunUDPTestCompletion struct {
	result workloadResult
	err    error
}

// fullTunHasCarrierMeasurementEnd reads the test-only frozen boundary under
// the same lock used by measurement publication.
func fullTunHasCarrierMeasurementEnd(path *fullTunPath) bool {
	path.measurementLock.Lock()
	defer path.measurementLock.Unlock()
	return path.carrierMeasurementEnd != nil
}

// A Started callback held before tracker publication cannot be skipped by the
// marker boundary. The frozen carrier snapshot remains absent until that exact
// source publication is released.
func TestFullTunUDPTerminalMarkerWaitsForHeldSourcePublication(t *testing.T) {
	if testing.Short() {
		return
	}
	testEnvironment := &server.TestEnv{ApplyDbMigrations: true, RerunCount: 0}
	testEnvironment.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		profile := initialNetworkProfiles(20260811)["clean-lan"]
		environment := newRouteEnvironmentWithNetworkPeers(ctx, t, profile, false)
		defer environment.close()
		path := newFullTunPath(ctx, t, environment, fullTunRouteExchangeH1)
		defer path.close()

		publicationHeld := make(chan struct{})
		releasePublication := make(chan struct{})
		var releaseOnce sync.Once
		release := func() {
			releaseOnce.Do(func() {
				close(releasePublication)
			})
		}
		defer release()
		var markerArmed atomic.Bool
		var publicationHeldOnce atomic.Bool
		path.beforeUdpTerminalMarkerForTest = func(
			ctx context.Context,
			upload bool,
			attempt int,
		) error {
			if !upload || attempt != 1 {
				return fmt.Errorf("unexpected marker upload=%t attempt=%d", upload, attempt)
			}
			markerArmed.Store(true)
			return nil
		}
		path.devicePackSends.setBeforeObserverPublishForTest(func(
			observation clientconnect.SendPackLifecycleObservation,
		) {
			if markerArmed.Load() &&
				observation.Phase == clientconnect.SendPackLifecyclePhaseStarted &&
				!observation.AckRequired &&
				observation.DestinationId == path.providerClientId &&
				publicationHeldOnce.CompareAndSwap(false, true) {
				close(publicationHeld)
				select {
				case <-releasePublication:
				case <-ctx.Done():
				}
			}
		})
		workloadDone := make(chan fullTunUDPTestCompletion, 1)
		go func() {
			result, err := measureFullTunUDPDirection(
				ctx,
				path,
				true,
				20*time.Millisecond,
				5_000_000,
				1000,
			)
			workloadDone <- fullTunUDPTestCompletion{result: result, err: err}
		}()
		select {
		case <-publicationHeld:
		case completion := <-workloadDone:
			t.Fatalf("UDP workload returned before held source publication: %+v", completion)
		case <-ctx.Done():
			t.Fatalf("UDP marker source publication was not observed: %v", ctx.Err())
		}
		if fullTunHasCarrierMeasurementEnd(path) {
			t.Fatal("carrier end was frozen while marker source publication was held")
		}
		select {
		case completion := <-workloadDone:
			t.Fatalf("UDP workload returned while source publication was held: %+v", completion)
		default:
		}
		release()
		select {
		case completion := <-workloadDone:
			if completion.err != nil {
				t.Fatal(completion.err)
			}
			if completion.result.TerminalMarkerAttemptCount != 1 {
				t.Fatalf("UDP marker result=%+v", completion.result)
			}
		case <-ctx.Done():
			t.Fatalf("UDP workload did not join released source publication: %v", ctx.Err())
		}
	})
}

// The first marker is deterministically blackholed on its direct source link.
// Restoration occurs only after the source carrier reports its exact terminal
// drop, but before reliable control Packs are joined. Attempt two therefore
// proves retry ordering without creating a teardown cycle in the test itself.
func TestFullTunUDPTerminalMarkerRetriesAfterExactCarrierDrop(t *testing.T) {
	if testing.Short() {
		return
	}
	testEnvironment := &server.TestEnv{ApplyDbMigrations: true, RerunCount: 0}
	testEnvironment.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		profile := initialNetworkProfiles(20260812)["clean-lan"]
		environment := newRouteEnvironmentWithNetworkPeers(ctx, t, profile, true)
		defer environment.close()
		path := newFullTunPath(ctx, t, environment, fullTunRouteP2pFast)
		defer path.close()
		sourceLink := path.p2pNetwork.reverseLink
		if err := path.waitForMeasurementBoundary(ctx); err != nil {
			t.Fatalf("terminal-marker carrier start did not become idle: %v", err)
		}
		carrierBefore, err := beginPerfvarCarrierMeasurement(path)
		if err != nil {
			t.Fatalf("begin terminal-marker carrier measurement: %v", err)
		}
		attemptBefore := map[int]directionalLinkSnapshot{}
		attemptAfter := map[int]directionalLinkSnapshot{}
		path.beforeUdpTerminalMarkerForTest = func(
			ctx context.Context,
			upload bool,
			attempt int,
		) error {
			if !upload {
				return fmt.Errorf("drop regression unexpectedly used download")
			}
			attemptBefore[attempt] = sourceLink.snapshot()
			if attempt == 1 {
				return path.p2pNetwork.setBlackhole(false, true)
			}
			return nil
		}
		path.afterUdpTerminalCarrierForTest = func(
			ctx context.Context,
			upload bool,
			attempt int,
		) error {
			attemptAfter[attempt] = sourceLink.snapshot()
			if attempt != 1 {
				return nil
			}
			before := attemptBefore[attempt]
			after := attemptAfter[attempt]
			if after.OutageDropPacketCount <= before.OutageDropPacketCount {
				return fmt.Errorf("first marker did not reach terminal outage drop: before=%+v after=%+v", before, after)
			}
			if after.QueuedPacketCount != 0 || after.QueuedByteCount != 0 {
				return fmt.Errorf("first marker carrier hook ran before source link terminal idle: %+v", after)
			}
			return path.p2pNetwork.setBlackhole(false, false)
		}
		result, err := measureFullTunUDPDirection(
			ctx,
			path,
			true,
			20*time.Millisecond,
			5_000_000,
			1000,
		)
		if err != nil {
			t.Fatal(err)
		}
		if result.TerminalMarkerAttemptCount != 2 {
			t.Fatalf("terminal marker attempts=%d, want 2; result=%+v", result.TerminalMarkerAttemptCount, result)
		}
		if len(attemptBefore) != 2 || len(attemptAfter) != 2 {
			t.Fatalf("terminal marker hook attempts before=%v after=%v", attemptBefore, attemptAfter)
		}
		carrier := observePerfvarWorkloadCarrier(path, carrierBefore)
		if !carrier.FenceInclusive || carrier.FenceApplicationPacketCount != 2 {
			t.Fatalf("terminal marker fence=%+v", carrier)
		}
		if carrier.P2PNetwork.Reverse.OutageDropPacketCount == 0 {
			t.Fatalf("terminal marker carrier omitted first outage drop: %+v", carrier.P2PNetwork)
		}
	})
}

// Physical delivery may finish before the receiving application publishes its
// positive marker receipt. Holding that exact callback proves link idle alone
// cannot freeze the workload's carrier end.
func TestFullTunUDPTerminalMarkerWaitsForDownstreamReceipt(t *testing.T) {
	if testing.Short() {
		return
	}
	testEnvironment := &server.TestEnv{ApplyDbMigrations: true, RerunCount: 0}
	testEnvironment.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		profile := initialNetworkProfiles(20260813)["clean-lan"]
		environment := newRouteEnvironmentWithNetworkPeers(ctx, t, profile, false)
		defer environment.close()
		path := newFullTunPath(ctx, t, environment, fullTunRouteExchangeH1)
		defer path.close()

		receiptHeld := make(chan struct{})
		receiptPublished := make(chan struct{})
		releaseReceipt := make(chan struct{})
		mainAfterIdle := make(chan struct{})
		releaseMain := make(chan struct{})
		var releaseReceiptOnce sync.Once
		releaseHeldReceipt := func() {
			releaseReceiptOnce.Do(func() {
				close(releaseReceipt)
			})
		}
		defer releaseHeldReceipt()
		var releaseMainOnce sync.Once
		releaseIdleMain := func() {
			releaseMainOnce.Do(func() {
				close(releaseMain)
			})
		}
		defer releaseIdleMain()
		var receiptHeldOnce atomic.Bool
		var receiptPublishedOnce atomic.Bool
		path.beforeUdpTerminalReceiptForTest = func(ctx context.Context, upload bool) {
			if upload && receiptHeldOnce.CompareAndSwap(false, true) {
				close(receiptHeld)
				select {
				case <-releaseReceipt:
				case <-ctx.Done():
				}
			}
		}
		path.afterUdpTerminalReceiptForTest = func(upload bool) {
			if upload && receiptPublishedOnce.CompareAndSwap(false, true) {
				close(receiptPublished)
			}
		}
		path.afterUdpTerminalMarkerForTest = func(
			ctx context.Context,
			upload bool,
			attempt int,
		) error {
			if !upload || attempt != 1 {
				return fmt.Errorf("unexpected marker upload=%t attempt=%d", upload, attempt)
			}
			close(mainAfterIdle)
			select {
			case <-releaseMain:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		workloadDone := make(chan fullTunUDPTestCompletion, 1)
		go func() {
			result, err := measureFullTunUDPDirection(
				ctx,
				path,
				true,
				20*time.Millisecond,
				5_000_000,
				1000,
			)
			workloadDone <- fullTunUDPTestCompletion{result: result, err: err}
		}()
		select {
		case <-receiptHeld:
		case completion := <-workloadDone:
			t.Fatalf("UDP workload returned before held downstream receipt: %+v", completion)
		case <-ctx.Done():
			t.Fatalf("UDP downstream marker receipt was not observed: %v", ctx.Err())
		}
		select {
		case <-mainAfterIdle:
		case completion := <-workloadDone:
			t.Fatalf("UDP workload returned before main reached post-idle barrier: %+v", completion)
		case <-ctx.Done():
			t.Fatalf("UDP main did not reach post-idle marker barrier: %v", ctx.Err())
		}
		if !path.waitForCarrierQuiescent(ctx) {
			t.Fatalf("carrier did not become idle while downstream receipt was held: %v", ctx.Err())
		}
		if fullTunHasCarrierMeasurementEnd(path) {
			t.Fatal("carrier end was frozen before positive downstream receipt")
		}
		select {
		case completion := <-workloadDone:
			t.Fatalf("UDP workload returned while downstream receipt was held: %+v", completion)
		default:
		}
		releaseHeldReceipt()
		select {
		case <-receiptPublished:
		case completion := <-workloadDone:
			t.Fatalf("UDP workload returned before receipt publication: %+v", completion)
		case <-ctx.Done():
			t.Fatalf("UDP receipt was not published after release: %v", ctx.Err())
		}
		if fullTunHasCarrierMeasurementEnd(path) {
			t.Fatal("carrier end was frozen while main remained at its post-idle barrier")
		}
		releaseIdleMain()
		select {
		case completion := <-workloadDone:
			if completion.err != nil {
				t.Fatal(completion.err)
			}
			if completion.result.TerminalMarkerAttemptCount != 1 {
				t.Fatalf("UDP marker result=%+v", completion.result)
			}
		case <-ctx.Done():
			t.Fatalf("UDP workload did not join released downstream receipt: %v", ctx.Err())
		}
	})
}

// Reliable exchange carriage closes the ordered marker boundary after
// deterministic outer loss and reordered scheduling have both been exercised.
func TestFullTunUDPTerminalBarriersRecoverExchangeLossAndReordering(t *testing.T) {
	if testing.Short() {
		return
	}
	testEnvironment := &server.TestEnv{ApplyDbMigrations: true, RerunCount: 0}
	testEnvironment.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		profile := initialNetworkProfiles(20260810)["clean-lan"]
		environment := newRouteEnvironmentWithNetworkPeers(ctx, t, profile, false)
		defer environment.close()
		path := newFullTunPath(ctx, t, environment, fullTunRouteExchangeH1)
		defer path.close()
		before := environment.network.snapshotLinks()
		_, err := environment.network.updateProfiles(
			ctx,
			"UDP loss and reordering regression",
			time.Now(),
			func(link linkProfile) linkProfile {
				link.LossModel = lossModelEveryN
				link.DropEveryPacketCount = 5
				link.ReorderProbability = 1
				return link
			},
		)
		if err != nil {
			t.Fatal(err)
		}
		result, err := measureFullTunUDP(ctx, path, 50*time.Millisecond, 5_000_000, 1000)
		if err != nil {
			t.Fatal(err)
		}
		if result.OfferedPacketCount == 0 || result.DeliveredPacketCount != result.OfferedPacketCount {
			t.Fatalf(
				"loss-and-reordering full-TUN UDP delivery=%d/%d",
				result.DeliveredPacketCount,
				result.OfferedPacketCount,
			)
		}
		if result.CorruptPacketCount != 0 {
			t.Fatalf("loss-and-reordering full-TUN UDP result=%+v", result)
		}
		links := subtractLinkSnapshots(before, environment.network.snapshotLinks(), result.Duration)
		var lossDropPacketCount uint64
		var reorderedPacketCount uint64
		for _, link := range links {
			lossDropPacketCount += link.LossDropPacketCount
			reorderedPacketCount += link.ReorderedPacketCount
		}
		if lossDropPacketCount == 0 || reorderedPacketCount == 0 {
			t.Fatalf("outer impairment was not exercised: links=%+v", links)
		}
	})
}

// Inner QUIC retains its own congestion control above the selected outer
// Connect carrier and verifies the exact stream at host egress.
const fullTunQUICInitialPacketSize = 1200

func measureFullTunQUIC(
	ctx context.Context,
	path *fullTunPath,
	byteCount int64,
) (workloadResult, error) {
	result, err := measureFullTunQUICWithStartHook(ctx, path, byteCount, nil)
	if err == nil {
		err = path.waitForPostWorkloadBoundary(ctx)
	}
	return result, err
}

// The optional hook runs after the first stream write has entered QUIC.
func measureFullTunQUICWithStartHook(
	ctx context.Context,
	path *fullTunPath,
	byteCount int64,
	startHook func() error,
) (workloadResult, error) {
	serverPacketConn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0})
	if err != nil {
		return workloadResult{}, err
	}
	clientPacketConn, err := path.appTun.ListenUDP(&net.UDPAddr{
		IP:   net.IP(path.appTun.LocalAddresses()[0].AsSlice()),
		Port: 0,
	})
	if err != nil {
		serverPacketConn.Close()
		return workloadResult{}, err
	}
	serverTlsConfig, clientTlsConfig, err := newWorkloadTlsConfigs()
	if err != nil {
		serverPacketConn.Close()
		clientPacketConn.Close()
		return workloadResult{}, err
	}
	roundTrip := fullTunOuterRoundTrip(path)
	quicConfig := &quic.Config{
		HandshakeIdleTimeout: max(15*time.Second, 12*roundTrip),
		MaxIdleTimeout:       max(30*time.Second, 20*roundTrip),
		// QUIC requires a client Initial UDP datagram of at least 1,200
		// bytes. The product VPN MTU is intentionally smaller, so the IPv4
		// application stack must fragment this datagram into legal tunnel
		// packets and the provider side must reassemble it before UDP egress.
		InitialPacketSize: fullTunQUICInitialPacketSize,
	}
	serverTransport := &quic.Transport{Conn: serverPacketConn}
	listener, err := serverTransport.ListenEarly(serverTlsConfig, quicConfig)
	if err != nil {
		serverTransport.Close()
		clientPacketConn.Close()
		return workloadResult{}, err
	}
	defer listener.Close()
	defer serverTransport.Close()
	expectedHash := deterministicPayloadHash(byteCount)
	workloadDeadline := boundedWorkloadDeadline(ctx, fullTunWorkloadTimeout(path, byteCount))
	serverResult := make(chan error, 1)
	serverDone := make(chan struct{})
	go func() {
		defer close(serverDone)
		connection, acceptErr := listener.Accept(ctx)
		if acceptErr != nil {
			serverResult <- acceptErr
			return
		}
		defer func() {
			_ = connection.CloseWithError(0, "")
		}()
		stopConnectionInterrupt := context.AfterFunc(ctx, func() {
			_ = connection.CloseWithError(0, "workload context canceled")
		})
		defer stopConnectionInterrupt()
		stream, streamErr := connection.AcceptStream(ctx)
		if streamErr != nil {
			serverResult <- streamErr
			return
		}
		stopStreamInterrupt := interruptDeadlineOnContext(ctx, stream)
		defer stopStreamInterrupt()
		if deadlineErr := stream.SetDeadline(workloadDeadline); deadlineErr != nil {
			serverResult <- deadlineErr
			return
		}
		hash := sha256.New()
		readByteCount, readErr := io.CopyN(hash, stream, byteCount)
		if readErr != nil {
			serverResult <- readErr
			return
		}
		if readByteCount != byteCount || hex.EncodeToString(hash.Sum(nil)) != expectedHash {
			serverResult <- fmt.Errorf("full-TUN QUIC content mismatch bytes=%d", readByteCount)
			return
		}
		serverResult <- nil
	}()

	clientTransport := &quic.Transport{Conn: clientPacketConn}
	defer clientTransport.Close()
	setupStart := time.Now()
	connection, err := clientTransport.DialEarly(ctx, serverPacketConn.LocalAddr(), clientTlsConfig, quicConfig)
	if err != nil {
		return workloadResult{}, err
	}
	defer connection.CloseWithError(0, "")
	stopConnectionInterrupt := context.AfterFunc(ctx, func() {
		_ = connection.CloseWithError(0, "workload context canceled")
	})
	defer stopConnectionInterrupt()
	stream, err := connection.OpenStreamSync(ctx)
	if err != nil {
		return workloadResult{}, err
	}
	stopStreamInterrupt := interruptDeadlineOnContext(ctx, stream)
	defer stopStreamInterrupt()
	if err := stream.SetDeadline(workloadDeadline); err != nil {
		return workloadResult{}, err
	}
	setupDuration := time.Since(setupStart)
	payload := deterministicPayload()
	startTime := time.Now()
	hookCalled := false
	for remaining := byteCount; 0 < remaining; {
		chunk := payload
		if remaining < int64(len(chunk)) {
			chunk = payload[:remaining]
		}
		writtenByteCount, writeErr := stream.Write(chunk)
		remaining -= int64(writtenByteCount)
		if writeErr != nil {
			return workloadResult{}, contextBoundWorkloadError(ctx, writeErr)
		}
		if !hookCalled && startHook != nil {
			hookCalled = true
			if err := startHook(); err != nil {
				return workloadResult{}, err
			}
		}
	}
	if err := stream.Close(); err != nil {
		return workloadResult{}, contextBoundWorkloadError(ctx, err)
	}
	select {
	case err := <-serverResult:
		if err != nil {
			return workloadResult{}, contextBoundWorkloadError(ctx, err)
		}
	case <-ctx.Done():
		return workloadResult{}, ctx.Err()
	}
	select {
	case <-serverDone:
	case <-ctx.Done():
		return workloadResult{}, ctx.Err()
	}
	duration := time.Since(startTime)
	return finishWorkloadResult(workloadResult{
		UsefulByteCount: byteCount,
		Duration:        duration,
		SetupDuration:   setupDuration,
		ContentHash:     expectedHash,
	}), nil
}

// fullTunWebLifecycleStage names an exact HTTP server/client completion edge.
type fullTunWebLifecycleStage string

const (
	// fullTunWebHandlerReturned occurs before net/http writes the terminal chunk.
	fullTunWebHandlerReturned fullTunWebLifecycleStage = "handler-returned"
	// fullTunWebResponseBodyEof occurs only when Body.Read returns io.EOF.
	fullTunWebResponseBodyEof fullTunWebLifecycleStage = "response-body-eof"
)

// fullTunWebLifecycleEvent identifies one request's ordered completion edge.
type fullTunWebLifecycleEvent struct {
	requestIndex int
	stage        fullTunWebLifecycleStage
}

// fullTunWebOptions enables exact chunk/lifecycle assertions without changing
// the default workload.
type fullTunWebOptions struct {
	forceChunked bool
	lifecycle    func(fullTunWebLifecycleEvent)
}

// Short HTTP exchanges measure connection setup and response completion through
// the same full TUN and provider route used by bulk traffic.
func measureFullTunWeb(
	ctx context.Context,
	path *fullTunPath,
) (workloadResult, error) {
	return measureFullTunWebWithOptions(ctx, path, fullTunWebOptions{})
}

// Optional lifecycle instrumentation makes handler return and the client's
// exact body EOF observable. The default options add no production behavior.
func measureFullTunWebWithOptions(
	ctx context.Context,
	path *fullTunPath,
	options fullTunWebOptions,
) (workloadResult, error) {
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		return workloadResult{}, err
	}
	defer listener.Close()
	smallBody := bytes.Repeat([]byte("s"), 16*1024)
	mediumBodyByteCount := 512 * 1024
	if options.forceChunked {
		mediumBodyByteCount = 2 * 1024 * 1024
	}
	mediumBody := bytes.Repeat([]byte("m"), mediumBodyByteCount)
	handler := http.NewServeMux()
	var handlerRequestCount atomic.Int64
	handleBody := func(body []byte) http.HandlerFunc {
		return func(writer http.ResponseWriter, request *http.Request) {
			requestIndex := int(handlerRequestCount.Add(1) - 1)
			defer func() {
				if options.lifecycle != nil {
					options.lifecycle(fullTunWebLifecycleEvent{
						requestIndex: requestIndex,
						stage:        fullTunWebHandlerReturned,
					})
				}
			}()
			if !options.forceChunked {
				_, _ = writer.Write(body)
				return
			}
			writer.Header().Set("Trailer", "X-Full-Tun-Complete")
			flusher, ok := writer.(http.Flusher)
			if !ok {
				http.Error(writer, "streaming unavailable", http.StatusInternalServerError)
				return
			}
			chunkByteCount := max(1, len(body)/4)
			for start := 0; start < len(body); start += chunkByteCount {
				end := min(len(body), start+chunkByteCount)
				if _, writeErr := writer.Write(body[start:end]); writeErr != nil {
					return
				}
				flusher.Flush()
			}
			writer.Header().Set("X-Full-Tun-Complete", "true")
		}
	}
	handler.HandleFunc("/small", handleBody(smallBody))
	handler.HandleFunc("/medium", handleBody(mediumBody))
	httpServer := &http.Server{Handler: handler}
	defer httpServer.Close()
	serverDone := make(chan struct{})
	go func() {
		defer close(serverDone)
		_ = httpServer.Serve(listener)
	}()
	transport := &http.Transport{
		DialContext: path.appTun.DialContext,
	}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport}
	requestBodies := [][]byte{smallBody, mediumBody, smallBody}
	requestPaths := []string{"/small", "/medium", "/small"}
	latencies := []time.Duration{}
	var firstByteTotal time.Duration
	var usefulByteCount int64
	contentHash := sha256.New()
	startTime := time.Now()
	for requestIndex, requestPath := range requestPaths {
		requestStart := time.Now()
		firstByteTime := time.Time{}
		request, err := http.NewRequestWithContext(
			ctx,
			http.MethodGet,
			fmt.Sprintf("http://%s%s", listener.Addr(), requestPath),
			nil,
		)
		if err != nil {
			return workloadResult{}, err
		}
		request = request.WithContext(httptrace.WithClientTrace(request.Context(), &httptrace.ClientTrace{
			GotFirstResponseByte: func() {
				firstByteTime = time.Now()
			},
		}))
		response, err := client.Do(request)
		if err != nil {
			return workloadResult{}, err
		}
		if options.forceChunked && !hasTransferEncoding(response.TransferEncoding, "chunked") {
			response.Body.Close()
			return workloadResult{}, fmt.Errorf(
				"full-TUN web response %d transfer encoding=%v, want chunked",
				requestIndex,
				response.TransferEncoding,
			)
		}
		if options.forceChunked && response.ContentLength != -1 {
			response.Body.Close()
			return workloadResult{}, fmt.Errorf(
				"full-TUN web response %d content length=%d, want no Content-Length",
				requestIndex,
				response.ContentLength,
			)
		}
		var body bytes.Buffer
		readBuffer := make([]byte, 32*1024)
		var readErr error
		for {
			readByteCount, bodyErr := response.Body.Read(readBuffer)
			if 0 < readByteCount {
				_, _ = body.Write(readBuffer[:readByteCount])
			}
			if bodyErr == io.EOF {
				if options.lifecycle != nil {
					options.lifecycle(fullTunWebLifecycleEvent{
						requestIndex: requestIndex,
						stage:        fullTunWebResponseBodyEof,
					})
				}
				break
			}
			if bodyErr != nil {
				readErr = bodyErr
				break
			}
		}
		closeErr := response.Body.Close()
		transport.CloseIdleConnections()
		if readErr != nil {
			return workloadResult{}, readErr
		}
		if closeErr != nil {
			return workloadResult{}, closeErr
		}
		if options.forceChunked && response.Trailer.Get("X-Full-Tun-Complete") != "true" {
			return workloadResult{}, fmt.Errorf(
				"full-TUN web response %d completion trailer=%q, want true",
				requestIndex,
				response.Trailer.Get("X-Full-Tun-Complete"),
			)
		}
		if response.StatusCode != http.StatusOK || !bytes.Equal(body.Bytes(), requestBodies[requestIndex]) {
			return workloadResult{}, fmt.Errorf("full-TUN web response %d failed integrity", requestIndex)
		}
		_, _ = contentHash.Write(body.Bytes())
		latencies = append(latencies, time.Since(requestStart))
		if !firstByteTime.IsZero() {
			firstByteTotal += firstByteTime.Sub(requestStart)
		}
		usefulByteCount += int64(body.Len())
	}
	duration := time.Since(startTime)
	transport.CloseIdleConnections()
	if err := httpServer.Close(); err != nil {
		return workloadResult{}, err
	}
	select {
	case <-serverDone:
	case <-ctx.Done():
		return workloadResult{}, ctx.Err()
	}
	result := finishWorkloadResult(workloadResult{
		UsefulByteCount: usefulByteCount,
		Duration:        duration,
		TimeToFirstByte: firstByteTotal / time.Duration(len(requestPaths)),
		Latency:         summarizeLatencies(latencies),
		ContentHash:     hex.EncodeToString(contentHash.Sum(nil)),
	})
	if err := path.waitForPostWorkloadBoundary(ctx); err != nil {
		return result, err
	}
	return result, nil
}

// Reports whether net/http decoded the requested transfer coding.
func hasTransferEncoding(encodings []string, want string) bool {
	for _, encoding := range encodings {
		if encoding == want {
			return true
		}
	}
	return false
}

// A host UDP echo probe observes interactive RTT before, during, and after a
// full-TUN bulk transfer over the same selected route. The compatibility entry
// point retains the original upload direction.
func measureFullTunLatencyUnderLoad(
	ctx context.Context,
	path *fullTunPath,
	bulkByteCount int64,
) (workloadResult, error) {
	return measureFullTunLatencyUnderLoadDirection(ctx, path, bulkByteCount, true)
}

func measureFullTunLatencyUnderLoadDirection(
	ctx context.Context,
	path *fullTunPath,
	bulkByteCount int64,
	upload bool,
) (workloadResult, error) {
	result, err := measureFullTunLatencyUnderLoadDirectionWithStartHook(
		ctx,
		path,
		bulkByteCount,
		upload,
		nil,
	)
	if err == nil {
		err = path.waitForPostWorkloadBoundary(ctx)
	}
	return result, err
}

// The optional hook gives cancellation tests an exact post-handshake bulk boundary.
func measureFullTunLatencyUnderLoadWithStartHook(
	ctx context.Context,
	path *fullTunPath,
	bulkByteCount int64,
	startHook func() error,
) (workloadResult, error) {
	return measureFullTunLatencyUnderLoadDirectionWithStartHook(
		ctx,
		path,
		bulkByteCount,
		true,
		startHook,
	)
}

func measureFullTunLatencyUnderLoadDirectionWithStartHook(
	ctx context.Context,
	path *fullTunPath,
	bulkByteCount int64,
	upload bool,
	startHook func() error,
) (workloadResult, error) {
	probeListener, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0})
	if err != nil {
		return workloadResult{}, err
	}
	stopProbeListenerInterrupt := context.AfterFunc(ctx, func() {
		_ = probeListener.Close()
	})
	defer stopProbeListenerInterrupt()
	probeServerDone := make(chan struct{})
	go func() {
		defer close(probeServerDone)
		defer func() {
			if path.beforeLatencyProbeDoneForTest != nil {
				path.beforeLatencyProbeDoneForTest()
			}
		}()
		packetBytes := make([]byte, 2048)
		for {
			readByteCount, sourceAddress, readErr := probeListener.ReadFrom(packetBytes)
			if readErr != nil {
				return
			}
			_, _ = probeListener.WriteTo(packetBytes[:readByteCount], sourceAddress)
		}
	}()
	var probeServerJoinOnce sync.Once
	joinProbeServer := func() {
		probeServerJoinOnce.Do(func() {
			_ = probeListener.Close()
			if path.beforeLatencyProbeWaitForTest != nil {
				path.beforeLatencyProbeWaitForTest()
			}
			<-probeServerDone
		})
	}
	defer joinProbeServer()
	probeConnection, err := path.appTun.DialContext(ctx, "udp", probeListener.LocalAddr().String())
	if err != nil {
		return workloadResult{}, err
	}
	defer probeConnection.Close()
	probeTimeout := max(3*time.Second, 8*fullTunOuterRoundTrip(path))
	probeMany := func(startSequence uint64, count int) latencyProbeSamples {
		samples := latencyProbeSamples{latencies: make([]time.Duration, 0, count)}
		for probeIndex := range count {
			if ctx.Err() != nil {
				break
			}
			latency, probeErr := runLatencyProbe(
				ctx,
				probeConnection,
				startSequence+uint64(probeIndex),
				probeTimeout,
			)
			samples.add(latency, probeErr)
			if err := waitForWorkloadDelay(ctx, 5*time.Millisecond); err != nil {
				break
			}
		}
		return samples
	}
	idleSamples := probeMany(latencyProbeIdleStartSequence, 8)
	if err := idleSamples.validate("idle"); err != nil {
		result := workloadResult{}
		applyLatencyProbeSamples(&result, idleSamples, latencyProbeSamples{}, latencyProbeSamples{})
		return result, err
	}
	bulkCtx, bulkCancel := context.WithCancel(ctx)
	bulkDone := make(chan workloadResult, 1)
	bulkErrors := make(chan error, 1)
	bulkFinished := make(chan struct{})
	bulkStarted := make(chan struct{})
	bulkStartHook := func() error {
		if startHook != nil {
			if err := startHook(); err != nil {
				return err
			}
		}
		close(bulkStarted)
		return nil
	}
	go func() {
		defer close(bulkFinished)
		var result workloadResult
		var measureErr error
		if upload {
			result, measureErr = measureFullTunUploadWithStartHook(
				bulkCtx,
				path,
				bulkByteCount,
				bulkByteCount,
				bulkStartHook,
			)
		} else {
			result, measureErr = measureFullTunDownloadWithWarmupAndStartHook(
				bulkCtx,
				path,
				0,
				bulkByteCount,
				bulkByteCount,
				bulkStartHook,
			)
		}
		if measureErr != nil {
			bulkErrors <- measureErr
			return
		}
		bulkDone <- result
	}()
	var bulkJoinOnce sync.Once
	joinBulkUpload := func() {
		bulkJoinOnce.Do(func() {
			bulkCancel()
			if path.beforeLatencyBulkWaitForTest != nil {
				path.beforeLatencyBulkWaitForTest()
			}
			<-bulkFinished
		})
	}
	defer joinBulkUpload()
	// The bulk helper takes its exact carrier snapshot before invoking the
	// start hook. Loaded probes must not enter the same carrier while that
	// snapshot is waiting for quiescence, or the probe train can make the
	// quiescent boundary impossible to reach.
	select {
	case <-bulkStarted:
	case err := <-bulkErrors:
		result := workloadResult{}
		applyLatencyProbeSamples(
			&result,
			idleSamples,
			latencyProbeSamples{},
			latencyProbeSamples{},
		)
		return result, err
	case <-ctx.Done():
		return workloadResult{}, ctx.Err()
	}
	if path.beforeLatencyLoadedProbeForTest != nil {
		if err := path.beforeLatencyLoadedProbeForTest(); err != nil {
			return workloadResult{}, err
		}
	}
	loadedSamples := runLoadedLatencyProbes(
		ctx,
		probeConnection,
		latencyProbeLoadedStartSequence,
		probeTimeout,
		loadedLatencyProbeIntervalForRate(
			bulkByteCount,
			fullTunEffectiveRateBitsPerSecond(path, upload),
		),
		bulkFinished,
		nil,
	)
	var bulkResult workloadResult
	select {
	case err := <-bulkErrors:
		applyLatencyProbeSamples(
			&bulkResult,
			idleSamples,
			loadedSamples,
			latencyProbeSamples{},
		)
		return bulkResult, err
	case bulkResult = <-bulkDone:
	case <-ctx.Done():
		return workloadResult{}, ctx.Err()
	}
	joinBulkUpload()
	postLoadSamples := probeMany(latencyProbePostLoadStartSequence, 8)
	joinProbeServer()
	applyLatencyProbeSamples(&bulkResult, idleSamples, loadedSamples, postLoadSamples)
	if err := validateLatencyProbeSamples(idleSamples, loadedSamples, postLoadSamples); err != nil {
		return bulkResult, err
	}
	return bulkResult, nil
}

// A full-route workload is held at an exact post-blackhole carrier submission,
// then explicit cancellation must stop it; the timeout is only a liveness cap.
func testFullTunWorkloadCancelAfterBlackhole(
	t *testing.T,
	seed int64,
	name string,
	measure func(context.Context, *fullTunPath, func() error) error,
) {
	if testing.Short() {
		return
	}
	testEnvironment := &server.TestEnv{ApplyDbMigrations: true, RerunCount: 0}
	testEnvironment.Run(t, func(t testing.TB) {
		fixtureCtx, fixtureCancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer fixtureCancel()
		profile := initialNetworkProfiles(seed)["clean-lan"]
		environment := newRouteEnvironmentWithNetworkPeers(fixtureCtx, t, profile, false)
		defer environment.close()
		path := newFullTunPath(fixtureCtx, t, environment, fullTunRouteExchangeH1)
		defer path.close()
		livenessCtx, livenessCancel := context.WithTimeout(fixtureCtx, 2*time.Minute)
		defer livenessCancel()
		workloadCtx, workloadCancel := context.WithCancel(livenessCtx)
		defer workloadCancel()
		barrierPublished := make(chan (<-chan struct{}), 1)
		completion := make(chan error, 1)
		go func() {
			completion <- measure(workloadCtx, path, func() error {
				if err := environment.network.setBlackhole(workloadCtx, true); err != nil {
					return err
				}
				barrierPublished <- holdLinkScheduleForTest(
					environment.network.directionalLinks(),
					func(observation linkScheduleObservation) bool {
						return observation.terminalDropCause == linkTerminalDropOutage &&
							1000 <= observation.packetByteCount
					},
					workloadCtx.Done(),
				)
				return nil
			})
		}()
		var carrierHeld <-chan struct{}
		select {
		case carrierHeld = <-barrierPublished:
		case err := <-completion:
			t.Fatalf("%s returned before publishing its carrier barrier: %v", name, err)
		case <-livenessCtx.Done():
			t.Fatalf("publish %s post-blackhole carrier barrier: %v", name, livenessCtx.Err())
		}
		select {
		case <-carrierHeld:
		case err := <-completion:
			t.Fatalf("%s returned before a post-blackhole carrier submission: %v", name, err)
		case <-livenessCtx.Done():
			t.Fatalf("wait for %s post-blackhole carrier submission: %v", name, livenessCtx.Err())
		}
		workloadCancel()
		var workloadErr error
		select {
		case workloadErr = <-completion:
		case <-livenessCtx.Done():
			t.Fatalf("%s did not stop after explicit cancellation: %v", name, livenessCtx.Err())
		}
		restoreErr := environment.network.setBlackhole(fixtureCtx, false)
		if restoreErr != nil {
			t.Fatalf("restore network after %s: %v", name, restoreErr)
		}
		if !errors.Is(workloadErr, context.Canceled) {
			t.Fatalf("%s blackhole error=%v, want explicit context cancellation", name, workloadErr)
		}
	})
}

// Full-route inner QUIC cannot retain its longer transport deadline after the run cap.
func TestFullTunQUICCancelAfterBlackhole(t *testing.T) {
	testFullTunWorkloadCancelAfterBlackhole(
		t,
		4011,
		"QUIC",
		func(ctx context.Context, path *fullTunPath, startHook func() error) error {
			_, err := measureFullTunQUICWithStartHook(ctx, path, 20*1024*1024, startHook)
			return err
		},
	)
}

// Full-route bulk TCP and concurrent probes cannot outlive the common run cap.
func TestFullTunLatencyUnderLoadCancelAfterBlackhole(t *testing.T) {
	testFullTunWorkloadCancelAfterBlackhole(
		t,
		4012,
		"latency-under-load",
		func(ctx context.Context, path *fullTunPath, startHook func() error) error {
			_, err := measureFullTunLatencyUnderLoadWithStartHook(
				ctx,
				path,
				32*1024*1024,
				startHook,
			)
			return err
		},
	)
}

// Provider-origin bulk traffic must keep interactive UDP probes live and exact
// through the production route just as device-origin upload traffic does.
func TestFullTunLatencyUnderLoadDownloadCorrectness(t *testing.T) {
	if testing.Short() {
		return
	}
	testEnvironment := &server.TestEnv{ApplyDbMigrations: true, RerunCount: 0}
	testEnvironment.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		profile := initialNetworkProfiles(4016)["clean-lan"]
		environment := newRouteEnvironmentWithNetworkPeers(ctx, t, profile, false)
		defer environment.close()
		path := newFullTunPath(ctx, t, environment, fullTunRouteExchangeH1)
		defer path.close()

		const bulkByteCount = int64(2 * 1024 * 1024)
		result, err := measureFullTunLatencyUnderLoadDirection(
			ctx,
			path,
			bulkByteCount,
			false,
		)
		if err != nil {
			t.Fatal(err)
		}
		if result.UsefulByteCount != bulkByteCount ||
			result.ContentHash != deterministicPayloadHash(bulkByteCount) ||
			result.IdleLatency.P50 <= 0 || result.LoadedLatency.P50 <= 0 ||
			result.PostLoadLatency.P50 <= 0 ||
			result.LoadedProbeSuccessCount < minimumLatencyProbeSuccessCount {
			t.Fatalf("download latency-under-load result=%+v", result)
		}
	})
}

// An error from the nested upload closes the probe listener but cannot return
// while the UDP server retains its final goroutine lifecycle credit.
func TestFullTunLatencyUnderLoadEarlyErrorJoinsProbeServer(t *testing.T) {
	if testing.Short() {
		return
	}
	testEnvironment := &server.TestEnv{ApplyDbMigrations: true, RerunCount: 0}
	testEnvironment.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		profile := initialNetworkProfiles(4014)["clean-lan"]
		environment := newRouteEnvironmentWithNetworkPeers(ctx, t, profile, false)
		defer environment.close()
		path := newFullTunPath(ctx, t, environment, fullTunRouteExchangeH1)
		defer path.close()

		expectedErr := errors.New("stop after full-TUN latency bulk readiness")
		probeServerHeld := make(chan struct{})
		probeServerWaitReached := make(chan struct{})
		releaseProbeServer := make(chan struct{})
		var releaseOnce sync.Once
		release := func() {
			releaseOnce.Do(func() {
				close(releaseProbeServer)
			})
		}
		defer release()
		path.beforeLatencyProbeDoneForTest = func() {
			close(probeServerHeld)
			<-releaseProbeServer
		}
		path.beforeLatencyProbeWaitForTest = func() {
			close(probeServerWaitReached)
		}
		completion := make(chan error, 1)
		go func() {
			_, err := measureFullTunLatencyUnderLoadWithStartHook(
				ctx,
				path,
				64*1024,
				func() error {
					return expectedErr
				},
			)
			completion <- err
		}()
		waitBarrier := func(name string, barrier <-chan struct{}) {
			select {
			case <-barrier:
			case err := <-completion:
				t.Fatalf("latency helper returned before %s: %v", name, err)
			case <-ctx.Done():
				t.Fatalf("wait for %s: %v", name, ctx.Err())
			}
		}
		waitBarrier("held probe server", probeServerHeld)
		waitBarrier("probe-server join", probeServerWaitReached)
		select {
		case err := <-completion:
			t.Fatalf("latency helper returned while probe server was held: %v", err)
		default:
		}
		release()
		select {
		case err := <-completion:
			if !errors.Is(err, expectedErr) {
				t.Fatalf("latency helper error=%v, want=%v", err, expectedErr)
			}
		case <-ctx.Done():
			t.Fatalf("latency helper did not return after probe-server release: %v", ctx.Err())
		}
	})
}

// Loaded probes cannot run before the bulk transfer crosses its exact carrier
// snapshot boundary. A failure at that point cancels and joins the nested bulk
// transfer before returning.
func TestFullTunLatencyUnderLoadEarlyErrorCancelsAndJoinsBulkUpload(t *testing.T) {
	if testing.Short() {
		return
	}
	testEnvironment := &server.TestEnv{ApplyDbMigrations: true, RerunCount: 0}
	testEnvironment.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		profile := initialNetworkProfiles(4015)["clean-lan"]
		environment := newRouteEnvironmentWithNetworkPeers(ctx, t, profile, false)
		defer environment.close()
		path := newFullTunPath(ctx, t, environment, fullTunRouteExchangeH1)
		defer path.close()

		expectedErr := errors.New("stop before loaded full-TUN latency probe")
		bulkUploadStarted := make(chan struct{})
		bulkUploadWaitReached := make(chan struct{})
		releaseBulkUpload := make(chan struct{})
		var releaseOnce sync.Once
		release := func() {
			releaseOnce.Do(func() {
				close(releaseBulkUpload)
			})
		}
		defer release()
		path.beforeLatencyLoadedProbeForTest = func() error {
			select {
			case <-bulkUploadStarted:
				return expectedErr
			default:
				return errors.New("loaded probes entered before the bulk carrier snapshot")
			}
		}
		path.beforeLatencyBulkWaitForTest = func() {
			close(bulkUploadWaitReached)
			select {
			case <-releaseBulkUpload:
			case <-ctx.Done():
			}
		}
		completion := make(chan error, 1)
		go func() {
			_, err := measureFullTunLatencyUnderLoadWithStartHook(
				ctx,
				path,
				64*1024,
				func() error {
					close(bulkUploadStarted)
					return nil
				},
			)
			completion <- err
		}()
		select {
		case <-bulkUploadStarted:
		case err := <-completion:
			t.Fatalf("latency helper returned before bulk upload started: %v", err)
		case <-ctx.Done():
			t.Fatalf("wait for held bulk upload: %v", ctx.Err())
		}
		select {
		case <-bulkUploadWaitReached:
		case err := <-completion:
			t.Fatalf("latency helper returned before bulk-upload join: %v", err)
		case <-ctx.Done():
			t.Fatalf("wait for bulk-upload join: %v", ctx.Err())
		}
		select {
		case err := <-completion:
			t.Fatalf("latency helper returned before the canceled bulk upload joined: %v", err)
		default:
		}
		release()
		select {
		case err := <-completion:
			if !errors.Is(err, expectedErr) {
				t.Fatalf("latency helper error=%v, want=%v", err, expectedErr)
			}
		case <-ctx.Done():
			t.Fatalf("latency helper did not return after bulk-upload release: %v", ctx.Err())
		}
	})
}

// Route workload memory counters use a single process and are comparative,
// so this helper records their current cumulative boundary when needed.
func readFullTunMemory() runtime.MemStats {
	var memory runtime.MemStats
	runtime.ReadMemStats(&memory)
	return memory
}

// Full application protocols remain correct above an exchange carrier before
// their longer opt-in performance variants are enabled.
func TestFullTunApplicationWorkloadsCorrectness(t *testing.T) {
	if testing.Short() {
		return
	}
	testEnvironment := &server.TestEnv{ApplyDbMigrations: true, RerunCount: 0}
	testEnvironment.Run(t, func(t testing.TB) {
		contextTimeout := 5 * time.Minute
		loadedByteCount := int64(2 * 1024 * 1024)
		if perfvarRaceEnabled {
			// The race runtime turns two back-to-back 2 MiB loaded phases into a
			// five-minute throughput test. This gate verifies protocol, lifecycle,
			// probe, and directional correctness; the non-race canonical gate keeps
			// the full payload used for performance-sensitive coverage.
			contextTimeout = 8 * time.Minute
			loadedByteCount = 256 * 1024
		}
		ctx, cancel := context.WithTimeout(context.Background(), contextTimeout)
		defer cancel()
		profile := initialNetworkProfiles(4010)["clean-lan"]
		environment := newRouteEnvironmentWithNetworkPeers(ctx, t, profile, false)
		defer environment.close()
		path := newFullTunPath(ctx, t, environment, fullTunRouteExchangeH1)
		defer path.close()

		parallelResult, err := measureFullTunParallelUploads(ctx, path, 2, 64*1024)
		if err != nil || parallelResult.UsefulByteCount != 2*64*1024 {
			t.Fatalf("parallel TCP result=%+v err=%v", parallelResult, err)
		}
		udpResult, err := measureFullTunUDP(ctx, path, 50*time.Millisecond, 2_000_000, 800)
		if err != nil || udpResult.DeliveredPacketCount == 0 || udpResult.CorruptPacketCount != 0 {
			t.Fatalf("UDP result=%+v err=%v", udpResult, err)
		}
		udpDownloadResult, err := measureFullTunUDPDirection(
			ctx,
			path,
			false,
			50*time.Millisecond,
			2_000_000,
			800,
		)
		if err != nil || udpDownloadResult.DeliveredPacketCount == 0 || udpDownloadResult.CorruptPacketCount != 0 {
			t.Fatalf("UDP download result=%+v err=%v", udpDownloadResult, err)
		}
		quicResult, err := measureFullTunQUIC(ctx, path, 64*1024)
		if err != nil || quicResult.UsefulByteCount != 64*1024 {
			t.Fatalf("QUIC result=%+v err=%v", quicResult, err)
		}
		webResult, err := measureFullTunWeb(ctx, path)
		if err != nil || webResult.UsefulByteCount == 0 || webResult.TimeToFirstByte <= 0 {
			t.Fatalf("web result=%+v err=%v", webResult, err)
		}
		loadedResult, err := measureFullTunLatencyUnderLoad(ctx, path, loadedByteCount)
		if err != nil || loadedResult.UsefulByteCount != loadedByteCount ||
			loadedResult.IdleLatency.P50 <= 0 || loadedResult.PostLoadLatency.P50 <= 0 ||
			loadedResult.LoadedProbeSuccessCount < minimumLatencyProbeSuccessCount {
			t.Fatalf("latency-under-load result=%+v err=%v", loadedResult, err)
		}
		loadedDownloadResult, err := measureFullTunLatencyUnderLoadDirection(
			ctx,
			path,
			loadedByteCount,
			false,
		)
		if err != nil || loadedDownloadResult.UsefulByteCount != loadedByteCount ||
			loadedDownloadResult.IdleLatency.P50 <= 0 ||
			loadedDownloadResult.PostLoadLatency.P50 <= 0 ||
			loadedDownloadResult.LoadedProbeSuccessCount < minimumLatencyProbeSuccessCount {
			t.Fatalf("download latency-under-load result=%+v err=%v", loadedDownloadResult, err)
		}
	})
}

// The exchange-H1 workload must receive the terminating chunk after the
// server handler has returned. Exact body bytes plus an observed io.EOF pin
// the failure where a provider return segment disappeared before Transfer and
// left net/http waiting forever for the remainder of a chunk.
func TestFullTunExchangeH1ChunkedWebResponseCompletesAtEof(t *testing.T) {
	if testing.Short() {
		return
	}
	testEnvironment := &server.TestEnv{ApplyDbMigrations: true, RerunCount: 0}
	testEnvironment.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		profile := initialNetworkProfiles(4013)["clean-lan"]
		environment := newRouteEnvironmentWithNetworkPeers(ctx, t, profile, false)
		defer environment.close()
		path := newFullTunPath(ctx, t, environment, fullTunRouteExchangeH1)
		defer path.close()

		lifecycle := make(chan fullTunWebLifecycleEvent, 6)
		result, err := measureFullTunWebWithOptions(ctx, path, fullTunWebOptions{
			forceChunked: true,
			lifecycle: func(event fullTunWebLifecycleEvent) {
				lifecycle <- event
			},
		})
		if err != nil {
			t.Fatalf("exchange-H1 chunked web workload: %v", err)
		}
		wantUsefulByteCount := int64(16*1024 + 2*1024*1024 + 16*1024)
		wantHash := sha256.New()
		_, _ = wantHash.Write(bytes.Repeat([]byte("s"), 16*1024))
		_, _ = wantHash.Write(bytes.Repeat([]byte("m"), 2*1024*1024))
		_, _ = wantHash.Write(bytes.Repeat([]byte("s"), 16*1024))
		wantContentHash := hex.EncodeToString(wantHash.Sum(nil))
		if result.UsefulByteCount != wantUsefulByteCount ||
			result.ContentHash != wantContentHash ||
			result.TimeToFirstByte <= 0 {
			t.Fatalf(
				"exchange-H1 chunked web result=%+v, want bytes=%d hash=%s and positive first-byte time",
				result,
				wantUsefulByteCount,
				wantContentHash,
			)
		}
		for requestIndex := range 3 {
			for _, stage := range []fullTunWebLifecycleStage{
				fullTunWebHandlerReturned,
				fullTunWebResponseBodyEof,
			} {
				select {
				case event := <-lifecycle:
					if event.requestIndex != requestIndex || event.stage != stage {
						t.Fatalf(
							"exchange-H1 web lifecycle=%+v, want request=%d stage=%s",
							event,
							requestIndex,
							stage,
						)
					}
				case <-ctx.Done():
					t.Fatalf(
						"exchange-H1 web lifecycle request=%d stage=%s: %v",
						requestIndex,
						stage,
						ctx.Err(),
					)
				}
			}
		}
		select {
		case event := <-lifecycle:
			t.Fatalf("unexpected exchange-H1 web lifecycle event: %+v", event)
		default:
		}
		if err := path.verifyRoute(); err != nil {
			t.Fatalf("exchange-H1 chunked web route verification: %v", err)
		}
		congestion := path.providerRemoteNat.CongestionDropStats()
		if congestion.ReturnQueuePacketCount != 0 ||
			congestion.ReturnQueueByteCount != 0 ||
			congestion.ReturnSendPacketCount != 0 ||
			congestion.ReturnSendByteCount != 0 {
			t.Fatalf("exchange-H1 chunked web provider return congestion=%+v, want zero", congestion)
		}
	})
}
