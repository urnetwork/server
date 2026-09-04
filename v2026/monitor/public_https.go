package monitor

import (
	"context"
	"fmt"
	"strings"
)

const ipv6ObserverRouteProbeAddress = "2606:4700:4700::1111"

type ipv6ObserverRouteState string

const (
	ipv6ObserverRouteAvailable    ipv6ObserverRouteState = "available"
	ipv6ObserverRouteAbsent       ipv6ObserverRouteState = "absent"
	ipv6ObserverRouteUnobservable ipv6ObserverRouteState = "unobservable"
)

type ipv6ObserverRouteObservation struct {
	state         ipv6ObserverRouteState
	interfaceName string
}

func mergeIPv6ObserverRouteObservations(
	before ipv6ObserverRouteObservation,
	after ipv6ObserverRouteObservation,
) ipv6ObserverRouteObservation {
	if before.state == ipv6ObserverRouteAbsent || after.state == ipv6ObserverRouteAbsent {
		return ipv6ObserverRouteObservation{state: ipv6ObserverRouteAbsent}
	}
	if before.state == ipv6ObserverRouteAvailable && after.state == ipv6ObserverRouteAvailable {
		if before.interfaceName == after.interfaceName {
			return before
		}
		return ipv6ObserverRouteObservation{state: ipv6ObserverRouteAvailable}
	}
	return ipv6ObserverRouteObservation{state: ipv6ObserverRouteUnobservable}
}

// observeIPv6ObserverRoute asks the monitor host's routing table whether it
// has an IPv6 path before exact-address probes begin. This is deliberately a
// local lookup: it neither contacts an edge nor turns an unrelated public
// endpoint into an availability dependency.
func observeIPv6ObserverRoute(ctx context.Context, runner probeRunner) ipv6ObserverRouteObservation {
	output, err := runner.local(ctx, "/sbin/route", "-n", "get", "-inet6", ipv6ObserverRouteProbeAddress)
	if err != nil {
		if ipv6RouteAbsentOutput(output) {
			return ipv6ObserverRouteObservation{state: ipv6ObserverRouteAbsent}
		}
		return ipv6ObserverRouteObservation{state: ipv6ObserverRouteUnobservable}
	}
	for _, line := range strings.Split(output, "\n") {
		fields := strings.Fields(strings.TrimSpace(line))
		if len(fields) == 2 && fields[0] == "interface:" && fields[1] != "" {
			return ipv6ObserverRouteObservation{
				state:         ipv6ObserverRouteAvailable,
				interfaceName: fields[1],
			}
		}
	}
	return ipv6ObserverRouteObservation{state: ipv6ObserverRouteUnobservable}
}

func ipv6RouteAbsentOutput(output string) bool {
	lower := strings.ToLower(output)
	for _, fragment := range []string{
		"no route to host",
		"network is unreachable",
		"not in table",
		"route has not been found",
	} {
		if strings.Contains(lower, fragment) {
			return true
		}
	}
	return false
}

func ipv6ObserverRouteFinding(scope string, observed string) finding {
	return finding{
		probeId: "monitor/visibility", tier: tierWarn,
		class: "ipv6-observer-route-unavailable", target: "monitor-host/" + scope, sustain: 1,
		symptom:   "The monitor host had no usable IPv6 route while every configured " + scope + " observation failed with the same no-route shape",
		mechanism: "A common monitor-side IPv6 route loss makes every exact edge look unreachable before a packet can leave the observer. It cannot prove a per-edge reset, public-ingress failure, or certificate state; host-local self-ingress remains a separate observation and does not replace externally routed coverage.",
		baseline:  "The monitor has a usable IPv6 route, an unrelated external IPv6 control is reachable, and every configured exact edge is observed independently.",
		observed:  observed,
		evidence:  "Only fixed-cardinality route state and aggregate target/control counts are retained. Raw route output, TLS errors, configured addresses, and endpoint identifiers are omitted.",
		action:    "Restore and diagnose the monitor host's IPv6 default-router/RA path before changing any edge, DNS record, LB, or certificate. Preserve each edge's host-local controls, but keep public ingress and certificate coverage unknown until a genuinely external observer can reach them.",
		verify:    "Require the monitor route lookup and an unrelated external IPv6 control to remain healthy, then obtain three consecutive five-minute exact-edge observations from a routed external observer; host-local self-HTTPS alone cannot close public reachability.",
		playbook:  "SIGNALS.md §18.1 and §18.2",
	}
}

func healthyIPv6ObserverRouteFinding(scope string) finding {
	// The scope belongs in target, not frame: ticket health ignores frame, so
	// one independently scheduled signal must not resolve another's finding.
	return healthyFinding(
		"monitor/visibility",
		tierWarn,
		"ipv6-observer-route-unavailable",
		"monitor-host/"+scope,
	)
}

// exactHTTPSResult is the shared public-ingress observation used by focused
// signals. Hostname remains the TLS SNI/HTTP host while Address pins one
// configured edge, so DNS health selection cannot substitute a sibling.
type exactHTTPSResult struct {
	values map[string]string
	output string
	err    error
}

func runExactHTTPS(
	ctx context.Context,
	runner probeRunner,
	hostname string,
	address string,
	path string,
) exactHTTPSResult {
	hostname = strings.TrimSpace(hostname)
	address = strings.TrimSpace(address)
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	resolve := fmt.Sprintf("%s:443:[%s]", hostname, address)
	args := []string{
		"--ipv6", "--http1.1", "--silent", "--show-error",
		"--connect-timeout", "3", "--max-time", "5", "--noproxy", "*",
		"--resolve", resolve,
		"--output", "/dev/null",
		"--write-out", httpsWriteOut,
		"https://" + hostname + path,
	}
	output, err := runner.local(ctx, "curl", args...)
	return exactHTTPSResult{
		values: parseKeyValueLines(output),
		output: output,
		err:    err,
	}
}

// runPublicHTTPS observes the user-facing DNS/CDN path. It deliberately does
// not follow redirects: a redirect or CDN-generated error is not a successful
// response for callers embedding the requested object.
func runPublicHTTPS(
	ctx context.Context,
	runner probeRunner,
	hostname string,
	path string,
) exactHTTPSResult {
	hostname = strings.TrimSpace(hostname)
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	args := []string{
		"--http1.1", "--silent", "--show-error",
		"--connect-timeout", "3", "--max-time", "5", "--noproxy", "*",
		"--output", "/dev/null",
		"--write-out", httpsWriteOut,
		"https://" + hostname + path,
	}
	output, err := runner.local(ctx, "curl", args...)
	return exactHTTPSResult{
		values: parseKeyValueLines(output),
		output: output,
		err:    err,
	}
}

const httpsWriteOut = "\nmonitor_http_code=%{http_code}\n" +
	"monitor_exitcode=%{exitcode}\n" +
	"monitor_remote_ip=%{remote_ip}\n" +
	"monitor_content_type=%{content_type}\n" +
	"monitor_size_download=%{size_download}\n" +
	"monitor_time_total=%{time_total}\n"

func exactHTTPSHealthy(result exactHTTPSResult) bool {
	return result.err == nil &&
		result.values["monitor_exitcode"] == "0" &&
		result.values["monitor_http_code"] == "200"
}
