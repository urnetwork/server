// Fee-context command fixtures are entirely local and retain exact file pins.
// Native signatures are independently encoded by the finality command fixture.
package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"testing"

	"github.com/urnetwork/server/v2026/strecovery"
)

// Replaying empty signed custody is deterministic and cannot claim coverage.
// Re-pinning an invalid certificate emits no partial report or source mutation.
func TestRecoveryCommandVerifiesFeeContextsOfflineWithoutFeeAuthority(t *testing.T) {
	args, reader, retainedFiles, proof, certificate := finalityCommandInputs(t)
	args[0] = "verify-fee-contexts"
	var first []byte
	for range 2 {
		var output bytes.Buffer
		if err := run(context.Background(), args, &output, reader); err != nil {
			t.Fatal(err)
		}
		var result strecovery.ReceiptFeeContexts
		if err := json.Unmarshal(output.Bytes(), &result); err != nil {
			t.Fatal(err)
		}
		if result.Schema != strecovery.ReceiptFeeContextsSchema || result.Finality == nil || !result.Finality.GrandpaCertificatesVerified || len(result.Blocks) != 0 || len(result.Transactions) != 0 ||
			result.FoundReceiptContextsComplete || result.AuthorityCheckpointAuthenticated || result.GenesisAuthenticated || result.RuntimeSourceAuthenticated || result.PayerBindingAuthenticated || result.NativeStateReadsAuthenticated ||
			result.NativeExtrinsicLocationsAuthenticated || result.FeeAttributionAuthenticated || result.FinalityAuthenticated || result.CanonicalReceiptsReconciled || result.ActualFeesReconciled || result.SpendingAuthorized || reader.calls != 2 {
			t.Fatal("offline replay invented receipt coverage/authority or reopened custody")
		}
		if first == nil {
			first = bytes.Clone(output.Bytes())
		} else if !bytes.Equal(first, output.Bytes()) {
			t.Fatal("offline context replay changed its result")
		}
	}
	for path, retained := range retainedFiles {
		raw, err := os.ReadFile(path)
		if err != nil || !bytes.Equal(raw, retained) {
			t.Fatal("context verification changed an input file")
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
		t.Fatal("forged finality produced fee context or touched original custody")
	}
}

// The command has no runtime, mapping, fee or checkpoint self-approval switch;
// complete pinned input and a bounded offline deadline precede all file reads.
func TestRecoveryCommandRequiresPinnedFeeContextInputsWithoutApprovalFlags(t *testing.T) {
	reader := &commandReader{fail: true}
	for _, args := range [][]string{
		{"verify-fee-contexts"},
		{"verify-fee-contexts", "--archive", "/private.example/archive.json"},
		{"verify-fee-contexts", "--approve-checkpoint"},
		{"verify-fee-contexts", "--runtime-authenticated"},
		{"verify-fee-contexts", "--payer", "0x1234"},
		{"verify-fee-contexts", "--actual-fee", "1"},
		{"verify-fee-contexts", "--native-root", "0x1234"},
		{"verify-fee-contexts", "--timeout", "16m"},
		{"verify-fee-contexts", "--rpc-url", "https://node.example"},
	} {
		var output bytes.Buffer
		if err := run(context.Background(), args, &output, reader); err == nil || output.Len() != 0 || reader.calls != 0 {
			t.Fatalf("incomplete or self-approving fee context input admitted: %v", args)
		}
	}
}
