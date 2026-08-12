// This file drives traffic through extended production topologies: adjacent
// P2P stream hops and distinct server/connect exchange edges. Userspace links
// reject topology shortcuts and retain per-segment attribution.
package perfvar

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/binary"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pion/logging"
	"github.com/pion/transport/v4"
	"github.com/pion/transport/v4/vnet"
	clientconnect "github.com/urnetwork/connect"
	"github.com/urnetwork/connect/protocol"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/api"
	connectserver "github.com/urnetwork/server/connect"
	"github.com/urnetwork/server/jwt"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/router"
	"github.com/urnetwork/server/session"
)

// One directional observation identifies traffic and impairment at an
// adjacent stream carrier.
type streamP2pDirectionSnapshot struct {
	ConfiguredRateBitsPerSecond int64                    `json:"configured_rate_bits_per_second"`
	ConfiguredBurstByteCount    int                      `json:"configured_burst_byte_count"`
	ConfiguredQueueByteCount    int                      `json:"configured_queue_byte_count"`
	Link                        directionalLinkSnapshot  `json:"link"`
	ReceiveCredits              p2pReceiveCreditSnapshot `json:"receive_credits"`
	PacketCount                 uint64                   `json:"packet_count"`
	PacketByteCount             uint64                   `json:"packet_byte_count"`
	DropCount                   uint64                   `json:"drop_count"`
	MtuDropCount                uint64                   `json:"mtu_drop_count"`
	MaximumPacketByteCount      uint64                   `json:"maximum_packet_byte_count"`
}

// One physical adjacency reports both directions independently.
type streamP2pHopSnapshot struct {
	HopIndex int                        `json:"hop_index"`
	Forward  streamP2pDirectionSnapshot `json:"forward"`
	Reverse  streamP2pDirectionSnapshot `json:"reverse"`
}

// Extended correctness records preserve topology-specific observations that
// do not fit the two-endpoint performance record.
type extendedTopologyRecord struct {
	SchemaVersion        int                                       `json:"schema_version"`
	RecordType           string                                    `json:"record_type"`
	Topology             string                                    `json:"topology"`
	Route                string                                    `json:"route"`
	AccessProfile        networkProfile                            `json:"access_profile"`
	InternalProfile      *networkProfile                           `json:"internal_exchange_profile,omitempty"`
	Result               workloadResult                            `json:"result"`
	Links                map[string]directionalLinkSnapshot        `json:"links,omitempty"`
	StreamP2PHops        []streamP2pHopSnapshot                    `json:"stream_p2p_hops,omitempty"`
	StreamP2PClientStats []clientconnect.P2pDataPlaneStatsSnapshot `json:"stream_p2p_client_stats,omitempty"`
	NonAdjacent          streamP2pNonAdjacentSnapshot              `json:"non_adjacent"`
	Correct              bool                                      `json:"correct"`
}

// Separates expected ICE candidate exploration from forbidden application
// traffic directed outside one physical adjacency.
type streamP2pNonAdjacentTracker struct {
	dialCount           atomic.Uint64
	stunPacketDropCount atomic.Uint64
	dataPacketDropCount atomic.Uint64

	stateLock    sync.Mutex
	eventStrings []string
}

// Captures bounded evidence for a failed run without retaining every periodic
// ICE connectivity check.
func (self *streamP2pNonAdjacentTracker) recordEvent(event string) {
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()

		if len(self.eventStrings) < 32 {
			self.eventStrings = append(self.eventStrings, event)
		}
	}()
}

// Records candidate-pair exploration rejected before a UDP packet exists.
func (self *streamP2pNonAdjacentTracker) recordDial(
	localAddress *net.UDPAddr,
	remoteAddress *net.UDPAddr,
) {
	self.dialCount.Add(1)
	self.recordEvent(fmt.Sprintf("class=ice-dial local=%v remote=%v", localAddress, remoteAddress))
}

// Identifies an RFC 5389 message by its fixed header and magic cookie.
func isStunDatagram(packet []byte) bool {
	if len(packet) < 20 || packet[0]&0xc0 != 0 ||
		binary.BigEndian.Uint32(packet[4:8]) != 0x2112a442 {
		return false
	}
	messageByteCount := int(binary.BigEndian.Uint16(packet[2:4]))
	return messageByteCount%4 == 0 && 20+messageByteCount == len(packet)
}

// Records and classifies a packet that the independent link router rejected.
func (self *streamP2pNonAdjacentTracker) recordPacket(
	hopIndex int,
	sourceAddress net.Addr,
	destinationAddress net.Addr,
	packet []byte,
) {
	packetClass := "data"
	if isStunDatagram(packet) {
		packetClass = "stun"
		self.stunPacketDropCount.Add(1)
	} else {
		self.dataPacketDropCount.Add(1)
	}
	self.recordEvent(fmt.Sprintf(
		"class=%s hop=%d source=%v destination=%v bytes=%d",
		packetClass,
		hopIndex,
		sourceAddress,
		destinationAddress,
		len(packet),
	))
}

// A value snapshot is race-free while WebRTC continues periodic checks.
type streamP2pNonAdjacentSnapshot struct {
	DialCount           uint64   `json:"dial_count"`
	StunPacketDropCount uint64   `json:"stun_packet_drop_count"`
	DataPacketDropCount uint64   `json:"data_packet_drop_count"`
	EventStrings        []string `json:"events,omitempty"`
}

// Copies counters and bounded diagnostic events at one observation boundary.
func (self *streamP2pNonAdjacentTracker) snapshot() streamP2pNonAdjacentSnapshot {
	snapshot := streamP2pNonAdjacentSnapshot{
		DialCount:           self.dialCount.Load(),
		StunPacketDropCount: self.stunPacketDropCount.Load(),
		DataPacketDropCount: self.dataPacketDropCount.Load(),
	}
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()

		snapshot.EventStrings = append([]string(nil), self.eventStrings...)
	}()
	return snapshot
}

// One logical client's WebRTC network can expose the independent physical
// links on its left and right without joining those links into one subnet.
type streamP2pNetEndpoint struct {
	network transport.Net
	address netip.Addr
	prefix  netip.Prefix
}

// Pion gathers one host candidate per adjacent physical link. Socket calls are
// dispatched by their bound address, so an intermediary cannot route directly
// between non-adjacent clients inside this adapter.
type streamP2pNodeNet struct {
	endpoints   []streamP2pNetEndpoint
	interfaces  []*transport.Interface
	nonAdjacent *streamP2pNonAdjacentTracker
}

// Compile-time interface coverage prevents a Pion upgrade from silently
// bypassing an unimplemented socket operation.
var _ transport.Net = (*streamP2pNodeNet)(nil)

// Each endpoint receives a distinct interface identity even though its native
// vnet interface is named eth0.
func newStreamP2pNodeNet(
	endpoints []streamP2pNetEndpoint,
	nonAdjacent *streamP2pNonAdjacentTracker,
) *streamP2pNodeNet {
	interfaces := make([]*transport.Interface, len(endpoints))
	for endpointIndex, endpoint := range endpoints {
		iface := transport.NewInterface(net.Interface{
			Index: endpointIndex + 2,
			MTU:   1500,
			Name:  fmt.Sprintf("eth%d", endpointIndex),
			Flags: net.FlagUp | net.FlagMulticast,
		})
		iface.AddAddress(&net.IPNet{
			IP:   net.IP(endpoint.address.AsSlice()),
			Mask: net.CIDRMask(endpoint.prefix.Bits(), 32),
		})
		interfaces[endpointIndex] = iface
	}
	return &streamP2pNodeNet{
		endpoints:   endpoints,
		interfaces:  interfaces,
		nonAdjacent: nonAdjacent,
	}
}

// Address dispatch selects the one independent vnet that owns an exact local
// address or can reach a remote address in its physical-link subnet.
func (self *streamP2pNodeNet) endpointForAddress(address net.IP) (*streamP2pNetEndpoint, bool) {
	if address == nil || address.IsUnspecified() {
		if len(self.endpoints) == 0 {
			return nil, false
		}
		return &self.endpoints[0], true
	}
	parsed, ok := netip.AddrFromSlice(address)
	if !ok {
		return nil, false
	}
	parsed = parsed.Unmap()
	for endpointIndex := range self.endpoints {
		endpoint := &self.endpoints[endpointIndex]
		if endpoint.address == parsed || endpoint.prefix.Contains(parsed) {
			return endpoint, true
		}
	}
	return nil, false
}

// Packet listeners are bound to the independent physical link named by the
// numeric local address.
func (self *streamP2pNodeNet) ListenPacket(network string, address string) (net.PacketConn, error) {
	localAddress, err := net.ResolveUDPAddr(network, address)
	if err != nil {
		return nil, err
	}
	return self.ListenUDP(network, localAddress)
}

// UDP listeners are the socket operation used by Pion host-candidate gather.
func (self *streamP2pNodeNet) ListenUDP(
	network string,
	localAddress *net.UDPAddr,
) (transport.UDPConn, error) {
	var localIP net.IP
	if localAddress != nil {
		localIP = localAddress.IP
	}
	endpoint, ok := self.endpointForAddress(localIP)
	if !ok {
		return nil, fmt.Errorf("stream P2P local address %s is not attached", localIP)
	}
	return endpoint.network.ListenUDP(network, localAddress)
}

// The WebRTC fixture is UDP-only.
func (self *streamP2pNodeNet) ListenTCP(
	network string,
	localAddress *net.TCPAddr,
) (transport.TCPListener, error) {
	_ = network
	_ = localAddress
	return nil, transport.ErrNotSupported
}

// Generic UDP dialing resolves through the same physical-link selector.
func (self *streamP2pNodeNet) Dial(network string, address string) (net.Conn, error) {
	remoteAddress, err := net.ResolveUDPAddr(network, address)
	if err != nil {
		return nil, err
	}
	return self.DialUDP(network, nil, remoteAddress)
}

// UDP dialing chooses the explicit local link first, then the remote subnet.
func (self *streamP2pNodeNet) DialUDP(
	network string,
	localAddress *net.UDPAddr,
	remoteAddress *net.UDPAddr,
) (transport.UDPConn, error) {
	var endpoint *streamP2pNetEndpoint
	var ok bool
	if localAddress != nil && localAddress.IP != nil && !localAddress.IP.IsUnspecified() {
		endpoint, ok = self.endpointForAddress(localAddress.IP)
	} else if remoteAddress != nil {
		endpoint, ok = self.endpointForAddress(remoteAddress.IP)
	}
	if !ok {
		self.nonAdjacent.recordDial(localAddress, remoteAddress)
		return nil, fmt.Errorf("stream P2P remote address %v is not adjacent", remoteAddress)
	}
	return endpoint.network.DialUDP(network, localAddress, remoteAddress)
}

// The WebRTC fixture is UDP-only.
func (self *streamP2pNodeNet) DialTCP(
	network string,
	localAddress *net.TCPAddr,
	remoteAddress *net.TCPAddr,
) (transport.TCPConn, error) {
	_ = network
	_ = localAddress
	_ = remoteAddress
	return nil, transport.ErrNotSupported
}

// Numeric host candidates use the standard resolver without external DNS.
func (self *streamP2pNodeNet) ResolveIPAddr(network string, address string) (*net.IPAddr, error) {
	return net.ResolveIPAddr(network, address)
}

// Numeric host candidates use the standard resolver without external DNS.
func (self *streamP2pNodeNet) ResolveUDPAddr(network string, address string) (*net.UDPAddr, error) {
	return net.ResolveUDPAddr(network, address)
}

// TCP resolution remains available to satisfy transport.Net even though this
// fixture does not create TCP sockets.
func (self *streamP2pNodeNet) ResolveTCPAddr(network string, address string) (*net.TCPAddr, error) {
	return net.ResolveTCPAddr(network, address)
}

// Interfaces returns stable synthetic names for every attached link.
func (self *streamP2pNodeNet) Interfaces() ([]*transport.Interface, error) {
	return append([]*transport.Interface(nil), self.interfaces...), nil
}

// Interface lookup uses the stable synthetic index.
func (self *streamP2pNodeNet) InterfaceByIndex(index int) (*transport.Interface, error) {
	for _, iface := range self.interfaces {
		if iface.Index == index {
			return iface, nil
		}
	}
	return nil, fmt.Errorf("%w: index=%d", transport.ErrInterfaceNotFound, index)
}

// Interface lookup uses the stable synthetic name.
func (self *streamP2pNodeNet) InterfaceByName(name string) (*transport.Interface, error) {
	for _, iface := range self.interfaces {
		if iface.Name == name {
			return iface, nil
		}
	}
	return nil, fmt.Errorf("%w: name=%s", transport.ErrInterfaceNotFound, name)
}

// The adapter's dialer retains address-based physical-link selection.
func (self *streamP2pNodeNet) CreateDialer(dialer *net.Dialer) transport.Dialer {
	_ = dialer
	return &streamP2pDialer{network: self}
}

// The adapter's listen configuration retains address-based link selection.
func (self *streamP2pNodeNet) CreateListenConfig(config *net.ListenConfig) transport.ListenConfig {
	_ = config
	return &streamP2pListenConfig{network: self}
}

// A transport dialer delegates to the composite network.
type streamP2pDialer struct {
	network *streamP2pNodeNet
}

// Dial retains the transport.Dialer surface.
func (self *streamP2pDialer) Dial(network string, address string) (net.Conn, error) {
	return self.network.Dial(network, address)
}

// A transport listen configuration delegates packet sockets to the composite
// network and rejects unused stream listeners.
type streamP2pListenConfig struct {
	network *streamP2pNodeNet
}

// Stream listeners are not used by WebRTC.
func (self *streamP2pListenConfig) Listen(
	ctx context.Context,
	network string,
	address string,
) (net.Listener, error) {
	_ = ctx
	_ = network
	_ = address
	return nil, transport.ErrNotSupported
}

// Packet listeners retain the caller's context ownership through socket close.
func (self *streamP2pListenConfig) ListenPacket(
	ctx context.Context,
	network string,
	address string,
) (net.PacketConn, error) {
	_ = ctx
	return self.network.ListenPacket(network, address)
}

// Every physical adjacency owns an independent vnet router. Intermediary
// clients expose both neighboring subnets through one transport.Net adapter.
type streamP2pNetwork struct {
	routers                  []*vnet.Router
	nets                     []transport.Net
	hopForwardLinks          []*directionalLink
	hopReverseLinks          []*directionalLink
	hopForwardReceiveCredits []*p2pReceiveCredits
	hopReverseReceiveCredits []*p2pReceiveCredits
	profile                  networkProfile
	closeOnce                sync.Once
	nonAdjacent              *streamP2pNonAdjacentTracker
}

// The chain maps one independent Pion router to every physical adjacency and
// applies the resolved directional rate, burst, and byte queue at its receiver.
func newStreamP2pNetwork(profile networkProfile, hopCount int) (*streamP2pNetwork, error) {
	if hopCount <= 0 || clientconnect.MaxMultihopLength+1 < hopCount {
		return nil, fmt.Errorf("stream P2P hop count=%d is outside 1..%d", hopCount, clientconnect.MaxMultihopLength+1)
	}
	network := &streamP2pNetwork{
		routers:                  make([]*vnet.Router, 0, hopCount),
		nets:                     make([]transport.Net, hopCount+1),
		hopForwardLinks:          make([]*directionalLink, hopCount),
		hopReverseLinks:          make([]*directionalLink, hopCount),
		hopForwardReceiveCredits: make([]*p2pReceiveCredits, hopCount),
		hopReverseReceiveCredits: make([]*p2pReceiveCredits, hopCount),
		profile:                  profile,
		nonAdjacent:              &streamP2pNonAdjacentTracker{},
	}
	nodeEndpoints := make([][]streamP2pNetEndpoint, hopCount+1)
	for hopIndex := range hopCount {
		network.hopForwardLinks[hopIndex] = newDirectionalLink(
			context.Background(),
			profile.Forward,
			profile.Seed+int64(2*hopIndex),
			nil,
		)
		network.hopReverseLinks[hopIndex] = newDirectionalLink(
			context.Background(),
			profile.Reverse,
			profile.Seed+int64(2*hopIndex+1),
			nil,
		)
		network.hopForwardReceiveCredits[hopIndex] = newP2pVnetReceiveCredits(
			p2pVnetReceiveCreditPacketCount,
		)
		network.hopReverseReceiveCredits[hopIndex] = newP2pVnetReceiveCredits(
			p2pVnetReceiveCreditPacketCount,
		)
		prefix, err := netip.ParsePrefix(fmt.Sprintf("10.241.%d.0/24", hopIndex))
		if err != nil {
			network.close()
			return nil, err
		}
		leftAddress := netip.AddrFrom4([4]byte{10, 241, byte(hopIndex), 1})
		rightAddress := netip.AddrFrom4([4]byte{10, 241, byte(hopIndex), 2})
		router, err := vnet.NewRouter(&vnet.RouterConfig{
			CIDR:          prefix.String(),
			QueueSize:     0,
			MinDelay:      0,
			MaxJitter:     0,
			LoggerFactory: logging.NewDefaultLoggerFactory(),
		})
		if err != nil {
			network.close()
			return nil, err
		}
		network.routers = append(network.routers, router)
		leftNet, err := vnet.NewNet(&vnet.NetConfig{StaticIPs: []string{leftAddress.String()}})
		if err != nil {
			network.close()
			return nil, err
		}
		rightNet, err := vnet.NewNet(&vnet.NetConfig{StaticIPs: []string{rightAddress.String()}})
		if err != nil {
			network.close()
			return nil, err
		}
		if err := router.AddNet(leftNet); err != nil {
			network.close()
			return nil, err
		}
		if err := router.AddNet(rightNet); err != nil {
			network.close()
			return nil, err
		}
		router.AddChunkFilter(func(chunk vnet.Chunk) bool {
			return network.filter(hopIndex, leftAddress, rightAddress, chunk)
		})
		if err := router.Start(); err != nil {
			network.close()
			return nil, err
		}
		leftLinkNet := newP2pLinkNetWithUntrackedDestinationObserver(
			leftNet,
			network.hopForwardLinks[hopIndex],
			func(sourceAddress net.Addr, destinationAddress net.Addr, packet []byte) {
				network.recordUntrackedDestination(
					hopIndex,
					leftAddress,
					rightAddress,
					sourceAddress,
					destinationAddress,
					packet,
				)
			},
			network.hopForwardReceiveCredits[hopIndex],
			network.hopReverseReceiveCredits[hopIndex],
		)
		leftLinkNet.setSourceIPv4ForWildcardBinds(leftAddress)
		rightLinkNet := newP2pLinkNetWithUntrackedDestinationObserver(
			rightNet,
			network.hopReverseLinks[hopIndex],
			func(sourceAddress net.Addr, destinationAddress net.Addr, packet []byte) {
				network.recordUntrackedDestination(
					hopIndex,
					leftAddress,
					rightAddress,
					sourceAddress,
					destinationAddress,
					packet,
				)
			},
			network.hopReverseReceiveCredits[hopIndex],
			network.hopForwardReceiveCredits[hopIndex],
		)
		rightLinkNet.setSourceIPv4ForWildcardBinds(rightAddress)
		nodeEndpoints[hopIndex] = append(nodeEndpoints[hopIndex], streamP2pNetEndpoint{
			network: leftLinkNet,
			address: leftAddress,
			prefix:  prefix,
		})
		nodeEndpoints[hopIndex+1] = append(nodeEndpoints[hopIndex+1], streamP2pNetEndpoint{
			network: rightLinkNet,
			address: rightAddress,
			prefix:  prefix,
		})
	}
	for nodeIndex, endpoints := range nodeEndpoints {
		network.nets[nodeIndex] = newStreamP2pNodeNet(endpoints, network.nonAdjacent)
	}
	return network, nil
}

// A destination-aware credit miss is classified before its delegate write.
// Adjacent sockets that have not opened yet are ordinary receiver drops;
// traffic outside the physical pair remains explicit topology evidence.
func (self *streamP2pNetwork) recordUntrackedDestination(
	hopIndex int,
	leftAddress netip.Addr,
	rightAddress netip.Addr,
	sourceAddress net.Addr,
	destinationAddress net.Addr,
	packet []byte,
) {
	source, sourceOk := sourceAddress.(*net.UDPAddr)
	destination, destinationOk := destinationAddress.(*net.UDPAddr)
	if sourceOk && destinationOk {
		sourceValue, sourceFound := netip.AddrFromSlice(source.IP)
		destinationValue, destinationFound := netip.AddrFromSlice(destination.IP)
		if sourceFound && destinationFound {
			sourceValue = sourceValue.Unmap()
			destinationValue = destinationValue.Unmap()
			if (sourceValue == leftAddress && destinationValue == rightAddress) ||
				(sourceValue == rightAddress && destinationValue == leftAddress) {
				return
			}
		}
	}
	self.nonAdjacent.recordPacket(
		hopIndex,
		sourceAddress,
		destinationAddress,
		packet,
	)
}

// The filter accepts only the two addresses on one physical router. All packet
// conditioning happens once at the sender-side directional wrapper; retaining
// a filter here would duplicate loss, delay, MTU, and queue behavior.
func (self *streamP2pNetwork) filter(
	hopIndex int,
	leftAddress netip.Addr,
	rightAddress netip.Addr,
	chunk vnet.Chunk,
) bool {
	source, sourceOk := chunk.SourceAddr().(*net.UDPAddr)
	destination, destinationOk := chunk.DestinationAddr().(*net.UDPAddr)
	if !sourceOk || !destinationOk {
		return true
	}
	sourceAddress, sourceFound := netip.AddrFromSlice(source.IP)
	destinationAddress, destinationFound := netip.AddrFromSlice(destination.IP)
	if !sourceFound || !destinationFound {
		self.nonAdjacent.recordPacket(
			hopIndex,
			chunk.SourceAddr(),
			chunk.DestinationAddr(),
			chunk.UserData(),
		)
		return false
	}
	sourceAddress = sourceAddress.Unmap()
	destinationAddress = destinationAddress.Unmap()
	adjacentForward := sourceAddress == leftAddress && destinationAddress == rightAddress
	adjacentReverse := sourceAddress == rightAddress && destinationAddress == leftAddress
	if adjacentForward {
		return self.hopForwardReceiveCredits[hopIndex].acceptRouterPayload(
			chunk.SourceAddr(),
			chunk.DestinationAddr(),
			chunk.UserData(),
		)
	}
	if adjacentReverse {
		return self.hopReverseReceiveCredits[hopIndex].acceptRouterPayload(
			chunk.SourceAddr(),
			chunk.DestinationAddr(),
			chunk.UserData(),
		)
	}
	self.nonAdjacent.recordPacket(
		hopIndex,
		chunk.SourceAddr(),
		chunk.DestinationAddr(),
		chunk.UserData(),
	)
	return false
}

// One stream direction derives packet-size and drop observations from the same
// link snapshot that owns its interval identity.
func newStreamP2pDirectionSnapshot(
	profile linkProfile,
	linkSnapshot directionalLinkSnapshot,
	receiveCredits p2pReceiveCreditSnapshot,
) streamP2pDirectionSnapshot {
	return streamP2pDirectionSnapshot{
		ConfiguredRateBitsPerSecond: profile.RateBitsPerSecond,
		ConfiguredBurstByteCount:    profile.BurstByteCount,
		ConfiguredQueueByteCount:    profile.QueueByteCount,
		Link:                        linkSnapshot,
		ReceiveCredits:              receiveCredits,
		PacketCount:                 linkSnapshot.submittedPacketCount,
		PacketByteCount:             linkSnapshot.AdmittedByteCount,
		DropCount:                   p2pLinkDropCount(linkSnapshot),
		MtuDropCount:                linkSnapshot.MtuDropPacketCount,
		MaximumPacketByteCount:      uint64(linkSnapshot.MaximumSubmittedPacketBytes),
	}
}

// A snapshot preserves each physical carrier instead of collapsing all stream
// traffic into endpoint totals.
func (self *streamP2pNetwork) snapshot() []streamP2pHopSnapshot {
	hops := make([]streamP2pHopSnapshot, len(self.hopForwardLinks))
	for hopIndex := range hops {
		hops[hopIndex] = streamP2pHopSnapshot{
			HopIndex: hopIndex,
			Forward: newStreamP2pDirectionSnapshot(
				self.profile.Forward,
				self.hopForwardLinks[hopIndex].snapshot(),
				self.hopForwardReceiveCredits[hopIndex].snapshot(),
			),
			Reverse: newStreamP2pDirectionSnapshot(
				self.profile.Reverse,
				self.hopReverseLinks[hopIndex].snapshot(),
				self.hopReverseReceiveCredits[hopIndex].snapshot(),
			),
		}
	}
	return hops
}

// Reset and baseline are one lock-held operation for every stream direction
// and receive pool; a route-wide generation check retries cross-hop traffic.
func (self *streamP2pNetwork) beginMeasurementSnapshot(
	ctx context.Context,
) ([]streamP2pHopSnapshot, bool) {
	hops := make([]streamP2pHopSnapshot, len(self.hopForwardLinks))
	for hopIndex := range self.hopForwardLinks {
		forward, ok := self.hopForwardLinks[hopIndex].beginMeasurementSnapshot(ctx)
		if !ok {
			return nil, false
		}
		reverse, ok := self.hopReverseLinks[hopIndex].beginMeasurementSnapshot(ctx)
		if !ok {
			return nil, false
		}
		forwardReceiveCredits, ok :=
			self.hopForwardReceiveCredits[hopIndex].beginMeasurementSnapshot(ctx)
		if !ok {
			return nil, false
		}
		reverseReceiveCredits, ok :=
			self.hopReverseReceiveCredits[hopIndex].beginMeasurementSnapshot(ctx)
		if !ok {
			return nil, false
		}
		hops[hopIndex] = streamP2pHopSnapshot{
			HopIndex: hopIndex,
			Forward: newStreamP2pDirectionSnapshot(
				self.profile.Forward,
				forward,
				forwardReceiveCredits,
			),
			Reverse: newStreamP2pDirectionSnapshot(
				self.profile.Reverse,
				reverse,
				reverseReceiveCredits,
			),
		}
	}
	return hops, true
}

// Stream interval records subtract monotonic counters, retain the resolved hop
// index, and read maxima from the exact workload epoch.
func subtractStreamP2pHopSnapshots(
	before []streamP2pHopSnapshot,
	after []streamP2pHopSnapshot,
	duration time.Duration,
) []streamP2pHopSnapshot {
	hops := make([]streamP2pHopSnapshot, len(after))
	subtractDirection := func(
		start streamP2pDirectionSnapshot,
		end streamP2pDirectionSnapshot,
	) streamP2pDirectionSnapshot {
		link := subtractDirectionalLinkSnapshot(start.Link, end.Link, duration)
		return streamP2pDirectionSnapshot{
			ConfiguredRateBitsPerSecond: end.ConfiguredRateBitsPerSecond,
			ConfiguredBurstByteCount:    end.ConfiguredBurstByteCount,
			ConfiguredQueueByteCount:    end.ConfiguredQueueByteCount,
			Link:                        link,
			ReceiveCredits:              subtractP2pReceiveCreditSnapshots(start.ReceiveCredits, end.ReceiveCredits),
			PacketCount:                 end.PacketCount - start.PacketCount,
			PacketByteCount:             end.PacketByteCount - start.PacketByteCount,
			DropCount:                   end.DropCount - start.DropCount,
			MtuDropCount:                end.MtuDropCount - start.MtuDropCount,
			MaximumPacketByteCount:      uint64(link.MaximumSubmittedPacketBytes),
		}
	}
	for hopIndex, end := range after {
		start := streamP2pHopSnapshot{HopIndex: end.HopIndex}
		if hopIndex < len(before) {
			start = before[hopIndex]
		}
		hops[hopIndex] = streamP2pHopSnapshot{
			HopIndex: end.HopIndex,
			Forward:  subtractDirection(start.Forward, end.Forward),
			Reverse:  subtractDirection(start.Reverse, end.Reverse),
		}
	}
	return hops
}

// Every physical hop and direction participates in route-wide terminal idle.
func (self *streamP2pNetwork) directionalLinks() []*directionalLink {
	links := make([]*directionalLink, 0, 2*len(self.hopForwardLinks))
	for hopIndex := range self.hopForwardLinks {
		links = append(links, self.hopForwardLinks[hopIndex], self.hopReverseLinks[hopIndex])
	}
	return links
}

// Every physical direction contributes one destination admission pool.
func (self *streamP2pNetwork) receiveCreditPools() []*p2pReceiveCredits {
	creditPools := make([]*p2pReceiveCredits, 0, 2*len(self.hopForwardReceiveCredits))
	for hopIndex := range self.hopForwardReceiveCredits {
		creditPools = append(
			creditPools,
			self.hopForwardReceiveCredits[hopIndex],
			self.hopReverseReceiveCredits[hopIndex],
		)
	}
	return creditPools
}

// The fixed-point link barrier includes every direction of every physical hop.
func (self *streamP2pNetwork) waitForTerminalIdle(ctx context.Context) bool {
	return waitForP2pTerminalIdle(ctx, self.directionalLinks(), self.receiveCreditPools(), nil)
}

// Closing first joins each directional scheduler, then stops the bare routers.
func (self *streamP2pNetwork) close() {
	self.closeOnce.Do(func() {
		for _, credits := range self.receiveCreditPools() {
			if credits != nil {
				credits.close()
			}
		}
		for _, link := range self.directionalLinks() {
			if link != nil {
				link.close()
			}
		}
		for _, router := range self.routers {
			if router != nil {
				_ = router.Stop()
			}
		}
	})
}

// Every adjacency retains the LTE uplink/downlink asymmetry instead of using
// one collapsed chain-wide shaping value.
func TestStreamP2pNetworkPreservesAsymmetricPerHopShaping(t *testing.T) {
	profile := initialNetworkProfiles(3300)["lte"]
	network, err := newStreamP2pNetwork(profile, 3)
	if err != nil {
		t.Fatalf("create asymmetric stream P2P network: %v", err)
	}
	defer network.close()
	hops := network.snapshot()
	if len(hops) != 3 {
		t.Fatalf("stream P2P hop count=%d", len(hops))
	}
	for hopIndex, hop := range hops {
		if hop.Forward.ConfiguredRateBitsPerSecond != profile.Forward.RateBitsPerSecond ||
			hop.Forward.ConfiguredBurstByteCount != profile.Forward.BurstByteCount ||
			hop.Forward.ConfiguredQueueByteCount != profile.Forward.QueueByteCount {
			t.Fatalf("hop %d forward shaping=%+v profile=%+v", hopIndex, hop.Forward, profile.Forward)
		}
		if hop.Reverse.ConfiguredRateBitsPerSecond != profile.Reverse.RateBitsPerSecond ||
			hop.Reverse.ConfiguredBurstByteCount != profile.Reverse.BurstByteCount ||
			hop.Reverse.ConfiguredQueueByteCount != profile.Reverse.QueueByteCount {
			t.Fatalf("hop %d reverse shaping=%+v profile=%+v", hopIndex, hop.Reverse, profile.Reverse)
		}
		if hop.Forward.ConfiguredRateBitsPerSecond == hop.Reverse.ConfiguredRateBitsPerSecond {
			t.Fatalf("hop %d collapsed LTE direction rates: %+v", hopIndex, hop)
		}
		if hop.Forward.ReceiveCredits.CapacityPacketCount != p2pVnetReceiveCreditPacketCount ||
			hop.Reverse.ReceiveCredits.CapacityPacketCount != p2pVnetReceiveCreditPacketCount ||
			hop.Forward.ReceiveCredits.Closed || hop.Reverse.ReceiveCredits.Closed {
			t.Fatalf("hop %d receive admission=%+v", hopIndex, hop)
		}
	}
	for nodeIndex, nodeNet := range network.nets {
		composite, ok := nodeNet.(*streamP2pNodeNet)
		if !ok {
			t.Fatalf("node %d transport type=%T", nodeIndex, nodeNet)
		}
		expectedEndpointCount := 2
		if nodeIndex == 0 || nodeIndex == len(network.nets)-1 {
			expectedEndpointCount = 1
		}
		if len(composite.endpoints) != expectedEndpointCount {
			t.Fatalf("node %d endpoint count=%d", nodeIndex, len(composite.endpoints))
		}
	}
}

// Concurrent MTU rejection and measurement must preserve the full per-cause
// directional snapshot and its compatibility aggregates on every hop.
func TestStreamP2pMultihopDropSnapshotsRemainConsistent(t *testing.T) {
	const hopCount = 3
	const dropCountPerDirection = 2500
	profile := initialNetworkProfiles(3304)["clean-lan"]
	profile.Forward.OuterMtu = 1280
	profile.Reverse.OuterMtu = 1280
	network, err := newStreamP2pNetwork(profile, hopCount)
	if err != nil {
		t.Fatalf("create stream P2P network: %v", err)
	}
	defer network.close()
	var writers sync.WaitGroup
	start := make(chan struct{})
	for hopIndex := range hopCount {
		for _, link := range []*directionalLink{
			network.hopForwardLinks[hopIndex],
			network.hopReverseLinks[hopIndex],
		} {
			writers.Add(1)
			go func() {
				defer writers.Done()
				<-start
				for range dropCountPerDirection {
					_, _ = link.submitOwnedWithDeliver(make([]byte, 1281), nil)
				}
			}()
		}
	}
	done := make(chan struct{})
	go func() {
		writers.Wait()
		close(done)
	}()
	close(start)
	for {
		for _, hop := range network.snapshot() {
			for directionIndex, direction := range []streamP2pDirectionSnapshot{
				hop.Forward,
				hop.Reverse,
			} {
				if direction.DropCount != p2pLinkDropCount(direction.Link) ||
					direction.MtuDropCount != direction.Link.MtuDropPacketCount {
					t.Fatalf(
						"hop %d direction %d torn drop snapshot: %+v",
						hop.HopIndex,
						directionIndex,
						direction,
					)
				}
			}
		}
		select {
		case <-done:
			for _, hop := range network.snapshot() {
				for _, direction := range []streamP2pDirectionSnapshot{hop.Forward, hop.Reverse} {
					if direction.Link.MtuDropPacketCount != dropCountPerDirection ||
						direction.DropCount != dropCountPerDirection {
						t.Fatalf("final multihop MTU attribution=%+v", direction)
					}
				}
			}
			return
		default:
		}
	}
}

// Pins the harness boundary between expected ICE exploration and forbidden
// application traffic. Both classes are dropped, but only data invalidates a
// topology measurement.
func TestStreamP2pNonAdjacentTrafficClassification(t *testing.T) {
	tracker := &streamP2pNonAdjacentTracker{}
	sourceAddress := &net.UDPAddr{IP: net.ParseIP("10.1.0.1"), Port: 10001}
	destinationAddress := &net.UDPAddr{IP: net.ParseIP("10.9.0.2"), Port: 10002}
	stunPacket := make([]byte, 20)
	binary.BigEndian.PutUint16(stunPacket[0:2], 0x0001)
	binary.BigEndian.PutUint32(stunPacket[4:8], 0x2112a442)
	tracker.recordPacket(2, sourceAddress, destinationAddress, stunPacket)
	tracker.recordPacket(
		2,
		sourceAddress,
		destinationAddress,
		[]byte("application carrier"),
	)
	nodeNetwork := newStreamP2pNodeNet(nil, tracker)
	_, err := nodeNetwork.DialUDP("udp4", nil, destinationAddress)
	if err == nil {
		t.Fatal("non-adjacent candidate dial unexpectedly succeeded")
	}

	snapshot := tracker.snapshot()
	if snapshot.DialCount != 1 || snapshot.StunPacketDropCount != 1 ||
		snapshot.DataPacketDropCount != 1 || len(snapshot.EventStrings) != 3 {
		t.Fatalf("non-adjacent classification mismatch: %+v", snapshot)
	}
	if !isStunDatagram(stunPacket) || isStunDatagram([]byte("application carrier")) {
		t.Fatal("STUN packet classifier did not preserve the protocol boundary")
	}
	badCookiePacket := append([]byte(nil), stunPacket...)
	badCookiePacket[7] = 0
	badLengthPacket := append([]byte(nil), stunPacket...)
	binary.BigEndian.PutUint16(badLengthPacket[2:4], 4)
	trailingDataPacket := append(append([]byte(nil), stunPacket...), 0)
	if isStunDatagram(badCookiePacket) || isStunDatagram(badLengthPacket) ||
		isStunDatagram(trailingDataPacket) {
		t.Fatal("malformed or trailing data was classified as STUN")
	}

	carrier := perfvarCarrierObservation{
		StreamP2PHops:                  make([]streamP2pHopSnapshot, 3),
		StreamP2PClientStats:           make([]clientconnect.P2pDataPlaneStatsSnapshot, 4),
		StreamNonAdjacentDialCount:     snapshot.DialCount,
		StreamNonAdjacentStunDropCount: snapshot.StunPacketDropCount,
		StreamNonAdjacentDataDropCount: snapshot.DataPacketDropCount,
	}
	verificationErr := verifyPerfvarTopologyCarrier(perfvarScenario{
		Route:     fullTunRouteP2pFast,
		Direction: perfvarDirectionUpload,
		Topology:  perfvarTopologyThreeHop,
	}, carrier, 1)
	expectedError := fmt.Sprintf(
		"three-hop attempted 1 non-adjacent application packets (ICE dials=1 STUN drops=1)",
	)
	if verificationErr == nil || verificationErr.Error() != expectedError {
		t.Fatalf("non-adjacent data was not rejected: %v", verificationErr)
	}
}

// Every physical hop and direction rejects an adjacent address with no live
// socket before vnet, leaving no phantom receive reservation behind.
func TestStreamP2pEveryHopMissingDestinationHasNoReceiveCredits(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	link := testP2pLinkProfile(1500, oversizeModeDrop)
	profile := networkProfile{Name: "stream-missing-destination", Seed: 8301, Forward: link, Reverse: link}
	network, err := newStreamP2pNetwork(profile, 3)
	if err != nil {
		t.Fatalf("create stream missing-destination network: %v", err)
	}
	defer network.close()
	for hopIndex := range 3 {
		forwardSource := &net.UDPAddr{
			IP:   net.IPv4(10, 241, byte(hopIndex), 1),
			Port: 53000 + hopIndex,
		}
		forwardDestination := &net.UDPAddr{
			IP:   net.IPv4(10, 241, byte(hopIndex), 2),
			Port: 54000 + hopIndex,
		}
		forward, dialErr := network.nets[hopIndex].DialUDP(
			"udp4",
			forwardSource,
			forwardDestination,
		)
		if dialErr != nil {
			t.Fatalf("dial stream hop %d forward missing destination: %v", hopIndex, dialErr)
		}
		if writtenByteCount, writeErr := forward.Write([]byte{byte(hopIndex)}); writeErr != nil ||
			writtenByteCount != 1 {
			t.Fatalf("write stream hop %d forward bytes=%d err=%v", hopIndex, writtenByteCount, writeErr)
		}
		if err := forward.Close(); err != nil {
			t.Fatalf("close stream hop %d forward sender: %v", hopIndex, err)
		}

		reverseSource := &net.UDPAddr{
			IP:   net.IPv4(10, 241, byte(hopIndex), 2),
			Port: 53100 + hopIndex,
		}
		reverseDestination := &net.UDPAddr{
			IP:   net.IPv4(10, 241, byte(hopIndex), 1),
			Port: 54100 + hopIndex,
		}
		reverse, dialErr := network.nets[hopIndex+1].DialUDP(
			"udp4",
			reverseSource,
			reverseDestination,
		)
		if dialErr != nil {
			t.Fatalf("dial stream hop %d reverse missing destination: %v", hopIndex, dialErr)
		}
		if writtenByteCount, writeErr := reverse.Write([]byte{byte(hopIndex)}); writeErr != nil ||
			writtenByteCount != 1 {
			t.Fatalf("write stream hop %d reverse bytes=%d err=%v", hopIndex, writtenByteCount, writeErr)
		}
		if err := reverse.Close(); err != nil {
			t.Fatalf("close stream hop %d reverse sender: %v", hopIndex, err)
		}
	}
	if !network.waitForTerminalIdle(ctx) {
		t.Fatalf("join stream missing-destination links: %v", ctx.Err())
	}
	for hopIndex := range 3 {
		forwardCredits := network.hopForwardReceiveCredits[hopIndex].snapshot()
		reverseCredits := network.hopReverseReceiveCredits[hopIndex].snapshot()
		if forwardCredits.AdmittedPacketCount != 0 || forwardCredits.OutstandingPacketCount != 0 ||
			forwardCredits.TrackedReservationCount != 0 ||
			reverseCredits.AdmittedPacketCount != 0 || reverseCredits.OutstandingPacketCount != 0 ||
			reverseCredits.TrackedReservationCount != 0 {
			t.Fatalf(
				"stream hop %d missing-destination credits forward=%+v reverse=%+v",
				hopIndex,
				forwardCredits,
				reverseCredits,
			)
		}
		if forward := network.hopForwardLinks[hopIndex].snapshot(); forward.ReceiverDropPacketCount != 1 || forward.DeliveredPacketCount != 0 {
			t.Fatalf("stream hop %d forward disposition=%+v", hopIndex, forward)
		}
		if reverse := network.hopReverseLinks[hopIndex].snapshot(); reverse.ReceiverDropPacketCount != 1 || reverse.DeliveredPacketCount != 0 {
			t.Fatalf("stream hop %d reverse disposition=%+v", hopIndex, reverse)
		}
	}
	if nonAdjacent := network.nonAdjacent.snapshot(); nonAdjacent.StunPacketDropCount != 0 || nonAdjacent.DataPacketDropCount != 0 {
		t.Fatalf("adjacent missing destinations were classified non-adjacent: %+v", nonAdjacent)
	}
}

// Every physical stream hop applies the same socket generation boundary. A
// packet paused before hop zero's final revalidation cannot reach a same-tuple
// replacement on the adjacent node.
func TestStreamP2pRouterInflightPacketCannotCrossSocketGeneration(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	link := testP2pLinkProfile(1500, oversizeModeDrop)
	profile := networkProfile{
		Name:    "stream-router-inflight-generation",
		Seed:    7211,
		Forward: link,
		Reverse: link,
	}
	network, err := newStreamP2pNetwork(profile, 3)
	if err != nil {
		t.Fatalf("create stream router-inflight network: %v", err)
	}
	defer network.close()
	source := &net.UDPAddr{IP: net.IPv4(10, 241, 0, 1), Port: 5541}
	destination := &net.UDPAddr{IP: net.IPv4(10, 241, 0, 2), Port: 5542}
	oldPayload := []byte("old-stream-generation")
	newPayload := []byte("new-stream-generation")
	filterEntered := make(chan struct{})
	releaseFilter := make(chan struct{})
	var filterOnce sync.Once
	network.hopForwardReceiveCredits[0].beforeRouterCompletionForTest = func(payload []byte) {
		if len(payload) == p2pVnetGenerationHeaderByteCount+len(oldPayload) &&
			bytes.Equal(payload[p2pVnetGenerationHeaderByteCount:], oldPayload) {
			filterOnce.Do(func() { close(filterEntered) })
			<-releaseFilter
		}
	}
	oldReceiver, err := network.nets[1].ListenUDP("udp4", destination)
	if err != nil {
		close(releaseFilter)
		t.Fatalf("listen old stream generation: %v", err)
	}
	sender, err := network.nets[0].DialUDP("udp4", source, destination)
	if err != nil {
		_ = oldReceiver.Close()
		close(releaseFilter)
		t.Fatalf("dial stream generation: %v", err)
	}
	defer sender.Close()
	if writtenByteCount, writeErr := sender.Write(oldPayload); writeErr != nil ||
		writtenByteCount != len(oldPayload) {
		_ = oldReceiver.Close()
		close(releaseFilter)
		t.Fatalf("write old stream generation bytes=%d err=%v", writtenByteCount, writeErr)
	}
	select {
	case <-ctx.Done():
		_ = oldReceiver.Close()
		close(releaseFilter)
		t.Fatalf("wait for stream router filter: %v", ctx.Err())
	case <-filterEntered:
	}
	if err := oldReceiver.Close(); err != nil {
		close(releaseFilter)
		t.Fatalf("close old stream generation: %v", err)
	}
	newReceiver, err := network.nets[1].ListenUDP("udp4", destination)
	if err != nil {
		close(releaseFilter)
		t.Fatalf("rebind stream generation: %v", err)
	}
	defer newReceiver.Close()
	if deadline, ok := ctx.Deadline(); ok {
		if err := newReceiver.SetReadDeadline(deadline); err != nil {
			close(releaseFilter)
			t.Fatalf("set stream generation read deadline: %v", err)
		}
	}
	routerWaitEntered := make(chan struct{})
	var routerWaitOnce sync.Once
	network.hopForwardReceiveCredits[0].routerPendingObservedForTest = func() {
		routerWaitOnce.Do(func() { close(routerWaitEntered) })
	}
	terminalJoined := make(chan bool, 1)
	go func() {
		terminalJoined <- network.waitForTerminalIdle(ctx)
	}()
	select {
	case <-ctx.Done():
		close(releaseFilter)
		t.Fatalf("wait for stream router-pending terminal barrier: %v", ctx.Err())
	case <-routerWaitEntered:
	}
	select {
	case joined := <-terminalJoined:
		close(releaseFilter)
		t.Fatalf("stream terminal barrier returned before router completion: %t", joined)
	default:
	}
	close(releaseFilter)
	select {
	case <-ctx.Done():
		t.Fatalf("join stream router-pending terminal barrier: %v", ctx.Err())
	case joined := <-terminalJoined:
		if !joined {
			t.Fatal("stream router-pending terminal barrier rejected exact state")
		}
	}
	if writtenByteCount, writeErr := sender.Write(newPayload); writeErr != nil ||
		writtenByteCount != len(newPayload) {
		t.Fatalf("write new stream generation bytes=%d err=%v", writtenByteCount, writeErr)
	}
	payload := make([]byte, len(newPayload))
	readByteCount, _, err := newReceiver.ReadFromUDP(payload)
	if err != nil || readByteCount != len(newPayload) || string(payload) != string(newPayload) {
		t.Fatalf(
			"replacement stream generation bytes=%d payload=%q err=%v",
			readByteCount,
			payload,
			err,
		)
	}
	if !network.waitForTerminalIdle(ctx) {
		t.Fatalf("join stream router generation: %v", ctx.Err())
	}
	snapshot := network.hopForwardReceiveCredits[0].snapshot()
	if snapshot.AdmittedPacketCount != 2 || snapshot.ReadPacketCount != 1 ||
		snapshot.CanceledPacketCount != 1 || snapshot.StaleGenerationDropCount != 1 ||
		snapshot.OutstandingPacketCount != 0 || snapshot.TrackedReservationCount != 0 ||
		snapshot.InvalidReleasePacketCount != 0 || !snapshot.isExactLiveTerminal() {
		t.Fatalf("stream router generation credits=%+v", snapshot)
	}
}

// A real composite-node socket with an explicit local link classifies
// non-adjacent STUN and data at the destination registry miss, without relying
// on the later asynchronous vnet router filter.
func TestStreamP2pNonAdjacentSocketTrafficClassification(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	link := testP2pLinkProfile(1500, oversizeModeDrop)
	profile := networkProfile{Name: "stream-non-adjacent-socket", Seed: 8302, Forward: link, Reverse: link}
	network, err := newStreamP2pNetwork(profile, 3)
	if err != nil {
		t.Fatalf("create non-adjacent socket network: %v", err)
	}
	defer network.close()
	localAddress := &net.UDPAddr{IP: net.IPv4(10, 241, 0, 2), Port: 53200}
	remoteAddress := &net.UDPAddr{IP: net.IPv4(10, 241, 2, 2), Port: 53201}
	connection, err := network.nets[1].DialUDP("udp4", localAddress, remoteAddress)
	if err != nil {
		t.Fatalf("dial explicit non-adjacent stream socket: %v", err)
	}
	defer connection.Close()
	stunPacket := make([]byte, 20)
	binary.BigEndian.PutUint16(stunPacket[0:2], 0x0001)
	binary.BigEndian.PutUint32(stunPacket[4:8], 0x2112a442)
	for _, packet := range [][]byte{stunPacket, []byte("non-adjacent-data")} {
		if writtenByteCount, writeErr := connection.Write(packet); writeErr != nil ||
			writtenByteCount != len(packet) {
			t.Fatalf("write non-adjacent stream packet bytes=%d err=%v", writtenByteCount, writeErr)
		}
	}
	if !network.hopReverseLinks[0].waitIdle(ctx) {
		t.Fatalf("join non-adjacent stream packets: %v", ctx.Err())
	}
	nonAdjacent := network.nonAdjacent.snapshot()
	if nonAdjacent.DialCount != 0 || nonAdjacent.StunPacketDropCount != 1 ||
		nonAdjacent.DataPacketDropCount != 1 {
		t.Fatalf("socket non-adjacent classification=%+v", nonAdjacent)
	}
	credits := network.hopReverseReceiveCredits[0].snapshot()
	if credits.AdmittedPacketCount != 0 || credits.OutstandingPacketCount != 0 ||
		credits.TrackedReservationCount != 0 {
		t.Fatalf("non-adjacent socket consumed credits=%+v", credits)
	}
	linkSnapshot := network.hopReverseLinks[0].snapshot()
	if linkSnapshot.ReceiverDropPacketCount != 2 || linkSnapshot.DeliveredPacketCount != 0 {
		t.Fatalf("non-adjacent socket link=%+v", linkSnapshot)
	}
}

// Carrier verification accepts compressed wire bytes below the exact useful
// payload size while still requiring traffic at every node and adjacency.
func TestVerifyPerfvarTopologyCarrierAllowsCompressedWireBytes(t *testing.T) {
	carrier := perfvarCarrierObservation{
		StreamP2PHops:        make([]streamP2pHopSnapshot, 3),
		StreamP2PClientStats: make([]clientconnect.P2pDataPlaneStatsSnapshot, 4),
	}
	for clientIndex := range carrier.StreamP2PClientStats {
		carrier.StreamP2PClientStats[clientIndex] = clientconnect.P2pDataPlaneStatsSnapshot{
			FastSendMessageCount:    1,
			FastSendByteCount:       1,
			FastReceiveMessageCount: 1,
			FastReceiveByteCount:    1,
		}
	}
	for hopIndex := range carrier.StreamP2PHops {
		carrier.StreamP2PHops[hopIndex] = streamP2pHopSnapshot{
			HopIndex: hopIndex,
			Forward: streamP2pDirectionSnapshot{
				PacketCount:     1,
				PacketByteCount: 1,
			},
			Reverse: streamP2pDirectionSnapshot{
				PacketCount:     1,
				PacketByteCount: 1,
			},
		}
	}

	err := verifyPerfvarTopologyCarrier(perfvarScenario{
		Route:     fullTunRouteP2pFast,
		Direction: perfvarDirectionUpload,
		Topology:  perfvarTopologyThreeHop,
	}, carrier, 64*1024)
	if err != nil {
		t.Fatalf("compressed carrier attribution failed: %v", err)
	}
}

// One receive probe creates the multi-hop contract and waits until every
// adjacent production fast carrier has transported at least one message.
func waitForProductionStreamP2p(
	ctx context.Context,
	source *routeClient,
	destination *routeClient,
	path clientconnect.MultiHopId,
	clients []*routeClient,
) (clientconnect.Id, error) {
	received := make(chan clientconnect.TransferPath, 1)
	unsub := destination.client.AddReceiveCallback(func(
		transferPath clientconnect.TransferPath,
		frames []*protocol.Frame,
		peer clientconnect.Peer,
	) {
		_ = peer
		if transferPath.SourceId != clientconnect.Id(source.clientId) {
			return
		}
		for _, frame := range frames {
			if frame.MessageType == protocol.MessageType_TestSimpleMessage {
				select {
				case received <- transferPath:
				default:
				}
			}
		}
	})
	defer unsub()
	deadline := time.NewTimer(2 * time.Minute)
	defer deadline.Stop()
	sendProbe := func(probeIndex int) error {
		frame, err := clientconnect.ToFrame(
			&protocol.SimpleMessage{Content: fmt.Sprintf("perfvar stream setup %d", probeIndex)},
			clientconnect.DefaultProtocolVersion,
		)
		if err != nil {
			return err
		}
		if !source.client.SendMultiHopWithTimeout(
			frame,
			path,
			nil,
			60*time.Second,
			clientconnect.ForceStream(),
		) {
			clientconnect.MessagePoolReturn(frame.MessageBytes)
			return fmt.Errorf("multi-hop setup send %d failed", probeIndex)
		}
		return nil
	}
	if err := sendProbe(0); err != nil {
		return clientconnect.Id{}, err
	}
	var streamId clientconnect.Id
	select {
	case <-ctx.Done():
		return clientconnect.Id{}, ctx.Err()
	case <-deadline.C:
		return clientconnect.Id{}, fmt.Errorf("initial multi-hop platform delivery timed out")
	case receivedPath := <-received:
		streamId = receivedPath.StreamId
	}
	if streamId == (clientconnect.Id{}) {
		// Application frames retain their original source/final-destination
		// wire path. The signed contract and StreamOpen controls carry the
		// stream id used by every adjacent P2P transport, so observe the source
		// hop directly from the production server model.
		_, streamHops := model.GetStreamHops(ctx, source.clientId)
		for hop := range streamHops {
			hopPath := hop.Path()
			if hopPath.DestinationId == clientconnect.Id(clients[1].clientId) {
				streamId = hopPath.StreamId
				break
			}
		}
	}
	if streamId == (clientconnect.Id{}) {
		return clientconnect.Id{}, fmt.Errorf("source production stream hop was not recorded")
	}
	writer := source.client.RouteManager().OpenMultiRouteWriter(
		clientconnect.DestinationId(clientconnect.Id(destination.clientId)),
	)
	defer source.client.RouteManager().CloseMultiRouteWriter(writer)
	routeStateObserver := clientconnect.TestingObserveMultiRouteWriterRouteState(writer)
	defer routeStateObserver.Close()
	if err := waitForRouteCount(ctx, routeStateObserver, 2); err != nil {
		return clientconnect.Id{}, fmt.Errorf("wait for source P2P promotion: %w", err)
	}
	for nodeIndex, client := range clients {
		minimumRouteCount := 2
		if nodeIndex == 0 || nodeIndex == len(clients)-1 {
			minimumRouteCount = 1
		}
		if _, err := client.routeStateTrace.WaitForMinimumRoutes(
			ctx,
			minimumRouteCount,
			minimumRouteCount,
		); err != nil {
			return clientconnect.Id{}, fmt.Errorf(
				"wait for P2P routes at stream node %d: %w",
				nodeIndex,
				err,
			)
		}
	}
	if err := sendProbe(1); err != nil {
		return clientconnect.Id{}, err
	}
	select {
	case <-ctx.Done():
		return clientconnect.Id{}, ctx.Err()
	case <-deadline.C:
		return clientconnect.Id{}, fmt.Errorf(
			"multi-hop P2P promotion timed out: stats=%v",
			streamClientStats(clients),
		)
	case <-received:
		return streamId, nil
	}
}

// Aggregated client snapshots make a failed hop attributable by node index.
func streamClientStats(clients []*routeClient) []clientconnect.P2pDataPlaneStatsSnapshot {
	stats := make([]clientconnect.P2pDataPlaneStatsSnapshot, len(clients))
	for clientIndex, client := range clients {
		stats[clientIndex] = client.stats.Snapshot()
	}
	return stats
}

// Public is required at intermediary nodes because their StreamOpen names two
// adjacent peers; Network alone authorizes endpoint provider work only.
func setProductionStreamProvide(ctx context.Context, client *clientconnect.Client) error {
	result := make(chan error, 1)
	client.ContractManager().SetProvideModesWithReturnTrafficWithAckCallback(
		map[protocol.ProvideMode]bool{
			protocol.ProvideMode_Network: true,
			protocol.ProvideMode_Public:  true,
		},
		func(err error) {
			select {
			case result <- err:
			default:
			}
		},
	)
	registrationTimer := time.NewTimer(90 * time.Second)
	defer registrationTimer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case err := <-result:
		return err
	case <-registrationTimer.C:
		return fmt.Errorf("stream provide registration timed out")
	}
}

// The measured transfer uses indexed, byte-patterned raw frames. Carrier
// selection is proved separately by closing platform routes and observing
// production fast-path counters; application paths intentionally retain the
// original source/final destination instead of exposing their stream id.
func measureProductionMultiHopRoute(
	ctx context.Context,
	source *routeClient,
	destination *routeClient,
	path clientconnect.MultiHopId,
	packetCount int,
) (workloadResult, error) {
	const payloadByteCount = 1200
	received := make(chan uint64, packetCount)
	var invalidPacketCount atomic.Uint64
	unsub := destination.client.AddReceiveCallback(func(
		transferPath clientconnect.TransferPath,
		frames []*protocol.Frame,
		peer clientconnect.Peer,
	) {
		_ = peer
		if transferPath.SourceId != clientconnect.Id(source.clientId) {
			return
		}
		for _, frame := range frames {
			if frame.MessageType != protocol.MessageType_TestSimpleMessage || len(frame.MessageBytes) != payloadByteCount {
				continue
			}
			sequence := binary.BigEndian.Uint64(frame.MessageBytes[:8])
			valid := sequence < uint64(packetCount)
			for byteIndex := 8; valid && byteIndex < len(frame.MessageBytes); byteIndex += 1 {
				valid = frame.MessageBytes[byteIndex] == byte((int(sequence)+byteIndex)%251)
			}
			if !valid {
				invalidPacketCount.Add(1)
				continue
			}
			select {
			case received <- sequence:
			default:
				invalidPacketCount.Add(1)
			}
		}
	})
	defer unsub()
	startTime := time.Now()
	for packetIndex := range packetCount {
		packetBytes := clientconnect.MessagePoolGet(payloadByteCount)
		binary.BigEndian.PutUint64(packetBytes[:8], uint64(packetIndex))
		for byteIndex := 8; byteIndex < len(packetBytes); byteIndex += 1 {
			packetBytes[byteIndex] = byte((packetIndex + byteIndex) % 251)
		}
		frame := &protocol.Frame{
			MessageType:  protocol.MessageType_TestSimpleMessage,
			MessageBytes: packetBytes,
			Raw:          true,
		}
		if !source.client.SendMultiHopWithTimeout(
			frame,
			path,
			nil,
			60*time.Second,
			clientconnect.NoAck(),
			clientconnect.ForceStream(),
		) {
			clientconnect.MessagePoolReturn(packetBytes)
			return workloadResult{}, fmt.Errorf("multi-hop route send %d/%d failed", packetIndex, packetCount)
		}
	}
	seen := make([]bool, packetCount)
	uniquePacketCount := 0
	duplicatePacketCount := int64(0)
	deadline := time.NewTimer(2 * time.Minute)
	defer deadline.Stop()
	for uniquePacketCount < packetCount {
		select {
		case <-ctx.Done():
			return workloadResult{}, ctx.Err()
		case <-deadline.C:
			return workloadResult{}, fmt.Errorf("multi-hop route received %d/%d packets", uniquePacketCount, packetCount)
		case sequence := <-received:
			if seen[sequence] {
				duplicatePacketCount += 1
				continue
			}
			seen[sequence] = true
			uniquePacketCount += 1
		}
	}
	if invalidPacketCount.Load() != 0 {
		return workloadResult{}, fmt.Errorf("multi-hop invalid packet count=%d", invalidPacketCount.Load())
	}
	if duplicatePacketCount != 0 {
		return workloadResult{}, fmt.Errorf("multi-hop duplicate packet count=%d", duplicatePacketCount)
	}
	return finishWorkloadResult(workloadResult{
		UsefulByteCount:      int64(packetCount * payloadByteCount),
		DeliveredPacketCount: int64(uniquePacketCount),
		DuplicatePacketCount: duplicatePacketCount,
		Duration:             time.Since(startTime),
	}), nil
}

// Every requested stream length traverses one independently negotiated real
// production fast carrier per adjacency after all platform routes are closed.
func measureProductionStreamP2pTopology(
	ctx context.Context,
	t testing.TB,
	profile networkProfile,
	hopCount int,
	packetCount int,
) workloadResult {
	// StreamOpen supplies every adjacency. Disabling independent Network-peer
	// announcements prevents a direct endpoint transport from competing with
	// the explicit multi-hop stream.
	environment := newRouteEnvironmentWithNetworkPeers(ctx, t, profile, false)
	streamNetwork, err := newStreamP2pNetwork(profile, hopCount)
	if err != nil {
		environment.close()
		t.Fatalf("create %d-hop P2P network: %v", hopCount, err)
	}
	defer func() {
		environment.close()
		streamNetwork.close()
	}()
	clients := make([]*routeClient, hopCount+1)
	for nodeIndex := range clients {
		clients[nodeIndex] = environment.newClient(
			fmt.Sprintf("stream node %d", nodeIndex),
			clientconnect.P2pDataPlaneModeFastOnly,
			streamNetwork.nets[nodeIndex],
			false,
		)
		environment.connectPlatform(clients[nodeIndex], clientconnect.TransportModeH1)
	}
	for nodeIndex, client := range clients {
		if !waitForPlatform(ctx, client.transport) {
			t.Fatalf("stream node %d platform did not connect", nodeIndex)
		}
		if err := setProductionStreamProvide(ctx, client.client); err != nil {
			t.Fatalf("stream node %d provide: %v", nodeIndex, err)
		}
	}
	pathIds := make([]clientconnect.Id, hopCount)
	for hopIndex := range hopCount {
		pathIds[hopIndex] = clientconnect.Id(clients[hopIndex+1].clientId)
	}
	path, err := clientconnect.NewMultiHopId(pathIds...)
	if err != nil {
		t.Fatalf("create %d-hop path: %v", hopCount, err)
	}
	_, err = waitForProductionStreamP2p(
		ctx,
		clients[0],
		clients[len(clients)-1],
		path,
		clients,
	)
	if err != nil {
		t.Fatalf(
			"%v; carriers=%+v non-adjacent=%+v",
			err,
			streamNetwork.snapshot(),
			streamNetwork.nonAdjacent.snapshot(),
		)
	}
	writer := clients[0].client.RouteManager().OpenMultiRouteWriter(
		clientconnect.DestinationId(clientconnect.Id(clients[len(clients)-1].clientId)),
	)
	defer clients[0].client.RouteManager().CloseMultiRouteWriter(writer)
	routeStateObserver := clientconnect.TestingObserveMultiRouteWriterRouteState(writer)
	defer routeStateObserver.Close()
	for hopIndex := range hopCount {
		leftId := clientconnect.Id(clients[hopIndex].clientId)
		rightId := clientconnect.Id(clients[hopIndex+1].clientId)
		clients[hopIndex].client.ContractManager().AddNoContractPeer(rightId)
		clients[hopIndex+1].client.ContractManager().AddNoContractPeer(leftId)
	}
	forcedRouteBarrier := routeStateObserver.Snapshot()
	if forcedRouteBarrier.ActiveRouteCount != 2 {
		t.Fatalf(
			"forced %d-hop P2P transition started from route state=%+v",
			hopCount,
			forcedRouteBarrier,
		)
	}
	for _, client := range clients {
		client.transport.Close()
	}
	if _, err := waitForRouteCountAfter(
		ctx,
		routeStateObserver,
		forcedRouteBarrier,
		1,
	); err != nil {
		t.Fatalf("wait for forced %d-hop P2P source route: %v", hopCount, err)
	}
	statsBefore := streamClientStats(clients)
	result, err := measureProductionMultiHopRoute(
		ctx,
		clients[0],
		clients[len(clients)-1],
		path,
		packetCount,
	)
	if err != nil {
		t.Fatalf("measure %d-hop production stream: %v; hops=%+v stats=%+v", hopCount, err, streamNetwork.snapshot(), streamClientStats(clients))
	}
	statsAfter := streamClientStats(clients)
	for nodeIndex := range clients {
		before := statsBefore[nodeIndex]
		after := statsAfter[nodeIndex]
		if after.FastFallbackCount != before.FastFallbackCount ||
			after.LegacySendMessageCount != before.LegacySendMessageCount ||
			after.LegacyReceiveMessageCount != before.LegacyReceiveMessageCount ||
			after.FastDropCount != before.FastDropCount {
			t.Fatalf("%d-hop node %d used wrong carrier: before=%+v after=%+v", hopCount, nodeIndex, before, after)
		}
		if nodeIndex != len(clients)-1 && (after.FastSendMessageCount == before.FastSendMessageCount ||
			after.FastSendByteCount-before.FastSendByteCount < uint64(result.UsefulByteCount)) {
			t.Fatalf("%d-hop node %d did not fast-send the workload: before=%+v after=%+v", hopCount, nodeIndex, before, after)
		}
		if nodeIndex != 0 && (after.FastReceiveMessageCount == before.FastReceiveMessageCount ||
			after.FastReceiveByteCount-before.FastReceiveByteCount < uint64(result.UsefulByteCount)) {
			t.Fatalf("%d-hop node %d did not fast-receive the workload: before=%+v after=%+v", hopCount, nodeIndex, before, after)
		}
	}
	hopSnapshots := streamNetwork.snapshot()
	nonAdjacent := streamNetwork.nonAdjacent.snapshot()
	if nonAdjacent.DataPacketDropCount != 0 {
		t.Fatalf("%d-hop attempted non-adjacent data traffic: %+v", hopCount, nonAdjacent)
	}
	for hopIndex, hop := range hopSnapshots {
		if hop.Forward.PacketCount == 0 || hop.Reverse.PacketCount == 0 {
			t.Fatalf("%d-hop carrier %d did not carry bidirectional protocol traffic: %+v", hopCount, hopIndex, hop)
		}
	}
	t.Logf(
		"[perfvar-extended] topology=%d-hop route=p2p-fast result=%+v carriers=%+v clients=%+v non-adjacent=%+v",
		hopCount,
		result,
		hopSnapshots,
		statsAfter,
		nonAdjacent,
	)
	emitPerfvarRecord(t, extendedTopologyRecord{
		SchemaVersion:        perfvarSchemaVersion,
		RecordType:           "extended-topology-correctness",
		Topology:             fmt.Sprintf("%d-hop", hopCount),
		Route:                string(fullTunRouteP2pFast),
		AccessProfile:        profile,
		Result:               result,
		StreamP2PHops:        hopSnapshots,
		StreamP2PClientStats: statsAfter,
		NonAdjacent:          nonAdjacent,
		Correct:              true,
	})
	return result
}

// The selected production lengths include both direct and maximum supported
// streams; each iteration owns fresh clients, contracts, and network state.
func TestProductionStreamP2pExtendedTopology(t *testing.T) {
	if testing.Short() {
		return
	}
	testEnvironment := &server.TestEnv{ApplyDbMigrations: true, RerunCount: 0}
	testEnvironment.Run(t, func(t testing.TB) {
		profile := initialNetworkProfiles(3301)["clean-lan"]
		for _, hopCount := range []int{1, 3, 5, 9} {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
			result := measureProductionStreamP2pTopology(ctx, t, profile, hopCount, 16)
			cancel()
			if result.UsefulByteCount != 16*1200 || result.DeliveredPacketCount != 16 {
				t.Fatalf("%d-hop result=%+v", hopCount, result)
			}
		}
	})
}

// One logical server/connect edge owns an independently addressed exchange,
// handler, and client-carrier endpoint.
type splitExchangeEdge struct {
	name       string
	tun        *clientconnect.Tun
	address    netip.Addr
	h1Port     int
	h3Port     int
	apiPort    int
	exchange   *connectserver.Exchange
	handler    *connectserver.ConnectHandler
	httpServer *http.Server
	apiServer  *http.Server
}

// The two-edge fixture separates application access, provider access, and the
// internal exchange TCP segment while sharing only database control state.
type splitExchangeEnvironment struct {
	t              testing.TB
	ctx            context.Context
	cancel         context.CancelFunc
	network        *simulatedIPNetwork
	edges          []*splitExchangeEdge
	deviceAccess   networkProfile
	providerAccess networkProfile
	internal       networkProfile
	networkId      server.Id
	userId         server.Id
	userSession    *session.ClientSession
	clients        []*routeClient
	nextClient     int
	announces      *routeAsyncLifecycle
	controls       *routeAsyncLifecycle

	poolOutstandingBefore int64
}

// Both edges are created before either exchange starts, so the production
// route map contains stable userspace addresses for each outbound dial.
func newSplitExchangeEnvironment(
	ctx context.Context,
	t testing.TB,
	accessProfile networkProfile,
	internalProfile networkProfile,
) *splitExchangeEnvironment {
	return newSplitExchangeEnvironmentWithProfiles(
		ctx,
		t,
		accessProfile,
		accessProfile,
		internalProfile,
	)
}

// Full-TUN split scenarios retain independent application and provider access
// profiles in addition to the separately conditioned internal exchange link.
func newSplitExchangeEnvironmentWithProfiles(
	ctx context.Context,
	t testing.TB,
	deviceAccessProfile networkProfile,
	providerAccessProfile networkProfile,
	internalProfile networkProfile,
) *splitExchangeEnvironment {
	poolOutstandingBefore := routeMessagePoolOutstanding()
	environmentCtx, cancel := context.WithCancel(ctx)
	network := newSimulatedIPNetwork(environmentCtx)
	edges := make([]*splitExchangeEdge, 2)
	exchangeListeners := make([]net.Listener, len(edges))
	h1Listeners := make([]net.Listener, len(edges))
	h3PacketConns := make([]net.PacketConn, len(edges))
	apiListeners := make([]net.Listener, len(edges))
	for edgeIndex := range edges {
		name := fmt.Sprintf("logical-edge-%d", edgeIndex)
		tunSettings := clientconnect.DefaultTunSettingsWithBufferSize(4096)
		tunSettings.Mtu = min(
			min(deviceAccessProfile.InnerMtu, providerAccessProfile.InnerMtu),
			internalProfile.InnerMtu,
		)
		edgeTun, err := clientconnect.CreateTun(environmentCtx, tunSettings)
		if err != nil {
			cancel()
			t.Fatalf("create %s TUN: %v", name, err)
		}
		if err := network.addTun(name, edgeTun); err != nil {
			edgeTun.Close()
			cancel()
			t.Fatalf("add %s TUN: %v", name, err)
		}
		address := edgeTun.LocalAddresses()[0]
		edgeIP := net.IP(address.AsSlice())
		exchangeListener, err := edgeTun.ListenTCP(&net.TCPAddr{IP: edgeIP, Port: 0})
		if err != nil {
			network.close()
			cancel()
			t.Fatalf("listen %s exchange: %v", name, err)
		}
		h1Listener, err := edgeTun.ListenTCP(&net.TCPAddr{IP: edgeIP, Port: 0})
		if err != nil {
			exchangeListener.Close()
			network.close()
			cancel()
			t.Fatalf("listen %s H1: %v", name, err)
		}
		h3PacketConn, err := edgeTun.ListenUDP(&net.UDPAddr{IP: edgeIP, Port: 0})
		if err != nil {
			h1Listener.Close()
			exchangeListener.Close()
			network.close()
			cancel()
			t.Fatalf("listen %s H3: %v", name, err)
		}
		apiListener, err := edgeTun.ListenTCP(&net.TCPAddr{IP: edgeIP, Port: 0})
		if err != nil {
			h3PacketConn.Close()
			h1Listener.Close()
			exchangeListener.Close()
			network.close()
			cancel()
			t.Fatalf("listen %s API: %v", name, err)
		}
		edges[edgeIndex] = &splitExchangeEdge{
			name:    name,
			tun:     edgeTun,
			address: address,
			h1Port:  h1Listener.Addr().(*net.TCPAddr).Port,
			h3Port:  h3PacketConn.LocalAddr().(*net.UDPAddr).Port,
			apiPort: apiListener.Addr().(*net.TCPAddr).Port,
		}
		exchangeListeners[edgeIndex] = exchangeListener
		h1Listeners[edgeIndex] = h1Listener
		h3PacketConns[edgeIndex] = h3PacketConn
		apiListeners[edgeIndex] = apiListener
	}
	if _, _, err := network.addBidirectionalLink(edges[0].name, edges[1].name, internalProfile); err != nil {
		network.close()
		cancel()
		t.Fatalf("add internal exchange link: %v", err)
	}
	routes := map[string]string{}
	for _, edge := range edges {
		routes[edge.name] = edge.address.String()
	}
	serverTlsConfig, _, err := newWorkloadTlsConfigs()
	if err != nil {
		network.close()
		cancel()
		t.Fatalf("create split exchange TLS: %v", err)
	}
	serverTlsConfig.NextProtos = []string{"http/1.1"}
	announces := newRouteAsyncLifecycle()
	for edgeIndex, edge := range edges {
		exchangePort := exchangeListeners[edgeIndex].Addr().(*net.TCPAddr).Port
		exchangeSettings := connectserver.DefaultExchangeSettings()
		exchangeSettings.ExchangeResidentTtl = 10 * time.Second
		exchangeSettings.EnableNetworkPeers = false
		exchangeSettings.KeyEventDelivery.Enabled = false
		exchangeSettings.ConnectionAnnounceTimeout = 0
		exchangeSettings.ConnectionRateLimitSettings.BurstConnectionCount = 1000
		exchangeSettings.ConnectionTestConfig = connectserver.V0TestConfig()
		exchangeSettings.DialContext = edge.tun.DialContext
		edge.exchange = connectserver.NewExchangeWithListeners(
			environmentCtx,
			edge.name,
			"connect",
			"test",
			map[int]int{exchangePort: exchangePort},
			routes,
			exchangeSettings,
			map[int]net.Listener{exchangePort: exchangeListeners[edgeIndex]},
		)
		handlerSettings := connectserver.DefaultConnectHandlerSettings()
		handlerSettings.ListenH3Port = 0
		handlerSettings.ListenDnsPort = 0
		handlerSettings.EnableProxyProtocol = false
		handlerSettings.TransportTlsSettings.EnableSelfSign = true
		handlerSettings.TransportTlsSettings.DefaultHostName = edge.address.String()
		handlerSettings.ConnectionAnnounceTimeout = 0
		handlerSettings.ConnectionRateLimitSettings.BurstConnectionCount = 1000
		handlerSettings.ConnectionTestConfig = connectserver.V0TestConfig()
		handlerSettings.ConnectionAnnounceSettings.LifecycleStarted = func() {
			if !announces.start() {
				panic("split exchange connection announcement started during teardown")
			}
		}
		handlerSettings.ConnectionAnnounceSettings.LifecycleDone = announces.done
		edge.handler = connectserver.NewConnectHandlerWithPacketConns(
			environmentCtx,
			server.NewId(),
			edge.exchange,
			handlerSettings,
			connectserver.ConnectHandlerPacketConns{H3: h3PacketConns[edgeIndex]},
		)
		httpRoutes := []*router.Route{
			router.NewRoute("GET", "/status", router.WarpStatus),
			router.NewRoute("GET", "/", edge.handler.Connect),
		}
		edge.httpServer = &http.Server{Handler: router.NewRouter(environmentCtx, httpRoutes)}
		go func(edge *splitExchangeEdge, listener net.Listener) {
			_ = edge.httpServer.Serve(tls.NewListener(listener, serverTlsConfig.Clone()))
		}(edge, h1Listeners[edgeIndex])
		edge.apiServer = &http.Server{Handler: router.NewRouter(environmentCtx, api.Routes())}
		go func(edge *splitExchangeEdge, listener net.Listener) {
			_ = edge.apiServer.Serve(listener)
		}(edge, apiListeners[edgeIndex])
	}
	networkId := server.NewId()
	userId := server.NewId()
	model.Testing_CreateNetwork(
		environmentCtx,
		networkId,
		fmt.Sprintf("perfvar-split-%s", networkId),
		userId,
	)
	if err := model.AddBasicTransferBalance(
		environmentCtx,
		networkId,
		model.ByteCount(1024*1024*1024*1024),
		server.NowUtc(),
		server.NowUtc().Add(365*24*time.Hour),
	); err != nil {
		for _, edge := range edges {
			edge.apiServer.Close()
			edge.httpServer.Close()
			edge.handler.Close()
			edge.exchange.Close()
		}
		network.close()
		cancel()
		t.Fatalf("fund split exchange network: %v", err)
	}
	userSession := session.Testing_CreateClientSession(environmentCtx, jwt.NewByJwt(
		networkId,
		userId,
		fmt.Sprintf("perfvar-split-%s", networkId),
		false,
		false,
	))
	return &splitExchangeEnvironment{
		t:              t,
		ctx:            environmentCtx,
		cancel:         cancel,
		network:        network,
		edges:          edges,
		deviceAccess:   deviceAccessProfile,
		providerAccess: providerAccessProfile,
		internal:       internalProfile,
		networkId:      networkId,
		userId:         userId,
		userSession:    userSession,
		announces:      announces,
		controls:       newRouteAsyncLifecycle(),

		poolOutstandingBefore: poolOutstandingBefore,
	}
}

// The common full-TUN builder needs endpoint and identity data, not ownership
// of the split servers. This view pins the device and provider to different
// logical edges while the split environment retains teardown ownership.
func (self *splitExchangeEnvironment) fullTunRouteView() *routeEnvironment {
	internalProfile := self.internal
	return &routeEnvironment{
		t:                       self.t,
		ctx:                     self.ctx,
		profile:                 self.deviceAccess,
		accessProfile:           self.deviceAccess,
		deviceAccessProfile:     self.deviceAccess,
		providerAccessProfile:   self.providerAccess,
		internalExchangeProfile: &internalProfile,
		network:                 self.network,
		edgeAddress:             self.edges[0].address,
		h1Port:                  self.edges[0].h1Port,
		h3Port:                  self.edges[0].h3Port,
		apiPort:                 self.edges[0].apiPort,
		deviceEdgeName:          self.edges[0].name,
		providerEdgeName:        self.edges[1].name,
		providerEdgeAddress:     self.edges[1].address,
		providerH1Port:          self.edges[1].h1Port,
		providerH3Port:          self.edges[1].h3Port,
		providerApiPort:         self.edges[1].apiPort,
		networkId:               self.networkId,
		userId:                  self.userId,
		userSession:             self.userSession,
		extenderErrors:          make(chan error, 32),
		announces:               self.announces,
		controls:                self.controls,
	}
}

// A client is pinned to exactly one logical edge; disabling WebRTC admission
// ensures the measured route cannot promote around the internal exchange link.
func (self *splitExchangeEnvironment) newClient(
	edgeIndex int,
	description string,
	mode clientconnect.TransportMode,
) *routeClient {
	if edgeIndex < 0 || len(self.edges) <= edgeIndex {
		self.t.Fatalf("split exchange edge index=%d", edgeIndex)
	}
	result, err := model.AuthNetworkClient(
		&model.AuthNetworkClientArgs{Description: description},
		self.userSession,
	)
	if err != nil {
		self.t.Fatalf("auth split exchange client: %v", err)
	}
	if result.Error != nil {
		self.t.Fatalf("auth split exchange client: %s", result.Error.Message)
	}
	self.nextClient += 1
	clientName := fmt.Sprintf("split-client-%d", self.nextClient)
	tunSettings := clientconnect.DefaultTunSettingsWithBufferSize(4096)
	accessProfile := self.deviceAccess
	if edgeIndex == 1 {
		accessProfile = self.providerAccess
	}
	tunSettings.Mtu = accessProfile.InnerMtu
	clientTun, err := clientconnect.CreateTun(self.ctx, tunSettings)
	if err != nil {
		self.t.Fatalf("create %s TUN: %v", clientName, err)
	}
	if err := self.network.addTun(clientName, clientTun); err != nil {
		clientTun.Close()
		self.t.Fatalf("add %s TUN: %v", clientName, err)
	}
	if _, _, err := self.network.addBidirectionalLink(clientName, self.edges[edgeIndex].name, accessProfile); err != nil {
		clientTun.Close()
		self.t.Fatalf("add %s access link: %v", clientName, err)
	}
	strategySettings := clientconnect.DefaultClientStrategySettings()
	strategySettings.EnableResilient = false
	strategySettings.MinNextConnectDelay = 0
	strategySettings.MaxNextConnectDelay = 0
	strategySettings.ConnectSettings.TlsConfig = &tls.Config{
		InsecureSkipVerify: true,
		MinVersion:         tls.VersionTLS13,
	}
	strategySettings.ConnectSettings.DialContextSettings = &clientconnect.DialContextSettings{
		DialContext: clientTun.DialContext,
	}
	strategy := clientconnect.NewClientStrategy(self.ctx, strategySettings)
	settings := clientconnect.DefaultClientSettings()
	settings.ControlPingTimeout = 10 * time.Second
	settings.WebRtcSettings.MemoryBudget = clientconnect.NewTransferMemoryBudget(0)
	stats := &clientconnect.P2pDataPlaneStats{}
	settings.StreamManagerSettings.StreamBufferSettings.P2pTransportSettings.DataPlaneStats = stats
	clientId := *result.ClientId
	clientJwt := *result.ByClientJwt
	client := clientconnect.NewClient(
		self.ctx,
		clientconnect.Id(clientId),
		&routeOutOfBandControl{
			ctx:                     self.ctx,
			clientId:                clientId,
			contractManagerSettings: settings.ContractManagerSettings,
			lifecycle:               self.controls,
		},
		settings,
	)
	edge := self.edges[edgeIndex]
	platformSettings := clientconnect.DefaultPlatformTransportSettings()
	platformSettings.QuicTlsConfig.InsecureSkipVerify = true
	platformSettings.H3Port = edge.h3Port
	platformSettings.DnsPort = 0
	platformSettings.H3PacketConnFactory = func(ctx context.Context) (net.PacketConn, error) {
		return clientTun.ListenUDP(&net.UDPAddr{
			IP:   net.IP(clientTun.LocalAddresses()[0].AsSlice()),
			Port: 0,
		})
	}
	transport := clientconnect.NewPlatformTransportWithTargetMode(
		self.ctx,
		strategy,
		client.RouteManager(),
		fmt.Sprintf("wss://%s:%d", edge.address, edge.h1Port),
		&clientconnect.ClientAuth{
			ByJwt:      clientJwt,
			InstanceId: clientconnect.NewId(),
			AppVersion: "perfvar",
		},
		mode,
		platformSettings,
	)
	routeClient := &routeClient{
		clientId:  clientId,
		clientJwt: clientJwt,
		tun:       clientTun,
		strategy:  strategy,
		client:    client,
		transport: transport,
		stats:     stats,
	}
	self.clients = append(self.clients, routeClient)
	return routeClient
}

// Teardown waits for both independent resident sets and their exchange
// connections before closing the shared userspace network.
func (self *splitExchangeEnvironment) close() {
	closeCtx, closeCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer closeCancel()
	if err := closeRouteClientLifecyclesAndWait(
		closeCtx,
		routeClientLifecycles(self.clients),
	); err != nil {
		self.t.Errorf("split exchange clients did not close: %v", err)
	}
	// Client close submits its final contract controls. Join them while the
	// fixture context is still live; canceling first turns valid database
	// cleanup into a teardown-only timeout under race instrumentation.
	if !self.controls.closeAndWait(closeCtx) {
		self.t.Errorf("split exchange out-of-band controls did not become idle")
	}
	for _, edge := range self.edges {
		edge.apiServer.Close()
		edge.httpServer.Close()
		edge.handler.Close()
		edge.exchange.Close()
	}
	self.cancel()
	for _, edge := range self.edges {
		if !edge.handler.WaitForIdle(closeCtx) {
			self.t.Errorf("%s handler did not become idle", edge.name)
		}
	}
	if !self.announces.closeAndWait(closeCtx) {
		self.t.Errorf("split exchange connection announcements did not become idle")
	}
	for _, edge := range self.edges {
		if !edge.exchange.WaitForIdle(closeCtx) {
			self.t.Errorf("%s exchange did not become idle", edge.name)
		}
	}
	self.network.close()
	poolSnapshotAfter, poolBalanced := routeMessagePoolBalance(self.poolOutstandingBefore)
	if !poolBalanced {
		self.t.Errorf(
			"split exchange message-pool ownership did not reconcile: %d -> %d classes=%v",
			self.poolOutstandingBefore,
			poolSnapshotAfter.outstanding,
			poolSnapshotAfter.classes,
		)
	}
}

// Exact delivery is accepted only when the separately named internal link
// carries both forward data and reverse protocol traffic.
func measureSplitExchangeRoute(
	ctx context.Context,
	t testing.TB,
	accessProfile networkProfile,
	internalProfile networkProfile,
	mode clientconnect.TransportMode,
	packetCount int,
) workloadResult {
	environment := newSplitExchangeEnvironment(ctx, t, accessProfile, internalProfile)
	defer environment.close()
	source := environment.newClient(0, "split exchange source", mode)
	destination := environment.newClient(1, "split exchange destination", mode)
	if !waitForPlatform(ctx, source.transport) || !waitForPlatform(ctx, destination.transport) {
		t.Fatal("split exchange platform transport did not connect")
	}
	if err := setRouteProvide(ctx, source.client); err != nil {
		t.Fatalf("split exchange source provide: %v", err)
	}
	if err := setRouteProvide(ctx, destination.client); err != nil {
		t.Fatalf("split exchange destination provide: %v", err)
	}
	if !waitForDirectionalLinksTerminalIdle(
		ctx,
		environment.network.directionalLinks(),
		nil,
	) {
		t.Fatal("split exchange network did not become idle")
	}
	before := environment.network.snapshotLinks()
	result, err := measureProductionRoute(ctx, source, destination, packetCount)
	if err != nil {
		t.Fatalf("split exchange %s route: %v", mode, err)
	}
	links := subtractLinkSnapshots(before, environment.network.snapshotLinks(), result.Duration)
	forwardName := environment.edges[0].name + "->" + environment.edges[1].name
	reverseName := environment.edges[1].name + "->" + environment.edges[0].name
	forward := links[forwardName]
	reverse := links[reverseName]
	if forward.DeliveredPacketCount == 0 || forward.DeliveredByteCount == 0 ||
		reverse.DeliveredPacketCount == 0 || reverse.DeliveredByteCount == 0 {
		t.Fatalf("split exchange bypassed internal link: forward=%+v reverse=%+v links=%+v", forward, reverse, links)
	}
	for clientIndex, client := range []*routeClient{source, destination} {
		stats := client.stats.Snapshot()
		if stats.FastSendMessageCount != 0 || stats.FastReceiveMessageCount != 0 ||
			stats.LegacySendMessageCount != 0 || stats.LegacyReceiveMessageCount != 0 {
			t.Fatalf("split exchange client %d used P2P: %+v", clientIndex, stats)
		}
	}
	t.Logf(
		"[perfvar-extended] topology=split-exchange route=%s access-profile=%s internal-profile=%s result=%+v links=%+v",
		mode,
		accessProfile.Name,
		internalProfile.Name,
		result,
		links,
	)
	route := fullTunRouteExchangeH1
	if mode == clientconnect.TransportModeH3 {
		route = fullTunRouteExchangeH3
	}
	emitPerfvarRecord(t, extendedTopologyRecord{
		SchemaVersion:   perfvarSchemaVersion,
		RecordType:      "extended-topology-correctness",
		Topology:        "split-exchange",
		Route:           string(route),
		AccessProfile:   accessProfile,
		InternalProfile: &internalProfile,
		Result:          result,
		Links:           links,
		Correct:         true,
	})
	return result
}

// Both endpoint carrier modes must cross two real handlers, two residents,
// and the separately conditioned internal exchange TCP segment.
func TestProductionSplitExchangeExtendedTopology(t *testing.T) {
	if testing.Short() {
		return
	}
	testEnvironment := &server.TestEnv{ApplyDbMigrations: true, RerunCount: 0}
	testEnvironment.Run(t, func(t testing.TB) {
		profiles := allNetworkProfiles(3302)
		accessProfile := profiles["clean-lan"]
		internalProfile := profiles["rtt-25ms"]
		for _, mode := range []clientconnect.TransportMode{
			clientconnect.TransportModeH1,
			clientconnect.TransportModeH3,
		} {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
			result := measureSplitExchangeRoute(ctx, t, accessProfile, internalProfile, mode, 16)
			cancel()
			if result.UsefulByteCount != 16*1200 || result.DeliveredPacketCount != 16 {
				t.Fatalf("split exchange %s result=%+v", mode, result)
			}
		}
	})
}

// One full application path proves exact TCP content in both directions and
// requires workload-bound counters at every adjacent production stream hop.
func testFullTunExtendedP2pTopology(
	ctx context.Context,
	t testing.TB,
	profile networkProfile,
	topology string,
	hopCount int,
) {
	enableNetworkPeers := hopCount == 1
	environment := newRouteEnvironmentWithNetworkPeers(
		ctx,
		t,
		profile,
		enableNetworkPeers,
	)
	defer environment.close()
	path, err := tryNewFullTunPathWithTopology(
		ctx,
		t,
		environment,
		fullTunRouteP2pFast,
		false,
		defaultTunResourceProfile(),
		hopCount,
	)
	if err != nil {
		t.Fatalf("construct full-TUN %s: %v", topology, err)
	}
	defer path.close()
	if err := path.waitForMeasurementBoundary(ctx); err != nil {
		t.Fatalf("full-TUN %s path did not reach its measurement boundary: %v", topology, err)
	}
	const payloadByteCount = 64 * 1024
	uploadBoundary, err := beginPerfvarCarrierMeasurement(path)
	if err != nil {
		t.Fatalf("begin full-TUN %s upload measurement: %v", topology, err)
	}
	upload, err := measureFullTunUpload(ctx, path, payloadByteCount)
	if err != nil {
		t.Fatalf(
			"full-TUN %s upload: %v; hops=%+v clients=%+v device-packets=%+v provider-packets=%+v device-probes=%+v provider-probes=%+v",
			topology,
			err,
			path.streamP2pNetwork.snapshot(),
			snapshotP2pDataPlaneStats(path.streamP2pStats),
			path.multiClient.PacketStats(),
			path.providerRemoteNat.PacketStats(),
			path.deviceProbeTrace.snapshot(),
			path.providerProbeTrace.snapshot(),
		)
	}
	if err := path.waitForPostWorkloadBoundary(ctx); err != nil {
		t.Fatalf("full-TUN %s upload boundary: %v", topology, err)
	}
	uploadCarrier := observePerfvarWorkloadCarrier(path, uploadBoundary)
	if err := verifyPerfvarTopologyCarrier(perfvarScenario{
		Route:     fullTunRouteP2pFast,
		Direction: perfvarDirectionUpload,
		Topology:  topology,
	}, uploadCarrier, upload.UsefulByteCount); err != nil {
		t.Fatal(err)
	}
	downloadBoundary, err := beginPerfvarCarrierMeasurement(path)
	if err != nil {
		t.Fatalf("begin full-TUN %s download measurement: %v", topology, err)
	}
	download, err := measureFullTunDownload(ctx, path, payloadByteCount)
	if err != nil {
		t.Fatalf(
			"full-TUN %s download: %v; hops=%+v clients=%+v device-packets=%+v provider-packets=%+v device-probes=%+v provider-probes=%+v",
			topology,
			err,
			path.streamP2pNetwork.snapshot(),
			snapshotP2pDataPlaneStats(path.streamP2pStats),
			path.multiClient.PacketStats(),
			path.providerRemoteNat.PacketStats(),
			path.deviceProbeTrace.snapshot(),
			path.providerProbeTrace.snapshot(),
		)
	}
	if err := path.waitForPostWorkloadBoundary(ctx); err != nil {
		t.Fatalf("full-TUN %s download boundary: %v", topology, err)
	}
	downloadCarrier := observePerfvarWorkloadCarrier(path, downloadBoundary)
	if err := verifyPerfvarTopologyCarrier(perfvarScenario{
		Route:     fullTunRouteP2pFast,
		Direction: perfvarDirectionDownload,
		Topology:  topology,
	}, downloadCarrier, download.UsefulByteCount); err != nil {
		t.Fatal(err)
	}
	if upload.UsefulByteCount != payloadByteCount || download.UsefulByteCount != payloadByteCount ||
		upload.ContentHash == "" || download.ContentHash == "" {
		t.Fatalf("full-TUN %s exact results upload=%+v download=%+v", topology, upload, download)
	}
	if err := path.verifyRoute(); err != nil {
		t.Fatal(err)
	}
	t.Logf(
		"[perfvar-extended] full-tun topology=%s upload=%+v download=%+v upload-carrier=%+v download-carrier=%+v",
		topology,
		upload,
		download,
		uploadCarrier,
		downloadCarrier,
	)
}

// A failed extended workload preserves every node's final data-plane counters
// so carrier progress can be distinguished from route retirement.
func snapshotP2pDataPlaneStats(
	stats []*clientconnect.P2pDataPlaneStats,
) []clientconnect.P2pDataPlaneStatsSnapshot {
	snapshots := make([]clientconnect.P2pDataPlaneStatsSnapshot, len(stats))
	for index, nodeStats := range stats {
		snapshots[index] = nodeStats.Snapshot()
	}
	return snapshots
}

// Each named correctness case owns a fresh database and route environment so
// a failed stream length can be reproduced without running the whole matrix.
func testFullTunP2pFastTopologyCorrectness(
	t *testing.T,
	topology string,
	hopCount int,
) {
	if testing.Short() {
		return
	}
	testEnvironment := &server.TestEnv{ApplyDbMigrations: true, RerunCount: 0}
	testEnvironment.Run(t, func(t testing.TB) {
		profile := initialNetworkProfiles(3303)["clean-lan"]
		ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
		testFullTunExtendedP2pTopology(ctx, t, profile, topology, hopCount)
		cancel()
	})
}

// The established direct fixture remains the one-hop control.
func TestFullTunP2pFastOneHopTopologyCorrectness(t *testing.T) {
	testFullTunP2pFastTopologyCorrectness(t, perfvarTopologyOneHop, 1)
}

// Three independently impaired adjacencies form the shortest extended stream.
func TestFullTunP2pFastThreeHopTopologyCorrectness(t *testing.T) {
	testFullTunP2pFastTopologyCorrectness(t, perfvarTopologyThreeHop, 3)
}

// Five independently impaired adjacencies cover an intermediate stream length.
func TestFullTunP2pFastFiveHopTopologyCorrectness(t *testing.T) {
	testFullTunP2pFastTopologyCorrectness(t, perfvarTopologyFiveHop, 5)
}

// Nine independently impaired adjacencies cover the maximum supported stream.
func TestFullTunP2pFastNineHopTopologyCorrectness(t *testing.T) {
	testFullTunP2pFastTopologyCorrectness(t, perfvarTopologyNineHop, 9)
}

// One exact workload observation retains the shared measurement fences while
// substituting the caller's extended topology for the one-hop default.
func measureExtendedTopologyWorkload(
	ctx context.Context,
	path *fullTunPath,
	topology string,
	workload perfvarWorkload,
	direction perfvarDirection,
	measure func(context.Context, *fullTunPath) (workloadResult, error),
) (perfvarCorrectnessObservation, error) {
	if err := path.waitForMeasurementBoundary(ctx); err != nil {
		return perfvarCorrectnessObservation{}, fmt.Errorf(
			"%s/%s/%s premeasurement boundary: %w",
			topology,
			workload,
			direction,
			err,
		)
	}
	boundary := perfvarCarrierBoundary{}
	if workload != perfvarWorkloadTCPWarmed {
		var err error
		boundary, err = beginPerfvarCarrierMeasurement(path)
		if err != nil {
			return perfvarCorrectnessObservation{}, fmt.Errorf(
				"%s/%s/%s carrier measurement start: %w",
				topology,
				workload,
				direction,
				err,
			)
		}
	}
	result, err := measure(ctx, path)
	if err == nil {
		err = path.waitForPostWorkloadBoundary(ctx)
	}
	if workload == perfvarWorkloadTCPWarmed {
		warmedBoundary := path.takeCarrierMeasurementStart()
		if warmedBoundary == nil {
			if err != nil {
				return perfvarCorrectnessObservation{Result: result}, err
			}
			return perfvarCorrectnessObservation{Result: result}, fmt.Errorf(
				"%s/%s/%s did not publish a post-warmup carrier boundary",
				topology,
				workload,
				direction,
			)
		}
		boundary = *warmedBoundary
	}
	carrier := observePerfvarWorkloadCarrier(path, boundary)
	observation := perfvarCorrectnessObservation{
		Result:  result,
		Carrier: carrier,
	}
	if err != nil {
		return observation, err
	}
	if result.CorruptPacketCount != 0 {
		return observation, fmt.Errorf(
			"%s/%s/%s corruption count=%d",
			topology,
			workload,
			direction,
			result.CorruptPacketCount,
		)
	}
	if carrier.WireByteCount == 0 {
		return observation, fmt.Errorf(
			"%s/%s/%s carrier recorded no workload bytes",
			topology,
			workload,
			direction,
		)
	}
	if err := path.verifyRoute(); err != nil {
		return observation, err
	}
	if err := verifyPerfvarTopologyCarrier(perfvarScenario{
		Route:     path.route,
		Workload:  workload,
		Direction: direction,
		Topology:  topology,
	}, carrier, result.UsefulByteCount); err != nil {
		return observation, err
	}
	return observation, nil
}

// A single established three-hop stream carries both a primed TCP connection
// and a non-TCP application protocol through every physical adjacency.
func TestFullTunP2pFastThreeHopExtendedApplicationWorkloadsCorrectness(t *testing.T) {
	if testing.Short() {
		return
	}
	testEnvironment := &server.TestEnv{ApplyDbMigrations: true, RerunCount: 0}
	testEnvironment.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
		defer cancel()
		profile := initialNetworkProfiles(3305)["clean-lan"]
		environment := newRouteEnvironmentWithNetworkPeers(ctx, t, profile, false)
		defer environment.close()
		path, err := tryNewFullTunPathWithTopology(
			ctx,
			t,
			environment,
			fullTunRouteP2pFast,
			false,
			defaultTunResourceProfile(),
			3,
		)
		if err != nil {
			t.Fatalf("construct full-TUN three-hop workload path: %v", err)
		}
		defer path.close()

		const warmupByteCount = int64(256 * 1024)
		const measuredByteCount = int64(256 * 1024)
		warmed, err := measureExtendedTopologyWorkload(
			ctx,
			path,
			perfvarTopologyThreeHop,
			perfvarWorkloadTCPWarmed,
			perfvarDirectionUpload,
			func(ctx context.Context, path *fullTunPath) (workloadResult, error) {
				return measureFullTunWarmedUpload(ctx, path, warmupByteCount, measuredByteCount)
			},
		)
		if err != nil {
			t.Fatalf("three-hop warmed TCP upload: %v", err)
		}
		if warmed.Result.WarmupByteCount != warmupByteCount ||
			warmed.Result.UsefulByteCount != measuredByteCount ||
			warmed.Result.ContentHash != deterministicPayloadHash(measuredByteCount) {
			t.Fatalf("three-hop warmed TCP result=%+v", warmed.Result)
		}
		if unconsumedBoundary := path.takeCarrierMeasurementStart(); unconsumedBoundary != nil {
			t.Fatal("three-hop helper left the exact post-warmup carrier boundary unconsumed")
		}

		const quicByteCount = int64(64 * 1024)
		quic, err := measureExtendedTopologyWorkload(
			ctx,
			path,
			perfvarTopologyThreeHop,
			perfvarWorkloadQUIC,
			perfvarDirectionUpload,
			func(ctx context.Context, path *fullTunPath) (workloadResult, error) {
				return measureFullTunQUIC(ctx, path, quicByteCount)
			},
		)
		if err != nil {
			t.Fatalf("three-hop QUIC upload: %v", err)
		}
		if quic.Result.UsefulByteCount != quicByteCount ||
			quic.Result.ContentHash != deterministicPayloadHash(quicByteCount) {
			t.Fatalf("three-hop QUIC result=%+v", quic.Result)
		}
		t.Logf(
			"[perfvar-extended] full-tun topology=three-hop warmed=%+v warmed-carrier=%+v quic=%+v quic-carrier=%+v",
			warmed.Result,
			warmed.Carrier,
			quic.Result,
			quic.Carrier,
		)
	})
}

// A complete split-edge application path requires exact TCP content plus the
// independently conditioned internal link for both H1 and H3 carriers.
func testFullTunSplitExchangeTopology(
	ctx context.Context,
	t testing.TB,
	deviceAccessProfile networkProfile,
	providerAccessProfile networkProfile,
	internalProfile networkProfile,
	route fullTunRoute,
) {
	splitEnvironment := newSplitExchangeEnvironmentWithProfiles(
		ctx,
		t,
		deviceAccessProfile,
		providerAccessProfile,
		internalProfile,
	)
	defer splitEnvironment.close()
	environment := splitEnvironment.fullTunRouteView()
	path, err := tryNewFullTunPathWithTopology(
		ctx,
		t,
		environment,
		route,
		false,
		defaultTunResourceProfile(),
		1,
	)
	if err != nil {
		t.Fatalf("construct full-TUN split exchange %s: %v", route, err)
	}
	defer path.close()
	if err := path.waitForMeasurementBoundary(ctx); err != nil {
		t.Fatalf("full-TUN split exchange %s path did not reach its measurement boundary: %v", route, err)
	}
	const payloadByteCount = 64 * 1024
	uploadBoundary, err := beginPerfvarCarrierMeasurement(path)
	if err != nil {
		t.Fatalf("begin split exchange %s upload measurement: %v", route, err)
	}
	upload, err := measureFullTunUpload(ctx, path, payloadByteCount)
	if err != nil {
		t.Fatalf("full-TUN split exchange %s upload: %v", route, err)
	}
	if err := path.waitForPostWorkloadBoundary(ctx); err != nil {
		t.Fatalf("full-TUN split exchange %s upload boundary: %v", route, err)
	}
	uploadCarrier := observePerfvarWorkloadCarrier(path, uploadBoundary)
	if err := verifyPerfvarTopologyCarrier(perfvarScenario{
		Route:     route,
		Direction: perfvarDirectionUpload,
		Topology:  perfvarTopologySplitExchange,
	}, uploadCarrier, upload.UsefulByteCount); err != nil {
		t.Fatal(err)
	}
	downloadBoundary, err := beginPerfvarCarrierMeasurement(path)
	if err != nil {
		t.Fatalf("begin split exchange %s download measurement: %v", route, err)
	}
	download, err := measureFullTunDownload(ctx, path, payloadByteCount)
	if err != nil {
		t.Fatalf("full-TUN split exchange %s download: %v", route, err)
	}
	if err := path.waitForPostWorkloadBoundary(ctx); err != nil {
		t.Fatalf("full-TUN split exchange %s download boundary: %v", route, err)
	}
	downloadCarrier := observePerfvarWorkloadCarrier(path, downloadBoundary)
	if err := verifyPerfvarTopologyCarrier(perfvarScenario{
		Route:     route,
		Direction: perfvarDirectionDownload,
		Topology:  perfvarTopologySplitExchange,
	}, downloadCarrier, download.UsefulByteCount); err != nil {
		t.Fatal(err)
	}
	if upload.UsefulByteCount != payloadByteCount || download.UsefulByteCount != payloadByteCount ||
		upload.ContentHash == "" || download.ContentHash == "" {
		t.Fatalf("full-TUN split exchange %s exact results upload=%+v download=%+v", route, upload, download)
	}
	if err := path.verifyRoute(); err != nil {
		t.Fatal(err)
	}
	t.Logf(
		"[perfvar-extended] full-tun topology=split-exchange route=%s upload=%+v download=%+v upload-carrier=%+v download-carrier=%+v",
		route,
		upload,
		download,
		uploadCarrier,
		downloadCarrier,
	)
}

// A split-exchange regression forces one carrier across two real handlers,
// two exchanges, and an independently conditioned internal gVisor TCP link.
func testFullTunSplitExchangeExtendedTopologyCorrectness(
	t *testing.T,
	route fullTunRoute,
) {
	if testing.Short() {
		return
	}
	testEnvironment := &server.TestEnv{ApplyDbMigrations: true, RerunCount: 0}
	testEnvironment.Run(t, func(t testing.TB) {
		profiles := allNetworkProfiles(3304)
		ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
		testFullTunSplitExchangeTopology(
			ctx,
			t,
			profiles["clean-lan"],
			profiles["clean-lan"],
			profiles["rtt-25ms"],
			route,
		)
		cancel()
	})
}

// H1 preserves the complete split-edge application path and ownership.
func TestFullTunSplitExchangeH1ExtendedTopologyCorrectness(t *testing.T) {
	testFullTunSplitExchangeExtendedTopologyCorrectness(t, fullTunRouteExchangeH1)
}

// H3 preserves the complete split-edge application path and ownership.
func TestFullTunSplitExchangeH3ExtendedTopologyCorrectness(t *testing.T) {
	testFullTunSplitExchangeExtendedTopologyCorrectness(t, fullTunRouteExchangeH3)
}

// Inner QUIC must traverse both real H3 handlers and the separately observed
// internal exchange link without relying on TCP application behavior.
func TestFullTunSplitExchangeH3QUICExtendedTopologyCorrectness(t *testing.T) {
	if testing.Short() {
		return
	}
	testEnvironment := &server.TestEnv{ApplyDbMigrations: true, RerunCount: 0}
	testEnvironment.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
		defer cancel()
		profiles := allNetworkProfiles(3306)
		splitEnvironment := newSplitExchangeEnvironmentWithProfiles(
			ctx,
			t,
			profiles["clean-lan"],
			profiles["clean-lan"],
			profiles["rtt-25ms"],
		)
		defer splitEnvironment.close()
		path, err := tryNewFullTunPathWithTopology(
			ctx,
			t,
			splitEnvironment.fullTunRouteView(),
			fullTunRouteExchangeH3,
			false,
			defaultTunResourceProfile(),
			1,
		)
		if err != nil {
			t.Fatalf("construct full-TUN split-exchange H3 QUIC path: %v", err)
		}
		defer path.close()

		const quicByteCount = int64(64 * 1024)
		quic, err := measureExtendedTopologyWorkload(
			ctx,
			path,
			perfvarTopologySplitExchange,
			perfvarWorkloadQUIC,
			perfvarDirectionUpload,
			func(ctx context.Context, path *fullTunPath) (workloadResult, error) {
				return measureFullTunQUIC(ctx, path, quicByteCount)
			},
		)
		if err != nil {
			t.Fatalf("split-exchange H3 QUIC upload: %v", err)
		}
		if quic.Result.UsefulByteCount != quicByteCount ||
			quic.Result.ContentHash != deterministicPayloadHash(quicByteCount) {
			t.Fatalf("split-exchange H3 QUIC result=%+v", quic.Result)
		}
		t.Logf(
			"[perfvar-extended] full-tun topology=split-exchange route=exchange-h3 quic=%+v carrier=%+v",
			quic.Result,
			quic.Carrier,
		)
	})
}
