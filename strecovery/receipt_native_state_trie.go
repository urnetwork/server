// The pinned SDK's no-extension NodeCodec is shared by LayoutV0 and LayoutV1.
// Hash-addressed raw StorageProof blobs are decoded without runtime inference.
package strecovery

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"

	"golang.org/x/crypto/blake2b"
)

// One invocation bounds decoded input before allocating, hashing or traversing.
type nativeStorageBudget struct {
	remaining int
}

// Empty bytes are canonical "0x". Limits precede allocation and hex validation.
func (self *nativeStorageBudget) decode(encoded string, maximum int) ([]byte, error) {
	if len(encoded) < 2 || len(encoded)%2 != 0 || len(encoded) > 2+2*maximum || encoded[:2] != "0x" {
		return nil, errors.New("native storage hex or item bound differs")
	}
	count := (len(encoded) - 2) / 2
	if count > self.remaining {
		return nil, errors.New("native storage shared decoded byte bound exceeded")
	}
	self.remaining -= count
	for _, value := range encoded[2:] {
		if !('0' <= value && value <= '9') && !('a' <= value && value <= 'f') {
			return nil, errors.New("native storage hex is not canonical")
		}
	}
	return hex.DecodeString(encoded[2:])
}

// Parsed fields borrow invocation-owned bytes; partials stay packed to avoid
// expansion. Hashed values are resolved only for the exact requested key.
type nativeStorageNode struct {
	empty       bool
	leaf        bool
	partial     []byte
	nibbles     int
	value       []byte
	hasValue    bool
	valueHashed bool
	children    [16][]byte
}

// A node's compact lengths reuse the already reviewed canonical u32 reader.
// Canonical node consumption is stricter than accepting an ignored suffix.
func decodeNativeStorageNode(raw []byte) (*nativeStorageNode, error) {
	reader := &finalityScaleReader{data: raw}
	first, err := reader.take(1)
	if err != nil {
		return nil, err
	}
	node := &nativeStorageNode{}
	bits := 0
	switch {
	case first[0] == 0:
		node.empty = true
	case first[0]&0xc0 == 0x40:
		node.leaf, node.hasValue, bits = true, true, 2
	case first[0]&0xc0 == 0x80:
		bits = 2
	case first[0]&0xc0 == 0xc0:
		node.hasValue, bits = true, 2
	case first[0]&0xe0 == 0x20:
		node.leaf, node.hasValue, node.valueHashed, bits = true, true, true, 3
	case first[0]&0xf0 == 0x10:
		node.hasValue, node.valueHashed, bits = true, true, 4
	default:
		return nil, errors.New("native storage node header is unsupported")
	}
	if !node.empty {
		maximum := byte(255 >> bits)
		node.nibbles = int(first[0] & maximum)
		if node.nibbles == int(maximum) {
			for {
				next, err := reader.take(1)
				if err != nil {
					return nil, err
				}
				node.nibbles += int(next[0])
				if node.nibbles > 2*maximumNativeStorageKeyBytes {
					return nil, errors.New("native storage partial key bound exceeded")
				}
				if next[0] != 255 {
					break
				}
			}
		}
		node.partial, err = reader.take((node.nibbles + 1) / 2)
		if err != nil {
			return nil, err
		}
		if node.nibbles%2 != 0 && node.partial[0]&0xf0 != 0 {
			return nil, errors.New("native storage odd partial padding differs")
		}
		bitmap := uint16(0)
		if !node.leaf {
			rawBitmap, err := reader.take(2)
			if err != nil {
				return nil, err
			}
			bitmap = binary.LittleEndian.Uint16(rawBitmap)
			if bitmap == 0 {
				return nil, errors.New("native storage branch bitmap is empty")
			}
		}
		if node.hasValue {
			length := uint64(32)
			if !node.valueHashed {
				length, err = reader.compact()
				if err != nil || length > maximumNativeStorageItemBytes {
					return nil, errors.New("native storage value length differs")
				}
			}
			node.value, err = reader.take(int(length))
			if err != nil {
				return nil, err
			}
		}
		for index := range 16 {
			if bitmap&(1<<index) == 0 {
				continue
			}
			length, err := reader.compact()
			if err != nil || length == 0 || length > 32 {
				return nil, errors.New("native storage child reference length differs")
			}
			node.children[index], err = reader.take(int(length))
			if err != nil {
				return nil, err
			}
		}
	}
	if reader.offset != len(raw) {
		return nil, errors.New("native storage node has trailing bytes")
	}
	return node, nil
}

// Each hashed blob is decoded at most once. Raw external values need not parse
// as nodes; unrelated bounded proof blobs are harmless and remain uninterpreted.
type nativeStorageTrie struct {
	blobs  map[[32]byte][]byte
	parsed map[[32]byte]*nativeStorageNode
}

// The empty trie is the SDK's intrinsic null node; no other missing hash may
// imply absence. Nonempty children consume at least one key nibble per step.
func (self *nativeStorageTrie) read(ctx context.Context, root [32]byte, key []byte) ([]byte, bool, error) {
	if root == blake2b.Sum256([]byte{0}) {
		return nil, false, nil
	}
	reference := root[:]
	offset := 0
	for step := 0; step <= 2*len(key); step++ {
		if err := ctx.Err(); err != nil {
			return nil, false, err
		}
		raw := reference
		var hash [32]byte
		if len(reference) == 32 {
			copy(hash[:], reference)
			var present bool
			raw, present = self.blobs[hash]
			if !present {
				return nil, false, fmt.Errorf("%w: missing node", ErrNativeStorageIncomplete)
			}
		} else {
			hash = blake2b.Sum256(raw)
		}
		node, cached := self.parsed[hash]
		if !cached {
			var err error
			node, err = decodeNativeStorageNode(raw)
			if err != nil {
				return nil, false, err
			}
			self.parsed[hash] = node
		}
		if node.empty || node.nibbles > 2*len(key)-offset {
			return nil, false, nil
		}
		for index := 0; index < node.nibbles; index++ {
			packed := index + node.nibbles%2
			partial := (node.partial[packed/2] >> (4 * (1 - packed%2))) & 15
			position := offset + index
			wanted := (key[position/2] >> (4 * (1 - position%2))) & 15
			if partial != wanted {
				return nil, false, nil
			}
		}
		offset += node.nibbles
		if offset == 2*len(key) {
			if !node.hasValue {
				return nil, false, nil
			}
			value := node.value
			if node.valueHashed {
				var valueHash [32]byte
				copy(valueHash[:], node.value)
				var present bool
				value, present = self.blobs[valueHash]
				if !present {
					return nil, false, fmt.Errorf("%w: missing value", ErrNativeStorageIncomplete)
				}
			}
			return value, true, nil
		}
		if node.leaf {
			return nil, false, nil
		}
		child := (key[offset/2] >> (4 * (1 - offset%2))) & 15
		reference = node.children[child]
		if len(reference) == 0 {
			return nil, false, nil
		}
		offset++
	}
	return nil, false, errors.New("native storage traversal bound exceeded")
}

// This internal primitive verifies only math against a supplied root. The sole
// public entry point derives that root by replaying receipt and finality proofs.
func verifyNativeStorageReads(ctx context.Context, root string, nodes []string, reads []NativeStorageRead) ([]NativeStorageRead, error) {
	return verifyBoundedNativeStorageReads(ctx, root, nodes, reads, maximumNativeStorageReads, maximumNativeStorageNodes)
}

// Both public profiles supply fixed limits. One invocation decodes/hashes each
// node once and shares its byte budget across all keys and claimed values.
func verifyBoundedNativeStorageReads(ctx context.Context, root string, nodes []string, reads []NativeStorageRead, maximumReads, maximumNodes int) ([]NativeStorageRead, error) {
	if ctx == nil || !canonicalHex(root, 32) || len(nodes) > maximumNodes || len(reads) == 0 || len(reads) > maximumReads {
		return nil, errors.New("native storage root or count differs")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	budget := &nativeStorageBudget{remaining: maximumNativeStorageDecodedBytes}
	trie := &nativeStorageTrie{blobs: map[[32]byte][]byte{}, parsed: map[[32]byte]*nativeStorageNode{}}
	for _, encoded := range nodes {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		raw, err := budget.decode(encoded, maximumNativeStorageItemBytes)
		if err != nil {
			return nil, err
		}
		hash := blake2b.Sum256(raw)
		if _, duplicate := trie.blobs[hash]; duplicate {
			return nil, errors.New("native storage proof contains duplicate nodes")
		}
		trie.blobs[hash] = raw
	}
	rootBytes, _ := hex.DecodeString(root[2:])
	var rootHash [32]byte
	copy(rootHash[:], rootBytes)
	seen := map[string]bool{}
	result := make([]NativeStorageRead, 0, len(reads))
	for index, read := range reads {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		key, err := budget.decode(read.Key, maximumNativeStorageKeyBytes)
		if err != nil {
			return nil, err
		}
		if seen[read.Key] {
			return nil, errors.New("native storage witness contains duplicate keys")
		}
		seen[read.Key] = true
		var expected []byte
		if read.Value != nil {
			expected, err = budget.decode(*read.Value, maximumNativeStorageItemBytes)
			if err != nil {
				return nil, err
			}
		}
		value, present, err := trie.read(ctx, rootHash, key)
		if err != nil {
			return nil, fmt.Errorf("native storage read %d: %w", index, err)
		}
		if present != (read.Value != nil) || !bytes.Equal(value, expected) {
			return nil, fmt.Errorf("native storage read %d differs from proven key/value or absence", index)
		}
		proven := NativeStorageRead{Key: read.Key}
		if present {
			encoded := "0x" + hex.EncodeToString(value)
			proven.Value = &encoded
		}
		result = append(result, proven)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return result, nil
}
