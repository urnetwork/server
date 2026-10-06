// Distinct pinned SDK roots enter real synthetic headers before linking and
// signing. No caller report, mocked verifier or live chain supplies the answers.
package strecovery

import (
	"context"
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

// The receipt's parent, child, collection boundary and future certificate all
// carry different real trie roots from the independently sealed SDK vectors.
type historicalStorageFixture struct {
	finality *receiptFinalityFixture
	vectors  []nativeStorageOracleCase
}

// Mapping choices and alternative winners reuse the qualified receipt fixture;
// header roots are independently encoded before every parent hash and signature.
func historicalStorageTestFixture(t testing.TB, winner int, mappings []int) *historicalStorageFixture {
	t.Helper()
	self := &historicalStorageFixture{finality: feeContextTestFixture(t, winner, mappings)}
	for _, name := range []string{"layout1-present-empty", "layout1-hashed-branch-value", "layout1-inline-branch", "layout1-long-partial", "layout1-value-threshold"} {
		self.vectors = append(self.vectors, nativeStorageCase(t, name))
	}
	rootNames := map[string]bool{}
	for _, vector := range self.vectors {
		if rootNames[vector.Root] {
			t.Fatal("historical storage fixture collapsed distinct SDK roots")
		}
		rootNames[vector.Root] = true
	}
	self.rebuild()
	return self
}

// Rebuilding links and signatures permits an explicit equal-root edge fixture
// without weakening the distinct-root defaults used by other tests.
func (self *historicalStorageFixture) rebuild() {
	parent := "0x" + strings.Repeat("9", 64)
	for index, header := range self.finality.headers {
		self.finality.headers[index] = nativeStorageHeader(uint32(700+index), parent, self.vectors[index].Root, header.digests...)
		parent = self.finality.headers[index].identity.Hash
	}
	self.finality.checkpoint.HeaderScale = "0x" + hex.EncodeToString(self.finality.headers[0].raw)
	self.finality.proof.Segments = []GrandpaFinalitySegment{self.finality.segment(self.finality.headers[1:], self.finality.keys, 9)}
	self.finality.bind()
}

// Construct identities from independently encoded headers, never a production
// context result. A missing checkpoint parent stays an intentionally bad request.
func (self *historicalStorageFixture) witness(role string, nativeIndex int) *ReceiptHistoricalNativeStateWitness {
	stateIndex := nativeIndex
	if role == NativeStorageRootParentExecution && stateIndex > 0 {
		stateIndex--
	}
	return &ReceiptHistoricalNativeStateWitness{
		Schema: ReceiptHistoricalNativeStateWitnessSchema, CodecProfile: NativeStorageCodecProfile,
		CollectionHash: self.finality.collection.ContentHash, CheckpointHash: self.finality.checkpoint.Hash(), FinalityHash: objectDigest(self.finality.proof),
		EvmBlock:           ObservedBlockIdentity{Number: 90, Hash: self.finality.receipts.headers[0].Hash().Hex()},
		NativeReceiptBlock: self.finality.headers[nativeIndex].identity, RootRole: role,
		NativeStateBlock: self.finality.headers[stateIndex].identity,
		Reads:            slices.Clone(self.vectors[stateIndex].Reads), Nodes: slices.Clone(self.vectors[stateIndex].Nodes),
	}
}

// The public entry point is always exercised with original proof inputs.
func (self *historicalStorageFixture) verify(ctx context.Context, witness *ReceiptHistoricalNativeStateWitness) (*ReceiptHistoricalNativeStateReconciliation, error) {
	return VerifyReceiptHistoricalNativeState(ctx, self.finality.receipts.archive, self.finality.collection, self.finality.checkpoint, self.finality.proof, witness)
}

// A signed descendant proves only finality; it cannot move historical state.
func (self *historicalStorageFixture) certifyDescendant() *finalityTestHeader {
	descendant := nativeStorageHeader(704, self.finality.headers[3].identity.Hash, self.vectors[4].Root,
		finalityTestFrontier(self.finality.receipts.headers[0].Hash().Hex()))
	self.finality.proof.Schema = ReceiptFinalityDescendantProofSchema
	self.finality.proof.Segments = []GrandpaFinalitySegment{self.finality.segment(append(slices.Clone(self.finality.headers[1:]), descendant), self.finality.keys, 9)}
	return descendant
}

// Parent and child reads preserve all replacement/cancellation histories while
// selecting actual historical roots instead of the unrelated final boundary.
func TestReceiptHistoricalNativeStatePreservesBothRolesAndAllAttempts(t *testing.T) {
	for _, winner := range []int{1, 2, 3} {
		for _, role := range []string{NativeStorageRootParentExecution, NativeStorageRootChildPostState} {
			fixture := historicalStorageTestFixture(t, winner, []int{-1, 0, 5, 10})
			witness := fixture.witness(role, 1)
			result, err := fixture.verify(context.Background(), witness)
			stateIndex := 1
			if role == NativeStorageRootParentExecution {
				stateIndex = 0
			}
			if err != nil || result == nil || result.RootRole != role || result.NativeStateBlock != fixture.finality.headers[stateIndex].identity ||
				result.NativeStateRoot != fixture.vectors[stateIndex].Root || result.NativeReceiptBlock != fixture.finality.headers[1].identity ||
				result.EvmBlock != witness.EvmBlock || objectDigest(result.Reads) != objectDigest(fixture.vectors[stateIndex].Reads) ||
				!result.HistoricalContextVerified || !result.NativeHeaderStorageVerified {
				t.Fatalf("historical root role lost authenticated state for %s: %v", role, err)
			}
			contexts, err := VerifyReceiptFeeContexts(context.Background(), fixture.finality.receipts.archive, fixture.finality.collection, fixture.finality.checkpoint, fixture.finality.proof)
			if err != nil || objectDigest(result.FeeContexts) != objectDigest(contexts) || len(result.FeeContexts.Transactions) != 6 ||
				len(result.FeeContexts.Finality.Receipts.Observations.Nonces) != 4 || result.FeeContexts.Finality.NativeFinalized.Number != 703 {
				t.Fatalf("historical storage changed freshly replayed receipt histories: %v", err)
			}
			found, unavailable, reverted := 0, 0, 0
			for _, transaction := range result.FeeContexts.Transactions {
				if transaction.ActualWithdrawalRao != nil || transaction.ActualRefundRao != nil || transaction.ActualGasDebitRao != nil || transaction.NativeExtrinsicIndex != nil {
					t.Fatal("historical raw reads fabricated transaction fee or placement")
				}
				if transaction.Receipt == nil {
					unavailable++
					continue
				}
				found++
				if transaction.Receipt.Status == 0 {
					reverted++
				}
				if transaction.Receipt.ActualGasFee != nil {
					t.Fatal("historical raw reads fabricated actual fee")
				}
			}
			expectedReverted := 0
			if winner == 3 {
				expectedReverted = 1
			}
			if found != 4 || unavailable != 2 || reverted != expectedReverted {
				t.Fatal("historical raw reads dropped replacement or reverted attempts")
			}
		}
	}
}

// Mathematical association and storage facts are the only positive statements;
// the result cannot promote raw bytes into execution, fee or signing authority.
func TestReceiptHistoricalNativeStateKeepsEveryAuthorityFalse(t *testing.T) {
	fixture := historicalStorageTestFixture(t, 3, []int{-1, 0, 5, 10})
	witness := fixture.witness(NativeStorageRootParentExecution, 1)
	result, err := fixture.verify(context.Background(), witness)
	if err != nil {
		t.Fatal(err)
	}
	if result.Schema != ReceiptHistoricalNativeStateReconciliationSchema || result.Admission != "unapproved_historical_storage_observation" ||
		result.CodecProfile != NativeStorageCodecProfile || result.CodecSdkSource != NativeFinalitySdkSource || result.WitnessHash != objectDigest(witness) || len(result.MissingAuthorities) != 5 ||
		result.AuthorityCheckpointAuthenticated || result.GenesisAuthenticated || result.RuntimeSourceAuthenticated || result.RuntimeStorageDecoded ||
		result.PayerBindingAuthenticated || result.NativeExtrinsicLocationsAuthenticated || result.FinalityAuthenticated || result.FeeAttributionAuthenticated ||
		result.OwnerWindowAuthorized || result.GlobalCustodyAuthorized || result.ActualFeesReconciled || result.FeeExposureAuthorized || result.SpendingAuthorized ||
		result.FeeContexts.AuthorityCheckpointAuthenticated || result.FeeContexts.GenesisAuthenticated || result.FeeContexts.RuntimeSourceAuthenticated ||
		result.FeeContexts.PayerBindingAuthenticated || result.FeeContexts.NativeExtrinsicLocationsAuthenticated || result.FeeContexts.NativeStateReadsAuthenticated ||
		result.FeeContexts.FeeAttributionAuthenticated || result.FeeContexts.FinalityAuthenticated || result.FeeContexts.CanonicalReceiptsReconciled ||
		result.FeeContexts.ActualFeesReconciled || result.FeeContexts.SpendingAuthorized {
		t.Fatal("historical raw storage acquired runtime, payer, custody or spending authority")
	}
}

// Canonical but incorrect associations exercise the equality layer, not syntax.
func TestReceiptHistoricalNativeStateBindsAllProofInputs(t *testing.T) {
	for _, fault := range []string{"collection", "checkpoint", "finality"} {
		fixture := historicalStorageTestFixture(t, 1, []int{-1, 0, 5, 10})
		witness := fixture.witness(NativeStorageRootChildPostState, 1)
		wrong := "sha256:" + strings.Repeat("1", 64)
		if !canonicalDigest(wrong) || wrong == witness.CollectionHash || wrong == witness.CheckpointHash || wrong == witness.FinalityHash {
			t.Fatal("historical association fault is not a canonical mismatch")
		}
		switch fault {
		case "collection":
			witness.CollectionHash = wrong
		case "checkpoint":
			witness.CheckpointHash = wrong
		case "finality":
			witness.FinalityHash = wrong
		}
		result, err := fixture.verify(context.Background(), witness)
		if err == nil || result != nil || !strings.Contains(err.Error(), "exact proof inputs") {
			t.Fatalf("historical proof association %s was bypassed: %v", fault, err)
		}
	}
}

// Exact EVM height and hash must select a committed receipt block. An ordinary
// retained ancestor or the final EVM boundary is not an alternate receipt context.
func TestReceiptHistoricalNativeStateBindsExactReceiptBlock(t *testing.T) {
	for _, fault := range []string{"number", "hash", "receipt-free-ancestor", "final-boundary"} {
		fixture := historicalStorageTestFixture(t, 1, []int{-1, 0, 5, 10})
		witness := fixture.witness(NativeStorageRootChildPostState, 1)
		switch fault {
		case "number":
			witness.EvmBlock.Number++
		case "hash":
			witness.EvmBlock.Hash = "0x" + strings.Repeat("1", 64)
		case "receipt-free-ancestor":
			witness.EvmBlock = ObservedBlockIdentity{Number: 95, Hash: fixture.finality.receipts.headers[5].Hash().Hex()}
		case "final-boundary":
			witness.EvmBlock = fixture.finality.receipts.observations.EvmFinalized
		}
		result, err := fixture.verify(context.Background(), witness)
		if err == nil || result != nil || !strings.Contains(err.Error(), "no committed receipt context") {
			t.Fatalf("historical receipt block selection %s was bypassed: %v", fault, err)
		}
	}
}

// A valid proof for the intended state cannot disguise the native child that
// actually maps the receipt, even when another retained child really exists.
func TestReceiptHistoricalNativeStateBindsMappedNativeChild(t *testing.T) {
	for _, fault := range []string{"number", "hash", "other-child"} {
		fixture := historicalStorageTestFixture(t, 1, []int{-1, 0, 5, 10})
		witness := fixture.witness(NativeStorageRootParentExecution, 1)
		switch fault {
		case "number":
			witness.NativeReceiptBlock.Number++
		case "hash":
			witness.NativeReceiptBlock.Hash = "0x" + strings.Repeat("1", 64)
		case "other-child":
			witness.NativeReceiptBlock = fixture.finality.headers[2].identity
		}
		result, err := fixture.verify(context.Background(), witness)
		if err == nil || result != nil || !strings.Contains(err.Error(), "mapped child identity differs") {
			t.Fatalf("historical mapped child selection %s was bypassed: %v", fault, err)
		}
	}
}

// The selected root's block identity is part of the witness even when the raw
// proof remains genuine. Relabelling cannot move facts into a different block.
func TestReceiptHistoricalNativeStateBindsSelectedStateBlock(t *testing.T) {
	for _, role := range []string{NativeStorageRootParentExecution, NativeStorageRootChildPostState} {
		for _, fault := range []string{"number", "hash", "boundary", "role-swap"} {
			fixture := historicalStorageTestFixture(t, 1, []int{-1, 0, 5, 10})
			witness := fixture.witness(role, 1)
			switch fault {
			case "number":
				witness.NativeStateBlock.Number++
			case "hash":
				witness.NativeStateBlock.Hash = "0x" + strings.Repeat("1", 64)
			case "boundary":
				witness.NativeStateBlock = fixture.finality.headers[3].identity
			case "role-swap":
				witness.RootRole = NativeStorageRootParentExecution
				if role == NativeStorageRootParentExecution {
					witness.RootRole = NativeStorageRootChildPostState
				}
			}
			result, err := fixture.verify(context.Background(), witness)
			if err == nil || result != nil || !strings.Contains(err.Error(), "selected state block differs") {
				t.Fatalf("historical state identity %s for %s was bypassed: %v", fault, role, err)
			}
		}
	}
}

// Unknown role values must not silently default to child post-state.
func TestReceiptHistoricalNativeStateRejectsUnsupportedRole(t *testing.T) {
	for _, role := range []string{"", "execution", "PARENT_EXECUTION", "approved_runtime"} {
		fixture := historicalStorageTestFixture(t, 1, []int{-1, 0, 5, 10})
		witness := fixture.witness(NativeStorageRootChildPostState, 1)
		witness.RootRole = role
		result, err := fixture.verify(context.Background(), witness)
		if err == nil || result != nil || !strings.Contains(err.Error(), "role is unsupported") {
			t.Fatalf("unsupported historical root role selected child state: %v", err)
		}
	}
}

// A missing mapping or two genuinely signed candidates cannot be resolved by
// choosing the first root, even if a supplied proof works at that convenient root.
func TestReceiptHistoricalNativeStateRejectsMissingAndAmbiguousMapping(t *testing.T) {
	for _, mappings := range [][]int{{-1, 0, 0, 10}, {-1, 5, -1, 10}} {
		fixture := historicalStorageTestFixture(t, 1, mappings)
		witness := fixture.witness(NativeStorageRootChildPostState, 1)
		contexts, err := VerifyReceiptFeeContexts(context.Background(), fixture.finality.receipts.archive, fixture.finality.collection, fixture.finality.checkpoint, fixture.finality.proof)
		if err != nil || len(contexts.Blocks) != 1 || contexts.FoundReceiptContextsComplete {
			t.Fatalf("historical unresolved-context prerequisite differs: %v", err)
		}
		result, err := fixture.verify(context.Background(), witness)
		if err == nil || result != nil || !strings.Contains(err.Error(), "one unambiguous native context") {
			t.Fatalf("historical unresolved mapping selected convenient state: %v", err)
		}
	}
}

// A checkpoint parent hash alone must not become an execution root. The same
// child's valid raw proof cannot be relabelled as proof of that unseen parent.
func TestReceiptHistoricalNativeStateRejectsUnavailableCheckpointParent(t *testing.T) {
	fixture := historicalStorageTestFixture(t, 1, []int{0, 5, -1, 10})
	witness := fixture.witness(NativeStorageRootParentExecution, 0)
	contexts, err := VerifyReceiptFeeContexts(context.Background(), fixture.finality.receipts.archive, fixture.finality.collection, fixture.finality.checkpoint, fixture.finality.proof)
	if err != nil || contexts.Blocks[0].State != "native_parent_unavailable" || contexts.Blocks[0].NativeContexts[0].NativeParentStateRoot != nil {
		t.Fatalf("historical missing-parent prerequisite differs: %v", err)
	}
	result, err := fixture.verify(context.Background(), witness)
	if err == nil || result != nil || !strings.Contains(err.Error(), "parent execution root is unavailable") {
		t.Fatalf("checkpoint parent absence became historical storage authority: %v", err)
	}
}

// Missing parent coverage does not invalidate the authenticated checkpoint
// child. Its raw fact leaves the nested fee-context completeness flag false.
func TestReceiptHistoricalNativeStateVerifiesCheckpointChildWithoutInventingParent(t *testing.T) {
	fixture := historicalStorageTestFixture(t, 1, []int{0, 5, -1, 10})
	witness := fixture.witness(NativeStorageRootChildPostState, 0)
	result, err := fixture.verify(context.Background(), witness)
	if err != nil || result == nil || result.NativeStateBlock != fixture.finality.headers[0].identity || result.NativeStateRoot != fixture.vectors[0].Root ||
		!result.NativeHeaderStorageVerified || result.FeeContexts.FoundReceiptContextsComplete || result.FeeContexts.Blocks[0].State != "native_parent_unavailable" ||
		result.FeeContexts.Blocks[0].NativeContexts[0].NativeParent != nil || result.FeeContexts.Blocks[0].NativeContexts[0].NativeParentStateRoot != nil ||
		result.RuntimeSourceAuthenticated || result.FeeAttributionAuthenticated || result.ActualFeesReconciled || result.SpendingAuthorized {
		t.Fatalf("checkpoint child proof was lost or invented a parent: %v", err)
	}
}

// Parent and child can share a trie root while remaining different blocks and
// roles. Only changing both selectors consistently describes a legitimate fact.
func TestReceiptHistoricalNativeStateBindsRolesEvenWhenRootsAreEqual(t *testing.T) {
	fixture := historicalStorageTestFixture(t, 1, []int{-1, 0, 5, 10})
	fixture.vectors[1] = fixture.vectors[0]
	fixture.rebuild()
	for _, fault := range []string{"role-only", "state-only", "both-consistent"} {
		witness := fixture.witness(NativeStorageRootParentExecution, 1)
		if fault != "state-only" {
			witness.RootRole = NativeStorageRootChildPostState
		}
		if fault != "role-only" {
			witness.NativeStateBlock = fixture.finality.headers[1].identity
		}
		result, err := fixture.verify(context.Background(), witness)
		if fault == "both-consistent" {
			if err != nil || result == nil || result.RootRole != NativeStorageRootChildPostState || result.NativeStateBlock != fixture.finality.headers[1].identity {
				t.Fatalf("consistent equal-root child fact was refused: %v", err)
			}
		} else if err == nil || result != nil || !strings.Contains(err.Error(), "selected state block differs") {
			t.Fatalf("equal-root proof bypassed historical role identity: %v", err)
		}
	}
}

// Wrong-root proofs retain their own valid oracle reads but omit the selected
// state's root node. They cannot substitute a parent, child, boundary or tip.
func TestReceiptHistoricalNativeStateRejectsProofFromAnotherRoleOrBlock(t *testing.T) {
	for _, role := range []string{NativeStorageRootParentExecution, NativeStorageRootChildPostState} {
		for _, foreignIndex := range []int{0, 1, 3, 4} {
			fixture := historicalStorageTestFixture(t, 1, []int{-1, 0, 5, 10})
			witness := fixture.witness(role, 1)
			selectedIndex := 1
			if role == NativeStorageRootParentExecution {
				selectedIndex = 0
			}
			if foreignIndex == selectedIndex {
				continue
			}
			for _, encoded := range fixture.vectors[foreignIndex].Nodes {
				raw, _ := hex.DecodeString(encoded[2:])
				hash := blake2b.Sum256(raw)
				if "0x"+hex.EncodeToString(hash[:]) == fixture.vectors[selectedIndex].Root {
					t.Fatal("foreign-root fixture accidentally contains selected root")
				}
			}
			witness.Nodes, witness.Reads = fixture.vectors[foreignIndex].Nodes, fixture.vectors[foreignIndex].Reads
			result, err := fixture.verify(context.Background(), witness)
			if err == nil || result != nil || !strings.Contains(err.Error(), "missing node") {
				t.Fatalf("historical proof from another role or block was accepted: %v", err)
			}
		}
	}
}

// A later certificate has its own valid trie and an additional mapping for the
// old EVM block. It neither makes history ambiguous nor replaces either root.
func TestReceiptHistoricalNativeStateKeepsRootsUnderDescendantCertificate(t *testing.T) {
	for _, role := range []string{NativeStorageRootParentExecution, NativeStorageRootChildPostState} {
		fixture := historicalStorageTestFixture(t, 1, []int{-1, 0, 5, 10})
		before := objectDigest(fixture.finality.collection)
		descendant := fixture.certifyDescendant()
		witness := fixture.witness(role, 1)
		result, err := fixture.verify(context.Background(), witness)
		if err != nil || result == nil || result.NativeStateBlock != witness.NativeStateBlock ||
			result.FeeContexts.Finality.NativeFinalized != fixture.finality.headers[3].identity || result.FeeContexts.Finality.NativeCertified == nil ||
			*result.FeeContexts.Finality.NativeCertified != descendant.identity || len(result.FeeContexts.Blocks[0].NativeContexts) != 1 || before != objectDigest(fixture.finality.collection) {
			t.Fatalf("descendant certificate moved historical raw storage context: %v", err)
		}
		witness.NativeStateBlock = descendant.identity
		witness.Nodes, witness.Reads = fixture.vectors[4].Nodes, fixture.vectors[4].Reads
		if result, err := fixture.verify(context.Background(), witness); err == nil || result != nil {
			t.Fatal("certificate-tip proof replaced historical state")
		}
	}
}

// A valid descendant may first mention the receipt's EVM hash after the original
// collection boundary. That future mapping is not historical receipt evidence.
func TestReceiptHistoricalNativeStateRejectsDescendantOnlyMapping(t *testing.T) {
	fixture := historicalStorageTestFixture(t, 1, []int{-1, 5, -1, 10})
	descendant := fixture.certifyDescendant()
	witness := fixture.witness(NativeStorageRootChildPostState, 1)
	witness.NativeReceiptBlock, witness.NativeStateBlock = descendant.identity, descendant.identity
	witness.Nodes, witness.Reads = fixture.vectors[4].Nodes, fixture.vectors[4].Reads
	contexts, err := VerifyReceiptFeeContexts(context.Background(), fixture.finality.receipts.archive, fixture.finality.collection, fixture.finality.checkpoint, fixture.finality.proof)
	if err != nil || contexts.Blocks[0].State != "native_mapping_unavailable" || contexts.Finality.NativeCertified == nil {
		t.Fatalf("descendant-only fixture prerequisite differs: %v", err)
	}
	if result, err := fixture.verify(context.Background(), witness); err == nil || result != nil {
		t.Fatal("descendant-only mapping supplied historical raw storage")
	}
}

// Resealed input digests cannot repair a corrupted signature or changed receipt
// gas. Every call must replay the original cryptographic evidence from scratch.
func TestReceiptHistoricalNativeStateReplaysForgedReceiptAndNativeEvidence(t *testing.T) {
	for _, fault := range []string{"certificate", "receipt-gas"} {
		fixture := historicalStorageTestFixture(t, 1, []int{-1, 0, 5, 10})
		if fault == "certificate" {
			raw, _ := hex.DecodeString(fixture.finality.proof.Segments[0].JustificationScale[2:])
			raw[81] ^= 1
			fixture.finality.proof.Segments[0].JustificationScale = "0x" + hex.EncodeToString(raw)
		} else {
			for index := range fixture.finality.receipts.observations.Receipts {
				if receipt := fixture.finality.receipts.observations.Receipts[index].Receipt; receipt != nil {
					receipt.GasUsed--
					break
				}
			}
		}
		fixture.finality.bind()
		witness := fixture.witness(NativeStorageRootChildPostState, 1)
		result, err := fixture.verify(context.Background(), witness)
		if err == nil || result != nil || !strings.Contains(err.Error(), "context replay") {
			t.Fatalf("historical storage bypassed fresh %s replay: %v", fault, err)
		}
	}
}

// Genuine header roots cannot turn a changed key/value or missing node into a
// successful historical read. This checks the public join still calls the trie.
func TestReceiptHistoricalNativeStateRequiresCompleteMatchingRawProof(t *testing.T) {
	for _, fault := range []string{"value", "key", "missing-root", "missing-value"} {
		fixture := historicalStorageTestFixture(t, 1, []int{-1, 0, 5, 10})
		witness := fixture.witness(NativeStorageRootChildPostState, 1)
		switch fault {
		case "value":
			wrong := "0x01"
			witness.Reads[0].Value = &wrong
		case "key":
			witness.Reads[0].Key = "0xff"
		case "missing-root":
			witness.Nodes = nil
		case "missing-value":
			value := *witness.Reads[0].Value
			witness.Nodes = slices.DeleteFunc(witness.Nodes, func(node string) bool { return node == value })
			if len(witness.Nodes)+1 != len(fixture.vectors[1].Nodes) {
				t.Fatal("historical external-value fixture did not remove one blob")
			}
		}
		if result, err := fixture.verify(context.Background(), witness); err == nil || result != nil {
			t.Fatalf("historical raw proof %s was not verified", fault)
		}
	}
}

// Raw value pointers and nested derived contexts belong to the returned result,
// so modifying them cannot rewrite original signed/archive/witness evidence.
func TestReceiptHistoricalNativeStateOwnsResultWithoutChangingInputs(t *testing.T) {
	fixture := historicalStorageTestFixture(t, 1, []int{-1, 0, 5, 10})
	witness := fixture.witness(NativeStorageRootChildPostState, 1)
	before := objectDigest([]any{fixture.finality.receipts.archive, fixture.finality.collection, fixture.finality.checkpoint, fixture.finality.proof, witness})
	result, err := fixture.verify(context.Background(), witness)
	if err != nil {
		t.Fatal(err)
	}
	for _, read := range result.Reads {
		if read.Value != nil {
			*read.Value = "0xffff"
		}
	}
	result.FeeContexts.Transactions[0].Origins[0].Source = "mutated-result"
	*result.FeeContexts.Blocks[0].NativeContexts[0].NativeParentStateRoot = "mutated-result"
	if before != objectDigest([]any{fixture.finality.receipts.archive, fixture.finality.collection, fixture.finality.checkpoint, fixture.finality.proof, witness}) {
		t.Fatal("historical output mutation changed borrowed evidence")
	}
}

// Fixed count/schema limits reject before any expensive proof work; invalid
// archived inputs distinguish that early gate from a later incidental rejection.
func TestReceiptHistoricalNativeStateBoundsBeforeReplay(t *testing.T) {
	fixture := historicalStorageTestFixture(t, 1, []int{-1, 0, 5, 10})
	for _, fault := range []string{"nodes", "reads", "empty-reads", "schema", "codec", "nil-witness"} {
		witness := fixture.witness(NativeStorageRootChildPostState, 1)
		switch fault {
		case "nodes":
			witness.Nodes = make([]string, maximumNativeStorageNodes+1)
		case "reads":
			witness.Reads = make([]NativeStorageRead, maximumNativeStorageReads+1)
		case "empty-reads":
			witness.Reads = nil
		case "schema":
			witness.Schema = "claimed-historical-authority"
		case "codec":
			witness.CodecProfile = "claimed-runtime-layout"
		case "nil-witness":
			witness = nil
		}
		result, err := VerifyReceiptHistoricalNativeState(context.Background(), nil, nil, nil, nil, witness)
		if err == nil || result != nil || !strings.Contains(err.Error(), "schema, codec or count differs") {
			t.Fatalf("historical %s bound did not precede replay: %v", fault, err)
		}
	}
}

// Counting actual context checks first avoids hardcoded scheduler/time budgets;
// cancellation then targets entry, replay and both final publication checks.
func TestReceiptHistoricalNativeStateCancellationReturnsNoPartialResult(t *testing.T) {
	fixture := historicalStorageTestFixture(t, 1, []int{-1, 0, 5, 10})
	witness := fixture.witness(NativeStorageRootChildPostState, 1)
	baseline, baselineCancel := context.WithCancel(context.Background())
	counter := &nativeStorageCancelContext{Context: baseline, cancel: baselineCancel}
	defer baselineCancel()
	if result, err := fixture.verify(counter, witness); err != nil || result == nil || counter.calls < 5 {
		t.Fatalf("historical cancellation baseline failed: %v", err)
	}
	for _, at := range []int{1, 3, counter.calls - 1, counter.calls} {
		base, cancel := context.WithCancel(context.Background())
		ctx := &nativeStorageCancelContext{Context: base, cancel: cancel, at: at}
		result, err := fixture.verify(ctx, witness)
		cancel()
		if !errors.Is(err, context.Canceled) || result != nil {
			t.Fatalf("historical cancellation leaked partial result at check %d: %v", at, err)
		}
	}
	if result, err := fixture.verify(nil, witness); err == nil || result != nil {
		t.Fatal("historical nil context reached proof work")
	}
}

// Byte mismatch and malformed digest syntax are separate faults; both are
// tested along with the private-file and strict duplicate/unknown-field gates.
func TestReceiptHistoricalNativeStateLoaderPinsPrivateStrictBytes(t *testing.T) {
	fixture := historicalStorageTestFixture(t, 1, []int{-1, 0, 5, 10})
	witness := fixture.witness(NativeStorageRootParentExecution, 1)
	raw, err := json.Marshal(witness)
	if err != nil {
		t.Fatal(err)
	}
	directory := t.TempDir()
	if err := os.Chmod(directory, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(directory, "historical-storage.json")
	for _, fault := range []string{"valid", "pin", "malformed-pin", "mode", "unknown-root", "authority", "duplicate", "trailing"} {
		encoded := slices.Clone(raw)
		mode := os.FileMode(0600)
		switch fault {
		case "mode":
			mode = 0644
		case "unknown-root":
			encoded = append([]byte(`{"native_state_root":"0x00",`), raw[1:]...)
		case "authority":
			encoded = append([]byte(`{"spending_authorized":true,`), raw[1:]...)
		case "duplicate":
			encoded = append([]byte(`{"root_role":"parent_execution",`), raw[1:]...)
		case "trailing":
			encoded = append(encoded, []byte(" {}")...)
		}
		if err := os.WriteFile(path, encoded, 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(path, mode); err != nil {
			t.Fatal(err)
		}
		reference := FileReference{Path: path, Sha256: digest(encoded)}
		if fault == "pin" {
			reference.Sha256 = "sha256:" + strings.Repeat("1", 64)
			if !canonicalDigest(reference.Sha256) || reference.Sha256 == digest(encoded) {
				t.Fatal("historical loader mismatch fixture is not canonical and distinct")
			}
		} else if fault == "malformed-pin" {
			reference.Sha256 = strings.Repeat("1", 64)
		}
		result, err := LoadReceiptHistoricalNativeStateWitness(context.Background(), reference)
		if fault == "valid" {
			if err != nil || objectDigest(result) != objectDigest(witness) {
				t.Fatalf("historical private witness did not round trip: %v", err)
			}
			continue
		}
		if err == nil || result != nil {
			t.Fatalf("historical loader accepted %s fault", fault)
		}
		if fault == "pin" && err.Error() != "historical native storage witness differs from its byte pin" {
			t.Fatalf("historical valid pin mismatch did not reach byte comparison: %v", err)
		}
		if fault == "malformed-pin" && err.Error() != "historical native storage witness requires a context and exact byte pin" {
			t.Fatalf("historical malformed pin did not reach syntax guard: %v", err)
		}
	}
}
