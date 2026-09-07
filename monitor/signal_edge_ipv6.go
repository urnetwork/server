package monitor

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// SIGNALS.md §18.1 maps to signal_edge_ipv6.go and
// signal_edge_ipv6_test.go. The exact public addresses come from the active
// services.yml version, while each request retains api-v6 SNI; DNS health
// selection therefore cannot hide one failed interface.
func NewEdgeIPv6Signal() Signal {
	return &signalAdapter{
		number: "18.1",
		key:    "edge-ipv6",
		name:   "Edge IPv6 public ingress",
		probe:  edgeIPv6Probe{},
	}
}

type edgeIPv6Probe struct{}

func (edgeIPv6Probe) id() string             { return "lb/edge-ipv6" }
func (edgeIPv6Probe) tier() string           { return tierPage }
func (edgeIPv6Probe) cadence() time.Duration { return 5 * time.Minute }

const (
	edgeIPv6IdentityMarker  = "monitor-signal-18.1-edge-ipv6-identity"
	edgeIPv6EgressMarker    = "monitor-signal-18.1-edge-ipv6-egress"
	edgeIPv6AdmissionMarker = "monitor-signal-18.1-edge-ipv6-lb-admission"
)

type edgeIPv6Result struct {
	host         *host
	configured   EdgeIPv6InterfaceSettings
	http         map[string]string
	httpOutput   string
	httpErr      error
	identity     map[string]string
	identityRaw  string
	identityErr  error
	egress       map[string]string
	egressRaw    string
	egressErr    error
	admission    map[string]string
	admissionErr error
}

func (edgeIPv6Probe) check(ctx context.Context, env *probeEnv) ([]finding, error) {
	tasks := []edgeIPv6Result{}
	for _, target := range env.cfg.hosts {
		for _, configured := range target.edgeIPv6 {
			tasks = append(tasks, edgeIPv6Result{host: target, configured: configured})
		}
	}
	if len(tasks) == 0 {
		return nil, nil
	}
	observerRouteBefore := observeIPv6ObserverRoute(ctx, env.runner)

	results := make(chan edgeIPv6Result, len(tasks))
	semaphore := make(chan struct{}, 8)
	var wait sync.WaitGroup
	for _, queued := range tasks {
		task := queued
		wait.Add(1)
		go func() {
			defer wait.Done()
			select {
			case semaphore <- struct{}{}:
				defer func() { <-semaphore }()
			case <-ctx.Done():
				task.httpErr = ctx.Err()
				results <- task
				return
			}
			results <- runEdgeIPv6Task(ctx, env, task)
		}()
	}
	wait.Wait()
	close(results)

	ordered := make([]edgeIPv6Result, 0, len(tasks))
	for result := range results {
		ordered = append(ordered, result)
	}
	sort.Slice(ordered, func(i, j int) bool {
		if ordered[i].host.name != ordered[j].host.name {
			return ordered[i].host.name < ordered[j].host.name
		}
		return ordered[i].configured.Interface < ordered[j].configured.Interface
	})

	allImmediateConnectFailures := edgeIPv6AllImmediateConnectFailures(ordered)
	observerRoute := observerRouteBefore
	if allImmediateConnectFailures {
		observerRoute = mergeIPv6ObserverRouteObservations(
			observerRouteBefore,
			observeIPv6ObserverRoute(ctx, env.runner),
		)
	}
	observerCommonMode := observerRoute.state == ipv6ObserverRouteAbsent &&
		allImmediateConnectFailures
	observerRouteUnknown := observerRoute.state == ipv6ObserverRouteUnobservable &&
		allImmediateConnectFailures
	findings := []finding{}
	for _, result := range ordered {
		findings = append(findings, edgeIPv6Findings(
			result,
			observerCommonMode,
			observerRouteUnknown,
		)...)
	}
	if observerCommonMode {
		resolvedTargets := map[string]bool{}
		for _, result := range ordered {
			target := result.host.name
			if resolvedTargets[target] {
				continue
			}
			resolvedTargets[target] = true
			findings = append(findings, healthyFinding(
				"lb/edge-ipv6",
				tierPage,
				"edge-ipv6-reset",
				target,
			))
		}
		findings = append(findings, ipv6ObserverRouteFinding(
			"edge-ipv6",
			edgeIPv6ObserverRouteSummary(ordered, observerRoute),
		))
	} else if observerRoute.state == ipv6ObserverRouteAvailable || edgeIPv6AnyPublicHealthy(ordered) {
		findings = append(findings, healthyIPv6ObserverRouteFinding("edge-ipv6"))
	}
	return findings, nil
}

func runEdgeIPv6Task(ctx context.Context, env *probeEnv, result edgeIPv6Result) edgeIPv6Result {
	configured := result.configured
	hostname := strings.TrimSpace(configured.ProbeHostname)
	if hostname == "" {
		hostname = "api-v6.bringyour.com"
	}
	public := runExactHTTPS(ctx, env.runner, hostname, configured.Address, "/hello")
	result.httpOutput = public.output
	result.httpErr = public.err
	result.http = public.values

	identityCommand := edgeIPv6IdentityCommand(configured)
	result.identityRaw, result.identityErr = env.runner.shell(ctx, result.host, identityCommand)
	result.identity = parseKeyValueLines(result.identityRaw)

	if exactHTTPSHealthy(public) {
		return result
	}
	egressCommand := edgeIPv6EgressCommand(configured)
	result.egressRaw, result.egressErr = env.runner.shell(ctx, result.host, egressCommand)
	result.egress = parseKeyValueLines(result.egressRaw)
	if edgeIPv6AdmissionCandidate(result) {
		admissionOutput, err := env.runner.shell(
			ctx,
			result.host,
			edgeIPv6AdmissionCommand(configured, env.cfg.env),
		)
		result.admissionErr = err
		if err == nil {
			result.admission, result.admissionErr = parseEdgeIPv6Admission(admissionOutput)
		}
	}
	return result
}

func edgeIPv6IdentityCommand(configured EdgeIPv6InterfaceSettings) string {
	interfaceName := shellSingleQuote(configured.Interface)
	address := shellSingleQuote(configured.Address)
	unit := shellSingleQuote("warp-main-lb-" + configured.Interface + ".service")
	return fmt.Sprintf(`# %s
interface_name=%s
configured_address=%s
operstate=$(cat /sys/class/net/"$interface_name"/operstate 2>/dev/null || true)
configured_present=$(ip -6 -o addr show dev "$interface_name" scope global 2>/dev/null | awk -v want="$configured_address" '{split($4,a,"/"); if (a[1] == want) found=1} END {print found+0}')
unit_active=$(systemctl is-active %s 2>/dev/null || true)
printf 'operstate=%%s\nconfigured_present=%%s\nunit_active=%%s\n' "$operstate" "$configured_present" "$unit_active"`,
		edgeIPv6IdentityMarker, interfaceName, address, unit)
}

func edgeIPv6EgressCommand(configured EdgeIPv6InterfaceSettings) string {
	address := shellSingleQuote(configured.Address)
	hostname := strings.TrimSpace(configured.ProbeHostname)
	if hostname == "" {
		hostname = "api-v6.bringyour.com"
	}
	probeHostname := shellSingleQuote(hostname)
	return fmt.Sprintf(`# %s
configured_address=%s
probe_hostname=%s
self_probe=$(curl --ipv6 --http1.1 --silent --show-error --connect-timeout 3 --max-time 5 --noproxy '*' --interface "$configured_address" --resolve "$probe_hostname:443:[$configured_address]" --output /dev/null --write-out 'self_http_code=%%{http_code}\nself_exitcode=%%{exitcode}\nself_time_total=%%{time_total}\n' "https://$probe_hostname/hello" 2>&1)
self_probe_status=$?
route_probe=$(ip -6 route get 2606:4700:4700::1111 from "$configured_address" 2>&1)
route_status=$?
route_device=$(printf '%%s\n' "$route_probe" | awk '{for (i=1; i<=NF; i++) if ($i == "dev" && i < NF) {print $(i+1); exit}}')
route_source=$(printf '%%s\n' "$route_probe" | awk '{for (i=1; i<=NF; i++) if ($i == "src" && i < NF) {print $(i+1); exit}}')
source_egress=$(curl --ipv6 --silent --show-error --connect-timeout 3 --max-time 5 --noproxy '*' --interface "$configured_address" https://api64.ipify.org 2>&1)
source_egress_status=$?
source_egress=$(printf '%%s' "$source_egress" | tr -d '\r\n')
printf '%%s\nself_probe_status=%%s\nroute_device=%%s\nroute_source=%%s\nroute_status=%%s\nsource_egress=%%s\nsource_egress_status=%%s\n' "$self_probe" "$self_probe_status" "$route_device" "$route_source" "$route_status" "$source_egress" "$source_egress_status"`,
		edgeIPv6EgressMarker, address, probeHostname)
}

// edgeIPv6AdmissionCommand reduces process, socket, and journal state on the
// host. It deliberately returns only counts: neither the unit arguments nor a
// journal message can enter alert evidence.
func edgeIPv6AdmissionCommand(configured EdgeIPv6InterfaceSettings, environment string) string {
	unit := shellSingleQuote("warp-" + environment + "-lb-" + configured.Interface + ".service")
	expectedEnvironment := shellSingleQuote(environment)
	expectedBlock := shellSingleQuote(configured.Interface)
	return fmt.Sprintf(`# %s
set -u
unit=%s
expected_environment=%s
expected_block=%s
for required in awk journalctl ss systemctl timeout tr; do
  command -v "$required" >/dev/null 2>&1 || exit 20
done
main_pid=$(timeout 5s systemctl show "$unit" -p MainPID --value 2>/dev/null) || exit 21
case "$main_pid" in ''|*[!0-9]*|0) exit 22 ;; esac
[ -r "/proc/$main_pid/cmdline" ] || exit 23
arguments=$(timeout 5s tr '\000' '\n' < "/proc/$main_pid/cmdline") || exit 24
identity_count=$(printf '%%s\n' "$arguments" | awk -v environment="$expected_environment" -v block="$expected_block" '
  { argument[++count]=$0 }
  END {
    matches=0
    for (i=1; i+4<=count; i++) {
      if (argument[i] == "service" && argument[i+1] == "run" &&
          argument[i+2] == environment && argument[i+3] == "lb" &&
          argument[i+4] == block) matches++
    }
    print matches
  }') || exit 25
[ "$identity_count" = 1 ] || exit 26
portblocks=$(printf '%%s\n' "$arguments" | awk '
  index($0, "--portblocks=") == 1 { value=substr($0, 14); matches++ }
  END { if (matches == 1 && value != "") print value; else exit 1 }') || exit 27
socket_rows=$(timeout 5s sh -c 'set -e; ss -ltnH; ss -lunH' 2>/dev/null) || exit 28
listener_count=$(printf '%%s\n' "$socket_rows" | awk -v blocks="$portblocks" '
  BEGIN {
    block_count=split(blocks, block, ";")
    for (i=1; i<=block_count; i++) {
      if (split(block[i], fields, ":") != 3) invalid=1
      spec_count=split(fields[3], spec, ",")
      for (j=1; j<=spec_count; j++) {
        range_count=split(spec[j], range, "-")
        if (range_count == 1 && range[1] ~ /^[0-9]+$/) {
          low[++ranges]=range[1]+0; high[ranges]=range[1]+0
        } else if (range_count == 2 && range[1] ~ /^[0-9]+$/ && range[2] ~ /^[0-9]+$/ && range[1]+0 <= range[2]+0) {
          low[++ranges]=range[1]+0; high[ranges]=range[2]+0
        } else invalid=1
      }
    }
  }
  {
    port=$4
    sub(/^.*:/, "", port)
    if (port !~ /^[0-9]+$/) next
    for (i=1; i<=ranges; i++) {
      if (low[i] <= port+0 && port+0 <= high[i]) { listeners++; break }
    }
  }
  END { if (invalid || ranges == 0) exit 1; print listeners+0 }') || exit 29
journal_identifier="warp|$expected_environment|lb|$expected_block"
journal_lines=$(timeout 10s journalctl --no-pager --quiet --since '15 minutes ago' -n 20 -o cat SYSLOG_IDENTIFIER="$journal_identifier" --grep='^nginx: \[emerg\] could not build map_hash, you should increase map_hash_bucket_size: [0-9]+$' 2>/dev/null)
journal_status=$?
if [ "$journal_status" -ne 0 ] && { [ "$journal_status" -ne 1 ] || [ -n "$journal_lines" ]; }; then
  exit 30
fi
map_hash_error_count=$(printf '%%s\n' "$journal_lines" | awk '
  NF {
    if ($0 !~ /^nginx: \[emerg\] could not build map_hash, you should increase map_hash_bucket_size: [0-9]+$/) invalid=1
    matches++
  }
  END { if (invalid || matches > 20) exit 1; print matches+0 }') || exit 31
printf 'lb_observation_status=1\nlb_listener_count=%%s\nlb_map_hash_error_count=%%s\n' "$listener_count" "$map_hash_error_count"`,
		edgeIPv6AdmissionMarker, unit, expectedEnvironment, expectedBlock)
}

func parseEdgeIPv6Admission(output string) (map[string]string, error) {
	values := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(output), "\n") {
		parts := strings.SplitN(strings.TrimSpace(line), "=", 2)
		if len(parts) != 2 || values[parts[0]] != "" {
			return nil, fmt.Errorf("edge IPv6 LB admission observation is malformed")
		}
		switch parts[0] {
		case "lb_observation_status", "lb_listener_count", "lb_map_hash_error_count":
			values[parts[0]] = parts[1]
		default:
			return nil, fmt.Errorf("edge IPv6 LB admission observation is malformed")
		}
	}
	listeners, listenerErr := strconv.Atoi(values["lb_listener_count"])
	errors, errorErr := strconv.Atoi(values["lb_map_hash_error_count"])
	if values["lb_observation_status"] != "1" || listenerErr != nil || listeners < 0 ||
		errorErr != nil || errors < 0 || errors > 20 {
		return nil, fmt.Errorf("edge IPv6 LB admission observation is malformed")
	}
	return values, nil
}

func edgeIPv6AdmissionCandidate(result edgeIPv6Result) bool {
	publicTotal, publicTotalErr := strconv.ParseFloat(result.http["monitor_time_total"], 64)
	selfTotal, selfTotalErr := strconv.ParseFloat(result.egress["self_time_total"], 64)
	return result.identityErr == nil && result.egressErr == nil &&
		result.http["monitor_exitcode"] == "7" && result.http["monitor_remote_ip"] == "" &&
		publicTotalErr == nil && publicTotal < 1 &&
		result.identity["configured_present"] == "1" && result.identity["operstate"] == "up" &&
		result.identity["unit_active"] == "active" &&
		result.egress["self_probe_status"] == "7" && result.egress["self_exitcode"] == "7" &&
		result.egress["self_http_code"] == "000" && selfTotalErr == nil && selfTotal < 1 &&
		result.egress["route_status"] == "0" &&
		result.egress["route_device"] == result.configured.Interface &&
		result.egress["route_source"] == result.configured.Address &&
		result.egress["source_egress_status"] == "0" &&
		result.egress["source_egress"] == result.configured.Address
}

func edgeIPv6Findings(
	result edgeIPv6Result,
	observerCommonMode bool,
	observerRouteUnknown bool,
) []finding {
	target := result.host.name
	frame := result.configured.Interface + "/" + result.configured.Address
	findings := []finding{}
	if result.identityErr != nil {
		findings = append(findings, cannotObserveFinding(target+"/"+result.configured.Interface+"/identity", result.identityErr))
	} else if result.identity["configured_present"] != "1" ||
		result.identity["operstate"] != "up" ||
		result.identity["unit_active"] != "active" {
		findings = append(findings, finding{
			probeId: "lb/edge-ipv6", tier: tierPage,
			class: "edge-ipv6-identity-drift", target: target, frame: frame, sustain: 1,
			symptom:   fmt.Sprintf("%s %s does not own its active services.yml IPv6 address", target, result.configured.Interface),
			mechanism: "Warpctl, the upstream ACL, and the public probe can target an address the host does not own when an interface or NIC-derived identity changes without every source of truth moving together.",
			baseline:  "Every active services.yml LB IPv6 address appears exactly on its configured live interface, whose link and LB controller are active.",
			observed:  fmt.Sprintf("configured=%s interface=%s operstate=%s configured_present=%s unit_active=%s", result.configured.Address, result.configured.Interface, result.identity["operstate"], result.identity["configured_present"], result.identity["unit_active"]),
			evidence:  strings.TrimSpace(result.identityRaw),
			action:    "Reconcile active Vault, the live interface, persistent host networking, DNS, and the upstream router permit destination before changing any route or container.",
			verify:    "The active Vault address equals the live interface and upstream ACL destination, then three pinned HTTP/1.1 IPv6 requests return 200.",
			playbook:  "SIGNALS.md §18.1",
		})
	}

	if exactHTTPSHealthy(exactHTTPSResult{values: result.http, output: result.httpOutput, err: result.httpErr}) {
		return findings
	}
	if result.egressErr != nil {
		findings = append(findings, cannotObserveFinding(target+"/"+result.configured.Interface+"/source-egress", result.egressErr))
	}
	if result.admissionErr != nil {
		findings = append(findings, cannotObserveFinding(target+"/"+result.configured.Interface+"/lb-admission", result.admissionErr))
	}
	if observerCommonMode {
		return findings
	}
	if observerRouteUnknown {
		findings = append(findings, edgeIPv6RouteUnobservableFinding(result))
		return findings
	}

	class, mechanism, action := classifyEdgeIPv6Failure(result)
	observed := fmt.Sprintf(
		"address=%s interface=%s http_code=%s curl_exit=%s remote_ip=%s total_seconds=%s operstate=%s configured_present=%s unit_active=%s self_http_code=%s self_exit=%s self_total_seconds=%s route_device=%s route_source=%s route_status=%s source_egress=%s source_egress_status=%s",
		result.configured.Address,
		result.configured.Interface,
		result.http["monitor_http_code"],
		result.http["monitor_exitcode"],
		result.http["monitor_remote_ip"],
		result.http["monitor_time_total"],
		result.identity["operstate"],
		result.identity["configured_present"],
		result.identity["unit_active"],
		result.egress["self_http_code"],
		result.egress["self_exitcode"],
		result.egress["self_time_total"],
		result.egress["route_device"],
		result.egress["route_source"],
		result.egress["route_status"],
		result.egress["source_egress"],
		result.egress["source_egress_status"],
	)
	if result.admission != nil {
		observed += fmt.Sprintf(
			" lb_listener_count=%s lb_map_hash_error_count=%s",
			result.admission["lb_listener_count"],
			result.admission["lb_map_hash_error_count"],
		)
	}
	evidence := strings.TrimSpace(strings.Join([]string{
		"public probe: " + strings.TrimSpace(result.httpOutput),
		"public probe error: " + errorString(result.httpErr),
		"host identity: " + strings.TrimSpace(result.identityRaw),
		"bound source egress: " + strings.TrimSpace(result.egressRaw),
	}, "\n"))
	verify := "Repeat three exact-address HTTP/1.1 IPv6 requests, require three 200 responses, and confirm the repaired layer's counters advance without changing the configured identity."
	if class == "edge-lb-config-rejected" {
		verify = "Confirm the exact corrected Warp artifact is deployed, at least one configured-pool listener is live, no new exact map-hash admission signature appears for 15 minutes, and three pinned HTTP/1.1 IPv6 requests from each of three independent external observers return 200."
	}
	findings = append(findings, finding{
		probeId: "lb/edge-ipv6", tier: tierPage,
		class: class, target: target, frame: frame, sustain: 2,
		symptom:   fmt.Sprintf("%s %s fails pinned public IPv6 HTTPS", target, result.configured.Interface),
		mechanism: mechanism,
		baseline:  "Every enabled edge LB interface returns HTTP 200 from three consecutive HTTPS requests pinned to its exact active services.yml IPv6 address with api-v6 SNI.",
		observed:  observed,
		evidence:  evidence,
		action:    action,
		verify:    verify,
		playbook:  "SIGNALS.md §18.1",
	})
	return findings
}

func edgeIPv6RouteUnobservableFinding(result edgeIPv6Result) finding {
	target := result.host.name + "/" + result.configured.Interface + "/public-ipv6"
	return finding{
		probeId: "monitor/visibility", tier: tierWarn,
		class: "cannot-observe", target: target, sustain: 2,
		symptom:   "The monitor could not distinguish an observer IPv6 route loss from an immediate exact-edge connection failure for " + target,
		mechanism: "Every configured exact-edge request failed immediately without a remote address, but the bounded monitor-local route lookup was itself unavailable or ambiguous. Assigning reset, DNAT, or certificate causality would be unsafe until that observer control works.",
		baseline:  "The monitor-local IPv6 route lookup is parseable before and after an all-edge failure, or another exact IPv6 target proves the observer route while this target fails.",
		observed:  "observer_route=unobservable curl_exit=7 remote_ip=none total_seconds=under_1",
		evidence:  "The finding retains only the allowlisted route state and curl result shape; raw command errors and configured addresses are omitted.",
		action:    "Restore the monitor-local route observation and rerun this signal. Do not remove DNAT targets, change edge routes, or restart an LB from this unknown result.",
		verify:    "Prove the monitor route and an unrelated IPv6 control, then repeat the exact edge request; classify the edge only from that routed observation.",
		playbook:  "SIGNALS.md §18.1",
	}
}

func edgeIPv6AllImmediateConnectFailures(results []edgeIPv6Result) bool {
	if len(results) == 0 {
		return false
	}
	for _, result := range results {
		total, err := strconv.ParseFloat(result.http["monitor_time_total"], 64)
		if err != nil || total >= 1 ||
			result.http["monitor_exitcode"] != "7" ||
			result.http["monitor_remote_ip"] != "" {
			return false
		}
	}
	return true
}

func edgeIPv6AnyPublicHealthy(results []edgeIPv6Result) bool {
	for _, result := range results {
		if exactHTTPSHealthy(exactHTTPSResult{
			values: result.http,
			output: result.httpOutput,
			err:    result.httpErr,
		}) {
			return true
		}
	}
	return false
}

func edgeIPv6ObserverRouteSummary(
	results []edgeIPv6Result,
	observerRoute ipv6ObserverRouteObservation,
) string {
	identityHealthy := 0
	selfHTTPSHealthy := 0
	sourceRouteExact := 0
	sourceEgressExact := 0
	for _, result := range results {
		if result.identityErr == nil &&
			result.identity["configured_present"] == "1" &&
			result.identity["operstate"] == "up" &&
			result.identity["unit_active"] == "active" {
			identityHealthy++
		}
		if result.egressErr == nil &&
			result.egress["self_probe_status"] == "0" &&
			result.egress["self_exitcode"] == "0" &&
			result.egress["self_http_code"] == "200" {
			selfHTTPSHealthy++
		}
		if result.egressErr == nil &&
			result.egress["route_status"] == "0" &&
			result.egress["route_device"] == result.configured.Interface &&
			result.egress["route_source"] == result.configured.Address {
			sourceRouteExact++
		}
		if result.egressErr == nil &&
			result.egress["source_egress_status"] == "0" &&
			result.egress["source_egress"] == result.configured.Address {
			sourceEgressExact++
		}
	}
	return fmt.Sprintf(
		"observer_route=%s configured_targets=%d immediate_connect_failures=%d identity_healthy=%d local_self_https_healthy=%d source_route_exact=%d source_egress_exact=%d",
		observerRoute.state,
		len(results),
		len(results),
		identityHealthy,
		selfHTTPSHealthy,
		sourceRouteExact,
		sourceEgressExact,
	)
}

func classifyEdgeIPv6Failure(result edgeIPv6Result) (class, mechanism, action string) {
	exitCode := result.http["monitor_exitcode"]
	total, _ := strconv.ParseFloat(result.http["monitor_time_total"], 64)
	sourceMatches := result.egress["source_egress_status"] == "0" &&
		result.egress["source_egress"] == result.configured.Address
	selfProbeHealthy := result.egress["self_probe_status"] == "0" &&
		result.egress["self_exitcode"] == "0" &&
		result.egress["self_http_code"] == "200"
	policyRouteMismatch := result.egress["route_status"] == "0" &&
		result.egress["route_device"] != "" && result.egress["route_source"] != "" &&
		(result.egress["route_device"] != result.configured.Interface ||
			result.egress["route_source"] != result.configured.Address)

	if exitCode == "28" || strings.Contains(strings.ToLower(result.httpOutput), "timed out") {
		if policyRouteMismatch && selfProbeHealthy && result.identity["configured_present"] == "1" {
			return "edge-ipv6-policy-route",
				"The host owns and locally serves the configured address, but a source-specific IPv6 lookup selects a different device or source. A carrier or network-manager cycle removed the LB policy routes/rules while its controller remained active, so replies leave through the lower-metric management default and external TLS times out.",
				"Inspect the exact IPv6 rule and LB route table. If the running Warpctl predates Warp 8924493, deploy that bounded non-transparent LB policy reconciliation and restart only the affected LB controller units with operator authorization; otherwise inspect its route-command errors. Require the route lookup to select this interface and source before repeating three external probes."
		}
		if sourceMatches && selfProbeHealthy && result.identity["configured_present"] == "1" {
			return "edge-ipv6-upstream-drop",
				"The host owns the configured address, serves HTTP 200 when the same SNI request is pinned locally to it, and returns exact-source IPv6 egress through it, but an external connection silently times out. That confines the fault to external ingress; unchanged host DNAT counters identify the upstream default-drop/ACL signature. A stale permit destination can still allow ICMPv6 and established return traffic.",
				"Compare the upstream IPv6 allow-rule destination with active services.yml and the live interface. Confirm the external probe leaves the host DNAT counter unchanged, then correct only stale destination identities; retain the default drop and existing ports/actions."
		}
		return "edge-ipv6-timeout",
			"The pinned TCP/TLS path silently timed out, but the source-bound return-path proof was absent or disagreed. Routing, NDP, upstream filtering, or host ingress must be localized before changing service state.",
			"Capture the pinned SYN at the host, inspect exact DNAT counters, and verify source-bound egress plus gateway reachability. Change only the first layer where packets disappear."
	}
	if exitCode == "7" && total < 1 {
		listenerCount, listenerErr := strconv.Atoi(result.admission["lb_listener_count"])
		mapHashErrors, mapHashErr := strconv.Atoi(result.admission["lb_map_hash_error_count"])
		if edgeIPv6AdmissionCandidate(result) && result.admissionErr == nil &&
			result.admission["lb_observation_status"] == "1" &&
			listenerErr == nil && listenerCount == 0 &&
			mapHashErr == nil && mapHashErrors > 0 {
			return "edge-lb-config-rejected",
				"The interface, configured address, LB controller, exact source route, and bound-source egress are healthy, but both public and host-local SNI fail immediately, no configured LB pool listener is live, and the bounded recent journal observation found nginx rejecting the generated configuration because its map hash bucket is too small. The active controller is not proof of LB readiness; this is configuration admission failure, not dead-first DNAT.",
				"Build and deploy a Warp LB artifact whose generated nginx HTTP configuration explicitly sizes map_hash_bucket_size for its longest generated status-map key and passes production-capable nginx validation. Do not edit live generated config, remove DNAT targets, change routes, or restart the unchanged artifact."
		}
		return "edge-ipv6-reset",
			"The exact public tuple rejected immediately. During a rolling LB drain this is the dead-first DNAT signature: an earlier rule can target a pool port whose nginx listener has closed while a later live target is shadowed.",
			"Inspect ordered IPv4/IPv6 DNAT rules and live sockets. Remove only a fully proven dead target, and deploy the Warp duplicate-to-single socket reconciliation; do not change the IPv6 address or route to treat a reset."
	}
	return "edge-ipv6-http",
		"The exact address connected but did not complete the expected api-v6 HTTP/1.1 200 response, so TLS/SNI, LB ownership, or application readiness is wrong even if the socket is open.",
		"Inspect the returned status/TLS error and the live LB generation for this interface, then repair that layer without allowing DNS to select a healthy sibling."
}

func errorString(err error) string {
	if err == nil {
		return "none"
	}
	return err.Error()
}
