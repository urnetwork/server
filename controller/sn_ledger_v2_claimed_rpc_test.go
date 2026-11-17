package controller

// The RPC boundary of vault2's claimed read in batches
// (CoreStClient.LedgerV2ClaimedAt), which GET /sn/ctf/client's claim states
// use: every account at the one block given, at most stMaximumEVMRPCBatchCalls
// eth_calls to a request, and each value matched by JSON-RPC id whatever order
// the endpoint answers in.

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"io"
	"math/big"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"

	"github.com/urfoundation/sn/stabi"
)

// stClaimedBatchRPC answers vault2.claimed(noId, index) with index·10 + 7,
// reversing each batch's answers, and records every request.
type stClaimedBatchRPC struct {
	t         *testing.T
	vault     common.Address
	noId      uint64
	finalized uint64
	// an index whose eth_call the endpoint refuses
	refuseIndex *uint64

	lock           sync.Mutex
	batchSizes     []int
	batchBlocks    []string
	finalizedCalls int
}

func (self *stClaimedBatchRPC) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	defer request.Body.Close()
	body, err := io.ReadAll(request.Body)
	if err != nil {
		self.t.Errorf("read claimed RPC body: %v", err)
		return
	}
	writer.Header().Set("Content-Type", "application/json")
	if !strings.HasPrefix(strings.TrimSpace(string(body)), "[") {
		var call stEpochRPCRequest
		if json.Unmarshal(body, &call) != nil {
			self.t.Errorf("invalid single claimed RPC request: %s", body)
			return
		}
		var result any
		switch call.Method {
		case "eth_chainId":
			result = "0x3b1"
		case "eth_getBlockByNumber":
			var selector string
			json.Unmarshal(call.Params[0], &selector)
			if selector != "finalized" {
				self.t.Errorf("block read at %q, want the finalized head", selector)
			}
			self.lock.Lock()
			self.finalizedCalls += 1
			self.lock.Unlock()
			result = stBlockBatchResult(self.finalized)
		default:
			self.t.Errorf("unexpected single claimed RPC method %s", call.Method)
			return
		}
		_ = json.NewEncoder(writer).Encode(map[string]any{"jsonrpc": "2.0", "id": call.ID, "result": result})
		return
	}

	var calls []stEpochRPCRequest
	if json.Unmarshal(body, &calls) != nil || len(calls) == 0 || stMaximumEVMRPCBatchCalls < len(calls) {
		self.t.Errorf("invalid claimed RPC batch of %d calls: %s", len(calls), body)
		return
	}
	parsed, err := stabi.STSettlementVault2MetaData.ParseABI()
	if err != nil {
		self.t.Errorf("parse vault2 ABI: %v", err)
		return
	}
	selector := stLedgerV2VaultAbi.PackClaimed(big.NewInt(0), big.NewInt(0))[:4]
	responses := make([]map[string]any, len(calls))
	for i, call := range calls {
		var callArgs struct {
			To   string `json:"to"`
			Data string `json:"data"`
		}
		var blockTag string
		if call.Method != "eth_call" || len(call.Params) != 2 || json.Unmarshal(call.Params[0], &callArgs) != nil || json.Unmarshal(call.Params[1], &blockTag) != nil {
			self.t.Errorf("invalid claimed batch element %d", i)
			return
		}
		calldata, err := hexutil.Decode(callArgs.Data)
		if err != nil || len(calldata) != 68 || !bytes.Equal(calldata[:4], selector) || !strings.EqualFold(callArgs.To, self.vault.Hex()) {
			self.t.Errorf("invalid claimed call %d to=%s data=%s", i, callArgs.To, callArgs.Data)
			return
		}
		if noId := new(big.Int).SetBytes(calldata[4:36]); !noId.IsUint64() || noId.Uint64() != self.noId {
			self.t.Errorf("claimed call %d of pool %s", i, noId)
			return
		}
		index := new(big.Int).SetBytes(calldata[36:68]).Uint64()
		if i == 0 {
			self.lock.Lock()
			self.batchSizes = append(self.batchSizes, len(calls))
			self.batchBlocks = append(self.batchBlocks, blockTag)
			self.lock.Unlock()
		}
		response := map[string]any{"jsonrpc": "2.0", "id": call.ID}
		if self.refuseIndex != nil && *self.refuseIndex == index {
			response["error"] = map[string]any{"code": -32000, "message": "claimed unavailable"}
		} else {
			encoded, err := parsed.Methods["claimed"].Outputs.Pack(new(big.Int).SetUint64(index*10 + 7))
			if err != nil {
				self.t.Errorf("pack claimed %d: %v", index, err)
				return
			}
			response["result"] = "0x" + hex.EncodeToString(encoded)
		}
		responses[len(calls)-1-i] = response
	}
	if err := json.NewEncoder(writer).Encode(responses); err != nil {
		self.t.Errorf("encode claimed batch response: %v", err)
	}
}

func (self *stClaimedBatchRPC) reads() ([]int, []string, int) {
	self.lock.Lock()
	defer self.lock.Unlock()
	sizes, blocks, finalized := self.batchSizes, self.batchBlocks, self.finalizedCalls
	self.batchSizes, self.batchBlocks, self.finalizedCalls = nil, nil, 0
	return sizes, blocks, finalized
}

// 120 accounts are three requests, 50, 50 and 20 eth_calls, every one at the
// block given, read once from the finalized head; each value is its own
// account's though the endpoint answers every batch backwards. A refused
// call fails the read, naming the account; no account, no request.
func TestCoreStClientLedgerV2ClaimedAtReadsOneBlockInBoundedBatches(t *testing.T) {
	pool := uint64(1<<32 + 1)
	fixture := &stClaimedBatchRPC{t: t, vault: common.HexToAddress(testLedgerV2Vault), noId: pool, finalized: 4_242}
	client := newStBlockBatchClient(t, fixture)
	client.cfg.VaultV2 = fixture.vault
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	block, err := client.LedgerV2FinalizedBlock(ctx)
	if err != nil || block != 4_242 {
		t.Fatalf("finalized block %d, %v", block, err)
	}
	indexes := []uint64{}
	for index := uint64(0); index < 120; index += 1 {
		// out of order, as an answer's newest wins come first
		indexes = append(indexes, (index*37)%120)
	}
	claimed, err := client.LedgerV2ClaimedAt(ctx, pool, block, indexes)
	if err != nil {
		t.Fatal(err)
	}
	sizes, blocks, finalized := fixture.reads()
	if !slices.Equal(sizes, []int{50, 50, 20}) || !slices.Equal(blocks, []string{"0x1092", "0x1092", "0x1092"}) || finalized != 1 {
		t.Fatalf("batches %v at %v after %d finalized reads; want 50, 50 and 20 at 0x1092 after one", sizes, blocks, finalized)
	}
	if len(claimed) != len(indexes) {
		t.Fatalf("%d values for %d accounts", len(claimed), len(indexes))
	}
	for i, index := range indexes {
		if claimed[i].Uint64() != index*10+7 {
			t.Fatalf("claimed of account %d is %s, want %d", index, claimed[i].Dec(), index*10+7)
		}
	}

	refused := uint64(77)
	fixture.refuseIndex = &refused
	if claimed, err := client.LedgerV2ClaimedAt(ctx, pool, block, indexes); err == nil || claimed != nil || !strings.Contains(err.Error(), "claimed(4294967297, 77)") {
		t.Fatalf("a refused call: %v, %v", claimed, err)
	}
	fixture.refuseIndex = nil
	fixture.reads()

	if claimed, err := client.LedgerV2ClaimedAt(ctx, pool, block, nil); err != nil || len(claimed) != 0 {
		t.Fatalf("no account: %v, %v", claimed, err)
	}
	if sizes, _, finalized := fixture.reads(); len(sizes) != 0 || finalized != 0 {
		t.Fatalf("no account: batches %v, %d finalized reads", sizes, finalized)
	}
}
