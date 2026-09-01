// RPC-boundary regression tests for coherent operator epoch snapshots.
package controller

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/ethclient"

	"github.com/urfoundation/sn/stabi"
)

// Minimal inbound JSON-RPC envelope used by the block-pinning endpoint.
type stEpochRPCRequest struct {
	JSONRPC string            `json:"jsonrpc"`
	ID      json.RawMessage   `json:"id"`
	Method  string            `json:"method"`
	Params  []json.RawMessage `json:"params"`
}

// Minimal successful JSON-RPC envelope returned by the endpoint.
type stEpochRPCResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  json.RawMessage `json:"result"`
}

// Encodes one ABI result as the JSON hex string expected by eth_call.
func stEpochRPCResult(t *testing.T, parsedMethod string, value any) json.RawMessage {
	t.Helper()
	parsed, err := stabi.STCoordinatorMetaData.ParseABI()
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := parsed.Methods[parsedMethod].Outputs.Pack(value)
	if err != nil {
		t.Fatal(err)
	}
	result, err := json.Marshal("0x" + hex.EncodeToString(encoded))
	if err != nil {
		t.Fatal(err)
	}
	return result
}

// One Epoch call must fetch one finalized head and pin all three contract
// fields to that exact block, even if the endpoint's live head could advance.
func TestCoreStClientEpochUsesOneFinalizedBlock(t *testing.T) {
	coordinator := stabi.NewSTCoordinator()
	policy := stabi.STCoordinatorPolicySnapshot{
		EpochBlocks: 2_400, RootCommitWindowBlocks: 10, FinalizeOffsetBlocks: 20,
		CloseGraceBlocks: 5, EpochDepositCapRao: big.NewInt(1), CampaignDepositCapRao: big.NewInt(2),
	}
	resultsBySelector := map[string]json.RawMessage{
		hex.EncodeToString(coordinator.PackCurrentEpoch()[:4]):                 stEpochRPCResult(t, "currentEpoch", big.NewInt(7)),
		hex.EncodeToString(coordinator.PackPolicyAt(big.NewInt(7))[:4]):        stEpochRPCResult(t, "policyAt", policy),
		hex.EncodeToString(coordinator.PackEpochStartBlock(big.NewInt(7))[:4]): stEpochRPCResult(t, "epochStartBlock", big.NewInt(700)),
	}
	header := &types.Header{
		ParentHash: common.HexToHash("0x01"), UncleHash: types.EmptyUncleHash,
		Coinbase: common.HexToAddress("0x1000000000000000000000000000000000000001"), Root: common.HexToHash("0x02"),
		TxHash: types.EmptyTxsHash, ReceiptHash: types.EmptyReceiptsHash, Difficulty: big.NewInt(1),
		Number: big.NewInt(100), GasLimit: 30_000_000, Time: 1_700_000_000,
		Extra: []byte{}, MixDigest: common.HexToHash("0x03"), BaseFee: big.NewInt(1),
	}
	headerJSON, err := json.Marshal(header)
	if err != nil {
		t.Fatal(err)
	}
	chainIDJSON, _ := json.Marshal("0x3b1")

	var stateLock sync.Mutex
	chainIDCalls := 0
	finalizedHeadCalls := 0
	contractCalls := 0
	blockTags := []string{}
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		defer request.Body.Close()
		var rpcRequest stEpochRPCRequest
		if err := json.NewDecoder(request.Body).Decode(&rpcRequest); err != nil {
			http.Error(writer, err.Error(), http.StatusBadRequest)
			return
		}
		var result json.RawMessage
		switch rpcRequest.Method {
		case "eth_chainId":
			stateLock.Lock()
			chainIDCalls++
			stateLock.Unlock()
			result = chainIDJSON
		case "eth_getBlockByNumber":
			stateLock.Lock()
			finalizedHeadCalls++
			stateLock.Unlock()
			result = headerJSON
		case "eth_call":
			var call map[string]string
			var blockTag string
			if len(rpcRequest.Params) != 2 || json.Unmarshal(rpcRequest.Params[0], &call) != nil || json.Unmarshal(rpcRequest.Params[1], &blockTag) != nil {
				http.Error(writer, "malformed eth_call", http.StatusBadRequest)
				return
			}
			input := call["input"]
			if input == "" {
				input = call["data"]
			}
			selector := strings.TrimPrefix(input, "0x")
			if len(selector) >= 8 {
				selector = selector[:8]
			}
			result = resultsBySelector[selector]
			if len(result) == 0 {
				http.Error(writer, "unknown selector", http.StatusBadRequest)
				return
			}
			stateLock.Lock()
			contractCalls++
			blockTags = append(blockTags, blockTag)
			stateLock.Unlock()
		default:
			http.Error(writer, "unexpected RPC method", http.StatusBadRequest)
			return
		}
		writer.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(writer).Encode(stEpochRPCResponse{JSONRPC: "2.0", ID: rpcRequest.ID, Result: result})
	}))
	defer server.Close()

	client := &CoreStClient{
		cfg:         &StConfig{RpcUrls: []string{server.URL}, ChainId: 945, ContractAddress: common.HexToAddress("0x2000000000000000000000000000000000000002")},
		coordinator: coordinator, clients: map[string]*ethclient.Client{},
	}
	state, err := client.Epoch(context.Background())
	client.stateLock.Lock()
	for _, rpcClient := range client.clients {
		rpcClient.Close()
	}
	client.stateLock.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	if state.Epoch != 7 || state.EpochStartBlock != 700 || state.HeadBlock != 100 || state.HeadBlockTime != time.Unix(int64(header.Time), 0).UTC() {
		t.Fatalf("epoch state = %+v", state)
	}
	stateLock.Lock()
	defer stateLock.Unlock()
	if chainIDCalls != 1 || finalizedHeadCalls != 1 || contractCalls != 3 {
		t.Fatalf("RPC calls chain-id=%d finalized-head=%d contract=%d", chainIDCalls, finalizedHeadCalls, contractCalls)
	}
	for _, blockTag := range blockTags {
		if blockTag != "0x64" {
			t.Fatalf("contract read block tag = %q, want 0x64", blockTag)
		}
	}
}
