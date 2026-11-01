// Synthetic HTTP peers exercise the actual MinIO SDK, retained-write wiring
// and preflight retry owner without live storage, sleeps or production identity.
package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

// The synthetic peer selects a precise location response for each discovery.
type blobLocationTestResponse struct {
	status     int
	code       string
	retryAfter string
}

// Counts observable HTTP work, not scheduler timing, at the production API.
type blobLocationRetryFixture struct {
	store               *minioBlobStore
	content             []byte
	localPath           string
	retainUntil         time.Time
	locationGets        atomic.Int32
	puts                atomic.Int32
	retentionGets       atomic.Int32
	versionGets         atomic.Int32
	stateLock           sync.Mutex
	requests            []string
	locationBodyBarrier func(int)
}

// Builds through NewBlobStore so an unused helper cannot make the regression
// pass while production construction still bypasses bounded preflight recovery.
func newBlobLocationRetryFixture(t *testing.T, location func(int) blobLocationTestResponse) *blobLocationRetryFixture {
	t.Helper()
	return newBlobLocationRetryFixtureWithSettings(t, location, nil)
}

// Optional programmatic settings exercise the same production constructor.
func newBlobLocationRetryFixtureWithSettings(t *testing.T, location func(int) blobLocationTestResponse, settings *BlobBucketLocationRetrySettings) *blobLocationRetryFixture {
	t.Helper()
	fixture := &blobLocationRetryFixture{
		content:     []byte("synthetic retained artifact bytes\n"),
		localPath:   filepath.Join(t.TempDir(), "synthetic.json"),
		retainUntil: time.Now().UTC().Add(24 * time.Hour).Truncate(time.Second),
	}
	if err := os.WriteFile(fixture.localPath, fixture.content, 0o600); err != nil {
		t.Fatal(err)
	}
	record := func(value string) {
		fixture.stateLock.Lock()
		defer fixture.stateLock.Unlock()
		fixture.requests = append(fixture.requests, value)
	}
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		query := request.URL.Query()
		switch {
		case request.Method == http.MethodGet && query.Has("location"):
			attempt := int(fixture.locationGets.Add(1))
			response := location(attempt)
			record("location:" + strconv.Itoa(response.status))
			w.Header().Set("Content-Type", "application/xml")
			if response.retryAfter != "" {
				w.Header().Set("Retry-After", response.retryAfter)
			}
			w.WriteHeader(response.status)
			if fixture.locationBodyBarrier != nil {
				w.(http.Flusher).Flush()
				fixture.locationBodyBarrier(attempt)
			}
			if response.status == http.StatusOK {
				fmt.Fprint(w, `<LocationConstraint>us-east-1</LocationConstraint>`)
			} else {
				fmt.Fprintf(w, `<Error><Code>%s</Code><Message>synthetic throttle evidence</Message></Error>`, response.code)
			}
		case request.Method == http.MethodPut:
			fixture.puts.Add(1)
			record("put")
			var reader io.Reader = request.Body
			if strings.HasPrefix(request.Header.Get("X-Amz-Content-Sha256"), "STREAMING-AWS4-HMAC-SHA256-PAYLOAD") {
				reader = httputil.NewChunkedReader(request.Body)
			}
			value, err := io.ReadAll(reader)
			if err != nil || !bytes.Equal(value, fixture.content) || request.Header.Get("X-Amz-Object-Lock-Mode") != "COMPLIANCE" ||
				request.Header.Get("X-Amz-Object-Lock-Retain-Until-Date") != fixture.retainUntil.Format(time.RFC3339) {
				http.Error(w, "synthetic upload contract mismatch", http.StatusBadRequest)
				return
			}
			w.Header().Set("X-Amz-Version-Id", "synthetic-version")
			w.Header().Set("ETag", `"`+strings.Repeat("a", 32)+`"`)
			w.WriteHeader(http.StatusOK)
		case request.Method == http.MethodGet && query.Has("retention"):
			fixture.retentionGets.Add(1)
			record("retention:" + query.Get("versionId"))
			if query.Get("versionId") != "synthetic-version" {
				http.Error(w, "synthetic version identity mismatch", http.StatusBadRequest)
				return
			}
			w.Header().Set("Content-Type", "application/xml")
			fmt.Fprintf(w, `<Retention><Mode>COMPLIANCE</Mode><RetainUntilDate>%s</RetainUntilDate></Retention>`, fixture.retainUntil.Format(time.RFC3339))
		case request.Method == http.MethodGet:
			fixture.versionGets.Add(1)
			record("get:" + query.Get("versionId"))
			if query.Get("versionId") != "synthetic-version" {
				http.Error(w, "synthetic version identity mismatch", http.StatusBadRequest)
				return
			}
			w.Header().Set("Content-Length", strconv.Itoa(len(fixture.content)))
			w.Header().Set("Last-Modified", time.Now().UTC().Format(http.TimeFormat))
			w.Header().Set("ETag", `"`+strings.Repeat("a", 32)+`"`)
			w.Header().Set("X-Amz-Version-Id", "synthetic-version")
			w.Write(fixture.content)
		default:
			http.Error(w, "unexpected synthetic storage request", http.StatusBadRequest)
		}
	}))
	t.Cleanup(peer.Close)
	store, err := NewBlobStore(&BlobStoreConfig{
		Authority: strings.TrimPrefix(peer.URL, "http://"), Bucket: "synthetic-evidence", Prefix: "synthetic",
		AccessKey: "synthetic-access", SecretKey: "synthetic-secret",
		BucketLocationRetrySettings: settings,
	})
	if err != nil {
		t.Fatal(err)
	}
	fixture.store = store.(*minioBlobStore)
	wanted := DefaultBlobBucketLocationRetrySettings()
	if settings != nil {
		wanted = *settings
	}
	if fixture.store.bucketLocationRetry == nil || fixture.store.bucketLocationRetry.settings != wanted {
		t.Fatal("production constructor omitted owned location-retry defaults")
	}
	return fixture
}

// A first 429 followed by successful discovery must still perform exactly one
// compliance upload, query its exact version and authenticate its complete bytes.
func TestBlobStoreRetainedBucketLocationThrottleRecovers(t *testing.T) {
	fixture := newBlobLocationRetryFixture(t, func(attempt int) blobLocationTestResponse {
		if attempt == 1 {
			return blobLocationTestResponse{status: 429, code: "TooManyRequests", retryAfter: "1"}
		}
		return blobLocationTestResponse{status: 200}
	})
	now := time.Now()
	fixture.store.bucketLocationRetry.now = func() time.Time { return now }
	var delays []time.Duration
	fixture.store.bucketLocationRetry.wait = func(ctx context.Context, delay time.Duration) error {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		delays = append(delays, delay)
		return nil
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	proof, err := fixture.store.PutRetained(ctx, "synthetic/artifact.json", fixture.localPath, "application/json", fixture.retainUntil)
	if err != nil || proof == nil || proof.Mode != "COMPLIANCE" || proof.VersionId != "synthetic-version" ||
		proof.Size != int64(len(fixture.content)) || !proof.RetainUntil.Equal(fixture.retainUntil) {
		t.Fatalf("recovered retention proof = %+v, locations=%d puts=%d error=%v", proof, fixture.locationGets.Load(), fixture.puts.Load(), err)
	}
	reader, err := fixture.store.GetVersion(ctx, proof.Key, proof.VersionId)
	if err != nil {
		t.Fatal(err)
	}
	content, readErr := io.ReadAll(reader)
	closeErr := reader.Close()
	if readErr != nil || closeErr != nil || sha256.Sum256(content) != sha256.Sum256(fixture.content) || ctx.Err() != nil {
		t.Fatalf("recovered exact-version body failed authentication: read=%v close=%v", readErr, closeErr)
	}
	fixture.stateLock.Lock()
	requests := append([]string(nil), fixture.requests...)
	fixture.stateLock.Unlock()
	if !reflect.DeepEqual(delays, []time.Duration{time.Second}) || !reflect.DeepEqual(requests, []string{
		"location:429", "location:200", "put", "retention:synthetic-version", "get:synthetic-version",
	}) {
		t.Fatalf("retry or immutable-write work changed: delays=%v requests=%v", delays, requests)
	}
}

// Persistent throttling stops at the owner count and never creates an object
// version, even though every discovery request is independently retryable.
func TestBlobStoreRetainedBucketLocationThrottleExhaustionDoesNotUpload(t *testing.T) {
	fixture := newBlobLocationRetryFixture(t, func(int) blobLocationTestResponse {
		return blobLocationTestResponse{status: 429, code: "TooManyRequests"}
	})
	var delays []time.Duration
	fixture.store.bucketLocationRetry.wait = func(_ context.Context, delay time.Duration) error {
		delays = append(delays, delay)
		return nil
	}
	proof, err := fixture.store.PutRetained(t.Context(), "synthetic/artifact.json", fixture.localPath, "application/json", fixture.retainUntil)
	if proof != nil || !blobBucketLocationThrottled(err) || fixture.locationGets.Load() != 3 ||
		fixture.puts.Load() != 0 || fixture.retentionGets.Load() != 0 || fixture.versionGets.Load() != 0 ||
		!reflect.DeepEqual(delays, []time.Duration{250 * time.Millisecond, 500 * time.Millisecond}) {
		t.Fatalf("unbounded throttle work: locations=%d puts=%d delays=%v error=%v", fixture.locationGets.Load(), fixture.puts.Load(), delays, err)
	}
}

// Explicit cancellation wins while the first backoff is held at a barrier;
// neither scheduling luck nor a short negative timeout proves the absence of PUTs.
func TestBlobStoreRetainedBucketLocationThrottleCancellationDoesNotUpload(t *testing.T) {
	fixture := newBlobLocationRetryFixture(t, func(int) blobLocationTestResponse {
		return blobLocationTestResponse{status: 429, code: "TooManyRequests"}
	})
	guard, stopGuard := context.WithTimeout(t.Context(), time.Minute)
	defer stopGuard()
	ctx, cancel := context.WithCancel(guard)
	defer cancel()
	waiting := make(chan struct{})
	fixture.store.bucketLocationRetry.wait = func(retryCtx context.Context, _ time.Duration) error {
		close(waiting)
		<-retryCtx.Done()
		return retryCtx.Err()
	}
	done := make(chan error, 1)
	go func() {
		_, err := fixture.store.PutRetained(ctx, "synthetic/artifact.json", fixture.localPath, "application/json", fixture.retainUntil)
		done <- err
	}()
	select {
	case <-waiting:
	case <-guard.Done():
		t.Fatal("preflight did not reach its explicit backoff barrier")
	}
	cancel()
	var err error
	select {
	case err = <-done:
	case <-guard.Done():
		t.Fatal("canceled preflight did not finish")
	}
	if !errors.Is(err, context.Canceled) || fixture.locationGets.Load() != 1 || fixture.puts.Load() != 0 {
		t.Fatalf("canceled preflight retained work: locations=%d puts=%d error=%v", fixture.locationGets.Load(), fixture.puts.Load(), err)
	}
}

// A remote delay outside the owned window is a refusal, never permission to
// retry sooner than the server requested or extend the submission deadline.
func TestBlobStoreRetainedBucketLocationRetryAfterBeyondWindowDoesNotUpload(t *testing.T) {
	for _, retryAfter := range []string{"60", "999999999999999999999999"} {
		fixture := newBlobLocationRetryFixture(t, func(int) blobLocationTestResponse {
			return blobLocationTestResponse{status: 429, code: "TooManyRequests", retryAfter: retryAfter}
		})
		fixture.store.bucketLocationRetry.wait = func(context.Context, time.Duration) error {
			t.Fatal("retry ignored the longer remote Retry-After")
			return nil
		}
		_, err := fixture.store.PutRetained(t.Context(), "synthetic/artifact.json", fixture.localPath, "application/json", fixture.retainUntil)
		if !blobBucketLocationThrottled(err) || fixture.locationGets.Load() != 1 || fixture.puts.Load() != 0 {
			t.Fatalf("long Retry-After escaped the window: locations=%d puts=%d error=%v", fixture.locationGets.Load(), fixture.puts.Load(), err)
		}
	}
}

// Advancing the explicit clock after one wait proves the window does not
// restart with each attempt, while its child never exceeds the parent deadline.
func TestBlobStoreRetainedBucketLocationRetryWindowKeepsOriginalDeadline(t *testing.T) {
	fixture := newBlobLocationRetryFixture(t, func(int) blobLocationTestResponse {
		return blobLocationTestResponse{status: 429, code: "TooManyRequests"}
	})
	now := time.Now()
	parentDeadline := now.Add(time.Hour)
	ctx, cancel := context.WithDeadline(t.Context(), parentDeadline)
	defer cancel()
	fixture.store.bucketLocationRetry.now = func() time.Time { return now }
	fixture.store.bucketLocationRetry.settings.RetryWindow = 2 * time.Hour
	waits := 0
	fixture.store.bucketLocationRetry.wait = func(retryCtx context.Context, _ time.Duration) error {
		deadline, present := retryCtx.Deadline()
		if !present || !deadline.Equal(parentDeadline) {
			t.Fatal("preflight extended its caller deadline")
		}
		waits++
		now = parentDeadline.Add(time.Second)
		return nil
	}
	_, err := fixture.store.PutRetained(ctx, "synthetic/artifact.json", fixture.localPath, "application/json", fixture.retainUntil)
	if !blobBucketLocationThrottled(err) || waits != 1 || fixture.locationGets.Load() != 1 || fixture.puts.Load() != 0 {
		t.Fatalf("elapsed retry work was restarted: waits=%d locations=%d puts=%d", waits, fixture.locationGets.Load(), fixture.puts.Load())
	}
}

// Already retryable SDK S3 codes retain their original owner; the new loop
// must not multiply SlowDown's existing retries by its own attempt count.
func TestBlobStoreRetainedBucketLocationPreservesSdkRetryOwner(t *testing.T) {
	fixture := newBlobLocationRetryFixture(t, func(attempt int) blobLocationTestResponse {
		if attempt == 1 {
			return blobLocationTestResponse{status: 429, code: "SlowDown"}
		}
		return blobLocationTestResponse{status: 200}
	})
	fixture.store.bucketLocationRetry.wait = func(context.Context, time.Duration) error {
		t.Fatal("new retry owner intercepted the SDK's SlowDown classification")
		return nil
	}
	proof, err := fixture.store.PutRetained(t.Context(), "synthetic/artifact.json", fixture.localPath, "application/json", fixture.retainUntil)
	if err != nil || proof == nil || fixture.locationGets.Load() != 2 || fixture.puts.Load() != 1 || fixture.retentionGets.Load() != 1 {
		t.Fatalf("existing SDK fallback changed: locations=%d puts=%d error=%v", fixture.locationGets.Load(), fixture.puts.Load(), err)
	}
}

// Explicit disabling preserves the original single cold-location failure;
// mutating the caller's settings later cannot enable retries on this client.
func TestBlobStoreRetainedBucketLocationSettingsCopiedAndPreflightDisabled(t *testing.T) {
	settings := DefaultBlobBucketLocationRetrySettings()
	settings.MaxAttempts = 1
	fixture := newBlobLocationRetryFixtureWithSettings(t, func(int) blobLocationTestResponse {
		return blobLocationTestResponse{status: 429, code: "TooManyRequests"}
	}, &settings)
	settings.MaxAttempts = 3
	fixture.store.bucketLocationRetry.wait = func(context.Context, time.Duration) error {
		t.Fatal("disabled preflight was enabled by the caller's later mutation")
		return nil
	}
	_, err := fixture.store.PutRetained(t.Context(), "synthetic/artifact.json", fixture.localPath, "application/json", fixture.retainUntil)
	if !blobBucketLocationThrottled(err) || fixture.store.bucketLocationRetry.settings.MaxAttempts != 1 ||
		fixture.locationGets.Load() != 1 || fixture.puts.Load() != 0 {
		t.Fatalf("per-client retry settings were not copied: locations=%d puts=%d error=%v", fixture.locationGets.Load(), fixture.puts.Load(), err)
	}
}

// A synthetic logical-time advance occurs while the first error body is held,
// after its headers were observed, so the window cannot restart after parsing.
func TestBlobStoreRetainedBucketLocationFirstBodyPastWindowDoesNotRetry(t *testing.T) {
	fixture := newBlobLocationRetryFixture(t, func(int) blobLocationTestResponse {
		return blobLocationTestResponse{status: 429, code: "TooManyRequests"}
	})
	guard, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	start := time.Now()
	var elapsed atomic.Int64
	var waits atomic.Int32
	headers := make(chan struct{})
	var headersOnce sync.Once
	fixture.store.bucketLocationRetry.now = func() time.Time {
		now := start.Add(time.Duration(elapsed.Load()))
		headersOnce.Do(func() { close(headers) })
		return now
	}
	fixture.store.bucketLocationRetry.wait = func(context.Context, time.Duration) error {
		waits.Add(1)
		return errors.New("expired synthetic first-response window was restarted")
	}
	releaseBody := make(chan struct{})
	var releaseOnce sync.Once
	t.Cleanup(func() { releaseOnce.Do(func() { close(releaseBody) }) })
	fixture.locationBodyBarrier = func(int) { <-releaseBody }
	done := make(chan error, 1)
	go func() {
		_, err := fixture.store.PutRetained(guard, "synthetic/artifact.json", fixture.localPath, "application/json", fixture.retainUntil)
		done <- err
	}()
	select {
	case <-headers:
	case <-guard.Done():
		t.Fatal("first response did not reach the explicit header/body boundary")
	}
	elapsed.Store(int64(fixture.store.bucketLocationRetry.settings.RetryWindow + time.Second))
	releaseOnce.Do(func() { close(releaseBody) })
	var err error
	select {
	case err = <-done:
	case <-guard.Done():
		t.Fatal("completed first error body did not return")
	}
	if !blobBucketLocationThrottled(err) || waits.Load() != 0 || fixture.locationGets.Load() != 1 || fixture.puts.Load() != 0 || guard.Err() != nil {
		t.Fatalf("first body shortened its parent or restarted recovery: locations=%d puts=%d error=%v", fixture.locationGets.Load(), fixture.puts.Load(), err)
	}
}

// A held successful first body can finish beyond the logical retry-only
// window while retaining the original healthy-operation budget and proof.
func TestBlobStoreRetainedBucketLocationHealthyFirstBodyKeepsOriginalLifetime(t *testing.T) {
	fixture := newBlobLocationRetryFixture(t, func(int) blobLocationTestResponse {
		return blobLocationTestResponse{status: 200}
	})
	guard, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	ready, releaseBody := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	t.Cleanup(func() { releaseOnce.Do(func() { close(releaseBody) }) })
	fixture.locationBodyBarrier = func(attempt int) {
		if attempt == 1 {
			close(ready)
			<-releaseBody
		}
	}
	start := time.Now()
	var elapsed atomic.Int64
	var clockReads atomic.Int32
	fixture.store.bucketLocationRetry.now = func() time.Time {
		clockReads.Add(1)
		return start.Add(time.Duration(elapsed.Load()))
	}
	fixture.store.bucketLocationRetry.wait = func(context.Context, time.Duration) error {
		return errors.New("healthy synthetic first lookup unexpectedly retried")
	}
	done := make(chan error, 1)
	go func() {
		proof, err := fixture.store.PutRetained(guard, "synthetic/artifact.json", fixture.localPath, "application/json", fixture.retainUntil)
		if err == nil && proof == nil {
			err = errors.New("synthetic healthy retention proof is missing")
		}
		done <- err
	}()
	select {
	case <-ready:
	case <-guard.Done():
		t.Fatal("healthy first body did not reach its explicit barrier")
	}
	elapsed.Store(int64(fixture.store.bucketLocationRetry.settings.RetryWindow + time.Minute))
	releaseOnce.Do(func() { close(releaseBody) })
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("healthy first body lost its budget: %v", err)
		}
	case <-guard.Done():
		t.Fatal("healthy first body did not complete after release")
	}
	if clockReads.Load() != 0 || fixture.locationGets.Load() != 1 || fixture.puts.Load() != 1 || fixture.retentionGets.Load() != 1 || guard.Err() != nil {
		t.Fatalf("healthy first body inherited recovery lifetime: clock_reads=%d locations=%d puts=%d", clockReads.Load(), fixture.locationGets.Load(), fixture.puts.Load())
	}
}

// Context-local labels are test-only identities, never transport headers.
type blobLocationTestProbeLabel struct{}

// Two cold probes on the same client keep separate headers and cancellation;
// canceling one waiter cannot consume or retire its healthy sibling's recovery.
func TestBlobStoreRetainedBucketLocationConcurrentProbesAreIsolated(t *testing.T) {
	fixture := newBlobLocationRetryFixture(t, func(attempt int) blobLocationTestResponse {
		if attempt <= 2 {
			return blobLocationTestResponse{status: 429, code: "TooManyRequests", retryAfter: strconv.Itoa(attempt)}
		}
		return blobLocationTestResponse{status: 200}
	})
	guard, stop := context.WithTimeout(t.Context(), time.Minute)
	defer stop()
	fixture.store.bucketLocationRetry.settings.RetryWindow = time.Minute
	now := time.Now()
	fixture.store.bucketLocationRetry.now = func() time.Time { return now }
	ctxA, cancelA := context.WithCancel(context.WithValue(guard, blobLocationTestProbeLabel{}, "a"))
	defer cancelA()
	ctxB := context.WithValue(guard, blobLocationTestProbeLabel{}, "b")
	waitingA, waitingB := make(chan time.Duration, 1), make(chan time.Duration, 1)
	releaseB := make(chan struct{})
	var releaseOnce sync.Once
	t.Cleanup(func() { releaseOnce.Do(func() { close(releaseB) }) })
	fixture.store.bucketLocationRetry.wait = func(ctx context.Context, delay time.Duration) error {
		switch ctx.Value(blobLocationTestProbeLabel{}) {
		case "a":
			waitingA <- delay
			<-ctx.Done()
			return ctx.Err()
		case "b":
			waitingB <- delay
			select {
			case <-releaseB:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		default:
			return errors.New("synthetic probe label is missing")
		}
	}
	start := func(ctx context.Context) <-chan error {
		done := make(chan error, 1)
		go func() {
			_, err := fixture.store.PutRetained(ctx, "synthetic/artifact.json", fixture.localPath, "application/json", fixture.retainUntil)
			done <- err
		}()
		return done
	}
	doneA := start(ctxA)
	select {
	case delay := <-waitingA:
		if delay != time.Second {
			t.Fatalf("first probe received another response's hint: %v", delay)
		}
	case <-guard.Done():
		t.Fatal("first probe did not reach its wait barrier")
	}
	doneB := start(ctxB)
	select {
	case delay := <-waitingB:
		if delay != 2*time.Second {
			t.Fatalf("second probe received another response's hint: %v", delay)
		}
	case <-guard.Done():
		t.Fatal("second probe did not reach its independent wait barrier")
	}
	cancelA()
	select {
	case err := <-doneA:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("first probe cancellation changed: %v", err)
		}
	case <-guard.Done():
		t.Fatal("canceled probe did not finish")
	}
	releaseOnce.Do(func() { close(releaseB) })
	select {
	case err := <-doneB:
		if err != nil {
			t.Fatalf("healthy sibling failed after independent cancellation: %v", err)
		}
	case <-guard.Done():
		t.Fatal("healthy sibling did not complete")
	}
	if fixture.locationGets.Load() != 3 || fixture.puts.Load() != 1 || fixture.retentionGets.Load() != 1 || ctxB.Err() != nil {
		t.Fatalf("concurrent probes shared retry work: locations=%d puts=%d", fixture.locationGets.Load(), fixture.puts.Load())
	}
}

// A delta-seconds hint starts at receipt, not after the original error body
// finishes. Counting elapsed parsing time avoids an unnecessarily early refusal.
func TestBlobStoreRetainedBucketLocationRetryAfterCountsElapsedBodyTime(t *testing.T) {
	fixture := newBlobLocationRetryFixture(t, func(attempt int) blobLocationTestResponse {
		if attempt == 1 {
			return blobLocationTestResponse{status: 429, code: "TooManyRequests", retryAfter: "3"}
		}
		return blobLocationTestResponse{status: 200}
	})
	start := time.Now()
	var clockReads atomic.Int32
	fixture.store.bucketLocationRetry.now = func() time.Time {
		if clockReads.Add(1) == 1 {
			return start
		}
		return start.Add(2 * time.Second)
	}
	var delays []time.Duration
	fixture.store.bucketLocationRetry.wait = func(_ context.Context, delay time.Duration) error {
		delays = append(delays, delay)
		return nil
	}
	proof, err := fixture.store.PutRetained(t.Context(), "synthetic/artifact.json", fixture.localPath, "application/json", fixture.retainUntil)
	if err != nil || proof == nil || !reflect.DeepEqual(delays, []time.Duration{time.Second}) || fixture.locationGets.Load() != 2 || fixture.puts.Load() != 1 {
		t.Fatalf("Retry-After was restarted after the body: delays=%v locations=%d error=%v", delays, fixture.locationGets.Load(), err)
	}
}

// A pure transport function permits exact first-request lifetime assertions.
type blobLocationTestRoundTripper func(*http.Request) (*http.Response, error)

// Executes the test's explicit response without a network or scheduler race.
func (self blobLocationTestRoundTripper) RoundTrip(request *http.Request) (*http.Response, error) {
	return self(request)
}

// Healthy initial discovery must not inherit a short retry-only timeout, even
// when the overall caller legitimately retains a much longer lifetime.
func TestBlobBucketLocationFirstHealthyRequestKeepsFullBudget(t *testing.T) {
	deadline := time.Now().Add(time.Hour)
	ctx, cancel := context.WithDeadline(t.Context(), deadline)
	defer cancel()
	calls := 0
	transport := &blobBucketLocationProbeTransport{base: blobLocationTestRoundTripper(func(request *http.Request) (*http.Response, error) {
		calls++
		actual, present := request.Context().Deadline()
		if !present || !actual.Equal(deadline) {
			t.Fatal("healthy first request lost its original caller budget")
		}
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`<LocationConstraint>us-east-1</LocationConstraint>`))}, nil
	})}
	client, err := minio.New("archive.example", &minio.Options{Transport: transport, Creds: credentials.NewStaticV4("synthetic-access", "synthetic-secret", "")})
	if err != nil {
		t.Fatal(err)
	}
	retry := &blobBucketLocationRetry{
		settings: DefaultBlobBucketLocationRetrySettings(),
		now:      func() time.Time { t.Fatal("healthy first request started a retry window"); return time.Time{} },
		wait: func(context.Context, time.Duration) error {
			t.Fatal("healthy first request waited for recovery")
			return nil
		},
	}
	if err := retry.prepare(ctx, client, "synthetic-evidence"); err != nil || calls != 1 || ctx.Err() != nil {
		t.Fatalf("healthy discovery changed: calls=%d error=%v", calls, err)
	}
}

// A counted body proves the observer adds no reads, truncation, close or
// cancellation ownership to either normal or error responses.
type blobLocationTestBody struct {
	io.Reader
	reads, closes int
}

// Counts every attempted body read, including EOF discovery.
func (self *blobLocationTestBody) Read(value []byte) (int, error) {
	self.reads++
	return self.Reader.Read(value)
}

// Closing has no side effects beyond its explicit observable count.
func (self *blobLocationTestBody) Close() error { self.closes++; return nil }

// The header observer never owns a response, so even an oversized final body
// remains byte-for-byte readable and is closed only by the SDK/caller owner.
func TestBlobBucketLocationProbePreservesResponseBodyOwnership(t *testing.T) {
	content := strings.Repeat("synthetic complete response bytes\n", 4096)
	for _, method := range []string{http.MethodGet, http.MethodPut} {
		body := &blobLocationTestBody{Reader: strings.NewReader(content)}
		ctx, cancel := context.WithCancel(t.Context())
		probe := &blobBucketLocationProbe{now: time.Now}
		request, err := http.NewRequestWithContext(context.WithValue(ctx, blobBucketLocationProbeKey{}, probe), method, "http://archive.example/synthetic-evidence/?location=", nil)
		if err != nil {
			cancel()
			t.Fatal(err)
		}
		calls := 0
		transport := &blobBucketLocationProbeTransport{base: blobLocationTestRoundTripper(func(*http.Request) (*http.Response, error) {
			calls++
			return &http.Response{StatusCode: 429, Header: http.Header{"Retry-After": {"60"}}, Body: body}, nil
		})}
		response, err := transport.RoundTrip(request)
		if err != nil || response.Body != body || calls != 1 || body.reads != 0 || body.closes != 0 || ctx.Err() != nil {
			cancel()
			t.Fatal("pass-through observer changed response lifetime or work")
		}
		value, readErr := io.ReadAll(response.Body)
		closeErr := response.Body.Close()
		if readErr != nil || closeErr != nil || string(value) != content || body.closes != 1 || ctx.Err() != nil {
			cancel()
			t.Fatal("observer truncated or prematurely closed the returned body")
		}
		if method == http.MethodGet && (probe.retryAfter != "60" || probe.receivedAt.IsZero()) ||
			method == http.MethodPut && (probe.retryAfter != "" || !probe.receivedAt.IsZero()) {
			cancel()
			t.Fatal("observer captured metadata outside read-only discovery")
		}
		cancel()
	}
}

// Numeric, date and overflow hints are protocol parsing, not wall-clock waits.
func TestBlobBucketLocationRetryAfterParsing(t *testing.T) {
	now := time.Date(2030, 1, 2, 3, 4, 5, 0, time.UTC)
	checks := []struct {
		value   string
		delay   time.Duration
		present bool
	}{
		{value: "2", delay: 2 * time.Second, present: true},
		{value: " 0 ", present: true},
		{value: now.Add(3 * time.Second).Format(http.TimeFormat), delay: 3 * time.Second, present: true},
		{value: now.Add(-time.Second).Format(http.TimeFormat), present: true},
		{value: "999999999999999999999999", delay: time.Duration(1<<63 - 1), present: true},
		{value: "synthetic-invalid-hint"},
		{},
	}
	for _, check := range checks {
		delay, present := blobBucketLocationRetryAfter(check.value, now)
		if delay != check.delay || present != check.present {
			t.Fatalf("Retry-After parse mismatch: delay=%v present=%t", delay, present)
		}
	}
}
