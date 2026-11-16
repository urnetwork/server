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
	// Unknown/new service states remain unavailable until their semantics are
	// reviewed. These fixed tokens are also the generated shell's allowlist.
	vpnSessionActiveStates = "active|reloading|inactive|failed|activating|deactivating|maintenance|refreshing"
	vpnSessionSubStates    = "dead|condition|start-pre|start|start-post|running|exited|reload|reload-signal|reload-notify|stop|stop-watchdog|stop-sigterm|stop-sigkill|stop-post|final-watchdog|final-sigterm|final-sigkill|failed|auto-restart|auto-restart-queued|cleaning|dead-before-auto-restart|failed-before-auto-restart|dead-resources-pinned"
)

var vpnSessionNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]*$`)

// Signal vpn-sessions implements SIGNALS.md §21.1. It compares the VPN
// server's current status with the explicitly enabled client inventory and
// groups current or recent losses by equal public source without exporting
// that address.
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
	unitObserved    bool
	activeState     string
	subState        string
	restarts        string
	statusMTime     time.Time
	statusObserved  bool
	journalObserved bool
	clients         map[string]vpnSessionClient
	reachable       map[string]string
	timeouts        map[string]vpnSessionTimeout
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
	configuredClientCount := len(clients)
	if scoped, ok := env.runner.(*hostScopeRunner); ok {
		permittedClients := make([]*host, 0, len(clients))
		for _, client := range clients {
			// The SSH server owns the command, but each nested ICMP target
			// needs its own admission. A shared endpoint cannot establish
			// that the permitted logical client owns the destination.
			if err := vpnSessionScopeClient(ctx, scoped, client); err != nil {
				if hostScopeOnlyError(err) {
					continue
				}
				return nil, err
			}
			permittedClients = append(permittedClients, client)
		}
		// Use the same admitted set for shell destinations and attribution;
		// cfg and the desired client denominator remain complete.
		clients = permittedClients
	}

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
	if !observation.unitObserved {
		return []finding{{
			probeId: "network/vpn-sessions", tier: tierWarn,
			class: "vpn-server-unavailable", target: server.name, sustain: 1,
			symptom:   "Management VPN server unit inspection is unavailable",
			mechanism: "Both systemctl state reads did not establish successful, recognized unit-state evidence. Inspection failure or missing provenance does not prove a stopped server, disconnected clients, or a remote-site outage.",
			baseline:  "Both bounded systemctl state reads succeed and return one recognized ActiveState/SubState value before unit or client health is evaluated.",
			observed:  fmt.Sprintf("unit_state_observed=false active_state=%s sub_state=%s nrestarts=%s", observation.activeState, observation.subState, observation.restarts),
			evidence:  "Only fixed unit-state tokens and optional restart-count availability are retained; raw command output and errors are not emitted.",
			action:    "Inspect the existing read-only unit-inspection path and permissions. Obtain authoritative unit state before assigning a VPN or site outage; preserve service and database generations.",
			verify:    "The next ordinary observation qualifies both unit-state reads and then evaluates the unchanged status freshness and admitted client paths.",
			playbook:  "SIGNALS.md §21.1",
		}}, nil
	}
	if observation.activeState != "active" || observation.subState != "running" {
		return []finding{{
			probeId: "network/vpn-sessions", tier: tierPage,
			class: "vpn-server-unhealthy", target: server.name, sustain: 1,
			symptom:   fmt.Sprintf("management VPN server %s is %s/%s", server.name, observation.activeState, observation.subState),
			mechanism: "Authoritative systemd state differs from the configured active/running baseline. This includes failed, stopped and transitional states; inspect the exact process before deciding whether it is absent or which control-plane operation failed. Client-session absence cannot independently identify every remote site as failed.",
			baseline:  "The configured OpenVPN server unit remains active/running and publishes a status file within its 60-second configured/default interval plus 30 seconds of tolerance.",
			observed:  fmt.Sprintf("active_state=%s sub_state=%s nrestarts=%s", observation.activeState, observation.subState, observation.restarts),
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

	if !observation.statusObserved {
		return []finding{vpnSessionsProducerUnavailable(server.name, "status")}, nil
	}

	missing := make([]*host, 0)
	unreachable := make([]*host, 0)
	findings := make([]finding, 0, len(clients)*2)
	reachableClients := 0
	reachableByGroup := map[int]int{}
	for _, client := range clients {
		_, present := observation.clients[client.overlayIp]
		reachable, reachabilityObserved := observation.reachable[client.overlayIp]
		if !reachabilityObserved {
			return nil, fmt.Errorf("vpn sessions: parse %s: missing reachability for %s", server.name, client.name)
		}
		// Execution uncertainty cannot certify a failed path or a control,
		// but complete status can independently prove missing membership.
		if reachable == "unknown" {
			findings = append(findings, cannotObserveFinding(client.name, fmt.Errorf("vpn sessions: bounded ICMP execution unavailable")))
		}
		if !present {
			missing = append(missing, client)
			continue
		}
		if reachable == "unknown" {
			continue
		}
		if reachable == "true" {
			reachableClients++
			reachableByGroup[observation.clients[client.overlayIp].group]++
		} else {
			unreachable = append(unreachable, client)
		}
	}

	if !observation.journalObserved {
		findings = append(findings, vpnSessionsProducerUnavailable(server.name, "journal"))
	}
	affectedByGroup := map[int][]string{}
	for _, client := range unreachable {
		group := observation.clients[client.overlayIp].group
		if group > 0 {
			affectedByGroup[group] = append(affectedByGroup[group], client.name)
		}
	}
	for _, client := range missing {
		timeout := observation.timeouts[client.name]
		if recentVPNSessionTimeout(now, timeout) {
			affectedByGroup[timeout.group] = append(affectedByGroup[timeout.group], client.name)
		}
	}
	for group := range affectedByGroup {
		sort.Strings(affectedByGroup[group])
	}
	controlWitness := func(group int) string {
		if reachableClients > reachableByGroup[group] {
			return " A reachable configured control has a different observed source group; source separation is established without identifying a failed device."
		}
		return " No reachable control in a different source group is established."
	}
	for _, client := range unreachable {
		session := observation.clients[client.overlayIp]
		peers := affectedByGroup[session.group]
		sharedSite := len(peers) >= 2 && reachableClients > 0
		class := "vpn-client-data-path-loss"
		tier := tierWarn
		frame := "session-present-overlay-unreachable"
		mechanism := "The VPN server has a current session for this exact virtual address but cannot reach it over the tunnel. The control plane is present while the client host or its tunnel data path is not forwarding packets."
		if sharedSite {
			class = "vpn-site-data-path-loss"
			tier = tierPage
			frame = "shared-public-source-data-path"
			mechanism = "This current-but-unreachable session and at least one other missing or unreachable configured client map to one current/recent public source, while at least one configured overlay control remains reachable. That is shared-source loss correlation with a partial central-path control, not by itself proof of an unrelated-source witness or a failed offsite LAN, router/NAT, WAN, or site-side VPN component." + controlWitness(session.group)
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
				"host=%s overlay_address=%s session_present=true data_path_reachable=false reachable_controls=%d unrelated_reachable_controls=%d configured_clients=%d shared_public_source=%t correlated_affected_hosts=%s connected_since=%s server_restarts=%s",
				client.name, client.overlayIp, reachableClients, reachableClients-reachableByGroup[session.group], configuredClientCount, sharedSite, peerText, session.connectedAt.Format(time.RFC3339), observation.restarts,
			),
			evidence: "The current status snapshot groups real-source equality only inside the VPN server and exports configured private overlay addresses plus configured host names; it never emits public sources, source ports, certificates, or unrelated identities. The server then probes each admitted configured overlay address directly.",
			context:  "A CLIENT_LIST row proves a control session, not usable forwarding. Correlate the dedicated direct public backup path: if it also disappears, the failure is broader than OpenVPN; if it advances, keep the diagnosis at the tunnel data path. Bulk backups must never move onto the management VPN.",
			action:   "Inspect the affected host and site router through an independent console: tunnel counters, address/route ownership, rp_filter/firewall state, WAN/link history, NAT/conntrack, UDP/443 reachability, and openvpn@by-pre journal. Preserve advancing Subtensor databases and the single Planetoid backup writer; do not restart databases, launch a duplicate transfer, or redesign the central VPN.",
			verify:   "The current session remains present and its exact overlay address answers two consecutive server-originated checks; all same-source configured peers recover; the dedicated non-VPN backup path advances when scheduled; and dependent host probes remain observable for ten minutes.",
			playbook: "SIGNALS.md §21.1, §17.1, and §11.22",
		})
	}

	for _, client := range missing {
		timeout := observation.timeouts[client.name]
		peers := []string{}
		if recentVPNSessionTimeout(now, timeout) {
			peers = append(peers, affectedByGroup[timeout.group]...)
		}
		sharedSite := len(peers) >= 2 && reachableClients > 0
		class := "vpn-client-session-loss"
		tier := tierWarn
		frame := "isolated-or-unknown-source"
		if sharedSite {
			class = "vpn-site-session-loss"
			tier = tierPage
			frame = "shared-public-source"
		}
		unrelatedControls := "unknown"
		if recentVPNSessionTimeout(now, timeout) {
			unrelatedControls = strconv.Itoa(reachableClients - reachableByGroup[timeout.group])
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
			mechanism = "This missing client and at least one other missing or unreachable configured client map to one current/recent public source while at least one configured overlay control remains reachable. That is shared-source loss correlation with a partial central-path control, not by itself proof of an unrelated-source witness or a failed offsite LAN, router/NAT, WAN, or site-side VPN component." + controlWitness(timeout.group)
		}
		findings = append(findings, finding{
			probeId: "network/vpn-sessions", tier: tier, class: class,
			target: client.name, frame: frame, sustain: 2,
			symptom:   fmt.Sprintf("management VPN session for %s is absent from %s", client.name, server.name),
			mechanism: mechanism,
			baseline:  "Every enabled vpn-client host has its exact configured overlay address in the VPN server's fresh CLIENT_LIST snapshot.",
			observed: fmt.Sprintf(
				"missing_host=%s overlay_address=%s missing_clients=%d configured_clients=%d last_inactivity_timeout=%s shared_public_source=%t correlated_affected_hosts=%s reachable_controls=%d unrelated_reachable_controls=%s server_restarts=%s",
				client.name, client.overlayIp, len(missing), configuredClientCount, lastTimeout, sharedSite, peerText, reachableClients, unrelatedControls, observation.restarts,
			),
			evidence: "The current status and bounded two-hour journal are reduced through one source-address equality map inside the VPN host. Only configured virtual addresses and names in the same group are exported; the public source itself is never emitted.",
			context:  "This proves management-session loss, not application death. Correlate a dedicated direct-path control: if Planetoid's public PostgreSQL/Redis SSH transfer also disappears, the fault is broader than OpenVPN; if that transfer advances, keep the diagnosis at the UDP/VPN path. Bulk backups must never move onto the management VPN.",
			action:   "Inspect the affected host and site router through an independent console: WAN/link history, address selection, NAT/conntrack state, UDP/443 reachability, and openvpn@by-pre state/journal. Preserve advancing Subtensor databases and the single Planetoid backup writer. Restore the site path or client session; do not restart databases, launch a duplicate transfer, or change the central VPN architecture to manufacture recovery.",
			verify:   "The exact client virtual address remains in two consecutive fresh server snapshots, all same-site configured peers recover, direct public backup traffic uses its dedicated non-VPN endpoint and advances when scheduled, and dependent host probes remain observable for ten minutes.",
			playbook: "SIGNALS.md §21.1, §17.1, and §11.22",
		})
	}
	return findings, nil
}

// Capture completion is independent from fresh metadata, unit state and ICMP.
func vpnSessionsProducerUnavailable(serverName, producer string) finding {
	class := "vpn-status-unavailable"
	symptom := "Management VPN status census is unavailable"
	mechanism := "The status-file producer did not complete successfully. Fresh mtime or a valid prefix cannot prove a complete client census; missing rows and session health remain unknown."
	if producer == "journal" {
		class = "vpn-session-history-unavailable"
		symptom = "Management VPN timeout history is unavailable"
		mechanism = "The bounded journal producer did not complete successfully. Partial timeout rows cannot establish recent source-group attribution. Independently complete current sessions and their admitted path checks retain their findings."
	}
	return finding{
		probeId: "network/vpn-sessions", tier: tierWarn, class: class, target: serverName, sustain: 1,
		symptom: symptom, mechanism: mechanism,
		baseline: "The selected producer exits successfully and its terminal completion marker reaches the redacted reducer before its evidence supplies absence, attribution or health.",
		observed: "producer=" + producer + " capture_complete=false",
		evidence: "Only fixed producer-completion flags are exported; status and journal bodies, command errors, public sources and unrelated identities remain private.",
		action:   "Inspect the existing read-only producer and status publication path. Preserve authoritative unit/current-session findings and the intended management VPN route; keep bulk backup payloads on their dedicated direct SSH path.",
		verify:   "The next ordinary read completes the selected producer. Then require the existing fresh-session, admitted path and recovery windows; unavailable history is not healthy absence.",
		playbook: "SIGNALS.md §21.1",
	}
}

func vpnSessionScopeClient(ctx context.Context, scoped *hostScopeRunner, client *host) error {
	if err := scoped.guardHost(ctx, client); err != nil {
		return err
	}
	// Admit exactly the trimmed destination that vpnSessionsCommand emits.
	destination := normalizeHostScopeEndpoint(strings.TrimSpace(client.overlayIp))
	if err := scoped.guardEndpoint(ctx, destination); err != nil {
		return err
	}
	for _, owner := range scoped.cfg.hosts {
		if normalizeHostScopeEndpoint(strings.TrimSpace(owner.overlayIp)) == destination {
			if err := scoped.guardHost(ctx, owner); err != nil {
				return err
			}
		}
	}
	return nil
}

func recentVPNSessionTimeout(now time.Time, timeout vpnSessionTimeout) bool {
	age := now.Sub(timeout.at)
	return timeout.group > 0 && !timeout.at.IsZero() && age >= 0 && age <= vpnSessionCorrelationLimit
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
unit_observed=true
if active_state=$(systemctl show "$vpn_service" -p ActiveState --value 2>/dev/null); then
  case "$active_state" in %s) ;; *) active_state=unknown; unit_observed=false ;; esac
else active_state=unknown; unit_observed=false
fi
if sub_state=$(systemctl show "$vpn_service" -p SubState --value 2>/dev/null); then
  case "$sub_state" in %s) ;; *) sub_state=unknown; unit_observed=false ;; esac
else sub_state=unknown; unit_observed=false
fi
if restarts=$(systemctl show "$vpn_service" -p NRestarts --value 2>/dev/null); then
  case "$restarts" in ''|*[!0-9]*) restarts=unknown ;; esac
else restarts=unknown
fi
printf 'server_unit_observed %%s\n' "$unit_observed"
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
	{
		if sudo -n cat "$vpn_status" 2>/dev/null; then
			printf '\n%%s\n' '---monitor-status-complete---'
		else
			printf '\n%%s\n' '---monitor-status-failed---'
		fi
		printf '%%s\n' '---monitor-journal---'
		if sudo -n journalctl -u "$vpn_service" --since "2 hours ago" --no-pager -o short-unix 2>/dev/null; then
			printf '\n%%s\n' '---monitor-journal-complete---'
		else
			printf '\n%%s\n' '---monitor-journal-failed---'
		fi
	} | awk -v wanted_names="$expected_names" -v wanted_addresses="$expected_addresses" '
  BEGIN {
	address_count=split(wanted_addresses, addresses, " ")
	for (i=1; i<=address_count; i++) expected_address[addresses[i]]=1
	name_count=split(wanted_names, names, " ")
	for (i=1; i<=name_count; i++) expected_name[names[i]]=1
  }
	$0=="---monitor-status-complete---" { status_complete++; next }
	$0=="---monitor-status-failed---" { status_failed++; next }
	$0=="---monitor-journal-complete---" { journal_complete++; next }
	$0=="---monitor-journal-failed---" { journal_failed++; next }
	$0=="---monitor-journal---" { journal=1; next }
	!journal {
		field_count=split($0, row, ",")
		if (field_count >= 9 && row[1]=="CLIENT_LIST" && expected_address[row[4]]) {
			source=row[3]
			sub(/:[0-9]+$/, "", source)
			if (source=="") next
			if (!(source in source_group)) source_group[source]=++source_groups
			connected=row[9]
			if (connected !~ /^[0-9]+$/) connected=0
			print "client", row[4], connected, source_group[source]
		}
		next
	}
	/Inactivity timeout \(--ping-restart\)/ {
		field_count=split($0, fields, /[[:space:]]+/)
		token=fields[4]
		parts=split(token, pair, "/")
		if (parts != 2) next
		client=pair[1]
		if (!expected_name[client]) next
		source=pair[2]
		sub(/:[0-9]+$/, "", source)
		if (source=="") next
		if (!(source in source_group)) source_group[source]=++source_groups
		epoch=fields[1]
		sub(/[.][0-9]+$/, "", epoch)
		if (epoch !~ /^[0-9]+$/) next
		print "timeout", client, epoch, source_group[source]
  }
  END {
    print "status_observed", (status_complete==1 && status_failed==0 ? "true" : "false")
    print "journal_observed", (journal_complete==1 && journal_failed==0 ? "true" : "false")
  }
'
# iputils ping: 0 is a reply, 1 is no reply; other exits do not measure the path.
for address in $expected_addresses; do
	if ping -n -c 1 -W 1 "$address" >/dev/null 2>&1; then
		reach=true
	else
		case "$?" in 1) reach=false ;; *) reach=unknown ;; esac
	fi
	printf 'reach %%s %%s\n' "$address" "$reach"
done`, vpnSessionsMarker, strings.Join(names, " "), strings.Join(addresses, " "), vpnSessionActiveStates, vpnSessionSubStates), nil
}

func clientName(client *host) string {
	if client == nil {
		return ""
	}
	return client.name
}

func parseVPNSessionsObservation(output string) (vpnSessionsObservation, error) {
	observation := vpnSessionsObservation{
		restarts:  "unknown",
		clients:   map[string]vpnSessionClient{},
		reachable: map[string]string{},
		timeouts:  map[string]vpnSessionTimeout{},
	}
	seen := map[string]bool{}
	for lineNumber, line := range strings.Split(output, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		switch fields[0] {
		case "server_unit_observed", "server_active_state", "server_sub_state", "server_restarts", "status_mtime_epoch":
			if len(fields) != 2 {
				return vpnSessionsObservation{}, fmt.Errorf("line %d: invalid %s field count", lineNumber+1, fields[0])
			}
			if seen[fields[0]] {
				return vpnSessionsObservation{}, fmt.Errorf("line %d: duplicate %s", lineNumber+1, fields[0])
			}
			seen[fields[0]] = true
			switch fields[0] {
			case "server_unit_observed":
				if fields[1] != "true" && fields[1] != "false" {
					return vpnSessionsObservation{}, fmt.Errorf("line %d: invalid unit observation", lineNumber+1)
				}
				observation.unitObserved = fields[1] == "true"
			case "server_active_state":
				observation.activeState = fields[1]
			case "server_sub_state":
				observation.subState = fields[1]
			case "server_restarts":
				if fields[1] == "unknown" {
					break
				}
				value, err := strconv.ParseInt(fields[1], 10, 64)
				if err != nil || value < 0 {
					break
				}
				observation.restarts = strconv.FormatInt(value, 10)
			case "status_mtime_epoch":
				value, err := strconv.ParseInt(fields[1], 10, 64)
				if err != nil || value < 0 {
					return vpnSessionsObservation{}, fmt.Errorf("line %d: invalid status_mtime_epoch", lineNumber+1)
				}
				observation.statusMTime = time.Unix(value, 0).UTC()
			}
		case "status_observed", "journal_observed":
			if len(fields) != 2 || (fields[1] != "true" && fields[1] != "false") {
				return vpnSessionsObservation{}, fmt.Errorf("line %d: invalid producer completion", lineNumber+1)
			}
			if seen[fields[0]] {
				return vpnSessionsObservation{}, fmt.Errorf("line %d: duplicate producer completion", lineNumber+1)
			}
			seen[fields[0]] = true
			if fields[0] == "status_observed" {
				observation.statusObserved = fields[1] == "true"
			} else {
				observation.journalObserved = fields[1] == "true"
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
			if len(fields) != 3 || net.ParseIP(fields[1]) == nil || (fields[2] != "true" && fields[2] != "false" && fields[2] != "unknown") {
				return vpnSessionsObservation{}, fmt.Errorf("line %d: invalid reachability row", lineNumber+1)
			}
			if _, duplicate := observation.reachable[fields[1]]; duplicate {
				return vpnSessionsObservation{}, fmt.Errorf("line %d: duplicate reachability address", lineNumber+1)
			}
			observation.reachable[fields[1]] = fields[2]
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
	// A legacy body has no command-success provenance. Even a claimed true
	// marker cannot make unknown, malformed or newer unreviewed states healthy.
	knownState := func(value, states string) bool {
		for _, known := range strings.Split(states, "|") {
			if value == known {
				return true
			}
		}
		return false
	}
	if !knownState(observation.activeState, vpnSessionActiveStates) {
		observation.activeState = "unknown"
		observation.unitObserved = false
	}
	if !knownState(observation.subState, vpnSessionSubStates) {
		observation.subState = "unknown"
		observation.unitObserved = false
	}
	// Failed or legacy optional history cannot lend a partial timeout prefix
	// to current membership or shared-source attribution.
	if !observation.journalObserved {
		observation.timeouts = map[string]vpnSessionTimeout{}
	}
	return observation, nil
}
