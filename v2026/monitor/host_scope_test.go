package monitor

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os/exec"
	"reflect"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// Force the independently selected Subtensor-role shipper through the actual
// signal adapter; a signal-constructor exclusion does not protect this path.
func TestHostScopeBlocksIndependentLogShipperTransport(t *testing.T) {
	seenHostCounts := map[string]int{}
	var stateLock sync.Mutex
	source := &syntheticSource{hostFn: func(configured HostSettings, _ string) (string, error) {
		stateLock.Lock()
		seenHostCounts[configured.Name]++
		stateLock.Unlock()
		if configured.Name == "allowed.example.test" {
			return logShipperFixture(map[string]string{"nofile_soft": "1024"}), nil
		}
		return logShipperFixture(nil), nil
	}}
	settings := syntheticSettings(source)
	settings.Hosts = []HostSettings{
		{Name: "excluded.example.test", Roles: []string{"subtensor"}},
		{Name: "allowed.example.test", Roles: []string{"services"}},
	}
	settings.LogServices = []string{"api", "proxy"}
	settings.LogServiceBlocks = map[string][]string{"api": {"blue"}, "proxy": {"green"}}
	settings.ProxyPathExpectedHosts = 7
	settings.MimirPublishers = MimirPublisherSettings{LoadState: "ready", PreferredFronts: map[string]string{"publisher.example.test": "front.example.test"}}
	beforeHosts := append([]HostSettings(nil), settings.Hosts...)
	scoped, err := ExcludeHosts(settings, "excluded.example.test")
	if err != nil {
		t.Fatal(err)
	}
	alerts, err := NewLogShipperSignal().Run(context.Background(), scoped)
	if err != nil {
		t.Fatal(err)
	}
	if seenHostCounts["excluded.example.test"] != 0 || seenHostCounts["allowed.example.test"] != 1 {
		t.Fatalf("host transport scope violated: %+v", seenHostCounts)
	}
	partial := false
	allowedProblem := false
	for _, alert := range alerts {
		partial = partial || alert.Class == "monitor-host-scope-partial"
		allowedProblem = allowedProblem || alert.Class == "log-shipper-fd-budget" && strings.HasPrefix(alert.Target, "allowed.example.test")
	}
	if !partial || !allowedProblem {
		t.Fatalf("scope did not preserve unknown coverage and allowed-target problem: %+v", alerts)
	}
	if !reflect.DeepEqual(scoped.Hosts, beforeHosts) || !reflect.DeepEqual(scoped.LogServices, settings.LogServices) ||
		!reflect.DeepEqual(scoped.LogServiceBlocks, settings.LogServiceBlocks) || scoped.ProxyPathExpectedHosts != 7 ||
		!reflect.DeepEqual(scoped.MimirPublishers, settings.MimirPublishers) {
		t.Fatal("host exclusion changed service streams, topology, or authoritative denominators")
	}
}

// Selector failures are fixed-vocabulary errors, not copies of command-line
// values or a fuzzy match that could miss the intended excluded target.
func TestExcludeHostsRejectsInvalidAndAmbiguousSelectors(t *testing.T) {
	for _, test := range []struct {
		name  string
		hosts []HostSettings
		value string
	}{
		{name: "empty", hosts: []HostSettings{{Name: "edge.example.test"}}},
		{name: "wildcard", hosts: []HostSettings{{Name: "edge.example.test"}}, value: "*.example.test"},
		{name: "unknown", hosts: []HostSettings{{Name: "edge.example.test"}}, value: "synthetic-secret-value"},
		{name: "ambiguous", hosts: []HostSettings{{Name: "edge.example.test"}, {Name: "edge.example.test"}}, value: "edge.example.test"},
		{name: "whitespace", hosts: []HostSettings{{Name: "edge.example.test"}}, value: " edge.example.test"},
		{name: "case mismatch", hosts: []HostSettings{{Name: "edge.example.test"}}, value: "EDGE.example.test"},
	} {
		_, err := ExcludeHosts(SignalSettings{Hosts: test.hosts}, test.value)
		if err == nil {
			t.Fatalf("%s selector was accepted", test.name)
		}
		if test.value != "" && strings.Contains(err.Error(), test.value) {
			t.Fatalf("%s selector value leaked into error", test.name)
		}
	}
}

// Policies are deduplicated and sorted without deleting or mutating any
// authoritative host settings or the caller's existing policy slice.
func TestExcludeHostsDeduplicatesWithoutMutatingSettings(t *testing.T) {
	settings := SignalSettings{
		Hosts:         []HostSettings{{Name: "a.example.test"}, {Name: "b.example.test"}},
		ExcludedHosts: []string{"b.example.test"},
	}
	scoped, err := ExcludeHosts(settings, "a.example.test", "b.example.test", "a.example.test")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(scoped.ExcludedHosts, []string{"a.example.test", "b.example.test"}) ||
		!reflect.DeepEqual(settings.ExcludedHosts, []string{"b.example.test"}) || !reflect.DeepEqual(scoped.Hosts, settings.Hosts) {
		t.Fatal("scope mutation or noncanonical repeated selectors")
	}
	scoped.ExcludedHosts[0] = "changed.example.test"
	if !reflect.DeepEqual(settings.ExcludedHosts, []string{"b.example.test"}) {
		t.Fatal("policy backing slice aliases caller")
	}
}

// Manually constructed reusable settings may contain repeated exact policy
// names; visibility reports unique hosts without mutating that caller input.
func TestHostScopeDirectSettingsDeduplicatesCoverageWithoutMutation(t *testing.T) {
	settings := syntheticSettings(&syntheticSource{})
	settings.Hosts = []HostSettings{{Name: "excluded.example.test"}}
	settings.ExcludedHosts = []string{"excluded.example.test", "excluded.example.test"}
	before := append([]string(nil), settings.ExcludedHosts...)
	alerts, err := NewSettingsFreshnessSignal().Run(context.Background(), settings)
	if err != nil || len(alerts) != 1 || alerts[0].Class != "monitor-host-scope-partial" {
		t.Fatalf("manual repeated policy was not observed: alerts=%d err=%v", len(alerts), err)
	}
	if !strings.Contains(alerts[0].Observed, "configured_hosts=1 excluded_hosts=1 blocked_hosts=0") {
		t.Fatal("repeated selectors overstated the excluded-host count")
	}
	if !reflect.DeepEqual(settings.ExcludedHosts, before) {
		t.Fatal("coverage reporting mutated manual settings")
	}
}

// Exercise every host-bearing seam through the actual environment wrapper;
// numeric family variants and curl overrides must not invoke the source.
func TestHostScopeBlocksAllInventoryTargetSeams(t *testing.T) {
	calls := 0
	source := &syntheticSource{
		postgresFn:    func(string) ([]Row, error) { calls++; return nil, nil },
		redisFn:       func(HostSettings, int, ...string) (string, error) { calls++; return "allowed", nil },
		hostFn:        func(HostSettings, string) (string, error) { calls++; return "allowed", nil },
		hostTimeoutFn: func(HostSettings, string, time.Duration) (string, error) { calls++; return "allowed", nil },
		localFn:       func(string, ...string) (string, error) { calls++; return "allowed", nil },
		tcpFn:         func(string, string, []byte, int) ([]byte, error) { calls++; return nil, nil },
		tlsFn: func(string, string, string) (TLSCertificateObservation, error) {
			calls++
			return TLSCertificateObservation{}, nil
		},
	}
	settings := syntheticSettings(source)
	settings.Hosts = []HostSettings{
		{Name: "excluded.example.test", LANAddress: "192.0.2.1", OverlayAddress: "198.51.100.1", Roles: []string{"pg-primary", "redis-cluster"}, EdgeIPv6: []EdgeIPv6InterfaceSettings{{Address: "2001:db8::1"}}},
		{Name: "allowed.example.test", LANAddress: "192.0.2.2", Roles: []string{"services"}},
	}
	settings, err := ExcludeHosts(settings, "excluded.example.test")
	if err != nil {
		t.Fatal(err)
	}
	env, err := newProbeEnv(settings)
	if err != nil {
		t.Fatal(err)
	}
	excluded := env.cfg.hosts[0]
	ctx := context.Background()
	for _, test := range []struct {
		name string
		run  func(context.Context) error
	}{
		{name: "postgres", run: func(ctx context.Context) error { _, err := env.runner.pg(ctx, "SELECT 1"); return err }},
		{name: "redis", run: func(ctx context.Context) error { _, err := env.runner.redis(ctx, excluded, 6379, "PING"); return err }},
		{name: "redis raw", run: func(ctx context.Context) error {
			_, err := env.runner.redisRaw(ctx, excluded, 6379, "PING")
			return err
		}},
		{name: "host", run: func(ctx context.Context) error { _, err := env.runner.shell(ctx, excluded, "true"); return err }},
		{name: "timed host", run: func(ctx context.Context) error {
			_, err := env.runner.sshTimeout(ctx, excluded, "true", "synthetic-secret-stdin", time.Second)
			return err
		}},
		{name: "tcp4", run: func(ctx context.Context) error {
			_, err := env.runner.tcpExchange(ctx, "tcp4", "192.0.2.1:443", nil, 0)
			return err
		}},
		{name: "mapped tcp4", run: func(ctx context.Context) error {
			_, err := env.runner.tcpExchange(ctx, "tcp", "[::ffff:192.0.2.1]:443", nil, 0)
			return err
		}},
		{name: "tcp6", run: func(ctx context.Context) error {
			_, err := env.runner.tcpExchange(ctx, "tcp6", "[2001:db8:0:0::1]:443", nil, 0)
			return err
		}},
		{name: "tls", run: func(ctx context.Context) error {
			_, err := env.runner.tlsCertificates(ctx, "tcp4", "192.0.2.1:443", "service.example.test")
			return err
		}},
		{name: "curl resolve", run: func(ctx context.Context) error {
			_, err := env.runner.local(ctx, "curl", "--resolve", "service.example.test:443:192.0.2.1", "https://service.example.test/status")
			return err
		}},
		{name: "curl resolve6", run: func(ctx context.Context) error {
			_, err := env.runner.local(ctx, "curl", "--resolve=service.example.test:443:[2001:db8::1]", "https://service.example.test/status")
			return err
		}},
		{name: "curl mixed resolve", run: func(ctx context.Context) error {
			_, err := env.runner.local(ctx, "curl", "--resolve", "service.example.test:443:192.0.2.2,192.0.2.1", "https://service.example.test/status")
			return err
		}},
		{name: "curl proxy", run: func(ctx context.Context) error {
			_, err := env.runner.local(ctx, "curl", "--proxy=https://excluded.example.test:443", "https://service.example.test/status")
			return err
		}},
		{name: "curl numeric url", run: func(ctx context.Context) error {
			_, err := env.runner.local(ctx, "curl", "https://198.51.100.1/status")
			return err
		}},
		{name: "curl malformed resolve", run: func(ctx context.Context) error {
			_, err := env.runner.local(ctx, "curl", "--resolve", "synthetic-unknown-format")
			return err
		}},
		{name: "curl unknown rewrite", run: func(ctx context.Context) error {
			_, err := env.runner.local(ctx, "curl", "--connect-to", "synthetic-unknown-format")
			return err
		}},
	} {
		if err := test.run(ctx); !hostScopeOnlyError(err) {
			t.Fatalf("%s was not denied as policy: %v", test.name, err)
		}
		if calls != 0 {
			t.Fatalf("%s invoked excluded transport", test.name)
		}
	}
	if _, err := env.runner.shell(ctx, env.cfg.hosts[1], "true"); err != nil {
		t.Fatal(err)
	}
	if _, err := env.runner.local(ctx, "curl", "--resolve", "service.example.test:443:192.0.2.2", "https://service.example.test/status"); err != nil {
		t.Fatal(err)
	}
	if _, err := env.runner.tcpExchange(ctx, "tcp4", "192.0.2.2:443", nil, 0); err != nil {
		t.Fatal(err)
	}
	if calls != 3 {
		t.Fatalf("permitted seams called %d times, want 3", calls)
	}
}

// A shared raw IP cannot prove which logical owner will receive a request;
// fail closed while still permitting explicitly named allowed-host commands.
func TestHostScopeSharedEndpointIsUnknownNotPermitted(t *testing.T) {
	calls := 0
	source := &syntheticSource{tcpFn: func(string, string, []byte, int) ([]byte, error) { calls++; return nil, nil }}
	settings := syntheticSettings(source)
	settings.Hosts = []HostSettings{{Name: "excluded.example.test", LANAddress: "192.0.2.1"}, {Name: "allowed.example.test", LANAddress: "192.0.2.1"}}
	settings, err := ExcludeHosts(settings, "excluded.example.test")
	if err != nil {
		t.Fatal(err)
	}
	env, err := newProbeEnv(settings)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := env.runner.tcpExchange(context.Background(), "tcp", "192.0.2.1:443", nil, 0); !hostScopeOnlyError(err) || calls != 0 {
		t.Fatalf("ambiguous ownership contacted an excluded endpoint: calls=%d err=%v", calls, err)
	}
}

// A per-host proxy advertisement is owned inventory, but the Subtensor
// reference RPC is external observational input, not a local node endpoint.
func TestHostScopePreservesExternalReferenceEndpoints(t *testing.T) {
	calls := 0
	settings := syntheticSettings(&syntheticSource{
		localFn: func(string, ...string) (string, error) { calls++; return "synthetic", nil },
		tcpFn:   func(string, string, []byte, int) ([]byte, error) { calls++; return nil, nil },
		hostFn:  func(HostSettings, string) (string, error) { calls++; return "synthetic", nil },
	})
	settings.Hosts = []HostSettings{{
		Name: "excluded.example.test", LANAddress: "192.0.2.1",
		Proxy:     &ProxyHostSettings{PublicHostname: "proxy.example.test"},
		Subtensor: &SubtensorHostSettings{PublicRPCURL: "https://reference.example.test/rpc"},
	}}
	settings, err := ExcludeHosts(settings, "excluded.example.test")
	if err != nil {
		t.Fatal(err)
	}
	env, err := newProbeEnv(settings)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.WithValue(context.Background(), hostScopeContextKey{}, env.runner.(*hostScopeRunner))
	if _, err := env.runner.local(ctx, "curl", "https://reference.example.test/rpc"); err != nil {
		t.Fatal("external reference was denied as host-owned")
	}
	if _, err := env.runner.tcpExchange(ctx, "tcp", "reference.example.test:443", nil, 0); err != nil {
		t.Fatal("external reference transport was denied as host-owned")
	}
	client := &hostScopeGrafanaClient{doFn: func(*http.Request) (*http.Response, error) {
		calls++
		return grafanaFixtureResponse(http.StatusOK, "synthetic"), nil
	}}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://reference.example.test/rpc", nil)
	if err != nil {
		t.Fatal(err)
	}
	response, err := doScopedGrafanaRequest(client, request)
	if err != nil {
		t.Fatal("external reference HTTP was denied as host-owned")
	}
	_ = response.Body.Close()
	if calls != 3 {
		t.Fatalf("external reference control did not contact the synthetic seams: %d", calls)
	}
	if _, err := env.runner.local(ctx, "curl", "--proxy", "proxy.example.test:8080", "https://reference.example.test/rpc"); !hostScopeOnlyError(err) || calls != 3 {
		t.Fatal("per-host proxy advertisement escaped host scope")
	}
	if _, err := env.runner.shell(ctx, env.cfg.hosts[0], "synthetic-local-node-query"); !hostScopeOnlyError(err) || calls != 3 {
		t.Fatal("external reference control bypassed the named local-node policy")
	}
}

// Synthetic probe control forces incomplete global findings, rather than
// relying on one current parser's incidental error/healthy behavior.
type hostScopeSyntheticProbe struct {
	checkFn func(context.Context, *probeEnv) ([]finding, error)
}

func (self hostScopeSyntheticProbe) id() string             { return "synthetic/host-scope" }
func (self hostScopeSyntheticProbe) tier() string           { return tierWarn }
func (self hostScopeSyntheticProbe) cadence() time.Duration { return time.Minute }
func (self hostScopeSyntheticProbe) check(ctx context.Context, env *probeEnv) ([]finding, error) {
	return self.checkFn(ctx, env)
}

// A denied member must not turn look-alike global placement/capacity or healthy
// conclusions into Alerts/recovery. Proven per-allowed-host findings survive.
func TestHostScopeReducerWithholdsIncompleteFleetConclusions(t *testing.T) {
	settings := syntheticSettings(&syntheticSource{})
	settings.Hosts = []HostSettings{{Name: "excluded.example.test"}, {Name: "allowed.example.test"}}
	settings, err := ExcludeHosts(settings, "excluded.example.test")
	if err != nil {
		t.Fatal(err)
	}
	env, err := newProbeEnv(settings)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := env.runner.shell(context.Background(), env.cfg.hosts[0], "true"); !hostScopeOnlyError(err) {
		t.Fatal(err)
	}
	observed := []finding{
		{probeId: "synthetic/placement", class: "synthetic-placement", target: "fleet"},
		{probeId: "synthetic/capacity", class: "synthetic-capacity", target: "fleet"},
		healthyFinding("synthetic/health", tierWarn, "synthetic-fleet-health", "fleet"),
		healthyFinding("monitor/visibility", tierWarn, "synthetic-fleet-visibility-health", "fleet"),
		{probeId: "monitor/visibility", class: "synthetic-fleet-unobservable", target: "fleet"},
		{probeId: "synthetic/issue", class: "synthetic-owned-issue", target: "allowed.example.test/process"},
		healthyFinding("synthetic/health", tierWarn, "synthetic-owned-health", "allowed.example.test:443"),
		{probeId: "synthetic/issue", class: "synthetic-excluded-issue", target: "excluded.example.test/process"},
		healthyFinding("synthetic/health", tierWarn, "synthetic-excluded-health", "excluded.example.test:443"),
	}
	reduced := env.runner.(*hostScopeRunner).reduceFindings(settings, observed)
	if len(reduced) != 4 {
		t.Fatalf("scope reducer produced %d findings, want two permitted, unknown visibility, and partial coverage", len(reduced))
	}
	for _, finding := range reduced {
		if finding.class != "synthetic-owned-issue" && finding.class != "synthetic-owned-health" && finding.class != "synthetic-fleet-unobservable" && finding.class != "monitor-host-scope-partial" {
			t.Fatalf("incomplete/excluded finding escaped: %s", finding.class)
		}
	}
	settings.ExcludedHosts = nil
	complete, err := newProbeEnv(settings)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := complete.runner.(*hostScopeRunner); ok {
		t.Fatal("no-policy caller acquired a scope wrapper")
	}
}

// Composite accept predicates must not discard the policy visibility Alert;
// a blocked sole PostgreSQL source is unknown, never a schema/canary diagnosis.
func TestHostScopeAdapterRetainsCoverageThroughAcceptPredicates(t *testing.T) {
	calls := 0
	settings := syntheticSettings(&syntheticSource{postgresFn: func(string) ([]Row, error) { calls++; return nil, nil }})
	settings.Hosts = []HostSettings{{Name: "excluded.example.test", Roles: []string{"pg-primary"}}}
	settings, err := ExcludeHosts(settings, "excluded.example.test")
	if err != nil {
		t.Fatal(err)
	}
	for _, signal := range []Signal{NewMigrationsSignal(), NewTaskCanariesSignal()} {
		alerts, err := signal.Run(context.Background(), settings)
		if err != nil || len(alerts) != 1 || alerts[0].Class != "monitor-host-scope-partial" || calls != 0 {
			t.Fatalf("%s lost sole-source unknown coverage: alerts=%d calls=%d err=%v", signal.Key(), len(alerts), calls, err)
		}
	}
	probe := hostScopeSyntheticProbe{checkFn: func(ctx context.Context, env *probeEnv) ([]finding, error) {
		_, err := env.runner.shell(ctx, env.cfg.hosts[0], "true")
		return nil, err
	}}
	signal := &signalAdapter{number: "1.6", key: "synthetic", name: "Synthetic", probe: probe, accept: func(finding) bool { return false }}
	alerts, err := signal.Run(context.Background(), settings)
	if err != nil || len(alerts) != 1 || alerts[0].Class != "monitor-host-scope-partial" {
		t.Fatalf("accept dropped operational visibility: %d %v", len(alerts), err)
	}
}

// The guard is before the actual SSH command seam, not only a test-source shim.
func TestHostScopeProductionRunnerDoesNotInvokeExcludedSSH(t *testing.T) {
	settings := SignalSettings{SSHUser: "synthetic-user", Hosts: []HostSettings{{Name: "excluded.example.test", LANAddress: "192.0.2.1"}, {Name: "allowed.example.test", LANAddress: "192.0.2.2"}}, AddressMode: AddressModeLAN}
	settings, err := ExcludeHosts(settings, "excluded.example.test")
	if err != nil {
		t.Fatal(err)
	}
	env, err := newProbeEnv(settings.withDefaults())
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	env.runner.(*hostScopeRunner).probeRunner.(*runner).runSSH = func(context.Context, []string, string) (string, string, error) { calls++; return "ok", "", nil }
	if _, err := env.runner.shell(context.Background(), env.cfg.hosts[0], "true"); !hostScopeOnlyError(err) || calls != 0 {
		t.Fatalf("excluded SSH started: %d %v", calls, err)
	}
	if _, err := env.runner.shell(context.Background(), env.cfg.hosts[1], "true"); err != nil || calls != 1 {
		t.Fatalf("permitted SSH changed: %d %v", calls, err)
	}
}

// Standing tails intentionally retain whole-environment service scope.
type hostScopeStreamingSource struct {
	*syntheticSource
	streamCalls int
}

func (self *hostScopeStreamingSource) StreamLocal(_ context.Context, name string, args ...string) (*exec.Cmd, io.ReadCloser, error) {
	if name != "warpctl" || len(args) < 3 || args[0] != "logs" {
		return nil, nil, fmt.Errorf("unexpected synthetic stream")
	}
	self.streamCalls++
	return nil, io.NopCloser(strings.NewReader("")), nil
}

func TestHostScopePreservesWholeEnvironmentServiceTails(t *testing.T) {
	source := &hostScopeStreamingSource{syntheticSource: &syntheticSource{}}
	settings := syntheticSettings(source)
	settings.Hosts = []HostSettings{{Name: "excluded.example.test", Roles: []string{"subtensor"}}}
	settings.LogServices = []string{"api", "proxy"}
	settings.LogServiceBlocks = map[string][]string{"api": {"blue"}, "proxy": {"green"}}
	settings, err := ExcludeHosts(settings, "excluded.example.test")
	if err != nil {
		t.Fatal(err)
	}
	_, tailers, err := NewWithSignals(settings, NewLogErrorsSignal()).prepareRunLoop(context.Background())
	if err != nil || len(tailers) != 2 {
		t.Fatalf("whole-environment tails changed: %d %v", len(tailers), err)
	}
	for index, tailer := range tailers {
		if tailer.service != settings.LogServices[index] || !reflect.DeepEqual(tailer.blocks, settings.LogServiceBlocks[tailer.service]) {
			t.Fatal("service tail topology changed")
		}
		_, stream, err := tailer.openStream(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		_ = stream.Close()
	}
	if source.streamCalls != 2 {
		t.Fatalf("standing sources called %d times, want 2", source.streamCalls)
	}
}

// No scope leaf can hide an independent joined failure or cancellation.
func TestHostScopeOnlyErrorRejectsMixedAndCanceledFailures(t *testing.T) {
	denied := &hostScopeExcludedError{}
	if !hostScopeOnlyError(fmt.Errorf("synthetic wrapper: %w", denied)) || !hostScopeOnlyError(errors.Join(denied, denied)) {
		t.Fatal("policy-only wrappers were not recognized")
	}
	if hostScopeOnlyError(errors.Join(denied, errors.New("synthetic-other-failure"))) || hostScopeOnlyError(context.Canceled) || hostScopeOnlyError(nil) {
		t.Fatal("non-policy failure became intentional scope")
	}
}

// HTTP callers retain their injected client identity; only the private request
// context gates exact inventory destinations before its Do method is invoked.
type hostScopeGrafanaClient struct {
	doFn func(*http.Request) (*http.Response, error)
}

func (self *hostScopeGrafanaClient) Do(request *http.Request) (*http.Response, error) {
	return self.doFn(request)
}

func TestHostScopeGuardsDefaultAndInjectedGrafanaRequests(t *testing.T) {
	settings := syntheticSettings(&syntheticSource{})
	settings.PublicDomain = "service.example.test"
	settings.Hosts = []HostSettings{{Name: "front.example.test", LANAddress: "192.0.2.1"}}
	settings, err := ExcludeHosts(settings, "front.example.test")
	if err != nil {
		t.Fatal(err)
	}
	env, err := newProbeEnv(settings)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.WithValue(context.Background(), hostScopeContextKey{}, env.runner.(*hostScopeRunner))
	calls := 0
	client := &hostScopeGrafanaClient{doFn: func(*http.Request) (*http.Response, error) {
		calls++
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(`{"synthetic":true}`))}, nil
	}}
	defaultClient := newGrafanaObservationHTTPClientWithClients(client, client)
	for _, endpoint := range []string{"https://synthetic-user:synthetic-secret@192.0.2.1/status", "https://FRONT.EXAMPLE.TEST./status"} {
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
		if err != nil {
			t.Fatal(err)
		}
		request.SetBasicAuth("synthetic-user", "synthetic-secret")
		for _, actual := range []grafanaHTTPClient{client, defaultClient} {
			_, err := doScopedGrafanaRequest(actual, request)
			if !hostScopeOnlyError(err) || calls != 0 {
				t.Fatalf("denied HTTP Do seam was invoked: %d %v", calls, err)
			}
			if strings.Contains(err.Error(), "synthetic-secret") || strings.Contains(err.Error(), "192.0.2.1") {
				t.Fatal("HTTP denial retained request values")
			}
		}
		if _, err := defaultClient.Do(request); !hostScopeOnlyError(err) || calls != 0 {
			t.Fatalf("direct default client bypassed scope: %d %v", calls, err)
		}
	}
	var destination map[string]bool
	if err := getBoundedGrafanaJSON(ctx, client, "https://192.0.2.1/api", "synthetic-secret", &destination); !hostScopeOnlyError(err) {
		t.Fatalf("JSON Do seam bypassed scope: %v", err)
	}
	if _, err := queryGrafanaDatasource(ctx, client, "https://192.0.2.1/api", "synthetic-secret", grafanaDatasourceQuerySpec{uid: "synthetic", typeName: "prometheus"}); !hostScopeOnlyError(err) {
		t.Fatalf("datasource Do seam bypassed scope: %v", err)
	}
	if _, err := observeSubscriptionMetricsDashboard(ctx, client, "https://192.0.2.1/api", "synthetic-secret"); !hostScopeOnlyError(err) {
		t.Fatalf("dashboard Do seam bypassed scope: %v", err)
	}
	if calls != 0 {
		t.Fatalf("explicit inventory endpoints invoked %d HTTP requests", calls)
	}
	publicRequest, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://grafana.service.example.test/api", nil)
	if err != nil {
		t.Fatal(err)
	}
	response, err := doScopedGrafanaRequest(defaultClient, publicRequest)
	if err != nil || calls != 1 {
		t.Fatalf("canonical public Grafana was disabled: %d %v", calls, err)
	}
	_ = response.Body.Close()
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := doScopedGrafanaRequest(client, publicRequest.Clone(canceled)); !errors.Is(err, context.Canceled) || calls != 1 {
		t.Fatalf("canceled Do invoked HTTP: %d %v", calls, err)
	}
	if defaultClient.primary != client || defaultClient.ipv4 != client {
		t.Fatal("scope mutated existing HTTP client seams")
	}
}

// The adapter installs scope for optional HTTP probes, not just for callers
// that manually decorate a request context in a utility unit test.
func TestHostScopeAdapterGuardsOptionalGrafanaHTTPProbe(t *testing.T) {
	calls := 0
	client := &hostScopeGrafanaClient{doFn: func(*http.Request) (*http.Response, error) {
		calls++
		return nil, errors.New("synthetic-should-not-run")
	}}
	settings := syntheticSettings(&syntheticSource{})
	settings.PublicDomain = "service.example.test"
	settings.Grafana.AdminPassword = "synthetic-admin-credential"
	settings.Hosts = []HostSettings{{Name: "front.example.test", LANAddress: "192.0.2.1"}}
	settings, err := ExcludeHosts(settings, "front.example.test")
	if err != nil {
		t.Fatal(err)
	}
	alerts, err := newGrafanaDatasourcesSignal(client, "https://192.0.2.1/api").Run(context.Background(), settings)
	if err != nil || calls != 0 {
		t.Fatalf("optional Do source bypassed adapter scope: %d %v", calls, err)
	}
	requireAlertClass(t, alerts, "monitor-host-scope-partial")
	for _, alert := range alerts {
		requireAlertOmits(t, alert, "synthetic-should-not-run", "192.0.2.1", "front.example.test")
	}
}

// The owned HTTP client must check each redirect before its transport sees
// the new inventory-owned destination; an initial request guard is not enough.
func TestHostScopeOwnedGrafanaClientRejectsExcludedRedirect(t *testing.T) {
	settings := syntheticSettings(&syntheticSource{})
	settings.Hosts = []HostSettings{{Name: "front.example.test", LANAddress: "192.0.2.1"}}
	settings, err := ExcludeHosts(settings, "front.example.test")
	if err != nil {
		t.Fatal(err)
	}
	env, err := newProbeEnv(settings)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.WithValue(context.Background(), hostScopeContextKey{}, env.runner.(*hostScopeRunner))
	for _, fallback := range []bool{false, true} {
		primaryCalls, publicCalls, excludedCalls := 0, 0, 0
		transport := grafanaRoundTripFunc(func(request *http.Request) (*http.Response, error) {
			if request.URL.Hostname() == "grafana.service.example.test" {
				publicCalls++
				response := grafanaFixtureResponse(http.StatusFound, "")
				response.Header.Set("Location", "https://192.0.2.1/private?synthetic-secret-query")
				return response, nil
			}
			excludedCalls++
			return grafanaFixtureResponse(http.StatusOK, "synthetic"), nil
		})
		primary := grafanaRoundTripFunc(func(request *http.Request) (*http.Response, error) {
			primaryCalls++
			if fallback {
				return nil, syscall.ENETUNREACH
			}
			return transport.RoundTrip(request)
		})
		client := newGrafanaObservationHTTPClientWithTransports(time.Second, primary, transport)
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://grafana.service.example.test/api", nil)
		if err != nil {
			t.Fatal(err)
		}
		response, err := client.Do(request)
		if response != nil {
			_ = response.Body.Close()
		}
		if !hostScopeOnlyError(err) || primaryCalls != 1 || publicCalls != 1 || excludedCalls != 0 {
			t.Fatalf("redirect reached excluded transport: fallback=%t primary=%d public=%d excluded=%d err=%v", fallback, primaryCalls, publicCalls, excludedCalls, err)
		}
		for _, value := range []string{"192.0.2.1", "private", "synthetic-secret-query", "grafana.service.example.test"} {
			if strings.Contains(err.Error(), value) {
				t.Fatal("redirect denial retained a request value")
			}
		}
	}
}

// A denied redirect remains explicit operational coverage through the real
// adapter instead of becoming a retryable network failure or a green query.
func TestHostScopeAdapterRetainsDeniedGrafanaRedirectCoverage(t *testing.T) {
	settings := syntheticSettings(&syntheticSource{})
	settings.PublicDomain = "service.example.test"
	settings.Grafana.AdminPassword = "synthetic-admin-credential"
	settings.Hosts = []HostSettings{{Name: "front.example.test", LANAddress: "192.0.2.1"}}
	settings, err := ExcludeHosts(settings, "front.example.test")
	if err != nil {
		t.Fatal(err)
	}
	excludedCalls := 0
	transport := grafanaRoundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.URL.Hostname() == "grafana.service.example.test" {
			response := grafanaFixtureResponse(http.StatusFound, "")
			response.Header.Set("Location", "https://192.0.2.1/private?synthetic-secret-query")
			return response, nil
		}
		excludedCalls++
		return grafanaFixtureResponse(http.StatusOK, "synthetic"), nil
	})
	client := newGrafanaObservationHTTPClientWithTransports(time.Second, transport, transport)
	alerts, err := newGrafanaDatasourcesSignal(client, "https://grafana.service.example.test/api").Run(context.Background(), settings)
	if err != nil || excludedCalls != 0 {
		t.Fatalf("adapter redirect bypassed scope: calls=%d err=%v", excludedCalls, err)
	}
	requireAlertClass(t, alerts, "monitor-host-scope-partial")
	for _, alert := range alerts {
		if alert.Class == "monitor-host-scope-partial" && (alert.SignalNumber != "11.15" || alert.SignalKey != "grafana-datasources" || alert.PageSustain != 0) {
			t.Fatal("coverage lost its originating probe or operational severity")
		}
		requireAlertOmits(t, alert, "192.0.2.1", "front.example.test", "synthetic-secret-query", "synthetic-admin-credential")
	}
}

// Preserve normal redirects and the standard ten-hop bound for both permitted
// whole-environment observations and clients with no host-exclusion policy.
func TestHostScopeOwnedGrafanaClientPreservesAllowedRedirectsAndBound(t *testing.T) {
	settings := syntheticSettings(&syntheticSource{})
	settings.Hosts = []HostSettings{{Name: "front.example.test", LANAddress: "192.0.2.1"}}
	settings, err := ExcludeHosts(settings, "front.example.test")
	if err != nil {
		t.Fatal(err)
	}
	env, err := newProbeEnv(settings)
	if err != nil {
		t.Fatal(err)
	}
	scoped := context.WithValue(context.Background(), hostScopeContextKey{}, env.runner.(*hostScopeRunner))
	for _, ctx := range []context.Context{context.Background(), scoped} {
		calls := 0
		transport := grafanaRoundTripFunc(func(request *http.Request) (*http.Response, error) {
			calls++
			if request.URL.Path == "/first" {
				response := grafanaFixtureResponse(http.StatusFound, "")
				response.Header.Set("Location", "https://allowed.service.example.test/final")
				return response, nil
			}
			return grafanaFixtureResponse(http.StatusOK, "synthetic"), nil
		})
		client := newGrafanaObservationHTTPClientWithTransports(time.Second, transport, transport)
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://grafana.service.example.test/first", nil)
		if err != nil {
			t.Fatal(err)
		}
		response, err := client.Do(request)
		if err != nil || calls != 2 || response.StatusCode != http.StatusOK {
			t.Fatalf("permitted redirect behavior changed: calls=%d err=%v", calls, err)
		}
		_ = response.Body.Close()
		calls = 0
		loop := grafanaRoundTripFunc(func(*http.Request) (*http.Response, error) {
			calls++
			response := grafanaFixtureResponse(http.StatusFound, "")
			response.Header.Set("Location", "https://grafana.service.example.test/loop")
			return response, nil
		})
		client = newGrafanaObservationHTTPClientWithTransports(time.Second, loop, loop)
		if _, err := client.Do(request); err == nil || calls != 10 {
			t.Fatalf("standard redirect bound changed: calls=%d err=%v", calls, err)
		}
	}
}

// Cancellation is lifecycle, not intentional observation exclusion. The
// redirect callback must not record a new denied host after parent shutdown.
func TestHostScopeOwnedRedirectCancellationDoesNotRecordDenial(t *testing.T) {
	settings := syntheticSettings(&syntheticSource{})
	settings.Hosts = []HostSettings{{Name: "excluded.example.test", LANAddress: "192.0.2.1"}}
	settings, err := ExcludeHosts(settings, "excluded.example.test")
	if err != nil {
		t.Fatal(err)
	}
	env, err := newProbeEnv(settings)
	if err != nil {
		t.Fatal(err)
	}
	scoped := env.runner.(*hostScopeRunner)
	ctx, cancel := context.WithCancel(context.WithValue(context.Background(), hostScopeContextKey{}, scoped))
	cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://192.0.2.1/status", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := checkGrafanaObservationRedirect(request, nil); !errors.Is(err, context.Canceled) || hostScopeOnlyError(err) {
		t.Fatalf("canceled redirect became scope denial: %v", err)
	}
	scoped.stateLock.Lock()
	blocked := len(scoped.blockedHostNames)
	scoped.stateLock.Unlock()
	if blocked != 0 {
		t.Fatal("canceled redirect recorded excluded observation")
	}
}

// Curl accepts scheme-less numeric URLs and proxy endpoints. These supported
// forms must not escape an inventory-target policy before the local seam.
func TestHostScopeCurlNumericArgumentForms(t *testing.T) {
	calls := 0
	settings := syntheticSettings(&syntheticSource{localFn: func(string, ...string) (string, error) {
		calls++
		return "synthetic", nil
	}})
	settings.Hosts = []HostSettings{{Name: "excluded.example.test", LANAddress: "192.0.2.1", EdgeIPv6: []EdgeIPv6InterfaceSettings{{Address: "2001:db8::1"}}}}
	settings, err := ExcludeHosts(settings, "excluded.example.test")
	if err != nil {
		t.Fatal(err)
	}
	env, err := newProbeEnv(settings)
	if err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"--proxy", "192.0.2.1:8080", "https://service.example.test/status"},
		{"--proxy=192.0.2.1:8080", "https://service.example.test/status"},
		{"-x", "[2001:db8::1]:8080", "https://service.example.test/status"},
		{"-x192.0.2.1:8080", "https://service.example.test/status"},
		{"--url", "192.0.2.1/status"},
		{"--url=[2001:db8::1]:8080/status"},
		{"192.0.2.1:8080/status"},
		{"[2001:db8::1]/status"},
	} {
		if _, err := env.runner.local(context.Background(), "curl", args...); !hostScopeOnlyError(err) || calls != 0 {
			t.Fatalf("numeric curl endpoint bypassed policy: calls=%d err=%v", calls, err)
		}
	}
	if _, err := env.runner.local(context.Background(), "curl", "--proxy", "192.0.2.2:8080", "198.51.100.2/status"); err != nil || calls != 1 {
		t.Fatalf("permitted numeric curl endpoint changed: calls=%d err=%v", calls, err)
	}
}

// An opt-in transport policy must not alter existing no-policy local readers.
// Cancellation can intentionally skip a delayed phase while retaining the
// completed immediate evaluation, as the PG and coverage controls require.
func TestHostScopePreservesNoPolicyCanceledLocalEvaluation(t *testing.T) {
	evaluations := 0
	probe := hostScopeSyntheticProbe{checkFn: func(ctx context.Context, env *probeEnv) ([]finding, error) {
		evaluations++
		if !errors.Is(ctx.Err(), context.Canceled) {
			t.Fatal("local control did not receive the canceled context")
		}
		if _, scoped := env.runner.(*hostScopeRunner); scoped {
			t.Fatal("no-policy local control acquired a host-scope runner")
		}
		return []finding{{probeId: "synthetic/local", tier: tierWarn, class: "synthetic-local-unavailable", target: "synthetic-local"}}, nil
	}}
	adapter := &signalAdapter{number: "1.6", key: "synthetic-local", name: "Synthetic local reader", probe: probe}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	alerts, err := adapter.Run(ctx, syntheticSettings(&syntheticSource{}))
	if err != nil || evaluations != 1 || len(alerts) != 1 || alerts[0].Class != "synthetic-local-unavailable" {
		t.Fatalf("no-policy local result was discarded: evaluations=%d alerts=%d err=%v", evaluations, len(alerts), err)
	}
}

// Without the opt-in policy, preserve the probe's own error authority rather
// than replacing a completed probe error with its parent cancellation state.
func TestHostScopePreservesNoPolicyProbeErrorOnCanceledContext(t *testing.T) {
	syntheticError := errors.New("synthetic-local-evaluation-error")
	probe := hostScopeSyntheticProbe{checkFn: func(context.Context, *probeEnv) ([]finding, error) {
		return nil, syntheticError
	}}
	adapter := &signalAdapter{number: "1.6", key: "synthetic-local", name: "Synthetic local reader", probe: probe}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	alerts, err := adapter.Run(ctx, syntheticSettings(&syntheticSource{}))
	if !errors.Is(err, syntheticError) || len(alerts) != 0 {
		t.Fatalf("no-policy probe error authority changed: alerts=%d err=%v", len(alerts), err)
	}
}

// Context cancellation is checked before the policy denial, so it cannot
// create an operational partial-coverage event or invoke any underlying seam.
func TestHostScopeCanceledWorkHasNoContactOrNewDenial(t *testing.T) {
	calls := 0
	settings := syntheticSettings(&syntheticSource{
		hostFn:  func(HostSettings, string) (string, error) { calls++; return "", nil },
		localFn: func(string, ...string) (string, error) { calls++; return "", nil },
	})
	settings.Hosts = []HostSettings{{Name: "excluded.example.test", LANAddress: "192.0.2.1"}}
	settings, err := ExcludeHosts(settings, "excluded.example.test")
	if err != nil {
		t.Fatal(err)
	}
	env, err := newProbeEnv(settings)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := env.runner.shell(ctx, env.cfg.hosts[0], "true"); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if _, err := env.runner.local(ctx, "curl", "https://192.0.2.1/status"); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if calls != 0 || len(env.runner.(*hostScopeRunner).blockedHostNames) != 0 {
		t.Fatal("canceled work invoked a source or recorded a scope denial")
	}
	alerts, err := NewLogShipperSignal().Run(ctx, settings)
	if !errors.Is(err, context.Canceled) || len(alerts) != 0 {
		t.Fatalf("canceled signal emitted coverage: %d %v", len(alerts), err)
	}
}

// Fixed counts describe policy without serializing any effective settings,
// private identity, credentials, key paths, or endpoint values.
func TestHostScopePartialMarkdownIsOperationalAndRedacted(t *testing.T) {
	settings := syntheticSettings(&syntheticSource{})
	settings.Hosts = []HostSettings{{Name: "private.example.test", LANAddress: "192.0.2.1"}}
	settings.PostgreSQL.Password = "synthetic-password-secret"
	settings.SSHKeyPaths = []string{"testdata/synthetic-private-key"}
	settings, err := ExcludeHosts(settings, "private.example.test")
	if err != nil {
		t.Fatal(err)
	}
	alerts, err := NewSettingsFreshnessSignal().Run(context.Background(), settings)
	if err != nil || len(alerts) != 1 {
		t.Fatalf("manual policy coverage was invisible: %d %v", len(alerts), err)
	}
	alert := requireAlertClass(t, alerts, "monitor-host-scope-partial")
	if alert.Severity != SeverityWarn || alert.PageSustain != 0 {
		t.Fatal("operational pause auto-pages")
	}
	for _, phrase := range []string{"excluded_hosts=1", "desired_topology_unchanged=true", "unknown", "owner", "re-enable", "full-fleet recovery"} {
		if !strings.Contains(alert.Markdown(), phrase) {
			t.Fatalf("scope Markdown omitted %q", phrase)
		}
	}
	requireAlertOmits(t, alert, "private.example.test", "192.0.2.1", settings.PostgreSQL.Password, settings.SSHKeyPaths[0])
	settings.ExcludedHosts = []string{"synthetic-invalid-secret-selector"}
	if err := settings.Validate(); err == nil || strings.Contains(err.Error(), settings.ExcludedHosts[0]) {
		t.Fatalf("synthetic Source skipped scope validation or leaked selector: %v", err)
	}
}
