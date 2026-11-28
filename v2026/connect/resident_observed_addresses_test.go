package connect

// The client's observed addresses (resident_observed_addresses.go): the edge
// resolves the address, the exchange header carries it to the resident in
// any process and across a rolling deploy, the resident sends the client the
// set of its transports' addresses on every change, and the client's
// provider then refuses consumer packets to them at its real entry point.

import (
	"bytes"
	"context"
	"encoding/gob"
	"net"
	"net/netip"
	"slices"
	"sync"
	"syscall"
	"testing"
	"testing/synctest"
	"time"

	clientconnect "github.com/urnetwork/connect/v2026"
	"github.com/urnetwork/connect/v2026/protocol"

	"github.com/urnetwork/server/v2026"
)

// The exchange header before it carried the client address, as a peer that
// predates the field encodes and decodes it.
type exchangeHeaderBeforeClientIp struct {
	Version                               int
	ClientId                              server.Id
	ResidentId                            server.Id
	Op                                    ExchangeOp
	UnreliableTransfer                    bool
	UnreliableTransferMaxMessageByteCount int
	UnreliableFlowIsolation               bool
	UnreliableFlowReserve                 bool
	ResidentMoveVersion                   uint32
	Carrier                               ResidentCarrier
	IpFamily                              uint8
	ProvideIntent                         bool
}

func gobRoundTrip(t *testing.T, from any, to any) {
	t.Helper()
	var encoded bytes.Buffer
	if err := gob.NewEncoder(&encoded).Encode(from); err != nil {
		t.Fatal(err)
	}
	if err := gob.NewDecoder(&encoded).Decode(to); err != nil {
		t.Fatal(err)
	}
}

// The edge resolves the client address from the ingress-resolved ip:port, the
// header carries it to the resident, and a rolling deploy decodes in both
// directions: a peer that predates the field sends no address, and ignores
// the field it does not know.
func TestResidentTransportEdgeCarriesClientIp(t *testing.T) {
	for _, c := range []struct {
		clientAddress string
		want          netip.Addr
	}{
		{"203.0.113.50:40000", netip.MustParseAddr("203.0.113.50")},
		{"[2a01:4f8:c17:1234::10]:443", netip.MustParseAddr("2a01:4f8:c17:1234::10")},
		{"[::ffff:203.0.113.50]:40000", netip.MustParseAddr("203.0.113.50")},
		{"not an address", netip.Addr{}},
		{"", netip.Addr{}},
	} {
		if got := residentTransportClientIp(c.clientAddress); got != c.want {
			t.Errorf("residentTransportClientIp(%q) = %v, want %v", c.clientAddress, got, c.want)
		}
	}

	exchange := &Exchange{settings: DefaultExchangeSettings()}
	for _, clientIp := range []netip.Addr{netip.MustParseAddr("203.0.113.50"), netip.MustParseAddr("2a01:4f8:c17:1234::10"), {}} {
		edge := ResidentTransportEdge{
			ResidentMoveVersion: 2,
			Carrier:             ResidentCarrierH1,
			IpFamily:            4,
			ProvideIntent:       true,
			ClientIp:            clientIp,
		}
		transport := NewResidentTransportWithEdge(
			context.Background(),
			exchange,
			server.NewId(),
			server.NewId(),
			clientconnect.TransferCarrierProperties{},
			ResidentTransportAdmission{},
			edge,
		)
		var header ExchangeHeader
		gobRoundTrip(t, &transport.header, &header)
		if got := header.transportEdge(); got != edge {
			t.Errorf("edge through the header = %+v, want %+v", got, edge)
		}
	}

	// a peer that predates the field sends no address
	var fromOld ExchangeHeader
	gobRoundTrip(t, &exchangeHeaderBeforeClientIp{Op: ExchangeOpTransport, IpFamily: 6, ProvideIntent: true}, &fromOld)
	if edge := fromOld.transportEdge(); edge.ClientIp.IsValid() || edge.IpFamily != 6 || !edge.ProvideIntent {
		t.Errorf("edge from an older peer = %+v, want its fields and no address", edge)
	}
	// and ignores the field
	var toOld exchangeHeaderBeforeClientIp
	gobRoundTrip(t, &ExchangeHeader{Op: ExchangeOpTransport, IpFamily: 4, ClientIp: netip.MustParseAddr("203.0.113.50")}, &toOld)
	if toOld.IpFamily != 4 || toOld.Op != ExchangeOpTransport {
		t.Errorf("header at an older peer = %+v", toOld)
	}
	// a mapped address arrives as its ipv4 address
	if edge := (ExchangeHeader{ClientIp: netip.MustParseAddr("::ffff:203.0.113.50")}).transportEdge(); edge.ClientIp != netip.MustParseAddr("203.0.113.50") {
		t.Errorf("a mapped address read as %v", edge.ClientIp)
	}
}

// observedAddressesFixture is a resident with the observed addresses listener
// running and every ObservedAddresses it sends captured before the send.
type observedAddressesFixture struct {
	resident *Resident
	listener *residentObservedAddressesListener

	stateLock sync.Mutex
	sent      [][]netip.Addr
}

func newObservedAddressesFixture(t testing.TB, ctx context.Context, settings *ExchangeSettings) *observedAddressesFixture {
	f := &observedAddressesFixture{}
	f.resident = newResidentCallbackLifecycleFixture(t, ctx, settings)
	f.resident.observedAddressesKick = make(chan struct{}, 1)
	f.resident.beforeListenerFrameSendForTest = func(frame *protocol.Frame) {
		if frame.MessageType != protocol.MessageType_TransferObservedAddresses {
			return
		}
		message, err := clientconnect.FromFrame(frame)
		if err != nil {
			t.Error(err)
			return
		}
		addresses := []netip.Addr{}
		for _, addressBytes := range message.(*protocol.ObservedAddresses).Addresses {
			addr, ok := netip.AddrFromSlice(addressBytes)
			if !ok {
				t.Errorf("sent a malformed address %x", addressBytes)
			}
			addresses = append(addresses, addr)
		}
		f.stateLock.Lock()
		defer f.stateLock.Unlock()
		f.sent = append(f.sent, addresses)
	}
	f.listener = newResidentObservedAddressesListener(
		f.resident.ctx,
		f.resident.observedAddresses,
		f.resident.sendListenerFrame,
		f.resident.observedAddressesKick,
	)
	return f
}

// attach adds a transport whose edge resolved clientIp (invalid for an edge
// that resolved none) and returns its close.
func (self *observedAddressesFixture) attach(t testing.TB, clientIp netip.Addr) func() {
	t.Helper()
	_, _, closeTransport, err := self.resident.addTransportWithEdge(
		clientconnect.TransferCarrierProperties{},
		ResidentTransportEdge{Carrier: ResidentCarrierH1, ClientIp: clientIp},
	)
	if err != nil {
		t.Fatal(err)
	}
	return closeTransport
}

// takeSent returns and clears what the listener sent.
func (self *observedAddressesFixture) takeSent() [][]netip.Addr {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	sent := self.sent
	self.sent = nil
	return sent
}

func (self *observedAddressesFixture) close() {
	self.listener.CloseAndWait()
	self.resident.CloseAndWait(context.Background())
}

// The resident sends the set of its transports' addresses once per change:
// on the first transport, on a new address, on a closed last transport of an
// address; not on a repeat or on a transport whose edge resolved none (an
// edge that predates the field), and not when the set is empty, which is
// sent again once a transport returns. synctest.Wait settles the listener, so
// an empty capture means nothing was sent.
func TestResidentSendsObservedAddressesOnChange(t *testing.T) {
	// the process-owned message pool worker starts outside the bubble
	warmLocalTransportPool()
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		f := newObservedAddressesFixture(t, ctx, DefaultExchangeSettings())
		defer f.close()

		v4 := netip.MustParseAddr("203.0.113.50")
		v6 := netip.MustParseAddr("2a01:4f8:c17:1234::10")
		expect := func(step string, want ...[]netip.Addr) {
			t.Helper()
			synctest.Wait()
			sent := f.takeSent()
			if len(sent) != len(want) {
				t.Fatalf("%s: sent %v, want %v", step, sent, want)
			}
			for i := range sent {
				if !slices.Equal(sent[i], want[i]) {
					t.Fatalf("%s: sent %v, want %v", step, sent, want)
				}
			}
		}

		closeFirst := f.attach(t, v4)
		expect("the first transport", []netip.Addr{v4})
		closeSecond := f.attach(t, v4)
		expect("a second transport from the same address")
		closeThird := f.attach(t, v6)
		expect("a transport from a new address", []netip.Addr{v4, v6})
		closeOld := f.attach(t, netip.Addr{})
		expect("a transport whose edge resolved no address")
		closeFirst()
		expect("closing one of two transports from an address")
		closeSecond()
		expect("closing the last transport from an address", []netip.Addr{v6})
		closeThird()
		closeOld()
		expect("closing every transport")
		closeAgain := f.attach(t, v4)
		expect("a transport after the set was empty", []netip.Addr{v4})
		closeAgain()
	})
}

// One frame carries at most ObservedAddressesMaxCount addresses, the lowest
// in address order.
func TestResidentObservedAddressesAreBounded(t *testing.T) {
	// the process-owned message pool worker starts outside the bubble
	warmLocalTransportPool()
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		settings := DefaultExchangeSettings()
		if settings.ObservedAddressesMaxCount != 16 {
			t.Fatalf("default ObservedAddressesMaxCount = %d, want 16", settings.ObservedAddressesMaxCount)
		}
		f := newObservedAddressesFixture(t, ctx, settings)
		defer f.close()

		closes := []func(){}
		for i := 20; 0 < i; i-- {
			closes = append(closes, f.attach(t, netip.AddrFrom4([4]byte{198, 51, 100, byte(i)})))
		}
		synctest.Wait()
		sent := f.takeSent()
		if len(sent) == 0 {
			t.Fatal("nothing sent")
		}
		last := sent[len(sent)-1]
		if len(last) != 16 || last[0] != netip.MustParseAddr("198.51.100.1") || last[15] != netip.MustParseAddr("198.51.100.16") {
			t.Fatalf("last set = %v, want 198.51.100.1 to .16", last)
		}
		for _, closeTransport := range closes {
			closeTransport()
		}
	})
}

// End to end without a database: the resident sends a device its transport's
// public address over the device's real control path, and the device's
// provider, on the default provider policy, then refuses a consumer packet to
// that address at RemoteUserNatProvider.ClientReceive, while another public
// destination still passes. The process snapshot is the connect library's;
// the test restores its observed addresses.
func TestObservedAddressReachesProviderGuard(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	t.Cleanup(func() {
		clientconnect.SetProviderObservedAddresses(nil)
	})

	f := newObservedAddressesFixture(t, ctx, DefaultExchangeSettings())
	defer f.close()
	// as the resident's constructor does: control frames to its client need no
	// contract
	f.resident.client.ContractManager().AddNoContractPeer(clientconnect.Id(f.resident.clientId))

	deviceSettings := clientconnect.DefaultClientSettings()
	deviceSettings.ControlPingTimeout = 0
	deviceSettings.EncryptionSettings.Mode = clientconnect.EncryptionModeOff
	deviceSettings.Log = clientconnect.NewNoopLogger()
	device := clientconnect.NewClient(ctx, clientconnect.Id(f.resident.clientId), clientconnect.NewNoContractClientOob(), deviceSettings)
	defer device.Cancel()
	observed := make(chan struct{}, 1)
	// registered after the client's own handler, so it runs after the
	// addresses are applied
	device.AddReceiveCallback(func(source clientconnect.TransferPath, frames []*protocol.Frame, peer clientconnect.Peer) {
		for _, frame := range frames {
			if source.IsControlSource() && frame.MessageType == protocol.MessageType_TransferObservedAddresses {
				select {
				case observed <- struct{}{}:
				default:
				}
			}
		}
	})
	// the exit refuses every dial, so nothing leaves this host
	natSettings := clientconnect.DefaultLocalUserNatSettings()
	refuse := &clientconnect.DialContextSettings{
		DialContext: func(ctx context.Context, network string, address string) (net.Conn, error) {
			return nil, &net.OpError{Op: "dial", Net: network, Err: syscall.ECONNREFUSED}
		},
	}
	natSettings.TcpBufferSettings.DialContextSettings = refuse
	natSettings.UdpBufferSettings.DialContextSettings = refuse
	nat := clientconnect.NewLocalUserNat(ctx, "observed-addresses-exit", natSettings)
	defer nat.Close()
	provider := clientconnect.NewRemoteUserNatProvider(device, nat, clientconnect.DefaultRemoteUserNatProviderSettings())
	defer provider.Close()

	consumer := clientconnect.SourceId(clientconnect.NewId())
	sourcePort := 46000
	receive := func(destination string) {
		t.Helper()
		sourcePort += 1
		packet := observedAddressesTestSyn(t, net.ParseIP("10.0.0.9"), sourcePort, net.ParseIP(destination), 8080)
		frame, err := clientconnect.ToFrame(&protocol.IpPacketToProvider{
			IpPacket: &protocol.IpPacket{PacketBytes: packet},
		}, clientconnect.DefaultProtocolVersion)
		if err != nil {
			t.Fatal(err)
		}
		provider.ClientReceive(consumer, []*protocol.Frame{frame}, clientconnect.Peer{ProvideMode: protocol.ProvideMode_Public})
	}
	providerLocalCount := func() uint64 {
		var count uint64
		for _, c := range provider.SecurityPolicyReasons(false)[clientconnect.SecurityPolicyReasonProviderLocal] {
			count += c
		}
		return count
	}

	// before the platform says anything, the router's public address is an
	// ordinary public destination
	receive("203.0.113.50")
	if count := providerLocalCount(); count != 0 {
		t.Fatalf("provider-local count before the message = %d, want 0", count)
	}

	send, receiveRoute, closeTransport, err := f.resident.addTransportWithEdge(
		clientconnect.TransferCarrierProperties{},
		ResidentTransportEdge{Carrier: ResidentCarrierH1, ClientIp: netip.MustParseAddr("203.0.113.50")},
	)
	if err != nil {
		t.Fatal(err)
	}
	defer closeTransport()
	device.RouteManager().UpdateTransport(clientconnect.NewReceiveGatewayTransport(), []clientconnect.Route{send})
	device.RouteManager().UpdateTransport(clientconnect.NewSendGatewayTransport(), []clientconnect.Route{receiveRoute})

	select {
	case <-observed:
	case <-ctx.Done():
		t.Fatal("the device never received its observed addresses")
	}
	receive("203.0.113.50")
	if count := providerLocalCount(); count != 1 {
		t.Fatalf("provider-local count after the message = %d, want 1", count)
	}
	receive("198.51.100.77")
	if count := providerLocalCount(); count != 1 {
		t.Fatalf("an unobserved public destination was refused: provider-local count = %d", count)
	}
}

// A tcp syn of the given endpoints, as a consumer sends it to a provider.
func observedAddressesTestSyn(t *testing.T, sourceIp net.IP, sourcePort int, destinationIp net.IP, destinationPort int) []byte {
	t.Helper()
	packet := make([]byte, 40)
	packet[0] = 0x45
	packet[2] = 0
	packet[3] = 40
	packet[8] = 64
	packet[9] = 6
	copy(packet[12:16], sourceIp.To4())
	copy(packet[16:20], destinationIp.To4())
	tcp := packet[20:]
	tcp[0], tcp[1] = byte(sourcePort>>8), byte(sourcePort)
	tcp[2], tcp[3] = byte(destinationPort>>8), byte(destinationPort)
	tcp[12] = 5 << 4
	tcp[13] = 0x02
	tcp[14], tcp[15] = 0xff, 0xff
	ipChecksum := observedAddressesTestChecksum(0, packet[:20])
	packet[10], packet[11] = byte(ipChecksum>>8), byte(ipChecksum)
	pseudo := make([]byte, 12)
	copy(pseudo[0:4], packet[12:16])
	copy(pseudo[4:8], packet[16:20])
	pseudo[9] = 6
	pseudo[10], pseudo[11] = 0, 20
	tcpChecksum := observedAddressesTestChecksum(observedAddressesTestSum(pseudo), tcp)
	tcp[16], tcp[17] = byte(tcpChecksum>>8), byte(tcpChecksum)
	return packet
}

func observedAddressesTestSum(b []byte) uint32 {
	var sum uint32
	for i := 0; i+1 < len(b); i += 2 {
		sum += uint32(b[i])<<8 | uint32(b[i+1])
	}
	if len(b)%2 == 1 {
		sum += uint32(b[len(b)-1]) << 8
	}
	return sum
}

func observedAddressesTestChecksum(initial uint32, b []byte) uint16 {
	sum := initial + observedAddressesTestSum(b)
	for sum>>16 != 0 {
		sum = sum&0xffff + sum>>16
	}
	return ^uint16(sum)
}
