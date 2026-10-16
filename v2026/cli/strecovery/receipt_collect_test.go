// Public collection tests use only a synthetic HTTP node and private empty
// custody; offline replay must work after that node is completely closed.
package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/rlp"
	"github.com/urnetwork/server/v2026/strecovery"
)

// Successful collection is idempotent; a later source conflict emits nothing
// and creates no new file. Original custody never reopens during these steps.
func TestRecoveryCommandCollectsAndReplaysPrivateReceiptEvidence(t *testing.T) {
	configPath, archivePath, _ := commandConfig(t)
	reader := &commandReader{}
	if err := run(context.Background(), []string{"collect", "--config", configPath, "--archive", archivePath}, new(bytes.Buffer), reader); err != nil {
		t.Fatal(err)
	}
	archive, err := strecovery.LoadArchive(context.Background(), archivePath)
	if err != nil {
		t.Fatal(err)
	}
	reader.fail = true
	header := &types.Header{Number: big.NewInt(91), Difficulty: new(big.Int), GasLimit: 30000000, Time: 1700000000123,
		ParentHash: common.HexToHash("0x" + strings.Repeat("d", 64)), Root: common.HexToHash("0x" + strings.Repeat("e", 64)),
		UncleHash: types.EmptyUncleHash, TxHash: types.EmptyTxsHash, ReceiptHash: types.EmptyReceiptsHash}
	rawHeader, err := rlp.EncodeToBytes(header)
	if err != nil {
		t.Fatal(err)
	}
	config := strecovery.ReceiptCollectionConfig{Schema: strecovery.ReceiptCollectionConfigSchema, CensusHash: archive.CensusHash, Source: "synthetic-command-node", NativeChain: "synthetic-chain",
		NativeFinalized: strecovery.ObservedBlockIdentity{Number: 711, Hash: "0x" + strings.Repeat("f", 64)}, EvmFinalized: strecovery.ObservedBlockIdentity{Number: 91, Hash: header.Hash().Hex()},
		MappingEvidenceHash: "sha256:" + strings.Repeat("a", 64), RetryWindowSeconds: 60}
	var failBoundary atomic.Bool
	var blockReads atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Id     int               `json:"id"`
			Method string            `json:"method"`
			Params []json.RawMessage `json:"params"`
		}
		if json.NewDecoder(r.Body).Decode(&request) != nil {
			t.Error("bad request")
			w.WriteHeader(400)
			return
		}
		var result any
		switch request.Method {
		case "eth_chainId":
			result = "0x7a69"
		case "system_chain":
			result = "synthetic-chain"
		case "chain_getBlockHash":
			var number uint64
			json.Unmarshal(request.Params[0], &number)
			if number == 0 {
				result = archive.Selection.Genesis
			} else {
				result = "0x" + strings.Repeat("f", 64)
			}
		case "eth_getBlockByNumber":
			hash := header.Hash().Hex()
			if count := blockReads.Add(1); failBoundary.Load() && count%3 == 0 {
				hash = "0x" + strings.Repeat("c", 64)
			}
			result = map[string]any{"hash": hash, "number": "0x5b", "gasUsed": "0x0", "gasLimit": "0x1c9c380"}
		case "eth_getTransactionCount":
			result = "0x0"
		case "debug_getRawHeader":
			result = "0x" + hex.EncodeToString(rawHeader)
		default:
			t.Errorf("unexpected read/mutation %s", request.Method)
			w.WriteHeader(400)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": request.Id, "result": result})
	}))
	defer server.Close()
	config.RpcUrl = server.URL
	rawConfig, err := json.Marshal(config)
	if err != nil {
		t.Fatal(err)
	}
	collectionConfigPath := filepath.Join(filepath.Dir(archivePath), "collection-config.json")
	if err := os.WriteFile(collectionConfigPath, rawConfig, 0600); err != nil {
		t.Fatal(err)
	}
	collectionPath := filepath.Join(filepath.Dir(archivePath), "collection.json")
	args := []string{"collect-receipts", "--archive", archivePath, "--config", collectionConfigPath, "--config-sha256", fmt.Sprintf("sha256:%x", sha256.Sum256(rawConfig)), "--collection", collectionPath}
	var output bytes.Buffer
	for i := 0; i < 2; i++ {
		output.Reset()
		if err := run(context.Background(), args, &output, reader); err != nil {
			t.Fatal(err)
		}
	}
	var report strecovery.ReceiptCommitmentReconciliation
	if err := json.Unmarshal(output.Bytes(), &report); err != nil || !report.EvmHeaderAncestryVerified || !report.FoundReceiptCommitmentsVerified || report.FinalityAuthenticated || report.ActualFeesReconciled || report.SpendingAuthorized || reader.calls != 2 {
		t.Fatalf("command invented authority or reopened custody: %v", err)
	}
	retained, err := os.ReadFile(collectionPath)
	if err != nil {
		t.Fatal(err)
	}
	failBoundary.Store(true)
	refusedPath := filepath.Join(filepath.Dir(archivePath), "refused.json")
	args[len(args)-1] = refusedPath
	output.Reset()
	if err := run(context.Background(), args, &output, reader); err == nil || output.Len() != 0 {
		t.Fatalf("conflicting source produced output: %v", err)
	}
	if _, err := os.Stat(refusedPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("failed collection published evidence")
	}
	server.Close()
	args = []string{"verify-collection", "--archive", archivePath, "--collection", collectionPath, "--collection-sha256", fmt.Sprintf("sha256:%x", sha256.Sum256(retained))}
	output.Reset()
	if err := run(context.Background(), args, &output, reader); err != nil || reader.calls != 2 {
		t.Fatalf("offline replay failed or reopened custody: %v", err)
	}
	unchanged, _ := os.ReadFile(collectionPath)
	if !bytes.Equal(retained, unchanged) {
		t.Fatal("refusal/replay changed original evidence")
	}
}

// No missing pin, unsupported deadline or invented finality switch reaches
// archive custody, a network connection, publication or command output.
func TestRecoveryCommandRequiresCompleteReceiptCollectionInputs(t *testing.T) {
	reader := &commandReader{fail: true}
	for _, args := range [][]string{
		{"collect-receipts", "--archive", "/private.example/archive.json", "--config", "/private.example/config.json", "--collection", "/private.example/output.json"},
		{"verify-collection", "--archive", "/private.example/archive.json", "--collection", "/private.example/output.json"},
		{"collect-receipts", "--finality-authenticated"},
		{"collect-receipts", "--archive", "/private.example/archive.json", "--config", "/private.example/config.json", "--config-sha256", "sha256:" + strings.Repeat("a", 64), "--collection", "/private.example/output.json", "--timeout", "30s"},
	} {
		var output bytes.Buffer
		if err := run(context.Background(), args, &output, reader); err == nil || output.Len() != 0 || reader.calls != 0 {
			t.Fatalf("incomplete arguments touched sources: %v", args)
		}
	}
}
