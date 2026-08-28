package acceptance

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/netip"
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
func newWireGuardTransport(ctx context.Context, proxyHost string, config *wireGuardConfig) (*http.Transport, func(), error) {
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

	transport := &http.Transport{DialContext: clientStack.DialContext}
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
	stack *stack.Stack
	nicID tcpip.NICID
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

	go func() {
		for {
			packet := endpoint.ReadContext(ctx)
			if packet == nil {
				return
			}
			data := append([]byte(nil), packet.ToView().AsSlice()...)
			packet.DecRef()
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
				packet := stack.NewPacketBuffer(stack.PacketBufferOptions{Payload: buffer.MakeWithData(data)})
				endpoint.InjectInbound(header.IPv4ProtocolNumber, packet)
				packet.DecRef()
			}
		}
	}()

	return &wireGuardStack{stack: s, nicID: nicID}, nil
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
	return gonet.DialContextTCP(ctx, s.stack, fullAddress, ipv4.ProtocolNumber)
}
