package acceptance

import (
	"context"
	"encoding/binary"
	"fmt"
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
	Packets            uint64
	Bytes              uint64
	LocalAddrPackets   uint64
	ForeignAddrPackets uint64
	TCPPackets         uint64
	TCPPayloadPackets  uint64
	TCPPayloadBytes    uint64
	Syn                uint64
	SynAck             uint64
	Fin                uint64
	Rst                uint64
	LastPacketNanos    int64
}

type wireGuardPacketStats struct {
	Outbound wireGuardPacketDirectionStats
	Inbound  wireGuardPacketDirectionStats
	DialAddr netip.Addr
}

type wireGuardDiagnosticTransport struct {
	*http.Transport
	stack        *wireGuardStack
	roundTripper http.RoundTripper
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
	flags := header.TCPFlags(packet[ipHeaderBytes+13])
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
	}
	payloadBytes := totalBytes - ipHeaderBytes - tcpHeaderBytes
	if 0 < payloadBytes {
		stats.TCPPayloadPackets++
		stats.TCPPayloadBytes += uint64(payloadBytes)
	}
}

func subtractWireGuardDirection(before, after wireGuardPacketDirectionStats) wireGuardPacketDirectionStats {
	return wireGuardPacketDirectionStats{
		Packets:            after.Packets - before.Packets,
		Bytes:              after.Bytes - before.Bytes,
		LocalAddrPackets:   after.LocalAddrPackets - before.LocalAddrPackets,
		ForeignAddrPackets: after.ForeignAddrPackets - before.ForeignAddrPackets,
		TCPPackets:         after.TCPPackets - before.TCPPackets,
		TCPPayloadPackets:  after.TCPPayloadPackets - before.TCPPayloadPackets,
		TCPPayloadBytes:    after.TCPPayloadBytes - before.TCPPayloadBytes,
		Syn:                after.Syn - before.Syn,
		SynAck:             after.SynAck - before.SynAck,
		Fin:                after.Fin - before.Fin,
		Rst:                after.Rst - before.Rst,
		LastPacketNanos:    after.LastPacketNanos,
	}
}

func wireGuardPacketStatsDelta(before, after wireGuardPacketStats, now time.Time) string {
	format := func(direction wireGuardPacketDirectionStats) string {
		last := "none"
		if direction.Packets != 0 && direction.LastPacketNanos != 0 {
			last = now.Sub(time.Unix(0, direction.LastPacketNanos)).Round(time.Millisecond).String() + " ago"
		}
		return fmt.Sprintf(
			"packets=%d bytes=%d local=%d foreign=%d tcp=%d payload=%d/%dB syn=%d synack=%d fin=%d rst=%d last=%s",
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
			last,
		)
	}
	return fmt.Sprintf(
		"target=%s out{%s} in{%s}",
		after.DialAddr,
		format(subtractWireGuardDirection(before.Outbound, after.Outbound)),
		format(subtractWireGuardDirection(before.Inbound, after.Inbound)),
	)
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
	s.statsLock.Unlock()
	return gonet.DialContextTCP(ctx, s.stack, fullAddress, ipv4.ProtocolNumber)
}
