// Live stream admission receives the same original frame identity retained by
// the SDK before sending; later decoded or normalized values cannot replace it.
package controller

import (
	"crypto/sha256"

	"github.com/urnetwork/connect/v2026/protocol"
	"google.golang.org/protobuf/proto"
)

// Unsupported evidence remains unknown while ordinary request handling keeps
// its existing behavior. The zero hash also clears inherited request context.
func providerWorkOriginalRequestFrameHash(frame *protocol.Frame) [32]byte {
	if frame == nil || frame.MessageType != protocol.MessageType_TransferCreateContract || frame.Raw || len(frame.ProtoReflect().GetUnknown()) != 0 || proto.Size(frame) > protocol.MaximumOriginalContractFrameBytes {
		return [32]byte{}
	}
	var request protocol.CreateContract
	if proto.Unmarshal(frame.MessageBytes, &request) != nil || len(request.ProtoReflect().GetUnknown()) != 0 {
		return [32]byte{}
	}
	raw, err := proto.Marshal(frame)
	if err != nil || len(raw) == 0 || len(raw) > protocol.MaximumOriginalContractFrameBytes {
		return [32]byte{}
	}
	return sha256.Sum256(raw)
}
