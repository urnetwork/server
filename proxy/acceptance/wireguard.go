package acceptance

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/urnetwork/userwireguard/conn"
	uwgdevice "github.com/urnetwork/userwireguard/device"
	"github.com/urnetwork/userwireguard/logger"
	"github.com/urnetwork/userwireguard/tun/tuntest"
	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"
	"gvisor.dev/gvisor/pkg/buffer"
	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/adapters/gonet"
	"gvisor.dev/gvisor/pkg/tcpip/checksum"
	"gvisor.dev/gvisor/pkg/tcpip/header"
	"gvisor.dev/gvisor/pkg/tcpip/link/channel"
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv4"
	"gvisor.dev/gvisor/pkg/tcpip/stack"
	"gvisor.dev/gvisor/pkg/tcpip/transport/icmp"
	"gvisor.dev/gvisor/pkg/tcpip/transport/tcp"
	"gvisor.dev/gvisor/pkg/tcpip/transport/udp"
)

// newWireGuardTransport creates a userspace WireGuard device and gVisor
// netstack. It proves the deployed tunnel without requiring root, changing
// routes, or depending on wg-quick being installed on the test host.
func newWireGuardTransport(ctx context.Context, proxyHost string, config *wireGuardConfig) (*wireGuardDiagnosticTransport, func(), error) {
	if proxyHost == "" || config == nil || config.ProxyPort <= 0 || config.ProxyPort > 65535 {
		return nil, nil, fmt.Errorf("WireGuard endpoint is incomplete")
	}
	clientPrivate, err := wgtypes.ParseKey(config.ClientPrivateKey)
	if err != nil {
		return nil, nil, fmt.Errorf("parse WireGuard client key: %w", err)
	}
	proxyPublic, err := wgtypes.ParseKey(config.ProxyPublicKey)
	if err != nil {
		return nil, nil, fmt.Errorf("parse WireGuard proxy key: %w", err)
	}
	clientIPv4, err := netip.ParseAddr(config.ClientIPv4)
	if err != nil || !clientIPv4.Is4() {
		return nil, nil, fmt.Errorf("WireGuard client IPv4 is invalid")
	}
	endpoint, err := net.ResolveUDPAddr("udp4", net.JoinHostPort(proxyHost, strconv.Itoa(config.ProxyPort)))
	if err != nil {
		return nil, nil, fmt.Errorf("resolve WireGuard endpoint: %w", err)
	}
	clientCtx, cancelClient := context.WithCancel(ctx)

	const mtu = 1420
	clientTUN := tuntest.NewChannelTUN()
	clientBind := newWireGuardTrackingBind(conn.NewDefaultBind())
	clientDevice := uwgdevice.NewDevice(
		clientTUN.TUN(),
		clientBind,
		logger.NewLogger(logger.LogLevelError, "proxy-acceptance-wg: "),
	)
	clientStack, err := newWireGuardStack(clientCtx, clientIPv4, mtu, clientTUN)
	if err != nil {
		cancelClient()
		clientDevice.Close()
		return nil, nil, fmt.Errorf("create WireGuard netstack: %w", err)
	}

	zeroPort := 0
	keepalive := 25 * time.Second
	deviceConfig := wgtypes.Config{
		PrivateKey:   &clientPrivate,
		ListenPort:   &zeroPort,
		ReplacePeers: true,
		Peers: []wgtypes.PeerConfig{{
			PublicKey:                   proxyPublic,
			Endpoint:                    endpoint,
			PersistentKeepaliveInterval: &keepalive,
			ReplaceAllowedIPs:           true,
			AllowedIPs: []net.IPNet{{
				IP:   net.IPv4zero.To4(),
				Mask: net.CIDRMask(0, 32),
			}},
		}},
	}
	if err := clientDevice.IpcSet(&deviceConfig); err != nil {
		cancelClient()
		clientStack.Close()
		clientDevice.Close()
		return nil, nil, fmt.Errorf("configure WireGuard client: %w", err)
	}
	if err := clientDevice.Up(); err != nil {
		cancelClient()
		clientStack.Close()
		clientDevice.Close()
		return nil, nil, fmt.Errorf("start WireGuard client: %w", err)
	}

	transport := &wireGuardDiagnosticTransport{
		Transport: &http.Transport{DialContext: clientStack.DialContext},
		stack:     clientStack,
		bind:      clientBind,
	}
	var closeOnce sync.Once
	closeClient := func() {
		closeOnce.Do(func() {
			cancelClient()
			transport.CloseIdleConnections()
			clientStack.Close()
			clientDevice.Close()
		})
	}
	go func() {
		<-clientCtx.Done()
		closeClient()
	}()
	return transport, closeClient, nil
}

type wireGuardStack struct {
	stack      *stack.Stack
	nicID      tcpip.NICID
	clientIPv4 netip.Addr
	statsLock  sync.Mutex
	stats      wireGuardPacketStats
}

type wireGuardPacketDirectionStats struct {
	Packets             uint64
	Bytes               uint64
	LocalAddrPackets    uint64
	ForeignAddrPackets  uint64
	TCPPackets          uint64
	TCPPayloadPackets   uint64
	TCPPayloadBytes     uint64
	Syn                 uint64
	SynAck              uint64
	Fin                 uint64
	Rst                 uint64
	TCPChecksumValid    uint64
	TCPChecksumInvalid  uint64
	RstForDial          uint64
	RstOther            uint64
	RstSequenceMatch    uint64
	RstSequenceMismatch uint64
	LastDialRst         wireGuardTCPPacket
	LastOtherRst        wireGuardTCPPacket
	LastPacketNanos     int64
}

type wireGuardPacketStats struct {
	Outbound        wireGuardPacketDirectionStats
	Inbound         wireGuardPacketDirectionStats
	DialAddr        netip.Addr
	DialPort        uint16
	DialFlow        wireGuardTCPFlow
	EventSequence   uint64
	RecentTCPEvents [wireGuardTCPPacketEventCount]wireGuardTCPPacketEvent
}

const wireGuardTCPPacketEventCount = 32

type wireGuardTCPPacketEvent struct {
	EventSequence   uint64
	Nanos           int64
	Outbound        bool
	SourcePort      uint16
	DestinationPort uint16
	TCPSequence     uint32
	Acknowledgment  uint32
	Flags           header.TCPFlags
	PayloadBytes    int
	ChecksumValid   bool
}

// wireGuardOuterPacketStats is the encrypted UDP boundary immediately below
// userwireguard. It deliberately retains only aggregate packet/byte/error
// counts and timestamps: endpoints and peer keys are unnecessary to distinguish
// a server/public-path silence from local decrypt/TUN loss.
type wireGuardOuterPacketStats struct {
	SendAttemptPackets uint64
	SendAttemptBytes   uint64
	SendPackets        uint64
	SendBytes          uint64
	SendErrors         uint64
	ReceivePackets     uint64
	ReceiveBytes       uint64
	ReceiveErrors      uint64
	LastSendNanos      int64
	LastReceiveNanos   int64
	// Indexes 1..4 are the public WireGuard message types (handshake
	// initiation, handshake response, cookie reply, transport data); index 0
	// is malformed/unknown. These are visible envelope types, not payload or
	// peer identity.
	SendMessageTypes    [5]uint64
	ReceiveMessageTypes [5]uint64
	EventSequence       uint64
	RecentEvents        [wireGuardOuterPacketEventCount]wireGuardOuterPacketEvent
}

const wireGuardOuterPacketEventCount = 16

type wireGuardOuterPacketEvent struct {
	Sequence    uint64
	Nanos       int64
	Bytes       int
	MessageType uint8
	Inbound     bool
}

type wireGuardTrackingBind struct {
	conn.Bind
	statsLock sync.Mutex
	stats     wireGuardOuterPacketStats
}

func newWireGuardTrackingBind(bind conn.Bind) *wireGuardTrackingBind {
	return &wireGuardTrackingBind{Bind: bind}
}

func (b *wireGuardTrackingBind) Open(bindIPv4 string, bindIPv6 string, port uint16) ([]conn.ReceiveFunc, uint16, error) {
	receiveFunctions, actualPort, err := b.Bind.Open(bindIPv4, bindIPv6, port)
	if err != nil {
		return nil, actualPort, err
	}
	tracked := make([]conn.ReceiveFunc, len(receiveFunctions))
	for i, receive := range receiveFunctions {
		receive := receive
		tracked[i] = func(packets [][]byte, sizes []int, endpoints []conn.Endpoint) (int, error) {
			n, receiveErr := receive(packets, sizes, endpoints)
			now := time.Now().UnixNano()
			packetCount := 0
			byteCount := 0
			messageTypes := [5]uint64{}
			events := make([]wireGuardOuterPacketEvent, 0, min(max(0, n), len(sizes)))
			for i, size := range sizes[:min(max(0, n), len(sizes))] {
				if 0 < size {
					packetCount++
					byteCount += size
					messageType := wireGuardOuterMessageType(packets[i], size)
					messageTypes[messageType]++
					events = append(events, wireGuardOuterPacketEvent{
						Nanos:       now,
						Bytes:       size,
						MessageType: uint8(messageType),
						Inbound:     true,
					})
				}
			}
			b.statsLock.Lock()
			b.stats.ReceivePackets += uint64(packetCount)
			b.stats.ReceiveBytes += uint64(byteCount)
			for messageType, count := range messageTypes {
				b.stats.ReceiveMessageTypes[messageType] += count
			}
			if 0 < packetCount {
				b.stats.LastReceiveNanos = now
			}
			if receiveErr != nil && !errors.Is(receiveErr, net.ErrClosed) {
				b.stats.ReceiveErrors++
			}
			for _, event := range events {
				b.recordEventWithLock(event)
			}
			b.statsLock.Unlock()
			return n, receiveErr
		}
	}
	return tracked, actualPort, nil
}

func (b *wireGuardTrackingBind) Send(packets [][]byte, endpoint conn.Endpoint) error {
	byteCount := 0
	messageTypes := [5]uint64{}
	events := make([]wireGuardOuterPacketEvent, 0, len(packets))
	for _, packet := range packets {
		byteCount += len(packet)
		messageType := wireGuardOuterMessageType(packet, len(packet))
		messageTypes[messageType]++
		events = append(events, wireGuardOuterPacketEvent{
			Bytes:       len(packet),
			MessageType: uint8(messageType),
		})
	}
	err := b.Bind.Send(packets, endpoint)
	now := time.Now().UnixNano()
	b.statsLock.Lock()
	b.stats.SendAttemptPackets += uint64(len(packets))
	b.stats.SendAttemptBytes += uint64(byteCount)
	if err == nil {
		b.stats.SendPackets += uint64(len(packets))
		b.stats.SendBytes += uint64(byteCount)
		for messageType, count := range messageTypes {
			b.stats.SendMessageTypes[messageType] += count
		}
		if 0 < len(packets) {
			b.stats.LastSendNanos = now
		}
		for _, event := range events {
			event.Nanos = now
			b.recordEventWithLock(event)
		}
	} else {
		b.stats.SendErrors++
	}
	b.statsLock.Unlock()
	return err
}

// recordEventWithLock retains only public WireGuard envelope metadata. The
// fixed ring prevents a long soak from growing memory and deliberately omits
// endpoints, peer keys, and packet contents.
func (b *wireGuardTrackingBind) recordEventWithLock(event wireGuardOuterPacketEvent) {
	b.stats.EventSequence++
	event.Sequence = b.stats.EventSequence
	b.stats.RecentEvents[(event.Sequence-1)%uint64(len(b.stats.RecentEvents))] = event
}

func wireGuardOuterMessageType(packet []byte, size int) int {
	if size < 4 || len(packet) < 4 {
		return 0
	}
	messageType := int(binary.LittleEndian.Uint32(packet[:4]))
	if 1 <= messageType && messageType <= 4 {
		return messageType
	}
	return 0
}

func (b *wireGuardTrackingBind) packetStats() wireGuardOuterPacketStats {
	b.statsLock.Lock()
	defer b.statsLock.Unlock()
	return b.stats
}

type wireGuardTCPFlow struct {
	SourcePort         uint16
	ExpectedInboundSeq uint32
	ExpectedSeqSet     bool
}

type wireGuardTCPPacket struct {
	SourceAddr      netip.Addr
	DestinationAddr netip.Addr
	SourcePort      uint16
	DestinationPort uint16
	Sequence        uint32
	Acknowledgment  uint32
	Flags           header.TCPFlags
	ChecksumValid   bool
	ExpectedSeq     uint32
}

type wireGuardDiagnosticTransport struct {
	*http.Transport
	stack        *wireGuardStack
	bind         *wireGuardTrackingBind
	roundTripper http.RoundTripper
}

type wireGuardDiagnosticBody struct {
	io.ReadCloser
	stack  *wireGuardStack
	before wireGuardPacketStats
	bind   *wireGuardTrackingBind
	outer  wireGuardOuterPacketStats
}

func (b *wireGuardDiagnosticBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	return n, b.wrapError(err)
}

func (b *wireGuardDiagnosticBody) Close() error {
	return b.wrapError(b.ReadCloser.Close())
}

func (b *wireGuardDiagnosticBody) wrapError(err error) error {
	if foreignErr := wireGuardForeignReturnError(b.stack.packetStats(), time.Now()); foreignErr != nil {
		return foreignErr
	}
	if err == nil || errors.Is(err, io.EOF) {
		return err
	}
	return fmt.Errorf(
		"%w; %s",
		err,
		wireGuardPacketTrace(b.before, b.stack.packetStats(), b.bind, b.outer, time.Now()),
	)
}

func (t *wireGuardDiagnosticTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	before := t.stack.packetStats()
	outerBefore := wireGuardOuterPacketStats{}
	if t.bind != nil {
		outerBefore = t.bind.packetStats()
	}
	if err := wireGuardForeignReturnError(before, time.Now()); err != nil {
		return nil, err
	}
	roundTripper := t.roundTripper
	if roundTripper == nil {
		roundTripper = t.Transport
	}
	response, err := roundTripper.RoundTrip(request)
	after := t.stack.packetStats()
	if err == nil {
		if foreignErr := wireGuardForeignReturnError(after, time.Now()); foreignErr != nil {
			if response != nil && response.Body != nil {
				_ = response.Body.Close()
			}
			return nil, foreignErr
		}
		if response != nil && response.Body != nil {
			response.Body = &wireGuardDiagnosticBody{
				ReadCloser: response.Body,
				stack:      t.stack,
				before:     before,
				bind:       t.bind,
				outer:      outerBefore,
			}
		}
		return response, nil
	}
	return response, fmt.Errorf("%w; %s", err, wireGuardPacketTrace(before, after, t.bind, outerBefore, time.Now()))
}

func wireGuardPacketTrace(
	innerBefore wireGuardPacketStats,
	innerAfter wireGuardPacketStats,
	bind *wireGuardTrackingBind,
	outerBefore wireGuardOuterPacketStats,
	now time.Time,
) string {
	detail := "WireGuard inner packet trace " + wireGuardPacketStatsDelta(innerBefore, innerAfter, now)
	if bind != nil {
		detail += "; WireGuard outer UDP trace " + wireGuardOuterPacketStatsDelta(outerBefore, bind.packetStats(), now)
	}
	return detail
}

func wireGuardForeignReturnError(stats wireGuardPacketStats, now time.Time) error {
	if stats.Inbound.ForeignAddrPackets == 0 {
		return nil
	}
	return fmt.Errorf(
		"WireGuard return stream received a packet for another proxy protocol: %s",
		wireGuardPacketStatsDelta(wireGuardPacketStats{}, stats, now),
	)
}

func (s *wireGuardStack) packetStats() wireGuardPacketStats {
	s.statsLock.Lock()
	defer s.statsLock.Unlock()
	return s.stats
}

func (s *wireGuardStack) observePacket(packet []byte, outbound bool) {
	s.statsLock.Lock()
	defer s.statsLock.Unlock()
	now := time.Now()
	stats := &s.stats.Inbound
	if outbound {
		stats = &s.stats.Outbound
	}
	stats.Packets++
	stats.Bytes += uint64(len(packet))
	stats.LastPacketNanos = now.UnixNano()

	if len(packet) < 20 || packet[0]>>4 != 4 {
		return
	}
	ipHeaderBytes := int(packet[0]&0x0f) * 4
	if ipHeaderBytes < 20 || len(packet) < ipHeaderBytes {
		return
	}
	addressOffset := 16
	if outbound {
		addressOffset = 12
	}
	packetAddress, addressOK := netip.AddrFromSlice(packet[addressOffset : addressOffset+4])
	if addressOK && packetAddress == s.clientIPv4 {
		stats.LocalAddrPackets++
	} else {
		stats.ForeignAddrPackets++
	}
	if packet[9] != uint8(header.TCPProtocolNumber) || len(packet) < ipHeaderBytes+20 {
		return
	}
	totalBytes := int(binary.BigEndian.Uint16(packet[2:4]))
	if totalBytes == 0 || len(packet) < totalBytes {
		totalBytes = len(packet)
	}
	tcpHeaderBytes := int(packet[ipHeaderBytes+12]>>4) * 4
	if tcpHeaderBytes < 20 || totalBytes < ipHeaderBytes+tcpHeaderBytes {
		return
	}
	stats.TCPPackets++
	tcpBytes := packet[ipHeaderBytes:totalBytes]
	tcpHeader := header.TCP(tcpBytes)
	payloadBytes := totalBytes - ipHeaderBytes - tcpHeaderBytes
	checksumValid := tcpHeader.IsChecksumValid(
		tcpip.AddrFrom4Slice(packet[12:16]),
		tcpip.AddrFrom4Slice(packet[16:20]),
		checksum.Checksum(tcpBytes[tcpHeaderBytes:], 0),
		uint16(payloadBytes),
	)
	if checksumValid {
		stats.TCPChecksumValid++
	} else {
		stats.TCPChecksumInvalid++
	}
	sourceAddr, _ := netip.AddrFromSlice(packet[12:16])
	destinationAddr, _ := netip.AddrFromSlice(packet[16:20])
	tcpPacket := wireGuardTCPPacket{
		SourceAddr:      sourceAddr,
		DestinationAddr: destinationAddr,
		SourcePort:      tcpHeader.SourcePort(),
		DestinationPort: tcpHeader.DestinationPort(),
		Sequence:        tcpHeader.SequenceNumber(),
		Acknowledgment:  tcpHeader.AckNumber(),
		Flags:           tcpHeader.Flags(),
		ChecksumValid:   checksumValid,
	}
	flags := tcpPacket.Flags
	if flags&header.TCPFlagSyn != 0 {
		stats.Syn++
		if flags&header.TCPFlagAck != 0 {
			stats.SynAck++
		}
	}
	if flags&header.TCPFlagFin != 0 {
		stats.Fin++
	}
	if flags&header.TCPFlagRst != 0 {
		stats.Rst++
		if !outbound && s.rstMatchesDial(tcpPacket) {
			stats.RstForDial++
			tcpPacket.ExpectedSeq = s.stats.DialFlow.ExpectedInboundSeq
			if s.stats.DialFlow.ExpectedSeqSet && tcpPacket.Sequence == s.stats.DialFlow.ExpectedInboundSeq {
				stats.RstSequenceMatch++
			} else {
				stats.RstSequenceMismatch++
			}
			stats.LastDialRst = tcpPacket
		} else {
			stats.RstOther++
			stats.LastOtherRst = tcpPacket
		}
	}
	if 0 < payloadBytes {
		stats.TCPPayloadPackets++
		stats.TCPPayloadBytes += uint64(payloadBytes)
	}
	if s.tcpPacketMatchesDial(tcpPacket, outbound) {
		s.stats.EventSequence++
		event := wireGuardTCPPacketEvent{
			EventSequence:   s.stats.EventSequence,
			Nanos:           now.UnixNano(),
			Outbound:        outbound,
			SourcePort:      tcpPacket.SourcePort,
			DestinationPort: tcpPacket.DestinationPort,
			TCPSequence:     tcpPacket.Sequence,
			Acknowledgment:  tcpPacket.Acknowledgment,
			Flags:           tcpPacket.Flags,
			PayloadBytes:    payloadBytes,
			ChecksumValid:   tcpPacket.ChecksumValid,
		}
		s.stats.RecentTCPEvents[(event.EventSequence-1)%uint64(len(s.stats.RecentTCPEvents))] = event
	}
	if outbound &&
		tcpPacket.SourceAddr == s.clientIPv4 &&
		tcpPacket.DestinationAddr == s.stats.DialAddr &&
		tcpPacket.DestinationPort == s.stats.DialPort {
		if flags&header.TCPFlagSyn != 0 && flags&header.TCPFlagAck == 0 {
			s.stats.DialFlow = wireGuardTCPFlow{SourcePort: tcpPacket.SourcePort}
		}
		if flags&header.TCPFlagAck != 0 && flags&header.TCPFlagRst == 0 &&
			tcpPacket.SourcePort == s.stats.DialFlow.SourcePort {
			s.stats.DialFlow.ExpectedInboundSeq = tcpPacket.Acknowledgment
			s.stats.DialFlow.ExpectedSeqSet = true
		}
	}
}

func (s *wireGuardStack) tcpPacketMatchesDial(packet wireGuardTCPPacket, outbound bool) bool {
	if outbound {
		return packet.SourceAddr == s.clientIPv4 &&
			packet.DestinationAddr == s.stats.DialAddr &&
			packet.DestinationPort == s.stats.DialPort
	}
	return packet.SourceAddr == s.stats.DialAddr &&
		packet.DestinationAddr == s.clientIPv4 &&
		packet.SourcePort == s.stats.DialPort
}

func (s *wireGuardStack) rstMatchesDial(packet wireGuardTCPPacket) bool {
	return packet.SourceAddr == s.stats.DialAddr &&
		packet.DestinationAddr == s.clientIPv4 &&
		packet.SourcePort == s.stats.DialPort &&
		packet.DestinationPort == s.stats.DialFlow.SourcePort
}

func subtractWireGuardDirection(before, after wireGuardPacketDirectionStats) wireGuardPacketDirectionStats {
	delta := wireGuardPacketDirectionStats{
		Packets:             after.Packets - before.Packets,
		Bytes:               after.Bytes - before.Bytes,
		LocalAddrPackets:    after.LocalAddrPackets - before.LocalAddrPackets,
		ForeignAddrPackets:  after.ForeignAddrPackets - before.ForeignAddrPackets,
		TCPPackets:          after.TCPPackets - before.TCPPackets,
		TCPPayloadPackets:   after.TCPPayloadPackets - before.TCPPayloadPackets,
		TCPPayloadBytes:     after.TCPPayloadBytes - before.TCPPayloadBytes,
		Syn:                 after.Syn - before.Syn,
		SynAck:              after.SynAck - before.SynAck,
		Fin:                 after.Fin - before.Fin,
		Rst:                 after.Rst - before.Rst,
		TCPChecksumValid:    after.TCPChecksumValid - before.TCPChecksumValid,
		TCPChecksumInvalid:  after.TCPChecksumInvalid - before.TCPChecksumInvalid,
		RstForDial:          after.RstForDial - before.RstForDial,
		RstOther:            after.RstOther - before.RstOther,
		RstSequenceMatch:    after.RstSequenceMatch - before.RstSequenceMatch,
		RstSequenceMismatch: after.RstSequenceMismatch - before.RstSequenceMismatch,
		LastPacketNanos:     after.LastPacketNanos,
	}
	if before.RstForDial != after.RstForDial {
		delta.LastDialRst = after.LastDialRst
	}
	if before.RstOther != after.RstOther {
		delta.LastOtherRst = after.LastOtherRst
	}
	return delta
}

func subtractWireGuardOuterStats(before, after wireGuardOuterPacketStats) wireGuardOuterPacketStats {
	delta := wireGuardOuterPacketStats{
		SendAttemptPackets: after.SendAttemptPackets - before.SendAttemptPackets,
		SendAttemptBytes:   after.SendAttemptBytes - before.SendAttemptBytes,
		SendPackets:        after.SendPackets - before.SendPackets,
		SendBytes:          after.SendBytes - before.SendBytes,
		SendErrors:         after.SendErrors - before.SendErrors,
		ReceivePackets:     after.ReceivePackets - before.ReceivePackets,
		ReceiveBytes:       after.ReceiveBytes - before.ReceiveBytes,
		ReceiveErrors:      after.ReceiveErrors - before.ReceiveErrors,
		LastSendNanos:      after.LastSendNanos,
		LastReceiveNanos:   after.LastReceiveNanos,
	}
	for messageType := range delta.SendMessageTypes {
		delta.SendMessageTypes[messageType] = after.SendMessageTypes[messageType] - before.SendMessageTypes[messageType]
		delta.ReceiveMessageTypes[messageType] = after.ReceiveMessageTypes[messageType] - before.ReceiveMessageTypes[messageType]
	}
	return delta
}

func wireGuardOuterPacketStatsDelta(before, after wireGuardOuterPacketStats, now time.Time) string {
	delta := subtractWireGuardOuterStats(before, after)
	lastAge := func(nanos int64) string {
		if nanos == 0 {
			return "none"
		}
		return now.Sub(time.Unix(0, nanos)).Round(time.Millisecond).String() + " ago"
	}
	return fmt.Sprintf(
		"out{attempt=%d/%dB sent=%d/%dB errors=%d types=%d/%d/%d/%d/%d(init/response/cookie/data/unknown) last=%s} in{packets=%d bytes=%d errors=%d types=%d/%d/%d/%d/%d(init/response/cookie/data/unknown) last=%s} recent=[%s]",
		delta.SendAttemptPackets,
		delta.SendAttemptBytes,
		delta.SendPackets,
		delta.SendBytes,
		delta.SendErrors,
		delta.SendMessageTypes[1],
		delta.SendMessageTypes[2],
		delta.SendMessageTypes[3],
		delta.SendMessageTypes[4],
		delta.SendMessageTypes[0],
		lastAge(delta.LastSendNanos),
		delta.ReceivePackets,
		delta.ReceiveBytes,
		delta.ReceiveErrors,
		delta.ReceiveMessageTypes[1],
		delta.ReceiveMessageTypes[2],
		delta.ReceiveMessageTypes[3],
		delta.ReceiveMessageTypes[4],
		delta.ReceiveMessageTypes[0],
		lastAge(delta.LastReceiveNanos),
		formatWireGuardOuterEvents(before, after),
	)
}

func formatWireGuardOuterEvents(before, after wireGuardOuterPacketStats) string {
	events := make([]wireGuardOuterPacketEvent, 0, len(after.RecentEvents))
	for _, event := range after.RecentEvents {
		if before.EventSequence < event.Sequence && event.Sequence <= after.EventSequence {
			events = append(events, event)
		}
	}
	sort.Slice(events, func(i int, j int) bool {
		return events[i].Sequence < events[j].Sequence
	})
	if len(events) == 0 {
		return "none"
	}
	start := time.Unix(0, events[0].Nanos)
	parts := make([]string, 0, len(events))
	for _, event := range events {
		direction := "out"
		if event.Inbound {
			direction = "in"
		}
		parts = append(parts, fmt.Sprintf(
			"+%s:%s:%s/%dB",
			time.Unix(0, event.Nanos).Sub(start).Round(time.Millisecond),
			direction,
			wireGuardOuterMessageTypeName(int(event.MessageType), event.Bytes),
			event.Bytes,
		))
	}
	return strings.Join(parts, ",")
}

func wireGuardOuterMessageTypeName(messageType int, bytes int) string {
	if messageType == 4 && bytes == 32 {
		return "keepalive"
	}
	switch messageType {
	case 1:
		return "init"
	case 2:
		return "response"
	case 3:
		return "cookie"
	case 4:
		return "data"
	default:
		return "unknown"
	}
}

func wireGuardPacketStatsDelta(before, after wireGuardPacketStats, now time.Time) string {
	format := func(direction wireGuardPacketDirectionStats) string {
		last := "none"
		if direction.LastPacketNanos != 0 {
			last = now.Sub(time.Unix(0, direction.LastPacketNanos)).Round(time.Millisecond).String() + " ago"
		}
		detail := fmt.Sprintf(
			"packets=%d bytes=%d local=%d foreign=%d tcp=%d payload=%d/%dB syn=%d synack=%d fin=%d rst=%d checksum=%d/%d(valid/invalid) rst_dial=%d rst_other=%d rst_seq=%d/%d(match/mismatch) last=%s",
			direction.Packets,
			direction.Bytes,
			direction.LocalAddrPackets,
			direction.ForeignAddrPackets,
			direction.TCPPackets,
			direction.TCPPayloadPackets,
			direction.TCPPayloadBytes,
			direction.Syn,
			direction.SynAck,
			direction.Fin,
			direction.Rst,
			direction.TCPChecksumValid,
			direction.TCPChecksumInvalid,
			direction.RstForDial,
			direction.RstOther,
			direction.RstSequenceMatch,
			direction.RstSequenceMismatch,
			last,
		)
		if direction.RstForDial != 0 {
			detail += " last_dial_rst={" + formatWireGuardTCPPacket(direction.LastDialRst, true) + "}"
		}
		if direction.RstOther != 0 {
			detail += " last_other_rst={" + formatWireGuardTCPPacket(direction.LastOtherRst, false) + "}"
		}
		return detail
	}
	detail := fmt.Sprintf(
		"target=%s out{%s} in{%s}",
		netip.AddrPortFrom(after.DialAddr, after.DialPort),
		format(subtractWireGuardDirection(before.Outbound, after.Outbound)),
		format(subtractWireGuardDirection(before.Inbound, after.Inbound)),
	)
	if recent := formatWireGuardTCPEvents(before, after); recent != "none" {
		detail += " tcp_recent=[" + recent + "]"
	}
	return detail
}

func formatWireGuardTCPEvents(before, after wireGuardPacketStats) string {
	events := make([]wireGuardTCPPacketEvent, 0, len(after.RecentTCPEvents))
	for _, event := range after.RecentTCPEvents {
		if before.EventSequence < event.EventSequence && event.EventSequence <= after.EventSequence {
			events = append(events, event)
		}
	}
	sort.Slice(events, func(i int, j int) bool {
		return events[i].EventSequence < events[j].EventSequence
	})
	if len(events) == 0 {
		return "none"
	}
	start := time.Unix(0, events[0].Nanos)
	parts := make([]string, 0, len(events))
	for _, event := range events {
		direction := "out"
		if !event.Outbound {
			direction = "in"
		}
		parts = append(parts, fmt.Sprintf(
			"+%s:%s:%d->%d seq=%d ack=%d flags=%s payload=%dB checksum=%t",
			time.Unix(0, event.Nanos).Sub(start).Round(time.Millisecond),
			direction,
			event.SourcePort,
			event.DestinationPort,
			event.TCPSequence,
			event.Acknowledgment,
			wireGuardTCPFlags(event.Flags),
			event.PayloadBytes,
			event.ChecksumValid,
		))
	}
	return strings.Join(parts, ",")
}

func formatWireGuardTCPPacket(packet wireGuardTCPPacket, includeExpected bool) string {
	detail := fmt.Sprintf(
		"%s->%s seq=%d ack=%d flags=%s checksum=%t",
		netip.AddrPortFrom(packet.SourceAddr, packet.SourcePort),
		netip.AddrPortFrom(packet.DestinationAddr, packet.DestinationPort),
		packet.Sequence,
		packet.Acknowledgment,
		wireGuardTCPFlags(packet.Flags),
		packet.ChecksumValid,
	)
	if includeExpected {
		detail += fmt.Sprintf(" expected_seq=%d", packet.ExpectedSeq)
	}
	return detail
}

func wireGuardTCPFlags(flags header.TCPFlags) string {
	values := make([]string, 0, 5)
	for _, value := range []struct {
		flag header.TCPFlags
		name string
	}{
		{header.TCPFlagSyn, "SYN"},
		{header.TCPFlagAck, "ACK"},
		{header.TCPFlagPsh, "PSH"},
		{header.TCPFlagFin, "FIN"},
		{header.TCPFlagRst, "RST"},
	} {
		if flags&value.flag != 0 {
			values = append(values, value.name)
		}
	}
	if len(values) == 0 {
		return "none"
	}
	return strings.Join(values, "+")
}

func newWireGuardStack(ctx context.Context, clientIPv4 netip.Addr, mtu int, tunDevice *tuntest.ChannelTUN) (*wireGuardStack, error) {
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

	nicID := tcpip.NICID(1)
	endpoint := channel.New(512, uint32(mtu), "")
	if tcpipErr := s.CreateNIC(nicID, endpoint); tcpipErr != nil {
		s.Close()
		return nil, fmt.Errorf("create nic: %v", tcpipErr)
	}
	protocolAddress := tcpip.ProtocolAddress{
		Protocol:          ipv4.ProtocolNumber,
		AddressWithPrefix: tcpip.AddrFromSlice(clientIPv4.AsSlice()).WithPrefix(),
	}
	if tcpipErr := s.AddProtocolAddress(nicID, protocolAddress, stack.AddressProperties{}); tcpipErr != nil {
		s.Close()
		return nil, fmt.Errorf("add address: %v", tcpipErr)
	}
	s.AddRoute(tcpip.Route{Destination: header.IPv4EmptySubnet, NIC: nicID})
	clientStack := &wireGuardStack{stack: s, nicID: nicID, clientIPv4: clientIPv4}

	go func() {
		for {
			packet := endpoint.ReadContext(ctx)
			if packet == nil {
				return
			}
			data := append([]byte(nil), packet.ToView().AsSlice()...)
			packet.DecRef()
			clientStack.observePacket(data, true)
			select {
			case tunDevice.Outbound <- data:
			case <-ctx.Done():
				return
			}
		}
	}()

	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case data, ok := <-tunDevice.Inbound:
				if !ok {
					return
				}
				clientStack.observePacket(data, false)
				packet := stack.NewPacketBuffer(stack.PacketBufferOptions{Payload: buffer.MakeWithData(data)})
				endpoint.InjectInbound(header.IPv4ProtocolNumber, packet)
				packet.DecRef()
			}
		}
	}()

	return clientStack, nil
}

func (s *wireGuardStack) Close() {
	s.stack.Close()
}

func (s *wireGuardStack) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	if !strings.HasPrefix(network, "tcp") {
		return nil, fmt.Errorf("unsupported WireGuard dial network %q", network)
	}
	host, portText, err := net.SplitHostPort(address)
	if err != nil {
		return nil, err
	}
	port, err := strconv.Atoi(portText)
	if err != nil || port < 1 || port > 65535 {
		return nil, fmt.Errorf("invalid destination port")
	}

	ip := net.ParseIP(host)
	if ip == nil {
		ips, err := net.DefaultResolver.LookupIP(ctx, "ip4", host)
		if err != nil {
			return nil, err
		}
		if len(ips) == 0 {
			return nil, fmt.Errorf("no IPv4 address for destination")
		}
		ip = ips[0]
	}
	ipv4Address := ip.To4()
	if ipv4Address == nil {
		return nil, fmt.Errorf("destination has no IPv4 address")
	}
	fullAddress := tcpip.FullAddress{
		NIC:  s.nicID,
		Addr: tcpip.AddrFromSlice(ipv4Address),
		Port: uint16(port),
	}
	s.statsLock.Lock()
	s.stats.DialAddr, _ = netip.AddrFromSlice(ipv4Address)
	s.stats.DialPort = uint16(port)
	s.stats.DialFlow = wireGuardTCPFlow{}
	s.statsLock.Unlock()
	return gonet.DialContextTCP(ctx, s.stack, fullAddress, ipv4.ProtocolNumber)
}
