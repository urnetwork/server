package handlers

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/crypto"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/controller"
	"github.com/urnetwork/server/v2026/startifact"
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

func TestReadSnEvidenceBytesRejectsTruncationAtBoundary(t *testing.T) {
	accepted, err := readSnEvidenceBytes(strings.NewReader("abcd"), 4)
	if err != nil || string(accepted) != "abcd" {
		t.Fatalf("exact-boundary read = %q, %v", accepted, err)
	}
	rejected, err := readSnEvidenceBytes(strings.NewReader("abcde"), 4)
	if err == nil || rejected != nil {
		t.Fatalf("oversized read = %q, %v; want nil bytes and an error", rejected, err)
	}
	if b, err := readSnEvidenceBytes(strings.NewReader("a"), 0); err == nil || b != nil {
		t.Fatalf("zero-limit read = %q, %v; want rejection", b, err)
	}
}

type snEvidenceListPageRecorder struct {
	server.BlobStore
	stateLock sync.Mutex
	prefix    string
	limit     int
	listCalls int
	pageCalls int
}

// snUnpagedBlobStore proves public history fails closed without the optional
// bounded-list capability while preserving BlobStore source compatibility.
type snUnpagedBlobStore struct {
	server.BlobStore
}

// snFixedPageBlobStore injects malformed backend pages at the public handler
// boundary without weakening the production stores' ordering contract.
type snFixedPageBlobStore struct {
	server.BlobStore
	objects []server.BlobObject
	more    bool
}

func (self *snFixedPageBlobStore) ListPage(context.Context, string, string, int) ([]server.BlobObject, bool, error) {
	return append([]server.BlobObject(nil), self.objects...), self.more, nil
}

func (self *snEvidenceListPageRecorder) List(ctx context.Context, keyPrefix string) ([]server.BlobObject, error) {
	self.stateLock.Lock()
	self.listCalls++
	self.stateLock.Unlock()
	return self.BlobStore.List(ctx, keyPrefix)
}

func (self *snEvidenceListPageRecorder) ListPage(ctx context.Context, keyPrefix string, startAfter string, limit int) ([]server.BlobObject, bool, error) {
	self.stateLock.Lock()
	self.prefix = keyPrefix
	self.limit = limit
	self.pageCalls++
	self.stateLock.Unlock()
	pagedStore, ok := self.BlobStore.(server.PagedBlobStore)
	if !ok {
		return nil, false, errors.New("wrapped store does not support bounded listing")
	}
	return pagedStore.ListPage(ctx, keyPrefix, startAfter, limit)
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

func (self *snEvidenceRaceStore) ListPage(ctx context.Context, keyPrefix string, startAfter string, limit int) ([]server.BlobObject, bool, error) {
	objects, err := self.List(ctx, keyPrefix)
	if err != nil {
		return nil, false, err
	}
	page := []server.BlobObject{}
	for _, object := range objects {
		if object.Key <= startAfter {
			continue
		}
		if len(page) == limit {
			return page, true, nil
		}
		page = append(page, object)
	}
	return page, false, nil
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

func TestSnArtifactHistoryIsBoundedAndPaginates(t *testing.T) {
	configureSnEvidenceHandler(t)
	store, ok := server.LoadBlobStore()
	if !ok {
		t.Fatal("configured test blob store is unavailable")
	}
	source := filepath.Join(t.TempDir(), "artifact.json")
	if err := os.WriteFile(source, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	hashes := []string{strings.Repeat("11", 32), strings.Repeat("22", 32), strings.Repeat("33", 32)}
	keys := []string{
		"blob/st/v1/history/test-deployment/7/1/2/" + hashes[0] + ".json",
		"blob/st/v1/history/test-deployment/7/1/2/" + hashes[1] + ".json",
		"blob/st/v1/history/test-deployment/7/2/2/" + hashes[2] + ".json",
	}
	for _, key := range keys {
		if err := store.Put(context.Background(), key, source, "application/json"); err != nil {
			t.Fatal(err)
		}
	}
	serve := func(target string) *httptest.ResponseRecorder {
		request := httptest.NewRequest(http.MethodGet, target, nil)
		response := httptest.NewRecorder()
		SnArtifactHistory(response, request)
		return response
	}
	target := "/sn/artifact/history?deployment_id=test-deployment&netuid=7&epoch=1&no_id=2&limit=1"
	first := serve(target)
	if first.Code != http.StatusOK || first.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("first page status = %d cache=%q body=%s", first.Code, first.Header().Get("Cache-Control"), first.Body.String())
	}
	var firstPage struct {
		Objects []struct {
			Key         string `json:"key"`
			ContentHash string `json:"content_hash"`
		} `json:"objects"`
		More      bool   `json:"more"`
		NextAfter string `json:"next_after"`
	}
	if err := json.Unmarshal(first.Body.Bytes(), &firstPage); err != nil {
		t.Fatal(err)
	}
	if len(firstPage.Objects) != 1 || !firstPage.More || firstPage.NextAfter != firstPage.Objects[0].Key || firstPage.Objects[0].ContentHash != "sha256:"+hashes[0] {
		t.Fatalf("first page = %+v", firstPage)
	}
	second := serve(target + "&after=" + url.QueryEscape(firstPage.NextAfter))
	var secondPage struct {
		Objects []struct {
			Key         string `json:"key"`
			ContentHash string `json:"content_hash"`
		} `json:"objects"`
		More bool `json:"more"`
	}
	if second.Code != http.StatusOK {
		t.Fatalf("second page status = %d, body = %s", second.Code, second.Body.String())
	}
	if err := json.Unmarshal(second.Body.Bytes(), &secondPage); err != nil {
		t.Fatal(err)
	}
	if len(secondPage.Objects) != 1 || secondPage.More || secondPage.Objects[0].Key != keys[1] || secondPage.Objects[0].ContentHash != "sha256:"+hashes[1] {
		t.Fatalf("second page = %+v", secondPage)
	}
	broaderScopes := []struct {
		target   string
		wantKeys []string
	}{
		{
			target:   "/sn/artifact/history?deployment_id=test-deployment&netuid=7&limit=10",
			wantKeys: keys,
		},
		{
			target:   "/sn/artifact/history?deployment_id=test-deployment&netuid=7&epoch=1&limit=10",
			wantKeys: keys[:2],
		},
	}
	for _, scope := range broaderScopes {
		response := serve(scope.target)
		if response.Code != http.StatusOK {
			t.Errorf("broad history %q status = %d, body = %s", scope.target, response.Code, response.Body.String())
			continue
		}
		var page struct {
			Objects []struct {
				Key string `json:"key"`
			} `json:"objects"`
			More bool `json:"more"`
		}
		if err := json.Unmarshal(response.Body.Bytes(), &page); err != nil {
			t.Error(err)
			continue
		}
		actualKeys := make([]string, len(page.Objects))
		for i, object := range page.Objects {
			actualKeys[i] = object.Key
		}
		if page.More || !slices.Equal(actualKeys, scope.wantKeys) {
			t.Errorf("broad history %q = %+v, want keys %+v", scope.target, page, scope.wantKeys)
		}
	}
}

func TestSnArtifactHistoryRejectsUnsafePageQuery(t *testing.T) {
	configureSnEvidenceHandler(t)
	base := "/sn/artifact/history?deployment_id=test-deployment&netuid=7&epoch=1&no_id=2"
	targets := []string{
		base + "&limit=0",
		base + "&limit=4097",
		base + "&after=" + url.QueryEscape("blob/st/v1/history/test-deployment/7/2/2/key.json"),
		base + "&after=" + url.QueryEscape("blob/st/v1/history/test-deployment/7/1/2/../key.json"),
	}
	for _, target := range targets {
		request := httptest.NewRequest(http.MethodGet, target, nil)
		response := httptest.NewRecorder()
		SnArtifactHistory(response, request)
		if response.Code != http.StatusBadRequest {
			t.Errorf("unsafe artifact history request %q status = %d, body = %s", target, response.Code, response.Body.String())
		}
	}
}

func TestValidateSnArtifactHistoryPageMatchesSelectorDepth(t *testing.T) {
	hashFilename := strings.Repeat("ab", 32) + ".json"
	tests := []struct {
		name     string
		prefix   string
		epochRaw string
		noIDRaw  string
		relative string
		wantErr  bool
	}{
		{name: "deployment scope", prefix: "blob/history/7/", relative: "0/2/" + hashFilename},
		{name: "epoch scope", prefix: "blob/history/7/0/", epochRaw: "0", relative: "2/" + hashFilename},
		{name: "operator scope", prefix: "blob/history/7/0/2/", epochRaw: "0", noIDRaw: "2", relative: hashFilename},
		{name: "zero no id", prefix: "blob/history/7/", relative: "0/0/" + hashFilename, wantErr: true},
		{name: "noncanonical epoch", prefix: "blob/history/7/", relative: "00/2/" + hashFilename, wantErr: true},
		{name: "missing no id", prefix: "blob/history/7/", relative: "0/" + hashFilename, wantErr: true},
		{name: "extra exact segment", prefix: "blob/history/7/0/2/", epochRaw: "0", noIDRaw: "2", relative: "extra/" + hashFilename, wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			objects := []server.BlobObject{{Key: test.prefix + test.relative, Size: 1}}
			err := validateSnArtifactHistoryPage(objects, false, test.prefix, "", 2, test.epochRaw, test.noIDRaw)
			if (err != nil) != test.wantErr {
				t.Fatalf("artifact page validation error = %v, want_error=%t", err, test.wantErr)
			}
		})
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
			"&netuid=7&kind=" + url.QueryEscape(evidence.Kind) +
			"&run_id=" + url.QueryEscape(evidence.RunID),
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

func TestSnEvidenceHistoryScopesDottedRunAndPaginates(t *testing.T) {
	config, artifactKey := configureSnEvidenceHandler(t)
	publish := func(runID string, marker string) startifact.Published {
		evidence := signedSnEvidence(t, config, artifactKey)
		evidence.RunID = runID
		evidence.Payload = json.RawMessage(fmt.Sprintf(`{"marker":%q}`, marker))
		if err := startifact.SignEvidence(evidence, artifactKey); err != nil {
			t.Fatal(err)
		}
		encoded, err := json.Marshal(evidence)
		if err != nil {
			t.Fatal(err)
		}
		response := serveSnEvidence(http.MethodPost, "/sn/evidence", encoded)
		if response.Code != http.StatusOK {
			t.Fatalf("publish %q status = %d, body = %s", runID, response.Code, response.Body.String())
		}
		var result startifact.Published
		if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
			t.Fatal(err)
		}
		return result
	}
	targetRunID := "release.v1-attempt-4"
	firstPublished := publish(targetRunID, "a")
	secondPublished := publish(targetRunID, "b")
	publish("unrelated-run", "c")
	target := "/sn/evidence/history?deployment_id=test-deployment&netuid=7&kind=scenario-result&run_id=" + url.QueryEscape(targetRunID) + "&limit=1"
	first := serveSnEvidenceHistory(target)
	if first.Code != http.StatusOK {
		t.Fatalf("first page status = %d, body = %s", first.Code, first.Body.String())
	}
	if first.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("history cache control = %q", first.Header().Get("Cache-Control"))
	}
	var firstPage struct {
		Objects   []server.BlobObject `json:"objects"`
		More      bool                `json:"more"`
		NextAfter string              `json:"next_after"`
	}
	if err := json.Unmarshal(first.Body.Bytes(), &firstPage); err != nil {
		t.Fatal(err)
	}
	if len(firstPage.Objects) != 1 || !firstPage.More || firstPage.NextAfter != firstPage.Objects[0].Key || !strings.Contains(firstPage.Objects[0].Key, "/"+targetRunID+"/") {
		t.Fatalf("first page = %+v", firstPage)
	}
	second := serveSnEvidenceHistory(target + "&after=" + url.QueryEscape(firstPage.NextAfter))
	if second.Code != http.StatusOK {
		t.Fatalf("second page status = %d, body = %s", second.Code, second.Body.String())
	}
	var secondPage struct {
		Objects []server.BlobObject `json:"objects"`
		More    bool                `json:"more"`
	}
	if err := json.Unmarshal(second.Body.Bytes(), &secondPage); err != nil {
		t.Fatal(err)
	}
	wantKeys := map[string]bool{firstPublished.HistoryKey: true, secondPublished.HistoryKey: true}
	delete(wantKeys, firstPage.Objects[0].Key)
	if len(secondPage.Objects) != 1 || secondPage.More || !wantKeys[secondPage.Objects[0].Key] {
		t.Fatalf("second page = %+v, remaining = %+v", secondPage, wantKeys)
	}
}

func TestSnEvidenceHistoryExactHashAvoidsAccumulatedRunFanout(t *testing.T) {
	config, artifactKey := configureSnEvidenceHandler(t)
	published := make([]startifact.Published, 10)
	for i := range published {
		evidence := signedSnEvidence(t, config, artifactKey)
		evidence.Kind = "deployment-manifest"
		evidence.RunID = ""
		evidence.Payload = json.RawMessage(fmt.Sprintf(`{"revision":%d}`, i))
		if err := startifact.SignEvidence(evidence, artifactKey); err != nil {
			t.Fatal(err)
		}
		encoded, err := json.Marshal(evidence)
		if err != nil {
			t.Fatal(err)
		}
		response := serveSnEvidence(http.MethodPost, "/sn/evidence", encoded)
		if response.Code != http.StatusOK {
			t.Fatalf("publish revision %d status = %d, body = %s", i, response.Code, response.Body.String())
		}
		if err := json.Unmarshal(response.Body.Bytes(), &published[i]); err != nil {
			t.Fatal(err)
		}
	}
	base := "/sn/evidence/history?deployment_id=test-deployment&netuid=7&kind=deployment-manifest&run_id=" +
		url.QueryEscape(startifact.EvidenceDeploymentHistoryRunID) + "&limit=8"
	page := serveSnEvidenceHistory(base)
	if page.Code != http.StatusOK {
		t.Fatalf("accumulated page status = %d, body = %s", page.Code, page.Body.String())
	}
	var listing struct {
		Objects   []server.BlobObject `json:"objects"`
		More      bool                `json:"more"`
		NextAfter string              `json:"next_after"`
	}
	if err := json.Unmarshal(page.Body.Bytes(), &listing); err != nil {
		t.Fatal(err)
	}
	if len(listing.Objects) != 8 || !listing.More || listing.NextAfter == "" {
		t.Fatalf("accumulated page = %+v", listing)
	}

	store, ok := server.LoadBlobStore()
	if !ok {
		t.Fatal("configured test blob store is unavailable")
	}
	previousLoad := loadSnEvidenceBlobStore
	loadSnEvidenceBlobStore = func() (server.BlobStore, bool) {
		return &snUnpagedBlobStore{BlobStore: store}, true
	}
	t.Cleanup(func() { loadSnEvidenceBlobStore = previousLoad })
	target := published[len(published)-1]
	exact := serveSnEvidenceHistory(base + "&hash=" + url.QueryEscape(target.ContentHash))
	if exact.Code != http.StatusOK {
		t.Fatalf("exact accumulated lookup status = %d, body = %s", exact.Code, exact.Body.String())
	}
	listing = struct {
		Objects   []server.BlobObject `json:"objects"`
		More      bool                `json:"more"`
		NextAfter string              `json:"next_after"`
	}{}
	if err := json.Unmarshal(exact.Body.Bytes(), &listing); err != nil {
		t.Fatal(err)
	}
	if len(listing.Objects) != 1 || listing.Objects[0].Key != target.HistoryKey || listing.More || listing.NextAfter != "" {
		t.Fatalf("exact accumulated lookup = %+v, want key %q", listing, target.HistoryKey)
	}
	missing := serveSnEvidenceHistory(base + "&hash=sha256:" + strings.Repeat("ff", 32))
	if missing.Code != http.StatusNotFound {
		t.Fatalf("missing exact hash status = %d, body = %s", missing.Code, missing.Body.String())
	}
	withCursor := serveSnEvidenceHistory(base + "&hash=" + url.QueryEscape(target.ContentHash) + "&after=" + url.QueryEscape(target.HistoryKey))
	if withCursor.Code != http.StatusBadRequest {
		t.Fatalf("exact hash with cursor status = %d, body = %s", withCursor.Code, withCursor.Body.String())
	}
}

func TestSnEvidenceHistoryExactHashReadsLegacyEmptyRunPath(t *testing.T) {
	config, artifactKey := configureSnEvidenceHandler(t)
	evidence := signedSnEvidence(t, config, artifactKey)
	evidence.Kind = "deployment-manifest"
	evidence.RunID = ""
	evidence.Payload = json.RawMessage(`{"revision":"legacy"}`)
	if err := startifact.SignEvidence(evidence, artifactKey); err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(evidence)
	if err != nil {
		t.Fatal(err)
	}
	store, ok := server.LoadBlobStore()
	if !ok {
		t.Fatal("configured test blob store is unavailable")
	}
	legacyKey, err := startifact.EvidenceHistoryKey(store, evidence.DeploymentID, evidence.Netuid, evidence.Kind, startifact.EvidenceLegacyDeploymentHistoryRunID, evidence.ContentHash)
	if err != nil {
		t.Fatal(err)
	}
	localPath := filepath.Join(t.TempDir(), "legacy-evidence.json")
	if err := os.WriteFile(localPath, b, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := store.Put(context.Background(), legacyKey, localPath, "application/json"); err != nil {
		t.Fatal(err)
	}
	target := "/sn/evidence/history?deployment_id=test-deployment&netuid=7&kind=deployment-manifest&run_id=deployment&limit=1&hash=" +
		url.QueryEscape(evidence.ContentHash)
	response := serveSnEvidenceHistory(target)
	if response.Code != http.StatusOK {
		t.Fatalf("legacy exact lookup status = %d, body = %s", response.Code, response.Body.String())
	}
	var listing struct {
		Objects []server.BlobObject `json:"objects"`
		More    bool                `json:"more"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &listing); err != nil {
		t.Fatal(err)
	}
	if len(listing.Objects) != 1 || listing.Objects[0].Key != legacyKey || listing.More {
		t.Fatalf("legacy exact lookup = %+v, want key %q", listing, legacyKey)
	}
}

func TestSnEvidenceHistoryRejectsUnsafeRunLimitAndCursor(t *testing.T) {
	configureSnEvidenceHandler(t)
	base := "/sn/evidence/history?deployment_id=test-deployment&netuid=7&kind=scenario-result"
	targets := []string{
		base,
		base + "&run_id=..%2Fescape",
		"/sn/evidence/history?deployment_id=test-deployment&netuid=7&run_id=run-1",
		"/sn/evidence/history?deployment_id=..%2Fdeployment&netuid=7&kind=scenario-result&run_id=run-1",
		"/sn/evidence/history?deployment_id=test-deployment&netuid=7&kind=..%2Fkind&run_id=run-1",
		base + "&run_id=run-1&limit=0",
		base + "&run_id=run-1&limit=4097",
		base + "&run_id=run-1&limit=not-a-number",
		base + "&run_id=run-1&after=" + url.QueryEscape("blob/other/key.json"),
		base + "&run_id=run-1&after=" + url.QueryEscape("blob/st/v1/evidence/history/test-deployment/7/scenario-result/run-1/../other.json"),
	}
	for _, target := range targets {
		response := serveSnEvidenceHistory(target)
		if response.Code != http.StatusBadRequest {
			t.Errorf("unsafe history request %q status = %d, body = %s", target, response.Code, response.Body.String())
		}
	}
}

func TestSnEvidenceHistoryRejectsMissingSelectorsBeforeListing(t *testing.T) {
	configureSnEvidenceHandler(t)
	store, ok := server.LoadBlobStore()
	if !ok {
		t.Fatal("configured test blob store is unavailable")
	}
	recorder := &snEvidenceListPageRecorder{BlobStore: store}
	previousLoad := loadSnEvidenceBlobStore
	loadSnEvidenceBlobStore = func() (server.BlobStore, bool) { return recorder, true }
	t.Cleanup(func() { loadSnEvidenceBlobStore = previousLoad })
	targets := []string{
		"/sn/evidence/history?deployment_id=test-deployment&netuid=7&kind=scenario-result",
		"/sn/evidence/history?deployment_id=test-deployment&netuid=7&run_id=run-1",
	}
	for _, target := range targets {
		response := serveSnEvidenceHistory(target)
		if response.Code != http.StatusBadRequest {
			t.Errorf("missing-selector request %q status = %d, body = %s", target, response.Code, response.Body.String())
		}
	}
	recorder.stateLock.Lock()
	listCalls, pageCalls := recorder.listCalls, recorder.pageCalls
	recorder.stateLock.Unlock()
	if listCalls != 0 || pageCalls != 0 {
		t.Fatalf("missing selectors reached storage: List=%d ListPage=%d", listCalls, pageCalls)
	}
}

func TestSnEvidenceHistoryRequiresBoundedStoreCapability(t *testing.T) {
	configureSnEvidenceHandler(t)
	store, ok := server.LoadBlobStore()
	if !ok {
		t.Fatal("configured test blob store is unavailable")
	}
	previousLoad := loadSnEvidenceBlobStore
	loadSnEvidenceBlobStore = func() (server.BlobStore, bool) {
		return &snUnpagedBlobStore{BlobStore: store}, true
	}
	t.Cleanup(func() { loadSnEvidenceBlobStore = previousLoad })
	response := serveSnEvidenceHistory("/sn/evidence/history?deployment_id=test-deployment&netuid=7&kind=scenario-result&run_id=run-1&limit=8")
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("unpaged history status = %d, body = %s", response.Code, response.Body.String())
	}
}

func TestSnEvidenceHistoryRejectsLegacyDeploymentEnumerationBeforeListing(t *testing.T) {
	configureSnEvidenceHandler(t)
	store, ok := server.LoadBlobStore()
	if !ok {
		t.Fatal("configured test blob store is unavailable")
	}
	recorder := &snEvidenceListPageRecorder{BlobStore: store}
	previousLoad := loadSnEvidenceBlobStore
	loadSnEvidenceBlobStore = func() (server.BlobStore, bool) { return recorder, true }
	t.Cleanup(func() { loadSnEvidenceBlobStore = previousLoad })
	response := serveSnEvidenceHistory("/sn/evidence/history?deployment_id=test-deployment&netuid=7&kind=deployment-manifest&run_id=deployment&limit=8")
	if response.Code != http.StatusBadRequest {
		t.Fatalf("legacy enumeration status = %d, body = %s", response.Code, response.Body.String())
	}
	recorder.stateLock.Lock()
	listCalls, pageCalls := recorder.listCalls, recorder.pageCalls
	recorder.stateLock.Unlock()
	if listCalls != 0 || pageCalls != 0 {
		t.Fatalf("legacy enumeration reached storage: List=%d ListPage=%d", listCalls, pageCalls)
	}
}

func TestSnEvidenceHistoryRejectsMalformedBackendPages(t *testing.T) {
	configureSnEvidenceHandler(t)
	store, ok := server.LoadBlobStore()
	if !ok {
		t.Fatal("configured test blob store is unavailable")
	}
	prefix := "blob/st/v1/evidence/history/test-deployment/7/scenario-result/run-1/"
	validKey := prefix + strings.Repeat("0", 64) + ".json"
	tests := []struct {
		name    string
		objects []server.BlobObject
		more    bool
	}{
		{name: "foreign prefix", objects: []server.BlobObject{{Key: "blob/other/" + strings.Repeat("0", 64) + ".json", Size: 1}}},
		{name: "nested key", objects: []server.BlobObject{{Key: prefix + "nested/" + strings.Repeat("0", 64) + ".json", Size: 1}}},
		{name: "noncanonical hash", objects: []server.BlobObject{{Key: prefix + strings.Repeat("A", 64) + ".json", Size: 1}}},
		{name: "negative size", objects: []server.BlobObject{{Key: validKey, Size: -1}}},
		{name: "short page claims more", objects: []server.BlobObject{{Key: validKey, Size: 1}}, more: true},
		{name: "duplicate keys", objects: []server.BlobObject{{Key: validKey, Size: 1}, {Key: validKey, Size: 1}}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixedStore := &snFixedPageBlobStore{BlobStore: store, objects: test.objects, more: test.more}
			previousLoad := loadSnEvidenceBlobStore
			loadSnEvidenceBlobStore = func() (server.BlobStore, bool) { return fixedStore, true }
			defer func() { loadSnEvidenceBlobStore = previousLoad }()
			response := serveSnEvidenceHistory("/sn/evidence/history?deployment_id=test-deployment&netuid=7&kind=scenario-result&run_id=run-1&limit=2")
			if response.Code != http.StatusBadGateway {
				t.Fatalf("malformed page status = %d, body = %s", response.Code, response.Body.String())
			}
		})
	}
}

func TestSnEvidenceHistoryUsesSignedSegmentGrammar(t *testing.T) {
	configureSnEvidenceHandler(t)
	response := serveSnEvidenceHistory("/sn/evidence/history?deployment_id=test.deployment&netuid=7&kind=scenario.result&run_id=run.1")
	if response.Code != http.StatusOK {
		t.Fatalf("dotted evidence identity status = %d, body = %s", response.Code, response.Body.String())
	}
}

func TestSnEvidenceHistoryBoundsExactRunAtStorageLayer(t *testing.T) {
	configureSnEvidenceHandler(t)
	store, ok := server.LoadBlobStore()
	if !ok {
		t.Fatal("configured test blob store is unavailable")
	}
	recorder := &snEvidenceListPageRecorder{BlobStore: store}
	previousLoad := loadSnEvidenceBlobStore
	loadSnEvidenceBlobStore = func() (server.BlobStore, bool) { return recorder, true }
	t.Cleanup(func() { loadSnEvidenceBlobStore = previousLoad })
	response := serveSnEvidenceHistory("/sn/evidence/history?deployment_id=test-deployment&netuid=7&kind=scenario-bundle&run_id=release.v1&limit=8")
	if response.Code != http.StatusOK {
		t.Fatalf("history status = %d, body = %s", response.Code, response.Body.String())
	}
	recorder.stateLock.Lock()
	prefix, limit, listCalls, pageCalls := recorder.prefix, recorder.limit, recorder.listCalls, recorder.pageCalls
	recorder.stateLock.Unlock()
	wantPrefix := "blob/st/v1/evidence/history/test-deployment/7/scenario-bundle/release.v1/"
	if prefix != wantPrefix || limit != 8 || listCalls != 0 || pageCalls != 1 {
		t.Fatalf("storage listing prefix=%q limit=%d unbounded_calls=%d bounded_calls=%d; want %q, 8, 0, 1", prefix, limit, listCalls, pageCalls, wantPrefix)
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
	history := serveSnEvidenceHistory("/sn/evidence/history?deployment_id=test-deployment&netuid=7&kind=scenario-result&run_id=run-1")
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
