// Exact SDK recorder vectors and independently signed synthetic headers exercise
// raw native facts without a runtime decoder, live chain, signer or RPC port.
package strecovery

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"golang.org/x/crypto/blake2b"
)

// Every case was emitted and independently replayed by the frozen Rust SDK.
type nativeStorageOracleCase struct {
	Name      string              `json:"name"`
	Layout    string              `json:"layout"`
	Root      string              `json:"root"`
	Entries   []NativeStorageRead `json:"entries"`
	Reads     []NativeStorageRead `json:"reads"`
	Nodes     []string            `json:"storage_proof_nodes"`
	Generated []string            `json:"generated_trie_proof_nodes"`
}

// Pin the complete independently generated fixture, not a hand-recomputed root.
func nativeStorageOracle(t testing.TB) []nativeStorageOracleCase {
	t.Helper()
	raw, err := os.ReadFile("testdata/native-storage-sdk-oracle.json")
	if err != nil {
		t.Fatal(err)
	}
	oracle, err := decodeNativeStorageOracle(raw)
	if err != nil {
		t.Fatal(err)
	}
	return oracle
}

// The independent file receipt uses bare SHA-256 hex; recovery protocol object
// digests use a sha256: prefix. Keep these domains explicit before JSON decoding.
func decodeNativeStorageOracle(raw []byte) ([]nativeStorageOracleCase, error) {
	hash := sha256.Sum256(raw)
	if hex.EncodeToString(hash[:]) != "b875b9eb1aa497233f68bb4cae02b4331320fcaeeaaefbefe867658f2aebaecd" {
		return nil, errors.New("native SDK oracle bytes changed")
	}
	var oracle struct {
		Schema string                    `json:"schema"`
		Sdk    string                    `json:"sdk_source"`
		Cases  []nativeStorageOracleCase `json:"cases"`
	}
	if err := decodeJson(raw, &oracle); err != nil {
		return nil, err
	}
	if oracle.Schema != "urnetwork-native-storage-sdk-oracle-v1" || oracle.Sdk != NativeFinalitySdkSource || len(oracle.Cases) != 18 {
		return nil, errors.New("native SDK oracle census changed")
	}
	return oracle.Cases, nil
}

// Selecting a case fails explicitly, so fixture drift cannot silently skip it.
func nativeStorageCase(t testing.TB, name string) nativeStorageOracleCase {
	t.Helper()
	for _, vector := range nativeStorageOracle(t) {
		if vector.Name == name {
			return vector
		}
	}
	t.Fatalf("missing native SDK oracle case %s", name)
	return nativeStorageOracleCase{}
}

// Independent fixture encoding changes the actual state-root bytes and header
// hash before signing. The production header or trie decoder is never an oracle.
func nativeStorageHeader(number uint32, parent, root string, digests ...[]byte) *finalityTestHeader {
	header := finalityTestNativeHeader(number, parent, digests...)
	rootBytes, _ := hex.DecodeString(root[2:])
	copy(header.raw[32+len(finalityTestCompact(number)):], rootBytes)
	hash := blake2b.Sum256(header.raw)
	header.identity.Hash = "0x" + hex.EncodeToString(hash[:])
	return header
}

// A real archived EVM transaction/receipt history is joined to a native root.
func nativeStorageFinalityFixture(t testing.TB, vector nativeStorageOracleCase) (*receiptFinalityFixture, *ReceiptNativeStateWitness) {
	t.Helper()
	fixture := receiptFinalityTestFixture(t)
	old := fixture.headers[3]
	fixture.headers[3] = nativeStorageHeader(703, fixture.headers[2].identity.Hash, vector.Root, old.digests...)
	fixture.proof.Segments = []GrandpaFinalitySegment{fixture.segment(fixture.headers[1:], fixture.keys, 9)}
	fixture.bind()
	witness := &ReceiptNativeStateWitness{
		Schema: ReceiptNativeStateWitnessSchema, CodecProfile: NativeStorageCodecProfile,
		CollectionHash: fixture.collection.ContentHash, CheckpointHash: fixture.checkpoint.Hash(),
		FinalityHash: objectDigest(fixture.proof), NativeBlock: fixture.headers[3].identity,
		Reads: vector.Reads, Nodes: vector.Nodes,
	}
	return fixture, witness
}

// Reproduce the original checksum-domain mistake without hiding it behind a
// fatal fixture helper. Even a semantically harmless byte change breaks the pin.
func TestNativeStorageProofOracleChecksumDomains(t *testing.T) {
	raw, err := os.ReadFile("testdata/native-storage-sdk-oracle.json")
	if err != nil {
		t.Fatal(err)
	}
	const filePin = "b875b9eb1aa497233f68bb4cae02b4331320fcaeeaaefbefe867658f2aebaecd"
	if digest(raw) != "sha256:"+filePin || digest(raw) == filePin {
		t.Fatal("native oracle regression lost its distinct protocol digest domain")
	}
	oracle, err := decodeNativeStorageOracle(raw)
	if err != nil || len(oracle) != 18 {
		t.Fatalf("native oracle file pin was confused with protocol digest: %v", err)
	}
	changed := append(slices.Clone(raw), ' ')
	oracle, err = decodeNativeStorageOracle(changed)
	if err == nil || oracle != nil || !strings.Contains(err.Error(), "bytes changed") {
		t.Fatalf("native oracle accepted changed file bytes: %v", err)
	}
}

// Both layouts share a reader; all nine SDK scenarios must agree byte for byte.
func TestNativeStorageProofMatchesBothPinnedSdkLayouts(t *testing.T) {
	for _, vector := range nativeStorageOracle(t) {
		result, err := verifyNativeStorageReads(context.Background(), vector.Root, vector.Nodes, vector.Reads)
		if err != nil || objectDigest(result) != objectDigest(vector.Reads) {
			t.Fatalf("SDK raw proof mismatch for %s: %v", vector.Name, err)
		}
	}
}

// A missing value and an empty value occupy different authenticated states.
func TestNativeStorageProofDistinguishesEmptyFromAbsent(t *testing.T) {
	vector := nativeStorageCase(t, "layout1-present-empty")
	for _, fault := range []string{"empty-as-absent", "absent-as-empty"} {
		reads := slices.Clone(vector.Reads)
		if fault == "empty-as-absent" {
			reads[0].Value = nil
		} else {
			empty := "0x"
			reads[1].Value = &empty
		}
		result, err := verifyNativeStorageReads(context.Background(), vector.Root, vector.Nodes, reads)
		if err == nil || result != nil || !strings.Contains(err.Error(), "differs from proven") {
			t.Fatalf("absence and empty value were conflated for %s: %v", fault, err)
		}
	}
	vector = nativeStorageCase(t, "layout0-empty-trie")
	if _, err := verifyNativeStorageReads(context.Background(), vector.Root, nil, vector.Reads); err != nil {
		t.Fatalf("SDK intrinsic empty root requires no supplied node: %v", err)
	}
}

// Altering a root, exact key or expected value cannot retain proof authority.
func TestNativeStorageProofRejectsRootKeyAndValueSubstitution(t *testing.T) {
	for _, fault := range []string{"root", "key", "value", "node-byte"} {
		vector := nativeStorageCase(t, "layout1-present-empty")
		switch fault {
		case "root":
			vector.Root = "0x" + strings.Repeat("55", 32)
		case "key":
			vector.Reads[0].Key = "0x14"
		case "value":
			value := "0x01"
			vector.Reads[0].Value = &value
		case "node-byte":
			vector.Nodes[0] = "0x42130401"
		}
		result, err := verifyNativeStorageReads(context.Background(), vector.Root, vector.Nodes, vector.Reads)
		if err == nil || result != nil {
			t.Fatalf("substituted native storage %s was accepted", fault)
		}
	}
}

// A partial proof cannot turn an unprovided branch into authenticated absence.
func TestNativeStorageProofRejectsMissingNodesAndExternalValues(t *testing.T) {
	for _, layout := range []string{"layout0", "layout1"} {
		vector := nativeStorageCase(t, layout+"-partial-proof")
		reads := []NativeStorageRead{{Key: "0x20202020"}}
		result, err := verifyNativeStorageReads(context.Background(), vector.Root, vector.Nodes, reads)
		if err == nil || result != nil || !strings.Contains(err.Error(), "missing node") {
			t.Fatalf("unprovided branch became absence for %s: %v", layout, err)
		}
	}
	vector := nativeStorageCase(t, "layout1-hashed-branch-value")
	value := *vector.Reads[0].Value
	nodes := slices.DeleteFunc(slices.Clone(vector.Nodes), func(encoded string) bool { return encoded == value })
	if len(nodes)+1 != len(vector.Nodes) {
		t.Fatal("external value fixture did not remove exactly one raw blob")
	}
	result, err := verifyNativeStorageReads(context.Background(), vector.Root, nodes, vector.Reads[:1])
	if err == nil || result != nil || !strings.Contains(err.Error(), "missing value") {
		t.Fatalf("missing external value was accepted: %v", err)
	}
}

// SDK generated proofs omit values and restructure nodes; they are not raw DBs.
func TestNativeStorageProofRejectsGeneratedEncoding(t *testing.T) {
	distinct := 0
	for _, vector := range nativeStorageOracle(t) {
		if slices.Equal(vector.Nodes, vector.Generated) {
			continue
		}
		distinct++
		result, err := verifyNativeStorageReads(context.Background(), vector.Root, vector.Generated, vector.Reads)
		if err == nil || result != nil {
			t.Fatalf("generated proof encoding was accepted as raw storage for %s", vector.Name)
		}
	}
	if distinct != 14 {
		t.Fatalf("raw/generated distinction lost: %d cases", distinct)
	}
}

// Raw proof ordering is irrelevant. Duplicate nodes/keys are ambiguous input;
// unrelated bounded blobs are allowed because proof unions include other reads.
func TestNativeStorageProofRejectsDuplicatesAndAcceptsUnorderedUnion(t *testing.T) {
	vector := nativeStorageCase(t, "layout1-hashed-branch-value")
	nodes := slices.Clone(vector.Nodes)
	slices.Reverse(nodes)
	nodes = append(nodes, "0x0102030405")
	if _, err := verifyNativeStorageReads(context.Background(), vector.Root, nodes, vector.Reads); err != nil {
		t.Fatalf("unordered proof union was rejected: %v", err)
	}
	for _, fault := range []string{"node", "key"} {
		nodes, reads := slices.Clone(vector.Nodes), slices.Clone(vector.Reads)
		if fault == "node" {
			nodes = append(nodes, nodes[0])
		} else {
			reads = append(reads, reads[0])
		}
		result, err := verifyNativeStorageReads(context.Background(), vector.Root, nodes, reads)
		if err == nil || result != nil || !strings.Contains(err.Error(), "duplicate") {
			t.Fatalf("duplicate native storage %s was accepted: %v", fault, err)
		}
	}
}

// Authenticated malformed bytes still cannot select unsupported node semantics.
func TestNativeStorageProofRejectsMalformedCanonicalNodes(t *testing.T) {
	malformed := []struct {
		name string
		raw  []byte
	}{
		{name: "empty", raw: nil}, {name: "unsupported", raw: []byte{1}},
		{name: "null-suffix", raw: []byte{0, 0}},
		{name: "odd-padding", raw: []byte{0x41, 0xa1, 0}},
		{name: "empty-bitmap", raw: []byte{0x80, 0, 0}},
		{name: "leaf-suffix", raw: []byte{0x40, 0, 0}},
		{name: "nonminimal-length", raw: []byte{0x40, 1, 0}},
		{name: "truncated-value", raw: []byte{0x40, 4}},
		{name: "truncated-hash", raw: []byte{0x20, 0}},
		{name: "zero-child", raw: []byte{0x80, 1, 0, 0}},
		{name: "large-child", raw: append([]byte{0x80, 1, 0, 132}, bytes.Repeat([]byte{1}, 33)...)},
		{name: "partial-bound", raw: append([]byte{0x7f}, bytes.Repeat([]byte{255}, 9)...)},
	}
	for _, fault := range malformed {
		hash := blake2b.Sum256(fault.raw)
		result, err := verifyNativeStorageReads(context.Background(), "0x"+hex.EncodeToString(hash[:]), []string{"0x" + hex.EncodeToString(fault.raw)}, []NativeStorageRead{{Key: "0x"}})
		if err == nil || result != nil {
			t.Fatalf("malformed native node %s was accepted", fault.name)
		}
	}
}

// Public counts and byte budgets apply even when a caller bypasses file loading.
func TestNativeStorageProofBoundsCountsBytesAndHex(t *testing.T) {
	vector := nativeStorageCase(t, "layout0-empty-trie")
	for _, fault := range []string{"nodes", "reads", "no-reads", "key", "item", "value", "uppercase", "odd", "prefix"} {
		nodes, reads := vector.Nodes, vector.Reads
		switch fault {
		case "nodes":
			nodes = make([]string, maximumNativeStorageNodes+1)
			for index := range nodes {
				nodes[index] = "0x" + hex.EncodeToString(binary.LittleEndian.AppendUint32(nil, uint32(index)))
			}
		case "reads":
			reads = make([]NativeStorageRead, maximumNativeStorageReads+1)
			for index := range reads {
				reads[index].Key = "0x" + hex.EncodeToString([]byte{byte(index)})
			}
		case "no-reads":
			reads = nil
		case "key":
			reads = []NativeStorageRead{{Key: "0x" + strings.Repeat("00", maximumNativeStorageKeyBytes+1)}}
		case "item":
			nodes = []string{"0x" + strings.Repeat("00", maximumNativeStorageItemBytes+1)}
		case "value":
			value := "0x" + strings.Repeat("00", maximumNativeStorageItemBytes+1)
			reads = []NativeStorageRead{{Key: "0x", Value: &value}}
		case "uppercase":
			nodes = []string{"0xAA"}
		case "odd":
			nodes = []string{"0x0"}
		case "prefix":
			nodes = []string{"00"}
		}
		result, err := verifyNativeStorageReads(context.Background(), vector.Root, nodes, reads)
		if err == nil || result != nil {
			t.Fatalf("native storage %s bound was bypassed", fault)
		}
	}
	budget := &nativeStorageBudget{remaining: 2}
	if _, err := budget.decode("0x0011", 2); err != nil {
		t.Fatal(err)
	}
	if _, err := budget.decode("0x22", 2); err == nil {
		t.Fatal("native storage cumulative byte budget was replenished")
	}
}

// A deterministic context transition cancels during work without scheduler or
// timeout dependence; its embedded real context closes Done at the same point.
type nativeStorageCancelContext struct {
	context.Context
	cancel context.CancelFunc
	calls  int
	at     int
}

// Counting only context checks leaves cryptographic input entirely unchanged.
func (self *nativeStorageCancelContext) Err() error {
	self.calls++
	if self.calls == self.at {
		self.cancel()
	}
	return self.Context.Err()
}

// Cancellation before and during decoding returns no partial result.
func TestNativeStorageProofCancellationReturnsNoPartialReads(t *testing.T) {
	vector := nativeStorageCase(t, "layout1-hashed-branch-value")
	for _, at := range []int{1, 3, len(vector.Nodes) + 4} {
		base, cancel := context.WithCancel(context.Background())
		ctx := &nativeStorageCancelContext{Context: base, cancel: cancel, at: at}
		result, err := verifyNativeStorageReads(ctx, vector.Root, vector.Nodes, vector.Reads)
		cancel()
		if !errors.Is(err, context.Canceled) || result != nil || ctx.calls < at {
			t.Fatalf("native storage cancellation %d leaked partial reads: %v", at, err)
		}
	}
}

// The public API covers all raw oracle shapes through real receipt/finality
// replay, preserves history/null fees and copies results without adding power.
func TestReceiptNativeStateReplaysProofsAndPreservesAuthorityBoundary(t *testing.T) {
	for _, vector := range nativeStorageOracle(t) {
		fixture, witness := nativeStorageFinalityFixture(t, vector)
		before := objectDigest([]any{fixture.receipts.archive, fixture.collection, fixture.checkpoint, fixture.proof, witness})
		result, err := VerifyReceiptNativeState(context.Background(), fixture.receipts.archive, fixture.collection, fixture.checkpoint, fixture.proof, witness)
		if err != nil {
			t.Fatalf("public native proof %s: %v", vector.Name, err)
		}
		if !result.NativeHeaderStorageVerified || result.NativeBlock != witness.NativeBlock || result.NativeStateRoot != vector.Root || objectDigest(result.Reads) != objectDigest(vector.Reads) ||
			result.Finality.NativeFinalized.Number != 703 || result.Finality.EvmFinalized.Number != 100 || len(result.Finality.Receipts.Observations.Transactions) != 6 {
			t.Fatalf("public native proof lost exact facts for %s", vector.Name)
		}
		if result.AuthorityCheckpointAuthenticated || result.GenesisAuthenticated || result.RuntimeSourceAuthenticated || result.RuntimeStorageDecoded || result.FinalityAuthenticated ||
			result.OwnerWindowAuthorized || result.GlobalCustodyAuthorized || result.ActualFeesReconciled || result.FeeExposureAuthorized || result.SpendingAuthorized ||
			result.Finality.FinalityAuthenticated || result.Admission != "unapproved_observation" || len(result.MissingAuthorities) != 4 {
			t.Fatal("native storage facts invented live authority")
		}
		for _, receipt := range result.Finality.Receipts.Receipts {
			if receipt.ActualGasFee != nil {
				t.Fatal("raw native storage fabricated an actual fee")
			}
		}
		for _, read := range result.Reads {
			if read.Value != nil {
				*read.Value = "0xff"
			}
		}
		if before != objectDigest([]any{fixture.receipts.archive, fixture.collection, fixture.checkpoint, fixture.proof, witness}) {
			t.Fatal("native storage result mutation changed borrowed evidence")
		}
	}
}

// A new certificate tip proves the original boundary; its different root and
// EVM digest cannot displace the collection whose receipts were reconciled.
func TestReceiptNativeStateKeepsBoundaryRootUnderDescendantCertificate(t *testing.T) {
	vector := nativeStorageCase(t, "layout1-hashed-branch-value")
	fixture, witness := nativeStorageFinalityFixture(t, vector)
	descendant := nativeStorageHeader(704, witness.NativeBlock.Hash, "0x"+strings.Repeat("77", 32), finalityTestFrontier("0x"+strings.Repeat("88", 32)))
	fixture.proof.Schema = ReceiptFinalityDescendantProofSchema
	fixture.proof.Segments = []GrandpaFinalitySegment{fixture.segment(append(slices.Clone(fixture.headers[1:]), descendant), fixture.keys, 9)}
	witness.FinalityHash = objectDigest(fixture.proof)
	result, err := VerifyReceiptNativeState(context.Background(), fixture.receipts.archive, fixture.collection, fixture.checkpoint, fixture.proof, witness)
	if err != nil || result.NativeStateRoot != vector.Root || result.NativeBlock != witness.NativeBlock || result.Finality.NativeCertified == nil || *result.Finality.NativeCertified != descendant.identity {
		t.Fatalf("descendant certificate moved the authenticated storage boundary: %v", err)
	}
	witness.NativeBlock = descendant.identity
	if result, err := VerifyReceiptNativeState(context.Background(), fixture.receipts.archive, fixture.collection, fixture.checkpoint, fixture.proof, witness); err == nil || result != nil {
		t.Fatal("descendant storage identity replaced the collection boundary")
	}
}

// Each supplied association is required even when all mathematical proofs hold.
func TestReceiptNativeStateRejectsWitnessContextSubstitution(t *testing.T) {
	for _, fault := range []string{"collection", "checkpoint", "finality", "native-hash", "native-number", "codec", "schema"} {
		fixture, witness := nativeStorageFinalityFixture(t, nativeStorageCase(t, "layout1-present-empty"))
		switch fault {
		case "collection":
			witness.CollectionHash = "sha256:" + strings.Repeat("1", 64)
		case "checkpoint":
			witness.CheckpointHash = "sha256:" + strings.Repeat("1", 64)
		case "finality":
			witness.FinalityHash = "sha256:" + strings.Repeat("1", 64)
		case "native-hash":
			witness.NativeBlock.Hash = "0x" + strings.Repeat("1", 64)
		case "native-number":
			witness.NativeBlock.Number--
		case "codec":
			witness.CodecProfile = "claimed-runtime-layout1"
		case "schema":
			witness.Schema = "claimed-approved-storage"
		}
		result, err := VerifyReceiptNativeState(context.Background(), fixture.receipts.archive, fixture.collection, fixture.checkpoint, fixture.proof, witness)
		if err == nil || result != nil {
			t.Fatalf("native storage witness context %s was accepted", fault)
		}
	}
}

// Rehashing the witness after corrupting a signature does not replace replay;
// likewise a resealed collection cannot hide a mutated archived receipt proof.
func TestReceiptNativeStateRequiresRealFinalityAndReceiptReplay(t *testing.T) {
	for _, fault := range []string{"certificate", "receipt"} {
		fixture, witness := nativeStorageFinalityFixture(t, nativeStorageCase(t, "layout1-present-empty"))
		if fault == "certificate" {
			raw, _ := hex.DecodeString(fixture.proof.Segments[0].JustificationScale[2:])
			raw[45] ^= 1
			fixture.proof.Segments[0].JustificationScale = "0x" + hex.EncodeToString(raw)
		} else {
			fixture.collection.Observations.Receipts[0].Hash = "0x" + strings.Repeat("22", 32)
			fixture.collection.ContentHash = fixture.collection.hash()
			fixture.proof.CollectionHash = fixture.collection.ContentHash
			witness.CollectionHash = fixture.collection.ContentHash
		}
		witness.FinalityHash = objectDigest(fixture.proof)
		result, err := VerifyReceiptNativeState(context.Background(), fixture.receipts.archive, fixture.collection, fixture.checkpoint, fixture.proof, witness)
		if err == nil || result != nil || !strings.Contains(err.Error(), "finality replay") {
			t.Fatalf("native storage bypassed real %s replay: %v", fault, err)
		}
	}
}

// A different native root with an otherwise valid certificate cannot accept
// the supplied raw nodes; the witness carries no self-authenticating root field.
func TestReceiptNativeStateRejectsProofFromDifferentAuthenticatedRoot(t *testing.T) {
	vector := nativeStorageCase(t, "layout1-present-empty")
	other := nativeStorageCase(t, "layout1-inline-branch")
	fixture, witness := nativeStorageFinalityFixture(t, other)
	witness.Reads, witness.Nodes = vector.Reads, vector.Nodes
	result, err := VerifyReceiptNativeState(context.Background(), fixture.receipts.archive, fixture.collection, fixture.checkpoint, fixture.proof, witness)
	if err == nil || result != nil || !strings.Contains(err.Error(), "missing node") {
		t.Fatalf("native storage proof escaped its authenticated root: %v", err)
	}
}

// The loader shares the private file boundary and strict duplicate/unknown JSON
// checks. A matching file pin is integrity evidence, never authority approval.
func TestReceiptNativeStateLoaderPinsPrivateStrictBytes(t *testing.T) {
	_, witness := nativeStorageFinalityFixture(t, nativeStorageCase(t, "layout1-present-empty"))
	raw, err := json.Marshal(witness)
	if err != nil {
		t.Fatal(err)
	}
	directory := t.TempDir()
	if err := os.Chmod(directory, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(directory, "storage.json")
	for _, fault := range []string{"valid", "pin", "malformed-pin", "duplicate", "unknown", "trailing", "public", "symlink"} {
		encoded := slices.Clone(raw)
		switch fault {
		case "duplicate":
			encoded = append([]byte(`{"schema":"duplicate",`), raw[1:]...)
		case "unknown":
			encoded = append([]byte(`{"approved":true,`), raw[1:]...)
		case "trailing":
			encoded = append(encoded, []byte(" {}")...)
		}
		if err := os.WriteFile(path, encoded, 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(path, 0600); err != nil {
			t.Fatal(err)
		}
		reference := FileReference{Path: path, Sha256: digest(encoded)}
		if fault == "pin" {
			reference.Sha256 = "sha256:" + strings.Repeat("1", 64)
			if !canonicalDigest(reference.Sha256) || reference.Sha256 == digest(encoded) {
				t.Fatal("native storage wrong-pin fixture must reach the byte comparison")
			}
		}
		if fault == "malformed-pin" {
			reference.Sha256 = strings.Repeat("1", 64)
		}
		if fault == "public" {
			if err := os.Chmod(path, 0644); err != nil {
				t.Fatal(err)
			}
		}
		if fault == "symlink" {
			reference.Path = filepath.Join(directory, "link.json")
			if err := os.Symlink(path, reference.Path); err != nil {
				t.Fatal(err)
			}
		}
		result, err := LoadReceiptNativeStateWitness(context.Background(), reference)
		if fault == "valid" {
			if err != nil || objectDigest(result) != objectDigest(witness) {
				t.Fatalf("valid private storage witness rejected: %v", err)
			}
		} else if err == nil || result != nil {
			t.Fatalf("native storage loader accepted %s evidence", fault)
		} else if fault == "pin" && err.Error() != "native storage witness differs from its byte pin" {
			t.Fatalf("native storage pin mismatch did not reach the byte comparison: %v", err)
		} else if fault == "malformed-pin" && err.Error() != "native storage witness requires a context and exact byte pin" {
			t.Fatalf("native storage malformed pin did not reach syntax validation: %v", err)
		}
	}
}
