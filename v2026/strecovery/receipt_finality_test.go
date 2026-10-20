// Synthetic Ed25519 authorities sign independently encoded SCALE certificates.
// These tests join real archived transaction proofs without any node or signer.
package strecovery

import (
	"bytes"
	"context"
	"crypto/ed25519"
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

// Fixture headers carry separately computed identities; production decoders
// are never used to produce signatures or expected ancestry.
type finalityTestHeader struct {
	raw      []byte
	identity ObservedBlockIdentity
	digests  [][]byte
}

// Independent SCALE encoding covers the u32 compact domain used by headers.
func finalityTestCompact(value uint32) []byte {
	switch {
	case value < 64:
		return []byte{byte(value * 4)}
	case value < 16384:
		return binary.LittleEndian.AppendUint16(nil, uint16(value*4+1))
	case value < 1<<30:
		return binary.LittleEndian.AppendUint32(nil, value*4+2)
	default:
		return binary.LittleEndian.AppendUint32([]byte{3}, value)
	}
}

// Hash bytes are synthetic and locally generated, never copied from a node.
func finalityTestNativeHeader(number uint32, parent string, digests ...[]byte) *finalityTestHeader {
	raw, _ := hex.DecodeString(parent[2:])
	raw = append(raw, finalityTestCompact(number)...)
	raw = append(raw, bytes.Repeat([]byte{0x42}, 32)...)
	raw = append(raw, bytes.Repeat([]byte{0x73}, 32)...)
	raw = append(raw, finalityTestCompact(uint32(len(digests)))...)
	for _, digest := range digests {
		raw = append(raw, digest...)
	}
	hash := blake2b.Sum256(raw)
	return &finalityTestHeader{raw: raw, identity: ObservedBlockIdentity{Number: uint64(number), Hash: "0x" + hex.EncodeToString(hash[:])}, digests: digests}
}

// This encodes Consensus(engine, payload), including both SCALE lengths.
func finalityTestDigest(engine string, payload []byte) []byte {
	raw := append([]byte{4}, []byte(engine)...)
	raw = append(raw, finalityTestCompact(uint32(len(payload)))...)
	return append(raw, payload...)
}

// The Frontier hash selects its own EVM header, independent of native height.
func finalityTestFrontier(hash string) []byte {
	raw, _ := hex.DecodeString(hash[2:])
	return finalityTestDigest("fron", append([]byte{3}, raw...))
}

// Deterministic seeds are test-only. Unequal weights detect vote-count shortcuts.
func finalityTestAuthorities(seed byte) ([]ed25519.PrivateKey, []GrandpaAuthority) {
	keys := make([]ed25519.PrivateKey, 4)
	authorities := make([]GrandpaAuthority, 4)
	for index := range keys {
		keys[index] = ed25519.NewKeyFromSeed(bytes.Repeat([]byte{seed + byte(index)}, ed25519.SeedSize))
		authorities[index] = GrandpaAuthority{PublicKey: "0x" + hex.EncodeToString(keys[index].Public().(ed25519.PublicKey)), Weight: uint64(4 - index)}
	}
	return keys, authorities
}

// The payload matches the independently read Rust codec field order; it never
// calls the production message builder or signature verifier.
func finalityTestCertificate(target *finalityTestHeader, targets []*finalityTestHeader, ancestry []*finalityTestHeader, signers []int, keys []ed25519.PrivateKey, round, setId uint64) string {
	raw := binary.LittleEndian.AppendUint64(nil, round)
	hash, _ := hex.DecodeString(target.identity.Hash[2:])
	raw = append(raw, hash...)
	raw = binary.LittleEndian.AppendUint32(raw, uint32(target.identity.Number))
	raw = append(raw, finalityTestCompact(uint32(len(signers)))...)
	for index, signer := range signers {
		voteTarget := target
		if targets != nil {
			voteTarget = targets[index]
		}
		hash, _ := hex.DecodeString(voteTarget.identity.Hash[2:])
		vote := binary.LittleEndian.AppendUint32(slices.Clone(hash), uint32(voteTarget.identity.Number))
		var message bytes.Buffer
		message.WriteByte(1)
		message.Write(vote)
		_ = binary.Write(&message, binary.LittleEndian, round)
		_ = binary.Write(&message, binary.LittleEndian, setId)
		raw = append(raw, vote...)
		raw = append(raw, ed25519.Sign(keys[signer], message.Bytes())...)
		raw = append(raw, keys[signer].Public().(ed25519.PublicKey)...)
	}
	raw = append(raw, finalityTestCompact(uint32(len(ancestry)))...)
	for _, header := range ancestry {
		raw = append(raw, header.raw...)
	}
	return "0x" + hex.EncodeToString(raw)
}

// Next keys and delay are committed in the native scheduled-change digest.
func finalityTestScheduled(authorities []GrandpaAuthority, delay uint32) []byte {
	raw := append([]byte{1}, finalityTestCompact(uint32(len(authorities)))...)
	for _, authority := range authorities {
		key, _ := hex.DecodeString(authority.PublicKey[2:])
		raw = append(raw, key...)
		raw = binary.LittleEndian.AppendUint64(raw, authority.Weight)
	}
	return finalityTestDigest("FRNK", binary.LittleEndian.AppendUint32(raw, delay))
}

// The fixture holds the original signed census and exact collection seal.
type receiptFinalityFixture struct {
	receipts   *receiptCommitmentFixture
	collection *ReceiptCollection
	checkpoint *NativeFinalityCheckpoint
	proof      *ReceiptFinalityProof
	keys       []ed25519.PrivateKey
	headers    []*finalityTestHeader
}

// Default history has three native successors for an unrelated EVM height.
func receiptFinalityTestFixture(t testing.TB) *receiptFinalityFixture {
	t.Helper()
	keys, authorities := finalityTestAuthorities(11)
	fixture := &receiptFinalityFixture{receipts: receiptCommitmentTestFixture(t, 1), keys: keys}
	anchor := finalityTestNativeHeader(700, "0x"+strings.Repeat("9", 64))
	fixture.headers = append(fixture.headers, anchor)
	for number := uint32(701); number <= 703; number++ {
		fixture.headers = append(fixture.headers, finalityTestNativeHeader(number, fixture.headers[len(fixture.headers)-1].identity.Hash, finalityTestFrontier(fixture.receipts.observations.EvmFinalized.Hash)))
	}
	fixture.checkpoint = &NativeFinalityCheckpoint{Schema: NativeFinalityCheckpointSchema, CodecProfile: NativeFinalityCodecProfile, Genesis: fixture.receipts.archive.Selection.Genesis,
		HeaderScale: "0x" + hex.EncodeToString(anchor.raw), SetId: 9, Authorities: authorities, LiveState: "live"}
	fixture.proof = &ReceiptFinalityProof{Schema: ReceiptFinalityProofSchema, Segments: []GrandpaFinalitySegment{fixture.segment(fixture.headers[1:], keys, 9)}}
	fixture.bind()
	return fixture
}

// Each certificate has a fresh complete segment; old-set signatures are never
// synthesized from a reported authority boolean or a claimed checkpoint hash.
func (self *receiptFinalityFixture) segment(headers []*finalityTestHeader, keys []ed25519.PrivateKey, setId uint64) GrandpaFinalitySegment {
	segment := GrandpaFinalitySegment{Headers: []string{}, JustificationScale: finalityTestCertificate(headers[len(headers)-1], nil, nil, []int{0, 1}, keys, 17, setId)}
	for _, header := range headers {
		segment.Headers = append(segment.Headers, "0x"+hex.EncodeToString(header.raw))
	}
	return segment
}

// Resealing file associations deliberately grants no signature repair.
func (self *receiptFinalityFixture) bind() {
	self.receipts.observations.NativeFinalized = self.headers[len(self.headers)-1].identity
	self.receipts.commitments.ObservationHash = objectDigest(self.receipts.observations)
	self.collection = &ReceiptCollection{Schema: ReceiptCollectionSchema, Admission: "unapproved_observation", Observations: self.receipts.observations, Commitments: self.receipts.commitments}
	self.collection.ContentHash = self.collection.hash()
	self.proof.CheckpointHash, self.proof.CollectionHash = self.checkpoint.Hash(), self.collection.ContentHash
}

// Rotation is signaled at 701 and enacted at 703, with an intermediate old-set
// certificate at 701 and a new-set certificate at 704.
func receiptFinalityRotationFixture(t testing.TB) (*receiptFinalityFixture, []ed25519.PrivateKey) {
	t.Helper()
	fixture := receiptFinalityTestFixture(t)
	newKeys, newAuthorities := finalityTestAuthorities(51)
	fixture.headers = fixture.headers[:1]
	for number := uint32(701); number <= 704; number++ {
		digests := [][]byte{finalityTestFrontier(fixture.receipts.observations.EvmFinalized.Hash)}
		if number == 701 {
			digests = append(digests, finalityTestScheduled(newAuthorities, 2))
		}
		fixture.headers = append(fixture.headers, finalityTestNativeHeader(number, fixture.headers[len(fixture.headers)-1].identity.Hash, digests...))
	}
	fixture.proof.Segments = []GrandpaFinalitySegment{fixture.segment(fixture.headers[1:2], fixture.keys, 9), fixture.segment(fixture.headers[2:4], fixture.keys, 9), fixture.segment(fixture.headers[4:], newKeys, 10)}
	fixture.bind()
	return fixture, newKeys
}

// All six signed attempts survive. Mathematical proof flags become true while
// genesis/checkpoint/runtime/finality/fee/spending authority stays false.
func TestReceiptFinalityVerifiesMappingAndPreservesUnapprovedHistory(t *testing.T) {
	fixture := receiptFinalityTestFixture(t)
	before := []string{objectDigest(fixture.receipts.archive), objectDigest(fixture.collection), fixture.checkpoint.Hash(), objectDigest(fixture.proof)}
	result, err := VerifyReceiptFinality(context.Background(), fixture.receipts.archive, fixture.collection, fixture.checkpoint, fixture.proof)
	if err != nil {
		t.Fatal(err)
	}
	if !result.GrandpaCertificatesVerified || !result.NativeHeaderAncestryVerified || !result.NativeEvmCommitmentVerified || result.NativeFinalized.Number != 703 || result.EvmFinalized.Number != 100 ||
		len(result.Receipts.Observations.Transactions) != 6 || len(result.Receipts.Receipts) != 4 || len(result.Certificates) != 1 || result.Certificates[0].SignedWeight != 7 || result.Certificates[0].RequiredWeight != 7 {
		t.Fatalf("incomplete commitment proof: %+v", result)
	}
	if result.Admission != "unapproved_checkpoint_proof" || result.AuthorityCheckpointAuthenticated || result.GenesisAuthenticated || result.RuntimeSourceAuthenticated || result.FinalityAuthenticated || result.CanonicalReceiptsReconciled || result.ActualFeesReconciled || result.SpendingAuthorized || len(result.MissingAuthorities) != 4 {
		t.Fatal("proof invented missing authority")
	}
	for _, receipt := range result.Receipts.Receipts {
		if receipt.ActualGasFee != nil {
			t.Fatal("native proof fabricated a fee")
		}
	}
	result.NextAuthorities[0].Weight++
	if !slices.Equal(before, []string{objectDigest(fixture.receipts.archive), objectDigest(fixture.collection), fixture.checkpoint.Hash(), objectDigest(fixture.proof)}) {
		t.Fatal("verification/result mutation changed original evidence")
	}
}

// This is the causal old gap: a valid EVM collection can be paired with an
// unrelated native certificate. Recomputing every file seal cannot fix it.
func TestReceiptFinalityRejectsSuppliedMappingWithValidUnrelatedNativeQuorum(t *testing.T) {
	fixture := receiptFinalityTestFixture(t)
	fixture.headers[3] = finalityTestNativeHeader(703, fixture.headers[2].identity.Hash, finalityTestFrontier("0x"+strings.Repeat("6", 64)))
	fixture.proof.Segments = []GrandpaFinalitySegment{fixture.segment(fixture.headers[1:], fixture.keys, 9)}
	fixture.bind()
	if _, err := VerifyReceiptCollection(context.Background(), fixture.receipts.archive, fixture.collection); err != nil {
		t.Fatalf("old path must accept this internally consistent collection: %v", err)
	}
	result, err := VerifyReceiptFinality(context.Background(), fixture.receipts.archive, fixture.collection, fixture.checkpoint, fixture.proof)
	if err == nil || result != nil || !strings.Contains(err.Error(), "Frontier commitment differs") {
		t.Fatalf("unrelated native quorum was accepted: %v", err)
	}
}

// Key count cannot replace voting weight, and exactly two thirds is not quorum.
func TestReceiptFinalityRequiresWeightedStrictSupermajority(t *testing.T) {
	for _, fault := range []string{"count-majority", "exact-two-thirds", "weight-overflow", "duplicate-authority", "zero-weight"} {
		fixture := receiptFinalityTestFixture(t)
		signers := []int{0, 1}
		switch fault {
		case "count-majority":
			signers = []int{1, 2, 3}
		case "exact-two-thirds":
			fixture.checkpoint.Authorities[1].Weight = 2
		case "weight-overflow":
			fixture.checkpoint.Authorities[0].Weight = ^uint64(0)
		case "duplicate-authority":
			fixture.checkpoint.Authorities[1].PublicKey = fixture.checkpoint.Authorities[0].PublicKey
		case "zero-weight":
			fixture.checkpoint.Authorities[0].Weight = 0
		}
		fixture.proof.Segments[0].JustificationScale = finalityTestCertificate(fixture.headers[3], nil, nil, signers, fixture.keys, 17, 9)
		fixture.bind()
		if result, err := VerifyReceiptFinality(context.Background(), fixture.receipts.archive, fixture.collection, fixture.checkpoint, fixture.proof); err == nil || result != nil {
			t.Fatalf("invalid weighted set/quorum admitted: %s", fault)
		}
	}
}

// Round, set ID, message kind, signature and repeated voters are independent
// rejection causes. A new file pin does not create missing cryptographic votes.
func TestReceiptFinalityRejectsSignatureDomainAndVoterForgery(t *testing.T) {
	for _, fault := range []string{"round", "set", "signature", "duplicate-voter", "unknown-voter", "target-height"} {
		fixture := receiptFinalityTestFixture(t)
		raw, _ := hex.DecodeString(fixture.proof.Segments[0].JustificationScale[2:])
		switch fault {
		case "round":
			raw[0]++
		case "set":
			fixture.checkpoint.SetId++
		case "signature":
			raw[81] ^= 1
		case "target-height":
			raw[40]++
		case "duplicate-voter":
			raw, _ = hex.DecodeString(finalityTestCertificate(fixture.headers[3], nil, nil, []int{0, 0}, fixture.keys, 17, 9)[2:])
		case "unknown-voter":
			keys, _ := finalityTestAuthorities(71)
			raw, _ = hex.DecodeString(finalityTestCertificate(fixture.headers[3], nil, nil, []int{0, 1}, keys, 17, 9)[2:])
		}
		fixture.proof.Segments[0].JustificationScale = "0x" + hex.EncodeToString(raw)
		fixture.bind()
		if result, err := VerifyReceiptFinality(context.Background(), fixture.receipts.archive, fixture.collection, fixture.checkpoint, fixture.proof); err == nil || result != nil {
			t.Fatalf("certificate forgery admitted: %s", fault)
		}
	}
}

// The outgoing set must certify delayed enactment before successor keys count.
func TestReceiptFinalityAuthenticatesDelayedAuthorityHandoffs(t *testing.T) {
	fixture, _ := receiptFinalityRotationFixture(t)
	result, err := VerifyReceiptFinality(context.Background(), fixture.receipts.archive, fixture.collection, fixture.checkpoint, fixture.proof)
	if err != nil {
		t.Fatal(err)
	}
	if result.AuthorityTransitions != 1 || result.NextSetId != 10 || result.PendingChange != nil || len(result.Certificates) != 3 || result.Certificates[0].SetId != 9 || result.Certificates[1].Target.Number != 703 || result.Certificates[1].SetId != 9 || result.Certificates[2].SetId != 10 || result.AuthorityCheckpointAuthenticated || result.FinalityAuthenticated {
		t.Fatalf("delayed handoff differs: %+v", result)
	}
}

// Both a premature new set and a stale old set are rejected; an old-set quorum
// over a later block cannot silently skip the exact enactment certificate.
func TestReceiptFinalityRejectsPrematureStaleAndSkippedAuthorityHandoffs(t *testing.T) {
	for _, fault := range []string{"premature-keys", "premature-set", "stale-keys", "stale-set", "skipped-enactment", "forged-next-keys"} {
		fixture, newKeys := receiptFinalityRotationFixture(t)
		switch fault {
		case "premature-keys":
			fixture.proof.Segments[1] = fixture.segment(fixture.headers[2:4], newKeys, 9)
		case "premature-set":
			fixture.proof.Segments[1] = fixture.segment(fixture.headers[2:4], fixture.keys, 10)
		case "stale-keys":
			fixture.proof.Segments[2] = fixture.segment(fixture.headers[4:], fixture.keys, 10)
		case "stale-set":
			fixture.proof.Segments[2] = fixture.segment(fixture.headers[4:], newKeys, 9)
		case "skipped-enactment":
			fixture.proof.Segments = []GrandpaFinalitySegment{fixture.segment(fixture.headers[1:], fixture.keys, 9)}
		case "forged-next-keys":
			keys, _ := finalityTestAuthorities(81)
			fixture.proof.Segments[2] = fixture.segment(fixture.headers[4:], keys, 10)
		}
		if result, err := VerifyReceiptFinality(context.Background(), fixture.receipts.archive, fixture.collection, fixture.checkpoint, fixture.proof); err == nil || result != nil {
			t.Fatalf("invalid authority handoff admitted: %s", fault)
		}
	}
}

// Rolling state carries a delayed schedule across verification boundaries and
// preserves it in a detached result without approving the next checkpoint.
func TestReceiptFinalityRetainsPendingStateAndAcceptsExplicitRollingCheckpoint(t *testing.T) {
	fixture, newKeys := receiptFinalityRotationFixture(t)
	_, nextAuthorities := finalityTestAuthorities(51)
	fixture.checkpoint.HeaderScale = "0x" + hex.EncodeToString(fixture.headers[1].raw)
	fixture.checkpoint.PendingChange = &GrandpaScheduledChange{ScheduledAt: 701, EnactmentNumber: 703, Authorities: nextAuthorities}
	fixture.proof.Segments = []GrandpaFinalitySegment{fixture.segment(fixture.headers[2:4], fixture.keys, 9), fixture.segment(fixture.headers[4:], newKeys, 10)}
	fixture.bind()
	if _, err := VerifyReceiptFinality(context.Background(), fixture.receipts.archive, fixture.collection, fixture.checkpoint, fixture.proof); err != nil {
		t.Fatal(err)
	}
	fixture.checkpoint.PendingChange = nil
	fixture.bind()
	if result, err := VerifyReceiptFinality(context.Background(), fixture.receipts.archive, fixture.collection, fixture.checkpoint, fixture.proof); err == nil || result != nil {
		t.Fatal("rolling checkpoint erased its pending change")
	}
	fixture, _ = receiptFinalityRotationFixture(t)
	fixture.headers = fixture.headers[:3]
	fixture.proof.Segments = []GrandpaFinalitySegment{fixture.segment(fixture.headers[1:], fixture.keys, 9)}
	fixture.bind()
	result, err := VerifyReceiptFinality(context.Background(), fixture.receipts.archive, fixture.collection, fixture.checkpoint, fixture.proof)
	if err != nil || result.PendingChange == nil || result.PendingChange.EnactmentNumber != 703 || result.NextSetId != 9 || result.AuthorityTransitions != 0 {
		t.Fatalf("pending authority state lost: %v", err)
	}
}

// Valid descendant precommits need exact hash/number routes and an exact GHOST.
func TestReceiptFinalityVerifiesDescendantPrecommitsAndRejectsAncestryForgery(t *testing.T) {
	for _, fault := range []string{"valid", "missing", "unused", "duplicate", "false-height", "disconnected", "ghost-below", "authority-signal"} {
		fixture := receiptFinalityTestFixture(t)
		target := fixture.headers[3]
		child := finalityTestNativeHeader(704, target.identity.Hash)
		ancestry := []*finalityTestHeader{child}
		targets := []*finalityTestHeader{target, child}
		switch fault {
		case "missing":
			ancestry = nil
		case "unused":
			ancestry = append(ancestry, finalityTestNativeHeader(705, child.identity.Hash))
		case "duplicate":
			ancestry = append(ancestry, child)
		case "false-height":
			child = finalityTestNativeHeader(705, target.identity.Hash)
			ancestry, targets = []*finalityTestHeader{child}, []*finalityTestHeader{target, child}
		case "disconnected":
			child = finalityTestNativeHeader(704, "0x"+strings.Repeat("5", 64))
			ancestry, targets = []*finalityTestHeader{child}, []*finalityTestHeader{target, child}
		case "ghost-below":
			targets = []*finalityTestHeader{child, child}
		case "authority-signal":
			child = finalityTestNativeHeader(704, target.identity.Hash, finalityTestScheduled(fixture.checkpoint.Authorities, 1))
			ancestry, targets = []*finalityTestHeader{child}, []*finalityTestHeader{target, child}
		}
		fixture.proof.Segments[0].JustificationScale = finalityTestCertificate(target, targets, ancestry, []int{0, 1}, fixture.keys, 17, 9)
		result, err := VerifyReceiptFinality(context.Background(), fixture.receipts.archive, fixture.collection, fixture.checkpoint, fixture.proof)
		if fault == "valid" {
			if err != nil || result == nil {
				t.Fatalf("valid descendant proof refused: %v", err)
			}
		} else if err == nil || result != nil {
			t.Fatalf("invalid vote ancestry admitted: %s", fault)
		}
	}
}

// Unsupported authority modes and duplicate/overlapping schedules do not gain
// the routine scheduled-change interpretation merely by carrying valid votes.
func TestReceiptFinalityRefusesUnsupportedAndConflictingAuthoritySignals(t *testing.T) {
	for _, fault := range []string{"forced", "disabled", "pause", "resume", "duplicate", "overlap", "trailing"} {
		fixture := receiptFinalityTestFixture(t)
		payload := []byte{2}
		switch fault {
		case "disabled":
			payload[0] = 3
		case "pause":
			payload[0] = 4
		case "resume":
			payload[0] = 5
		}
		digests := [][]byte{finalityTestDigest("FRNK", payload), finalityTestFrontier(fixture.receipts.observations.EvmFinalized.Hash)}
		if fault == "duplicate" || fault == "overlap" || fault == "trailing" {
			digests[0] = finalityTestScheduled(fixture.checkpoint.Authorities, 3)
			if fault == "duplicate" {
				digests = append(digests, digests[0])
			}
			if fault == "trailing" {
				digests[0] = append(digests[0], 0)
			}
		}
		fixture.headers[1] = finalityTestNativeHeader(701, fixture.headers[0].identity.Hash, digests...)
		for index := 2; index < len(fixture.headers); index++ {
			nextDigests := [][]byte{finalityTestFrontier(fixture.receipts.observations.EvmFinalized.Hash)}
			if fault == "overlap" && index == 2 {
				nextDigests = append(nextDigests, finalityTestScheduled(fixture.checkpoint.Authorities, 1))
			}
			fixture.headers[index] = finalityTestNativeHeader(uint32(700+index), fixture.headers[index-1].identity.Hash, nextDigests...)
		}
		fixture.proof.Segments = []GrandpaFinalitySegment{fixture.segment(fixture.headers[1:], fixture.keys, 9)}
		fixture.bind()
		if result, err := VerifyReceiptFinality(context.Background(), fixture.receipts.archive, fixture.collection, fixture.checkpoint, fixture.proof); err == nil || result != nil {
			t.Fatalf("unsupported authority profile admitted: %s", fault)
		}
	}
}

// The native proof cannot bypass older signed transaction and receipt proofs.
func TestReceiptFinalityReplaysReceiptProofsAndBindsExactInputs(t *testing.T) {
	for _, fault := range []string{"receipt", "checkpoint", "collection", "genesis", "native-boundary", "ancestry"} {
		fixture := receiptFinalityTestFixture(t)
		switch fault {
		case "receipt":
			fixture.receipts.commitments.Receipts[0].TransactionNodes = nil
			fixture.bind()
		case "checkpoint":
			fixture.proof.CheckpointHash = "sha256:" + strings.Repeat("1", 64)
		case "collection":
			fixture.proof.CollectionHash = "sha256:" + strings.Repeat("2", 64)
		case "genesis":
			fixture.checkpoint.Genesis = "0x" + strings.Repeat("3", 64)
			fixture.bind()
		case "native-boundary":
			fixture.receipts.observations.NativeFinalized.Hash = "0x" + strings.Repeat("4", 64)
			fixture.receipts.commitments.ObservationHash = objectDigest(fixture.receipts.observations)
			fixture.collection.ContentHash = fixture.collection.hash()
			fixture.proof.CollectionHash = fixture.collection.ContentHash
		case "ancestry":
			fixture.proof.Segments[0].Headers[1] = fixture.proof.Segments[0].Headers[0]
		}
		if result, err := VerifyReceiptFinality(context.Background(), fixture.receipts.archive, fixture.collection, fixture.checkpoint, fixture.proof); err == nil || result != nil {
			t.Fatalf("unbound evidence admitted: %s", fault)
		}
	}
}

// Every public input has a finite count/byte bound; cancellation never returns
// a partial proof report even when original evidence is otherwise complete.
func TestReceiptFinalityBoundsDirectInputsAndCancellation(t *testing.T) {
	for _, fault := range []string{"certificates", "headers", "hex", "justification", "interval", "cancelled", "nil-context"} {
		fixture := receiptFinalityTestFixture(t)
		ctx := context.Background()
		switch fault {
		case "certificates":
			fixture.proof.Segments = make([]GrandpaFinalitySegment, maximumGrandpaCertificates+1)
		case "headers":
			fixture.proof.Segments[0].Headers = make([]string, maximumNativeFinalityHeaders+1)
		case "hex":
			fixture.proof.Segments[0].Headers[0] = "0x" + strings.Repeat("00", maximumNativeFinalityHeaderBytes+1)
		case "justification":
			fixture.proof.Segments[0].JustificationScale += "00"
		case "interval":
			fixture.receipts.observations.NativeFinalized.Number += maximumNativeFinalityHeaders
			fixture.receipts.commitments.ObservationHash = objectDigest(fixture.receipts.observations)
			fixture.collection.ContentHash = fixture.collection.hash()
			fixture.proof.CollectionHash = fixture.collection.ContentHash
		case "cancelled":
			cancelled, cancel := context.WithCancel(ctx)
			cancel()
			ctx = cancelled
		case "nil-context":
			ctx = nil
		}
		if result, err := VerifyReceiptFinality(ctx, fixture.receipts.archive, fixture.collection, fixture.checkpoint, fixture.proof); err == nil || result != nil {
			t.Fatalf("unbounded or cancelled input admitted: %s", fault)
		}
	}
	budget := nativeFinalityBudget{remaining: 5}
	if _, err := budget.decode("0x0102", 10); err == nil {
		t.Fatal("shared encoded byte limit was ignored")
	}
}

// Private pins are byte-level associations, while invented authority flags and
// duplicate JSON fields are refused. Loading never changes original evidence.
func TestReceiptFinalityPrivateLoadersRejectAuthorityAndCustodySubstitution(t *testing.T) {
	fixture := receiptFinalityTestFixture(t)
	directory := t.TempDir()
	if err := os.Chmod(directory, 0700); err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"checkpoint", "proof"} {
		var value any = fixture.checkpoint
		if kind == "proof" {
			value = fixture.proof
		}
		raw, _ := json.Marshal(value)
		path := filepath.Join(directory, kind+".json")
		load := func(reference FileReference) error {
			if kind == "checkpoint" {
				_, err := LoadNativeFinalityCheckpoint(context.Background(), reference)
				return err
			}
			_, err := LoadReceiptFinalityProof(context.Background(), reference)
			return err
		}
		for _, fault := range []string{"valid", "wrong-pin", "unknown-authority", "duplicate-key", "shared-mode", "symlink", "hardlink"} {
			current := slices.Clone(raw)
			if fault == "unknown-authority" {
				current = append([]byte(`{"finality_authenticated":true,`), raw[1:]...)
			}
			if fault == "duplicate-key" {
				current = append([]byte(`{"schema":"forged",`), raw[1:]...)
			}
			if err := os.WriteFile(path, current, 0600); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(path, 0600); err != nil {
				t.Fatal(err)
			}
			reference := FileReference{Path: path, Sha256: digest(current)}
			alias := path + ".alias"
			switch fault {
			case "wrong-pin":
				reference.Sha256 = "sha256:" + strings.Repeat("a", 64)
			case "shared-mode":
				if err := os.Chmod(path, 0640); err != nil {
					t.Fatal(err)
				}
			case "symlink":
				if err := os.Symlink(path, alias); err != nil {
					t.Fatal(err)
				}
				reference.Path = alias
			case "hardlink":
				if err := os.Link(path, alias); err != nil {
					t.Fatal(err)
				}
			}
			err := load(reference)
			if fault == "valid" {
				if err != nil {
					t.Fatal(err)
				}
			} else if err == nil {
				t.Fatalf("%s loader accepted %s", kind, fault)
			}
			if err := os.Remove(alias); err != nil && !errors.Is(err, os.ErrNotExist) {
				t.Fatal(err)
			}
			retained, err := os.ReadFile(path)
			if err != nil || !bytes.Equal(retained, current) {
				t.Fatal("loader changed input bytes")
			}
		}
	}
}
