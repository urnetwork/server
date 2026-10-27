// Finalized evm reads for the subnet family readers. Every contract view is
// pinned by the canonical rpc-supplied block hash with requireCanonical set
// (eip-1898); Subtensor's synthetic evm hash is never reconstructed from
// header fields. A run starts from one finalized checkpoint and rechecks it
// before any verdict, so a reorged or regressed read stays unknown.
package monitor

import (
	"bytes"
	"context"
	"encoding/json"
	"math/big"
	"reflect"
	"strings"

	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/core/types"
)

// One finalized evm block identity as the node reported it.
type subnetReplayEvmCheckpoint struct {
	number uint64
	hash   common.Hash
}

// The raw {number, hash} pair of an evm block; other fields are ignored.
type subnetReplayRpcBlock struct {
	Number *hexutil.Big `json:"number"`
	Hash   *common.Hash `json:"hash"`
}

// Validates the explicit identity; a missing or zero field is not a block.
func (self *subnetReplayRpcBlock) identity() (subnetReplayEvmCheckpoint, bool) {
	if self == nil || self.Number == nil || self.Hash == nil || !(*big.Int)(self.Number).IsUint64() || *self.Hash == (common.Hash{}) {
		return subnetReplayEvmCheckpoint{}, false
	}
	return subnetReplayEvmCheckpoint{number: (*big.Int)(self.Number).Uint64(), hash: *self.Hash}, true
}

// Reads the evm chain id, the native genesis and the finalized evm head in
// one batch and compares the chain identity with the pin before any view.
func readSubnetReplayEvmCheckpoint(ctx context.Context, rpc *subnetReplayRpc, pin *SubnetReplayPin) (subnetReplayEvmCheckpoint, error) {
	var chainId *hexutil.Big
	var genesis *common.Hash
	var finalized *subnetReplayRpcBlock
	calls := []*subnetReplayRpcCall{
		{method: "eth_chainId", result: &chainId},
		{method: "chain_getBlockHash", params: []any{0}, result: &genesis},
		{method: "eth_getBlockByNumber", params: []any{"finalized", false}, result: &finalized},
	}
	if err := rpc.call(ctx, calls...); err != nil {
		return subnetReplayEvmCheckpoint{}, err
	}
	if err := subnetReplayRpcFailed(calls...); err != nil {
		return subnetReplayEvmCheckpoint{}, err
	}
	if chainId == nil || !(*big.Int)(chainId).IsUint64() || (*big.Int)(chainId).Uint64() != pin.chainId || genesis == nil || *genesis != pin.genesisHash {
		return subnetReplayEvmCheckpoint{}, newSubnetReplayUnknown("chain-identity-mismatch", nil)
	}
	checkpoint, ok := finalized.identity()
	if !ok || checkpoint.number == 0 {
		return subnetReplayEvmCheckpoint{}, newSubnetReplayUnknown("finalized-head-invalid", nil)
	}
	return checkpoint, nil
}

// Proves the checkpoint is still the canonical block at its height and that
// finality did not regress during the run. Contradictory finalized history is
// a §22.1 incident; for a reader it leaves every verdict unknown.
func recheckSubnetReplayEvmCheckpoint(ctx context.Context, rpc *subnetReplayRpc, checkpoint subnetReplayEvmCheckpoint) error {
	var atHeight *subnetReplayRpcBlock
	var finalized *subnetReplayRpcBlock
	calls := []*subnetReplayRpcCall{
		{method: "eth_getBlockByNumber", params: []any{hexutil.EncodeUint64(checkpoint.number), false}, result: &atHeight},
		{method: "eth_getBlockByNumber", params: []any{"finalized", false}, result: &finalized},
	}
	if err := rpc.call(ctx, calls...); err != nil {
		return err
	}
	if err := subnetReplayRpcFailed(calls...); err != nil {
		return err
	}
	current, ok := atHeight.identity()
	if !ok || current != checkpoint {
		return newSubnetReplayUnknown("checkpoint-not-canonical", nil)
	}
	head, ok := finalized.identity()
	if !ok || head.number < checkpoint.number {
		return newSubnetReplayUnknown("finality-regressed", nil)
	}
	return nil
}

// One block-pinned contract view. A revert is retained only when the caller
// allows it, so an expected typed revert is distinct from a failed read.
type subnetReplayView struct {
	contract    *abi.ABI
	to          common.Address
	method      string
	args        []any
	allowRevert bool
	values      []any
	revert      []byte
	output      *hexutil.Bytes
	call        subnetReplayRpcCall
}

func newSubnetReplayView(contract *abi.ABI, to common.Address, method string, args ...any) *subnetReplayView {
	return &subnetReplayView{contract: contract, to: to, method: method, args: args}
}

// Marks the view as one whose typed revert is meaningful evidence.
func (self *subnetReplayView) revertible() *subnetReplayView {
	self.allowRevert = true
	return self
}

// True when the view reverted with exactly this custom error.
func (self *subnetReplayView) revertedWith(contract *abi.ABI, name string) bool {
	failure, ok := contract.Errors[name]
	return ok && 4 <= len(self.revert) && bytes.Equal(self.revert[:4], failure.ID[:4])
}

// Executes every view at the checkpoint. Empty, malformed or noncanonical abi
// output is a gap: an answer the abi would not re-encode byte for byte is
// refused, exactly as the validator's hash-pinned views refuse it.
func readSubnetReplayViews(ctx context.Context, rpc *subnetReplayRpc, checkpoint subnetReplayEvmCheckpoint, views ...*subnetReplayView) error {
	selector := map[string]any{"blockHash": checkpoint.hash, "requireCanonical": true}
	calls := make([]*subnetReplayRpcCall, 0, len(views))
	for _, view := range views {
		data, err := view.contract.Pack(view.method, view.args...)
		if err != nil {
			return newSubnetReplayUnknown("view-request-invalid", err)
		}
		view.output = nil
		view.values = nil
		view.revert = nil
		view.call = subnetReplayRpcCall{
			method: "eth_call",
			params: []any{map[string]any{"to": view.to, "input": hexutil.Bytes(data)}, selector},
			result: &view.output,
		}
		calls = append(calls, &view.call)
	}
	if err := rpc.call(ctx, calls...); err != nil {
		return err
	}
	for _, view := range views {
		if view.call.failure != nil {
			revert, ok := subnetReplayRevertData(view.call.failure)
			if ok && view.allowRevert {
				view.revert = revert
				continue
			}
			return newSubnetReplayUnknown("view-failed", nil)
		}
		if view.output == nil || len(*view.output) == 0 {
			return newSubnetReplayUnknown("view-empty", nil)
		}
		values, err := view.contract.Unpack(view.method, *view.output)
		if err != nil {
			return newSubnetReplayUnknown("view-malformed", err)
		}
		canonical, err := view.contract.Methods[view.method].Outputs.Pack(values...)
		if err != nil || !bytes.Equal(canonical, *view.output) {
			return newSubnetReplayUnknown("view-noncanonical", err)
		}
		view.values = values
	}
	return nil
}

// Revert bytes travel as a hex string in the json rpc error data.
func subnetReplayRevertData(failure *subnetReplayRpcFailure) ([]byte, bool) {
	if failure == nil || len(failure.Data) == 0 {
		return nil, false
	}
	var encoded string
	if err := json.Unmarshal(failure.Data, &encoded); err != nil || !strings.HasPrefix(encoded, "0x") {
		return nil, false
	}
	raw, err := hexutil.Decode(encoded)
	if err != nil || len(raw) < 4 {
		return nil, false
	}
	return raw, true
}

// Converts one decoded abi output to the binding's typed value. Scalars decode
// to exactly the binding's type; a decoded tuple converts into the binding's
// named struct only when the two shapes correspond field by field, because the
// abi converter recurses without bound on some mismatched pairs (an array
// target with a big integer source), which no recover can catch.
func subnetReplayValue[T any](view *subnetReplayView, index int) (value T, resultErr error) {
	if index < 0 || len(view.values) <= index || view.values[index] == nil {
		return value, newSubnetReplayUnknown("view-shape-invalid", nil)
	}
	if typed, ok := view.values[index].(T); ok {
		return typed, nil
	}
	target := reflect.TypeOf((*T)(nil)).Elem()
	if target.Kind() != reflect.Struct || !subnetReplayShapeCorresponds(reflect.TypeOf(view.values[index]), target) {
		return value, newSubnetReplayUnknown("view-shape-invalid", nil)
	}
	// Any remaining converter panic is a gap, not a crash.
	defer func() {
		if recovered := recover(); recovered != nil {
			var zero T
			value, resultErr = zero, newSubnetReplayUnknown("view-shape-invalid", nil)
		}
	}()
	converted, ok := abi.ConvertType(view.values[index], new(T)).(*T)
	if !ok || converted == nil {
		return value, newSubnetReplayUnknown("view-shape-invalid", nil)
	}
	return *converted, nil
}

// Whether a decoded type maps onto a binding type: identical types, or
// structs, slices and arrays whose members correspond in order and name.
func subnetReplayShapeCorresponds(source reflect.Type, target reflect.Type) bool {
	if source == target {
		return true
	}
	switch {
	case source.Kind() == reflect.Struct && target.Kind() == reflect.Struct:
		if source.NumField() != target.NumField() {
			return false
		}
		for i := 0; i < source.NumField(); i += 1 {
			if source.Field(i).Name != target.Field(i).Name || !subnetReplayShapeCorresponds(source.Field(i).Type, target.Field(i).Type) {
				return false
			}
		}
		return true
	case source.Kind() == reflect.Slice && target.Kind() == reflect.Slice:
		return subnetReplayShapeCorresponds(source.Elem(), target.Elem())
	case source.Kind() == reflect.Array && target.Kind() == reflect.Array:
		return source.Len() == target.Len() && subnetReplayShapeCorresponds(source.Elem(), target.Elem())
	}
	return false
}

// One inclusive finalized log window for one contract address.
type subnetReplayLogWindow struct {
	from uint64
	to   uint64
	logs []types.Log
	call subnetReplayRpcCall
}

// Reads bounded windows of one contract's logs. Every log must belong to the
// address and window, must not be removed, and must be strictly ordered; a
// window that reaches the entry bound is treated as truncated, not complete.
func readSubnetReplayLogs(ctx context.Context, rpc *subnetReplayRpc, address common.Address, maximumEntries int, windows ...*subnetReplayLogWindow) error {
	calls := make([]*subnetReplayRpcCall, 0, len(windows))
	for _, window := range windows {
		if window.to < window.from {
			return newSubnetReplayUnknown("log-window-invalid", nil)
		}
		window.logs = nil
		window.call = subnetReplayRpcCall{
			method: "eth_getLogs",
			params: []any{map[string]any{
				"address":   address,
				"fromBlock": hexutil.EncodeUint64(window.from),
				"toBlock":   hexutil.EncodeUint64(window.to),
			}},
			result: &window.logs,
		}
		calls = append(calls, &window.call)
	}
	if err := rpc.call(ctx, calls...); err != nil {
		return err
	}
	if err := subnetReplayRpcFailed(calls...); err != nil {
		return err
	}
	for _, window := range windows {
		if maximumEntries <= len(window.logs) {
			return newSubnetReplayUnknown("log-window-truncated", nil)
		}
		previousBlock, previousIndex := uint64(0), uint(0)
		for i, log := range window.logs {
			if log.Address != address || log.Removed || log.BlockNumber < window.from || window.to < log.BlockNumber || log.BlockHash == (common.Hash{}) || log.TxHash == (common.Hash{}) || len(log.Topics) == 0 {
				return newSubnetReplayUnknown("log-entry-invalid", nil)
			}
			if 0 < i && (log.BlockNumber < previousBlock || (log.BlockNumber == previousBlock && log.Index <= previousIndex)) {
				return newSubnetReplayUnknown("log-order-invalid", nil)
			}
			previousBlock, previousIndex = log.BlockNumber, log.Index
		}
	}
	return nil
}
