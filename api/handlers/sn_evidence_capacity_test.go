// Fixed fixtures exercise transport capacity, signed identity and rejection
// boundaries without relying on timing or a live object store.
package handlers

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/startifact"
)

// A synthetic source need only have the transport schema: plan approval is
// independently verified by the simulator, never asserted by this endpoint.
func snEvidencePlanTestBytes(t *testing.T, padding int) []byte {
	t.Helper()
	value := struct {
		Schema       string `json:"schema"`
		DeploymentId string `json:"deployment_id"`
		PlanHash     string `json:"plan_hash"`
		Body         string `json:"body"`
	}{Schema: "urnetwork-sim-plan-v12", DeploymentId: "test-deployment", PlanHash: "0x" + strings.Repeat("11", 32), Body: strings.Repeat("x", padding)}
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// Produce exactly the canonical field order used by simulator publication.
func snEvidencePlanTestEnvelope(t *testing.T, kind, name string, data []byte) (*startifact.EvidenceEnvelope, []byte) {
	t.Helper()
	config, key := configureSnEvidenceHandler(t)
	envelope := signedSnEvidence(t, config, key)
	envelope.Kind = kind
	payload := snEvidenceFilePayload{Schema: snEvidenceSemanticFileSchema, RunId: envelope.RunID, Path: name, ContentHash: snEvidenceDigest(data), Size: uint64(len(data)), Data: data}
	if kind == snEvidenceCampaignFileKind {
		payload.Schema, payload.Scope = snEvidenceCampaignFileSchema, "run"
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	envelope.Payload = raw
	if err := startifact.SignEvidence(envelope, key); err != nil {
		t.Fatal(err)
	}
	wire, err := startifact.EvidenceBytes(envelope)
	if err != nil {
		t.Fatal(err)
	}
	return envelope, wire
}

// Exact keys expose immutable fixture bytes and owned read/close outcomes.
type snEvidencePlanTestStore struct {
	server.BlobStore
	objectKVs map[string][]byte
	closeErr  error
	closes    int
	reads     int
}

// The test owner keeps process-global handler replacement scoped to one test.
func snEvidencePlanTestInstallStore(t *testing.T, store *snEvidencePlanTestStore) {
	t.Helper()
	previous := loadSnEvidenceBlobStore
	loadSnEvidenceBlobStore = func() (server.BlobStore, bool) { return store, true }
	t.Cleanup(func() { loadSnEvidenceBlobStore = previous })
}

// No list operation or unrelated key can supply a selected content object.
func (self *snEvidencePlanTestStore) Get(_ context.Context, key string) (io.ReadCloser, error) {
	raw, found := self.objectKVs[key]
	if !found {
		return nil, errors.New("test evidence key missing")
	}
	self.reads++
	return &snEvidencePlanTestCloser{Reader: bytes.NewReader(raw), store: self}, nil
}

// Synthetic namespace names never refer to deployment resources.
func (self *snEvidencePlanTestStore) Prefix() string { return "synthetic-evidence" }

// The store counts every owned close, including failed authentication.
type snEvidencePlanTestCloser struct {
	io.Reader
	store *snEvidencePlanTestStore
}

// Close failures must reach the endpoint before response bytes are released.
func (self *snEvidencePlanTestCloser) Close() error {
	self.store.closes++
	return self.store.closeErr
}

// A deadline-recording writer proves per-request propagation without sleeps.
type snEvidencePlanTestResponse struct {
	*httptest.ResponseRecorder
	deadline time.Time
	err      error
}

// Production ResponseController reaches the actual connection through this
// same interface; tests retain the exact requested deadline.
func (self *snEvidencePlanTestResponse) SetWriteDeadline(deadline time.Time) error {
	self.deadline = deadline
	return self.err
}

// This crosses the former real64MiB failure at both public retrieval routes;
// the same bytes remain ineligible for the unchanged upload route.
func TestSnEvidenceLargePlanGetAndExactHistory(t *testing.T) {
	envelope, raw := snEvidencePlanTestEnvelope(t, snEvidenceSemanticFileKind, "final-derived/setup-plan.json", snEvidencePlanTestBytes(t, 48*1024*1024))
	if len(raw) <= maximumSnEvidenceBytes {
		t.Fatal("fixture does not cross the former public response limit")
	}
	store := &snEvidencePlanTestStore{objectKVs: map[string][]byte{}}
	contentKey, err := startifact.EvidenceContentKey(store, envelope.ContentHash)
	if err != nil {
		t.Fatal(err)
	}
	historyKey, err := startifact.EvidenceHistoryKey(store, envelope.DeploymentID, envelope.Netuid, envelope.Kind, envelope.RunID, envelope.ContentHash)
	if err != nil {
		t.Fatal(err)
	}
	store.objectKVs[contentKey], store.objectKVs[historyKey] = raw, raw
	snEvidencePlanTestInstallStore(t, store)
	response := &snEvidencePlanTestResponse{ResponseRecorder: httptest.NewRecorder()}
	SnEvidence(response, httptest.NewRequest(http.MethodGet, "/sn/evidence?hash="+url.QueryEscape(envelope.ContentHash), nil))
	if response.Code != http.StatusOK || !bytes.Equal(response.Body.Bytes(), raw) || store.closes != 1 {
		t.Fatalf("large evidence get status=%d bytes=%d closes=%d", response.Code, response.Body.Len(), store.closes)
	}
	query := url.Values{"deployment_id": {envelope.DeploymentID}, "netuid": {fmt.Sprint(envelope.Netuid)}, "kind": {envelope.Kind}, "run_id": {envelope.RunID}, "hash": {envelope.ContentHash}}
	history := &snEvidencePlanTestResponse{ResponseRecorder: httptest.NewRecorder()}
	SnEvidenceHistory(history, httptest.NewRequest(http.MethodGet, "/sn/evidence/history?"+query.Encode(), nil))
	var historyResult struct {
		Objects []server.BlobObject `json:"objects"`
	}
	if err := json.Unmarshal(history.Body.Bytes(), &historyResult); err != nil {
		t.Fatal(err)
	}
	if history.Code != http.StatusOK || len(historyResult.Objects) != 1 || historyResult.Objects[0].Key != historyKey || historyResult.Objects[0].Size != int64(len(raw)) || store.closes != 2 {
		t.Fatalf("large evidence history status=%d closes=%d body=%s", history.Code, store.closes, history.Body.String())
	}
	post := serveSnEvidence(http.MethodPost, "/sn/evidence", raw)
	if post.Code != http.StatusBadRequest || store.reads != 2 {
		t.Fatalf("large evidence post status=%d reads=%d", post.Code, store.reads)
	}
	if response.deadline.IsZero() || history.deadline.IsZero() || len(snEvidencePlanReadSlots) != 0 {
		t.Fatal("large request lost its deadline or retained its response slot")
	}
}

// Exact path and arithmetic boundaries need no giant allocation to exercise.
func TestSnEvidencePlanCapacityPathsAndBoundaries(t *testing.T) {
	digest := strings.Repeat("11", 32)
	for _, value := range []struct {
		path    string
		maximum uint64
	}{
		{path: "final-derived/setup-plan.json", maximum: snEvidencePlanBytes},
		{path: "final-derived/validator-activation-plan-" + digest + ".json", maximum: snEvidencePlanBytes},
		{path: "final-derived/historical-coordinator/plans/" + digest + ".json", maximum: snEvidencePlanBytes},
		{path: "final-derived/fleet-generation/renewal-17-approval.json", maximum: snEvidencePlanBytes},
		{path: "final-derived/fleet-generation-lineage.json", maximum: snEvidenceLineageBytes},
		{path: "final-derived/fleet-lifecycle-lineage.json", maximum: snEvidenceLineageBytes},
		{path: "final-inputs/bundles/plan-history-001-of-002.json", maximum: snEvidencePlanBundleBytes},
		{path: "final-inputs/bundles/launch-foundation.json", maximum: snEvidencePlanBundleBytes},
		{path: "final-inputs/prior-release/semantic-files/" + digest + ".plan.evidence.json", maximum: snEvidencePriorCarrierBytes},
	} {
		if actual := snEvidencePlanPathBytes(value.path); actual != value.maximum {
			t.Fatalf("path %s capacity=%d want=%d", value.path, actual, value.maximum)
		}
		kind := snEvidenceSemanticFileKind
		header := snEvidenceFileHeader{Schema: snEvidenceSemanticFileSchema, RunId: "synthetic-run", Path: value.path, ContentHash: "sha256:" + digest, Size: value.maximum}
		if strings.HasPrefix(value.path, "final-inputs/") {
			kind, header.Schema, header.Scope = snEvidenceCampaignFileKind, snEvidenceCampaignFileSchema, "run"
		}
		maximum, err := snEvidenceFileReadBytes(kind, header.RunId, header)
		if err != nil || maximum != ((value.maximum+2)/3*4)+snEvidenceCarrierOverhead {
			t.Fatalf("path %s exact-boundary=%d err=%v", value.path, maximum, err)
		}
		header.Size++
		if _, err := snEvidenceFileReadBytes(kind, header.RunId, header); err == nil {
			t.Fatalf("path %s admitted one-over raw size", value.path)
		}
	}
	for _, name := range []string{
		"final-derived/ordinary.json", "final-derived/setup-plan.json/extra", "final-derived/../setup-plan.json",
		"final-derived/fleet-generation/renewal-01-approval.json", "final-derived/fleet-generation/renewal-0-approval.json",
		"final-inputs/bundles/plan-history-extra.json", "final-inputs/bundles/plan-history-1-of-2.json", "final-inputs/bundles/plan-history-000-of-002.json", "final-inputs/bundles/plan-history-003-of-002.json", "final-inputs/bundles/plan-history-001-of-001.json",
		"final-derived/validator-activation-plan-" + strings.Repeat("AA", 32) + ".json",
		"final-inputs/prior-release/semantic-files/" + digest + ".evidence.json",
	} {
		if snEvidencePlanPathBytes(name) != 0 {
			t.Fatalf("alias or ordinary path acquired plan capacity: %s", name)
		}
	}
}

// Payload headers are bounded independently of claimed data size and cannot
// acquire capacity by field duplication, kind confusion or reordered data.
func TestSnEvidencePlanHeaderRejectsInvalidClaims(t *testing.T) {
	envelope, raw := snEvidencePlanTestEnvelope(t, snEvidenceSemanticFileKind, "final-derived/setup-plan.json", snEvidencePlanTestBytes(t, 0))
	maximum, err := readSnEvidencePlanHeader(json.NewDecoder(bytes.NewReader(raw)))
	if err != nil || maximum == 0 {
		t.Fatalf("valid header: %d %v", maximum, err)
	}
	for _, invalid := range [][]byte{
		bytes.Replace(raw, []byte(`"path":"final-derived/setup-plan.json"`), []byte(`"path":"final-derived/setup-plan.json","path":"final-derived/setup-plan.json"`), 1),
		bytes.Replace(raw, []byte(`"kind":"scenario-semantic-file"`), []byte(`"kind":"ordinary-proof"`), 1),
		bytes.Replace(raw, []byte(`"schema":"urnetwork-final-semantic-supplement-file-v1"`), []byte(`"schema":"ordinary-proof"`), 1),
		bytes.Replace(raw, []byte(`"size":`), []byte(`"data":"","size":`), 1),
	} {
		if _, err := readSnEvidencePlanHeader(json.NewDecoder(bytes.NewReader(invalid))); err == nil {
			t.Fatal("invalid header acquired plan capacity")
		}
	}
	var file snEvidenceFilePayload
	if err := json.Unmarshal(envelope.Payload, &file); err != nil {
		t.Fatal(err)
	}
	file.Size = snEvidencePlanBytes + 1
	payload, err := json.Marshal(file)
	if err != nil {
		t.Fatal(err)
	}
	envelope.Payload = payload
	invalid, err := json.Marshal(envelope)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := readSnEvidenceEnvelope(t.Context(), bytes.NewReader(invalid)); !errors.Is(err, errSnEvidencePlanHeader) {
		t.Fatalf("over-capacity claim did not fail before payload read: %v", err)
	}
}

// A signature alone cannot admit a different inner hash, size or typed body.
func TestSnEvidencePlanFileRejectsChangedSources(t *testing.T) {
	envelope, _ := snEvidencePlanTestEnvelope(t, snEvidenceSemanticFileKind, "final-derived/setup-plan.json", snEvidencePlanTestBytes(t, 0))
	if err := validateSnEvidencePlanFile(envelope); err != nil {
		t.Fatal(err)
	}
	var file snEvidenceFilePayload
	if err := json.Unmarshal(envelope.Payload, &file); err != nil {
		t.Fatal(err)
	}
	for _, change := range []func(*snEvidenceFilePayload){
		func(value *snEvidenceFilePayload) { value.Size++ },
		func(value *snEvidenceFilePayload) { value.ContentHash = "sha256:" + strings.Repeat("22", 32) },
		func(value *snEvidenceFilePayload) { value.Path = "final-derived/ordinary-proof.json" },
		func(value *snEvidenceFilePayload) { value.Schema = "ordinary-proof" },
		func(value *snEvidenceFilePayload) { value.RunId = "another-run" },
		func(value *snEvidenceFilePayload) {
			value.Data = []byte(`{"schema":"ordinary-proof"}`)
			value.Size = uint64(len(value.Data))
			value.ContentHash = snEvidenceDigest(value.Data)
		},
	} {
		changed := file
		change(&changed)
		raw, err := json.Marshal(changed)
		if err != nil {
			t.Fatal(err)
		}
		copyEnvelope := *envelope
		copyEnvelope.Payload = raw
		if err := validateSnEvidencePlanFile(&copyEnvelope); err == nil {
			t.Fatal("changed typed source accepted")
		}
	}
	duplicate := *envelope
	duplicate.Payload = bytes.Replace(envelope.Payload, []byte(`"path":`), []byte(`"path":"final-derived/setup-plan.json","path":`), 1)
	if err := validateSnEvidencePlanFile(&duplicate); err == nil {
		t.Fatal("duplicate payload routing field accepted")
	}
}

// Both compound families authenticate embedded original hashes and do not
// grant the outer large allowance to an ordinary source slot.
func TestSnEvidencePlanCompoundSourceValidation(t *testing.T) {
	envelope, _ := snEvidencePlanTestEnvelope(t, snEvidenceSemanticFileKind, "final-derived/setup-plan.json", snEvidencePlanTestBytes(t, 0))
	data := snEvidencePlanTestBytes(t, 0)
	file := snEvidenceSourceFile{Path: "plan.json", ContentHash: snEvidenceDigest(data), SizeBytes: uint64(len(data)), Data: data}
	bundle := struct {
		Schema string                       `json:"schema"`
		Name   string                       `json:"name"`
		Files  []snEvidenceBundleSourceFile `json:"files"`
	}{Schema: "urnetwork-final-collected-file-bundle-v1", Name: "launch-foundation", Files: []snEvidenceBundleSourceFile{{Path: file.Path, ContentHash: file.ContentHash, SizeBytes: file.SizeBytes, Data: file.Data}}}
	raw, err := json.Marshal(bundle)
	if err != nil {
		t.Fatal(err)
	}
	if err := validateSnEvidencePlanBody("final-inputs/bundles/launch-foundation.json", raw, envelope, true); err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(raw, []byte(`"bytes":`)) {
		t.Fatal("bundle fixture lost the original source byte-count field")
	}
	if err := validateSnEvidencePlanBody("final-inputs/bundles/launch-foundation.json", bytes.Replace(raw, []byte(`"bytes":`), []byte(`"size_bytes":`), 1), envelope, true); err == nil {
		t.Fatal("bundle admitted a lineage byte-count alias")
	}
	bundle.Files = append(bundle.Files, snEvidenceBundleSourceFile{Path: "zero.log", ContentHash: snEvidenceDigest(nil)})
	raw, err = json.Marshal(bundle)
	if err != nil {
		t.Fatal(err)
	}
	if err := validateSnEvidencePlanBody("final-inputs/bundles/launch-foundation.json", raw, envelope, true); err != nil {
		t.Fatalf("empty ordinary bundle entry lost original compatibility: %v", err)
	}
	bundle.Files[0].ContentHash = "sha256:" + strings.Repeat("22", 32)
	raw, err = json.Marshal(bundle)
	if err != nil {
		t.Fatal(err)
	}
	if err := validateSnEvidencePlanBody("final-inputs/bundles/launch-foundation.json", raw, envelope, true); err == nil {
		t.Fatal("changed compound source accepted")
	}
	file.Path = "launch-foundation/plan.json"
	lineage := struct {
		Schema       string                 `json:"schema"`
		DeploymentId string                 `json:"deployment_id"`
		PlanHash     string                 `json:"plan_hash"`
		Files        []snEvidenceSourceFile `json:"files"`
	}{Schema: "urnetwork-final-fleet-generation-lineage-v1", DeploymentId: envelope.DeploymentID, PlanHash: "0x" + strings.Repeat("11", 32), Files: []snEvidenceSourceFile{file}}
	raw, err = json.Marshal(lineage)
	if err != nil {
		t.Fatal(err)
	}
	if err := validateSnEvidencePlanBody("final-derived/fleet-generation-lineage.json", raw, envelope, true); err != nil {
		t.Fatal(err)
	}
	lineage.Files = append(lineage.Files, file)
	raw, err = json.Marshal(lineage)
	if err != nil {
		t.Fatal(err)
	}
	if err := validateSnEvidencePlanBody("final-derived/fleet-generation-lineage.json", raw, envelope, true); err == nil {
		t.Fatal("duplicate lineage source accepted")
	}
}

// A reader may return useful bytes and a terminal failure together. Header
// decoder read-ahead must not turn that failure into a successful later read.
type snEvidencePlanTerminalReader struct {
	raw []byte
	err error
}

// The first read returns both outcomes; subsequent reads cannot repair it.
func (self *snEvidencePlanTerminalReader) Read(value []byte) (int, error) {
	count := copy(value, self.raw)
	self.raw = self.raw[count:]
	if len(self.raw) == 0 {
		return count, self.err
	}
	return count, nil
}

// Preservation is checked with a deterministic final data/error read rather
// than a timeout or a scheduled cancellation.
func TestSnEvidencePlanHeaderPreservesDataAndReadFailure(t *testing.T) {
	_, raw := snEvidencePlanTestEnvelope(t, snEvidenceSemanticFileKind, "final-derived/setup-plan.json", snEvidencePlanTestBytes(t, 0))
	sentinel := errors.New("synthetic terminal read failure")
	reader := &snEvidencePlanTerminalReader{raw: raw, err: sentinel}
	if _, _, err := readSnEvidenceEnvelope(t.Context(), reader); !errors.Is(err, sentinel) {
		t.Fatalf("header read lost its terminal error: %v", err)
	}
}

// A synthetic source supplies an exact byte count without a giant fixture.
type snEvidencePlanCountingReader struct {
	remaining int
	reads     int
	bytes     int
	empty     bool
}

// Deterministic finite input proves the default boundary and empty-read cap.
func (self *snEvidencePlanCountingReader) Read(value []byte) (int, error) {
	self.reads++
	if self.empty {
		return 0, nil
	}
	if self.remaining == 0 {
		return 0, io.EOF
	}
	count := min(self.remaining, len(value))
	for index := range value[:count] {
		value[index] = 'x'
	}
	self.remaining -= count
	self.bytes += count
	return count, nil
}

// Unrecognized headers cannot allocate beyond the original public owner;
// neither a stalled reader nor an ordinary blob can masquerade as a plan.
func TestSnEvidencePlanReaderKeepsOrdinaryAndProgressBounds(t *testing.T) {
	reader := &snEvidencePlanCountingReader{remaining: maximumSnEvidenceBytes + 32}
	if _, _, err := readSnEvidenceEnvelope(t.Context(), reader); err == nil || !strings.Contains(err.Error(), "exceeds byte limit") || reader.bytes != maximumSnEvidenceBytes+1 {
		t.Fatalf("ordinary boundary bytes=%d err=%v", reader.bytes, err)
	}
	stalled := &snEvidencePlanCountingReader{empty: true}
	if _, _, err := readSnEvidenceEnvelope(t.Context(), stalled); !errors.Is(err, io.ErrNoProgress) || stalled.reads != maximumSnEvidenceEmptyReads {
		t.Fatalf("stalled read count=%d err=%v", stalled.reads, err)
	}
}

// The endpoint authenticates original signatures independently of typed size
// admission and never emits a truncated or modified immutable envelope.
func TestSnEvidencePlanReaderRejectsTruncationAndSignatureChanges(t *testing.T) {
	_, raw := snEvidencePlanTestEnvelope(t, snEvidenceSemanticFileKind, "final-derived/setup-plan.json", snEvidencePlanTestBytes(t, 0))
	for _, invalid := range [][]byte{
		raw[:len(raw)-1],
		bytes.Replace(raw, []byte(`"signature":"0x`), []byte(`"signature":"0xf`), 1),
		bytes.Replace(raw, []byte(`"payload":`), []byte(`"kind":"scenario-semantic-file","payload":`), 1),
	} {
		if accepted, _, err := readSnEvidenceEnvelope(t.Context(), bytes.NewReader(invalid)); err == nil || accepted != nil {
			t.Fatal("truncated or changed evidence accepted")
		}
	}
}

// Deadline arithmetic and parent selection are checked directly, without
// waiting for timeouts or relying on scheduler speed.
func TestSnEvidencePlanReadOperationDeadlineAndBounds(t *testing.T) {
	if snEvidencePlanReadDuration(8*1024*1024) != 121*time.Second || snEvidencePlanReadDuration(8*1024*1024+1) != 122*time.Second || snEvidencePlanReadDuration(20*1024*1024*1024) != 20*time.Minute {
		t.Fatal("large evidence lifetime is not finitely byte-scaled")
	}
	parentDeadline := time.Now().Add(time.Minute)
	ctx, cancel := context.WithDeadline(t.Context(), parentDeadline)
	defer cancel()
	writer := &snEvidencePlanTestResponse{ResponseRecorder: httptest.NewRecorder()}
	operation := &snEvidenceReadOperation{writer: writer}
	defer operation.close()
	owned, err := operation.admit(ctx, snEvidenceLineageBytes)
	if err != nil {
		t.Fatal(err)
	}
	deadline, exists := owned.Deadline()
	if !exists || deadline != parentDeadline || writer.deadline != parentDeadline || len(snEvidencePlanReadSlots) != 1 {
		t.Fatal("large operation ignored its parent or response ownership")
	}
	second := &snEvidenceReadOperation{writer: writer}
	defer second.close()
	if _, err := second.admit(t.Context(), snEvidenceLineageBytes); !errors.Is(err, errSnEvidencePlanReadBusy) {
		t.Fatalf("second large read was not refused: %v", err)
	}
	ordinary := &snEvidenceReadOperation{writer: &snEvidencePlanTestResponse{ResponseRecorder: httptest.NewRecorder()}}
	if unchanged, err := ordinary.admit(ctx, maximumSnEvidenceBytes); err != nil || unchanged != ctx || ordinary.active {
		t.Fatal("ordinary evidence borrowed a large deadline or owner")
	}
	operation.close()
	operation.close()
	if len(snEvidencePlanReadSlots) != 0 || !errors.Is(owned.Err(), context.Canceled) {
		t.Fatal("operation close retained a slot or a live child context")
	}
}

// Failed response-control admission returns its token through the same owner
// as successful reads, so a broken writer cannot pin every future recovery.
func TestSnEvidencePlanReadOperationReleasesFailedDeadline(t *testing.T) {
	sentinel := errors.New("synthetic deadline failure")
	writer := &snEvidencePlanTestResponse{ResponseRecorder: httptest.NewRecorder(), err: sentinel}
	operation := &snEvidenceReadOperation{writer: writer}
	defer operation.close()
	if _, err := operation.admit(t.Context(), snEvidenceLineageBytes); !errors.Is(err, sentinel) {
		t.Fatalf("write-deadline failure lost: %v", err)
	}
	operation.close()
	if len(snEvidencePlanReadSlots) != 0 {
		t.Fatal("write-deadline refusal retained the large owner")
	}
}

// The source reaches an explicit blocked read only after header admission;
// cancellation must close that read rather than waiting for another chunk.
type snEvidencePlanBlockedReader struct {
	prefix  []byte
	entered chan struct{}
	closed  chan struct{}
	closes  atomic.Int32
	err     error
}

// A channel barrier exposes the exact cancellation interleaving to the test.
func (self *snEvidencePlanBlockedReader) Read(value []byte) (int, error) {
	if len(self.prefix) != 0 {
		count := copy(value, self.prefix)
		self.prefix = self.prefix[count:]
		return count, nil
	}
	close(self.entered)
	<-self.closed
	return 0, io.ErrClosedPipe
}

// One owner interrupts the read and preserves its actual terminal close error.
func (self *snEvidencePlanBlockedReader) Close() error {
	if self.closes.Add(1) == 1 {
		close(self.closed)
	}
	return self.err
}

// A derived lifetime must interrupt the already opened source and join Close;
// checking context only between reads would hang at this exact barrier.
func TestSnEvidencePlanAdmittedCancellationInterruptsAndJoinsSource(t *testing.T) {
	sentinel := errors.New("synthetic source close failure")
	reader := &snEvidencePlanBlockedReader{prefix: snEvidencePlanTestHeader(), entered: make(chan struct{}), closed: make(chan struct{}), err: sentinel}
	operationCtx, cancel := context.WithCancel(t.Context())
	defer cancel()
	result := make(chan error, 1)
	go func() {
		_, _, err := readSnEvidenceEnvelopeWithAdmission(t.Context(), reader, func(context.Context, uint64) (context.Context, error) { return operationCtx, nil })
		result <- err
	}()
	<-reader.entered
	cancel()
	err := <-result
	if !errors.Is(err, context.Canceled) || !errors.Is(err, sentinel) || reader.closes.Load() != 1 {
		t.Fatalf("admitted cancellation err=%v closes=%d", err, reader.closes.Load())
	}
}

// The fixed prefix ends before the body and claims the real maximum plan size.
func snEvidencePlanTestHeader() []byte {
	return []byte(fmt.Sprintf(`{"schema":"urnetwork-release-evidence-v1","deployment_id":"test-deployment","chain_id":945,"genesis_hash":"0x%s","netuid":7,"kind":"scenario-semantic-file","run_id":"synthetic-run","created_at":"2020-01-01T00:00:00Z","payload":{"schema":"urnetwork-final-semantic-supplement-file-v1","run_id":"synthetic-run","path":"final-derived/setup-plan.json","content_hash":"sha256:%s","size":%d,"data":`, strings.Repeat("11", 32), strings.Repeat("22", 32), snEvidencePlanBytes))
}

// Refusal must keep the parent context for cleanup, even when the admitting
// owner correctly returns no derived context alongside its error.
func TestSnEvidencePlanAdmissionRefusalClosesWithoutLosingParent(t *testing.T) {
	reader := &snEvidencePlanBlockedReader{prefix: snEvidencePlanTestHeader(), entered: make(chan struct{}), closed: make(chan struct{})}
	_, _, err := readSnEvidenceEnvelopeWithAdmission(t.Context(), reader, func(context.Context, uint64) (context.Context, error) { return nil, errSnEvidencePlanReadBusy })
	if !errors.Is(err, errSnEvidencePlanReadBusy) || reader.closes.Load() != 1 {
		t.Fatalf("admission refusal err=%v closes=%d", err, reader.closes.Load())
	}
	select {
	case <-reader.entered:
		t.Fatal("refused admission consumed the body")
	default:
	}
}

// Completed-prior carriers have a separate suffix and exactly one signed
// level, so arbitrary prior files cannot borrow a lineage owner's capacity.
func TestSnEvidencePriorPlanCarrierIdentityAndPath(t *testing.T) {
	inner, raw := snEvidencePlanTestEnvelope(t, snEvidenceSemanticFileKind, "final-derived/setup-plan.json", snEvidencePlanTestBytes(t, 0))
	outer := *inner
	outer.Kind, outer.RunID = snEvidenceCampaignFileKind, "successor-run"
	digest := sha256.Sum256([]byte("final-derived/setup-plan.json"))
	name := "final-inputs/prior-release/semantic-files/" + hex.EncodeToString(digest[:]) + ".plan.evidence.json"
	if err := validateSnEvidencePlanBody(name, raw, &outer, true); err != nil {
		t.Fatal(err)
	}
	if err := validateSnEvidencePlanBody(name, raw, &outer, false); err == nil {
		t.Fatal("nested prior carrier accepted")
	}
	if err := validateSnEvidencePlanBody(strings.Replace(name, hex.EncodeToString(digest[:]), strings.Repeat("22", 32), 1), raw, &outer, true); err == nil {
		t.Fatal("changed prior path accepted")
	}
	outer.DeploymentID = "another-deployment"
	if err := validateSnEvidencePlanBody(name, raw, &outer, true); err == nil {
		t.Fatal("foreign prior authority accepted")
	}
}

// Parent cancellation and owned Close errors remain failures at both routes.
func TestSnEvidencePlanReadCancellationAndCloseFailure(t *testing.T) {
	envelope, raw := snEvidencePlanTestEnvelope(t, snEvidenceSemanticFileKind, "final-derived/setup-plan.json", snEvidencePlanTestBytes(t, 0))
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, _, err := readSnEvidenceEnvelope(ctx, bytes.NewReader(raw)); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled evidence read: %v", err)
	}
	store := &snEvidencePlanTestStore{objectKVs: map[string][]byte{}, closeErr: errors.New("synthetic close failure")}
	key, err := startifact.EvidenceContentKey(store, envelope.ContentHash)
	if err != nil {
		t.Fatal(err)
	}
	store.objectKVs[key] = raw
	snEvidencePlanTestInstallStore(t, store)
	response := serveSnEvidence(http.MethodGet, "/sn/evidence?hash="+url.QueryEscape(envelope.ContentHash), nil)
	if response.Code != http.StatusBadGateway || store.closes != 1 {
		t.Fatalf("close error response=%d closes=%d", response.Code, store.closes)
	}
	request := httptest.NewRequest(http.MethodGet, "/sn/evidence?hash="+url.QueryEscape(envelope.ContentHash), nil).WithContext(ctx)
	response = httptest.NewRecorder()
	SnEvidence(response, request)
	if response.Code != http.StatusBadGateway || store.closes != 2 {
		t.Fatalf("canceled response=%d closes=%d", response.Code, store.closes)
	}
}
