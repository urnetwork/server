package monitor

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestActiveProxyPathsFromServicesUsesCurrentPlacementAndStableRoutingTable(t *testing.T) {
	services := servicesYaml{
		Domain: "example.test",
		Versions: []servicesVersionYaml{
			{
				RoutingTables: "100-102",
				HostServices: map[string][]string{
					"alpha.example.test": {"proxy"},
					"beta.example.test":  {"proxy"},
					"edge.example.test":  {"api"},
				},
				Services: map[string]servicesServiceYaml{
					"proxy": {Hosts: []string{"alpha.example.test", "beta.example.test"}},
				},
				LB: servicesLBYaml{Interfaces: map[string]map[string]servicesLBInterfaceYaml{
					"alpha.example.test": {
						"proxy0": {IPv4: "192.0.2.10", IPv6: "2001:db8::10", Transparent: true},
					},
					"beta.example.test": {
						"proxy0": {IPv4: "192.0.2.20", Transparent: true},
					},
					"edge.example.test": {
						"public0": {IPv4: "192.0.2.30"},
					},
				}},
			},
			{
				RoutingTables: "100-102",
				LB: servicesLBYaml{Interfaces: map[string]map[string]servicesLBInterfaceYaml{
					"alpha.example.test": {
						"public0": {IPv4: "192.0.2.11", ExternalPorts: map[int]int{443: 443}},
						"proxy0":  {IPv4: "192.0.2.10", IPv6: "2001:db8::10", Transparent: true},
					},
					"beta.example.test": {
						"proxy0": {IPv4: "192.0.2.20", Transparent: true},
					},
				}},
			},
		},
	}

	got, configured, err := activeProxyPathsFromServices("synthetic", services)
	if err != nil {
		t.Fatal(err)
	}
	if !configured {
		t.Fatal("active proxy service was reported absent")
	}
	want := map[string]*ProxyHostSettings{
		"alpha": {
			PublicHostname: "alpha.example.test", PublicInterface: "proxy0",
			RoutingTable: 101, LoadBalancerUnit: "warp-synthetic-lb-proxy0.service",
			AddressFamilies: []string{"ipv4", "ipv6"},
		},
		"beta": {
			PublicHostname: "beta.example.test", PublicInterface: "proxy0",
			RoutingTable: 100, LoadBalancerUnit: "warp-synthetic-lb-proxy0.service",
			AddressFamilies: []string{"ipv4"},
		},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("proxy paths = %#v, want %#v", got, want)
	}
}

func TestActiveProxyPathsDistinguishesAbsentServiceFromEmptyPlacement(t *testing.T) {
	legacy := &ProxyHostSettings{PublicHostname: "legacy.example.test"}
	absent := servicesYaml{
		Domain: "example.test",
		Versions: []servicesVersionYaml{{
			RoutingTables: "100-101",
			Services:      map[string]servicesServiceYaml{"api": {}},
		}},
	}
	paths, configured, err := activeProxyPathsFromServices("synthetic", absent)
	if err != nil {
		t.Fatal(err)
	}
	if configured || len(paths) != 0 {
		t.Fatalf("absent proxy service = configured %v, paths %d", configured, len(paths))
	}
	if got := selectedProxyHostSettings(nil, configured, legacy); !reflect.DeepEqual(got, legacy) {
		t.Fatalf("absent service did not retain legacy fallback: %#v", got)
	}

	presentEmpty := absent
	presentEmpty.Versions = append([]servicesVersionYaml(nil), absent.Versions...)
	presentEmpty.Versions[0].Services = map[string]servicesServiceYaml{"proxy": {}}
	paths, configured, err = activeProxyPathsFromServices("synthetic", presentEmpty)
	if err != nil {
		t.Fatal(err)
	}
	if !configured || len(paths) != 0 {
		t.Fatalf("empty proxy placement = configured %v, paths %d", configured, len(paths))
	}
	if got := selectedProxyHostSettings(nil, configured, legacy); got != nil {
		t.Fatalf("present empty placement resurrected legacy target: %#v", got)
	}
}

func TestProxyPathSignalDoesNotSilentlyIgnoreMissingDerivedInventory(t *testing.T) {
	settings := syntheticSettings(&syntheticSource{
		hostFn: func(HostSettings, string) (string, error) {
			return "", errors.New("host transport must not run without an armed target")
		},
	})
	settings.ProxyPathExpectedHosts = 2

	alerts, err := NewProxyPathSignal().Run(context.Background(), settings)
	if err != nil {
		t.Fatal(err)
	}
	alert := requireAlertClass(t, alerts, "cannot-observe")
	if alert.Target != "proxy-path/inventory" || !strings.Contains(alert.Observed, "error_class=observation-unclassified") {
		t.Fatalf("missing inventory alert = %+v", alert)
	}
}

func TestProxyPathSignalClassifiesDockerDiscoveryFailure(t *testing.T) {
	hostile := "private-runtime-detail-should-not-render"
	source := &syntheticSource{hostFn: func(_ HostSettings, command string) (string, error) {
		if !strings.Contains(command, proxyAllocationMarker) ||
			!strings.Contains(command, `docker_status=$?`) ||
			!strings.Contains(command, `inspect_status=$?`) ||
			!strings.Contains(command, `exit "$docker_status"`) ||
			!strings.Contains(command, `exit "$inspect_status"`) {
			return "", errors.New("unexpected synthetic host command")
		}
		return "", errors.New("permission denied: " + hostile)
	}}
	settings := syntheticSettings(source)
	settings.ProxyPathExpectedHosts = 1
	settings.Hosts = append(settings.Hosts, HostSettings{
		Name: "proxy-1",
		Proxy: &ProxyHostSettings{
			PublicHostname:  "proxy.example.test",
			AddressFamilies: []string{"ipv4"},
		},
	})

	alerts, err := NewProxyPathSignal().Run(context.Background(), settings)
	if err != nil {
		t.Fatal(err)
	}
	alert := requireAlertClass(t, alerts, "cannot-observe")
	if !strings.Contains(alert.Observed, "error_class=observation-access-denied") {
		t.Fatalf("discovery failure lost fixed class: %+v", alert)
	}
	markdown := alert.Markdown()
	for _, want := range []string{
		"allocation_count=unknown",
		"cannot distinguish an absent allocation from a healthy allocation",
		"least-privilege, read-only helper",
		"Do not add the identity to the Docker group",
		"Run proxy-path twice",
	} {
		if !strings.Contains(markdown, want) {
			t.Fatalf("discovery visibility alert missing %q:\n%s", want, markdown)
		}
	}
	if strings.Contains(markdown, hostile) {
		t.Fatal("discovery failure leaked raw runtime error")
	}
}

func TestProxyPathAllocationAccessDeniedDoesNotProveRemoteExecution(t *testing.T) {
	for _, testCase := range []struct {
		name   string
		status int
		stderr string
		class  string
	}{
		{"ssh-authentication", 255, "Permission denied (publickey)", observationErrorClassAccessDenied},
		{"remote-runtime-denial", 1, "permission denied while connecting to the Docker daemon", observationErrorClassAccessDenied},
		{"ambiguous-remote-status-255", 255, "permission denied", observationErrorClassAccessDenied},
		{"other-command-failure", 1, "synthetic command failed", observationErrorClassCommandFailed},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			nativeExit := syntheticProcessExit(t, testCase.status)
			cfg := &monitorConfig{addressMode: addressModeOverlay, commandTimeout: time.Second}
			runner := newRunner(cfg)
			runner.runSSH = func(_ context.Context, args []string, _ string) (string, string, error) {
				if !strings.Contains(args[len(args)-1], proxyAllocationMarker) {
					t.Fatal("fixture did not exercise the actual allocation discovery command")
				}
				return "", testCase.stderr + " private-auth-reason address=192.0.2.91", nativeExit
			}
			target := &host{name: "proxy-1", overlayIp: "192.0.2.91"}
			allocations, err := discoverProxyAllocations(context.Background(), &probeEnv{cfg: cfg, runner: runner}, target)
			if err == nil || len(allocations) != 0 || classifyObservationError(err) != testCase.class {
				t.Fatal("allocation discovery lost the native observation failure")
			}
			finding := proxyAllocationCannotObserveFinding(target.name, err)
			alert := alertFromFinding(syntheticSettings(&syntheticSource{}), "14.5", "proxy-path", "Public proxy path", finding)
			if alert.Class != "cannot-observe" || alert.Target != "proxy-1/proxy-allocations" || alert.Sustain != 2 {
				t.Fatal("phase correction changed visibility identity or sustain")
			}
			requireAlertOmits(t, alert, "private-auth-reason", "192.0.2.91", "Permission denied (publickey)")
			if testCase.class == observationErrorClassCommandFailed {
				if alert.Observed != "error_class="+observationErrorClassCommandFailed {
					t.Fatal("non-access failure was relabeled as access denial")
				}
				return
			}
			for _, required := range []string{"SSH authentication", "remote container-runtime/helper authorization", "does not prove that the remote command ran", "allocation_count=unknown", "phase-specific evidence"} {
				if !strings.Contains(alert.Markdown(), required) {
					t.Fatalf("allocation denial lost phase ambiguity: missing %q", required)
				}
			}
			for _, falseClaim := range []string{"The monitor reached the proxy host", "The container-runtime command returned nonzero", "allocations=0 would be healthy"} {
				if strings.Contains(alert.Markdown(), falseClaim) {
					t.Fatal("access-denied class alone asserted remote execution or service health")
				}
			}
		})
	}
}

func TestProxyPathSignalReportsBlockWithNoReadyAllocation(t *testing.T) {
	source := &syntheticSource{hostFn: func(_ HostSettings, command string) (string, error) {
		switch {
		case strings.Contains(command, proxyAllocationMarker):
			return strings.Join([]string{
				"synthetic-proxy-g1-first|80:12080,8080:12081,8081:12082,8082:12083|503",
				"synthetic-proxy-g1-second|80:13080,8080:13081,8081:13082,8082:13083|missing",
			}, "\n"), nil
		case strings.Contains(command, proxyRouteMarker):
			return "networkd_start=100\nlb_start=200\nv4_routes=1\nv6_routes=0\nv4_rules=1\nv6_rules=0", nil
		case strings.Contains(command, edgeAutoUpgradeMarker):
			return "periodic_enable=0\napt-daily.timer=masked\napt-daily-upgrade.timer=masked\napt-daily.service=masked\napt-daily-upgrade.service=masked\nunattended-upgrades.service=masked", nil
		default:
			return "", errors.New("unexpected synthetic host command")
		}
	}}
	settings := syntheticSettings(source)
	settings.ProxyPathExpectedHosts = 1
	settings.Hosts = append(settings.Hosts, HostSettings{
		Name: "proxy-1",
		Proxy: &ProxyHostSettings{
			PublicHostname:   "proxy.example.test",
			PublicInterface:  "public0",
			RoutingTable:     100,
			LoadBalancerUnit: "warp-synthetic-lb-public0.service",
			AddressFamilies:  []string{"ipv4"},
		},
	})

	alerts, err := NewProxyPathSignal().Run(context.Background(), settings)
	if err != nil {
		t.Fatal(err)
	}
	alert := requireAlertClass(t, alerts, "proxy-allocation-unready")
	for _, want := range []string{
		"allocations=2 ready_allocations=0",
		"http-503:1,unavailable:1",
		"false green",
		"not a public DNAT",
		"do not change public DNS",
		"Two consecutive proxy-path probes",
	} {
		if !strings.Contains(strings.ToLower(alert.Markdown()), strings.ToLower(want)) {
			t.Fatalf("unready allocation alert missing %q:\n%s", want, alert.Markdown())
		}
	}
	for _, unexpected := range alerts {
		if unexpected.Class == "proxy-public-handshake" {
			t.Fatalf("unready allocation was also tested as a public handshake: %+v", unexpected)
		}
	}
}

func TestProxyPathAllocationDiscoveryPrefersRestrictedHelper(t *testing.T) {
	cfg := &monitorConfig{env: "main", addressMode: addressModeOverlay, commandTimeout: time.Second}
	runner := newRunner(cfg)
	runner.runSSH = func(_ context.Context, args []string, _ string) (string, string, error) {
		command := args[len(args)-1]
		helper := strings.Index(command, "/usr/bin/sudo -n /usr/local/sbin/monitor-proxy-allocations")
		fallback := strings.Index(command, "names=$(docker ps")
		if !strings.Contains(command, proxyAllocationMarker) ||
			!strings.Contains(command, "if [ -x /usr/local/sbin/monitor-proxy-allocations ]; then") ||
			helper < 0 || fallback <= helper ||
			!strings.Contains(command, "exit $?") {
			t.Fatal("allocation discovery did not prefer the restricted helper and stop on its failure")
		}
		return "main-proxy-g1-synthetic|80:12080,8080:12081|204\n", "", nil
	}
	target := &host{name: "synthetic-proxy", overlayIp: "192.0.2.91"}
	allocations, err := discoverProxyAllocations(context.Background(), &probeEnv{cfg: cfg, runner: runner}, target)
	if err != nil {
		t.Fatal(err)
	}
	if len(allocations) != 1 || allocations[0].block != "g1" || allocations[0].ports[80] != 12080 || allocations[0].internalStatus != 204 {
		t.Fatalf("restricted helper output was not parsed: %+v", allocations)
	}
}

func TestProxyPathToleratesUnreadyDrainingSiblingWhenBlockRemainsReady(t *testing.T) {
	source := &syntheticSource{
		localFn: func(name string, _ ...string) (string, error) {
			if name != "curl" {
				return "", errors.New("unexpected synthetic local command")
			}
			return "407", nil
		},
		tcpFn: func(string, string, []byte, int) ([]byte, error) {
			return []byte{0x05, 0x02}, nil
		},
	}
	env, err := newProbeEnv(syntheticSettings(source))
	if err != nil {
		t.Fatal(err)
	}
	target := &host{
		name: "proxy-1",
		proxy: &ProxyHostSettings{
			PublicHostname: "proxy.example.test",
		},
	}
	ports := map[int]int{80: 13080, 8080: 13081, 8081: 13082, 8082: 13083}
	findings := evaluateProxyHandshakes(
		context.Background(),
		env,
		target,
		[]string{"ipv4"},
		[]proxyAllocation{
			{container: "synthetic-proxy-g1-draining", block: "g1", ports: ports, internalStatus: 503},
			{container: "synthetic-proxy-g1-current", block: "g1", ports: ports, internalStatus: 200},
		},
	)
	if len(findings) != 0 {
		t.Fatalf("ready block with an unready draining sibling alerted: %+v", findings)
	}
}

func TestProxyPathSignalSyntheticProblems(t *testing.T) {
	source := &syntheticSource{
		hostFn: func(_ HostSettings, command string) (string, error) {
			switch {
			case strings.Contains(command, proxyAllocationMarker):
				return "synthetic-proxy-g1-current|80:12689,8080:12719,8081:12720,8082:12721|200", nil
			case strings.Contains(command, proxyRouteMarker):
				return "networkd_start=200\nlb_start=100\nv4_routes=0\nv6_routes=1\nv4_rules=0\nv6_rules=1", nil
			case strings.Contains(command, edgeAutoUpgradeMarker):
				return "periodic_enable=1\napt-daily.timer=enabled\napt-daily-upgrade.timer=enabled\napt-daily.service=static\napt-daily-upgrade.service=static\nunattended-upgrades.service=enabled", nil
			default:
				return "", errors.New("unexpected synthetic host command")
			}
		},
		localFn: func(name string, args ...string) (string, error) {
			if name != "curl" {
				return "", errors.New("unexpected local command")
			}
			if strings.Contains(strings.Join(args, " "), "https://invalid:invalid@") {
				return "000", errors.New("synthetic HTTPS proxy timeout")
			}
			return "407", nil
		},
		tcpFn: func(string, string, []byte, int) ([]byte, error) {
			return []byte{0x05, 0x00}, nil
		},
	}
	settings := syntheticSettings(source)
	settings.Hosts = append(settings.Hosts, HostSettings{
		Name: "proxy-1",
		Proxy: &ProxyHostSettings{
			PublicHostname:   "proxy.example",
			PublicInterface:  "eno1",
			RoutingTable:     100,
			LoadBalancerUnit: "warp-main-lb-eno1.service",
			AddressFamilies:  []string{"ipv4"},
		},
	})

	alerts, err := NewProxyPathSignal().Run(context.Background(), settings)
	if err != nil {
		t.Fatal(err)
	}
	for _, class := range []string{"proxy-public-handshake", "policy-route-drift", "edge-auto-upgrades"} {
		alert := requireAlertClass(t, alerts, class)
		if alert.SignalKey != "proxy-path" {
			t.Fatalf("class %s signal key = %q", class, alert.SignalKey)
		}
	}
}
