package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
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
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethdb/memorydb"
	"github.com/ethereum/go-ethereum/rlp"
	"github.com/ethereum/go-ethereum/trie"
	"github.com/urnetwork/server/v2026/strecovery"
	"golang.org/x/crypto/blake2b"
)

func historicalCommandWrite(t testing.TB, path string, value any) string {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	return fmt.Sprintf("sha256:%x", sha256.Sum256(raw))
}

// Actual signed custody, an indexed reverted Ethereum receipt and independent
// native SCALE/certificate bytes exercise the complete public command route.
func historicalCommandInputs(t *testing.T) ([]string, *commandReader, strecovery.ReceiptHistoricalNativeCaptureConfig, string, string) {
	t.Helper()
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
	historicalCommandWrite(t, configPath, config)
	to := common.HexToAddress("0x" + strings.Repeat("c", 40))
	transaction, err := types.SignTx(types.NewTx(&types.LegacyTx{Nonce: 7, Gas: 30000, GasPrice: big.NewInt(100), To: &to, Value: new(big.Int)}), types.LatestSignerForChainID(big.NewInt(31337)), key)
	if err != nil {
		t.Fatal(err)
	}
	rawTransaction, err := transaction.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(storePath, transaction.Hash().Hex()[2:]+".rlp"), rawTransaction, 0600); err != nil {
		t.Fatal(err)
	}
	reader := &commandReader{}
	if err := run(t.Context(), []string{"collect", "--config", configPath, "--archive", archivePath}, new(bytes.Buffer), reader); err != nil {
		t.Fatal(err)
	}
	archive, err := strecovery.LoadArchive(t.Context(), archivePath)
	if err != nil {
		t.Fatal(err)
	}
	reader.fail = true
	evmProof := func(raw []byte) (common.Hash, []string) {
		tree := trie.NewEmpty(nil)
		index, _ := rlp.EncodeToBytes(uint64(0))
		if err := tree.Update(index, raw); err != nil {
			t.Fatal(err)
		}
		db := memorydb.New()
		defer db.Close()
		if err := tree.Prove(index, db); err != nil {
			t.Fatal(err)
		}
		iterator := db.NewIterator(nil, nil)
		defer iterator.Release()
		nodes := []string{}
		for iterator.Next() {
			nodes = append(nodes, "0x"+hex.EncodeToString(iterator.Value()))
		}
		if err := iterator.Error(); err != nil {
			t.Fatal(err)
		}
		return tree.Hash(), nodes
	}
	txRoot, txNodes := evmProof(rawTransaction)
	receipt := &types.Receipt{Type: types.LegacyTxType, Status: types.ReceiptStatusFailed, CumulativeGasUsed: 21000, Logs: []*types.Log{}}
	rawReceipt, err := receipt.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	receiptRoot, receiptNodes := evmProof(rawReceipt)
	header := &types.Header{Number: big.NewInt(91), Difficulty: new(big.Int), GasLimit: 30000000, GasUsed: 21000, Time: 1700000000123,
		ParentHash: common.HexToHash("0x" + strings.Repeat("d", 64)), Root: common.HexToHash("0x" + strings.Repeat("e", 64)), UncleHash: types.EmptyUncleHash, TxHash: txRoot, ReceiptHash: receiptRoot}
	rawHeader, err := rlp.EncodeToBytes(header)
	if err != nil {
		t.Fatal(err)
	}
	// Native LayoutV0/V1 inline leaf for key 0x01 and raw value 0xab. The
	// other selected key 0x02 is proven absent by this same independent node.
	node := []byte{0x42, 0x01, 0x04, 0xab}
	root := blake2b.Sum256(node)
	native := func(number uint16, parent, stateRoot []byte, frontier bool) ([]byte, []byte) {
		raw := append([]byte{}, parent...)
		raw = binary.LittleEndian.AppendUint16(raw, number*4+1)
		raw = append(raw, stateRoot...)
		raw = append(raw, bytes.Repeat([]byte{0x27}, 32)...)
		if frontier {
			raw = append(raw, 4, 4, 'f', 'r', 'o', 'n', 132, 3)
			raw = append(raw, header.Hash().Bytes()...)
		} else {
			raw = append(raw, 0)
		}
		hash := blake2b.Sum256(raw)
		return raw, hash[:]
	}
	anchor, anchorHash := native(700, bytes.Repeat([]byte{0x37}, 32), root[:], false)
	target, targetHash := native(701, anchorHash, bytes.Repeat([]byte{0x27}, 32), true)
	nativeKey := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{0x19}, ed25519.SeedSize))
	checkpoint := &strecovery.NativeFinalityCheckpoint{Schema: strecovery.NativeFinalityCheckpointSchema, CodecProfile: strecovery.NativeFinalityCodecProfile, Genesis: archive.Selection.Genesis,
		HeaderScale: "0x" + hex.EncodeToString(anchor), SetId: 7, Authorities: []strecovery.GrandpaAuthority{{PublicKey: "0x" + hex.EncodeToString(nativeKey.Public().(ed25519.PublicKey)), Weight: 1}}, LiveState: "live"}
	nonce, index, kind, status := uint64(8), uint64(0), uint8(types.LegacyTxType), uint64(types.ReceiptStatusFailed)
	observations := &strecovery.ReceiptObservations{Schema: strecovery.ReceiptObservationsSchema, CensusHash: archive.CensusHash, ChainId: config.ChainId, Genesis: config.Genesis,
		Source: "synthetic-storage-command", NativeFinalized: strecovery.ObservedBlockIdentity{Number: 701, Hash: "0x" + hex.EncodeToString(targetHash)},
		EvmFinalized: strecovery.ObservedBlockIdentity{Number: 91, Hash: header.Hash().Hex()}, MappingEvidenceHash: "sha256:" + strings.Repeat("a", 64),
		Blocks:   []strecovery.ObservedCanonicalBlock{{Number: 91, Hash: header.Hash().Hex(), GasLimit: header.GasLimit, GasUsed: header.GasUsed}},
		Accounts: []strecovery.ObservedAccount{{Role: config.Roles[0].Id, Address: config.Roles[0].Address, BlockHash: header.Hash().Hex(), Outcome: "available", Nonce: &nonce}},
		Receipts: []strecovery.ReceiptObservation{{Hash: transaction.Hash().Hex(), Outcome: "found", Receipt: &strecovery.ObservedReceipt{TransactionHash: transaction.Hash().Hex(), Type: &kind, Status: &status, BlockNumber: 91, BlockHash: header.Hash().Hex(), TransactionIndex: &index, GasUsed: 21000, CumulativeGasUsed: 21000, EffectiveGasPrice: "100"}}}}
	commitments := &strecovery.ReceiptCommitments{Schema: strecovery.ReceiptCommitmentsSchema, CensusHash: archive.CensusHash, ObservationHash: finalityCommandDigest(t, observations), Headers: []string{"0x" + hex.EncodeToString(rawHeader)}, Receipts: []strecovery.ReceiptInclusionProof{{Hash: transaction.Hash().Hex(), TransactionNodes: txNodes, ReceiptNodes: receiptNodes}}}
	collection := &strecovery.ReceiptCollection{Schema: strecovery.ReceiptCollectionSchema, Admission: "unapproved_observation", Observations: observations, Commitments: commitments}
	collection.ContentHash = finalityCommandDigest(t, collection)
	vote := binary.LittleEndian.AppendUint32(append([]byte{}, targetHash...), 701)
	message := append([]byte{1}, vote...)
	message = binary.LittleEndian.AppendUint64(message, 23)
	message = binary.LittleEndian.AppendUint64(message, 7)
	certificate := binary.LittleEndian.AppendUint64(nil, 23)
	certificate = append(certificate, vote...)
	certificate = append(certificate, 4)
	certificate = append(certificate, vote...)
	certificate = append(certificate, ed25519.Sign(nativeKey, message)...)
	certificate = append(certificate, nativeKey.Public().(ed25519.PublicKey)...)
	certificate = append(certificate, 0)
	proof := &strecovery.ReceiptFinalityProof{Schema: strecovery.ReceiptFinalityProofSchema, CollectionHash: collection.ContentHash, CheckpointHash: checkpoint.Hash(), Segments: []strecovery.GrandpaFinalitySegment{{Headers: []string{"0x" + hex.EncodeToString(target)}, JustificationScale: "0x" + hex.EncodeToString(certificate)}}}
	args := []string{"capture-historical-state", "--archive", archivePath}
	for _, item := range []struct {
		name  string
		value any
	}{{"collection", collection}, {"checkpoint", checkpoint}, {"proof", proof}} {
		path := filepath.Join(filepath.Dir(archivePath), item.name+".json")
		pin := historicalCommandWrite(t, path, item.value)
		args = append(args, "--"+item.name, path, "--"+item.name+"-sha256", pin)
	}
	captureConfig := strecovery.ReceiptHistoricalNativeCaptureConfig{Schema: strecovery.ReceiptHistoricalNativeCaptureConfigSchema, CollectionHash: collection.ContentHash, CheckpointHash: checkpoint.Hash(), FinalityHash: finalityCommandDigest(t, proof), Source: "synthetic-storage-command", EvmBlock: observations.EvmFinalized, RootRole: strecovery.NativeStorageRootParentExecution, Keys: []string{"0x01", "0x02"}, RetryWindowSeconds: 60}
	return args, reader, captureConfig, "0x" + hex.EncodeToString(anchorHash), "0x" + hex.EncodeToString(node)
}

func TestRecoveryCommandCapturesHistoricalStorageAndReplaysAfterNodeRemoval(t *testing.T) {
	args, reader, config, stateHash, node := historicalCommandInputs(t)
	archive, err := strecovery.LoadArchive(t.Context(), args[2])
	if err != nil {
		t.Fatal(err)
	}
	var reads atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		var payload struct {
			Id     int               `json:"id"`
			Method string            `json:"method"`
			Params []json.RawMessage `json:"params"`
		}
		if err := json.NewDecoder(request.Body).Decode(&payload); err != nil {
			t.Error(err)
			writer.WriteHeader(400)
			return
		}
		reads.Add(1)
		var result any
		switch payload.Method {
		case "chain_getBlockHash":
			if len(payload.Params) != 1 || string(payload.Params[0]) != "0" {
				t.Error("unexpected numeric selector")
			}
			result = archive.Selection.Genesis
		case "state_getReadProof", "state_getStorage":
			if len(payload.Params) != 2 || string(payload.Params[1]) != fmt.Sprintf("%q", stateHash) {
				t.Error("historical block selector changed")
			}
			if payload.Method == "state_getReadProof" {
				result = map[string]any{"at": stateHash, "proof": []string{node}}
			} else if string(payload.Params[0]) == `"0x01"` {
				result = "0xab"
			}
		default:
			t.Error("capture emitted non-profile method", payload.Method)
			writer.WriteHeader(400)
			return
		}
		_ = json.NewEncoder(writer).Encode(map[string]any{"jsonrpc": "2.0", "id": payload.Id, "result": result})
	}))
	defer server.Close()
	config.RpcUrl = server.URL
	base := filepath.Dir(args[2])
	configPath := filepath.Join(base, "storage-config.json")
	configPin := historicalCommandWrite(t, configPath, config)
	directory := filepath.Join(base, "storage-capture")
	if err := os.Mkdir(directory, 0700); err != nil {
		t.Fatal(err)
	}
	captureArgs := append(append([]string{}, args...), "--config", configPath, "--config-sha256", configPin, "--capture-dir", directory)
	var first bytes.Buffer
	if err := run(t.Context(), captureArgs, &first, reader); err != nil {
		t.Fatal(err)
	}
	var result strecovery.ReceiptHistoricalNativeCaptureResult
	if err := json.Unmarshal(first.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if !result.Reconciliation.NativeHeaderStorageVerified || result.Reconciliation.ActualFeesReconciled || result.Reconciliation.SpendingAuthorized || result.Reconciliation.Reads[1].Value != nil || reader.calls != 2 || reads.Load() != 4 {
		t.Fatal("public capture lost facts, reopened custody or fabricated authority")
	}
	server.Close()
	var resumed bytes.Buffer
	if err := run(t.Context(), captureArgs, &resumed, reader); err != nil || !bytes.Equal(first.Bytes(), resumed.Bytes()) || reads.Load() != 4 {
		t.Fatal("complete restart changed facts or opened node", err)
	}
	args[0] = "verify-historical-state"
	args = append(args, "--witness", result.Witness.Path, "--witness-sha256", result.Witness.Sha256)
	var replay bytes.Buffer
	if err := run(t.Context(), args, &replay, reader); err != nil {
		t.Fatal(err)
	}
	var verified strecovery.ReceiptHistoricalNativeStateReconciliation
	if err := json.Unmarshal(replay.Bytes(), &verified); err != nil || finalityCommandDigest(t, &verified) != finalityCommandDigest(t, result.Reconciliation) || reader.calls != 2 {
		t.Fatal("offline public replay differs", err)
	}
}

func TestRecoveryCommandHistoricalStorageRequiresCompletePinsWithoutApprovalFlags(t *testing.T) {
	for _, command := range []string{"capture-historical-state", "verify-historical-state"} {
		base := []string{command, "--archive", "/private.example/archive.json", "--collection", "/private.example/collection.json", "--collection-sha256", "sha256:" + strings.Repeat("1", 64), "--checkpoint", "/private.example/checkpoint.json", "--checkpoint-sha256", "sha256:" + strings.Repeat("2", 64), "--proof", "/private.example/proof.json", "--proof-sha256", "sha256:" + strings.Repeat("3", 64)}
		if command == "capture-historical-state" {
			base = append(base, "--config", "/private.example/config.json", "--config-sha256", "sha256:"+strings.Repeat("4", 64), "--capture-dir", "/private.example/capture")
		} else {
			base = append(base, "--witness", "/private.example/witness.json", "--witness-sha256", "sha256:"+strings.Repeat("4", 64))
		}
		for index := 1; index < len(base); index += 2 {
			args := append(append([]string{}, base[:index]...), base[index+2:]...)
			var output bytes.Buffer
			reader := &commandReader{fail: true}
			if err := run(context.Background(), args, &output, reader); err == nil || output.Len() != 0 || reader.calls != 0 {
				t.Fatal("incomplete historical input admitted", base[index])
			}
		}
		for _, extra := range [][]string{{"--approve-checkpoint"}, {"--native-state-root", "0x1234"}, {"--send"}, {"--actual-fee", "1"}, {"--timeout", "16m"}, {"--timeout", "0s"}} {
			var output bytes.Buffer
			reader := &commandReader{fail: true}
			if err := run(context.Background(), append(append([]string{}, base...), extra...), &output, reader); err == nil || output.Len() != 0 || reader.calls != 0 {
				t.Fatal("authority or unbounded input admitted", extra)
			}
		}
	}
}
