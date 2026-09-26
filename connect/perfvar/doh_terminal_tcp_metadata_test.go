//go:build acklineagetrace

package perfvar

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"hash/crc64"
	"sync/atomic"
	"testing"
	"time"
	"unsafe"

	clientconnect "github.com/urnetwork/connect"
	"github.com/urnetwork/connect/protocol"
)

const dohTerminalTCPCapacity = 4096

var dohTerminalTCPChecksum = crc64.MakeTable(crc64.ECMA)
var dohTransferRetireForTest func(func())

func retireDohTransferOperation(retire func()) {
	if hook := dohTransferRetireForTest; hook != nil {
		hook(retire)
	} else {
		retire()
	}
}

type dohTerminalRetirementPhase struct {
	phase      atomic.Uint32
	begin, end atomic.Int64
}

func (self *dohTerminalRetirementPhase) retire(retire func()) {
	self.begin.Store(time.Now().UnixNano())
	self.phase.Store(1)
	retire()
	self.end.Store(time.Now().UnixNano())
	self.phase.Store(2)
}

// Scalar metadata only: no packet/frame slices, addresses, or TLS/DNS bytes.
// The fixed first-event prefix never overwrites evidence; overflow is invalid.
type dohTerminalTCPEvent struct {
	AtNS                              int64
	Message, Sequence                 clientconnect.Id
	Number                            uint64
	Frame                             int
	Phase                             uint32
	TCPSequence, TCPAck               uint32
	Flags                             uint8
	PayloadBytes, PacketBytes         int
	FlowHash, PacketHash, PayloadHash uint64
	ToProvider, Resend, NoAck         bool
}

type dohTerminalTCPRecorder struct {
	phase           *dohTerminalRetirementPhase
	next, malformed atomic.Uint64
	events          [dohTerminalTCPCapacity]dohTerminalTCPEvent
	published       [dohTerminalTCPCapacity]atomic.Bool
}

func (self *dohTerminalTCPRecorder) observe(observation clientconnect.TransferWireMessageObservation) {
	pack, err := dohTransferDecodePack(observation.TransferFrameBytes)
	if err != nil {
		self.malformed.Add(1)
		return
	}
	if pack == nil {
		return
	}
	for index, frame := range pack.Frames {
		if frame.MessageType != protocol.MessageType_IpIpPacketToProvider && frame.MessageType != protocol.MessageType_IpIpPacketFromProvider {
			continue
		}
		message, err := clientconnect.FromFrame(frame)
		if err != nil {
			self.malformed.Add(1)
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
			continue
		}
		if len(pack.MessageId) != 16 || len(pack.SequenceId) != 16 {
			self.malformed.Add(1)
			continue
		}
		ipHeader := int(packet[0]&15) * 4
		payloadOffset := ipHeader + int(packet[ipHeader+12]>>4)*4
		toProvider := frame.MessageType == protocol.MessageType_IpIpPacketToProvider
		var tuple [12]byte
		if toProvider {
			copy(tuple[:4], packet[12:16])
			copy(tuple[4:8], packet[16:20])
			copy(tuple[8:], packet[ipHeader:ipHeader+4])
		} else {
			copy(tuple[:4], packet[16:20])
			copy(tuple[4:8], packet[12:16])
			copy(tuple[8:10], packet[ipHeader+2:ipHeader+4])
			copy(tuple[10:], packet[ipHeader:ipHeader+2])
		}
		event := dohTerminalTCPEvent{AtNS: time.Now().UnixNano(), Number: pack.SequenceNumber, Frame: index, Phase: self.phase.phase.Load(),
			TCPSequence: header.Sequence, TCPAck: binary.BigEndian.Uint32(packet[ipHeader+8:]), Flags: packet[ipHeader+13],
			PayloadBytes: header.PayloadBytes, PacketBytes: len(packet), FlowHash: crc64.Checksum(tuple[:], dohTerminalTCPChecksum),
			PacketHash: crc64.Checksum(packet, dohTerminalTCPChecksum), PayloadHash: crc64.Checksum(packet[payloadOffset:payloadOffset+header.PayloadBytes], dohTerminalTCPChecksum),
			ToProvider: toProvider, Resend: observation.Resend, NoAck: pack.Nack}
		copy(event.Message[:], pack.MessageId)
		copy(event.Sequence[:], pack.SequenceId)
		slot := self.next.Add(1) - 1
		if slot < dohTerminalTCPCapacity {
			self.events[slot] = event
			self.published[slot].Store(true)
		}
	}
}

func (self *dohTerminalTCPRecorder) dump(t testing.TB, role string) {
	count := self.next.Load()
	var unpublished uint64
	for index := uint64(0); index < min(count, dohTerminalTCPCapacity); index++ {
		if !self.published[index].Load() {
			unpublished++
		}
	}
	header, _ := json.Marshal(map[string]any{"kind": "doh-terminal-tcp-header", "role": role, "count": count, "capacity": dohTerminalTCPCapacity,
		"overflow": count - min(count, dohTerminalTCPCapacity), "unpublished": unpublished, "malformed": self.malformed.Load(), "recorder_bytes": unsafe.Sizeof(*self),
		"retire_begin_ns": self.phase.begin.Load(), "retire_end_ns": self.phase.end.Load(), "baseline_eligible": false})
	t.Logf("[doh-terminal-tcp] %s", header)
	for index := uint64(0); index < min(count, dohTerminalTCPCapacity); index++ {
		if !self.published[index].Load() {
			continue
		}
		event := self.events[index]
		row, _ := json.Marshal(map[string]any{"kind": "doh-terminal-tcp-event", "role": role, "ordinal": index, "at_unix_nano": event.AtNS,
			"message": progressTraceIdentity(event.Message), "sequence": progressTraceIdentity(event.Sequence), "number": event.Number, "frame": event.Frame, "retire_phase": event.Phase,
			"tcp_sequence": event.TCPSequence, "tcp_ack": event.TCPAck, "flags": fmt.Sprintf("%02x", event.Flags), "payload_bytes": event.PayloadBytes, "packet_bytes": event.PacketBytes,
			"flow_hash": fmt.Sprintf("%016x", event.FlowHash), "packet_hash": fmt.Sprintf("%016x", event.PacketHash), "payload_hash": fmt.Sprintf("%016x", event.PayloadHash),
			"to_provider": event.ToProvider, "resend": event.Resend, "no_ack": event.NoAck})
		t.Logf("[doh-terminal-tcp] %s", row)
	}
	if count > dohTerminalTCPCapacity || unpublished != 0 || self.malformed.Load() != 0 {
		t.Errorf("terminal TCP metadata incomplete: role=%s count=%d unpublished=%d malformed=%d", role, count, unpublished, self.malformed.Load())
	}
}

func newDohTerminalTCPTrace(phase *dohTerminalRetirementPhase) *perfvarProgressTrace {
	trace := newH1AckReplayTrace(12)
	recorder := &dohTerminalTCPRecorder{phase: phase}
	trace.configureForTest = func(settings *clientconnect.ClientSettings) {
		settings.SendBufferSettings.TransferWireMessageObserver = recorder.observe
	}
	prior := trace.dumpForTest
	trace.dumpForTest = func(t testing.TB, role string) { prior(t, role); recorder.dump(t, role) }
	return trace
}

func TestDohTerminalTCPMetadataIdentityBoundsAndBorrow(t *testing.T) {
	phase := &dohTerminalRetirementPhase{}
	phase.phase.Store(2)
	recorder := &dohTerminalTCPRecorder{phase: phase}
	packet := dohTransferTestPacket(1234, 443, 24)
	binary.BigEndian.PutUint32(packet[24:28], 0xfffffff0)
	binary.BigEndian.PutUint32(packet[28:32], 123)
	packet[33] = 0x18
	message, sequence := clientconnect.NewId(), clientconnect.NewId()
	wire, err := clientconnect.ProtoMarshal(&protocol.TransferFrame{Pack: &protocol.Pack{MessageId: message[:], SequenceId: sequence[:], SequenceNumber: 61,
		Frames: []*protocol.Frame{{MessageType: protocol.MessageType_IpIpPacketToProvider, Raw: true, MessageBytes: packet}}}})
	if err != nil {
		t.Fatal(err)
	}
	defer clientconnect.MessagePoolReturn(wire)
	observer := newDohTransferEndpointObserver(false)
	observer.wire.port.Store(443)
	settings := clientconnect.DefaultClientSettings()
	settings.SendBufferSettings.TransferWireMessageObserver = recorder.observe
	observer.configure(settings, 8*time.Second)
	event := clientconnect.TransferWireMessageObservation{TransferFrameBytes: wire, WireMessageBytes: wire, Resend: true}
	settings.SendBufferSettings.TransferWireMessageObserver(event)
	got := recorder.events[0]
	if recorder.next.Load() != 1 || !recorder.published[0].Load() || got.Message != message || got.Sequence != sequence || got.Number != 61 || got.TCPSequence != 0xfffffff0 || got.TCPAck != 123 || got.Flags != 0x18 || got.PayloadBytes != 24 || got.Phase != 2 || !got.Resend || !got.ToProvider || got.NoAck || got.PacketHash != crc64.Checksum(packet, dohTerminalTCPChecksum) || observer.wire.snapshot().PayloadFrames != 1 {
		t.Fatalf("metadata or existing wire accounting lost: %+v", got)
	}
	for range dohTerminalTCPCapacity + 2 {
		recorder.observe(event)
	}
	if recorder.next.Load() != dohTerminalTCPCapacity+3 || recorder.events[0] != got {
		t.Fatal("bounded first-event prefix overwritten")
	}
	clear(wire)
	clear(packet)
	if recorder.events[0] != got {
		t.Fatal("borrowed packet retained")
	}
}

func TestDohTerminalTCPRetirementPhaseAndNilPath(t *testing.T) {
	if dohTransferRetireForTest != nil {
		t.Fatal("retirement diagnostic enabled by default")
	}
	called := 0
	retire := func() { called++ }
	if allocs := testing.AllocsPerRun(10, func() { retireDohTransferOperation(retire) }); allocs != 0 {
		t.Fatal("ordinary retirement allocates diagnostic metadata")
	}
	phase := &dohTerminalRetirementPhase{}
	phase.retire(func() {
		if phase.phase.Load() != 1 || phase.begin.Load() == 0 || phase.end.Load() != 0 {
			t.Fatal("retirement begin not visible before Close")
		}
	})
	if called == 0 || phase.phase.Load() != 2 || phase.end.Load() < phase.begin.Load() {
		t.Fatal("retirement completion not recorded")
	}
}
