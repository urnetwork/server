// This file compares the two post-authentication H3 data carriers while the
// production Transfer protocol remains the only application recovery layer.
package perfvar

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	quic "github.com/quic-go/quic-go"
	clientconnect "github.com/urnetwork/connect/v2026"
	"github.com/urnetwork/connect/v2026/protocol"
)

const (
	// Matches DefaultPlatformTransportSettings().TransportBufferSize. Sender
	// backpressure at this route is part of production H3 behavior.
	h3TransferCarrierRouteCapacity            = 32
	h3TransferCarrierLegacyBatchMessageCount  = 16
	h3TransferCarrierLegacyBatchByteCount     = 64 * 1024
	h3TransferCarrierTestMessageCount         = 48
	h3TransferCarrierTestMessagePayloadBytes  = 896
	h3TransferCarrierTestMaximumRunDuration   = 90 * time.Second
	h3TransferCarrierTestClientCleanupTimeout = 10 * time.Second
)

// The selected mode changes only how complete routed TransferFrame bytes cross
// one established QUIC connection.
type h3TransferCarrierMode string

const (
	h3TransferCarrierLegacyStream h3TransferCarrierMode = "legacy-stream"
	h3TransferCarrierDatagram     h3TransferCarrierMode = "datagram-only"
	h3TransferCarrierHybrid       h3TransferCarrierMode = "hybrid-v2"
)

// Writer observations distinguish Transfer retries from QUIC wire retries.
// One endpoint writer owns the mutable value until carrier shutdown joins it.
type h3TransferCarrierRouteStats struct {
	MessageCount            uint64
	MessageByteCount        uint64
	PackCount               uint64
	AckCount                uint64
	RepeatedPackCount       uint64
	MaximumMessageBytes     int
	PackWriteSpan           time.Duration
	MaximumPackRetryGap     time.Duration
	MaximumPackAttemptCount int
	seenPackSequenceItems   map[string]bool
	packLastWriteTimes      map[string]time.Time
	packAttemptCounts       map[string]int
	packWriteTimes          map[string][]time.Time
	packSequenceNumbers     map[string]uint64
	firstPackWriteTime      time.Time
	lastPackWriteTime       time.Time
}

// Transfer emits this observation immediately before one route admission
// attempt. It therefore counts recovery intent independently from the later
// carrier writer, which may still have accepted frames queued when the useful
// workload completes on an extremely slow uplink.
type h3TransferCarrierWireStats struct {
	messageCount       atomic.Uint64
	packCount          atomic.Uint64
	ackCount           atomic.Uint64
	resendMessageCount atomic.Uint64
	resendPackCount    atomic.Uint64
	resendAckCount     atomic.Uint64
	decodeErrorCount   atomic.Uint64
}

type h3TransferCarrierWireStatsSnapshot struct {
	MessageCount       uint64
	PackCount          uint64
	AckCount           uint64
	ResendMessageCount uint64
	ResendPackCount    uint64
	ResendAckCount     uint64
	DecodeErrorCount   uint64
}

func (self *h3TransferCarrierWireStats) observe(
	observation clientconnect.TransferWireMessageObservation,
) {
	self.messageCount.Add(1)
	transferFrame := &protocol.TransferFrame{}
	if err := clientconnect.ProtoUnmarshal(observation.TransferFrameBytes, transferFrame); err != nil {
		self.decodeErrorCount.Add(1)
		return
	}
	if transferFrame.GetPack() != nil {
		self.packCount.Add(1)
		if observation.Resend {
			self.resendPackCount.Add(1)
		}
	}
	if transferFrame.GetAck() != nil {
		self.ackCount.Add(1)
		if observation.Resend {
			self.resendAckCount.Add(1)
		}
	}
	if observation.Resend {
		self.resendMessageCount.Add(1)
	}
}

func (self *h3TransferCarrierWireStats) snapshot() h3TransferCarrierWireStatsSnapshot {
	return h3TransferCarrierWireStatsSnapshot{
		MessageCount:       self.messageCount.Load(),
		PackCount:          self.packCount.Load(),
		AckCount:           self.ackCount.Load(),
		ResendMessageCount: self.resendMessageCount.Load(),
		ResendPackCount:    self.resendPackCount.Load(),
		ResendAckCount:     self.resendAckCount.Load(),
		DecodeErrorCount:   self.decodeErrorCount.Load(),
	}
}

// Route messages remaining after both carrier workers join were admitted by
// Transfer but never handed to QUIC. Keep these separate from carrier writes;
// silently treating them as transmitted undercounts the low-bandwidth tail.
type h3TransferCarrierDrainStats struct {
	MessageCount     uint64
	PackCount        uint64
	AckCount         uint64
	DecodeErrorCount uint64
}

// One endpoint owns one carrier sender and receiver. Send is allowed to block;
// receive delivery is a zero-wait handoff and drops when the Client route is full.
type h3TransferCarrierEndpoint struct {
	ctx                   context.Context
	cancel                context.CancelFunc
	mode                  h3TransferCarrierMode
	connection            *quic.Conn
	stream                *quic.Stream
	outbound              clientconnect.Route
	inbound               clientconnect.Route
	framer                *clientconnect.Framer
	fragmenter            *clientconnect.H3DatagramFragmenter
	reassembler           *clientconnect.H3DatagramReassembler
	datagramSettings      *clientconnect.H3DatagramSettings
	datagramStats         *clientconnect.H3DatagramStats
	maxDatagramByteCount  int
	routeStats            h3TransferCarrierRouteStats
	discardedOutbound     h3TransferCarrierDrainStats
	receiveRouteDropCount atomic.Uint64
	carrierErrors         chan<- error
	workerWaitGroup       *sync.WaitGroup
}

// A pair owns both QUIC endpoints, packet sockets, routes, and workers so every
// pooled message has one deterministic teardown path.
type h3TransferCarrierPair struct {
	ctx             context.Context
	cancel          context.CancelFunc
	clientEndpoint  *h3TransferCarrierEndpoint
	serverEndpoint  *h3TransferCarrierEndpoint
	clientTransport *quic.Transport
	serverTransport *quic.Transport
	clientPacket    net.PacketConn
	serverPacket    net.PacketConn
	errors          chan error
	workerWaitGroup sync.WaitGroup
	closeOnce       sync.Once
	datagramStats   *clientconnect.H3DatagramStats
}

// The result keeps application completion, Transfer retry, carrier, and exact
// simulated-link evidence together for one mode.
type h3TransferCarrierResult struct {
	Mode                  h3TransferCarrierMode
	UsefulByteCount       int64
	Duration              time.Duration
	TimeToFirstMessage    time.Duration
	ForwardLink           directionalLinkSnapshot
	ReverseLink           directionalLinkSnapshot
	ClientRouteStats      h3TransferCarrierRouteStats
	ServerRouteStats      h3TransferCarrierRouteStats
	ClientRecoveryStats   clientconnect.ClientSendRecoveryStatsSnapshot
	ClientWireStats       h3TransferCarrierWireStatsSnapshot
	ClientDiscardedRoute  h3TransferCarrierDrainStats
	FirstRouteWriteErrors uint64
	ReceiveRouteDropCount uint64
	DatagramStats         clientconnect.H3DatagramStatsSnapshot
}

// A sender-side parse counts complete Transfer writes and repeated sequence
// items without changing the borrowed pooled bytes.
func (self *h3TransferCarrierEndpoint) observeRouteMessage(message []byte) error {
	transferFrame := &protocol.TransferFrame{}
	if err := clientconnect.ProtoUnmarshal(message, transferFrame); err != nil {
		return fmt.Errorf("decode routed Transfer frame: %w", err)
	}
	self.routeStats.MessageCount += 1
	self.routeStats.MessageByteCount += uint64(len(message))
	self.routeStats.MaximumMessageBytes = max(self.routeStats.MaximumMessageBytes, len(message))
	if pack := transferFrame.GetPack(); pack != nil {
		writeTime := time.Now()
		self.routeStats.PackCount += 1
		key := string(pack.GetSequenceId()) + "/" + strconv.FormatUint(pack.GetSequenceNumber(), 10)
		if self.routeStats.seenPackSequenceItems[key] {
			self.routeStats.RepeatedPackCount += 1
		} else {
			self.routeStats.seenPackSequenceItems[key] = true
		}
		if self.routeStats.firstPackWriteTime.IsZero() {
			self.routeStats.firstPackWriteTime = writeTime
		}
		if previousWriteTime, ok := self.routeStats.packLastWriteTimes[key]; ok {
			self.routeStats.MaximumPackRetryGap = max(
				self.routeStats.MaximumPackRetryGap,
				writeTime.Sub(previousWriteTime),
			)
		}
		self.routeStats.packLastWriteTimes[key] = writeTime
		self.routeStats.packAttemptCounts[key] += 1
		self.routeStats.packWriteTimes[key] = append(self.routeStats.packWriteTimes[key], writeTime)
		self.routeStats.packSequenceNumbers[key] = pack.GetSequenceNumber()
		self.routeStats.MaximumPackAttemptCount = max(
			self.routeStats.MaximumPackAttemptCount,
			self.routeStats.packAttemptCounts[key],
		)
		self.routeStats.lastPackWriteTime = writeTime
		self.routeStats.PackWriteSpan = writeTime.Sub(self.routeStats.firstPackWriteTime)
	}
	if transferFrame.GetAck() != nil {
		self.routeStats.AckCount += 1
	}
	return nil
}

// Summarizes the most-retried Pack as sequence:attempt-offsets. This keeps the
// A/B row compact while exposing whether its tail follows RTT or RTO cadence.
func (self h3TransferCarrierRouteStats) retryTimeline() string {
	var selectedKey string
	selectedSequenceNumber := ^uint64(0)
	selectedAttemptCount := 0
	for key, attemptTimes := range self.packWriteTimes {
		sequenceNumber := self.packSequenceNumbers[key]
		if selectedAttemptCount < len(attemptTimes) ||
			(selectedAttemptCount == len(attemptTimes) && sequenceNumber < selectedSequenceNumber) {
			selectedKey = key
			selectedSequenceNumber = sequenceNumber
			selectedAttemptCount = len(attemptTimes)
		}
	}
	if selectedAttemptCount == 0 {
		return "none"
	}
	attemptTimes := self.packWriteTimes[selectedKey]
	offsets := make([]string, 0, len(attemptTimes))
	for _, attemptTime := range attemptTimes {
		offsets = append(offsets, attemptTime.Sub(attemptTimes[0]).Round(time.Millisecond).String())
	}
	return fmt.Sprintf("%d:%s", selectedSequenceNumber, strings.Join(offsets, ","))
}

// A first terminal endpoint error cancels both directions. Reporting is
// nonblocking so a failing receive pump cannot become a second deadlock.
func (self *h3TransferCarrierEndpoint) fail(err error) {
	if err == nil || self.ctx.Err() != nil {
		return
	}
	select {
	case self.carrierErrors <- err:
	default:
	}
	self.cancel()
}

// Transfer ownership moves to the Client only when its receive route accepts
// immediately; a full route drops and returns the pooled message.
func (self *h3TransferCarrierEndpoint) deliver(message []byte) {
	select {
	case <-self.ctx.Done():
		clientconnect.MessagePoolReturn(message)
	case self.inbound <- message:
	default:
		self.receiveRouteDropCount.Add(1)
		clientconnect.MessagePoolReturn(message)
	}
}

// The reliable carrier reproduces the production ready-only 16-message/64-KiB
// batching rule. It never waits to form a batch.
func (self *h3TransferCarrierEndpoint) runLegacyWriter() {
	defer self.workerWaitGroup.Done()
	storage := make([]byte, h3TransferCarrierLegacyBatchByteCount)
	var pendingMessage []byte
	defer func() {
		if pendingMessage != nil {
			clientconnect.MessagePoolReturn(pendingMessage)
		}
	}()
	for {
		message := pendingMessage
		pendingMessage = nil
		if message == nil {
			select {
			case <-self.ctx.Done():
				return
			case message = <-self.outbound:
			}
		}

		var messageStorage [h3TransferCarrierLegacyBatchMessageCount][]byte
		messages := messageStorage[:1]
		messages[0] = message
		batchByteCount := len(message) + 4
	drainReady:
		for len(messages) < cap(messages) {
			select {
			case <-self.ctx.Done():
				break drainReady
			case nextMessage := <-self.outbound:
				framedByteCount := len(nextMessage) + 4
				if h3TransferCarrierLegacyBatchByteCount < batchByteCount+framedByteCount {
					pendingMessage = nextMessage
					break drainReady
				}
				messages = append(messages, nextMessage)
				batchByteCount += framedByteCount
			default:
				break drainReady
			}
		}

		var writeErr error
		for _, routedMessage := range messages {
			if err := self.observeRouteMessage(routedMessage); err != nil && writeErr == nil {
				writeErr = err
			}
		}
		if writeErr == nil {
			writeErr = self.framer.WriteBatchWithStorage(self.stream, messages, storage)
		}
		for _, routedMessage := range messages {
			clientconnect.MessagePoolReturn(routedMessage)
		}
		if writeErr != nil {
			self.fail(fmt.Errorf("write legacy H3 batch: %w", writeErr))
			return
		}
	}
}

// The candidate sends each complete routed Transfer frame through the bounded
// production fragmenter. A path-size rejection retries once under a new id,
// matching the live client and server writers.
func (self *h3TransferCarrierEndpoint) runDatagramWriter() {
	defer self.workerWaitGroup.Done()
	for {
		select {
		case <-self.ctx.Done():
			return
		case message := <-self.outbound:
			if err := self.observeRouteMessage(message); err != nil {
				clientconnect.MessagePoolReturn(message)
				self.fail(err)
				return
			}
			_, sendErr := self.sendDatagramMessage(message, true)
			clientconnect.MessagePoolReturn(message)
			if sendErr != nil {
				self.fail(fmt.Errorf("write H3 DATAGRAM message: %w", sendErr))
				return
			}
		}
	}
}

func (self *h3TransferCarrierEndpoint) sendDatagramMessage(
	message []byte,
	allowFragmentation bool,
) (useStream bool, sendErr error) {
	if !allowFragmentation {
		useStream, nextMaxDatagramByteCount, err := self.fragmenter.SendHybrid(
			message,
			self.maxDatagramByteCount,
			self.connection.SendDatagram,
		)
		self.maxDatagramByteCount = nextMaxDatagramByteCount
		return useStream, err
	}
	_, sendErr = self.fragmenter.Send(
		message,
		self.maxDatagramByteCount,
		self.connection.SendDatagram,
	)
	var tooLargeErr *quic.DatagramTooLargeError
	if errors.As(sendErr, &tooLargeErr) &&
		clientconnect.H3DatagramHeaderByteCount < int(tooLargeErr.MaxDatagramPayloadSize) &&
		int(tooLargeErr.MaxDatagramPayloadSize) < self.maxDatagramByteCount {
		self.maxDatagramByteCount = int(tooLargeErr.MaxDatagramPayloadSize)
		_, sendErr = self.fragmenter.Send(
			message,
			self.maxDatagramByteCount,
			self.connection.SendDatagram,
		)
	}
	return false, sendErr
}

// The production candidate selects each complete routed frame by the v2
// threshold. Ready stream messages batch together, but encountering a small
// DATAGRAM frame stops the batch and leaves it pending for the next iteration.
func (self *h3TransferCarrierEndpoint) runHybridWriter() {
	defer self.workerWaitGroup.Done()
	var storage []byte
	var pendingMessage []byte
	defer func() {
		if pendingMessage != nil {
			clientconnect.MessagePoolReturn(pendingMessage)
		}
	}()
	for {
		message := pendingMessage
		pendingMessage = nil
		if message == nil {
			select {
			case <-self.ctx.Done():
				return
			case message = <-self.outbound:
			}
		}

		if self.datagramSettings.UseDatagramForPath(
			len(message),
			self.maxDatagramByteCount,
		) {
			useStream, sendErr := self.sendDatagramMessage(message, false)
			if sendErr == nil && !useStream {
				sendErr = self.observeRouteMessage(message)
			}
			if !useStream {
				clientconnect.MessagePoolReturn(message)
			}
			if sendErr != nil {
				self.fail(fmt.Errorf("write hybrid H3 DATAGRAM message: %w", sendErr))
				return
			}
			if !useStream {
				continue
			}
		}

		var messageStorage [h3TransferCarrierLegacyBatchMessageCount][]byte
		messages := messageStorage[:1]
		messages[0] = message
		batchByteCount := len(message) + 4
	drainReady:
		for len(messages) < cap(messages) {
			select {
			case <-self.ctx.Done():
				break drainReady
			case nextMessage := <-self.outbound:
				if self.datagramSettings.UseDatagramForPath(
					len(nextMessage),
					self.maxDatagramByteCount,
				) {
					pendingMessage = nextMessage
					break drainReady
				}
				framedByteCount := len(nextMessage) + 4
				if h3TransferCarrierLegacyBatchByteCount < batchByteCount+framedByteCount {
					pendingMessage = nextMessage
					break drainReady
				}
				messages = append(messages, nextMessage)
				batchByteCount += framedByteCount
			default:
				break drainReady
			}
		}

		var writeErr error
		for _, routedMessage := range messages {
			if err := self.observeRouteMessage(routedMessage); err != nil && writeErr == nil {
				writeErr = err
			}
		}
		if storage == nil {
			storage = make([]byte, h3TransferCarrierLegacyBatchByteCount)
		}
		if writeErr == nil {
			writeErr = self.framer.WriteBatchWithStorage(self.stream, messages, storage)
		}
		if writeErr == nil {
			for _, routedMessage := range messages {
				self.datagramStats.RecordStreamSent(len(routedMessage))
			}
		}
		for _, routedMessage := range messages {
			clientconnect.MessagePoolReturn(routedMessage)
		}
		if writeErr != nil {
			self.fail(fmt.Errorf("write hybrid H3 stream batch: %w", writeErr))
			return
		}
	}
}

// One stream reader returns complete pooled frames to the zero-wait route
// boundary. QUIC owns recovery below this read.
func (self *h3TransferCarrierEndpoint) runLegacyReader() {
	defer self.workerWaitGroup.Done()
	for {
		message, err := self.framer.Read(self.stream)
		if err != nil {
			self.fail(fmt.Errorf("read legacy H3 frame: %w", err))
			return
		}
		self.deliver(message)
	}
}

func (self *h3TransferCarrierEndpoint) runHybridStreamReader() {
	defer self.workerWaitGroup.Done()
	for {
		message, err := self.framer.Read(self.stream)
		if err != nil {
			self.fail(fmt.Errorf("read hybrid H3 stream frame: %w", err))
			return
		}
		self.datagramStats.RecordStreamReceived(len(message))
		self.deliver(message)
	}
}

// The unreliable reader exposes only complete reassembled Transfer frames and
// leaves missing fragments to the real Transfer retry loop above it.
func (self *h3TransferCarrierEndpoint) runDatagramReader() {
	defer self.workerWaitGroup.Done()
	for {
		datagram, err := self.connection.ReceiveDatagram(self.ctx)
		if err != nil {
			self.fail(fmt.Errorf("read H3 DATAGRAM: %w", err))
			return
		}
		message := self.reassembler.Accept(datagram, time.Now())
		if message != nil {
			self.deliver(message)
		}
	}
}

// Starts exactly one sender and one receiver for this endpoint.
func (self *h3TransferCarrierEndpoint) start() {
	switch self.mode {
	case h3TransferCarrierDatagram:
		self.workerWaitGroup.Add(2)
		go self.runDatagramWriter()
		go self.runDatagramReader()
	case h3TransferCarrierHybrid:
		self.workerWaitGroup.Add(3)
		go self.runHybridWriter()
		go self.runDatagramReader()
		go self.runHybridStreamReader()
	default:
		self.workerWaitGroup.Add(2)
		go self.runLegacyWriter()
		go self.runLegacyReader()
	}
}

// Drains a route only after every producer and consumer has been joined. The
// returned shape makes admitted-but-unwritten carrier work visible without
// classifying it as an on-wire send.
func drainH3TransferCarrierRoute(route clientconnect.Route) h3TransferCarrierDrainStats {
	var stats h3TransferCarrierDrainStats
	for {
		select {
		case message := <-route:
			stats.MessageCount += 1
			transferFrame := &protocol.TransferFrame{}
			if err := clientconnect.ProtoUnmarshal(message, transferFrame); err != nil {
				stats.DecodeErrorCount += 1
			} else {
				if transferFrame.GetPack() != nil {
					stats.PackCount += 1
				}
				if transferFrame.GetAck() != nil {
					stats.AckCount += 1
				}
			}
			clientconnect.MessagePoolReturn(message)
		default:
			return stats
		}
	}
}

// Teardown first interrupts socket I/O, then joins workers, releases incomplete
// reassemblies, and finally returns every unconsumed route buffer.
func (self *h3TransferCarrierPair) Close() {
	self.closeOnce.Do(func() {
		self.cancel()
		if self.clientEndpoint != nil && self.clientEndpoint.connection != nil {
			_ = self.clientEndpoint.connection.CloseWithError(0, "carrier comparison complete")
		}
		if self.serverEndpoint != nil && self.serverEndpoint.connection != nil {
			_ = self.serverEndpoint.connection.CloseWithError(0, "carrier comparison complete")
		}
		if self.clientTransport != nil {
			_ = self.clientTransport.Close()
		}
		if self.serverTransport != nil {
			_ = self.serverTransport.Close()
		}
		if self.clientPacket != nil {
			_ = self.clientPacket.Close()
		}
		if self.serverPacket != nil {
			_ = self.serverPacket.Close()
		}
		self.workerWaitGroup.Wait()
		if self.clientEndpoint != nil && self.clientEndpoint.reassembler != nil {
			self.clientEndpoint.reassembler.Close()
		}
		if self.serverEndpoint != nil && self.serverEndpoint.reassembler != nil {
			self.serverEndpoint.reassembler.Close()
		}
		if self.clientEndpoint != nil {
			self.clientEndpoint.discardedOutbound =
				drainH3TransferCarrierRoute(self.clientEndpoint.outbound)
			_ = drainH3TransferCarrierRoute(self.clientEndpoint.inbound)
		}
		if self.serverEndpoint != nil {
			self.serverEndpoint.discardedOutbound =
				drainH3TransferCarrierRoute(self.serverEndpoint.outbound)
			_ = drainH3TransferCarrierRoute(self.serverEndpoint.inbound)
		}
	})
}

// A framed empty exchange completes the QUIC handshake and opens the one
// bidirectional control/data stream before any measured route bytes exist.
func newH3TransferCarrierPair(
	ctx context.Context,
	path *tunPath,
	profile networkProfile,
	mode h3TransferCarrierMode,
) (*h3TransferCarrierPair, error) {
	pairCtx, cancel := context.WithCancel(ctx)
	pair := &h3TransferCarrierPair{
		ctx:           pairCtx,
		cancel:        cancel,
		errors:        make(chan error, 8),
		datagramStats: &clientconnect.H3DatagramStats{},
	}
	fail := func(err error) (*h3TransferCarrierPair, error) {
		pair.Close()
		return nil, err
	}

	serverPacket, err := path.right.ListenUDP(&net.UDPAddr{
		IP:   path.endpointAddress(false),
		Port: 0,
	})
	if err != nil {
		return fail(fmt.Errorf("listen server UDP: %w", err))
	}
	pair.serverPacket = serverPacket
	clientPacket, err := path.left.ListenUDP(&net.UDPAddr{
		IP:   path.endpointAddress(true),
		Port: 0,
	})
	if err != nil {
		return fail(fmt.Errorf("listen client UDP: %w", err))
	}
	pair.clientPacket = clientPacket

	serverTlsConfig, clientTlsConfig, err := newWorkloadTlsConfigs()
	if err != nil {
		return fail(fmt.Errorf("create H3 carrier TLS: %w", err))
	}
	roundTrip := profile.Forward.BaseDelay + profile.Forward.ProcessingDelay +
		profile.Reverse.BaseDelay + profile.Reverse.ProcessingDelay
	quicConfig := &quic.Config{
		HandshakeIdleTimeout: max(15*time.Second, 12*roundTrip),
		MaxIdleTimeout:       max(30*time.Second, 20*roundTrip),
		InitialPacketSize:    1200,
		EnableDatagrams:      true,
	}
	serverTransport := &quic.Transport{Conn: serverPacket}
	pair.serverTransport = serverTransport
	listener, err := serverTransport.ListenEarly(serverTlsConfig, quicConfig)
	if err != nil {
		return fail(fmt.Errorf("listen H3 carrier: %w", err))
	}
	defer listener.Close()

	framerSettings := clientconnect.DefaultFramerSettings(
		int(clientconnect.DefaultClientSettings().MinimumMessageLenLimit()),
	)
	type acceptedEndpoint struct {
		connection *quic.Conn
		stream     *quic.Stream
	}
	serverEndpointResult := make(chan acceptedEndpoint, 1)
	serverAcceptError := make(chan error, 1)
	go func() {
		connection, acceptErr := listener.Accept(pairCtx)
		if acceptErr != nil {
			serverAcceptError <- acceptErr
			return
		}
		stream, acceptErr := connection.AcceptStream(pairCtx)
		if acceptErr != nil {
			_ = connection.CloseWithError(0, "accept stream failed")
			serverAcceptError <- acceptErr
			return
		}
		framer := clientconnect.NewFramer(framerSettings)
		message, readErr := framer.Read(stream)
		if readErr != nil {
			_ = connection.CloseWithError(0, "warm read failed")
			serverAcceptError <- readErr
			return
		}
		if len(message) != 0 {
			clientconnect.MessagePoolReturn(message)
			_ = connection.CloseWithError(0, "unexpected warm message")
			serverAcceptError <- fmt.Errorf("H3 warm message has %d bytes", len(message))
			return
		}
		clientconnect.MessagePoolReturn(message)
		if writeErr := framer.Write(stream, []byte{}); writeErr != nil {
			_ = connection.CloseWithError(0, "warm write failed")
			serverAcceptError <- writeErr
			return
		}
		serverEndpointResult <- acceptedEndpoint{connection: connection, stream: stream}
	}()

	clientTransport := &quic.Transport{Conn: clientPacket}
	pair.clientTransport = clientTransport
	clientConnection, err := clientTransport.DialEarly(
		pairCtx,
		serverPacket.LocalAddr(),
		clientTlsConfig,
		quicConfig,
	)
	if err != nil {
		return fail(fmt.Errorf("dial H3 carrier: %w", err))
	}
	clientStream, err := clientConnection.OpenStreamSync(pairCtx)
	if err != nil {
		_ = clientConnection.CloseWithError(0, "open stream failed")
		return fail(fmt.Errorf("open H3 carrier stream: %w", err))
	}
	clientFramer := clientconnect.NewFramer(framerSettings)
	if err := clientFramer.Write(clientStream, []byte{}); err != nil {
		_ = clientConnection.CloseWithError(0, "warm write failed")
		return fail(fmt.Errorf("write H3 warm frame: %w", err))
	}
	warmResponse, err := clientFramer.Read(clientStream)
	if err != nil {
		_ = clientConnection.CloseWithError(0, "warm read failed")
		return fail(fmt.Errorf("read H3 warm response: %w", err))
	}
	if len(warmResponse) != 0 {
		clientconnect.MessagePoolReturn(warmResponse)
		_ = clientConnection.CloseWithError(0, "unexpected warm response")
		return fail(fmt.Errorf("H3 warm response has %d bytes", len(warmResponse)))
	}
	clientconnect.MessagePoolReturn(warmResponse)

	var serverAccepted acceptedEndpoint
	select {
	case serverAccepted = <-serverEndpointResult:
	case acceptErr := <-serverAcceptError:
		_ = clientConnection.CloseWithError(0, "server accept failed")
		return fail(fmt.Errorf("accept H3 carrier endpoint: %w", acceptErr))
	case <-pairCtx.Done():
		_ = clientConnection.CloseWithError(0, "setup canceled")
		return fail(pairCtx.Err())
	}
	if mode != h3TransferCarrierLegacyStream {
		clientState := clientConnection.ConnectionState()
		serverState := serverAccepted.connection.ConnectionState()
		if !clientState.SupportsDatagrams.Local || !clientState.SupportsDatagrams.Remote ||
			!serverState.SupportsDatagrams.Local || !serverState.SupportsDatagrams.Remote {
			_ = clientConnection.CloseWithError(0, "DATAGRAM not negotiated")
			_ = serverAccepted.connection.CloseWithError(0, "DATAGRAM not negotiated")
			return fail(fmt.Errorf(
				"H3 DATAGRAM unavailable: client=%+v server=%+v",
				clientState.SupportsDatagrams,
				serverState.SupportsDatagrams,
			))
		}
	}

	datagramSettings := clientconnect.DefaultH3DatagramSettings()
	sharedBudget := clientconnect.NewH3DatagramReassemblyBudget(
		2 * datagramSettings.ProcessReassemblyByteCount,
	)
	newEndpoint := func(connection *quic.Conn, stream *quic.Stream) (*h3TransferCarrierEndpoint, error) {
		endpoint := &h3TransferCarrierEndpoint{
			ctx:                  pairCtx,
			cancel:               cancel,
			mode:                 mode,
			connection:           connection,
			stream:               stream,
			outbound:             make(clientconnect.Route, h3TransferCarrierRouteCapacity),
			inbound:              make(clientconnect.Route, h3TransferCarrierRouteCapacity),
			framer:               clientconnect.NewFramer(framerSettings),
			maxDatagramByteCount: datagramSettings.TargetDatagramByteCount,
			datagramSettings:     datagramSettings,
			datagramStats:        pair.datagramStats,
			routeStats: h3TransferCarrierRouteStats{
				seenPackSequenceItems: map[string]bool{},
				packLastWriteTimes:    map[string]time.Time{},
				packAttemptCounts:     map[string]int{},
				packWriteTimes:        map[string][]time.Time{},
				packSequenceNumbers:   map[string]uint64{},
			},
			carrierErrors:   pair.errors,
			workerWaitGroup: &pair.workerWaitGroup,
		}
		if mode != h3TransferCarrierLegacyStream {
			fragmenter, fragmenterErr := clientconnect.NewH3DatagramFragmenter(
				datagramSettings,
				pair.datagramStats,
			)
			if fragmenterErr != nil {
				return nil, fragmenterErr
			}
			reassembler, reassemblerErr := clientconnect.NewH3DatagramReassembler(
				datagramSettings,
				sharedBudget,
				pair.datagramStats,
			)
			if reassemblerErr != nil {
				return nil, reassemblerErr
			}
			endpoint.fragmenter = fragmenter
			endpoint.reassembler = reassembler
		}
		return endpoint, nil
	}
	pair.clientEndpoint, err = newEndpoint(clientConnection, clientStream)
	if err != nil {
		_ = clientConnection.CloseWithError(0, "endpoint init failed")
		_ = serverAccepted.connection.CloseWithError(0, "endpoint init failed")
		return fail(fmt.Errorf("create client H3 endpoint: %w", err))
	}
	pair.serverEndpoint, err = newEndpoint(serverAccepted.connection, serverAccepted.stream)
	if err != nil {
		_ = clientConnection.CloseWithError(0, "endpoint init failed")
		_ = serverAccepted.connection.CloseWithError(0, "endpoint init failed")
		return fail(fmt.Errorf("create server H3 endpoint: %w", err))
	}
	pair.clientEndpoint.start()
	pair.serverEndpoint.start()
	return pair, nil
}

// Test payloads are valid protobuf strings with an exact fixed byte count and
// a six-digit receive index.
func h3TransferCarrierTestContents(messageCount int, payloadByteCount int) ([]string, error) {
	if messageCount < 1 || 999999 < messageCount || payloadByteCount < 7 {
		return nil, fmt.Errorf(
			"invalid H3 carrier workload messages=%d payload-bytes=%d",
			messageCount,
			payloadByteCount,
		)
	}
	contents := make([]string, messageCount)
	for messageIndex := range messageCount {
		prefix := fmt.Sprintf("%06d:", messageIndex)
		contents[messageIndex] = prefix + strings.Repeat("x", payloadByteCount-len(prefix))
	}
	return contents, nil
}

// Cancellation joins Client-owned workers without consuming the measurement
// context after the carrier has been stopped.
func closeH3TransferCarrierClient(client *clientconnect.Client) error {
	if client == nil {
		return nil
	}
	cleanupCtx, cleanupCancel := context.WithTimeout(
		context.Background(),
		h3TransferCarrierTestClientCleanupTimeout,
	)
	defer cleanupCancel()
	return client.CloseAndWait(cleanupCtx)
}

// Runs actual Pack, ACK, RTT estimation, deduplication, and resend state over
// one established carrier. Only the post-auth H3 delivery mechanism differs.
func measureH3TransferCarrier(
	ctx context.Context,
	profile networkProfile,
	mode h3TransferCarrierMode,
	messageCount int,
	payloadByteCount int,
) (result h3TransferCarrierResult, resultErr error) {
	// This path represents the physical QUIC access link. The profile's
	// InnerMtu is for traffic nested inside the VPN and would incorrectly make
	// QUIC's minimum 1,200-byte UDP payload itself require IP fragmentation.
	carrierProfile := profile
	carrierProfile.InnerMtu = min(profile.Forward.OuterMtu, profile.Reverse.OuterMtu)
	path, err := newTunPath(ctx, carrierProfile, mobileTunResourceProfile())
	if err != nil {
		return result, err
	}
	defer path.close()
	pair, err := newH3TransferCarrierPair(ctx, path, profile, mode)
	if err != nil {
		return result, err
	}
	defer pair.Close()

	var firstRouteWriteErrors atomic.Uint64
	var clientWireStats h3TransferCarrierWireStats
	clientSettings := func(
		lifecycleObserver func(clientconnect.SendPackLifecycleObservation),
		wireObserver func(clientconnect.TransferWireMessageObservation),
	) *clientconnect.ClientSettings {
		settings := clientconnect.DefaultClientSettings()
		settings.EncryptionSettings.Mode = clientconnect.EncryptionModeOff
		settings.SendBufferSettings.SendPackLifecycleObserver = lifecycleObserver
		settings.SendBufferSettings.TransferWireMessageObserver = wireObserver
		return settings
	}
	client := clientconnect.NewClient(
		ctx,
		clientconnect.NewId(),
		clientconnect.NewNoContractClientOob(),
		clientSettings(func(observation clientconnect.SendPackLifecycleObservation) {
			if observation.Phase == clientconnect.SendPackLifecyclePhaseFirstRouteWrite &&
				observation.Err != nil {
				firstRouteWriteErrors.Add(1)
			}
		}, clientWireStats.observe),
	)
	server := clientconnect.NewClient(
		ctx,
		clientconnect.NewId(),
		clientconnect.NewNoContractClientOob(),
		clientSettings(nil, nil),
	)
	defer func() {
		pair.Close()
		clientErr := closeH3TransferCarrierClient(client)
		serverErr := closeH3TransferCarrierClient(server)
		_ = drainH3TransferCarrierRoute(pair.clientEndpoint.outbound)
		_ = drainH3TransferCarrierRoute(pair.clientEndpoint.inbound)
		_ = drainH3TransferCarrierRoute(pair.serverEndpoint.outbound)
		_ = drainH3TransferCarrierRoute(pair.serverEndpoint.inbound)
		if resultErr == nil {
			resultErr = errors.Join(clientErr, serverErr)
		}
	}()

	sendCarrierProperties := clientconnect.TransferCarrierProperties{}
	if mode != h3TransferCarrierLegacyStream {
		sendCarrierProperties.Unreliable = true
	}
	client.RouteManager().UpdateTransportWithProperties(
		clientconnect.NewSendClientTransport(clientconnect.DestinationId(server.ClientId())),
		[]clientconnect.Route{pair.clientEndpoint.outbound},
		sendCarrierProperties,
	)
	client.RouteManager().UpdateTransport(
		clientconnect.NewReceiveGatewayTransport(),
		[]clientconnect.Route{pair.clientEndpoint.inbound},
	)
	server.RouteManager().UpdateTransportWithProperties(
		clientconnect.NewSendClientTransport(clientconnect.DestinationId(client.ClientId())),
		[]clientconnect.Route{pair.serverEndpoint.outbound},
		sendCarrierProperties,
	)
	server.RouteManager().UpdateTransport(
		clientconnect.NewReceiveGatewayTransport(),
		[]clientconnect.Route{pair.serverEndpoint.inbound},
	)
	client.ContractManager().AddNoContractPeer(server.ClientId())
	server.ContractManager().AddNoContractPeer(client.ClientId())

	contents, err := h3TransferCarrierTestContents(messageCount, payloadByteCount)
	if err != nil {
		return result, err
	}
	receivedStates := make([]atomic.Uint32, messageCount)
	var receivedCount atomic.Int64
	var acknowledgedCount atomic.Int64
	var firstReceiveElapsedNano atomic.Int64
	allReceived := make(chan struct{})
	allAcknowledged := make(chan struct{})
	asyncErrors := make(chan error, messageCount)
	recordAsyncError := func(err error) {
		select {
		case asyncErrors <- err:
		default:
		}
	}
	var workloadStart time.Time
	removeReceiveCallback := server.AddReceiveCallback(func(
		_ clientconnect.TransferPath,
		frames []*protocol.Frame,
		_ clientconnect.Peer,
	) {
		for _, frame := range frames {
			if frame.GetMessageType() != protocol.MessageType_TestSimpleMessage {
				continue
			}
			message, decodeErr := clientconnect.FromFrame(frame)
			if decodeErr != nil {
				recordAsyncError(fmt.Errorf("decode received application frame: %w", decodeErr))
				continue
			}
			simpleMessage, ok := message.(*protocol.SimpleMessage)
			if !ok {
				recordAsyncError(fmt.Errorf("received application type %T", message))
				continue
			}
			content := simpleMessage.GetContent()
			if len(content) != payloadByteCount || len(content) < 7 || content[6] != ':' {
				recordAsyncError(fmt.Errorf("received malformed payload with %d bytes", len(content)))
				continue
			}
			messageIndex, parseErr := strconv.Atoi(content[:6])
			if parseErr != nil || messageIndex < 0 || messageCount <= messageIndex ||
				content != contents[messageIndex] {
				recordAsyncError(fmt.Errorf("received corrupt payload index=%q", content[:6]))
				continue
			}
			if !receivedStates[messageIndex].CompareAndSwap(0, 1) {
				recordAsyncError(fmt.Errorf("application message %d delivered twice", messageIndex))
				continue
			}
			firstReceiveElapsedNano.CompareAndSwap(0, time.Since(workloadStart).Nanoseconds())
			if receivedCount.Add(1) == int64(messageCount) {
				close(allReceived)
			}
		}
	})
	defer removeReceiveCallback()

	measurementStart, err := path.beginMeasurement(ctx)
	if err != nil {
		return result, err
	}
	workloadStart = time.Now()
	for messageIndex, content := range contents {
		frame, frameErr := clientconnect.ToFrame(
			&protocol.SimpleMessage{Content: content},
			clientconnect.DefaultProtocolVersion,
		)
		if frameErr != nil {
			return result, fmt.Errorf("encode application message %d: %w", messageIndex, frameErr)
		}
		acknowledge := func(ackErr error) {
			if ackErr != nil {
				recordAsyncError(fmt.Errorf("application acknowledgement: %w", ackErr))
				return
			}
			if acknowledgedCount.Add(1) == int64(messageCount) {
				close(allAcknowledged)
			}
		}
		accepted, sendErr := client.SendWithTimeoutDetailed(
			frame,
			server.ClientId(),
			acknowledge,
			15*time.Second,
		)
		if !accepted {
			clientconnect.MessagePoolReturn(frame.MessageBytes)
			if sendErr == nil {
				sendErr = fmt.Errorf("send was not admitted")
			}
			return result, fmt.Errorf("send application message %d: %w", messageIndex, sendErr)
		}
	}

	receivedComplete := false
	acknowledgedComplete := false
	for !receivedComplete || !acknowledgedComplete {
		select {
		case asyncErr := <-asyncErrors:
			return result, asyncErr
		case carrierErr := <-pair.errors:
			return result, carrierErr
		case <-allReceived:
			receivedComplete = true
			allReceived = nil
		case <-allAcknowledged:
			acknowledgedComplete = true
			allAcknowledged = nil
		case <-ctx.Done():
			return result, ctx.Err()
		}
	}
	duration := time.Since(workloadStart)

	// Stop the carrier before the end boundary so delayed QUIC acknowledgements
	// and connection-close packets cannot escape the measured link interval.
	pair.Close()
	forwardLink, reverseLink, err := path.finishMeasurement(ctx, measurementStart)
	if err != nil {
		return result, fmt.Errorf("finish H3 Transfer carrier measurement: %w", err)
	}
	result = h3TransferCarrierResult{
		Mode:                  mode,
		UsefulByteCount:       int64(messageCount * payloadByteCount),
		Duration:              duration,
		TimeToFirstMessage:    time.Duration(firstReceiveElapsedNano.Load()),
		ForwardLink:           forwardLink,
		ReverseLink:           reverseLink,
		ClientRouteStats:      pair.clientEndpoint.routeStats,
		ServerRouteStats:      pair.serverEndpoint.routeStats,
		ClientRecoveryStats:   client.SendRecoveryStats(),
		ClientWireStats:       clientWireStats.snapshot(),
		ClientDiscardedRoute:  pair.clientEndpoint.discardedOutbound,
		FirstRouteWriteErrors: firstRouteWriteErrors.Load(),
		ReceiveRouteDropCount: pair.clientEndpoint.receiveRouteDropCount.Load() +
			pair.serverEndpoint.receiveRouteDropCount.Load(),
		DatagramStats: pair.datagramStats.Snapshot(),
	}
	return result, nil
}

// Checks one completed mode and emits one machine-searchable evidence row.
func requireH3TransferCarrierResult(
	t *testing.T,
	workload string,
	profile networkProfile,
	messageCount int,
	payloadByteCount int,
	result h3TransferCarrierResult,
) {
	t.Helper()
	if result.UsefulByteCount != int64(messageCount*payloadByteCount) ||
		result.Duration <= 0 || result.TimeToFirstMessage <= 0 {
		t.Fatalf("H3 Transfer carrier mode=%s incomplete result: %+v", result.Mode, result)
	}
	if result.ForwardLink.UnexpectedLossDropPacketCount != 0 ||
		result.ForwardLink.UnexpectedMtuDropPacketCount != 0 ||
		result.ForwardLink.UnexpectedQueueDropPacketCount != 0 ||
		result.ForwardLink.UnexpectedOutageDropPacketCount != 0 ||
		result.ReverseLink.UnexpectedLossDropPacketCount != 0 ||
		result.ReverseLink.UnexpectedMtuDropPacketCount != 0 ||
		result.ReverseLink.UnexpectedQueueDropPacketCount != 0 ||
		result.ReverseLink.UnexpectedOutageDropPacketCount != 0 {
		t.Fatalf("H3 Transfer carrier mode=%s had unexpected link drops: forward=%+v reverse=%+v", result.Mode, result.ForwardLink, result.ReverseLink)
	}
	// A full production-shaped receive boundary deliberately drops rather than
	// blocking the carrier callback. Exact Transfer delivery and the measured
	// drop counter are the gate; forbidding every drop would make this test
	// contradict connect/CODESTYLE.md and hide stream-burst collapse behavior.
	recoveryWriteCount := result.ClientRecoveryStats.TimeoutResendWriteCount +
		result.ClientRecoveryStats.SelectiveGapWriteCount +
		result.ClientRecoveryStats.AckTailProbeWriteCount +
		result.ClientRecoveryStats.CumulativeProbeWriteCount
	if result.ClientWireStats.DecodeErrorCount != 0 ||
		result.ClientDiscardedRoute.DecodeErrorCount != 0 {
		t.Fatalf(
			"H3 Transfer carrier mode=%s could not decode observed Transfer frames: wire=%+v discarded=%+v",
			result.Mode,
			result.ClientWireStats,
			result.ClientDiscardedRoute,
		)
	}
	if recoveryWriteCount != result.ClientWireStats.ResendMessageCount ||
		result.ClientWireStats.ResendMessageCount != result.ClientWireStats.ResendPackCount ||
		result.ClientWireStats.ResendAckCount != 0 {
		t.Fatalf(
			"H3 Transfer carrier mode=%s recovery attempt accounting differs from observed Transfer writes: recovery=%d wire=%+v stats=%+v",
			result.Mode,
			recoveryWriteCount,
			result.ClientWireStats,
			result.ClientRecoveryStats,
		)
	}
	admittedRecoveryWriteCount := recoveryWriteCount -
		result.ClientRecoveryStats.RecoveryWriteErrorCount
	if admittedRecoveryWriteCount != result.ClientRouteStats.RepeatedPackCount+
		result.ClientDiscardedRoute.PackCount+
		result.FirstRouteWriteErrors {
		t.Fatalf(
			"H3 Transfer carrier mode=%s recovery admissions=%d, repeated carrier Pack writes=%d, discarded queued Packs=%d, first-write errors=%d, recovery-write errors=%d: wire=%+v stats=%+v",
			result.Mode,
			admittedRecoveryWriteCount,
			result.ClientRouteStats.RepeatedPackCount,
			result.ClientDiscardedRoute.PackCount,
			result.FirstRouteWriteErrors,
			result.ClientRecoveryStats.RecoveryWriteErrorCount,
			result.ClientWireStats,
			result.ClientRecoveryStats,
		)
	}
	if result.Mode != h3TransferCarrierLegacyStream {
		if result.DatagramStats.SentMessageCount == 0 ||
			result.DatagramStats.ReceivedMessageCount == 0 ||
			result.DatagramStats.SentFragmentCount < result.DatagramStats.SentMessageCount {
			t.Fatalf("H3 DATAGRAM carrier counters are incomplete: %+v", result.DatagramStats)
		}
	} else if result.DatagramStats != (clientconnect.H3DatagramStatsSnapshot{}) {
		t.Fatalf("legacy H3 unexpectedly used DATAGRAM: %+v", result.DatagramStats)
	}
	t.Logf(
		"[lowbar-h3-ab] workload=%s mode=%s profile=%s useful_bytes=%d duration=%s first_message=%s forward_wire_bytes=%d reverse_wire_bytes=%d forward_loss=%d reverse_loss=%d forward_queue_drop=%d reverse_queue_drop=%d receive_route_drops=%d pack_writes=%d pack_rewrites=%d pack_write_span=%s max_pack_retry_gap=%s max_pack_attempts=%d retry_timeline=%s first_route_write_errors=%d timeout_resend_writes=%d gap_recovery_writes=%d tail_probe_writes=%d cumulative_probe_writes=%d recovery_write_errors=%d transfer_resend_attempts=%d discarded_route_packs=%d flight_waits=%d flight_gaps=%d flight_timeouts=%d flight_reductions=%d max_flight_bytes=%d max_flight_limit_bytes=%d max_flight_messages=%d max_flight_message_limit=%d ack_writes=%d max_transfer_bytes=%d datagram_messages=%d datagram_fragments=%d datagram_timeouts=%d stream_messages=%d stream_bytes=%d",
		workload,
		result.Mode,
		profile.Name,
		result.UsefulByteCount,
		result.Duration,
		result.TimeToFirstMessage,
		result.ForwardLink.WireByteCount,
		result.ReverseLink.WireByteCount,
		result.ForwardLink.LossDropPacketCount,
		result.ReverseLink.LossDropPacketCount,
		result.ForwardLink.QueueDropPacketCount,
		result.ReverseLink.QueueDropPacketCount,
		result.ReceiveRouteDropCount,
		result.ClientRouteStats.PackCount,
		result.ClientRouteStats.RepeatedPackCount,
		result.ClientRouteStats.PackWriteSpan,
		result.ClientRouteStats.MaximumPackRetryGap,
		result.ClientRouteStats.MaximumPackAttemptCount,
		result.ClientRouteStats.retryTimeline(),
		result.FirstRouteWriteErrors,
		result.ClientRecoveryStats.TimeoutResendWriteCount,
		result.ClientRecoveryStats.SelectiveGapWriteCount,
		result.ClientRecoveryStats.AckTailProbeWriteCount,
		result.ClientRecoveryStats.CumulativeProbeWriteCount,
		result.ClientRecoveryStats.RecoveryWriteErrorCount,
		result.ClientWireStats.ResendMessageCount,
		result.ClientDiscardedRoute.PackCount,
		result.ClientRecoveryStats.UnreliableFlightWaitCount,
		result.ClientRecoveryStats.UnreliableFlightGapCount,
		result.ClientRecoveryStats.UnreliableFlightTimeoutCount,
		result.ClientRecoveryStats.UnreliableFlightReductionCount,
		result.ClientRecoveryStats.UnreliableFlightMaximumByteCount,
		result.ClientRecoveryStats.UnreliableFlightMaximumLimitByteCount,
		result.ClientRecoveryStats.UnreliableFlightMaximumMessageCount,
		result.ClientRecoveryStats.UnreliableFlightMaximumMessageLimit,
		result.ServerRouteStats.AckCount,
		max(result.ClientRouteStats.MaximumMessageBytes, result.ServerRouteStats.MaximumMessageBytes),
		result.DatagramStats.SentMessageCount,
		result.DatagramStats.SentFragmentCount,
		result.DatagramStats.ReassemblyTimeoutCount,
		result.DatagramStats.StreamSentMessageCount+result.DatagramStats.StreamReceivedMessageCount,
		result.DatagramStats.StreamSentMessageByteCount+result.DatagramStats.StreamReceivedMessageByteCount,
	)
}

// Runs one homogeneous two-mode workload with a fresh seeded link per mode.
func runH3TransferCarrierComparison(
	t *testing.T,
	profileName string,
	workload string,
	messageCount int,
	payloadByteCount int,
) {
	t.Helper()
	profile := cellEdgeNetworkProfiles(20260817)[profileName]
	for _, mode := range []h3TransferCarrierMode{
		h3TransferCarrierLegacyStream,
		h3TransferCarrierDatagram,
		h3TransferCarrierHybrid,
	} {
		ctx, cancel := context.WithTimeout(
			context.Background(),
			h3TransferCarrierTestMaximumRunDuration,
		)
		result, err := measureH3TransferCarrier(
			ctx,
			profile,
			mode,
			messageCount,
			payloadByteCount,
		)
		cancel()
		if err != nil {
			t.Fatalf("H3 Transfer carrier workload=%s mode=%s: %v", workload, mode, err)
		}
		requireH3TransferCarrierResult(
			t,
			workload,
			profile,
			messageCount,
			payloadByteCount,
			result,
		)
	}
}

// A deterministic seeded cell-edge run covers the current two-fragment Pack
// shape and emits the evidence needed for repeated A/B runs.
func TestH3TransferCarrierCellEdgeComparison(t *testing.T) {
	runH3TransferCarrierComparison(
		t,
		cellEdge1mDown250kUpName,
		"two-fragment-pack",
		h3TransferCarrierTestMessageCount,
		h3TransferCarrierTestMessagePayloadBytes,
	)
}

// The same useful-byte volume stays below one DATAGRAM per two-frame Pack. This
// isolates fragment-loss amplification from the carrier and Transfer timers.
func TestH3TransferCarrierCellEdgeSingleDatagramComparison(t *testing.T) {
	runH3TransferCarrierComparison(
		t,
		cellEdge1mDown250kUpName,
		"single-datagram-pack",
		2*h3TransferCarrierTestMessageCount,
		h3TransferCarrierTestMessagePayloadBytes/2,
	)
}

// The faster cell-edge point verifies that the cold flight opens quickly
// enough when the uplink has four times the constrained-profile capacity.
func TestH3TransferCarrierCellEdge5mComparison(t *testing.T) {
	runH3TransferCarrierComparison(
		t,
		cellEdge5mDown1mUpName,
		"two-fragment-pack",
		h3TransferCarrierTestMessageCount,
		h3TransferCarrierTestMessagePayloadBytes,
	)
}

// The severe 64 kbit/s uplink verifies that byte-bounded admission remains
// useful when serialization time and queue delay dominate the path.
func TestH3TransferCarrierSevereCellEdgeComparison(t *testing.T) {
	runH3TransferCarrierComparison(
		t,
		cellEdge256kDown64kUpName,
		"two-fragment-pack",
		h3TransferCarrierTestMessageCount,
		h3TransferCarrierTestMessagePayloadBytes,
	)
}
