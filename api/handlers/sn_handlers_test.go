package handlers

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/crypto"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/controller"
	"github.com/urnetwork/server/startifact"
)

func configureSnEvidenceHandler(t *testing.T) (*controller.StConfig, *ecdsa.PrivateKey) {
	t.Helper()
	artifactKey, err := crypto.HexToECDSA("0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef")
	if err != nil {
		t.Fatal(err)
	}
	var genesisHash [32]byte
	for i := range genesisHash {
		genesisHash[i] = 0x11
	}
	config := &controller.StConfig{
		Enabled:      true,
		ChainId:      945,
		GenesisHash:  genesisHash,
		DeploymentId: "test-deployment",
		Netuid:       7,
		ArtifactKey:  artifactKey,
	}
	controller.SetStConfig(config)
	t.Cleanup(func() { controller.SetStConfig(nil) })
	root := t.TempDir()
	popBlobConfig := server.Vault.PushSimpleResource("minio.yml", []byte(fmt.Sprintf(
		"authority: local\npath: %s\nprefix: blob\nmax_bytes: %d\n",
		root,
		16*1024*1024,
	)))
	t.Cleanup(popBlobConfig)
	return config, artifactKey
}

func signedSnEvidence(t *testing.T, config *controller.StConfig, artifactKey *ecdsa.PrivateKey) *startifact.EvidenceEnvelope {
	t.Helper()
	evidence := &startifact.EvidenceEnvelope{
		DeploymentID: config.DeploymentId,
		ChainID:      config.ChainId,
		GenesisHash:  fmt.Sprintf("0x%x", config.GenesisHash),
		Netuid:       uint16(config.Netuid),
		Kind:         "scenario-result",
		RunID:        "run-1",
		CreatedAt:    time.Unix(1_700_000_000, 0).UTC().Format(time.RFC3339Nano),
		Payload:      json.RawMessage(`{"result":"pass"}`),
	}
	if err := startifact.SignEvidence(evidence, artifactKey); err != nil {
		t.Fatal(err)
	}
	return evidence
}

func serveSnEvidence(method string, target string, body []byte) *httptest.ResponseRecorder {
	request := httptest.NewRequest(method, target, bytes.NewReader(body))
	response := httptest.NewRecorder()
	SnEvidence(response, request)
	return response
}

func serveSnEvidenceHistory(target string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(http.MethodGet, target, nil)
	response := httptest.NewRecorder()
	SnEvidenceHistory(response, request)
	return response
}

// snEvidenceRaceStore gives both the obsolete List/Put sequence and the new
// PutIfAbsent path the same two-party content-key barrier.
type snEvidenceRaceStore struct {
	stateLock    sync.Mutex
	prefix       string
	objects      map[string][]byte
	raceKey      string
	raceArrivals int
	raceReady    chan struct{}
}

func newSnEvidenceRaceStore() *snEvidenceRaceStore {
	return &snEvidenceRaceStore{
		prefix:    "blob",
		objects:   map[string][]byte{},
		raceReady: make(chan struct{}),
	}
}

func (self *snEvidenceRaceStore) waitAtRace(ctx context.Context) error {
	self.stateLock.Lock()
	self.raceArrivals++
	if self.raceArrivals == 2 {
		close(self.raceReady)
	}
	ready := self.raceReady
	self.stateLock.Unlock()
	select {
	case <-ready:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (self *snEvidenceRaceStore) Put(ctx context.Context, key string, localPath string, contentType string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	content, err := os.ReadFile(localPath)
	if err != nil {
		return err
	}
	self.stateLock.Lock()
	self.objects[key] = append([]byte(nil), content...)
	self.stateLock.Unlock()
	return nil
}

func (self *snEvidenceRaceStore) PutIfAbsent(ctx context.Context, key string, localPath string, contentType string) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	content, err := os.ReadFile(localPath)
	if err != nil {
		return false, err
	}
	if key == self.raceKey {
		if err := self.waitAtRace(ctx); err != nil {
			return false, err
		}
	}
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if _, ok := self.objects[key]; ok {
		return false, nil
	}
	self.objects[key] = append([]byte(nil), content...)
	return true, nil
}

func (self *snEvidenceRaceStore) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	content, ok := self.objects[key]
	if !ok {
		return nil, fmt.Errorf("object %s does not exist", key)
	}
	return io.NopCloser(bytes.NewReader(append([]byte(nil), content...))), nil
}

func (self *snEvidenceRaceStore) List(ctx context.Context, keyPrefix string) ([]server.BlobObject, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	self.stateLock.Lock()
	objects := []server.BlobObject{}
	for key, content := range self.objects {
		if strings.HasPrefix(key, keyPrefix) {
			objects = append(objects, server.BlobObject{Key: key, Size: int64(len(content))})
		}
	}
	self.stateLock.Unlock()
	if keyPrefix == self.raceKey {
		if err := self.waitAtRace(ctx); err != nil {
			return nil, err
		}
	}
	sort.Slice(objects, func(i, j int) bool { return objects[i].Key < objects[j].Key })
	return objects, nil
}

func (self *snEvidenceRaceStore) SetLifecycle(context.Context, []server.BlobLifecycleRule) error {
	return nil
}

func (self *snEvidenceRaceStore) Bucket() string    { return "race-bucket" }
func (self *snEvidenceRaceStore) Prefix() string    { return self.prefix }
func (self *snEvidenceRaceStore) Authority() string { return "race" }

func TestPayoutArtifactHistoryPrefixScopesExactEpochAndOperator(t *testing.T) {
	got, err := payoutArtifactHistoryPrefix("blob/operator-1", "ur-subnet-testnet-v1", "521", "0", "2")
	if err != nil {
		t.Fatal(err)
	}
	want := "blob/operator-1/st/v1/history/ur-subnet-testnet-v1/521/0/2/"
	if got != want {
		t.Fatalf("history prefix = %q, want %q", got, want)
	}
	all, err := payoutArtifactHistoryPrefix("blob/operator-1", "ur-subnet-testnet-v1", "521", "", "")
	if err != nil || all != "blob/operator-1/st/v1/history/ur-subnet-testnet-v1/521/" {
		t.Fatalf("deployment history prefix = %q, %v", all, err)
	}
}

func TestPayoutArtifactHistoryPrefixRejectsAmbiguousAndUnsafeFilters(t *testing.T) {
	tests := []struct {
		deployment string
		netuid     string
		epoch      string
		noID       string
	}{
		{deployment: "../other", netuid: "521"},
		{deployment: "release", netuid: "not-a-number"},
		{deployment: "release", netuid: "0"},
		{deployment: "release", netuid: "521", noID: "1"},
		{deployment: "release", netuid: "521", epoch: "x"},
		{deployment: "release", netuid: "521", epoch: "0", noID: "0"},
	}
	for _, test := range tests {
		if got, err := payoutArtifactHistoryPrefix("blob", test.deployment, test.netuid, test.epoch, test.noID); err == nil {
			t.Errorf("unsafe history filter produced %q for %+v", got, test)
		}
	}
}

func TestSnEvidencePostGetAndHistoryRoundTrip(t *testing.T) {
	config, artifactKey := configureSnEvidenceHandler(t)
	evidence := signedSnEvidence(t, config, artifactKey)
	encoded, err := json.Marshal(evidence)
	if err != nil {
		t.Fatal(err)
	}
	expected, err := startifact.EvidenceBytes(evidence)
	if err != nil {
		t.Fatal(err)
	}

	post := serveSnEvidence(http.MethodPost, "/sn/evidence", encoded)
	if post.Code != http.StatusOK {
		t.Fatalf("POST status = %d, body = %s", post.Code, post.Body.String())
	}
	var published startifact.Published
	if err := json.Unmarshal(post.Body.Bytes(), &published); err != nil {
		t.Fatal(err)
	}
	if published.ContentHash != evidence.ContentHash || published.ContentKey == "" || published.HistoryKey == "" {
		t.Fatalf("published = %+v", published)
	}
	idempotent := serveSnEvidence(http.MethodPost, "/sn/evidence", encoded)
	if idempotent.Code != http.StatusOK {
		t.Fatalf("idempotent POST status = %d, body = %s", idempotent.Code, idempotent.Body.String())
	}
	var republished startifact.Published
	if err := json.Unmarshal(idempotent.Body.Bytes(), &republished); err != nil {
		t.Fatal(err)
	}
	if republished != published {
		t.Fatalf("republished = %+v, want %+v", republished, published)
	}

	get := serveSnEvidence(http.MethodGet, "/sn/evidence?hash="+url.QueryEscape(evidence.ContentHash), nil)
	if get.Code != http.StatusOK || !bytes.Equal(get.Body.Bytes(), expected) {
		t.Fatalf("GET status = %d, body = %s", get.Code, get.Body.String())
	}
	if get.Header().Get("ETag") != `"`+evidence.ContentHash+`"` {
		t.Fatalf("GET ETag = %q", get.Header().Get("ETag"))
	}
	history := serveSnEvidenceHistory(
		"/sn/evidence/history?deployment_id=" + url.QueryEscape(evidence.DeploymentID) +
			"&netuid=7&kind=" + url.QueryEscape(evidence.Kind),
	)
	if history.Code != http.StatusOK {
		t.Fatalf("history status = %d, body = %s", history.Code, history.Body.String())
	}
	var listing struct {
		Schema  string              `json:"schema"`
		Objects []server.BlobObject `json:"objects"`
	}
	if err := json.Unmarshal(history.Body.Bytes(), &listing); err != nil {
		t.Fatal(err)
	}
	if listing.Schema != "urnetwork-release-evidence-history-v1" || len(listing.Objects) != 1 ||
		listing.Objects[0].Key != published.HistoryKey || listing.Objects[0].Size != int64(len(expected)) {
		t.Fatalf("history = %+v", listing)
	}
}

func TestSnEvidencePostRequiresConfiguredSigner(t *testing.T) {
	config, _ := configureSnEvidenceHandler(t)
	foreignKey, err := crypto.HexToECDSA("1123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef")
	if err != nil {
		t.Fatal(err)
	}
	evidence := signedSnEvidence(t, config, foreignKey)
	encoded, err := json.Marshal(evidence)
	if err != nil {
		t.Fatal(err)
	}
	post := serveSnEvidence(http.MethodPost, "/sn/evidence", encoded)
	if post.Code != http.StatusBadRequest {
		t.Fatalf("foreign signer POST status = %d, body = %s", post.Code, post.Body.String())
	}
	store, ok := server.LoadBlobStore()
	if !ok {
		t.Fatal("configured test blob store is unavailable")
	}
	objects, err := store.List(context.Background(), "blob/st/v1/evidence/")
	if err != nil {
		t.Fatal(err)
	}
	if len(objects) != 0 {
		t.Fatalf("unauthorized evidence was stored: %+v", objects)
	}
}

func TestSnEvidenceConflictDoesNotRepairCorruptWinner(t *testing.T) {
	config, artifactKey := configureSnEvidenceHandler(t)
	evidence := signedSnEvidence(t, config, artifactKey)
	encoded, err := json.Marshal(evidence)
	if err != nil {
		t.Fatal(err)
	}
	post := serveSnEvidence(http.MethodPost, "/sn/evidence", encoded)
	if post.Code != http.StatusOK {
		t.Fatalf("POST status = %d, body = %s", post.Code, post.Body.String())
	}
	var published startifact.Published
	if err := json.Unmarshal(post.Body.Bytes(), &published); err != nil {
		t.Fatal(err)
	}
	store, ok := server.LoadBlobStore()
	if !ok {
		t.Fatal("configured test blob store is unavailable")
	}
	corrupt := []byte(`{"corrupt":true}`)
	corruptPath := filepath.Join(t.TempDir(), "corrupt.json")
	if err := os.WriteFile(corruptPath, corrupt, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := store.Put(context.Background(), published.ContentKey, corruptPath, "application/json"); err != nil {
		t.Fatal(err)
	}

	conflict := serveSnEvidence(http.MethodPost, "/sn/evidence", encoded)
	if conflict.Code != http.StatusBadRequest {
		t.Fatalf("conflicting POST status = %d, body = %s", conflict.Code, conflict.Body.String())
	}
	reader, err := store.Get(context.Background(), published.ContentKey)
	if err != nil {
		t.Fatal(err)
	}
	stored, readErr := io.ReadAll(reader)
	closeErr := reader.Close()
	if readErr != nil || closeErr != nil || !bytes.Equal(stored, corrupt) {
		t.Fatalf("stored conflict winner = %q, read error = %v, close error = %v", stored, readErr, closeErr)
	}
	get := serveSnEvidence(http.MethodGet, "/sn/evidence?hash="+url.QueryEscape(evidence.ContentHash), nil)
	if get.Code != http.StatusBadGateway {
		t.Fatalf("corrupt GET status = %d, body = %s", get.Code, get.Body.String())
	}
}

func TestSnEvidenceConcurrentConflictingEncodingsKeepOneWinner(t *testing.T) {
	config, artifactKey := configureSnEvidenceHandler(t)
	withPrefix := signedSnEvidence(t, config, artifactKey)
	withoutPrefix := *withPrefix
	withoutPrefix.Signature = strings.TrimPrefix(withoutPrefix.Signature, "0x")
	if err := startifact.VerifyEvidence(&withoutPrefix); err != nil {
		t.Fatal(err)
	}
	evidenceValues := []*startifact.EvidenceEnvelope{withPrefix, &withoutPrefix}
	encodedValues := make([][]byte, len(evidenceValues))
	canonicalValues := make([][]byte, len(evidenceValues))
	for i, evidence := range evidenceValues {
		var err error
		encodedValues[i], err = json.Marshal(evidence)
		if err != nil {
			t.Fatal(err)
		}
		canonicalValues[i], err = startifact.EvidenceBytes(evidence)
		if err != nil {
			t.Fatal(err)
		}
	}
	if bytes.Equal(canonicalValues[0], canonicalValues[1]) {
		t.Fatal("conflicting encodings unexpectedly have identical stored bytes")
	}

	store := newSnEvidenceRaceStore()
	raceKey, err := startifact.EvidenceContentKey(store, withPrefix.ContentHash)
	if err != nil {
		t.Fatal(err)
	}
	store.raceKey = raceKey
	previousPublish := publishSnEvidence
	previousLoad := loadSnEvidenceBlobStore
	publishSnEvidence = func(ctx context.Context, encoded []byte) (*startifact.Published, error) {
		var evidence startifact.EvidenceEnvelope
		if err := json.Unmarshal(encoded, &evidence); err != nil {
			return nil, err
		}
		if err := startifact.VerifyEvidence(&evidence); err != nil {
			return nil, err
		}
		return startifact.PublishEvidence(ctx, store, &evidence)
	}
	loadSnEvidenceBlobStore = func() (server.BlobStore, bool) { return store, true }
	t.Cleanup(func() {
		publishSnEvidence = previousPublish
		loadSnEvidenceBlobStore = previousLoad
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	type handlerResult struct {
		writerIndex int
		response    *httptest.ResponseRecorder
	}
	results := make(chan handlerResult, len(encodedValues))
	for i, encoded := range encodedValues {
		writerIndex := i
		encoded := encoded
		go func() {
			request := httptest.NewRequest(http.MethodPost, "/sn/evidence", bytes.NewReader(encoded)).WithContext(ctx)
			response := httptest.NewRecorder()
			SnEvidence(response, request)
			results <- handlerResult{writerIndex: writerIndex, response: response}
		}()
	}
	successIndex := -1
	conflictCount := 0
	for range encodedValues {
		select {
		case result := <-results:
			switch result.response.Code {
			case http.StatusOK:
				if successIndex != -1 {
					t.Fatalf("writers %d and %d both returned success", successIndex, result.writerIndex)
				}
				successIndex = result.writerIndex
			case http.StatusBadRequest:
				conflictCount++
			default:
				t.Fatalf("writer %d status = %d, body = %s", result.writerIndex, result.response.Code, result.response.Body.String())
			}
		case <-ctx.Done():
			t.Fatalf("concurrent POSTs did not finish: %v", ctx.Err())
		}
	}
	if successIndex == -1 || conflictCount != 1 {
		t.Fatalf("success index = %d, conflict count = %d", successIndex, conflictCount)
	}
	get := serveSnEvidence(http.MethodGet, "/sn/evidence?hash="+url.QueryEscape(withPrefix.ContentHash), nil)
	if get.Code != http.StatusOK || !bytes.Equal(get.Body.Bytes(), canonicalValues[successIndex]) {
		t.Fatalf("winning GET status = %d, body = %s", get.Code, get.Body.String())
	}
	history := serveSnEvidenceHistory("/sn/evidence/history?deployment_id=test-deployment&netuid=7&kind=scenario-result")
	if history.Code != http.StatusOK {
		t.Fatalf("history status = %d, body = %s", history.Code, history.Body.String())
	}
	var listing struct {
		Objects []server.BlobObject `json:"objects"`
	}
	if err := json.Unmarshal(history.Body.Bytes(), &listing); err != nil {
		t.Fatal(err)
	}
	if len(listing.Objects) != 1 {
		t.Fatalf("history objects = %+v", listing.Objects)
	}
}
