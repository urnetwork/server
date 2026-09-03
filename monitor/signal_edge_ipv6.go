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
	edgeIPv6IdentityMarker = "monitor-signal-18.1-edge-ipv6-identity"
	edgeIPv6EgressMarker   = "monitor-signal-18.1-edge-ipv6-egress"
)

type edgeIPv6Result struct {
	host        *host
	configured  EdgeIPv6InterfaceSettings
	http        map[string]string
	httpOutput  string
	httpErr     error
	identity    map[string]string
	identityRaw string
	identityErr error
	egress      map[string]string
	egressRaw   string
	egressErr   error
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

	findings := []finding{}
	for _, result := range ordered {
		findings = append(findings, edgeIPv6Findings(result)...)
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
self_probe=$(curl --ipv6 --http1.1 --silent --show-error --connect-timeout 3 --max-time 5 --noproxy '*' --interface "$configured_address" --resolve "$probe_hostname:443:[$configured_address]" --output /dev/null --write-out 'self_http_code=%%{http_code}\nself_exitcode=%%{exitcode}\n' "https://$probe_hostname/hello" 2>&1)
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

func edgeIPv6Findings(result edgeIPv6Result) []finding {
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

	class, mechanism, action := classifyEdgeIPv6Failure(result)
	observed := fmt.Sprintf(
		"address=%s interface=%s http_code=%s curl_exit=%s remote_ip=%s total_seconds=%s operstate=%s configured_present=%s unit_active=%s self_http_code=%s self_exit=%s route_device=%s route_source=%s route_status=%s source_egress=%s source_egress_status=%s",
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
		result.egress["route_device"],
		result.egress["route_source"],
		result.egress["route_status"],
		result.egress["source_egress"],
		result.egress["source_egress_status"],
	)
	evidence := strings.TrimSpace(strings.Join([]string{
		"public probe: " + strings.TrimSpace(result.httpOutput),
		"public probe error: " + errorString(result.httpErr),
		"host identity: " + strings.TrimSpace(result.identityRaw),
		"bound source egress: " + strings.TrimSpace(result.egressRaw),
	}, "\n"))
	findings = append(findings, finding{
		probeId: "lb/edge-ipv6", tier: tierPage,
		class: class, target: target, frame: frame, sustain: 2,
		symptom:   fmt.Sprintf("%s %s fails pinned public IPv6 HTTPS", target, result.configured.Interface),
		mechanism: mechanism,
		baseline:  "Every enabled edge LB interface returns HTTP 200 from three consecutive HTTPS requests pinned to its exact active services.yml IPv6 address with api-v6 SNI.",
		observed:  observed,
		evidence:  evidence,
		action:    action,
		verify:    "Repeat three exact-address HTTP/1.1 IPv6 requests, require three 200 responses, and confirm the repaired layer's counters advance without changing the configured identity.",
		playbook:  "SIGNALS.md §18.1",
	})
	return findings
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
