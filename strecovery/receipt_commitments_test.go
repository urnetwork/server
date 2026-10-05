// Real signed transactions and independently constructed indexed tries expose
// fabricated RPC receipt fields without a node, database or timing dependency.
package strecovery

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/ethdb/memorydb"
	"github.com/ethereum/go-ethereum/rlp"
	"github.com/ethereum/go-ethereum/trie"
)

// One non-archived transaction separates archived inclusions, forcing gas
// derivation to use the actual preceding receipt rather than census ordering.
type receiptCommitmentFixture struct {
	archive      *Archive
	observations *ReceiptObservations
	commitments  *ReceiptCommitments
	transactions []*types.Transaction
	receipts     []*types.Receipt
	headers      []*types.Header
	hashIndexes  map[string]uint64
	receiptTrie  *trie.Trie
}

// All three historical attempt kinds use the same independently signed census.
func receiptCommitmentTestFixture(t testing.TB, winner int) *receiptCommitmentFixture {
	t.Helper()
	return receiptCommitmentTestFixtureOnChain(t, winner, 31337)
}

func receiptCommitmentTestFixtureOnChain(t testing.TB, winner int, chainId uint64) *receiptCommitmentFixture {
	t.Helper()
	archive, observations := receiptTestFixtureOnChain(t, winner, chainId)
	fixture := &receiptCommitmentFixture{archive: archive, observations: observations, hashIndexes: map[string]uint64{},
		commitments: &ReceiptCommitments{Schema: ReceiptCommitmentsSchema, CensusHash: archive.CensusHash}}
	var cumulative uint64
	for i := range observations.Receipts {
		observation := &observations.Receipts[i]
		if observation.Outcome != "found" {
			continue
		}
		if len(fixture.transactions) == 1 {
			foreign, _ := censusTestTransactionOnChain(t, censusTestKey(t, 3), 1, "execution", false, 100, chainId)
			cumulative += 23000
			fixture.transactions = append(fixture.transactions, foreign)
			fixture.receipts = append(fixture.receipts, &types.Receipt{Type: foreign.Type(), Status: types.ReceiptStatusSuccessful, CumulativeGasUsed: cumulative, Logs: []*types.Log{}})
		}
		tx, _, err := decodeTransaction(archive.Transactions[i].Raw, archive.Selection.ChainId)
		if err != nil {
			t.Fatal(err)
		}
		index := uint64(len(fixture.transactions))
		gasUsed := min(tx.Gas(), uint64(25000))
		cumulative += gasUsed
		status := uint64(types.ReceiptStatusSuccessful)
		if tx.Gas() == 21000 {
			status = types.ReceiptStatusFailed
		}
		fixture.hashIndexes[observation.Hash] = index
		fixture.transactions = append(fixture.transactions, tx)
		fixture.receipts = append(fixture.receipts, &types.Receipt{Type: tx.Type(), Status: status, CumulativeGasUsed: cumulative, Logs: []*types.Log{}})
		observation.Receipt.TransactionIndex = &index
		observation.Receipt.Status = &status
		observation.Receipt.GasUsed = gasUsed
		observation.Receipt.CumulativeGasUsed = cumulative
	}
	for number := uint64(90); number <= 100; number++ {
		fixture.headers = append(fixture.headers, &types.Header{Number: new(big.Int).SetUint64(number), Difficulty: new(big.Int), GasLimit: 30000000,
			ParentHash: common.HexToHash("0x" + strings.Repeat("1", 64)), TxHash: types.EmptyTxsHash, ReceiptHash: types.EmptyReceiptsHash,
			UncleHash: types.EmptyUncleHash, Root: common.HexToHash("0x" + strings.Repeat("2", 64)), Time: 1700000000123 + number*12000})
	}
	fixture.seal(t)
	return fixture
}

// The fixture builds trie paths directly with RLP indices. It does not invoke
// production proof decoding or observation extraction to construct the oracle.
func (self *receiptCommitmentFixture) seal(t testing.TB) {
	t.Helper()
	transactionTrie, receiptTrie := trie.NewEmpty(nil), trie.NewEmpty(nil)
	for index, transaction := range self.transactions {
		key, err := rlp.EncodeToBytes(uint64(index))
		if err != nil {
			t.Fatal(err)
		}
		raw, err := transaction.MarshalBinary()
		if err != nil {
			t.Fatal(err)
		}
		if err := transactionTrie.Update(key, raw); err != nil {
			t.Fatal(err)
		}
		raw, err = self.receipts[index].MarshalBinary()
		if err != nil {
			t.Fatal(err)
		}
		if err := receiptTrie.Update(key, raw); err != nil {
			t.Fatal(err)
		}
	}
	self.receiptTrie = receiptTrie
	self.headers[0].TxHash, self.headers[0].ReceiptHash = transactionTrie.Hash(), receiptTrie.Hash()
	self.headers[0].GasUsed = self.receipts[len(self.receipts)-1].CumulativeGasUsed
	for index := 1; index < len(self.headers); index++ {
		self.headers[index].ParentHash = self.headers[index-1].Hash()
	}
	self.commitments.Receipts = nil
	for _, observation := range self.observations.Receipts {
		if observation.Outcome != "found" {
			continue
		}
		index := self.hashIndexes[observation.Hash]
		proof := ReceiptInclusionProof{Hash: observation.Hash, TransactionNodes: receiptCommitmentTestProof(t, transactionTrie, index), ReceiptNodes: receiptCommitmentTestProof(t, receiptTrie, index)}
		if index > 0 {
			proof.PreviousReceiptNodes = receiptCommitmentTestProof(t, receiptTrie, index-1)
		}
		self.commitments.Receipts = append(self.commitments.Receipts, proof)
	}
	self.encodeHeaders(t)
}

// Header identities and observation pins follow raw headers, preserving their
// millisecond timestamps without adding the RPC renderer's base fee field.
func (self *receiptCommitmentFixture) encodeHeaders(t testing.TB) {
	t.Helper()
	self.commitments.Headers = nil
	for _, header := range self.headers {
		raw, err := rlp.EncodeToBytes(header)
		if err != nil {
			t.Fatal(err)
		}
		self.commitments.Headers = append(self.commitments.Headers, "0x"+hex.EncodeToString(raw))
	}
	first, last := self.headers[0], self.headers[len(self.headers)-1]
	self.observations.EvmFinalized.Hash = last.Hash().Hex()
	for i := range self.observations.Accounts {
		self.observations.Accounts[i].BlockHash = last.Hash().Hex()
	}
	self.observations.Blocks[0].Hash, self.observations.Blocks[0].GasUsed = first.Hash().Hex(), first.GasUsed
	self.observations.Blocks[1].Hash = last.Hash().Hex()
	for i := range self.observations.Receipts {
		if receipt := self.observations.Receipts[i].Receipt; receipt != nil {
			receipt.BlockHash = first.Hash().Hex()
		}
	}
	self.commitments.ObservationHash = objectDigest(self.observations)
}

// Proof nodes are exported by an independent trie builder in deterministic
// hash order. No claimed key is passed to the production proof database.
func receiptCommitmentTestProof(t testing.TB, tree *trie.Trie, index uint64) []string {
	t.Helper()
	key, err := rlp.EncodeToBytes(index)
	if err != nil {
		t.Fatal(err)
	}
	db := memorydb.New()
	defer db.Close()
	if err := tree.Prove(key, db); err != nil {
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
	return nodes
}

// All attempt kinds retain their real bytes and gas once. A reverted
// cancellation remains charged conditionally, and all actual fees stay unknown.
func TestReceiptCommitmentsVerifyAllAttemptKindsAndCommittedGas(t *testing.T) {
	for _, winner := range []int{1, 2, 3} {
		fixture := receiptCommitmentTestFixture(t, winner)
		beforeArchive, beforeObservations, beforeCommitments := objectDigest(fixture.archive), objectDigest(fixture.observations), objectDigest(fixture.commitments)
		result, err := ReconcileReceiptCommitments(context.Background(), fixture.archive, fixture.observations, fixture.commitments)
		if err != nil {
			t.Fatal(err)
		}
		if !result.EvmHeaderAncestryVerified || !result.FoundReceiptCommitmentsVerified || result.HeaderCount != 11 || len(result.Receipts) != 4 ||
			!result.Observations.ObservationAccountingComplete || len(result.Observations.Transactions) != 6 || len(result.Observations.Nonces) != 4 || result.CommitmentHash != beforeCommitments {
			t.Fatalf("committed census lost its complete history: %+v", result)
		}
		var gas uint64
		for _, receipt := range result.Receipts {
			gas += receipt.GasUsed
			if receipt.ActualGasFee != nil || receipt.ReceiptBytesHash == "" || receipt.TransactionIndex == 2 && receipt.PreviousCumulativeGasUsed != min(fixture.transactions[0].Gas(), uint64(25000))+23000 {
				t.Fatalf("receipt ignored the non-archived predecessor or fabricated fees: %+v", receipt)
			}
		}
		expectedGas := uint64(100000)
		if winner == 3 {
			expectedGas = 96000
		}
		if gas != expectedGas || result.FinalityAuthenticated || result.CanonicalReceiptsReconciled || result.AccountNoncesAuthenticated || result.ActualFeesReconciled || result.SpendingAuthorized ||
			result.Observations.FinalityAuthenticated || result.Observations.ActualFeesReconciled || result.TrustRequirement == "" || result.FeeRequirement == "" {
			t.Fatal("byte verification acquired finality/fee authority or changed gas")
		}
		if objectDigest(fixture.archive) != beforeArchive || objectDigest(fixture.observations) != beforeObservations || objectDigest(fixture.commitments) != beforeCommitments {
			t.Fatal("commitment verification mutated retained evidence")
		}
	}
}

// This reproduces the precise old gap: forged gas/status remain internally
// consistent and are accepted by the conditional join, but contradict the trie.
func TestReceiptCommitmentsRejectPlausibleForgedGasAndOutcome(t *testing.T) {
	for _, fault := range []string{"gas", "status"} {
		fixture := receiptCommitmentTestFixture(t, 1)
		for i := range fixture.observations.Receipts {
			if receipt := fixture.observations.Receipts[i].Receipt; receipt != nil {
				if fault == "gas" {
					receipt.GasUsed--
				} else {
					*receipt.Status = types.ReceiptStatusFailed
				}
				break
			}
		}
		fixture.commitments.ObservationHash = objectDigest(fixture.observations)
		conditional, err := ReconcileReceipts(context.Background(), fixture.archive, fixture.observations)
		if err != nil || !conditional.ObservationAccountingComplete {
			t.Fatalf("causal forged observation was not accepted by the old conditional path: %s %v", fault, err)
		}
		result, err := ReconcileReceiptCommitments(context.Background(), fixture.archive, fixture.observations, fixture.commitments)
		if err == nil || result != nil || !strings.Contains(err.Error(), "gas or outcome differs") {
			t.Fatalf("forged %s escaped its consensus commitment: result%v error%v", fault, result, err)
		}
	}
}

// A genuine proof for a different transaction at the correct slot must not
// authenticate an archived signature merely because its supplied hash matches.
func TestReceiptCommitmentsRequireArchivedBytesAtReceiptPosition(t *testing.T) {
	fixture := receiptCommitmentTestFixture(t, 1)
	archiveIndex := receiptTestIndex(t, fixture.archive, "operator-a", 7, 1)
	foreignIndex := receiptTestIndex(t, fixture.archive, "operator-a", 7, 2)
	index := fixture.hashIndexes[fixture.archive.Transactions[archiveIndex].Hash]
	replacement, _, err := decodeTransaction(fixture.archive.Transactions[foreignIndex].Raw, fixture.archive.Selection.ChainId)
	if err != nil {
		t.Fatal(err)
	}
	fixture.transactions[index] = replacement
	fixture.seal(t)
	result, err := ReconcileReceiptCommitments(context.Background(), fixture.archive, fixture.observations, fixture.commitments)
	if err == nil || result != nil || !strings.Contains(err.Error(), "does not contain the archived signed transaction") {
		t.Fatalf("another retained candidate supplied transaction identity: result%v error%v", result, err)
	}
}

// The predecessor must be proven at exactly index-1 under the same receipt
// root. Omitting it or borrowing a nearby archived receipt cannot prove gas.
func TestReceiptCommitmentsRequireExactPredecessorProof(t *testing.T) {
	for _, fault := range []string{"missing", "wrong-index", "unexpected-at-zero"} {
		fixture := receiptCommitmentTestFixture(t, 1)
		for i := range fixture.commitments.Receipts {
			proof := &fixture.commitments.Receipts[i]
			index := fixture.hashIndexes[proof.Hash]
			if fault == "unexpected-at-zero" && index == 0 {
				proof.PreviousReceiptNodes = receiptCommitmentTestProof(t, fixture.receiptTrie, 0)
			}
			if index == 2 {
				if fault == "missing" {
					proof.PreviousReceiptNodes = nil
				} else if fault == "wrong-index" {
					proof.PreviousReceiptNodes = receiptCommitmentTestProof(t, fixture.receiptTrie, 0)
				}
			}
		}
		result, err := ReconcileReceiptCommitments(context.Background(), fixture.archive, fixture.observations, fixture.commitments)
		if err == nil || result != nil || !strings.Contains(err.Error(), "predecessor") {
			t.Fatalf("invalid predecessor %s authenticated gas: result%v error%v", fault, result, err)
		}
	}
}

// A tampered proof cannot supply its original content-addressed key. Changing
// receipt bytes without changing the anchored header therefore fails membership.
func TestReceiptCommitmentsRejectCorruptReceiptTrieNodes(t *testing.T) {
	fixture := receiptCommitmentTestFixture(t, 1)
	for index, encoded := range fixture.commitments.Receipts[0].ReceiptNodes {
		raw, err := hex.DecodeString(encoded[2:])
		if err != nil {
			t.Fatal(err)
		}
		raw[len(raw)-1] ^= 1
		fixture.commitments.Receipts[0].ReceiptNodes[index] = "0x" + hex.EncodeToString(raw)
	}
	result, err := ReconcileReceiptCommitments(context.Background(), fixture.archive, fixture.observations, fixture.commitments)
	if err == nil || result != nil || !strings.Contains(err.Error(), "receipt commitment inclusion") {
		t.Fatalf("modified consensus bytes passed receipt proof: result%v error%v", result, err)
	}
}

// Both endpoints still hash correctly after an internally disconnected branch
// is supplied. Only verifying every parent link detects this root cause.
func TestReceiptCommitmentsRejectDisconnectedHeaderAncestry(t *testing.T) {
	fixture := receiptCommitmentTestFixture(t, 1)
	fixture.headers[1].ParentHash = common.HexToHash("0x" + strings.Repeat("3", 64))
	for index := 2; index < len(fixture.headers); index++ {
		fixture.headers[index].ParentHash = fixture.headers[index-1].Hash()
	}
	fixture.encodeHeaders(t)
	result, err := ReconcileReceiptCommitments(context.Background(), fixture.archive, fixture.observations, fixture.commitments)
	if err == nil || result != nil || !strings.Contains(err.Error(), "header ancestry differs") {
		t.Fatalf("disconnected inclusion became an ancestor: result%v error%v", result, err)
	}
}

// Raw header profile, exact endpoint, canonical projections and full height
// coverage are independent guards; no sparse lookup establishes ancestry.
func TestReceiptCommitmentsRejectHeaderSubstitutionAndUnsupportedProfile(t *testing.T) {
	for _, fault := range []string{"boundary", "profile", "number", "gas", "omitted"} {
		fixture := receiptCommitmentTestFixture(t, 1)
		switch fault {
		case "boundary":
			hash := "0x" + strings.Repeat("4", 64)
			fixture.observations.EvmFinalized.Hash = hash
			fixture.observations.Blocks[1].Hash = hash
			for i := range fixture.observations.Accounts {
				fixture.observations.Accounts[i].BlockHash = hash
			}
		case "profile":
			fixture.headers[3].BaseFee = big.NewInt(90)
			fixture.seal(t)
		case "number":
			fixture.headers[3].Number = big.NewInt(92)
			fixture.seal(t)
		case "gas":
			fixture.observations.Blocks[0].GasUsed++
		case "omitted":
			fixture.commitments.Headers = append(fixture.commitments.Headers[:3], fixture.commitments.Headers[4:]...)
		}
		fixture.commitments.ObservationHash = objectDigest(fixture.observations)
		result, err := ReconcileReceiptCommitments(context.Background(), fixture.archive, fixture.observations, fixture.commitments)
		if err == nil || result != nil {
			t.Fatalf("header fault %s passed: result%v error%v", fault, result, err)
		}
	}
}

// A valid inclusion cannot resolve an unread sibling or manufacture a runtime
// base fee. Even a completely verified receipt retains an unknown actual fee.
func TestReceiptCommitmentsPreserveUnavailableSiblingsAndUnknownFees(t *testing.T) {
	for _, fault := range []string{"unavailable-sibling", "missing-base-fee"} {
		fixture := receiptCommitmentTestFixture(t, 2)
		if fault == "unavailable-sibling" {
			index := receiptTestIndex(t, fixture.archive, "operator-a", 7, 1)
			fixture.observations.Receipts[index].Outcome = "unavailable"
		} else {
			fixture.observations.Blocks[0].BaseFeePerGas = nil
		}
		fixture.commitments.ObservationHash = objectDigest(fixture.observations)
		result, err := ReconcileReceiptCommitments(context.Background(), fixture.archive, fixture.observations, fixture.commitments)
		if err != nil {
			t.Fatal(err)
		}
		joined := receiptTestNonce(t, result.Observations, "operator-a", 7)
		if !result.FoundReceiptCommitmentsVerified || result.Observations.ObservationAccountingComplete || joined.AccountedGasFee != nil || result.ActualFeesReconciled || result.FinalityAuthenticated {
			t.Fatalf("verified bytes hid %s: %+v", fault, result)
		}
		for _, receipt := range result.Receipts {
			if receipt.ActualGasFee != nil {
				t.Fatal("unproven Frontier execution price became an actual charge")
			}
		}
	}
}

// Every found hash has exactly one proof. The direct API shares the file
// adapter's finite shape/byte bounds and refuses contradictory context seals.
func TestReceiptCommitmentsRejectIncompleteContextCoverageAndBounds(t *testing.T) {
	for _, fault := range []string{"nil", "schema", "census", "observations", "missing", "repeated", "extra", "header-count", "node-count", "node-size", "hex"} {
		fixture := receiptCommitmentTestFixture(t, 1)
		switch fault {
		case "nil":
			fixture.commitments = nil
		case "schema":
			fixture.commitments.Schema = "unknown"
		case "census":
			fixture.commitments.CensusHash = digest([]byte("another census"))
		case "observations":
			fixture.commitments.ObservationHash = digest([]byte("another observation"))
		case "missing":
			fixture.commitments.Receipts = fixture.commitments.Receipts[:3]
		case "repeated":
			fixture.commitments.Receipts[1] = fixture.commitments.Receipts[0]
		case "extra":
			proof := fixture.commitments.Receipts[0]
			index := receiptTestIndex(t, fixture.archive, "operator-a", 7, 2)
			proof.Hash = fixture.archive.Transactions[index].Hash
			fixture.commitments.Receipts = append(fixture.commitments.Receipts, proof)
		case "header-count":
			fixture.commitments.Headers = make([]string, maximumCommitmentHeaders+1)
		case "node-count":
			fixture.commitments.Receipts[0].ReceiptNodes = make([]string, maximumCommitmentProofNodes+1)
		case "node-size":
			fixture.commitments.Receipts[0].ReceiptNodes[0] = "0x" + strings.Repeat("00", maximumCommitmentNodeBytes+1)
		case "hex":
			fixture.commitments.Headers[0] = "0xzz"
		}
		result, err := ReconcileReceiptCommitments(context.Background(), fixture.archive, fixture.observations, fixture.commitments)
		if err == nil || result != nil {
			t.Fatalf("commitment fault %s returned a report", fault)
		}
	}
}

// One context owns all work and no cancellation returns a verified prefix.
func TestReceiptCommitmentsCanceledContextReturnsNoReport(t *testing.T) {
	fixture := receiptCommitmentTestFixture(t, 1)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	result, err := ReconcileReceiptCommitments(ctx, fixture.archive, fixture.observations, fixture.commitments)
	if err == nil || result != nil {
		t.Fatal("canceled commitment verification published a report")
	}
}

// Individually bounded nodes cannot reset the operation's shared byte budget.
// Reusing the same backing string keeps this resource regression small itself.
func TestReceiptCommitmentsEnforceSharedProofByteBudget(t *testing.T) {
	fixture := receiptCommitmentTestFixture(t, 1)
	node := "0x" + strings.Repeat("00", maximumCommitmentNodeBytes)
	fixture.commitments.Receipts[0].ReceiptNodes = nil
	for range 17 {
		fixture.commitments.Receipts[0].ReceiptNodes = append(fixture.commitments.Receipts[0].ReceiptNodes, node)
	}
	result, err := ReconcileReceiptCommitments(context.Background(), fixture.archive, fixture.observations, fixture.commitments)
	if err == nil || result != nil || !strings.Contains(err.Error(), "shared byte bound") {
		t.Fatalf("per-node bounds bypassed the total proof budget: result%v error%v", result, err)
	}
}

// Private evidence pins cover exact file bytes; neither duplicate keys nor an
// added approval boolean can bypass the fixed commitment wire contract.
func TestReceiptCommitmentsPinnedFilesRejectTamperingAndAuthorityClaims(t *testing.T) {
	fixture := receiptCommitmentTestFixture(t, 1)
	raw, err := json.Marshal(fixture.commitments)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(censusTestDir(t), "commitments.json")
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	reference := FileReference{Path: path, Sha256: digest(raw)}
	loaded, err := LoadReceiptCommitments(context.Background(), reference)
	if err != nil || objectDigest(loaded) != objectDigest(fixture.commitments) {
		t.Fatalf("exact private commitment bytes failed: %v", err)
	}
	for _, suffix := range []string{`,"finality_authenticated":true}`, `,"schema":"unknown"}`, `,"Schema":"unknown"}`} {
		changed := append(append([]byte{}, raw[:len(raw)-1]...), []byte(suffix)...)
		if err := os.WriteFile(path, changed, 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadReceiptCommitments(context.Background(), reference); err == nil {
			t.Fatal("changed file passed its original byte pin")
		}
		repinned := FileReference{Path: path, Sha256: digest(changed)}
		if _, err := LoadReceiptCommitments(context.Background(), repinned); err == nil {
			t.Fatal("repinned malformed or authority-bearing input passed strict decoding")
		}
	}
}
