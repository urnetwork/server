// The public compact-packet ABI carries real application TUN traffic. Provider
// sockets translate only four fixture documentation endpoints to owned origins.
package perfvar

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"sync"
	"sync/atomic"
	"time"

	clientconnect "github.com/urnetwork/connect"
	"github.com/urnetwork/connect/protocol"
)

// Different origin destinations may concurrently reuse a local TCP source port.
// Both endpoints identify the socket; neither is serialized in fixture reports.
type sdkBrowsingOriginTuple struct {
	sourceAddr      string
	destinationAddr string
}

// Origin mappings belong to upstream socket lifetimes, not provider identities.
// Only short map snapshots hold stateLock; API and socket I/O run outside it.
type sdkBrowsingNative struct {
	fixture               *sdkBrowsingSetupFixture
	scenario              *sdkBrowsingScenario
	path                  *fullTunPath
	stateLock             sync.Mutex
	ports                 [nativeBrowsingOriginCount]int
	originCount           int
	coldLinkOriginForTest bool
	coldLinkPortForTest   int
	upstreamConns         map[sdkBrowsingOriginTuple]*sdkBrowsingOriginConn
	bridgeDone            chan struct{}
	receiveUnsub          func()
	bridgeStarted         atomic.Uint64
	bridgeCompleted       atomic.Uint64
	bridgeAccepted        atomic.Uint64
	bridgeRefused         atomic.Uint64
	bridgeMaximum         atomic.Int64
	bridgeInvalid         atomic.Bool
	originDials           [6]atomic.Uint64
	originCloses          [6]atomic.Uint64
	// Explicit body-consumer fixtures leave encrypted socket reads unheld.
	suppressOriginReadHoldForTest atomic.Bool
}

// The constructor creates no host interface or route; CreateTun uses gVisor.
func newSdkBrowsingNative(fixture *sdkBrowsingSetupFixture, scenario *sdkBrowsingScenario, resources tunResourceProfile, profile networkProfile) (*sdkBrowsingNative, error) {
	self := &sdkBrowsingNative{fixture: fixture, scenario: scenario, upstreamConns: map[sdkBrowsingOriginTuple]*sdkBrowsingOriginConn{}, bridgeDone: make(chan struct{})}
	path := &fullTunPath{t: fixture.t, ctx: fixture.ctx, environment: fixture.environment, route: fullTunRouteExchangeH1, p2pHopCount: 1, bridgeSends: newFullTunBridgeSendTracker()}
	settings := clientconnect.DefaultTunSettingsWithBufferSize(resources.ChannelSize)
	settings.Mtu = resolvedFullTunApplicationMtu(profile, resources)
	applyFullTunApplicationDialSettings(settings, path)
	applyTunResourceProfile(settings, resources)
	appTun, err := clientconnect.CreateTun(fixture.ctx, settings)
	if err != nil {
		return nil, err
	}
	path.appTun, self.path = appTun, path
	self.receiveUnsub = fixture.device.AddReceivePacketsCallback(func(_ clientconnect.TransferPath, _ protocol.ProvideMode, _ *clientconnect.IpPath, packets [][]byte) {
		_, _ = appTun.WriteBatch(packets)
	})
	go self.run(resources)
	return self, nil
}

// The compact call copies input. Every original pooled TUN packet stays here
// until returned once; its public result is an aggregate, not a per-packet mask.
func (self *sdkBrowsingNative) run(resources tunResourceProfile) {
	defer close(self.bridgeDone)
	packets := make([][]byte, max(1, resources.BatchSize))
	returnPackets := func() {
		for index, packet := range packets {
			if packet != nil {
				clientconnect.MessagePoolReturn(packet)
				packets[index] = nil
			}
		}
	}
	defer returnPackets()
	for {
		count, err := self.path.appTun.ReadBatch(packets)
		if err != nil {
			return
		}
		if count <= 0 || len(packets) < count || 64 < count {
			self.bridgeInvalid.Store(true)
			return
		}
		byteCount, valid := 0, true
		for _, packet := range packets[:count] {
			valid = valid && 0 < len(packet) && len(packet) <= 65535
			byteCount += 2 + len(packet)
		}
		if !valid || 96*1024 < byteCount {
			self.bridgeInvalid.Store(true)
			return
		}
		compact := make([]byte, byteCount)
		offset := 0
		for _, packet := range packets[:count] {
			binary.BigEndian.PutUint16(compact[offset:offset+2], uint16(len(packet)))
			offset += 2
			copy(compact[offset:], packet)
			offset += len(packet)
		}
		self.bridgeStarted.Add(uint64(count))
		for old := self.bridgeMaximum.Load(); old < int64(count) && !self.bridgeMaximum.CompareAndSwap(old, int64(count)); old = self.bridgeMaximum.Load() {
		}
		self.path.bridgeSends.batchCountByPacketCount[count].Add(1)
		if resources.AppDelay > 0 {
			start := time.Now()
			time.Sleep(resources.AppDelay)
			self.path.bridgeSends.delayDurationNanoseconds.Add(uint64(time.Since(start)))
		}
		start := time.Now()
		accepted := self.fixture.device.SendPacketBatch(compact)
		self.path.bridgeSends.sendDurationNanoseconds.Add(uint64(time.Since(start)))
		if accepted < 0 || int32(count) < accepted {
			self.bridgeInvalid.Store(true)
			return
		}
		self.bridgeAccepted.Add(uint64(accepted))
		self.bridgeRefused.Add(uint64(int32(count) - accepted))
		returnPackets()
		self.bridgeCompleted.Add(uint64(count))
	}
}

// The listener is local; the application still addresses its documentation IP.
func (self *sdkBrowsingNative) addOrigin(listener net.Listener) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if self.originCount < len(self.ports) {
		self.ports[self.originCount] = listener.Addr().(*net.TCPAddr).Port
		self.originCount++
	} else if self.coldLinkOriginForTest && self.originCount == nativeBrowsingOriginCount {
		self.coldLinkPortForTest = listener.Addr().(*net.TCPAddr).Port
		self.originCount++
	}
}

// Fixture exception scope disappears with its servers, independently of sockets.
func (self *sdkBrowsingNative) clearOrigins() {
	self.stateLock.Lock()
	self.ports, self.originCount = [nativeBrowsingOriginCount]int{}, 0
	self.coldLinkPortForTest, self.coldLinkOriginForTest = 0, false
	self.stateLock.Unlock()
}

// Both security directions admit only a currently owned TCP origin tuple.
func (self *sdkBrowsingNative) matches(ip net.IP, port int, protocol clientconnect.IpProtocol) bool {
	if protocol != clientconnect.IpProtocolTcp || port != 443 {
		return false
	}
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	for ordinal := 0; ordinal < self.originCount; ordinal++ {
		if ip.Equal(net.IPv4(203, 0, 113, byte(10+ordinal))) {
			return true
		}
	}
	return false
}

// This is a logical TUN destination, never an operating-system route.
func (self *sdkBrowsingNative) destination(ordinal int, _ net.Listener) string {
	return net.JoinHostPort(net.IPv4(203, 0, 113, byte(10+ordinal)).String(), "443")
}

// One successful upstream dial retains exact provider attribution until Close.
type sdkBrowsingOriginConn struct {
	net.Conn
	native    *sdkBrowsingNative
	ordinal   int
	tuple     sdkBrowsingOriginTuple
	closeOnce sync.Once
	closeErr  error
}

// The exact positive CSS read is held before NAT can forward its bytes.
func (self *sdkBrowsingOriginConn) Read(buffer []byte) (int, error) {
	count, err := self.Conn.Read(buffer)
	if !self.native.suppressOriginReadHoldForTest.Load() {
		self.native.scenario.holdOriginRead(self, count)
	}
	return count, err
}

// Pointer equality prevents a late close from removing another socket owner.
func (self *sdkBrowsingOriginConn) Close() error {
	self.closeOnce.Do(func() {
		self.closeErr = self.Conn.Close()
		self.native.stateLock.Lock()
		if self.native.upstreamConns[self.tuple] == self {
			delete(self.native.upstreamConns, self.tuple)
		}
		self.native.stateLock.Unlock()
		self.native.originCloses[self.ordinal].Add(1)
	})
	return self.closeErr
}

// Public probe traffic keeps its ordinary dialer. Only named documentation
// endpoints translate; old-provider refusal is an actual NAT dial error.
func (self *sdkBrowsingNative) dialProvider(ordinal int, ctx context.Context, network, address string) (net.Conn, error) {
	if self.scenario.rejectNavigationDial(ordinal, address) {
		return nil, errors.New("synthetic old-provider new-navigation refusal")
	}
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, err
	}
	ip, err := netip.ParseAddr(host)
	if err != nil {
		return nil, err
	}
	if !netip.MustParsePrefix("203.0.113.0/24").Contains(ip) {
		return (&net.Dialer{}).DialContext(ctx, network, address)
	}
	self.stateLock.Lock()
	destination := ""
	for origin := 0; origin < self.originCount; origin++ {
		if ip == netip.AddrFrom4([4]byte{203, 0, 113, byte(10 + origin)}) && port == "443" {
			originPort := self.coldLinkPortForTest
			if origin < nativeBrowsingOriginCount {
				originPort = self.ports[origin]
			}
			destination = net.JoinHostPort("127.0.0.1", fmt.Sprint(originPort))
			break
		}
	}
	self.stateLock.Unlock()
	if destination == "" || network != "tcp" && network != "tcp4" {
		return nil, errors.New("unowned documentation fixture endpoint")
	}
	connection, err := (&net.Dialer{}).DialContext(ctx, "tcp4", destination)
	if err != nil {
		return nil, err
	}
	return self.ownOriginConnection(connection, ordinal)
}

// The same ownership boundary serves real dials and deterministic tuple controls.
// An already-live identical full tuple is still refused without changing its owner.
func (self *sdkBrowsingNative) ownOriginConnection(connection net.Conn, ordinal int) (*sdkBrowsingOriginConn, error) {
	tuple := sdkBrowsingOriginTuple{sourceAddr: connection.LocalAddr().String(), destinationAddr: connection.RemoteAddr().String()}
	owned := &sdkBrowsingOriginConn{Conn: connection, native: self, ordinal: ordinal, tuple: tuple}
	self.stateLock.Lock()
	if self.upstreamConns[tuple] != nil {
		self.stateLock.Unlock()
		connection.Close()
		return nil, errors.New("live origin full-tuple attribution collided")
	}
	self.upstreamConns[tuple] = owned
	self.stateLock.Unlock()
	self.originDials[ordinal].Add(1)
	return owned, nil
}

// The accepted local endpoint is the dial's remote destination. Missing local
// context never falls back to source-only lookup or a guessed provider.
func (self *sdkBrowsingNative) originConnection(request *http.Request) *sdkBrowsingOriginConn {
	if request == nil || request.RemoteAddr == "" {
		return nil
	}
	localAddr, ok := request.Context().Value(http.LocalAddrContextKey).(net.Addr)
	if !ok || localAddr == nil {
		return nil
	}
	tuple := sdkBrowsingOriginTuple{sourceAddr: request.RemoteAddr, destinationAddr: localAddr.String()}
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.upstreamConns[tuple]
}

// HTTP attribution joins the actual full tuple, never source port or list order.
func (self *sdkBrowsingNative) provider(request *http.Request) (int, bool) {
	if connection := self.originConnection(request); connection != nil {
		return connection.ordinal, true
	}
	return -1, false
}

// Public security-policy factories retain ordinary policy for every other IP.
type sdkBrowsingOriginPolicy struct {
	clientconnect.SecurityPolicy
	native *sdkBrowsingNative
}

// Outbound client and provider packets refer to opposite ends of the same flow.
func (self *sdkBrowsingOriginPolicy) InspectEgress(mode protocol.ProvideMode, path *clientconnect.IpPath, packet []byte) (clientconnect.SecurityPolicyResult, error) {
	if path != nil {
		ip, port := path.DestinationIp, path.DestinationPort
		if self.native.matches(ip, port, path.Protocol) {
			return clientconnect.SecurityPolicyResultAllow, nil
		}
	}
	return self.SecurityPolicy.InspectEgress(mode, path, packet)
}

// Reply policy uses the inverse endpoint and the same narrow origin set.
func (self *sdkBrowsingOriginPolicy) InspectIngress(mode protocol.ProvideMode, path *clientconnect.IpPath, packet []byte) (clientconnect.SecurityPolicyResult, error) {
	if path != nil {
		ip, port := path.SourceIp, path.SourcePort
		if self.native.matches(ip, port, path.Protocol) {
			return clientconnect.SecurityPolicyResultAllow, nil
		}
	}
	return self.SecurityPolicy.InspectIngress(mode, path, packet)
}

// Providers exist before the application TUN. Their policy takes a short atomic
// snapshot when needed; ordinary provider policy remains active before then.
type sdkBrowsingProviderOriginPolicy struct {
	clientconnect.SecurityPolicy
	fixture *sdkBrowsingSetupFixture
}

// An outbound provider reply names the logical origin as its source.
func (self *sdkBrowsingProviderOriginPolicy) InspectEgress(mode protocol.ProvideMode, path *clientconnect.IpPath, packet []byte) (clientconnect.SecurityPolicyResult, error) {
	if native := self.fixture.native.Load(); native != nil && path != nil && native.matches(path.SourceIp, path.SourcePort, path.Protocol) {
		return clientconnect.SecurityPolicyResultAllow, nil
	}
	return self.SecurityPolicy.InspectEgress(mode, path, packet)
}

// An inbound provider packet names the logical origin as its destination.
func (self *sdkBrowsingProviderOriginPolicy) InspectIngress(mode protocol.ProvideMode, path *clientconnect.IpPath, packet []byte) (clientconnect.SecurityPolicyResult, error) {
	if native := self.fixture.native.Load(); native != nil && path != nil && native.matches(path.DestinationIp, path.DestinationPort, path.Protocol) {
		return clientconnect.SecurityPolicyResultAllow, nil
	}
	return self.SecurityPolicy.InspectIngress(mode, path, packet)
}
