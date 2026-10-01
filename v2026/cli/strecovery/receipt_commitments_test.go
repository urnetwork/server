// The public command consumes genuinely signed private custody and indexed
// receipt proofs. Its read-only ports cannot sign, submit or restore custody.
package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethdb/memorydb"
	"github.com/ethereum/go-ethereum/rlp"
	"github.com/ethereum/go-ethereum/trie"
	"github.com/urnetwork/server/v2026/strecovery"
)

// A store-only signed transaction and one reverted consensus receipt exercise
// the complete collect/archive/verify command path without a database fixture.
func TestRecoveryCommandVerifiesPinnedReceiptCommitmentsWithoutAuthority(t *testing.T) {
	configPath, archivePath, storePath := commandConfig(t)
	rawConfig, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	var config strecovery.Config
	if err := json.Unmarshal(rawConfig, &config); err != nil {
		t.Fatal(err)
	}
	keyBytes := make([]byte, 32)
	keyBytes[31] = 9
	key, err := crypto.ToECDSA(keyBytes)
	if err != nil {
		t.Fatal(err)
	}
	config.Roles[0].Address = strings.ToLower(crypto.PubkeyToAddress(key.PublicKey).Hex())
	config.Roles[0].FirstNonce, config.Roles[0].NextNonce = 7, 8
	writePrivate := func(path string, value any) string {
		raw, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, raw, 0600); err != nil {
			t.Fatal(err)
		}
		return fmt.Sprintf("sha256:%x", sha256.Sum256(raw))
	}
	writePrivate(configPath, config)
	to := common.HexToAddress("0x" + strings.Repeat("c", 40))
	transaction, err := types.SignTx(types.NewTx(&types.LegacyTx{Nonce: 7, Gas: 30000, GasPrice: big.NewInt(100), To: &to, Value: new(big.Int)}), types.LatestSignerForChainID(big.NewInt(31337)), key)
	if err != nil {
		t.Fatal(err)
	}
	rawTransaction, err := transaction.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(storePath, strings.TrimPrefix(transaction.Hash().Hex(), "0x")+".rlp"), rawTransaction, 0600); err != nil {
		t.Fatal(err)
	}
	reader := &commandReader{}
	if err := run(context.Background(), []string{"collect", "--config", configPath, "--archive", archivePath}, new(bytes.Buffer), reader); err != nil {
		t.Fatal(err)
	}
	archive, err := strecovery.LoadArchive(context.Background(), archivePath)
	if err != nil {
		t.Fatal(err)
	}
	reader.fail = true
	if len(archive.Transactions) != 1 {
		t.Fatal("synthetic stored signature was not collected")
	}
	proof := func(raw []byte) (common.Hash, []string) {
		tree := trie.NewEmpty(nil)
		indexKey, err := rlp.EncodeToBytes(uint64(0))
		if err != nil {
			t.Fatal(err)
		}
		if err := tree.Update(indexKey, raw); err != nil {
			t.Fatal(err)
		}
		db := memorydb.New()
		defer db.Close()
		if err := tree.Prove(indexKey, db); err != nil {
			t.Fatal(err)
		}
		iterator := db.NewIterator(nil, nil)
		defer iterator.Release()
		var nodes []string
		for iterator.Next() {
			nodes = append(nodes, "0x"+hex.EncodeToString(iterator.Value()))
		}
		if err := iterator.Error(); err != nil {
			t.Fatal(err)
		}
		return tree.Hash(), nodes
	}
	txRoot, txNodes := proof(rawTransaction)
	receipt := &types.Receipt{Type: types.LegacyTxType, Status: types.ReceiptStatusFailed, CumulativeGasUsed: 21000, Logs: []*types.Log{}}
	rawReceipt, err := receipt.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	receiptRoot, receiptNodes := proof(rawReceipt)
	header := &types.Header{Number: big.NewInt(91), Difficulty: new(big.Int), GasLimit: 30000000, GasUsed: 21000, Time: 1700000000123,
		ParentHash: common.HexToHash("0x" + strings.Repeat("d", 64)), Root: common.HexToHash("0x" + strings.Repeat("e", 64)), UncleHash: types.EmptyUncleHash, TxHash: txRoot, ReceiptHash: receiptRoot}
	rawHeader, err := rlp.EncodeToBytes(header)
	if err != nil {
		t.Fatal(err)
	}
	nonce, index, kind, status := uint64(8), uint64(0), uint8(types.LegacyTxType), uint64(types.ReceiptStatusFailed)
	observations := &strecovery.ReceiptObservations{Schema: strecovery.ReceiptObservationsSchema, CensusHash: archive.CensusHash, ChainId: config.ChainId, Genesis: config.Genesis,
		Source: "synthetic-observer", NativeFinalized: strecovery.ObservedBlockIdentity{Number: 711, Hash: "0x" + strings.Repeat("f", 64)},
		EvmFinalized: strecovery.ObservedBlockIdentity{Number: 91, Hash: header.Hash().Hex()}, MappingEvidenceHash: "sha256:" + strings.Repeat("a", 64),
		Blocks:   []strecovery.ObservedCanonicalBlock{{Number: 91, Hash: header.Hash().Hex(), GasLimit: header.GasLimit, GasUsed: header.GasUsed}},
		Accounts: []strecovery.ObservedAccount{{Role: config.Roles[0].Id, Address: config.Roles[0].Address, BlockHash: header.Hash().Hex(), Outcome: "available", Nonce: &nonce}},
		Receipts: []strecovery.ReceiptObservation{{Hash: transaction.Hash().Hex(), Outcome: "found", Receipt: &strecovery.ObservedReceipt{TransactionHash: transaction.Hash().Hex(), Type: &kind, Status: &status,
			BlockNumber: 91, BlockHash: header.Hash().Hex(), TransactionIndex: &index, GasUsed: 21000, CumulativeGasUsed: 21000, EffectiveGasPrice: "100"}}}}
	observationsPath, commitmentsPath := filepath.Join(filepath.Dir(archivePath), "observations.json"), filepath.Join(filepath.Dir(archivePath), "commitments.json")
	observationPin := writePrivate(observationsPath, observations)
	commitments := &strecovery.ReceiptCommitments{Schema: strecovery.ReceiptCommitmentsSchema, CensusHash: archive.CensusHash, ObservationHash: observationPin,
		Headers: []string{"0x" + hex.EncodeToString(rawHeader)}, Receipts: []strecovery.ReceiptInclusionProof{{Hash: transaction.Hash().Hex(), TransactionNodes: txNodes, ReceiptNodes: receiptNodes}}}
	commitmentPin := writePrivate(commitmentsPath, commitments)
	args := []string{"verify-receipts", "--archive", archivePath, "--observations", observationsPath, "--observations-sha256", observationPin, "--commitments", commitmentsPath, "--commitments-sha256", commitmentPin}
	var output bytes.Buffer
	if err := run(context.Background(), args, &output, reader); err != nil {
		t.Fatal(err)
	}
	var result strecovery.ReceiptCommitmentReconciliation
	if err := json.Unmarshal(output.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.Schema != strecovery.ReceiptCommitmentReconciliationSchema || len(result.Receipts) != 1 || result.Receipts[0].GasUsed != 21000 || result.Receipts[0].Status != types.ReceiptStatusFailed ||
		!result.FoundReceiptCommitmentsVerified || !result.EvmHeaderAncestryVerified || result.FinalityAuthenticated || result.ActualFeesReconciled || result.SpendingAuthorized ||
		result.Receipts[0].ActualGasFee != nil || result.Observations.Fees[0].RevertedTransactions != 1 || reader.calls != 2 {
		t.Fatalf("command lost committed evidence or invented authority: %+v", result)
	}
	// Repinning a plausible but forged gas claim exercises the public refusal
	// after all custody and parser checks. No partial JSON may escape.
	observations.Receipts[0].Receipt.GasUsed = 20000
	observationPin = writePrivate(observationsPath, observations)
	commitments.ObservationHash = observationPin
	commitmentPin = writePrivate(commitmentsPath, commitments)
	args[6], args[10] = observationPin, commitmentPin
	output.Reset()
	if err := run(context.Background(), args, &output, reader); err == nil || !strings.Contains(err.Error(), "gas or outcome differs") || output.Len() != 0 || reader.calls != 2 {
		t.Fatalf("command published forged gas or reached source custody: output%q error%v", output.String(), err)
	}
}

// Missing proof inputs and invented finality switches fail in argument parsing
// before any archive/source is touched; verification never silently downgrades.
func TestRecoveryCommandRequiresCompleteReceiptCommitmentInputs(t *testing.T) {
	reader := &commandReader{}
	for _, args := range [][]string{
		{"verify-receipts", "--archive", "/private.example/archive.json", "--observations", "/private.example/observations.json", "--observations-sha256", "sha256:" + strings.Repeat("a", 64)},
		{"verify-receipts", "--archive", "/private.example/archive.json", "--finality-authenticated"},
		{"reconcile", "--archive", "/private.example/archive.json", "--commitments", "/private.example/proofs.json"},
	} {
		var output bytes.Buffer
		if err := run(context.Background(), args, &output, reader); err == nil || output.Len() != 0 || reader.calls != 0 {
			t.Fatalf("incomplete verification reached custody or produced output: %v", args)
		}
	}
}
