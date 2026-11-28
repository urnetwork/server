package monitor

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	proxyMemoryMarker          = "monitor-signal-14.7-proxy-memory"
	proxyMemoryReserveKiB      = int64(8 * 1024 * 1024)
	proxyMemoryReserveFactor   = 0.05
	proxyKernelJournalMaxBytes = int64(4 * 1024 * 1024)

	proxyRolloutGuardFull      = rolloutGuardFull
	proxyRolloutGuardDrainOnly = rolloutGuardDrainOnly
	proxyRolloutGuardDisabled  = rolloutGuardDisabled
	proxyRolloutGuardMissing   = rolloutGuardMissing
	proxyRolloutGuardUnknown   = rolloutGuardUnknown
	proxyRolloutGuardCommit    = rolloutGuardCommit

	proxyUDPReceiveDropWarnPerMinute = 100.0
	proxyUDPReceiveDropPagePerMinute = 10_000.0
	proxyUDPReceiveDropPageRatio     = 0.01
)

// The kernel boot_id source returns one UUID; unavailable sentinels are not generations.
var proxyMemoryBootIdPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// Signal proxy-memory implements SIGNALS.md §14.7. It treats a proxy image or
// config-generation rollout as a host-capacity event: either change creates
// candidates that coexist with the old block processes, so steady-state health
// alone cannot prove that the replacement fleet fits.
func NewProxyMemorySignal() Signal {
	return &signalAdapter{
		number: "14.7", key: "proxy-memory", name: "Proxy rollout memory headroom and host OOM",
		probe: &proxyMemoryProbe{udpSamples: map[string]proxyUDPCounterSample{}},
	}
}

type proxyMemoryProbe struct {
	udpLock    sync.Mutex
	udpSamples map[string]proxyUDPCounterSample
}

func (*proxyMemoryProbe) id() string             { return "proxy/host-memory" }
func (*proxyMemoryProbe) tier() string           { return tierWarn }
func (*proxyMemoryProbe) cadence() time.Duration { return time.Minute }

func (self *proxyMemoryProbe) check(ctx context.Context, env *probeEnv) ([]finding, error) {
	findings := []finding{}
	observedAt := env.now().UTC()
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
		delta, observed := self.observeUDP(ctx, target.name, sample, observedAt)
		if sample.bootID == "" {
			unavailable := cannotObserveFinding(target.name+"/proxy-udp-receive-drops", fmt.Errorf("proxy memory: invalid response for kernel boot_id"))
			unavailable.observed += " boot_identity=unavailable"
			unavailable.mechanism = "The kernel boot identity is missing, unavailable, malformed, or ambiguous, so these cumulative UDP counters cannot prove a same-boot interval. This does not prove a reboot or packet loss; independent memory and kernel OOM evidence remains valid."
			unavailable.verify = "Restore the successful kernel boot-id read. A matching known UUID and nondecreasing counters can measure from the last valid sample; a new UUID or absent baseline requires a second valid sample at least 30 seconds later."
			unavailable.playbook = "SIGNALS.md §14.7"
			findings = append(findings, unavailable)
		} else if observed {
			findings = append(findings, evaluateProxyUDPReceiveDrops(target.name, sample, delta))
		} else {
			findings = append(findings, healthyFinding("proxy/host-memory", tierWarn, "proxy-udp-receive-drops", target.name))
		}
	}
	return findings, nil
}

const proxyMemoryScript = `set -u
running_units_unknown=0
if running_unit_rows=$(systemctl list-units --type=service --state=running --no-legend "$proxy_unit_pattern" 2>/dev/null); then
  :
else
  running_unit_rows=
  running_units_unknown=1
fi
running_units=$(printf '%s\n' "$running_unit_rows" | awk '$1 ~ /[.]service$/ {n++} END{print n+0}')
if ! process_rows=$(LC_ALL=C ps -e -o pid=,rss=,comm= 2>/dev/null); then
  exit 1
fi
if ! proxy_process_rows=$(printf '%s\n' "$process_rows" |
  awk 'NF==3 && $1 ~ /^[0-9]+$/ && $2 ~ /^[0-9]+$/ && $3=="bringyour-proxy" {print $1, $2}'); then
  exit 1
fi
proxy_processes=$(printf '%s\n' "$proxy_process_rows" | awk 'NF>=2 {n++} END{print n+0}')
if [ "$running_units" -eq 0 ] && [ "$proxy_processes" -eq 0 ]; then
  if [ "$running_units_unknown" -ne 0 ]; then
    exit 1
  fi
  printf 'proxy_host 0\n'
  exit 0
fi
printf 'proxy_host 1\n'
printf 'running_units_unknown %s\n' "$running_units_unknown"

if ! boot_id=$(tr -d '\n' < /proc/sys/kernel/random/boot_id 2>/dev/null); then
  boot_id=unknown
fi
if [ -z "$boot_id" ]; then
  boot_id=unknown
fi
printf 'boot_id %s\n' "$boot_id"

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
for pid in $(printf '%s\n' "$proxy_process_rows" | awk '{print $1}'); do
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
    for (i=2; i<=NF; i++) {
      if ($i=="InDatagrams") in_column=i
      if ($i=="RcvbufErrors") error_column=i
    }
    seen=1
    next
  }
  if (in_column>0) print "udp_in_datagrams", $in_column
  if (error_column>0) print "udp_rcvbuf_errors", $error_column
}' /proc/net/snmp

# Bound stdout before materialization; retain only stderr presence, never its body.
# timeout owns and joins journalctl; this shell joins both its reader and owner.
umask 077
journal_dir=$(mktemp -d) || exit 1
journal_reader_pid=
journal_error_pid=
journal_pid=
proxy_journal_cleanup() {
  trap '' HUP INT TERM
  if [ -n "$journal_reader_pid" ]; then kill "$journal_reader_pid" 2>/dev/null || :; fi
  if [ -n "$journal_error_pid" ]; then kill "$journal_error_pid" 2>/dev/null || :; fi
  if [ -n "$journal_pid" ]; then kill "$journal_pid" 2>/dev/null || :; fi
  if [ -n "$journal_reader_pid" ]; then wait "$journal_reader_pid" 2>/dev/null || :; fi
  if [ -n "$journal_error_pid" ]; then wait "$journal_error_pid" 2>/dev/null || :; fi
  if [ -n "$journal_pid" ]; then wait "$journal_pid" 2>/dev/null || :; fi
  rm -rf -- "$journal_dir"
}
trap proxy_journal_cleanup EXIT
trap 'exit 1' HUP INT TERM
mkfifo "$journal_dir/stdout" "$journal_dir/stderr" || exit 1
# A short reader closes the pipe at N+1; timeout also bounds a stalled producer.
timeout --kill-after=2s 10s journalctl -k --utc --since=-60min --reverse -o cat \
  >"$journal_dir/stdout" 2>"$journal_dir/stderr" &
journal_pid=$!
head -c 4194305 <"$journal_dir/stdout" >"$journal_dir/raw" &
journal_reader_pid=$!
head -c 1 <"$journal_dir/stderr" >"$journal_dir/stderr-presence" &
journal_error_pid=$!
if wait "$journal_reader_pid"; then journal_reader_status=0; else journal_reader_status=$?; fi
journal_reader_pid=
if wait "$journal_error_pid"; then journal_error_status=0; else journal_error_status=$?; fi
journal_error_pid=
if wait "$journal_pid"; then kernel_journal_status=0; else kernel_journal_status=$?; fi
journal_pid=
journal_bytes=$(wc -c <"$journal_dir/raw" | tr -d '[:space:]')
journal_stderr=$(wc -c <"$journal_dir/stderr-presence" | tr -d '[:space:]')
case "$journal_stderr" in 0|1) ;; *) exit 1 ;; esac
case "$journal_bytes" in ''|*[!0-9]*) exit 1 ;; esac
[ "$journal_bytes" -le 4194305 ] || exit 1
journal_complete=1
journal_reason=complete
journal_records=1
if [ "$journal_reader_status" -ne 0 ] || [ "$journal_error_status" -ne 0 ] || \
   { [ "$kernel_journal_status" -ne 0 ] && \
     { [ "$kernel_journal_status" -ne 141 ] || [ "$journal_bytes" -ne 4194305 ] || [ "$journal_stderr" -ne 0 ]; }; }; then
  journal_complete=0
  journal_reason=source-failed
  journal_records=0
elif [ "$journal_bytes" -gt 4194304 ]; then
  journal_complete=0
  journal_reason=byte-bound
elif [ "$journal_stderr" -ne 0 ]; then
  journal_complete=0
  journal_reason=stderr-observed
elif [ "$journal_bytes" -gt 0 ]; then
  journal_last_byte=$(tail -c 1 "$journal_dir/raw" | od -An -tu1 | tr -d '[:space:]')
  if [ "$journal_last_byte" != 10 ]; then
    journal_complete=0
    journal_reason=incomplete-record
  fi
fi
printf 'kernel_journal_schema 1\nkernel_journal_status %s\nkernel_journal_complete %s\nkernel_journal_bytes %s\nkernel_journal_reason %s\nkernel_journal_stderr %s\n' \
  "$kernel_journal_status" "$journal_complete" "$journal_bytes" "$journal_reason" "$journal_stderr"
# LC_ALL=C makes length byte-counted. Only newline-terminated stdout lines
# wholly within N are eligible; a split/oversized final record cannot qualify.
LC_ALL=C awk -v bytes="$journal_bytes" -v records="$journal_records" '
  {
    end += length($0)+1
    if (records!=1 || end>bytes || end>4194304) next
  }
  /^Out of memory: Killed process [0-9]+ [(]bringyour-proxy[)]([[:space:]].*)?$/ {
    kills++
    if (latest=="") latest=$0
  }
  /^\[[[:space:]]*[0-9]+\].*[[:space:]]bringyour-proxy$/ {
    pid=$1
    gsub(/[^0-9]/, "", pid)
    if (pid!="" && !seen_pid[pid]++) process_count++
  }
  END {
    print "kernel_journal_positive_qualified", (kills>0 ? 1 : 0)
    print "recent_proxy_oom_kills", kills+0
    print "oom_proxy_processes", process_count+0
    if (latest!="") print "oom_line", latest
  }
' "$journal_dir/raw"
`

var proxyKernelOOMLinePattern = regexp.MustCompile(`^Out of memory: Killed process [0-9]+ [(]bringyour-proxy[)]([[:space:]].*)?$`)

type proxyMemorySample struct {
	proxyHost                      bool
	bootID                         string
	warpctlRolloutGuard            string
	memTotalKiB                    int64
	memAvailableKiB                int64
	swapTotalKiB                   int64
	swapFreeKiB                    int64
	runningUnits                   int64
	runningUnitsUnknown            bool
	proxyProcesses                 int64
	proxyRSSKiB                    int64
	proxyMaxRSSKiB                 int64
	proxyMemoryBounded             int64
	proxyMemoryUnbounded           int64
	proxyMemoryUnknown             int64
	udpInDatagrams                 int64
	udpRcvbufErrors                int64
	kernelJournalStatus            int64
	kernelJournalComplete          bool
	kernelJournalBytes             int64
	kernelJournalStderr            bool
	kernelJournalReason            string
	kernelJournalPositiveQualified bool
	recentProxyOOMKills            int64
	oomProxyProcesses              int64
	oomLine                        string
}

type proxyUDPCounterSample struct {
	bootID       string
	observedAt   time.Time
	inDatagrams  int64
	rcvbufErrors int64
}

type proxyUDPDelta struct {
	elapsed      time.Duration
	inBefore     int64
	inAfter      int64
	rcvbufBefore int64
	rcvbufAfter  int64
	inDatagrams  int64
	rcvbufErrors int64
}

// Compares consecutive valid host samples. Unavailable boot identity preserves
// the last known baseline; matching valid UUIDs prove the full elapsed interval.
// First samples, generation/counter resets and immediate reruns remain warmups.
func (self *proxyMemoryProbe) observeUDP(ctx context.Context, host string, sample proxyMemorySample, observedAt time.Time) (proxyUDPDelta, bool) {
	// Context callbacks stay outside the lock; only its captured channel is
	// checked at the per-host publication checkpoint. This is not rollback.
	done := ctx.Done()
	self.udpLock.Lock()
	defer self.udpLock.Unlock()
	select {
	case <-done:
		return proxyUDPDelta{}, false
	default:
	}
	if self.udpSamples == nil {
		self.udpSamples = map[string]proxyUDPCounterSample{}
	}
	previous, initialized := self.udpSamples[host]
	if initialized && !proxyMemoryBootIdPattern.MatchString(previous.bootID) {
		delete(self.udpSamples, host)
		initialized = false
	}
	if !proxyMemoryBootIdPattern.MatchString(sample.bootID) {
		return proxyUDPDelta{}, false
	}
	current := proxyUDPCounterSample{
		bootID:       sample.bootID,
		observedAt:   observedAt,
		inDatagrams:  sample.udpInDatagrams,
		rcvbufErrors: sample.udpRcvbufErrors,
	}
	self.udpSamples[host] = current
	if !initialized || previous.bootID != current.bootID ||
		current.inDatagrams < previous.inDatagrams || current.rcvbufErrors < previous.rcvbufErrors ||
		!previous.observedAt.Before(current.observedAt) {
		return proxyUDPDelta{}, false
	}
	elapsed := current.observedAt.Sub(previous.observedAt)
	if elapsed < 30*time.Second {
		return proxyUDPDelta{}, false
	}
	return proxyUDPDelta{
		elapsed:      elapsed,
		inBefore:     previous.inDatagrams,
		inAfter:      current.inDatagrams,
		rcvbufBefore: previous.rcvbufErrors,
		rcvbufAfter:  current.rcvbufErrors,
		inDatagrams:  current.inDatagrams - previous.inDatagrams,
		rcvbufErrors: current.rcvbufErrors - previous.rcvbufErrors,
	}, true
}

func parseProxyMemorySample(output string) (proxyMemorySample, error) {
	sample := proxyMemorySample{}
	proxyHostValue := int64(-1)
	runningUnitsUnknownValue := int64(0)
	journalSchema, journalComplete, journalPositive, journalStderr := int64(0), int64(0), int64(0), int64(0)
	values := map[string]*int64{
		"proxy_host":                        &proxyHostValue,
		"mem_total_kib":                     &sample.memTotalKiB,
		"mem_available_kib":                 &sample.memAvailableKiB,
		"swap_total_kib":                    &sample.swapTotalKiB,
		"swap_free_kib":                     &sample.swapFreeKiB,
		"running_units":                     &sample.runningUnits,
		"running_units_unknown":             &runningUnitsUnknownValue,
		"proxy_processes":                   &sample.proxyProcesses,
		"proxy_rss_kib":                     &sample.proxyRSSKiB,
		"proxy_max_rss_kib":                 &sample.proxyMaxRSSKiB,
		"proxy_memory_bounded":              &sample.proxyMemoryBounded,
		"proxy_memory_unbounded":            &sample.proxyMemoryUnbounded,
		"proxy_memory_unknown":              &sample.proxyMemoryUnknown,
		"udp_in_datagrams":                  &sample.udpInDatagrams,
		"udp_rcvbuf_errors":                 &sample.udpRcvbufErrors,
		"kernel_journal_status":             &sample.kernelJournalStatus,
		"kernel_journal_schema":             &journalSchema,
		"kernel_journal_complete":           &journalComplete,
		"kernel_journal_bytes":              &sample.kernelJournalBytes,
		"kernel_journal_stderr":             &journalStderr,
		"kernel_journal_positive_qualified": &journalPositive,
		"recent_proxy_oom_kills":            &sample.recentProxyOOMKills,
		"oom_proxy_processes":               &sample.oomProxyProcesses,
	}
	seen := map[string]bool{}
	bootIdRows := 0
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		key, value, ok := strings.Cut(line, " ")
		value = strings.TrimSpace(value)
		if !ok && key == "running_units_unknown" {
			return proxyMemorySample{}, fmt.Errorf("proxy memory: invalid unit census qualifier")
		}
		if key == "boot_id" {
			bootIdRows++
			if bootIdRows == 1 && proxyMemoryBootIdPattern.MatchString(value) {
				sample.bootID = value
			} else {
				sample.bootID = ""
			}
			continue
		}
		if !ok {
			continue
		}
		if key == "kernel_journal_reason" {
			if seen[key] || (value != "complete" && value != "byte-bound" && value != "incomplete-record" && value != "source-failed" && value != "stderr-observed") {
				return proxyMemorySample{}, fmt.Errorf("proxy memory: invalid journal reason")
			}
			sample.kernelJournalReason = value
			seen[key] = true
			continue
		}
		if key == "oom_line" {
			if seen[key] {
				return proxyMemorySample{}, fmt.Errorf("proxy memory: duplicate kernel OOM record")
			}
			sample.oomLine = value
			seen[key] = true
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
		if key == "running_units_unknown" && (seen[key] || (value != "0" && value != "1")) {
			return proxyMemorySample{}, fmt.Errorf("proxy memory: invalid unit census qualifier")
		}
		if (strings.HasPrefix(key, "kernel_journal_") || key == "recent_proxy_oom_kills" || key == "oom_proxy_processes") && seen[key] {
			return proxyMemorySample{}, fmt.Errorf("proxy memory: duplicate journal qualifier")
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
	sample.runningUnitsUnknown = runningUnitsUnknownValue == 1
	if !sample.proxyHost {
		if sample.runningUnitsUnknown {
			return proxyMemorySample{}, fmt.Errorf("proxy memory: unknown unit census cannot establish absence")
		}
		return sample, nil
	}
	for _, key := range []string{
		"warpctl_rollout_guard",
		"mem_total_kib", "mem_available_kib", "swap_total_kib", "swap_free_kib",
		"running_units", "proxy_processes", "proxy_rss_kib", "proxy_max_rss_kib",
		"proxy_memory_bounded", "proxy_memory_unbounded", "proxy_memory_unknown",
		"udp_in_datagrams", "udp_rcvbuf_errors", "kernel_journal_status", "recent_proxy_oom_kills", "oom_proxy_processes",
		"kernel_journal_schema", "kernel_journal_complete", "kernel_journal_bytes", "kernel_journal_reason", "kernel_journal_positive_qualified", "kernel_journal_stderr",
	} {
		if !seen[key] {
			return proxyMemorySample{}, fmt.Errorf("proxy memory: observation omitted %s", key)
		}
	}
	sample.kernelJournalComplete = journalComplete == 1
	sample.kernelJournalPositiveQualified = journalPositive == 1
	sample.kernelJournalStderr = journalStderr == 1
	if journalSchema != 1 || journalComplete > 1 || journalPositive > 1 || journalStderr > 1 || sample.kernelJournalBytes > proxyKernelJournalMaxBytes+1 {
		return proxyMemorySample{}, fmt.Errorf("proxy memory: invalid journal qualifier")
	}
	switch sample.kernelJournalReason {
	case "complete":
		if !sample.kernelJournalComplete || sample.kernelJournalStatus != 0 || sample.kernelJournalStderr || sample.kernelJournalBytes > proxyKernelJournalMaxBytes {
			return proxyMemorySample{}, fmt.Errorf("proxy memory: inconsistent complete journal")
		}
	case "byte-bound":
		if sample.kernelJournalComplete || sample.kernelJournalBytes != proxyKernelJournalMaxBytes+1 || (sample.kernelJournalStatus != 0 && (sample.kernelJournalStatus != 141 || sample.kernelJournalStderr)) {
			return proxyMemorySample{}, fmt.Errorf("proxy memory: inconsistent journal byte boundary")
		}
	case "incomplete-record":
		if sample.kernelJournalComplete || sample.kernelJournalStatus != 0 || sample.kernelJournalStderr || sample.kernelJournalBytes == 0 || sample.kernelJournalBytes > proxyKernelJournalMaxBytes {
			return proxyMemorySample{}, fmt.Errorf("proxy memory: inconsistent journal framing")
		}
	case "stderr-observed":
		if sample.kernelJournalComplete || sample.kernelJournalStatus != 0 || !sample.kernelJournalStderr || sample.kernelJournalBytes > proxyKernelJournalMaxBytes {
			return proxyMemorySample{}, fmt.Errorf("proxy memory: inconsistent journal diagnostic qualifier")
		}
	case "source-failed":
		if sample.kernelJournalComplete || sample.kernelJournalPositiveQualified || sample.recentProxyOOMKills != 0 || sample.oomProxyProcesses != 0 || sample.oomLine != "" {
			return proxyMemorySample{}, fmt.Errorf("proxy memory: failed journal claimed positive evidence")
		}
	}
	if sample.kernelJournalPositiveQualified != (sample.recentProxyOOMKills > 0) ||
		(sample.kernelJournalPositiveQualified && (!proxyKernelOOMLinePattern.MatchString(sample.oomLine) || int64(len(sample.oomLine))+1 > min(sample.kernelJournalBytes, proxyKernelJournalMaxBytes))) ||
		(!sample.kernelJournalPositiveQualified && sample.oomLine != "") {
		return proxyMemorySample{}, fmt.Errorf("proxy memory: invalid qualified kernel OOM record")
	}
	if sample.memTotalKiB == 0 || sample.memAvailableKiB > sample.memTotalKiB || sample.swapFreeKiB > sample.swapTotalKiB {
		return proxyMemorySample{}, fmt.Errorf("proxy memory: inconsistent host memory observation")
	}
	if sample.runningUnitsUnknown && (sample.runningUnits != 0 || sample.proxyProcesses == 0) {
		return proxyMemorySample{}, fmt.Errorf("proxy memory: unknown unit census requires independently observed proxy processes")
	}
	if sample.proxyProcesses != sample.proxyMemoryBounded+sample.proxyMemoryUnbounded+sample.proxyMemoryUnknown {
		return proxyMemorySample{}, fmt.Errorf("proxy memory: process and cgroup counts disagree")
	}
	return sample, nil
}

func evaluateProxyUDPReceiveDrops(host string, sample proxyMemorySample, delta proxyUDPDelta) finding {
	runningUnitsObservation := strconv.FormatInt(sample.runningUnits, 10)
	if sample.runningUnitsUnknown {
		runningUnitsObservation = "unknown"
	}
	elapsedMinutes := delta.elapsed.Minutes()
	if elapsedMinutes <= 0 {
		return healthyFinding("proxy/host-memory", tierWarn, "proxy-udp-receive-drops", host)
	}
	dropRate := float64(delta.rcvbufErrors) / elapsedMinutes
	totalObserved := delta.inDatagrams + delta.rcvbufErrors
	dropRatio := 0.0
	if totalObserved > 0 {
		dropRatio = float64(delta.rcvbufErrors) / float64(totalObserved)
	}
	tier := tierWarn
	if dropRate >= proxyUDPReceiveDropPagePerMinute || dropRatio >= proxyUDPReceiveDropPageRatio {
		tier = tierPage
	} else if dropRate < proxyUDPReceiveDropWarnPerMinute {
		return healthyFinding("proxy/host-memory", tierWarn, "proxy-udp-receive-drops", host)
	}
	action := "If a proxy rollout is active, stop launching candidates on this host and preserve the exact interval. Correlate process overlap, RSS, MemAvailable, swap, kernel OOM, CPU scheduling, and per-socket receive queues before changing configuration. If memory is stable, identify the owning UDP socket/read loop with ss -u -m -p. Do not restart WireGuard, reinstall peers, or blindly enlarge receive buffers."
	if sample.warpctlRolloutGuard != proxyRolloutGuardFull {
		action = fmt.Sprintf("Do not begin or continue a proxy rollout on this host. Deploy Warp commit %s or later and restart every service worker so candidate start through drain is serialized. Preserve the exact interval and correlate process overlap, RSS, MemAvailable, swap, kernel OOM, CPU scheduling, and per-socket receive queues. Do not restart WireGuard, reinstall peers, or blindly enlarge receive buffers.", proxyRolloutGuardCommit)
	}
	return finding{
		probeId: "proxy/host-memory", tier: tier,
		class: "proxy-udp-receive-drops", target: host, frame: "receive-buffer", sustain: 1,
		symptom: fmt.Sprintf("%s dropped %d UDP datagrams from full receive buffers in %.0f seconds (%.0f/min)",
			host, delta.rcvbufErrors, delta.elapsed.Seconds(), dropRate),
		mechanism: "Linux increments Udp.RcvbufErrors when a datagram reaches the host but the destination socket's receive queue has no room before userspace drains it. This is host-wide loss below WireGuard authentication: memory pressure, CPU starvation, a slow receive loop, or undersized socket buffering can cause it; a peer-install or handshake change cannot recover the discarded datagram.",
		baseline:  fmt.Sprintf("No material live receive-buffer loss: udp_rcvbuf_errors_rate stays below %.0f/min and udp_receive_drop_ratio stays below %.2f%% of delivered-plus-dropped datagrams; incident windows remain at zero.", proxyUDPReceiveDropWarnPerMinute, proxyUDPReceiveDropPageRatio*100),
		observed: fmt.Sprintf("warpctl_rollout_guard=%s sample_elapsed_s=%.0f udp_in_datagrams_delta=%d udp_rcvbuf_errors_delta=%d udp_rcvbuf_errors_rate_per_min=%.2f udp_receive_drop_ratio_pct=%.4f udp_in_datagrams_before=%d udp_in_datagrams_after=%d udp_rcvbuf_errors_before=%d udp_rcvbuf_errors_after=%d running_block_units=%s proxy_processes=%d proxy_rss_gib=%.2f mem_available_gib=%.2f swap_free_gib=%.2f recent_proxy_oom_kills=%d",
			sample.warpctlRolloutGuard, delta.elapsed.Seconds(), delta.inDatagrams, delta.rcvbufErrors,
			dropRate, dropRatio*100, delta.inBefore, delta.inAfter, delta.rcvbufBefore, delta.rcvbufAfter,
			runningUnitsObservation, sample.proxyProcesses, kiBToGiB(sample.proxyRSSKiB),
			kiBToGiB(sample.memAvailableKiB), kiBToGiB(sample.swapFreeKiB), sample.recentProxyOOMKills),
		evidence: fmt.Sprintf("Known same-UUID /proc/net/snmp counters advanced over %.0fs between consecutive valid samples. Unavailable observations never replace a valid baseline; first samples, boot-id changes, counter regressions, and sub-30s reruns are warmups rather than loss findings.", delta.elapsed.Seconds()),
		context:  "Udp.RcvbufErrors and InDatagrams cover the whole host, so the delta proves live kernel receive loss but not socket ownership by itself. A high drop ratio or incident-sized rate makes this page-tier. Larger buffers can absorb a short burst but cannot create CPU, RAM, or per-process client capacity; sustained loss at a steady single fleet may require application receive-loop work or additional proxy hardware.",
		action:   action,
		verify:   "Across at least ten minutes of production-shaped UDP load and one controlled serialized proxy rollout, adjacent same-boot samples show zero RcvbufErrors delta, process overlap stays bounded, memory reserve and swap remain available, no OOM occurs, and WireGuard acceptance succeeds without a timeout.",
		playbook: "SIGNALS.md §14.7",
	}
}

func evaluateProxyMemory(host string, sample proxyMemorySample) []finding {
	findings := []finding{}
	reserveKiB := max(proxyMemoryReserveKiB, int64(float64(sample.memTotalKiB)*proxyMemoryReserveFactor))
	fullRolloutRequiredKiB := sample.proxyRSSKiB + reserveKiB
	fullRolloutDeficitKiB := max(int64(0), fullRolloutRequiredKiB-sample.memAvailableKiB)
	guardObservation := fmt.Sprintf("warpctl_rollout_guard=%s", sample.warpctlRolloutGuard)
	runningUnitsObservation := strconv.FormatInt(sample.runningUnits, 10)
	if sample.runningUnitsUnknown {
		runningUnitsObservation = "unknown"
		findings = append(findings, cannotObserveFinding(host+"/proxy-running-units", fmt.Errorf("proxy memory: running unit census unavailable")))
	}

	if !sample.kernelJournalComplete {
		unknown := cannotObserveFinding(host+"/proxy-kernel-oom", &observationStateUnavailableError{})
		unknown.observed += fmt.Sprintf(" journal_complete=false reason=%s observed_bytes=%d observation_budget_bytes=%d positive_record_qualified=%t stderr_present=%t", sample.kernelJournalReason, sample.kernelJournalBytes, proxyKernelJournalMaxBytes, sample.kernelJournalPositiveQualified, sample.kernelJournalStderr)
		unknown.mechanism = "The bounded kernel journal observation is incomplete. Only independently complete validated stdout records can retain positive OOM evidence; missing history is not a healthy absence."
		findings = append(findings, unknown)
	}
	if sample.kernelJournalPositiveQualified {
		mechanism := "A global host OOM selected a proxy process. Proxy cgroups have no effective memory ceiling, so overlapping old and candidate processes compete with the kernel and every other service for physical RAM."
		if sample.kernelJournalComplete && !sample.runningUnitsUnknown && sample.runningUnits > 0 && sample.oomProxyProcesses > sample.runningUnits {
			mechanism = fmt.Sprintf("The kernel OOM task table contained %d proxy processes for %d running block units, proving old and candidate fleets overlapped. Their aggregate resident memory exhausted host RAM and swap before the old fleet drained.", sample.oomProxyProcesses, sample.runningUnits)
		}
		if !sample.kernelJournalComplete {
			mechanism = "An independently complete newline-terminated kernel stdout OOM record selected a proxy process. The rest of the journal window is unobserved; observed task rows are not a complete process cohort or proof of the current cause."
		}
		action := "The full-overlap host guard is installed; preserve the incident processes and validate why more than one replacement entered. Then reduce steady proxy RSS or add RAM if one old/candidate pair plus reserve cannot fit. Do not restart WireGuard, reinstall peers, or set a cgroup limit below the measured steady process size."
		if sample.warpctlRolloutGuard == proxyRolloutGuardDrainOnly || sample.warpctlRolloutGuard == proxyRolloutGuardDisabled {
			action = fmt.Sprintf("Deploy Warp commit %s or later and restart every Warp service worker before another proxy image or config-generation rollout. It must serialize candidate start through old-process drain, not only the drains. Then reduce steady proxy RSS or add RAM if one old/candidate pair plus reserve cannot fit. Do not restart WireGuard, reinstall peers, or set a cgroup limit below measured steady RSS.", proxyRolloutGuardCommit)
		} else if sample.warpctlRolloutGuard != proxyRolloutGuardFull {
			action = fmt.Sprintf("Do not start another proxy image or config-generation rollout until the installed Warp path is resolved and commit %s or later is running in every service worker. Then validate one serialized replacement, reduce steady proxy RSS, or add RAM if that pair plus reserve cannot fit. Do not restart WireGuard or reinstall peers for this signature.", proxyRolloutGuardCommit)
		}
		findings = append(findings, finding{
			probeId: "proxy/host-memory", tier: tierPage,
			class: "proxy-host-oom", target: host, frame: "global-oom", sustain: 1,
			symptom:   fmt.Sprintf("%s entered a global OOM and the kernel killed a proxy process", host),
			mechanism: mechanism,
			baseline:  "No proxy process is OOM-killed; a rollout never overlaps more candidates than host memory can hold while preserving at least 8 GiB of operational reserve and unused swap.",
			observed: fmt.Sprintf("%s recent_proxy_oom_kills=%d oom_task_table_proxy_processes=%d running_block_units=%s current_proxy_processes=%d current_proxy_rss_gib=%.2f mem_available_gib=%.2f swap_free_gib=%.2f udp_rcvbuf_errors_since_boot=%d unbounded_proxy_cgroups=%d",
				guardObservation, sample.recentProxyOOMKills, sample.oomProxyProcesses, runningUnitsObservation, sample.proxyProcesses,
				kiBToGiB(sample.proxyRSSKiB), kiBToGiB(sample.memAvailableKiB), kiBToGiB(sample.swapFreeKiB),
				sample.udpRcvbufErrors, sample.proxyMemoryUnbounded) + fmt.Sprintf(" journal_complete=%t journal_reason=%s positive_record_qualified=true", sample.kernelJournalComplete, sample.kernelJournalReason),
			evidence: sample.oomLine,
			context:  "UdpRcvbufErrors is cumulative since boot, not a current rate. A large incident-correlated increase supports host receive starvation, but the kernel OOM and excess proxy-process count are the causal memory evidence; changing WireGuard peers or only enlarging UDP buffers does not restore rollout capacity.",
			action:   action,
			verify:   "During a complete proxy image or config-generation rollout, process count never exceeds running blocks plus configured host concurrency, MemAvailable stays above the reserve, swap is not exhausted, no new kernel OOM appears, UdpRcvbufErrors has zero incident-window delta, and the WireGuard acceptance request succeeds.",
			playbook: "SIGNALS.md §14.7",
		})
	}

	if !sample.runningUnitsUnknown && sample.proxyProcesses > sample.runningUnits && sample.memAvailableKiB < sample.proxyMaxRSSKiB+reserveKiB {
		action := "Pause new candidates on this host until an old proxy exits or capacity is restored, then resume the proxy image or config-generation rollout with host-aware bounded concurrency and the same memory preflight. Preserve the current processes long enough to identify old versus candidate ownership."
		if sample.warpctlRolloutGuard != proxyRolloutGuardFull {
			action = fmt.Sprintf("Pause new candidates and do not resume a proxy image or config-generation rollout until Warp commit %s or later is installed and every service worker has restarted. Preserve the current processes long enough to identify old versus candidate ownership, then validate one serialized replacement with the memory preflight.", proxyRolloutGuardCommit)
		}
		findings = append(findings, finding{
			probeId: "proxy/host-memory", tier: tierPage,
			class: "proxy-rollout-overlap", target: host, frame: "live-overlap", sustain: 1,
			symptom:   fmt.Sprintf("%s is overlapping proxy candidates without room for another process", host),
			mechanism: "More proxy processes than block units proves a rollout overlap is live, while available memory is below the largest current proxy process plus the host reserve. Launching another candidate can force swap or a global OOM before an old process exits.",
			baseline:  "Live proxy process count is at most block count plus bounded host rollout concurrency, and available memory exceeds the next candidate's expected RSS plus operational reserve.",
			observed: fmt.Sprintf("%s running_block_units=%s proxy_processes=%d largest_proxy_rss_gib=%.2f mem_available_gib=%.2f next_candidate_plus_reserve_gib=%.2f swap_free_gib=%.2f",
				guardObservation, runningUnitsObservation, sample.proxyProcesses, kiBToGiB(sample.proxyMaxRSSKiB), kiBToGiB(sample.memAvailableKiB),
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
			mechanism = "The installed drain-only or disabled guard can start a candidate for every block before old processes drain. A proxy service-image or config-generation change can trigger the same replacement path. The current fleet's aggregate RSS is the best measured estimate of that second fleet; adding operational reserve exceeds MemAvailable on this host."
			action = fmt.Sprintf("Deploy Warp commit %s or later and restart every service worker before another proxy image or config-generation rollout, then keep this host serialized. Separately reduce steady proxy RSS or add RAM/hosts if greater concurrency or client capacity is required.", proxyRolloutGuardCommit)
		} else if sample.warpctlRolloutGuard != proxyRolloutGuardFull {
			mechanism = "The host cannot hold a second complete proxy fleet, and the monitor cannot verify that its deployed Warp serializes candidate starts. The current fleet's aggregate RSS plus operational reserve exceeds MemAvailable."
			action = fmt.Sprintf("Do not release a proxy image or config generation until Warp commit %s or later is verified in every service worker. Keep rollout serialized, and add RAM/hosts if greater concurrency or client capacity is required.", proxyRolloutGuardCommit)
		}
		findings = append(findings, finding{
			probeId: "proxy/host-memory", tier: tierWarn,
			class: "proxy-rollout-headroom", target: host, frame: "full-fleet-overlap", sustain: 1,
			symptom:   fmt.Sprintf("%s cannot hold a second full proxy fleet with host reserve", host),
			mechanism: mechanism,
			baseline:  "MemAvailable is at least the current proxy fleet's aggregate RSS plus the larger of 8 GiB or 5% of physical RAM before an all-block image or config-generation rollout begins.",
			observed: fmt.Sprintf("%s running_block_units=%s proxy_processes=%d proxy_rss_gib=%.2f mem_total_gib=%.2f mem_available_gib=%.2f operational_reserve_gib=%.2f full_rollout_required_available_gib=%.2f capacity_deficit_gib=%.2f unbounded_proxy_cgroups=%d udp_rcvbuf_errors_since_boot=%d",
				guardObservation, runningUnitsObservation, sample.proxyProcesses, kiBToGiB(sample.proxyRSSKiB), kiBToGiB(sample.memTotalKiB),
				kiBToGiB(sample.memAvailableKiB), kiBToGiB(reserveKiB), kiBToGiB(fullRolloutRequiredKiB),
				kiBToGiB(fullRolloutDeficitKiB), sample.proxyMemoryUnbounded, sample.udpRcvbufErrors),
			context:  "Summed RSS is conservative because shared pages can be counted more than once, but proxy heaps dominate the measured processes and the kernel OOM task table uses the same resident-memory boundary. A host-safe serialized rollout needs only one candidate RSS plus reserve, not a duplicate full fleet.",
			action:   action,
			verify:   "A full proxy image or config-generation rollout completes with process count inside the configured bound, at least 8 GiB available, unused swap, no new OOM, flat UdpRcvbufErrors over the rollout window, and successful WireGuard plus public-proxy acceptance.",
			playbook: "SIGNALS.md §14.7",
		})
	}

	return findings
}

func kiBToGiB(value int64) float64 {
	return float64(value) / (1024 * 1024)
}
