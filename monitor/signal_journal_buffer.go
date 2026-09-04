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

// SIGNALS.md §8.5b maps to signal_journal_buffer.go and
// signal_journal_buffer_test.go. The local journal is a bounded recovery
// buffer; Loki, rather than host uptime, owns durable history.
func NewJournalBufferSignal() Signal {
	return &signalAdapter{
		number: "8.5b", key: "journal-buffer", name: "Local journal buffer",
		probe: journalBufferProbe{},
	}
}

type journalBufferProbe struct{}

func (journalBufferProbe) id() string             { return "host/journal-buffer" }
func (journalBufferProbe) tier() string           { return tierWarn }
func (journalBufferProbe) cadence() time.Duration { return 5 * time.Minute }

const journalBufferMarker = "monitor-signal-8.5b-journal-buffer"

// The boundary query is both time-bounded and output-bounded. A host that has
// been up for 70 minutes should still have at least one record between 50 and
// 55 minutes ago under the one-hour policy. This tolerance accommodates whole
// journal-file rotation at the retention edge.
const journalBufferCommand = `# ` + journalBufferMarker + `
set -u

journald_active=$(systemctl is-active systemd-journald.service 2>/dev/null || true)
effective_config=$(systemd-analyze cat-config systemd/journald.conf 2>/dev/null) || exit 31
max_use=$(printf '%s\n' "$effective_config" | awk -F= '
  /^[[:space:]]*SystemMaxUse[[:space:]]*=/ {value=$2}
  END {gsub(/[[:space:]]/, "", value); print value}
')
max_file_size=$(printf '%s\n' "$effective_config" | awk -F= '
  /^[[:space:]]*SystemMaxFileSize[[:space:]]*=/ {value=$2}
  END {gsub(/[[:space:]]/, "", value); print value}
')
max_files=$(printf '%s\n' "$effective_config" | awk -F= '
  /^[[:space:]]*SystemMaxFiles[[:space:]]*=/ {value=$2}
  END {gsub(/[[:space:]]/, "", value); print value}
')
max_retention=$(printf '%s\n' "$effective_config" | awk -F= '
  /^[[:space:]]*MaxRetentionSec[[:space:]]*=/ {value=$2}
  END {gsub(/[[:space:]]/, "", value); print value}
')
uptime_seconds=$(awk '{printf "%.0f", $1}' /proc/uptime 2>/dev/null) || exit 32

boundary_checked=0
boundary_present=0
if [ "${uptime_seconds:-0}" -ge 4200 ]; then
  boundary_checked=1
  if journalctl -q --since '55 minutes ago' --until '50 minutes ago' -n 1 \
       --no-pager --output-fields=__REALTIME_TIMESTAMP -o json 2>/dev/null |
       awk 'NF {found=1} END {exit !found}'; then
    boundary_present=1
  fi
fi

printf '%s\n' \
  'observation_schema=1' \
  "journald_active=${journald_active}" \
  "max_use=${max_use:--}" \
  "max_file_size=${max_file_size:--}" \
  "max_files=${max_files:--}" \
  "max_retention=${max_retention:--}" \
  "uptime_seconds=${uptime_seconds:--}" \
  "boundary_checked=${boundary_checked}" \
  "boundary_present=${boundary_present}" \
  'boundary_oldest_seconds=3300' \
  'boundary_newest_seconds=3000'
`

type journalBufferSample struct {
	journaldActive       string
	maxUse               string
	maxFileSize          string
	maxFiles             string
	maxRetention         string
	uptimeSeconds        int
	boundaryChecked      bool
	boundaryPresent      bool
	boundaryOldestSecond int
	boundaryNewestSecond int
}

type journalBufferResult struct {
	host   *host
	sample journalBufferSample
	err    error
}

func (journalBufferProbe) check(ctx context.Context, env *probeEnv) ([]finding, error) {
	hosts := env.cfg.hostsWithRole("services")
	if len(hosts) == 0 {
		return nil, fmt.Errorf("journal buffer: no services hosts in inventory")
	}

	results := make(chan journalBufferResult, len(hosts))
	semaphore := make(chan struct{}, 4)
	var wait sync.WaitGroup
	for _, configuredHost := range hosts {
		target := configuredHost
		wait.Add(1)
		go func() {
			defer wait.Done()
			select {
			case semaphore <- struct{}{}:
				defer func() { <-semaphore }()
			case <-ctx.Done():
				results <- journalBufferResult{host: target, err: ctx.Err()}
				return
			}
			output, err := env.runner.shell(ctx, target, journalBufferCommand)
			if err != nil {
				results <- journalBufferResult{host: target, err: err}
				return
			}
			sample, err := parseJournalBufferSample(output)
			results <- journalBufferResult{host: target, sample: sample, err: err}
		}()
	}
	wait.Wait()
	close(results)

	ordered := make([]journalBufferResult, 0, len(hosts))
	for result := range results {
		ordered = append(ordered, result)
	}
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].host.name < ordered[j].host.name })

	findings := make([]finding, 0, len(ordered)*3)
	for _, result := range ordered {
		target := result.host.name
		if result.err != nil {
			findings = append(findings, cannotObserveFinding(target+"/journal-buffer", result.err))
			continue
		}
		findings = append(findings, evaluateJournalBuffer(target, result.sample)...)
	}
	return findings, nil
}

func parseJournalBufferSample(raw string) (journalBufferSample, error) {
	required := []string{
		"observation_schema", "journald_active", "max_use", "max_file_size", "max_files",
		"max_retention", "uptime_seconds", "boundary_checked", "boundary_present",
		"boundary_oldest_seconds", "boundary_newest_seconds",
	}
	values := map[string]string{}
	allowed := map[string]bool{}
	for _, key := range required {
		allowed[key] = true
	}
	for _, rawLine := range strings.Split(raw, "\n") {
		line := strings.TrimSpace(rawLine)
		if line == "" {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok || !allowed[key] {
			return journalBufferSample{}, fmt.Errorf("journal buffer: malformed or unexpected observation field")
		}
		if _, exists := values[key]; exists {
			return journalBufferSample{}, fmt.Errorf("journal buffer: duplicate %s field", key)
		}
		values[key] = strings.TrimSpace(value)
	}
	for _, key := range required {
		if values[key] == "" {
			return journalBufferSample{}, fmt.Errorf("journal buffer: observation omitted %s", key)
		}
	}
	if values["observation_schema"] != "1" {
		return journalBufferSample{}, fmt.Errorf("journal buffer: unsupported observation schema")
	}

	parseNonnegative := func(key string) (int, error) {
		value, err := strconv.Atoi(values[key])
		if err != nil || value < 0 {
			return 0, fmt.Errorf("journal buffer: invalid %s", key)
		}
		return value, nil
	}
	uptime, err := parseNonnegative("uptime_seconds")
	if err != nil {
		return journalBufferSample{}, err
	}
	oldest, err := parseNonnegative("boundary_oldest_seconds")
	if err != nil {
		return journalBufferSample{}, err
	}
	newest, err := parseNonnegative("boundary_newest_seconds")
	if err != nil {
		return journalBufferSample{}, err
	}
	parseBool := func(key string) (bool, error) {
		switch values[key] {
		case "0":
			return false, nil
		case "1":
			return true, nil
		default:
			return false, fmt.Errorf("journal buffer: invalid %s", key)
		}
	}
	checked, err := parseBool("boundary_checked")
	if err != nil {
		return journalBufferSample{}, err
	}
	present, err := parseBool("boundary_present")
	if err != nil {
		return journalBufferSample{}, err
	}
	if !checked && present {
		return journalBufferSample{}, fmt.Errorf("journal buffer: unchecked boundary cannot be present")
	}
	if oldest <= newest || newest != 3000 || oldest != 3300 {
		return journalBufferSample{}, fmt.Errorf("journal buffer: invalid coverage boundary")
	}
	return journalBufferSample{
		journaldActive: values["journald_active"], maxUse: values["max_use"],
		maxFileSize: values["max_file_size"], maxFiles: values["max_files"], maxRetention: values["max_retention"],
		uptimeSeconds: uptime, boundaryChecked: checked, boundaryPresent: present,
		boundaryOldestSecond: oldest, boundaryNewestSecond: newest,
	}, nil
}

func evaluateJournalBuffer(target string, sample journalBufferSample) []finding {
	observed := fmt.Sprintf(
		"journald_active=%s max_use=%s max_file_size=%s max_files=%s max_retention=%s uptime_seconds=%d boundary_checked=%t boundary_present=%t boundary=%d..%ds",
		sample.journaldActive, sample.maxUse, sample.maxFileSize, sample.maxFiles, sample.maxRetention,
		sample.uptimeSeconds, sample.boundaryChecked, sample.boundaryPresent,
		sample.boundaryNewestSecond, sample.boundaryOldestSecond,
	)
	findings := []finding{}
	if sample.journaldActive != "active" {
		findings = append(findings, finding{
			probeId: "host/journal-buffer", tier: tierPage,
			class: "journal-buffer-unavailable", target: target, sustain: 1,
			symptom:   fmt.Sprintf("%s has no active system journal", target),
			mechanism: "systemd-journald is not active, so local recovery evidence and Fluent Bit's journal input cannot advance.",
			baseline:  "systemd-journald remains active on every enabled services host.", observed: observed,
			evidence: "journald service=" + sample.journaldActive,
			context:  "This is an observability outage; it does not by itself prove that a Warp workload failed.",
			action:   "Inspect the journald unit and filesystem without rebooting the host. Restore the service, then prove both the local boundary and downstream Loki freshness.",
			verify:   "Require active journald and a record in the 50-to-55-minute boundary after the host has been up for 70 minutes.",
			playbook: "SIGNALS.md §8.5b and §11.14",
		})
	} else {
		findings = append(findings, healthyFinding("host/journal-buffer", tierPage, "journal-buffer-unavailable", target))
	}

	configOK := sample.maxUse == "100G" && sample.maxFileSize == "256M" && sample.maxFiles == "1024" && sample.maxRetention == "1hour"
	if !configOK {
		findings = append(findings, finding{
			probeId: "host/journal-buffer", tier: tierWarn,
			class: "journal-buffer-config", target: target, sustain: 2,
			symptom:   fmt.Sprintf("%s local journal policy differs from the one-hour buffer contract", target),
			mechanism: "An unbounded retention policy can consume disk as host uptime grows; an undersized cap can discard the recovery window before Fluent Bit/Loki investigations can use it.",
			baseline:  "Effective journald settings are SystemMaxUse=100G, SystemMaxFileSize=256M, SystemMaxFiles=1024, and MaxRetentionSec=1hour.", observed: observed,
			evidence: fmt.Sprintf("effective max_use=%s max_file_size=%s max_files=%s max_retention=%s", sample.maxUse, sample.maxFileSize, sample.maxFiles, sample.maxRetention),
			context:  "Loki owns durable history. Increasing local retention is not a substitute for repairing the shipper or Loki.",
			action:   "Run the reviewed edge Ansible configuration to restore the exact bounded policy; do not reboot solely to apply it.",
			verify:   "Read the effective merged journald configuration and require the exact four settings on every enabled edge.",
			playbook: "SIGNALS.md §8.5b",
		})
	} else {
		findings = append(findings, healthyFinding("host/journal-buffer", tierWarn, "journal-buffer-config", target))
	}

	if sample.boundaryChecked && !sample.boundaryPresent {
		findings = append(findings, finding{
			probeId: "host/journal-buffer", tier: tierWarn,
			class: "journal-buffer-short", target: target, sustain: 2,
			symptom:   fmt.Sprintf("%s discarded local journal evidence before the one-hour target", target),
			mechanism: "The configured size cap or journal failure removed every record in the bounded 50-to-55-minute test interval even though the host has been up long enough to populate it.",
			baseline:  "A host up for at least 70 minutes retains at least one record from 50 to 55 minutes ago while older records age into Loki.", observed: observed,
			evidence: "bounded target-boundary query returned no entry",
			context:  "This is local evidence loss, not proof that Loki also lost the records. Compare end-to-end shipper freshness before assigning data loss.",
			action:   "Measure journal bytes and top producers over a bounded suffix, confirm the effective cap, and verify Fluent Bit/Loki freshness. Reduce pathological log amplification or resize the buffer only from measured throughput.",
			verify:   "Require the boundary record on two consecutive probes and independently query fresh host data in Loki.",
			playbook: "SIGNALS.md §8.5b and §11.14",
		})
	} else {
		findings = append(findings, healthyFinding("host/journal-buffer", tierWarn, "journal-buffer-short", target))
	}
	return findings
}
