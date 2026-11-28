// Synthetic HTTP peers exercise startup protection through the real SDK
// without live storage, mutation requests, sleeps or production identity.
package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/minio/minio-go/v7"
)

// Response settings are copied before the synthetic peer accepts requests.
type blobReadinessTestConfiguration struct {
	region                 string
	objectLock             string
	versioning             string
	replicationStatus      string
	replicationTarget      string
	replicationCheckStatus int
}

// Counts actual SDK requests and rejects every mutation at the peer boundary.
type blobReadinessRetryFixture struct {
	store        *minioBlobStore
	stateLock    sync.Mutex
	requests     []string
	mutations    int
	locationGets int
}

// Constructs the production client so a disconnected helper cannot satisfy
// the readiness regression while startup still bypasses location recovery.
func newBlobReadinessRetryFixture(t *testing.T, location func(int) int, config *blobReadinessTestConfiguration) *blobReadinessRetryFixture {
	t.Helper()
	settings := blobReadinessTestConfiguration{
		objectLock: "Enabled", versioning: "Enabled", replicationStatus: "Enabled",
		replicationTarget: "arn:aws:s3:::synthetic-backup", replicationCheckStatus: http.StatusOK,
	}
	if config != nil {
		settings = *config
	}
	fixture := &blobReadinessRetryFixture{}
	record := func(value string) {
		fixture.stateLock.Lock()
		defer fixture.stateLock.Unlock()
		fixture.requests = append(fixture.requests, value)
	}
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		w.Header().Set("Content-Type", "application/xml")
		if request.Method != http.MethodGet && request.Method != http.MethodHead {
			fixture.stateLock.Lock()
			fixture.mutations++
			fixture.stateLock.Unlock()
			record("mutation:" + request.Method)
			http.Error(w, "unexpected synthetic mutation", http.StatusBadRequest)
			return
		}
		if request.URL.Path != "/synthetic-readiness" && request.URL.Path != "/synthetic-readiness/" {
			record("unexpected-path")
			http.Error(w, "unexpected synthetic path", http.StatusBadRequest)
			return
		}
		query := request.URL.Query()
		switch {
		case request.Method == http.MethodGet && len(query) == 1 && query.Has("location"):
			fixture.stateLock.Lock()
			fixture.locationGets++
			attempt := fixture.locationGets
			fixture.stateLock.Unlock()
			status := location(attempt)
			record("location:" + strconv.Itoa(status))
			w.WriteHeader(status)
			if status == http.StatusOK {
				fmt.Fprint(w, `<LocationConstraint>us-east-1</LocationConstraint>`)
			} else {
				fmt.Fprint(w, `<Error><Code>TooManyRequests</Code><Message>synthetic throttle</Message></Error>`)
			}
		case request.Method == http.MethodHead && len(query) == 0:
			record("bucket:head")
			w.WriteHeader(http.StatusOK)
		case request.Method == http.MethodGet && len(query) == 1 && query.Has("object-lock"):
			record("object-lock")
			fmt.Fprintf(w, `<ObjectLockConfiguration><ObjectLockEnabled>%s</ObjectLockEnabled></ObjectLockConfiguration>`, settings.objectLock)
		case request.Method == http.MethodGet && len(query) == 1 && query.Has("versioning"):
			record("versioning")
			fmt.Fprintf(w, `<VersioningConfiguration><Status>%s</Status></VersioningConfiguration>`, settings.versioning)
		case request.Method == http.MethodGet && len(query) == 1 && query.Has("replication"):
			record("replication")
			fmt.Fprintf(w, `<ReplicationConfiguration><Rule><ID>synthetic-rule</ID><Status>%s</Status><Priority>1</Priority><Destination><Bucket>%s</Bucket></Destination></Rule></ReplicationConfiguration>`, settings.replicationStatus, settings.replicationTarget)
		case request.Method == http.MethodGet && len(query) == 1 && query.Has("replication-check"):
			record("replication-check")
			w.WriteHeader(settings.replicationCheckStatus)
			if settings.replicationCheckStatus != http.StatusOK {
				fmt.Fprint(w, `<Error><Code>AccessDenied</Code><Message>synthetic protection failure</Message></Error>`)
			}
		default:
			record("unexpected:" + request.Method + ":" + request.URL.RawQuery)
			http.Error(w, "unexpected synthetic protection request", http.StatusBadRequest)
		}
	}))
	t.Cleanup(peer.Close)
	store, err := NewBlobStore(&BlobStoreConfig{
		Authority: strings.TrimPrefix(peer.URL, "http://"), Bucket: "synthetic-readiness", Prefix: "synthetic",
		AccessKey: "synthetic-access", SecretKey: "synthetic-secret", Region: settings.region,
	})
	if err != nil {
		t.Fatal(err)
	}
	fixture.store = store.(*minioBlobStore)
	if fixture.store.bucketLocationRetry == nil {
		t.Fatal("production constructor omitted the shared location retry owner")
	}
	return fixture
}

// Request snapshots are synchronized with the synthetic HTTP handlers.
func (self *blobReadinessRetryFixture) snapshot() ([]string, int) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return append([]string(nil), self.requests...), self.mutations
}

// One cold throttle warms the exact startup client, then all protection reads
// still run. Default proof caching preserves a live HEAD on the next check.
func TestBlobStoreRetentionReadinessBucketLocationThrottleRecovers(t *testing.T) {
	fixture := newBlobReadinessRetryFixture(t, func(attempt int) int {
		if attempt == 1 {
			return http.StatusTooManyRequests
		}
		return http.StatusOK
	}, nil)
	var delays []time.Duration
	fixture.store.bucketLocationRetry.wait = func(ctx context.Context, delay time.Duration) error {
		delays = append(delays, delay)
		return ctx.Err()
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	if err := fixture.store.CheckRetention(ctx); err != nil {
		t.Fatal(err)
	}
	wanted := []string{"location:429", "location:200", "bucket:head", "object-lock", "versioning", "replication", "replication-check"}
	requests, mutations := fixture.snapshot()
	if !reflect.DeepEqual(requests, wanted) || mutations != 0 || !reflect.DeepEqual(delays, []time.Duration{250 * time.Millisecond}) {
		t.Fatalf("startup requests=%v mutations=%d delays=%v", requests, mutations, delays)
	}
	proof, err := fixture.store.CheckProtection(ctx)
	if err != nil || proof == nil || proof.Authority != fixture.store.authority || proof.Bucket != "synthetic-readiness" ||
		proof.ObjectLock != "COMPLIANCE" || !proof.Versioning || !reflect.DeepEqual(proof.ReplicationTargets, []string{"arn:aws:s3:::synthetic-backup"}) || ctx.Err() != nil {
		t.Fatalf("recovered startup protection=%+v error=%v", proof, err)
	}
	wanted = append(wanted, "bucket:head")
	requests, mutations = fixture.snapshot()
	if !reflect.DeepEqual(requests, wanted) || mutations != 0 {
		t.Fatalf("cached startup requests=%v mutations=%d", requests, mutations)
	}
}

// A configured region bypasses discovery even when a location lookup would
// throttle. The default proof cache still requires a live HEAD on every check.
func TestBlobStoreRetentionReadinessPinnedRegionAvoidsDiscovery(t *testing.T) {
	fixture := newBlobReadinessRetryFixture(t, func(int) int { return http.StatusTooManyRequests }, &blobReadinessTestConfiguration{
		region: DefaultBlobRegion, objectLock: "Enabled", versioning: "Enabled", replicationStatus: "Enabled",
		replicationTarget: "arn:aws:s3:::synthetic-backup", replicationCheckStatus: http.StatusOK,
	})
	fixture.store.bucketLocationRetry.wait = func(context.Context, time.Duration) error {
		t.Error("pinned region entered location retry backoff")
		return errors.New("unexpected synthetic retry")
	}
	if err := fixture.store.CheckRetention(t.Context()); err != nil {
		t.Fatal(err)
	}
	proof, err := fixture.store.CheckProtection(t.Context())
	if err != nil || proof == nil || proof.ObjectLock != "COMPLIANCE" || !proof.Versioning ||
		!reflect.DeepEqual(proof.ReplicationTargets, []string{"arn:aws:s3:::synthetic-backup"}) {
		t.Fatalf("pinned-region protection=%+v error=%v", proof, err)
	}
	requests, mutations := fixture.snapshot()
	wanted := []string{"bucket:head", "object-lock", "versioning", "replication", "replication-check", "bucket:head"}
	if !reflect.DeepEqual(requests, wanted) || mutations != 0 {
		t.Fatalf("pinned-region requests=%v mutations=%d", requests, mutations)
	}
}

// The sibling owned by a writer's retry policy must not inherit primary-store
// discovery recovery: even an unpinned cold location is attempted only once.
func TestBlobStoreRetentionReadinessSingleAttemptKeepsOneLocationAttempt(t *testing.T) {
	fixture := newBlobReadinessRetryFixture(t, func(int) int { return http.StatusTooManyRequests }, nil)
	single := SingleAttemptBlobStore(fixture.store).(*minioBlobStore)
	if single == fixture.store || single.bucketLocationRetry != nil || single.protection != fixture.store.protection {
		t.Fatal("single-attempt sibling changed retry ownership or protection-cache identity")
	}
	err := single.CheckRetention(t.Context())
	var response minio.ErrorResponse
	if err == nil || !errors.As(err, &response) || response.StatusCode != http.StatusTooManyRequests || response.Code != "TooManyRequests" {
		t.Fatalf("single-attempt startup lost its fail-closed cause: %v", err)
	}
	requests, mutations := fixture.snapshot()
	if !reflect.DeepEqual(requests, []string{"location:429"}) || mutations != 0 {
		t.Fatalf("single-attempt requests=%v mutations=%d", requests, mutations)
	}
}

// Persistent cold throttles stop at the shared attempt bound without HEAD,
// protection queries, writes or a false-ready result, retaining the S3 cause.
func TestBlobStoreRetentionReadinessBucketLocationThrottleFailsClosed(t *testing.T) {
	fixture := newBlobReadinessRetryFixture(t, func(int) int { return http.StatusTooManyRequests }, nil)
	var delays []time.Duration
	fixture.store.bucketLocationRetry.wait = func(ctx context.Context, delay time.Duration) error {
		delays = append(delays, delay)
		return ctx.Err()
	}
	err := fixture.store.CheckRetention(t.Context())
	var response minio.ErrorResponse
	if err == nil || !errors.As(err, &response) || response.StatusCode != http.StatusTooManyRequests || response.Code != "TooManyRequests" ||
		!strings.Contains(err.Error(), "check minio blob bucket existence:") {
		t.Fatalf("persistent startup throttle lost its fail-closed cause: %v", err)
	}
	requests, mutations := fixture.snapshot()
	if !reflect.DeepEqual(requests, []string{"location:429", "location:429", "location:429"}) || mutations != 0 ||
		!reflect.DeepEqual(delays, []time.Duration{250 * time.Millisecond, 500 * time.Millisecond}) {
		t.Fatalf("failed startup requests=%v mutations=%d delays=%v", requests, mutations, delays)
	}
}

// Cancellation during the existing owner's wait prevents further discovery
// and all protection work, preserving the caller's cancellation identity.
func TestBlobStoreRetentionReadinessBucketLocationCancellation(t *testing.T) {
	fixture := newBlobReadinessRetryFixture(t, func(int) int { return http.StatusTooManyRequests }, nil)
	waiting := make(chan struct{})
	fixture.store.bucketLocationRetry.wait = func(ctx context.Context, delay time.Duration) error {
		close(waiting)
		<-ctx.Done()
		return ctx.Err()
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	outcome := make(chan error, 1)
	go func() { outcome <- fixture.store.CheckRetention(ctx) }()
	select {
	case <-waiting:
	case err := <-outcome:
		t.Fatalf("startup returned before bounded backoff: %v", err)
	case <-t.Context().Done():
		t.Fatal("startup did not reach cancellable backoff")
	}
	cancel()
	if err := <-outcome; !errors.Is(err, context.Canceled) {
		t.Fatalf("startup cancellation identity changed: %v", err)
	}
	requests, mutations := fixture.snapshot()
	if !reflect.DeepEqual(requests, []string{"location:429"}) || mutations != 0 {
		t.Fatalf("canceled startup requests=%v mutations=%d", requests, mutations)
	}
}

// Recovery is not a protection waiver: absent object lock/versioning or an
// invalid replication configuration still fails at its original check.
func TestBlobStoreRetentionReadinessBucketLocationPreservesProtectionGates(t *testing.T) {
	cases := []struct {
		name                   string
		objectLock             string
		versioning             string
		replicationStatus      string
		replicationTarget      string
		replicationCheckStatus int
		message                string
		checks                 []string
	}{
		{name: "object lock", objectLock: "Disabled", versioning: "Enabled", replicationStatus: "Enabled", replicationTarget: "arn:aws:s3:::synthetic-backup", replicationCheckStatus: 200,
			message: "does not have object lock enabled", checks: []string{"bucket:head", "object-lock"}},
		{name: "versioning", objectLock: "Enabled", versioning: "Suspended", replicationStatus: "Enabled", replicationTarget: "arn:aws:s3:::synthetic-backup", replicationCheckStatus: 200,
			message: "does not have versioning enabled", checks: []string{"bucket:head", "object-lock", "versioning"}},
		{name: "enabled replication", objectLock: "Enabled", versioning: "Enabled", replicationStatus: "Disabled", replicationTarget: "arn:aws:s3:::synthetic-backup", replicationCheckStatus: 200,
			message: "does not have an enabled replication destination", checks: []string{"bucket:head", "object-lock", "versioning", "replication"}},
		{name: "replication destination", objectLock: "Enabled", versioning: "Enabled", replicationStatus: "Enabled", replicationTarget: "", replicationCheckStatus: 200,
			message: "without a destination bucket", checks: []string{"bucket:head", "object-lock", "versioning", "replication"}},
		{name: "replication validation", objectLock: "Enabled", versioning: "Enabled", replicationStatus: "Enabled", replicationTarget: "arn:aws:s3:::synthetic-backup", replicationCheckStatus: 403,
			message: "minio replication validation:", checks: []string{"bucket:head", "object-lock", "versioning", "replication", "replication-check"}},
	}
	for _, testCase := range cases {
		fixture := newBlobReadinessRetryFixture(t, func(attempt int) int {
			if attempt == 1 {
				return http.StatusTooManyRequests
			}
			return http.StatusOK
		}, &blobReadinessTestConfiguration{
			objectLock: testCase.objectLock, versioning: testCase.versioning,
			replicationStatus: testCase.replicationStatus, replicationTarget: testCase.replicationTarget,
			replicationCheckStatus: testCase.replicationCheckStatus,
		})
		fixture.store.bucketLocationRetry.wait = func(ctx context.Context, delay time.Duration) error { return ctx.Err() }
		proof, err := fixture.store.CheckProtection(t.Context())
		if err == nil || proof != nil || !strings.Contains(err.Error(), testCase.message) {
			t.Fatalf("%s gate bypassed: proof=%+v error=%v", testCase.name, proof, err)
		}
		requests, mutations := fixture.snapshot()
		wanted := append([]string{"location:429", "location:200"}, testCase.checks...)
		if !reflect.DeepEqual(requests, wanted) || mutations != 0 {
			t.Fatalf("%s requests=%v mutations=%d", testCase.name, requests, mutations)
		}
	}
}
