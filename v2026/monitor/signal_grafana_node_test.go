package monitor

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestGrafanaNodeSignalSyntheticOOMLANLoss(t *testing.T) {
	ndiscAt := time.Date(2026, 8, 31, 12, 47, 55, 0, time.UTC).Unix()
	source := &syntheticSource{hostFn: func(host HostSettings, command string) (string, error) {
		if host.Name != "fireside" || !strings.Contains(command, grafanaNodeMarker) {
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
		{Name: "fireside", LANAddress: "192.0.2.196", Roles: []string{"services", "grafana"}},
		{Name: "pg-1", LANAddress: "192.0.2.43", Roles: []string{"pg-primary"}},
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
	settings.Hosts = []HostSettings{{Name: "grafana-1", LANAddress: "192.0.2.10", Roles: []string{"grafana"}}}

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
		t.Run(test.name, func(t *testing.T) {
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
				{Name: "grafana-1", LANAddress: "192.0.2.10", Roles: []string{"grafana"}},
				{Name: "pg-1", LANAddress: "192.0.2.43", Roles: []string{"pg-primary"}},
			}
			alerts, err := NewGrafanaNodeSignal().Run(context.Background(), settings)
			if err != nil {
				t.Fatal(err)
			}
			alert := requireAlertClass(t, alerts, test.class)
			if !strings.Contains(alert.Markdown(), test.want) {
				t.Fatalf("%s alert missing %q:\n%s", test.class, test.want, alert.Markdown())
			}
		})
	}
}

func TestGrafanaNodeSignalSyntheticHealthyAndSkipsNonGrafanaHost(t *testing.T) {
	hostCalls := 0
	source := &syntheticSource{hostFn: func(host HostSettings, command string) (string, error) {
		hostCalls++
		if host.Name != "grafana-1" || !strings.Contains(command, grafanaNodeMarker) {
			return "", fmt.Errorf("unexpected Grafana node target %s", host.Name)
		}
		return grafanaNodeFixture(healthyGrafanaNodeSample()), nil
	}}
	settings := syntheticSettings(source)
	settings.Hosts = []HostSettings{
		{Name: "grafana-1", LANAddress: "192.0.2.10", Roles: []string{"services", "grafana"}},
		{Name: "ordinary-edge", LANAddress: "192.0.2.11", Roles: []string{"services"}},
		{Name: "pg-1", LANAddress: "192.0.2.43", Roles: []string{"pg-primary"}},
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

func healthyGrafanaNodeSample() grafanaNodeSample {
	return grafanaNodeSample{
		unitActive: true, lanPresent: true, schedulerTCP: true, databaseTCP: 1,
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
	return fmt.Sprintf(
		"unit_active %d\nlan_present %d\nnetwork_failed_links %d\nscheduler_tcp %d\ndatabase_tcp %d\nquery_exit %d\nquery_http %d\nquery_seconds %.3f\nnetworkd_ndisc_timeouts %d\nnetworkd_ndisc_last_epoch %d\nmemory_pressure_events %d\nmemory_pressure_before_ndisc_epoch %d\nmemory_pressure_after_ndisc_epoch %d\noom_kills %d\noom_after_ndisc_epoch %d\n",
		boolInt(sample.unitActive), boolInt(sample.lanPresent), sample.networkFailedLinks,
		boolInt(sample.schedulerTCP), sample.databaseTCP, sample.queryExit, sample.queryHTTP,
		sample.querySeconds, sample.networkdNDiscTimeouts, sample.networkdNDiscLastEpoch,
		sample.memoryPressureEvents, sample.memoryPressureBeforeNDiscEpoch, sample.memoryPressureAfterNDiscEpoch,
		sample.oomKills, sample.oomAfterNDiscEpoch,
	)
}
