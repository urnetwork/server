// The bounded client-key evidence writer against two synthetic stores: an
// in-memory BlobStore whose requests can be held, refused or throttled, and
// an S3 endpoint with a fixed request pool that answers 429 at once when it is
// full, the way MinIO does, reached through the actual MinIO client. Genuine
// operator-signed wrappers; every key, name and endpoint is synthetic.
package controller

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/minio/minio-go/v7"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/urfoundation/sn/protocol"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/startifact"
)

// One genuine registration wrapper per synthetic client.
type stClientKeyEvidenceWrapperTest struct {
	encoded     []byte
	contentHash string
}

func newStClientKeyEvidenceWrappersTest(t testing.TB, count int) []stClientKeyEvidenceWrapperTest {
	t.Helper()
	rootKey, err := crypto.HexToECDSA(strings.Repeat("31", 32))
	if err != nil {
		t.Fatal(err)
	}
	artifactKey, err := crypto.HexToECDSA(strings.Repeat("32", 32))
	if err != nil {
		t.Fatal(err)
	}
	deployment := "synthetic-evidence-writer"
	domain := protocol.ClientKeyHistoryDomain{ChainID: 31337, GenesisHash: [32]byte{1}, Netuid: 77, Coordinator: common.Address{2}, SettlementVault: common.Address{3}, DeploymentIDHash: sha256.Sum256([]byte(deployment)), PolicyHash: [32]byte{4}, NoID: 1}
	wrappers := make([]stClientKeyEvidenceWrapperTest, count)
	for index := range wrappers {
		var clientID [16]byte
		clientID[0] = 0xe0
		binary.BigEndian.PutUint64(clientID[8:], uint64(index))
		registration := protocol.ClientKeyRegistration{Domain: domain, ClientID: clientID, NetworkID: [16]byte{7}, Generation: 1, Present: true, PublicKey: [32]byte{8}, EffectiveBoundary: protocol.ClientKeyEffectiveBoundary{Block: 70, Hash: [32]byte{9}}}
		if err := protocol.SignClientKeyRegistration(&registration, rootKey); err != nil {
			t.Fatal(err)
		}
		encoded, contentHash, err := startifact.SealClientKeyRegistrationEvidence(deployment, registration, artifactKey, time.Unix(1700000000, 0))
		if err != nil {
			t.Fatal(err)
		}
		wrappers[index] = stClientKeyEvidenceWrapperTest{encoded: encoded, contentHash: contentHash}
	}
	return wrappers
}

// Short timings for tests; the default Workers.
func stClientKeyEvidenceWriterSettingsTest() stClientKeyEvidenceWriterSettings {
	settings := defaultStClientKeyEvidenceWriterSettings()
	settings.MinBackoff = 20 * time.Millisecond
	settings.MaxBackoff = 160 * time.Millisecond
	settings.AttemptTimeout = 30 * time.Second
	return settings
}

// A writer with private metrics, closed when the test ends.
func newStClientKeyEvidenceWriterTest(t testing.TB, settings stClientKeyEvidenceWriterSettings) *stClientKeyEvidenceWriter {
	t.Helper()
	writer := newStClientKeyEvidenceWriter(settings, newStClientKeyEvidenceMetrics())
	t.Cleanup(writer.close)
	return writer
}

// Records every final outcome by content hash.
type stClientKeyEvidenceOutcomesTest struct {
	stateLock sync.Mutex
	outcomes  map[string][]string
}

func observeStClientKeyEvidenceOutcomesTest(writer *stClientKeyEvidenceWriter) *stClientKeyEvidenceOutcomesTest {
	observed := &stClientKeyEvidenceOutcomesTest{outcomes: map[string][]string{}}
	writer.afterOutcomeForTest = func(contentHash string, outcome string) {
		observed.stateLock.Lock()
		defer observed.stateLock.Unlock()
		observed.outcomes[contentHash] = append(observed.outcomes[contentHash], outcome)
	}
	return observed
}

func (self *stClientKeyEvidenceOutcomesTest) snapshot() map[string][]string {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	copied := map[string][]string{}
	for hash, outcomes := range self.outcomes {
		copied[hash] = append([]string(nil), outcomes...)
	}
	return copied
}

func waitStClientKeyEvidenceIdleTest(t testing.TB, writer *stClientKeyEvidenceWriter) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()
	if err := writer.waitIdle(ctx); err != nil {
		t.Fatal("the writer did not finish its publications", err)
	}
}

func stClientKeyEvidencePublicationsTest(writer *stClientKeyEvidenceWriter, outcome string) int {
	return int(testutil.ToFloat64(writer.metrics.publications.WithLabelValues(outcome)))
}

func stClientKeyEvidenceAttemptsTest(writer *stClientKeyEvidenceWriter, result string) int {
	return int(testutil.ToFloat64(writer.metrics.attempts.WithLabelValues(result)))
}

// An in-memory store that counts and can hold, refuse or throttle requests.
type stClientKeyEvidenceStoreTest struct {
	stateLock sync.Mutex
	objects   map[string][]byte
	puts      int
	gets      int
	active    int
	maxActive int
	// every request waits for it to close; nil passes
	gate chan struct{}
	// answers a request before the store does; nil answers nothing
	fault func(operation string, key string, request int) error
	// held for each request
	latency time.Duration
}

func newStClientKeyEvidenceStoreTest() *stClientKeyEvidenceStoreTest {
	return &stClientKeyEvidenceStoreTest{objects: map[string][]byte{}}
}

func (self *stClientKeyEvidenceStoreTest) enter(ctx context.Context, operation string, key string) error {
	gate, fault, latency, request := func() (chan struct{}, func(string, string, int) error, time.Duration, int) {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.active++
		self.maxActive = max(self.maxActive, self.active)
		if operation == "put" {
			self.puts++
		} else {
			self.gets++
		}
		return self.gate, self.fault, self.latency, self.puts + self.gets
	}()
	if gate != nil {
		select {
		case <-gate:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if 0 < latency {
		time.Sleep(latency)
	}
	if fault != nil {
		return fault(operation, key, request)
	}
	return nil
}

func (self *stClientKeyEvidenceStoreTest) leave() {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.active--
}

func (self *stClientKeyEvidenceStoreTest) PutIfAbsent(ctx context.Context, key string, localPath string, contentType string) (bool, error) {
	defer self.leave()
	if err := self.enter(ctx, "put", key); err != nil {
		return false, err
	}
	content, err := os.ReadFile(localPath)
	if err != nil {
		return false, err
	}
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if _, ok := self.objects[key]; ok {
		return false, nil
	}
	self.objects[key] = content
	return true, nil
}

func (self *stClientKeyEvidenceStoreTest) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	defer self.leave()
	if err := self.enter(ctx, "get", key); err != nil {
		return nil, err
	}
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	content, ok := self.objects[key]
	if !ok {
		return nil, &fs.PathError{Op: "open", Path: key, Err: fs.ErrNotExist}
	}
	return io.NopCloser(bytes.NewReader(bytes.Clone(content))), nil
}

func (self *stClientKeyEvidenceStoreTest) Put(ctx context.Context, key string, localPath string, contentType string) error {
	return errors.New("synthetic evidence store accepts only conditional creates")
}

func (self *stClientKeyEvidenceStoreTest) List(ctx context.Context, keyPrefix string) ([]server.BlobObject, error) {
	return nil, errors.New("synthetic evidence store does not list")
}

func (self *stClientKeyEvidenceStoreTest) SetLifecycle(ctx context.Context, rules []server.BlobLifecycleRule) error {
	return nil
}

func (self *stClientKeyEvidenceStoreTest) Bucket() string    { return "synthetic" }
func (self *stClientKeyEvidenceStoreTest) Prefix() string    { return "blob" }
func (self *stClientKeyEvidenceStoreTest) Authority() string { return "synthetic-evidence-store" }

func (self *stClientKeyEvidenceStoreTest) object(t testing.TB, wrapper stClientKeyEvidenceWrapperTest) ([]byte, bool) {
	t.Helper()
	key, err := startifact.EvidenceContentKey(self, wrapper.contentHash)
	if err != nil {
		t.Fatal(err)
	}
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	content, ok := self.objects[key]
	return content, ok
}

func (self *stClientKeyEvidenceStoreTest) counts() (puts int, gets int, maxActive int) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.puts, self.gets, self.maxActive
}

func (self *stClientKeyEvidenceStoreTest) activeRequests() int {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.active
}

// Waits until the store holds n requests, failing at the deadline.
func (self *stClientKeyEvidenceStoreTest) awaitActive(t testing.TB, n int) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for self.activeRequests() < n {
		if time.Now().After(deadline) {
			t.Fatalf("the store never held %d requests", n)
		}
		time.Sleep(time.Millisecond)
	}
}

var stClientKeyEvidenceThrottleTest = minio.ErrorResponse{StatusCode: http.StatusTooManyRequests, Code: "TooManyRequests", Message: "Please reduce your request rate"}

// A burst many times the cap never holds more than Workers storage requests
// at once, reaches the cap, and stores every object.
func TestStClientKeyEvidenceWriterNeverExceedsItsConcurrency(t *testing.T) {
	wrappers := newStClientKeyEvidenceWrappersTest(t, 400)
	settings := stClientKeyEvidenceWriterSettingsTest()
	writer := newStClientKeyEvidenceWriterTest(t, settings)
	store := newStClientKeyEvidenceStoreTest()
	store.latency = 2 * time.Millisecond
	var burst sync.WaitGroup
	for _, wrapper := range wrappers {
		burst.Go(func() {
			if err := writer.enqueue(store, wrapper.encoded, wrapper.contentHash, false); err != nil {
				t.Error(err)
			}
		})
	}
	burst.Wait()
	waitStClientKeyEvidenceIdleTest(t, writer)
	puts, gets, maxActive := store.counts()
	if maxActive != settings.Workers {
		t.Fatalf("the burst held %d concurrent storage requests, want the cap %d", maxActive, settings.Workers)
	}
	if puts != len(wrappers) || gets != len(wrappers) {
		t.Fatalf("puts=%d gets=%d for %d publications", puts, gets, len(wrappers))
	}
	for _, wrapper := range wrappers {
		if content, ok := store.object(t, wrapper); !ok || !bytes.Equal(content, wrapper.encoded) {
			t.Fatal("an admitted publication was not stored exactly")
		}
	}
	if ok := stClientKeyEvidencePublicationsTest(writer, stClientKeyEvidenceOutcomeOk); ok != len(wrappers) {
		t.Fatalf("ok outcomes = %d", ok)
	}
	if depth := testutil.ToFloat64(writer.metrics.queueDepth); depth != 0 {
		t.Fatalf("queue depth %v after quiescence", depth)
	}
}

// Admission returns while storage holds every request; the publication
// completes after storage answers.
func TestStClientKeyEvidenceWriterAdmissionDoesNotWaitForStorage(t *testing.T) {
	wrappers := newStClientKeyEvidenceWrappersTest(t, 8)
	writer := newStClientKeyEvidenceWriterTest(t, stClientKeyEvidenceWriterSettingsTest())
	store := newStClientKeyEvidenceStoreTest()
	gate := make(chan struct{})
	store.gate = gate
	opened := sync.OnceFunc(func() { close(gate) })
	t.Cleanup(opened)
	admitted := make(chan error, 1)
	go func() {
		for _, wrapper := range wrappers {
			if err := writer.enqueue(store, wrapper.encoded, wrapper.contentHash, false); err != nil {
				admitted <- err
				return
			}
		}
		admitted <- nil
	}()
	select {
	case err := <-admitted:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("admission waited for the held store")
	}
	// every admission has returned and the store still holds its requests
	store.awaitActive(t, writer.settings.Workers)
	if _, ok := store.object(t, wrappers[0]); ok {
		t.Fatal("storage completed while it was held")
	}
	opened()
	waitStClientKeyEvidenceIdleTest(t, writer)
	for _, wrapper := range wrappers {
		if content, ok := store.object(t, wrapper); !ok || !bytes.Equal(content, wrapper.encoded) {
			t.Fatal("a publication admitted while storage was held was not stored")
		}
	}
}

// Identical publications pending together send one write; verified content
// is remembered (bounded, expiring) and sends nothing; an exact retry reads
// before it writes.
func TestStClientKeyEvidenceWriterSkipsDuplicates(t *testing.T) {
	wrappers := newStClientKeyEvidenceWrappersTest(t, 3)
	settings := stClientKeyEvidenceWriterSettingsTest()
	settings.KnownCapacity = 2
	writer := newStClientKeyEvidenceWriterTest(t, settings)
	var clockLock sync.Mutex
	now := time.Now()
	writer.now = func() time.Time {
		clockLock.Lock()
		defer clockLock.Unlock()
		return now
	}
	advance := func(by time.Duration) {
		clockLock.Lock()
		defer clockLock.Unlock()
		now = now.Add(by)
	}
	store := newStClientKeyEvidenceStoreTest()
	gate := make(chan struct{})
	store.gate = gate
	first := wrappers[0]
	var duplicates sync.WaitGroup
	for range 50 {
		duplicates.Go(func() {
			if err := writer.enqueue(store, first.encoded, first.contentHash, false); err != nil {
				t.Error(err)
			}
		})
	}
	duplicates.Wait()
	close(gate)
	waitStClientKeyEvidenceIdleTest(t, writer)
	if puts, gets, _ := store.counts(); puts != 1 || gets != 1 {
		t.Fatalf("50 identical publications sent puts=%d gets=%d", puts, gets)
	}
	if ok, exists := stClientKeyEvidencePublicationsTest(writer, stClientKeyEvidenceOutcomeOk), stClientKeyEvidencePublicationsTest(writer, stClientKeyEvidenceOutcomeExists); ok != 1 || exists != 49 {
		t.Fatalf("outcomes ok=%d exists=%d", ok, exists)
	}
	// remembered: nothing is sent
	if err := writer.enqueue(store, first.encoded, first.contentHash, false); err != nil {
		t.Fatal(err)
	}
	waitStClientKeyEvidenceIdleTest(t, writer)
	if puts, gets, _ := store.counts(); puts != 1 || gets != 1 {
		t.Fatalf("remembered content sent puts=%d gets=%d", puts, gets)
	}
	// expired: an exact retry reads, finds the stored bytes and writes nothing
	advance(settings.KnownTtl)
	if err := writer.enqueue(store, first.encoded, first.contentHash, true); err != nil {
		t.Fatal(err)
	}
	waitStClientKeyEvidenceIdleTest(t, writer)
	if puts, gets, _ := store.counts(); puts != 1 || gets != 2 {
		t.Fatalf("an expired exact retry sent puts=%d gets=%d", puts, gets)
	}
	if exists := stClientKeyEvidenceAttemptsTest(writer, stClientKeyEvidenceOutcomeExists); exists != 1 {
		t.Fatalf("exists attempts = %d", exists)
	}
	// bounded: verifying two more evicts the first
	for _, wrapper := range wrappers[1:] {
		if err := writer.enqueue(store, wrapper.encoded, wrapper.contentHash, false); err != nil {
			t.Fatal(err)
		}
		waitStClientKeyEvidenceIdleTest(t, writer)
	}
	func() {
		writer.stateLock.Lock()
		defer writer.stateLock.Unlock()
		if writer.known.len() != settings.KnownCapacity {
			t.Fatalf("the memory holds %d entries, want its capacity %d", writer.known.len(), settings.KnownCapacity)
		}
	}()
	if err := writer.enqueue(store, first.encoded, first.contentHash, false); err != nil {
		t.Fatal(err)
	}
	waitStClientKeyEvidenceIdleTest(t, writer)
	if puts, _, _ := store.counts(); puts != 4 {
		t.Fatalf("an evicted entry was not verified again: puts=%d", puts)
	}
}

// Every throttled or failed attempt pauses the writer for a jittered delay of
// at least MinBackoff before its next request, and the delays grow with the
// failure count. With one worker the next request after a refusal can only
// come from a later attempt.
func TestStClientKeyEvidenceWriterBacksOffAfterThrottling(t *testing.T) {
	wrappers := newStClientKeyEvidenceWrappersTest(t, 4)
	settings := stClientKeyEvidenceWriterSettingsTest()
	settings.Workers = 1
	settings.MaxAttempts = 100
	writer := newStClientKeyEvidenceWriterTest(t, settings)
	type requestTest struct {
		start     time.Time
		end       time.Time
		throttled bool
	}
	var requestsLock sync.Mutex
	var requests []requestTest
	store := newStClientKeyEvidenceStoreTest()
	store.fault = func(operation string, key string, request int) error {
		requestsLock.Lock()
		defer requestsLock.Unlock()
		throttled := request <= 12
		requests = append(requests, requestTest{start: time.Now(), throttled: throttled})
		if throttled {
			return stClientKeyEvidenceThrottleTest
		}
		return nil
	}
	var delaysLock sync.Mutex
	var delays []time.Duration
	var delayFailures []int
	writer.afterBackoffForTest = func(failures int, delay time.Duration) {
		delaysLock.Lock()
		defer delaysLock.Unlock()
		delays = append(delays, delay)
		delayFailures = append(delayFailures, failures)
	}
	for _, wrapper := range wrappers {
		if err := writer.enqueue(store, wrapper.encoded, wrapper.contentHash, false); err != nil {
			t.Fatal(err)
		}
	}
	waitStClientKeyEvidenceIdleTest(t, writer)
	for _, wrapper := range wrappers {
		if content, ok := store.object(t, wrapper); !ok || !bytes.Equal(content, wrapper.encoded) {
			t.Fatal("a throttled publication was not stored after its backoff")
		}
	}
	requestsLock.Lock()
	defer requestsLock.Unlock()
	if len(requests) != 12+2*len(wrappers) {
		t.Fatalf("%d requests, want 12 refused attempts and one create plus readback each", len(requests))
	}
	for index := 1; index < len(requests); index++ {
		if requests[index-1].throttled {
			if gap := requests[index].start.Sub(requests[index-1].start); gap < settings.MinBackoff {
				t.Fatalf("request %d followed a 429 after %v, under the %v floor", index, gap, settings.MinBackoff)
			}
		}
	}
	delaysLock.Lock()
	defer delaysLock.Unlock()
	if len(delays) != 12 {
		t.Fatalf("%d pauses for 12 refused attempts", len(delays))
	}
	distinct := map[time.Duration]bool{}
	for index, delay := range delays {
		ceiling := settings.MinBackoff << min(delayFailures[index], 16)
		if delay < settings.MinBackoff || min(ceiling, settings.MaxBackoff) < delay {
			t.Fatalf("pause %v after %d failures is outside [%v, %v]", delay, delayFailures[index], settings.MinBackoff, min(ceiling, settings.MaxBackoff))
		}
		distinct[delay] = true
	}
	if len(distinct) < 2 {
		t.Fatal("every pause had the same length: the backoff is not jittered")
	}
	if throttled := stClientKeyEvidenceAttemptsTest(writer, stClientKeyEvidenceOutcomeThrottled); throttled != 12 {
		t.Fatalf("throttled attempts = %d", throttled)
	}
}

// Every admitted or refused publication gets exactly one final outcome:
// stored (ok) or reported throttled, error or dropped, with counters to match.
func TestStClientKeyEvidenceWriterReportsEveryPublication(t *testing.T) {
	wrappers := newStClientKeyEvidenceWrappersTest(t, 40)
	settings := stClientKeyEvidenceWriterSettingsTest()
	settings.QueueCapacity = 16
	settings.MaxAttempts = 3
	writer := newStClientKeyEvidenceWriterTest(t, settings)
	outcomes := observeStClientKeyEvidenceOutcomesTest(writer)
	store := newStClientKeyEvidenceStoreTest()
	keys := map[string]int{}
	for index, wrapper := range wrappers {
		key, err := startifact.EvidenceContentKey(store, wrapper.contentHash)
		if err != nil {
			t.Fatal(err)
		}
		keys[key] = index
	}
	var faultLock sync.Mutex
	requests := map[string]int{}
	store.fault = func(operation string, key string, request int) error {
		faultLock.Lock()
		defer faultLock.Unlock()
		requests[key]++
		switch keys[key] % 4 {
		case 1:
			return stClientKeyEvidenceThrottleTest
		case 2:
			return minio.ErrorResponse{StatusCode: http.StatusForbidden, Code: "AccessDenied", Message: "synthetic refusal"}
		case 3:
			// the first create fails in transit; the next attempt succeeds
			if requests[key] == 1 {
				return errors.New("synthetic connection reset")
			}
		}
		return nil
	}
	gate := make(chan struct{})
	store.gate = gate
	for _, wrapper := range wrappers {
		if err := writer.enqueue(store, wrapper.encoded, wrapper.contentHash, false); err != nil {
			t.Fatal(err)
		}
	}
	close(gate)
	waitStClientKeyEvidenceIdleTest(t, writer)
	census := map[string]int{}
	for index, wrapper := range wrappers {
		final := outcomes.snapshot()[wrapper.contentHash]
		if len(final) != 1 {
			t.Fatalf("publication %d has outcomes %v, want exactly one", index, final)
		}
		outcome := final[0]
		census[outcome]++
		content, stored := store.object(t, wrapper)
		if (outcome == stClientKeyEvidenceOutcomeOk) != stored || stored && !bytes.Equal(content, wrapper.encoded) {
			t.Fatalf("publication %d: outcome %s but stored=%t", index, outcome, stored)
		}
		if index < settings.QueueCapacity {
			want := map[int]string{0: stClientKeyEvidenceOutcomeOk, 1: stClientKeyEvidenceOutcomeThrottled, 2: stClientKeyEvidenceOutcomeError, 3: stClientKeyEvidenceOutcomeOk}[index%4]
			if outcome != want {
				t.Fatalf("admitted publication %d: outcome %s, want %s", index, outcome, want)
			}
		} else if outcome != stClientKeyEvidenceOutcomeDropped {
			t.Fatalf("publication %d over the queue capacity: outcome %s, want dropped", index, outcome)
		}
	}
	total := 0
	for _, outcome := range []string{stClientKeyEvidenceOutcomeOk, stClientKeyEvidenceOutcomeExists, stClientKeyEvidenceOutcomeThrottled, stClientKeyEvidenceOutcomeError, stClientKeyEvidenceOutcomeDropped} {
		counted := stClientKeyEvidencePublicationsTest(writer, outcome)
		if counted != census[outcome] {
			t.Fatalf("%s counter %d, census %d", outcome, counted, census[outcome])
		}
		total += counted
	}
	if total != len(wrappers) || census[stClientKeyEvidenceOutcomeDropped] != len(wrappers)-settings.QueueCapacity {
		t.Fatalf("census %v for %d publications", census, len(wrappers))
	}
}

// Closing the writer reports everything still pending as dropped.
func TestStClientKeyEvidenceWriterCloseReportsPending(t *testing.T) {
	wrappers := newStClientKeyEvidenceWrappersTest(t, 6)
	writer := newStClientKeyEvidenceWriterTest(t, stClientKeyEvidenceWriterSettingsTest())
	outcomes := observeStClientKeyEvidenceOutcomesTest(writer)
	store := newStClientKeyEvidenceStoreTest()
	store.gate = make(chan struct{})
	for _, wrapper := range wrappers {
		if err := writer.enqueue(store, wrapper.encoded, wrapper.contentHash, false); err != nil {
			t.Fatal(err)
		}
	}
	store.awaitActive(t, writer.settings.Workers)
	writer.close()
	waitStClientKeyEvidenceIdleTest(t, writer)
	if err := writer.enqueue(store, wrappers[0].encoded, wrappers[0].contentHash, false); err != nil {
		t.Fatal(err)
	}
	for _, wrapper := range wrappers {
		final := outcomes.snapshot()[wrapper.contentHash]
		if len(final) < 1 || final[0] != stClientKeyEvidenceOutcomeDropped {
			t.Fatalf("a publication pending at close has outcomes %v", final)
		}
	}
	if dropped := stClientKeyEvidencePublicationsTest(writer, stClientKeyEvidenceOutcomeDropped); dropped != len(wrappers)+1 {
		t.Fatalf("dropped = %d", dropped)
	}
}

// A synthetic S3 endpoint with MinIO's admission: a fixed request pool, and
// 429 TooManyRequests at once for a request that finds it full.
type stClientKeyEvidenceS3Test struct {
	server *httptest.Server

	stateLock sync.Mutex
	objects   map[string][]byte
	pool      int
	admitted  int
	latency   time.Duration
	// admitted creates wait for it to close; nil passes
	gate chan struct{}
	// creates waiting for the gate
	held     int
	arrivals int
	// the most requests the endpoint held at once, refused ones included
	maxArrivals int
	requests    []stClientKeyEvidenceS3RequestTest
	locations   int
}

type stClientKeyEvidenceS3RequestTest struct {
	method string
	status int
	start  time.Time
	end    time.Time
}

func newStClientKeyEvidenceS3Test(t testing.TB, pool int, latency time.Duration) *stClientKeyEvidenceS3Test {
	t.Helper()
	fake := &stClientKeyEvidenceS3Test{objects: map[string][]byte{}, pool: pool, latency: latency}
	fake.server = httptest.NewServer(http.HandlerFunc(fake.serve))
	t.Cleanup(fake.server.Close)
	return fake
}

func (self *stClientKeyEvidenceS3Test) authority() string {
	return strings.TrimPrefix(self.server.URL, "http://")
}

func (self *stClientKeyEvidenceS3Test) store(t testing.TB) server.BlobStore {
	t.Helper()
	store, err := server.NewBlobStore(&server.BlobStoreConfig{Authority: self.authority(), Bucket: "synthetic", Prefix: "blob", AccessKey: "synthetic-access", SecretKey: "synthetic-secret", Region: server.DefaultBlobRegion})
	if err != nil {
		t.Fatal(err)
	}
	return store
}

func (self *stClientKeyEvidenceS3Test) serve(w http.ResponseWriter, r *http.Request) {
	request := stClientKeyEvidenceS3RequestTest{method: r.Method, start: time.Now()}
	refuse := func() bool {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.arrivals++
		self.maxArrivals = max(self.maxArrivals, self.arrivals)
		if r.URL.Query().Has("location") {
			self.locations++
		}
		if 0 < self.pool && self.pool <= self.admitted {
			return true
		}
		self.admitted++
		return false
	}()
	defer func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.arrivals--
		if !refuse {
			self.admitted--
		}
		request.end = time.Now()
		self.requests = append(self.requests, request)
	}()
	var reader io.Reader = r.Body
	if strings.HasPrefix(r.Header.Get("X-Amz-Content-Sha256"), "STREAMING-") {
		reader = httputil.NewChunkedReader(r.Body)
	}
	body, err := io.ReadAll(reader)
	if err != nil {
		request.status = http.StatusBadRequest
		http.Error(w, "synthetic body read failed", request.status)
		return
	}
	writeXml := func(status int, content string) {
		request.status = status
		w.Header().Set("Content-Type", "application/xml")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, content)
	}
	if refuse {
		writeXml(http.StatusTooManyRequests, `<Error><Code>TooManyRequests</Code><Message>Please reduce your request rate</Message></Error>`)
		return
	}
	time.Sleep(self.latency)
	path := strings.TrimPrefix(r.URL.Path, "/")
	switch r.Method {
	case http.MethodPut:
		gate := func() chan struct{} {
			self.stateLock.Lock()
			defer self.stateLock.Unlock()
			if self.gate != nil {
				self.held++
			}
			return self.gate
		}()
		if gate != nil {
			select {
			case <-gate:
			case <-r.Context().Done():
			}
			func() {
				self.stateLock.Lock()
				defer self.stateLock.Unlock()
				self.held--
			}()
		}
		created := func() bool {
			self.stateLock.Lock()
			defer self.stateLock.Unlock()
			if _, ok := self.objects[path]; ok && r.Header.Get("If-None-Match") == "*" {
				return false
			}
			self.objects[path] = body
			return true
		}()
		if !created {
			writeXml(http.StatusPreconditionFailed, `<Error><Code>PreconditionFailed</Code><Message>exists</Message></Error>`)
			return
		}
		request.status = http.StatusOK
		w.Header().Set("ETag", `"synthetic"`)
		w.WriteHeader(http.StatusOK)
	case http.MethodGet:
		content, ok := func() ([]byte, bool) {
			self.stateLock.Lock()
			defer self.stateLock.Unlock()
			content, ok := self.objects[path]
			return content, ok
		}()
		if !ok {
			writeXml(http.StatusNotFound, `<Error><Code>NoSuchKey</Code><Message>none</Message></Error>`)
			return
		}
		request.status = http.StatusOK
		w.Header().Set("Content-Length", fmt.Sprint(len(content)))
		w.Header().Set("Last-Modified", time.Now().UTC().Format(http.TimeFormat))
		w.Header().Set("ETag", `"synthetic"`)
		_, _ = w.Write(content)
	default:
		writeXml(http.StatusBadRequest, `<Error><Code>InvalidRequest</Code><Message>unexpected</Message></Error>`)
	}
}

// Holds every admitted create until gate closes.
func (self *stClientKeyEvidenceS3Test) holdCreates(gate chan struct{}) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.gate = gate
}

// Waits until n creates are held at the gate, failing at the deadline.
func (self *stClientKeyEvidenceS3Test) awaitHeld(t testing.TB, n int) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		held := func() int {
			self.stateLock.Lock()
			defer self.stateLock.Unlock()
			return self.held
		}()
		if n <= held {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the endpoint never held %d creates", n)
		}
		time.Sleep(time.Millisecond)
	}
}

// Requests by method.
func (self *stClientKeyEvidenceS3Test) methods() (puts int, gets int) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	for _, request := range self.requests {
		switch request.method {
		case http.MethodPut:
			puts++
		case http.MethodGet:
			gets++
		}
	}
	return puts, gets
}

func (self *stClientKeyEvidenceS3Test) object(path string) ([]byte, bool) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	content, ok := self.objects[path]
	return content, ok
}

func (self *stClientKeyEvidenceS3Test) census() (requests []stClientKeyEvidenceS3RequestTest, maxArrivals int, locations int) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return append([]stClientKeyEvidenceS3RequestTest(nil), self.requests...), self.maxArrivals, self.locations
}

// Through the actual MinIO client against a request pool smaller than the
// writer's concurrency: refusals are answered with backoff, never more than
// Workers requests reach the endpoint at once, no request is retried by the
// SDK, and every publication is stored.
func TestStClientKeyEvidenceWriterThrottledByFullRequestPool(t *testing.T) {
	wrappers := newStClientKeyEvidenceWrappersTest(t, 60)
	fake := newStClientKeyEvidenceS3Test(t, 2, 5*time.Millisecond)
	store := fake.store(t)
	settings := stClientKeyEvidenceWriterSettingsTest()
	settings.MaxAttempts = 1000
	writer := newStClientKeyEvidenceWriterTest(t, settings)
	for _, wrapper := range wrappers {
		if err := writer.enqueue(store, wrapper.encoded, wrapper.contentHash, false); err != nil {
			t.Fatal(err)
		}
	}
	waitStClientKeyEvidenceIdleTest(t, writer)
	for _, wrapper := range wrappers {
		key, err := startifact.EvidenceContentKey(store, wrapper.contentHash)
		if err != nil {
			t.Fatal(err)
		}
		if content, ok := fake.object("synthetic/" + key); !ok || !bytes.Equal(content, wrapper.encoded) {
			t.Fatal("a publication refused by the full pool was never stored")
		}
	}
	requests, maxArrivals, locations := fake.census()
	if maxArrivals > settings.Workers {
		t.Fatalf("the endpoint held %d requests at once, over the writer's %d", maxArrivals, settings.Workers)
	}
	if locations != 0 {
		t.Fatalf("%d bucket location lookups", locations)
	}
	throttled := 0
	for _, request := range requests {
		if request.status == http.StatusTooManyRequests {
			throttled++
		}
	}
	if throttled == 0 {
		t.Fatal("the request pool never refused the writer; the test proves nothing")
	}
	attempts := 0
	for _, result := range []string{stClientKeyEvidenceOutcomeOk, stClientKeyEvidenceOutcomeExists, stClientKeyEvidenceOutcomeThrottled, stClientKeyEvidenceOutcomeError} {
		attempts += stClientKeyEvidenceAttemptsTest(writer, result)
	}
	if throttledAttempts := stClientKeyEvidenceAttemptsTest(writer, stClientKeyEvidenceOutcomeThrottled); throttledAttempts != throttled {
		t.Fatalf("%d 429 answers but %d throttled attempts: the SDK retried a refusal", throttled, throttledAttempts)
	}
	// each successful attempt is one create and one readback; a refused one
	// ends with its one refused request, after its create when the readback
	// was refused
	if len(requests) != 2*len(wrappers)+throttled+stClientKeyEvidenceReadbackRefusalsTest(requests) {
		t.Fatalf("%d requests for %d attempts", len(requests), attempts)
	}
}

// Refused readbacks follow an accepted create in the same attempt.
func stClientKeyEvidenceReadbackRefusalsTest(requests []stClientKeyEvidenceS3RequestTest) int {
	refused := 0
	for _, request := range requests {
		if request.method == http.MethodGet && request.status == http.StatusTooManyRequests {
			refused++
		}
	}
	return refused
}
