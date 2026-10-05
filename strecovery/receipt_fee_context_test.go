// Synthetic receipt tries and independently signed native paths separate the
// three state roots, historical coverage and runtime authority without a node.
package strecovery

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"slices"
	"strings"
	"testing"

	"golang.org/x/crypto/blake2b"
)

// Every native root differs from its parent and from the EVM intermediate root.
// Altering the raw independent encoding precedes header hashing and signing.
func feeContextTestHeader(number uint32, parent string, root byte, digests ...[]byte) *finalityTestHeader {
	header := finalityTestNativeHeader(number, parent, digests...)
	offset := 32 + len(finalityTestCompact(number))
	copy(header.raw[offset:offset+32], bytes.Repeat([]byte{root}, 32))
	hash := blake2b.Sum256(header.raw)
	header.identity.Hash = "0x" + hex.EncodeToString(hash[:])
	return header
}

// Mapping indices choose exact EVM header hashes; -1 emits no Frontier log.
// Native heights start at 700 while EVM receipts are in block 90.
func feeContextTestFixture(t testing.TB, winner int, mappings []int) *receiptFinalityFixture {
	t.Helper()
	return feeContextTestFixtureOnChain(t, winner, mappings, 31337)
}

func feeContextTestFixtureOnChain(t testing.TB, winner int, mappings []int, chainId uint64) *receiptFinalityFixture {
	t.Helper()
	keys, authorities := finalityTestAuthorities(31)
	fixture := &receiptFinalityFixture{receipts: receiptCommitmentTestFixtureOnChain(t, winner, chainId), keys: keys}
	parent := "0x" + strings.Repeat("9", 64)
	for index, mapping := range mappings {
		var digests [][]byte
		if mapping >= 0 {
			digests = append(digests, finalityTestFrontier(fixture.receipts.headers[mapping].Hash().Hex()))
		}
		header := feeContextTestHeader(uint32(700+index), parent, byte(0x70+index), digests...)
		fixture.headers = append(fixture.headers, header)
		parent = header.identity.Hash
	}
	fixture.checkpoint = &NativeFinalityCheckpoint{Schema: NativeFinalityCheckpointSchema, CodecProfile: NativeFinalityCodecProfile,
		Genesis: fixture.receipts.archive.Selection.Genesis, HeaderScale: "0x" + hex.EncodeToString(fixture.headers[0].raw), SetId: 9, Authorities: authorities, LiveState: "live"}
	fixture.proof = &ReceiptFinalityProof{Schema: ReceiptFinalityProofSchema, Segments: []GrandpaFinalitySegment{fixture.segment(fixture.headers[1:], keys, 9)}}
	fixture.bind()
	return fixture
}

// Proved roots and gas do not admit a payer or invent native amounts, including
// the reverted cancellation. All archived alternatives and nonce groups survive.
func TestReceiptFeeContextsJoinEarlierNativeParentAndPreserveEveryAttempt(t *testing.T) {
	for _, winner := range []int{1, 2, 3} {
		fixture := feeContextTestFixture(t, winner, []int{-1, 0, 5, 10})
		before := []string{objectDigest(fixture.receipts.archive), objectDigest(fixture.collection), fixture.checkpoint.Hash(), objectDigest(fixture.proof)}
		result, err := VerifyReceiptFeeContexts(context.Background(), fixture.receipts.archive, fixture.collection, fixture.checkpoint, fixture.proof)
		if err != nil {
			t.Fatal(err)
		}
		if len(result.Blocks) != 1 || !result.FoundReceiptContextsComplete || len(result.Transactions) != 6 || len(result.Finality.Receipts.Observations.Nonces) != 4 {
			t.Fatalf("historical receipt contexts lost coverage: %+v", result)
		}
		block := result.Blocks[0]
		if block.EvmBlock.Number != 90 || block.EvmBlock.Hash != fixture.receipts.headers[0].Hash().Hex() || block.EvmStateRoot != "0x"+strings.Repeat("22", 32) || block.State != "native_context_complete" || len(block.NativeContexts) != 1 {
			t.Fatalf("receipt was attached to the wrong EVM context: %+v", block)
		}
		native := block.NativeContexts[0]
		if native.NativeBlock != fixture.headers[1].identity || native.NativeBlock.Number != 701 || native.NativeStateRoot != "0x"+strings.Repeat("71", 32) ||
			native.NativeParentHash != fixture.headers[0].identity.Hash || native.NativeParent == nil || *native.NativeParent != fixture.headers[0].identity ||
			native.NativeParentStateRoot == nil || *native.NativeParentStateRoot != "0x"+strings.Repeat("70", 32) || native.FrontierPostLogVariant != 3 {
			t.Fatalf("native parent/child roots were substituted: %+v", native)
		}
		if result.AuthorityCheckpointAuthenticated || result.GenesisAuthenticated || result.RuntimeSourceAuthenticated || result.PayerBindingAuthenticated || result.NativeExtrinsicLocationsAuthenticated ||
			result.NativeStateReadsAuthenticated || result.FeeAttributionAuthenticated || result.FinalityAuthenticated || result.CanonicalReceiptsReconciled || result.ActualFeesReconciled || result.SpendingAuthorized ||
			result.Admission != "unapproved_fee_context_proof" || result.Profile != ReceiptFeeContextProfile || result.ProfileRuntimeSource != NativeFinalityRuntimeSource {
			t.Fatal("relative block proof acquired runtime, payer or fee authority")
		}
		found, unavailable, reverted := 0, 0, 0
		var gas uint64
		for index, transaction := range result.Transactions {
			archived := fixture.receipts.archive.Transactions[index]
			if transaction.Hash != archived.Hash || transaction.Role != archived.Role || transaction.Sender != archived.Sender || transaction.Nonce != archived.Nonce || !slices.Equal(transaction.Origins, archived.Origins) ||
				transaction.NativeExtrinsicIndex != nil || transaction.ActualWithdrawalRao != nil || transaction.ActualRefundRao != nil || transaction.ActualGasDebitRao != nil || len(transaction.MissingEvidence) == 0 {
				t.Fatal("native context discarded history or invented transaction-scoped money")
			}
			// Independent streaming form checks the source profile against the
			// actual signed sender; this remains only a candidate account.
			hasher, _ := blake2b.New256(nil)
			_, _ = hasher.Write([]byte{'e', 'v', 'm', ':'})
			address, _ := hex.DecodeString(archived.Sender[2:])
			_, _ = hasher.Write(address)
			if transaction.ProfileMappedAccount != "0x"+hex.EncodeToString(hasher.Sum(nil)) {
				t.Fatal("source-profile mapping changed the signed sender or namespace")
			}
			if transaction.Receipt == nil {
				unavailable++
				if transaction.NativeContextState != "receipt_unavailable" {
					t.Fatal("missing alternate receipt acquired a block context")
				}
				continue
			}
			found++
			gas += transaction.Receipt.GasUsed
			if transaction.Receipt.Status == 0 {
				reverted++
			}
			if transaction.NativeContextState != block.State || transaction.Receipt.ActualGasFee != nil || transaction.Receipt.Hash != transaction.Hash {
				t.Fatal("committed gas became a fee or changed receipt identity")
			}
		}
		expectedGas, expectedReverted := uint64(100000), 0
		if winner == 3 {
			expectedGas, expectedReverted = 96000, 1
		}
		if found != 4 || unavailable != 2 || gas != expectedGas || reverted != expectedReverted {
			t.Fatalf("nonce alternatives or reverted gas changed: found=%d unavailable=%d gas=%d reverted=%d", found, unavailable, gas, reverted)
		}
		baseline, err := VerifyReceiptFinality(context.Background(), fixture.receipts.archive, fixture.collection, fixture.checkpoint, fixture.proof)
		if err != nil || objectDigest(result.Finality) != objectDigest(baseline) {
			t.Fatal("fee contexts rewrote native/receipt evidence history")
		}
		result.Transactions[0].Origins[0].Source = "modified-output"
		if !slices.Equal(before, []string{objectDigest(fixture.receipts.archive), objectDigest(fixture.collection), fixture.checkpoint.Hash(), objectDigest(fixture.proof)}) {
			t.Fatal("context verification or result mutation changed borrowed evidence")
		}
	}
}

// Final-boundary commitment does not supply an earlier receipt's native block.
// Unavailable history remains a useful unresolved report, never a guessed root.
func TestReceiptFeeContextsLeaveUnmappedEarlierReceiptUnresolved(t *testing.T) {
	fixture := feeContextTestFixture(t, 1, []int{-1, 5, -1, 10})
	result, err := VerifyReceiptFeeContexts(context.Background(), fixture.receipts.archive, fixture.collection, fixture.checkpoint, fixture.proof)
	if err != nil {
		t.Fatal(err)
	}
	if result.FoundReceiptContextsComplete || len(result.Blocks) != 1 || result.Blocks[0].State != "native_mapping_unavailable" || len(result.Blocks[0].NativeContexts) != 0 || !result.Finality.NativeEvmCommitmentVerified {
		t.Fatal("an unrelated final boundary supplied an earlier receipt root")
	}
	for _, transaction := range result.Transactions {
		if transaction.Receipt != nil && transaction.NativeContextState != "native_mapping_unavailable" {
			t.Fatal("missing native mapping was not propagated to its exact receipt")
		}
	}
}

// Two genuine signed commitments are ambiguity, even if both parent roots are
// available. Neither chronological order nor a caller chooses the payer state.
func TestReceiptFeeContextsRetainAmbiguousNativeMappings(t *testing.T) {
	fixture := feeContextTestFixture(t, 1, []int{-1, 0, 0, 10})
	result, err := VerifyReceiptFeeContexts(context.Background(), fixture.receipts.archive, fixture.collection, fixture.checkpoint, fixture.proof)
	if err != nil {
		t.Fatal(err)
	}
	block := result.Blocks[0]
	if result.FoundReceiptContextsComplete || block.State != "native_mapping_ambiguous" || len(block.NativeContexts) != 2 ||
		block.NativeContexts[0].NativeBlock != fixture.headers[1].identity || block.NativeContexts[1].NativeBlock != fixture.headers[2].identity {
		t.Fatalf("ambiguous receipt mapping selected a convenient root: %+v", block)
	}
	for _, transaction := range result.Transactions {
		if transaction.Receipt != nil && transaction.NativeContextState != "native_mapping_ambiguous" {
			t.Fatal("ambiguous native roots became a complete transaction context")
		}
	}
}

// A checkpoint commits its parent hash, not its parent's root. Its own state
// must never be passed off as the execution parent for a receipt in that block.
func TestReceiptFeeContextsKeepCheckpointParentUnavailable(t *testing.T) {
	fixture := feeContextTestFixture(t, 1, []int{0, 5, -1, 10})
	result, err := VerifyReceiptFeeContexts(context.Background(), fixture.receipts.archive, fixture.collection, fixture.checkpoint, fixture.proof)
	if err != nil {
		t.Fatal(err)
	}
	block := result.Blocks[0]
	if result.FoundReceiptContextsComplete || block.State != "native_parent_unavailable" || len(block.NativeContexts) != 1 {
		t.Fatalf("checkpoint mapping invented historical coverage: %+v", block)
	}
	native := block.NativeContexts[0]
	if native.NativeBlock != fixture.headers[0].identity || native.NativeStateRoot != "0x"+strings.Repeat("70", 32) || native.NativeParentHash != "0x"+strings.Repeat("9", 64) || native.NativeParent != nil || native.NativeParentStateRoot != nil {
		t.Fatal("checkpoint's unseen parent was replaced by a supplied or child root")
	}
}

// A later certificate can finalize the original boundary. Its additional
// headers cannot move old receipt execution to a future native state.
func TestReceiptFeeContextsExcludeDescendantOnlyHistoricalMapping(t *testing.T) {
	fixture := feeContextTestFixture(t, 1, []int{-1, 5, -1, 10})
	before := objectDigest(fixture.collection)
	descendant := feeContextTestHeader(704, fixture.headers[3].identity.Hash, 0x74, finalityTestFrontier(fixture.receipts.headers[0].Hash().Hex()))
	fixture.proof.Schema = ReceiptFinalityDescendantProofSchema
	fixture.proof.Segments = []GrandpaFinalitySegment{fixture.segment(append(slices.Clone(fixture.headers[1:]), descendant), fixture.keys, 9)}
	result, err := VerifyReceiptFeeContexts(context.Background(), fixture.receipts.archive, fixture.collection, fixture.checkpoint, fixture.proof)
	if err != nil {
		t.Fatal(err)
	}
	if result.Finality.NativeFinalized != fixture.headers[3].identity || result.Finality.NativeCertified == nil || *result.Finality.NativeCertified != descendant.identity ||
		result.FoundReceiptContextsComplete || result.Blocks[0].State != "native_mapping_unavailable" || len(result.Blocks[0].NativeContexts) != 0 || before != objectDigest(fixture.collection) {
		t.Fatal("descendant certificate rewrote historical collection/context coverage")
	}
}

// A quorum over a malformed intermediate Frontier log remains valid native
// finality, but cannot be decoded as this fee-context profile.
func TestReceiptFeeContextsRejectUnsupportedIntermediateFrontierSemantics(t *testing.T) {
	fixture := feeContextTestFixture(t, 1, []int{-1, 0, 5, 10})
	fixture.headers[1] = feeContextTestHeader(701, fixture.headers[0].identity.Hash, 0x71, finalityTestDigest("fron", append([]byte{2}, bytes.Repeat([]byte{0x56}, 32)...)))
	fixture.headers[2] = feeContextTestHeader(702, fixture.headers[1].identity.Hash, 0x72)
	fixture.headers[3] = feeContextTestHeader(703, fixture.headers[2].identity.Hash, 0x73, finalityTestFrontier(fixture.receipts.observations.EvmFinalized.Hash))
	fixture.proof.Segments = []GrandpaFinalitySegment{fixture.segment(fixture.headers[1:], fixture.keys, 9)}
	fixture.bind()
	if _, err := VerifyReceiptFinality(context.Background(), fixture.receipts.archive, fixture.collection, fixture.checkpoint, fixture.proof); err != nil {
		t.Fatalf("native signature/ancestry prerequisite failed: %v", err)
	}
	if result, err := VerifyReceiptFeeContexts(context.Background(), fixture.receipts.archive, fixture.collection, fixture.checkpoint, fixture.proof); err == nil || result != nil || !strings.Contains(err.Error(), "unsupported") {
		t.Fatalf("unsupported historical Frontier semantics silently supplied context: %v", err)
	}
}

// Re-pinned receipt summaries and independently signed unrelated native
// ancestry cannot bypass the underlying cryptographic entry point.
func TestReceiptFeeContextsRejectResealedReceiptAndNativeForgery(t *testing.T) {
	for _, fault := range []string{"gas", "signature", "boundary-mapping"} {
		fixture := feeContextTestFixture(t, 1, []int{-1, 0, 5, 10})
		switch fault {
		case "gas":
			for index := range fixture.receipts.observations.Receipts {
				if receipt := fixture.receipts.observations.Receipts[index].Receipt; receipt != nil {
					receipt.GasUsed--
					break
				}
			}
		case "signature":
			raw, _ := hex.DecodeString(fixture.proof.Segments[0].JustificationScale[2:])
			raw[81] ^= 1
			fixture.proof.Segments[0].JustificationScale = "0x" + hex.EncodeToString(raw)
		case "boundary-mapping":
			fixture.headers[3] = feeContextTestHeader(703, fixture.headers[2].identity.Hash, 0x73, finalityTestFrontier(fixture.receipts.headers[5].Hash().Hex()))
			fixture.proof.Segments = []GrandpaFinalitySegment{fixture.segment(fixture.headers[1:], fixture.keys, 9)}
		}
		fixture.bind()
		if result, err := VerifyReceiptFeeContexts(context.Background(), fixture.receipts.archive, fixture.collection, fixture.checkpoint, fixture.proof); err == nil || result != nil {
			t.Fatalf("forged %s evidence produced native fee context", fault)
		}
	}
}

// Bounds apply before expensive archive verification and across all matching
// native candidates, including duplicates. Cancellation needs no timed sleep.
func TestReceiptFeeContextsBoundCandidatesOriginsAndCancellation(t *testing.T) {
	for _, archive := range []*Archive{
		{Transactions: make([]Transaction, maximumFeeContextTransactions+1)},
		{Transactions: []Transaction{{Origins: make([]Origin, maximumFeeContextOrigins+1)}}},
	} {
		if result, err := VerifyReceiptFeeContexts(context.Background(), archive, nil, nil, nil); err == nil || result != nil || !strings.Contains(err.Error(), "fee context") {
			t.Fatalf("unbounded archive reached proof work: %v", err)
		}
	}
	fixture := feeContextTestFixture(t, 1, []int{-1, 0, 5, 10})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if result, err := VerifyReceiptFeeContexts(ctx, fixture.receipts.archive, fixture.collection, fixture.checkpoint, fixture.proof); !errors.Is(err, context.Canceled) || result != nil {
		t.Fatalf("cancelled fee verification continued: %v", err)
	}
	mappings := append([]int{-1}, make([]int, maximumFeeNativeContexts+1)...)
	mappings = append(mappings, 10)
	fixture = feeContextTestFixture(t, 1, mappings)
	if result, err := VerifyReceiptFeeContexts(context.Background(), fixture.receipts.archive, fixture.collection, fixture.checkpoint, fixture.proof); err == nil || result != nil || !strings.Contains(err.Error(), "native candidate bound") {
		t.Fatalf("native candidate work exceeded its fixed bound: %v", err)
	}
}
