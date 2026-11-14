// The shared decoder is exercised with the collector's independently built
// transaction/receipt tries, without a node, database, or archive authority.
package strecovery

import (
	"context"
	"encoding/hex"
	"errors"
	"strings"
	"testing"

	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/rlp"
)

// The fixture serializes exact consensus bytes before invoking the new API.
func receiptBlockCensusTestBytes(t testing.TB, fixture *receiptCommitmentFixture) (string, string, []string) {
	t.Helper()
	header, err := rlp.EncodeToBytes(fixture.headers[0])
	if err != nil {
		t.Fatal(err)
	}
	block, err := rlp.EncodeToBytes(types.NewBlockWithHeader(fixture.headers[0]).WithBody(types.Body{Transactions: fixture.transactions}))
	if err != nil {
		t.Fatal(err)
	}
	receipts := []string{}
	for _, receipt := range fixture.receipts {
		raw, err := receipt.MarshalBinary()
		if err != nil {
			t.Fatal(err)
		}
		receipts = append(receipts, "0x"+hex.EncodeToString(raw))
	}
	return "0x" + hex.EncodeToString(header), "0x" + hex.EncodeToString(block), receipts
}

// Foreign transactions are preserved in their original positions, not filtered
// through the archived operator's signed attempts or a claimed receipt census.
func TestReceiptBlockCensusPreservesCompleteForeignTraffic(t *testing.T) {
	fixture := receiptCommitmentTestFixture(t, 1)
	header, block, receipts := receiptBlockCensusTestBytes(t, fixture)
	census, err := AuthenticateReceiptBlockCensus(context.Background(), fixture.headers[0].Hash().Hex(), 90, header, block, receipts)
	if err != nil || census == nil || len(census.Transactions) != len(fixture.transactions) || len(census.Receipts) != len(fixture.receipts) {
		t.Fatalf("complete block census refused its independently committed vectors: %v", err)
	}
	for index := range fixture.transactions {
		raw, err := census.Receipts[index].MarshalBinary()
		if err != nil || census.Transactions[index].Hash() != fixture.transactions[index].Hash() || "0x"+hex.EncodeToString(raw) != receipts[index] {
			t.Fatalf("complete block census changed committed position %d: %v", index, err)
		}
	}
	census.Header.GasUsed = 1
	if fixture.headers[0].GasUsed == 1 {
		t.Fatal("block census returned a borrowed header")
	}
}

// Count and trie commitments independently reject omissions, reordering, and a
// plausible forged outcome that still has valid RLP and cumulative gas.
func TestReceiptBlockCensusRefusesIncompleteOrChangedVectors(t *testing.T) {
	for _, fault := range []string{"missing", "reordered", "outcome", "block"} {
		fixture := receiptCommitmentTestFixture(t, 1)
		header, block, receipts := receiptBlockCensusTestBytes(t, fixture)
		switch fault {
		case "missing":
			receipts = receipts[:len(receipts)-1]
		case "reordered":
			receipts[0], receipts[1] = receipts[1], receipts[0]
		case "outcome":
			fixture.receipts[0].Status = 1 - fixture.receipts[0].Status
			_, _, receipts = receiptBlockCensusTestBytes(t, fixture)
		case "block":
			fixture.transactions[0], fixture.transactions[1] = fixture.transactions[1], fixture.transactions[0]
			_, block, _ = receiptBlockCensusTestBytes(t, fixture)
		}
		census, err := AuthenticateReceiptBlockCensus(context.Background(), fixture.headers[0].Hash().Hex(), 90, header, block, receipts)
		if err == nil || census != nil {
			t.Fatalf("block census accepted incomplete or changed committed vector %s", fault)
		}
	}
}

// Direct callers cannot bypass the transport's resource bounds or reinterpret
// absent receipts as a successful empty block; cancellation emits no census.
func TestReceiptBlockCensusBoundsAndCancellation(t *testing.T) {
	fixture := receiptCommitmentTestFixture(t, 1)
	header, block, receipts := receiptBlockCensusTestBytes(t, fixture)
	for _, fault := range []string{"nil-context", "cancelled", "nil-vector", "count", "bytes", "header"} {
		ctx := context.Background()
		inputHeader, inputBlock, inputReceipts := header, block, receipts
		switch fault {
		case "nil-context":
			ctx = nil
		case "cancelled":
			cancelled, cancel := context.WithCancel(ctx)
			cancel()
			ctx = cancelled
		case "nil-vector":
			inputReceipts = nil
		case "count":
			inputReceipts = make([]string, maximumCollectionBlockTransactions+1)
		case "bytes":
			inputBlock = strings.Repeat("0", MaximumReceiptBlockCensusEncodedBytes)
		case "header":
			inputHeader = "0xc0"
		}
		census, err := AuthenticateReceiptBlockCensus(ctx, fixture.headers[0].Hash().Hex(), 90, inputHeader, inputBlock, inputReceipts)
		if err == nil || census != nil || fault == "cancelled" && !errors.Is(err, context.Canceled) {
			t.Fatalf("block census escaped direct boundary %s: %v", fault, err)
		}
	}
}

// A genuinely empty committed vector is distinct from an absent RPC result.
func TestReceiptBlockCensusAdmitsExplicitEmptyBlock(t *testing.T) {
	fixture := receiptCommitmentTestFixture(t, 1)
	fixture.transactions, fixture.receipts = []*types.Transaction{}, []*types.Receipt{}
	fixture.headers[0].TxHash, fixture.headers[0].ReceiptHash = types.EmptyTxsHash, types.EmptyReceiptsHash
	fixture.headers[0].GasUsed = 0
	header, block, receipts := receiptBlockCensusTestBytes(t, fixture)
	census, err := AuthenticateReceiptBlockCensus(t.Context(), fixture.headers[0].Hash().Hex(), 90, header, block, receipts)
	if err != nil || census == nil || census.Transactions == nil || census.Receipts == nil || len(census.Transactions) != 0 || len(census.Receipts) != 0 {
		t.Fatalf("explicit empty committed block was confused with absent evidence: %v", err)
	}
}
