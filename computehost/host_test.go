package computehost

// The compute block (CSRV-M12-03; D-CSRV-1, DESIGN-COMPUTE §14.2): run on Postgres with MG-C1 and MG-C2 as its groups,
// ready only once this binary links them, every route closed by §19.11's compute gate as each env's sidecar.yml writes
// it, and a host built with every credential given reading no vault file. The full block on the private stores is
// TestComputeBlockServesOnThePrivateStores (.stack/server-test.sh, the sidecar_pending leg).

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/computeservice"
	"github.com/urnetwork/sidecar/sidecartest"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/router"
	"github.com/urnetwork/server/sidecarhost"
)

// The state a test says the network holds: the token's sidecar admitted in it, no feature on.
type hostTestAuthState struct{}

func (hostTestAuthState) CheckRevocation(ctx context.Context, principal *sidecar.Principal) error {
	return nil
}

func (hostTestAuthState) Caller(principal *sidecar.Principal) (*computeservice.ComputeCaller, computeservice.ComputeAuthAnswer) {
	return &computeservice.ComputeCaller{
		Principal: principal,
		NetworkId: sidecar.Id(principal.NetworkId),
		ClientId:  sidecar.Id(principal.ClientId),
		SidecarId: sidecar.Id(*principal.SidecarId),
	}, computeservice.ComputeAuthAllowed
}

func (hostTestAuthState) Features(networkId sidecar.Id) (computeservice.ComputeAuthFeatures, computeservice.ComputeAuthAnswer) {
	return 0, computeservice.ComputeAuthAllowed
}

// The block's options for sidecarhost.Run: the compute service on Postgres, its groups MG-C1 and MG-C2 by server's
// markers, its routes on the status port.
func TestComputeBlockRunsOnPostgresWithItsGroups(t *testing.T) {
	host, err := NewHost(t.Context(), sidecartest.NewFakeHost(t.Context(), nil, nil, nil, nil).ServiceHost(), nil, &HostDeps{AuthState: hostTestAuthState{}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(host.Close)
	options := blockRunOptions(host, 80, &HostDeps{})
	if options.Service != "compute" || !options.Postgres || options.Http == nil || options.Stream != nil {
		t.Fatalf("options %+v", options)
	}
	if len(options.Groups) != 2 || options.Groups[0].Name != "MG-C1" || options.Groups[1].Name != "MG-C2" ||
		options.Groups[0].Linked() != server.SidecarMgC1Linked() || options.Groups[1].Linked() != server.SidecarMgC2Linked() {
		t.Fatalf("the block's groups are %+v", options.Groups)
	}
	if err := options.Validate(); err != nil {
		t.Fatal(err)
	}
}

// The block is ready only in a binary that links MG-C1 and MG-C2: the sidecar_pending leg, as local and canary build
// it, and production once M12's Gn-out registers them; in any other its host names each group it waits for, as
// /status's log says, and so does sidecarhost.Run's readiness.
func TestComputeBlockReadyAfterItsGroups(t *testing.T) {
	host, err := NewHost(t.Context(), sidecartest.NewFakeHost(t.Context(), nil, nil, nil, nil).ServiceHost(), nil, &HostDeps{AuthState: hostTestAuthState{}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(host.Close)
	linked := server.SidecarMgC1Linked() && server.SidecarMgC2Linked()
	if host.Ready() != linked {
		t.Fatalf("ready %v in a binary linking the groups %v: %v", host.Ready(), linked, host.Missing())
	}
	missing := strings.Join(host.Missing(), "; ")
	if linked != !strings.Contains(missing, "MG-C1's tables") || linked != !strings.Contains(missing, "MG-C2's tables") {
		t.Fatalf("the host waits for %q", missing)
	}
	// MG-C3 is the enclave routes' alone, never the block's
	if strings.Contains(missing, "MG-C3") {
		t.Fatalf("the block waits for MG-C3: %q", missing)
	}
	// a host told the groups are linked is ready on every leg, and one with no auth state never is
	allLinked := func(group computeservice.ComputeMigrationGroup) bool { return true }
	linkedHost, err := NewHost(t.Context(), sidecartest.NewFakeHost(t.Context(), nil, nil, nil, nil).ServiceHost(), nil, &HostDeps{AuthState: hostTestAuthState{}, GroupLinked: allLinked})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(linkedHost.Close)
	stateless, err := NewHost(t.Context(), sidecartest.NewFakeHost(t.Context(), nil, nil, nil, nil).ServiceHost(), nil, &HostDeps{GroupLinked: allLinked})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(stateless.Close)
	if !linkedHost.Ready() || stateless.Ready() {
		t.Fatalf("with the groups linked: ready %v; with no auth state: ready %v", linkedHost.Ready(), stateless.Ready())
	}
}

// §19.11's compute gate as each env's sidecar.yml writes it, and as a file with no compute line leaves it, answers
// every route of a running engine 503 with the reread interval's Retry-After, the engine staying ready; opened to the
// network, the same request reaches the guard's last step.
func TestComputeGateClosedByDefault(t *testing.T) {
	configText, err := os.ReadFile(filepath.Join("..", "..", "config", "main", "sidecar.yml"))
	if err != nil {
		t.Skipf("no config checkout beside the server: %v", err)
	}
	networkId := connect.NewId()
	clock := sidecartest.NewFakeClock(sidecartest.DefaultClockStart())
	fakeHost := sidecartest.NewFakeHost(t.Context(), clock, nil, nil, nil)
	serviceHost := fakeHost.ServiceHost()
	gatesCtx, cancelGates := context.WithCancel(t.Context())
	defer cancelGates()
	sidecarId := connect.NewId()
	token := fakeHost.Authenticator.Issue(&sidecar.Principal{
		NetworkId: networkId, ClientId: connect.NewId(), SidecarId: &sidecarId, Audience: computeservice.ComputeAudience,
		IssuedAt: clock.Now(), ExpiresAt: clock.Now().Add(15 * time.Minute),
	})
	allLinked := func(group computeservice.ComputeMigrationGroup) bool { return true }
	for _, text := range []string{"", string(configText), "sidecar:\n  compute: {enabled: true, networks: [" + networkId.String() + "], percent: 0}\n"} {
		pop := server.Testing_PushSidecarConfig(text)
		serviceHost.Gates = sidecarhost.NewGates(gatesCtx, nil)
		host, err := NewHost(t.Context(), serviceHost, nil, &HostDeps{AuthState: hostTestAuthState{}, GroupLinked: allLinked})
		if err != nil {
			pop()
			t.Fatal(err)
		}
		request := httptest.NewRequest(http.MethodGet, "http://compute.example/v1/catalog", nil)
		request.Header.Set("Authorization", "Bearer "+token)
		recorder := httptest.NewRecorder()
		host.Handler().ServeHTTP(recorder, request)
		opened := strings.Contains(text, "enabled: true")
		switch {
		case !host.Ready():
			t.Errorf("%q: the engine is not ready", text)
		case opened && (recorder.Code != http.StatusServiceUnavailable || !strings.Contains(recorder.Body.String(), "not_built")):
			t.Errorf("%q: an opened gate answered %d %s", text, recorder.Code, recorder.Body.String())
		case !opened && (recorder.Code != http.StatusServiceUnavailable || recorder.Header().Get("Retry-After") != "60" ||
			!strings.Contains(recorder.Body.String(), `"code":"unavailable"`)):
			t.Errorf("%q: a closed gate answered %d %s, Retry-After %q", text, recorder.Code, recorder.Body.String(), recorder.Header().Get("Retry-After"))
		}
		host.Close()
		pop()
	}
}

// A host given every credential reads no vault file: with an empty vault it starts, and is ready where its groups are
// linked. The areas' file reader takes the areas' files alone, from the block's vault.
func TestComputeHostReadsNoVaultInTests(t *testing.T) {
	vault := filepath.Join(t.TempDir(), "vault")
	if err := os.MkdirAll(vault, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("WARP_VAULT_HOME", vault)
	clock := sidecartest.NewFakeClock(sidecartest.DefaultClockStart())
	clusters := &hostTestClusters{FakeBlobs: sidecartest.NewFakeBlobs(clock, nil, nil, nil)}
	engine := newBlockEngine(t.Context(), DefaultHostSettings(), &HostDeps{
		AuthState:   hostTestAuthState{},
		Clusters:    clusters,
		Clock:       clock,
		GroupLinked: func(group computeservice.ComputeMigrationGroup) bool { return true },
		SecretFile:  func(name string) ([]byte, error) { return []byte("throwaway " + name), nil },
	})
	computeHost, ok := engine.(*Host)
	if !ok {
		t.Fatalf("a host with every credential given did not start: %T", engine)
	}
	if !computeHost.Ready() {
		t.Fatalf("not ready: %v", computeHost.Missing())
	}
	if data, err := computeHost.Engine().SecretFile(HostProvidersFileName); err != nil || string(data) != "throwaway "+HostProvidersFileName {
		t.Fatalf("the areas' file read %q, %v", data, err)
	}
	computeHost.Close()
	if !clusters.closed {
		t.Error("Close left the clusters open")
	}

	if err := os.WriteFile(filepath.Join(vault, HostProvidersFileName), []byte("synthetic: providers\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if data, err := vaultSecretFile(HostProvidersFileName); err != nil || string(data) != "synthetic: providers\n" {
		t.Fatalf("the providers' file read %q, %v", data, err)
	}
	for _, name := range []string{"pg.yml", "redis.yml", HostAdmissionsRedisFileName, "oauth-jwks-svc.json"} {
		if _, err := vaultSecretFile(name); err == nil {
			t.Errorf("%s was read for an area", name)
		}
	}
	if _, err := vaultSecretFile(HostGammaSignerFileName); err == nil || strings.Contains(err.Error(), "synthetic") {
		t.Errorf("a file the vault does not hold: %v", err)
	}
}

// Clusters a test holds, recording their end.
type hostTestClusters struct {
	*sidecartest.FakeBlobs
	closed bool
}

func (self *hostTestClusters) Close() {
	self.closed = true
}

// How long a step that waits on another goroutine may take before the test fails.
const hostTestStepTimeout = 30 * time.Second

// Metrics whose ready gauge says each value it is set to, so a test waits for the block's latch.
type hostTestMetrics struct {
	sidecar.Metrics
	ready chan float64
}

func (self *hostTestMetrics) Gauge(name string, help string, labelNames ...string) sidecar.MetricsGauge {
	gauge := self.Metrics.Gauge(name, help, labelNames...)
	if name != "urnetwork_compute_ready" {
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

// The block on the private stores (.stack/server-test.sh, the sidecar_pending leg, where this binary links MG-C1 and
// MG-C2): sidecarhost.Run on a loopback listener with the database at its head, the gate cache following {acl_}v on
// the test store's Redis over faked clusters. It latches ready, /status says ok, the guard answers a route with no
// token 401 unauthenticated as JSON, and ctx's end drains and closes the block and what it built.
func TestComputeBlockServesOnThePrivateStores(t *testing.T) {
	if !server.SidecarMgC1Linked() || !server.SidecarMgC2Linked() {
		t.Skip("this binary links neither MG-C1 nor MG-C2: TestComputeBlockReadyAfterItsGroups holds the block unready")
	}
	server.DefaultTestEnv().Run(t, func(tb testing.TB) {
		ctx, cancel := context.WithCancel(tb.Context())
		defer cancel()
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			tb.Fatal(err)
		}
		base := "http://" + listener.Addr().String()
		clock := sidecartest.NewFakeClock(sidecartest.DefaultClockStart())
		clusters := &hostTestClusters{FakeBlobs: sidecartest.NewFakeBlobs(clock, nil, nil, nil)}
		deps := &HostDeps{Clusters: clusters, Clock: clock, StatusListener: listener, WarpStatus: &router.WarpStatusState{}}
		engine := newBlockEngine(ctx, DefaultHostSettings(), deps)
		computeHost, ok := engine.(*Host)
		if !ok {
			tb.Fatalf("the block did not start: %T", engine)
		}
		metrics := &hostTestMetrics{Metrics: sidecartest.NewFakeMetrics(nil), ready: make(chan float64, 8)}
		options := blockRunOptions(engine, 0, deps)
		options.Metrics = metrics
		options.Settings = sidecarhost.DefaultRunSettings()
		// the drain's keepalive grace is the lb's, and no lb holds this test's connections
		options.Settings.HttpServer.KeepaliveDrainTimeout = 0
		done := make(chan error, 1)
		go func() {
			done <- sidecarhost.Run(ctx, options)
		}()
		awaitReady := time.After(hostTestStepTimeout)
		for latched := false; !latched; {
			select {
			case value := <-metrics.ready:
				latched = value == 1
			case <-awaitReady:
				tb.Fatalf("the block never latched ready: %s", deps.WarpStatus.Status())
			}
		}
		if status := deps.WarpStatus.Status(); status != "ok" {
			tb.Fatalf("/status %q once ready", status)
		}
		response, err := http.Get(base + "/v1/catalog")
		if err != nil {
			tb.Fatal(err)
		}
		body, err := io.ReadAll(response.Body)
		response.Body.Close()
		if err != nil {
			tb.Fatal(err)
		}
		refusal := &struct {
			Code string `json:"code"`
		}{}
		if response.StatusCode != http.StatusUnauthorized || json.Unmarshal(body, refusal) != nil || refusal.Code != "unauthenticated" {
			tb.Fatalf("a route with no token answered %d %s", response.StatusCode, body)
		}
		// the block's SIGTERM: drained, the engine closed, then what it built
		cancel()
		select {
		case err := <-done:
			if err != nil {
				tb.Fatal(err)
			}
		case <-time.After(hostTestStepTimeout):
			tb.Fatal("Run did not return")
		}
		computeHost.Close()
		if computeHost.Ready() || !clusters.closed {
			tb.Fatalf("after the drain: ready %t, clusters closed %t", computeHost.Ready(), clusters.closed)
		}
	})
}
