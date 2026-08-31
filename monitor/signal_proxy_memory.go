package monitor

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"
)

const (
	proxyMemoryMarker        = "monitor-signal-14.7-proxy-memory"
	proxyMemoryReserveKiB    = int64(8 * 1024 * 1024)
	proxyMemoryReserveFactor = 0.05

	proxyRolloutGuardFull      = "full-overlap"
	proxyRolloutGuardDrainOnly = "drain-only"
	proxyRolloutGuardDisabled  = "disabled"
	proxyRolloutGuardMissing   = "missing"
	proxyRolloutGuardUnknown   = "unknown"
	proxyRolloutGuardCommit    = "7e2075c"
)

// Signal proxy-memory implements SIGNALS.md §14.7. It treats a proxy rollout
// as a host-capacity event: candidates coexist with the old block processes,
// so steady-state health alone cannot prove that the replacement fleet fits.
func NewProxyMemorySignal() Signal {
	return &signalAdapter{
		number: "14.7", key: "proxy-memory", name: "Proxy rollout memory headroom and host OOM",
		probe: proxyMemoryProbe{},
	}
}

type proxyMemoryProbe struct{}

func (proxyMemoryProbe) id() string             { return "proxy/host-memory" }
func (proxyMemoryProbe) tier() string           { return tierWarn }
func (proxyMemoryProbe) cadence() time.Duration { return time.Minute }

func (proxyMemoryProbe) check(ctx context.Context, env *probeEnv) ([]finding, error) {
	findings := []finding{}
	for _, target := range env.cfg.hosts {
		if target.proxy == nil && !target.hasRole("services") {
			continue
		}
		command := "# " + proxyMemoryMarker + "\n" +
			"proxy_unit_pattern=" + shellSingleQuote("warp-"+env.cfg.env+"-proxy-*.service") + "\n" +
			proxyMemoryScript
		output, err := env.runner.shell(ctx, target, command)
		if err != nil {
			findings = append(findings, cannotObserveFinding(target.name+"/proxy-memory", err))
			continue
		}
		sample, err := parseProxyMemorySample(output)
		if err != nil {
			findings = append(findings, cannotObserveFinding(target.name+"/proxy-memory", err))
			continue
		}
		if !sample.proxyHost {
			continue
		}
		findings = append(findings, evaluateProxyMemory(target.name, sample)...)
	}
	return findings, nil
}

const proxyMemoryScript = `set -u
running_units=$(systemctl list-units --type=service --state=running --no-legend "$proxy_unit_pattern" 2>/dev/null |
  awk '$1 ~ /[.]service$/ {n++} END{print n+0}')
proxy_process_rows=$(ps -C bringyour-proxy -o pid=,rss= 2>/dev/null || true)
proxy_processes=$(printf '%s\n' "$proxy_process_rows" | awk 'NF>=2 {n++} END{print n+0}')
if [ "$running_units" -eq 0 ] && [ "$proxy_processes" -eq 0 ]; then
  printf 'proxy_host 0\n'
  exit 0
fi
printf 'proxy_host 1\n'

warpctl_path=/usr/local/sbin/warpctl
rollout_guard=missing
if [ -r "$warpctl_path" ]; then
  if systemctl show --all "$proxy_unit_pattern" -p Environment --value 2>/dev/null |
       grep -Fq 'WARPCTL_STAGGER_HOST_DRAIN=0'; then
    rollout_guard=disabled
  elif grep -aFq 'host rollout lock not acquired within' "$warpctl_path"; then
    rollout_guard=full-overlap
  elif grep -aFq 'Draining %d overlapping container(s) (staggered=%t)' "$warpctl_path"; then
    rollout_guard=drain-only
  else
    rollout_guard=unknown
  fi
fi
printf 'warpctl_rollout_guard %s\n' "$rollout_guard"

awk '
  /^MemTotal:/ {print "mem_total_kib", $2}
  /^MemAvailable:/ {print "mem_available_kib", $2}
  /^SwapTotal:/ {print "swap_total_kib", $2}
  /^SwapFree:/ {print "swap_free_kib", $2}
' /proc/meminfo
printf 'running_units %s\n' "$running_units"

printf '%s\n' "$proxy_process_rows" |
  awk '{n++; sum+=$2; if ($2>max) max=$2} END {
    print "proxy_processes", n+0
    print "proxy_rss_kib", sum+0
    print "proxy_max_rss_kib", max+0
  }'

bounded=0
unbounded=0
unknown=0
for pid in $(ps -C bringyour-proxy -o pid= 2>/dev/null); do
  cgroup=$(awk -F: '$1=="0" {print $3; exit}' "/proc/$pid/cgroup" 2>/dev/null)
  limit_path="/sys/fs/cgroup${cgroup}/memory.max"
  if [ -z "$cgroup" ] || [ ! -r "$limit_path" ]; then
    unknown=$((unknown+1))
  elif [ "$(tr -d '\n' < "$limit_path")" = "max" ]; then
    unbounded=$((unbounded+1))
  else
    bounded=$((bounded+1))
  fi
done
printf 'proxy_memory_bounded %s\nproxy_memory_unbounded %s\nproxy_memory_unknown %s\n' "$bounded" "$unbounded" "$unknown"

awk '$1=="Udp:" {
  if (!seen) {
    for (i=2; i<=NF; i++) if ($i=="RcvbufErrors") column=i
    seen=1
    next
  }
  if (column>0) print "udp_rcvbuf_errors", $column
}' /proc/net/snmp

kernel_log=$(journalctl -k --utc --since=-60min -o cat 2>&1)
kernel_journal_status=$?
printf 'kernel_journal_status %s\n' "$kernel_journal_status"
printf '%s\n' "$kernel_log" | awk '
  /Out of memory: Killed process [0-9]+ [(]bringyour-proxy[)]/ {
    kills++
    latest=$0
  }
  /^\[[[:space:]]*[0-9]+\].*[[:space:]]bringyour-proxy$/ {
    pid=$1
    gsub(/[^0-9]/, "", pid)
    if (pid!="" && !seen_pid[pid]++) process_count++
  }
  END {
    print "recent_proxy_oom_kills", kills+0
    print "oom_proxy_processes", process_count+0
    if (latest!="") print "oom_line", latest
  }
'
`

type proxyMemorySample struct {
	proxyHost            bool
	warpctlRolloutGuard  string
	memTotalKiB          int64
	memAvailableKiB      int64
	swapTotalKiB         int64
	swapFreeKiB          int64
	runningUnits         int64
	proxyProcesses       int64
	proxyRSSKiB          int64
	proxyMaxRSSKiB       int64
	proxyMemoryBounded   int64
	proxyMemoryUnbounded int64
	proxyMemoryUnknown   int64
	udpRcvbufErrors      int64
	kernelJournalStatus  int64
	recentProxyOOMKills  int64
	oomProxyProcesses    int64
	oomLine              string
}

func parseProxyMemorySample(output string) (proxyMemorySample, error) {
	sample := proxyMemorySample{}
	proxyHostValue := int64(-1)
	values := map[string]*int64{
		"proxy_host":             &proxyHostValue,
		"mem_total_kib":          &sample.memTotalKiB,
		"mem_available_kib":      &sample.memAvailableKiB,
		"swap_total_kib":         &sample.swapTotalKiB,
		"swap_free_kib":          &sample.swapFreeKiB,
		"running_units":          &sample.runningUnits,
		"proxy_processes":        &sample.proxyProcesses,
		"proxy_rss_kib":          &sample.proxyRSSKiB,
		"proxy_max_rss_kib":      &sample.proxyMaxRSSKiB,
		"proxy_memory_bounded":   &sample.proxyMemoryBounded,
		"proxy_memory_unbounded": &sample.proxyMemoryUnbounded,
		"proxy_memory_unknown":   &sample.proxyMemoryUnknown,
		"udp_rcvbuf_errors":      &sample.udpRcvbufErrors,
		"kernel_journal_status":  &sample.kernelJournalStatus,
		"recent_proxy_oom_kills": &sample.recentProxyOOMKills,
		"oom_proxy_processes":    &sample.oomProxyProcesses,
	}
	seen := map[string]bool{}
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		key, value, ok := strings.Cut(line, " ")
		if !ok {
			continue
		}
		value = strings.TrimSpace(value)
		if key == "oom_line" {
			sample.oomLine = value
			continue
		}
		if key == "warpctl_rollout_guard" {
			switch value {
			case proxyRolloutGuardFull, proxyRolloutGuardDrainOnly, proxyRolloutGuardDisabled,
				proxyRolloutGuardMissing, proxyRolloutGuardUnknown:
				sample.warpctlRolloutGuard = value
				seen[key] = true
				continue
			default:
				return proxyMemorySample{}, fmt.Errorf("proxy memory: invalid %s %q", key, value)
			}
		}
		destination, ok := values[key]
		if !ok {
			continue
		}
		parsed, err := strconv.ParseInt(value, 10, 64)
		if err != nil || parsed < 0 {
			return proxyMemorySample{}, fmt.Errorf("proxy memory: invalid %s %q", key, value)
		}
		*destination = parsed
		seen[key] = true
	}
	if !seen["proxy_host"] || (proxyHostValue != 0 && proxyHostValue != 1) {
		return proxyMemorySample{}, fmt.Errorf("proxy memory: observation omitted or invalid proxy_host")
	}
	sample.proxyHost = proxyHostValue == 1
	if !sample.proxyHost {
		return sample, nil
	}
	for _, key := range []string{
		"warpctl_rollout_guard",
		"mem_total_kib", "mem_available_kib", "swap_total_kib", "swap_free_kib",
		"running_units", "proxy_processes", "proxy_rss_kib", "proxy_max_rss_kib",
		"proxy_memory_bounded", "proxy_memory_unbounded", "proxy_memory_unknown",
		"udp_rcvbuf_errors", "kernel_journal_status", "recent_proxy_oom_kills", "oom_proxy_processes",
	} {
		if !seen[key] {
			return proxyMemorySample{}, fmt.Errorf("proxy memory: observation omitted %s", key)
		}
	}
	if sample.memTotalKiB == 0 || sample.memAvailableKiB > sample.memTotalKiB || sample.swapFreeKiB > sample.swapTotalKiB {
		return proxyMemorySample{}, fmt.Errorf("proxy memory: inconsistent host memory observation")
	}
	if sample.proxyProcesses != sample.proxyMemoryBounded+sample.proxyMemoryUnbounded+sample.proxyMemoryUnknown {
		return proxyMemorySample{}, fmt.Errorf("proxy memory: process and cgroup counts disagree")
	}
	return sample, nil
}

func evaluateProxyMemory(host string, sample proxyMemorySample) []finding {
	findings := []finding{}
	reserveKiB := max(proxyMemoryReserveKiB, int64(float64(sample.memTotalKiB)*proxyMemoryReserveFactor))
	fullRolloutRequiredKiB := sample.proxyRSSKiB + reserveKiB
	fullRolloutDeficitKiB := max(int64(0), fullRolloutRequiredKiB-sample.memAvailableKiB)
	guardObservation := fmt.Sprintf("warpctl_rollout_guard=%s", sample.warpctlRolloutGuard)

	switch sample.warpctlRolloutGuard {
	case proxyRolloutGuardDrainOnly, proxyRolloutGuardDisabled:
		mechanism := "The deployed Warp binary acquires its host lock only after every service worker can start a candidate. That drain-only scope permits a nearly complete duplicate proxy fleet before old processes leave."
		if sample.warpctlRolloutGuard == proxyRolloutGuardDisabled {
			mechanism = "At least one proxy service explicitly sets WARPCTL_STAGGER_HOST_DRAIN=0, disabling the host rollout lease. Service workers can therefore start candidates concurrently and create a nearly complete duplicate proxy fleet."
		}
		findings = append(findings, finding{
			probeId: "proxy/host-memory", tier: tierWarn,
			class: "proxy-rollout-guard-stale", target: host, frame: sample.warpctlRolloutGuard, sustain: 1,
			symptom:   fmt.Sprintf("%s does not have the full-overlap proxy rollout guard enabled", host),
			mechanism: mechanism,
			baseline:  "Every proxy host runs Warp with a host lease that begins before candidate start, remains held through synchronous old-container drain, and refuses the replacement if the lease times out.",
			observed: fmt.Sprintf("%s running_block_units=%d proxy_processes=%d proxy_rss_gib=%.2f mem_available_gib=%.2f full_fleet_capacity_deficit_gib=%.2f",
				guardObservation, sample.runningUnits, sample.proxyProcesses, kiBToGiB(sample.proxyRSSKiB),
				kiBToGiB(sample.memAvailableKiB), kiBToGiB(fullRolloutDeficitKiB)),
			context:  "This is a deployable software guard, separate from the hardware headroom boundary. Installing it prevents all-block overlap; it does not create enough RAM for one old/candidate pair or raise the fleet's active-client ceiling.",
			action:   fmt.Sprintf("Deploy Warp commit %s or later to this host and restart every Warp service worker before any proxy release. Remove WARPCTL_STAGGER_HOST_DRAIN=0 if present. Do not test the fix by starting a full proxy rollout while the drain-only or disabled guard is active.", proxyRolloutGuardCommit),
			verify:   "The probe reports warpctl_rollout_guard=full-overlap after every worker restart. During a controlled proxy rollout, process count stays at or below running blocks plus one, memory reserve and swap remain available, and no OOM or UDP receive-drop delta occurs.",
			playbook: "SIGNALS.md §14.7",
		})
	case proxyRolloutGuardMissing, proxyRolloutGuardUnknown:
		findings = append(findings, finding{
			probeId: "proxy/host-memory", tier: tierWarn,
			class: "proxy-rollout-guard-unverified", target: host, frame: sample.warpctlRolloutGuard, sustain: 1,
			symptom:   fmt.Sprintf("%s proxy rollout serialization cannot be verified", host),
			mechanism: "The running proxy host either has no readable /usr/local/sbin/warpctl or its binary has neither the full-overlap nor known drain-only signature. A release could duplicate the fleet before the monitor can prove that candidate starts are serialized.",
			baseline:  "The installed Warp binary exposes the full-overlap host-lease signature and no proxy service disables it.",
			observed:  fmt.Sprintf("%s running_block_units=%d proxy_processes=%d", guardObservation, sample.runningUnits, sample.proxyProcesses),
			context:   "Treat an unverified guard as unsafe for a memory-heavy fleet. This alert does not itself prove that a rollout or memory incident is active.",
			action:    fmt.Sprintf("Resolve the Warp executable used by the proxy units, deploy commit %s or later if needed, and restart every Warp service worker. Do not begin a proxy rollout until this probe reports full-overlap.", proxyRolloutGuardCommit),
			verify:    "The probe reports warpctl_rollout_guard=full-overlap for the host and a controlled single-overlap rollout completes without excess processes, reserve loss, OOM, or UDP receive-drop delta.",
			playbook:  "SIGNALS.md §14.7",
		})
	}

	if sample.kernelJournalStatus != 0 {
		findings = append(findings, cannotObserveFinding(host+"/proxy-kernel-oom", fmt.Errorf("kernel journal exited with status %d", sample.kernelJournalStatus)))
	} else if sample.recentProxyOOMKills > 0 {
		mechanism := "A global host OOM selected a proxy process. Proxy cgroups have no effective memory ceiling, so overlapping old and candidate processes compete with the kernel and every other service for physical RAM."
		if sample.runningUnits > 0 && sample.oomProxyProcesses > sample.runningUnits {
			mechanism = fmt.Sprintf("The kernel OOM task table contained %d proxy processes for %d running block units, proving old and candidate fleets overlapped. Their aggregate resident memory exhausted host RAM and swap before the old fleet drained.", sample.oomProxyProcesses, sample.runningUnits)
		}
		action := "The full-overlap host guard is installed; preserve the incident processes and validate why more than one replacement entered. Then reduce steady proxy RSS or add RAM if one old/candidate pair plus reserve cannot fit. Do not restart WireGuard, reinstall peers, or set a cgroup limit below the measured steady process size."
		if sample.warpctlRolloutGuard == proxyRolloutGuardDrainOnly || sample.warpctlRolloutGuard == proxyRolloutGuardDisabled {
			action = fmt.Sprintf("Deploy Warp commit %s or later and restart every Warp service worker before another proxy release. It must serialize candidate start through old-process drain, not only the drains. Then reduce steady proxy RSS or add RAM if one old/candidate pair plus reserve cannot fit. Do not restart WireGuard, reinstall peers, or set a cgroup limit below measured steady RSS.", proxyRolloutGuardCommit)
		} else if sample.warpctlRolloutGuard != proxyRolloutGuardFull {
			action = fmt.Sprintf("Do not start another proxy release until the installed Warp path is resolved and commit %s or later is running in every service worker. Then validate one serialized replacement, reduce steady proxy RSS, or add RAM if that pair plus reserve cannot fit. Do not restart WireGuard or reinstall peers for this signature.", proxyRolloutGuardCommit)
		}
		findings = append(findings, finding{
			probeId: "proxy/host-memory", tier: tierPage,
			class: "proxy-host-oom", target: host, frame: "global-oom", sustain: 1,
			symptom:   fmt.Sprintf("%s entered a global OOM and the kernel killed a proxy process", host),
			mechanism: mechanism,
			baseline:  "No proxy process is OOM-killed; a rollout never overlaps more candidates than host memory can hold while preserving at least 8 GiB of operational reserve and unused swap.",
			observed: fmt.Sprintf("%s recent_proxy_oom_kills=%d oom_task_table_proxy_processes=%d running_block_units=%d current_proxy_processes=%d current_proxy_rss_gib=%.2f mem_available_gib=%.2f swap_free_gib=%.2f udp_rcvbuf_errors_since_boot=%d unbounded_proxy_cgroups=%d",
				guardObservation, sample.recentProxyOOMKills, sample.oomProxyProcesses, sample.runningUnits, sample.proxyProcesses,
				kiBToGiB(sample.proxyRSSKiB), kiBToGiB(sample.memAvailableKiB), kiBToGiB(sample.swapFreeKiB),
				sample.udpRcvbufErrors, sample.proxyMemoryUnbounded),
			evidence: sample.oomLine,
			context:  "UdpRcvbufErrors is cumulative since boot, not a current rate. A large incident-correlated increase supports host receive starvation, but the kernel OOM and excess proxy-process count are the causal memory evidence; changing WireGuard peers or only enlarging UDP buffers does not restore rollout capacity.",
			action:   action,
			verify:   "During a complete proxy rollout, process count never exceeds running blocks plus configured host concurrency, MemAvailable stays above the reserve, swap is not exhausted, no new kernel OOM appears, UdpRcvbufErrors has zero incident-window delta, and the WireGuard acceptance request succeeds.",
			playbook: "SIGNALS.md §14.7",
		})
	}

	if sample.proxyProcesses > sample.runningUnits && sample.memAvailableKiB < sample.proxyMaxRSSKiB+reserveKiB {
		action := "Pause new candidates on this host until an old proxy exits or capacity is restored, then resume with host-aware bounded concurrency and the same memory preflight. Preserve the current processes long enough to identify old versus candidate ownership."
		if sample.warpctlRolloutGuard != proxyRolloutGuardFull {
			action = fmt.Sprintf("Pause new candidates and do not resume a proxy release until Warp commit %s or later is installed and every service worker has restarted. Preserve the current processes long enough to identify old versus candidate ownership, then validate one serialized replacement with the memory preflight.", proxyRolloutGuardCommit)
		}
		findings = append(findings, finding{
			probeId: "proxy/host-memory", tier: tierPage,
			class: "proxy-rollout-overlap", target: host, frame: "live-overlap", sustain: 1,
			symptom:   fmt.Sprintf("%s is overlapping proxy candidates without room for another process", host),
			mechanism: "More proxy processes than block units proves a rollout overlap is live, while available memory is below the largest current proxy process plus the host reserve. Launching another candidate can force swap or a global OOM before an old process exits.",
			baseline:  "Live proxy process count is at most block count plus bounded host rollout concurrency, and available memory exceeds the next candidate's expected RSS plus operational reserve.",
			observed: fmt.Sprintf("%s running_block_units=%d proxy_processes=%d largest_proxy_rss_gib=%.2f mem_available_gib=%.2f next_candidate_plus_reserve_gib=%.2f swap_free_gib=%.2f",
				guardObservation, sample.runningUnits, sample.proxyProcesses, kiBToGiB(sample.proxyMaxRSSKiB), kiBToGiB(sample.memAvailableKiB),
				kiBToGiB(sample.proxyMaxRSSKiB+reserveKiB), kiBToGiB(sample.swapFreeKiB)),
			context:  "This is a live capacity boundary, not proof that one proxy leaked. Compare per-process RSS and the old/candidate ownership before attributing growth to application state.",
			action:   action,
			verify:   "Process overlap returns within the configured bound, available memory stays above one candidate plus reserve throughout the remaining rollout, swap remains available, and no OOM or UDP receive-drop delta occurs.",
			playbook: "SIGNALS.md §14.7",
		})
	}

	if sample.proxyProcesses > 0 && fullRolloutDeficitKiB > 0 {
		mechanism := "The installed full-overlap guard should serialize replacements to one old/candidate pair. This host still cannot hold a second complete fleet: the current fleet's aggregate RSS plus operational reserve exceeds MemAvailable, so all-block parallel rollout is a hardware-capacity requirement, not a safe software setting."
		action := "Keep proxy deployment serialized. Reduce the roughly per-process steady RSS or provision enough RAM/hosts before requiring greater parallel rollout concurrency; the full-overlap guard does not raise the active-client ceiling."
		if sample.warpctlRolloutGuard == proxyRolloutGuardDrainOnly || sample.warpctlRolloutGuard == proxyRolloutGuardDisabled {
			mechanism = "The installed drain-only or disabled guard can start a candidate for every block before old processes drain. The current fleet's aggregate RSS is the best measured estimate of that second fleet; adding operational reserve exceeds MemAvailable on this host."
			action = fmt.Sprintf("Deploy Warp commit %s or later and restart every service worker before another proxy release, then keep this host serialized. Separately reduce steady proxy RSS or add RAM/hosts if greater concurrency or client capacity is required.", proxyRolloutGuardCommit)
		} else if sample.warpctlRolloutGuard != proxyRolloutGuardFull {
			mechanism = "The host cannot hold a second complete proxy fleet, and the monitor cannot verify that its deployed Warp serializes candidate starts. The current fleet's aggregate RSS plus operational reserve exceeds MemAvailable."
			action = fmt.Sprintf("Do not release proxy until Warp commit %s or later is verified in every service worker. Keep rollout serialized, and add RAM/hosts if greater concurrency or client capacity is required.", proxyRolloutGuardCommit)
		}
		findings = append(findings, finding{
			probeId: "proxy/host-memory", tier: tierWarn,
			class: "proxy-rollout-headroom", target: host, frame: "full-fleet-overlap", sustain: 1,
			symptom:   fmt.Sprintf("%s cannot hold a second full proxy fleet with host reserve", host),
			mechanism: mechanism,
			baseline:  "MemAvailable is at least the current proxy fleet's aggregate RSS plus the larger of 8 GiB or 5% of physical RAM before an all-block rollout begins.",
			observed: fmt.Sprintf("%s running_block_units=%d proxy_processes=%d proxy_rss_gib=%.2f mem_total_gib=%.2f mem_available_gib=%.2f operational_reserve_gib=%.2f full_rollout_required_available_gib=%.2f capacity_deficit_gib=%.2f unbounded_proxy_cgroups=%d udp_rcvbuf_errors_since_boot=%d",
				guardObservation, sample.runningUnits, sample.proxyProcesses, kiBToGiB(sample.proxyRSSKiB), kiBToGiB(sample.memTotalKiB),
				kiBToGiB(sample.memAvailableKiB), kiBToGiB(reserveKiB), kiBToGiB(fullRolloutRequiredKiB),
				kiBToGiB(fullRolloutDeficitKiB), sample.proxyMemoryUnbounded, sample.udpRcvbufErrors),
			context:  "Summed RSS is conservative because shared pages can be counted more than once, but proxy heaps dominate the measured processes and the kernel OOM task table uses the same resident-memory boundary. A host-safe serialized rollout needs only one candidate RSS plus reserve, not a duplicate full fleet.",
			action:   action,
			verify:   "A full rollout completes with process count inside the configured bound, at least 8 GiB available, unused swap, no new OOM, flat UdpRcvbufErrors over the rollout window, and successful WireGuard plus public-proxy acceptance.",
			playbook: "SIGNALS.md §14.7",
		})
	}

	return findings
}

func kiBToGiB(value int64) float64 {
	return float64(value) / (1024 * 1024)
}
