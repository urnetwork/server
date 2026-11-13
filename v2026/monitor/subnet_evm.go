// SIGNALS.md §22.0 Evm reads bound to canonical Rpc-supplied block hashes.
// Every contract call, code and storage read names its block by Eip-1898 hash
// with requireCanonical, so a moving head or a reorganized block cannot answer.
// Subtensor's synthetic Evm hash is taken from Rpc, never reconstructed.
package monitor

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	ethtypes "github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"

	"github.com/urfoundation/sn/v2026/ss58"
)

// One canonical Evm block identity.
type subnetEvmBlock struct {
	number uint64
	hash   common.Hash
}

// One Evm endpoint of a run session.
type subnetEvmReader struct {
	session  *subnetRpcSession
	endpoint subnetRpcEndpoint
}

// The Eip-1898 selector of an exact canonical block.
func (self subnetEvmBlock) selector() map[string]any {
	return map[string]any{"blockHash": self.hash.Hex(), "requireCanonical": true}
}

func (self subnetEvmReader) chainId(ctx context.Context) (uint64, error) {
	var raw string
	if err := self.session.call(ctx, self.endpoint, "eth_chainId", nil, &raw); err != nil {
		return 0, err
	}
	value, ok := subnetParseQuantity(raw)
	if !ok {
		return 0, subnetReadFailure("chain-id-invalid", nil)
	}
	return value, nil
}

type subnetEvmBlockJson struct {
	Number string `json:"number"`
	Hash   string `json:"hash"`
}

// Decodes one block header reply; null is an absent block.
func subnetDecodeEvmBlock(raw *subnetEvmBlockJson) (subnetEvmBlock, error) {
	if raw == nil {
		return subnetEvmBlock{}, subnetReadFailure("block-absent", nil)
	}
	number, numberOk := subnetParseQuantity(raw.Number)
	hash, hashOk := subnetParseHash(strings.ToLower(raw.Hash))
	if !numberOk || !hashOk || raw.Hash != strings.ToLower(raw.Hash) {
		return subnetEvmBlock{}, subnetReadFailure("block-invalid", nil)
	}
	return subnetEvmBlock{number: number, hash: common.Hash(hash)}, nil
}

// The endpoint's finalized Evm block.
func (self subnetEvmReader) finalized(ctx context.Context) (subnetEvmBlock, error) {
	var raw *subnetEvmBlockJson
	if err := self.session.call(ctx, self.endpoint, "eth_getBlockByNumber", []any{"finalized", false}, &raw); err != nil {
		return subnetEvmBlock{}, err
	}
	return subnetDecodeEvmBlock(raw)
}

// The canonical block at a height, which must report that height.
func (self subnetEvmReader) blockAt(ctx context.Context, number uint64) (subnetEvmBlock, error) {
	var raw *subnetEvmBlockJson
	if err := self.session.call(ctx, self.endpoint, "eth_getBlockByNumber", []any{hexutil.EncodeUint64(number), false}, &raw); err != nil {
		return subnetEvmBlock{}, err
	}
	block, err := subnetDecodeEvmBlock(raw)
	if err == nil && block.number != number {
		return subnetEvmBlock{}, subnetReadFailure("block-number-mismatch", nil)
	}
	return block, err
}

// One view call. A view always returns data; an empty reply is a missing
// contract or method, not a zero value.
func (self subnetEvmReader) call(ctx context.Context, to common.Address, data []byte, at subnetEvmBlock) ([]byte, error) {
	var raw string
	if err := self.session.call(ctx, self.endpoint, "eth_call", []any{map[string]any{"to": to.Hex(), "data": hexutil.Encode(data)}, at.selector()}, &raw); err != nil {
		return nil, err
	}
	output, err := hexutil.Decode(raw)
	if err != nil {
		return nil, subnetReadFailure("call-output-invalid", err)
	}
	if len(output) == 0 {
		return nil, subnetReadFailure("call-output-empty", nil)
	}
	return output, nil
}

// One view call in a batch, with its own output and error.
type subnetEvmCall struct {
	to     common.Address
	data   []byte
	output []byte
	err    error
}

// Bounded batches of view calls at one block. A transport failure fails all.
func (self subnetEvmReader) callBatch(ctx context.Context, calls []subnetEvmCall, at subnetEvmBlock) error {
	for start := 0; start < len(calls); start += self.session.limits.maxBatchCalls {
		end := min(start+self.session.limits.maxBatchCalls, len(calls))
		raws := make([]string, end-start)
		requests := make([]subnetRpcCall, end-start)
		for i := start; i < end; i++ {
			requests[i-start] = subnetRpcCall{method: "eth_call", params: []any{map[string]any{"to": calls[i].to.Hex(), "data": hexutil.Encode(calls[i].data)}, at.selector()}, result: &raws[i-start]}
		}
		if err := self.session.batch(ctx, self.endpoint, requests); err != nil {
			return err
		}
		for i := start; i < end; i++ {
			if err := requests[i-start].err; err != nil {
				calls[i].err = err
				continue
			}
			output, err := hexutil.Decode(raws[i-start])
			switch {
			case err != nil:
				calls[i].err = subnetReadFailure("call-output-invalid", err)
			case len(output) == 0:
				calls[i].err = subnetReadFailure("call-output-empty", nil)
			default:
				calls[i].output = output
			}
		}
	}
	return nil
}

// Deployed runtime bytecode and its keccak256.
func (self subnetEvmReader) codeHash(ctx context.Context, address common.Address, at subnetEvmBlock) (common.Hash, int, error) {
	var raw string
	if err := self.session.call(ctx, self.endpoint, "eth_getCode", []any{address.Hex(), at.selector()}, &raw); err != nil {
		return common.Hash{}, 0, err
	}
	code, err := hexutil.Decode(raw)
	if err != nil {
		return common.Hash{}, 0, subnetReadFailure("code-invalid", err)
	}
	return crypto.Keccak256Hash(code), len(code), nil
}

// One raw storage word.
func (self subnetEvmReader) storageAt(ctx context.Context, address common.Address, slot common.Hash, at subnetEvmBlock) (common.Hash, error) {
	var raw string
	if err := self.session.call(ctx, self.endpoint, "eth_getStorageAt", []any{address.Hex(), slot.Hex(), at.selector()}, &raw); err != nil {
		return common.Hash{}, err
	}
	word, err := hexutil.Decode(raw)
	if err != nil || len(word) != 32 {
		return common.Hash{}, subnetReadFailure("storage-word-invalid", err)
	}
	return common.BytesToHash(word), nil
}

// Logs of one address over an inclusive block range. A reply at the count
// limit is refused rather than treated as complete; the caller narrows the
// range. Removed logs and logs outside the request are invalid.
func (self subnetEvmReader) logs(ctx context.Context, address common.Address, topics [][]common.Hash, from, to uint64) ([]ethtypes.Log, error) {
	filter := map[string]any{"address": address.Hex(), "fromBlock": hexutil.EncodeUint64(from), "toBlock": hexutil.EncodeUint64(to)}
	if len(topics) != 0 {
		filter["topics"] = topics
	}
	var raws []json.RawMessage
	if err := self.session.call(ctx, self.endpoint, "eth_getLogs", []any{filter}, &raws); err != nil {
		return nil, err
	}
	if len(raws) >= self.session.limits.maxLogsPerRequest {
		return nil, subnetReadFailure("logs-overflow", nil)
	}
	requested := map[common.Hash]bool{}
	if len(topics) != 0 {
		for _, topic := range topics[0] {
			requested[topic] = true
		}
	}
	logs := make([]ethtypes.Log, 0, len(raws))
	seen := map[[2]uint64]bool{}
	for _, raw := range raws {
		var log ethtypes.Log
		if err := json.Unmarshal(raw, &log); err != nil {
			return nil, subnetReadFailure("log-invalid", err)
		}
		position := [2]uint64{log.BlockNumber, uint64(log.Index)}
		if log.Removed || log.Address != address || log.BlockNumber < from || to < log.BlockNumber || log.TxHash == (common.Hash{}) || log.BlockHash == (common.Hash{}) || seen[position] || len(log.Topics) == 0 || (len(requested) != 0 && !requested[log.Topics[0]]) {
			return nil, subnetReadFailure("log-invalid", nil)
		}
		seen[position] = true
		logs = append(logs, log)
	}
	return logs, nil
}

type subnetEvmTransactionJson struct {
	Hash      string  `json:"hash"`
	BlockHash string  `json:"blockHash"`
	To        *string `json:"to"`
	Input     string  `json:"input"`
}

// The destination and calldata of one included transaction.
func (self subnetEvmReader) transactionInput(ctx context.Context, hash common.Hash) (common.Address, []byte, common.Hash, error) {
	var raw *subnetEvmTransactionJson
	if err := self.session.call(ctx, self.endpoint, "eth_getTransactionByHash", []any{hash.Hex()}, &raw); err != nil {
		return common.Address{}, nil, common.Hash{}, err
	}
	if raw == nil || raw.To == nil || !strings.EqualFold(raw.Hash, hash.Hex()) || !common.IsHexAddress(*raw.To) {
		return common.Address{}, nil, common.Hash{}, subnetReadFailure("transaction-invalid", nil)
	}
	blockHash, ok := subnetParseHash(strings.ToLower(raw.BlockHash))
	input, err := hexutil.Decode(raw.Input)
	if !ok || err != nil {
		return common.Address{}, nil, common.Hash{}, subnetReadFailure("transaction-invalid", err)
	}
	return common.HexToAddress(*raw.To), input, common.Hash(blockHash), nil
}

// A typed contract view at an exact canonical block.
func subnetEvmView[T any](ctx context.Context, reader subnetEvmReader, at subnetEvmBlock, to common.Address, data []byte, unpack func([]byte) (T, error)) (T, error) {
	var zero T
	output, err := reader.call(ctx, to, data, at)
	if err != nil {
		return zero, err
	}
	value, err := unpack(output)
	if err != nil {
		return zero, subnetReadFailure("call-output-invalid", err)
	}
	return value, nil
}

// A contract's Evm coldkey is the mirrored account of its own address.
func subnetMirroredColdkey(address common.Address) [32]byte {
	return ss58.EvmMirrorPubkey([20]byte(address))
}

// The Subtensor staking precompile reads one (hotkey, coldkey, netuid) stake.
var subnetStakingPrecompile = common.HexToAddress("0x0000000000000000000000000000000000000805")

const subnetStakingAbiJson = `[{"type":"function","name":"getStake","inputs":[{"name":"hotkey","type":"bytes32"},{"name":"coldkey","type":"bytes32"},{"name":"netuid","type":"uint256"}],"outputs":[{"name":"","type":"uint256"}],"stateMutability":"view"}]`
