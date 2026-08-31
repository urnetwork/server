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

	if sample.kernelJournalStatus != 0 {
		findings = append(findings, cannotObserveFinding(host+"/proxy-kernel-oom", fmt.Errorf("kernel journal exited with status %d", sample.kernelJournalStatus)))
	} else if sample.recentProxyOOMKills > 0 {
		mechanism := "A global host OOM selected a proxy process. Proxy cgroups have no effective memory ceiling, so overlapping old and candidate processes compete with the kernel and every other service for physical RAM."
		if sample.runningUnits > 0 && sample.oomProxyProcesses > sample.runningUnits {
			mechanism = fmt.Sprintf("The kernel OOM task table contained %d proxy processes for %d running block units, proving old and candidate fleets overlapped. Their aggregate resident memory exhausted host RAM and swap before the old fleet drained.", sample.oomProxyProcesses, sample.runningUnits)
		}
		findings = append(findings, finding{
			probeId: "proxy/host-memory", tier: tierPage,
			class: "proxy-host-oom", target: host, frame: "global-oom", sustain: 1,
			symptom:   fmt.Sprintf("%s entered a global OOM and the kernel killed a proxy process", host),
			mechanism: mechanism,
			baseline:  "No proxy process is OOM-killed; a rollout never overlaps more candidates than host memory can hold while preserving at least 8 GiB of operational reserve and unused swap.",
			observed: fmt.Sprintf("recent_proxy_oom_kills=%d oom_task_table_proxy_processes=%d running_block_units=%d current_proxy_processes=%d current_proxy_rss_gib=%.2f mem_available_gib=%.2f swap_free_gib=%.2f udp_rcvbuf_errors_since_boot=%d unbounded_proxy_cgroups=%d",
				sample.recentProxyOOMKills, sample.oomProxyProcesses, sample.runningUnits, sample.proxyProcesses,
				kiBToGiB(sample.proxyRSSKiB), kiBToGiB(sample.memAvailableKiB), kiBToGiB(sample.swapFreeKiB),
				sample.udpRcvbufErrors, sample.proxyMemoryUnbounded),
			evidence: sample.oomLine,
			context:  "UdpRcvbufErrors is cumulative since boot, not a current rate. A large incident-correlated increase supports host receive starvation, but the kernel OOM and excess proxy-process count are the causal memory evidence; changing WireGuard peers or only enlarging UDP buffers does not restore rollout capacity.",
			action:   "Serialize proxy candidates per host (one block at a time on a host of this size) and require MemAvailable to exceed one candidate's measured RSS plus operational reserve before launching it. Then reduce steady proxy RSS or add RAM before increasing concurrency. Do not restart WireGuard, reinstall peers, or set a cgroup limit below the measured steady process size.",
			verify:   "During a complete proxy rollout, process count never exceeds running blocks plus configured host concurrency, MemAvailable stays above the reserve, swap is not exhausted, no new kernel OOM appears, UdpRcvbufErrors has zero incident-window delta, and the WireGuard acceptance request succeeds.",
			playbook: "SIGNALS.md §14.7",
		})
	}

	if sample.proxyProcesses > sample.runningUnits && sample.memAvailableKiB < sample.proxyMaxRSSKiB+reserveKiB {
		findings = append(findings, finding{
			probeId: "proxy/host-memory", tier: tierPage,
			class: "proxy-rollout-overlap", target: host, frame: "live-overlap", sustain: 1,
			symptom:   fmt.Sprintf("%s is overlapping proxy candidates without room for another process", host),
			mechanism: "More proxy processes than block units proves a rollout overlap is live, while available memory is below the largest current proxy process plus the host reserve. Launching another candidate can force swap or a global OOM before an old process exits.",
			baseline:  "Live proxy process count is at most block count plus bounded host rollout concurrency, and available memory exceeds the next candidate's expected RSS plus operational reserve.",
			observed: fmt.Sprintf("running_block_units=%d proxy_processes=%d largest_proxy_rss_gib=%.2f mem_available_gib=%.2f next_candidate_plus_reserve_gib=%.2f swap_free_gib=%.2f",
				sample.runningUnits, sample.proxyProcesses, kiBToGiB(sample.proxyMaxRSSKiB), kiBToGiB(sample.memAvailableKiB),
				kiBToGiB(sample.proxyMaxRSSKiB+reserveKiB), kiBToGiB(sample.swapFreeKiB)),
			context:  "This is a live capacity boundary, not proof that one proxy leaked. Compare per-process RSS and the old/candidate ownership before attributing growth to application state.",
			action:   "Pause new candidates on this host until an old proxy exits or capacity is restored, then resume with host-aware bounded concurrency and the same memory preflight. Preserve the current processes long enough to identify old versus candidate ownership.",
			verify:   "Process overlap returns within the configured bound, available memory stays above one candidate plus reserve throughout the remaining rollout, swap remains available, and no OOM or UDP receive-drop delta occurs.",
			playbook: "SIGNALS.md §14.7",
		})
	}

	if sample.proxyProcesses > 0 && fullRolloutDeficitKiB > 0 {
		findings = append(findings, finding{
			probeId: "proxy/host-memory", tier: tierWarn,
			class: "proxy-rollout-headroom", target: host, frame: "full-fleet-overlap", sustain: 1,
			symptom:   fmt.Sprintf("%s cannot hold a second full proxy fleet with host reserve", host),
			mechanism: "The current deploy model can start a candidate for every block before old processes drain. The current fleet's aggregate RSS is the best measured estimate of that second fleet; adding the operational reserve exceeds MemAvailable on this host.",
			baseline:  "MemAvailable is at least the current proxy fleet's aggregate RSS plus the larger of 8 GiB or 5% of physical RAM before an all-block rollout begins.",
			observed: fmt.Sprintf("running_block_units=%d proxy_processes=%d proxy_rss_gib=%.2f mem_total_gib=%.2f mem_available_gib=%.2f operational_reserve_gib=%.2f full_rollout_required_available_gib=%.2f capacity_deficit_gib=%.2f unbounded_proxy_cgroups=%d udp_rcvbuf_errors_since_boot=%d",
				sample.runningUnits, sample.proxyProcesses, kiBToGiB(sample.proxyRSSKiB), kiBToGiB(sample.memTotalKiB),
				kiBToGiB(sample.memAvailableKiB), kiBToGiB(reserveKiB), kiBToGiB(fullRolloutRequiredKiB),
				kiBToGiB(fullRolloutDeficitKiB), sample.proxyMemoryUnbounded, sample.udpRcvbufErrors),
			context:  "Summed RSS is conservative because shared pages can be counted more than once, but proxy heaps dominate the measured processes and the kernel OOM task table uses the same resident-memory boundary. A host-safe serialized rollout needs only one candidate RSS plus reserve, not a duplicate full fleet.",
			action:   "Make proxy deployment concurrency host-aware and serialize candidates on this host, with a MemAvailable preflight before each start. Separately reduce the roughly per-process steady RSS or provision enough RAM if all-block parallel rollout remains a requirement.",
			verify:   "A full rollout completes with process count inside the configured bound, at least 8 GiB available, unused swap, no new OOM, flat UdpRcvbufErrors over the rollout window, and successful WireGuard plus public-proxy acceptance.",
			playbook: "SIGNALS.md §14.7",
		})
	}

	return findings
}

func kiBToGiB(value int64) float64 {
	return float64(value) / (1024 * 1024)
}
