package monitor

import (
	"context"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
)

// SIGNALS.md §3.4 maps to signal_redis_process.go and signal_redis_process_test.go.
func NewRedisProcessSignal() Signal {
	return &signalAdapter{number: "3.4", key: "redis-process", name: "Redis node process signals", probe: redisProcessProbe{}}
}

type redisProcessProbe struct{}

func (redisProcessProbe) id() string             { return "redis/process" }
func (redisProcessProbe) tier() string           { return tierWarn }
func (redisProcessProbe) cadence() time.Duration { return 5 * time.Minute }

func (redisProcessProbe) check(ctx context.Context, env *probeEnv) ([]finding, error) {
	hosts := env.cfg.redisSweepHosts()
	if len(hosts) == 0 {
		return nil, fmt.Errorf("no redis-cluster host in inventory")
	}
	// every redis-cluster host runs its own Redis processes and kernel
	findings := []finding{}
	for _, host := range hosts {
		hostFindings, err := redisProcessHostFindings(ctx, env, host)
		if err != nil {
			if len(hosts) == 1 {
				return nil, err
			}
			findings = append(findings, cannotObserveFinding(host.name+"/redis-process", err))
			continue
		}
		findings = append(findings, hostFindings...)
	}
	return findings, nil
}

// The bounded collector reads only proc stat/boot identity, never process argv.
// Both complete censuses and every same-generation interval must succeed before
// any CPU row is published. Source failures cannot discard kernel evidence.
const redisProcessCpuScript = `import hashlib
import os
import re
import sys
import time

MAX_PROCESSES = 8192
MAX_ENTRIES = 16384
MAX_REDIS = 512
MAX_STAT_BYTES = 4096
MAX_CENSUS_NS = 250000000
MAX_UINT64 = 18446744073709551615

def read_bounded(path, limit):
    with open(path, "rb") as source:
        data = source.read(limit + 1)
    if not data or len(data) > limit or not data.endswith(b"\n"):
        raise ValueError()
    return data

def number(raw):
    if not re.fullmatch(rb"[0-9]+", raw):
        raise ValueError()
    value = int(raw)
    if value > MAX_UINT64:
        raise ValueError()
    return value

def boot_id():
    value = read_bounded("/proc/sys/kernel/random/boot_id", 64)[:-1]
    if not re.fullmatch(rb"[0-9a-f]{8}(-[0-9a-f]{4}){3}-[0-9a-f]{12}", value):
        raise ValueError()
    return value

def sample():
    began = time.monotonic_ns()
    if began <= 0:
        raise ValueError()
    boot = boot_id()
    pids = []
    with os.scandir("/proc") as entries:
        for count, entry in enumerate(entries, 1):
            if count > MAX_ENTRIES:
                raise ValueError()
            if re.fullmatch(r"[1-9][0-9]*", entry.name):
                pids.append(number(entry.name.encode("ascii")))
                if len(pids) > MAX_PROCESSES:
                    raise ValueError()
    if len(set(pids)) != len(pids):
        raise ValueError()
    processes = {}
    for pid in sorted(pids):
        if not 0 <= time.monotonic_ns() - began <= MAX_CENSUS_NS:
            raise ValueError()
        raw = read_bounded("/proc/" + str(pid) + "/stat", MAX_STAT_BYTES)
        observed = time.monotonic_ns()
        if not 0 <= observed - began <= MAX_CENSUS_NS:
            raise ValueError()
        prefix = (str(pid) + " (").encode("ascii")
        end = raw.rfind(b") ")
        if not raw.startswith(prefix) or end < len(prefix):
            raise ValueError()
        comm = raw[len(prefix):end]
        fields = raw[end + 2:].split()
        if len(fields) < 22 or len(fields[0]) != 1 or not fields[0].isalpha():
            raise ValueError()
        user, system, start, virt, rss = [number(fields[index]) for index in (11, 12, 19, 20, 21)]
        if user + system > MAX_UINT64:
            raise ValueError()
        if comm.startswith(b"redis-server"):
            if start == 0:
                raise ValueError()
            processes[pid] = (start, user, system, observed, virt, rss)
            if len(processes) > MAX_REDIS:
                raise ValueError()
    if boot != boot_id() or not 0 <= time.monotonic_ns() - began <= MAX_CENSUS_NS:
        raise ValueError()
    return boot, processes, began

try:
    ticks = os.sysconf("SC_CLK_TCK")
    page_bytes = os.sysconf("SC_PAGE_SIZE")
    if not 0 < ticks <= 1000000 or not 0 < page_bytes <= 1024 * 1024:
        raise ValueError()
    first_boot, first, first_ns = sample()
    time.sleep(1)
    last_boot, last, last_ns = sample()
    if first_boot != last_boot or first.keys() != last.keys() or not 1000000000 <= last_ns - first_ns <= 3000000000:
        raise ValueError()
    rows = []
    for pid in sorted(last):
        before = first[pid]
        after = last[pid]
        elapsed = after[3] - before[3]
        if before[0] != after[0] or after[1] < before[1] or after[2] < before[2] or not 1000000000 <= elapsed <= 3000000000:
            raise ValueError()
        cpu = (after[1] - before[1] + after[2] - before[2]) * 100000000000 / ticks / elapsed
        generation = hashlib.sha256(first_boot + ("/" + str(pid) + "/" + str(after[0])).encode("ascii")).hexdigest()
        virt_kb = after[4] // 1024
        rss_kb = after[5] * page_bytes // 1024
        if rss_kb > MAX_UINT64:
            raise ValueError()
        rows.append("redis {} {:.15g} {} {} {} {}".format(pid, cpu, virt_kb, rss_kb, generation, elapsed))
    rows.append("process_source complete " + str(len(rows)))
    sys.stdout.write("\n".join(rows) + "\n")
except Exception:
    sys.exit(1)
`

func redisProcessHostFindings(ctx context.Context, env *probeEnv, host *host) ([]finding, error) {
	command := fmt.Sprintf(`
if redis_process_rows=$(timeout -k 1s 5s python3 - 2>/dev/null <<'REDIS_PROCESS'
%s
REDIS_PROCESS
); then
  printf '%%s\n' "$redis_process_rows"
else
  printf 'process_source unavailable\n'
fi
if redis_kernel_rows=$(/bin/bash -o pipefail <<'REDIS_KERNEL'
dmesg -T 2>/dev/null | %s
REDIS_KERNEL
); then
  printf '%%s\n' "$redis_kernel_rows"
  printf 'kernel_source complete\n'
else
  printf 'kernel_source unavailable\n'
fi
`, redisProcessCpuScript, redisProcessKernelOomSelector())
	out, err := env.runner.shell(ctx, host, command)
	if err != nil {
		return nil, err
	}
	// Only a successful full pipeline and its terminal frame authorize kernel
	// evidence. Buffering the selected row discards partial output on failure.
	lines := strings.Split(strings.TrimSpace(out), "\n")
	kernelStatus := ""
	kernelRows := 0
	kernelMalformed := false
	for index, line := range lines {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		switch fields[0] {
		case "redis", "process_source":
		case "kernel_oom":
			kernelRows++
			if len(fields) < 2 || kernelRows > 1 {
				kernelMalformed = true
			}
		case "kernel_source":
			if len(fields) != 2 || kernelStatus != "" || index != len(lines)-1 {
				kernelMalformed = true
			} else {
				kernelStatus = fields[1]
			}
		default:
			kernelMalformed = true
		}
	}
	var kernelErr error
	if kernelMalformed || kernelStatus != "complete" && kernelStatus != "unavailable" {
		kernelErr = fmt.Errorf("invalid response from Redis kernel source")
	} else if kernelStatus == "unavailable" {
		kernelErr = fmt.Errorf("Redis kernel source exit status failure")
	}
	processErr := redisProcessSourceError(lines)
	findings := []finding{}
	for _, line := range lines {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		switch fields[0] {
		case "redis":
			if processErr != nil || atof(fields[2]) <= 200 {
				continue
			}
			findings = append(findings, finding{
				probeId: "redis/process", tier: tierWarn,
				class: "redis-cpu-sustained", target: host.name, frame: fields[1] + "/" + fields[5], sustain: 2,
				symptom:   fmt.Sprintf("redis process %s averaged %s%% CPU over %.3fs", fields[1], fields[2], atof(fields[6])/1e9),
				mechanism: "Repeated multi-core Redis CPU can indicate io-thread/lazyfree churn; a pegged fork child can stall the serving parent's event loop during BGSAVE.",
				baseline:  "No same-generation Redis process exceeds 200% CPU across two measured intervals.",
				observed:  fmt.Sprintf("pid=%s cpu_pct=%s cpu_window_ns=%s virt_kb=%s res_kb=%s", fields[1], fields[2], fields[6], fields[3], fields[4]),
				evidence:  "CPU percent = 100 * delta(utime + stime) / CLK_TCK / monotonic elapsed seconds, with matching boot, PID and process start.",
				context:   "Each observation averages a bounded 1–3 second interval. Two observations at five-minute cadence do not establish continuous CPU pressure between them; one-shot runs bypass sustain.",
				action:    "Identify whether the PID is a serving master or BGSAVE child, then correlate command rate and save cadence before intervening.",
				verify:    "Two complete interval observations return below 200% CPU and local PING latency remains healthy.",
				playbook:  "SIGNALS.md §3.4 and §5.2",
			})
		case "kernel_oom":
			if kernelErr != nil {
				continue
			}
			victim := redisProcessKernelOomVictim(strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "kernel_oom")))
			if victim == "other" {
				continue
			}
			if victim == "unknown" {
				findings = append(findings, cannotObserveFinding(host.name+"/redis-kernel", fmt.Errorf("invalid response: kernel OOM victim is ambiguous")))
				continue
			}
			findings = append(findings, finding{
				probeId: "redis/process", tier: tierPage,
				class: "redis-kernel-oom", target: host.name, sustain: 1,
				symptom:   "The retained kernel log identifies a redis-server OOM victim on the cluster host.",
				mechanism: "The kernel selected a task named redis-server during an OOM event. The record alone does not identify its Redis role, the exhausted memory limit, or current cluster impact.",
				baseline:  "No explicit Redis victim in the retained kernel records.",
				observed:  "retained kernel record includes an explicit redis-server victim",
				evidence:  "victim_comm=redis-server; source=kernel-oom-victim-record",
				context:   "Kernel records may predate this observation. Confirm the event time and process identity before attributing current shard loss or recovery.",
				action:    "Confirm the victim PID, event time, Redis process role, cgroup and host memory limits, and cluster slot impact before choosing remediation.",
				verify:    "Every node PING succeeds, cluster slots are covered, and a bounded follow-up window records no new Redis OOM event.",
				playbook:  "SIGNALS.md §3.4 and §5.2",
			})
		}
	}
	if processErr != nil {
		findings = append(findings, cannotObserveFinding(host.name+"/redis-process", processErr))
	}
	if kernelErr != nil {
		findings = append(findings, cannotObserveFinding(host.name+"/redis-kernel", kernelErr))
	}
	if len(findings) == 0 {
		findings = append(findings, healthyFinding("redis/process", tierWarn, "redis-process", host.name))
	}
	return findings, nil
}

// Process authority is independent of kernel authority; no partial census can
// establish absence or publish a numeric finding from malformed source rows.
func redisProcessSourceError(lines []string) error {
	status := ""
	rows := 0
	seenPids := map[uint64]bool{}
	for _, line := range lines {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		switch fields[0] {
		case "redis":
			if status != "" || len(fields) != 7 {
				return fmt.Errorf("invalid response from Redis process source")
			}
			pid, pidErr := strconv.ParseUint(fields[1], 10, 64)
			cpu, cpuErr := strconv.ParseFloat(fields[2], 64)
			_, virtErr := strconv.ParseUint(fields[3], 10, 64)
			_, resErr := strconv.ParseUint(fields[4], 10, 64)
			windowNs, windowErr := strconv.ParseUint(fields[6], 10, 64)
			generationValid := len(fields[5]) == 64 && strings.Trim(fields[5], "0123456789abcdef") == ""
			if pidErr != nil || pid == 0 || seenPids[pid] || cpuErr != nil || math.IsNaN(cpu) || math.IsInf(cpu, 0) || cpu < 0 || virtErr != nil || resErr != nil || !generationValid || windowErr != nil || windowNs < 1000000000 || windowNs > 3000000000 {
				return fmt.Errorf("invalid response from Redis process source")
			}
			seenPids[pid] = true
			rows++
			if rows > 512 {
				return fmt.Errorf("invalid response from Redis process source")
			}
		case "process_source":
			if status != "" {
				return fmt.Errorf("invalid response from Redis process source")
			}
			if len(fields) == 2 && fields[1] == "unavailable" && rows == 0 {
				return fmt.Errorf("Redis process source exit status failure")
			}
			if len(fields) != 3 || fields[1] != "complete" {
				return fmt.Errorf("invalid response from Redis process source")
			}
			count, err := strconv.Atoi(fields[2])
			if err != nil || count != rows {
				return fmt.Errorf("invalid response from Redis process source")
			}
			status = fields[1]
		case "kernel_oom", "kernel_source":
			if status == "" {
				return fmt.Errorf("invalid response from Redis process source")
			}
		default:
			if status == "" {
				return fmt.Errorf("invalid response from Redis process source")
			}
		}
	}
	if status != "complete" {
		return fmt.Errorf("invalid response from Redis process source")
	}
	return nil
}
