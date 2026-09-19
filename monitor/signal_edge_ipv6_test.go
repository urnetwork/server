package monitor

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"reflect"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
)

func TestActiveEdgeIPv6FromServicesUsesOnlyCurrentNontransparentInterfaces(t *testing.T) {
	services := servicesYaml{
		Domain: "network.example",
		Versions: []servicesVersionYaml{
			{LB: servicesLBYaml{Interfaces: map[string]map[string]servicesLBInterfaceYaml{
				"synthetic-edge-3.network.example": {
					"public-b": {IPv6: "2001:db8:1::2"},
					"public-a": {IPv6: "2001:db8:1::1"},
				},
				"synthetic-edge-proxy.network.example": {
					"public-a": {IPv6: "2001:db8:2::1", Transparent: true},
				},
			}}},
			{LB: servicesLBYaml{Interfaces: map[string]map[string]servicesLBInterfaceYaml{
				"synthetic-edge-3.network.example": {
					"historical": {IPv6: "2001:db8:3::1"},
				},
			}}},
		},
	}

	got, err := activeEdgeIPv6FromServices(services)
	if err != nil {
		t.Fatal(err)
	}
	edge := got["synthetic-edge-3"]
	if len(edge) != 2 {
		t.Fatalf("active edge interfaces = %+v, want two", edge)
	}
	if edge[0].Interface != "public-a" || edge[0].Block != "synthetic-edge-3.network.example-public-a" ||
		edge[0].Address != "2001:db8:1::1" || edge[1].Interface != "public-b" ||
		edge[1].Block != "synthetic-edge-3.network.example-public-b" ||
		edge[1].Address != "2001:db8:1::2" {
		t.Fatalf("active edge interfaces = %+v", edge)
	}
	if edge[0].ProbeHostname != "api-v6.network.example" {
		t.Fatalf("probe hostname = %q", edge[0].ProbeHostname)
	}
	if _, ok := got["synthetic-edge-proxy"]; ok {
		t.Fatalf("transparent proxy host became an edge IPv6 target: %+v", got)
	}
	for _, configured := range edge {
		if configured.Address == "2001:db8:3::1" || configured.Interface == "historical" {
			t.Fatalf("historical version leaked into active targets: %+v", edge)
		}
	}
}

func TestEdgeIPv6SignalSyntheticRootCauseClasses(t *testing.T) {
	addresses := map[string]string{
		"healthy": "2001:db8:1::1",
		"drift":   "2001:db8:2::2",
		"reset":   "2001:db8:3::3",
		"drop":    "2001:db8:4::4",
		"policy":  "2001:db8:5::5",
	}
	source := &syntheticSource{
		localFn: func(name string, args ...string) (string, error) {
			if name != "curl" {
				return "", errors.New("unexpected local command")
			}
			joined := strings.Join(args, " ")
			switch {
			case strings.Contains(joined, "["+addresses["healthy"]+"]"):
				return edgeHTTPFixture("200", "0", addresses["healthy"], "0.080"), nil
			case strings.Contains(joined, "["+addresses["reset"]+"]"):
				return edgeHTTPFixture("000", "7", "", "0.041"), edgeCommandExitError(7)
			case strings.Contains(joined, "["+addresses["drop"]+"]"):
				return "curl: (28) Timeout was reached\n" + edgeHTTPFixture("000", "28", "", "3.002"), edgeCommandExitError(28)
			case strings.Contains(joined, "["+addresses["policy"]+"]"):
				return "curl: (28) Timeout was reached\n" + edgeHTTPFixture("000", "28", "", "3.003"), edgeCommandExitError(28)
			case strings.Contains(joined, "["+addresses["drift"]+"]"):
				return "curl: (28) Timeout was reached\n" + edgeHTTPFixture("000", "28", "", "3.001"), edgeCommandExitError(28)
			default:
				return "", errors.New("unexpected edge address")
			}
		},
		hostFn: func(_ HostSettings, command string) (string, error) {
			switch {
			case strings.Contains(command, edgeIPv6IdentityMarker):
				present := "1"
				if strings.Contains(command, addresses["drift"]) {
					present = "0"
				}
				return "operstate=up\nconfigured_present=" + present + "\nunit_active=active\n", nil
			case strings.Contains(command, edgeIPv6EgressMarker):
				for name, address := range addresses {
					if strings.Contains(command, address) {
						if name == "policy" {
							return "self_http_code=200\nself_exitcode=0\nself_probe_status=0\nroute_device=management0\nroute_source=2001:db8:ffff::1\nroute_status=0\nsource_egress_status=28\n", nil
						}
						return "self_http_code=200\nself_exitcode=0\nself_probe_status=0\nsource_egress=" + address + "\nsource_egress_status=0\n", nil
					}
				}
				return "", errors.New("missing synthetic egress address")
			default:
				return "", errors.New("unexpected synthetic host command")
			}
		},
	}
	settings := syntheticSettings(source)
	settings.Hosts = []HostSettings{{
		Name: "synthetic-edge-3",
		EdgeIPv6: []EdgeIPv6InterfaceSettings{
			{Interface: "eno-healthy", Address: addresses["healthy"], ProbeHostname: "api-v6.example"},
			{Interface: "eno-drift", Address: addresses["drift"], ProbeHostname: "api-v6.example"},
			{Interface: "eno-reset", Address: addresses["reset"], ProbeHostname: "api-v6.example"},
			{Interface: "eno-drop", Address: addresses["drop"], ProbeHostname: "api-v6.example"},
			{Interface: "eno-policy", Address: addresses["policy"], ProbeHostname: "api-v6.example"},
		},
	}}

	alerts, err := NewEdgeIPv6Signal().Run(context.Background(), settings)
	if err != nil {
		t.Fatal(err)
	}
	if len(alerts) != 5 {
		t.Fatalf("alerts = %d, want drift plus its path failure, reset, upstream drop, and policy-route failure: %+v", len(alerts), alerts)
	}
	drift := requireAlertClass(t, alerts, "edge-ipv6-identity-drift")
	reset := requireAlertClass(t, alerts, "edge-ipv6-reset")
	drop := requireAlertClass(t, alerts, "edge-ipv6-upstream-drop")
	policy := requireAlertClass(t, alerts, "edge-ipv6-policy-route")
	for _, alert := range []Alert{drift, reset, drop, policy} {
		if alert.SignalNumber != "18.1" || alert.SignalKey != "edge-ipv6" {
			t.Fatalf("wrong signal identity: %+v", alert)
		}
	}
	if !strings.Contains(drift.Markdown(), "active services.yml") {
		t.Fatalf("identity drift lacks source-of-truth detail: %s", drift.Markdown())
	}
	if !strings.Contains(reset.Markdown(), "dead-first DNAT") {
		t.Fatalf("reset lacks stale-rule diagnosis: %s", reset.Markdown())
	}
	if !strings.Contains(drop.Markdown(), "upstream default-drop/ACL") {
		t.Fatalf("timeout lacks upstream ACL diagnosis: %s", drop.Markdown())
	}
	if !strings.Contains(policy.Markdown(), "lower-metric management default") ||
		!strings.Contains(policy.Observed, "route_device=management0") ||
		!strings.Contains(policy.Action, "Warp 8924493") {
		t.Fatalf("policy-route failure lacks the return-path diagnosis: %s", policy.Markdown())
	}
	for _, alert := range alerts {
		if strings.Contains(alert.Frame, "eno-healthy") {
			t.Fatalf("healthy interface alerted: %+v", alert)
		}
	}
}

func TestEdgeIPv6SignalSyntheticLBConfigRejectionIsNotDeadFirstDNAT(t *testing.T) {
	addresses := map[string]string{
		"healthy":    "2001:db8:10::1",
		"rejected":   "2001:db8:10::2",
		"dead_first": "2001:db8:10::3",
	}
	interfaces := map[string]string{
		"healthy":    "synthetic-if-a",
		"rejected":   "synthetic-if-b",
		"dead_first": "synthetic-if-c",
	}
	var admissionCalls atomic.Int32
	source := &syntheticSource{
		localFn: func(name string, args ...string) (string, error) {
			if name == "/sbin/route" {
				return "route to: 2001:db8:ffff::1\ninterface: synthetic0\n", nil
			}
			if name != "curl" {
				return "", errors.New("unexpected synthetic local command")
			}
			joined := strings.Join(args, " ")
			switch {
			case strings.Contains(joined, "["+addresses["healthy"]+"]"):
				return edgeHTTPFixture("200", "0", addresses["healthy"], "0.080000"), nil
			case strings.Contains(joined, "["+addresses["rejected"]+"]"):
				return "curl: (7) Failed to connect to api-v6.example port 443 after 73 ms: Couldn't connect to server\n" +
					edgeHTTPFixture("000", "7", "", "0.073000"), edgeCommandExitError(7)
			case strings.Contains(joined, "["+addresses["dead_first"]+"]"):
				return "curl: (7) Failed to connect to api-v6.example port 443 after 37 ms: Couldn't connect to server\n" +
					edgeHTTPFixture("000", "7", "", "0.037000"), edgeCommandExitError(7)
			default:
				return "", errors.New("unexpected synthetic edge address")
			}
		},
		hostFn: func(_ HostSettings, command string) (string, error) {
			switch {
			case strings.Contains(command, edgeIPv6IdentityMarker):
				return "operstate=up\nconfigured_present=1\nunit_active=active\n", nil
			case strings.Contains(command, edgeIPv6EgressMarker):
				for name, address := range addresses {
					if name == "healthy" || !strings.Contains(command, address) {
						continue
					}
					interfaceName := interfaces[name]
					return "curl: (7) synthetic self refusal\n" +
						"self_http_code=000\nself_exitcode=7\nself_time_total=0.000800\nself_probe_status=7\n" +
						"route_device=" + interfaceName + "\nroute_source=" + address + "\nroute_status=0\n" +
						"source_egress=" + address + "\nsource_egress_status=0\n", nil
				}
				return "", errors.New("missing synthetic egress address")
			case strings.Contains(command, edgeIPv6AdmissionMarker):
				admissionCalls.Add(1)
				for _, want := range []string{
					"journalctl --no-pager --quiet --since '15 minutes ago' -n 20 -o cat",
					"SYSLOG_IDENTIFIER=\"$journal_identifier\"",
					"could not build map_hash",
					"ss -ltnH",
					"ss -lunH",
				} {
					if !strings.Contains(command, want) {
						t.Fatalf("admission command missing bounded discriminator %q", want)
					}
				}
				if strings.Contains(command, "journalctl -u") || strings.Contains(command, "systemctl cat") {
					t.Fatalf("admission command dumps unit or journal contents: %s", command)
				}
				if strings.Contains(command, "expected_block='synthetic-edge.example-lb-b'") {
					return "lb_observation_status=1\nlb_listener_count=0\nlb_map_hash_error_count=2\n", nil
				}
				if strings.Contains(command, "expected_block='synthetic-edge.example-lb-c'") {
					return "lb_observation_status=1\nlb_listener_count=1\nlb_map_hash_error_count=0\n", nil
				}
				return "", errors.New("unexpected synthetic admission command")
			default:
				return "", errors.New("unexpected synthetic host command")
			}
		},
	}
	settings := syntheticSettings(source)
	settings.Hosts = []HostSettings{{
		Name: "edge-synthetic.example",
		EdgeIPv6: []EdgeIPv6InterfaceSettings{
			{Interface: interfaces["healthy"], Block: "synthetic-edge.example-lb-a", Address: addresses["healthy"], ProbeHostname: "api-v6.example"},
			{Interface: interfaces["rejected"], Block: "synthetic-edge.example-lb-b", Address: addresses["rejected"], ProbeHostname: "api-v6.example"},
			{Interface: interfaces["dead_first"], Block: "synthetic-edge.example-lb-c", Address: addresses["dead_first"], ProbeHostname: "api-v6.example"},
		},
	}}

	alerts, err := NewEdgeIPv6Signal().Run(context.Background(), settings)
	if err != nil {
		t.Fatal(err)
	}
	if admissionCalls.Load() != 2 {
		t.Fatalf("admission calls = %d, want rejected and dead-first controls", admissionCalls.Load())
	}
	if len(alerts) != 2 {
		t.Fatalf("alerts = %d, want rejected and dead-first only: %+v", len(alerts), alerts)
	}
	rejected := requireAlertClass(t, alerts, "edge-lb-config-rejected")
	if !strings.Contains(rejected.Mechanism, "configuration admission failure") ||
		!strings.Contains(rejected.Action, "map_hash_bucket_size") ||
		!strings.Contains(rejected.Observed, "lb_listener_count=0") ||
		!strings.Contains(rejected.Observed, "lb_map_hash_error_count=2") ||
		!strings.Contains(rejected.Verify, "exact corrected Warp artifact") {
		t.Fatalf("config-rejected alert lacks bounded causal evidence: %s", rejected.Markdown())
	}
	for _, forbidden := range []string{"dead-first DNAT", "[emerg] could not build map_hash", "ExecStart", "--portblocks="} {
		if strings.Contains(rejected.Evidence, forbidden) {
			t.Fatalf("config-rejected evidence leaked %q: %s", forbidden, rejected.Evidence)
		}
	}
	deadFirst := requireAlertClass(t, alerts, "edge-ipv6-reset")
	if !strings.Contains(deadFirst.Mechanism, "dead-first DNAT") {
		t.Fatalf("dead-first control lost reset diagnosis: %s", deadFirst.Markdown())
	}
	for _, alert := range alerts {
		if strings.Contains(alert.Frame, "synthetic-healthy") {
			t.Fatalf("healthy control alerted: %+v", alert)
		}
	}
}

func TestEdgeIPv6SignalSyntheticUnknownLBAdmissionDoesNotBecomeReset(t *testing.T) {
	address := "2001:db8:20::1"
	result := edgeIPv6Result{
		host: &host{name: "synthetic-edge.example"},
		configured: EdgeIPv6InterfaceSettings{
			Interface:     "synthetic-if-a",
			Block:         "synthetic-edge.example-lb-a",
			Address:       address,
			ProbeHostname: "api-v6.example",
		},
		http: map[string]string{
			"monitor_http_code":     "000",
			"monitor_exitcode":      "7",
			"monitor_remote_ip":     "",
			"monitor_time_total":    "0.050000",
			"monitor_content_type":  "",
			"monitor_size_download": "0",
		},
		httpOutput: edgeHTTPFixture("000", "7", "", "0.050000"),
		httpErr:    edgeCommandExitError(7),
		identity: map[string]string{
			"configured_present": "1",
			"operstate":          "up",
			"unit_active":        "active",
		},
		egress: map[string]string{
			"self_probe_status":    "7",
			"self_exitcode":        "7",
			"self_http_code":       "000",
			"self_time_total":      "0.010000",
			"route_status":         "0",
			"route_device":         "synthetic-if-a",
			"route_source":         address,
			"source_egress_status": "0",
			"source_egress":        address,
		},
		admissionErr: errors.New("synthetic bounded journal observation timed out"),
	}

	findings := edgeIPv6Findings(result, false, false)
	if len(findings) != 1 {
		t.Fatalf("findings = %d, want only the admission visibility failure: %+v", len(findings), findings)
	}
	if findings[0].class != "cannot-observe" || findings[0].target != "synthetic-edge.example/synthetic-if-a/lb-admission" {
		t.Fatalf("admission visibility finding = %+v", findings[0])
	}
}

func TestEdgeIPv6SignalSyntheticObserverNoRouteDoesNotPageEveryEdge(t *testing.T) {
	addresses := []string{
		"2001:db8:1::10",
		"2001:db8:1::11",
		"2001:db8:2::20",
		"2001:db8:2::21",
	}
	routeCalls := 0
	source := &syntheticSource{
		localFn: func(name string, args ...string) (string, error) {
			if name == "/sbin/route" {
				routeCalls++
				if strings.Join(args, " ") != "-n get -inet6 "+ipv6ObserverRouteProbeAddress {
					t.Fatalf("route arguments = %q", strings.Join(args, " "))
				}
				// macOS route can report an absent route in its output while
				// still exiting zero. The diagnostic text remains authoritative.
				return "route: writing to routing socket: not in table\n", nil
			}
			if name != "curl" {
				return "", errors.New("unexpected local command")
			}
			return "curl: (7) Failed to connect to api-v6.example port 443 after 0 ms: Couldn't connect to server\n" +
				edgeHTTPFixture("000", "7", "", "0.000106"), edgeCommandExitError(7)
		},
		hostFn: func(_ HostSettings, command string) (string, error) {
			switch {
			case strings.Contains(command, edgeIPv6IdentityMarker):
				return "operstate=up\nconfigured_present=1\nunit_active=active\n", nil
			case strings.Contains(command, edgeIPv6EgressMarker):
				for index, address := range addresses {
					if strings.Contains(command, address) {
						return "self_http_code=200\nself_exitcode=0\nself_probe_status=0\n" +
							"route_device=public" + string(rune('0'+index%2)) + "\n" +
							"route_source=" + address + "\nroute_status=0\n" +
							"source_egress=" + address + "\nsource_egress_status=0\n", nil
					}
				}
			}
			return "", errors.New("unexpected synthetic host command")
		},
	}
	settings := syntheticSettings(source)
	settings.Hosts = []HostSettings{
		{Name: "edge-a", EdgeIPv6: []EdgeIPv6InterfaceSettings{
			{Interface: "public0", Address: addresses[0], ProbeHostname: "api-v6.example"},
			{Interface: "public1", Address: addresses[1], ProbeHostname: "api-v6.example"},
		}},
		{Name: "edge-b", EdgeIPv6: []EdgeIPv6InterfaceSettings{
			{Interface: "public0", Address: addresses[2], ProbeHostname: "api-v6.example"},
			{Interface: "public1", Address: addresses[3], ProbeHostname: "api-v6.example"},
		}},
	}

	env, err := newProbeEnv(settings.withDefaults())
	if err != nil {
		t.Fatal(err)
	}
	findings, err := (edgeIPv6Probe{}).check(context.Background(), env)
	if err != nil {
		t.Fatal(err)
	}
	if routeCalls != 2 {
		t.Fatalf("route calls = %d, want fleet-level before/after lookups", routeCalls)
	}
	observer := findingByClass(t, findings, "ipv6-observer-route-unavailable")
	if observer.healthy || observer.tier != tierWarn || observer.target != "monitor-host/edge-ipv6" {
		t.Fatalf("observer finding = %+v", observer)
	}
	for _, want := range []string{
		"observer_route=absent",
		"configured_targets=4",
		"immediate_connect_failures=4",
		"identity_healthy=4",
		"local_self_https_healthy=4",
		"source_route_exact=4",
		"source_egress_exact=4",
		"externally routed coverage",
	} {
		if !strings.Contains(observer.observed+" "+observer.mechanism, want) {
			t.Fatalf("observer finding missing %q: %+v", want, observer)
		}
	}
	for _, forbidden := range append(addresses, "not in table", "Couldn't connect to server") {
		if strings.Contains(observer.evidence+observer.observed+observer.mechanism, forbidden) {
			t.Fatalf("observer finding leaked %q: %+v", forbidden, observer)
		}
	}
	resolved := map[string]int{}
	for _, finding := range findings {
		if finding.class == "edge-ipv6-reset" {
			if !finding.healthy {
				t.Fatalf("observer route loss emitted per-edge reset: %+v", finding)
			}
			resolved[finding.target]++
		}
	}
	if resolved["edge-a"] != 1 || resolved["edge-b"] != 1 {
		t.Fatalf("reset resolution findings = %+v, want one per target", resolved)
	}
}

func TestEdgeIPv6SignalSyntheticObserverRoutePreservesRealReset(t *testing.T) {
	address := "2001:db8:3::30"
	source := &syntheticSource{
		localFn: func(name string, _ ...string) (string, error) {
			if name == "/sbin/route" {
				return "route to: 2001:db8:ffff::1\ninterface: synthetic0\n", nil
			}
			return "curl: (7) Failed to connect to api-v6.example port 443 after 0 ms: Couldn't connect to server\n" +
				edgeHTTPFixture("000", "7", "", "0.000072"), edgeCommandExitError(7)
		},
		hostFn: func(_ HostSettings, command string) (string, error) {
			if strings.Contains(command, edgeIPv6IdentityMarker) {
				return "operstate=up\nconfigured_present=1\nunit_active=active\n", nil
			}
			if strings.Contains(command, edgeIPv6EgressMarker) {
				return "self_http_code=200\nself_exitcode=0\nself_probe_status=0\n" +
					"route_device=public0\nroute_source=" + address + "\nroute_status=0\n" +
					"source_egress=" + address + "\nsource_egress_status=0\n", nil
			}
			return "", errors.New("unexpected host command")
		},
	}
	settings := syntheticSettings(source)
	settings.Hosts = []HostSettings{{Name: "edge-a", EdgeIPv6: []EdgeIPv6InterfaceSettings{{
		Interface: "public0", Address: address, ProbeHostname: "api-v6.example",
	}}}}

	alerts, err := NewEdgeIPv6Signal().Run(context.Background(), settings)
	if err != nil {
		t.Fatal(err)
	}
	if len(alerts) != 1 || alerts[0].Class != "edge-ipv6-reset" {
		t.Fatalf("routed refusal alerts = %+v, want the genuine reset", alerts)
	}
}

// A route can disappear during connection establishment, leaving a TLS timeout with
// a peer address rather than the immediate, peer-less curl exit 7 shape.
func TestEdgeIpv6SignalObserverRouteLossDuringTls(t *testing.T) {
	for _, test := range []struct {
		name       string
		routes     []string
		allTimeout bool
	}{
		{name: "lost during requests", routes: []string{"interface: synthetic0\n", "not in table\n"}},
		{name: "recovered during host controls", routes: []string{"not in table\n", "interface: synthetic0\n"}},
		{name: "absent throughout", routes: []string{"not in table\n", "not in table\n"}},
		{name: "all requests retained peers", routes: []string{"interface: synthetic0\n", "not in table\n"}, allTimeout: true},
	} {
		settings, routeCalls := edgeObserverTlsSettings(test.routes)
		identityErr := &sshCommandError{err: fmt.Errorf("synthetic-private-identity-detail: %w", edgeCommandExitError(255))}
		source := settings.Source.(*syntheticSource)
		localFn := source.localFn
		source.localFn = func(name string, args ...string) (string, error) {
			if test.allTimeout && name == "curl" && strings.Contains(strings.Join(args, " "), "[2001:db8:50::3]") {
				return edgeHTTPFixture("000", "28", "2001:db8:50::3", "3.002"), edgeCommandExitError(28)
			}
			return localFn(name, args...)
		}
		hostFn := source.hostFn
		source.hostFn = func(host HostSettings, command string) (string, error) {
			if strings.Contains(command, edgeIPv6IdentityMarker) && strings.Contains(command, "2001:db8:50::1") {
				return "", identityErr
			}
			return hostFn(host, command)
		}
		alerts, err := NewEdgeIPv6Signal().Run(context.Background(), settings)
		if err != nil {
			t.Fatalf("%s: %v", test.name, err)
		}
		if *routeCalls != 2 || len(alerts) != 2 {
			t.Fatalf("%s: route calls=%d alerts=%d, want before/after control and route/identity visibility", test.name, *routeCalls, len(alerts))
		}
		observer := requireAlertClass(t, alerts, "ipv6-observer-route-unavailable")
		if observer.Severity != SeverityWarn || observer.Target != "monitor-host/edge-ipv6" {
			t.Fatalf("%s: wrong observer identity or severity", test.name)
		}
		immediate, timeouts := 1, 2
		if test.allTimeout {
			immediate, timeouts = 0, 3
		}
		for _, want := range []string{"configured_targets=3", fmt.Sprintf("immediate_connect_failures=%d", immediate), fmt.Sprintf("connect_timeouts=%d", timeouts), "identity_healthy=2", "local_self_https_healthy=3", "source_route_exact=3", "source_egress_exact=3"} {
			if !strings.Contains(observer.Observed, want) {
				t.Errorf("%s: observer summary lacks %s", test.name, want)
			}
		}
		identity := requireAlertClass(t, alerts, "cannot-observe")
		if !strings.HasSuffix(identity.Target, "/public0/identity") || identity.Observed != "error_class=observation-ssh-exit-255" {
			t.Errorf("%s: identity transport class was lost", test.name)
		}
		for _, alert := range alerts {
			requireAlertOmits(t, alert, "synthetic-private-identity-detail", "not in table", "SSL connection timeout", "2001:db8:50::")
		}
	}
}

func TestEdgeIpv6ObserverTlsFailureControls(t *testing.T) {
	for _, test := range []struct {
		name          string
		routes        []string
		changedOutput string
		changedExit   int
		identityDrift bool
		routeMismatch bool
		wantClasses   map[string]int
	}{
		{name: "route available", routes: []string{"interface: synthetic0\n", "interface: synthetic0\n"}, wantClasses: map[string]int{"edge-ipv6-upstream-drop": 2, "edge-ipv6-reset": 1}},
		{name: "route unavailable", routes: []string{"unparseable synthetic route\n", "unparseable synthetic route\n"}, wantClasses: map[string]int{"cannot-observe": 3}},
		{name: "healthy sibling", changedOutput: edgeHTTPFixture("200", "0", "2001:db8:50::1", "0.080"), wantClasses: map[string]int{"edge-ipv6-upstream-drop": 1, "edge-ipv6-reset": 1}},
		{name: "HTTP response", changedOutput: edgeHTTPFixture("503", "0", "2001:db8:50::1", "0.080"), wantClasses: map[string]int{"edge-ipv6-http": 1, "edge-ipv6-upstream-drop": 1, "edge-ipv6-reset": 1}},
		{name: "timeout after response", changedOutput: edgeHTTPFixture("200", "28", "2001:db8:50::1", "3.000"), changedExit: 28, wantClasses: map[string]int{"edge-ipv6-http": 1, "edge-ipv6-upstream-drop": 1, "edge-ipv6-reset": 1}},
		{name: "malformed native result", changedOutput: "monitor_http_code=000\nmonitor_exitcode=28\n", changedExit: 28, wantClasses: map[string]int{"cannot-observe": 1, "edge-ipv6-upstream-drop": 1, "edge-ipv6-reset": 1}},
		{name: "independent identity drift", identityDrift: true, wantClasses: map[string]int{"edge-ipv6-identity-drift": 1, "ipv6-observer-route-unavailable": 1}},
		{name: "independent source-route drift", routeMismatch: true, wantClasses: map[string]int{"edge-ipv6-policy-route": 1, "ipv6-observer-route-unavailable": 1}},
	} {
		routes := test.routes
		if routes == nil {
			routes = []string{"not in table\n", "not in table\n"}
		}
		settings, _ := edgeObserverTlsSettings(routes)
		source := settings.Source.(*syntheticSource)
		localFn, hostFn := source.localFn, source.hostFn
		source.localFn = func(name string, args ...string) (string, error) {
			if test.changedOutput != "" && name == "curl" && strings.Contains(strings.Join(args, " "), "[2001:db8:50::1]") {
				return test.changedOutput, edgeCommandExitError(test.changedExit)
			}
			return localFn(name, args...)
		}
		source.hostFn = func(host HostSettings, command string) (string, error) {
			if test.identityDrift && strings.Contains(command, edgeIPv6IdentityMarker) && strings.Contains(command, "2001:db8:50::1") {
				return "operstate=up\nconfigured_present=0\nunit_active=active\n", nil
			}
			output, err := hostFn(host, command)
			if test.routeMismatch && strings.Contains(command, edgeIPv6EgressMarker) && strings.Contains(command, "2001:db8:50::1") {
				output = strings.ReplaceAll(output, "route_device=public0", "route_device=management0")
			}
			return output, err
		}
		alerts, err := NewEdgeIPv6Signal().Run(context.Background(), settings)
		if err != nil {
			t.Fatalf("%s: %v", test.name, err)
		}
		classes := map[string]int{}
		for _, alert := range alerts {
			classes[alert.Class]++
		}
		if !reflect.DeepEqual(classes, test.wantClasses) {
			t.Errorf("%s: classes=%v want=%v", test.name, classes, test.wantClasses)
		}
	}
}

func TestEdgeIpv6ObserverRouteLossPreservesIndependentAdmissionFault(t *testing.T) {
	settings, _ := edgeObserverTlsSettings([]string{"not in table\n", "not in table\n"})
	source := settings.Source.(*syntheticSource)
	hostFn := source.hostFn
	source.hostFn = func(host HostSettings, command string) (string, error) {
		if strings.Contains(command, edgeIPv6AdmissionMarker) {
			return "lb_observation_status=1\nlb_listener_count=0\nlb_map_hash_error_count=1\n", nil
		}
		output, err := hostFn(host, command)
		if strings.Contains(command, edgeIPv6EgressMarker) && strings.Contains(command, "2001:db8:50::3") {
			output = strings.ReplaceAll(output, "self_http_code=200\nself_exitcode=0\nself_probe_status=0", "self_http_code=000\nself_exitcode=7\nself_probe_status=7")
		}
		return output, err
	}
	alerts, err := NewEdgeIPv6Signal().Run(context.Background(), settings)
	if err != nil || len(alerts) != 2 {
		t.Fatalf("alerts=%d err=%v, want observer visibility plus the independently observed admission fault", len(alerts), err)
	}
	requireAlertClass(t, alerts, "ipv6-observer-route-unavailable")
	requireAlertClass(t, alerts, "edge-lb-config-rejected")
}

// The synthetic source joins each configured tuple to its own host controls.
// Two requests time out with selected peers; one fails without a selected peer.
func edgeObserverTlsSettings(routes []string) (SignalSettings, *int) {
	routeCalls := new(int)
	source := &syntheticSource{
		localFn: func(name string, args ...string) (string, error) {
			if name == "/sbin/route" {
				index := min(*routeCalls, len(routes)-1)
				*routeCalls++
				return routes[index], nil
			}
			for index := 1; index <= 3; index++ {
				address := fmt.Sprintf("2001:db8:50::%d", index)
				if strings.Contains(strings.Join(args, " "), "["+address+"]") {
					if index == 3 {
						return edgeHTTPFixture("000", "7", "", "0.0001"), edgeCommandExitError(7)
					}
					return "curl: (28) SSL connection timeout\n" + edgeHTTPFixture("000", "28", address, "3.002"), edgeCommandExitError(28)
				}
			}
			return "", errors.New("unexpected synthetic public target")
		},
		hostFn: func(_ HostSettings, command string) (string, error) {
			if strings.Contains(command, edgeIPv6IdentityMarker) {
				return "operstate=up\nconfigured_present=1\nunit_active=active\n", nil
			}
			for index := 1; index <= 3; index++ {
				address := fmt.Sprintf("2001:db8:50::%d", index)
				if strings.Contains(command, address) {
					return "self_http_code=200\nself_exitcode=0\nself_probe_status=0\nself_time_total=0.080\nroute_device=public" + strconv.Itoa(index-1) + "\nroute_source=" + address + "\nroute_status=0\nsource_egress=" + address + "\nsource_egress_status=0\n", nil
				}
			}
			return "", errors.New("unexpected synthetic host command")
		},
	}
	settings := syntheticSettings(source)
	settings.Hosts = []HostSettings{{Name: "synthetic-edge.example.test"}}
	for index := 1; index <= 3; index++ {
		settings.Hosts[0].EdgeIPv6 = append(settings.Hosts[0].EdgeIPv6, EdgeIPv6InterfaceSettings{Interface: "public" + strconv.Itoa(index-1), Address: fmt.Sprintf("2001:db8:50::%d", index), ProbeHostname: "api-v6.example.test"})
	}
	return settings, routeCalls
}

func TestEdgeIpv6IdentityFailureRetainsPrivateSafeClass(t *testing.T) {
	for _, test := range []struct {
		name   string
		output string
		err    error
		want   string
	}{
		{name: "ssh exit", err: &sshCommandError{err: fmt.Errorf("private synthetic text: %w", edgeCommandExitError(255))}, want: "observation-ssh-exit-255"},
		{name: "remote command", err: &sshCommandError{err: edgeCommandExitError(24)}, want: "observation-command-failed"},
		{name: "child timeout", err: context.DeadlineExceeded, want: "observation-timeout"},
		{name: "access denied", err: errors.New("permission denied private synthetic text"), want: "observation-access-denied"},
		{name: "malformed output", output: "private synthetic text\n", want: "observation-invalid-response"},
		{name: "partial output", output: "operstate=up\n", want: "observation-invalid-response"},
		{name: "absent output", want: "observation-invalid-response"},
	} {
		settings := edgeObservationSettings(edgeHTTPFixture("200", "0", "2001:db8::41", "0.080"), nil, test.output, test.err)
		alerts, err := NewEdgeIPv6Signal().Run(context.Background(), settings)
		if err != nil || len(alerts) != 1 {
			t.Fatalf("%s: alerts=%d err=%v", test.name, len(alerts), err)
		}
		if alerts[0].Class != "cannot-observe" || alerts[0].Observed != "error_class="+test.want {
			t.Errorf("%s: class=%s observed=%s", test.name, alerts[0].Class, alerts[0].Observed)
		}
		requireAlertOmits(t, alerts[0], "private synthetic text", "exit status", "permission denied")
	}
}

func TestEdgeIpv6MalformedIdentityCannotLeakIntoCompanionPathAlert(t *testing.T) {
	settings := edgeObservationSettings(edgeHTTPFixture("000", "28", "2001:db8::41", "3.002"), edgeCommandExitError(28), "private synthetic malformed identity\n", nil)
	alerts, err := NewEdgeIPv6Signal().Run(context.Background(), settings)
	if err != nil || len(alerts) != 2 {
		t.Fatalf("alerts=%d err=%v, want identity visibility plus the observed routed timeout", len(alerts), err)
	}
	requireAlertClass(t, alerts, "cannot-observe")
	requireAlertClass(t, alerts, "edge-ipv6-timeout")
	for _, alert := range alerts {
		requireAlertOmits(t, alert, "private synthetic malformed identity")
	}
}

func TestEdgeIPv6SignalSyntheticUnobservableRouteStaysPerTargetUnknown(t *testing.T) {
	addresses := []string{"2001:db8:4::40", "2001:db8:4::41"}
	source := &syntheticSource{
		localFn: func(name string, _ ...string) (string, error) {
			if name == "/sbin/route" {
				return "synthetic private route diagnostic", errors.New("synthetic route command failure")
			}
			return "curl: (7) Failed to connect to api-v6.example port 443 after 0 ms: Couldn't connect to server\n" +
				edgeHTTPFixture("000", "7", "", "0.000081"), edgeCommandExitError(7)
		},
		hostFn: func(_ HostSettings, command string) (string, error) {
			if strings.Contains(command, edgeIPv6IdentityMarker) {
				return "operstate=up\nconfigured_present=1\nunit_active=active\n", nil
			}
			if strings.Contains(command, edgeIPv6EgressMarker) {
				for index, address := range addresses {
					if strings.Contains(command, address) {
						return "self_http_code=200\nself_exitcode=0\nself_probe_status=0\n" +
							"route_device=public" + string(rune('0'+index)) + "\n" +
							"route_source=" + address + "\nroute_status=0\n" +
							"source_egress=" + address + "\nsource_egress_status=0\n", nil
					}
				}
			}
			return "", errors.New("unexpected host command")
		},
	}
	settings := syntheticSettings(source)
	settings.Hosts = []HostSettings{{Name: "edge-a", EdgeIPv6: []EdgeIPv6InterfaceSettings{
		{Interface: "public0", Address: addresses[0], ProbeHostname: "api-v6.example"},
		{Interface: "public1", Address: addresses[1], ProbeHostname: "api-v6.example"},
	}}}

	alerts, err := NewEdgeIPv6Signal().Run(context.Background(), settings)
	if err != nil {
		t.Fatal(err)
	}
	if len(alerts) != 2 {
		t.Fatalf("alerts = %d, want one unknown per target: %+v", len(alerts), alerts)
	}
	for _, alert := range alerts {
		if alert.Class != "cannot-observe" || alert.Severity != SeverityWarn {
			t.Fatalf("unobservable route alert = %+v", alert)
		}
		requireAlertOmits(t, alert, "synthetic private route diagnostic", "synthetic route command failure")
	}
}

func TestClassifyEdgeIPv6FailureSyntheticBranches(t *testing.T) {
	base := edgeIPv6Result{
		configured: EdgeIPv6InterfaceSettings{Interface: "public0", Address: "2001:db8::1"},
		http:       map[string]string{"monitor_http_code": "000", "monitor_exitcode": "28", "monitor_time_total": "3.001"},
		httpOutput: "curl: (28) Timeout was reached",
		identity:   map[string]string{"configured_present": "1"},
		egress: map[string]string{
			"self_http_code":       "200",
			"self_exitcode":        "0",
			"self_probe_status":    "0",
			"source_egress":        "2001:db8::1",
			"source_egress_status": "0",
			"route_device":         "public0",
			"route_source":         "2001:db8::1",
			"route_status":         "0",
		},
	}
	tests := []struct {
		name string
		edit func(*edgeIPv6Result)
		want string
	}{
		{name: "upstream drop", want: "edge-ipv6-upstream-drop", edit: func(*edgeIPv6Result) {}},
		{name: "policy route mismatch", want: "edge-ipv6-policy-route", edit: func(result *edgeIPv6Result) {
			result.egress["route_device"] = "management0"
			result.egress["route_source"] = "2001:db8:ffff::1"
		}},
		{name: "unproven timeout", want: "edge-ipv6-timeout", edit: func(result *edgeIPv6Result) {
			result.egress["self_http_code"] = "000"
		}},
		{name: "immediate reset", want: "edge-ipv6-reset", edit: func(result *edgeIPv6Result) {
			result.httpOutput = "connection refused"
			result.http["monitor_exitcode"] = "7"
			result.http["monitor_time_total"] = "0.041"
		}},
		{name: "connected non-200", want: "edge-ipv6-http", edit: func(result *edgeIPv6Result) {
			result.httpOutput = ""
			result.http["monitor_exitcode"] = "0"
			result.http["monitor_time_total"] = "0.080"
			result.http["monitor_http_code"] = "503"
		}},
		{name: "HTTP200 partial response timeout", want: "edge-ipv6-http", edit: func(result *edgeIPv6Result) {
			result.http["monitor_http_code"] = "200"
		}},
		{name: "non200 partial response timeout with route mismatch", want: "edge-ipv6-http", edit: func(result *edgeIPv6Result) {
			result.http["monitor_http_code"] = "503"
			result.egress["route_device"] = "management0"
			result.egress["route_source"] = "2001:db8:ffff::1"
		}},
		{name: "non200 partial response timeout without source proof", want: "edge-ipv6-http", edit: func(result *edgeIPv6Result) {
			result.http["monitor_http_code"] = "503"
			result.egress["self_http_code"] = "000"
		}},
	}
	for _, test := range tests {
		result := base
		result.http = cloneStringMap(base.http)
		result.egress = cloneStringMap(base.egress)
		test.edit(&result)
		class, mechanism, _ := classifyEdgeIPv6Failure(result)
		if class != test.want {
			t.Errorf("%s: class = %q, want %q", test.name, class, test.want)
		}
		if result.http["monitor_http_code"] != "000" && (strings.Contains(mechanism, "external ingress") || strings.Contains(mechanism, "silently timed out") || strings.Contains(mechanism, "policy routes/rules")) {
			t.Errorf("%s: observed HTTP response was attributed to initial TCP/TLS or ingress failure", test.name)
		}
	}
}

func cloneStringMap(values map[string]string) map[string]string {
	clone := make(map[string]string, len(values))
	for key, value := range values {
		clone[key] = value
	}
	return clone
}

func edgeHTTPFixture(code, exitCode, remoteIP, total string) string {
	return "monitor_http_code=" + code + "\n" +
		"monitor_exitcode=" + exitCode + "\n" +
		"monitor_remote_ip=" + remoteIP + "\n" +
		"monitor_content_type=\n" +
		"monitor_size_download=0\n" +
		"monitor_time_total=" + total + "\n"
}

func TestEdgeIPv6SignalInvalidNativeObservationsRemainUnknown(t *testing.T) {
	valid := edgeHTTPFixture("200", "0", "2001:db8::41", "0.080")
	refusal := edgeHTTPFixture("000", "7", "", "0.041")
	cases := []struct {
		name   string
		output string
		err    error
	}{
		{name: "missing curl", err: &exec.Error{Name: "curl", Err: exec.ErrNotFound}},
		{name: "empty success"},
		{name: "transport observation error", err: errors.New("synthetic private transport detail")},
		{name: "two-key forged healthy", output: "monitor_http_code=200\nmonitor_exitcode=0\n"},
		{name: "missing field", output: strings.Replace(valid, "monitor_remote_ip=2001:db8::41\n", "", 1)},
		{name: "duplicate field", output: valid + "monitor_exitcode=0\n"},
		{name: "extra native field", output: valid + "monitor_unrecognized=synthetic-private-value\n"},
		{name: "malformed field", output: strings.Replace(valid, "monitor_time_total=0.080", "monitor_time_total", 1)},
		{name: "wrong remote peer", output: strings.Replace(valid, "2001:db8::41", "2001:db8::42", 1)},
		{name: "missing connected peer", output: strings.Replace(valid, "2001:db8::41", "", 1)},
		{name: "invalid status range", output: strings.Replace(valid, "monitor_http_code=200", "monitor_http_code=999", 1)},
		{name: "invalid exit", output: strings.Replace(valid, "monitor_exitcode=0", "monitor_exitcode=invalid", 1)},
		{name: "inconsistent zero status", output: strings.Replace(valid, "monitor_http_code=200", "monitor_http_code=000", 1)},
		{name: "inconsistent process error", output: valid, err: errors.New("synthetic private transport detail")},
		{name: "refusal missing process error", output: refusal},
		{name: "signed HTTP code", output: strings.Replace(refusal, "monitor_http_code=000", "monitor_http_code=-00", 1), err: edgeCommandExitError(7)},
		{name: "plus-signed HTTP zero", output: strings.Replace(refusal, "monitor_http_code=000", "monitor_http_code=+00", 1), err: edgeCommandExitError(7)},
		{name: "mismatched process status", output: refusal, err: edgeCommandExitError(28)},
		{name: "complete metadata with transport error", output: refusal, err: errors.New("synthetic private transport detail")},
		{name: "forged exit-status transport error", output: refusal, err: errors.New("exit status 7")},
		{name: "HTTP200 incompatible refusal", output: edgeHTTPFixture("200", "7", "2001:db8::41", "0.041"), err: edgeCommandExitError(7)},
		{name: "HTTP503 incompatible refusal", output: edgeHTTPFixture("503", "7", "2001:db8::41", "0.041"), err: edgeCommandExitError(7)},
		{name: "HTTP200 incompatible TLS failure", output: edgeHTTPFixture("200", "60", "2001:db8::41", "0.080"), err: edgeCommandExitError(60)},
		{name: "HTTP503 incompatible TLS failure", output: edgeHTTPFixture("503", "60", "2001:db8::41", "0.080"), err: edgeCommandExitError(60)},
		{name: "NaN duration", output: strings.Replace(valid, "monitor_time_total=0.080", "monitor_time_total=NaN", 1)},
		{name: "infinite duration", output: strings.Replace(valid, "monitor_time_total=0.080", "monitor_time_total=+Inf", 1)},
		{name: "negative duration", output: strings.Replace(valid, "monitor_time_total=0.080", "monitor_time_total=-0.080", 1)},
		{name: "NaN size", output: strings.Replace(valid, "monitor_size_download=0", "monitor_size_download=NaN", 1)},
		{name: "negative size", output: strings.Replace(valid, "monitor_size_download=0", "monitor_size_download=-1", 1)},
	}
	for _, test := range cases {
		settings := edgeObservationSettings(test.output, test.err, "operstate=up\nconfigured_present=1\nunit_active=active\n", nil)
		alerts, err := NewEdgeIPv6Signal().Run(context.Background(), settings)
		if err != nil {
			t.Fatalf("%s: %v", test.name, err)
		}
		if len(alerts) != 1 || alerts[0].Class != "cannot-observe" || alerts[0].Severity != SeverityWarn {
			t.Errorf("%s: incomplete native observation produced %d alerts instead of one visibility warning", test.name, len(alerts))
			continue
		}
		if alerts[0].SignalNumber != "18.1" || alerts[0].SignalKey != "edge-ipv6" || !strings.Contains(alerts[0].Markdown(), "SIGNALS.md §18.1") {
			t.Errorf("%s: native observation warning lost owning signal/playbook", test.name)
		}
		requireAlertOmits(t, alerts[0], "synthetic-private-value", "synthetic private transport detail")
	}
}

func TestEdgeIPv6SignalCompleteNativeResultsPreserveFailures(t *testing.T) {
	cases := []struct {
		name     string
		http     string
		exit     int
		peer     string
		duration string
		want     string
	}{
		{name: "healthy", http: "200", peer: "2001:db8::41", duration: "0.080"},
		{name: "equivalent expanded IPv6 peer", http: "200", peer: "2001:0db8:0000:0000:0000:0000:0000:0041", duration: "0.080"},
		{name: "real refusal", http: "000", exit: 7, duration: "0.041", want: "edge-ipv6-reset"},
		{name: "real timeout", http: "000", exit: 28, duration: "3.001", want: "edge-ipv6-upstream-drop"},
		{name: "HTTP200 partial response timeout", http: "200", exit: 28, peer: "2001:db8::41", duration: "3.001", want: "edge-ipv6-http"},
		{name: "HTTP503 partial response timeout", http: "503", exit: 28, peer: "2001:db8::41", duration: "3.001", want: "edge-ipv6-http"},
		{name: "real TLS failure", http: "000", exit: 60, peer: "2001:db8::41", duration: "0.080", want: "edge-ipv6-http"},
		{name: "observed HTTP non200", http: "503", peer: "2001:db8::41", duration: "0.080", want: "edge-ipv6-http"},
	}
	for _, test := range cases {
		settings := edgeObservationSettings(edgeHTTPFixture(test.http, strconv.Itoa(test.exit), test.peer, test.duration), edgeCommandExitError(test.exit), "operstate=up\nconfigured_present=1\nunit_active=active\n", nil)
		alerts, err := NewEdgeIPv6Signal().Run(context.Background(), settings)
		if err != nil {
			t.Fatalf("%s: %v", test.name, err)
		}
		if test.want == "" {
			if len(alerts) != 0 {
				t.Errorf("%s: complete healthy edge emitted %d alerts", test.name, len(alerts))
			}
			continue
		}
		if len(alerts) != 1 || alerts[0].Class != test.want || alerts[0].Severity != SeverityPage || alerts[0].Sustain != 2 {
			t.Errorf("%s: genuine native failure lost its existing class/severity", test.name)
			continue
		}
		if test.http != "000" && (strings.Contains(alerts[0].Mechanism, "external ingress") || strings.Contains(alerts[0].Mechanism, "silently timed out") || strings.Contains(alerts[0].Mechanism, "policy routes/rules")) {
			t.Errorf("%s: observed HTTP response was attributed to initial TCP/TLS or ingress failure", test.name)
		}
		if test.http == "000" && test.want == "edge-ipv6-http" && strings.Contains(alerts[0].Mechanism, "exact address connected") {
			t.Errorf("%s: request failure falsely proves a connection", test.name)
		}
	}
}

func TestEdgeIPv6IdentityGeneratedCommandDistinguishesObservationFailure(t *testing.T) {
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Fatal(err)
	}
	awk, err := exec.LookPath("awk")
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name string
		want string
	}{
		{name: "healthy"},
		{name: "address-absent", want: "edge-ipv6-identity-drift"},
		{name: "link-down", want: "edge-ipv6-identity-drift"},
		{name: "unit-inactive", want: "edge-ipv6-identity-drift"},
		{name: "cat-missing", want: "cannot-observe"},
		{name: "ip-missing", want: "cannot-observe"},
		{name: "systemctl-missing", want: "cannot-observe"},
		{name: "cat-interrupted", want: "cannot-observe"},
		{name: "ip-interrupted", want: "cannot-observe"},
		{name: "systemctl-interrupted", want: "cannot-observe"},
		{name: "malformed-ip", want: "cannot-observe"},
		{name: "malformed-state", want: "cannot-observe"},
	}
	for _, test := range cases {
		prefix := "scenario=" + shellSingleQuote(test.name) + `
cat() {
 case "$scenario" in cat-interrupted) return 143;; link-down) printf 'down\n';; malformed-state) printf 'invalid-state\n';; *) printf 'up\n';; esac
}
ip() {
 case "$scenario" in
  ip-interrupted) return 143;;
  address-absent) return 0;;
  malformed-ip) printf 'synthetic invalid row\n';;
  *) printf '2: synthetic-if-a inet6 2001:db8::41/64 scope global\n';;
 esac
}
systemctl() {
 case "$scenario" in systemctl-interrupted) return 143;; unit-inactive) printf 'inactive\n'; return 3;; *) printf 'active\n';; esac
}
timeout() { shift; "$@"; }
` + "awk() { " + shellSingleQuote(awk) + " \"$@\"; }\n" + `
case "$scenario" in cat-missing) unset -f cat;; ip-missing) unset -f ip;; systemctl-missing) unset -f systemctl;; esac
`
		configured := EdgeIPv6InterfaceSettings{Interface: "synthetic-if-a", Address: "2001:db8::41", ProbeHostname: "api-v6.example.test"}
		command := exec.Command(bash, "-c", prefix+edgeIPv6IdentityCommand(configured))
		command.Env = append(os.Environ(), "PATH="+t.TempDir())
		output, commandErr := command.CombinedOutput()
		settings := edgeObservationSettings(edgeHTTPFixture("200", "0", configured.Address, "0.080"), nil, string(output), commandErr)
		alerts, err := NewEdgeIPv6Signal().Run(context.Background(), settings)
		if err != nil {
			t.Fatalf("%s: %v", test.name, err)
		}
		if test.want == "" {
			if len(alerts) != 0 {
				t.Errorf("%s: observed healthy identity produced %d alerts", test.name, len(alerts))
			}
			continue
		}
		if len(alerts) != 1 || alerts[0].Class != test.want {
			t.Errorf("%s: actual identity command converted observation failure into the wrong finding", test.name)
		}
	}
}

func TestEdgeIPv6InvalidDurationCannotDefineCommonMode(t *testing.T) {
	for _, duration := range []string{"NaN", "+Inf", "-Inf", "-0.1", "", "invalid"} {
		output := edgeHTTPFixture("000", "7", "", duration)
		result := edgeIPv6Result{
			host: &host{name: "synthetic-edge.example.test"}, configured: EdgeIPv6InterfaceSettings{Interface: "synthetic-if-a", Address: "2001:db8::41"},
			http: parseKeyValueLines(output), httpOutput: output, httpErr: edgeCommandExitError(7),
			identity: map[string]string{"operstate": "up", "configured_present": "1", "unit_active": "active"},
			egress: map[string]string{
				"self_probe_status": "7", "self_exitcode": "7", "self_http_code": "000", "self_time_total": duration,
				"route_status": "0", "route_device": "synthetic-if-a", "route_source": "2001:db8::41", "source_egress_status": "0", "source_egress": "2001:db8::41",
			},
		}
		if edgeIPv6AllImmediateConnectFailures([]edgeIPv6Result{result}) || edgeIPv6AdmissionCandidate(result) {
			t.Errorf("%q: unobserved/invalid duration defined common mode or admission cause", duration)
		}
		findings := edgeIPv6Findings(result, false, false)
		if len(findings) != 1 || findings[0].class != "cannot-observe" || findings[0].healthy {
			t.Errorf("%q: invalid duration produced outage/health instead of visibility", duration)
		}
	}
}

func TestEdgeIPv6InvalidSelfDurationCannotProveAdmission(t *testing.T) {
	output := edgeHTTPFixture("000", "7", "", "0.041")
	result := edgeIPv6Result{
		http: parseKeyValueLines(output), httpOutput: output, httpErr: edgeCommandExitError(7),
		configured: EdgeIPv6InterfaceSettings{Interface: "synthetic-if-a", Address: "2001:db8::41"},
		identity:   map[string]string{"operstate": "up", "configured_present": "1", "unit_active": "active"},
		egress: map[string]string{
			"self_probe_status": "7", "self_exitcode": "7", "self_http_code": "000", "self_time_total": "0.041",
			"route_status": "0", "route_device": "synthetic-if-a", "route_source": "2001:db8::41", "source_egress_status": "0", "source_egress": "2001:db8::41",
		},
	}
	if !edgeIPv6AdmissionCandidate(result) {
		t.Fatal("complete valid refusal lost its admission-candidate control")
	}
	for _, duration := range []string{"NaN", "+Inf", "-Inf", "-0.1", "", "invalid"} {
		result.egress["self_time_total"] = duration
		if edgeIPv6AdmissionCandidate(result) {
			t.Errorf("%q: unobserved self duration proved admission causality", duration)
		}
	}
}

func TestEdgeIPv6SignalPreCanceledContextMakesNoSourceCalls(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var calls atomic.Int32
	source := &syntheticSource{
		localFn: func(string, ...string) (string, error) { calls.Add(1); return "", ctx.Err() },
		hostFn:  func(HostSettings, string) (string, error) { calls.Add(1); return "", ctx.Err() },
	}
	settings := syntheticSettings(source)
	settings.Hosts = []HostSettings{{Name: "synthetic-edge.example.test", EdgeIPv6: []EdgeIPv6InterfaceSettings{{Interface: "synthetic-if-a", Address: "2001:db8::41", ProbeHostname: "api-v6.example.test"}}}}
	alerts, err := NewEdgeIPv6Signal().Run(ctx, settings)
	if !errors.Is(err, context.Canceled) || len(alerts) != 0 || calls.Load() != 0 {
		t.Fatalf("parent cancellation manufactured findings or source work: canceled=%t alerts=%d calls=%d", errors.Is(err, context.Canceled), len(alerts), calls.Load())
	}
}

func TestEdgeIPv6SignalInFlightCancellationCannotFabricateFindings(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	started := make(chan struct{})
	release := make(chan struct{})
	var hostCalls atomic.Int32
	source := &syntheticSource{
		localFn: func(name string, _ ...string) (string, error) {
			if name == "/sbin/route" {
				return "interface: synthetic0\n", nil
			}
			close(started)
			<-release
			return "", ctx.Err()
		},
		hostFn: func(HostSettings, string) (string, error) { hostCalls.Add(1); return "", ctx.Err() },
	}
	settings := syntheticSettings(source)
	settings.Hosts = []HostSettings{{Name: "synthetic-edge.example.test", EdgeIPv6: []EdgeIPv6InterfaceSettings{{Interface: "synthetic-if-a", Address: "2001:db8::41", ProbeHostname: "api-v6.example.test"}}}}
	type signalResult struct {
		alerts Alerts
		err    error
	}
	result := make(chan signalResult, 1)
	go func() {
		alerts, err := NewEdgeIPv6Signal().Run(ctx, settings)
		result <- signalResult{alerts: alerts, err: err}
	}()
	<-started
	cancel()
	close(release)
	observed := <-result
	if !errors.Is(observed.err, context.Canceled) || len(observed.alerts) != 0 || hostCalls.Load() != 0 {
		t.Fatalf("in-flight parent cancellation fabricated findings/work: canceled=%t alerts=%d host_calls=%d", errors.Is(observed.err, context.Canceled), len(observed.alerts), hostCalls.Load())
	}
}

func TestEdgeIPv6SignalChildDeadlinePreservesIndependentIdentity(t *testing.T) {
	settings := edgeObservationSettings("", context.DeadlineExceeded, "operstate=up\nconfigured_present=0\nunit_active=active\n", nil)
	alerts, err := NewEdgeIPv6Signal().Run(context.Background(), settings)
	if err != nil {
		t.Fatal("child timeout replaced the live parent's result")
	}
	if len(alerts) != 2 {
		t.Fatalf("child timeout lost independent evidence or fabricated an HTTP outage: alerts=%d", len(alerts))
	}
	requireAlertClass(t, alerts, "edge-ipv6-identity-drift")
	requireAlertClass(t, alerts, "cannot-observe")
}

// edgeObservationSettings isolates every transport behind fixed synthetic
// results while retaining the normal Signal adapter and public observation.
func edgeObservationSettings(output string, commandErr error, identity string, identityErr error) SignalSettings {
	source := &syntheticSource{
		localFn: func(name string, _ ...string) (string, error) {
			if name == "/sbin/route" {
				return "interface: synthetic0\n", nil
			}
			return output, commandErr
		},
		hostFn: func(_ HostSettings, command string) (string, error) {
			if strings.Contains(command, edgeIPv6IdentityMarker) {
				return identity, identityErr
			}
			return "self_http_code=200\nself_exitcode=0\nself_probe_status=0\nself_time_total=0.080\nroute_device=synthetic-if-a\nroute_source=2001:db8::41\nroute_status=0\nsource_egress=2001:db8::41\nsource_egress_status=0\n", nil
		},
	}
	settings := syntheticSettings(source)
	settings.Hosts = []HostSettings{{Name: "synthetic-edge.example.test", EdgeIPv6: []EdgeIPv6InterfaceSettings{{Interface: "synthetic-if-a", Address: "2001:db8::41", ProbeHostname: "api-v6.example.test"}}}}
	return settings
}

// edgeCommandExitError carries the real local process exit semantics without
// launching curl or contacting any configured endpoint.
func edgeCommandExitError(code int) error {
	if code == 0 {
		return nil
	}
	return exec.Command("/bin/sh", "-c", "exit "+strconv.Itoa(code)).Run()
}
