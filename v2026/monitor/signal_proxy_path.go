package monitor

import (
	"context"
	"fmt"
	"net"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// SIGNALS.md §14.5 maps to signal_proxy_path.go and signal_proxy_path_test.go.
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
	for _, target := range env.cfg.hosts {
		if target.proxy == nil {
			continue
		}
		families, err := normalizedAddressFamilies(target.proxy.AddressFamilies)
		if err != nil {
			findings = append(findings, cannotObserveFinding(target.name+"/proxy-families", err))
			continue
		}

		if target.proxy.PublicHostname != "" {
			allocations, allocationErr := discoverProxyAllocations(ctx, env, target)
			if allocationErr != nil {
				findings = append(findings, cannotObserveFinding(target.name+"/proxy-allocations", allocationErr))
			} else {
				findings = append(findings, evaluateProxyHandshakes(ctx, env, target, families, allocations)...)
			}
		}

		if target.proxy.PublicInterface != "" && target.proxy.RoutingTable > 0 {
			routeFinding, routeErr := evaluateProxyRouteState(ctx, env, target, families)
			if routeErr != nil {
				findings = append(findings, cannotObserveFinding(target.name+"/policy-routing", routeErr))
			} else if routeFinding != nil {
				findings = append(findings, *routeFinding)
			}
		}

		upgradeFinding, upgradeErr := evaluateEdgeAutoUpgrades(ctx, env, target)
		if upgradeErr != nil {
			findings = append(findings, cannotObserveFinding(target.name+"/apt-policy", upgradeErr))
		} else if upgradeFinding != nil {
			findings = append(findings, *upgradeFinding)
		}
	}
	return findings, nil
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
docker ps --format '{{.Names}}' | while IFS= read -r name; do
  case "$name" in
    *-proxy-*) ;;
    *) continue ;;
  esac
  ports=$(docker inspect --format '{{range .Config.Env}}{{println .}}{{end}}' "$name" 2>/dev/null | sed -n 's/^WARP_PORTS=//p' | head -1)
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
		return []finding{{
			probeId: "proxy/public-path", tier: tierWarn,
			class: "proxy-public-handshake", target: target.name, frame: "allocations", sustain: 2,
			symptom:   fmt.Sprintf("no running %s proxy allocation could be discovered on %s", env.cfg.env, target.name),
			mechanism: "Without a live container WARP_PORTS mapping, the monitor cannot pair internal readiness with the current public protocol ports; the service may be absent or deployment state may be unreadable.",
			baseline:  "Every configured proxy host exposes at least one running allocation with service ports 80, 8080, 8081, and 8082 mapped to current host ports.",
			observed:  "allocations=0",
			action:    "Inspect running proxy containers and their WARP_PORTS environment, then repair the failed deployment or monitor permissions before testing public ingress.",
			verify:    "Rerun the signal and require a current allocation plus successful internal readiness and public protocol ownership for every block.",
			playbook:  "SIGNALS.md §14.5",
		}}
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

	findings := []finding{}
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

func evaluateProxyRouteState(ctx context.Context, env *probeEnv, target *host, families []string) (*finding, error) {
	unit := target.proxy.LoadBalancerUnit
	if unit == "" {
		unit = "warp-main-lb-" + target.proxy.PublicInterface + ".service"
	}
	table := target.proxy.RoutingTable
	command := fmt.Sprintf(`# %s
networkd_start=$(systemctl show systemd-networkd.service -p ActiveEnterTimestampMonotonic --value 2>/dev/null)
lb_start=$(systemctl show %s -p ActiveEnterTimestampMonotonic --value 2>/dev/null)
v4_routes=$(ip -4 route show table %d 2>/dev/null | sed '/^[[:space:]]*$/d' | wc -l)
v6_routes=$(ip -6 route show table %d 2>/dev/null | sed '/^[[:space:]]*$/d' | wc -l)
v4_rules=$(ip -4 rule show 2>/dev/null | grep -Ec 'lookup[[:space:]]+%d([[:space:]]|$)' || true)
v6_rules=$(ip -6 rule show 2>/dev/null | grep -Ec 'lookup[[:space:]]+%d([[:space:]]|$)' || true)
printf 'networkd_start=%%s\nlb_start=%%s\nv4_routes=%%s\nv6_routes=%%s\nv4_rules=%%s\nv6_rules=%%s\n' "$networkd_start" "$lb_start" "$v4_routes" "$v6_routes" "$v4_rules" "$v6_rules"`,
		proxyRouteMarker, shellSingleQuote(unit), table, table, table, table)
	out, err := env.runner.shell(ctx, target, command)
	if err != nil {
		return nil, err
	}
	values := parseKeyValueLines(out)
	networkdStart, networkdErr := strconv.ParseInt(values["networkd_start"], 10, 64)
	lbStart, lbErr := strconv.ParseInt(values["lb_start"], 10, 64)
	if networkdErr != nil || lbErr != nil || networkdStart <= 0 || lbStart <= 0 {
		return nil, fmt.Errorf("invalid networkd/LB clocks in %q", strings.TrimSpace(out))
	}
	if networkdStart <= lbStart {
		return nil, nil
	}

	missing := []string{}
	if atoi(values["v4_routes"]) == 0 && familyEnabled(families, "ipv4") {
		missing = append(missing, "IPv4 table routes")
	}
	if atoi(values["v4_rules"]) == 0 && familyEnabled(families, "ipv4") {
		missing = append(missing, "IPv4 source/fwmark rules")
	}
	if atoi(values["v6_routes"]) == 0 && familyEnabled(families, "ipv6") {
		missing = append(missing, "IPv6 table routes")
	}
	if atoi(values["v6_rules"]) == 0 && familyEnabled(families, "ipv6") {
		missing = append(missing, "IPv6 source/fwmark rules")
	}
	if len(missing) == 0 {
		return nil, nil
	}
	return &finding{
		probeId: "host/policy-route-drift", tier: tierWarn,
		class: "policy-route-drift", target: target.name, frame: target.proxy.PublicInterface, sustain: 1,
		symptom:   fmt.Sprintf("systemd-networkd restarted after %s's transparent LB and owned routing state is missing", target.name),
		mechanism: "A later networkd activation removed Warp's public source-policy routes/rules while the LB process remained up; public SYNs can arrive and SYN-ACKs leave through the LAN default route.",
		baseline:  fmt.Sprintf("routing table %d retains non-empty routes and source/fwmark lookup rules for every enabled family, or the transparent LB starts after networkd and reconciles them", table),
		observed:  fmt.Sprintf("networkd_start=%d lb_start=%d %s", networkdStart, lbStart, strings.TrimSpace(out)),
		evidence:  "missing: " + strings.Join(missing, ", "),
		action:    "Run source-aware route-get and packet-capture proofs, then restore the persistent Warp-owned table/rules; do not lower the public main-table metric as a workaround.",
		verify:    "Require both families' owned table/rules, correct source-aware route selection, and successful public SOCKS/HTTP/HTTPS protocol handshakes.",
		playbook:  "SIGNALS.md §14.5",
	}, nil
}

const edgeAutoUpgradeMarker = "monitor-signal-14.5-auto-upgrades"

var aptUnits = []string{
	"apt-daily.timer",
	"apt-daily-upgrade.timer",
	"apt-daily.service",
	"apt-daily-upgrade.service",
	"unattended-upgrades.service",
}

func evaluateEdgeAutoUpgrades(ctx context.Context, env *probeEnv, target *host) (*finding, error) {
	command := `# ` + edgeAutoUpgradeMarker + `
periodic=$(apt-config dump 2>/dev/null | awk -F'"' '$1 ~ /^APT::Periodic::Enable / {print $2; exit}')
printf 'periodic_enable=%s\n' "${periodic:-unset}"
for unit in apt-daily.timer apt-daily-upgrade.timer apt-daily.service apt-daily-upgrade.service unattended-upgrades.service; do
  state=$(systemctl is-enabled "$unit" 2>/dev/null || true)
  printf '%s=%s\n' "$unit" "${state:-unknown}"
done`
	out, err := env.runner.shell(ctx, target, command)
	if err != nil {
		return nil, err
	}
	values := parseKeyValueLines(out)
	drift := []string{}
	if values["periodic_enable"] != "0" {
		drift = append(drift, "APT::Periodic::Enable="+firstNonempty(values["periodic_enable"], "missing"))
	}
	for _, unit := range aptUnits {
		if !strings.HasPrefix(values[unit], "masked") {
			drift = append(drift, unit+"="+firstNonempty(values[unit], "missing"))
		}
	}
	if len(drift) == 0 {
		return nil, nil
	}
	return &finding{
		probeId: "host/edge-auto-upgrades", tier: tierWarn,
		class: "edge-auto-upgrades", target: target.name, sustain: 1,
		symptom:   fmt.Sprintf("%s can run unattended APT work that restarts network services outside a controlled maintenance window", target.name),
		mechanism: "APT periodic configuration or systemd masks drifted, allowing apt-daily/unattended-upgrades to restart networkd after Warp installed transparent-LB policy routing.",
		baseline:  "APT::Periodic::Enable is exactly 0 and apt-daily*, apt-daily-upgrade*, and unattended-upgrades.service are masked on every configured edge.",
		observed:  strings.TrimSpace(out),
		evidence:  "drift: " + strings.Join(drift, ", "),
		action:    "Disable both APT periodic execution layers and schedule OS/security updates in a controlled window with proxy return-path verification afterward.",
		verify:    "Rerun apt-config and systemctl is-enabled, require the documented disabled/masked shape, then repeat source-aware public protocol probes.",
		playbook:  "SIGNALS.md §14.5",
	}, nil
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
