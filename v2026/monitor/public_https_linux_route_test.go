package monitor

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"runtime"
	"strings"
	"testing"
)

// The native adapter must be selected by the real helper, so this regression
// also compiles against the prior Darwin-only implementation for a causal red.
func TestIpv6ObserverRouteLinuxNativeAvailable(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Linux native adapter")
	}
	calls := 0
	source := &syntheticSource{localFn: func(name string, args ...string) (string, error) {
		if name != "ip" || strings.Join(args, " ") != "-j -6 route get "+ipv6ObserverRouteProbeAddress {
			return "", exec.ErrNotFound
		}
		calls++
		// Echo only the requested destination; all interface data is synthetic.
		return fmt.Sprintf(`[{"dst":%q,"dev":"synthetic0","flags":[],"cache":[]}]`, args[len(args)-1]), nil
	}}
	observed := observeIPv6ObserverRoute(context.Background(), &sourceRunner{source: source})
	if observed.state != ipv6ObserverRouteAvailable || observed.interfaceName != "synthetic0" || calls != 1 {
		t.Fatalf("Linux native route state=%s interface_matches=%t native_calls=%d; want available, synthetic interface, one native call", observed.state, observed.interfaceName == "synthetic0", calls)
	}
}

// Only a native route-get exit and its exact fixed no-route diagnostic establish
// route absence. Missing tools and unqualified diagnostics stay unknown.
func TestIpv6ObserverRouteLinuxNativeAbsent(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Linux native adapter")
	}
	for _, diagnostic := range []string{
		"RTNETLINK answers: Network is unreachable\n",
		"RTNETLINK answers: No route to host\n",
	} {
		source := &syntheticSource{localFn: func(name string, args ...string) (string, error) {
			if !ipv6ObserverRouteTestCommand(name, args) {
				return "", exec.ErrNotFound
			}
			return diagnostic, edgeCommandExitError(2)
		}}
		observed := observeIPv6ObserverRoute(context.Background(), &sourceRunner{source: source})
		if observed.state != ipv6ObserverRouteAbsent || observed.interfaceName != "" {
			t.Fatalf("native absence state=%s retained_interface=%t", observed.state, observed.interfaceName != "")
		}
	}
}

// Incomplete, ambiguous and failed native observations cannot supply either a
// healthy observer or the affirmative absence used by a complete failure cohort.
func TestIpv6ObserverRouteLinuxNativeUnknown(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Linux native adapter")
	}
	valid := fmt.Sprintf(`[{"dst":%q,"dev":"synthetic0"}]`, ipv6ObserverRouteProbeAddress)
	for _, test := range []struct {
		name   string
		output string
		err    error
	}{
		{name: "missing tool", err: exec.ErrNotFound},
		{name: "timeout", err: context.DeadlineExceeded},
		{name: "opaque absence error", output: "RTNETLINK answers: Network is unreachable", err: errors.New("synthetic transport failure")},
		{name: "zero exit absence text", output: "RTNETLINK answers: Network is unreachable"},
		{name: "wrong exit absence text", output: "RTNETLINK answers: Network is unreachable", err: edgeCommandExitError(1)},
		{name: "unsupported command", output: "synthetic unsupported option", err: edgeCommandExitError(2)},
		{name: "failed valid result", output: valid, err: edgeCommandExitError(2)},
		{name: "absent result"},
		{name: "empty records", output: "[]"},
		{name: "null records", output: "null"},
		{name: "partial JSON", output: `[{"dev":"synthetic0"`},
		{name: "missing destination", output: `[{"dev":"synthetic0"}]`},
		{name: "wrong destination", output: `[{"dst":"2001:db8::1","dev":"synthetic0"}]`},
		{name: "wrong family", output: `[{"dst":"192.0.2.1","dev":"synthetic0"}]`},
		{name: "missing interface", output: fmt.Sprintf(`[{"dst":%q}]`, ipv6ObserverRouteProbeAddress)},
		{name: "blank interface", output: fmt.Sprintf(`[{"dst":%q,"dev":" "}]`, ipv6ObserverRouteProbeAddress)},
		{name: "multiple interfaces", output: fmt.Sprintf(`[{"dst":%q,"dev":"synthetic0 synthetic1"}]`, ipv6ObserverRouteProbeAddress)},
		{name: "duplicate interface", output: fmt.Sprintf(`[{"dst":%q,"dev":"synthetic0","dev":"synthetic1"}]`, ipv6ObserverRouteProbeAddress)},
		{name: "multiple records", output: strings.TrimSuffix(valid, "]") + "," + strings.TrimPrefix(valid, "[")},
		{name: "trailing JSON", output: valid + " []"},
		{name: "trailing diagnostic", output: valid + " synthetic diagnostic"},
		{name: "blackhole route", output: fmt.Sprintf(`[{"dst":%q,"dev":"synthetic0","type":"blackhole"}]`, ipv6ObserverRouteProbeAddress)},
		{name: "local route", output: fmt.Sprintf(`[{"dst":%q,"dev":"synthetic0","type":"local"}]`, ipv6ObserverRouteProbeAddress)},
		{name: "malformed route type", output: fmt.Sprintf(`[{"dst":%q,"dev":"synthetic0","type":1}]`, ipv6ObserverRouteProbeAddress)},
		{name: "null route type", output: fmt.Sprintf(`[{"dst":%q,"dev":"synthetic0","type":null}]`, ipv6ObserverRouteProbeAddress)},
		{name: "empty route type", output: fmt.Sprintf(`[{"dst":%q,"dev":"synthetic0","type":""}]`, ipv6ObserverRouteProbeAddress)},
		{name: "oversized result", output: valid + strings.Repeat(" ", 16*1024)},
	} {
		source := &syntheticSource{localFn: func(name string, args ...string) (string, error) {
			if !ipv6ObserverRouteTestCommand(name, args) {
				return "", exec.ErrNotFound
			}
			return test.output, test.err
		}}
		observed := observeIPv6ObserverRoute(context.Background(), &sourceRunner{source: source})
		if observed.state != ipv6ObserverRouteUnobservable || observed.interfaceName != "" {
			t.Errorf("%s: state=%s retained_interface=%t, want unknown without route data", test.name, observed.state, observed.interfaceName != "")
		}
	}
}

// Restored route visibility must retain the independent public timeout and
// host-source gaps without claiming an upstream or host routing fault.
func TestEdgeIpv6LinuxRouteRetainsTimeoutWithUnavailableHostControls(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Linux native adapter")
	}
	source := &syntheticSource{
		localFn: func(name string, args ...string) (string, error) {
			if ipv6ObserverRouteTestCommand(name, args) {
				return fmt.Sprintf(`[{"dst":%q,"dev":"synthetic0","type":"unicast"}]`, args[len(args)-1]), nil
			}
			if name != "curl" {
				return "", exec.ErrNotFound
			}
			return edgeHTTPFixture("000", "28", "", "3.002"), edgeCommandExitError(28)
		},
		hostFn: func(HostSettings, string) (string, error) {
			return "", errors.New("synthetic host source unavailable")
		},
	}
	settings := syntheticSettings(source)
	settings.Hosts = []HostSettings{{Name: "synthetic-edge.example", EdgeIPv6: []EdgeIPv6InterfaceSettings{{Interface: "synthetic0", Address: "2001:db8::41", ProbeHostname: "api-v6.example"}}}}
	alerts, err := NewEdgeIPv6Signal().Run(context.Background(), settings)
	if err != nil || len(alerts) != 3 {
		t.Fatalf("route-qualified partial-host observations: alerts=%d err=%v, want timeout plus both source gaps", len(alerts), err)
	}
	timeout := requireAlertClass(t, alerts, "edge-ipv6-timeout")
	if timeout.Severity != SeverityPage {
		t.Fatalf("observed public timeout severity=%s, want page", timeout.Severity)
	}
	unknowns := 0
	for _, alert := range alerts {
		if alert.Class == "cannot-observe" && alert.Severity == SeverityWarn {
			unknowns++
		}
		requireAlertOmits(t, alert, "synthetic host source unavailable")
	}
	if unknowns != 2 {
		t.Fatalf("host-source unknowns=%d, want both identity and source-egress gaps", unknowns)
	}

	// Private alerts retain exact target identity. Address privacy belongs to
	// the existing public publisher, which the loopback webhook observes here.
	privateIdentity := timeout.Identity()
	if privateIdentity != "lb/edge-ipv6|edge-ipv6-timeout|synthetic-edge.example|synthetic0/2001:db8::41" ||
		!strings.Contains(timeout.Markdown(), "address=2001:db8::41") {
		t.Fatal("private timeout lost its exact target identity or observed address")
	}
	webhook, webhookUrl := newFakeSlackWebhook(t)
	harness := newSlackTestHarness()
	notifier := NewSlackAlertNotifier(harness.settings(webhookUrl))
	t.Cleanup(func() { closeSlackNotifier(t, notifier) })
	if !notifier.post(alerts.Markdown()).delivered {
		t.Fatal("loopback public publisher did not deliver the synthetic alert document")
	}
	posted := webhook.posted()
	if len(posted) != 1 {
		t.Fatalf("loopback public documents=%d, want one", len(posted))
	}
	if strings.Contains(posted[0], "2001:db8::41") || strings.Contains(posted[0], "synthetic host source unavailable") {
		t.Fatal("public publisher retained a synthetic address or private host diagnostic")
	}
	for _, want := range []string{
		"## [PAGE]",
		"lb/edge-ipv6|edge-ipv6-timeout|synthetic-edge.example|synthetic0/[scrubbed]",
		"address=[scrubbed]", "http_code=000", "curl_exit=28",
	} {
		if !strings.Contains(posted[0], want) {
			t.Fatalf("public publisher lost safe timeout evidence %q", want)
		}
	}
	if timeout.Identity() != privateIdentity {
		t.Fatal("public publication changed the private timeout identity")
	}
}

// A TLS timeout is not the no-route error required by TLS cohort attribution,
// even when the independent Linux route lookup reports absence.
func TestTlsExpiryLinuxRouteAbsencePreservesTimeoutUnknown(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Linux native adapter")
	}
	source := &syntheticSource{
		localFn: func(name string, args ...string) (string, error) {
			if !ipv6ObserverRouteTestCommand(name, args) {
				return "", exec.ErrNotFound
			}
			return "RTNETLINK answers: Network is unreachable\n", edgeCommandExitError(2)
		},
		tlsFn: func(string, string, string) (TLSCertificateObservation, error) {
			return TLSCertificateObservation{}, errors.New("dial tcp6: i/o timeout")
		},
	}
	settings := syntheticSettings(source)
	settings.ManagerHostname = "manager.example"
	settings.Hosts = []HostSettings{{Name: "synthetic-edge.example", PublicLB: []PublicLBInterfaceSettings{{Interface: "synthetic0", IPv6Address: "2001:db8::41"}}}}
	alerts, err := NewTLSExpirySignal().Run(context.Background(), settings)
	if err != nil || len(alerts) != 1 {
		t.Fatalf("TLS timeout with route absence: alerts=%d err=%v, want one endpoint unknown", len(alerts), err)
	}
	if alerts[0].Class != "tls-certificate-unobservable" || alerts[0].Severity != SeverityWarn || alerts[0].Frame != "ipv6" {
		t.Fatalf("TLS timeout lost its endpoint unknown: class=%s severity=%s frame=%s", alerts[0].Class, alerts[0].Severity, alerts[0].Frame)
	}
}

// Existing signal fixtures model route state independently of the test host.
// New native-adapter regressions above bypass this fixture translation.
func ipv6ObserverRouteTestCommand(name string, args []string) bool {
	if runtime.GOOS == "linux" {
		return name == "ip" && strings.Join(args, " ") == "-j -6 route get "+ipv6ObserverRouteProbeAddress
	}
	return name == "/sbin/route" && strings.Join(args, " ") == "-n get -inet6 "+ipv6ObserverRouteProbeAddress
}

// Convert only the existing synthetic Darwin states into native Linux output;
// malformed state text remains malformed on both platforms.
func ipv6ObserverRouteTestResult(output string) (string, error) {
	if runtime.GOOS != "linux" {
		return output, nil
	}
	if strings.Contains(output, "not in table") {
		return "RTNETLINK answers: Network is unreachable\n", edgeCommandExitError(2)
	}
	for _, line := range strings.Split(output, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 && fields[0] == "interface:" {
			return fmt.Sprintf(`[{"dst":%q,"dev":%q}]`, ipv6ObserverRouteProbeAddress, fields[1]), nil
		}
	}
	return output, nil
}
