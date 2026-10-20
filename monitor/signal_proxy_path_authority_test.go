// Exercise the generated proxy host collectors and their complete alert consumer
// with synthetic executables; no host, APT, systemd or network service is used.
package monitor

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const proxyAuthorityPrivate = "synthetic-private-native-diagnostic"

// One producer's complete stdout and native termination, independent of peers.
type proxyAuthorityResult struct {
	output string
	status int
}

// Healthy defaults use successful property reads, including exact masked states.
func proxyAuthorityHealthy() map[string]proxyAuthorityResult {
	results := map[string]proxyAuthorityResult{
		"NETWORKD": {output: "200\n"},
		"LB":       {output: "100\n"},
		"V4ROUTE":  {output: "default via 192.0.2.1 dev proxy0\n"},
		"V6ROUTE":  {output: "default via 2001:db8::1 dev proxy0\n"},
		"V4RULE":   {output: "100: from 192.0.2.0/24 lookup 100\n"},
		"V6RULE":   {output: "100: from 2001:db8::/32 lookup 100\n"},
		"APT":      {output: "monitor_periodic='0'\n"},
		"APT_DUMP": {output: "APT::Periodic::Enable \"0\";\n"},
	}
	for _, key := range []string{"DAILY_TIMER", "UPGRADE_TIMER", "DAILY_SERVICE", "UPGRADE_SERVICE", "UNATTENDED"} {
		results[key] = proxyAuthorityResult{output: "masked\n"}
	}
	return results
}

// Execute exactly the command produced by the probe, with every service-facing
// executable replaced. Frame mutations occur after genuine shell completion.
func runProxyAuthority(t *testing.T, results map[string]proxyAuthorityResult, families []string, handshakeFailure bool, mutate func(string, string) string) []Alert {
	t.Helper()
	directory := t.TempDir()
	scripts := map[string]string{
		"ip": `case "$1 $2" in
  '-4 route') key=V4ROUTE ;;
  '-6 route') key=V6ROUTE ;;
  '-4 rule') key=V4RULE ;;
  '-6 rule') key=V6RULE ;;
  *) exit 91 ;;
esac`,
		"systemctl": `case "$*" in
  'show systemd-networkd.service -p ActiveEnterTimestampMonotonic --value') key=NETWORKD ;;
  'show warp-synthetic-lb-proxy0.service -p ActiveEnterTimestampMonotonic --value') key=LB ;;
  'show apt-daily.timer -p UnitFileState --value'|'is-enabled apt-daily.timer') key=DAILY_TIMER ;;
  'show apt-daily-upgrade.timer -p UnitFileState --value'|'is-enabled apt-daily-upgrade.timer') key=UPGRADE_TIMER ;;
  'show apt-daily.service -p UnitFileState --value'|'is-enabled apt-daily.service') key=DAILY_SERVICE ;;
  'show apt-daily-upgrade.service -p UnitFileState --value'|'is-enabled apt-daily-upgrade.service') key=UPGRADE_SERVICE ;;
  'show unattended-upgrades.service -p UnitFileState --value'|'is-enabled unattended-upgrades.service') key=UNATTENDED ;;
  *) exit 92 ;;
esac`,
		"apt-config": `case "$*" in
  'shell monitor_periodic APT::Periodic::Enable') key=APT ;;
  dump) key=APT_DUMP ;;
  *) exit 93 ;;
esac`,
	}
	if _, ok := results["REDUCER"]; ok {
		scripts["awk"] = "key=REDUCER"
	}
	for name, body := range scripts {
		body = "#!/bin/sh\n" + body + `
printf '%s\n' "$key" >> "$PROXY_AUTHORITY_TRACE"
printf '%s\n' 'synthetic-private-native-diagnostic' >&2
cat "$PROXY_AUTHORITY_DIRECTORY/$key.output"
status=$(cat "$PROXY_AUTHORITY_DIRECTORY/$key.status")
exit "$status"
`
		if err := os.WriteFile(filepath.Join(directory, name), []byte(body), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	for key, result := range results {
		if err := os.WriteFile(filepath.Join(directory, key+".output"), []byte(result.output), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(directory, key+".status"), []byte(fmt.Sprint(result.status)), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	trace := filepath.Join(directory, "calls")
	source := &syntheticSource{
		hostFn: func(_ HostSettings, command string) (string, error) {
			if strings.Contains(command, proxyAllocationMarker) {
				return "synthetic-proxy-g1-current|80:12080,8080:12081,8081:12082,8082:12083|200\n", nil
			}
			marker := ""
			for _, candidate := range []string{proxyRouteMarker, edgeAutoUpgradeMarker} {
				if strings.Contains(command, candidate) {
					marker = candidate
				}
			}
			if marker == "" {
				t.Fatal("unexpected generated proxy collector")
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if result, ok := results[marker]; ok {
				command += fmt.Sprintf("\nexit %d\n", result.status)
			}
			process := exec.CommandContext(ctx, "/bin/sh", "-c", command)
			process.Env = []string{
				"PATH=" + directory + ":/usr/bin:/bin", "LC_ALL=C",
				"PROXY_AUTHORITY_DIRECTORY=" + directory, "PROXY_AUTHORITY_TRACE=" + trace,
			}
			out, err := process.Output()
			if strings.Contains(string(out), proxyAuthorityPrivate) {
				t.Fatal("collector published native diagnostics in its frame")
			}
			if mutate != nil {
				out = []byte(mutate(marker, string(out)))
			}
			return string(out), err
		},
		localFn: func(name string, _ ...string) (string, error) {
			if name != "curl" {
				t.Fatal("unexpected public-path executable")
			}
			return "407", nil
		},
		tcpFn: func(string, string, []byte, int) ([]byte, error) {
			if handshakeFailure {
				return []byte{0x05, 0x00}, nil
			}
			return []byte{0x05, 0x02}, nil
		},
	}
	settings := syntheticSettings(source)
	settings.ProxyPathExpectedHosts = 1
	settings.Hosts = append(settings.Hosts, HostSettings{
		Name: "proxy-synthetic",
		Proxy: &ProxyHostSettings{
			PublicHostname: "proxy.example", PublicInterface: "proxy0", RoutingTable: 100,
			LoadBalancerUnit: "warp-synthetic-lb-proxy0.service", AddressFamilies: families,
		},
	})
	alerts, err := NewProxyPathSignal().Run(context.Background(), settings)
	if err != nil {
		t.Fatal(err)
	}
	calls, err := os.ReadFile(trace)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := results["REDUCER"]; ok {
		count := strings.Count(string(calls), "REDUCER\n")
		if count != 4 && count != 1 {
			t.Fatal("unexpected native reduction count")
		}
		calls = []byte(strings.ReplaceAll(string(calls), "REDUCER\n", ""))
	}
	want := "NETWORKD\nLB\nV4ROUTE\nV4RULE\nV6ROUTE\nV6RULE\nAPT\nDAILY_TIMER\nUPGRADE_TIMER\nDAILY_SERVICE\nUPGRADE_SERVICE\nUNATTENDED\n"
	// The prior implementation used dump and queried routes before rules.
	legacy := "NETWORKD\nLB\nV4ROUTE\nV6ROUTE\nV4RULE\nV6RULE\nAPT_DUMP\nDAILY_TIMER\nUPGRADE_TIMER\nDAILY_SERVICE\nUPGRADE_SERVICE\nUNATTENDED\n"
	if string(calls) != want && string(calls) != legacy {
		t.Fatal("collector skipped, retried or reordered an independent native producer")
	}
	for _, alert := range alerts {
		encoded, err := json.Marshal(alert)
		if err != nil {
			t.Fatal(err)
		}
		for _, forbidden := range []string{proxyAuthorityPrivate, "private-policy-value", "987654321012345678", "192.0.2.1", "2001:db8::1", directory} {
			if strings.Contains(string(encoded), forbidden) || strings.Contains(alert.Markdown(), forbidden) {
				t.Fatal("raw producer data leaked through the complete alert consumer")
			}
		}
	}
	return alerts
}

// Compare the entire classification set so an extra product or visibility
// finding cannot hide behind a single expected alert.
func requireProxyAuthorityClasses(t *testing.T, alerts []Alert, expected map[string]int) {
	t.Helper()
	actual := map[string]int{}
	for _, alert := range alerts {
		actual[alert.Class]++
	}
	if len(actual) != len(expected) {
		t.Fatalf("classification set = %v, want %v", actual, expected)
	}
	for class, count := range expected {
		if actual[class] != count {
			t.Fatalf("classification set = %v, want %v", actual, expected)
		}
	}
}

// A filter's success cannot certify the failed producer, even when that
// producer printed a plausible nonempty prefix or no matching rule.
func TestProxyPathAuthorityNativeRouteFailure(t *testing.T) {
	for _, key := range []string{"V4ROUTE", "V4RULE", "NETWORKD", "LB"} {
		for _, partial := range []bool{false, true} {
			results := proxyAuthorityHealthy()
			result := results[key]
			result.status = 17
			if !partial {
				result.output = ""
			}
			results[key] = result
			alerts := runProxyAuthority(t, results, []string{"ipv4", "ipv6"}, false, nil)
			requireProxyAuthorityClasses(t, alerts, map[string]int{"cannot-observe": 1})
		}
	}
	results := proxyAuthorityHealthy()
	results["V4RULE"] = proxyAuthorityResult{output: "0: from all lookup local\n", status: 17}
	requireProxyAuthorityClasses(t, runProxyAuthority(t, results, []string{"ipv4"}, false, nil), map[string]int{"cannot-observe": 1})
}

// Default-enabled is authoritative only after successful APT completion.
func TestProxyPathAuthorityNativeAptFailure(t *testing.T) {
	for _, output := range []string{"", "monitor_periodic='0'\n", "monitor_periodic='1'\n"} {
		results := proxyAuthorityHealthy()
		results["APT"] = proxyAuthorityResult{output: output, status: 17}
		results["APT_DUMP"] = proxyAuthorityResult{output: strings.ReplaceAll(output, "monitor_periodic=", "APT::Periodic::Enable "), status: 17}
		alerts := runProxyAuthority(t, results, []string{"ipv4"}, false, nil)
		requireProxyAuthorityClasses(t, alerts, map[string]int{"cannot-observe": 1})
	}
}

// Successful documented properties retain their policy meaning. Every failed,
// empty or malformed query is unknown, including plausible nonzero prefixes.
func TestProxyPathAuthorityNativeUnitStatusAndShape(t *testing.T) {
	for _, sample := range []struct {
		output string
		status int
		class  string
	}{
		{output: "masked\n"},
		{output: "masked-runtime\n"},
		{output: "disabled\n", class: "edge-auto-upgrades"},
		{output: "linked\n", class: "edge-auto-upgrades"},
		{output: "linked-runtime\n", class: "edge-auto-upgrades"},
		{output: "enabled\n", class: "edge-auto-upgrades"},
		{output: "enabled-runtime\n", class: "edge-auto-upgrades"},
		{output: "static\n", class: "edge-auto-upgrades"},
		{class: "cannot-observe"},
		{status: 1, class: "cannot-observe"},
		{output: "enabled\n", status: 17, class: "cannot-observe"},
		{output: "masked\n", status: 1, class: "cannot-observe"},
		{output: "masked\n", status: 17, class: "cannot-observe"},
		{output: "disabled\n", status: 1, class: "cannot-observe"},
		{output: "linked\n", status: 1, class: "cannot-observe"},
		{output: "not-found\n", status: 4, class: "cannot-observe"},
		{output: "not-found\n", class: "cannot-observe"},
		{output: "invalid\n", class: "cannot-observe"},
		{output: "alias\n", class: "cannot-observe"},
		{output: "generated\n", class: "cannot-observe"},
		{output: "transient\n", class: "cannot-observe"},
		{output: "indirect\n", class: "cannot-observe"},
		{output: "masked-private-policy-value\n", class: "cannot-observe"},
		{output: "masked\nenabled\n", class: "cannot-observe"},
	} {
		results := proxyAuthorityHealthy()
		results["DAILY_TIMER"] = proxyAuthorityResult{output: sample.output, status: sample.status}
		expected := map[string]int{}
		if sample.class != "" {
			expected[sample.class] = 1
		}
		requireProxyAuthorityClasses(t, runProxyAuthority(t, results, []string{"ipv4"}, false, nil), expected)
	}
}

// Measured emptiness and ordinary no-match rules retain the original ordering
// predicate; a failed observation is still unknown when the LB is newer.
func TestProxyPathAuthorityNativeHealthyAndRealDrift(t *testing.T) {
	for _, sample := range []struct {
		networkd string
		route    proxyAuthorityResult
		rule     string
		class    string
	}{
		{networkd: "200\n", route: proxyAuthorityResult{output: "default via 192.0.2.1 dev proxy0\n"}, rule: "100: from all lookup 100\n"},
		{networkd: "200\n", class: "policy-route-drift"},
		{networkd: "200\n", rule: "0: from all lookup local\n", class: "policy-route-drift"},
		{networkd: "50\n"},
		{networkd: "100\n"},
		{networkd: "50\n", route: proxyAuthorityResult{status: 17}, class: "cannot-observe"},
	} {
		results := proxyAuthorityHealthy()
		results["NETWORKD"] = proxyAuthorityResult{output: sample.networkd}
		results["V4ROUTE"] = sample.route
		results["V4RULE"] = proxyAuthorityResult{output: sample.rule}
		expected := map[string]int{}
		if sample.class != "" {
			expected[sample.class] = 1
		}
		requireProxyAuthorityClasses(t, runProxyAuthority(t, results, []string{"ipv4"}, false, nil), expected)
	}
	results := proxyAuthorityHealthy()
	results["APT"] = proxyAuthorityResult{}
	results["APT_DUMP"] = proxyAuthorityResult{}
	requireProxyAuthorityClasses(t, runProxyAuthority(t, results, []string{"ipv4"}, false, nil), map[string]int{"edge-auto-upgrades": 1})
}

// One unknown source cannot erase the known deficit in another source or the
// separately established public handshake failure.
func TestProxyPathAuthorityNativePreservesIndependentPositives(t *testing.T) {
	results := proxyAuthorityHealthy()
	results["V4ROUTE"] = proxyAuthorityResult{}
	results["V6RULE"] = proxyAuthorityResult{status: 17}
	results["APT"] = proxyAuthorityResult{status: 17}
	results["APT_DUMP"] = proxyAuthorityResult{status: 17}
	results["DAILY_TIMER"] = proxyAuthorityResult{output: "enabled\n"}
	alerts := runProxyAuthority(t, results, []string{"ipv4", "ipv6"}, true, nil)
	requireProxyAuthorityClasses(t, alerts, map[string]int{
		"cannot-observe": 2, "policy-route-drift": 1, "edge-auto-upgrades": 1, "proxy-public-handshake": 2,
	})
	for _, alert := range alerts {
		if alert.Class == "policy-route-drift" && (!strings.Contains(alert.Observed, "v6_rules=unknown") || strings.Contains(alert.Evidence, "IPv6 source/fwmark rules")) {
			t.Fatal("unknown rule was converted to measured absence")
		}
		if alert.Class == "edge-auto-upgrades" && (!strings.Contains(alert.Observed, "periodic_enable=unknown") || strings.Contains(alert.Evidence, "periodic_enable=")) {
			t.Fatal("unknown APT key was converted to policy drift")
		}
	}
}

// Clocks, fixed-domain APT values and reducer frames need their own shape
// authority; successful shell termination alone does not establish health.
func TestProxyPathAuthorityNativeMalformedValues(t *testing.T) {
	for _, sample := range []struct {
		key, output string
	}{
		{key: "NETWORKD", output: "200\n100\n"},
		{key: "LB", output: "0\n"},
		{key: "NETWORKD", output: "+200\n"},
		{key: "APT", output: "monitor_periodic='private-policy-value'\n"},
		{key: "APT", output: "monitor_periodic='0'\nmonitor_periodic='1'\n"},
		{key: "APT", output: "monitor_periodic=''\n"},
	} {
		results := proxyAuthorityHealthy()
		results[sample.key] = proxyAuthorityResult{output: sample.output}
		if sample.key == "APT" {
			results["APT_DUMP"] = proxyAuthorityResult{output: "APT::Periodic::Enable \"private-policy-value\";\n"}
		}
		requireProxyAuthorityClasses(t, runProxyAuthority(t, results, []string{"ipv4"}, false, nil), map[string]int{"cannot-observe": 1})
	}
}

// Reject incomplete frames, duplicate values in both orders, invalid numbers,
// foreign rows and trailing output without disclosing any untrusted text.
func TestProxyPathAuthorityNativeFrameValidation(t *testing.T) {
	for _, marker := range []string{proxyRouteMarker, edgeAutoUpgradeMarker} {
		for _, mutation := range []string{"missing-trailer", "missing-field", "duplicate-first", "duplicate-last", "malformed", "extra-row", "trailing-row"} {
			mutate := func(actualMarker, out string) string {
				if actualMarker != marker {
					return out
				}
				key, row := "v4_routes", "v4_routes=0|1\n"
				if marker == edgeAutoUpgradeMarker {
					key, row = "periodic_enable", "periodic_enable=0|0\n"
				}
				extra := key + "=0|private-policy-value\n"
				switch mutation {
				case "missing-trailer":
					if end := strings.LastIndex(out, "complete="); end >= 0 {
						return out[:end]
					}
					return out
				case "missing-field":
					return strings.Replace(out, row, "", 1)
				case "duplicate-first":
					return strings.Replace(out, row, extra+row, 1)
				case "duplicate-last":
					return strings.Replace(out, row, row+extra, 1)
				case "malformed":
					return strings.Replace(out, row, extra, 1)
				case "extra-row":
					return strings.Replace(out, "complete=", "private-policy-value\ncomplete=", 1)
				default:
					return out + "private-policy-value\n"
				}
			}
			requireProxyAuthorityClasses(t, runProxyAuthority(t, proxyAuthorityHealthy(), []string{"ipv4"}, false, mutate), map[string]int{"cannot-observe": 1})
		}
	}
}

// A disabled address family is outside the configured routing obligation.
func TestProxyPathAuthorityNativeDisabledFamily(t *testing.T) {
	results := proxyAuthorityHealthy()
	results["V6ROUTE"] = proxyAuthorityResult{status: 17}
	results["V6RULE"] = proxyAuthorityResult{status: 17}
	requireProxyAuthorityClasses(t, runProxyAuthority(t, results, []string{"ipv4"}, false, nil), map[string]int{})
}

// Plausible complete output cannot override failed outer shell completion.
func TestProxyPathAuthorityNativeOuterFailure(t *testing.T) {
	for _, marker := range []string{proxyRouteMarker, edgeAutoUpgradeMarker} {
		results := proxyAuthorityHealthy()
		results[marker] = proxyAuthorityResult{status: 17}
		requireProxyAuthorityClasses(t, runProxyAuthority(t, results, []string{"ipv4"}, false, nil), map[string]int{"cannot-observe": 1})
	}
}

// Counts must be unsigned decimal integers and bounded native statuses. A bad
// field stays unknown even when the restart ordering would otherwise be quiet.
func TestProxyPathAuthorityNativeCountDomains(t *testing.T) {
	for _, value := range []string{"0|", "0|-1", "0|1.0", "0|NaN", "0|18446744073709551616", "0|+1", "256|1", "-1|1", "0|1|1"} {
		results := proxyAuthorityHealthy()
		results["NETWORKD"] = proxyAuthorityResult{output: "50\n"}
		mutate := func(marker, out string) string {
			if marker == proxyRouteMarker {
				return strings.Replace(out, "v4_routes=0|1\n", "v4_routes="+value+"\n", 1)
			}
			return out
		}
		requireProxyAuthorityClasses(t, runProxyAuthority(t, results, []string{"ipv4"}, false, mutate), map[string]int{"cannot-observe": 1})
	}
}

// A successful native producer still requires a completed numeric reduction.
func TestProxyPathAuthorityNativeReducerFailure(t *testing.T) {
	for _, result := range []proxyAuthorityResult{
		{status: 17},
		{output: "1\n", status: 17},
		{output: "private-policy-value\n"},
	} {
		results := proxyAuthorityHealthy()
		results["REDUCER"] = result
		requireProxyAuthorityClasses(t, runProxyAuthority(t, results, []string{"ipv4"}, false, nil), map[string]int{"cannot-observe": 1})
	}
}

// Even a parseable noncompliant numeric setting is reduced to a fixed label.
func TestProxyPathAuthorityNativeAptValuePrivacy(t *testing.T) {
	results := proxyAuthorityHealthy()
	results["APT"] = proxyAuthorityResult{output: "monitor_periodic='987654321012345678'\n"}
	results["APT_DUMP"] = proxyAuthorityResult{output: "APT::Periodic::Enable \"987654321012345678\";\n"}
	alerts := runProxyAuthority(t, results, []string{"ipv4"}, false, nil)
	requireProxyAuthorityClasses(t, alerts, map[string]int{"edge-auto-upgrades": 1})
	if !strings.Contains(alerts[0].Observed, "periodic_enable=other-numeric") {
		t.Fatal("noncompliant APT value did not use the fixed public label")
	}
}

// A duplicate sibling field invalidates both copies without discarding a
// deficit proved by a different, uniquely framed native result.
func TestProxyPathAuthorityNativeDuplicatePreservesIndependentPositive(t *testing.T) {
	for _, duplicateFirst := range []bool{false, true} {
		results := proxyAuthorityHealthy()
		results["V4ROUTE"] = proxyAuthorityResult{}
		mutate := func(marker, out string) string {
			if marker != proxyRouteMarker {
				return out
			}
			row := "v6_rules=0|1\n"
			duplicate := "v6_rules=0|private-policy-value\n"
			if duplicateFirst {
				return strings.Replace(out, row, duplicate+row, 1)
			}
			return strings.Replace(out, row, row+duplicate, 1)
		}
		requireProxyAuthorityClasses(t, runProxyAuthority(t, results, []string{"ipv4", "ipv6"}, false, mutate), map[string]int{"policy-route-drift": 1, "cannot-observe": 1})
	}
}

// A legacy is-enabled receipt cannot be reinterpreted as a successful property
// read even when its values would otherwise satisfy the exact mask policy.
func TestProxyPathAuthorityNativeRejectsLegacyPolicyFrame(t *testing.T) {
	mutate := func(marker, out string) string {
		if marker == edgeAutoUpgradeMarker {
			return strings.Replace(out, "complete=apt-policy-v2", "complete=apt-policy-v1", 1)
		}
		return out
	}
	requireProxyAuthorityClasses(t, runProxyAuthority(t, proxyAuthorityHealthy(), []string{"ipv4"}, false, mutate), map[string]int{"cannot-observe": 1})
}

// Failure of a property read does not erase another unit's successful policy
// observation, regardless of which result the collector encounters first.
func TestProxyPathAuthorityNativeFailedPropertyPreservesPeer(t *testing.T) {
	for _, failedUnit := range []string{"DAILY_TIMER", "UNATTENDED"} {
		for _, status := range []int{1, 4, 17, 126, 127, 255} {
			results := proxyAuthorityHealthy()
			results[failedUnit] = proxyAuthorityResult{output: "masked\n", status: status}
			results["UPGRADE_TIMER"] = proxyAuthorityResult{output: "enabled\n"}
			alerts := runProxyAuthority(t, results, []string{"ipv4"}, false, nil)
			requireProxyAuthorityClasses(t, alerts, map[string]int{"cannot-observe": 1, "edge-auto-upgrades": 1})
		}
	}
}
