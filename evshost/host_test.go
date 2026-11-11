package evshost

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"
	"gopkg.in/yaml.v3"

	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/evsservice"
	"github.com/urnetwork/sidecar/protocol"
	"github.com/urnetwork/sidecar/sidecartest"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/router"
	"github.com/urnetwork/server/sidecarhost"
)

// How long a step that waits on another goroutine may take before the test fails.
const hostTestStepTimeout = 30 * time.Second

// The block's status port as warp names it, which WARP_PORTS maps to a loopback port of the test's.
const hostTestStatusPort = 80

// What a test says the newest verified state holds: every binding and scope, and no repo created.
type hostTestAuthState struct{}

func (hostTestAuthState) CheckRevocation(ctx context.Context, principal *sidecar.Principal) error {
	return nil
}

func (hostTestAuthState) MatchBinding(principal *sidecar.Principal) evsservice.EvsAuthAnswer {
	return evsservice.EvsAuthAllowed
}

func (hostTestAuthState) HonorScope(principal *sidecar.Principal, repoId sidecar.Id, rights sidecar.AclEvsRights, now time.Time) evsservice.EvsAuthAnswer {
	return evsservice.EvsAuthAllowed
}

func (hostTestAuthState) Repo(principal *sidecar.Principal, repoId sidecar.Id) (*evsservice.EvsRepoFacts, evsservice.EvsAuthAnswer) {
	return nil, evsservice.EvsAuthDenied
}

// A gate view source with no network loaded: every push answers 503, as a process whose cache has not loaded.
type hostTestGateViews struct{}

func (hostTestGateViews) View(networkId sidecar.Id) (sidecar.GkmAuthView, evsservice.EvsAuthAnswer) {
	return nil, evsservice.EvsAuthUnknown
}

// One synthetic receipt key, issuing from an hour before start.
func hostTestReceiptKeys(t testing.TB, start time.Time) *evsservice.ReceiptKeys {
	t.Helper()
	keys, err := evsservice.NewReceiptKeys([]*evsservice.ReceiptKey{
		{KeyId: 1, Key: bytes.Repeat([]byte{0x5a}, 32), IssueFrom: start.Add(-time.Hour)},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	return keys
}

// The fake clusters, whose sentinel the test says is read or not, as server.FileClusters.Ready answers.
type hostTestClusters struct {
	*sidecartest.FakeBlobs
	ready  atomic.Bool
	closed atomic.Bool
}

func newHostTestClusters(clock sidecartest.Clock) *hostTestClusters {
	return &hostTestClusters{FakeBlobs: sidecartest.NewFakeBlobs(clock, nil, nil, nil)}
}

func (self *hostTestClusters) Ready() bool {
	return self.ready.Load()
}

func (self *hostTestClusters) Close() {
	self.closed.Store(true)
}

// Clusters holding no segment cluster, as a vault file naming none would give.
type hostTestNoSegmentBlobs struct {
	sidecar.BlobStore
}

func (self hostTestNoSegmentBlobs) SegmentCluster() (sidecar.BlobCluster, error) {
	return nil, sidecar.ErrBlobClusterUnknown
}

// Waits for the engine's first probe of the first cluster to finish.
func awaitHostTestProbe(t testing.TB, engine *evsservice.EvsEngine) {
	t.Helper()
	deadline := time.After(hostTestStepTimeout)
	for {
		count, changed := engine.Probes()
		if 1 <= count {
			return
		}
		select {
		case <-changed:
		case <-deadline:
			t.Fatal("the engine never probed its first cluster")
		}
	}
}

// A host on a fake ServiceHost with every seam given, its first probe done.
func newHostTestHost(t testing.TB, adjust func(host *sidecar.ServiceHost, deps *HostDeps)) *Host {
	t.Helper()
	clock := sidecartest.NewFakeClock(sidecartest.DefaultClockStart())
	host := sidecartest.NewFakeHost(t.Context(), clock, nil, nil, nil).ServiceHost()
	deps := &HostDeps{
		AuthState:   hostTestAuthState{},
		GateViews:   hostTestGateViews{},
		ReceiptKeys: hostTestReceiptKeys(t, clock.Now()),
	}
	if adjust != nil {
		adjust(host, deps)
	}
	evsHost, err := NewHost(t.Context(), host, nil, deps)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(evsHost.Close)
	awaitHostTestProbe(t, evsHost.Engine())
	return evsHost
}

// A write budget whose at_limit flag reads set on one cluster, as the budget loop sets it at the cohort limit.
type hostTestFullBudget struct {
	sidecar.WriteBudget
	fullClusterId string
}

func (self hostTestFullBudget) AtLimit(clusterId string) bool {
	return clusterId == self.fullClusterId
}

// The host holds its uploads to the cohort limit over the process's write budget (EVSB-M3-02): a cluster whose flag
// reads set is full and no other is, and a budget that keeps no flag is never full.
func TestHostHoldsUploadsToTheCohortLimit(t *testing.T) {
	full := newHostTestHost(t, func(host *sidecar.ServiceHost, deps *HostDeps) {
		host.Budget = hostTestFullBudget{WriteBudget: host.Budget, fullClusterId: "meta-1"}
	})
	if !full.clusterLimit.Full("meta-1") || full.clusterLimit.Full("meta-2") {
		t.Fatal("the cohort limit does not read the budget's flag per cluster")
	}
	if refusal := full.clusterLimit.UploadRefusal(); refusal.GetCode() != protocol.EvsRefusalCode_EVS_REFUSAL_CLUSTER_FULL || refusal.GetRetryAfterMs() == 0 {
		t.Fatalf("refusal %v", refusal)
	}
	plain := newHostTestHost(t, nil)
	if plain.clusterLimit == nil || plain.clusterLimit.Full("meta-1") {
		t.Fatal("a budget that keeps no flag reads full, or the host built no limit")
	}
}

// Every seam given and the first cluster answering: ready, with nothing missing.
func TestHostReadyWithEverySeam(t *testing.T) {
	evsHost := newHostTestHost(t, nil)
	if missing := evsHost.Missing(); len(missing) != 0 {
		t.Fatalf("missing %v", missing)
	}
	if !evsHost.Ready() {
		t.Fatal("not ready with every seam and the first cluster answering")
	}
}

// Each seam server has no implementation of yet keeps the block not ready, and says which: the quota and the
// authorization state; so do the receipt keys, which a block reads from its vault, and a segment cluster, without
// which every upload would answer 503.
func TestHostNotReadyWithoutEachSeam(t *testing.T) {
	t.Setenv("WARP_VAULT_HOME", filepath.Join(t.TempDir(), "vault"))
	cases := []struct {
		name   string
		adjust func(host *sidecar.ServiceHost, deps *HostDeps)
		want   string
	}{
		{"quota", func(host *sidecar.ServiceHost, deps *HostDeps) { host.Quota = nil }, "QUOTA-M2-11"},
		{"auth state", func(host *sidecar.ServiceHost, deps *HostDeps) { deps.AuthState = nil }, "ACL-M2-14"},
		{"gate views", func(host *sidecar.ServiceHost, deps *HostDeps) { deps.GateViews = nil }, "NewAclGateViews"},
		{"receipt keys", func(host *sidecar.ServiceHost, deps *HostDeps) { deps.ReceiptKeys = nil }, HostReceiptKeysFileName},
		{"segment cluster", func(host *sidecar.ServiceHost, deps *HostDeps) {
			host.Blobs = hostTestNoSegmentBlobs{BlobStore: host.Blobs}
		}, "segment cluster"},
	}
	for _, c := range cases {
		evsHost := newHostTestHost(t, c.adjust)
		missing := evsHost.Missing()
		if len(missing) != 1 || !strings.Contains(missing[0], c.want) {
			t.Errorf("%s: missing %v, want one naming %s", c.name, missing, c.want)
		}
		if evsHost.Ready() {
			t.Errorf("%s: ready without it", c.name)
		}
		// the engine still answers through its guard, so a request without a token is refused, never served
		recorder := hostTestServe(evsHost.Handler(), http.MethodPut, "/evs/v1/repos/"+sidecar.EvsKeyId(sidecar.Id{1})+"/segments/"+strings.Repeat("ab", 32))
		if recorder.Code != http.StatusUnauthorized {
			t.Errorf("%s: an upload with no token answered %d", c.name, recorder.Code)
		}
	}
}

// A block's receipt keys come from its vault's file, as the segment route's MAC keys (§11.13's keys row).
func TestHostReadsItsReceiptKeysFromItsVault(t *testing.T) {
	vault := filepath.Join(t.TempDir(), "vault")
	if err := os.MkdirAll(vault, 0o700); err != nil {
		t.Fatal(err)
	}
	keyText := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{0x5a}, 32))
	start := sidecartest.DefaultClockStart()
	file := fmt.Sprintf(`{"keys": [{"key_id": 1, "key": %q, "issue_from_ms": %d}]}`, keyText, start.Add(-time.Hour).UnixMilli())
	if err := os.WriteFile(filepath.Join(vault, HostReceiptKeysFileName), []byte(file), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("WARP_VAULT_HOME", vault)
	evsHost := newHostTestHost(t, func(host *sidecar.ServiceHost, deps *HostDeps) {
		deps.ReceiptKeys = nil
	})
	if missing := evsHost.Missing(); len(missing) != 0 {
		t.Fatalf("missing %v with the keys in the vault", missing)
	}
	if !evsHost.Ready() {
		t.Fatal("not ready with the keys in the vault")
	}
}

// A Quota that says whether it has read its over-limit list, as quotahost.HostQuota does.
type hostTestReadyQuota struct {
	sidecar.Quota
	ready *atomic.Bool
}

func (self hostTestReadyQuota) Ready() error {
	if !self.ready.Load() {
		return errors.New("no over-limit list read yet")
	}
	return nil
}

// A Quota that reads its over-limit list from api keeps the block not ready until it has read one, so a block
// never serves on an empty list it did not read (QUOTA-M2-11; plan-SOCK-gate-data).
func TestHostWaitsForItsQuotasFirstList(t *testing.T) {
	var ready atomic.Bool
	evsHost := newHostTestHost(t, func(host *sidecar.ServiceHost, deps *HostDeps) {
		host.Quota = hostTestReadyQuota{Quota: host.Quota, ready: &ready}
	})
	if missing := evsHost.Missing(); len(missing) != 0 {
		t.Fatalf("missing %v", missing)
	}
	if evsHost.Ready() {
		t.Fatal("ready before the Quota read its first list")
	}
	ready.Store(true)
	if !evsHost.Ready() {
		t.Fatal("not ready once the Quota read its list")
	}
}

// §19.4: ready also needs each cluster's sentinel, whatever the engine's own probe found.
func TestHostWaitsForTheClustersSentinel(t *testing.T) {
	var sentinel atomic.Bool
	evsHost := newHostTestHost(t, func(host *sidecar.ServiceHost, deps *HostDeps) {
		deps.ClustersReady = sentinel.Load
	})
	if evsHost.Ready() {
		t.Fatal("ready before every cluster's sentinel read")
	}
	sentinel.Store(true)
	if !evsHost.Ready() {
		t.Fatal("not ready once the sentinels read")
	}
	evsHost.Close()
	if evsHost.Ready() {
		t.Fatal("ready after Close")
	}
}

// A host with no clusters has nothing to serve from, and says so rather than start.
func TestHostRefusesAHostWithNoClusters(t *testing.T) {
	host := sidecartest.NewFakeHost(t.Context(), nil, nil, nil, nil).ServiceHost()
	host.Blobs = nil
	if _, err := NewHost(t.Context(), host, nil, &HostDeps{AuthState: hostTestAuthState{}}); err == nil {
		t.Fatal("a host with no clusters started")
	}
}

// A block whose vault holds no file-clusters-evs.yml cannot start: its engine is never ready and serves no
// route, so /status says not ready and a deploy reverts.
func TestBlockWithNoClustersFileIsNeverReady(t *testing.T) {
	home := t.TempDir()
	t.Setenv("WARP_VAULT_HOME", filepath.Join(home, "vault"))
	t.Setenv("WARP_CONFIG_HOME", filepath.Join(home, "config"))
	engine := newBlockEngine(t.Context(), nil, &HostDeps{AuthState: hostTestAuthState{}})
	defer engine.Close()
	if _, ok := engine.(notReadyEngine); !ok {
		t.Fatalf("a block with no clusters file built %T", engine)
	}
	options := blockRunOptions(engine, hostTestStatusPort, &HostDeps{})
	if options.Http != nil {
		t.Error("a block that cannot start serves routes")
	}
	if err := options.Validate(); err != nil {
		t.Error(err)
	}
}

// R-EVS-15: the block holds no Postgres pool and reads no pending group's tables, so sidecarhost.Run never
// reads migration_audit for it.
func TestBlockHoldsNoPostgres(t *testing.T) {
	evsHost := newHostTestHost(t, nil)
	options := blockRunOptions(evsHost, hostTestStatusPort, &HostDeps{})
	if options.Postgres || 0 < len(options.Groups) {
		t.Errorf("the block holds Postgres %t with groups %v", options.Postgres, options.Groups)
	}
	if options.Service != "evs" || options.Http == nil || options.Stream != nil {
		t.Errorf("options %+v", options)
	}
	if err := options.Validate(); err != nil {
		t.Error(err)
	}
}

// What one request with no token gets from handler.
func hostTestServe(handler http.Handler, method string, path string) *httptest.ResponseRecorder {
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(method, "http://evs.example"+path, nil))
	return recorder
}

// A loopback port free a moment ago, for WARP_PORTS to map the block's status port to.
func hostTestFreePort(t testing.TB) int {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	return listener.Addr().(*net.TCPAddr).Port
}

// Waits until the block's status port accepts. The bind is the operating system's, with no hook to wait on, so
// this dials until it is there; the test's outcome does not rest on how long that takes.
func awaitHostTestListening(t testing.TB, address string) {
	t.Helper()
	deadline := time.After(hostTestStepTimeout)
	for {
		conn, err := net.Dial("tcp", address)
		if err == nil {
			conn.Close()
			return
		}
		select {
		case <-deadline:
			t.Fatalf("the status port never bound: %v", err)
		case <-time.After(5 * time.Millisecond):
		}
	}
}

// GET /status's status over the block's port.
func hostTestStatus(t testing.TB, base string) string {
	t.Helper()
	response, err := http.Get(base + "/status")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("/status answered %d", response.StatusCode)
	}
	result := &router.WarpStatusResult{}
	if err := json.NewDecoder(response.Body).Decode(result); err != nil {
		t.Fatal(err)
	}
	return result.Status
}

// Metrics whose ready gauge says each value it is set to, so a test waits for the block's latch.
type hostTestMetrics struct {
	sidecar.Metrics
	readyName string
	ready     chan float64
}

func (self *hostTestMetrics) Gauge(name string, help string, labelNames ...string) sidecar.MetricsGauge {
	gauge := self.Metrics.Gauge(name, help, labelNames...)
	if name != self.readyName {
		return gauge
	}
	return &hostTestGauge{inner: gauge, values: self.ready}
}

type hostTestGauge struct {
	inner  sidecar.MetricsGauge
	values chan float64
}

func (self *hostTestGauge) Set(value float64, labelValues ...string) {
	self.inner.Set(value, labelValues...)
	self.values <- value
}

// The block on the private stores (.stack/server-test.sh): sidecarhost.Run on a loopback port, the host's Redis
// server's pool on the test store, which the write budget, the HDD share and the hint writer use, and the clusters
// faked. /status answers from the first moment and says not ready while a cluster's sentinel is unread, the
// engine's routes wait, then the block latches ready and evs's guard answers its routes, and ctx's end drains and
// closes it.
func TestRunHostServesStatusOnThePrivateStores(t *testing.T) {
	port := hostTestFreePort(t)
	t.Setenv("WARP_HOST_IPV4", "127.0.0.1")
	t.Setenv("WARP_PORTS", fmt.Sprintf("%d:%d", hostTestStatusPort, port))
	(&server.TestEnv{ApplyDbMigrations: false}).Run(t, func(tb testing.TB) {
		ctx, cancel := context.WithCancel(tb.Context())
		defer cancel()
		engineClock := sidecartest.NewFakeClock(sidecartest.DefaultClockStart())
		// Run's own, so its readiness poll is the only timer on it before the latch
		runClock := sidecartest.NewFakeClock(sidecartest.DefaultClockStart())
		clusters := newHostTestClusters(engineClock)
		deps := &HostDeps{
			AuthState:   hostTestAuthState{},
			GateViews:   hostTestGateViews{},
			ReceiptKeys: hostTestReceiptKeys(tb, engineClock.Now()),
			Clusters:    clusters,
			ClockCheck:  sidecartest.NewFakeClockCheck(nil),
			Quota:       sidecartest.NewFakeQuota(engineClock, nil, nil),
			Clock:       engineClock,
			WarpStatus:  &router.WarpStatusState{},
		}
		engine := newBlockEngine(ctx, nil, deps)
		evsHost, ok := engine.(*Host)
		if !ok {
			tb.Fatalf("the block did not start: %T", engine)
		}
		awaitHostTestProbe(tb, evsHost.Engine())
		if evsHost.Ready() {
			tb.Fatal("ready before every cluster's sentinel read")
		}
		metrics := &hostTestMetrics{
			Metrics:   sidecartest.NewFakeMetrics(nil),
			readyName: "urnetwork_evs_ready",
			ready:     make(chan float64, 8),
		}
		options := blockRunOptions(engine, hostTestStatusPort, deps)
		options.Clock = runClock
		options.Metrics = metrics
		// the drain's keepalive grace is the lb's, and no lb holds this test's connections
		options.Settings = sidecarhost.DefaultRunSettings()
		options.Settings.HttpServer.KeepaliveDrainTimeout = 0
		done := make(chan error, 1)
		go func() {
			done <- sidecarhost.Run(ctx, options)
		}()
		address := fmt.Sprintf("127.0.0.1:%d", port)
		base := "http://" + address
		awaitHostTestListening(tb, address)

		if status := hostTestStatus(tb, base); !strings.HasPrefix(status, "error not ready") {
			tb.Fatalf("/status %q before the sentinel read", status)
		}
		repoPath := "/evs/v1/repos/" + sidecar.EvsKeyId(sidecar.Id{7})
		request, err := http.NewRequest(http.MethodPut, base+repoPath, nil)
		if err != nil {
			tb.Fatal(err)
		}
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			tb.Fatal(err)
		}
		io.Copy(io.Discard, response.Body)
		response.Body.Close()
		if response.StatusCode != http.StatusServiceUnavailable || response.Header.Get("Retry-After") == "" {
			tb.Fatalf("a route before readiness answered %d", response.StatusCode)
		}

		// the sentinel reads, and Run's next poll finds the block ready
		clusters.ready.Store(true)
		if err := runClock.AwaitTimers(ctx, 1); err != nil {
			tb.Fatal(err)
		}
		runClock.Advance(sidecarhost.DefaultRunSettings().ReadyPollInterval)
		awaitReady := time.After(hostTestStepTimeout)
		for latched := false; !latched; {
			select {
			case value := <-metrics.ready:
				latched = value == 1
			case <-awaitReady:
				tb.Fatal("the block never latched ready")
			}
		}
		if status := hostTestStatus(tb, base); status != "ok" {
			tb.Fatalf("/status %q once ready", status)
		}
		// evs's guard answers its route: no token is a 401 with an EvsRefusalReply body
		response, err = http.DefaultClient.Do(request)
		if err != nil {
			tb.Fatal(err)
		}
		body, err := io.ReadAll(response.Body)
		response.Body.Close()
		if err != nil {
			tb.Fatal(err)
		}
		if response.StatusCode != http.StatusUnauthorized || response.Header.Get("Content-Type") != "application/x-protobuf" {
			tb.Fatalf("a route with no token answered %d %s", response.StatusCode, response.Header.Get("Content-Type"))
		}
		if err := proto.Unmarshal(body, &protocol.EvsRefusalReply{}); err != nil {
			tb.Fatalf("a refusal that does not parse: %v", err)
		}

		// the block's SIGTERM: drained, the engine closed, then what it used
		cancel()
		select {
		case err := <-done:
			if err != nil {
				tb.Fatal(err)
			}
		case <-time.After(hostTestStepTimeout):
			tb.Fatal("Run did not return")
		}
		if evsHost.Ready() || !clusters.closed.Load() {
			tb.Fatalf("after the drain: ready %t, clusters closed %t", evsHost.Ready(), clusters.closed.Load())
		}
	})
}

// The operations connect/api/bringyour.yml documents on evs's host: "METHOD /path" with each parameter as {},
// and whether each is still planned. Every /evs/ path names evs's host as its own server, one host for all of
// them, which is none of the document's: api serves no evs route.
func evsSpecOperations(t testing.TB) map[string]bool {
	t.Helper()
	// go test runs in the package's directory, and connect sits beside server
	data, err := os.ReadFile(filepath.Join("..", "..", "connect", "api", "bringyour.yml"))
	if err != nil {
		t.Fatal(err)
	}
	spec := &struct {
		Servers []struct {
			Url string `yaml:"url"`
		} `yaml:"servers"`
		Paths map[string]map[string]yaml.Node `yaml:"paths"`
	}{}
	if err := yaml.Unmarshal(data, spec); err != nil {
		t.Fatal(err)
	}
	apiHosts := map[string]bool{}
	for _, server := range spec.Servers {
		apiHosts[server.Url] = true
	}
	evsHosts := map[string]bool{}
	operations := map[string]bool{}
	for path, item := range spec.Paths {
		if !strings.HasPrefix(path, "/evs/") {
			continue
		}
		serversNode, ok := item["servers"]
		servers := []struct {
			Url string `yaml:"url"`
		}{}
		if ok {
			if err := serversNode.Decode(&servers); err != nil {
				t.Fatal(err)
			}
		}
		if len(servers) != 1 {
			t.Errorf("%s names %d servers of its own, not evs's one", path, len(servers))
			continue
		}
		evsHosts[servers[0].Url] = true
		for method, node := range item {
			switch method {
			case "get", "put", "post", "delete", "patch":
			default:
				continue
			}
			operation := &struct {
				Status string `yaml:"x-status"`
			}{}
			if err := node.Decode(operation); err != nil {
				t.Fatal(err)
			}
			operations[strings.ToUpper(method)+" "+evsSpecPathKey(path)] = operation.Status == "planned"
		}
	}
	if len(evsHosts) != 1 {
		t.Errorf("evs's paths name hosts %v, not one", evsHosts)
	}
	for host := range evsHosts {
		if apiHosts[host] {
			t.Errorf("evs's host %s is one of api's", host)
		}
	}
	return operations
}

// A path with each {parameter} as {}, so the engine's pattern and the spec's path compare whatever they name
// their parameters.
func evsSpecPathKey(path string) string {
	return evsSpecParameter.ReplaceAllString(path, "{}")
}

var evsSpecParameter = regexp.MustCompile(`\{[^/}]+\}`)

// The engine's routes against connect/api/bringyour.yml (connect-m1-c11-route-scope): every route the block
// serves is documented on evs's host with its planned mark lifted, as the task that serves a route lifts it, and
// every documented evs operation no longer planned is one the engine serves. check-routes.sh leaves these paths
// to this test, since they name a server of their own.
func TestEngineRoutesAreBringyourYmlsEvsOperations(t *testing.T) {
	evsHost := newHostTestHost(t, nil)
	operations := evsSpecOperations(t)
	if len(operations) == 0 {
		t.Fatal("bringyour.yml documents no evs operation")
	}
	served := map[string]bool{}
	for _, route := range evsHost.Engine().Routes() {
		key := route.Method + " " + evsSpecPathKey(route.Pattern)
		served[key] = true
		planned, documented := operations[key]
		switch {
		case !documented:
			t.Errorf("the engine serves %s, which bringyour.yml does not document on evs's host", key)
		case planned:
			t.Errorf("the engine serves %s, which bringyour.yml still marks planned", key)
		}
	}
	for key, planned := range operations {
		if !planned && !served[key] {
			t.Errorf("bringyour.yml documents %s as served, and the engine has no such route", key)
		}
	}
	// the route §11.20 lacked, whose shape plan-EVS1-repo-activate-route settled, is served (EVS1-M2-19)
	if planned, documented := operations["POST /evs/v1/repos/{}/activate"]; !documented || planned {
		t.Errorf("POST /evs/v1/repos/{}/activate documented %t, planned %t", documented, planned)
	}
}
