package perfvar

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"encoding/binary"
	"reflect"
	"testing"
	"time"

	quic "github.com/quic-go/quic-go"
	clientconnect "github.com/urnetwork/connect/v2026"
	"github.com/urnetwork/connect/v2026/protocol"
	"google.golang.org/protobuf/proto"
)

// Full-MTU IP bytes travel inside a compact-head Pack and its encrypted outer
// Transfer frame, so the lane selector must account for the complete envelope.
func h3FullTunPacketTrackWireMessage(t testing.TB, packetByteCount int) []byte {
	t.Helper()
	packet := make([]byte, packetByteCount)
	packet[0], packet[9] = 0x45, 6
	binary.BigEndian.PutUint16(packet[2:4], uint16(packetByteCount))
	path := &protocol.TransferPath{
		SourceId: bytes.Repeat([]byte{1}, 16), DestinationId: bytes.Repeat([]byte{2}, 16),
	}
	plain, err := proto.Marshal(&protocol.TransferFrame{TransferPath: path, Pack: &protocol.Pack{
		MessageId: bytes.Repeat([]byte{3}, 16), SequenceId: bytes.Repeat([]byte{4}, 16),
		SequenceNumber: 1, Head: true, ContractId: bytes.Repeat([]byte{5}, 16),
		Frames: []*protocol.Frame{{MessageType: protocol.MessageType_IpIpPacketToProvider, MessageBytes: packet, Raw: true}},
	}})
	if err != nil {
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
		TransferPath: path, EncryptedTransferFrame: aead.Seal(nonce, nonce, plain, nil),
		SessionRole: protocol.SequenceRole_SequenceRoleClient.Enum(), SessionCompanion: proto.Bool(false),
	})
	if err != nil {
		t.Fatal(err)
	}
	return message
}

// Each callback copies the fragmenter's reused scratch buffer and enforces the
// actual live QUIC payload ceiling without opening a socket.
func h3FullTunPacketTrackSend(t testing.TB, settings *clientconnect.H3DatagramSettings, message []byte, pathByteCount int, delay *clientconnect.TransportDelayH3) (bool, [][]byte) {
	t.Helper()
	fragmenter, err := clientconnect.NewH3DatagramFragmenter(settings, &clientconnect.H3DatagramStats{})
	if err != nil {
		t.Fatal(err)
	}
	var datagrams [][]byte
	send := func(datagram []byte) error {
		if pathByteCount < len(datagram) {
			t.Fatalf("DATAGRAM bytes=%d exceeded live path=%d", len(datagram), pathByteCount)
		}
		datagrams = append(datagrams, bytes.Clone(datagram))
		return nil
	}
	var stream bool
	var nextPathByteCount int
	if delay == nil {
		stream, nextPathByteCount, err = fragmenter.SendHybrid(message, pathByteCount, send)
	} else {
		stream, nextPathByteCount, err = delay.SendHybrid(fragmenter, message, pathByteCount, send)
	}
	if err != nil || nextPathByteCount != pathByteCount {
		t.Fatalf("lane send next_path=%d err=%v", nextPathByteCount, err)
	}
	return stream, datagrams
}

// The actual experimental fixture policy must send and reassemble a full-MTU
// encrypted Transfer frame on both legacy and delay-header DATAGRAM wires.
func TestH3FullTunPacketTrackFragmentedSettingsRoundTrip(t *testing.T) {
	message := h3FullTunPacketTrackWireMessage(t, clientconnect.DefaultMtu)
	for _, delayed := range []bool{false, true} {
		settings := h3FullTunPacketTrackDatagramSettings(8)
		var delay *clientconnect.TransportDelayH3
		if delayed {
			delay = clientconnect.NewTransportDelayH3Peer(clientconnect.Id{}, clientconnect.DefaultFramerSettings(settings.MaxMessageByteCount))
		}
		stream, datagrams := h3FullTunPacketTrackSend(t, settings, message, clientconnect.H3InitialDatagramByteCount, delay)
		if stream || len(datagrams) != 2 {
			t.Fatalf("fragmented fixture routed full MTU to stream=%t fragments=%d wire_bytes=%d hybrid_limit=%d max_fragments=%d delayed=%t", stream, len(datagrams), len(message), settings.HybridDatagramMessageByteCount, settings.MaxFragmentCount, delayed)
		}
		reassembler, err := clientconnect.NewH3DatagramReassembler(settings,
			clientconnect.NewH3DatagramReassemblyBudget(settings.ProcessReassemblyByteCount), &clientconnect.H3DatagramStats{})
		if err != nil {
			t.Fatal(err)
		}
		func() {
			defer reassembler.Close()
			for index, datagram := range datagrams {
				var received []byte
				if delay == nil {
					received = reassembler.Accept(datagram, time.Time{})
				} else {
					received = reassembler.AcceptWithHeader(datagram, time.Time{}, make([]byte, delay.HeaderByteCount()))
				}
				if index != len(datagrams)-1 {
					if received != nil {
						clientconnect.MessagePoolReturn(received)
						t.Fatal("incomplete message escaped reassembly")
					}
					continue
				}
				if received == nil {
					t.Fatal("complete fragmented message was not reassembled")
				}
				defer clientconnect.MessagePoolReturn(received)
				if !bytes.Equal(received, message) {
					t.Fatal("fragmented fixture changed encrypted Transfer bytes")
				}
			}
		}()
		t.Logf("delayed=%t mtu=%d wire_bytes=%d fragments=%d", delayed, clientconnect.DefaultMtu, len(message), len(datagrams))
	}
}

// Production and one-DATAGRAM controls retain their original complete-message
// selector and still send small traffic unordered while streaming full MTUs.
func TestH3FullTunPacketTrackDefaultAndSingleDatagramSettings(t *testing.T) {
	for _, fragmentCount := range []int{0, 1} {
		settings := h3FullTunPacketTrackDatagramSettings(fragmentCount)
		if !reflect.DeepEqual(settings, clientconnect.DefaultH3DatagramSettings()) {
			t.Fatalf("fragment override=%d changed production defaults", fragmentCount)
		}
		for _, packetByteCount := range []int{64, clientconnect.DefaultMtu} {
			message := h3FullTunPacketTrackWireMessage(t, packetByteCount)
			stream, datagrams := h3FullTunPacketTrackSend(t, settings, message, clientconnect.H3InitialDatagramByteCount, nil)
			if packetByteCount == 64 {
				if stream || len(datagrams) != 1 {
					t.Fatalf("default small packet stream=%t fragments=%d", stream, len(datagrams))
				}
			} else if !stream || len(datagrams) != 0 {
				t.Fatalf("default full MTU stream=%t fragments=%d", stream, len(datagrams))
			}
		}
	}
}

// Fragmentation changes only the experiment's lane threshold. Message, peer,
// process, age and stream-queue bounds still reject oversized or reduced paths.
func TestH3FullTunPacketTrackFragmentedSettingsBoundaries(t *testing.T) {
	settings := h3FullTunPacketTrackDatagramSettings(8)
	unchanged := *settings
	defaults := clientconnect.DefaultH3DatagramSettings()
	unchanged.MaxFragmentCount = defaults.MaxFragmentCount
	unchanged.HybridDatagramMessageByteCount = defaults.HybridDatagramMessageByteCount
	if !reflect.DeepEqual(&unchanged, defaults) {
		t.Fatal("fragmented control changed an unrelated hard bound")
	}
	for _, testCase := range []struct {
		bytes, path, fragments int
		stream                 bool
	}{
		{bytes: settings.MaxMessageByteCount, path: clientconnect.H3InitialDatagramByteCount, fragments: 8},
		{bytes: settings.MaxMessageByteCount + 1, path: clientconnect.H3InitialDatagramByteCount, stream: true},
		{bytes: 8 * 64, path: clientconnect.H3DatagramHeaderByteCount + 64, fragments: 8},
		{bytes: 8*64 + 1, path: clientconnect.H3DatagramHeaderByteCount + 64, stream: true},
		{bytes: 1, path: clientconnect.H3DatagramHeaderByteCount, stream: true},
	} {
		stream, datagrams := h3FullTunPacketTrackSend(t, settings, make([]byte, testCase.bytes), testCase.path, nil)
		if stream != testCase.stream || len(datagrams) != testCase.fragments {
			t.Fatalf("bytes=%d path=%d stream=%t fragments=%d want_stream=%t want_fragments=%d", testCase.bytes, testCase.path, stream, len(datagrams), testCase.stream, testCase.fragments)
		}
	}
	fragmenter, err := clientconnect.NewH3DatagramFragmenter(settings, &clientconnect.H3DatagramStats{})
	if err != nil {
		t.Fatal(err)
	}
	const reducedPath = clientconnect.H3DatagramHeaderByteCount + 100
	attempts := 0
	stream, nextPath, err := fragmenter.SendHybrid(h3FullTunPacketTrackWireMessage(t, clientconnect.DefaultMtu), clientconnect.H3InitialDatagramByteCount, func([]byte) error {
		attempts++
		return &quic.DatagramTooLargeError{MaxDatagramPayloadSize: reducedPath}
	})
	if err != nil || !stream || nextPath != reducedPath || attempts != 1 {
		t.Fatalf("path reduction stream=%t next_path=%d attempts=%d err=%v", stream, nextPath, attempts, err)
	}
}
