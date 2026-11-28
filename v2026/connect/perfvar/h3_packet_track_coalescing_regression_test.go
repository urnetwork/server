// The explicit small-MTU experiment must bound complete coalesced messages.
package perfvar

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"encoding/binary"
	"testing"
	"testing/synctest"

	clientconnect "github.com/urnetwork/connect/v2026"
	"github.com/urnetwork/connect/v2026/protocol"
	"google.golang.org/protobuf/proto"
)

// A fixed test-only key applies the same AEAD envelope to the production
// coalescer's actual plaintext without introducing handshake traffic.
func h3PacketTrackEncryptPlaintext(t testing.TB, plain []byte) []byte {
	t.Helper()
	var transfer protocol.TransferFrame
	if err := clientconnect.ProtoUnmarshal(plain, &transfer); err != nil {
		t.Fatal(err)
	}
	block, err := aes.NewCipher(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		t.Fatal(err)
	}
	nonce := make([]byte, aead.NonceSize())
	message, err := proto.Marshal(&protocol.TransferFrame{
		TransferPath:           transfer.TransferPath,
		EncryptedTransferFrame: aead.Seal(nonce, nonce, plain, nil),
		SessionRole:            protocol.SequenceRole_SequenceRoleClient.Enum(),
		SessionCompanion:       proto.Bool(false),
	})
	if err != nil {
		t.Fatal(err)
	}
	return message
}

// The warmup observer holds the real sequence worker while both data owners
// enter its queue. No timer, scheduler race, or repeated run creates the batch.
func TestH3FullTunSingleDatagramMtuBoundsReadyCoalescing(t *testing.T) {
	clientconnect.MessagePoolReturn(clientconnect.MessagePoolGet(1))
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		settings := clientconnect.DefaultClientSettings()
		settings.Log = clientconnect.NewNoopLogger()
		settings.ControlPingTimeout = 0
		settings.ClientKeyRegistrationRequired = true
		settings.EncryptionSettings.Mode = clientconnect.EncryptionModeOff
		configureH3FullTunPacketTrackClient(settings, 900, newH3FullTunLaneTrace())
		observe := settings.SendBufferSettings.TransferWireMessageObserver
		entered, release := make(chan struct{}), make(chan struct{})
		settings.SendBufferSettings.TransferWireMessageObserver = func(event clientconnect.TransferWireMessageObservation) {
			observe(event)
			var transfer protocol.TransferFrame
			if err := clientconnect.ProtoUnmarshal(event.TransferFrameBytes, &transfer); err != nil {
				t.Error(err)
				return
			}
			if pack := transfer.GetPack(); !event.Resend && pack != nil && len(pack.Frames) == 1 && pack.Frames[0].MessageType == protocol.MessageType_TestSimpleMessage {
				close(entered)
				select {
				case <-release:
				case <-ctx.Done():
				}
			}
		}
		client := clientconnect.NewClient(ctx, clientconnect.NewId(), clientconnect.NewNoContractClientOob(), settings)
		peerId := clientconnect.NewId()
		outbound := make(clientconnect.Route, 8)
		client.RouteManager().UpdateTransportWithProperties(
			clientconnect.NewSendGatewayTransportWithType(clientconnect.TransportTypeH3),
			[]clientconnect.Route{outbound},
			clientconnect.TransferCarrierProperties{Unreliable: true, UnreliableFlowIsolation: true, UnreliableFlowReserve: true, UnreliableMaxMessageByteCount: clientconnect.H3InitialDatagramByteCount - clientconnect.H3DatagramHeaderByteCount},
		)
		client.ContractManager().AddNoContractPeer(peerId)
		defer func() {
			cancel()
			if err := client.CloseAndWait(context.Background()); err != nil {
				t.Error(err)
			}
			for len(outbound) > 0 {
				clientconnect.MessagePoolReturn(<-outbound)
			}
		}()
		warmup, err := clientconnect.ToFrame(&protocol.SimpleMessage{Content: "hold ready coalescing"}, clientconnect.DefaultProtocolVersion)
		if err != nil {
			t.Fatal(err)
		}
		opts := clientconnect.TransferOptions{Ack: true, ForceStream: true}
		if !client.SendWithTimeout(warmup, peerId, nil, -1, opts) {
			clientconnect.MessagePoolReturn(warmup.MessageBytes)
			t.Fatal("warmup admission failed")
		}
		<-entered
		var expectedPackets [][]byte
		for _, packetByteCount := range []int{900, 220} {
			packet := make([]byte, packetByteCount)
			packet[0], packet[9], packet[32], packet[33] = 0x45, 6, 0x50, 0x18
			binary.BigEndian.PutUint16(packet[2:4], uint16(packetByteCount))
			copy(packet[12:16], []byte{192, 0, 2, 1})
			copy(packet[16:20], []byte{198, 51, 100, 2})
			expectedPackets = append(expectedPackets, packet)
			frame := &protocol.Frame{MessageType: protocol.MessageType_IpIpPacketToProvider, Raw: true, MessageBytes: clientconnect.MessagePoolCopy(packet)}
			if !client.SendWithTimeout(frame, peerId, nil, -1, opts) {
				clientconnect.MessagePoolReturn(frame.MessageBytes)
				t.Fatal("data admission failed")
			}
		}
		close(release)
		synctest.Wait()
		var actualPackets [][]byte
		for len(outbound) > 0 {
			plain := <-outbound
			var transfer protocol.TransferFrame
			if err := clientconnect.ProtoUnmarshal(plain, &transfer); err != nil {
				clientconnect.MessagePoolReturn(plain)
				t.Fatal(err)
			}
			pack := transfer.GetPack()
			if pack != nil && len(pack.Frames) > 0 && pack.Frames[0].MessageType == protocol.MessageType_IpIpPacketToProvider {
				message := h3PacketTrackEncryptPlaintext(t, plain)
				for _, delayed := range []bool{false, true} {
					var delay *clientconnect.TransportDelayH3
					if delayed {
						delay = clientconnect.NewTransportDelayH3Peer(clientconnect.Id{}, clientconnect.DefaultFramerSettings(clientconnect.DefaultH3DatagramSettings().MaxMessageByteCount))
					}
					stream, datagrams := h3FullTunPacketTrackSend(t, h3FullTunPacketTrackDatagramSettings(1), message, clientconnect.H3InitialDatagramByteCount, delay)
					if stream || len(datagrams) != 1 {
						t.Errorf("900-byte application MTU coalesced %d IP frames into %d encrypted bytes: stream=%t datagrams=%d delayed=%t", len(pack.Frames), len(message), stream, len(datagrams), delayed)
					}
				}
				for _, frame := range pack.Frames {
					actualPackets = append(actualPackets, frame.MessageBytes)
				}
			}
			clientconnect.MessagePoolReturn(plain)
		}
		if len(actualPackets) != len(expectedPackets) {
			t.Fatalf("packet workload changed: got=%d want=%d", len(actualPackets), len(expectedPackets))
		}
		for index, expected := range expectedPackets {
			if !bytes.Equal(actualPackets[index], expected) {
				t.Fatalf("packet %d changed or reordered", index)
			}
		}
	})
}
