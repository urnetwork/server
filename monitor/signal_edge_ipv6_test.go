package monitor

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestActiveEdgeIPv6FromServicesUsesOnlyCurrentNontransparentInterfaces(t *testing.T) {
	services := servicesYaml{
		Domain: "bringyour.com",
		Versions: []servicesVersionYaml{
			{LB: servicesLBYaml{Interfaces: map[string]map[string]servicesLBInterfaceYaml{
				"by-us-fmt-5-edge-3.bringyour.com": {
					"eno2np1": {IPv6: "2001:db8:5860::381"},
					"eno1np0": {IPv6: "2001:db8:5880::380"},
				},
				"fireside.bringyour.com": {
					"eno1np0": {IPv6: "2001:db8:5960::1", Transparent: true},
				},
			}}},
			{LB: servicesLBYaml{Interfaces: map[string]map[string]servicesLBInterfaceYaml{
				"by-us-fmt-5-edge-3.bringyour.com": {
					"eno3": {IPv6: "2001:db8::e382"},
				},
			}}},
		},
	}

	got, err := activeEdgeIPv6FromServices(services)
	if err != nil {
		t.Fatal(err)
	}
	edge := got["by-us-fmt-5-edge-3"]
	if len(edge) != 2 {
		t.Fatalf("active edge interfaces = %+v, want two", edge)
	}
	if edge[0].Interface != "eno1np0" || edge[0].Address != "2001:db8:5880::380" ||
		edge[1].Interface != "eno2np1" || edge[1].Address != "2001:db8:5860::381" {
		t.Fatalf("active edge interfaces = %+v", edge)
	}
	if edge[0].ProbeHostname != "api-v6.bringyour.com" {
		t.Fatalf("probe hostname = %q", edge[0].ProbeHostname)
	}
	if _, ok := got["fireside"]; ok {
		t.Fatalf("transparent proxy host became an edge IPv6 target: %+v", got)
	}
	for _, configured := range edge {
		if strings.Contains(configured.Address, "e382") || configured.Interface == "eno3" {
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
				return edgeHTTPFixture("000", "7", "", "0.041"), errors.New("exit status 7")
			case strings.Contains(joined, "["+addresses["drop"]+"]"):
				return "curl: (28) Timeout was reached\n" + edgeHTTPFixture("000", "28", "", "3.002"), errors.New("exit status 28")
			case strings.Contains(joined, "["+addresses["policy"]+"]"):
				return "curl: (28) Timeout was reached\n" + edgeHTTPFixture("000", "28", "", "3.003"), errors.New("exit status 28")
			case strings.Contains(joined, "["+addresses["drift"]+"]"):
				return "curl: (28) Timeout was reached\n" + edgeHTTPFixture("000", "28", "", "3.001"), errors.New("exit status 28")
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
		Name: "by-us-fmt-5-edge-3",
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
				return "route: writing to routing socket: not in table\n", errors.New("exit status 1")
			}
			if name != "curl" {
				return "", errors.New("unexpected local command")
			}
			return "curl: (7) Failed to connect to api-v6.example port 443 after 0 ms: Couldn't connect to server\n" +
				edgeHTTPFixture("000", "7", "", "0.000106"), errors.New("exit status 7")
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
				return "route to: 2606:4700:4700::1111\ninterface: en0\n", nil
			}
			return "curl: (7) Failed to connect to api-v6.example port 443 after 0 ms: Couldn't connect to server\n" +
				edgeHTTPFixture("000", "7", "", "0.000072"), errors.New("exit status 7")
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

func TestEdgeIPv6SignalSyntheticUnobservableRouteStaysPerTargetUnknown(t *testing.T) {
	addresses := []string{"2001:db8:4::40", "2001:db8:4::41"}
	source := &syntheticSource{
		localFn: func(name string, _ ...string) (string, error) {
			if name == "/sbin/route" {
				return "synthetic private route diagnostic", errors.New("synthetic route command failure")
			}
			return "curl: (7) Failed to connect to api-v6.example port 443 after 0 ms: Couldn't connect to server\n" +
				edgeHTTPFixture("000", "7", "", "0.000081"), errors.New("exit status 7")
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
		http:       map[string]string{"monitor_exitcode": "28", "monitor_time_total": "3.001"},
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
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			result := base
			result.http = cloneStringMap(base.http)
			result.egress = cloneStringMap(base.egress)
			test.edit(&result)
			class, _, _ := classifyEdgeIPv6Failure(result)
			if class != test.want {
				t.Fatalf("class = %q, want %q", class, test.want)
			}
		})
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
		"monitor_time_total=" + total + "\n"
}
