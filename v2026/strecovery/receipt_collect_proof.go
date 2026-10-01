// Proof construction authenticates complete block/receipt vectors before
// exporting any path. Truncated RPC vectors never become partial evidence.
package strecovery

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"strings"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethdb/memorydb"
	"github.com/ethereum/go-ethereum/rlp"
	"github.com/ethereum/go-ethereum/trie"
)

const maximumCollectionBlockTransactions = 2048

// Raw RPC bytes use one canonical spelling; malformed/oversized fields are
// refused before hex decoding. Missing raw capabilities have no JSON fallback.
func collectionHex(encoded string, maximum int) ([]byte, error) {
	if len(encoded) < 4 || len(encoded) > 2+2*maximum || len(encoded)%2 != 0 || !strings.HasPrefix(encoded, "0x") || encoded != strings.ToLower(encoded) {
		return nil, errors.New("receipt collection raw hex is malformed or exceeds its bound")
	}
	return hex.DecodeString(encoded[2:])
}

// Hash exact stored RLP, including Frontier's millisecond timestamp, before
// interpreting its reviewed fifteen-field header. No inferred base fee enters.
func collectionHeader(encoded, hash string, number uint64) (*types.Header, error) {
	raw, err := collectionHex(encoded, maximumCommitmentHeaderBytes)
	if err != nil || crypto.Keccak256Hash(raw).Hex() != hash {
		return nil, errors.New("receipt collection raw header hash differs")
	}
	content, rest, err := rlp.SplitList(raw)
	if err != nil || len(rest) != 0 {
		return nil, errors.New("receipt collection raw header is not one RLP list")
	}
	count, err := rlp.CountValues(content)
	if err != nil || count != 15 {
		return nil, errors.New("receipt collection raw header is outside the Frontier RLP15 profile")
	}
	var header types.Header
	if err := rlp.DecodeBytes(raw, &header); err != nil || header.Number == nil || !header.Number.IsUint64() || header.Number.Uint64() != number ||
		header.TxHash == (common.Hash{}) || header.ReceiptHash == (common.Hash{}) || header.GasLimit == 0 || header.GasUsed > header.GasLimit {
		return nil, errors.New("receipt collection raw header number or gas differs")
	}
	return &header, nil
}

// Whole raw block decoding preserves legacy list and typed-string transaction
// encodings. Only the reviewed block envelope and transaction kinds are used.
func collectionTries(ctx context.Context, header *types.Header, encodedBlock string, encodedReceipts []string) (*trie.Trie, *trie.Trie, error) {
	rawBlock, err := collectionHex(encodedBlock, maximumCollectionReplyBytes/2)
	if err != nil {
		return nil, nil, err
	}
	blockContent, blockRest, blockErr := rlp.SplitList(rawBlock)
	blockCount, countErr := rlp.CountValues(blockContent)
	if blockErr != nil || len(blockRest) != 0 || countErr != nil || blockCount != 3 {
		return nil, nil, errors.New("receipt collection raw block is outside the three-field Frontier envelope")
	}
	var fields []rlp.RawValue
	if err := rlp.DecodeBytes(rawBlock, &fields); err != nil || len(fields) != 3 || crypto.Keccak256Hash(fields[0]) != header.Hash() || !bytes.Equal(fields[2], []byte{0xc0}) {
		return nil, nil, errors.New("receipt collection raw block header or Frontier envelope differs")
	}
	content, rest, err := rlp.SplitList(fields[1])
	if err != nil || len(rest) != 0 {
		return nil, nil, errors.New("receipt collection transaction vector is malformed")
	}
	count, err := rlp.CountValues(content)
	if err != nil || count > maximumCollectionBlockTransactions || count != len(encodedReceipts) {
		return nil, nil, errors.New("receipt collection complete transaction/receipt vector count differs or exceeds bound")
	}
	var entries []rlp.RawValue
	if err := rlp.DecodeBytes(fields[1], &entries); err != nil {
		return nil, nil, err
	}
	transactions, receipts := trie.NewEmpty(nil), trie.NewEmpty(nil)
	var previousCumulative uint64
	for index, entry := range entries {
		if err := ctx.Err(); err != nil {
			return nil, nil, err
		}
		rawTransaction := []byte(entry)
		if len(entry) > 0 && entry[0] < 0xc0 {
			if err := rlp.DecodeBytes(entry, &rawTransaction); err != nil {
				return nil, nil, errors.New("receipt collection typed transaction envelope is malformed")
			}
		}
		var transaction types.Transaction
		if err := transaction.UnmarshalBinary(rawTransaction); err != nil || transaction.Type() > types.DynamicFeeTxType {
			return nil, nil, errors.New("receipt collection transaction is outside the reviewed profile")
		}
		rawReceipt, err := collectionHex(encodedReceipts[index], maximumCommitmentNodeBytes)
		if err != nil {
			return nil, nil, err
		}
		var receipt types.Receipt
		if err := receipt.UnmarshalBinary(rawReceipt); err != nil || len(receipt.PostState) != 0 || receipt.Type != transaction.Type() || receipt.Status > types.ReceiptStatusSuccessful ||
			receipt.CumulativeGasUsed <= previousCumulative || receipt.CumulativeGasUsed-previousCumulative > transaction.Gas() {
			return nil, nil, errors.New("receipt collection raw receipt type, outcome or gas differs")
		}
		previousCumulative = receipt.CumulativeGasUsed
		key, _ := rlp.EncodeToBytes(uint64(index))
		if err := transactions.Update(key, rawTransaction); err != nil {
			return nil, nil, err
		}
		if err := receipts.Update(key, rawReceipt); err != nil {
			return nil, nil, err
		}
	}
	if previousCumulative != header.GasUsed || transactions.Hash() != header.TxHash || receipts.Hash() != header.ReceiptHash {
		return nil, nil, errors.New("receipt collection complete transaction/receipt roots or total gas differ")
	}
	return transactions, receipts, nil
}

// The proof budget is shared by every header and every exported node. A large
// census cannot refresh its allowance for each inclusion or predecessor.
func collectionProof(ctx context.Context, tree *trie.Trie, index uint64, remaining *int) ([]string, error) {
	db := memorydb.New()
	defer db.Close()
	key, _ := rlp.EncodeToBytes(index)
	if err := tree.Prove(key, db); err != nil {
		return nil, err
	}
	iterator := db.NewIterator(nil, nil)
	defer iterator.Release()
	nodes := []string{}
	for iterator.Next() {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		raw := iterator.Value()
		size := 2 + 2*len(raw)
		if len(raw) > maximumCommitmentNodeBytes || len(nodes) >= maximumCommitmentProofNodes || size > *remaining {
			return nil, errors.New("receipt collection proof exceeds its shared node/byte budget")
		}
		*remaining -= size
		nodes = append(nodes, "0x"+hex.EncodeToString(raw))
	}
	return nodes, iterator.Error()
}
