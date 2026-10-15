// Export real synthetic signatures and tries for the conservation command's
// independently fixed network profile. No production network data is used.
package strecovery

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestReceiptFeeContextsNetworkSelectionRemainsInSignedOriginals(t *testing.T) {
	for _, chainId := range []uint64{31337, 964} {
		fixture := feeContextTestFixtureOnChain(t, 1, []int{-1, 0, 5, 10}, chainId)
		value, err := VerifyReceiptFeeContexts(context.Background(), fixture.receipts.archive, fixture.collection, fixture.checkpoint, fixture.proof)
		if err != nil || value == nil || fixture.receipts.archive.Selection.ChainId != chainId {
			t.Fatal("original signed fee network fixture failed", chainId, err)
		}
		for _, transaction := range fixture.receipts.archive.Transactions {
			if _, _, err := decodeTransaction(transaction.Raw, chainId); err != nil {
				t.Fatal("fixture selected a chain without re-signing original transactions", chainId, err)
			}
			if _, _, err := decodeTransaction(transaction.Raw, chainId+1); err == nil {
				t.Fatal("original transaction signature accepted a substituted network")
			}
		}
	}
}

func TestExportEconomicConservationFeeInputs(t *testing.T) {
	directory := os.Getenv("URNETWORK_ECONOMIC_FEE_EXPORT_DIR")
	if directory == "" {
		directory = t.TempDir()
	}
	if !filepath.IsAbs(directory) {
		t.Fatal("fee export directory must be absolute")
	}
	if err := os.MkdirAll(directory, 0700); err != nil {
		t.Fatal(err)
	}
	for _, fixtureCase := range []struct {
		name   string
		winner int
	}{{name: "success", winner: 1}, {name: "reverted", winner: 3}} {
		fixture := feeContextTestFixtureOnChain(t, fixtureCase.winner, []int{-1, 0, 5, 10}, 964)
		expected, err := VerifyReceiptFeeContexts(context.Background(), fixture.receipts.archive, fixture.collection, fixture.checkpoint, fixture.proof)
		if err != nil || expected == nil || len(expected.Blocks) != 1 || len(expected.Blocks[0].NativeContexts) != 1 {
			t.Fatal("complete original fee-context export is absent", err)
		}
		value := struct {
			Archive      *Archive                  `json:"archive"`
			Collection   *ReceiptCollection        `json:"collection"`
			Checkpoint   *NativeFinalityCheckpoint `json:"checkpoint"`
			Proof        *ReceiptFinalityProof     `json:"proof"`
			Expected     *ReceiptFeeContexts       `json:"expected"`
			ParentHeader string                    `json:"parent_header"`
			ChildHeader  string                    `json:"child_header"`
		}{Archive: fixture.receipts.archive, Collection: fixture.collection, Checkpoint: fixture.checkpoint, Proof: fixture.proof, Expected: expected, ParentHeader: "0x" + hex.EncodeToString(fixture.headers[0].raw), ChildHeader: "0x" + hex.EncodeToString(fixture.headers[1].raw)}
		raw, err := json.MarshalIndent(value, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(directory, fixtureCase.name+".json"), append(raw, '\n'), 0600); err != nil {
			t.Fatal(err)
		}
	}
}
