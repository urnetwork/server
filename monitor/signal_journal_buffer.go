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

// The journal metadata query is output-bounded to the current boot. Comparing
// its first retained entry with the host clock proves coverage without treating
// a genuinely quiet five-minute interval as lost data. The service-age gate
// lets the buffer refill after a deliberate journald restart.
const journalBufferCommand = `# ` + journalBufferMarker + `
set -u

journald_active=$(systemctl is-active systemd-journald.service 2>/dev/null || true)
effective_config=$(systemd-analyze cat-config systemd/journald.conf 2>/dev/null) || exit 31
storage=$(printf '%s\n' "$effective_config" | awk -F= '
  /^[[:space:]]*Storage[[:space:]]*=/ {value=$2}
  END {gsub(/[[:space:]]/, "", value); print value}
')
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
max_file_sec=$(printf '%s\n' "$effective_config" | awk -F= '
  /^[[:space:]]*MaxFileSec[[:space:]]*=/ {value=$2}
  END {gsub(/[[:space:]]/, "", value); print value}
')
max_retention=$(printf '%s\n' "$effective_config" | awk -F= '
  /^[[:space:]]*MaxRetentionSec[[:space:]]*=/ {value=$2}
  END {gsub(/[[:space:]]/, "", value); print value}
')
uptime_seconds=$(awk '{printf "%.0f", $1}' /proc/uptime 2>/dev/null) || exit 32
journald_active_seconds=0
if [ "$journald_active" = active ]; then
  active_enter_us=$(systemctl show systemd-journald.service \
    --property=ActiveEnterTimestampMonotonic --value 2>/dev/null) || exit 33
  case "$active_enter_us" in ''|*[!0-9]*) exit 33 ;; esac
  uptime_us=$(awk '{printf "%.0f", $1 * 1000000}' /proc/uptime 2>/dev/null) || exit 33
  journald_active_seconds=$(( (uptime_us - active_enter_us) / 1000000 ))
  [ "$journald_active_seconds" -ge 0 ] || exit 33
fi

boot_metadata=$(journalctl -q --list-boots -n 1 -o json --no-pager 2>/dev/null) || exit 34
first_entry_us=$(printf '%s\n' "$boot_metadata" |
  sed -n 's/.*"first_entry":\([0-9][0-9]*\).*/\1/p' | head -n 1)
oldest_entry_age_seconds=0
case "$first_entry_us" in
  ''|*[!0-9]*) ;;
  *)
    now_seconds=$(date +%s) || exit 35
    oldest_entry_age_seconds=$(( now_seconds - first_entry_us / 1000000 ))
    [ "$oldest_entry_age_seconds" -ge 0 ] || exit 35
    ;;
esac

coverage_checked=0
coverage_present=0
if [ "${uptime_seconds:-0}" -ge 4200 ] && [ "$journald_active_seconds" -ge 4200 ]; then
  coverage_checked=1
  if [ "$oldest_entry_age_seconds" -ge 3000 ]; then
    coverage_present=1
  fi
fi

printf '%s\n' \
  'observation_schema=2' \
  "journald_active=${journald_active}" \
  "journald_active_seconds=${journald_active_seconds}" \
  "storage=${storage:--}" \
  "max_use=${max_use:--}" \
  "max_file_size=${max_file_size:--}" \
  "max_files=${max_files:--}" \
  "max_file_sec=${max_file_sec:--}" \
  "max_retention=${max_retention:--}" \
  "uptime_seconds=${uptime_seconds:--}" \
  "coverage_checked=${coverage_checked}" \
  "coverage_present=${coverage_present}" \
  "oldest_entry_age_seconds=${oldest_entry_age_seconds}" \
  'coverage_target_seconds=3000'
`

type journalBufferSample struct {
	journaldActive        string
	journaldActiveSeconds int
	storage               string
	maxUse                string
	maxFileSize           string
	maxFiles              string
	maxFileSec            string
	maxRetention          string
	uptimeSeconds         int
	coverageChecked       bool
	coveragePresent       bool
	oldestEntryAgeSeconds int
	coverageTargetSeconds int
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
		"observation_schema", "journald_active", "journald_active_seconds", "storage",
		"max_use", "max_file_size", "max_files", "max_file_sec", "max_retention",
		"uptime_seconds", "coverage_checked", "coverage_present",
		"oldest_entry_age_seconds", "coverage_target_seconds",
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
	if values["observation_schema"] != "2" {
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
	activeSeconds, err := parseNonnegative("journald_active_seconds")
	if err != nil {
		return journalBufferSample{}, err
	}
	oldestEntryAge, err := parseNonnegative("oldest_entry_age_seconds")
	if err != nil {
		return journalBufferSample{}, err
	}
	coverageTarget, err := parseNonnegative("coverage_target_seconds")
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
	checked, err := parseBool("coverage_checked")
	if err != nil {
		return journalBufferSample{}, err
	}
	present, err := parseBool("coverage_present")
	if err != nil {
		return journalBufferSample{}, err
	}
	if !checked && present {
		return journalBufferSample{}, fmt.Errorf("journal buffer: unchecked coverage cannot be present")
	}
	if coverageTarget != 3000 {
		return journalBufferSample{}, fmt.Errorf("journal buffer: invalid coverage target")
	}
	if checked && present != (oldestEntryAge >= coverageTarget) {
		return journalBufferSample{}, fmt.Errorf("journal buffer: inconsistent coverage result")
	}
	return journalBufferSample{
		journaldActive: values["journald_active"], journaldActiveSeconds: activeSeconds,
		storage: values["storage"], maxUse: values["max_use"], maxFileSize: values["max_file_size"],
		maxFiles: values["max_files"], maxFileSec: values["max_file_sec"], maxRetention: values["max_retention"],
		uptimeSeconds: uptime, coverageChecked: checked, coveragePresent: present,
		oldestEntryAgeSeconds: oldestEntryAge, coverageTargetSeconds: coverageTarget,
	}, nil
}

func evaluateJournalBuffer(target string, sample journalBufferSample) []finding {
	observed := fmt.Sprintf(
		"journald_active=%s journald_active_seconds=%d storage=%s max_use=%s max_file_size=%s max_files=%s max_file_sec=%s max_retention=%s uptime_seconds=%d coverage_checked=%t coverage_present=%t oldest_entry_age_seconds=%d coverage_target_seconds=%d",
		sample.journaldActive, sample.journaldActiveSeconds, sample.storage, sample.maxUse, sample.maxFileSize,
		sample.maxFiles, sample.maxFileSec, sample.maxRetention, sample.uptimeSeconds,
		sample.coverageChecked, sample.coveragePresent, sample.oldestEntryAgeSeconds, sample.coverageTargetSeconds,
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
			verify:   "Require active journald and at least 50 minutes of current-boot journal coverage after both the host and journald have been active for 70 minutes.",
			playbook: "SIGNALS.md §8.5b and §11.14",
		})
	} else {
		findings = append(findings, healthyFinding("host/journal-buffer", tierPage, "journal-buffer-unavailable", target))
	}

	configOK := sample.storage == "persistent" && sample.maxUse == "100G" && sample.maxFileSize == "256M" &&
		sample.maxFiles == "1024" && sample.maxFileSec == "5min" && sample.maxRetention == "1hour"
	if !configOK {
		findings = append(findings, finding{
			probeId: "host/journal-buffer", tier: tierWarn,
			class: "journal-buffer-config", target: target, sustain: 2,
			symptom:   fmt.Sprintf("%s local journal policy differs from the one-hour buffer contract", target),
			mechanism: "An unbounded retention policy can consume disk as host uptime grows; volatile storage loses the buffer on journald restart; and month-long journal files make whole-file rotation coarse, so age vacuuming can delete a large slice of newer low-volume evidence at once.",
			baseline:  "Effective journald settings are Storage=persistent, SystemMaxUse=100G, SystemMaxFileSize=256M, SystemMaxFiles=1024, MaxFileSec=5min, and MaxRetentionSec=1hour.", observed: observed,
			evidence: fmt.Sprintf("effective storage=%s max_use=%s max_file_size=%s max_files=%s max_file_sec=%s max_retention=%s", sample.storage, sample.maxUse, sample.maxFileSize, sample.maxFiles, sample.maxFileSec, sample.maxRetention),
			context:  "Loki owns durable history. Increasing local retention is not a substitute for repairing the shipper or Loki.",
			action:   "Run the reviewed edge Ansible configuration to restore the exact bounded policy; do not reboot solely to apply it.",
			verify:   "Read the effective merged journald configuration and require the exact six settings on every enabled edge.",
			playbook: "SIGNALS.md §8.5b",
		})
	} else {
		findings = append(findings, healthyFinding("host/journal-buffer", tierWarn, "journal-buffer-config", target))
	}

	if sample.coverageChecked && !sample.coveragePresent {
		findings = append(findings, finding{
			probeId: "host/journal-buffer", tier: tierWarn,
			class: "journal-buffer-short", target: target, sustain: 2,
			symptom:   fmt.Sprintf("%s retained less than 50 minutes of current-boot journal evidence", target),
			mechanism: "The oldest retained current-boot record is younger than the recovery boundary even though both the host and journald have been active long enough to refill it. Size pressure, coarse whole-file rotation, or a journal failure removed the usable window.",
			baseline:  "After both host and journald have been active for 70 minutes, the oldest retained current-boot record is at least 50 minutes old while older records age into Loki.", observed: observed,
			evidence: fmt.Sprintf("oldest retained current-boot record age=%ds, required=%ds", sample.oldestEntryAgeSeconds, sample.coverageTargetSeconds),
			context:  "This is local evidence loss, not proof that Loki also lost the records. Compare end-to-end shipper freshness before assigning data loss.",
			action:   "Measure journal bytes and top producers over a bounded suffix, confirm the effective cap, and verify Fluent Bit/Loki freshness. Reduce pathological log amplification or resize the buffer only from measured throughput.",
			verify:   "Require at least 50 minutes of current-boot coverage on two consecutive probes and independently query fresh host data in Loki.",
			playbook: "SIGNALS.md §8.5b and §11.14",
		})
	} else {
		findings = append(findings, healthyFinding("host/journal-buffer", tierWarn, "journal-buffer-short", target))
	}
	return findings
}
