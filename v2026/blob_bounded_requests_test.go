// The MinIO store's request census: a pinned region sends no bucket lookup,
// the single-attempt sibling sends each refused request once, and protection
// checks reuse their verified configuration while every check stays live.
// The peer is a synthetic S3 endpoint; all names and keys are synthetic.
package server

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/minio/minio-go/v7"
)

// Counts every request by kind and answers the ones the store sends.
type blobRequestCensusPeer struct {
	server *httptest.Server

	stateLock sync.Mutex
	counts    map[string]int
	objects   map[string][]byte
	// answers every object request with 429 TooManyRequests
	throttleObjects bool
	// answers HeadBucket with this status instead of 200
	headStatus int
}

func newBlobRequestCensusPeer(t testing.TB) *blobRequestCensusPeer {
	t.Helper()
	peer := &blobRequestCensusPeer{counts: map[string]int{}, objects: map[string][]byte{}}
	peer.server = httptest.NewServer(http.HandlerFunc(peer.serve))
	t.Cleanup(peer.server.Close)
	return peer
}

func (self *blobRequestCensusPeer) authority() string {
	return strings.TrimPrefix(self.server.URL, "http://")
}

func (self *blobRequestCensusPeer) count(kind string) int {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.counts[kind]
}

func (self *blobRequestCensusPeer) set(change func()) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	change()
}

func writeBlobCensusXml(w http.ResponseWriter, status int, body string) {
	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, body)
}

func (self *blobRequestCensusPeer) serve(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	path := strings.TrimPrefix(r.URL.Path, "/")
	bucket, object, _ := strings.Cut(path, "/")
	kind := r.Method + " object"
	switch {
	case object == "" && query.Has("location"):
		kind = "location"
	case object == "" && query.Has("object-lock"):
		kind = "object-lock"
	case object == "" && query.Has("versioning"):
		kind = "versioning"
	case object == "" && query.Has("replication-check"):
		kind = "replication-check"
	case object == "" && query.Has("replication"):
		kind = "replication"
	case object == "" && r.Method == http.MethodHead:
		kind = "head-bucket"
	}
	var throttle bool
	var headStatus int
	var stored []byte
	var found bool
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.counts[kind]++
		throttle, headStatus = self.throttleObjects, self.headStatus
		stored, found = self.objects[path]
	}()
	switch kind {
	case "location":
		writeBlobCensusXml(w, http.StatusOK, `<LocationConstraint xmlns="http://s3.amazonaws.com/doc/2006-03-01/"></LocationConstraint>`)
	case "head-bucket":
		if headStatus != 0 {
			w.WriteHeader(headStatus)
			return
		}
		w.WriteHeader(http.StatusOK)
	case "object-lock":
		writeBlobCensusXml(w, http.StatusOK, `<ObjectLockConfiguration><ObjectLockEnabled>Enabled</ObjectLockEnabled></ObjectLockConfiguration>`)
	case "versioning":
		writeBlobCensusXml(w, http.StatusOK, `<VersioningConfiguration><Status>Enabled</Status></VersioningConfiguration>`)
	case "replication":
		writeBlobCensusXml(w, http.StatusOK, `<ReplicationConfiguration><Role></Role><Rule><ID>synthetic</ID><Status>Enabled</Status><Priority>1</Priority><DeleteMarkerReplication><Status>Disabled</Status></DeleteMarkerReplication><Destination><Bucket>arn:minio:replication::synthetic:backup</Bucket></Destination><Filter><Prefix></Prefix></Filter></Rule></ReplicationConfiguration>`)
	case "replication-check":
		w.WriteHeader(http.StatusOK)
	case "PUT object":
		var reader io.Reader = r.Body
		if strings.HasPrefix(r.Header.Get("X-Amz-Content-Sha256"), "STREAMING-") {
			reader = httputil.NewChunkedReader(r.Body)
		}
		body, err := io.ReadAll(reader)
		if err != nil {
			http.Error(w, "synthetic body read failed", http.StatusBadRequest)
			return
		}
		if throttle {
			writeBlobCensusXml(w, http.StatusTooManyRequests, `<Error><Code>TooManyRequests</Code><Message>Please reduce your request rate</Message></Error>`)
			return
		}
		if found && r.Header.Get("If-None-Match") == "*" {
			writeBlobCensusXml(w, http.StatusPreconditionFailed, `<Error><Code>PreconditionFailed</Code><Message>exists</Message></Error>`)
			return
		}
		self.set(func() { self.objects[path] = body })
		w.Header().Set("ETag", `"synthetic"`)
		w.WriteHeader(http.StatusOK)
	case "GET object":
		if throttle {
			writeBlobCensusXml(w, http.StatusTooManyRequests, `<Error><Code>TooManyRequests</Code><Message>Please reduce your request rate</Message></Error>`)
			return
		}
		if !found {
			writeBlobCensusXml(w, http.StatusNotFound, `<Error><Code>NoSuchKey</Code><Message>none</Message></Error>`)
			return
		}
		w.Header().Set("Content-Length", fmt.Sprint(len(stored)))
		w.Header().Set("Last-Modified", time.Now().UTC().Format(http.TimeFormat))
		w.Header().Set("ETag", `"synthetic"`)
		_, _ = w.Write(stored)
	default:
		http.Error(w, "unexpected synthetic request "+kind+" "+bucket, http.StatusBadRequest)
	}
}

// Points LoadBlobStore at peer through a pushed minio.yml. extra is appended
// to the resource. The vault, config and site roots do not exist.
func pushBlobCensusResource(t *testing.T, peer *blobRequestCensusPeer, extra string) {
	t.Helper()
	base := t.TempDir()
	t.Setenv("WARP_VAULT_HOME", filepath.Join(base, "absent", "vault"))
	t.Setenv("WARP_CONFIG_HOME", filepath.Join(base, "absent", "config"))
	t.Setenv("WARP_SITE_HOME", filepath.Join(base, "absent", "site"))
	t.Setenv("WARP_ENV", "synthetic-main")
	t.Setenv("WARP_HOST", "synthetic-host")
	resource := fmt.Sprintf("authority: %s\nbucket: synthetic\naccess_key: synthetic-access\nsecret_key: synthetic-secret\n%s", peer.authority(), extra)
	pop := Vault.PushSimpleResource("minio.yml", []byte(resource))
	t.Cleanup(pop)
}

func writeBlobCensusSource(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "synthetic.json")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func readBlobCensusObject(t *testing.T, store BlobStore, key string) []byte {
	t.Helper()
	reader, err := store.Get(t.Context(), key)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	content, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}
	return content
}

// minio.yml pins the region, us-east-1 by default, and bounds the evidence
// writer's concurrency.
func TestLoadBlobStoreConfigPinsRegionAndEvidenceConcurrency(t *testing.T) {
	peer := newBlobRequestCensusPeer(t)
	for _, test := range []struct {
		extra       string
		region      string
		concurrency int
	}{
		{extra: "", region: DefaultBlobRegion, concurrency: DefaultClientKeyEvidenceConcurrency},
		{extra: "region: synthetic-region-1\n", region: "synthetic-region-1", concurrency: DefaultClientKeyEvidenceConcurrency},
		{extra: "client_key_evidence_concurrency: 9\n", region: DefaultBlobRegion, concurrency: 9},
		{extra: "client_key_evidence_concurrency: 4096\n", region: DefaultBlobRegion, concurrency: MaxClientKeyEvidenceConcurrency},
		{extra: "client_key_evidence_concurrency: 0\n", region: DefaultBlobRegion, concurrency: DefaultClientKeyEvidenceConcurrency},
		{extra: "client_key_evidence_concurrency: -3\n", region: DefaultBlobRegion, concurrency: DefaultClientKeyEvidenceConcurrency},
	} {
		pushBlobCensusResource(t, peer, test.extra)
		config, present := LoadBlobStoreConfig()
		if !present || config.Region != test.region || config.ClientKeyEvidenceConcurrency != test.concurrency {
			t.Fatalf("minio.yml %q: config = %+v, present %t", test.extra, config, present)
		}
	}
}

// With minio.yml's region pinned, no request from the configured store, its
// single-attempt sibling or a store derived from its configuration looks the
// bucket up first.
func TestLoadBlobStoreSendsNoBucketLocation(t *testing.T) {
	peer := newBlobRequestCensusPeer(t)
	pushBlobCensusResource(t, peer, "")
	store, ok := LoadBlobStore()
	if !ok {
		t.Fatal("configured MinIO store is unavailable")
	}
	source := writeBlobCensusSource(t, `{"synthetic":1}`)
	for _, candidate := range []BlobStore{store, SingleAttemptBlobStore(store)} {
		if _, err := candidate.PutIfAbsent(t.Context(), "blob/synthetic.json", source, "application/json"); err != nil {
			t.Fatal(err)
		}
		if content := readBlobCensusObject(t, candidate, "blob/synthetic.json"); string(content) != `{"synthetic":1}` {
			t.Fatalf("read back %q", content)
		}
	}
	config, _ := LoadBlobStoreConfig()
	config.Bucket = "synthetic-other"
	derived, err := NewBlobStore(config)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := derived.(ProtectedBlobStore).CheckProtection(t.Context()); err != nil {
		t.Fatal(err)
	}
	if locations := peer.count("location"); locations != 0 {
		t.Fatalf("%d GetBucketLocation requests with a pinned region", locations)
	}
	if peer.count("PUT object") != 2 || peer.count("GET object") != 2 {
		t.Fatalf("object census put=%d get=%d", peer.count("PUT object"), peer.count("GET object"))
	}
}

// The SDK's own client retries a 429 up to ten times; the single-attempt
// sibling of the same store sends it once and returns the refusal.
func TestSingleAttemptBlobStoreSendsEachThrottledRequestOnce(t *testing.T) {
	peer := newBlobRequestCensusPeer(t)
	pushBlobCensusResource(t, peer, "")
	store, ok := LoadBlobStore()
	if !ok {
		t.Fatal("configured MinIO store is unavailable")
	}
	single := SingleAttemptBlobStore(store)
	if single == store || SingleAttemptBlobStore(single) != single {
		t.Fatal("the single-attempt sibling is not distinct and stable")
	}
	if single.Bucket() != store.Bucket() || single.Prefix() != store.Prefix() || single.Authority() != store.Authority() {
		t.Fatal("the sibling names another bucket, prefix or endpoint")
	}
	peer.set(func() { peer.throttleObjects = true })
	source := writeBlobCensusSource(t, `{"synthetic":2}`)
	_, err := single.PutIfAbsent(t.Context(), "blob/throttled.json", source, "application/json")
	if !IsBlobThrottled(err) {
		t.Fatalf("throttled create = %v", err)
	}
	if puts := peer.count("PUT object"); puts != 1 {
		t.Fatalf("one throttled create sent %d requests", puts)
	}
	reader, err := single.Get(t.Context(), "blob/throttled.json")
	if err == nil {
		_, err = io.ReadAll(reader)
		reader.Close()
	}
	if !IsBlobThrottled(err) || peer.count("GET object") != 1 {
		t.Fatalf("throttled read = %v after %d requests", err, peer.count("GET object"))
	}
	// the shared client keeps the SDK's retries for every other writer
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	if _, err := store.PutIfAbsent(ctx, "blob/throttled.json", source, "application/json"); !IsBlobThrottled(err) {
		t.Fatalf("shared client throttled create = %v", err)
	}
	if puts := peer.count("PUT object"); puts != 1+minio.MaxRetry {
		t.Fatalf("shared client sent %d creates, want its %d", puts-1, minio.MaxRetry)
	}
}

// Protection checks read the bucket configuration once per TTL, and
// concurrent checks share one re-read; HeadBucket runs every check.
func TestMinioBlobProtectionCheckReusesVerifiedConfiguration(t *testing.T) {
	peer := newBlobRequestCensusPeer(t)
	store, err := NewBlobStore(&BlobStoreConfig{Authority: peer.authority(), Bucket: "synthetic", AccessKey: "synthetic-access", SecretKey: "synthetic-secret", Region: DefaultBlobRegion})
	if err != nil {
		t.Fatal(err)
	}
	minioStore := store.(*minioBlobStore)
	var clockLock sync.Mutex
	now := time.Date(2026, 11, 1, 18, 0, 0, 0, time.UTC)
	minioStore.protection.now = func() time.Time {
		clockLock.Lock()
		defer clockLock.Unlock()
		return now
	}
	advance := func(by time.Duration) {
		clockLock.Lock()
		defer clockLock.Unlock()
		now = now.Add(by)
	}
	configuration := []string{"object-lock", "versioning", "replication", "replication-check"}
	assertCensus := func(heads int, reads int) {
		t.Helper()
		if got := peer.count("head-bucket"); got != heads {
			t.Fatalf("HeadBucket sent %d times, want %d", got, heads)
		}
		for _, kind := range configuration {
			if got := peer.count(kind); got != reads {
				t.Fatalf("%s read %d times, want %d", kind, got, reads)
			}
		}
	}
	for range 10 {
		proof, err := minioStore.CheckProtection(t.Context())
		if err != nil || proof.Bucket != "synthetic" || proof.ObjectLock != "COMPLIANCE" || !proof.Versioning || len(proof.ReplicationTargets) != 1 {
			t.Fatalf("protection = %+v, %v", proof, err)
		}
		// a caller's change cannot reach a later check
		proof.ReplicationTargets[0] = "changed-by-a-caller"
	}
	if err := minioStore.CheckRetention(t.Context()); err != nil {
		t.Fatal(err)
	}
	assertCensus(11, 1)
	if proof, err := minioStore.CheckProtection(t.Context()); err != nil || proof.ReplicationTargets[0] != "arn:minio:replication::synthetic:backup" {
		t.Fatalf("a caller changed the remembered proof: %+v, %v", proof, err)
	}
	advance(blobProtectionProofTtl - time.Second)
	if _, err := minioStore.CheckProtection(t.Context()); err != nil {
		t.Fatal(err)
	}
	assertCensus(13, 1)
	advance(time.Second)
	var checks sync.WaitGroup
	failures := make(chan error, 16)
	for range 16 {
		checks.Go(func() {
			if _, err := minioStore.CheckProtection(t.Context()); err != nil {
				failures <- err
			}
		})
	}
	checks.Wait()
	close(failures)
	for err := range failures {
		t.Fatal(err)
	}
	assertCensus(29, 2)
}

// The live HeadBucket fails a check through a remembered proof: refused
// credentials or an unreachable bucket at once, and a missing bucket also
// forgets the proof so its return is verified again.
func TestMinioBlobProtectionCheckSeesOutageThroughRememberedProof(t *testing.T) {
	peer := newBlobRequestCensusPeer(t)
	store, err := NewBlobStore(&BlobStoreConfig{Authority: peer.authority(), Bucket: "synthetic", AccessKey: "synthetic-access", SecretKey: "synthetic-secret", Region: DefaultBlobRegion})
	if err != nil {
		t.Fatal(err)
	}
	protected := store.(ProtectedBlobStore)
	if _, err := protected.CheckProtection(t.Context()); err != nil {
		t.Fatal(err)
	}
	peer.set(func() { peer.headStatus = http.StatusForbidden })
	if _, err := protected.CheckProtection(t.Context()); err == nil {
		t.Fatal("a refused HeadBucket passed through the remembered proof")
	}
	if err := protected.CheckRetention(t.Context()); err == nil {
		t.Fatal("a refused HeadBucket passed retention through the remembered proof")
	}
	peer.set(func() { peer.headStatus = 0 })
	if _, err := protected.CheckProtection(t.Context()); err != nil || peer.count("object-lock") != 1 {
		t.Fatalf("a recovered bucket did not reuse its proof: %v, %d", err, peer.count("object-lock"))
	}
	peer.set(func() { peer.headStatus = http.StatusNotFound })
	if _, err := protected.CheckProtection(t.Context()); err == nil || !strings.Contains(err.Error(), "does not exist") {
		t.Fatalf("a missing bucket = %v", err)
	}
	peer.set(func() { peer.headStatus = 0 })
	if _, err := protected.CheckProtection(t.Context()); err != nil || peer.count("object-lock") != 2 {
		t.Fatalf("a returned bucket was not verified again: %v, %d", err, peer.count("object-lock"))
	}
}

// The classifications a bounded writer acts on, through wrapped and joined
// chains.
func TestBlobErrorClassification(t *testing.T) {
	throttled := []minio.ErrorResponse{
		{StatusCode: http.StatusTooManyRequests, Code: "TooManyRequests"},
		{StatusCode: http.StatusServiceUnavailable, Code: "SlowDownWrite"},
		{StatusCode: http.StatusServiceUnavailable, Code: "ServiceUnavailable"},
		{StatusCode: http.StatusBadRequest, Code: "SlowDown"},
	}
	for _, response := range throttled {
		for _, err := range []error{response, &response, fmt.Errorf("create: %w", response), errors.Join(errors.New("read"), response)} {
			if !IsBlobThrottled(err) || IsBlobRequestRefused(err) || IsBlobNotFound(err) {
				t.Fatalf("%v is not classified as throttled only", err)
			}
		}
	}
	refused := []minio.ErrorResponse{
		{StatusCode: http.StatusForbidden, Code: "AccessDenied"},
		{StatusCode: http.StatusNotFound, Code: "NoSuchBucket"},
		{StatusCode: http.StatusBadRequest, Code: "InvalidArgument"},
	}
	for _, response := range refused {
		err := fmt.Errorf("create: %w", response)
		if IsBlobThrottled(err) || !IsBlobRequestRefused(err) || IsBlobNotFound(err) {
			t.Fatalf("%v is not classified as refused only", err)
		}
	}
	notFound := fmt.Errorf("read: %w", minio.ErrorResponse{StatusCode: http.StatusNotFound, Code: minio.NoSuchKey})
	if !IsBlobNotFound(notFound) || !IsBlobNotFound(fmt.Errorf("open: %w", os.ErrNotExist)) {
		t.Fatal("a missing object is not classified as not found")
	}
	for _, err := range []error{
		errors.New("connection reset"),
		context.DeadlineExceeded,
		minio.ErrorResponse{StatusCode: http.StatusInternalServerError, Code: "InternalError"},
		minio.ErrorResponse{StatusCode: http.StatusRequestTimeout, Code: "RequestTimeout"},
		minio.ErrorResponse{StatusCode: http.StatusConflict, Code: "ConditionalRequestConflict"},
	} {
		if IsBlobThrottled(err) || IsBlobRequestRefused(err) || IsBlobNotFound(err) {
			t.Fatalf("%v is not a transient failure", err)
		}
	}
}
