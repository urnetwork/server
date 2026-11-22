// Complete encrypted compact envelopes qualify the small-MTU experiment.
package perfvar

import (
	"encoding/binary"
	"reflect"
	"testing"

	clientconnect "github.com/urnetwork/connect/v2026"
	"github.com/urnetwork/connect/v2026/protocol"
	"google.golang.org/protobuf/proto"
)

// Full proof/control messages keep their existing hybrid policy. These shapes
// exercise the compact IP envelopes admitted by the experimental byte policy.
func TestH3FullTunSingleDatagramMtuCompactEnvelopeBoundaries(t *testing.T) {
	settings := clientconnect.DefaultClientSettings()
	configureH3FullTunPacketTrackClient(settings, 900, newH3FullTunLaneTrace())
	chunkSettings := settings.SendBufferSettings.ChunkSettings
	limit := int(chunkSettings.SharedMaxMessageByteCount)
	if limit <= 0 || 900 < limit || chunkSettings.PureTcpAckMaxFrames != 16 {
		t.Fatalf("invalid small-MTU coalescing policy: %+v", chunkSettings)
	}
	ackCount := min(chunkSettings.PureTcpAckMaxFrames, limit/52)
	if ackCount == 0 {
		t.Fatal("small-MTU policy cannot coalesce one timestamp ACK")
	}
	ackByteCounts := make([]int, ackCount)
	for index := range ackByteCounts {
		ackByteCounts[index] = 52
	}
	for _, packetByteCounts := range [][]int{
		{900},
		{limit / 2, limit - limit/2},
		ackByteCounts,
	} {
		path := &protocol.TransferPath{SourceId: make([]byte, 16), DestinationId: make([]byte, 16)}
		pack := &protocol.Pack{MessageId: make([]byte, 16), SequenceId: make([]byte, 16),
			SequenceNumber: ^uint64(0), Head: true, Nack: true,
			Tag: &protocol.Tag{SendTime: ^uint64(0)}, ContractId: make([]byte, 16),
			ForceStream: true, CompanionContract: true, LogicalLane: 8, DeliverImmediately: true}
		for _, packetByteCount := range packetByteCounts {
			packet := make([]byte, packetByteCount)
			packet[0], packet[9], packet[32], packet[33] = 0x45, 6, 0x50, 0x10
			binary.BigEndian.PutUint16(packet[2:4], uint16(packetByteCount))
			copy(packet[12:16], []byte{192, 0, 2, 1})
			copy(packet[16:20], []byte{198, 51, 100, 2})
			if packetByteCount == 52 {
				packet[32] = 0x80
				copy(packet[40:44], []byte{1, 1, 8, 10})
			}
			pack.Frames = append(pack.Frames, &protocol.Frame{
				MessageType:  protocol.MessageType_IpIpPacketToProvider,
				MessageBytes: packet, Raw: true,
			})
		}
		plain, err := clientconnect.ProtoMarshal(&protocol.TransferFrame{
			TransferPath: path, MessageType: protocol.MessageType_TransferPack.Enum(), Pack: pack,
			SessionRole: protocol.SequenceRole_SequenceRoleServer.Enum(), SessionCompanion: proto.Bool(true),
		})
		if err != nil {
			t.Fatal(err)
		}
		message := h3PacketTrackEncryptPlaintext(t, plain)
		clientconnect.MessagePoolReturn(plain)
		for _, delayed := range []bool{false, true} {
			var delay *clientconnect.TransportDelayH3
			if delayed {
				delay = clientconnect.NewTransportDelayH3Peer(clientconnect.Id{}, clientconnect.DefaultFramerSettings(clientconnect.DefaultH3DatagramSettings().MaxMessageByteCount))
			}
			stream, datagrams := h3FullTunPacketTrackSend(t, h3FullTunPacketTrackDatagramSettings(1), message, clientconnect.H3InitialDatagramByteCount, delay)
			if stream || len(datagrams) != 1 {
				t.Errorf("compact packet shape=%v cap=%d wire=%d delayed=%t stream=%t fragments=%d", packetByteCounts, limit, len(message), delayed, stream, len(datagrams))
			}
		}
	}
}

// Copying the settings prevents one experimental endpoint from changing a
// shared constructor value; narrower owner policy and all other fields survive.
func TestH3FullTunSingleDatagramMtuSettingsIsolation(t *testing.T) {
	for _, applicationMtu := range []int{0, 900, clientconnect.DefaultMtu} {
		for _, lowerLimit := range []clientconnect.ByteCount{0, 64} {
			settings := clientconnect.DefaultClientSettings()
			original := settings.SendBufferSettings
			if lowerLimit > 0 {
				original.ChunkSettings.SharedMaxMessageByteCount = lowerLimit
			}
			before := *original
			configureH3FullTunPacketTrackClient(settings, applicationMtu, newH3FullTunLaneTrace())
			if settings.SendBufferSettings == original {
				t.Fatal("experimental endpoint retained a shared mutable send-settings pointer")
			}
			if !reflect.DeepEqual(*original, before) {
				t.Fatal("experimental endpoint changed the original send settings")
			}
			after := *settings.SendBufferSettings
			if lowerLimit > 0 && after.ChunkSettings.SharedMaxMessageByteCount != lowerLimit {
				t.Fatal("small-MTU experiment raised a lower owner byte limit")
			}
			if applicationMtu == 900 {
				after.ChunkSettings.SharedMaxMessageByteCount = before.ChunkSettings.SharedMaxMessageByteCount
			}
			after.TransferWireMessageObserver = before.TransferWireMessageObserver
			if !reflect.DeepEqual(after, before) {
				t.Fatalf("application MTU=%d changed unrelated sender policy", applicationMtu)
			}
		}
	}
}
