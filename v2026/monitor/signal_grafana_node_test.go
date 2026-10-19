package monitor

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestGrafanaNodeSignalSyntheticOOMLANLoss(t *testing.T) {
	ndiscAt := time.Date(2026, 8, 31, 12, 47, 55, 0, time.UTC).Unix()
	source := &syntheticSource{hostFn: func(host HostSettings, command string) (string, error) {
		if host.Name != "grafana.example.test" || !strings.Contains(command, grafanaNodeMarker) {
			return "", fmt.Errorf("unexpected Grafana node command for %s", host.Name)
		}
		for _, want := range []string{
			"expected_lan_address='192.0.2.196'",
			"postgres_lan_address='192.0.2.43'",
			"grafana_unit_pattern='warp-synthetic-grafana-*-g1.service'",
			"vector(1)",
			"-o short-unix",
			"networkd_ndisc_last_epoch",
			"memory_pressure_before_ndisc_epoch",
			"oom_after_ndisc_epoch",
		} {
			if !strings.Contains(command, want) {
				return "", fmt.Errorf("Grafana node command missing %q", want)
			}
		}
		return grafanaNodeFixture(grafanaNodeSample{
			unitActive: true, lanPresent: false, networkFailedLinks: 1,
			schedulerTCP: false, databaseTCP: 0, queryExit: 28, queryHTTP: 0, querySeconds: 4,
			networkdNDiscTimeouts: 2, networkdNDiscLastEpoch: ndiscAt,
			memoryPressureEvents: 11, memoryPressureBeforeNDiscEpoch: ndiscAt - 54,
			memoryPressureAfterNDiscEpoch: ndiscAt + 2,
			oomKills:                      1, oomAfterNDiscEpoch: ndiscAt + 329,
		}), nil
	}}
	settings := syntheticSettings(source)
	settings.Hosts = []HostSettings{
		{Name: "grafana.example.test", LANAddress: "192.0.2.196", Roles: []string{"services", "grafana"}},
		{Name: "pg.example.test", LANAddress: "192.0.2.43", Roles: []string{"pg-primary"}},
	}

	alerts, err := NewGrafanaNodeSignal().Run(context.Background(), settings)
	if err != nil {
		t.Fatal(err)
	}
	alert := requireAlertClass(t, alerts, "grafana-lan-identity")
	for _, want := range []string{
		"does not own Grafana's configured LAN address",
		"network_failed_links=1",
		"networkd_ndisc_timeouts_72h=2",
		"memory_pressure_events_72h=11",
		"oom_kills_72h=1",
		"networkd_ndisc_last=2026-08-31T12:47:55Z",
		"pressure_before_delta=54s",
		"pressure_after_delta=2s",
		"oom_after_delta=5m29s",
		"pressure_linked=true",
		"brackets the networkd NDisc timeout",
		"static service-host LAN configuration",
		"serialized Proxy rollout guard",
		"Do not restart or redeploy Grafana as the first action",
	} {
		if !strings.Contains(alert.Markdown(), want) {
			t.Fatalf("LAN-loss alert missing %q:\n%s", want, alert.Markdown())
		}
	}
}

func TestGrafanaNodeSignalDoesNotAttributeUnrelatedPressureWindow(t *testing.T) {
	ndiscAt := time.Date(2026, 8, 31, 12, 47, 55, 0, time.UTC).Unix()
	source := &syntheticSource{hostFn: func(_ HostSettings, _ string) (string, error) {
		return grafanaNodeFixture(grafanaNodeSample{
			unitActive: true, lanPresent: false, networkFailedLinks: 1,
			schedulerTCP: false, databaseTCP: 0, queryExit: 28, queryHTTP: 0, querySeconds: 4,
			networkdNDiscTimeouts: 1, networkdNDiscLastEpoch: ndiscAt,
			memoryPressureEvents: 1, memoryPressureBeforeNDiscEpoch: ndiscAt - int64((6 * time.Hour).Seconds()),
			oomKills: 1, oomAfterNDiscEpoch: ndiscAt + int64((20 * time.Hour).Seconds()),
		}), nil
	}}
	settings := syntheticSettings(source)
	settings.Hosts = []HostSettings{{Name: "grafana.example.test", LANAddress: "192.0.2.10", Roles: []string{"grafana"}}}

	alerts, err := NewGrafanaNodeSignal().Run(context.Background(), settings)
	if err != nil {
		t.Fatal(err)
	}
	markdown := requireAlertClass(t, alerts, "grafana-lan-identity").Markdown()
	for _, want := range []string{
		"pressure_before_delta=6h0m0s",
		"oom_after_delta=20h0m0s",
		"pressure_linked=false",
		"counts are context only and do not establish the cause",
		"Diagnose the networkd failure independently",
	} {
		if !strings.Contains(markdown, want) {
			t.Fatalf("unlinked-pressure alert missing %q:\n%s", want, markdown)
		}
	}
	for _, falseAttribution := range []string{
		"brackets the networkd NDisc timeout",
		"proven global memory-pressure precursor",
	} {
		if strings.Contains(markdown, falseAttribution) {
			t.Fatalf("unlinked-pressure alert retained false attribution %q:\n%s", falseAttribution, markdown)
		}
	}
}

func TestGrafanaNodeSignalSyntheticFailureClasses(t *testing.T) {
	tests := []struct {
		name  string
		alter func(*grafanaNodeSample)
		class string
		want  string
	}{
		{name: "networkd", class: "grafana-networkd-link", want: "networkd still reports 1 failed link", alter: func(sample *grafanaNodeSample) { sample.networkFailedLinks = 1 }},
		{name: "unit", class: "grafana-node-unit", want: "unit is not active", alter: func(sample *grafanaNodeSample) { sample.unitActive = false }},
		{name: "ring", class: "grafana-ring-local", want: "own Mimir scheduler", alter: func(sample *grafanaNodeSample) { sample.schedulerTCP = false }},
		{name: "database", class: "grafana-database-path", want: "cannot reach PostgreSQL", alter: func(sample *grafanaNodeSample) { sample.databaseTCP = 0 }},
		{name: "query", class: "grafana-node-query", want: "trivial query", alter: func(sample *grafanaNodeSample) { sample.queryExit = 28; sample.queryHTTP = 0; sample.querySeconds = 4 }},
	}
	for _, test := range tests {
		sample := healthyGrafanaNodeSample()
		test.alter(&sample)
		source := &syntheticSource{hostFn: func(_ HostSettings, command string) (string, error) {
			if !strings.Contains(command, grafanaNodeMarker) {
				return "", fmt.Errorf("missing Grafana node marker")
			}
			return grafanaNodeFixture(sample), nil
		}}
		settings := syntheticSettings(source)
		settings.Hosts = []HostSettings{
			{Name: "grafana.example.test", LANAddress: "192.0.2.10", Roles: []string{"grafana"}},
			{Name: "pg.example.test", LANAddress: "192.0.2.43", Roles: []string{"pg-primary"}},
		}
		alerts, err := NewGrafanaNodeSignal().Run(context.Background(), settings)
		if err != nil {
			t.Fatal(err)
		}
		alert := requireAlertClass(t, alerts, test.class)
		if !strings.Contains(alert.Markdown(), test.want) {
			t.Fatalf("%s alert missing %q:\n%s", test.class, test.want, alert.Markdown())
		}
	}
}

func TestGrafanaNodeSignalSyntheticHealthyAndSkipsNonGrafanaHost(t *testing.T) {
	hostCalls := 0
	source := &syntheticSource{hostFn: func(host HostSettings, command string) (string, error) {
		hostCalls++
		if host.Name != "grafana.example.test" || !strings.Contains(command, grafanaNodeMarker) {
			return "", fmt.Errorf("unexpected Grafana node target %s", host.Name)
		}
		return grafanaNodeFixture(healthyGrafanaNodeSample()), nil
	}}
	settings := syntheticSettings(source)
	settings.Hosts = []HostSettings{
		{Name: "grafana.example.test", LANAddress: "192.0.2.10", Roles: []string{"services", "grafana"}},
		{Name: "ordinary.example.test", LANAddress: "192.0.2.11", Roles: []string{"services"}},
		{Name: "pg.example.test", LANAddress: "192.0.2.43", Roles: []string{"pg-primary"}},
	}

	alerts, err := NewGrafanaNodeSignal().Run(context.Background(), settings)
	if err != nil {
		t.Fatal(err)
	}
	if len(alerts) != 0 {
		t.Fatalf("healthy Grafana node alerts = %+v", alerts)
	}
	if hostCalls != 1 {
		t.Fatalf("Grafana node host calls = %d, want one active Grafana host", hostCalls)
	}
}

func TestGrafanaNodeScopeGeneratedCommandDoesNotContactExcludedDatabaseOrSharedScheduler(t *testing.T) {
	for _, test := range []struct {
		paused     string
		wantTarget string
	}{
		{paused: "pg.example.test", wantTarget: "192.0.2.10:6490"},
		{paused: "shared.example.test", wantTarget: "192.0.2.43:5432"},
	} {
		settings, attempts, _ := grafanaNodeCommandTestSettings(t)
		before := append([]HostSettings(nil), settings.Hosts...)
		settings, err := ExcludeHosts(settings, test.paused)
		if err != nil {
			t.Fatal(err)
		}
		alerts, err := NewGrafanaNodeSignal().Run(context.Background(), settings)
		if err != nil {
			t.Fatal(err)
		}
		observed, err := os.ReadFile(attempts)
		if err != nil {
			t.Fatal(err)
		}
		if strings.TrimSpace(string(observed)) != test.wantTarget {
			t.Errorf("actual generated command attempted a denied nested endpoint: paused=%s", test.paused)
		}
		partial, unknown := false, false
		for _, alert := range alerts {
			partial = partial || alert.Class == "monitor-host-scope-partial" && alert.SignalNumber == "11.17a" && alert.SignalKey == "grafana-node"
			unknown = unknown || alert.Class == "cannot-observe"
			if alert.Class == "grafana-ring-local" || alert.Class == "grafana-database-path" || alert.Class == "grafana-lan-identity" {
				t.Error("denied nested destination became a local outage")
			}
		}
		if !partial || !unknown || !reflect.DeepEqual(before, settings.Hosts) {
			t.Error("nested scope lost explicit unknown or desired inventory")
		}
	}
}

func TestGrafanaNodeScopeGeneratedCommandPreservesNoPolicyAndUnrelatedExclusion(t *testing.T) {
	for _, policy := range []bool{false, true} {
		settings, attempts, _ := grafanaNodeCommandTestSettings(t)
		if policy {
			settings.Hosts = append(settings.Hosts, HostSettings{Name: "unrelated.example.test", LANAddress: "192.0.2.99", OverlayAddress: "198.51.100.99"})
			var err error
			settings, err = ExcludeHosts(settings, "unrelated.example.test")
			if err != nil {
				t.Fatal(err)
			}
		}
		alerts, err := NewGrafanaNodeSignal().Run(context.Background(), settings)
		if err != nil {
			t.Fatal(err)
		}
		observed, err := os.ReadFile(attempts)
		if err != nil {
			t.Fatal(err)
		}
		if string(observed) != "192.0.2.10:6490\n192.0.2.43:5432\n" || len(alerts) != 0 {
			t.Errorf("no-policy/unrelated scope lost the two permitted tcp attempts: policy=%t alerts=%d", policy, len(alerts))
		}
	}
}

func TestGrafanaNodeScopeGeneratedCommandRejectsSharedDatabaseOwner(t *testing.T) {
	settings, attempts, _ := grafanaNodeCommandTestSettings(t)
	settings.Hosts[2].LANAddress = "192.0.2.43"
	settings, err := ExcludeHosts(settings, "shared.example.test")
	if err != nil {
		t.Fatal(err)
	}
	alerts, err := NewGrafanaNodeSignal().Run(context.Background(), settings)
	if err != nil {
		t.Fatal(err)
	}
	observed, err := os.ReadFile(attempts)
	if err != nil {
		t.Fatal(err)
	}
	if string(observed) != "192.0.2.10:6490\n" {
		t.Error("permitted logical PG host contacted an excluded shared LAN owner")
	}
	requireAlertClass(t, alerts, "monitor-host-scope-partial")
	requireAlertClass(t, alerts, "cannot-observe")
}

func TestGrafanaNodeScopeCancellationDoesNotRunCommandOrAlert(t *testing.T) {
	settings, _, calls := grafanaNodeCommandTestSettings(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	alerts, err := NewGrafanaNodeSignal().Run(ctx, settings)
	if !errors.Is(err, context.Canceled) || len(alerts) != 0 || *calls != 0 {
		t.Fatalf("canceled nested Grafana work contacted a source or alerted: calls=%d alerts=%d err=%v", *calls, len(alerts), err)
	}
}

func TestGrafanaNodeScopeRetainsLocalFailuresWithoutUnobservedTcpAttribution(t *testing.T) {
	for _, test := range []struct {
		paused string
		class  string
		unit   bool
	}{
		{paused: "pg.example.test", class: "grafana-node-unit"},
		{paused: "shared.example.test", class: "grafana-node-unit"},
		{paused: "pg.example.test", class: "grafana-node-query", unit: true},
		{paused: "shared.example.test", class: "grafana-node-query", unit: true},
	} {
		settings, _, _ := grafanaNodeCommandTestSettings(t)
		sample := healthyGrafanaNodeSample()
		sample.unitActive = test.unit
		sample.queryExit = 28
		sample.queryHTTP = 0
		sample.querySeconds = 4
		settings.Source = &syntheticSource{hostFn: func(_ HostSettings, _ string) (string, error) {
			// Even an injected healthy TCP claim cannot override a skip.
			return grafanaNodeFixture(sample), nil
		}}
		var err error
		settings, err = ExcludeHosts(settings, test.paused)
		if err != nil {
			t.Fatal(err)
		}
		alerts, err := NewGrafanaNodeSignal().Run(context.Background(), settings)
		if err != nil {
			t.Fatal(err)
		}
		requireAlertClass(t, alerts, "monitor-host-scope-partial")
		requireAlertClass(t, alerts, "cannot-observe")
		alert := requireAlertClass(t, alerts, test.class)
		if test.class == "grafana-node-query" && (strings.Contains(alert.Mechanism, "and the direct TCP prerequisites pass") ||
			!strings.Contains(alert.Mechanism, "at least one direct TCP prerequisite is unobserved")) {
			t.Error("valid query failure implied skipped TCP inputs passed or isolated its cause")
		}
		if test.paused == "shared.example.test" && !strings.Contains(alert.Observed, "scheduler_tcp=unobservable") {
			t.Error("skipped scheduler lost its explicit unobserved sample state")
		}
		for _, alert := range alerts {
			if alert.Class == "grafana-ring-local" || alert.Class == "grafana-database-path" {
				t.Error("injected TCP result superseded immutable admission")
			}
		}
	}
}

func TestGrafanaNodeSchedulerUnobservedSampleCannotBecomeHealthOrOutage(t *testing.T) {
	raw := strings.Replace(grafanaNodeFixture(healthyGrafanaNodeSample()), "scheduler_tcp 1\n", "scheduler_tcp -1\n", 1)
	sample, err := parseGrafanaNodeSample(raw)
	if err != nil || !sample.schedulerUnobserved || sample.schedulerTCP {
		t.Fatal("scheduler -1 did not preserve explicit unobserved state")
	}
	if _, err := parseGrafanaNodeSample(strings.Replace(raw, "scheduler_tcp -1\n", "scheduler_tcp 2\n", 1)); err == nil {
		t.Error("malformed scheduler sample was accepted")
	}
	settings := syntheticSettings(&syntheticSource{hostFn: func(_ HostSettings, _ string) (string, error) { return raw, nil }})
	settings.Hosts = []HostSettings{{Name: "grafana.example.test", LANAddress: "192.0.2.10", Roles: []string{"grafana"}}}
	alerts, err := NewGrafanaNodeSignal().Run(context.Background(), settings)
	if err != nil {
		t.Fatal(err)
	}
	if len(alerts) != 1 || alerts[0].Class != "cannot-observe" || alerts[0].Target != "grafana.example.test/grafana-scheduler-lan" {
		t.Error("unobserved scheduler source became healthy or a listener outage")
	}
}

func TestGrafanaNodeScopeServerExclusionAndMidCancellationDoNotContactOrAlert(t *testing.T) {
	settings, _, calls := grafanaNodeCommandTestSettings(t)
	settings, err := ExcludeHosts(settings, "grafana.example.test")
	if err != nil {
		t.Fatal(err)
	}
	alerts, err := NewGrafanaNodeSignal().Run(context.Background(), settings)
	if err != nil || *calls != 0 {
		t.Error("excluded Grafana SSH owner reached the source")
	}
	if len(alerts) != 1 || alerts[0].Class != "monitor-host-scope-partial" {
		t.Error("excluded server obtained workload outage or recovery credit")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	settings, _, calls = grafanaNodeCommandTestSettings(t)
	settings.Source = &syntheticSource{hostFn: func(_ HostSettings, _ string) (string, error) {
		*calls++
		cancel()
		return grafanaNodeFixture(healthyGrafanaNodeSample()), nil
	}}
	alerts, err = NewGrafanaNodeSignal().Run(ctx, settings)
	if !errors.Is(err, context.Canceled) || len(alerts) != 0 || *calls != 1 {
		t.Error("mid-observation cancellation became a completed node finding")
	}
}

func TestGrafanaNodeQueryObservationIntegrityRejectsImpossibleTuples(t *testing.T) {
	for _, test := range []struct {
		old string
		bad string
	}{
		{old: "query_seconds 0.005", bad: "query_seconds NaN"},
		{old: "query_seconds 0.005", bad: "query_seconds +Inf"},
		{old: "query_seconds 0.005", bad: "query_seconds -Inf"},
		{old: "query_exit 0", bad: "query_exit 256"},
		{old: "query_exit 0", bad: "query_exit 999"},
		{old: "query_http 200", bad: "query_http 99"},
		{old: "query_http 200", bad: "query_http 600"},
		{old: "query_http 200", bad: "query_http 999"},
		{old: "query_http 200", bad: "query_http 0"},
	} {
		raw := strings.Replace(grafanaNodeFixture(healthyGrafanaNodeSample()), test.old, test.bad, 1)
		settings := grafanaNodeObservationTestSettings(raw)
		alerts, err := NewGrafanaNodeSignal().Run(context.Background(), settings)
		if err != nil {
			t.Fatal(err)
		}
		if len(alerts) != 1 || alerts[0].Class != "cannot-observe" {
			t.Errorf("impossible query tuple became outage or health: field=%s", strings.Fields(test.bad)[0])
		}
	}
	for _, sample := range []grafanaNodeSample{
		healthyGrafanaNodeSample(),
		{unitActive: true, lanPresent: true, schedulerTCP: true, databaseTCP: 1, databaseProtocol: 1, queryExit: 28, queryHTTP: 0, querySeconds: 4},
		{unitActive: true, lanPresent: true, schedulerTCP: true, databaseTCP: 1, databaseProtocol: 1, queryExit: 28, queryHTTP: 200, querySeconds: 4},
	} {
		alerts, err := NewGrafanaNodeSignal().Run(context.Background(), grafanaNodeObservationTestSettings(grafanaNodeFixture(sample)))
		if err != nil {
			t.Fatal(err)
		}
		if sample.queryExit == 0 && len(alerts) != 0 || sample.queryExit != 0 && (len(alerts) != 1 || alerts[0].Class != "grafana-node-query") {
			t.Error("valid healthy/transport-timeout/partial-response query outcome changed")
		}
	}
}

func TestGrafanaNodeNativeQueryMissingOrExtraWriteoutIsUnknown(t *testing.T) {
	for _, body := range []string{
		"exit 0", "printf '200\\n'", "printf '200 0.003 extra\\n'",
		"printf '200 NaN\\n'", "printf '999 0.003\\n'", "exit 127",
	} {
		settings, attempts, _ := grafanaNodeCommandTestSettings(t)
		grafanaNodeFixtureTool(t, attempts, "curl", body)
		alerts, err := NewGrafanaNodeSignal().Run(context.Background(), settings)
		if err != nil {
			t.Fatal(err)
		}
		if len(alerts) != 1 || alerts[0].Class != "cannot-observe" {
			t.Error("native query missing/extra/impossible write-out became health or query outage")
		}
	}
}

func TestGrafanaNodeNativeTcpUnstartedOrInterruptedToolsAreUnknown(t *testing.T) {
	for _, test := range []struct {
		missing string
		body    string
	}{
		{missing: "timeout"}, {missing: "bash"},
		{body: "exit 126"}, {body: "exit 127"}, {body: "exit 143"},
		{body: "printf 'started\\n'; exit 143"},
		{body: "exit 124"},
	} {
		settings, attempts, _ := grafanaNodeCommandTestSettings(t)
		if test.missing != "" {
			if err := os.Remove(filepath.Join(filepath.Dir(attempts), test.missing)); err != nil {
				t.Fatal(err)
			}
		} else {
			grafanaNodeFixtureTool(t, attempts, "timeout", test.body)
		}
		alerts, err := NewGrafanaNodeSignal().Run(context.Background(), settings)
		if err != nil {
			t.Fatal(err)
		}
		unknown := false
		for _, alert := range alerts {
			unknown = unknown || alert.Class == "cannot-observe"
			if alert.Class == "grafana-ring-local" || alert.Class == "grafana-database-path" {
				t.Error("unstarted/interrupted TCP observation became definitive outage")
			}
		}
		if !unknown {
			t.Error("unstarted/interrupted TCP observation became silent health")
		}
	}
}

func TestGrafanaNodeNativeTcpAttemptRefusalAndTimeoutRemainKnownFailures(t *testing.T) {
	for _, test := range []struct {
		port  int
		body  string
		exit  int
		class string
	}{
		{port: 6490, body: "started\\nclosed\\n", exit: 1, class: "grafana-ring-local"},
		{port: 6490, body: "started\\n", exit: 124, class: "grafana-ring-local"},
		{port: 5432, body: "started\\nclosed\\n", exit: 1, class: "grafana-database-path"},
		{port: 5432, body: "started\\n", exit: 124, class: "grafana-database-path"},
	} {
		settings, attempts, _ := grafanaNodeCommandTestSettings(t)
		grafanaNodeFixtureTool(t, attempts, "bash", grafanaNodeTcpOutcomeFixtureBody(test.port, test.body, test.exit))
		alerts, err := NewGrafanaNodeSignal().Run(context.Background(), settings)
		if err != nil {
			t.Fatal(err)
		}
		if len(alerts) != 1 || alerts[0].Class != test.class {
			t.Error("proved owned TCP refusal/timeout lost its existing failure identity")
		}
	}
}

func TestGrafanaNodeNativeDatabaseProtocolFailureRetainsTcpProof(t *testing.T) {
	for _, test := range []struct {
		body  string
		exit  int
		class string
	}{
		{body: "started\\nopen\\nprotocol-failed\\n", exit: 1, class: "grafana-database-path"},
		{body: "started\\nopen\\n", exit: 143, class: "cannot-observe"},
		{body: "started\\nopen\\n", exit: 124, class: "cannot-observe"},
	} {
		settings, attempts, _ := grafanaNodeCommandTestSettings(t)
		grafanaNodeFixtureTool(t, attempts, "bash", grafanaNodeTcpOutcomeFixtureBody(5432, test.body, test.exit))
		alerts, err := NewGrafanaNodeSignal().Run(context.Background(), settings)
		if err != nil {
			t.Fatal(err)
		}
		if len(alerts) != 1 || alerts[0].Class != test.class || !strings.Contains(alerts[0].Observed, "database_tcp=1") {
			t.Error("post-connect protocol outcome erased proved TCP-open or invented TCP-closed")
			continue
		}
		if test.class == "grafana-database-path" && (!strings.Contains(alerts[0].Mechanism, "TCP connection opened") || strings.Contains(alerts[0].Mechanism, "cannot open")) {
			t.Error("proved protocol failure was narrated as TCP unreachable")
		}
		if test.class == "grafana-database-path" && (!strings.Contains(alerts[0].Action, "post-connect boundary") || strings.Contains(alerts[0].Action, "Repair the failed network boundary")) {
			t.Error("proved protocol failure prescribed an unproved LAN-route repair")
		}
		if test.class == "grafana-database-path" && (!strings.Contains(alerts[0].Context, "TCP-open proof") || strings.Contains(alerts[0].Context, "host LAN/database route boundary")) {
			t.Error("proved protocol failure context asserted a LAN-route diagnosis")
		}
	}
}

func TestGrafanaNodeNativeUnknownPreservesIndependentEvidenceAndConditionalDatabase(t *testing.T) {
	for _, test := range []struct {
		tool  string
		body  string
		class string
	}{
		{tool: "curl", body: "printf '200 NaN\\n'", class: "grafana-node-unit"},
		{tool: "timeout", body: "exit 127", class: "grafana-node-unit"},
		{tool: "ip", body: "exit 0", class: "grafana-lan-identity"},
	} {
		settings, attempts, _ := grafanaNodeCommandTestSettings(t)
		grafanaNodeFixtureTool(t, attempts, test.tool, test.body)
		if test.class == "grafana-node-unit" {
			grafanaNodeFixtureTool(t, attempts, "systemctl", "exit 0")
		}
		alerts, err := NewGrafanaNodeSignal().Run(context.Background(), settings)
		if err != nil {
			t.Fatal(err)
		}
		local, unknown := false, false
		for _, alert := range alerts {
			local = local || alert.Class == test.class
			unknown = unknown || alert.Class == "cannot-observe"
		}
		if !local || !unknown {
			t.Error("unknown native operation lost independent evidence or visibility")
		}
		if test.class == "grafana-lan-identity" {
			observed := requireAlertClass(t, alerts, test.class).Observed
			if !strings.Contains(observed, "scheduler_tcp=unobservable") || !strings.Contains(observed, "database_tcp=-1") {
				t.Error("no attempted TCP operation was reported as closed")
			}
		}
	}
	settings, attempts, _ := grafanaNodeCommandTestSettings(t)
	settings.Hosts[1].Roles = nil
	alerts, err := NewGrafanaNodeSignal().Run(context.Background(), settings)
	if err != nil || len(alerts) != 0 {
		t.Fatal("not-configured primary changed the conditional database baseline")
	}
	observed, err := os.ReadFile(attempts)
	if err != nil || string(observed) != "192.0.2.10:6490\n" {
		t.Error("conditional database absence altered permitted scheduler or attempted DB")
	}
}

func TestGrafanaNodeNativeQueryWithUnknownProtocolDoesNotEraseTcpProof(t *testing.T) {
	settings, attempts, _ := grafanaNodeCommandTestSettings(t)
	grafanaNodeFixtureTool(t, attempts, "bash", grafanaNodeTcpOutcomeFixtureBody(5432, "started\\nopen\\n", 143))
	grafanaNodeFixtureTool(t, attempts, "curl", "printf '000 4.000\\n'; exit 28")
	alerts, err := NewGrafanaNodeSignal().Run(context.Background(), settings)
	if err != nil {
		t.Fatal(err)
	}
	query := requireAlertClass(t, alerts, "grafana-node-query")
	requireAlertClass(t, alerts, "cannot-observe")
	if !strings.Contains(query.Observed, "database_tcp=1") || !strings.Contains(query.Mechanism, "protocol prerequisite is unobserved") ||
		strings.Contains(query.Mechanism, "direct TCP prerequisite is unobserved") {
		t.Error("query attribution erased known TCP-open or made protocol unknown a TCP unknown")
	}
}

func TestGrafanaNodeObservationStateTuplesAreStrictAndRedacted(t *testing.T) {
	for _, test := range []struct{ old, bad string }{
		{old: "query_observed 1", bad: "query_observed 2"},
		{old: "query_observed 1", bad: "query_observed 0"},
		{old: "database_protocol 1", bad: "database_protocol 2"},
		{old: "database_tcp 1", bad: "database_tcp 0"},
		{old: "query_seconds 0.005", bad: "query_seconds synthetic-payload"},
	} {
		raw := strings.Replace(grafanaNodeFixture(healthyGrafanaNodeSample()), test.old, test.bad, 1)
		if _, err := parseGrafanaNodeSample(raw); err == nil {
			t.Error("malformed/inconsistent explicit observation state was accepted")
		}
		alerts, err := NewGrafanaNodeSignal().Run(context.Background(), grafanaNodeObservationTestSettings(raw))
		if err != nil || len(alerts) != 1 || alerts[0].Class != "cannot-observe" {
			t.Fatal("invalid state tuple became health/outage")
		}
		if strings.Contains(alerts[0].Markdown(), "synthetic-payload") {
			t.Error("unknown query parser exposed raw diagnostic payload")
		}
	}
}

func grafanaNodeObservationTestSettings(raw string) SignalSettings {
	settings := syntheticSettings(&syntheticSource{hostFn: func(_ HostSettings, _ string) (string, error) { return raw, nil }})
	settings.Hosts = []HostSettings{
		{Name: "grafana.example.test", LANAddress: "192.0.2.10", Roles: []string{"grafana"}},
		{Name: "pg.example.test", LANAddress: "192.0.2.43", Roles: []string{"pg-primary"}},
	}
	return settings
}

func grafanaNodeFixtureTool(t *testing.T, attempts, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(filepath.Dir(attempts), name), []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
}

// Model the requested owned program's phase output; the original program has
// no phase markers, so its same OS outcome remains status-only. This keeps
// original refusal/deadline positives valid during red-before-green replay.
func grafanaNodeTcpOutcomeFixtureBody(port int, output string, status int) string {
	return fmt.Sprintf(`[ "$1" = -c ] && [ "$3" = monitor ] || exit 64
case "$2" in
  *monitor-grafana-node-owned-tcp*)
    if [ "$5" = %d ]; then printf '%s'; exit %d; fi
    printf 'started\nopen\n'
    if [ "$5" = 5432 ]; then printf 'protocol-accepted\n'; fi
    ;;
  *) if [ "$5" = %d ]; then exit %d; fi ;;
esac`, port, output, status, port, status)
}

// Execute the actual POSIX command. Only the nested Bash TCP operation is a
// recorder; systemd/network/curl/journal tools return fixed local fixtures.
func grafanaNodeCommandTestSettings(t *testing.T) (SignalSettings, string, *int) {
	t.Helper()
	directory := t.TempDir()
	attempts := filepath.Join(directory, "attempts")
	if err := os.WriteFile(attempts, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	realSh, err := exec.LookPath("sh")
	if err != nil {
		t.Fatal(err)
	}
	realAwk, err := exec.LookPath("awk")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(realAwk, filepath.Join(directory, "awk")); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{
		"systemctl":  "printf '%s\\n' 'warp-synthetic-grafana-a-g1.service loaded active running fixture'",
		"ip":         "printf '%s\\n' '2: fixture inet 192.0.2.10/24 scope global fixture'",
		"networkctl": "printf '%s\\n' '2 fixture ether routable configured'",
		"timeout":    "[ \"$1\" = 2 ] || exit 64\nshift\nexec \"$@\"",
		"bash":       "[ \"$1\" = -c ] && [ \"$3\" = monitor ] || exit 64\nprintf '%s:%s\\n' \"$4\" \"$5\" >> \"$GRAFANA_NODE_TEST_ATTEMPTS\"\nprintf 'started\\nopen\\n'\nif [ \"$5\" = 5432 ]; then printf 'protocol-accepted\\n'; fi",
		"curl":       "printf '200 0.003\\n'",
		"journalctl": "exit 0",
	} {
		if err := os.WriteFile(filepath.Join(directory, name), []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	calls := new(int)
	settings := syntheticSettings(&syntheticSource{hostFn: func(_ HostSettings, command string) (string, error) {
		*calls++
		process := exec.Command(realSh, "-c", command)
		process.Env = append(os.Environ(), "PATH="+directory, "GRAFANA_NODE_TEST_ATTEMPTS="+attempts)
		output, err := process.CombinedOutput()
		return string(output), err
	}})
	settings.Hosts = []HostSettings{
		{Name: "grafana.example.test", LANAddress: "192.0.2.10", OverlayAddress: "198.51.100.10", Roles: []string{"grafana"}},
		{Name: "pg.example.test", LANAddress: "192.0.2.43", OverlayAddress: "198.51.100.43", Roles: []string{"pg-primary"}},
		{Name: "shared.example.test", LANAddress: "192.0.2.10", OverlayAddress: "198.51.100.99"},
	}
	return settings, attempts, calls
}

func healthyGrafanaNodeSample() grafanaNodeSample {
	return grafanaNodeSample{
		unitActive: true, lanPresent: true, schedulerTCP: true, databaseTCP: 1, databaseProtocol: 1,
		queryExit: 0, queryHTTP: 200, querySeconds: 0.005,
	}
}

func grafanaNodeFixture(sample grafanaNodeSample) string {
	boolInt := func(value bool) int {
		if value {
			return 1
		}
		return 0
	}
	scheduler := boolInt(sample.schedulerTCP)
	if sample.schedulerUnobserved {
		scheduler = -1
	}
	protocol := sample.databaseProtocol
	if sample.databaseTCP != 1 {
		protocol = -1
	}
	queryExit, queryHttp, querySeconds := sample.queryExit, sample.queryHTTP, sample.querySeconds
	if sample.queryUnobserved {
		queryExit, queryHttp, querySeconds = -1, -1, -1
	}
	return fmt.Sprintf(
		"unit_active %d\nlan_present %d\nnetwork_failed_links %d\nscheduler_tcp %d\ndatabase_tcp %d\ndatabase_protocol %d\nquery_observed %d\nquery_exit %d\nquery_http %d\nquery_seconds %.3f\nnetworkd_ndisc_timeouts %d\nnetworkd_ndisc_last_epoch %d\nmemory_pressure_events %d\nmemory_pressure_before_ndisc_epoch %d\nmemory_pressure_after_ndisc_epoch %d\noom_kills %d\noom_after_ndisc_epoch %d\n",
		boolInt(sample.unitActive), boolInt(sample.lanPresent), sample.networkFailedLinks,
		scheduler, sample.databaseTCP, protocol, boolInt(!sample.queryUnobserved), queryExit, queryHttp,
		querySeconds, sample.networkdNDiscTimeouts, sample.networkdNDiscLastEpoch,
		sample.memoryPressureEvents, sample.memoryPressureBeforeNDiscEpoch, sample.memoryPressureAfterNDiscEpoch,
		sample.oomKills, sample.oomAfterNDiscEpoch,
	)
}
