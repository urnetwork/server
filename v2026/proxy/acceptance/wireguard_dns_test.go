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
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/urnetwork/userwireguard/v2026/conn"
	uwgdevice "github.com/urnetwork/userwireguard/v2026/device"
	"github.com/urnetwork/userwireguard/v2026/logger"
	"github.com/urnetwork/userwireguard/v2026/tun/tuntest"
	"golang.org/x/net/dns/dnsmessage"
	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"
	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/adapters/gonet"
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv4"
)

// This is an encrypted loopback WireGuard pair, not a mock DialContext. The
// profile's DNS address and origin exist only inside its isolated netstack.
// The host resolver returns NXDOMAIN, reproducing round-04's no-such-host
// boundary; a healthy tunnel must not depend on it after endpoint bootstrap.
func TestWireGuardUsesProfileDNSWhenHostLookupFails(t *testing.T) {
	hostLookups := failAcceptanceHostDNS(t)
	transport, dnsUDP, _, originRequests := acceptanceDNSWireGuardPair(t, false, false)
	client := &http.Client{Transport: transport, Timeout: 5 * time.Second}
	for _, target := range []string{"1.1.1.1", "acceptance-dns.invalid"} {
		request, err := http.NewRequest(http.MethodGet, "http://"+target+"/", nil)
		if err != nil {
			t.Fatal(err)
		}
		request.Close = true
		response, err := client.Do(request)
		if err != nil {
			t.Fatalf("healthy tunnel request to %s failed: %v (host DNS calls=%d, tunnel DNS calls=%d, origin requests=%d)", target, err, hostLookups.Load(), dnsUDP.Load(), originRequests.Load())
		}
		response.Body.Close()
		if response.StatusCode != http.StatusNoContent {
			t.Fatalf("status = %d", response.StatusCode)
		}
	}
	if hostLookups.Load() != 0 || dnsUDP.Load() == 0 || originRequests.Load() != 2 {
		t.Fatalf("host DNS=%d, tunnel DNS=%d, origin requests=%d", hostLookups.Load(), dnsUDP.Load(), originRequests.Load())
	}
}

func TestWireGuardProfileDNSFallsBackToTCPInsideTunnel(t *testing.T) {
	hostLookups := failAcceptanceHostDNS(t)
	transport, dnsUDP, dnsTCP, originRequests := acceptanceDNSWireGuardPair(t, true, false)
	client := &http.Client{Transport: transport, Timeout: 5 * time.Second}
	response, err := client.Get("http://acceptance-dns.invalid/")
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusNoContent || hostLookups.Load() != 0 || dnsUDP.Load() == 0 || dnsTCP.Load() == 0 || originRequests.Load() != 1 {
		t.Fatalf("status=%d host DNS=%d tunnel UDP/TCP=%d/%d origin=%d", response.StatusCode, hostLookups.Load(), dnsUDP.Load(), dnsTCP.Load(), originRequests.Load())
	}
}

func TestWireGuardTunnelDNSFailureDoesNotFallBackToHost(t *testing.T) {
	hostLookups := failAcceptanceHostDNS(t)
	transport, dnsUDP, _, originRequests := acceptanceDNSWireGuardPair(t, false, true)
	client := &http.Client{Transport: transport, Timeout: 5 * time.Second}
	response, err := client.Get("http://acceptance-dns.invalid/")
	if response != nil {
		response.Body.Close()
	}
	var dnsError *net.DNSError
	if !errors.As(err, &dnsError) || !dnsError.IsNotFound || dnsError.Server != "1.1.1.1:53" {
		t.Fatalf("expected tunneled NXDOMAIN attributed to profile DNS, got %v", err)
	}
	if hostLookups.Load() != 0 || dnsUDP.Load() == 0 || originRequests.Load() != 0 {
		t.Fatalf("host DNS=%d tunnel DNS=%d origin=%d", hostLookups.Load(), dnsUDP.Load(), originRequests.Load())
	}
}

func TestWireGuardDNSCancellationClosesBlockedPacketRead(t *testing.T) {
	transport, _, _, _ := acceptanceDNSWireGuardPair(t, false, false)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	connection, err := transport.stack.dialDNSContext(ctx, "udp", "host-resolver.invalid:53")
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	if _, ok := connection.(net.PacketConn); !ok {
		t.Fatal("UDP DNS connection lost packet framing")
	}
	finished := make(chan error, 1)
	go func() {
		_, err := connection.Read(make([]byte, 512))
		finished <- err
	}()
	cancel()
	select {
	case err := <-finished:
		if err == nil {
			t.Fatal("canceled read unexpectedly succeeded")
		}
	case <-time.After(time.Second):
		t.Fatal("cancellation left DNS read blocked")
	}
	if _, err := transport.stack.dialDNSContext(ctx, "udp", "ignored:53"); !errors.Is(err, context.Canceled) {
		t.Fatalf("pre-canceled DNS dial = %v", err)
	}
	if _, err := transport.stack.dialDNSContext(context.Background(), "udp6", "ignored:53"); err == nil {
		t.Fatal("unexpectedly accepted unsupported DNS network")
	}
}

func TestWireGuardProfileDNSParsingFailsClosedWithoutLeakingKeys(t *testing.T) {
	for _, test := range []struct {
		name, profile, want string
	}{
		{"supplied", "[Interface]\nDNS = 1.1.1.1\n[Peer]\n", "1.1.1.1"},
		{"list", "[Interface]\nDNS = 2606:4700:4700::1111, 9.9.9.9, 1.1.1.1\n", "9.9.9.9"},
		{"comments", " [Interface] # interface\r\n DNS = 9.9.9.9 # resolver\r\n", "9.9.9.9"},
		{"missing", "[Interface]\nAddress = 10.0.0.2/32\n", ""},
		{"wrong-section", "[Peer]\nDNS = 1.1.1.1\n", ""},
		{"hostname", "[Interface]\nDNS = resolver.invalid\n", ""},
		{"ipv6-only", "[Interface]\nDNS = 2606:4700:4700::1111\n", ""},
		{"unspecified", "[Interface]\nDNS = 0.0.0.0\n", ""},
		{"multicast", "[Interface]\nDNS = 224.0.0.1\n", ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			address, err := wireGuardProfileDNS(test.profile + "PrivateKey = never-print-this-key\n")
			if test.want == "" {
				if err == nil || strings.Contains(err.Error(), "never-print-this-key") {
					t.Fatalf("invalid DNS profile did not fail privately: %v", err)
				}
			} else if err != nil || address.String() != test.want {
				t.Fatalf("DNS = %s, %v; want %s", address, err, test.want)
			}
		})
	}
	var stack wireGuardStack
	if _, err := stack.DialContext(context.Background(), "tcp", "acceptance-dns.invalid:80"); err == nil || !strings.Contains(err.Error(), "DNS is not configured") {
		t.Fatalf("unconfigured tunnel unexpectedly fell back to host DNS: %v", err)
	}
}

func failAcceptanceHostDNS(t *testing.T) *atomic.Int64 {
	t.Helper()
	calls := new(atomic.Int64)
	original := net.DefaultResolver
	net.DefaultResolver = &net.Resolver{
		PreferGo: true,
		Dial: func(context.Context, string, string) (net.Conn, error) {
			calls.Add(1)
			client, server := net.Pipe()
			go func() {
				defer server.Close()
				_ = server.SetDeadline(time.Now().Add(5 * time.Second))
				query, err := acceptanceReadDNSStream(server)
				if err != nil {
					return
				}
				answer, err := acceptanceDNSAnswer(query, true, false)
				if err == nil {
					_ = acceptanceWriteDNSStream(server, answer)
				}
			}()
			return client, nil
		},
	}
	t.Cleanup(func() { net.DefaultResolver = original })
	return calls
}

func acceptanceDNSWireGuardPair(t *testing.T, truncateUDP, notFound bool) (*wireGuardDiagnosticTransport, *atomic.Int64, *atomic.Int64, *atomic.Int64) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	serverTUN := tuntest.NewChannelTUN()
	serverDevice := uwgdevice.NewDevice(serverTUN.TUN(), conn.NewDefaultBind(), logger.NewLogger(logger.LogLevelSilent, "acceptance-test: "))
	serverStack, err := newWireGuardStack(ctx, netip.MustParseAddr("1.1.1.1"), 1420, serverTUN)
	if err != nil {
		cancel()
		serverDevice.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cancel()
		serverStack.Close()
		serverDevice.Close()
	})
	clientKey, err := wgtypes.GeneratePrivateKey()
	if err != nil {
		t.Fatal(err)
	}
	serverKey, err := wgtypes.GeneratePrivateKey()
	if err != nil {
		t.Fatal(err)
	}
	zero := 0
	if err := serverDevice.IpcSet(&wgtypes.Config{
		PrivateKey: &serverKey, ListenPort: &zero, ReplacePeers: true,
		Peers: []wgtypes.PeerConfig{{PublicKey: clientKey.PublicKey(), ReplaceAllowedIPs: true,
			AllowedIPs: []net.IPNet{{IP: net.IPv4(10, 0, 0, 2).To4(), Mask: net.CIDRMask(32, 32)}}}},
	}); err != nil {
		t.Fatal(err)
	}
	if err := serverDevice.Up(); err != nil {
		t.Fatal(err)
	}
	serverConfig, err := serverDevice.IpcGet()
	if err != nil {
		t.Fatal(err)
	}
	dnsAddress := tcpip.FullAddress{NIC: serverStack.nicID, Addr: tcpip.AddrFrom4([4]byte{1, 1, 1, 1}), Port: 53}
	udpDNS, err := gonet.DialUDP(serverStack.stack, &dnsAddress, nil, ipv4.ProtocolNumber)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { udpDNS.Close() })
	udpCalls := new(atomic.Int64)
	go func() {
		buffer := make([]byte, 4096)
		for {
			n, peer, err := udpDNS.ReadFrom(buffer)
			if err != nil {
				return
			}
			udpCalls.Add(1)
			answer, err := acceptanceDNSAnswer(buffer[:n], notFound, truncateUDP)
			if err == nil {
				_, _ = udpDNS.WriteTo(answer, peer)
			}
		}
	}()
	tcpDNS, err := gonet.ListenTCP(serverStack.stack, dnsAddress, ipv4.ProtocolNumber)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { tcpDNS.Close() })
	tcpCalls := new(atomic.Int64)
	go func() {
		for {
			connection, err := tcpDNS.Accept()
			if err != nil {
				return
			}
			tcpCalls.Add(1)
			go func() {
				defer connection.Close()
				_ = connection.SetDeadline(time.Now().Add(5 * time.Second))
				query, err := acceptanceReadDNSStream(connection)
				if err != nil {
					return
				}
				answer, err := acceptanceDNSAnswer(query, notFound, false)
				if err == nil {
					_ = acceptanceWriteDNSStream(connection, answer)
				}
			}()
		}
	}()
	originAddress := dnsAddress
	originAddress.Port = 80
	originListener, err := gonet.ListenTCP(serverStack.stack, originAddress, ipv4.ProtocolNumber)
	if err != nil {
		t.Fatal(err)
	}
	originCalls := new(atomic.Int64)
	origin := &http.Server{Handler: http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		originCalls.Add(1)
		response.WriteHeader(http.StatusNoContent)
	})}
	go origin.Serve(originListener)
	t.Cleanup(func() { origin.Close() })
	transport, closeTransport, err := newWireGuardTransport(ctx, "127.0.0.1", &wireGuardConfig{
		ProxyPort: serverConfig.ListenPort, ClientPrivateKey: clientKey.String(), ProxyPublicKey: serverKey.PublicKey().String(),
		ClientIPv4: "10.0.0.2", Config: "[Interface]\nDNS = 1.1.1.1\n[Peer]\nAllowedIPs = 0.0.0.0/0\n",
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(closeTransport)
	return transport, udpCalls, tcpCalls, originCalls
}

func acceptanceDNSAnswer(query []byte, notFound, truncated bool) ([]byte, error) {
	var message dnsmessage.Message
	if err := message.Unpack(query); err != nil {
		return nil, err
	}
	message.Header = dnsmessage.Header{ID: message.ID, Response: true, RecursionDesired: true, RecursionAvailable: true, Truncated: truncated}
	message.Answers = nil
	message.Additionals = nil
	if notFound {
		message.RCode = dnsmessage.RCodeNameError
	} else if !truncated {
		for _, question := range message.Questions {
			if question.Type == dnsmessage.TypeA {
				message.Answers = append(message.Answers, dnsmessage.Resource{
					Header: dnsmessage.ResourceHeader{Name: question.Name, Type: dnsmessage.TypeA, Class: dnsmessage.ClassINET, TTL: 60},
					Body:   &dnsmessage.AResource{A: [4]byte{1, 1, 1, 1}},
				})
			}
		}
	}
	return message.Pack()
}

func acceptanceReadDNSStream(connection net.Conn) ([]byte, error) {
	var size [2]byte
	if _, err := io.ReadFull(connection, size[:]); err != nil {
		return nil, err
	}
	message := make([]byte, int(binary.BigEndian.Uint16(size[:])))
	_, err := io.ReadFull(connection, message)
	return message, err
}

func acceptanceWriteDNSStream(connection net.Conn, message []byte) error {
	if len(message) > 65535 {
		return fmt.Errorf("oversized DNS test message")
	}
	frame := make([]byte, 2+len(message))
	binary.BigEndian.PutUint16(frame, uint16(len(message)))
	copy(frame[2:], message)
	_, err := connection.Write(frame)
	return err
}
