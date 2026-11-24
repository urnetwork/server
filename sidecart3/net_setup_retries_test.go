package sidecart3

// A T3 Net's setup on a store that resets connections under load, and its stores' rereads of admin info as its clock
// moves (QD-1395). The setup's calls, the MakeBucket of each bucket and the clusters' first admin read, are sent
// again after a transport failure, at most SetupRetryCount times, and a refusal is never sent again: held here on a
// fake store served by net/http, which resets the connections of the requests a test names before any answer, through
// minio-go's and server's own clients and the errors they return. The rereads, once the Net's clock has moved past
// AdminInfoRereadAfter since the last, are held on T2's Nets with a check that counts its reads.
// net_setup_retries_t3_test.go builds T3 Nets through resets of the local stack's MinIO and steps a T3 Net's clock.
// Nothing sleeps: resets are counted, never timed, and the tests set the waits between tries short.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"

	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/sidecartest"
	"github.com/urnetwork/sidecar/sidecartest/localstack"

	"github.com/urnetwork/server"
)

// The routes of a store's requests a test names.
const (
	// a GET of MinIO's admin info, which the clusters' checks and a Net's first admin read send
	storeRouteAdminInfo = "admin info"
	// a PUT of a bucket with no key: a MakeBucket
	storeRouteMakeBucket = "make bucket"
	// every other request
	storeRouteOther = "other"
)

// A request's route, path style, as minio-go and server's file clusters send to a store at an address.
func storeRoute(request *http.Request) string {
	switch {
	case request.URL.Path == "/minio/admin/v3/info":
		return storeRouteAdminInfo
	case request.Method == http.MethodPut && !strings.Contains(strings.Trim(request.URL.Path, "/"), "/"):
		return storeRouteMakeBucket
	}
	return storeRouteOther
}

// A store on loopback above the local stack's port floor, an address a Net's setup takes for the stack's, which
// resets the connection of each request whose route has resets left, as the local stack's MinIO does under load
// behind Docker's port forward: an RST before any answer, so its client reads a reset. Every other request goes to
// next. Keep-alives are off, so each request has a connection of its own and no client's transport sends one again by
// itself, as it may on a connection it reused. Safe for concurrent use.
type resettingStore struct {
	address string
	next    http.Handler

	stateLock sync.Mutex
	// by route
	routeRemainingResetCounts map[string]int
	routeResetCounts          map[string]int
	routeRequestCounts        map[string]int
}

// A store serving next, closed with t.
func newResettingStore(t *testing.T, next http.Handler) *resettingStore {
	t.Helper()
	self := &resettingStore{
		next:                      next,
		routeRemainingResetCounts: map[string]int{},
		routeResetCounts:          map[string]int{},
		routeRequestCounts:        map[string]int{},
	}
	httpServer := httptest.NewUnstartedServer(self)
	httpServer.Listener.Close()
	httpServer.Listener = listenAboveStackFloor(t)
	httpServer.Config.SetKeepAlivesEnabled(false)
	httpServer.Start()
	t.Cleanup(httpServer.Close)
	self.address = httpServer.Listener.Addr().String()
	return self
}

// A loopback listener at a port the local stack's address check takes. The system's ephemeral ports mostly are above
// its floor, and a port below is held open while the next is tried, so each try gets another.
func listenAboveStackFloor(t *testing.T) net.Listener {
	t.Helper()
	heldListeners := []net.Listener{}
	defer func() {
		for _, listener := range heldListeners {
			listener.Close()
		}
	}()
	for range 64 {
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		if localstack.CheckStackAddress(listener.Addr().String()) == nil {
			return listener
		}
		heldListeners = append(heldListeners, listener)
	}
	t.Fatal("no loopback port above the local stack's floor")
	return nil
}

func (self *resettingStore) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	route := storeRoute(request)
	reset := func() bool {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.routeRequestCounts[route] += 1
		if self.routeRemainingResetCounts[route] <= 0 {
			return false
		}
		self.routeRemainingResetCounts[route] -= 1
		self.routeResetCounts[route] += 1
		return true
	}()
	if !reset {
		self.next.ServeHTTP(writer, request)
		return
	}
	connection, _, err := writer.(http.Hijacker).Hijack()
	if err != nil {
		// the server closes the connection, which the client reads as one lost too
		panic(http.ErrAbortHandler)
	}
	if tcpConnection, ok := connection.(*net.TCPConn); ok {
		// an RST, not a FIN
		tcpConnection.SetLinger(0)
	}
	connection.Close()
}

// Resets the next count requests of route.
func (self *resettingStore) reset(route string, count int) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.routeRemainingResetCounts[route] += count
}

// How many requests of route came, and how many of them were reset.
func (self *resettingStore) counts(route string) (requestCount int, resetCount int) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.routeRequestCounts[route], self.routeResetCounts[route]
}

// MinIO's answer to a user its policy does not allow.
const accessDeniedXml = `<?xml version="1.0" encoding="UTF-8"?>
<Error><Code>AccessDenied</Code><Message>Access Denied.</Message><Resource>/</Resource><RequestId>0</RequestId><HostId>0</HostId></Error>`

// Admin info as MinIO answers it for one pool on one host, in the fields server's file clusters read.
const fakeAdminInfoJson = `{"mode":"online","backend":{"backendType":"Erasure","totalSets":[1],"totalDrivesPerSet":[1]},"servers":[{"state":"online","endpoint":"store.example:9000","poolNumbers":[1]}]}`

// What a fake store answers, as the local stack's MinIO would: admin info with one pool, a bucket made, and no object
// or bucket to a read, as of a sentinel no one wrote or a failed setup's cleanup; and the route refusedRoute names, if
// any, 403 AccessDenied.
func fakeStoreAnswers(refusedRoute string) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		route := storeRoute(request)
		switch {
		case route == refusedRoute:
			writer.Header().Set("Content-Type", "application/xml")
			writer.WriteHeader(http.StatusForbidden)
			io.WriteString(writer, accessDeniedXml)
		case route == storeRouteAdminInfo:
			writer.Header().Set("Content-Type", "application/json")
			io.WriteString(writer, fakeAdminInfoJson)
		case route == storeRouteMakeBucket:
			writer.WriteHeader(http.StatusOK)
		default:
			writer.WriteHeader(http.StatusNotFound)
		}
	})
}

// One metadata cluster on store, as a Net's setup reads it, and the buckets naming it. The clusters run no check loop,
// so the setup's reads are all the store counts.
func newSetupClusters(t *testing.T, store *resettingStore) (*server.FileClusters, *buckets) {
	t.Helper()
	settings := DefaultSettings()
	setupBuckets := &buckets{clusters: []*bucketCluster{{
		clusterId: metadataClusterId,
		kind:      server.FileClusterKindMetadata,
		first:     true,
		endpoints: []string{store.address},
		accessKey: settings.MinioAccessKey,
		secretKey: settings.MinioSecretKey,
		region:    settings.MinioRegion,
		bucket:    "sidecar-t3-setup-meta",
	}}}
	clusterSettings := server.DefaultFileClusterSettings()
	clusterSettings.NoChecks = true
	clusters, err := server.NewFileClusters(t.Context(), setupBuckets.configs(), clusterSettings)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(clusters.Close)
	return clusters, setupBuckets
}

// The clusters' first admin read rides out resets, sent again after each try they fail until answered, at most
// SetupRetryCount times. A try the store resets reads twice: the check's read, and the read after it that tells a
// reset from a refusal. So two resets fail one try, and the third read is kept; one reset leaves the read after it
// answered, which sends the check again (errAdminInfoUnkept); and with fewer retries than the resets take, the setup
// fails naming the cluster, as every T3 Net's did on the first reset before QD-1395.
func TestTheFirstAdminReadIsSentAgainAfterResets(t *testing.T) {
	type firstAdminReadCase struct {
		resetCount int
		retryCount int
		// whether the cluster ends with admin info kept
		kept         bool
		requestCount int
	}
	cases := []*firstAdminReadCase{
		{resetCount: 2, retryCount: 5, kept: true, requestCount: 3},
		{resetCount: 1, retryCount: 5, kept: true, requestCount: 3},
		{resetCount: 4, retryCount: 2, kept: true, requestCount: 5},
		{resetCount: 2, retryCount: 0, kept: false, requestCount: 2},
		{resetCount: 1, retryCount: 0, kept: false, requestCount: 2},
		{resetCount: 4, retryCount: 1, kept: false, requestCount: 4},
	}
	for _, c := range cases {
		store := newResettingStore(t, fakeStoreAnswers(""))
		store.reset(storeRouteAdminInfo, c.resetCount)
		clusters, setupBuckets := newSetupClusters(t, store)
		settings := DefaultSettings()
		settings.SetupRetryCount = c.retryCount
		settings.SetupRetryWait = time.Millisecond
		err := firstAdminRead(t.Context(), clusters, setupBuckets, settings)
		cluster, clusterErr := clusters.Cluster(metadataClusterId)
		if clusterErr != nil {
			t.Fatal(clusterErr)
		}
		kept := cluster.(*server.FileCluster).LastAdminInfo() != nil
		requestCount, resetCount := store.counts(storeRouteAdminInfo)
		if c.kept && err != nil {
			t.Errorf("%d resets and %d retries: the first admin read failed: %v", c.resetCount, c.retryCount, err)
		}
		if !c.kept && (err == nil || !strings.Contains(err.Error(), "gave no admin info for "+metadataClusterId)) {
			t.Errorf("%d resets and %d retries: the first admin read ended with %v", c.resetCount, c.retryCount, err)
		}
		if kept != c.kept || requestCount != c.requestCount || resetCount != c.resetCount {
			t.Errorf("%d resets and %d retries: admin info kept %t after %d reads, %d reset, not %t after %d", c.resetCount, c.retryCount, kept, requestCount, resetCount, c.kept, c.requestCount)
		}
	}
}

// An admin read the store refuses fails the setup at once, after the check's read and the read after it, which tells
// a refusal from a reset, however many retries it has: a refusal, unlike a reset, is the same each time.
func TestARefusedAdminReadFailsTheSetupAtOnce(t *testing.T) {
	store := newResettingStore(t, fakeStoreAnswers(storeRouteAdminInfo))
	clusters, setupBuckets := newSetupClusters(t, store)
	settings := DefaultSettings()
	settings.SetupRetryWait = 0
	err := firstAdminRead(t.Context(), clusters, setupBuckets, settings)
	if err == nil || !strings.Contains(err.Error(), "gave no admin info for "+metadataClusterId) || !strings.Contains(err.Error(), "answered 403") {
		t.Errorf("a refused first admin read ended with %v", err)
	}
	if requestCount, _ := store.counts(storeRouteAdminInfo); requestCount != 2 {
		t.Errorf("a refused first admin read was sent %d times, not twice", requestCount)
	}
}

// A MakeBucket the store refuses fails the setup at once, sent once however many retries it has, as minio-go sends an
// AccessDenied once too.
func TestARefusedMakeBucketFailsTheSetupAtOnce(t *testing.T) {
	store := newResettingStore(t, fakeStoreAnswers(storeRouteMakeBucket))
	settings := DefaultSettings()
	settings.MinioEndpoint = store.address
	settings.SetupRetryWait = 0
	_, err := newBuckets(t.Context(), settings)
	var errorResponse minio.ErrorResponse
	if !errors.As(err, &errorResponse) || errorResponse.Code != "AccessDenied" || !strings.Contains(err.Error(), "made no bucket") {
		t.Errorf("a refused MakeBucket ended with %v", err)
	}
	if requestCount, _ := store.counts(storeRouteMakeBucket); requestCount != 1 {
		t.Errorf("a refused MakeBucket was sent %d times, not once", requestCount)
	}
}

// What minio-go returns for a MakeBucket whose connection the store reset, its own retries spent, is a transport
// failure, which retrySetup sends again until answered and at most SetupRetryCount times, while its AccessDenied is
// sent once, and nothing is sent again once ctx has ended. The errors are minio-go's own, from a client sending each
// request once to stores that reset or refuse it: a Net's bucket client sends one up to minio-go's own 10 times
// before retrySetup sees its error, which would make this wait out minio-go's backoff.
func TestRetrySetupSendsAResetMakeBucketAgainAndARefusedOneOnce(t *testing.T) {
	makeBucketError := func(store *resettingStore) error {
		t.Helper()
		settings := DefaultSettings()
		client, err := minio.New(store.address, &minio.Options{
			Creds:      credentials.NewStaticV4(settings.MinioAccessKey, settings.MinioSecretKey, ""),
			Region:     settings.MinioRegion,
			MaxRetries: 1,
		})
		if err != nil {
			t.Fatal(err)
		}
		err = client.MakeBucket(t.Context(), "sidecar-t3-setup-meta", minio.MakeBucketOptions{Region: settings.MinioRegion})
		if err == nil {
			t.Fatal("the store answered a MakeBucket it was to reset or refuse")
		}
		return err
	}
	resetStore := newResettingStore(t, fakeStoreAnswers(""))
	resetStore.reset(storeRouteMakeBucket, 1)
	resetErr := makeBucketError(resetStore)
	refusedErr := makeBucketError(newResettingStore(t, fakeStoreAnswers(storeRouteMakeBucket)))
	settings := DefaultSettings()
	settings.SetupRetryWait = 0
	callCount := 0
	// reset twice, then answered
	err := retrySetup(t.Context(), settings, func() error {
		callCount += 1
		if callCount <= 2 {
			return resetErr
		}
		return nil
	})
	if err != nil || callCount != 3 {
		t.Errorf("a MakeBucket reset twice (%v) and then answered ended with %v after %d calls, not 3", resetErr, err, callCount)
	}
	// reset every time: the first call and SetupRetryCount more
	settings.SetupRetryCount = 2
	callCount = 0
	err = retrySetup(t.Context(), settings, func() error {
		callCount += 1
		return resetErr
	})
	if err == nil || callCount != 3 {
		t.Errorf("a MakeBucket reset every time with 2 retries ended with %v after %d calls, not 3", err, callCount)
	}
	callCount = 0
	err = retrySetup(t.Context(), settings, func() error {
		callCount += 1
		return refusedErr
	})
	var errorResponse minio.ErrorResponse
	if !errors.As(err, &errorResponse) || errorResponse.Code != "AccessDenied" || callCount != 1 {
		t.Errorf("a refused MakeBucket ended with %v after %d calls, not 1", err, callCount)
	}
	endedCtx, cancel := context.WithCancel(t.Context())
	cancel()
	callCount = 0
	err = retrySetup(endedCtx, settings, func() error {
		callCount += 1
		return resetErr
	})
	if err == nil || callCount != 1 {
		t.Errorf("a MakeBucket reset after its ctx ended ended with %v after %d calls, not 1", err, callCount)
	}
}

// T2's fake blobs behind faultBlobs, which rereads admin info through a check counting its reads once the Net's clock
// has moved past rereadAfter since the last reread.
type rereadFixture struct {
	net        *sidecartest.Net
	admin      *fakeAdminInfo
	cluster    sidecar.BlobCluster
	writeCount int
}

func newRereadFixture(t *testing.T, rereadAfter time.Duration) *rereadFixture {
	t.Helper()
	net := sidecartest.NewNet(t.Context(), nil)
	t.Cleanup(net.Close)
	store := sidecartest.NewFakeBlobs(net.Clock(), sidecartest.NewFaultQueue(), nil, nil)
	admin := &fakeAdminInfo{clock: net.Clock()}
	blobs := newFaultBlobs(store, admin.check, net, nil, rereadAfter, nil)
	cluster, err := blobs.FirstCluster()
	if err != nil {
		t.Fatal(err)
	}
	return &rereadFixture{net: net, admin: admin, cluster: cluster}
}

// A step of the Net's clock (none for zero), then a write, answered.
func (self *rereadFixture) stepAndWrite(t *testing.T, step time.Duration) {
	t.Helper()
	if 0 < step {
		self.net.Clock().Advance(step)
	}
	self.writeCount += 1
	key := fmt.Sprintf("reread/%d", self.writeCount)
	if _, err := self.cluster.PutIfNoneMatch(self.net.Context(), key, []byte(key), nil); err != nil {
		t.Fatalf("%s read %v", key, err)
	}
}

func (self *rereadFixture) expectReads(t *testing.T, readCount int, after string) {
	t.Helper()
	if got := self.admin.reads(); got != readCount {
		t.Errorf("after %s, admin info was read %d times, not %d", after, got, readCount)
	}
}

// Small steps of the Net's clock, as a clock following real time takes (evshost's TestFs5OnT3 steps it every 10 ms),
// reread admin info about once each AdminInfoRereadAfter, 30 s by default, and no longer before every request after a
// step, as before QD-1395: the first request reads it whatever the clock says, steps summing to the threshold read
// nothing, and the first request past it reads it once. Steps of 100 ms with a write after each, for 95 s, read it
// four times: at the start and 30.1, 60.2 and 90.3 s in.
func TestSmallStepsOfTheClockRereadAdminInfoOncePerThreshold(t *testing.T) {
	threshold := DefaultSettings().AdminInfoRereadAfter
	fixture := newRereadFixture(t, threshold)
	fixture.stepAndWrite(t, 0)
	fixture.expectReads(t, 1, "the first request")
	for range 3 {
		fixture.stepAndWrite(t, threshold/3)
	}
	fixture.expectReads(t, 1, "steps summing to the threshold")
	fixture.stepAndWrite(t, time.Millisecond)
	fixture.expectReads(t, 2, "a step past it")
	for range 3 {
		fixture.stepAndWrite(t, threshold/3)
	}
	fixture.expectReads(t, 2, "steps summing to the threshold again")
	fixture.stepAndWrite(t, time.Millisecond)
	fixture.expectReads(t, 3, "a step past it again")

	follower := newRereadFixture(t, threshold)
	follower.stepAndWrite(t, 0)
	for range 950 {
		follower.stepAndWrite(t, 100*time.Millisecond)
	}
	follower.expectReads(t, 4, "95 s of steps 100 ms apart")
}

// A schedule's jump of the Net's clock past AdminInfoRereadAfter rereads admin info before its next request, a read
// as well as a write, as every move did before QD-1395, and a step within the threshold after it reads nothing. A
// threshold of zero rereads after every move, and after no request without one.
func TestAJumpOfTheClockRereadsAdminInfoBeforeItsNextRequest(t *testing.T) {
	settings := DefaultSettings()
	fixture := newRereadFixture(t, settings.AdminInfoRereadAfter)
	fixture.stepAndWrite(t, 0)
	fixture.net.Clock().Advance(48 * time.Hour)
	if _, err := fixture.cluster.Head(fixture.net.Context(), "reread/1"); err != nil {
		t.Fatal(err)
	}
	fixture.expectReads(t, 2, "a jump of two days and a read")
	fixture.stepAndWrite(t, time.Second)
	fixture.expectReads(t, 2, "a step of a second after it")
	fixture.stepAndWrite(t, settings.FileClusters.CheckStaleAfter+time.Second)
	fixture.expectReads(t, 3, "a jump past CheckStaleAfter")

	everyMove := newRereadFixture(t, 0)
	everyMove.stepAndWrite(t, 0)
	everyMove.stepAndWrite(t, time.Millisecond)
	everyMove.stepAndWrite(t, 0)
	everyMove.stepAndWrite(t, time.Millisecond)
	everyMove.expectReads(t, 3, "two moves of a millisecond with no threshold")
}

// QD-1395's defaults: five retries of a setup call a second apart, and admin info reread once the Net's clock has
// moved past the file clusters' CheckInterval, below their CheckStaleAfter. A Net whose threshold reaches
// CheckStaleAfter is refused before it sends a store anything, as a request after a move just short of it would find
// admin info stale.
func TestTheSetupRetryAndRereadDefaults(t *testing.T) {
	settings := DefaultSettings()
	if settings.SetupRetryCount != 5 || settings.SetupRetryWait != time.Second {
		t.Errorf("the setup retries %d times %s apart", settings.SetupRetryCount, settings.SetupRetryWait)
	}
	fileClusters := server.DefaultFileClusterSettings()
	if settings.AdminInfoRereadAfter != fileClusters.CheckInterval || fileClusters.CheckStaleAfter <= settings.AdminInfoRereadAfter {
		t.Errorf("admin info is reread after %s, the clusters checking every %s and stale after %s", settings.AdminInfoRereadAfter, fileClusters.CheckInterval, fileClusters.CheckStaleAfter)
	}
	// a store that refuses a MakeBucket, should the Net get that far
	store := newResettingStore(t, fakeStoreAnswers(storeRouteMakeBucket))
	settings.MinioEndpoint = store.address
	settings.AdminInfoRereadAfter = settings.FileClusters.CheckStaleAfter
	net := sidecartest.NewNet(t.Context(), nil)
	defer net.Close()
	_, _, err := newHost(t.Context(), t, net, settings, func() {})
	if err == nil || !strings.Contains(err.Error(), "AdminInfoRereadAfter") {
		t.Errorf("a Net rereading after CheckStaleAfter ended with %v", err)
	}
	for _, route := range []string{storeRouteAdminInfo, storeRouteMakeBucket, storeRouteOther} {
		if requestCount, _ := store.counts(route); requestCount != 0 {
			t.Errorf("a Net refused its settings sent the store %d %s requests", requestCount, route)
		}
	}
}
