//go:build acklineagetrace

package perfvar

import (
	"encoding/binary"
	"encoding/json"
	"strings"
	"testing"

	clientconnect "github.com/urnetwork/connect/v2026"
)

func h1LoadedSyntheticTCP() []byte {
	packet := make([]byte, 43)
	packet[0], packet[9] = 0x45, 6
	binary.BigEndian.PutUint16(packet[2:4], uint16(len(packet)))
	copy(packet[12:16], []byte{192, 0, 2, 1})
	copy(packet[16:20], []byte{192, 0, 2, 2})
	binary.BigEndian.PutUint16(packet[20:22], 12345)
	binary.BigEndian.PutUint16(packet[22:24], 443)
	binary.BigEndian.PutUint32(packet[24:28], 0xfffffff0)
	binary.BigEndian.PutUint32(packet[28:32], 42)
	packet[32], packet[33] = 5<<4, 0x18
	copy(packet[40:], []byte("XYZ"))
	return packet
}

func TestH1LoadedTransportTCPMetadataDirectionPrivacyAndMalformed(t *testing.T) {
	packet := h1LoadedSyntheticTCP()
	key, metadata, tcp, valid := h1LoadedTCPHeader(packet)
	if !tcp || !valid || metadata.Sequence != 0xfffffff0 || metadata.Ack != 42 || metadata.PayloadBytes != 3 || metadata.Flags != 0x18 {
		t.Fatalf("metadata=%+v tcp=%t valid=%t", metadata, tcp, valid)
	}
	reversed := append([]byte(nil), packet...)
	copy(reversed[12:16], packet[16:20])
	copy(reversed[16:20], packet[12:16])
	copy(reversed[20:22], packet[22:24])
	copy(reversed[22:24], packet[20:22])
	opposite, _, _, ok := h1LoadedTCPHeader(reversed)
	if !ok || opposite != key {
		t.Fatal("reverse TCP flow identity changed")
	}
	r := &h1LoadedTransportRecorder{}
	r.observePacket("edge-to-device", "scheduled", linkScheduleObservation{sequence: 1}, packet)
	r.observePacket("device-to-edge", "scheduled", linkScheduleObservation{sequence: 2}, reversed)
	packet[24] = 0 // Borrowed bytes cannot change already captured metadata.
	if r.packets[0].Sequence != 0xfffffff0 || r.packets[0].Flow != r.packets[1].Flow || r.flowCount != 1 {
		t.Fatal("packet retained bytes or split reverse flow")
	}
	encoded, _ := json.Marshal(r.packets[:r.packetCount])
	for _, secret := range []string{"192.0.2", "12345", "XYZ", "source_port", "destination_port"} {
		if strings.Contains(string(encoded), secret) {
			t.Fatalf("private endpoint/payload emitted: %q", secret)
		}
	}
	for _, mutate := range []func([]byte) []byte{
		func(b []byte) []byte { return b[:19] },
		func(b []byte) []byte { b[0] = 0x44; return b },
		func(b []byte) []byte { b[6] = 0x20; return b },
		func(b []byte) []byte { b[32] = 15 << 4; return b },
		func(b []byte) []byte { b[2], b[3] = 0xff, 0xff; return b },
	} {
		if _, _, _, valid := h1LoadedTCPHeader(mutate(h1LoadedSyntheticTCP())); valid {
			t.Fatal("malformed TCP accepted")
		}
	}
}

func TestH1LoadedTransportRecorderBounds(t *testing.T) {
	r := &h1LoadedTransportRecorder{}
	packet := h1LoadedSyntheticTCP()
	for i := 0; i < h1LoadedPacketCapacity+1; i++ {
		r.observePacket("edge-to-device", "scheduled", linkScheduleObservation{sequence: uint64(i + 1)}, packet)
	}
	if r.packetCount != h1LoadedPacketCapacity || r.packetOverflow != 1 {
		t.Fatalf("packet bounds=%d/%d", r.packetCount, r.packetOverflow)
	}
	if r.packets[0].LinkNumber != 1 || r.packets[h1LoadedPacketCapacity-1].LinkNumber != h1LoadedPacketCapacity {
		t.Fatal("packet prefix overwritten")
	}
	r = &h1LoadedTransportRecorder{}
	for i := 0; i < h1LoadedFlowCapacity+1; i++ {
		binary.BigEndian.PutUint16(packet[20:22], uint16(1000+i))
		r.observePacket("edge-to-device", "scheduled", linkScheduleObservation{}, packet)
	}
	if r.flowCount != h1LoadedFlowCapacity || r.flowOverflow != 1 {
		t.Fatalf("flow bounds=%d/%d", r.flowCount, r.flowOverflow)
	}
	r = &h1LoadedTransportRecorder{}
	for i := 0; i < h1LoadedPacingCapacity+1; i++ {
		r.observePacing(clientconnect.AckLineagePacingSnapshot{})
	}
	if r.pacingNext.Load() != h1LoadedPacingCapacity+1 || r.pacingOverflow.Load() != 1 {
		t.Fatal("pacing overflow concealed")
	}
	for i := range r.pacingPublished {
		if !r.pacingPublished[i].Load() {
			t.Fatal("pacing prefix unpublished")
		}
	}
}
