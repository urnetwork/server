// An execution owns one read-only proof transport and one original deadline.
// RPC data supplies candidate nodes only; the original trie/VM must authenticate
// the requested paths against the independently finalized parent state root.
package strecovery

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"time"
)

const MaximumNativeExecutionProofReplyBytes = 129 * 1024 * 1024
const maximumNativeExecutionProofTransferBytes = 256 * 1024 * 1024

// Complete returned contradictions are distinct from missing proof coverage.
var ErrNativeExecutionProofConflict = errors.New("native execution proof evidence conflicts")

// This reader is serial and belongs to a single owned VM capture. It has no
// latest-block method, transaction route, storage writer or finality authority.
type NativeExecutionProofReader struct {
	rpc *receiptCollectorRpc
}

// Separate wire/transfer/node dimensions leave the historical receipt profile
// unchanged. A caller's earlier deadline bounds every subsequent refill too.
func NewNativeExecutionProofReader(endpoint string, budget time.Duration) (*NativeExecutionProofReader, error) {
	if err := collectionRpcUrl(endpoint); err != nil {
		return nil, err
	}
	if budget < time.Minute || budget > 15*time.Minute {
		return nil, errors.New("native proof feed requires a60–900second original owner budget")
	}
	rpc := newReceiptCollectorRpc(endpoint)
	rpc.nativeProof, rpc.requiredFinality = true, true
	rpc.retryWindow, rpc.remaining = budget, maximumNativeExecutionProofTransferBytes
	return &NativeExecutionProofReader{rpc: rpc}, nil
}

// Returned at is checked before any node can enter the caller's owned cache.
// Missing nodes remain unavailable, not proof of an absent storage value.
type NativeExecutionReadProof struct {
	At    string   `json:"at"`
	Proof []string `json:"proof"`
}

func (self *NativeExecutionProofReader) Read(ctx context.Context, parent string, child, key []byte) (*NativeExecutionReadProof, error) {
	if ctx == nil || self == nil || self.rpc == nil || !canonicalHex(parent, 32) || len(key) > 1024 || len(child) > 1024 {
		return nil, errors.New("native proof refill has invalid parent, child or key")
	}
	method := "state_getReadProof"
	params := []any{[]string{"0x" + hex.EncodeToString(key)}, parent}
	if len(child) != 0 {
		method = "state_getChildReadProof"
		params = []any{"0x" + hex.EncodeToString(child), []string{"0x" + hex.EncodeToString(key)}, parent}
	}
	var proof NativeExecutionReadProof
	self.rpc.validateResult = func(raw json.RawMessage) error {
		if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
			return nil
		}
		if err := json.Unmarshal(raw, &proof); err != nil {
			return errors.Join(ErrNativeExecutionProofConflict, err)
		}
		if proof.At != parent || len(proof.Proof) > MaximumNativeExecutionStorageNodes {
			return errors.Join(ErrNativeExecutionProofConflict, errors.New("native proof reply differs from original parent or bounded node profile"))
		}
		return nil
	}
	defer func() { self.rpc.validateResult = nil }()
	_, err := self.rpc.call(ctx, method, params)
	if err != nil {
		return nil, err
	}
	return &proof, nil
}

func (self *NativeExecutionProofReader) Close() {
	if self != nil && self.rpc != nil {
		self.rpc.client.CloseIdleConnections()
		self.rpc = nil
	}
}
