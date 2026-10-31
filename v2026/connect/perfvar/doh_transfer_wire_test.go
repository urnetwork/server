//go:build acklineagetrace

package perfvar

import (
	"encoding/binary"
	"fmt"
	"net"
	"net/netip"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	clientconnect "github.com/urnetwork/connect/v2026"
	"github.com/urnetwork/connect/v2026/protocol"
)

// These diagnostics retain numbers and endpoint headers, never DNS, TLS, or
// borrowed Transfer payloads. The production nil-observer path is unchanged.
type dohTransferWireSnapshot struct {
	Packs, Frames, PacketBytes, WireBytes uint64
	AckPacks, NoAckPacks, HeadPacks       uint64
	PayloadFrames, PayloadPacks           uint64
	AckPayloadPacks, NoAckPayloadPacks    uint64
	PromotedPacks, ContractPacks          uint64
	ResendPacks, ResendWireBytes          uint64
	Malformed, WrongDirection             uint64
}

func dohTransferWireDelta(after, before dohTransferWireSnapshot) dohTransferWireSnapshot {
	return dohTransferWireSnapshot{
		Packs: after.Packs - before.Packs, Frames: after.Frames - before.Frames,
		PacketBytes: after.PacketBytes - before.PacketBytes, WireBytes: after.WireBytes - before.WireBytes,
		AckPacks: after.AckPacks - before.AckPacks, NoAckPacks: after.NoAckPacks - before.NoAckPacks,
		PayloadFrames: after.PayloadFrames - before.PayloadFrames, PayloadPacks: after.PayloadPacks - before.PayloadPacks,
		AckPayloadPacks: after.AckPayloadPacks - before.AckPayloadPacks, NoAckPayloadPacks: after.NoAckPayloadPacks - before.NoAckPayloadPacks,
		HeadPacks: after.HeadPacks - before.HeadPacks, PromotedPacks: after.PromotedPacks - before.PromotedPacks,
		ContractPacks: after.ContractPacks - before.ContractPacks,
		ResendPacks:   after.ResendPacks - before.ResendPacks, ResendWireBytes: after.ResendWireBytes - before.ResendWireBytes,
		Malformed: after.Malformed - before.Malformed, WrongDirection: after.WrongDirection - before.WrongDirection,
	}
}

type dohTransferTCPHeader struct {
	Source, Destination netip.AddrPort
	Sequence            uint32
	PayloadBytes        int
}

// The selected fixture is explicitly IPv4. Reject fragments and malformed
// lengths instead of accidentally treating an arbitrary byte as a TCP port.
func dohTransferTCPPacket(packet []byte) (dohTransferTCPHeader, bool) {
	var result dohTransferTCPHeader
	if len(packet) < 40 || packet[0]>>4 != 4 || packet[9] != 6 || binary.BigEndian.Uint16(packet[6:8])&0x3fff != 0 {
		return result, false
	}
	ipHeader := int(packet[0]&15) * 4
	total := int(binary.BigEndian.Uint16(packet[2:4]))
	if ipHeader < 20 || total > len(packet) || total < ipHeader+20 {
		return result, false
	}
	tcpHeader := int(packet[ipHeader+12]>>4) * 4
	if tcpHeader < 20 || ipHeader+tcpHeader > total {
		return result, false
	}
	result.Source = netip.AddrPortFrom(netip.AddrFrom4([4]byte(packet[12:16])), binary.BigEndian.Uint16(packet[ipHeader:]))
	result.Destination = netip.AddrPortFrom(netip.AddrFrom4([4]byte(packet[16:20])), binary.BigEndian.Uint16(packet[ipHeader+2:]))
	result.Sequence = binary.BigEndian.Uint32(packet[ipHeader+4:])
	result.PayloadBytes = total - ipHeader - tcpHeader
	return result, true
}

type dohTransferWireObserver struct {
	provider bool
	port     atomic.Uint32
	fault    atomic.Pointer[dohTransferCarrierFault]
	mu       sync.Mutex
	totals   dohTransferWireSnapshot
}

func (self *dohTransferWireObserver) snapshot() dohTransferWireSnapshot {
	self.mu.Lock()
	defer self.mu.Unlock()
	return self.totals
}

func dohTransferDecodePack(bytes []byte) (*protocol.Pack, error) {
	var frame protocol.TransferFrame
	if err := clientconnect.ProtoUnmarshal(bytes, &frame); err != nil {
		return nil, err
	}
	if frame.Pack != nil {
		return frame.Pack, nil
	}
	if frame.Frame == nil || frame.Frame.MessageType != protocol.MessageType_TransferPack {
		return nil, nil // ACK/control messages are not IP data Packs.
	}
	var pack protocol.Pack
	if err := clientconnect.ProtoUnmarshal(frame.Frame.MessageBytes, &pack); err != nil {
		return nil, err
	}
	return &pack, nil
}

func (self *dohTransferWireObserver) observe(observation clientconnect.TransferWireMessageObservation) {
	port := uint16(self.port.Load())
	if port == 0 {
		return
	}
	pack, err := dohTransferDecodePack(observation.TransferFrameBytes)
	var delta dohTransferWireSnapshot
	if err != nil {
		delta.Malformed++
	} else if pack != nil {
		for _, frame := range pack.Frames {
			if frame.MessageType != protocol.MessageType_IpIpPacketToProvider && frame.MessageType != protocol.MessageType_IpIpPacketFromProvider {
				continue
			}
			message, err := clientconnect.FromFrame(frame)
			if err != nil {
				delta.Malformed++
				continue
			}
			var packet []byte
			switch value := message.(type) {
			case *protocol.IpPacketToProvider:
				packet = value.GetIpPacket().GetPacketBytes()
			case *protocol.IpPacketFromProvider:
				packet = value.GetIpPacket().GetPacketBytes()
			}
			header, ok := dohTransferTCPPacket(packet)
			if !ok {
				continue // UDP/health probes and other flows are not this query.
			}
			toProvider := frame.MessageType == protocol.MessageType_IpIpPacketToProvider
			if toProvider && header.Destination.Port() != port || !toProvider && header.Source.Port() != port {
				continue
			}
			if toProvider == self.provider {
				delta.WrongDirection++
			}
			delta.Frames++
			delta.PacketBytes += uint64(len(packet))
			if header.PayloadBytes > 0 {
				delta.PayloadFrames++
			}
		}
		if delta.Frames > 0 {
			delta.Packs, delta.WireBytes = 1, uint64(len(observation.WireMessageBytes))
			if pack.Nack {
				delta.NoAckPacks = 1
			} else {
				delta.AckPacks = 1
			}
			if delta.PayloadFrames > 0 {
				delta.PayloadPacks = 1
				delta.AckPayloadPacks, delta.NoAckPayloadPacks = delta.AckPacks, delta.NoAckPacks
			}
			if pack.Head {
				delta.HeadPacks = 1
			}
			if pack.ContractFrame != nil {
				delta.ContractPacks = 1
			}
			if observation.PromotedHead {
				delta.PromotedPacks = 1
			}
			if observation.Resend {
				delta.ResendPacks, delta.ResendWireBytes = 1, delta.WireBytes
			}
			// The loss predicate is installed before the request, but becomes
			// eligible only when the actual query-flow Pack reaches its final
			// route-attempt boundary. Warmup and independent probes cannot arm it.
			if fault := self.fault.Load(); fault != nil && delta.PayloadFrames > 0 {
				fault.armed.Store(true)
			}
		}
	}
	self.mu.Lock()
	self.totals.Packs += delta.Packs
	self.totals.Frames += delta.Frames
	self.totals.PacketBytes += delta.PacketBytes
	self.totals.WireBytes += delta.WireBytes
	self.totals.AckPacks += delta.AckPacks
	self.totals.NoAckPacks += delta.NoAckPacks
	self.totals.PayloadFrames += delta.PayloadFrames
	self.totals.PayloadPacks += delta.PayloadPacks
	self.totals.AckPayloadPacks += delta.AckPayloadPacks
	self.totals.NoAckPayloadPacks += delta.NoAckPayloadPacks
	self.totals.HeadPacks += delta.HeadPacks
	self.totals.PromotedPacks += delta.PromotedPacks
	self.totals.ContractPacks += delta.ContractPacks
	self.totals.ResendPacks += delta.ResendPacks
	self.totals.ResendWireBytes += delta.ResendWireBytes
	self.totals.Malformed += delta.Malformed
	self.totals.WrongDirection += delta.WrongDirection
	self.mu.Unlock()
}

// A direction-specific fixed set of old native H1 tuples excludes new dials,
// control connections on other ports, TCP ACK-only packets, and other nodes.
// Closing the old native sockets replaces only the carrier, never the client,
// the provider, or an established inner TCP stream.
type dohTransferCarrierTuple struct {
	Source, Destination netip.AddrPort
}

type dohTransferCarrierFault struct {
	tuples  []dohTransferCarrierTuple
	limit   int64
	armed   atomic.Bool
	target  atomic.Pointer[dohTransferCarrierTarget]
	drops   atomic.Int64
	bytes   atomic.Int64
	firstNS atomic.Int64
	first   chan struct{}
}

type dohTransferCarrierTarget struct {
	Tuple    dohTransferCarrierTuple
	Sequence uint32
}

func (self *dohTransferCarrierFault) drop(packet []byte) bool {
	if !self.armed.Load() {
		return false
	}
	header, ok := dohTransferTCPPacket(packet)
	if !ok || header.PayloadBytes == 0 {
		return false
	}
	matched := false
	for _, tuple := range self.tuples {
		if tuple.Source == header.Source && tuple.Destination == header.Destination {
			matched = true
			break
		}
	}
	if !matched {
		return false
	}
	// Pin one byte of one physical TCP stream. Later data and different
	// streams cannot exhaust the fault budget; a retransmission with different
	// segmentation still loses when it covers this same byte (including wrap).
	target := self.target.Load()
	if target == nil {
		candidate := &dohTransferCarrierTarget{Tuple: dohTransferCarrierTuple{header.Source, header.Destination}, Sequence: header.Sequence}
		self.target.CompareAndSwap(nil, candidate)
		target = self.target.Load()
	}
	if target.Tuple.Source != header.Source || target.Tuple.Destination != header.Destination || target.Sequence-header.Sequence >= uint32(header.PayloadBytes) {
		return false
	}
	for {
		count := self.drops.Load()
		if count >= self.limit {
			return false
		}
		if self.drops.CompareAndSwap(count, count+1) {
			self.bytes.Add(int64(header.PayloadBytes))
			if count == 0 {
				self.firstNS.Store(time.Now().UnixNano())
				close(self.first)
			}
			return true
		}
	}
}

func dohTransferConnTuple(connection net.Conn) (dohTransferCarrierTuple, error) {
	local, localOK := connection.LocalAddr().(*net.TCPAddr)
	remote, remoteOK := connection.RemoteAddr().(*net.TCPAddr)
	if !localOK || !remoteOK {
		return dohTransferCarrierTuple{}, fmt.Errorf("native H1 connection lacks TCP addresses")
	}
	return dohTransferCarrierTuple{
		Source:      netip.AddrPortFrom(local.AddrPort().Addr().Unmap(), uint16(local.Port)),
		Destination: netip.AddrPortFrom(remote.AddrPort().Addr().Unmap(), uint16(remote.Port)),
	}, nil
}

func dohTransferTestPacket(source, destination uint16, payload int) []byte {
	packet := make([]byte, 40+payload)
	packet[0], packet[9], packet[32] = 0x45, 6, 0x50
	copy(packet[12:16], []byte{10, 0, 0, 1})
	copy(packet[16:20], []byte{10, 0, 0, 2})
	binary.BigEndian.PutUint16(packet[2:4], uint16(len(packet)))
	binary.BigEndian.PutUint16(packet[20:22], source)
	binary.BigEndian.PutUint16(packet[22:24], destination)
	return packet
}

func TestDohTransferWireActualPolicyAndIdentity(t *testing.T) {
	for _, provider := range []bool{false, true} {
		for _, noAck := range []bool{false, true} {
			for _, legacy := range []bool{false, true} {
				observer := &dohTransferWireObserver{provider: provider}
				observer.port.Store(443)
				packet := dohTransferTestPacket(1234, 443, 1)
				kind := protocol.MessageType_IpIpPacketToProvider
				if provider {
					packet = dohTransferTestPacket(443, 1234, 1)
					kind = protocol.MessageType_IpIpPacketFromProvider
				}
				pack := &protocol.Pack{Nack: noAck, Head: true, Frames: []*protocol.Frame{{MessageType: kind, Raw: true, MessageBytes: packet}}}
				frame := &protocol.TransferFrame{Pack: pack}
				if legacy {
					frame = &protocol.TransferFrame{Frame: clientconnect.RequireToFrame(pack, 1)}
				}
				wire, err := clientconnect.ProtoMarshal(frame)
				if legacy {
					clientconnect.MessagePoolReturn(frame.Frame.MessageBytes)
				}
				if err != nil {
					t.Fatal(err)
				}
				observer.observe(clientconnect.TransferWireMessageObservation{TransferFrameBytes: wire, WireMessageBytes: wire, Resend: true, PromotedHead: true})
				got := observer.snapshot()
				if got.Packs != 1 || got.Frames != 1 || got.PayloadPacks != 1 || got.PayloadFrames != 1 || got.Malformed != 0 || got.WrongDirection != 0 || got.ResendPacks != 1 || got.PromotedPacks != 1 || (got.NoAckPacks == 1) != noAck || (got.AckPacks == 1) == noAck || got.AckPayloadPacks != got.AckPacks || got.NoAckPayloadPacks != got.NoAckPacks {
					t.Fatalf("provider=%t nack=%t legacy=%t: %+v", provider, noAck, legacy, got)
				}
				// Borrowed bytes can be destroyed immediately after observation.
				clear(wire)
				clientconnect.MessagePoolReturn(wire)
				if observer.snapshot() != got {
					t.Fatal("observer retained borrowed bytes")
				}
			}
		}
	}
}

func TestDohTransferFaultExactTupleAndBoundary(t *testing.T) {
	packet := dohTransferTestPacket(1234, 443, 1)
	header, _ := dohTransferTCPPacket(packet)
	fault := &dohTransferCarrierFault{tuples: []dohTransferCarrierTuple{{header.Source, header.Destination}}, limit: 3, first: make(chan struct{})}
	if fault.drop(packet) {
		t.Fatal("fault acted before the query-flow boundary")
	}
	fault.armed.Store(true)
	for _, invalid := range [][]byte{nil, dohTransferTestPacket(1235, 443, 1), dohTransferTestPacket(1234, 444, 1), dohTransferTestPacket(1234, 443, 0)} {
		if fault.drop(invalid) {
			t.Fatal("fault accepted an unrelated tuple, malformed packet, or pure ACK")
		}
	}
	fragment := append([]byte(nil), packet...)
	fragment[6] = 0x20
	if fault.drop(fragment) {
		t.Fatal("fault interpreted a fragment as a complete TCP packet")
	}
	for i := range 5 {
		if fault.drop(packet) != (i < 3) {
			t.Fatal("bounded loss trace overshot/undershot")
		}
	}
	if fault.drops.Load() != 3 || fault.bytes.Load() != 3 || fault.firstNS.Load() == 0 {
		t.Fatal("fault counters lost their exact identity")
	}
}

func TestDohTransferWireUnrelatedFlowCannotArmFault(t *testing.T) {
	observer := &dohTransferWireObserver{}
	observer.port.Store(443)
	fault := &dohTransferCarrierFault{}
	observer.fault.Store(fault)
	for _, packet := range [][]byte{dohTransferTestPacket(1234, 444, 1), dohTransferTestPacket(1234, 443, 0), {0x45}, nil} {
		wire, _ := clientconnect.ProtoMarshal(&protocol.TransferFrame{Pack: &protocol.Pack{Frames: []*protocol.Frame{{MessageType: protocol.MessageType_IpIpPacketToProvider, Raw: true, MessageBytes: packet}}}})
		observer.observe(clientconnect.TransferWireMessageObservation{TransferFrameBytes: wire})
		clientconnect.MessagePoolReturn(wire)
	}
	if fault.armed.Load() || observer.snapshot().PayloadPacks != 0 {
		t.Fatal("unrelated, ACK-only, or malformed IP traffic armed the query fault")
	}
}

func TestDohTransferCarrierTupleNormalizesNativeIPv4(t *testing.T) {
	connection := &h1FailureFakeTCPConn{port: 443}
	tuple, err := dohTransferConnTuple(connection)
	if err != nil || !tuple.Source.Addr().Is4() || !tuple.Destination.Addr().Is4() || tuple.Source.Port() != 1234 || tuple.Destination.Port() != 443 {
		t.Fatalf("native IPv4 tuple cannot match packet headers: %+v %v", tuple, err)
	}
}

func TestDohTransferFaultPinsSameTCPByteAcrossSegmentationAndWrap(t *testing.T) {
	for _, sequence := range []uint32{1000, 0xfffffff0} {
		packet := dohTransferTestPacket(1234, 443, 8)
		binary.BigEndian.PutUint32(packet[24:28], sequence)
		header, _ := dohTransferTCPPacket(packet)
		fault := &dohTransferCarrierFault{tuples: []dohTransferCarrierTuple{{header.Source, header.Destination}}, limit: 3, first: make(chan struct{})}
		fault.armed.Store(true)
		if !fault.drop(packet) {
			t.Fatal("initial exact target byte not dropped")
		}
		later := dohTransferTestPacket(1234, 443, 64)
		binary.BigEndian.PutUint32(later[24:28], sequence+8)
		if fault.drop(later) || fault.drops.Load() != 1 {
			t.Fatal("new data consumed the retransmission-loss quota")
		}
		combined := dohTransferTestPacket(1234, 443, 32)
		binary.BigEndian.PutUint32(combined[24:28], sequence-16)
		before := dohTransferTestPacket(1234, 443, 16)
		binary.BigEndian.PutUint32(before[24:28], sequence-16)
		if fault.drop(before) || !fault.drop(combined) || !fault.drop(packet) || fault.drop(packet) || fault.drops.Load() != 3 {
			t.Fatal("same-byte segmentation/wrap boundary changed the exact quota")
		}
	}
}
