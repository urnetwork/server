package monitor

// The lbalt-status signal (signal_lbalt_status.go, SIGNALS.md §16.10): its
// findings for each verdict, its enrollment from monitor.yml and services.yml,
// the clones and freshness category of that enrollment, and the host scope of
// the addresses it contacts.

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/urnetwork/server"
)

// Signal settings with the synthetic matrix enrolled.
func lbAltStatusTestEnrolledSettings(source SignalSource) SignalSettings {
	settings := lbAltStatusTestSignalSettings(source)
	settings.LbAltStatus = lbAltStatusTestSettings()
	return settings
}

// A source answering every attempt healthy unless answer replaces it, and
// counting the attempts.
func lbAltStatusTestSignalSource(answer func(LbAltStatusRequest, LbAltStatusObservation) LbAltStatusObservation) (*lbAltStatusTestSource, *atomic.Int32) {
	calls := &atomic.Int32{}
	source := &lbAltStatusTestSource{syntheticSource: &syntheticSource{}, observe: func(_ context.Context, request LbAltStatusRequest) (LbAltStatusObservation, error) {
		calls.Add(1)
		observation := lbAltStatusTestHealthy(request)
		if answer != nil {
			observation = answer(request, observation)
		}
		return observation, nil
	}}
	return source, calls
}

// The signal is registered with its catalog identity and its probe's cadence.
func TestLbAltStatusSignalIsRegistered(t *testing.T) {
	registered := 0
	for _, signal := range NewSignals() {
		if signal.Key() != "lbalt-status" {
			continue
		}
		registered++
		if signal.Number() != "16.10" || signal.ID() != "lb/lbalt-status" || signal.Cadence() != DefaultLbAltStatusProbeSettings().Cadence {
			t.Fatalf("lbalt-status is registered as %s %s every %s", signal.Number(), signal.ID(), signal.Cadence())
		}
	}
	if registered != 1 {
		t.Fatalf("lbalt-status is registered %d times", registered)
	}
}

// An absent or disabled enrollment is unarmed: no contact and no recovery.
func TestLbAltStatusSignalDisabledEnrollmentMakesNoCall(t *testing.T) {
	for _, enrollment := range []LbAltStatusSettings{{}, func() LbAltStatusSettings {
		settings := lbAltStatusTestSettings()
		settings.Enabled = false
		return settings
	}()} {
		source, calls := lbAltStatusTestSignalSource(nil)
		settings := lbAltStatusTestSignalSettings(source)
		settings.LbAltStatus = enrollment
		env, err := newProbeEnv(settings)
		if err != nil {
			t.Fatal(err)
		}
		findings, err := (lbAltStatusProbe{}).check(context.Background(), env)
		if err != nil || len(findings) != 0 || calls.Load() != 0 {
			t.Fatalf("an unarmed enrollment gave %d findings and %d calls: %v", len(findings), calls.Load(), err)
		}
	}
}

// A refused matrix is one inventory finding with its fixed reason, and no
// interface is contacted.
func TestLbAltStatusSignalRefusedMatrixIsOneInventoryFinding(t *testing.T) {
	source, calls := lbAltStatusTestSignalSource(nil)
	settings := lbAltStatusTestEnrolledSettings(source)
	settings.LbAltStatus.ExpectedBlocks = 4
	alerts, err := NewLbAltStatusSignal().Run(context.Background(), settings)
	if err != nil || calls.Load() != 0 || len(alerts) != 1 {
		t.Fatalf("a refused matrix gave %d alerts and %d calls: %v", len(alerts), calls.Load(), err)
	}
	alert := requireAlertClass(t, alerts, "lbalt-status-inventory")
	if alert.Observed != "inventory_complete=false reason=block-denominator" || alert.Sustain != 1 || alert.Target != "lbalt-status-inventory" {
		t.Fatalf("the inventory finding is %+v", alert)
	}
}

// A healthy pass raises nothing and resolves both classes of every target,
// one target per interface family, route and block.
func TestLbAltStatusSignalHealthyPassResolvesEveryTarget(t *testing.T) {
	source, calls := lbAltStatusTestSignalSource(nil)
	settings := lbAltStatusTestEnrolledSettings(source)
	alerts, err := NewLbAltStatusSignal().Run(context.Background(), settings)
	attempts := 3 * (1 + len(lbAltStatusTestBlocks))
	if err != nil || len(alerts) != 0 || int(calls.Load()) != attempts {
		t.Fatalf("a healthy pass gave %d alerts after %d calls: %v", len(alerts), calls.Load(), err)
	}
	env, err := newProbeEnv(settings)
	if err != nil {
		t.Fatal(err)
	}
	findings, err := (lbAltStatusProbe{}).check(context.Background(), env)
	if err != nil {
		t.Fatal(err)
	}
	targets := map[string][]string{}
	for _, result := range findings {
		if !result.healthy {
			t.Fatalf("a healthy pass gave %s %s", result.class, result.observed)
		}
		targets[result.target] = append(targets[result.target], result.class)
	}
	if len(targets) != 1+attempts {
		t.Fatalf("a healthy pass resolved %d targets, want %d", len(targets), 1+attempts)
	}
	for _, target := range []string{
		"edge-synthetic/lbalt-status/edge-synthetic-eth1/ipv4/lb",
		"edge-synthetic/lbalt-status/edge-synthetic-eth1/ipv6/block/connect/g3",
		"edge-synthetic/lbalt-status/edge-synthetic-eth2/ipv4/block/connect/beta",
	} {
		if classes := targets[target]; strings.Join(classes, ",") != "lbalt-status-path,lbalt-status-observation" {
			t.Fatalf("%s resolved %v", target, classes)
		}
	}
}

// A redirect on one family's lb route is a path finding held for two
// evaluations on that target alone, naming the route, the reason and the
// status code, and none of the prefix, the lb name or the pinned address.
func TestLbAltStatusSignalRedirectIsAPathFinding(t *testing.T) {
	source, _ := lbAltStatusTestSignalSource(func(request LbAltStatusRequest, observation LbAltStatusObservation) LbAltStatusObservation {
		if request.Target == "edge-synthetic-eth1" && request.Family == "ipv6" && request.Route == lbAltStatusRouteLb {
			observation.StatusCode = 301
		}
		return observation
	})
	alerts, err := NewLbAltStatusSignal().Run(context.Background(), lbAltStatusTestEnrolledSettings(source))
	if err != nil || len(alerts) != 1 {
		t.Fatalf("one redirect gave %d alerts: %v", len(alerts), err)
	}
	alert := requireAlertClass(t, alerts, "lbalt-status-path")
	if alert.Target != "edge-synthetic/lbalt-status/edge-synthetic-eth1/ipv6/lb" || alert.Sustain != 2 || alert.PageSustain != 0 ||
		alert.Observed != "verified=false route=lb reason=redirect status_code=301 status=ok front_readiness=absent lbalt_front=true" {
		t.Fatalf("the redirect finding is %s %s sustain %d", alert.Target, alert.Observed, alert.Sustain)
	}
	requireAlertOmits(t, alert, lbAltStatusTestPrefix, lbAltStatusTestServerName, "2001:db8:10::10", "198.51.100.10")
}

// A block that reports its front not ready fails its own block route, which
// the other blocks' healthy answers cannot resolve.
func TestLbAltStatusSignalBlockFailureKeepsItsBlock(t *testing.T) {
	source, _ := lbAltStatusTestSignalSource(func(request LbAltStatusRequest, observation LbAltStatusObservation) LbAltStatusObservation {
		if request.Block == "g2" {
			observation.FrontReadiness = LbAltStatusFrontReadinessNotReady
		}
		return observation
	})
	settings := lbAltStatusTestEnrolledSettings(source)
	env, err := newProbeEnv(settings)
	if err != nil {
		t.Fatal(err)
	}
	findings, err := (lbAltStatusProbe{}).check(context.Background(), env)
	if err != nil {
		t.Fatal(err)
	}
	broken := map[string]bool{}
	for _, result := range findings {
		if !result.healthy {
			if result.class != "lbalt-status-path" || !strings.HasSuffix(result.target, "/block/connect/g2") || !strings.Contains(result.frame, "block=g2") ||
				!strings.Contains(result.observed, "reason=front-not-ready") || !strings.Contains(result.observed, "front_readiness=not-ready") {
				t.Fatalf("a front that is not ready gave %s %s %s", result.class, result.target, result.observed)
			}
			broken[result.target] = true
		}
	}
	if len(broken) != 3 {
		t.Fatalf("%d block targets failed, want g2 in each of 3 families", len(broken))
	}
	for _, result := range findings {
		if result.healthy && broken[result.target] {
			t.Fatalf("a healthy finding resolves the failed %s", result.target)
		}
	}
}

// Anything short of a verdict is an unknown observation, never a path
// failure or recovery: here a source without the capability.
func TestLbAltStatusSignalUnknownIsAnObservation(t *testing.T) {
	alerts, err := NewLbAltStatusSignal().Run(context.Background(), lbAltStatusTestEnrolledSettings(&syntheticSource{}))
	if err != nil || len(alerts) != 3*(1+len(lbAltStatusTestBlocks)) {
		t.Fatalf("a missing capability gave %d alerts: %v", len(alerts), err)
	}
	for _, alert := range alerts {
		if alert.Class != "lbalt-status-observation" || alert.Observed != "verified=false reason=source-unavailable" || alert.Sustain != 1 {
			t.Fatalf("a missing capability gave %s %s", alert.Class, alert.Observed)
		}
	}
}

// Only the closed domains reach a finding: a status or readiness a source
// made up renders as other or invalid, never as its text.
func TestLbAltStatusSignalRendersOnlyFixedDomains(t *testing.T) {
	const private = "synthetic-private-status-text"
	source, _ := lbAltStatusTestSignalSource(func(request LbAltStatusRequest, observation LbAltStatusObservation) LbAltStatusObservation {
		if request.Route == lbAltStatusRouteBlock && request.Block == "g1" {
			observation.Status = LbAltStatusState(private)
			observation.FrontReadiness = LbAltStatusFrontReadiness(private)
		}
		return observation
	})
	alerts, err := NewLbAltStatusSignal().Run(context.Background(), lbAltStatusTestEnrolledSettings(source))
	if err != nil || len(alerts) != 3 {
		t.Fatalf("a made-up status gave %d alerts: %v", len(alerts), err)
	}
	for _, alert := range alerts {
		if alert.Observed != "verified=false route=block reason=status-not-ok status_code=200 status=other front_readiness=invalid lbalt_front=false" {
			t.Fatalf("a made-up status rendered %s", alert.Observed)
		}
		requireAlertOmits(t, alert, private)
	}
}

// A canceled pass joins its attempts and publishes nothing. The sources'
// entry is the barrier.
func TestLbAltStatusSignalCancellationPublishesNothing(t *testing.T) {
	entered := make(chan struct{}, 64)
	source := &lbAltStatusTestSource{syntheticSource: &syntheticSource{}, observe: func(ctx context.Context, request LbAltStatusRequest) (LbAltStatusObservation, error) {
		entered <- struct{}{}
		<-ctx.Done()
		return lbAltStatusTestHealthy(request), ctx.Err()
	}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	type runResult struct {
		alerts Alerts
		err    error
	}
	done := make(chan runResult, 1)
	go func() {
		alerts, err := NewLbAltStatusSignal().Run(ctx, lbAltStatusTestEnrolledSettings(source))
		done <- runResult{alerts: alerts, err: err}
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the source-entry barrier was not reached")
	}
	cancel()
	select {
	case result := <-done:
		if !errors.Is(result.err, context.Canceled) || len(result.alerts) != 0 {
			t.Fatalf("a canceled pass gave %d alerts: %v", len(result.alerts), result.err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the canceled pass did not join its attempts")
	}
}

// monitor.yml lbalt_status and services.yml lb_hidden_prefixes reach the
// loaded settings, and from them the matrix: pinned to the inventory
// addresses, under the first prefix, for the active blocks.
func TestLbAltStatusSettingsLoadFromMonitorAndServicesYaml(t *testing.T) {
	t.Setenv("WARP_ENV", "synthetic")
	t.Setenv("WARP_HOME", t.TempDir())
	t.Setenv("WARP_VAULT_HOME", t.TempDir())
	t.Setenv("WARP_CONFIG_HOME", t.TempDir())
	popMonitor := server.Vault.PushSimpleResource("monitor.yml", []byte(`ssh:
  user: monitor
hosts:
  - name: pg-1
    overlay_ip: 192.0.2.10
    roles: [pg-primary]
  - name: synthetic-edge-a
    overlay_ip: 192.0.2.11
    roles: [edge]
lbalt_status:
  enabled: true
  expected_targets: 1
  targets:
    - name: synthetic-edge-a-eth1
      host: synthetic-edge-a
      interface: eth1
      families: [ipv4, ipv6]
  block_service: connect
  expected_blocks: 2
`))
	defer popMonitor()
	popServices := server.Vault.PushSimpleResource("services.yml", []byte(`domain: edge.example
lb_hidden_prefixes: [`+lbAltStatusTestPrefix+`, synthetic-second]
versions:
  - lb:
      interfaces:
        synthetic-edge-a:
          eth1:
            ipv4: 198.51.100.10
            ipv6: 2001:db8:10::10
    services:
      connect:
        blocks:
          - g1: 50
          - g2: 50
`))
	defer popServices()

	settings, err := loadSignalSettingsSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	want := LbAltStatusSettings{
		Enabled:         true,
		ExpectedTargets: 1,
		Targets: []LbAltStatusTargetSettings{
			{Name: "synthetic-edge-a-eth1", Host: "synthetic-edge-a", Interface: "eth1", Families: []string{"ipv4", "ipv6"}},
		},
		BlockService:   "connect",
		ExpectedBlocks: 2,
		routePrefix:    "/" + lbAltStatusTestPrefix,
	}
	if !lbAltStatusTestSettingsEqual(settings.LbAltStatus, want) {
		t.Fatalf("loaded %+v, want %+v", settings.LbAltStatus, want)
	}
	requests, reason := lbAltStatusRequests(configFromSignalSettings(settings).lbAltStatus, DefaultLbAltStatusProbeSettings(), configFromSignalSettings(settings))
	if reason != "" || len(requests) != 2*3 {
		t.Fatalf("the loaded matrix gave %d requests: %q", len(requests), reason)
	}
	for _, request := range requests {
		if !strings.HasPrefix(request.Path, "/"+lbAltStatusTestPrefix+"/") || request.ServerName != "synthetic-lb.edge.example" ||
			request.Address != "198.51.100.10" && request.Address != "2001:db8:10::10" {
			t.Fatalf("the loaded matrix requested %+v", request)
		}
	}
}

// Whether two enrollments are the same, the private route prefix included.
func lbAltStatusTestSettingsEqual(a LbAltStatusSettings, b LbAltStatusSettings) bool {
	if a.Enabled != b.Enabled || a.ExpectedTargets != b.ExpectedTargets || a.BlockService != b.BlockService ||
		a.ExpectedBlocks != b.ExpectedBlocks || a.Invalid != b.Invalid || a.routePrefix != b.routePrefix || len(a.Targets) != len(b.Targets) {
		return false
	}
	for index := range a.Targets {
		if a.Targets[index].Name != b.Targets[index].Name || a.Targets[index].Host != b.Targets[index].Host ||
			a.Targets[index].Interface != b.Targets[index].Interface || !slices.Equal(a.Targets[index].Families, b.Targets[index].Families) {
			return false
		}
	}
	return true
}

// The enrollment is cloned into the defaults, the probe configuration and the
// startup copy the freshness check compares, so a caller's later mutation
// reaches none of them.
func TestLbAltStatusSettingsAreClonedAtEveryHandoff(t *testing.T) {
	settings := lbAltStatusTestEnrolledSettings(&syntheticSource{})
	var startupLock sync.Mutex
	startupTargets := []LbAltStatusTargetSettings{}
	settings.SettingsGenerationCheck = func(_ context.Context, startup SignalSettings) (bool, error) {
		startupLock.Lock()
		defer startupLock.Unlock()
		startupTargets = startup.LbAltStatus.Targets
		return true, nil
	}
	withDefaults := settings.withDefaults()
	cfg := configFromSignalSettings(settings)
	settings.LbAltStatus.Targets[0].Families[0] = "invalid"
	settings.LbAltStatus.Targets[1].Interface = "eth9"
	if _, err := cfg.routerGenerationCheck(context.Background()); err != nil {
		t.Fatal(err)
	}
	for name, targets := range map[string][]LbAltStatusTargetSettings{
		"the defaults":              withDefaults.LbAltStatus.Targets,
		"the probe configuration":   cfg.lbAltStatus.Targets,
		"the startup configuration": startupTargets,
	} {
		if targets[0].Families[0] != "ipv4" || targets[1].Interface != "eth2" {
			t.Errorf("a later mutation reached %s: %+v", name, targets)
		}
	}
	if cfg.lbAltStatus.routePrefix != "/"+lbAltStatusTestPrefix {
		t.Fatal("the probe configuration lost the route prefix")
	}
}

// A changed enrollment, or a changed route prefix alone, is a stale settings
// generation under its own category, which renders neither value.
func TestLbAltStatusSettingsHaveAFreshnessCategory(t *testing.T) {
	for _, mutate := range []func(*LbAltStatusSettings){
		func(settings *LbAltStatusSettings) { settings.Targets[0].Interface = "eth3" },
		func(settings *LbAltStatusSettings) { settings.routePrefix = "/synthetic-rotated-prefix" },
	} {
		settings, current := settingsFreshnessFixture(t)
		settings.LbAltStatus = lbAltStatusTestSettings()
		current.LbAltStatus = lbAltStatusTestSettings()
		mutate(&current.LbAltStatus)
		alerts, err := NewSettingsFreshnessSignal().Run(context.Background(), settings)
		if err != nil {
			t.Fatal(err)
		}
		alert := requireAlertClass(t, alerts, "settings-generation-stale")
		if alert.Observed != "settings_generation_current=false mismatch_categories=lbalt_status" {
			t.Fatalf("a changed enrollment gave %s", alert.Observed)
		}
		requireAlertOmits(t, alert, lbAltStatusTestPrefix, "synthetic-rotated-prefix", "eth3")
	}
}
