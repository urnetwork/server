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
// over, is never sent and marks no host failed, nor does a request that reached its connection with less than
// DeadlineHostMinimum left and was answered 503 or cancelled at its own deadline (owner decision 790; QD-1405), nor a
// PUT that failed at its body's client; a host that fails a request it had time for is still marked.
//
// The stores' own checks go outside the cap too (owner decision 790; QD-1406): the admin route, and a HEAD or LIST
// on a ctx sidecar.WithBlobProbe marks, so with the cap held full a cluster's check and an engine's probe still reach
// the store and read it, while a store that fails still fails them, and its cluster closes once they go stale.

import (
	"bytes"
	"context"
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

// The path part that marks a request a store-backed held host holds: a key under held/.
const fileClusterHeldPathPart = "/held/"

// The host name a store-backed held host gives its store, as fakeS3.downHosts names it.
const fileClusterHeldHostName = "held-host"

// One MinIO host whose every request waits at the host for the test's word, counting the connections it takes, at
// once and in all. A request whose body its client breaks off ends at the handler, unanswered and never arrived.
// Over a store, only a request for a key under held/ waits, and the store answers every other at once, the admin
// route among them. Safe for concurrent use.
type fileClusterHeldHost struct {
	server *httptest.Server
	// nil for none: every request is held
	store *fakeS3
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
	return newFileClusterHeldHostOver(t, nil)
}

// A held host over store, nil for none (the type's doc).
func newFileClusterHeldHostOver(t *testing.T, store *fakeS3) *fileClusterHeldHost {
	t.Helper()
	host := &fileClusterHeldHost{store: store, arrived: make(chan struct{}, 64), release: make(chan struct{}), stop: make(chan struct{})}
	host.server = httptest.NewUnstartedServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		func() {
			host.stateLock.Lock()
			defer host.stateLock.Unlock()
			host.paths = append(host.paths, request.URL.Path)
		}()
		if host.store != nil && !strings.Contains(request.URL.Path, fileClusterHeldPathPart) {
			host.serveFromStore(writer, request)
			return
		}
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

// Answers request as the store does, and resets its connection when the store fails it at the transport, as a host
// that is down does.
func (self *fileClusterHeldHost) serveFromStore(writer http.ResponseWriter, request *http.Request) {
	response, err := self.store.serve(fileClusterHeldHostName, request)
	if err != nil {
		panic(http.ErrAbortHandler)
	}
	defer response.Body.Close()
	for name, values := range response.Header {
		writer.Header()[name] = values
	}
	writer.WriteHeader(response.StatusCode)
	io.Copy(writer, response.Body)
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
// whose base transport is the capped one's without the cap and as shared between the clusters naming the host, and
// so does a HEAD or LIST whose ctx says a store check sends it, and the admin route; any other request, a write on
// either ctx, a GET on a probe's and a HEAD or LIST on a paced one among them, the capped one, whose slots the
// clusters naming the host share as they share its base transport.
func TestFileClusterHostSendsOnlyPacedBodiesAndChecksOutsideTheCap(t *testing.T) {
	settings := DefaultFileClusterSettings()
	settings.NoChecks = true
	clusters, err := newFileClusters(t.Context(), fileClusterTestConfigs("host-a.example:9000"), settings, newFileClusterBaseTransport(settings), func() *SidecarConfig { return &SidecarConfig{} })
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(clusters.Close)
	cluster := clusters.clusterIdClusters["meta-1"]
	host := cluster.picker.hosts[0]
	paced := sidecar.WithBlobClientPaced(t.Context())
	probe := sidecar.WithBlobProbe(t.Context())
	for _, c := range []struct {
		name  string
		ctx   context.Context
		kind  fileClusterRequestKind
		paced bool
	}{
		{name: "a paced body", ctx: paced, kind: fileClusterRequestBody, paced: true},
		{name: "a probe's HEAD or LIST", ctx: probe, kind: fileClusterRequestHeadOrList, paced: true},
		{name: "a body", ctx: t.Context(), kind: fileClusterRequestBody},
		{name: "a HEAD or LIST", ctx: t.Context(), kind: fileClusterRequestHeadOrList},
		{name: "a write", ctx: t.Context(), kind: fileClusterRequestWrite},
		{name: "a paced write", ctx: paced, kind: fileClusterRequestWrite},
		{name: "a probe's write", ctx: probe, kind: fileClusterRequestWrite},
		{name: "a probe's GET", ctx: probe, kind: fileClusterRequestBody},
		{name: "a paced HEAD or LIST", ctx: paced, kind: fileClusterRequestHeadOrList},
	} {
		want := host.core
		if c.paced {
			want = host.pacedCore
		}
		if host.client(c.ctx, c.kind) != want {
			t.Errorf("%s took the other client", c.name)
		}
	}
	if cluster.adminClient(host).Transport != host.pacedTransport {
		t.Error("the admin route goes through the capped transport")
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

// A HEAD to a host whose one capped connection another holds, which reaches that connection with left of its
// RequestTimeout deadline and is answered with the status the host is set to by then: the holder takes the slot at
// once, the HEAD waits for it, and the hand clock moves to left before the deadline as the holder's answer frees
// it. The holder is answered 200, the HEAD what the test sets once it has arrived; this returns the HEAD's error.
func fileClusterHeadConnectedWithLeft(t *testing.T, host *fileClusterHeldHost, clock *fileClusterHandClock, cluster *FileCluster, left time.Duration, status int) error {
	t.Helper()
	holder := make(chan error, 1)
	go func() {
		_, err := cluster.Head(t.Context(), "held/holder")
		holder <- err
	}()
	<-host.arrived
	issued := clock.Now()
	waiting := make(chan error, 1)
	go func() {
		_, err := cluster.Head(t.Context(), "held/waiting")
		waiting <- err
	}()
	// the waiting request's deadline, set as it starts to wait for the slot
	clock.awaitTimers(t, 1)
	clock.set(issued.Add(cluster.RequestTimeout() - left))
	host.setAnswerStatus(http.StatusOK)
	host.release <- struct{}{}
	if err := <-holder; err != nil {
		t.Fatal(err)
	}
	<-host.arrived
	host.setAnswerStatus(status)
	host.release <- struct{}{}
	return <-waiting
}

// A request that reaches its connection with less than DeadlineHostMinimum left, as one that waited in the cap does,
// and is answered 503, as the fork refuses a request its deadline passes at, marks no host failed: here half the
// floor, 500 ms, five times what the clocks alone account for. One that reaches it with the floor left, or with its
// whole deadline ahead, and is answered 503 still marks its host, as one under the floor answered 500 does.
func TestFileClusterRequestConnectedUnderTheFloorMarksNoHostFor503(t *testing.T) {
	host := newFileClusterHeldHost(t)
	clock := newFileClusterHandClock(sidecartest.DefaultClockStart())
	settings := fileClusterHeldSettings(1, clock)
	if settings.DeadlineHostMinimum != time.Second {
		t.Fatalf("DeadlineHostMinimum %v by default, want 1 s", settings.DeadlineHostMinimum)
	}
	clusters := newFileClustersOnHost(t, host.server.URL, settings, settings)
	cluster := clusters.clusterIdClusters["meta-1"]
	clusterHost := cluster.picker.hosts[0]

	for _, c := range []struct {
		name   string
		left   time.Duration
		status int
		marked bool
	}{
		{name: "under the floor, 503", left: settings.DeadlineHostMinimum / 2, status: http.StatusServiceUnavailable},
		{name: "at the floor, 503", left: settings.DeadlineHostMinimum, status: http.StatusServiceUnavailable, marked: true},
		{name: "with its whole deadline, 503", left: settings.RequestTimeout, status: http.StatusServiceUnavailable, marked: true},
		{name: "under the floor, 500", left: settings.DeadlineHostMinimum / 2, status: http.StatusInternalServerError, marked: true},
	} {
		clusterHost.failedAtNanos.Store(0)
		if err := fileClusterHeadConnectedWithLeft(t, host, clock, cluster, c.left, c.status); err == nil || errors.Is(err, errFileClusterConnWait) {
			t.Fatalf("%s: %v", c.name, err)
		}
		if marked := clusterHost.failedAtNanos.Load() != 0; marked != c.marked {
			t.Errorf("%s: the host marked failed %v, want %v", c.name, marked, c.marked)
		}
	}
}

// A request with its own deadline that reaches its connection with less than DeadlineHostMinimum of it left, and is
// cancelled there with the host not yet answering, marks no host failed. One cancelled at its own deadline that
// reached its connection with more than the floor left still marks its host.
func TestFileClusterRequestConnectedUnderTheFloorMarksNoHostForItsCancel(t *testing.T) {
	host := newFileClusterHeldHost(t)
	clock := newFileClusterHandClock(sidecartest.DefaultClockStart())
	settings := fileClusterHeldSettings(1, clock)
	clusters := newFileClustersOnHost(t, host.server.URL, settings, settings)
	cluster := clusters.clusterIdClusters["meta-1"]
	clusterHost := cluster.picker.hosts[0]
	ownTimeout := 2 * settings.DeadlineHostMinimum

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
	clock.set(own.Add(-settings.DeadlineHostMinimum / 2))
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
		t.Fatal("the cancel of a request connected under the floor marked its host failed")
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
		t.Error("the cancel of a request that had more than the floor left did not mark its host failed")
	}
	host.release <- struct{}{}
}

// A request whose own deadline is under DeadlineHostMinimum by design, as a marker's or drop's DELETE 50 ms before
// until_ms is, reaches its connection under the floor however soon it is sent: neither its 503 nor its cancel at its
// own deadline marks its host, while a 500 to it still does.
func TestFileClusterShortOwnDeadlineMarksNoHostFor503OrItsCancel(t *testing.T) {
	host := newFileClusterHeldHost(t)
	clock := newFileClusterHandClock(sidecartest.DefaultClockStart())
	settings := fileClusterHeldSettings(1, clock)
	clusters := newFileClustersOnHost(t, host.server.URL, settings, settings)
	cluster := clusters.clusterIdClusters["meta-1"]
	fileClusterTestPoolsChecked(cluster)
	clusterHost := cluster.picker.hosts[0]
	short := settings.DeadlineHostMinimum / 2
	// a DELETE with its own deadline short ahead, arrived at the host
	deleteHeld := func(key string) (own time.Time, result chan error) {
		own = clock.Now().Add(short)
		result = make(chan error, 1)
		go func() {
			result <- cluster.Delete(sidecar.WithBlobDeadline(t.Context(), own), key)
		}()
		<-host.arrived
		return own, result
	}

	host.setAnswerStatus(http.StatusServiceUnavailable)
	_, refused := deleteHeld("held/refused")
	host.release <- struct{}{}
	if err := <-refused; err == nil || errors.Is(err, errFileClusterConnWait) {
		t.Fatalf("a short DELETE answered 503: %v", err)
	}
	if clusterHost.failedAtNanos.Load() != 0 {
		t.Fatal("a 503 to a short DELETE marked its host failed")
	}

	own, cancelled := deleteHeld("held/cancelled")
	clock.set(own)
	clock.fireDue()
	if err := <-cancelled; err == nil || errors.Is(err, errFileClusterConnWait) {
		t.Fatalf("a short DELETE cancelled at its own deadline: %v", err)
	}
	if clusterHost.failedAtNanos.Load() != 0 {
		t.Fatal("the cancel of a short DELETE at its own deadline marked its host failed")
	}
	host.release <- struct{}{}

	host.setAnswerStatus(http.StatusInternalServerError)
	_, failed := deleteHeld("held/failed")
	host.release <- struct{}{}
	if err := <-failed; err == nil {
		t.Fatal("a DELETE answered 500 read as done")
	}
	if clusterHost.failedAtNanos.Load() == 0 {
		t.Error("a 500 to a short DELETE did not mark its host failed")
	}
}

// Clusters over a store-backed held host whose cap of one connection a HEAD holds, so every capped request waits,
// with the sentinel xops writes in each bucket and one repo's place/. The base transport gives a held request a
// minute, which no test step outlasts, and sidecarYaml is sidecar.yml. Returns the clusters, and what releases the
// holding HEAD and returns its error.
func newFileClustersWithTheCapHeld(t *testing.T, store *fakeS3, clock sidecar.Clock, sidecarYaml string) (*FileClusters, *fileClusterHeldHost, func() error) {
	t.Helper()
	for _, key := range []string{"meta-bucket/" + fileClusterSentinelKey, "seg-bucket/" + fileClusterSentinelKey, "meta-bucket/" + sidecar.EvsPlaceKeyPrefix + "repo-1"} {
		store.objects[key] = &fakeS3Object{body: []byte("synthetic"), etag: "etag-synthetic"}
	}
	host := newFileClusterHeldHostOver(t, store)
	settings := fileClusterHeldSettings(1, clock)
	baseSettings := fileClusterHeldSettings(1, clock)
	baseSettings.RequestTimeout = time.Minute
	config, err := parseSidecarConfig([]byte(sidecarYaml))
	if err != nil {
		t.Fatal(err)
	}
	endpoint := strings.TrimPrefix(host.server.URL, "http://")
	clusters, err := newFileClusters(t.Context(), fileClusterTestConfigs(endpoint), settings, newFileClusterBaseTransport(baseSettings), func() *SidecarConfig { return config })
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(clusters.Close)
	holder := make(chan error, 1)
	go func() {
		_, err := clusters.clusterIdClusters["meta-1"].Head(t.Context(), "held/holder")
		holder <- err
	}()
	<-host.arrived
	return clusters, host, func() error {
		host.release <- struct{}{}
		return <-holder
	}
}

// Runs work, whose requests all go outside the cap, and fails the test as soon as one of them waits in it instead: a
// request waiting for a capped slot sets a timer on clock for its deadline, and nothing else in work sets one. No
// timer is pending as it starts. work must not touch t, and its results are the caller's to read once this returns.
func requireFileClusterNoCappedWait(t *testing.T, clock *sidecartest.FakeClock, work func()) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		work()
	}()
	waitCtx, cancel := context.WithCancel(t.Context())
	defer cancel()
	waited := make(chan error, 1)
	go func() {
		waited <- clock.AwaitTimers(waitCtx, 1)
	}()
	select {
	case <-done:
	case err := <-waited:
		if err == nil {
			t.Fatal("a request that goes outside the cap waited in it")
		}
		<-done
	}
}

// With the host's one capped connection held, each cluster's check, admin info and the sentinel's HEAD, reads the
// store, so both clusters are ready, and a probe's LIST of place/ and HEAD read it too, on connections of their own
// past the cap, none of them waiting in it; an unmarked LIST still waits in the cap, its deadline passing there
// unsent, and no host is marked.
func TestFileClusterChecksAndProbesReachTheStorePastAFullCap(t *testing.T) {
	store := newFakeS3(1, "build-1")
	clock := sidecartest.NewFakeClock(sidecartest.DefaultClockStart())
	clusters, host, releaseHolder := newFileClustersWithTheCapHeld(t, store, clock, "")
	meta := clusters.clusterIdClusters["meta-1"]

	requireFileClusterNoCappedWait(t, clock, func() {
		clusters.Check(t.Context())
	})
	for _, clusterId := range clusters.ClusterIds() {
		cluster := clusters.clusterIdClusters[clusterId]
		if cluster.LastAdminInfo() == nil || !cluster.Ready() {
			t.Errorf("%s: its check did not read the store past the full cap", clusterId)
		}
	}
	probe := sidecar.WithBlobProbe(t.Context())
	var objects []*sidecar.BlobInfo
	var listErr, headErr error
	requireFileClusterNoCappedWait(t, clock, func() {
		objects, _, listErr = meta.List(probe, sidecar.EvsPlaceKeyPrefix, "", 1)
		_, headErr = meta.Head(probe, fileClusterSentinelKey)
	})
	if listErr != nil || len(objects) != 1 {
		t.Fatalf("a probe's LIST past the full cap: %v, %d keys", listErr, len(objects))
	}
	if headErr != nil {
		t.Fatalf("a probe's HEAD past the full cap: %v", headErr)
	}

	unmarked := make(chan error, 1)
	go func() {
		_, _, err := meta.List(t.Context(), sidecar.EvsPlaceKeyPrefix, "", 1)
		unmarked <- err
	}()
	// the unmarked LIST's deadline, set as it starts to wait for the slot
	if err := clock.AwaitTimers(t.Context(), 1); err != nil {
		t.Fatal(err)
	}
	clock.Advance(meta.RequestTimeout())
	if err := <-unmarked; !errors.Is(err, errFileClusterConnWait) {
		t.Fatalf("an unmarked LIST with the cap full: %v", err)
	}
	if err := releaseHolder(); err != nil {
		t.Fatal(err)
	}
	if meta.picker.hosts[0].failedAtNanos.Load() != 0 {
		t.Error("a host was marked failed")
	}
	host.shutdown()

	// outside the cap a request keeps its deadline header, which the admin route never carries
	adminCount, sentinelCount, listCount := 0, 0, 0
	for _, request := range store.takeRequests() {
		switch {
		case request.path == fileClusterAdminInfoPath:
			adminCount += 1
			if request.deadline != "" {
				t.Errorf("admin info carried a deadline header %s", request.deadline)
			}
		case request.method == http.MethodHead && strings.HasSuffix(request.path, "/"+fileClusterSentinelKey):
			sentinelCount += 1
			if request.deadline == "" {
				t.Errorf("a sentinel HEAD outside the cap carried no deadline header")
			}
		case strings.Contains(request.query, "list-type=2"):
			listCount += 1
			if request.deadline == "" {
				t.Errorf("a probe's LIST outside the cap carried no deadline header")
			}
		}
	}
	// a check of each of the two clusters, then the probe's HEAD and LIST; never the unmarked LIST
	if adminCount != 2 || sentinelCount != 3 || listCount != 1 {
		t.Errorf("the store saw %d admin info reads, %d sentinel HEADs and %d LISTs, want 2, 3 and 1", adminCount, sentinelCount, listCount)
	}
	if takenCount, _ := host.counts(); takenCount <= 1 {
		t.Errorf("the host took %d connections, the checks and probes none past the held one", takenCount)
	}
}

// With the host's one capped connection held, a store that answers its checks keeps its clusters ready, writable and
// settled; once it resets every request but the held one, a check and a probe fail, and when the last good read
// goes stale at CheckStaleAfter the cluster is not ready, refuses a write before it is sent and reads not settled.
func TestFileClusterChecksPastAFullCapStillFailClosed(t *testing.T) {
	store := newFakeS3(1, "build-1")
	clock := sidecartest.NewFakeClock(sidecartest.DefaultClockStart())
	clusters, host, releaseHolder := newFileClustersWithTheCapHeld(t, store, clock, "sidecar:\n  file_clusters:\n    meta-1: {settle_passed_build: build-1}\n")
	meta := clusters.clusterIdClusters["meta-1"]

	requireFileClusterNoCappedWait(t, clock, func() {
		clusters.Check(t.Context())
	})
	if !meta.Ready() || meta.checkPools() != nil || !meta.SettlePassed() {
		t.Fatal("a cluster whose store answers its checks is not ready, writable and settled")
	}

	func() {
		store.stateLock.Lock()
		defer store.stateLock.Unlock()
		store.downHosts[fileClusterHeldHostName] = true
	}()
	var probeErr error
	requireFileClusterNoCappedWait(t, clock, func() {
		clusters.Check(t.Context())
		_, _, probeErr = meta.List(sidecar.WithBlobProbe(t.Context()), sidecar.EvsPlaceKeyPrefix, "", 1)
	})
	if probeErr == nil {
		t.Fatal("a probe of a store that resets it read as answered")
	}
	// the last good read stands until it goes stale
	if !meta.Ready() {
		t.Fatal("not ready before its last good read went stale")
	}
	clock.Advance(clusters.settings.CheckStaleAfter)
	if meta.Ready() || clusters.Ready() {
		t.Error("ready on checks gone stale")
	}
	if meta.SettlePassed() {
		t.Error("settled on checks gone stale")
	}
	store.takeRequests()
	if _, err := meta.PutIfNoneMatch(t.Context(), "head/a/_", []byte("v1"), nil); !errors.Is(err, ErrFileClusterPools) {
		t.Errorf("a write on checks gone stale: %v", err)
	}
	if err := releaseHolder(); err != nil {
		t.Fatal(err)
	}
	host.shutdown()
	for _, request := range store.takeRequests() {
		if request.method == http.MethodPut {
			t.Errorf("a write on checks gone stale reached the store: %s", request.path)
		}
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
