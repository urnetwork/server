// Local HTTP fixtures retain exact signed histories and independently built
// tries. Faults occur at explicit read boundaries, never via scheduler timing.
package strecovery

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/rlp"
)

// One callback changes one read after the independent fixture produced it.
// Per-method counts force route changes after, rather than before, body reads.
type collectionTestCall struct {
	Method string
	Count  int
	Params []json.RawMessage
}

// The test server exposes exactly the read methods admitted by the collector.
func collectionTestServer(t testing.TB, fixture *receiptCommitmentFixture, fault func(collectionTestCall, any) (any, int)) (*httptest.Server, ReceiptCollectionConfig) {
	t.Helper()
	config := ReceiptCollectionConfig{Schema: ReceiptCollectionConfigSchema, CensusHash: fixture.archive.CensusHash, Source: "synthetic-owned-rpc", NativeChain: "synthetic-chain",
		NativeFinalized: fixture.observations.NativeFinalized, EvmFinalized: fixture.observations.EvmFinalized, MappingEvidenceHash: fixture.observations.MappingEvidenceHash}
	block := types.NewBlockWithHeader(fixture.headers[0]).WithBody(types.Body{Transactions: fixture.transactions})
	rawBlock, err := rlp.EncodeToBytes(block)
	if err != nil {
		t.Fatal(err)
	}
	rawReceipts := []string{}
	for _, receipt := range fixture.receipts {
		raw, err := receipt.MarshalBinary()
		if err != nil {
			t.Fatal(err)
		}
		rawReceipts = append(rawReceipts, "0x"+hex.EncodeToString(raw))
	}
	var stateLock sync.Mutex
	counts := map[string]int{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Id     int               `json:"id"`
			Method string            `json:"method"`
			Params []json.RawMessage `json:"params"`
		}
		if r.Method != http.MethodPost || json.NewDecoder(r.Body).Decode(&request) != nil {
			t.Error("unexpected collection request envelope")
			w.WriteHeader(400)
			return
		}
		stateLock.Lock()
		defer stateLock.Unlock()
		counts[request.Method]++
		call := collectionTestCall{Method: request.Method, Count: counts[request.Method], Params: request.Params}
		var result any
		hashSelector := func(position int) string {
			var selector struct {
				Hash      string `json:"blockHash"`
				Canonical bool   `json:"requireCanonical"`
			}
			if position >= len(request.Params) || json.Unmarshal(request.Params[position], &selector) != nil || !selector.Canonical {
				t.Error("historical read lost its canonical hash selector")
			}
			return selector.Hash
		}
		switch request.Method {
		case "eth_chainId":
			result = hexutil.EncodeUint64(fixture.archive.Selection.ChainId)
		case "system_chain":
			result = config.NativeChain
		case "chain_getBlockHash":
			var number uint64
			json.Unmarshal(request.Params[0], &number)
			if number == 0 {
				result = fixture.archive.Selection.Genesis
			} else if number == config.NativeFinalized.Number {
				result = config.NativeFinalized.Hash
			} else {
				t.Error("native lookup inferred an EVM height")
			}
		case "eth_getTransactionCount":
			if hashSelector(1) != config.EvmFinalized.Hash {
				t.Error("nonce read moved off the selected EVM hash")
			}
			var address string
			json.Unmarshal(request.Params[0], &address)
			for _, role := range fixture.archive.Selection.Roles {
				if role.Address == address {
					result = hexutil.EncodeUint64(role.NextNonce)
				}
			}
		case "eth_getTransactionReceipt":
			var hash string
			json.Unmarshal(request.Params[0], &hash)
			for _, observation := range fixture.observations.Receipts {
				if observation.Hash == hash && observation.Receipt != nil {
					receipt := observation.Receipt
					price, _ := new(big.Int).SetString(receipt.EffectiveGasPrice, 10)
					result = map[string]any{"transactionHash": hash, "type": hexutil.EncodeUint64(uint64(*receipt.Type)), "status": hexutil.EncodeUint64(*receipt.Status),
						"blockNumber": hexutil.EncodeUint64(receipt.BlockNumber), "blockHash": receipt.BlockHash, "transactionIndex": hexutil.EncodeUint64(*receipt.TransactionIndex),
						"gasUsed": hexutil.EncodeUint64(receipt.GasUsed), "cumulativeGasUsed": hexutil.EncodeUint64(receipt.CumulativeGasUsed), "effectiveGasPrice": hexutil.EncodeBig(price)}
				}
			}
		case "eth_getBlockByNumber":
			var encoded string
			json.Unmarshal(request.Params[0], &encoded)
			number, _ := hexutil.DecodeUint64(encoded)
			for _, header := range fixture.headers {
				if number == header.Number.Uint64() {
					result = map[string]any{"hash": header.Hash().Hex(), "number": encoded, "gasUsed": hexutil.EncodeUint64(header.GasUsed), "gasLimit": hexutil.EncodeUint64(header.GasLimit), "baseFeePerGas": "0x5a"}
				}
			}
		case "debug_getRawHeader":
			hash := hashSelector(0)
			for i, header := range fixture.headers {
				if header.Hash().Hex() == hash {
					result = fixture.commitments.Headers[i]
				}
			}
		case "debug_getRawBlock":
			if hashSelector(0) != fixture.headers[0].Hash().Hex() {
				t.Error("raw block identity differs")
			}
			result = "0x" + hex.EncodeToString(rawBlock)
		case "debug_getRawReceipts":
			if hashSelector(0) != fixture.headers[0].Hash().Hex() {
				t.Error("raw receipts identity differs")
			}
			result = append([]string{}, rawReceipts...)
		default:
			t.Errorf("unexpected or mutating method %s", request.Method)
			w.WriteHeader(400)
			return
		}
		if fault != nil {
			var status int
			result, status = fault(call, result)
			if status != 0 {
				w.WriteHeader(status)
				return
			}
		}
		json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": request.Id, "result": result})
	}))
	config.RpcUrl = server.URL
	t.Cleanup(server.Close)
	return server, config
}

// Original, replacement and reverted cancellation histories all survive the
// real HTTP/collector/offline replay path without changing the source archive.
func TestReceiptCollectionPreservesSignedHistoriesWithoutAuthority(t *testing.T) {
	for _, winner := range []int{1, 2, 3} {
		fixture := receiptCommitmentTestFixture(t, winner)
		_, config := collectionTestServer(t, fixture, nil)
		before := objectDigest(fixture.archive)
		collection, err := CollectReceiptEvidence(context.Background(), fixture.archive, config)
		if err != nil {
			t.Fatal(err)
		}
		result, err := VerifyReceiptCollection(context.Background(), fixture.archive, collection)
		if err != nil || !result.FoundReceiptCommitmentsVerified || !result.EvmHeaderAncestryVerified || len(result.Observations.Transactions) != len(fixture.archive.Transactions) || len(result.Receipts) != len(fixture.hashIndexes) ||
			result.FinalityAuthenticated || result.AccountNoncesAuthenticated || result.CanonicalReceiptsReconciled || result.ActualFeesReconciled || result.SpendingAuthorized || before != objectDigest(fixture.archive) {
			t.Fatalf("collector lost history or invented authority: %v", err)
		}
		for _, receipt := range result.Receipts {
			if receipt.ActualGasFee != nil {
				t.Fatal("collector manufactured actual fees")
			}
		}
		if !reflect.DeepEqual(collection.Observations.Receipts, fixture.observations.Receipts) {
			t.Fatal("collector changed explicit receipt status/index or an absent sibling")
		}
	}
}

// Truncation of the complete receipt vector must fail before path export,
// including when every selected archived transaction precedes the omission.
func TestReceiptCollectionRejectsPartialRawReceiptVector(t *testing.T) {
	fixture := receiptCommitmentTestFixture(t, 1)
	_, config := collectionTestServer(t, fixture, func(call collectionTestCall, result any) (any, int) {
		if call.Method == "debug_getRawReceipts" {
			entries := result.([]string)
			return entries[:len(entries)-1], 0
		}
		return result, 0
	})
	collection, err := CollectReceiptEvidence(context.Background(), fixture.archive, config)
	if err == nil || collection != nil || !strings.Contains(err.Error(), "vector count differs") {
		t.Fatalf("partial block became proof: %v", err)
	}
}

// A same-length vector with valid but changed consensus bytes must not acquire
// membership merely because the receipt JSON still has plausible gas/status.
func TestReceiptCollectionRejectsConflictingRawReceipts(t *testing.T) {
	fixture := receiptCommitmentTestFixture(t, 1)
	_, config := collectionTestServer(t, fixture, func(call collectionTestCall, result any) (any, int) {
		if call.Method == "debug_getRawReceipts" {
			entries := result.([]string)
			receipt := *fixture.receipts[0]
			receipt.Status ^= 1
			raw, _ := receipt.MarshalBinary()
			entries[0] = "0x" + hex.EncodeToString(raw)
			return entries, 0
		}
		return result, 0
	})
	collection, err := CollectReceiptEvidence(context.Background(), fixture.archive, config)
	if err == nil || collection != nil || !strings.Contains(err.Error(), "roots or total gas differ") {
		t.Fatalf("substituted raw receipt became proof: %v", err)
	}
}

// Complete-vector roots alone do not authenticate a fabricated selected gas
// claim. The public producer must replay the offline per-position verifier.
func TestReceiptCollectionRejectsForgedObservedGas(t *testing.T) {
	fixture := receiptCommitmentTestFixture(t, 1)
	_, config := collectionTestServer(t, fixture, func(call collectionTestCall, result any) (any, int) {
		if call.Method == "eth_getTransactionReceipt" && result != nil {
			result.(map[string]any)["gasUsed"] = "0x4e20"
		}
		return result, 0
	})
	collection, err := CollectReceiptEvidence(context.Background(), fixture.archive, config)
	if err == nil || collection != nil || !strings.Contains(err.Error(), "gas or outcome differs") {
		t.Fatalf("forged observed gas escaped final replay: %v", err)
	}
}

// Late canonical drift occurs only after raw data has been read. No complete
// looking artifact can be published under the earlier selected boundary.
func TestReceiptCollectionRechecksBoundaryAfterBodyReads(t *testing.T) {
	fixture := receiptCommitmentTestFixture(t, 1)
	_, config := collectionTestServer(t, fixture, func(call collectionTestCall, result any) (any, int) {
		if call.Method == "eth_getBlockByNumber" && call.Count == 4 {
			result.(map[string]any)["hash"] = "0x" + strings.Repeat("b", 64)
		}
		return result, 0
	})
	collection, err := CollectReceiptEvidence(context.Background(), fixture.archive, config)
	if err == nil || collection != nil || !strings.Contains(err.Error(), "canonical EVM block identity") {
		t.Fatalf("late boundary drift escaped: %v", err)
	}
}

// Network/genesis changes are not hidden by stable EVM block hashes or cached
// headers. The second network observation follows all account and body reads.
func TestReceiptCollectionRechecksNetworkAfterBodyReads(t *testing.T) {
	fixture := receiptCommitmentTestFixture(t, 1)
	_, config := collectionTestServer(t, fixture, func(call collectionTestCall, result any) (any, int) {
		if call.Method == "eth_chainId" && call.Count == 2 {
			return "0x1", 0
		}
		return result, 0
	})
	collection, err := CollectReceiptEvidence(context.Background(), fixture.archive, config)
	if err == nil || collection != nil || !strings.Contains(err.Error(), "network or supplied native boundary") {
		t.Fatalf("late route switch escaped: %v", err)
	}
}

// Missing zero-valued quantities cannot silently become legacy/success/index0.
func TestReceiptCollectionRequiresCompleteReceiptProjection(t *testing.T) {
	for _, field := range []string{"type", "status", "transactionIndex", "effectiveGasPrice"} {
		fixture := receiptCommitmentTestFixture(t, 1)
		_, config := collectionTestServer(t, fixture, func(call collectionTestCall, result any) (any, int) {
			if call.Method == "eth_getTransactionReceipt" && result != nil {
				delete(result.(map[string]any), field)
			}
			return result, 0
		})
		collection, err := CollectReceiptEvidence(context.Background(), fixture.archive, config)
		if err == nil || collection != nil {
			t.Fatalf("missing %s acquired a zero default", field)
		}
	}
}

// A missing raw method or null response cannot fall back to rendered headers
// or turn missing receipt bytes into an empty successful block.
func TestReceiptCollectionRequiresRawCapabilities(t *testing.T) {
	for _, method := range []string{"debug_getRawHeader", "debug_getRawBlock", "debug_getRawReceipts"} {
		fixture := receiptCommitmentTestFixture(t, 1)
		_, config := collectionTestServer(t, fixture, func(call collectionTestCall, result any) (any, int) {
			if call.Method == method {
				return nil, 0
			}
			return result, 0
		})
		collection, err := CollectReceiptEvidence(context.Background(), fixture.archive, config)
		if err == nil || collection != nil || !strings.Contains(err.Error(), "unavailable or malformed") {
			t.Fatalf("absent raw capability %s fell back: %v", method, err)
		}
	}
}

// Past the former three-attempt limit, the same hash-selected body still
// succeeds. The wait hook proves attempts deterministically without sleeping.
func TestReceiptCollectionRetriesTransientReadsBeyondThreeAttempts(t *testing.T) {
	fixture := receiptCommitmentTestFixture(t, 1)
	_, config := collectionTestServer(t, fixture, func(call collectionTestCall, result any) (any, int) {
		if call.Method == "debug_getRawReceipts" && call.Count <= 5 {
			return nil, http.StatusGatewayTimeout
		}
		return result, 0
	})
	client := newReceiptCollectorRpc(config.RpcUrl)
	defer client.client.CloseIdleConnections()
	waits := 0
	client.wait = func(ctx context.Context, duration time.Duration) error {
		waits++
		if duration != time.Second {
			t.Fatal("retry lost pacing")
		}
		return ctx.Err()
	}
	collection, err := collectReceiptEvidence(context.Background(), fixture.archive, config, client)
	if err != nil || collection == nil || waits != 5 || client.retryWindow != 300*time.Second {
		t.Fatalf("bounded retry stopped early or changed default: waits %d, error %v", waits, err)
	}
}

// The first retry barrier cancels the entire collection. No raw/header prefix
// or partially read archive census may escape as a successful result.
func TestReceiptCollectionCancellationAtRetryReturnsNoEvidence(t *testing.T) {
	fixture := receiptCommitmentTestFixture(t, 1)
	_, config := collectionTestServer(t, fixture, func(call collectionTestCall, result any) (any, int) {
		if call.Method == "debug_getRawReceipts" {
			return nil, http.StatusBadGateway
		}
		return result, 0
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	client := newReceiptCollectorRpc(config.RpcUrl)
	defer client.client.CloseIdleConnections()
	client.wait = func(ctx context.Context, _ time.Duration) error { cancel(); return ctx.Err() }
	collection, err := collectReceiptEvidence(ctx, fixture.archive, config, client)
	if !errors.Is(err, context.Canceled) || collection != nil {
		t.Fatalf("canceled prefix escaped: %v", err)
	}
}

// A sealed artifact is created once; changed evidence cannot overwrite it,
// and a proof failure or cancellation cannot publish a new evidence path.
func TestReceiptCollectionPublicationIsPrivateVerifiedAndCreateOnly(t *testing.T) {
	fixture := receiptCommitmentTestFixture(t, 1)
	_, config := collectionTestServer(t, fixture, nil)
	collection, err := CollectReceiptEvidence(context.Background(), fixture.archive, config)
	if err != nil {
		t.Fatal(err)
	}
	directory := t.TempDir()
	if err := os.Chmod(directory, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(directory, "collection.json")
	if err := WriteReceiptCollection(context.Background(), path, fixture.archive, collection); err != nil {
		t.Fatal(err)
	}
	firstInfo, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := WriteReceiptCollection(context.Background(), path, fixture.archive, collection); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	// The shared publisher seals the writable temporary file to 0400 before
	// fsync/rename. An identical retry must reuse that original sealed inode.
	if info.Mode().Perm() != 0400 || !info.Mode().IsRegular() || !os.SameFile(firstInfo, info) {
		t.Fatalf("evidence publication must preserve one read-only private regular inode: mode %v", info.Mode())
	}
	loaded, err := LoadReceiptCollection(context.Background(), FileReference{Path: path, Sha256: digest(raw)})
	if err != nil || loaded.ContentHash != collection.ContentHash {
		t.Fatalf("pinned evidence replay failed: %v", err)
	}
	// A correct seal cannot bypass physical privacy/alias checks on either
	// replay or an identical publication to an already existing destination.
	if err := os.Chmod(path, 0440); err != nil {
		t.Fatal(err)
	}
	if err := WriteReceiptCollection(context.Background(), path, fixture.archive, collection); err == nil {
		t.Fatal("exposed existing evidence was reused")
	}
	if _, err := LoadReceiptCollection(context.Background(), FileReference{Path: path, Sha256: digest(raw)}); err == nil {
		t.Fatal("exposed evidence was loaded")
	}
	if err := os.Chmod(path, 0400); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(directory, "alias.json")
	if err := os.Symlink(path, alias); err != nil {
		t.Fatal(err)
	}
	if err := WriteReceiptCollection(context.Background(), alias, fixture.archive, collection); err == nil {
		t.Fatal("symlink destination was reused")
	}
	if err := os.Remove(alias); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(path, alias); err != nil {
		t.Fatal(err)
	}
	if err := WriteReceiptCollection(context.Background(), path, fixture.archive, collection); err == nil {
		t.Fatal("hard-linked destination was reused")
	}
	if _, err := LoadReceiptCollection(context.Background(), FileReference{Path: path, Sha256: digest(raw)}); err == nil {
		t.Fatal("hard-linked evidence was loaded")
	}
	if err := os.Remove(alias); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(directory, 0710); err != nil {
		t.Fatal(err)
	}
	if err := WriteReceiptCollection(context.Background(), path, fixture.archive, collection); err == nil {
		t.Fatal("nonprivate destination directory was reused")
	}
	if err := os.Chmod(directory, 0700); err != nil {
		t.Fatal(err)
	}
	if err := WriteReceiptCollection(context.Background(), path, fixture.archive, collection); err != nil {
		t.Fatalf("restored private physical custody was not reusable: %v", err)
	}
	collection.Observations.Source = "another-observer"
	collection.Commitments.ObservationHash = objectDigest(collection.Observations)
	collection.ContentHash = collection.hash()
	if err := WriteReceiptCollection(context.Background(), path, fixture.archive, collection); err == nil {
		t.Fatal("different evidence overwrote source artifact")
	}
	retained, _ := os.ReadFile(path)
	if !reflect.DeepEqual(raw, retained) {
		t.Fatal("existing evidence changed on refusal")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	refused := filepath.Join(directory, "refused.json")
	if err := WriteReceiptCollection(ctx, refused, fixture.archive, collection); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled publish: %v", err)
	}
	collection.Commitments.Receipts[0].TransactionNodes = nil
	collection.ContentHash = collection.hash()
	if err := WriteReceiptCollection(context.Background(), refused, fixture.archive, collection); err == nil {
		t.Fatal("unverified proof was published")
	}
	if _, err := os.Stat(refused); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("refused evidence path exists")
	}
}

// Config bounds and identity are rejected before an endpoint may be contacted.
func TestReceiptCollectionRejectsInvalidConfiguration(t *testing.T) {
	fixture := receiptCommitmentTestFixture(t, 1)
	config := ReceiptCollectionConfig{Schema: ReceiptCollectionConfigSchema, CensusHash: fixture.archive.CensusHash, Source: "synthetic", NativeChain: "synthetic-chain", RpcUrl: "https://rpc.example",
		NativeFinalized: fixture.observations.NativeFinalized, EvmFinalized: fixture.observations.EvmFinalized, MappingEvidenceHash: fixture.observations.MappingEvidenceHash}
	for _, mutate := range []func(*ReceiptCollectionConfig){
		func(config *ReceiptCollectionConfig) { config.RetryWindowSeconds = 59 },
		func(config *ReceiptCollectionConfig) { config.RetryWindowSeconds = 901 },
		func(config *ReceiptCollectionConfig) { config.RpcUrl = "https://user:secret@rpc.example" },
		func(config *ReceiptCollectionConfig) { config.RpcUrl = "https://rpc.example?token=synthetic" },
		func(config *ReceiptCollectionConfig) { config.CensusHash = "sha256:" + strings.Repeat("a", 64) },
	} {
		candidate := config
		mutate(&candidate)
		if err := candidate.validate(fixture.archive); err == nil {
			t.Fatalf("invalid selection accepted: %s", fmt.Sprint(candidate.Source))
		}
	}
}
