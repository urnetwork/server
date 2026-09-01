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
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/urnetwork/userwireguard/v2026/conn"
	uwgdevice "github.com/urnetwork/userwireguard/v2026/device"
	"github.com/urnetwork/userwireguard/v2026/logger"
	"github.com/urnetwork/userwireguard/v2026/tun/tuntest"
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
	clientDevice := uwgdevice.NewDevice(
		clientTUN.TUN(),
		conn.NewDefaultBind(),
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
	Outbound wireGuardPacketDirectionStats
	Inbound  wireGuardPacketDirectionStats
	DialAddr netip.Addr
	DialPort uint16
	DialFlow wireGuardTCPFlow
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
	roundTripper http.RoundTripper
}

type wireGuardDiagnosticBody struct {
	io.ReadCloser
	stack  *wireGuardStack
	before wireGuardPacketStats
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
		"WireGuard inner packet trace %s: %w",
		wireGuardPacketStatsDelta(b.before, b.stack.packetStats(), time.Now()),
		err,
	)
}

func (t *wireGuardDiagnosticTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	before := t.stack.packetStats()
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
			}
		}
		return response, nil
	}
	return response, fmt.Errorf("WireGuard inner packet trace %s: %w", wireGuardPacketStatsDelta(before, after, time.Now()), err)
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
	stats := &s.stats.Inbound
	if outbound {
		stats = &s.stats.Outbound
	}
	stats.Packets++
	stats.Bytes += uint64(len(packet))
	stats.LastPacketNanos = time.Now().UnixNano()

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

func wireGuardPacketStatsDelta(before, after wireGuardPacketStats, now time.Time) string {
	format := func(direction wireGuardPacketDirectionStats) string {
		last := "none"
		if direction.Packets != 0 && direction.LastPacketNanos != 0 {
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
	return fmt.Sprintf(
		"target=%s out{%s} in{%s}",
		netip.AddrPortFrom(after.DialAddr, after.DialPort),
		format(subtractWireGuardDirection(before.Outbound, after.Outbound)),
		format(subtractWireGuardDirection(before.Inbound, after.Inbound)),
	)
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
