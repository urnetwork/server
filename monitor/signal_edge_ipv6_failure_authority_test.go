package monitor

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"
)

// A genuine public failure survives missing cause authority; only complete
// controls may qualify an attribution, and neither diagnostics nor history do.
func TestEdgeIpv6ImmediateFailureDoesNotInventResetOrDnatCause(t *testing.T) {
	const address = "2001:db8:70::1"
	cases := []struct {
		name                string
		code                string
		exit                int
		total               string
		diagnostic          string
		hostUnavailable     bool
		observerUnavailable bool
		malformed           bool
		wantClass           string
		wantUnknown         bool
	}{
		{name: "connect failure with unavailable host", code: "000", exit: 7, total: "0.050", hostUnavailable: true, wantClass: "edge-ipv6-reset", wantUnknown: true},
		{name: "reported reset is not DNAT evidence", code: "000", exit: 7, total: "0.050", diagnostic: "curl: connection reset by peer\n", wantClass: "edge-ipv6-reset"},
		{name: "TLS failure", code: "000", exit: 35, total: "0.050", wantClass: "edge-ipv6-http"},
		{name: "read failure", code: "000", exit: 56, total: "0.050", wantClass: "edge-ipv6-http"},
		{name: "slower connection failure", code: "000", exit: 7, total: "2.000", wantClass: "edge-ipv6-http"},
		{name: "healthy public control", code: "200", exit: 0, total: "0.080"},
		{name: "incomplete native observation", code: "000", exit: 7, total: "0.050", malformed: true, wantUnknown: true},
		{name: "observer unavailable", code: "000", exit: 7, total: "0.050", observerUnavailable: true, wantUnknown: true},
	}
	for _, test := range cases {
		source := &syntheticSource{
			localFn: func(name string, args ...string) (string, error) {
				if ipv6ObserverRouteTestCommand(name, args) {
					if test.observerUnavailable {
						return "", errors.New("synthetic route observation unavailable")
					}
					return ipv6ObserverRouteTestResult("route to: 2001:db8:ffff::1\ninterface: synthetic0\n")
				}
				if name != "curl" {
					return "", errors.New("unexpected synthetic command")
				}
				if test.malformed {
					return "monitor_http_code=000\nmonitor_exitcode=7\n", edgeCommandExitError(test.exit)
				}
				peer := ""
				if test.exit == 0 {
					peer = address
				}
				return test.diagnostic + edgeHTTPFixture(test.code, strconv.Itoa(test.exit), peer, test.total), edgeCommandExitError(test.exit)
			},
			hostFn: func(_ HostSettings, command string) (string, error) {
				if test.hostUnavailable {
					return "", errors.New("synthetic host observation unavailable")
				}
				if strings.Contains(command, edgeIPv6IdentityMarker) {
					return "operstate=up\nconfigured_present=1\nunit_active=active\n", nil
				}
				if strings.Contains(command, edgeIPv6EgressMarker) {
					return "self_http_code=200\nself_exitcode=0\nself_probe_status=0\nroute_device=public0\nroute_source=" + address + "\nroute_status=0\nsource_egress=" + address + "\nsource_egress_status=0\n", nil
				}
				return "", errors.New("unexpected synthetic host observation")
			},
		}
		settings := syntheticSettings(source)
		settings.Hosts = []HostSettings{{Name: "synthetic-edge", EdgeIPv6: []EdgeIPv6InterfaceSettings{{Interface: "public0", Address: address, ProbeHostname: "api-v6.example"}}}}
		alerts, err := NewEdgeIPv6Signal().Run(context.Background(), settings)
		if err != nil {
			t.Fatalf("%s: %v", test.name, err)
		}
		failures, unknowns := 0, 0
		for _, alert := range alerts {
			if alert.Class == "cannot-observe" {
				unknowns++
			}
			if alert.Severity != SeverityPage {
				continue
			}
			failures++
			if alert.Class != test.wantClass {
				t.Fatalf("%s: failure class = %q, want %q", test.name, alert.Class, test.wantClass)
			}
			if strings.Contains(alert.Mechanism, "dead-first DNAT signature") || strings.Contains(alert.Action, "Remove only") || strings.Contains(alert.Action, "deploy the Warp duplicate") {
				t.Fatalf("%s: native failure acquired an unsupported forwarding cause or repair", test.name)
			}
			if alert.Class == "edge-ipv6-reset" && !strings.Contains(alert.Mechanism, "cause remains unknown") {
				t.Fatalf("%s: legacy immediate-failure identity asserted a cause", test.name)
			}
		}
		if (test.wantClass == "" && failures != 0) || (test.wantClass != "" && failures != 1) {
			t.Fatalf("%s: PAGE findings = %d for expected class %q", test.name, failures, test.wantClass)
		}
		if test.wantUnknown && unknowns == 0 {
			t.Fatalf("%s: unavailable authority was hidden", test.name)
		}
		if test.name == "healthy public control" && len(alerts) != 0 {
			t.Fatalf("%s: complete healthy public observation did not remain healthy", test.name)
		}
	}
}
