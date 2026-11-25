package server

// §11.15's fan-out (owner decision 780; QD-1394): a host's base transport caps the connections it opens there at
// FileClusterSettings.MaxConnsPerHost and keeps as many idle; one serves each host whichever clusters name it; and
// requests to a held host through both clusters of one FileClusters never open more than the cap to it, the
// requests past the cap waiting for a connection and all of them answered. A GET or PutStream at a client's own pace
// (QD-1402) goes through a second client of the host, whose base transport is the first's without the cap, and
// every other request through the capped one.

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/minio/minio-go/v7"

	"github.com/urnetwork/sidecar"
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
// once and in all. Safe for concurrent use.
type fileClusterHeldHost struct {
	server *httptest.Server
	// one value as each request reaches a handler
	arrived chan struct{}
	// one value answers one request at a handler
	release chan struct{}
	// closed at cleanup, answering every request
	stop chan struct{}

	stateLock    sync.Mutex
	openCount    int
	maxOpenCount int
	takenCount   int
}

func newFileClusterHeldHost(t *testing.T) *fileClusterHeldHost {
	t.Helper()
	host := &fileClusterHeldHost{arrived: make(chan struct{}, 64), release: make(chan struct{}), stop: make(chan struct{})}
	host.server = httptest.NewUnstartedServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		io.Copy(io.Discard, request.Body)
		host.arrived <- struct{}{}
		select {
		case <-host.release:
		case <-host.stop:
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
	t.Cleanup(func() {
		close(host.stop)
		host.server.Close()
	})
	return host
}

// The connections the host took in all, and the most open at once.
func (self *fileClusterHeldHost) counts() (takenCount int, maxOpenCount int) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.takenCount, self.maxOpenCount
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
// other request, and a request with no body on such a ctx, the capped one.
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
