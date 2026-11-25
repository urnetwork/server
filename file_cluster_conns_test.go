package server

// §11.15's fan-out (owner decision 780; QD-1394): a host's base transport caps the connections it opens there at
// FileClusterSettings.MaxConnsPerHost and keeps as many idle; one serves each host whichever clusters name it; and
// requests to a held host through both clusters of one FileClusters never open more than the cap to it, the
// requests past the cap waiting for a connection and all of them answered. A GET or PutStream at a client's own pace
// (QD-1402) goes through a second client of the host, whose base transport is the first's without the cap, and
// every other request through the capped one.
//
// The capped client holds the host's slots, one for each request in flight, and the wait for one ends at the
// request's deadline: a request whose deadline passes in it, as it is given its slot, or as its connection is handed
// over, is never sent and marks no host failed, nor does a request that reached its connection within
// DeadlineClockMargin of its deadline and was answered 503 or cancelled at its own deadline, nor admin info that
// ended in the wait, nor a PUT that failed at its body's client; a host that fails a request is still marked.

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/minio/minio-go/v7"

	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/sidecartest"
)

// The base transport newFileClusterBaseTransport makes for settings, as the http.Transport it is.
func fileClusterTestBaseTransport(t *testing.T, settings *FileClusterSettings) *http.Transport {
	t.Helper()
	base, err := newFileClusterBaseTransport(settings)(false)
	if err != nil {
		t.Fatal(err)
	}
	transport, ok := base.(*http.Transport)
	if !ok {
		t.Fatalf("a base transport of %T", base)
	}
	return transport
}

// The cap is 64 by default and the idle connections kept match it, the total too; a cap past minio-go's idle total
// raises that; and a cap that is not positive leaves minio-go's transport uncapped as it was.
func TestFileClusterBaseTransportCapsItsHost(t *testing.T) {
	settings := DefaultFileClusterSettings()
	if settings.MaxConnsPerHost != 64 {
		t.Fatalf("MaxConnsPerHost %d by default, want 64", settings.MaxConnsPerHost)
	}
	transport := fileClusterTestBaseTransport(t, settings)
	if transport.MaxConnsPerHost != 64 || transport.MaxIdleConnsPerHost != 64 || transport.MaxIdleConns < 64 {
		t.Fatalf("%d connections a host, %d idle, %d idle in all", transport.MaxConnsPerHost, transport.MaxIdleConnsPerHost, transport.MaxIdleConns)
	}
	// HTTP/1 alone, one request on a connection, as the check at a connection's handover needs
	if transport.Protocols == nil || !transport.Protocols.HTTP1() || transport.Protocols.HTTP2() || transport.Protocols.UnencryptedHTTP2() {
		t.Fatalf("protocols %v", transport.Protocols)
	}
	if paced := fileClusterPacedTransport(transport).(*http.Transport); paced.Protocols == nil || paced.Protocols.HTTP2() {
		t.Fatalf("the paced transport's protocols %v", paced.Protocols)
	}
	settings.MaxConnsPerHost = 1024
	if transport := fileClusterTestBaseTransport(t, settings); transport.MaxIdleConnsPerHost != 1024 || transport.MaxIdleConns < 1024 {
		t.Fatalf("a cap of 1024 keeps %d idle, %d in all", transport.MaxIdleConnsPerHost, transport.MaxIdleConns)
	}
	minioTransport, err := minio.DefaultTransport(false)
	if err != nil {
		t.Fatal(err)
	}
	for _, maxConnsPerHost := range []int{0, -1} {
		settings.MaxConnsPerHost = maxConnsPerHost
		transport := fileClusterTestBaseTransport(t, settings)
		if transport.MaxConnsPerHost != 0 || transport.MaxIdleConnsPerHost != minioTransport.MaxIdleConnsPerHost || transport.MaxIdleConns != minioTransport.MaxIdleConns {
			t.Errorf("a cap of %d: %d connections a host, %d idle, %d idle in all", maxConnsPerHost, transport.MaxConnsPerHost, transport.MaxIdleConnsPerHost, transport.MaxIdleConns)
		}
	}
}

// A base transport a test tells apart by its pointer, answering nothing.
type fileClusterTestTransport struct {
	tls bool
}

func (self *fileClusterTestTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	return nil, errors.New("a test's transport sends nothing")
}

// Two clusters naming the same two hosts make one base transport a host, which each cluster's client of that host
// sends through; a cluster reaching a host by tls takes its own.
func TestFileClustersShareOneBaseTransportAHost(t *testing.T) {
	settings := DefaultFileClusterSettings()
	settings.NoChecks = true
	endpoints := []string{"host-a.example:9000", "host-b.example:9000"}
	made := []*fileClusterTestTransport{}
	newBaseTransport := func(tls bool) (http.RoundTripper, error) {
		transport := &fileClusterTestTransport{tls: tls}
		made = append(made, transport)
		return transport, nil
	}
	sidecarConfig := func() *SidecarConfig { return &SidecarConfig{} }
	clusters, err := newFileClusters(t.Context(), fileClusterTestConfigs(endpoints...), settings, newBaseTransport, sidecarConfig)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(clusters.Close)
	if len(made) != len(endpoints) {
		t.Fatalf("%d base transports for %d hosts", len(made), len(endpoints))
	}
	baseOf := func(cluster *FileCluster, index int) http.RoundTripper {
		return cluster.picker.hosts[index].transport.(*fileClusterTransport).base
	}
	meta, segment := clusters.clusterIdClusters["meta-1"], clusters.clusterIdClusters["seg-1"]
	for index, endpoint := range endpoints {
		if baseOf(meta, index) != baseOf(segment, index) || baseOf(meta, index) != made[index] {
			t.Errorf("%s: the clusters send through different base transports", endpoint)
		}
	}

	made = nil
	configs := fileClusterTestConfigs(endpoints...)
	configs[1].Tls = true
	tlsClusters, err := newFileClusters(t.Context(), configs, settings, newBaseTransport, sidecarConfig)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(tlsClusters.Close)
	if len(made) != 2*len(endpoints) || made[0].tls || !made[len(endpoints)].tls {
		t.Fatalf("%d base transports for %d hosts each reached with and without tls", len(made), len(endpoints))
	}
}

// One MinIO host whose every request waits at the host for the test's word, counting the connections it takes, at
// once and in all. A request whose body its client breaks off ends at the handler, unanswered and never arrived.
// Safe for concurrent use.
type fileClusterHeldHost struct {
	server *httptest.Server
	// one value as each request reaches a handler
	arrived chan struct{}
	// one value answers one request at a handler
	release chan struct{}
	// closed at shutdown, answering every request
	stop     chan struct{}
	stopOnce sync.Once

	stateLock    sync.Mutex
	openCount    int
	maxOpenCount int
	takenCount   int
	// what a released request is answered, 200 when zero
	answerStatus int
	// the path of each request that reached a handler, in order
	paths []string
}

func newFileClusterHeldHost(t *testing.T) *fileClusterHeldHost {
	t.Helper()
	host := &fileClusterHeldHost{arrived: make(chan struct{}, 64), release: make(chan struct{}), stop: make(chan struct{})}
	host.server = httptest.NewUnstartedServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		func() {
			host.stateLock.Lock()
			defer host.stateLock.Unlock()
			host.paths = append(host.paths, request.URL.Path)
		}()
		if _, err := io.Copy(io.Discard, request.Body); err != nil {
			return
		}
		host.arrived <- struct{}{}
		select {
		case <-host.release:
		case <-host.stop:
		}
		if status := host.status(); status != http.StatusOK {
			// the status alone, as a HEAD carries the fork's refusal of a request past its deadline
			writer.WriteHeader(status)
			return
		}
		// what a HEAD of an object answers, and an empty GET's or a PUT's
		header := writer.Header()
		header.Set("ETag", `"etag-1"`)
		header.Set("Last-Modified", time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC).Format(http.TimeFormat))
		header.Set("Content-Type", "application/octet-stream")
		header.Set("Content-Length", "0")
		writer.WriteHeader(http.StatusOK)
	}))
	host.server.Config.ConnState = func(conn net.Conn, state http.ConnState) {
		host.stateLock.Lock()
		defer host.stateLock.Unlock()
		switch state {
		case http.StateNew:
			host.takenCount += 1
			host.openCount += 1
			host.maxOpenCount = max(host.maxOpenCount, host.openCount)
		case http.StateClosed, http.StateHijacked:
			host.openCount -= 1
		}
	}
	host.server.Start()
	t.Cleanup(host.shutdown)
	return host
}

// The connections the host took in all, and the most open at once.
func (self *fileClusterHeldHost) counts() (takenCount int, maxOpenCount int) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.takenCount, self.maxOpenCount
}

func (self *fileClusterHeldHost) status() int {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if self.answerStatus == 0 {
		return http.StatusOK
	}
	return self.answerStatus
}

// What every request released from now on is answered.
func (self *fileClusterHeldHost) setAnswerStatus(status int) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.answerStatus = status
}

// Answers every request still held and closes the server once its handlers are done: at cleanup, or before a test
// reads which requests reached the host, so none is still on its way.
func (self *fileClusterHeldHost) shutdown() {
	self.stopOnce.Do(func() {
		close(self.stop)
		self.server.Close()
	})
}

// The path of each request that reached a handler. Complete once the host is shut down.
func (self *fileClusterHeldHost) requestPaths() []string {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return slices.Clone(self.paths)
}

// Eight HEADs through both clusters of one FileClusters, which name one host, under a cap of three: three reach the
// host on three connections, and as the host answers each, a waiting request takes its connection, so all eight are
// answered over the three, never more open.
func TestFileClusterTransportOpensNoMoreThanItsCapToAHost(t *testing.T) {
	const capCount = 3
	const requestCount = 8
	host := newFileClusterHeldHost(t)
	settings := DefaultFileClusterSettings()
	settings.MaxConnsPerHost = capCount
	settings.NoChecks = true
	// a request the test holds at the host is no dead host
	settings.RequestTimeout = time.Minute
	endpoint := strings.TrimPrefix(host.server.URL, "http://")
	clusters, err := newFileClusters(t.Context(), fileClusterTestConfigs(endpoint), settings, newFileClusterBaseTransport(settings), func() *SidecarConfig { return &SidecarConfig{} })
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(clusters.Close)
	results := make(chan error, requestCount)
	for i := range requestCount {
		cluster := clusters.clusterIdClusters["meta-1"]
		if i%2 == 1 {
			cluster = clusters.clusterIdClusters["seg-1"]
		}
		go func() {
			_, err := cluster.Head(t.Context(), "held/object")
			results <- err
		}()
	}
	for range capCount {
		<-host.arrived
	}
	for released := range requestCount {
		host.release <- struct{}{}
		// the answered request's connection goes to a waiting one
		if released+capCount < requestCount {
			<-host.arrived
		}
	}
	for range requestCount {
		if err := <-results; err != nil {
			t.Fatal(err)
		}
	}
	if takenCount, maxOpenCount := host.counts(); capCount < maxOpenCount || capCount < takenCount {
		t.Fatalf("the host took %d connections, %d open at once, past the cap of %d", takenCount, maxOpenCount, capCount)
	}
}

// A request's client: a GET or PutStream whose ctx says its body moves at a client's own pace takes the paced one,
// whose base transport is the capped one's without the cap and as shared between the clusters naming the host; any
// other request, and a request with no body on such a ctx, the capped one, whose slots the clusters naming the host
// share as they share its base transport.
func TestFileClusterHostSendsOnlyAPacedBodyOutsideTheCap(t *testing.T) {
	settings := DefaultFileClusterSettings()
	settings.NoChecks = true
	clusters, err := newFileClusters(t.Context(), fileClusterTestConfigs("host-a.example:9000"), settings, newFileClusterBaseTransport(settings), func() *SidecarConfig { return &SidecarConfig{} })
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(clusters.Close)
	host := clusters.clusterIdClusters["meta-1"].picker.hosts[0]
	paced := sidecar.WithBlobClientPaced(t.Context())
	if host.client(paced, true) != host.pacedCore || host.client(paced, false) != host.core || host.client(t.Context(), true) != host.core {
		t.Fatal("a request took the other client")
	}
	baseOf := func(transport http.RoundTripper) *http.Transport {
		return transport.(*fileClusterTransport).base.(*http.Transport)
	}
	capped, uncapped := baseOf(host.transport), baseOf(host.pacedTransport)
	if capped == uncapped || capped.MaxConnsPerHost != settings.MaxConnsPerHost || uncapped.MaxConnsPerHost != 0 || uncapped.MaxIdleConnsPerHost != settings.MaxConnsPerHost {
		t.Fatalf("capped %d a host, paced %d a host keeping %d idle", capped.MaxConnsPerHost, uncapped.MaxConnsPerHost, uncapped.MaxIdleConnsPerHost)
	}
	segmentHost := clusters.clusterIdClusters["seg-1"].picker.hosts[0]
	if baseOf(segmentHost.transport) != capped || baseOf(segmentHost.pacedTransport) != uncapped {
		t.Fatal("the clusters naming one host send through different base transports")
	}
	// the capped client's slots, one for each request in flight, are the host's alike, and the paced one has none
	slotsOf := func(transport http.RoundTripper) chan struct{} {
		return transport.(*fileClusterTransport).slots
	}
	if slots := slotsOf(host.transport); slots == nil || cap(slots) != settings.MaxConnsPerHost || slotsOf(segmentHost.transport) != slots {
		t.Fatal("the clusters naming one host hold different slots, or the cap's number of them is wrong")
	}
	if slotsOf(host.pacedTransport) != nil || slotsOf(segmentHost.pacedTransport) != nil {
		t.Fatal("the paced client holds slots")
	}
}

// With two HEADs holding both capped connections at the host, a GET and a PutStream at a client's own pace go
// through connections of their own: every request is answered, and the host took more connections than the cap,
// which requests sharing the capped transport could never open.
func TestFileClusterClientPacedBodiesTakeNoCappedConnection(t *testing.T) {
	const capCount = 2
	host := newFileClusterHeldHost(t)
	settings := DefaultFileClusterSettings()
	settings.MaxConnsPerHost = capCount
	settings.NoChecks = true
	settings.RequestTimeout = time.Minute
	endpoint := strings.TrimPrefix(host.server.URL, "http://")
	clusters, err := newFileClusters(t.Context(), fileClusterTestConfigs(endpoint), settings, newFileClusterBaseTransport(settings), func() *SidecarConfig { return &SidecarConfig{} })
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(clusters.Close)
	meta, segment := clusters.clusterIdClusters["meta-1"], clusters.clusterIdClusters["seg-1"]
	// the pool check a write makes first, as a check that read one pool leaves it
	segment.stateLock.Lock()
	segment.adminInfo = &FileClusterAdminInfo{ReadTime: clusters.clock.Now(), PoolCount: 1}
	segment.stateLock.Unlock()

	const requestCount = capCount + 2
	results := make(chan error, requestCount)
	for range capCount {
		go func() {
			_, err := meta.Head(t.Context(), "held/object")
			results <- err
		}()
	}
	for range capCount {
		<-host.arrived
	}
	paced := sidecar.WithBlobClientPaced(t.Context())
	go func() {
		object, err := meta.Get(paced, "held/object")
		if err == nil {
			_, err = io.ReadAll(object.Body)
			object.Body.Close()
		}
		results <- err
	}()
	go func() {
		segmentBytes := []byte("segment")
		sum := sha256.Sum256(segmentBytes)
		_, err := segment.PutStream(paced, "seg/held", bytes.NewReader(segmentBytes), int64(len(segmentBytes)), sum[:])
		results <- err
	}()
	for range requestCount {
		host.release <- struct{}{}
	}
	for range requestCount {
		if err := <-results; err != nil {
			t.Fatal(err)
		}
	}
	if takenCount, _ := host.counts(); takenCount <= capCount {
		t.Fatalf("the host took %d connections for %d requests, two of them at a client's pace, under a cap of %d", takenCount, requestCount, capCount)
	}
}

// A clock a test sets by hand, whose timers fire only when the test fires them, so its time can pass a timer's
// before the timer runs: how a test hands a request its connection after its deadline, or near it, with the
// deadline's timer still to come. Safe for concurrent use.
type fileClusterHandClock struct {
	stateLock sync.Mutex
	now       time.Time
	timers    []*fileClusterHandTimer
	// closed and replaced as a timer is set
	timerAdded chan struct{}
}

type fileClusterHandTimer struct {
	at       time.Time
	function func()
}

func newFileClusterHandClock(now time.Time) *fileClusterHandClock {
	return &fileClusterHandClock{now: now, timerAdded: make(chan struct{})}
}

func (self *fileClusterHandClock) Now() time.Time {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.now
}

// Never fires: these clusters run no check loop (NoChecks), After's one reader.
func (self *fileClusterHandClock) After(timeout time.Duration) <-chan time.Time {
	return make(chan time.Time)
}

func (self *fileClusterHandClock) AfterFunc(timeout time.Duration, function func()) (stop func() bool) {
	timer := &fileClusterHandTimer{function: function}
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		timer.at = self.now.Add(timeout)
		self.timers = append(self.timers, timer)
		close(self.timerAdded)
		self.timerAdded = make(chan struct{})
	}()
	return func() bool {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		index := slices.Index(self.timers, timer)
		if index < 0 {
			return false
		}
		self.timers = slices.Delete(self.timers, index, index+1)
		return true
	}
}

// Sets the time, firing nothing.
func (self *fileClusterHandClock) set(now time.Time) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.now = now
}

// Fires, on this goroutine, every timer due by the time set.
func (self *fileClusterHandClock) fireDue() {
	due := func() []*fileClusterHandTimer {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		due := []*fileClusterHandTimer{}
		pending := []*fileClusterHandTimer{}
		for _, timer := range self.timers {
			if timer.at.After(self.now) {
				pending = append(pending, timer)
			} else {
				due = append(due, timer)
			}
		}
		self.timers = pending
		return due
	}()
	for _, timer := range due {
		timer.function()
	}
}

// Returns once timerCount timers are set and neither fired nor stopped: how a test knows a request has issued, its
// deadline set, before moving the clock.
func (self *fileClusterHandClock) awaitTimers(t *testing.T, timerCount int) {
	t.Helper()
	for {
		added, enough := func() (chan struct{}, bool) {
			self.stateLock.Lock()
			defer self.stateLock.Unlock()
			return self.timerAdded, timerCount <= len(self.timers)
		}()
		if enough {
			return
		}
		select {
		case <-added:
		case <-t.Context().Done():
			t.Fatal(t.Context().Err())
		}
	}
}

// Settings for clusters over a held host: a cap of capCount connections a host, no check loop, and clock.
func fileClusterHeldSettings(capCount int, clock sidecar.Clock) *FileClusterSettings {
	settings := DefaultFileClusterSettings()
	settings.MaxConnsPerHost = capCount
	settings.NoChecks = true
	settings.Clock = clock
	return settings
}

// Clusters on settings over the one host at url, each host's base transport made from baseSettings, which a test
// may set apart from settings.
func newFileClustersOnHost(t *testing.T, url string, settings *FileClusterSettings, baseSettings *FileClusterSettings) *FileClusters {
	t.Helper()
	endpoint := strings.TrimPrefix(url, "http://")
	clusters, err := newFileClusters(t.Context(), fileClusterTestConfigs(endpoint), settings, newFileClusterBaseTransport(baseSettings), func() *SidecarConfig { return &SidecarConfig{} })
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(clusters.Close)
	return clusters
}

// Lets cluster take writes, as a check that read one pool leaves it.
func fileClusterTestPoolsChecked(cluster *FileCluster) {
	cluster.stateLock.Lock()
	defer cluster.stateLock.Unlock()
	cluster.adminInfo = &FileClusterAdminInfo{ReadTime: cluster.clusters.clock.Now(), PoolCount: 1}
}

// A streamed PUT on a cluster's capped client whose body waits on the test after its first byte: it holds one of
// its host's slots and one of its capped connections, with no timer running, until the test breaks its body, which
// gives the slot back and closes the connection. Its failure is its body's.
type fileClusterHeldPut struct {
	body   *io.PipeWriter
	result chan error
}

func startFileClusterHeldPut(t *testing.T, cluster *FileCluster, key string) *fileClusterHeldPut {
	t.Helper()
	reader, writer := io.Pipe()
	put := &fileClusterHeldPut{body: writer, result: make(chan error, 1)}
	t.Cleanup(func() {
		writer.CloseWithError(errors.New("the test ended"))
	})
	go func() {
		sum := sha256.Sum256([]byte("held"))
		_, err := cluster.PutStream(t.Context(), key, reader, 1024, sum[:])
		put.result <- err
	}()
	// the transport takes the body's first byte only once the PUT has its connection
	if _, err := writer.Write([]byte{0}); err != nil {
		t.Fatal(err)
	}
	return put
}

// Breaks the body, ending the PUT and closing its connection, and returns the PUT's error.
func (self *fileClusterHeldPut) breakBody() error {
	self.body.CloseWithError(errors.New("the test broke the body"))
	return <-self.result
}

// With the cap at one and a HEAD holding the host's one connection, a second HEAD whose deadline passes as it
// waits fails here with errFileClusterConnWait, never sent, and the host stays unmarked, while the first is
// answered as before. A host that answers a request 500 is still marked failed.
func TestFileClusterRequestWhoseDeadlinePassesInTheCappedWaitIsNotSent(t *testing.T) {
	host := newFileClusterHeldHost(t)
	clock := sidecartest.NewFakeClock(sidecartest.DefaultClockStart())
	settings := fileClusterHeldSettings(1, clock)
	clusters := newFileClustersOnHost(t, host.server.URL, settings, settings)
	cluster := clusters.clusterIdClusters["meta-1"]
	clusterHost := cluster.picker.hosts[0]

	first := make(chan error, 1)
	go func() {
		_, err := cluster.Head(t.Context(), "held/first")
		first <- err
	}()
	<-host.arrived
	late := make(chan error, 1)
	go func() {
		_, err := cluster.Head(t.Context(), "held/late")
		late <- err
	}()
	// the one deadline set is the second request's, as it starts to wait: the first took its slot at once
	if err := clock.AwaitTimers(t.Context(), 1); err != nil {
		t.Fatal(err)
	}
	clock.Advance(settings.RequestTimeout)
	if err := <-late; !errors.Is(err, errFileClusterConnWait) {
		t.Fatalf("the request whose deadline passed in the wait: %v", err)
	}
	host.release <- struct{}{}
	if err := <-first; err != nil {
		t.Fatalf("the request that held the connection: %v", err)
	}
	if clusterHost.failedAtNanos.Load() != 0 {
		t.Fatal("a wait for a capped connection marked a healthy host failed")
	}

	host.setAnswerStatus(http.StatusInternalServerError)
	failed := make(chan error, 1)
	go func() {
		_, err := cluster.Head(t.Context(), "held/failed")
		failed <- err
	}()
	<-host.arrived
	host.release <- struct{}{}
	if err := <-failed; err == nil || errors.Is(err, errFileClusterConnWait) {
		t.Fatalf("a request the host failed: %v", err)
	}
	if clusterHost.failedAtNanos.Load() == 0 {
		t.Error("a host that answered 500 was not marked failed")
	}
	host.shutdown()
	if paths := host.requestPaths(); slices.Contains(paths, "/meta-bucket/held/late") {
		t.Errorf("the request whose deadline passed in the wait reached the host: %q", paths)
	}
	if takenCount, _ := host.counts(); takenCount != 1 {
		t.Errorf("the host took %d connections under a cap of 1", takenCount)
	}
}

// A request whose deadline passes just as the slot it waits for frees, before its deadline's timer runs, gives the
// slot back unsent: it fails with errFileClusterConnWait, takes no connection, and no host is marked. The slot frees
// as a held PUT's body breaks, a failure of its client's that marks no host either.
func TestFileClusterRequestGivenItsSlotPastItsDeadlineIsNotSent(t *testing.T) {
	host := newFileClusterHeldHost(t)
	clock := newFileClusterHandClock(sidecartest.DefaultClockStart())
	settings := fileClusterHeldSettings(1, clock)
	clusters := newFileClustersOnHost(t, host.server.URL, settings, settings)
	meta, segment := clusters.clusterIdClusters["meta-1"], clusters.clusterIdClusters["seg-1"]
	fileClusterTestPoolsChecked(segment)

	put := startFileClusterHeldPut(t, segment, "seg/held")
	late := make(chan error, 1)
	go func() {
		_, err := meta.Head(t.Context(), "held/late")
		late <- err
	}()
	// a streamed PUT sets no deadline, so this is the HEAD's, set as it starts to wait
	clock.awaitTimers(t, 1)
	clock.set(clock.Now().Add(settings.RequestTimeout))
	if err := put.breakBody(); err == nil {
		t.Fatal("a PUT whose body broke was answered")
	}
	if err := <-late; !errors.Is(err, errFileClusterConnWait) {
		t.Fatalf("the request given its slot past its deadline: %v", err)
	}
	if meta.picker.hosts[0].failedAtNanos.Load() != 0 {
		t.Error("a host was marked failed")
	}
	host.shutdown()
	if paths := host.requestPaths(); slices.Contains(paths, "/meta-bucket/held/late") {
		t.Errorf("the request given its slot past its deadline reached the host: %q", paths)
	}
	if takenCount, _ := host.counts(); takenCount != 1 {
		t.Errorf("the host took %d connections, the held PUT's alone being due", takenCount)
	}
}

// A request with its slot that waits inside http.Transport for a connection, as one can a moment for a connection
// another request is closing, and is handed it only past its deadline, its deadline's timer not yet run, is stopped
// at the handover: the connection is closed before a byte of the request is written, the request fails with
// errFileClusterConnWait, and no host is marked. Here the slots let two requests through and the transport one
// connection, so the second waits inside http.Transport; it carries its own deadline, whose timer, set at issue,
// says it has issued.
func TestFileClusterRequestHandedItsConnectionPastItsDeadlineIsNotSent(t *testing.T) {
	host := newFileClusterHeldHost(t)
	clock := newFileClusterHandClock(sidecartest.DefaultClockStart())
	settings := fileClusterHeldSettings(2, clock)
	clusters := newFileClustersOnHost(t, host.server.URL, settings, fileClusterHeldSettings(1, clock))
	meta, segment := clusters.clusterIdClusters["meta-1"], clusters.clusterIdClusters["seg-1"]
	fileClusterTestPoolsChecked(segment)

	put := startFileClusterHeldPut(t, segment, "seg/held")
	own := clock.Now().Add(time.Second)
	late := make(chan error, 1)
	go func() {
		_, err := meta.Head(sidecar.WithBlobDeadline(t.Context(), own), "held/late")
		late <- err
	}()
	clock.awaitTimers(t, 1)
	clock.set(own)
	if err := put.breakBody(); err == nil {
		t.Fatal("a PUT whose body broke was answered")
	}
	if err := <-late; !errors.Is(err, errFileClusterConnWait) {
		t.Fatalf("the request handed its connection past its deadline: %v", err)
	}
	if meta.picker.hosts[0].failedAtNanos.Load() != 0 {
		t.Error("a host was marked failed")
	}
	host.shutdown()
	if paths := host.requestPaths(); slices.Contains(paths, "/meta-bucket/held/late") {
		t.Errorf("the request handed its connection past its deadline reached the host: %q", paths)
	}
}

// A request that reaches its connection within DeadlineClockMargin of its deadline, as one that waited in the cap
// that long does, and is answered 503, as the fork refuses on arrival a request its clock reads as past its
// deadline, marks no host failed. One with its time ahead of it that is answered 503 still marks its host.
func TestFileClusterRequestConnectedNearItsDeadlineMarksNoHostFor503(t *testing.T) {
	host := newFileClusterHeldHost(t)
	clock := newFileClusterHandClock(sidecartest.DefaultClockStart())
	settings := fileClusterHeldSettings(1, clock)
	clusters := newFileClustersOnHost(t, host.server.URL, settings, settings)
	cluster := clusters.clusterIdClusters["meta-1"]
	clusterHost := cluster.picker.hosts[0]

	first := make(chan error, 1)
	go func() {
		_, err := cluster.Head(t.Context(), "held/first")
		first <- err
	}()
	<-host.arrived
	late := make(chan error, 1)
	go func() {
		_, err := cluster.Head(t.Context(), "held/late")
		late <- err
	}()
	// the second request's deadline, set as it starts to wait for the slot
	clock.awaitTimers(t, 1)
	// the slot frees half the margin before that deadline
	clock.set(clock.Now().Add(settings.RequestTimeout - settings.DeadlineClockMargin/2))
	host.release <- struct{}{}
	if err := <-first; err != nil {
		t.Fatal(err)
	}
	<-host.arrived
	host.setAnswerStatus(http.StatusServiceUnavailable)
	host.release <- struct{}{}
	if err := <-late; err == nil || errors.Is(err, errFileClusterConnWait) {
		t.Fatalf("the request connected near its deadline: %v", err)
	}
	if clusterHost.failedAtNanos.Load() != 0 {
		t.Fatal("a 503 to a request connected near its deadline marked its host failed")
	}

	onTime := make(chan error, 1)
	go func() {
		_, err := cluster.Head(t.Context(), "held/on-time")
		onTime <- err
	}()
	<-host.arrived
	host.release <- struct{}{}
	if err := <-onTime; err == nil {
		t.Fatal("a 503 read as an answer")
	}
	if clusterHost.failedAtNanos.Load() == 0 {
		t.Error("a 503 to a request with its time ahead of it did not mark its host failed")
	}
}

// A request with its own deadline that reaches its connection within DeadlineClockMargin of it, and is cancelled
// there with the host not yet answering, marks no host failed. One cancelled at its own deadline that reached its
// connection with its time ahead of it still marks its host.
func TestFileClusterRequestConnectedNearItsOwnDeadlineMarksNoHostForItsCancel(t *testing.T) {
	host := newFileClusterHeldHost(t)
	clock := newFileClusterHandClock(sidecartest.DefaultClockStart())
	settings := fileClusterHeldSettings(1, clock)
	clusters := newFileClustersOnHost(t, host.server.URL, settings, settings)
	cluster := clusters.clusterIdClusters["meta-1"]
	clusterHost := cluster.picker.hosts[0]
	ownTimeout := time.Second

	first := make(chan error, 1)
	go func() {
		_, err := cluster.Head(t.Context(), "held/first")
		first <- err
	}()
	<-host.arrived
	own := clock.Now().Add(ownTimeout)
	late := make(chan error, 1)
	go func() {
		_, err := cluster.Head(sidecar.WithBlobDeadline(t.Context(), own), "held/late")
		late <- err
	}()
	// the second request's own deadline, set at issue
	clock.awaitTimers(t, 1)
	clock.set(own.Add(-settings.DeadlineClockMargin / 2))
	host.release <- struct{}{}
	if err := <-first; err != nil {
		t.Fatal(err)
	}
	<-host.arrived
	// the host holds it to its own deadline
	clock.set(own)
	clock.fireDue()
	if err := <-late; err == nil || errors.Is(err, errFileClusterConnWait) {
		t.Fatalf("the request cancelled at its own deadline: %v", err)
	}
	if clusterHost.failedAtNanos.Load() != 0 {
		t.Fatal("the cancel of a request connected near its own deadline marked its host failed")
	}
	host.release <- struct{}{}

	ownOnTime := clock.Now().Add(ownTimeout)
	onTime := make(chan error, 1)
	go func() {
		_, err := cluster.Head(sidecar.WithBlobDeadline(t.Context(), ownOnTime), "held/on-time")
		onTime <- err
	}()
	<-host.arrived
	clock.set(ownOnTime)
	clock.fireDue()
	if err := <-onTime; err == nil {
		t.Fatal("a request cancelled at its own deadline read as answered")
	}
	if clusterHost.failedAtNanos.Load() == 0 {
		t.Error("the cancel of a request that had its time ahead of it did not mark its host failed")
	}
	host.release <- struct{}{}
}

// Admin info that waits for the capped connection past its RequestTimeout fails with errFileClusterConnWait,
// never sent, and marks no host failed. AdminInfo bounds its request in real time, so the clusters' RequestTimeout
// is a millisecond here; the held HEAD keeps the one connection throughout, so that bound, and no race, ends the
// wait. The base transport keeps the default bound, which the held HEAD outlasts.
func TestFileClusterAdminInfoEndedInTheCappedWaitMarksNoHost(t *testing.T) {
	host := newFileClusterHeldHost(t)
	clock := sidecartest.NewFakeClock(sidecartest.DefaultClockStart())
	settings := fileClusterHeldSettings(1, clock)
	settings.RequestTimeout = time.Millisecond
	clusters := newFileClustersOnHost(t, host.server.URL, settings, fileClusterHeldSettings(1, clock))
	cluster := clusters.clusterIdClusters["meta-1"]

	first := make(chan error, 1)
	go func() {
		_, err := cluster.Head(t.Context(), "held/first")
		first <- err
	}()
	<-host.arrived
	if _, err := cluster.AdminInfo(t.Context()); !errors.Is(err, errFileClusterConnWait) {
		t.Fatalf("admin info past its RequestTimeout in the wait: %v", err)
	}
	if cluster.picker.hosts[0].failedAtNanos.Load() != 0 {
		t.Error("admin info's wait marked a healthy host failed")
	}
	host.release <- struct{}{}
	if err := <-first; err != nil {
		t.Fatal(err)
	}
	host.shutdown()
	if paths := host.requestPaths(); slices.Contains(paths, fileClusterAdminInfoPath) {
		t.Errorf("admin info reached the host: %q", paths)
	}
}

// A request issued with its own deadline already past is sent nowhere: it fails with errFileClusterConnWait, a
// write's as an unknown outcome its caller rereads, and marks no host.
func TestFileClusterRequestIssuedPastItsOwnDeadlineIsNotSent(t *testing.T) {
	fixture := newFileClusterFixture(t, newFakeS3(1, "build-1"), "")
	past := sidecar.WithBlobDeadline(t.Context(), fixture.clock.Now().Add(-time.Millisecond))
	if _, err := fixture.cluster.Head(past, "head/a/_"); !errors.Is(err, errFileClusterConnWait) {
		t.Errorf("a HEAD issued past its own deadline: %v", err)
	}
	err := fixture.cluster.Delete(past, "head/a/_")
	var unknown *sidecar.BlobUnknownError
	if !errors.Is(err, errFileClusterConnWait) || !errors.As(err, &unknown) {
		t.Errorf("a DELETE issued past its own deadline: %v", err)
	}
	if requests := fixture.store.takeRequests(); len(requests) != 0 {
		t.Errorf("%d requests reached the store", len(requests))
	}
	if fixture.cluster.picker.hosts[0].failedAtNanos.Load() != 0 {
		t.Error("a request never sent marked its host failed")
	}
}

// One host that reads a PUT's body up to a count and then, at the test's word, answers 503 RequestTimeout without
// waiting for the rest, as the fork answers a body idle past its connection's timeout.
type fileClusterImpatientHost struct {
	server *httptest.Server
	// the bytes of the PUT's body read before its answer
	bodyRead chan int64
	// closed to answer
	answer     chan struct{}
	answerOnce sync.Once
}

func newFileClusterImpatientHost(t *testing.T, readByteCount int64) *fileClusterImpatientHost {
	t.Helper()
	host := &fileClusterImpatientHost{bodyRead: make(chan int64, 1), answer: make(chan struct{})}
	host.server = httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		// the answer goes with the body unread
		http.NewResponseController(writer).EnableFullDuplex()
		count, _ := io.CopyN(io.Discard, request.Body, readByteCount)
		host.bodyRead <- count
		<-host.answer
		writer.Header().Set("Connection", "close")
		writer.Header().Set("Content-Type", "application/xml")
		writer.WriteHeader(http.StatusServiceUnavailable)
		io.WriteString(writer, `<?xml version="1.0" encoding="UTF-8"?><Error><Code>RequestTimeout</Code><Message>A timeout occurred</Message></Error>`)
	}))
	t.Cleanup(func() {
		host.answerNow()
		host.server.Close()
	})
	return host
}

func (self *fileClusterImpatientHost) answerNow() {
	self.answerOnce.Do(func() {
		close(self.answer)
	})
}

// A body that gives what source has, then says it stalls and waits on the test until it ends.
type fileClusterStallingBody struct {
	source      io.Reader
	stalled     chan struct{}
	stalledOnce sync.Once
	end         chan struct{}
}

func (self *fileClusterStallingBody) Read(buffer []byte) (int, error) {
	if count, err := self.source.Read(buffer); 0 < count || err != io.EOF {
		return count, err
	}
	self.stalledOnce.Do(func() {
		close(self.stalled)
	})
	<-self.end
	return 0, errors.New("the client went away")
}

// A PUT whose body comes at its client's own pace and fails at that client marks no host failed: one the store
// answers 503 while it waits on a stalled client, as the fork answers RequestTimeout to a body idle past its
// connection's timeout, and one whose body ends short or runs long. A 503 to a PUT whose body came whole still
// marks its host.
func TestFileClusterPacedPutFailedAtItsClientMarksNoHost(t *testing.T) {
	const byteCount = 2 * 1024
	segmentBytes := bytes.Repeat([]byte{7}, byteCount)
	sum := sha256.Sum256(segmentBytes)
	newSegment := func(t *testing.T, host *fileClusterImpatientHost) *FileCluster {
		clock := sidecartest.NewFakeClock(sidecartest.DefaultClockStart())
		settings := fileClusterHeldSettings(1, clock)
		segment := newFileClustersOnHost(t, host.server.URL, settings, settings).clusterIdClusters["seg-1"]
		fileClusterTestPoolsChecked(segment)
		return segment
	}
	paced := sidecar.WithBlobClientPaced(t.Context())

	// stalled: the store answers while the transport waits on the client for the second half
	host := newFileClusterImpatientHost(t, byteCount/2)
	segment := newSegment(t, host)
	body := &fileClusterStallingBody{source: bytes.NewReader(segmentBytes[:byteCount/2]), stalled: make(chan struct{}), end: make(chan struct{})}
	defer close(body.end)
	stalled := make(chan error, 1)
	go func() {
		_, err := segment.PutStream(paced, "seg/stalled", body, byteCount, sum[:])
		stalled <- err
	}()
	<-host.bodyRead
	<-body.stalled
	host.answerNow()
	if err := <-stalled; err == nil {
		t.Fatal("a PUT answered 503 read as stored")
	}
	if segment.picker.hosts[0].failedAtNanos.Load() != 0 {
		t.Error("a store's 503 to a stalled client marked its host failed")
	}

	// short and long: the body breaks its own declared length
	for _, c := range []struct {
		name string
		body []byte
	}{
		{name: "short", body: segmentBytes[:byteCount/2]},
		{name: "long", body: append(slices.Clone(segmentBytes), 7)},
	} {
		host := newFileClusterImpatientHost(t, byteCount)
		segment := newSegment(t, host)
		if _, err := segment.PutStream(paced, "seg/"+c.name, bytes.NewReader(c.body), byteCount, sum[:]); err == nil {
			t.Errorf("%s: a body of %d bytes for %d was stored", c.name, len(c.body), byteCount)
		}
		if segment.picker.hosts[0].failedAtNanos.Load() != 0 {
			t.Errorf("%s: a body that broke its length marked its host failed", c.name)
		}
	}

	// whole: the store answers 503 once the whole body is written, its host's failure
	host = newFileClusterImpatientHost(t, byteCount)
	segment = newSegment(t, host)
	wrote := make(chan struct{})
	traced := httptrace.WithClientTrace(paced, &httptrace.ClientTrace{
		WroteRequest: func(info httptrace.WroteRequestInfo) {
			close(wrote)
		},
	})
	whole := make(chan error, 1)
	go func() {
		_, err := segment.PutStream(traced, "seg/whole", bytes.NewReader(segmentBytes), byteCount, sum[:])
		whole <- err
	}()
	<-host.bodyRead
	<-wrote
	host.answerNow()
	if err := <-whole; err == nil {
		t.Fatal("a PUT answered 503 read as stored")
	}
	if segment.picker.hosts[0].failedAtNanos.Load() == 0 {
		t.Error("a 503 to a PUT whose body came whole did not mark its host failed")
	}
}
