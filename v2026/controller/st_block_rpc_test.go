package controller

// Deterministic RPC-boundary tests for Subtensor's explicit EVM block-hash
// domain and bounded canonical-block batches.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/ethclient"
)

// stBlockBatchRPC returns explicit block number/hash/time values and records
// the physical HTTP batches used by the client.
type stBlockBatchRPC struct {
	t                 *testing.T
	finalized         uint64
	tamperedRequested uint64
	batchSizes        []int
	singleBlockCalls  int
}

// stBlockBatchHash creates a deterministic nonzero RPC hash unrelated to a
// locally recomputed Ethereum header hash.
func stBlockBatchHash(block uint64) string {
	return fmt.Sprintf("0x%056x%08x", block+1, block^0xa5a5a5a5)
}

// stBlockBatchResult returns the minimal explicit block object consumed by
// the production decoder.
func stBlockBatchResult(block uint64) map[string]any {
	return map[string]any{
		"number":    hexutil.EncodeUint64(block),
		"hash":      stBlockBatchHash(block),
		"timestamp": hexutil.EncodeUint64(1_700_000_000 + block),
	}
}

// ServeHTTP implements eth_chainId plus single and batched
// eth_getBlockByNumber calls. Batch responses are deliberately reversed to
// prove JSON-RPC IDs, not response position, preserve logical ordering.
func (self *stBlockBatchRPC) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	self.t.Helper()
	defer request.Body.Close()
	body, err := io.ReadAll(request.Body)
	if err != nil {
		self.t.Errorf("read block RPC body: %v", err)
		return
	}
	writer.Header().Set("Content-Type", "application/json")
	if !strings.HasPrefix(strings.TrimSpace(string(body)), "[") {
		var call stEpochRPCRequest
		if json.Unmarshal(body, &call) != nil {
			self.t.Errorf("invalid single block RPC request: %s", body)
			return
		}
		result := any("0x3b1")
		if call.Method == "eth_getBlockByNumber" {
			self.singleBlockCalls++
			var selector string
			if len(call.Params) != 2 || json.Unmarshal(call.Params[0], &selector) != nil {
				self.t.Errorf("invalid single block selector")
				return
			}
			block := self.finalized
			if selector != "finalized" {
				block, err = hexutil.DecodeUint64(selector)
				if err != nil {
					self.t.Errorf("decode single block selector %q: %v", selector, err)
					return
				}
			}
			result = stBlockBatchResult(block)
		} else if call.Method != "eth_chainId" {
			self.t.Errorf("unexpected single block RPC method %s", call.Method)
			return
		}
		if err := json.NewEncoder(writer).Encode(map[string]any{"jsonrpc": "2.0", "id": call.ID, "result": result}); err != nil {
			self.t.Errorf("encode single block response: %v", err)
		}
		return
	}
	var calls []stEpochRPCRequest
	if json.Unmarshal(body, &calls) != nil || len(calls) == 0 || len(calls) > stMaximumEVMRPCBatchCalls {
		self.t.Errorf("invalid block RPC batch: %s", body)
		return
	}
	self.batchSizes = append(self.batchSizes, len(calls))
	responses := make([]map[string]any, len(calls))
	for index, call := range calls {
		var selector string
		if call.Method != "eth_getBlockByNumber" || len(call.Params) != 2 || json.Unmarshal(call.Params[0], &selector) != nil {
			self.t.Errorf("invalid block batch element %d", index)
			return
		}
		block, decodeErr := hexutil.DecodeUint64(selector)
		if decodeErr != nil {
			self.t.Errorf("decode block batch selector %q: %v", selector, decodeErr)
			return
		}
		resultBlock := block
		if block == self.tamperedRequested {
			resultBlock++
		}
		responses[len(calls)-1-index] = map[string]any{"jsonrpc": "2.0", "id": call.ID, "result": stBlockBatchResult(resultBlock)}
	}
	if err := json.NewEncoder(writer).Encode(responses); err != nil {
		self.t.Errorf("encode block batch response: %v", err)
	}
}

// newStBlockBatchClient creates a CoreStClient and synchronously closes every
// cached endpoint connection during test cleanup.
func newStBlockBatchClient(t testing.TB, fixture http.Handler) *CoreStClient {
	t.Helper()
	server := httptest.NewServer(fixture)
	client := &CoreStClient{cfg: &StConfig{RpcUrls: []string{server.URL}, ChainId: 945}, clients: map[string]*ethclient.Client{}}
	t.Cleanup(func() {
		client.stateLock.Lock()
		clients := make([]*ethclient.Client, 0, len(client.clients))
		for _, rpcClient := range client.clients {
			clients = append(clients, rpcClient)
		}
		client.stateLock.Unlock()
		for _, rpcClient := range clients {
			rpcClient.Close()
		}
		server.Close()
	})
	return client
}

// A 120-block canonical surface must consume exactly three HTTP batches while
// preserving every requested hash despite reversed JSON-RPC responses.
func TestCoreStClientBlockHashesUsesExplicitBoundedBatches(t *testing.T) {
	fixture := &stBlockBatchRPC{t: t, finalized: 999}
	client := newStBlockBatchClient(t, fixture)
	blocks := make([]uint64, 120)
	for index := range blocks {
		blocks[index] = uint64(100 + index)
	}
	hashes, err := client.BlockHashes(context.Background(), blocks)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(fixture.batchSizes, []int{50, 50, 20}) || len(hashes) != len(blocks) {
		t.Fatalf("batch sizes/hashes=%v/%d, want [50 50 20]/%d", fixture.batchSizes, len(hashes), len(blocks))
	}
	for index, block := range blocks {
		if common.BytesToHash(hashes[index][:]).Hex() != common.HexToHash(stBlockBatchHash(block)).Hex() {
			t.Fatalf("block %d hash=%s want=%s", block, common.BytesToHash(hashes[index][:]).Hex(), stBlockBatchHash(block))
		}
	}
}

// The finalized identity must expose the endpoint's explicit hash and
// timestamp, not a locally recomputed Ethereum header hash.
func TestCoreStClientFinalizedHeadUsesExplicitRPCHashDomain(t *testing.T) {
	fixture := &stBlockBatchRPC{t: t, finalized: 777}
	client := newStBlockBatchClient(t, fixture)
	number, hash, blockTime, err := client.FinalizedHead(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	wantTime := time.Unix(1_700_000_777, 0).UTC()
	if number != 777 || common.BytesToHash(hash[:]).Hex() != common.HexToHash(stBlockBatchHash(777)).Hex() || !blockTime.Equal(wantTime) || fixture.singleBlockCalls != 1 {
		t.Fatalf("finalized number/hash/time/calls=%d/%s/%s/%d", number, common.BytesToHash(hash[:]).Hex(), blockTime, fixture.singleBlockCalls)
	}
}

// Malformed number, hash, and timestamp fields cannot enter checkpoint or
// receipt comparisons through either the single-block or batched RPC path.
func TestDecodeStRPCBlockIdentityRejectsMalformedCanonicalFields(t *testing.T) {
	requested := uint64(7)
	valid := &stRPCBlockIdentity{Number: "0x7", Hash: stBlockBatchHash(7), Timestamp: "0x6553f107"}
	identity, err := decodeStRPCBlockIdentity(valid, &requested)
	if err != nil || identity.Number != requested || identity.Hash == ([32]byte{}) || !identity.Time.Equal(time.Unix(1_700_000_007, 0).UTC()) {
		t.Fatalf("valid identity=%+v error=%v", identity, err)
	}

	badNumber := *valid
	badNumber.Number = "7"
	mismatchedNumber := *valid
	mismatch := uint64(8)
	shortHash := *valid
	shortHash.Hash = "0x01"
	zeroHash := *valid
	zeroHash.Hash = common.Hash{}.Hex()
	badTimestamp := *valid
	badTimestamp.Timestamp = "tomorrow"
	overflowTimestamp := *valid
	overflowTimestamp.Timestamp = hexutil.EncodeUint64(uint64(1) << 63)
	cases := []struct {
		label     string
		block     *stRPCBlockIdentity
		requested *uint64
	}{
		{label: "nil block", requested: &requested},
		{label: "invalid number", block: &badNumber, requested: &requested},
		{label: "mismatched number", block: &mismatchedNumber, requested: &mismatch},
		{label: "short hash", block: &shortHash, requested: &requested},
		{label: "zero hash", block: &zeroHash, requested: &requested},
		{label: "invalid timestamp", block: &badTimestamp, requested: &requested},
		{label: "overflow timestamp", block: &overflowTimestamp, requested: &requested},
	}
	for _, testCase := range cases {
		if value, err := decodeStRPCBlockIdentity(testCase.block, testCase.requested); err == nil {
			t.Errorf("%s accepted as %+v", testCase.label, value)
		}
	}
}

// Duplicate requests and mismatched embedded heights fail closed; a mismatch
// cannot be reassigned to the requested position by batch order.
func TestCoreStClientBlockHashesRejectsDuplicateAndMismatchedNumbers(t *testing.T) {
	duplicateFixture := &stBlockBatchRPC{t: t, finalized: 999}
	duplicateClient := newStBlockBatchClient(t, duplicateFixture)
	if _, err := duplicateClient.BlockHashes(context.Background(), nil); err == nil || len(duplicateFixture.batchSizes) != 0 || duplicateFixture.singleBlockCalls != 0 {
		t.Fatalf("empty blocks error=%v batches/singles=%v/%d", err, duplicateFixture.batchSizes, duplicateFixture.singleBlockCalls)
	}
	if _, err := duplicateClient.BlockHashes(context.Background(), []uint64{7, 7}); err == nil || len(duplicateFixture.batchSizes) != 0 {
		t.Fatalf("duplicate blocks error=%v batches=%v", err, duplicateFixture.batchSizes)
	}

	mismatchFixture := &stBlockBatchRPC{t: t, finalized: 999, tamperedRequested: 8}
	mismatchClient := newStBlockBatchClient(t, mismatchFixture)
	if _, err := mismatchClient.BlockHashes(context.Background(), []uint64{7, 8, 9}); err == nil || !strings.Contains(err.Error(), "does not match requested 8") {
		t.Fatalf("mismatched block error=%v", err)
	}
}
