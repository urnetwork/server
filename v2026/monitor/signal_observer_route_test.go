// Synthetic native route answers exercise the observer contract through the
// public signal. Every lookup is a fake local command; no route is read.
package monitor

import (
	"context"
	"errors"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// Documentation addresses stand in for the inventory's overlay addresses.
func observerRouteTestSettings(t *testing.T, contract ObserverOverlaySettings, local func(name string, args ...string) (string, error)) SignalSettings {
	t.Helper()
	settings := syntheticSettings(&syntheticSource{localFn: local, hostFn: func(HostSettings, string) (string, error) {
		t.Fatal("the observer route contacted a host")
		return "", nil
	}})
	settings.Hosts = []HostSettings{
		{Name: "edge-1", OverlayAddress: "192.0.2.11", Roles: []string{"services"}},
		{Name: "edge-2", OverlayAddress: "192.0.2.12", Roles: []string{"pg-primary"}},
		{Name: "edge-3", OverlayAddress: "2001:db8::13", Roles: []string{"redis-cluster"}},
	}
	settings.AddressMode = AddressModeOverlay
	settings.ObserverOverlay = contract
	return settings
}

// A darwin route answer through interface and gateway.
func observerRouteTestDarwin(interfaceName, gateway string) string {
	text := "   route to: 198.51.100.1\ndestination: 198.51.100.0\n       mask: 255.255.240.0\n"
	if gateway != "" {
		text += "    gateway: " + gateway + "\n"
	}
	return text + "  interface: " + interfaceName + "\n      flags: <UP,GATEWAY,DONE,STATIC,PRCLONING>\n"
}

func observerRouteTestRun(t *testing.T, platform string, settings SignalSettings) Alerts {
	t.Helper()
	signalSettings := DefaultObserverRouteSettings()
	signalSettings.Platform = platform
	alerts, err := NewObserverRouteSignalWithSettings(signalSettings).Run(context.Background(), settings)
	if err != nil {
		t.Fatal("observer route run failed")
	}
	return alerts
}

func TestObserverRouteRegistered(t *testing.T) {
	found := 0
	for _, signal := range NewSignals() {
		if signal.Key() != "observer-route" {
			continue
		}
		found++
		if signal.Number() != "1.7" || signal.Cadence() != 5*time.Minute || signal.ID() != "monitor/observer-route" {
			t.Fatal("observer route lost its catalog identity or five-minute cadence")
		}
	}
	if found != 1 {
		t.Fatal("the §1.7 observer overlay route signal is not registered")
	}
}

// Without the operator contract the attribution is explicitly unknown, kept
// in the alert file as a configured standing state, and no lookup runs.
func TestObserverRouteMissingContractIsConfiguredUnknown(t *testing.T) {
	settings := observerRouteTestSettings(t, ObserverOverlaySettings{}, func(string, ...string) (string, error) {
		t.Fatal("a missing contract ran a route lookup")
		return "", nil
	})
	alerts := observerRouteTestRun(t, "darwin", settings)
	if len(alerts) != 1 {
		t.Fatalf("got %d alerts, want the missing contract", len(alerts))
	}
	alert := requireAlertClass(t, alerts, "observer-route-contract-missing")
	if !alert.unobservable || !alert.configuredOff || alert.Sustain != 1 || alert.Target != "monitor-host/overlay" {
		t.Fatalf("missing contract has the wrong policy: %+v", alert)
	}
	settings.AddressMode = AddressModeLAN
	if alerts := observerRouteTestRun(t, "darwin", settings); len(alerts) != 0 {
		t.Fatal("LAN mode judged the overlay contract")
	}
}

// Every overlay address through the contract interface or gateway is healthy;
// a route to the lan or default interface and a missing route are diverted,
// and only inventory host names and interface names leave the host.
func TestObserverRouteDarwinContract(t *testing.T) {
	for _, test := range []struct {
		name     string
		contract ObserverOverlaySettings
		answers  map[string]string
		absent   bool
		diverted string
	}{
		{name: "interface contract", contract: ObserverOverlaySettings{Interfaces: []string{"utun4"}},
			answers: map[string]string{"192.0.2.11": observerRouteTestDarwin("utun4", "198.51.100.1"), "192.0.2.12": observerRouteTestDarwin("utun4", "198.51.100.1"), "2001:db8::13": observerRouteTestDarwin("utun4", "")}},
		{name: "gateway contract", contract: ObserverOverlaySettings{Gateways: []string{"198.51.100.1"}},
			answers: map[string]string{"192.0.2.11": observerRouteTestDarwin("utun7", "198.51.100.1"), "192.0.2.12": observerRouteTestDarwin("utun7", "198.51.100.1"), "2001:db8::13": observerRouteTestDarwin("utun9", "198.51.100.1")}},
		{name: "default route", contract: ObserverOverlaySettings{Interfaces: []string{"utun4"}},
			answers:  map[string]string{"192.0.2.11": observerRouteTestDarwin("en0", "203.0.113.1"), "192.0.2.12": observerRouteTestDarwin("utun4", "198.51.100.1"), "2001:db8::13": observerRouteTestDarwin("utun4", "")},
			diverted: "diverted=[edge-1] absent=[]"},
		{name: "no route", contract: ObserverOverlaySettings{Gateways: []string{"198.51.100.1"}},
			answers:  map[string]string{"192.0.2.11": observerRouteTestDarwin("utun4", "198.51.100.1"), "192.0.2.12": "route: writing to routing socket: not in table\n", "2001:db8::13": observerRouteTestDarwin("utun4", "198.51.100.1")},
			diverted: "diverted=[] absent=[edge-2]"},
	} {
		settings := observerRouteTestSettings(t, test.contract, func(name string, args ...string) (string, error) {
			if name != "/sbin/route" || len(args) != 4 || args[0] != "-n" || args[1] != "get" {
				t.Fatalf("%s: unexpected local lookup", test.name)
			}
			family := "-inet"
			if strings.Contains(args[3], ":") {
				family = "-inet6"
			}
			if args[2] != family {
				t.Fatalf("%s: lookup used the wrong address family", test.name)
			}
			return test.answers[args[3]], nil
		})
		alerts := observerRouteTestRun(t, "darwin", settings)
		if test.diverted == "" {
			if len(alerts) != 0 {
				t.Fatalf("%s: conforming routes alerted: %s", test.name, alerts[0].Observed)
			}
			continue
		}
		if len(alerts) != 1 {
			t.Fatalf("%s: got %d alerts, want the diverted route", test.name, len(alerts))
		}
		alert := requireAlertClass(t, alerts, "observer-route-diverted")
		if !alert.unobservable || alert.Sustain != 2 || alert.Target != "monitor-host/overlay" || !strings.Contains(alert.Evidence, test.diverted) {
			t.Fatalf("%s: wrong diverted alert: %s / %s", test.name, alert.Observed, alert.Evidence)
		}
		requireAlertOmits(t, alert, "192.0.2.11", "192.0.2.12", "2001:db8::13", "198.51.100.1", "203.0.113.1")
	}
}

// The linux adapter reads exactly one ip -j route object for the address.
func TestObserverRouteLinuxContract(t *testing.T) {
	answer := func(address, device, gateway string) string {
		text := `[{"dst":"` + address + `","dev":"` + device + `"`
		if gateway != "" {
			text += `,"gateway":"` + gateway + `"`
		}
		return text + `,"prefsrc":"198.51.100.9","flags":[],"uid":1000,"cache":[]}]`
	}
	unreachable := &exec.ExitError{}
	for _, test := range []struct {
		name    string
		answers map[string]string
		errors  map[string]error
		want    string
		unknown string
	}{
		{name: "tunnel", answers: map[string]string{"192.0.2.11": answer("192.0.2.11", "wg0", ""), "192.0.2.12": answer("192.0.2.12", "wg0", ""), "2001:db8::13": answer("2001:db8::13", "wg0", "")}},
		{name: "lan", answers: map[string]string{"192.0.2.11": answer("192.0.2.11", "eth0", "203.0.113.1"), "192.0.2.12": answer("192.0.2.12", "wg0", ""), "2001:db8::13": answer("2001:db8::13", "wg0", "")}, want: "diverted=[edge-1]"},
		{name: "duplicate key", answers: map[string]string{"192.0.2.11": `[{"dst":"192.0.2.11","dev":"wg0","dev":"eth0"}]`, "192.0.2.12": answer("192.0.2.12", "wg0", ""), "2001:db8::13": answer("2001:db8::13", "wg0", "")}, unknown: "unknown_hosts=[edge-1]"},
		{name: "foreign destination", answers: map[string]string{"192.0.2.11": answer("192.0.2.99", "wg0", ""), "192.0.2.12": answer("192.0.2.12", "wg0", ""), "2001:db8::13": answer("2001:db8::13", "wg0", "")}, unknown: "unknown_hosts=[edge-1]"},
		{name: "command failed", answers: map[string]string{"192.0.2.12": answer("192.0.2.12", "wg0", ""), "2001:db8::13": answer("2001:db8::13", "wg0", "")}, errors: map[string]error{"192.0.2.11": unreachable}, unknown: "unknown_hosts=[edge-1]"},
	} {
		settings := observerRouteTestSettings(t, ObserverOverlaySettings{Interfaces: []string{"wg0"}}, func(name string, args ...string) (string, error) {
			if name != "ip" || args[0] != "-j" {
				t.Fatalf("%s: unexpected local lookup", test.name)
			}
			address := args[len(args)-1]
			if strings.Contains(address, ":") != (args[1] == "-6") {
				t.Fatalf("%s: lookup used the wrong address family", test.name)
			}
			return test.answers[address], test.errors[address]
		})
		alerts := observerRouteTestRun(t, "linux", settings)
		switch {
		case test.want == "" && test.unknown == "":
			if len(alerts) != 0 {
				t.Fatalf("%s: conforming routes alerted", test.name)
			}
		case test.want != "":
			if alert := requireAlertClass(t, alerts, "observer-route-diverted"); !strings.Contains(alert.Evidence, test.want) {
				t.Fatalf("%s: wrong diverted hosts: %s", test.name, alert.Evidence)
			}
		default:
			if alert := requireAlertClass(t, alerts, "observer-route-unobservable"); !strings.Contains(alert.Observed, test.unknown) {
				t.Fatalf("%s: wrong unknown hosts: %s", test.name, alert.Observed)
			}
		}
		for _, alert := range alerts {
			requireAlertOmits(t, alert, "192.0.2.11", "198.51.100.9", "203.0.113.1")
		}
	}
}

// An invalid contract, a host without an overlay address, an unsupported
// platform and an unparsed answer stay unknown; an excluded host is neither
// looked up nor certified.
func TestObserverRouteUnknownAndExcludedTargets(t *testing.T) {
	conforming := func(_ string, args ...string) (string, error) { return observerRouteTestDarwin("utun4", ""), nil }
	settings := observerRouteTestSettings(t, ObserverOverlaySettings{Gateways: []string{"private-source-canary"}}, conforming)
	if alert := requireAlertClass(t, observerRouteTestRun(t, "darwin", settings), "observer-route-unobservable"); !strings.Contains(alert.Observed, "reason=invalid-contract-gateway") {
		t.Fatal("an invalid contract gateway was accepted")
	}
	settings = observerRouteTestSettings(t, ObserverOverlaySettings{Interfaces: []string{"utun4"}}, conforming)
	settings.Hosts[1].OverlayAddress = ""
	if alert := requireAlertClass(t, observerRouteTestRun(t, "darwin", settings), "observer-route-unobservable"); !strings.Contains(alert.Observed, "unknown_hosts=[edge-2]") {
		t.Fatal("a host without an overlay address was certified")
	}
	settings = observerRouteTestSettings(t, ObserverOverlaySettings{Interfaces: []string{"utun4"}}, conforming)
	if alert := requireAlertClass(t, observerRouteTestRun(t, "plan9", settings), "observer-route-unobservable"); !strings.Contains(alert.Observed, "unknown=3") {
		t.Fatal("an unsupported platform certified routes")
	}
	settings = observerRouteTestSettings(t, ObserverOverlaySettings{Interfaces: []string{"utun4"}}, func(string, ...string) (string, error) {
		return "private-source-canary\n", nil
	})
	alert := requireAlertClass(t, observerRouteTestRun(t, "darwin", settings), "observer-route-unobservable")
	requireAlertOmits(t, alert, "private-source-canary")

	lookups := 0
	settings = observerRouteTestSettings(t, ObserverOverlaySettings{Interfaces: []string{"utun4"}}, func(_ string, args ...string) (string, error) {
		lookups++
		if args[3] == "192.0.2.12" {
			t.Fatal("an excluded host was looked up")
		}
		return observerRouteTestDarwin("utun4", ""), nil
	})
	settings.ExcludedHosts = []string{"edge-2"}
	alerts := observerRouteTestRun(t, "darwin", settings)
	if lookups != 2 || len(alerts) != 1 || alerts[0].Class != "monitor-host-scope-partial" {
		t.Fatal("an excluded host's route was judged instead of staying scope-unknown")
	}
}

// The monitor.yml contract decodes into settings.
func TestObserverRouteContractDecodesFromMonitorYaml(t *testing.T) {
	y, err := decodeMonitorYaml([]byte("ssh:\n  user: monitor\nobserver_overlay:\n  interfaces: [wg0]\n  gateways: [198.51.100.1]\n"))
	if err != nil || len(y.ObserverOverlay.Interfaces) != 1 || y.ObserverOverlay.Interfaces[0] != "wg0" ||
		len(y.ObserverOverlay.Gateways) != 1 || y.ObserverOverlay.Gateways[0] != "198.51.100.1" {
		t.Fatal("observer_overlay did not decode from monitor.yml")
	}
	settings := SignalSettings{ObserverOverlay: y.ObserverOverlay}
	if cfg := configFromSignalSettings(settings.withDefaults()); len(cfg.observerOverlay.Gateways) != 1 {
		t.Fatal("the observer contract did not reach the probe configuration")
	}
}

// Cancellation before or during a lookup publishes nothing.
func TestObserverRouteCancellation(t *testing.T) {
	for _, beforeContact := range []bool{false, true} {
		ctx, cancel := context.WithCancel(context.Background())
		settings := observerRouteTestSettings(t, ObserverOverlaySettings{Interfaces: []string{"utun4"}}, func(string, ...string) (string, error) {
			if beforeContact {
				t.Fatal("canceled work ran a lookup")
			}
			cancel()
			return "", errors.New("private-source-canary")
		})
		if beforeContact {
			cancel()
		}
		signalSettings := DefaultObserverRouteSettings()
		signalSettings.Platform = "darwin"
		alerts, err := NewObserverRouteSignalWithSettings(signalSettings).Run(ctx, settings)
		cancel()
		if len(alerts) != 0 || !errors.Is(err, context.Canceled) {
			t.Fatal("canceled observer route published findings")
		}
	}
}
