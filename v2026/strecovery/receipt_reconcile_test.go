// Synthetic observation sets force nonce winners, missing siblings and
// contradictory inclusions without a database, RPC endpoint or clock schedule.
package strecovery

import (
	"context"
	"encoding/json"
	"math/big"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
)

// Four nonce groups include an original/replacement/cancellation trio, terminal
// database generations, duplicate store custody and one store-only signature.
func receiptTestFixture(t testing.TB, winner int) (*Archive, *ReceiptObservations) {
	t.Helper()
	return receiptTestFixtureOnChain(t, winner, 31337)
}

func receiptTestFixtureOnChain(t testing.TB, winner int, chainId uint64) (*Archive, *ReceiptObservations) {
	t.Helper()
	config, reader := censusTestFixtureOnChain(t, chainId)
	archive, err := Collect(context.Background(), config, reader)
	if err != nil {
		t.Fatal(err)
	}
	return archive, receiptTestObservations(t, archive, winner)
}

// The native and EVM heads deliberately have different numbers and hashes.
// Every quantity is synthetic; the evidence hash does not pretend to be proof.
func receiptTestObservations(t testing.TB, archive *Archive, winner int) *ReceiptObservations {
	t.Helper()
	baseFee := "90"
	boundaryHash, inclusionHash := "0x"+strings.Repeat("d", 64), "0x"+strings.Repeat("e", 64)
	observations := &ReceiptObservations{Schema: ReceiptObservationsSchema, CensusHash: archive.CensusHash, ChainId: archive.Selection.ChainId, Genesis: archive.Selection.Genesis,
		Source: "synthetic-observer", NativeFinalized: ObservedBlockIdentity{Number: 777, Hash: "0x" + strings.Repeat("f", 64)},
		EvmFinalized: ObservedBlockIdentity{Number: 100, Hash: boundaryHash}, MappingEvidenceHash: digest([]byte("synthetic unverified mapping evidence")),
		Blocks: []ObservedCanonicalBlock{{Number: 90, Hash: inclusionHash, GasUsed: 200000, GasLimit: 30000000, BaseFeePerGas: &baseFee},
			{Number: 100, Hash: boundaryHash, GasUsed: 0, GasLimit: 30000000, BaseFeePerGas: &baseFee}},
		Accounts: []ObservedAccount{}, Receipts: []ReceiptObservation{}}
	for _, role := range archive.Selection.Roles {
		nonce := role.NextNonce
		observations.Accounts = append(observations.Accounts, ObservedAccount{Role: role.Id, Address: role.Address, BlockHash: boundaryHash, Outcome: "available", Nonce: &nonce})
	}
	for index, tx := range archive.Transactions {
		selected := true
		if tx.Role == "operator-a" && tx.Nonce == 7 {
			selected = false
			for _, origin := range tx.Origins {
				selected = selected || origin.Attempt == winner
			}
		}
		observation := ReceiptObservation{Hash: tx.Hash, Outcome: "not-found"}
		if selected {
			observation.Outcome, observation.Receipt = "found", receiptTestFoundOnChain(t, tx, uint64(index), archive.Selection.ChainId)
		}
		observations.Receipts = append(observations.Receipts, observation)
	}
	return observations
}

// Real signed fields determine the receipt price; declared maximum fees never
// substitute for gas used. Index-derived cumulative gas avoids shared slots.
func receiptTestFound(t testing.TB, transaction Transaction, index uint64) *ObservedReceipt {
	t.Helper()
	return receiptTestFoundOnChain(t, transaction, index, 31337)
}

func receiptTestFoundOnChain(t testing.TB, transaction Transaction, index, chainId uint64) *ObservedReceipt {
	t.Helper()
	tx, _, err := decodeTransaction(transaction.Raw, chainId)
	if err != nil {
		t.Fatal(err)
	}
	gas := uint64(25000)
	if gas > tx.Gas() {
		gas = tx.Gas()
	}
	price := tx.GasPrice().String()
	if tx.Type() == types.DynamicFeeTxType {
		price = "100"
	}
	status := uint64(types.ReceiptStatusSuccessful)
	kind := tx.Type()
	return &ObservedReceipt{TransactionHash: transaction.Hash, Type: &kind, Status: &status, BlockNumber: 90, BlockHash: "0x" + strings.Repeat("e", 64),
		TransactionIndex: &index, GasUsed: gas, CumulativeGasUsed: (index + 1) * 30000, EffectiveGasPrice: price}
}

// Tests select immutable census identities independently of hash sort order.
func receiptTestIndex(t testing.TB, archive *Archive, role string, nonce uint64, attempt int) int {
	t.Helper()
	for index, tx := range archive.Transactions {
		if tx.Role != role || tx.Nonce != nonce {
			continue
		}
		if attempt == 0 {
			return index
		}
		for _, origin := range tx.Origins {
			if origin.Attempt == attempt {
				return index
			}
		}
	}
	t.Fatal("synthetic transaction selection absent")
	return -1
}

// Nonce outcomes remain explicit even when no candidate can be accounted.
func receiptTestNonce(t testing.TB, result *ReceiptReconciliation, role string, nonce uint64) NonceReconciliation {
	t.Helper()
	for _, entry := range result.Nonces {
		if entry.Role == role && entry.Nonce == nonce {
			return entry
		}
	}
	t.Fatal("synthetic nonce result absent")
	return NonceReconciliation{}
}

// Every possible historical winner, including a reverted cancellation, pays
// exactly its observed fee once. Duplicate provenance contributes no extra fee.
func TestReceiptReconciliationAccountsEveryAttemptKindOnce(t *testing.T) {
	for _, test := range []struct {
		winner int
		revert bool
		fee    string
	}{{winner: 1, fee: "5250000"}, {winner: 2, fee: "5250000"}, {winner: 3, fee: "5900000"}, {winner: 1, revert: true, fee: "5250000"}, {winner: 3, revert: true, fee: "5900000"}} {
		archive, observations := receiptTestFixture(t, test.winner)
		winnerIndex := receiptTestIndex(t, archive, "operator-a", 7, test.winner)
		if test.revert {
			*observations.Receipts[winnerIndex].Receipt.Status = types.ReceiptStatusFailed
		}
		beforeArchive, beforeObservations := objectDigest(archive), objectDigest(observations)
		result, err := ReconcileReceipts(context.Background(), archive, observations)
		if err != nil {
			t.Fatal(err)
		}
		if !result.ObservationAccountingComplete || len(result.Transactions) != 6 || len(result.Nonces) != 4 || len(result.Unsigned) != 1 || result.OpaqueNativeFiles != 1 ||
			result.Fees[0].ObservedFinalizedGasFee != test.fee || result.Fees[1].ObservedFinalizedGasFee != "5000000" || result.Fees[0].ResolvedNonces != 2 {
			t.Fatalf("winner %d revert %t lost custody or charged fee envelopes: %+v", test.winner, test.revert, result)
		}
		if result.FinalityAuthenticated || result.CanonicalReceiptsReconciled || result.ActualFeesReconciled || result.SpendingAuthorized || result.TrustRequirement == "" {
			t.Fatal("unverified offline observations became canonical finality or spending authority")
		}
		if test.revert && result.Fees[0].RevertedTransactions != 1 || !test.revert && result.Fees[0].RevertedTransactions != 0 {
			t.Fatal("failed execution was discarded or labeled successful")
		}
		for index, entry := range result.Transactions {
			if len(entry.Origins) != len(archive.Transactions[index].Origins) || entry.AccountedGasFee == nil {
				t.Fatal("provenance or conditional fee was lost")
			}
			if entry.Role == "operator-a" && entry.Nonce == 7 && index != winnerIndex && *entry.AccountedGasFee != "0" {
				t.Fatal("same-nonce alternative was charged twice")
			}
		}
		if objectDigest(archive) != beforeArchive || objectDigest(observations) != beforeObservations {
			t.Fatal("read-only reconciliation mutated original evidence")
		}
	}
}

// A finalized database label and an advanced account nonce supply no receipt.
// Missing originals remain unknown consumers, never zero actual expenditure.
func TestReceiptReconciliationNeverUsesDatabaseStatusOrNonceAsReceipt(t *testing.T) {
	archive, observations := receiptTestFixture(t, 1)
	for index := range observations.Receipts {
		observations.Receipts[index].Outcome, observations.Receipts[index].Receipt = "not-found", nil
	}
	result, err := ReconcileReceipts(context.Background(), archive, observations)
	if err != nil {
		t.Fatal(err)
	}
	if result.ObservationAccountingComplete || result.Fees[0].ObservedFinalizedGasFee != "0" || result.Fees[0].UnresolvedNonces != 2 {
		t.Fatal("terminal database status manufactured an actual fee")
	}
	for _, nonce := range result.Nonces {
		if nonce.State != "unknown-nonce-consumer" || nonce.AccountedGasFee != nil || nonce.WinnerHash != "" {
			t.Fatalf("advanced nonce manufactured a receipt: %+v", nonce)
		}
	}
}

// A winner cannot hide a failed read or malformed sibling, whichever historical
// attempt won. This is the causal guard against first-valid-receipt accounting.
func TestReceiptReconciliationUnresolvedSiblingWithholdsWinnerAccounting(t *testing.T) {
	for _, winner := range []int{1, 2, 3} {
		for _, fault := range []string{"unavailable", "transaction-hash", "orphan-price"} {
			archive, observations := receiptTestFixture(t, winner)
			sibling := receiptTestIndex(t, archive, "operator-a", 7, winner%3+1)
			observations.Receipts[sibling].Outcome = "unavailable"
			if fault != "unavailable" {
				observations.Receipts[sibling].Outcome = "found"
				observations.Receipts[sibling].Receipt = receiptTestFound(t, archive.Transactions[sibling], uint64(sibling))
				if fault == "transaction-hash" {
					observations.Receipts[sibling].Receipt.TransactionHash = "0x" + strings.Repeat("9", 64)
				} else {
					observations.Receipts[sibling].Receipt.BlockHash = "0x" + strings.Repeat("9", 64)
					observations.Receipts[sibling].Receipt.EffectiveGasPrice = ""
				}
			}
			result, err := ReconcileReceipts(context.Background(), archive, observations)
			if err != nil {
				t.Fatal(err)
			}
			joined := receiptTestNonce(t, result, "operator-a", 7)
			if joined.State != "unresolved-observations" || joined.WinnerHash != "" || result.ObservationAccountingComplete || result.Fees[0].ObservedFinalizedGasFee != "2750000" || len(joined.TransactionHashes) != 3 {
				t.Fatalf("winner %d hid unread sibling: %+v", winner, joined)
			}
			for _, entry := range result.Transactions {
				if entry.Role == "operator-a" && entry.Nonce == 7 && entry.AccountedGasFee != nil {
					t.Fatal("incomplete receipt census contributed a fee")
				}
			}
		}
	}
}

// Hash, type, failed-status presence, canonical block identity and gas fields
// are checked before any candidate can supply a conditional actual fee.
func TestReceiptReconciliationRejectsMalformedReceiptIdentityAndGas(t *testing.T) {
	for _, test := range []struct {
		name   string
		change func(*ObservedReceipt)
	}{
		{name: "transaction hash", change: func(r *ObservedReceipt) { r.TransactionHash = "0x" + strings.Repeat("9", 64) }},
		{name: "type", change: func(r *ObservedReceipt) { *r.Type = 2 }},
		{name: "absent type", change: func(r *ObservedReceipt) { r.Type = nil }},
		{name: "absent index", change: func(r *ObservedReceipt) { r.TransactionIndex = nil }},
		{name: "absent status", change: func(r *ObservedReceipt) { r.Status = nil }},
		{name: "invalid status", change: func(r *ObservedReceipt) { *r.Status = 2 }},
		{name: "zero block", change: func(r *ObservedReceipt) { r.BlockNumber = 0 }},
		{name: "zero block hash", change: func(r *ObservedReceipt) { r.BlockHash = "0x" + strings.Repeat("0", 64) }},
		{name: "zero gas", change: func(r *ObservedReceipt) { r.GasUsed = 0 }},
		{name: "excess gas", change: func(r *ObservedReceipt) { r.GasUsed = 30001 }},
		{name: "cumulative below gas", change: func(r *ObservedReceipt) { r.CumulativeGasUsed = r.GasUsed - 1 }},
		{name: "cumulative beyond block", change: func(r *ObservedReceipt) { r.CumulativeGasUsed = 200001 }},
		{name: "missing price", change: func(r *ObservedReceipt) { r.EffectiveGasPrice = "" }},
		{name: "negative price", change: func(r *ObservedReceipt) { r.EffectiveGasPrice = "-1" }},
		{name: "ambiguous price", change: func(r *ObservedReceipt) { r.EffectiveGasPrice = "0100" }},
		{name: "wrong legacy price", change: func(r *ObservedReceipt) { r.EffectiveGasPrice = "99" }},
	} {
		archive, observations := receiptTestFixture(t, 1)
		index := receiptTestIndex(t, archive, "operator-a", 7, 1)
		test.change(observations.Receipts[index].Receipt)
		result, err := ReconcileReceipts(context.Background(), archive, observations)
		if err != nil {
			t.Fatalf("%s should remain bounded unresolved evidence: %v", test.name, err)
		}
		entry := result.Transactions[index]
		if entry.ObservationState != "invalid-receipt" || entry.ObservedGasFee != nil || entry.AccountedGasFee != nil || entry.NonceState != "unresolved-observations" {
			t.Fatalf("%s accepted malformed receipt: %+v", test.name, entry)
		}
	}
}

// The signed fee cap is only an upper bound: base fee plus priority fee, capped
// by the signature, must equal the observed dynamic effective price exactly.
func TestReceiptReconciliationAuthenticatesDynamicEffectiveGasPrice(t *testing.T) {
	for _, test := range []struct {
		base  string
		price string
		valid bool
	}{{base: "90", price: "100", valid: true}, {base: "195", price: "200", valid: true}, {base: "200", price: "200", valid: true},
		{base: "201", price: "200"}, {base: "90", price: "200"}, {base: "90", price: "99"}, {base: "", price: "100"}} {
		archive, observations := receiptTestFixture(t, 2)
		index := receiptTestIndex(t, archive, "operator-a", 7, 2)
		observations.Blocks[0].BaseFeePerGas = &test.base
		if test.base == "" {
			observations.Blocks[0].BaseFeePerGas = nil
		}
		observations.Receipts[index].Receipt.EffectiveGasPrice = test.price
		result, err := ReconcileReceipts(context.Background(), archive, observations)
		if err != nil {
			t.Fatal(err)
		}
		entry := result.Transactions[index]
		if (entry.AccountedGasFee != nil) != test.valid {
			t.Fatalf("base %s price %s valid %t: %+v", test.base, test.price, test.valid, entry)
		}
		if test.valid {
			price, _ := new(big.Int).SetString(test.price, 10)
			if *entry.AccountedGasFee != new(big.Int).Mul(big.NewInt(25000), price).String() {
				t.Fatal("dynamic fee used its gas or price envelope")
			}
		}
	}
}

// Same-nonce originals and replacements can never both consume gas on one
// canonical chain. Preserve both observations and withhold the entire nonce.
func TestReceiptReconciliationConflictingNonceWinnersRemainUnresolved(t *testing.T) {
	archive, observations := receiptTestFixture(t, 1)
	index := receiptTestIndex(t, archive, "operator-a", 7, 2)
	observations.Receipts[index].Outcome, observations.Receipts[index].Receipt = "found", receiptTestFound(t, archive.Transactions[index], uint64(index))
	result, err := ReconcileReceipts(context.Background(), archive, observations)
	if err != nil {
		t.Fatal(err)
	}
	joined := receiptTestNonce(t, result, "operator-a", 7)
	if joined.State != "conflicting-observations" || joined.AccountedGasFee != nil || result.Fees[0].ObservedFinalizedGasFee != "2750000" {
		t.Fatalf("duplicate canonical nonce was accounted: %+v", joined)
	}
}

// A forged transaction index can collide across roles and nonce groups; checking
// candidates only inside one nonce would still charge this impossible block.
func TestReceiptReconciliationConflictingInclusionSlotsCrossNonceGroups(t *testing.T) {
	archive, observations := receiptTestFixture(t, 1)
	a := receiptTestIndex(t, archive, "operator-a", 7, 1)
	b := receiptTestIndex(t, archive, "operator-b", 11, 0)
	*observations.Receipts[a].Receipt.TransactionIndex, *observations.Receipts[b].Receipt.TransactionIndex = 100, 100
	observations.Blocks[0].GasUsed = 500000
	first, second := a, b
	if second < first {
		first, second = second, first
	}
	observations.Receipts[first].Receipt.CumulativeGasUsed, observations.Receipts[second].Receipt.CumulativeGasUsed = 300000, 330000
	result, err := ReconcileReceipts(context.Background(), archive, observations)
	if err != nil {
		t.Fatal(err)
	}
	if result.Transactions[a].NonceState != "conflicting-observations" || result.Transactions[b].NonceState != "conflicting-observations" ||
		result.Transactions[a].AccountedGasFee != nil || result.Transactions[b].AccountedGasFee != nil || result.Fees[1].ObservedFinalizedGasFee != "2500000" {
		t.Fatal("one inclusion slot was charged for two different transactions")
	}
}

// Different receipt slots cannot claim overlapping gas intervals, even when
// each individual receipt fits the signed envelope and the block gas bound.
func TestReceiptReconciliationConflictingCumulativeGasRemainsUnresolved(t *testing.T) {
	archive, observations := receiptTestFixture(t, 1)
	indexes := []int{}
	for index, observation := range observations.Receipts {
		if observation.Receipt != nil {
			indexes = append(indexes, index)
		}
	}
	first, second := indexes[0], indexes[1]
	observations.Receipts[second].Receipt.CumulativeGasUsed = observations.Receipts[first].Receipt.CumulativeGasUsed + observations.Receipts[second].Receipt.GasUsed - 1
	result, err := ReconcileReceipts(context.Background(), archive, observations)
	if err != nil {
		t.Fatal(err)
	}
	if result.Transactions[first].NonceState != "conflicting-observations" || result.Transactions[second].NonceState != "conflicting-observations" ||
		result.Transactions[first].AccountedGasFee != nil || result.Transactions[second].AccountedGasFee != nil || result.ObservationAccountingComplete {
		t.Fatal("overlapping gas intervals contributed terminal accounting")
	}
}

// Orphan, absent-block and above-boundary observations survive without becoming
// finalized; an explicit unchanged nonce likewise supplies no send authority.
func TestReceiptReconciliationPreservesOrphanMissingAndFutureInclusions(t *testing.T) {
	for _, state := range []string{"orphaned", "missing-block", "observed-unfinalized", "not-found"} {
		archive, observations := receiptTestFixture(t, 1)
		index := receiptTestIndex(t, archive, "operator-a", 7, 1)
		*observations.Accounts[0].Nonce = 7
		switch state {
		case "orphaned":
			observations.Receipts[index].Receipt.BlockHash = "0x" + strings.Repeat("9", 64)
		case "missing-block":
			observations.Receipts[index].Receipt.BlockNumber = 89
		case "observed-unfinalized":
			observations.Blocks = append(observations.Blocks, ObservedCanonicalBlock{Number: 101, Hash: "0x" + strings.Repeat("9", 64), GasUsed: 200000, GasLimit: 30000000})
			observations.Receipts[index].Receipt.BlockNumber, observations.Receipts[index].Receipt.BlockHash = 101, observations.Blocks[2].Hash
		case "not-found":
			observations.Receipts[index].Outcome, observations.Receipts[index].Receipt = "not-found", nil
		}
		result, err := ReconcileReceipts(context.Background(), archive, observations)
		if err != nil {
			t.Fatal(err)
		}
		entry := result.Transactions[index]
		if entry.ObservationState != state || entry.AccountedGasFee != nil || entry.WinnerHash != "" || result.ObservationAccountingComplete || result.SpendingAuthorized {
			t.Fatalf("%s became finalized or disposable: %+v", state, entry)
		}
	}
}

// Finalized receipt claims must agree with the exact boundary account nonce.
// Missing reads and contradictions cannot be converted into a terminal result.
func TestReceiptReconciliationRequiresBoundaryAccountConsistency(t *testing.T) {
	for _, unavailable := range []bool{false, true} {
		archive, observations := receiptTestFixture(t, 1)
		*observations.Accounts[0].Nonce = 7
		state := "conflicting-observations"
		if unavailable {
			observations.Accounts[0].Outcome, observations.Accounts[0].Nonce = "unavailable", nil
			state = "unavailable-account"
		}
		result, err := ReconcileReceipts(context.Background(), archive, observations)
		if err != nil {
			t.Fatal(err)
		}
		joined := receiptTestNonce(t, result, "operator-a", 7)
		if joined.State != state || joined.AccountedGasFee != nil || result.Fees[0].ObservedFinalizedGasFee != "0" {
			t.Fatalf("boundary account disagreement supplied fees: %+v", joined)
		}
	}
}

// Multiplication and cross-nonce sums stay exact even when the fee product
// exceeds 256 bits. No uint64 or floating-point accounting shortcut is safe.
func TestReceiptReconciliationRetainsArbitraryPrecisionFeeProducts(t *testing.T) {
	config, reader := censusTestFixture(t)
	key := censusTestKey(t, 1)
	price := new(big.Int).Lsh(big.NewInt(1), 255)
	to := common.HexToAddress(reader.images["database-a"].Intents[0].To)
	tx, err := types.SignTx(types.NewTx(&types.LegacyTx{Nonce: 7, To: &to, Gas: 30000, GasPrice: price, Value: new(big.Int), Data: []byte{1, 2, 3}}), types.LatestSignerForChainID(big.NewInt(31337)), key)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := tx.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	attempt := &reader.images["database-a"].Attempts[0]
	priceText := price.String()
	attempt.Raw, attempt.Hash, attempt.GasPrice = raw, tx.Hash().Hex(), &priceText
	archive, err := Collect(context.Background(), config, reader)
	if err != nil {
		t.Fatal(err)
	}
	observations := receiptTestObservations(t, archive, 1)
	observations.Blocks[0].GasUsed = 300000
	result, err := ReconcileReceipts(context.Background(), archive, observations)
	if err != nil {
		t.Fatal(err)
	}
	want := new(big.Int).Add(new(big.Int).Mul(big.NewInt(25000), price), big.NewInt(2750000)).String()
	if result.Fees[0].ObservedFinalizedGasFee != want || !result.ObservationAccountingComplete {
		t.Fatal("large product narrowed or duplicate store signature was charged")
	}
}

// A self-consistent unrelated set, repeated receipt or missing signature cannot
// produce a partial-green report. File trust is also never a caller-set field.
func TestReceiptReconciliationRefusesCoverageContextAndBoundChanges(t *testing.T) {
	for _, test := range []struct {
		name   string
		change func(*ReceiptObservations)
	}{
		{name: "schema", change: func(o *ReceiptObservations) { o.Schema += "x" }},
		{name: "census", change: func(o *ReceiptObservations) { o.CensusHash = digest([]byte("other")) }},
		{name: "chain", change: func(o *ReceiptObservations) { o.ChainId++ }},
		{name: "genesis", change: func(o *ReceiptObservations) { o.Genesis = "0x" + strings.Repeat("9", 64) }},
		{name: "native domain", change: func(o *ReceiptObservations) { o.NativeFinalized.Hash = "" }},
		{name: "mapping evidence", change: func(o *ReceiptObservations) { o.MappingEvidenceHash = "" }},
		{name: "boundary identity", change: func(o *ReceiptObservations) { o.EvmFinalized.Hash = "0x" + strings.Repeat("9", 64) }},
		{name: "missing receipt", change: func(o *ReceiptObservations) { o.Receipts = o.Receipts[1:] }},
		{name: "duplicate receipt", change: func(o *ReceiptObservations) { o.Receipts[1] = o.Receipts[0] }},
		{name: "foreign receipt", change: func(o *ReceiptObservations) { o.Receipts[0].Hash = "0x" + strings.Repeat("9", 64) }},
		{name: "missing account", change: func(o *ReceiptObservations) { o.Accounts = o.Accounts[1:] }},
		{name: "account boundary", change: func(o *ReceiptObservations) { o.Accounts[0].BlockHash = o.NativeFinalized.Hash }},
		{name: "account identity", change: func(o *ReceiptObservations) { o.Accounts[0].Address = o.Accounts[1].Address }},
		{name: "ambiguous account", change: func(o *ReceiptObservations) { o.Accounts[0].Outcome = "unavailable" }},
		{name: "duplicate block", change: func(o *ReceiptObservations) { o.Blocks = append(o.Blocks, o.Blocks[0]) }},
		{name: "missing boundary", change: func(o *ReceiptObservations) { o.Blocks = o.Blocks[:1] }},
		{name: "block gas", change: func(o *ReceiptObservations) { o.Blocks[0].GasUsed = o.Blocks[0].GasLimit + 1 }},
		{name: "base fee spelling", change: func(o *ReceiptObservations) { value := "+90"; o.Blocks[0].BaseFeePerGas = &value }},
		{name: "unbounded receipt", change: func(o *ReceiptObservations) {
			for i := range o.Receipts {
				if o.Receipts[i].Receipt != nil {
					o.Receipts[i].Receipt.EffectiveGasPrice = strings.Repeat("1", 79)
					break
				}
			}
		}},
	} {
		archive, observations := receiptTestFixture(t, 1)
		test.change(observations)
		if result, err := ReconcileReceipts(context.Background(), archive, observations); err == nil || result != nil {
			t.Fatalf("%s admitted a partial or unrelated set", test.name)
		}
	}
}

// Transport pins bind exact bytes, while strict JSON refuses authority flags,
// duplicate keys and trailing values before any semantic report is possible.
func TestReceiptObservationsLoadRequiresPrivatePinnedUnambiguousBytes(t *testing.T) {
	archive, observations := receiptTestFixture(t, 1)
	raw, err := json.Marshal(observations)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(censusTestDir(t), "observations.json")
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadReceiptObservations(context.Background(), FileReference{Path: path, Sha256: digest(raw)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ReconcileReceipts(context.Background(), archive, loaded); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadReceiptObservations(context.Background(), FileReference{Path: path, Sha256: digest([]byte("different"))}); err == nil {
		t.Fatal("wrong byte pin was accepted")
	}
	for _, malformed := range [][]byte{
		[]byte(strings.Replace(string(raw), `"schema":`, `"schema":"duplicate","schema":`, 1)),
		[]byte(strings.Replace(string(raw), `"schema":`, `"SCHEMA":"duplicate","schema":`, 1)),
		[]byte(strings.Replace(string(raw), `"schema":`, `"finality_authenticated":true,"schema":`, 1)),
		append(append([]byte{}, raw...), []byte(` {}`)...),
	} {
		if err := os.WriteFile(path, malformed, 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadReceiptObservations(context.Background(), FileReference{Path: path, Sha256: digest(malformed)}); err == nil {
			t.Fatal("ambiguous observations were accepted")
		}
	}
}

// Source ordering changes its content digest, but not transaction, nonce or fee
// outcomes. Cancellation and invalid archives never return usable partial work.
func TestReceiptReconciliationReplayAndCancellationAreDeterministic(t *testing.T) {
	archive, observations := receiptTestFixture(t, 1)
	first, err := ReconcileReceipts(context.Background(), archive, observations)
	if err != nil {
		t.Fatal(err)
	}
	for left, right := 0, len(observations.Receipts)-1; left < right; left, right = left+1, right-1 {
		observations.Receipts[left], observations.Receipts[right] = observations.Receipts[right], observations.Receipts[left]
	}
	observations.Accounts[0], observations.Accounts[1] = observations.Accounts[1], observations.Accounts[0]
	observations.Blocks[0], observations.Blocks[1] = observations.Blocks[1], observations.Blocks[0]
	second, err := ReconcileReceipts(context.Background(), archive, observations)
	if err != nil {
		t.Fatal(err)
	}
	if first.ObservationHash == second.ObservationHash || !reflect.DeepEqual(first.Transactions, second.Transactions) || !reflect.DeepEqual(first.Nonces, second.Nonces) || !reflect.DeepEqual(first.Fees, second.Fees) {
		t.Fatal("input ordering changed conditional accounting")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if result, err := ReconcileReceipts(ctx, archive, observations); err == nil || result != nil {
		t.Fatal("canceled replay returned a report")
	}
	if result, err := ReconcileReceipts(nil, archive, observations); err == nil || result != nil {
		t.Fatal("nil context returned a report")
	}
	if result, err := ReconcileReceipts(context.Background(), archive, nil); err == nil || result != nil {
		t.Fatal("absent observations returned a report")
	}
	archive.Transactions[0].GasLimit++
	if result, err := ReconcileReceipts(context.Background(), archive, observations); err == nil || result != nil {
		t.Fatal("changed signed census returned a report")
	}
}
