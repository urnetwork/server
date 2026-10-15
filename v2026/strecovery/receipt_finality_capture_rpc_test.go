// Transport and journal controls force exact interruption/budget boundaries.
// Decoder negatives use Rust's serialized shapes, not production encoders.
package strecovery

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A process that loses its response never gets a free request or byte allowance.
func TestReceiptFinalityCaptureJournalReservesBeforeReadAndChargesInterruptedReply(t *testing.T) {
	fixture := receiptFinalityTestFixture(t)
	_, directory := captureTestInputs(t, fixture, 0)
	store, err := openPrivatePath(directory, true)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	capture := &finalityCapture{directory: store, rpc: newReceiptCollectorRpc("http://archive.example")}
	capture.rpc.finality = true
	if err := capture.save(context.Background(), "request-00001.json", finalityCaptureAttempt{RequestHash: digest([]byte("synthetic interrupted request"))}); err != nil {
		t.Fatal(err)
	}
	if err := capture.resumeBudget(context.Background()); err != nil {
		t.Fatal(err)
	}
	if capture.rpc.requests != 1 || capture.rpc.remaining != maximumCollectionTotalReplyBytes-maximumCollectionReplyBytes-1 {
		t.Fatal("interrupted request regained its reserved allowance")
	}
	rpc := captureTestRpcFixture(fixture)
	rpc.intercept = func(string, []json.RawMessage) (any, error, bool) {
		if _, err := os.Stat(filepath.Join(directory, "request-00002.json")); err != nil {
			t.Fatal("transport preceded durable reservation")
		}
		return nil, nil, false
	}
	rpc.configure(capture.rpc)
	if _, err := capture.blockHash(context.Background(), 0); err != nil {
		t.Fatal(err)
	}
	remaining := capture.rpc.remaining
	if err := capture.resumeBudget(context.Background()); err != nil {
		t.Fatal(err)
	}
	if capture.rpc.requests != 2 || capture.rpc.remaining != remaining {
		t.Fatal("restart reset successful or interrupted response debits")
	}
}

// Eight interrupted maximum replies exhaust the lifetime allowance even when
// every restart constructs a fresh HTTP client and a fresh timeout context.
func TestReceiptFinalityCaptureExhaustedJournalCannotResetResponseBudget(t *testing.T) {
	fixture := receiptFinalityTestFixture(t)
	config, directory := captureTestInputs(t, fixture, 0)
	store, err := openPrivatePath(directory, true)
	if err != nil {
		t.Fatal(err)
	}
	capture := &finalityCapture{directory: store}
	for index := 1; index <= 8; index++ {
		if err := capture.save(context.Background(), fmt.Sprintf("request-%05d.json", index), finalityCaptureAttempt{RequestHash: digest([]byte(fmt.Sprintf("synthetic interrupted request %d", index)))}); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	rpc := captureTestRpcFixture(fixture)
	result, err := captureReceiptFinality(context.Background(), fixture.receipts.archive, fixture.collection, fixture.checkpoint, config, directory, rpc.configure)
	if err == nil || result != nil || len(rpc.calls) != 0 || !strings.Contains(err.Error(), "shared request/response budget") {
		t.Fatalf("exhausted retained budget reset: %v", err)
	}
}

// Journal gaps are contradictions, not a signal to start again with request one.
func TestReceiptFinalityCaptureRejectsJournalGapsAndOrphanCompletions(t *testing.T) {
	fixture := receiptFinalityTestFixture(t)
	for _, name := range []string{"request-00002.json", "read-00001.json"} {
		config, directory := captureTestInputs(t, fixture, 0)
		raw := []byte(`{"reply_bytes":0}`)
		if strings.HasPrefix(name, "request-") {
			raw, _ = json.Marshal(finalityCaptureAttempt{RequestHash: digest([]byte("synthetic"))})
		}
		if err := os.WriteFile(filepath.Join(directory, name), raw, 0400); err != nil {
			t.Fatal(err)
		}
		rpc := captureTestRpcFixture(fixture)
		result, err := captureReceiptFinality(context.Background(), fixture.receipts.archive, fixture.collection, fixture.checkpoint, config, directory, rpc.configure)
		if err == nil || result != nil || len(rpc.calls) != 0 || !strings.Contains(err.Error(), "gap or orphan") {
			t.Fatalf("journal contradiction accepted: %v", err)
		}
	}
}

// Native capture cannot call a signer, broadcast, runtime authority, EVM path
// or an unpinned mutable head. The older collector keeps its original profile.
func TestReceiptFinalityCaptureRpcHasOnlyRequiredNativeReadMethods(t *testing.T) {
	client := newReceiptCollectorRpc("http://archive.example")
	client.finality = true
	for _, method := range []string{"author_submitExtrinsic", "eth_sendRawTransaction", "state_call", "grandpa_roundState", "chain_getFinalizedHead", "eth_chainId"} {
		if raw, err := client.call(context.Background(), method, []any{}); err == nil || raw != nil || client.requests != 0 {
			t.Fatalf("native capture admitted %s", method)
		}
	}
	client.finality = false
	if _, err := client.call(context.Background(), "chain_getHeader", []any{}); err == nil || client.requests != 0 {
		t.Fatal("old collector read profile expanded")
	}
}

// The actual SDK serde representation is a pair of numeric byte arrays. A
// malformed engine cannot hide a missing certificate behind a known block.
func TestReceiptFinalityCaptureRejectsMalformedJustificationSerde(t *testing.T) {
	fixture := receiptFinalityTestFixture(t)
	header := fixture.headers[3]
	for _, justifications := range []json.RawMessage{
		json.RawMessage(`[["FRNK",[1,2]]]`),
		json.RawMessage(`[[[70,82,78,75],"AQI="]]`),
		json.RawMessage(`[[[70,82,78,75],[null]]]`),
		json.RawMessage(`[[[70,82,78,75],[256]]]`),
		json.RawMessage(`[[[70,82,78,75],[]]]`),
		json.RawMessage(`[[[70,82,78,75],[1]],[[70,82,78,75],[2]]]`),
	} {
		raw, _ := json.Marshal(map[string]any{"block": map[string]any{"header": captureTestHeaderJson(header), "extrinsics": []string{}}, "justifications": justifications})
		if _, certificate, err := captureBlockCertificate(raw, header.identity.Hash); err == nil || certificate != "" {
			t.Fatalf("malformed justification accepted: %s", justifications)
		}
	}
}

// Unknown authorities and invalid signatures remain terminal even when the
// endpoint returns a nonempty stored certificate at exactly the requested hash.
func TestReceiptFinalityCaptureRetainsButNeverPublishesBadCertificate(t *testing.T) {
	fixture := receiptFinalityTestFixture(t)
	rpc := captureTestRpcFixture(fixture)
	hash := fixture.collection.Observations.NativeFinalized.Hash
	_, unknown := finalityTestAuthorities(99)
	fixture.checkpoint.Authorities = unknown
	config, directory := captureTestInputs(t, fixture, 0)
	result, err := captureReceiptFinality(context.Background(), fixture.receipts.archive, fixture.collection, fixture.checkpoint, config, directory, rpc.configure)
	if err == nil || result != nil {
		t.Fatal("wrong authority certificate accepted")
	}
	if _, err := os.Stat(filepath.Join(directory, "certificate-"+hash[2:]+".json")); err != nil {
		t.Fatal("rejected certificate bytes were discarded")
	}
	if _, err := os.Stat(filepath.Join(directory, "proof.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("rejected certificate published as a proof")
	}
}

// V2 authenticates the boundary as an exact ancestor, not merely a height under
// a later quorum. Re-pinning a changed collection cannot rewrite signed history.
func TestReceiptFinalityDescendantRejectsReboundCollectionAncestor(t *testing.T) {
	fixture := receiptFinalityTestFixture(t)
	rpc := captureTestRpcFixture(fixture)
	captureTestDescendant(fixture, rpc, true)
	proof := &ReceiptFinalityProof{Schema: ReceiptFinalityDescendantProofSchema, CheckpointHash: fixture.checkpoint.Hash(), CollectionHash: fixture.collection.ContentHash, Segments: []GrandpaFinalitySegment{fixture.segment(fixture.headers[1:], fixture.keys, 9)}}
	if result, err := VerifyReceiptFinality(context.Background(), fixture.receipts.archive, fixture.collection, fixture.checkpoint, proof); err != nil || result == nil {
		t.Fatalf("positive descendant prerequisite failed: %v", err)
	}
	fixture.collection.Observations.NativeFinalized.Hash = "0x" + strings.Repeat("7", 64)
	fixture.collection.Commitments.ObservationHash = objectDigest(fixture.collection.Observations)
	fixture.collection.ContentHash = fixture.collection.hash()
	proof.CollectionHash = fixture.collection.ContentHash
	if result, err := VerifyReceiptFinality(context.Background(), fixture.receipts.archive, fixture.collection, fixture.checkpoint, proof); err == nil || result != nil || !strings.Contains(err.Error(), "exact collection boundary") {
		t.Fatalf("rebound ancestor passed valid descendant quorum: %v", err)
	}
}
