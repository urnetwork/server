package monitor

import (
	"context"
	"errors"
	"fmt"
	"net"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// SIGNALS.md §14.5 maps to this file, signal_proxy_path_test.go, and
// signal_proxy_path_authority_test.go.
// Dynamic proxy allocations are discovered from each running container on
// every pass; SignalSettings contains only stable host/public-route identity.
func NewProxyPathSignal() Signal {
	return &signalAdapter{number: "14.5", key: "proxy-path", name: "Public proxy protocol and return path", probe: proxyPublicPathProbe{}}
}

type proxyPublicPathProbe struct{}

func (proxyPublicPathProbe) id() string             { return "proxy/public-path" }
func (proxyPublicPathProbe) tier() string           { return tierWarn }
func (proxyPublicPathProbe) cadence() time.Duration { return 5 * time.Minute }

func (proxyPublicPathProbe) check(ctx context.Context, env *probeEnv) ([]finding, error) {
	findings := []finding{}
	armedHosts := 0
	for _, target := range env.cfg.hosts {
		if target.proxy == nil {
			continue
		}
		armedHosts++
		families, err := normalizedAddressFamilies(target.proxy.AddressFamilies)
		if err != nil {
			findings = append(findings, cannotObserveFinding(target.name+"/proxy-families", err))
			continue
		}

		if target.proxy.PublicHostname != "" {
			allocations, allocationErr := discoverProxyAllocations(ctx, env, target)
			if allocationErr != nil {
				findings = append(findings, proxyAllocationCannotObserveFinding(target.name, allocationErr))
			} else {
				findings = append(findings, evaluateProxyHandshakes(ctx, env, target, families, allocations)...)
			}
		}

		if target.proxy.PublicInterface != "" && target.proxy.RoutingTable > 0 {
			routeFinding, routeErr := evaluateProxyRouteState(ctx, env, target, families)
			if routeErr != nil {
				findings = append(findings, cannotObserveFinding(target.name+"/policy-routing", routeErr))
			}
			if routeFinding != nil {
				findings = append(findings, *routeFinding)
			}
		}

		upgradeFinding, upgradeErr := evaluateEdgeAutoUpgrades(ctx, env, target)
		if upgradeErr != nil {
			findings = append(findings, cannotObserveFinding(target.name+"/apt-policy", upgradeErr))
		}
		if upgradeFinding != nil {
			findings = append(findings, *upgradeFinding)
		}
	}
	if env.cfg.proxyPathExpectedHosts > 0 && armedHosts != env.cfg.proxyPathExpectedHosts {
		findings = append(findings, cannotObserveFinding(
			"proxy-path/inventory",
			fmt.Errorf("active proxy hosts=%d armed proxy hosts=%d", env.cfg.proxyPathExpectedHosts, armedHosts),
		))
	}
	return findings, nil
}

func proxyAllocationCannotObserveFinding(host string, err error) finding {
	target := host + "/proxy-allocations"
	finding := cannotObserveFinding(target, err)
	if classifyObservationError(err) != "observation-access-denied" {
		return finding
	}
	finding.mechanism = "Access was denied while attempting current proxy allocation discovery. The denial may be SSH authentication or remote container-runtime/helper authorization; this class does not prove that the remote command ran. The monitor therefore cannot distinguish an absent allocation from a healthy allocation it is not permitted to inspect; treating the empty output as allocations=0 would be a false service diagnosis."
	finding.baseline = "Every configured proxy host exposes a narrowly scoped, read-only inventory of current proxy container names, WARP_PORTS mappings, and internal readiness to the monitor identity."
	finding.observed = "error_class=observation-access-denied allocation_count=unknown"
	finding.evidence = "The observation attempt returned an access-denied class before any allocation rows were accepted. The denial phase and current allocation count remain unproved; raw command output and runtime details are intentionally omitted."
	finding.context = "This is an operational observation-access prerequisite, not proof that the Proxy service or its public path is down. Membership in the Docker group is effectively root access and is not an acceptable casual monitoring workaround."
	finding.action = "First distinguish SSH authentication from remote container-runtime/helper authorization using bounded phase-specific evidence. Restore the intended SSH authentication only when that phase is proved. If remote inventory authorization is denied, provide the monitor identity a reviewed least-privilege, read-only helper or equivalent surface for only the required allocation fields. Do not add the identity to the Docker group, run the entire monitor as root, or infer zero allocations while access is denied."
	finding.verify = "Run proxy-path twice and require a concrete current allocation on every configured proxy host, internal readiness for each allocation, all configured public family/protocol handshakes, and no cannot-observe or inventory-coverage finding."
	finding.playbook = "SIGNALS.md §14.5 and MONITOR.md §3.6"
	return finding
}

type proxyAllocation struct {
	container      string
	block          string
	ports          map[int]int
	internalStatus int
}

const proxyAllocationMarker = "monitor-signal-14.5-allocations"

func discoverProxyAllocations(ctx context.Context, env *probeEnv, target *host) ([]proxyAllocation, error) {
	out, err := env.runner.shell(ctx, target, `# `+proxyAllocationMarker+`
if [ -x /usr/local/sbin/monitor-proxy-allocations ]; then
  /usr/bin/sudo -n /usr/local/sbin/monitor-proxy-allocations
  exit $?
fi
names=$(docker ps --format '{{.Names}}')
docker_status=$?
if [ "$docker_status" -ne 0 ]; then
  exit "$docker_status"
fi
for name in $names; do
  case "$name" in
    *-proxy-*) ;;
    *) continue ;;
  esac
  environment=$(docker inspect --format '{{range .Config.Env}}{{println .}}{{end}}' "$name" 2>/dev/null)
  inspect_status=$?
  if [ "$inspect_status" -ne 0 ]; then
    exit "$inspect_status"
  fi
  ports=$(printf '%s\n' "$environment" | sed -n 's/^WARP_PORTS=//p' | head -1)
  [ -n "$ports" ] || continue
  status_port=$(printf '%s\n' "$ports" | tr ',' '\n' | awk -F: '$1 == 80 {print $2; exit}')
  if [ -n "$status_port" ]; then
    status=$(curl -sS -o /dev/null -w '%{http_code}' --max-time 4 "http://127.0.0.1:${status_port}/status" 2>/dev/null || true)
  else
    status=missing
  fi
  printf '%s|%s|%s\n' "$name" "$ports" "$status"
done`)
	if err != nil {
		return nil, err
	}
	allocations := []proxyAllocation{}
	prefix := env.cfg.env + "-proxy-"
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		parts := strings.Split(strings.TrimSpace(line), "|")
		if len(parts) != 3 {
			continue
		}
		container := strings.TrimSpace(parts[0])
		if env.cfg.env != "" && !strings.HasPrefix(container, prefix) {
			continue
		}
		ports := parseWarpPorts(parts[1])
		status, _ := strconv.Atoi(strings.TrimSpace(parts[2]))
		block := container
		if rest := strings.TrimPrefix(container, prefix); rest != container {
			if component := strings.Split(rest, "-"); len(component) > 0 && component[0] != "" {
				block = component[0]
			}
		}
		allocations = append(allocations, proxyAllocation{
			container: container, block: block, ports: ports, internalStatus: status,
		})
	}
	return allocations, nil
}

func parseWarpPorts(value string) map[int]int {
	ports := map[int]int{}
	for _, pair := range strings.Split(strings.TrimSpace(value), ",") {
		parts := strings.SplitN(strings.TrimSpace(pair), ":", 2)
		if len(parts) != 2 {
			continue
		}
		servicePort, serviceErr := strconv.Atoi(parts[0])
		hostPort, hostErr := strconv.Atoi(parts[1])
		if serviceErr == nil && hostErr == nil && servicePort > 0 && hostPort > 0 {
			ports[servicePort] = hostPort
		}
	}
	return ports
}

type proxyHandshakeTask struct {
	allocation proxyAllocation
	family     string
}

type proxyHandshakeResult struct {
	allocation proxyAllocation
	family     string
	problems   []string
}

func evaluateProxyHandshakes(ctx context.Context, env *probeEnv, target *host, families []string, allocations []proxyAllocation) []finding {
	if len(allocations) == 0 {
		// nothing on this host was evaluated: readiness and handshakes are unknown
		return []finding{{
			probeId: "proxy/public-path", tier: tierWarn,
			class: "proxy-public-handshake", target: target.name, frame: "allocations", sustain: 2, unobservable: true,
			symptom:   fmt.Sprintf("no running %s proxy allocation could be discovered on %s", env.cfg.env, target.name),
			mechanism: "Without a live container WARP_PORTS mapping, the monitor cannot pair internal readiness with the current public protocol ports; the service may be absent or deployment state may be unreadable.",
			baseline:  "Every configured proxy host exposes at least one running allocation with service ports 80, 8080, 8081, and 8082 mapped to current host ports.",
			observed:  "allocations=0",
			action:    "Inspect running proxy containers and their WARP_PORTS environment, then repair the failed deployment or monitor permissions before testing public ingress.",
			verify:    "Rerun the signal and require a current allocation plus successful internal readiness and public protocol ownership for every block.",
			playbook:  "SIGNALS.md §14.5",
		}}
	}

	type blockReadiness struct {
		total    int
		ready    int
		statuses map[string]int
	}
	readinessByBlock := map[string]*blockReadiness{}
	for _, allocation := range allocations {
		state := readinessByBlock[allocation.block]
		if state == nil {
			state = &blockReadiness{statuses: map[string]int{}}
			readinessByBlock[allocation.block] = state
		}
		state.total++
		if allocation.internalStatus >= 200 && allocation.internalStatus < 300 {
			state.ready++
		}
		state.statuses[proxyInternalReadinessClass(allocation.internalStatus)]++
	}
	blocks := make([]string, 0, len(readinessByBlock))
	for block := range readinessByBlock {
		blocks = append(blocks, block)
	}
	sort.Strings(blocks)
	findings := []finding{}
	for _, block := range blocks {
		state := readinessByBlock[block]
		if state.ready > 0 {
			continue
		}
		statusClasses := make([]string, 0, len(state.statuses))
		for statusClass, count := range state.statuses {
			statusClasses = append(statusClasses, fmt.Sprintf("%s:%d", statusClass, count))
		}
		sort.Strings(statusClasses)
		findings = append(findings, finding{
			probeId: "proxy/public-path", tier: tierWarn,
			class: "proxy-allocation-unready", target: target.name, frame: block, sustain: 2,
			symptom:   fmt.Sprintf("proxy block %s has running allocations but none has a successful internal readiness response", block),
			mechanism: "Allocation discovery succeeded, but every running generation for this stable block returned a missing, failed, or non-2xx internal /status result. Public handshake testing requires a ready process and is intentionally not attempted against these allocations; omitting a separate readiness finding would turn an unready block into false green.",
			baseline:  "Every placed proxy block has at least one current running allocation whose mapped service-port 80 returns HTTP 2xx; a draining sibling may be unready only while another generation remains ready.",
			observed:  fmt.Sprintf("allocations=%d ready_allocations=0 status_classes=%s", state.total, strings.Join(statusClasses, ",")),
			evidence:  "Readiness results are reduced to bounded HTTP status classes; response bodies, container identifiers, and runtime output are not retained.",
			context:   "This proves an internal allocation-readiness failure, not a public DNAT, protocol, return-route, or authenticated-egress failure. Check rollout and process health before interpreting the skipped public layer.",
			action:    "Inspect the stable block's current and draining generations, internal status listener, startup logs, dependencies, and rollout state. Repair the readiness cause or complete/revert the failed rollout; do not change public DNS, DNAT, or policy routing from this finding alone.",
			verify:    "Two consecutive proxy-path probes discover at least one 2xx-ready current allocation for the block, then complete every configured public address-family and protocol handshake without proxy-allocation-unready or proxy-public-handshake findings.",
			playbook:  "SIGNALS.md §14.5",
		})
	}

	tasks := []proxyHandshakeTask{}
	for _, allocation := range allocations {
		// The §14.5 discriminator is specifically internal readiness green while
		// the public path is broken. Other signals own an internally unready
		// process, so it must not be mislabeled as return-path drift here.
		if allocation.internalStatus < 200 || allocation.internalStatus >= 300 {
			continue
		}
		for _, family := range families {
			tasks = append(tasks, proxyHandshakeTask{allocation: allocation, family: family})
		}
	}

	probeCtx := ctx
	if env.cfg.commandTimeout > 0 {
		var cancel context.CancelFunc
		probeCtx, cancel = context.WithTimeout(ctx, env.cfg.commandTimeout)
		defer cancel()
	}
	results := make(chan proxyHandshakeResult, len(tasks))
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
			case <-probeCtx.Done():
				results <- proxyHandshakeResult{allocation: task.allocation, family: task.family, problems: []string{probeCtx.Err().Error()}}
				return
			}
			results <- runProxyHandshakeTask(probeCtx, env, target, task)
		}()
	}
	wait.Wait()
	close(results)

	// Multiple live drain generations can share a block. Aggregate their
	// failures under the stable block/family identity while preserving each
	// concrete container and port in evidence.
	problemsByIdentity := map[string][]string{}
	for result := range results {
		if len(result.problems) == 0 {
			continue
		}
		identity := result.allocation.block + "/" + result.family
		for _, problem := range result.problems {
			problemsByIdentity[identity] = append(problemsByIdentity[identity], result.allocation.container+": "+problem)
		}
	}
	identities := make([]string, 0, len(problemsByIdentity))
	for identity := range problemsByIdentity {
		identities = append(identities, identity)
	}
	sort.Strings(identities)

	for _, identity := range identities {
		problems := problemsByIdentity[identity]
		findings = append(findings, finding{
			probeId: "proxy/public-path", tier: tierWarn,
			class: "proxy-public-handshake", target: target.name, frame: identity, sustain: 2,
			symptom:   fmt.Sprintf("proxy %s has internal HTTP readiness but one or more public protocol handshakes fail", identity),
			mechanism: "The running process is ready on its current allocation, but the family-specific public path does not reach and negotiate with that process; DNAT, proxy ownership, policy routing, or the return path is broken.",
			baseline:  "For every live block and configured address family: internal /status is 2xx, SOCKS selects username/password (05 02), and HTTP plus HTTPS-proxy requests reach a prompt 401/407 authentication rejection.",
			observed:  fmt.Sprintf("host=%s public=%s identity=%s failed_checks=%d", target.name, target.proxy.PublicHostname, identity, len(problems)),
			evidence:  strings.Join(problems, "\n"),
			action:    "Pin the public tuple, compare it with the live allocation, capture the full TCP handshake, and inspect source-aware routes/rules before changing the container or main-table default route.",
			verify:    "Repeat SOCKS, HTTP, and HTTPS-proxy negotiation over every configured family and require the expected authentication response while internal readiness remains 2xx.",
			playbook:  "SIGNALS.md §14.5",
		})
	}
	return findings
}

func proxyInternalReadinessClass(status int) string {
	if status <= 0 {
		return "unavailable"
	}
	return fmt.Sprintf("http-%d", status)
}

var proxyAuthResponse = regexp.MustCompile(`(^|[^0-9])(401|407)([^0-9]|$)`)

func runProxyHandshakeTask(ctx context.Context, env *probeEnv, target *host, task proxyHandshakeTask) proxyHandshakeResult {
	result := proxyHandshakeResult{allocation: task.allocation, family: task.family}
	network := "tcp4"
	curlFamily := "--ipv4"
	if task.family == "ipv6" {
		network = "tcp6"
		curlFamily = "--ipv6"
	}

	socksPort := task.allocation.ports[8080]
	if socksPort == 0 {
		result.problems = append(result.problems, "WARP_PORTS has no SOCKS service-port 8080 allocation")
	} else {
		address := net.JoinHostPort(target.proxy.PublicHostname, strconv.Itoa(socksPort))
		response, err := env.runner.tcpExchange(ctx, network, address, []byte{0x05, 0x01, 0x02}, 2)
		if err != nil {
			result.problems = append(result.problems, fmt.Sprintf("SOCKS %s failed at %s: %s", task.family, address, err))
		} else if len(response) != 2 || response[0] != 0x05 || response[1] != 0x02 {
			result.problems = append(result.problems, fmt.Sprintf("SOCKS %s returned %x at %s, expected 0502", task.family, response, address))
		}
	}

	for _, protocol := range []struct {
		name        string
		scheme      string
		servicePort int
	}{
		{name: "HTTP", scheme: "http", servicePort: 8081},
		{name: "HTTPS-proxy", scheme: "https", servicePort: 8082},
	} {
		port := task.allocation.ports[protocol.servicePort]
		if port == 0 {
			result.problems = append(result.problems, fmt.Sprintf("WARP_PORTS has no %s service-port %d allocation", protocol.name, protocol.servicePort))
			continue
		}
		address := net.JoinHostPort(target.proxy.PublicHostname, strconv.Itoa(port))
		args := []string{curlFamily, "--silent", "--show-error", "--max-time", "4"}
		if protocol.scheme == "https" {
			args = append(args, "--proxy-insecure")
		}
		args = append(args,
			"--proxy", protocol.scheme+"://invalid:invalid@"+address,
			"--output", "/dev/null", "--write-out", "%{http_code}",
			"https://example.com/",
		)
		out, err := env.runner.local(ctx, "curl", args...)
		if !proxyAuthResponse.MatchString(out) {
			result.problems = append(result.problems, fmt.Sprintf("%s %s at %s did not reach a 401/407 auth rejection: output=%q error=%v", protocol.name, task.family, address, strings.TrimSpace(out), err))
		}
	}
	return result
}

const proxyRouteMarker = "monitor-signal-14.5-policy-route"

// Preserve each producer's completion before reducing its output. A known
// deficit survives a sibling observation failure, but unknown clocks cannot
// establish the restart ordering required for this finding.
func evaluateProxyRouteState(ctx context.Context, env *probeEnv, target *host, families []string) (*finding, error) {
	unit := target.proxy.LoadBalancerUnit
	if unit == "" {
		unit = "warp-" + env.cfg.env + "-lb-" + target.proxy.PublicInterface + ".service"
	}
	table := target.proxy.RoutingTable
	command := fmt.Sprintf(`# %s
export LC_ALL=C
proxy_clock() {
  value=$(systemctl show "$2" -p ActiveEnterTimestampMonotonic --value 2>/dev/null)
  status=$?
  case "$value" in ''|*[!0-9]*) value=invalid ;; esac
  printf '%%s=%%s|%%s\n' "$1" "$status" "$value"
}
proxy_clock networkd_start systemd-networkd.service
proxy_clock lb_start %s
for family in 4 6; do
  routes=$(ip "-$family" route show table %d 2>/dev/null)
  status=$?
  count=invalid
  if [ "$status" -eq 0 ]; then
    count=$(awk 'NF { count++ } END { print count+0 }' 2>/dev/null <<EOF
$routes
EOF
    )
    status=$?
  fi
  case "$count" in ''|*[!0-9]*) count=invalid ;; esac
  printf 'v%%s_routes=%%s|%%s\n' "$family" "$status" "$count"
  rules=$(ip "-$family" rule show 2>/dev/null)
  status=$?
  count=invalid
  if [ "$status" -eq 0 ]; then
    count=$(awk -v table=%d '{ for (i=1; i<NF; i++) if ($i == "lookup" && $(i+1) == table) { count++; break } } END { print count+0 }' 2>/dev/null <<EOF
$rules
EOF
    )
    status=$?
  fi
  case "$count" in ''|*[!0-9]*) count=invalid ;; esac
  printf 'v%%s_rules=%%s|%%s\n' "$family" "$status" "$count"
done
printf 'complete=proxy-route-v1\n'`,
		proxyRouteMarker, shellSingleQuote(unit), table, table)
	out, err := env.runner.shell(ctx, target, command)
	if err != nil {
		return nil, err
	}
	keys := []string{"networkd_start", "lb_start", "v4_routes", "v6_routes", "v4_rules", "v6_rules"}
	values, frameErr := parseProxyNativeFrame(out, "proxy-route-v1", keys)
	observationErrors := []error{frameErr}
	numbers := map[string]uint64{}
	observed := []string{}
	for _, key := range keys {
		if strings.HasPrefix(key, "v4_") && !familyEnabled(families, "ipv4") ||
			strings.HasPrefix(key, "v6_") && !familyEnabled(families, "ipv6") {
			continue
		}
		status, value, resultErr := proxyNativeResult(values, key)
		number, numberOk := proxyNativeUint(value)
		switch {
		case resultErr != nil:
			observationErrors = append(observationErrors, resultErr)
		case status != 0:
			observationErrors = append(observationErrors, fmt.Errorf("proxy routing %s: exit status %d", key, status))
		case !numberOk || (key == "networkd_start" || key == "lb_start") && number == 0:
			observationErrors = append(observationErrors, fmt.Errorf("invalid response: proxy routing %s", key))
		default:
			numbers[key] = number
			observed = append(observed, fmt.Sprintf("%s=%d", key, number))
			continue
		}
		observed = append(observed, key+"=unknown")
	}
	observationErr := errors.Join(observationErrors...)
	networkdStart, networkdKnown := numbers["networkd_start"]
	lbStart, lbKnown := numbers["lb_start"]
	if !networkdKnown || !lbKnown || networkdStart <= lbStart {
		return nil, observationErr
	}

	missing := []string{}
	for _, field := range []struct {
		key, description string
	}{
		{key: "v4_routes", description: "IPv4 table routes"},
		{key: "v4_rules", description: "IPv4 source/fwmark rules"},
		{key: "v6_routes", description: "IPv6 table routes"},
		{key: "v6_rules", description: "IPv6 source/fwmark rules"},
	} {
		if count, known := numbers[field.key]; known && count == 0 {
			missing = append(missing, field.description)
		}
	}
	if len(missing) == 0 {
		return nil, observationErr
	}
	return &finding{
		probeId: "host/policy-route-drift", tier: tierWarn,
		class: "policy-route-drift", target: target.name, frame: target.proxy.PublicInterface, sustain: 1,
		symptom:   fmt.Sprintf("systemd-networkd restarted after %s's transparent LB and owned routing state is missing", target.name),
		mechanism: "A later networkd activation precedes the observed absence of Warp's public source-policy routes/rules while the LB process remained up; public SYNs can arrive and SYN-ACKs leave through the LAN default route. The clocks and empty routing results do not alone prove which actor removed that state.",
		baseline:  fmt.Sprintf("routing table %d retains non-empty routes and source/fwmark lookup rules for every enabled family, or the transparent LB starts after networkd and reconciles them", table),
		observed:  strings.Join(observed, " "),
		evidence:  "missing: " + strings.Join(missing, ", "),
		action:    "Run source-aware route-get and packet-capture proofs, then restore the persistent Warp-owned table/rules; do not lower the public main-table metric as a workaround.",
		verify:    "Require both families' owned table/rules, correct source-aware route selection, and successful public SOCKS/HTTP/HTTPS protocol handshakes.",
		playbook:  "SIGNALS.md §14.5",
	}, observationErr
}

const edgeAutoUpgradeMarker = "monitor-signal-14.5-auto-upgrades"

var aptUnits = []string{
	"apt-daily.timer",
	"apt-daily-upgrade.timer",
	"apt-daily.service",
	"apt-daily-upgrade.service",
	"unattended-upgrades.service",
}

// Read only the owned APT key, without evaluating config as shell code. Read
// UnitFileState through show so every accepted property has successful native
// completion; is-enabled's nonzero state results do not certify completion.
func evaluateEdgeAutoUpgrades(ctx context.Context, env *probeEnv, target *host) (*finding, error) {
	command := `# ` + edgeAutoUpgradeMarker + `
export LC_ALL=C
periodic=$(apt-config shell monitor_periodic APT::Periodic::Enable 2>/dev/null)
status=$?
case "$periodic" in
  '') periodic=unset ;;
  "monitor_periodic='"*"'")
    periodic=${periodic#monitor_periodic=\'}
    periodic=${periodic%\'}
    case "$periodic" in ''|*[!0-9]*) periodic=invalid ;; esac ;;
  *) periodic=invalid ;;
esac
printf 'periodic_enable=%s|%s\n' "$status" "$periodic"
for unit in apt-daily.timer apt-daily-upgrade.timer apt-daily.service apt-daily-upgrade.service unattended-upgrades.service; do
  state=$(systemctl show "$unit" -p UnitFileState --value 2>/dev/null)
  status=$?
  case "$state" in
    enabled|enabled-runtime|linked|linked-runtime|masked|masked-runtime|static|disabled) ;;
    *) state=invalid ;;
  esac
  printf '%s=%s|%s\n' "$unit" "$status" "$state"
done
printf 'complete=apt-policy-v2\n'`
	out, err := env.runner.shell(ctx, target, command)
	if err != nil {
		return nil, err
	}
	keys := append([]string{"periodic_enable"}, aptUnits...)
	values, frameErr := parseProxyNativeFrame(out, "apt-policy-v2", keys)
	observationErrors := []error{frameErr}
	drift := []string{}
	observed := []string{}
	for _, key := range keys {
		status, value, resultErr := proxyNativeResult(values, key)
		if resultErr != nil {
			observationErrors = append(observationErrors, resultErr)
			observed = append(observed, key+"=unknown")
			continue
		}
		known, compliant := false, false
		if key == "periodic_enable" {
			_, numeric := proxyNativeUint(value)
			known = status == 0 && (value == "unset" || numeric)
			compliant = value == "0"
		} else {
			switch value {
			case "enabled", "enabled-runtime", "linked", "linked-runtime", "masked", "masked-runtime", "static", "disabled":
				known = status == 0
			}
			compliant = value == "masked" || value == "masked-runtime"
		}
		if !known {
			if status != 0 {
				observationErrors = append(observationErrors, fmt.Errorf("APT policy %s: exit status %d without a valid result", key, status))
			} else {
				observationErrors = append(observationErrors, fmt.Errorf("invalid response: APT policy %s", key))
			}
			observed = append(observed, key+"=unknown")
			continue
		}
		if key == "periodic_enable" && value != "0" && value != "unset" {
			value = "other-numeric"
		}
		observed = append(observed, key+"="+value)
		if !compliant {
			drift = append(drift, key+"="+value)
		}
	}
	observationErr := errors.Join(observationErrors...)
	if len(drift) == 0 {
		return nil, observationErr
	}
	return &finding{
		probeId: "host/edge-auto-upgrades", tier: tierWarn,
		class: "edge-auto-upgrades", target: target.name, sustain: 1,
		symptom:   fmt.Sprintf("%s does not match the disabled periodic APT and masked unit policy", target.name),
		mechanism: "An authoritative APT periodic setting or unit state differs from the required edge policy. Unmasked units may permit unattended maintenance, but these configuration results do not prove that APT ran or that networkd restarted. An absent or unreadable unit property remains unknown.",
		baseline:  "APT::Periodic::Enable is exactly 0 and apt-daily*, apt-daily-upgrade*, and unattended-upgrades.service are masked on every configured edge.",
		observed:  strings.Join(observed, " "),
		evidence:  "drift: " + strings.Join(drift, ", "),
		action:    "Disable both APT periodic execution layers and schedule OS/security updates in a controlled window with proxy return-path verification afterward.",
		verify:    "Rerun apt-config and systemctl show -p UnitFileState --value, require the documented disabled/masked shape, then repeat source-aware public protocol probes.",
		playbook:  "SIGNALS.md §14.5",
	}, observationErr
}

// Require the completion trailer before accepting any rows. Duplicates and
// missing keys invalidate only their own observation; unrelated known fields
// remain usable. Raw fields and diagnostics never enter an error or finding.
func parseProxyNativeFrame(out string, marker string, keys []string) (map[string]string, error) {
	values := map[string]string{}
	if len(out) > 4096 || !strings.HasSuffix(out, "\ncomplete="+marker+"\n") {
		return values, fmt.Errorf("invalid response: incomplete proxy native frame")
	}
	rows := strings.Split(strings.TrimSuffix(out, "complete="+marker+"\n"), "\n")
	allowed := map[string]bool{}
	for _, key := range keys {
		allowed[key] = true
	}
	seen := map[string]bool{}
	var frameErr error
	for _, row := range rows[:len(rows)-1] {
		key, value, ok := strings.Cut(row, "=")
		if !ok || !allowed[key] {
			frameErr = fmt.Errorf("invalid response: unexpected proxy native field")
			continue
		}
		if seen[key] {
			delete(values, key)
			frameErr = fmt.Errorf("invalid response: duplicate proxy native field")
			continue
		}
		seen[key] = true
		values[key] = value
	}
	return values, frameErr
}

// Decode only a bounded native exit code paired with one fixed-domain value.
func proxyNativeResult(values map[string]string, key string) (int, string, error) {
	status, value, ok := strings.Cut(values[key], "|")
	code, numeric := proxyNativeUint(status)
	if !ok || !numeric || code > 255 || value == "" || strings.ContainsAny(value, "|\r\n") {
		return 0, "", fmt.Errorf("invalid response: proxy native %s", key)
	}
	return int(code), value, nil
}

// Decimal syntax excludes signs, whitespace and overflow, including malformed
// counts that the old permissive atoi reduced to a measured zero.
func proxyNativeUint(value string) (uint64, bool) {
	if value == "" {
		return 0, false
	}
	for _, char := range value {
		if char < '0' || char > '9' {
			return 0, false
		}
	}
	number, err := strconv.ParseUint(value, 10, 64)
	return number, err == nil
}

func normalizedAddressFamilies(configured []string) ([]string, error) {
	if len(configured) == 0 {
		return []string{"ipv4", "ipv6"}, nil
	}
	families := []string{}
	seen := map[string]bool{}
	for _, value := range configured {
		family := strings.ToLower(strings.TrimSpace(value))
		switch family {
		case "4", "v4", "ipv4":
			family = "ipv4"
		case "6", "v6", "ipv6":
			family = "ipv6"
		default:
			return nil, fmt.Errorf("unsupported proxy address family %q", value)
		}
		if !seen[family] {
			seen[family] = true
			families = append(families, family)
		}
	}
	return families, nil
}

func familyEnabled(families []string, family string) bool {
	for _, candidate := range families {
		if candidate == family {
			return true
		}
	}
	return false
}

func parseKeyValueLines(out string) map[string]string {
	values := map[string]string{}
	for _, line := range strings.Split(out, "\n") {
		parts := strings.SplitN(strings.TrimSpace(line), "=", 2)
		if len(parts) == 2 && parts[0] != "" {
			values[parts[0]] = strings.TrimSpace(parts[1])
		}
	}
	return values
}

func shellSingleQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", `'"'"'`) + "'"
}
