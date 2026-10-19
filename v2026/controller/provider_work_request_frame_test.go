// Request provenance follows the exact received frame through ordinary decoding
// without substituting a newly encoded inner message or a previous request.
package controller

import (
	"bytes"
	"crypto/sha256"
	"testing"

	"github.com/urnetwork/connect/v2026/protocol"
	"google.golang.org/protobuf/proto"
)

// A deliberately noncanonical field ordering remains the original frame body.
// Re-encoding the parsed request would silently choose a different commitment.
func TestProviderWorkRequestFrameRetainsExactReceivedBody(t *testing.T) {
	request := &protocol.CreateContract{DestinationId: bytes.Repeat([]byte{21}, 16), TransferByteCount: 97}
	body, err := proto.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	// A duplicate scalar with its identical value is valid protobuf and keeps
	// the decoded request unchanged while proving raw body custody.
	body = append(body, 0x10, 97)
	frame := &protocol.Frame{MessageType: protocol.MessageType_TransferCreateContract, MessageBytes: body}
	original, err := proto.Marshal(frame)
	if err != nil {
		t.Fatal(err)
	}
	expected := sha256.Sum256(original)
	if actual := providerWorkOriginalRequestFrameHash(frame); actual != expected || actual == ([32]byte{}) {
		t.Fatal("live request lost exact received frame identity", actual, expected)
	}
	var parsed protocol.CreateContract
	if err := proto.Unmarshal(body, &parsed); err != nil {
		t.Fatal(err)
	}
	canonical, err := proto.Marshal(&parsed)
	if err != nil {
		t.Fatal(err)
	}
	frame.MessageBytes = canonical
	if providerWorkOriginalRequestFrameHash(frame) == expected {
		t.Fatal("fixture failed to distinguish original from reconstructed request")
	}
}

// Unsupported originals cannot gain a nonzero source commitment. Ordinary
// controller validation remains separate from this optional evidence profile.
func TestProviderWorkRequestFrameUnknownProfilesStayUnknown(t *testing.T) {
	for _, frame := range []*protocol.Frame{
		nil,
		{MessageType: protocol.MessageType_TransferCloseContract},
		{MessageType: protocol.MessageType_TransferCreateContract, Raw: true},
		{MessageType: protocol.MessageType_TransferCreateContract, MessageBytes: []byte{0xff}},
		{MessageType: protocol.MessageType_TransferCreateContract, MessageBytes: make([]byte, protocol.MaximumOriginalContractFrameBytes+1)},
	} {
		if providerWorkOriginalRequestFrameHash(frame) != ([32]byte{}) {
			t.Fatal("unsupported request acquired original stream authority", frame)
		}
	}
}
