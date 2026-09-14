// This file connects production gVisor TUNs and native Pion sockets through
// userspace-only network models that run unchanged on macOS.
package perfvar

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"sort"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pion/logging"
	"github.com/pion/transport/v4"
	"github.com/pion/transport/v4/vnet"
	clientconnect "github.com/urnetwork/connect/v2026"
)

// One ordered pair identifies an independently conditioned direction.
type tunLinkKey struct {
	source      string
	destination string
}

// An acknowledged live change retains both the requested and actual boundary
// for deterministic event measurements.
type networkProfileUpdateResult struct {
	LinkName      string
	EventName     string
	ScheduledTime time.Time
	ActualTime    time.Time
}

// A TUN node has separate nonblocking network ingress and blocking stack write.
type simulatedTunNode struct {
	name       string
	tun        *clientconnect.Tun
	address    netip.Addr
	deliveries chan []byte
}

// A diagnostic observer borrows one complete IP packet exactly as it leaves a
// source TUN. It must return synchronously and must not retain or mutate bytes.
type simulatedIPPacketObserver func(sourceNode string, packet []byte)

// The packet router reads real IP packets emitted by each production TUN.
type simulatedIPNetwork struct {
	ctx       context.Context
	cancel    context.CancelFunc
	closeOnce sync.Once
	waitGroup sync.WaitGroup

	stateLock sync.Mutex
	nodes     map[string]*simulatedTunNode
	addresses map[netip.Addr]*simulatedTunNode
	links     map[tunLinkKey]*directionalLink

	unknownDestinationPacketCount atomic.Uint64
	packetObserver                atomic.Pointer[simulatedIPPacketObserver]
}

func (self *simulatedIPNetwork) setPacketObserver(observer simulatedIPPacketObserver) {
	if observer == nil {
		self.packetObserver.Store(nil)
		return
	}
	self.packetObserver.Store(&observer)
}

// The router starts empty so callers can define topologies before traffic.
func newSimulatedIPNetwork(ctx context.Context) *simulatedIPNetwork {
	networkCtx, cancel := context.WithCancel(ctx)
	return &simulatedIPNetwork{
		ctx:       networkCtx,
		cancel:    cancel,
		nodes:     map[string]*simulatedTunNode{},
		addresses: map[netip.Addr]*simulatedTunNode{},
		links:     map[tunLinkKey]*directionalLink{},
	}
}

// Each TUN must have one IPv4 address and becomes owned by the network.
func (self *simulatedIPNetwork) addTun(name string, tun *clientconnect.Tun) error {
	addresses := tun.LocalAddresses()
	if len(addresses) == 0 || !addresses[0].Is4() {
		return fmt.Errorf("TUN %q has no IPv4 address", name)
	}
	node := &simulatedTunNode{
		name:    name,
		tun:     tun,
		address: addresses[0],
		// Link queues own intentional capacity limits. This handoff only
		// isolates a blocking TUN write and must not become a second bottleneck.
		deliveries: make(chan []byte, 64*1024),
	}
	self.stateLock.Lock()
	if _, ok := self.nodes[name]; ok {
		self.stateLock.Unlock()
		return fmt.Errorf("duplicate TUN node %q", name)
	}
	if _, ok := self.addresses[node.address]; ok {
		self.stateLock.Unlock()
		return fmt.Errorf("duplicate TUN address %s", node.address)
	}
	self.nodes[name] = node
	self.addresses[node.address] = node
	self.stateLock.Unlock()

	self.waitGroup.Add(2)
	go self.readTun(node)
	go self.writeTun(node)
	return nil
}

// One link applies only to packets emitted by source for destination.
func (self *simulatedIPNetwork) addLink(
	source string,
	destination string,
	profile linkProfile,
	seed int64,
) (*directionalLink, error) {
	self.stateLock.Lock()
	sourceNode := self.nodes[source]
	destinationNode := self.nodes[destination]
	key := tunLinkKey{source: source, destination: destination}
	if sourceNode == nil || destinationNode == nil {
		self.stateLock.Unlock()
		return nil, fmt.Errorf("unknown TUN link %q -> %q", source, destination)
	}
	if _, ok := self.links[key]; ok {
		self.stateLock.Unlock()
		return nil, fmt.Errorf("duplicate TUN link %q -> %q", source, destination)
	}
	deliver := func(packetBytes []byte) bool {
		select {
		case destinationNode.deliveries <- packetBytes:
			return true
		default:
			return false
		}
	}
	link := newDirectionalLink(self.ctx, profile, seed, deliver)
	self.links[key] = link
	self.stateLock.Unlock()
	return link, nil
}

// A common two-node path uses independent forward and reverse schedulers.
func (self *simulatedIPNetwork) addBidirectionalLink(
	source string,
	destination string,
	profile networkProfile,
) (*directionalLink, *directionalLink, error) {
	forward, err := self.addLink(source, destination, profile.Forward, profile.Seed)
	if err != nil {
		return nil, nil, err
	}
	reverse, err := self.addLink(destination, source, profile.Reverse, profile.Seed+1)
	if err != nil {
		forward.close()
		return nil, nil, err
	}
	return forward, reverse, nil
}

// The source reader copies through the selected link and returns pooled TUN bytes.
func (self *simulatedIPNetwork) readTun(node *simulatedTunNode) {
	defer self.waitGroup.Done()
	packets := make([][]byte, 64)
	for {
		packetCount, err := node.tun.ReadBatch(packets)
		if err != nil {
			return
		}
		for _, packetBytes := range packets[:packetCount] {
			if observer := self.packetObserver.Load(); observer != nil {
				(*observer)(node.name, packetBytes)
			}
			destinationAddress, ok := ipv4Destination(packetBytes)
			if !ok {
				self.unknownDestinationPacketCount.Add(1)
				clientconnect.MessagePoolReturn(packetBytes)
				continue
			}
			self.stateLock.Lock()
			destinationNode := self.addresses[destinationAddress]
			var link *directionalLink
			if destinationNode != nil {
				link = self.links[tunLinkKey{
					source:      node.name,
					destination: destinationNode.name,
				}]
			}
			self.stateLock.Unlock()
			if link == nil {
				self.unknownDestinationPacketCount.Add(1)
			} else {
				_, _ = link.submit(packetBytes)
			}
			clientconnect.MessagePoolReturn(packetBytes)
		}
		select {
		case <-self.ctx.Done():
			return
		default:
		}
	}
}

// A dedicated writer isolates one slow TUN from every unrelated link scheduler.
func (self *simulatedIPNetwork) writeTun(node *simulatedTunNode) {
	defer self.waitGroup.Done()
	for {
		select {
		case <-self.ctx.Done():
			return
		case packetBytes := <-node.deliveries:
			_, _ = node.tun.Write(packetBytes)
		}
	}
}

// IPv4 destination parsing stays allocation-free on the packet hot path.
func ipv4Destination(packetBytes []byte) (netip.Addr, bool) {
	if len(packetBytes) < 20 || packetBytes[0]>>4 != 4 {
		return netip.Addr{}, false
	}
	return netip.AddrFrom4([4]byte{
		packetBytes[16],
		packetBytes[17],
		packetBytes[18],
		packetBytes[19],
	}), true
}

// Closing TUNs unblocks readers before the router joins every worker.
func (self *simulatedIPNetwork) close() {
	self.closeOnce.Do(func() {
		self.cancel()
		self.stateLock.Lock()
		links := make([]*directionalLink, 0, len(self.links))
		nodes := make([]*simulatedTunNode, 0, len(self.nodes))
		for _, link := range self.links {
			links = append(links, link)
		}
		for _, node := range self.nodes {
			nodes = append(nodes, node)
		}
		self.stateLock.Unlock()
		for _, link := range links {
			link.close()
		}
		for _, node := range nodes {
			node.tun.Close()
		}
		self.waitGroup.Wait()
	})
}

// A measurement boundary takes an immutable view of every configured link.
func (self *simulatedIPNetwork) snapshotLinks() map[string]directionalLinkSnapshot {
	self.stateLock.Lock()
	links := make(map[tunLinkKey]*directionalLink, len(self.links))
	for key, link := range self.links {
		links[key] = link
	}
	self.stateLock.Unlock()
	snapshots := make(map[string]directionalLinkSnapshot, len(links))
	for key, link := range links {
		snapshots[fmt.Sprintf("%s->%s", key.source, key.destination)] = link.snapshot()
	}
	return snapshots
}

// Current profiles let event tests verify every requested axis at the
// acknowledged boundary without adding simulator state to production code.
func (self *simulatedIPNetwork) snapshotProfiles() map[string]linkProfile {
	self.stateLock.Lock()
	links := make(map[tunLinkKey]*directionalLink, len(self.links))
	for key, link := range self.links {
		links[key] = link
	}
	self.stateLock.Unlock()
	profiles := make(map[string]linkProfile, len(links))
	for key, link := range links {
		link.stateLock.Lock()
		profile := link.profile
		link.stateLock.Unlock()
		profiles[fmt.Sprintf("%s->%s", key.source, key.destination)] = profile
	}
	return profiles
}

// A filtered scheduled update changes selected directional links in a stable
// order while production carrier and route objects remain live.
func (self *simulatedIPNetwork) updateProfilesWhere(
	ctx context.Context,
	eventName string,
	scheduledTime time.Time,
	update func(tunLinkKey, linkProfile) (linkProfile, bool),
) ([]networkProfileUpdateResult, error) {
	if update == nil {
		return nil, fmt.Errorf("network profile update is nil")
	}
	type namedLink struct {
		name string
		key  tunLinkKey
		link *directionalLink
	}
	self.stateLock.Lock()
	links := make([]namedLink, 0, len(self.links))
	for key, link := range self.links {
		links = append(links, namedLink{
			name: fmt.Sprintf("%s->%s", key.source, key.destination),
			key:  key,
			link: link,
		})
	}
	self.stateLock.Unlock()
	sort.Slice(links, func(i int, j int) bool {
		return links[i].name < links[j].name
	})
	if wait := time.Until(scheduledTime); 0 < wait {
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
	results := make([]networkProfileUpdateResult, 0, len(links))
	for _, named := range links {
		named.link.stateLock.Lock()
		profile := named.link.profile
		named.link.stateLock.Unlock()
		updatedProfile, selected := update(named.key, profile)
		if !selected {
			continue
		}
		actualTime, err := named.link.updateProfile(
			updatedProfile,
			eventName,
			scheduledTime,
		)
		if err != nil {
			return results, fmt.Errorf("update %s: %w", named.name, err)
		}
		results = append(results, networkProfileUpdateResult{
			LinkName:      named.name,
			EventName:     eventName,
			ScheduledTime: scheduledTime,
			ActualTime:    actualTime,
		})
		select {
		case <-ctx.Done():
			return results, ctx.Err()
		default:
		}
	}
	return results, nil
}

// A route-wide scheduled update changes every existing directional link.
func (self *simulatedIPNetwork) updateProfiles(
	ctx context.Context,
	eventName string,
	scheduledTime time.Time,
	update func(linkProfile) linkProfile,
) ([]networkProfileUpdateResult, error) {
	if update == nil {
		return nil, fmt.Errorf("network profile update is nil")
	}
	return self.updateProfilesWhere(
		ctx,
		eventName,
		scheduledTime,
		func(_ tunLinkKey, profile linkProfile) (linkProfile, bool) {
			return update(profile), true
		},
	)
}

// Device-oriented updates touch only links incident to one TUN node. Outbound
// is application upload; inbound is application download. Provider and
// internal exchange links remain unchanged.
func (self *simulatedIPNetwork) updateNodeProfiles(
	ctx context.Context,
	nodeName string,
	eventName string,
	scheduledTime time.Time,
	forward *linkProfile,
	reverse *linkProfile,
) ([]networkProfileUpdateResult, error) {
	if nodeName == "" {
		return nil, errors.New("network profile update node is empty")
	}
	return self.updateProfilesWhere(
		ctx,
		eventName,
		scheduledTime,
		func(key tunLinkKey, profile linkProfile) (linkProfile, bool) {
			switch {
			case key.source == nodeName && forward != nil:
				return *forward, true
			case key.destination == nodeName && reverse != nil:
				return *reverse, true
			default:
				return profile, false
			}
		},
	)
}

// Construction can recover the stable simulator node identity without relying
// on client creation order.
func (self *simulatedIPNetwork) nodeNameForTun(tun *clientconnect.Tun) (string, bool) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	for name, node := range self.nodes {
		if node.tun == tun {
			return name, true
		}
	}
	return "", false
}

// The workload terminal boundary joins every current link and repeats if a
// carrier admitted work during that pass. Production delivery supplies the
// upstream stop boundary; this method proves the simulator tail is empty.
func (self *simulatedIPNetwork) directionalLinks() []*directionalLink {
	self.stateLock.Lock()
	links := make([]*directionalLink, 0, len(self.links))
	for _, link := range self.links {
		links = append(links, link)
	}
	self.stateLock.Unlock()
	return links
}

// Each access or exchange link resets and captures its own interval baseline
// atomically; the caller retries if traffic crossed the multi-link start pass.
func (self *simulatedIPNetwork) beginMeasurementSnapshotLinks(
	ctx context.Context,
) (map[string]directionalLinkSnapshot, bool) {
	self.stateLock.Lock()
	links := make(map[tunLinkKey]*directionalLink, len(self.links))
	for key, link := range self.links {
		links[key] = link
	}
	self.stateLock.Unlock()
	snapshots := make(map[string]directionalLinkSnapshot, len(links))
	for key, link := range links {
		snapshot, ok := link.beginMeasurementSnapshot(ctx)
		if !ok {
			return nil, false
		}
		snapshots[fmt.Sprintf("%s->%s", key.source, key.destination)] = snapshot
	}
	return snapshots, true
}

// The fixed-point link barrier includes every current simulated IP direction.
func (self *simulatedIPNetwork) waitForTerminalIdle(ctx context.Context) bool {
	links := self.directionalLinks()
	return waitForDirectionalLinksTerminalIdle(ctx, links, nil)
}

// A route-wide live update models an access-link outage while retaining every
// production TCP, QUIC, WebSocket, handler, and exchange object.
func (self *simulatedIPNetwork) setBlackhole(ctx context.Context, blackhole bool) error {
	_, err := self.updateProfiles(ctx, "route-blackhole", time.Now(), func(profile linkProfile) linkProfile {
		profile.Blackhole = blackhole
		return profile
	})
	return err
}

// Pion network counters attribute deterministic filter drops by direction.
type p2pNetworkSnapshot struct {
	Forward                directionalLinkSnapshot  `json:"forward"`
	Reverse                directionalLinkSnapshot  `json:"reverse"`
	ForwardReceiveCredits  p2pReceiveCreditSnapshot `json:"forward_receive_credits"`
	ReverseReceiveCredits  p2pReceiveCreditSnapshot `json:"reverse_receive_credits"`
	ForwardPacketCount     uint64                   `json:"forward_packet_count"`
	ReversePacketCount     uint64                   `json:"reverse_packet_count"`
	ForwardWireByteCount   uint64                   `json:"forward_wire_byte_count"`
	ReverseWireByteCount   uint64                   `json:"reverse_wire_byte_count"`
	ForwardDropCount       uint64                   `json:"forward_drop_count"`
	ReverseDropCount       uint64                   `json:"reverse_drop_count"`
	ForwardMtuDropCount    uint64                   `json:"forward_mtu_drop_count"`
	ReverseMtuDropCount    uint64                   `json:"reverse_mtu_drop_count"`
	MtuDropCount           uint64                   `json:"mtu_drop_count"`
	MaximumPacketByteCount uint64                   `json:"maximum_packet_byte_count"`
}

// A stable transport.Net identity delegates each new socket or interface query
// to the current vnet endpoint. Existing UDP sockets retain their old endpoint.
type swappableP2pNet struct {
	stateLock sync.RWMutex
	current   transport.Net
}

// Construction requires a complete Pion network implementation.
func newSwappableP2pNet(current transport.Net) *swappableP2pNet {
	return &swappableP2pNet{current: current}
}

// A short read lock returns one immutable delegate for an entire operation.
func (self *swappableP2pNet) currentNet() transport.Net {
	self.stateLock.RLock()
	defer self.stateLock.RUnlock()
	return self.current
}

// Future operations move to the replacement without changing the injected
// interface value held by the production WebRTC manager.
func (self *swappableP2pNet) swap(current transport.Net) {
	self.stateLock.Lock()
	self.current = current
	self.stateLock.Unlock()
}

// Packet listeners bind on the endpoint current at call time.
func (self *swappableP2pNet) ListenPacket(network string, address string) (net.PacketConn, error) {
	return self.currentNet().ListenPacket(network, address)
}

// UDP listeners bind on the endpoint current at call time.
func (self *swappableP2pNet) ListenUDP(network string, address *net.UDPAddr) (transport.UDPConn, error) {
	return self.currentNet().ListenUDP(network, address)
}

// TCP listeners bind on the endpoint current at call time.
func (self *swappableP2pNet) ListenTCP(network string, address *net.TCPAddr) (transport.TCPListener, error) {
	return self.currentNet().ListenTCP(network, address)
}

// Generic dialing uses one current delegate.
func (self *swappableP2pNet) Dial(network string, address string) (net.Conn, error) {
	return self.currentNet().Dial(network, address)
}

// UDP dialing uses one current delegate.
func (self *swappableP2pNet) DialUDP(
	network string,
	localAddress *net.UDPAddr,
	remoteAddress *net.UDPAddr,
) (transport.UDPConn, error) {
	return self.currentNet().DialUDP(network, localAddress, remoteAddress)
}

// TCP dialing uses one current delegate.
func (self *swappableP2pNet) DialTCP(
	network string,
	localAddress *net.TCPAddr,
	remoteAddress *net.TCPAddr,
) (transport.TCPConn, error) {
	return self.currentNet().DialTCP(network, localAddress, remoteAddress)
}

// IP resolution uses the current endpoint's resolver.
func (self *swappableP2pNet) ResolveIPAddr(network string, address string) (*net.IPAddr, error) {
	return self.currentNet().ResolveIPAddr(network, address)
}

// UDP resolution uses the current endpoint's resolver.
func (self *swappableP2pNet) ResolveUDPAddr(network string, address string) (*net.UDPAddr, error) {
	return self.currentNet().ResolveUDPAddr(network, address)
}

// TCP resolution uses the current endpoint's resolver.
func (self *swappableP2pNet) ResolveTCPAddr(network string, address string) (*net.TCPAddr, error) {
	return self.currentNet().ResolveTCPAddr(network, address)
}

// Interface enumeration exposes only the current endpoint.
func (self *swappableP2pNet) Interfaces() ([]*transport.Interface, error) {
	return self.currentNet().Interfaces()
}

// Index lookup exposes only the current endpoint.
func (self *swappableP2pNet) InterfaceByIndex(index int) (*transport.Interface, error) {
	return self.currentNet().InterfaceByIndex(index)
}

// Name lookup exposes only the current endpoint.
func (self *swappableP2pNet) InterfaceByName(name string) (*transport.Interface, error) {
	return self.currentNet().InterfaceByName(name)
}

// A created dialer remains attached to the endpoint selected now.
func (self *swappableP2pNet) CreateDialer(dialer *net.Dialer) transport.Dialer {
	return self.currentNet().CreateDialer(dialer)
}

// A created listener configuration remains attached to the endpoint selected now.
func (self *swappableP2pNet) CreateListenConfig(config *net.ListenConfig) transport.ListenConfig {
	return self.currentNet().CreateListenConfig(config)
}

// The native WebRTC model retains real ICE, DTLS, SCTP, and SRTP above vnet.
// Packet filtering, live toggles, and snapshots are safe concurrently;
// migration and close belong to the fixture lifecycle.
type p2pNetwork struct {
	router                *vnet.Router
	left                  transport.Net
	right                 *swappableP2pNet
	rightNet              *vnet.Net
	rightLinkNet          *p2pLinkNet
	forwardLink           *directionalLink
	reverseLink           *directionalLink
	forwardReceiveCredits *p2pReceiveCredits
	reverseReceiveCredits *p2pReceiveCredits
	closeOnce             sync.Once
	profile               networkProfile

	leftAddress        atomic.Uint32
	rightAddress       atomic.Uint32
	lastForwardAddress atomic.Uint32
	lastReverseAddress atomic.Uint32
	lastForwardPort    atomic.Uint64
	lastReversePort    atomic.Uint64
	addressMigrations  atomic.Uint64
}

// Address observations prove that replacement ICE traffic uses a migrated
// userspace interface rather than the retired address.
type p2pNetworkAddressSnapshot struct {
	LeftAddress              string
	RightAddress             string
	LastForwardSourceAddress string
	LastReverseSourceAddress string
	LastForwardSourcePort    int
	LastReverseSourcePort    int
	AddressMigrationCount    uint64
}

// Only IPv4 is used by this hermetic Pion topology.
func p2pIPv4Value(ip net.IP) (uint32, bool) {
	ipv4 := ip.To4()
	if ipv4 == nil {
		return 0, false
	}
	return binary.BigEndian.Uint32(ipv4), true
}

// Zero means that no packet has yet provided an address observation.
func p2pIPv4String(value uint32) string {
	if value == 0 {
		return ""
	}
	bytes := [4]byte{}
	binary.BigEndian.PutUint32(bytes[:], value)
	return netip.AddrFrom4(bytes).String()
}

// The vnet adapter mirrors the resolved profile where Pion exposes controls.
func newP2pNetwork(profile networkProfile) (*p2pNetwork, error) {
	router, err := vnet.NewRouter(&vnet.RouterConfig{
		CIDR:          "10.240.0.0/24",
		QueueSize:     0,
		MinDelay:      0,
		MaxJitter:     0,
		LoggerFactory: logging.NewDefaultLoggerFactory(),
	})
	if err != nil {
		return nil, err
	}
	leftNet, err := vnet.NewNet(&vnet.NetConfig{StaticIPs: []string{"10.240.0.1"}})
	if err != nil {
		return nil, err
	}
	rightNet, err := vnet.NewNet(&vnet.NetConfig{StaticIPs: []string{"10.240.0.2"}})
	if err != nil {
		return nil, err
	}
	if err := router.AddNet(leftNet); err != nil {
		return nil, err
	}
	if err := router.AddNet(rightNet); err != nil {
		return nil, err
	}

	forwardLink := newDirectionalLink(context.Background(), profile.Forward, profile.Seed, nil)
	reverseLink := newDirectionalLink(context.Background(), profile.Reverse, profile.Seed+1, nil)
	forwardReceiveCredits := newP2pVnetReceiveCredits(p2pVnetReceiveCreditPacketCount)
	reverseReceiveCredits := newP2pVnetReceiveCredits(p2pVnetReceiveCreditPacketCount)
	network := &p2pNetwork{
		router:                router,
		rightNet:              rightNet,
		forwardLink:           forwardLink,
		reverseLink:           reverseLink,
		forwardReceiveCredits: forwardReceiveCredits,
		reverseReceiveCredits: reverseReceiveCredits,
		profile:               profile,
	}
	leftSourceAddress := netip.MustParseAddr("10.240.0.1")
	rightSourceAddress := netip.MustParseAddr("10.240.0.2")
	leftLinkNet := newP2pLinkNet(
		leftNet,
		forwardLink,
		forwardReceiveCredits,
		reverseReceiveCredits,
	)
	leftLinkNet.setSourceIPv4ForWildcardBinds(leftSourceAddress)
	network.left = leftLinkNet
	network.rightLinkNet = newP2pLinkNet(
		rightNet,
		reverseLink,
		reverseReceiveCredits,
		forwardReceiveCredits,
	)
	network.rightLinkNet.setSourceIPv4ForWildcardBinds(rightSourceAddress)
	network.right = newSwappableP2pNet(network.rightLinkNet)
	leftAddress, _ := p2pIPv4Value(net.ParseIP("10.240.0.1"))
	rightAddress, _ := p2pIPv4Value(net.ParseIP("10.240.0.2"))
	network.leftAddress.Store(leftAddress)
	network.rightAddress.Store(rightAddress)
	// The router only observes real vnet addresses. All packet conditioning is
	// already complete at each endpoint's link-backed UDP write boundary.
	router.AddChunkFilter(func(chunk vnet.Chunk) bool {
		source, sourceOk := chunk.SourceAddr().(*net.UDPAddr)
		destination, destinationOk := chunk.DestinationAddr().(*net.UDPAddr)
		if !sourceOk || !destinationOk {
			return true
		}
		sourceAddress, sourceAddressOk := p2pIPv4Value(source.IP)
		destinationAddress, destinationAddressOk := p2pIPv4Value(destination.IP)
		if !sourceAddressOk || !destinationAddressOk {
			return true
		}
		if sourceAddress == network.leftAddress.Load() {
			if !network.forwardReceiveCredits.acceptRouterPayload(
				chunk.SourceAddr(),
				chunk.DestinationAddr(),
				chunk.UserData(),
			) {
				return false
			}
		}
		if destinationAddress == network.leftAddress.Load() {
			if !network.reverseReceiveCredits.acceptRouterPayload(
				chunk.SourceAddr(),
				chunk.DestinationAddr(),
				chunk.UserData(),
			) {
				return false
			}
		}
		if sourceAddress == network.leftAddress.Load() &&
			destinationAddress == network.rightAddress.Load() {
			network.lastForwardAddress.Store(sourceAddress)
			network.lastForwardPort.Store(uint64(source.Port))
		}
		if sourceAddress == network.rightAddress.Load() &&
			destinationAddress == network.leftAddress.Load() {
			network.lastReverseAddress.Store(sourceAddress)
			network.lastReversePort.Store(uint64(source.Port))
		}
		return true
	})
	if err := router.Start(); err != nil {
		network.close()
		return nil, err
	}
	return network, nil
}

// Replacing the right-side endpoint models a wifi-to-cellular interface change.
// Existing sockets retain the retired endpoint while a rebuilt factory sees
// the replacement through the stable injected transport.Net identity.
func (self *p2pNetwork) migrateRightAddress(address net.IP) error {
	addressValue, ok := p2pIPv4Value(address)
	if !ok {
		return fmt.Errorf("P2P migration address %q is not IPv4", address)
	}
	previousAddressValue := self.rightAddress.Load()
	if addressValue == previousAddressValue {
		return fmt.Errorf("P2P migration address %s is unchanged", address)
	}
	interfaces, err := self.rightNet.Interfaces()
	if err != nil {
		return err
	}
	if len(interfaces) == 0 {
		return fmt.Errorf("P2P right network has no interface")
	}
	newNet, err := vnet.NewNet(&vnet.NetConfig{StaticIPs: []string{address.String()}})
	if err != nil {
		return err
	}
	if err := self.router.AddNet(newNet); err != nil {
		return err
	}
	previousAddress := net.ParseIP(p2pIPv4String(previousAddressValue))
	if err := self.rightNet.RemoveAddress(interfaces[0].Name, previousAddress); err != nil {
		return err
	}
	self.forwardReceiveCredits.retireOwner(self.rightLinkNet)
	self.rightLinkNet = newP2pLinkNet(
		newNet,
		self.reverseLink,
		self.reverseReceiveCredits,
		self.forwardReceiveCredits,
	)
	self.rightLinkNet.setSourceIPv4ForWildcardBinds(
		netip.MustParseAddr(p2pIPv4String(addressValue)),
	)
	self.right.swap(self.rightLinkNet)
	self.rightNet = newNet
	self.rightAddress.Store(addressValue)
	self.addressMigrations.Add(1)
	return nil
}

// Interface migration replaces only the right vnet endpoint; both directional
// credit pools retain their identity so old and new sockets share one bound.
func TestP2pNetworkMigrationRetainsReceiveAdmissionPools(t *testing.T) {
	profile := testP2pLinkProfile(1500, oversizeModeDrop)
	network, err := newP2pNetwork(networkProfile{
		Name:    "receive-credit-migration",
		Seed:    7203,
		Forward: profile,
		Reverse: profile,
	})
	if err != nil {
		t.Fatalf("create P2P receive-credit migration network: %v", err)
	}
	defer network.close()
	forwardCredits := network.forwardReceiveCredits
	reverseCredits := network.reverseReceiveCredits
	if err := network.migrateRightAddress(net.ParseIP("10.240.0.3")); err != nil {
		t.Fatalf("migrate P2P receive-credit network: %v", err)
	}
	right, ok := network.right.currentNet().(*p2pLinkNet)
	if !ok {
		t.Fatalf("migrated right network type=%T", network.right.currentNet())
	}
	if right.outboundCredits != reverseCredits || right.inboundCredits != forwardCredits {
		t.Fatal("migrated right endpoint replaced directional receive-credit identity")
	}
	left, ok := network.left.(*p2pLinkNet)
	if !ok {
		t.Fatalf("left network type=%T", network.left)
	}
	if left.outboundCredits != forwardCredits || left.inboundCredits != reverseCredits {
		t.Fatal("migration changed the stable left receive-credit identity")
	}
}

// Address migration retires every old endpoint socket before the swappable
// Net exposes its replacement. Writes to the removed NIC therefore drop
// without creating a receive reservation that no socket can release.
func TestP2pNetworkMigrationRetiresOldDestinationCredits(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	profile := testP2pLinkProfile(1500, oversizeModeDrop)
	network, err := newP2pNetwork(networkProfile{
		Name:    "receive-credit-old-address",
		Seed:    7204,
		Forward: profile,
		Reverse: profile,
	})
	if err != nil {
		t.Fatalf("create old-address migration network: %v", err)
	}
	defer network.close()
	oldDestination := &net.UDPAddr{IP: net.IPv4(10, 240, 0, 2), Port: 52201}
	receiver, err := network.right.ListenUDP("udp4", oldDestination)
	if err != nil {
		t.Fatalf("listen old migration destination: %v", err)
	}
	defer receiver.Close()
	if err := network.migrateRightAddress(net.IPv4(10, 240, 0, 3)); err != nil {
		t.Fatalf("migrate right P2P address: %v", err)
	}
	sender, err := network.left.DialUDP(
		"udp4",
		&net.UDPAddr{IP: net.IPv4(10, 240, 0, 1), Port: 0},
		oldDestination,
	)
	if err != nil {
		t.Fatalf("dial retired migration destination: %v", err)
	}
	defer sender.Close()
	payload := []byte("retired-address")
	if writtenByteCount, writeErr := sender.Write(payload); writeErr != nil ||
		writtenByteCount != len(payload) {
		t.Fatalf("write retired migration destination bytes=%d err=%v", writtenByteCount, writeErr)
	}
	if !network.forwardLink.waitIdle(ctx) {
		t.Fatalf("join retired migration destination: %v", ctx.Err())
	}
	snapshot := network.forwardReceiveCredits.snapshot()
	if snapshot.AdmittedPacketCount != 0 || snapshot.OutstandingPacketCount != 0 ||
		snapshot.TrackedReservationCount != 0 || snapshot.InvalidReleasePacketCount != 0 {
		t.Fatalf("retired migration destination credits=%+v", snapshot)
	}
	linkSnapshot := network.forwardLink.snapshot()
	if linkSnapshot.ReceiverDropPacketCount != 1 || linkSnapshot.DeliveredPacketCount != 0 {
		t.Fatalf("retired migration destination link=%+v", linkSnapshot)
	}
}

// A live snapshot does not stop Pion or mutate its interface list.
func (self *p2pNetwork) addressSnapshot() p2pNetworkAddressSnapshot {
	return p2pNetworkAddressSnapshot{
		LeftAddress:              p2pIPv4String(self.leftAddress.Load()),
		RightAddress:             p2pIPv4String(self.rightAddress.Load()),
		LastForwardSourceAddress: p2pIPv4String(self.lastForwardAddress.Load()),
		LastReverseSourceAddress: p2pIPv4String(self.lastReverseAddress.Load()),
		LastForwardSourcePort:    int(self.lastForwardPort.Load()),
		LastReverseSourcePort:    int(self.lastReversePort.Load()),
		AddressMigrationCount:    self.addressMigrations.Load(),
	}
}

// Live directional blackholes model a bounded direct-path outage without
// replacing production ICE, DTLS, SCTP, or SRTP objects.
func (self *p2pNetwork) setBlackhole(forward bool, reverse bool) error {
	update := func(link *directionalLink, blackhole bool) error {
		link.stateLock.Lock()
		profile := link.profile
		link.stateLock.Unlock()
		profile.Blackhole = blackhole
		_, err := link.updateProfile(profile, "P2P-blackhole", time.Now())
		return err
	}
	return errors.Join(
		update(self.forwardLink, forward),
		update(self.reverseLink, reverse),
	)
}

// A direct P2P live update keeps the established ICE/DTLS/SCTP objects while
// changing both physical directions at one acknowledged boundary.
func (self *p2pNetwork) updateProfiles(
	ctx context.Context,
	eventName string,
	scheduledTime time.Time,
	forward linkProfile,
	reverse linkProfile,
) ([]networkProfileUpdateResult, error) {
	if wait := time.Until(scheduledTime); 0 < wait {
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
	results := make([]networkProfileUpdateResult, 0, 2)
	for _, update := range []struct {
		name    string
		link    *directionalLink
		profile linkProfile
	}{
		{name: "p2p-provider-to-device", link: self.forwardLink, profile: forward},
		{name: "p2p-device-to-provider", link: self.reverseLink, profile: reverse},
	} {
		actualTime, err := update.link.updateProfile(update.profile, eventName, scheduledTime)
		if err != nil {
			return results, fmt.Errorf("update %s: %w", update.name, err)
		}
		results = append(results, networkProfileUpdateResult{
			LinkName:      update.name,
			EventName:     eventName,
			ScheduledTime: scheduledTime,
			ActualTime:    actualTime,
		})
	}
	return results, nil
}

// Every current direct carrier direction participates in route-wide idle.
func (self *p2pNetwork) directionalLinks() []*directionalLink {
	return []*directionalLink{self.forwardLink, self.reverseLink}
}

// Both destination pools participate in route-wide terminal ownership.
func (self *p2pNetwork) receiveCreditPools() []*p2pReceiveCredits {
	return []*p2pReceiveCredits{self.forwardReceiveCredits, self.reverseReceiveCredits}
}

// A terminal barrier proves no direct P2P packet remains scheduled or inside
// its deferred vnet write, even if admission raced an earlier idle pass.
func (self *p2pNetwork) waitForTerminalIdle(ctx context.Context) bool {
	return waitForP2pTerminalIdle(ctx, self.directionalLinks(), self.receiveCreditPools(), nil)
}

// Every terminal drop cause contributes exactly once to the total.
func p2pLinkDropCount(snapshot directionalLinkSnapshot) uint64 {
	return snapshot.LossDropPacketCount + snapshot.MtuDropPacketCount +
		snapshot.QueueDropPacketCount + snapshot.OutageDropPacketCount +
		snapshot.ReceiverDropPacketCount + snapshot.CanceledDropPacketCount
}

// One consistent direct-carrier view derives packet maxima and total drops
// from the same directional link snapshots that own their interval epochs.
func newP2pNetworkSnapshot(
	forward directionalLinkSnapshot,
	reverse directionalLinkSnapshot,
	forwardReceiveCredits p2pReceiveCreditSnapshot,
	reverseReceiveCredits p2pReceiveCreditSnapshot,
) p2pNetworkSnapshot {
	return p2pNetworkSnapshot{
		Forward:                forward,
		Reverse:                reverse,
		ForwardReceiveCredits:  forwardReceiveCredits,
		ReverseReceiveCredits:  reverseReceiveCredits,
		ForwardPacketCount:     forward.submittedPacketCount,
		ReversePacketCount:     reverse.submittedPacketCount,
		ForwardWireByteCount:   forward.WireByteCount,
		ReverseWireByteCount:   reverse.WireByteCount,
		ForwardDropCount:       p2pLinkDropCount(forward),
		ReverseDropCount:       p2pLinkDropCount(reverse),
		ForwardMtuDropCount:    forward.MtuDropPacketCount,
		ReverseMtuDropCount:    reverse.MtuDropPacketCount,
		MtuDropCount:           forward.MtuDropPacketCount + reverse.MtuDropPacketCount,
		MaximumPacketByteCount: uint64(max(forward.MaximumSubmittedPacketBytes, reverse.MaximumSubmittedPacketBytes)),
	}
}

// Reset and baseline are one lock-held operation for each direct direction and
// receive pool; a route-wide generation check retries cross-object traffic.
func (self *p2pNetwork) beginMeasurementSnapshot(
	ctx context.Context,
) (p2pNetworkSnapshot, bool) {
	forward, ok := self.forwardLink.beginMeasurementSnapshot(ctx)
	if !ok {
		return p2pNetworkSnapshot{}, false
	}
	reverse, ok := self.reverseLink.beginMeasurementSnapshot(ctx)
	if !ok {
		return p2pNetworkSnapshot{}, false
	}
	forwardReceiveCredits, ok := self.forwardReceiveCredits.beginMeasurementSnapshot(ctx)
	if !ok {
		return p2pNetworkSnapshot{}, false
	}
	reverseReceiveCredits, ok := self.reverseReceiveCredits.beginMeasurementSnapshot(ctx)
	if !ok {
		return p2pNetworkSnapshot{}, false
	}
	return newP2pNetworkSnapshot(
		forward,
		reverse,
		forwardReceiveCredits,
		reverseReceiveCredits,
	), true
}

// The snapshot is safe while Pion is active.
func (self *p2pNetwork) snapshot() p2pNetworkSnapshot {
	forward := self.forwardLink.snapshot()
	reverse := self.reverseLink.snapshot()
	return newP2pNetworkSnapshot(
		forward,
		reverse,
		self.forwardReceiveCredits.snapshot(),
		self.reverseReceiveCredits.snapshot(),
	)
}

// Concurrent live boundaries never tear one rejection away from its cause.
func TestP2pNetworkDropSnapshotsAreCauseConsistent(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	profile := testP2pLinkProfile(1280, oversizeModeDrop)
	network := &p2pNetwork{
		forwardLink: newDirectionalLink(ctx, profile, 7201, nil),
		reverseLink: newDirectionalLink(ctx, profile, 7202, nil),
	}
	defer network.forwardLink.close()
	defer network.reverseLink.close()
	start := make(chan struct{})
	var waitGroup sync.WaitGroup
	for _, forward := range []bool{true, false} {
		waitGroup.Add(1)
		go func() {
			defer waitGroup.Done()
			<-start
			for dropIndex := range 20_000 {
				packet := make([]byte, 1281+dropIndex%2)
				if forward {
					_, _ = network.forwardLink.submitOwnedWithDeliver(packet, nil)
				} else {
					_, _ = network.reverseLink.submitOwnedWithDeliver(packet, nil)
				}
			}
		}()
	}
	close(start)
	for range 20_000 {
		snapshot := network.snapshot()
		if snapshot.ForwardDropCount < snapshot.ForwardMtuDropCount ||
			snapshot.ReverseDropCount < snapshot.ReverseMtuDropCount ||
			snapshot.MtuDropCount != snapshot.ForwardMtuDropCount+snapshot.ReverseMtuDropCount {
			t.Fatalf("torn drop snapshot: %+v", snapshot)
		}
	}
	waitGroup.Wait()
}

// Closing links before the router joins all deferred writes and Pion workers.
func (self *p2pNetwork) close() {
	self.closeOnce.Do(func() {
		if self.forwardReceiveCredits != nil {
			self.forwardReceiveCredits.close()
		}
		if self.reverseReceiveCredits != nil {
			self.reverseReceiveCredits.close()
		}
		if self.forwardLink != nil {
			self.forwardLink.close()
		}
		if self.reverseLink != nil {
			self.reverseLink.close()
		}
		if self.router != nil {
			_ = self.router.Stop()
		}
	})
}
