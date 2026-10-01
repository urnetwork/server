// Independent synthetic native headers/certificates are rendered in the pinned
// SDK's JSON shape. Deterministic transport transitions exercise durable capture
// without a live node, scheduler races, mainnet keys or production routes.
package strecovery

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// Fixture results are built from the independent test encoder's own inputs,
// never from the production header decoder or capture implementation.
type captureTestRpc struct {
	fixture      *receiptFinalityFixture
	certificates map[string]string
	calls        []string
	intercept    func(string, []json.RawMessage) (any, error, bool)
}

// Convert a test-only native header to the source-defined RPC serde fields.
func captureTestHeaderJson(header *finalityTestHeader) any {
	logs := []string{}
	for _, digest := range header.digests {
		logs = append(logs, "0x"+hex.EncodeToString(digest))
	}
	return map[string]any{"parentHash": "0x" + hex.EncodeToString(header.raw[:32]), "number": fmt.Sprintf("0x%x", header.identity.Number),
		"stateRoot": "0x" + strings.Repeat("42", 32), "extrinsicsRoot": "0x" + strings.Repeat("73", 32), "digest": map[string]any{"logs": logs}}
}

// Rust plain bytes serialize as numeric arrays; Go []byte would emit base64.
func captureTestBytes(raw []byte) []uint16 {
	result := make([]uint16, len(raw))
	for index, value := range raw {
		result[index] = uint16(value)
	}
	return result
}

// Every fixture certificate remains the independently signed SCALE payload.
func captureTestRpcFixture(fixture *receiptFinalityFixture) *captureTestRpc {
	rpc := &captureTestRpc{fixture: fixture, certificates: map[string]string{}}
	for _, segment := range fixture.proof.Segments {
		for _, header := range fixture.headers {
			if segment.Headers[len(segment.Headers)-1] == "0x"+hex.EncodeToString(header.raw) {
				rpc.certificates[header.identity.Hash] = segment.JustificationScale
			}
		}
	}
	return rpc
}

// Fixed height/hash selectors are recorded independently at the transport seam.
func (self *captureTestRpc) configure(client *receiptCollectorRpc) {
	client.wait = func(context.Context, time.Duration) error { return nil }
	client.client.Transport = collectionTestRoundTripper(func(request *http.Request) (*http.Response, error) {
		var payload struct {
			Id     int               `json:"id"`
			Method string            `json:"method"`
			Params []json.RawMessage `json:"params"`
		}
		if err := json.NewDecoder(request.Body).Decode(&payload); err != nil {
			return nil, err
		}
		self.calls = append(self.calls, payload.Method+":"+string(payload.Params[0]))
		var result any
		handled := false
		if self.intercept != nil {
			var err error
			result, err, handled = self.intercept(payload.Method, payload.Params)
			if err != nil {
				return nil, err
			}
		}
		if !handled {
			switch payload.Method {
			case "chain_getBlockHash":
				var number uint64
				if err := json.Unmarshal(payload.Params[0], &number); err != nil {
					return nil, err
				}
				if number == 0 {
					result = self.fixture.checkpoint.Genesis
				}
				for _, header := range self.fixture.headers {
					if header.identity.Number == number {
						result = header.identity.Hash
					}
				}
			case "chain_getHeader", "chain_getBlock":
				var hash string
				if err := json.Unmarshal(payload.Params[0], &hash); err != nil {
					return nil, err
				}
				for _, header := range self.fixture.headers {
					if header.identity.Hash != hash {
						continue
					}
					result = captureTestHeaderJson(header)
					if payload.Method == "chain_getBlock" {
						var justifications any
						if certificate := self.certificates[hash]; certificate != "" {
							raw, _ := hex.DecodeString(certificate[2:])
							justifications = []any{[]any{[]uint16{70, 82, 78, 75}, captureTestBytes(raw)}}
						}
						result = map[string]any{"block": map[string]any{"header": result, "extrinsics": []string{}}, "justifications": justifications}
					}
				}
			default:
				return nil, errors.New("fixture received a method outside the native read profile")
			}
		}
		raw, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": payload.Id, "result": result})
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(bytes.NewReader(raw))}, err
	})
}

// Each owner has a physical private directory and explicitly bounded settings.
func captureTestInputs(t testing.TB, fixture *receiptFinalityFixture, descendants uint64) (ReceiptFinalityCaptureConfig, string) {
	t.Helper()
	directory := t.TempDir()
	if err := os.Chmod(directory, 0700); err != nil {
		t.Fatal(err)
	}
	return ReceiptFinalityCaptureConfig{Schema: ReceiptFinalityCaptureConfigSchema, CollectionHash: fixture.collection.ContentHash, CheckpointHash: fixture.checkpoint.Hash(),
		Source: "synthetic-owned-archive", RpcUrl: "http://archive.example", MaximumDescendantHeaders: descendants, RetryWindowSeconds: 300}, directory
}

// Preserve the original collection; only the transport gains a later header.
func captureTestDescendant(fixture *receiptFinalityFixture, rpc *captureTestRpc, certificate bool) *finalityTestHeader {
	parent := fixture.headers[len(fixture.headers)-1]
	header := finalityTestNativeHeader(uint32(parent.identity.Number+1), parent.identity.Hash, finalityTestFrontier("0x"+strings.Repeat("8", 64)))
	fixture.headers = append(fixture.headers, header)
	if certificate {
		rpc.certificates[header.identity.Hash] = finalityTestCertificate(header, nil, nil, []int{0, 1}, fixture.keys, 19, fixture.checkpoint.SetId)
	}
	return header
}

// A complete private proof is pinned, replayable and idempotent without RPC.
func TestReceiptFinalityCapturePublishesPinnedProofAndReplaysWithoutRpc(t *testing.T) {
	fixture := receiptFinalityTestFixture(t)
	rpc := captureTestRpcFixture(fixture)
	config, directory := captureTestInputs(t, fixture, 0)
	before := []string{objectDigest(fixture.receipts.archive), objectDigest(fixture.collection), fixture.checkpoint.Hash()}
	result, err := captureReceiptFinality(context.Background(), fixture.receipts.archive, fixture.collection, fixture.checkpoint, config, directory, rpc.configure)
	if err != nil {
		t.Fatal(err)
	}
	proof, err := LoadReceiptFinalityProof(context.Background(), result.Proof)
	if err != nil {
		t.Fatal(err)
	}
	if proof.Schema != ReceiptFinalityDescendantProofSchema || len(proof.Segments) != 1 || proof.Segments[0].JustificationScale != fixture.proof.Segments[0].JustificationScale {
		t.Fatal("capture changed exact certificate")
	}
	replay, err := VerifyReceiptFinality(context.Background(), fixture.receipts.archive, fixture.collection, fixture.checkpoint, proof)
	if err != nil || objectDigest(replay) != objectDigest(result.Reconciliation) {
		t.Fatalf("offline replay differs: %v", err)
	}
	if replay.AuthorityCheckpointAuthenticated || replay.GenesisAuthenticated || replay.RuntimeSourceAuthenticated || replay.FinalityAuthenticated || replay.CanonicalReceiptsReconciled || replay.ActualFeesReconciled || replay.SpendingAuthorized {
		t.Fatal("capture invented authority")
	}
	for _, receipt := range replay.Receipts.Receipts {
		if receipt.ActualGasFee != nil {
			t.Fatal("capture fabricated actual fees")
		}
	}
	info, err := os.Stat(result.Proof.Path)
	if err != nil || info.Mode().Perm() != 0400 {
		t.Fatal("proof is not sealed private evidence")
	}
	again, err := captureReceiptFinality(context.Background(), fixture.receipts.archive, fixture.collection, fixture.checkpoint, config, directory, func(*receiptCollectorRpc) { t.Fatal("complete restart opened RPC") })
	if err != nil || objectDigest(result) != objectDigest(again) {
		t.Fatalf("complete replay changed: %v", err)
	}
	after, _ := os.Stat(result.Proof.Path)
	if !os.SameFile(info, after) || !slices.Equal(before, []string{objectDigest(fixture.receipts.archive), objectDigest(fixture.collection), fixture.checkpoint.Hash()}) {
		t.Fatal("capture replaced proof or mutated original histories")
	}
}

// A later signed certificate proves the retained boundary without substituting
// the descendant's unrelated Frontier digest or its native identity.
func TestReceiptFinalityCaptureAcceptsCertifiedDescendantWithoutMovingCollection(t *testing.T) {
	fixture := receiptFinalityTestFixture(t)
	rpc := captureTestRpcFixture(fixture)
	delete(rpc.certificates, fixture.collection.Observations.NativeFinalized.Hash)
	descendant := captureTestDescendant(fixture, rpc, true)
	config, directory := captureTestInputs(t, fixture, 2)
	before := objectDigest(fixture.collection)
	result, err := captureReceiptFinality(context.Background(), fixture.receipts.archive, fixture.collection, fixture.checkpoint, config, directory, rpc.configure)
	if err != nil {
		t.Fatal(err)
	}
	if result.Reconciliation.NativeFinalized != fixture.collection.Observations.NativeFinalized || result.Reconciliation.NativeCertified == nil || *result.Reconciliation.NativeCertified != descendant.identity || before != objectDigest(fixture.collection) {
		t.Fatal("descendant capture moved or rewrote the original collection")
	}
	proof, err := LoadReceiptFinalityProof(context.Background(), result.Proof)
	if err != nil {
		t.Fatal(err)
	}
	proof.Schema = ReceiptFinalityProofSchema
	if report, err := VerifyReceiptFinality(context.Background(), fixture.receipts.archive, fixture.collection, fixture.checkpoint, proof); err == nil || report != nil {
		t.Fatal("legacy v1 exact-boundary contract was weakened")
	}
}

// A valid descendant quorum cannot authenticate a different collection ancestor.
func TestReceiptFinalityCaptureRejectsUnrelatedAncestorDespiteValidDescendantQuorum(t *testing.T) {
	fixture := receiptFinalityTestFixture(t)
	fixture.headers[3] = finalityTestNativeHeader(703, fixture.headers[2].identity.Hash, finalityTestFrontier("0x"+strings.Repeat("8", 64)))
	fixture.bind()
	rpc := captureTestRpcFixture(fixture)
	delete(rpc.certificates, fixture.collection.Observations.NativeFinalized.Hash)
	captureTestDescendant(fixture, rpc, true)
	config, directory := captureTestInputs(t, fixture, 1)
	result, err := captureReceiptFinality(context.Background(), fixture.receipts.archive, fixture.collection, fixture.checkpoint, config, directory, rpc.configure)
	if err == nil || result != nil || !strings.Contains(err.Error(), "Frontier commitment") {
		t.Fatalf("unrelated native mapping accepted: %v", err)
	}
	if _, err := os.Stat(filepath.Join(directory, "proof.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("rejected final replay published a proof")
	}
}

// Outgoing-set certificates are mandatory even if later keys sign a valid tip.
func TestReceiptFinalityCaptureRequiresOutgoingAuthorityCertificate(t *testing.T) {
	fixture, _ := receiptFinalityRotationFixture(t)
	rpc := captureTestRpcFixture(fixture)
	delete(rpc.certificates, fixture.headers[3].identity.Hash)
	config, directory := captureTestInputs(t, fixture, 0)
	result, err := captureReceiptFinality(context.Background(), fixture.receipts.archive, fixture.collection, fixture.checkpoint, config, directory, rpc.configure)
	if err == nil || result != nil || !strings.Contains(err.Error(), "outgoing GRANDPA enactment") {
		t.Fatalf("missing handoff downgraded: %v", err)
	}
}

// The capture discovers scheduled transitions from complete native digests.
func TestReceiptFinalityCapturePreservesScheduledAuthorityHandoffs(t *testing.T) {
	fixture, _ := receiptFinalityRotationFixture(t)
	rpc := captureTestRpcFixture(fixture)
	config, directory := captureTestInputs(t, fixture, 0)
	result, err := captureReceiptFinality(context.Background(), fixture.receipts.archive, fixture.collection, fixture.checkpoint, config, directory, rpc.configure)
	if err != nil {
		t.Fatal(err)
	}
	if result.Reconciliation.AuthorityTransitions != 1 || result.Reconciliation.NextSetId != 10 || len(result.Reconciliation.Certificates) != 2 {
		t.Fatal("capture omitted authenticated authority handoff")
	}
}

// When the collection lies before enactment, the outgoing certificate at the
// descendant also determines the next rolling checkpoint's post-finalization set.
func TestReceiptFinalityCaptureReportsAuthorityStateAtCertifiedDescendant(t *testing.T) {
	fixture, _ := receiptFinalityRotationFixture(t)
	headers := fixture.headers
	fixture.headers = fixture.headers[:2]
	fixture.bind()
	fixture.headers = headers
	rpc := captureTestRpcFixture(fixture)
	delete(rpc.certificates, headers[1].identity.Hash)
	config, directory := captureTestInputs(t, fixture, 3)
	result, err := captureReceiptFinality(context.Background(), fixture.receipts.archive, fixture.collection, fixture.checkpoint, config, directory, rpc.configure)
	if err != nil {
		t.Fatal(err)
	}
	if result.Reconciliation.NativeFinalized.Number != 701 || result.Reconciliation.NativeCertified.Number != 703 || result.Reconciliation.NextSetId != 10 || result.Reconciliation.PendingChange != nil {
		t.Fatal("rolling authority state was attached to the wrong native header")
	}
}

// An absent stored certificate is refreshed on restart, preserving completed
// headers. Later delivery of that certificate can complete the original capture.
func TestReceiptFinalityCaptureDoesNotCacheMissingCertificateAsFinalityFailure(t *testing.T) {
	fixture := receiptFinalityTestFixture(t)
	rpc := captureTestRpcFixture(fixture)
	hash := fixture.collection.Observations.NativeFinalized.Hash
	certificate := rpc.certificates[hash]
	delete(rpc.certificates, hash)
	config, directory := captureTestInputs(t, fixture, 0)
	result, err := captureReceiptFinality(context.Background(), fixture.receipts.archive, fixture.collection, fixture.checkpoint, config, directory, rpc.configure)
	if err == nil || result != nil || !strings.Contains(err.Error(), "no stored GRANDPA") {
		t.Fatalf("absence became success: %v", err)
	}
	retained, err := os.ReadFile(filepath.Join(directory, "header-"+hash[2:]+".json"))
	if err != nil {
		t.Fatal("partial header not retained")
	}
	rpc.certificates[hash] = certificate
	rpc.calls = nil
	result, err = captureReceiptFinality(context.Background(), fixture.receipts.archive, fixture.collection, fixture.checkpoint, config, directory, rpc.configure)
	if err != nil || result == nil {
		t.Fatalf("new stored certificate was not observed: %v", err)
	}
	for _, call := range rpc.calls {
		if strings.HasPrefix(call, "chain_getHeader:") {
			t.Fatal("restart refetched immutable complete headers")
		}
	}
	after, _ := os.ReadFile(filepath.Join(directory, "header-"+hash[2:]+".json"))
	if !bytes.Equal(retained, after) {
		t.Fatal("restart rewrote partial evidence")
	}
}

// Exact numbered discovery remains finite when the archive has no certificate.
func TestReceiptFinalityCaptureExhaustsOnlyItsPinnedDescendantWindow(t *testing.T) {
	fixture := receiptFinalityTestFixture(t)
	rpc := captureTestRpcFixture(fixture)
	clear(rpc.certificates)
	captureTestDescendant(fixture, rpc, false)
	captureTestDescendant(fixture, rpc, false)
	captureTestDescendant(fixture, rpc, true)
	config, directory := captureTestInputs(t, fixture, 2)
	result, err := captureReceiptFinality(context.Background(), fixture.receipts.archive, fixture.collection, fixture.checkpoint, config, directory, rpc.configure)
	if err == nil || result != nil || !strings.Contains(err.Error(), "pinned descendant window") {
		t.Fatalf("capture widened its bound: %v", err)
	}
	var heights []string
	for _, call := range rpc.calls {
		if strings.HasPrefix(call, "chain_getBlockHash:") {
			heights = append(heights, call)
		}
	}
	if !slices.Equal(heights, []string{"chain_getBlockHash:0", "chain_getBlockHash:704", "chain_getBlockHash:705"}) {
		t.Fatalf("discovery selectors changed: %v", heights)
	}
}

// Cancellation happens at an explicit request boundary after headers are synced.
// Restart consumes the interrupted reservation and reuses those same headers.
func TestReceiptFinalityCaptureResumesAfterCanceledReadWithoutLosingBudget(t *testing.T) {
	fixture := receiptFinalityTestFixture(t)
	rpc := captureTestRpcFixture(fixture)
	config, directory := captureTestInputs(t, fixture, 0)
	ctx, cancel := context.WithCancel(context.Background())
	rpc.intercept = func(method string, _ []json.RawMessage) (any, error, bool) {
		if method == "chain_getBlock" {
			cancel()
			return nil, context.Canceled, true
		}
		return nil, nil, false
	}
	result, err := captureReceiptFinality(ctx, fixture.receipts.archive, fixture.collection, fixture.checkpoint, config, directory, rpc.configure)
	if !errors.Is(err, context.Canceled) || result != nil {
		t.Fatalf("canceled capture succeeded: %v", err)
	}
	if _, err := os.Stat(filepath.Join(directory, "request-00005.json")); err != nil {
		t.Fatal("request was not durably reserved before transport")
	}
	rpc.intercept = nil
	rpc.calls = nil
	result, err = captureReceiptFinality(context.Background(), fixture.receipts.archive, fixture.collection, fixture.checkpoint, config, directory, rpc.configure)
	if err != nil || result == nil {
		t.Fatalf("partial capture could not resume: %v", err)
	}
	if _, err := os.Stat(filepath.Join(directory, "request-00007.json")); err != nil {
		t.Fatal("restart reset its request budget")
	}
	for _, call := range rpc.calls {
		if strings.HasPrefix(call, "chain_getHeader:") {
			t.Fatal("restart lost a completed native header")
		}
	}
}

// More than three transient failures recover with the same selector and shared
// journal; no wall-clock sleep is used to trigger this retry sequence.
func TestReceiptFinalityCaptureRetriesTransientReadsWithDurableAttempts(t *testing.T) {
	fixture := receiptFinalityTestFixture(t)
	rpc := captureTestRpcFixture(fixture)
	config, directory := captureTestInputs(t, fixture, 0)
	failures := 0
	rpc.intercept = func(method string, params []json.RawMessage) (any, error, bool) {
		if failures < 7 {
			failures++
			if method != "chain_getBlockHash" || string(params[0]) != "0" {
				t.Fatal("retry changed genesis selector")
			}
			return nil, collectionTestTimeout{}, true
		}
		return nil, nil, false
	}
	result, err := captureReceiptFinality(context.Background(), fixture.receipts.archive, fixture.collection, fixture.checkpoint, config, directory, rpc.configure)
	if err != nil || result == nil || failures != 7 {
		t.Fatalf("transient failures became terminal: %v", err)
	}
	if _, err := os.Stat(filepath.Join(directory, "read-00012.json")); err != nil {
		t.Fatal("retries escaped durable request accounting")
	}
}

// Complete header bytes must reproduce their selectors before any partial
// header or final proof is admitted, even if the RPC labels them finalized.
func TestReceiptFinalityCaptureRejectsMismatchedHeaderBytes(t *testing.T) {
	fixture := receiptFinalityTestFixture(t)
	rpc := captureTestRpcFixture(fixture)
	config, directory := captureTestInputs(t, fixture, 0)
	rpc.intercept = func(method string, _ []json.RawMessage) (any, error, bool) {
		if method == "chain_getHeader" {
			header := captureTestHeaderJson(fixture.headers[3]).(map[string]any)
			header["stateRoot"] = "0x" + strings.Repeat("1", 64)
			return header, nil, true
		}
		return nil, nil, false
	}
	result, err := captureReceiptFinality(context.Background(), fixture.receipts.archive, fixture.collection, fixture.checkpoint, config, directory, rpc.configure)
	if err == nil || result != nil || !strings.Contains(err.Error(), "complete header hash") {
		t.Fatalf("bad header accepted: %v", err)
	}
	if _, err := os.Stat(filepath.Join(directory, "header-"+fixture.headers[3].identity.Hash[2:]+".json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("mismatched native bytes cached")
	}
}

// Config or checkpoint changes cannot reinterpret a previously owned journal.
func TestReceiptFinalityCaptureRejectsContextSubstitutionBeforeRpc(t *testing.T) {
	fixture := receiptFinalityTestFixture(t)
	rpc := captureTestRpcFixture(fixture)
	clear(rpc.certificates)
	config, directory := captureTestInputs(t, fixture, 0)
	_, _ = captureReceiptFinality(context.Background(), fixture.receipts.archive, fixture.collection, fixture.checkpoint, config, directory, rpc.configure)
	before, err := os.ReadFile(filepath.Join(directory, "capture.json"))
	if err != nil {
		t.Fatal(err)
	}
	config.RpcUrl = "http://replacement.example"
	result, err := captureReceiptFinality(context.Background(), fixture.receipts.archive, fixture.collection, fixture.checkpoint, config, directory, func(*receiptCollectorRpc) { t.Fatal("conflicting route reached RPC") })
	if err == nil || result != nil {
		t.Fatal("capture route substitution accepted")
	}
	after, _ := os.ReadFile(filepath.Join(directory, "capture.json"))
	if !bytes.Equal(before, after) {
		t.Fatal("original journal overwritten")
	}
}

// Unknown checkpoint authority cannot be synthesized from RPC or a new flag.
func TestReceiptFinalityCaptureRequiresCheckpointAndBoundsBeforeOwnership(t *testing.T) {
	fixture := receiptFinalityTestFixture(t)
	config, directory := captureTestInputs(t, fixture, 0)
	for _, checkpoint := range []*NativeFinalityCheckpoint{nil, {Schema: NativeFinalityCheckpointSchema, CodecProfile: NativeFinalityCodecProfile, Genesis: fixture.checkpoint.Genesis}} {
		result, err := captureReceiptFinality(context.Background(), fixture.receipts.archive, fixture.collection, checkpoint, config, directory, func(*receiptCollectorRpc) { t.Fatal("unknown authority reached RPC") })
		if err == nil || result != nil {
			t.Fatal("unknown authority accepted")
		}
	}
	config.MaximumDescendantHeaders = maximumNativeFinalityHeaders
	result, err := captureReceiptFinality(context.Background(), fixture.receipts.archive, fixture.collection, fixture.checkpoint, config, directory, func(*receiptCollectorRpc) { t.Fatal("unbounded config reached RPC") })
	if err == nil || result != nil {
		t.Fatal("unbounded capture accepted")
	}
	files, _ := os.ReadDir(directory)
	if len(files) != 0 {
		t.Fatal("invalid input acquired journal ownership")
	}
}

// Proof custody checks remain on the completed replay path as well as capture.
func TestReceiptFinalityCaptureRefusesExposedCompletedProof(t *testing.T) {
	fixture := receiptFinalityTestFixture(t)
	rpc := captureTestRpcFixture(fixture)
	config, directory := captureTestInputs(t, fixture, 0)
	result, err := captureReceiptFinality(context.Background(), fixture.receipts.archive, fixture.collection, fixture.checkpoint, config, directory, rpc.configure)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(result.Proof.Path, 0644); err != nil {
		t.Fatal(err)
	}
	result, err = captureReceiptFinality(context.Background(), fixture.receipts.archive, fixture.collection, fixture.checkpoint, config, directory, func(*receiptCollectorRpc) { t.Fatal("bad retained custody reached RPC") })
	if err == nil || result != nil {
		t.Fatal("exposed completed proof admitted")
	}
}
