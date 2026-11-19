package monitor

// The lb status probe (lbalt_status.go): its monitor.yml enrollment, the
// matrix it validates against the inventory, the pass that runs its attempts,
// the scope guards in front of its transport, and the verdict of each
// attempt.

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

const (
	// a synthetic hidden prefix, distinctive enough that a leak into any
	// diagnostic is found by substring
	lbAltStatusTestPrefix = "synthetic-hidden-7f3a"
	// the lb name of the synthetic environment and its primary domain
	lbAltStatusTestServerName = "synthetic-lb.edge.example"
	// an observer address an answer echoes, which no diagnostic may keep
	lbAltStatusTestClientAddress = "203.0.113.77:41000"
)

var lbAltStatusTestBlocks = []string{"beta", "g1", "g2", "g3", "g4"}

// Signal settings with two lb interfaces on one host, one dual-stack and one
// ipv4 only, and five connect blocks.
func lbAltStatusTestSignalSettings(source SignalSource) SignalSettings {
	settings := syntheticSettings(source)
	settings.PublicDomain = "edge.example"
	settings.Hosts = append(settings.Hosts, HostSettings{
		Name:  "edge-synthetic",
		Roles: []string{"edge"},
		PublicLB: []PublicLBInterfaceSettings{
			{Interface: "eth1", IPv4Address: "198.51.100.10", IPv6Address: "2001:db8:10::10"},
			{Interface: "eth2", IPv4Address: "198.51.100.11"},
		},
	})
	settings.LogServices = []string{"api", "connect"}
	settings.LogServiceBlocks = map[string][]string{
		"api":     {"g1", "g2"},
		"connect": append([]string(nil), lbAltStatusTestBlocks...),
	}
	return settings
}

// Enrollment of both synthetic interfaces, with the route prefix the loader
// takes from lb_hidden_prefixes.
func lbAltStatusTestSettings() LbAltStatusSettings {
	return LbAltStatusSettings{
		Enabled:         true,
		ExpectedTargets: 2,
		Targets: []LbAltStatusTargetSettings{
			{Name: "edge-synthetic-eth1", Host: "edge-synthetic", Interface: "eth1", Families: []string{"ipv4", "ipv6"}},
			{Name: "edge-synthetic-eth2", Host: "edge-synthetic", Interface: "eth2", Families: []string{"ipv4"}},
		},
		BlockService:   "connect",
		ExpectedBlocks: len(lbAltStatusTestBlocks),
		routePrefix:    "/" + lbAltStatusTestPrefix,
	}
}

// The requests of the synthetic matrix.
func lbAltStatusTestRequests(t *testing.T, cfg *monitorConfig) []LbAltStatusRequest {
	t.Helper()
	requests, reason := lbAltStatusRequests(lbAltStatusTestSettings(), DefaultLbAltStatusProbeSettings(), cfg)
	if reason != "" {
		t.Fatalf("the synthetic matrix was refused: %s", reason)
	}
	return requests
}

// The answer a healthy front gives a request, as its transport observes it.
func lbAltStatusTestHealthy(request LbAltStatusRequest) LbAltStatusObservation {
	observation := LbAltStatusObservation{
		AttemptId:       request.AttemptId,
		RequestedTuple:  lbAltStatusTuple(request),
		Dialed:          true,
		FreshConnection: true,
		StatusCode:      200,
		JsonContentType: true,
		ResponseBytes:   96,
		BodyJson:        true,
		Status:          LbAltStatusStateOk,
	}
	if request.Route == lbAltStatusRouteBlock {
		observation.TlsVerified = true
		observation.BlockMatched = true
		observation.FrontReadiness = LbAltStatusFrontReadinessReady
	} else {
		observation.LbAltFront = true
	}
	return observation
}

// A synthetic source with the lb status capability.
type lbAltStatusTestSource struct {
	*syntheticSource
	observe func(context.Context, LbAltStatusRequest) (LbAltStatusObservation, error)
}

func (self *lbAltStatusTestSource) LbAltStatus(ctx context.Context, request LbAltStatusRequest) (LbAltStatusObservation, error) {
	return self.observe(ctx, request)
}

// Runs one pass of the synthetic matrix through the probe environment the
// settings build, with its scope guards.
func lbAltStatusTestPass(t *testing.T, signalSettings SignalSettings, probe lbAltStatusProbe) ([]LbAltStatusRequest, []lbAltStatusResult, error) {
	t.Helper()
	env, err := newProbeEnv(signalSettings)
	if err != nil {
		t.Fatal(err)
	}
	requests := lbAltStatusTestRequests(t, env.cfg)
	results, err := probe.observe(context.Background(), env.runner, requests)
	return requests, results, err
}

// Each enrolled interface family expands into its lb route on 80 and one
// block route on 443 per active block, pinned to the inventory address, named
// for the lb host, in a fixed order.
func TestLbAltStatusRequestsExpandEveryInterfaceFamilyAndBlock(t *testing.T) {
	cfg := configFromSignalSettings(lbAltStatusTestSignalSettings(&syntheticSource{}))
	requests := lbAltStatusTestRequests(t, cfg)
	want := []LbAltStatusRequest{}
	for _, pin := range []struct {
		target  string
		family  string
		address string
	}{
		{target: "edge-synthetic-eth1", family: "ipv4", address: "198.51.100.10"},
		{target: "edge-synthetic-eth1", family: "ipv6", address: "2001:db8:10::10"},
		{target: "edge-synthetic-eth2", family: "ipv4", address: "198.51.100.11"},
	} {
		want = append(want, LbAltStatusRequest{
			Host:       "edge-synthetic",
			Target:     pin.target,
			Family:     pin.family,
			Route:      "lb",
			Address:    pin.address,
			Port:       80,
			ServerName: lbAltStatusTestServerName,
			Path:       "/" + lbAltStatusTestPrefix + "/status",
		})
		for _, block := range lbAltStatusTestBlocks {
			want = append(want, LbAltStatusRequest{
				Host:       "edge-synthetic",
				Target:     pin.target,
				Family:     pin.family,
				Route:      "block",
				Service:    "connect",
				Block:      block,
				Address:    pin.address,
				Port:       443,
				ServerName: lbAltStatusTestServerName,
				Path:       "/" + lbAltStatusTestPrefix + "/by/b/connect/" + block + "/status",
			})
		}
	}
	if !reflect.DeepEqual(requests, want) {
		t.Fatalf("requests = %+v\nwant %+v", requests, want)
	}
}

// Any defect refuses the whole matrix with its fixed reason before a
// connection opens: a partial inventory never becomes a smaller healthy one.
func TestLbAltStatusRequestsRefuseAnIncompleteMatrix(t *testing.T) {
	edge := func(signalSettings *SignalSettings) *HostSettings {
		return &signalSettings.Hosts[len(signalSettings.Hosts)-1]
	}
	cases := []struct {
		name   string
		mutate func(settings *LbAltStatusSettings, signalSettings *SignalSettings, probeSettings *LbAltStatusProbeSettings)
		reason string
	}{
		{
			name: "an invalid block",
			mutate: func(settings *LbAltStatusSettings, _ *SignalSettings, _ *LbAltStatusProbeSettings) {
				settings.Invalid = true
			},
			reason: "target-denominator",
		},
		{
			name: "a missing target",
			mutate: func(settings *LbAltStatusSettings, _ *SignalSettings, _ *LbAltStatusProbeSettings) {
				settings.ExpectedTargets = 3
			},
			reason: "target-denominator",
		},
		{
			name: "no targets",
			mutate: func(settings *LbAltStatusSettings, _ *SignalSettings, _ *LbAltStatusProbeSettings) {
				settings.ExpectedTargets, settings.Targets = 0, nil
			},
			reason: "target-denominator",
		},
		{
			name: "more targets than the bound",
			mutate: func(_ *LbAltStatusSettings, _ *SignalSettings, probeSettings *LbAltStatusProbeSettings) {
				probeSettings.MaxTargetCount = 1
			},
			reason: "target-denominator",
		},
		{
			name: "no lb hidden prefix",
			mutate: func(settings *LbAltStatusSettings, _ *SignalSettings, _ *LbAltStatusProbeSettings) {
				settings.routePrefix = ""
			},
			reason: "route-prefix",
		},
		{
			name: "a prefix that is only a slash",
			mutate: func(settings *LbAltStatusSettings, _ *SignalSettings, _ *LbAltStatusProbeSettings) {
				settings.routePrefix = "/"
			},
			reason: "route-prefix",
		},
		{
			name: "a prefix with a query",
			mutate: func(settings *LbAltStatusSettings, _ *SignalSettings, _ *LbAltStatusProbeSettings) {
				settings.routePrefix = "/" + lbAltStatusTestPrefix + "?x"
			},
			reason: "route-prefix",
		},
		{
			name: "a prefix that climbs",
			mutate: func(settings *LbAltStatusSettings, _ *SignalSettings, _ *LbAltStatusProbeSettings) {
				settings.routePrefix = "/a/../b"
			},
			reason: "route-prefix",
		},
		{
			name: "no primary domain",
			mutate: func(_ *LbAltStatusSettings, signalSettings *SignalSettings, _ *LbAltStatusProbeSettings) {
				signalSettings.PublicDomain = ""
			},
			reason: "lb-authority",
		},
		{
			name: "an environment that is no dns label",
			mutate: func(_ *LbAltStatusSettings, signalSettings *SignalSettings, _ *LbAltStatusProbeSettings) {
				signalSettings.Environment = "Synthetic"
			},
			reason: "lb-authority",
		},
		{
			name: "a block count the inventory does not have",
			mutate: func(settings *LbAltStatusSettings, _ *SignalSettings, _ *LbAltStatusProbeSettings) {
				settings.ExpectedBlocks = 4
			},
			reason: "block-denominator",
		},
		{
			name: "a block service with no blocks",
			mutate: func(settings *LbAltStatusSettings, _ *SignalSettings, _ *LbAltStatusProbeSettings) {
				settings.BlockService = "web"
			},
			reason: "block-denominator",
		},
		{
			name: "no block service",
			mutate: func(settings *LbAltStatusSettings, _ *SignalSettings, _ *LbAltStatusProbeSettings) {
				settings.BlockService = ""
			},
			reason: "block-denominator",
		},
		{
			name: "more blocks than the bound",
			mutate: func(_ *LbAltStatusSettings, _ *SignalSettings, probeSettings *LbAltStatusProbeSettings) {
				probeSettings.MaxBlockCount = 4
			},
			reason: "block-denominator",
		},
		{
			name: "a block name that is no dns label",
			mutate: func(_ *LbAltStatusSettings, signalSettings *SignalSettings, _ *LbAltStatusProbeSettings) {
				signalSettings.LogServiceBlocks["connect"][0] = "Beta"
			},
			reason: "block-denominator",
		},
		{
			name: "a repeated target name",
			mutate: func(settings *LbAltStatusSettings, _ *SignalSettings, _ *LbAltStatusProbeSettings) {
				settings.Targets[1].Name = settings.Targets[0].Name
			},
			reason: "target-identity",
		},
		{
			name: "a target name that is no dns label",
			mutate: func(settings *LbAltStatusSettings, _ *SignalSettings, _ *LbAltStatusProbeSettings) {
				settings.Targets[0].Name = "Edge"
			},
			reason: "target-identity",
		},
		{
			name: "a host outside the inventory",
			mutate: func(settings *LbAltStatusSettings, _ *SignalSettings, _ *LbAltStatusProbeSettings) {
				settings.Targets[0].Host = "edge-absent"
			},
			reason: "target-identity",
		},
		{
			name: "an interface outside the host's inventory",
			mutate: func(settings *LbAltStatusSettings, _ *SignalSettings, _ *LbAltStatusProbeSettings) {
				settings.Targets[0].Interface = "eth9"
			},
			reason: "interface-inventory",
		},
		{
			name: "a padded interface",
			mutate: func(settings *LbAltStatusSettings, _ *SignalSettings, _ *LbAltStatusProbeSettings) {
				settings.Targets[0].Interface = "eth1 "
			},
			reason: "interface-inventory",
		},
		{
			name: "no families",
			mutate: func(settings *LbAltStatusSettings, _ *SignalSettings, _ *LbAltStatusProbeSettings) {
				settings.Targets[0].Families = nil
			},
			reason: "family-denominator",
		},
		{
			name: "a repeated family",
			mutate: func(settings *LbAltStatusSettings, _ *SignalSettings, _ *LbAltStatusProbeSettings) {
				settings.Targets[0].Families = []string{"ipv4", "ipv4"}
			},
			reason: "family-denominator",
		},
		{
			name: "a family the interface lacks",
			mutate: func(settings *LbAltStatusSettings, _ *SignalSettings, _ *LbAltStatusProbeSettings) {
				settings.Targets[1].Families = []string{"ipv4", "ipv6"}
			},
			reason: "family-address",
		},
		{
			name: "a private inventory address",
			mutate: func(_ *LbAltStatusSettings, signalSettings *SignalSettings, _ *LbAltStatusProbeSettings) {
				edge(signalSettings).PublicLB[0].IPv4Address = "10.0.0.10"
			},
			reason: "family-address",
		},
		{
			name: "an inventory address of the other family",
			mutate: func(_ *LbAltStatusSettings, signalSettings *SignalSettings, _ *LbAltStatusProbeSettings) {
				edge(signalSettings).PublicLB[0].IPv6Address = "198.51.100.12"
			},
			reason: "family-address",
		},
		{
			name: "an inventory family left unprobed",
			mutate: func(settings *LbAltStatusSettings, _ *SignalSettings, _ *LbAltStatusProbeSettings) {
				settings.Targets[0].Families = []string{"ipv4"}
			},
			reason: "unprobed-family",
		},
		{
			name: "one interface enrolled twice",
			mutate: func(settings *LbAltStatusSettings, _ *SignalSettings, _ *LbAltStatusProbeSettings) {
				settings.Targets[1].Interface = "eth1"
				settings.Targets[1].Families = []string{"ipv4", "ipv6"}
			},
			reason: "duplicate-path",
		},
		{
			name: "no attempt timeout",
			mutate: func(_ *LbAltStatusSettings, _ *SignalSettings, probeSettings *LbAltStatusProbeSettings) {
				probeSettings.AttemptTimeout = 0
			},
			reason: "probe-settings",
		},
		{
			name: "a negative attempt timeout",
			mutate: func(_ *LbAltStatusSettings, _ *SignalSettings, probeSettings *LbAltStatusProbeSettings) {
				probeSettings.AttemptTimeout = -time.Second
			},
			reason: "probe-settings",
		},
		{
			name: "an unbounded body",
			mutate: func(_ *LbAltStatusSettings, _ *SignalSettings, probeSettings *LbAltStatusProbeSettings) {
				probeSettings.ResponseBodyByteLimit = 0
			},
			reason: "probe-settings",
		},
	}
	for _, c := range cases {
		settings := lbAltStatusTestSettings()
		signalSettings := lbAltStatusTestSignalSettings(&syntheticSource{})
		probeSettings := DefaultLbAltStatusProbeSettings()
		c.mutate(&settings, &signalSettings, probeSettings)
		requests, reason := lbAltStatusRequests(settings, probeSettings, configFromSignalSettings(signalSettings))
		if reason != c.reason || requests != nil {
			t.Errorf("%s: reason %q with %d requests, want %q and none", c.name, reason, len(requests), c.reason)
		}
	}
	cfg := configFromSignalSettings(lbAltStatusTestSignalSettings(&syntheticSource{}))
	if requests, reason := lbAltStatusRequests(lbAltStatusTestSettings(), nil, cfg); reason != "probe-settings" || requests != nil {
		t.Errorf("no probe settings: reason %q with %d requests", reason, len(requests))
	}
}

// A pass runs at its settings' cadence, and at the default's when that is not
// positive, since the scheduler cannot run a non-positive interval.
func TestLbAltStatusCadenceTakesTheDefaultWhenNotPositive(t *testing.T) {
	defaultCadence := DefaultLbAltStatusProbeSettings().Cadence
	cases := []struct {
		settings *LbAltStatusProbeSettings
		cadence  time.Duration
	}{
		{settings: &LbAltStatusProbeSettings{Cadence: 30 * time.Second}, cadence: 30 * time.Second},
		{settings: &LbAltStatusProbeSettings{Cadence: 0}, cadence: defaultCadence},
		{settings: &LbAltStatusProbeSettings{Cadence: -time.Minute}, cadence: defaultCadence},
		{settings: nil, cadence: defaultCadence},
	}
	for _, c := range cases {
		if cadence := (lbAltStatusProbe{settings: c.settings}).cadence(); cadence != c.cadence || cadence <= 0 {
			t.Errorf("settings %+v gave a cadence of %s, want %s", c.settings, cadence, c.cadence)
		}
	}
}

// The monitor.yml block is off when absent and when it says so. A present
// block that does not decode is invalid rather than off, and the route prefix
// can come only from lb_hidden_prefixes, trimmed as Route53's path is.
func TestLbAltStatusSettingsFromYamlAreOffUnlessEnrolled(t *testing.T) {
	decode := func(text string) yaml.Node {
		document := struct {
			LbAltStatus yaml.Node `yaml:"lbalt_status"`
		}{}
		if err := yaml.Unmarshal([]byte(text), &document); err != nil {
			t.Fatal(err)
		}
		return document.LbAltStatus
	}
	lbHiddenPrefixes := []string{"/" + lbAltStatusTestPrefix + "/", "synthetic-second"}
	enrolled := `
lbalt_status:
  enabled: true
  expected_targets: 1
  targets:
    - name: edge-synthetic-eth1
      host: edge-synthetic
      interface: eth1
      families: [ipv4, ipv6]
  block_service: connect
  expected_blocks: 5
`
	want := LbAltStatusSettings{
		Enabled:         true,
		ExpectedTargets: 1,
		Targets: []LbAltStatusTargetSettings{
			{Name: "edge-synthetic-eth1", Host: "edge-synthetic", Interface: "eth1", Families: []string{"ipv4", "ipv6"}},
		},
		BlockService:   "connect",
		ExpectedBlocks: 5,
		routePrefix:    "/" + lbAltStatusTestPrefix,
	}
	if settings := lbAltStatusSettingsFromNode(decode(enrolled), lbHiddenPrefixes); !reflect.DeepEqual(settings, want) {
		t.Fatalf("enrolled settings = %+v, want %+v", settings, want)
	}
	if settings := lbAltStatusSettingsFromNode(decode("public_udp:\n  enabled: false\n"), lbHiddenPrefixes); !reflect.DeepEqual(settings, LbAltStatusSettings{}) {
		t.Fatalf("an absent block is not off: %+v", settings)
	}
	if settings := lbAltStatusSettingsFromNode(decode("lbalt_status:\n  enabled: false\n"), lbHiddenPrefixes); settings.Enabled || settings.Invalid {
		t.Fatalf("a disabled block is not off: %+v", settings)
	}
	for _, text := range []string{
		"lbalt_status:\n",
		"lbalt_status: true\n",
		"lbalt_status: [enabled]\n",
		"lbalt_status:\n  enabled: true\n  route_prefix: /synthetic-exposed\n",
		"lbalt_status:\n  enabled: true\n  expected_targets: many\n",
		"lbalt_status:\n  enabled: true\n  targets:\n    - name: edge-synthetic-eth1\n      ipv4_address: 198.51.100.10\n",
	} {
		if settings := lbAltStatusSettingsFromNode(decode(text), lbHiddenPrefixes); !settings.Enabled || !settings.Invalid {
			t.Errorf("a malformed block was not invalid: %q gave %+v", text, settings)
		}
	}
	unprefixed := lbAltStatusSettingsFromNode(decode(enrolled), nil)
	cfg := configFromSignalSettings(lbAltStatusTestSignalSettings(&syntheticSource{}))
	if _, reason := lbAltStatusRequests(unprefixed, DefaultLbAltStatusProbeSettings(), cfg); reason != "route-prefix" {
		t.Fatalf("an enrollment with no lb hidden prefix gave %q", reason)
	}
}

// A clone keeps the enrollment matrix apart from its source.
func TestLbAltStatusCloneRetainsIndependentTargets(t *testing.T) {
	settings := lbAltStatusTestSettings()
	cloned := cloneLbAltStatusSettings(settings)
	settings.Targets[0].Families[0] = "invalid"
	settings.Targets[1].Interface = "eth9"
	if cloned.Targets[0].Families[0] != "ipv4" || cloned.Targets[1].Interface != "eth2" || cloned.routePrefix != settings.routePrefix {
		t.Fatalf("a later mutation reached the clone: %+v", cloned)
	}
}

// A pass runs every request once with a fresh 32-hex correlation id, keeps
// their order, and gives each healthy answer a healthy verdict. A second pass
// draws new ids.
func TestLbAltStatusPassRunsEveryAttemptFreshly(t *testing.T) {
	var stateLock sync.Mutex
	seen := []LbAltStatusRequest{}
	source := &lbAltStatusTestSource{syntheticSource: &syntheticSource{}, observe: func(_ context.Context, request LbAltStatusRequest) (LbAltStatusObservation, error) {
		func() {
			stateLock.Lock()
			defer stateLock.Unlock()
			seen = append(seen, request)
		}()
		return lbAltStatusTestHealthy(request), nil
	}}
	attemptIds := map[string]bool{}
	for range 2 {
		requests, results, err := lbAltStatusTestPass(t, lbAltStatusTestSignalSettings(source), lbAltStatusProbe{})
		if err != nil || len(results) != len(requests) || len(requests) != 3*(1+len(lbAltStatusTestBlocks)) {
			t.Fatalf("a pass returned %d results for %d requests: %v", len(results), len(requests), err)
		}
		for index, result := range results {
			attempted := result.request
			attempted.AttemptId = ""
			if attempted != requests[index] {
				t.Fatalf("result %d is for %+v, want %+v", index, result.request, requests[index])
			}
			if len(result.request.AttemptId) != 32 || attemptIds[result.request.AttemptId] {
				t.Fatalf("an attempt id was reused or malformed: %q", result.request.AttemptId)
			}
			attemptIds[result.request.AttemptId] = true
			if !result.verdict.healthy || result.verdict.reason != "healthy" {
				t.Fatalf("a healthy answer for %s got %+v", result.request.Route, result.verdict)
			}
		}
	}
	if len(seen) != 2*3*(1+len(lbAltStatusTestBlocks)) {
		t.Fatalf("the source saw %d attempts", len(seen))
	}
}

// Each attempt of a pass runs under the pass's own settings and is bounded by
// their attempt timeout: a raised timeout reaches every attempt's context,
// which the source records.
func TestLbAltStatusPassBoundsEachAttemptByItsSettings(t *testing.T) {
	probeSettings := DefaultLbAltStatusProbeSettings()
	probeSettings.AttemptTimeout = time.Hour
	var stateLock sync.Mutex
	attemptDeadlines := []time.Time{}
	unboundedCount := 0
	source := &lbAltStatusTestSource{syntheticSource: &syntheticSource{}, observe: func(ctx context.Context, request LbAltStatusRequest) (LbAltStatusObservation, error) {
		func() {
			stateLock.Lock()
			defer stateLock.Unlock()
			if deadline, ok := ctx.Deadline(); ok {
				attemptDeadlines = append(attemptDeadlines, deadline)
			} else {
				unboundedCount++
			}
		}()
		return lbAltStatusTestHealthy(request), nil
	}}
	started := time.Now()
	requests, results, err := lbAltStatusTestPass(t, lbAltStatusTestSignalSettings(source), lbAltStatusProbe{settings: probeSettings})
	finished := time.Now()
	if err != nil || len(results) != len(requests) || unboundedCount != 0 || len(attemptDeadlines) != len(requests) {
		t.Fatalf("a pass of %d requests gave %d results and %d bounded and %d unbounded attempts: %v", len(requests), len(results), len(attemptDeadlines), unboundedCount, err)
	}
	for index, deadline := range attemptDeadlines {
		if deadline.Before(started.Add(probeSettings.AttemptTimeout)) || deadline.After(finished.Add(probeSettings.AttemptTimeout)) {
			t.Fatalf("attempt %d ran under a deadline %s after the start", index, deadline.Sub(started))
		}
	}
}

// A canceled pass joins the attempts in flight, starts no other and publishes
// nothing. The sources' entry is the barrier.
func TestLbAltStatusPassCancellationJoinsWithoutResults(t *testing.T) {
	entered := make(chan struct{}, 2)
	var exited atomic.Int32
	var calls atomic.Int32
	source := &lbAltStatusTestSource{syntheticSource: &syntheticSource{}, observe: func(ctx context.Context, request LbAltStatusRequest) (LbAltStatusObservation, error) {
		calls.Add(1)
		entered <- struct{}{}
		<-ctx.Done()
		exited.Add(1)
		return lbAltStatusTestHealthy(request), ctx.Err()
	}}
	probeSettings := DefaultLbAltStatusProbeSettings()
	probeSettings.WorkerCount = 2
	env, err := newProbeEnv(lbAltStatusTestSignalSettings(source))
	if err != nil {
		t.Fatal(err)
	}
	requests := lbAltStatusTestRequests(t, env.cfg)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	type passResult struct {
		results []lbAltStatusResult
		err     error
	}
	done := make(chan passResult, 1)
	go func() {
		results, err := lbAltStatusProbe{settings: probeSettings}.observe(ctx, env.runner, requests)
		done <- passResult{results: results, err: err}
	}()
	for range 2 {
		select {
		case <-entered:
		case <-time.After(5 * time.Second):
			t.Fatal("the source-entry barrier was not reached")
		}
	}
	cancel()
	select {
	case result := <-done:
		if !errors.Is(result.err, context.Canceled) || result.results != nil || exited.Load() != 2 || calls.Load() != 2 {
			t.Fatalf("cancellation published %d results, err %v, with %d of %d calls exited", len(result.results), result.err, exited.Load(), calls.Load())
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the canceled pass did not join its attempts")
	}
}

// A source that answers healthy after its attempt expired cannot establish
// health: the attempt's own expiry is captured before its cleanup.
func TestLbAltStatusLatePositiveCannotOutliveItsAttempt(t *testing.T) {
	var returned atomic.Int32
	source := &lbAltStatusTestSource{syntheticSource: &syntheticSource{}, observe: func(ctx context.Context, request LbAltStatusRequest) (LbAltStatusObservation, error) {
		// an explicit state transition after entry and before the positive
		// answer; no sleep or scheduling is involved
		close(ctx.(*publicUdpTestDeadlineContext).expired)
		returned.Add(1)
		return lbAltStatusTestHealthy(request), nil
	}}
	probe := lbAltStatusProbe{attemptContext: func(parent context.Context) (context.Context, context.CancelFunc) {
		return &publicUdpTestDeadlineContext{Context: parent, expired: make(chan struct{})}, func() {}
	}}
	requests, results, err := lbAltStatusTestPass(t, lbAltStatusTestSignalSettings(source), probe)
	if err != nil || len(results) != len(requests) || int(returned.Load()) != len(requests) {
		t.Fatalf("the pass did not run every expiring attempt: %v", err)
	}
	for _, result := range results {
		if result.verdict.healthy || result.verdict.path || result.verdict.reason != "observation-incomplete" {
			t.Fatalf("a late positive answer gave %+v", result.verdict)
		}
	}
}

// A source without the lb status capability leaves every attempt unknown; it
// never falls back to a native connection.
func TestLbAltStatusMissingSourceCapabilityIsUnknown(t *testing.T) {
	_, results, err := lbAltStatusTestPass(t, lbAltStatusTestSignalSettings(&syntheticSource{}), lbAltStatusProbe{})
	if err != nil || len(results) == 0 {
		t.Fatalf("the pass failed: %v", err)
	}
	for _, result := range results {
		if result.verdict.healthy || result.verdict.path || result.verdict.reason != "source-unavailable" {
			t.Fatalf("a missing capability gave %+v", result.verdict)
		}
	}
}

// The shared host-scope guard runs before the transport: an excluded host is
// never contacted, and neither is an address shared with a paused owner, while
// the host's other addresses still are.
func TestLbAltStatusExcludedHostAndSharedPausedEndpointMakeNoCall(t *testing.T) {
	for _, shared := range []bool{false, true} {
		var stateLock sync.Mutex
		addresses := map[string]int{}
		source := &lbAltStatusTestSource{syntheticSource: &syntheticSource{}, observe: func(_ context.Context, request LbAltStatusRequest) (LbAltStatusObservation, error) {
			func() {
				stateLock.Lock()
				defer stateLock.Unlock()
				addresses[request.Address]++
			}()
			return lbAltStatusTestHealthy(request), nil
		}}
		signalSettings := lbAltStatusTestSignalSettings(source)
		if shared {
			signalSettings.disabledHosts = []HostSettings{{Name: "paused-synthetic", scopeEndpoints: []string{"198.51.100.10"}}}
		} else {
			signalSettings.ExcludedHosts = []string{"edge-synthetic"}
		}
		_, results, err := lbAltStatusTestPass(t, signalSettings, lbAltStatusProbe{})
		if err != nil {
			t.Fatal(err)
		}
		want := map[string]int{}
		if shared {
			want = map[string]int{"2001:db8:10::10": 1 + len(lbAltStatusTestBlocks), "198.51.100.11": 1 + len(lbAltStatusTestBlocks)}
		}
		if !reflect.DeepEqual(addresses, want) {
			t.Fatalf("shared=%t: the transport saw %v, want %v", shared, addresses, want)
		}
		for _, result := range results {
			if _, admitted := want[result.request.Address]; result.verdict.healthy != admitted {
				t.Fatalf("shared=%t: %s gave %+v", shared, result.request.Address, result.verdict)
			}
		}
	}
}

// Only a fresh, correlated and bounded answer meeting its route's contract is
// healthy. Forged, partial or contradictory evidence is unknown; an answer
// outside the contract, or a failed exchange after a dial, is a path failure
// with its fixed reason. A redirect is a path failure, though Route53 counts
// it healthy.
func TestLbAltStatusVerdictRefusesForgedOrPartialEvidence(t *testing.T) {
	cfg := configFromSignalSettings(lbAltStatusTestSignalSettings(&syntheticSource{}))
	requests := lbAltStatusTestRequests(t, cfg)
	lbRequest, blockRequest := requests[0], requests[1]
	lbRequest.AttemptId, blockRequest.AttemptId = "synthetic-attempt-lb", "synthetic-attempt-block"
	settings := DefaultLbAltStatusProbeSettings()
	cases := []struct {
		name    string
		request LbAltStatusRequest
		mutate  func(*LbAltStatusObservation)
		err     error
		path    bool
		reason  string
	}{
		{name: "a healthy lb answer", request: lbRequest, mutate: func(*LbAltStatusObservation) {}, reason: "healthy"},
		{name: "nginx's lb answer", request: lbRequest, mutate: func(observed *LbAltStatusObservation) { observed.LbAltFront = false }, reason: "healthy"},
		{name: "a healthy block answer", request: blockRequest, mutate: func(*LbAltStatusObservation) {}, reason: "healthy"},
		{
			name: "a block answer with no front header", request: blockRequest,
			mutate: func(observed *LbAltStatusObservation) { observed.FrontReadiness = LbAltStatusFrontReadinessAbsent }, reason: "healthy",
		},
		{name: "another attempt", request: lbRequest, mutate: func(observed *LbAltStatusObservation) { observed.AttemptId = "previous" }, reason: "observation-authority-invalid"},
		{name: "another pin", request: lbRequest, mutate: func(observed *LbAltStatusObservation) { observed.RequestedTuple = "198.51.100.12:80" }, reason: "observation-authority-invalid"},
		{name: "an impossible status", request: lbRequest, mutate: func(observed *LbAltStatusObservation) { observed.StatusCode = 700 }, reason: "observation-authority-invalid"},
		{name: "a negative body", request: lbRequest, mutate: func(observed *LbAltStatusObservation) { observed.ResponseBytes = -1 }, reason: "observation-authority-invalid"},
		{
			name: "a body past its bound", request: lbRequest,
			mutate: func(observed *LbAltStatusObservation) { observed.ResponseBytes = settings.ResponseBodyByteLimit + 1 }, reason: "observation-authority-invalid",
		},
		{name: "a connection never dialed", request: lbRequest, mutate: func(observed *LbAltStatusObservation) { observed.Dialed = false }, reason: "observation-authority-invalid"},
		{name: "an unknown failure", request: lbRequest, mutate: func(observed *LbAltStatusObservation) { observed.Failure = "synthetic-private" }, reason: "observation-authority-invalid"},
		{name: "a reused connection", request: lbRequest, mutate: func(observed *LbAltStatusObservation) { observed.FreshConnection = false }, reason: "observation-incomplete"},
		{name: "no response", request: lbRequest, mutate: func(observed *LbAltStatusObservation) { observed.StatusCode = 0 }, reason: "observation-incomplete"},
		{name: "an unverified certificate", request: blockRequest, mutate: func(observed *LbAltStatusObservation) { observed.TlsVerified = false }, reason: "observation-incomplete"},
		{name: "an attempt error", request: lbRequest, mutate: func(*LbAltStatusObservation) {}, err: errors.New("synthetic"), reason: "observation-incomplete"},
		{
			name: "a connect failure with no dial", request: lbRequest,
			mutate: func(observed *LbAltStatusObservation) {
				observed.Dialed, observed.FreshConnection, observed.Failure = false, false, LbAltStatusFailureConnect
			},
			reason: "observation-incomplete",
		},
		{
			name: "a request failure with no connection", request: lbRequest,
			mutate: func(observed *LbAltStatusObservation) {
				observed.FreshConnection, observed.Failure = false, LbAltStatusFailureRequest
			},
			reason: "observation-incomplete",
		},
		{name: "no route", request: lbRequest, mutate: func(observed *LbAltStatusObservation) { observed.Failure = LbAltStatusFailureRoute }, reason: "observer-route-unavailable"},
		{name: "no certificate", request: blockRequest, mutate: func(observed *LbAltStatusObservation) { observed.Failure = LbAltStatusFailureTls }, reason: "tls-authentication-unverified"},
		{name: "a refused request", request: lbRequest, mutate: func(observed *LbAltStatusObservation) { observed.Failure = LbAltStatusFailureConfiguration }, reason: "configuration-invalid"},
		{name: "no source", request: lbRequest, mutate: func(observed *LbAltStatusObservation) { observed.Failure = LbAltStatusFailureSource }, reason: "source-unavailable"},
		{
			name: "a refused connection", request: lbRequest,
			mutate: func(observed *LbAltStatusObservation) {
				observed.FreshConnection, observed.Failure = false, LbAltStatusFailureConnect
			},
			path: true, reason: "connect-failed",
		},
		{name: "a timeout", request: blockRequest, mutate: func(observed *LbAltStatusObservation) { observed.Failure = LbAltStatusFailureTimeout }, path: true, reason: "timeout"},
		{name: "a broken exchange", request: lbRequest, mutate: func(observed *LbAltStatusObservation) { observed.Failure = LbAltStatusFailureRequest }, path: true, reason: "request-failed"},
		{
			name: "a body too large", request: lbRequest,
			mutate: func(observed *LbAltStatusObservation) { observed.Failure = LbAltStatusFailureBudget }, path: true, reason: "response-budget-exhausted",
		},
		{name: "a moved route", request: lbRequest, mutate: func(observed *LbAltStatusObservation) { observed.StatusCode = 301 }, path: true, reason: "redirect"},
		{name: "a found route", request: lbRequest, mutate: func(observed *LbAltStatusObservation) { observed.StatusCode = 302 }, path: true, reason: "redirect"},
		{name: "a permanent redirect", request: blockRequest, mutate: func(observed *LbAltStatusObservation) { observed.StatusCode = 308 }, path: true, reason: "redirect"},
		{name: "an unavailable route", request: lbRequest, mutate: func(observed *LbAltStatusObservation) { observed.StatusCode = 503 }, path: true, reason: "http-status"},
		{name: "a created response", request: lbRequest, mutate: func(observed *LbAltStatusObservation) { observed.StatusCode = 201 }, path: true, reason: "http-status"},
		{name: "a text answer", request: lbRequest, mutate: func(observed *LbAltStatusObservation) { observed.JsonContentType = false }, path: true, reason: "content-type"},
		{name: "a body that is no object", request: lbRequest, mutate: func(observed *LbAltStatusObservation) { observed.BodyJson = false }, path: true, reason: "body-invalid"},
		{
			name: "a draining answer", request: lbRequest,
			mutate: func(observed *LbAltStatusObservation) { observed.Status = LbAltStatusStateDraining }, path: true, reason: "status-not-ok",
		},
		{
			name: "an absent status", request: blockRequest,
			mutate: func(observed *LbAltStatusObservation) { observed.Status = LbAltStatusStateAbsent }, path: true, reason: "status-not-ok",
		},
		{name: "another block's answer", request: blockRequest, mutate: func(observed *LbAltStatusObservation) { observed.BlockMatched = false }, path: true, reason: "block-mismatch"},
		{
			name: "a front not ready", request: blockRequest,
			mutate: func(observed *LbAltStatusObservation) { observed.FrontReadiness = LbAltStatusFrontReadinessNotReady }, path: true, reason: "front-not-ready",
		},
		{
			name: "a malformed front header", request: blockRequest,
			mutate: func(observed *LbAltStatusObservation) { observed.FrontReadiness = LbAltStatusFrontReadinessInvalid }, path: true, reason: "front-readiness-invalid",
		},
	}
	for _, c := range cases {
		observed := lbAltStatusTestHealthy(c.request)
		c.mutate(&observed)
		verdict := lbAltStatusEvaluate(c.request, observed, c.err, settings)
		want := lbAltStatusVerdict{healthy: c.reason == "healthy", path: c.path, reason: c.reason}
		if verdict != want {
			t.Errorf("%s: verdict %+v, want %+v", c.name, verdict, want)
		}
	}
}
