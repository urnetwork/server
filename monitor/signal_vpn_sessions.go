package monitor

import (
	"context"
	"fmt"
	"net"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	vpnSessionsMarker = "monitor-signal-21.1-vpn-sessions"
	// OpenVPN's status directive has a 60-second default update interval when
	// no explicit frequency is configured. Allow one interval plus 30 seconds
	// of scheduler/write jitter before declaring the snapshot unobservable.
	vpnStatusFreshness         = 90 * time.Second
	vpnStatusFutureTolerance   = 30 * time.Second
	vpnSessionCorrelationLimit = 2 * time.Hour
)

var vpnSessionNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]*$`)

// Signal vpn-sessions implements SIGNALS.md §21.1. It compares the VPN
// server's current status with the explicitly enabled client inventory and
// groups recent losses by equal public source without exporting that address.
func NewVPNSessionsSignal() Signal {
	return &signalAdapter{
		number: "21.1", key: "vpn-sessions", name: "Management VPN client sessions",
		probe: vpnSessionsProbe{},
	}
}

type vpnSessionsProbe struct{}

func (vpnSessionsProbe) id() string             { return "network/vpn-sessions" }
func (vpnSessionsProbe) tier() string           { return tierWarn }
func (vpnSessionsProbe) cadence() time.Duration { return time.Minute }

type vpnSessionTimeout struct {
	client string
	at     time.Time
	group  int
}

type vpnSessionClient struct {
	connectedAt time.Time
	group       int
}

type vpnSessionsObservation struct {
	activeState string
	subState    string
	restarts    int64
	statusMTime time.Time
	clients     map[string]vpnSessionClient
	reachable   map[string]bool
	timeouts    map[string]vpnSessionTimeout
}

func (vpnSessionsProbe) check(ctx context.Context, env *probeEnv) ([]finding, error) {
	servers := env.cfg.hostsWithRole("vpn-server")
	clients := env.cfg.hostsWithRole("vpn-client")
	if len(servers) == 0 && len(clients) == 0 {
		return nil, nil
	}
	if len(servers) != 1 {
		return nil, fmt.Errorf("vpn sessions: expected exactly one vpn-server host, got %d", len(servers))
	}
	if len(clients) == 0 {
		return nil, fmt.Errorf("vpn sessions: vpn-server is configured without vpn-client hosts")
	}
	clients = append([]*host(nil), clients...)
	sort.Slice(clients, func(i, j int) bool { return clients[i].name < clients[j].name })

	command, err := vpnSessionsCommand(clients)
	if err != nil {
		return nil, err
	}
	server := servers[0]
	output, err := env.runner.shell(ctx, server, command)
	if err != nil {
		return nil, fmt.Errorf("vpn sessions: inspect %s: %w", server.name, err)
	}
	observation, err := parseVPNSessionsObservation(output)
	if err != nil {
		return nil, fmt.Errorf("vpn sessions: parse %s: %w", server.name, err)
	}

	now := env.now().UTC()
	if observation.activeState != "active" || observation.subState != "running" {
		return []finding{{
			probeId: "network/vpn-sessions", tier: tierPage,
			class: "vpn-server-unhealthy", target: server.name, sustain: 1,
			symptom:   fmt.Sprintf("management VPN server %s is %s/%s", server.name, observation.activeState, observation.subState),
			mechanism: "The OpenVPN server unit does not own a running process. Client-session absence is therefore a server-side control-plane outage, not evidence that every remote site failed independently.",
			baseline:  "The configured OpenVPN server unit remains active/running and publishes a status file within its 60-second configured/default interval plus 30 seconds of tolerance.",
			observed:  fmt.Sprintf("active_state=%s sub_state=%s nrestarts=%d", observation.activeState, observation.subState, observation.restarts),
			evidence:  "The probe reads only systemd state and the redacted OpenVPN status contract on the configured VPN server; it does not export client public addresses, certificates, or keys.",
			context:   "This management path carries control traffic. PostgreSQL and Redis backup payloads must continue using their dedicated direct SSH forwards and cannot be repaired by moving bulk traffic onto the VPN.",
			action:    "Inspect the VPN server process, host checks, UDP/443 listener, journal, and current EC2/network boundary. Restore the existing service architecture; do not restart remote databases or reroute bulk backups merely because their control sessions disappeared.",
			verify:    "The same server generation is active/running, its status mtime remains within 90 seconds, and every configured client virtual address is present and reachable for two consecutive one-minute cadences.",
			playbook:  "SIGNALS.md §21.1",
		}}, nil
	}

	statusAge := now.Sub(observation.statusMTime)
	if statusAge > vpnStatusFreshness || statusAge < -vpnStatusFutureTolerance {
		return []finding{{
			probeId: "network/vpn-sessions", tier: tierWarn,
			class: "vpn-status-stale", target: server.name, sustain: 2,
			symptom:   fmt.Sprintf("management VPN status on %s is not fresh", server.name),
			mechanism: "The server process may still be running, but its configured status writer is absent, stale, or timestamped in the future. Without a fresh snapshot, missing rows cannot be interpreted as disconnected clients.",
			baseline:  "The OpenVPN status file is no more than 90 seconds old: its 60-second configured/default interval plus 30 seconds of scheduler/write tolerance.",
			observed:  fmt.Sprintf("status_mtime=%s status_age_s=%.0f active_state=%s sub_state=%s", observation.statusMTime.Format(time.RFC3339), statusAge.Seconds(), observation.activeState, observation.subState),
			evidence:  "The mtime is read on the VPN server beside the status file; no client endpoint or credential is retained.",
			context:   "A stale status snapshot is UNKNOWN, not proof that the clients listed in the old file remain connected.",
			action:    "Inspect the OpenVPN status directive, file ownership, disk state, and server journal. Restore fresh status publication before assigning a remote-site outage.",
			verify:    "Two consecutive one-minute probes read a status mtime no more than 90 seconds old and then evaluate every configured client session and overlay path.",
			playbook:  "SIGNALS.md §21.1",
		}}, nil
	}

	missing := make([]*host, 0)
	unreachable := make([]*host, 0)
	reachableClients := 0
	for _, client := range clients {
		_, present := observation.clients[client.overlayIp]
		reachable, reachabilityObserved := observation.reachable[client.overlayIp]
		if !reachabilityObserved {
			return nil, fmt.Errorf("vpn sessions: parse %s: missing reachability for %s", server.name, client.name)
		}
		if !present {
			missing = append(missing, client)
			continue
		}
		if reachable {
			reachableClients++
		} else {
			unreachable = append(unreachable, client)
		}
	}

	findings := make([]finding, 0, len(missing)+len(unreachable))
	unreachableByGroup := map[int][]string{}
	for _, client := range unreachable {
		group := observation.clients[client.overlayIp].group
		if group > 0 {
			unreachableByGroup[group] = append(unreachableByGroup[group], client.name)
		}
	}
	for group := range unreachableByGroup {
		sort.Strings(unreachableByGroup[group])
	}
	for _, client := range unreachable {
		session := observation.clients[client.overlayIp]
		peers := unreachableByGroup[session.group]
		sharedSite := len(peers) >= 2 && reachableClients > 0
		class := "vpn-client-data-path-loss"
		tier := tierWarn
		frame := "session-present-overlay-unreachable"
		mechanism := "The VPN server has a current session for this exact virtual address but cannot reach it over the tunnel. The control plane is present while the client host or its tunnel data path is not forwarding packets."
		if sharedSite {
			class = "vpn-site-data-path-loss"
			tier = tierPage
			frame = "shared-public-source-data-path"
			mechanism = "The VPN server has current sessions for multiple clients sharing one public source, can reach unrelated configured clients, and cannot reach these peers over their virtual addresses. That isolates a session-present data-plane blackhole at the shared offsite client/router/NAT/WAN path rather than the central process or the monitor workstation."
		}
		peerText := "none"
		if len(peers) > 0 {
			peerText = strings.Join(peers, ",")
		}
		findings = append(findings, finding{
			probeId: "network/vpn-sessions", tier: tier, class: class,
			target: client.name, frame: frame, sustain: 2,
			symptom:   fmt.Sprintf("management VPN session for %s is present but its overlay data path is unreachable from %s", client.name, server.name),
			mechanism: mechanism,
			baseline:  "Every enabled vpn-client has its exact overlay address in the fresh server snapshot and answers a bounded server-originated overlay reachability check.",
			observed: fmt.Sprintf(
				"host=%s overlay_address=%s session_present=true data_path_reachable=false reachable_controls=%d configured_clients=%d shared_public_source=%t correlated_unreachable_hosts=%s connected_since=%s server_restarts=%d",
				client.name, client.overlayIp, reachableClients, len(clients), sharedSite, peerText, session.connectedAt.Format(time.RFC3339), observation.restarts,
			),
			evidence: "The current status snapshot groups real-source equality only inside the VPN server and exports configured private overlay addresses plus configured host names; it never emits public sources, source ports, certificates, or unrelated identities. The server then probes each configured overlay address directly.",
			context:  "A CLIENT_LIST row proves a control session, not usable forwarding. Correlate the dedicated direct public backup path: if it also disappears, the failure is broader than OpenVPN; if it advances, keep the diagnosis at the tunnel data path. Bulk backups must never move onto the management VPN.",
			action:   "Inspect the affected host and site router through an independent console: tunnel counters, address/route ownership, rp_filter/firewall state, WAN/link history, NAT/conntrack, UDP/443 reachability, and openvpn@by-pre journal. Preserve advancing Subtensor databases and the single Planetoid backup writer; do not restart databases, launch a duplicate transfer, or redesign the central VPN.",
			verify:   "The current session remains present and its exact overlay address answers two consecutive server-originated checks; all same-source configured peers recover; the dedicated non-VPN backup path advances when scheduled; and dependent host probes remain observable for ten minutes.",
			playbook: "SIGNALS.md §21.1, §17.1, and §11.22",
		})
	}

	missingByName := make(map[string]*host, len(missing))
	for _, client := range missing {
		missingByName[client.name] = client
	}
	correlated := map[string][]string{}
	for _, timeout := range observation.timeouts {
		if timeout.group <= 0 || now.Sub(timeout.at) < 0 || now.Sub(timeout.at) > vpnSessionCorrelationLimit {
			continue
		}
		if _, isMissing := missingByName[timeout.client]; !isMissing {
			continue
		}
		key := strconv.Itoa(timeout.group)
		correlated[key] = append(correlated[key], timeout.client)
	}
	for key := range correlated {
		sort.Strings(correlated[key])
	}

	for _, client := range missing {
		timeout := observation.timeouts[client.name]
		peers := []string{}
		if timeout.group > 0 {
			peers = append(peers, correlated[strconv.Itoa(timeout.group)]...)
		}
		sharedSite := len(peers) >= 2
		class := "vpn-client-session-loss"
		tier := tierWarn
		frame := "isolated-or-unknown-source"
		if sharedSite {
			class = "vpn-site-session-loss"
			tier = tierPage
			frame = "shared-public-source"
		}
		lastTimeout := "not-seen-in-two-hour-window"
		if !timeout.at.IsZero() {
			lastTimeout = timeout.at.Format(time.RFC3339)
		}
		peerText := "none"
		if len(peers) > 0 {
			peerText = strings.Join(peers, ",")
		}
		mechanism := "The VPN server and its fresh status writer are healthy, but the configured virtual address is absent. That localizes the management failure to this client, its host, or the route/NAT path between it and the server."
		if sharedSite {
			mechanism = "The VPN server and unrelated client sessions remain healthy while multiple missing clients' latest inactivity timeouts share one public source. That equality localizes the common boundary to their offsite LAN, router/NAT, WAN, or site-side OpenVPN reachability rather than the central VPN process or each application independently."
		}
		findings = append(findings, finding{
			probeId: "network/vpn-sessions", tier: tier, class: class,
			target: client.name, frame: frame, sustain: 2,
			symptom:   fmt.Sprintf("management VPN session for %s is absent from %s", client.name, server.name),
			mechanism: mechanism,
			baseline:  "Every enabled vpn-client host has its exact configured overlay address in the VPN server's fresh CLIENT_LIST snapshot.",
			observed: fmt.Sprintf(
				"missing_host=%s overlay_address=%s missing_clients=%d configured_clients=%d last_inactivity_timeout=%s shared_public_source=%t correlated_missing_hosts=%s server_restarts=%d",
				client.name, client.overlayIp, len(missing), len(clients), lastTimeout, sharedSite, peerText, observation.restarts,
			),
			evidence: "The server-side status file supplies only configured virtual addresses. A bounded two-hour journal reduction compares source-address equality inside the VPN host and exports only the names of configured peers in the same group; the public source itself is never emitted.",
			context:  "This proves management-session loss, not application death. Correlate a dedicated direct-path control: if Planetoid's public PostgreSQL/Redis SSH transfer also disappears, the fault is broader than OpenVPN; if that transfer advances, keep the diagnosis at the UDP/VPN path. Bulk backups must never move onto the management VPN.",
			action:   "Inspect the affected host and site router through an independent console: WAN/link history, address selection, NAT/conntrack state, UDP/443 reachability, and openvpn@by-pre state/journal. Preserve advancing Subtensor databases and the single Planetoid backup writer. Restore the site path or client session; do not restart databases, launch a duplicate transfer, or change the central VPN architecture to manufacture recovery.",
			verify:   "The exact client virtual address remains in two consecutive fresh server snapshots, all same-site configured peers recover, direct public backup traffic uses its dedicated non-VPN endpoint and advances when scheduled, and dependent host probes remain observable for ten minutes.",
			playbook: "SIGNALS.md §21.1, §17.1, and §11.22",
		})
	}
	return findings, nil
}

func vpnSessionsCommand(clients []*host) (string, error) {
	names := make([]string, 0, len(clients))
	addresses := make([]string, 0, len(clients))
	seenNames := map[string]bool{}
	seenAddresses := map[string]bool{}
	for _, client := range clients {
		if client == nil || !vpnSessionNamePattern.MatchString(client.name) {
			return "", fmt.Errorf("vpn sessions: invalid client name %q", clientName(client))
		}
		address := strings.TrimSpace(client.overlayIp)
		parsed := net.ParseIP(address)
		if parsed == nil || parsed.To4() == nil {
			return "", fmt.Errorf("vpn sessions: %s has invalid overlay IPv4 address %q", client.name, address)
		}
		if seenNames[client.name] {
			return "", fmt.Errorf("vpn sessions: duplicate client name %q", client.name)
		}
		if seenAddresses[address] {
			return "", fmt.Errorf("vpn sessions: duplicate overlay address %q", address)
		}
		seenNames[client.name] = true
		seenAddresses[address] = true
		names = append(names, client.name)
		addresses = append(addresses, address)
	}
	return fmt.Sprintf(`# %s
set -eu
vpn_service=openvpn-server@server.service
vpn_status=/var/log/openvpn/openvpn-status.log
expected_names='%s'
expected_addresses='%s'
active_state=$(systemctl show "$vpn_service" -p ActiveState --value 2>/dev/null || true)
sub_state=$(systemctl show "$vpn_service" -p SubState --value 2>/dev/null || true)
restarts=$(systemctl show "$vpn_service" -p NRestarts --value 2>/dev/null || true)
case "$active_state" in '') active_state=unknown ;; esac
case "$sub_state" in '') sub_state=unknown ;; esac
case "$restarts" in ''|*[!0-9]*) restarts=0 ;; esac
printf 'server_active_state %%s\n' "$active_state"
printf 'server_sub_state %%s\n' "$sub_state"
printf 'server_restarts %%s\n' "$restarts"
if ! sudo -n test -r "$vpn_status"; then
  printf 'status_mtime_epoch 0\n'
  exit 0
fi
status_mtime=$(sudo -n stat -c '%%Y' "$vpn_status" 2>/dev/null || true)
case "$status_mtime" in ''|*[!0-9]*) status_mtime=0 ;; esac
printf 'status_mtime_epoch %%s\n' "$status_mtime"
sudo -n awk -F, -v wanted="$expected_addresses" '
  BEGIN {
    count=split(wanted, values, " ")
    for (i=1; i<=count; i++) expected[values[i]]=1
  }
  $1=="CLIENT_LIST" && expected[$4] {
	 source=$3
	 sub(/:[0-9]+$/, "", source)
	 if (!(source in source_group)) source_group[source]=++source_groups
    connected=$9
    if (connected !~ /^[0-9]+$/) connected=0
	 print "client", $4, connected, source_group[source]
  }
' "$vpn_status"
for address in $expected_addresses; do
	if ping -n -c 1 -W 1 "$address" >/dev/null 2>&1; then
		printf 'reach %%s true\n' "$address"
	else
		printf 'reach %%s false\n' "$address"
	fi
done
sudo -n journalctl -u "$vpn_service" --since "2 hours ago" --no-pager -o short-unix 2>/dev/null |
awk -v wanted="$expected_names" '
  BEGIN {
    count=split(wanted, values, " ")
    for (i=1; i<=count; i++) expected[values[i]]=1
  }
  /Inactivity timeout \(--ping-restart\)/ {
    token=$4
    parts=split(token, pair, "/")
    if (parts != 2) next
    client=pair[1]
    if (!expected[client]) next
    source=pair[2]
    sub(/:[0-9]+$/, "", source)
    if (source=="") next
    if (!(source in source_group)) source_group[source]=++source_groups
    epoch=$1
    sub(/[.][0-9]+$/, "", epoch)
    if (epoch !~ /^[0-9]+$/) next
    print "timeout", client, epoch, source_group[source]
  }
'`, vpnSessionsMarker, strings.Join(names, " "), strings.Join(addresses, " ")), nil
}

func clientName(client *host) string {
	if client == nil {
		return ""
	}
	return client.name
}

func parseVPNSessionsObservation(output string) (vpnSessionsObservation, error) {
	observation := vpnSessionsObservation{
		clients:   map[string]vpnSessionClient{},
		reachable: map[string]bool{},
		timeouts:  map[string]vpnSessionTimeout{},
	}
	seen := map[string]bool{}
	for lineNumber, line := range strings.Split(output, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		switch fields[0] {
		case "server_active_state", "server_sub_state", "server_restarts", "status_mtime_epoch":
			if len(fields) != 2 {
				return vpnSessionsObservation{}, fmt.Errorf("line %d: invalid %s field count", lineNumber+1, fields[0])
			}
			if seen[fields[0]] {
				return vpnSessionsObservation{}, fmt.Errorf("line %d: duplicate %s", lineNumber+1, fields[0])
			}
			seen[fields[0]] = true
			switch fields[0] {
			case "server_active_state":
				observation.activeState = fields[1]
			case "server_sub_state":
				observation.subState = fields[1]
			case "server_restarts":
				value, err := strconv.ParseInt(fields[1], 10, 64)
				if err != nil || value < 0 {
					return vpnSessionsObservation{}, fmt.Errorf("line %d: invalid server_restarts", lineNumber+1)
				}
				observation.restarts = value
			case "status_mtime_epoch":
				value, err := strconv.ParseInt(fields[1], 10, 64)
				if err != nil || value < 0 {
					return vpnSessionsObservation{}, fmt.Errorf("line %d: invalid status_mtime_epoch", lineNumber+1)
				}
				observation.statusMTime = time.Unix(value, 0).UTC()
			}
		case "client":
			if len(fields) != 4 || net.ParseIP(fields[1]) == nil {
				return vpnSessionsObservation{}, fmt.Errorf("line %d: invalid client row", lineNumber+1)
			}
			if _, duplicate := observation.clients[fields[1]]; duplicate {
				return vpnSessionsObservation{}, fmt.Errorf("line %d: duplicate client address", lineNumber+1)
			}
			epoch, err := strconv.ParseInt(fields[2], 10, 64)
			group, groupErr := strconv.Atoi(fields[3])
			if err != nil || epoch <= 0 || groupErr != nil || group <= 0 {
				return vpnSessionsObservation{}, fmt.Errorf("line %d: invalid client connection time", lineNumber+1)
			}
			observation.clients[fields[1]] = vpnSessionClient{connectedAt: time.Unix(epoch, 0).UTC(), group: group}
		case "reach":
			if len(fields) != 3 || net.ParseIP(fields[1]) == nil || (fields[2] != "true" && fields[2] != "false") {
				return vpnSessionsObservation{}, fmt.Errorf("line %d: invalid reachability row", lineNumber+1)
			}
			if _, duplicate := observation.reachable[fields[1]]; duplicate {
				return vpnSessionsObservation{}, fmt.Errorf("line %d: duplicate reachability address", lineNumber+1)
			}
			observation.reachable[fields[1]] = fields[2] == "true"
		case "timeout":
			if len(fields) != 4 || !vpnSessionNamePattern.MatchString(fields[1]) {
				return vpnSessionsObservation{}, fmt.Errorf("line %d: invalid timeout row", lineNumber+1)
			}
			epoch, epochErr := strconv.ParseInt(fields[2], 10, 64)
			group, groupErr := strconv.Atoi(fields[3])
			if epochErr != nil || epoch <= 0 || groupErr != nil || group <= 0 {
				return vpnSessionsObservation{}, fmt.Errorf("line %d: invalid timeout values", lineNumber+1)
			}
			timeout := vpnSessionTimeout{client: fields[1], at: time.Unix(epoch, 0).UTC(), group: group}
			if previous, ok := observation.timeouts[timeout.client]; !ok || timeout.at.After(previous.at) {
				observation.timeouts[timeout.client] = timeout
			}
		default:
			return vpnSessionsObservation{}, fmt.Errorf("line %d: unknown field %q", lineNumber+1, fields[0])
		}
	}
	for _, required := range []string{"server_active_state", "server_sub_state", "server_restarts", "status_mtime_epoch"} {
		if !seen[required] {
			return vpnSessionsObservation{}, fmt.Errorf("missing %s", required)
		}
	}
	return observation, nil
}
