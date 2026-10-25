// The public native-proof command replays only synthetic private local files.
// Its independent fixture encodes SCALE directly and owns no network adapter.
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
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/rlp"
	"github.com/urnetwork/server/v2026/strecovery"
	"golang.org/x/crypto/blake2b"
)

// Seals are hashes of the exact typed JSON, matching the documented input
// format. This fixture does not call any native proof production decoder.
func finalityCommandDigest(t *testing.T, value any) string {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return fmt.Sprintf("sha256:%x", sha256.Sum256(raw))
}

// Independent native SCALE encoding supplies reusable private CLI evidence.
func finalityCommandInputs(t *testing.T) ([]string, *commandReader, map[string][]byte, *strecovery.ReceiptFinalityProof, []byte) {
	t.Helper()
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
	evmHeader := &types.Header{Number: big.NewInt(91), Difficulty: new(big.Int), GasLimit: 30000000, Time: 1700000000123,
		ParentHash: common.HexToHash("0x" + strings.Repeat("d", 64)), Root: common.HexToHash("0x" + strings.Repeat("e", 64)), UncleHash: types.EmptyUncleHash, TxHash: types.EmptyTxsHash, ReceiptHash: types.EmptyReceiptsHash}
	rawEvm, err := rlp.EncodeToBytes(evmHeader)
	if err != nil {
		t.Fatal(err)
	}
	native := func(number uint16, parent []byte, frontier bool) ([]byte, []byte) {
		raw := append([]byte{}, parent...)
		raw = binary.LittleEndian.AppendUint16(raw, number*4+1)
		raw = append(raw, bytes.Repeat([]byte{0x27}, 64)...)
		if frontier {
			raw = append(raw, 4, 4, 'f', 'r', 'o', 'n', 132, 3)
			raw = append(raw, evmHeader.Hash().Bytes()...)
		} else {
			raw = append(raw, 0)
		}
		hash := blake2b.Sum256(raw)
		return raw, hash[:]
	}
	anchor, anchorHash := native(700, bytes.Repeat([]byte{0x37}, 32), false)
	target, targetHash := native(701, anchorHash, true)
	key := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{0x19}, ed25519.SeedSize))
	checkpoint := &strecovery.NativeFinalityCheckpoint{Schema: strecovery.NativeFinalityCheckpointSchema, CodecProfile: strecovery.NativeFinalityCodecProfile, Genesis: archive.Selection.Genesis,
		HeaderScale: "0x" + hex.EncodeToString(anchor), SetId: 7, Authorities: []strecovery.GrandpaAuthority{{PublicKey: "0x" + hex.EncodeToString(key.Public().(ed25519.PublicKey)), Weight: 1}}, LiveState: "live"}
	nonce := uint64(0)
	observations := &strecovery.ReceiptObservations{Schema: strecovery.ReceiptObservationsSchema, CensusHash: archive.CensusHash, ChainId: archive.Selection.ChainId, Genesis: archive.Selection.Genesis,
		Source: "synthetic-native-reader", NativeFinalized: strecovery.ObservedBlockIdentity{Number: 701, Hash: "0x" + hex.EncodeToString(targetHash)}, EvmFinalized: strecovery.ObservedBlockIdentity{Number: 91, Hash: evmHeader.Hash().Hex()},
		MappingEvidenceHash: "sha256:" + strings.Repeat("b", 64), Blocks: []strecovery.ObservedCanonicalBlock{{Number: 91, Hash: evmHeader.Hash().Hex(), GasLimit: evmHeader.GasLimit}},
		Accounts: []strecovery.ObservedAccount{{Role: archive.Selection.Roles[0].Id, Address: archive.Selection.Roles[0].Address, BlockHash: evmHeader.Hash().Hex(), Outcome: "available", Nonce: &nonce}}, Receipts: []strecovery.ReceiptObservation{}}
	collection := &strecovery.ReceiptCollection{Schema: strecovery.ReceiptCollectionSchema, Admission: "unapproved_observation", Observations: observations,
		Commitments: &strecovery.ReceiptCommitments{Schema: strecovery.ReceiptCommitmentsSchema, CensusHash: archive.CensusHash, ObservationHash: finalityCommandDigest(t, observations), Headers: []string{"0x" + hex.EncodeToString(rawEvm)}, Receipts: []strecovery.ReceiptInclusionProof{}}}
	collection.ContentHash = finalityCommandDigest(t, collection)
	vote := binary.LittleEndian.AppendUint32(append([]byte{}, targetHash...), 701)
	message := append([]byte{1}, vote...)
	message = binary.LittleEndian.AppendUint64(message, 23)
	message = binary.LittleEndian.AppendUint64(message, 7)
	certificate := binary.LittleEndian.AppendUint64(nil, 23)
	certificate = append(certificate, vote...)
	certificate = append(certificate, 4)
	certificate = append(certificate, vote...)
	certificate = append(certificate, ed25519.Sign(key, message)...)
	certificate = append(certificate, key.Public().(ed25519.PublicKey)...)
	certificate = append(certificate, 0)
	proof := &strecovery.ReceiptFinalityProof{Schema: strecovery.ReceiptFinalityProofSchema, CollectionHash: collection.ContentHash, CheckpointHash: checkpoint.Hash(),
		Segments: []strecovery.GrandpaFinalitySegment{{Headers: []string{"0x" + hex.EncodeToString(target)}, JustificationScale: "0x" + hex.EncodeToString(certificate)}}}
	args := []string{"verify-finality", "--archive", archivePath}
	retainedFiles := map[string][]byte{}
	for _, input := range []struct {
		name  string
		value any
	}{{name: "collection", value: collection}, {name: "checkpoint", value: checkpoint}, {name: "proof", value: proof}} {
		raw, err := json.Marshal(input.value)
		if err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(filepath.Dir(archivePath), input.name+".json")
		if err := os.WriteFile(path, raw, 0600); err != nil {
			t.Fatal(err)
		}
		retainedFiles[path] = raw
		args = append(args, "--"+input.name, path, "--"+input.name+"-sha256", fmt.Sprintf("sha256:%x", sha256.Sum256(raw)))
	}
	return args, reader, retainedFiles, proof, certificate
}

// Successful output remains provisional and byte-identical on repeated replay.
// A correctly re-pinned forged certificate emits no output or custody mutation.
func TestRecoveryCommandVerifiesNativeFinalityOfflineWithoutSelfApproval(t *testing.T) {
	args, reader, retainedFiles, proof, certificate := finalityCommandInputs(t)
	var first []byte
	for range 2 {
		var output bytes.Buffer
		if err := run(context.Background(), args, &output, reader); err != nil {
			t.Fatal(err)
		}
		var result strecovery.ReceiptFinalityReconciliation
		if err := json.Unmarshal(output.Bytes(), &result); err != nil {
			t.Fatal(err)
		}
		if !result.NativeEvmCommitmentVerified || !result.GrandpaCertificatesVerified || result.AuthorityCheckpointAuthenticated || result.GenesisAuthenticated || result.FinalityAuthenticated || result.RuntimeSourceAuthenticated || result.ActualFeesReconciled || result.SpendingAuthorized || reader.calls != 2 {
			t.Fatal("offline command reopened custody or invented approval")
		}
		if first == nil {
			first = bytes.Clone(output.Bytes())
		} else if !bytes.Equal(first, output.Bytes()) {
			t.Fatal("offline replay changed its result")
		}
	}
	for path, retained := range retainedFiles {
		raw, err := os.ReadFile(path)
		if err != nil || !bytes.Equal(raw, retained) {
			t.Fatal("verification changed an input file")
		}
	}
	certificate[81] ^= 1
	proof.Segments[0].JustificationScale = "0x" + hex.EncodeToString(certificate)
	raw, _ := json.Marshal(proof)
	if err := os.WriteFile(args[len(args)-3], raw, 0600); err != nil {
		t.Fatal(err)
	}
	args[len(args)-1] = fmt.Sprintf("sha256:%x", sha256.Sum256(raw))
	var output bytes.Buffer
	if err := run(context.Background(), args, &output, reader); err == nil || output.Len() != 0 || reader.calls != 2 {
		t.Fatal("forged proof produced output or touched source custody")
	}
}

// Incomplete pins, unsupported flags and unbounded deadlines are rejected
// before any archive read, database access or output can occur.
func TestRecoveryCommandRequiresEveryNativeFinalityInputWithoutApprovalFlags(t *testing.T) {
	reader := &commandReader{fail: true}
	for _, args := range [][]string{{"verify-finality"}, {"verify-finality", "--archive", "/private.example/archive.json"}, {"verify-finality", "--finality-authenticated"}, {"verify-finality", "--approve-checkpoint"}, {"verify-finality", "--timeout", "16m"}, {"verify-finality", "--rpc-url", "https://node.example"}} {
		var output bytes.Buffer
		if err := run(context.Background(), args, &output, reader); err == nil || output.Len() != 0 || reader.calls != 0 {
			t.Fatalf("incomplete or self-approving input admitted: %v", args)
		}
	}
}
