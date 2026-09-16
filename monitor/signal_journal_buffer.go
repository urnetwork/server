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

// The journal entry queries are time- and output-bounded to the current boot.
// A latest-entry query first proves readable machine output; a reverse query at
// the 50-minute cutoff then proves coverage without requiring activity in one
// narrow interval. The service-age gate lets the buffer refill after a
// deliberate journald restart.
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

parse_journal_timestamp() {
  record=$1
  [ -n "$record" ] || return 1
  [ "$(printf '%s\n' "$record" | awk 'NF {count++} END {print count+0}')" -eq 1 ] || return 1
  case "$record" in \{*\}) ;; *) return 1 ;; esac
  [ "$(printf '%s\n' "$record" | awk -F'"__REALTIME_TIMESTAMP"' '{print NF-1}')" -eq 1 ] || return 1
  journal_timestamp_us=$(printf '%s\n' "$record" |
    sed -n 's/.*"__REALTIME_TIMESTAMP"[[:space:]]*:[[:space:]]*"\{0,1\}\([0-9][0-9]*\)"\{0,1\}.*/\1/p')
  case "$journal_timestamp_us" in ''|*[!0-9]*) return 1 ;; esac
}

latest_record=$(timeout 10s journalctl -q -b 0 --reverse -n 1 --no-pager \
  --output-fields=__REALTIME_TIMESTAMP -o json 2>&1) || exit 34
parse_journal_timestamp "$latest_record" || exit 35
latest_entry_seconds=$(( journal_timestamp_us / 1000000 ))

boundary_record=$(timeout 10s journalctl -q -b 0 --reverse \
  --until '50 minutes ago' -n 1 --no-pager \
  --output-fields=__REALTIME_TIMESTAMP -o json 2>&1) || exit 34
boundary_entry_seconds=0
boundary_entry_age_seconds=0
if [ -n "$boundary_record" ]; then
  parse_journal_timestamp "$boundary_record" || exit 35
  boundary_entry_seconds=$(( journal_timestamp_us / 1000000 ))
fi

# Validate against a clock sampled after both bounded reads. Sampling before
# journalctl races an ordinary second rollover: a valid latest row can appear
# one second "in the future", and an exact 50-minute cutoff can look 2999s old.
now_seconds=$(date +%s) || exit 34
[ "$latest_entry_seconds" -le "$now_seconds" ] || exit 35
if [ -n "$boundary_record" ]; then
  boundary_entry_age_seconds=$(( now_seconds - boundary_entry_seconds ))
  [ "$boundary_entry_age_seconds" -ge 3000 ] || exit 35
fi

coverage_checked=0
coverage_present=0
if [ "${uptime_seconds:-0}" -ge 4200 ] && [ "$journald_active_seconds" -ge 4200 ]; then
  coverage_checked=1
  if [ "$boundary_entry_age_seconds" -ge 3000 ]; then
    coverage_present=1
  fi
fi

# Count the default system journal's machine directory, as journald does.
# File counts measure capacity, not the cause of rotation. Keep the machine ID
# and file metadata on-host, including when identity or visibility is unknown.
journal_file_scan_state=unavailable
journal_files=0
journal_bytes=0
journal_archived_files_5m=0
journal_system_archived_files_5m=0
journal_user_archived_files_5m=0
journal_machine_id=$(
  # SSH uses the account shell, which need not support pipefail. Read byte
  # codes so an awk that drops raw NULs cannot normalize a malformed ID.
  # Require the producer's successful terminal footer before emitting it.
  {
    timeout 5s od -An -v -tu1 -N 34 /etc/machine-id 2>/dev/null
    journal_machine_id_read_status=$?
    printf 'monitor-journal-machine-id-status=%d\n' "$journal_machine_id_read_status"
  } |
    LC_ALL=C awk '
      function fail() {failed=1; exit 1}
      /^monitor-journal-machine-id-status=/ {
        if (complete || $0 != "monitor-journal-machine-id-status=0") fail()
        complete=1
        next
      }
      complete || NF > 16 {fail()}
      NF == 0 {next}
      {
        for (i=1; i<=NF; i++) {
          if ($i !~ /^[0-9]+$/) fail()
          byte=$i+0
          bytes++
          if (bytes <= 32) {
            if (!((byte >= 48 && byte <= 57) || (byte >= 65 && byte <= 70) || (byte >= 97 && byte <= 102))) fail()
            id=id sprintf("%c", byte)
            if (byte != 48) nonzero=1
          } else if (bytes != 33 || byte != 10) fail()
        }
      }
      END {
        if (!failed && complete && nonzero && (bytes == 32 || bytes == 33)) print tolower(id)
        else exit 1
      }
    '
)
journal_machine_id_status=$?
journal_directory="/var/log/journal/$journal_machine_id"
if [ "$journal_machine_id_status" -eq 0 ] &&
   [ -d "$journal_directory" ] && [ ! -L "$journal_directory" ] &&
   [ -r "$journal_directory" ] && [ -x "$journal_directory" ]; then
  journal_file_now=$(date +%s) || exit 36
  journal_file_reduction=$(
    {
      timeout 10 find "$journal_directory" -xdev -mindepth 1 -maxdepth 1 -type f \
        \( -name '*.journal' -o -name '*.journal~' \) \
        -printf '%T@ %b %f\n' 2>/dev/null
      journal_find_status=$?
      printf 'monitor-journal-file-scan-status=%d\n' "$journal_find_status"
    } |
      LC_ALL=C awk -v now="$journal_file_now" '
        function fail(code) {failed=1; exit code}
        function hex(value, width) {return length(value) == width && value !~ /[^0-9a-fA-F]/}
        /^monitor-journal-file-scan-status=/ {
          if (complete || $0 != "monitor-journal-file-scan-status=0") fail(43)
          complete=1
          next
        }
        complete {fail(43)}
        NF != 3 {fail(43)}
        {
          mtime=$1
          blocks=$2
          name=$3
          if (mtime !~ /^[0-9]+([.][0-9]+)?$/ || blocks !~ /^[0-9]+$/) fail(43)
          if (name !~ /[.]journal~?$/) fail(43)
          files++
          if (files > 4096) fail(42)
          bytes += blocks * 512
          # Vacuum counts even unfamiliar or malformed archive names as
          # active files. Only canonical archive suffixes identify archives.
          archive=name
          sub(/[.]journal~?$/, "", archive)
          sub(/^.*@/, "", archive)
          parts=split(archive, fields, "-")
          is_archive=name ~ /@/ &&
            ((name ~ /[.]journal$/ && parts == 3 && hex(fields[1], 32) && hex(fields[2], 16) && hex(fields[3], 16)) ||
             (name ~ /[.]journal~$/ && parts == 2 && hex(fields[1], 16) && hex(fields[2], 16)))
          age=now-mtime
          if (age >= -2 && age <= 300 && is_archive) {
            archived++
            prefix=name
            sub(/@[^@]*$/, "", prefix)
            if (prefix == "system") system_archived++
            else if (prefix ~ /^user-[0-9]+$/) user_archived++
          }
        }
        END {
          if (!failed) {
            if (!complete) exit 43
            printf "%d %.0f %d %d %d\n", files+0, bytes+0,
              archived+0, system_archived+0, user_archived+0
          }
        }
      ' 2>/dev/null
  )
  journal_file_status=$?
  if [ "$journal_file_status" -eq 0 ]; then
    set -- $journal_file_reduction
    if [ "$#" -eq 5 ]; then
      journal_files=$1
      journal_bytes=$2
      journal_archived_files_5m=$3
      journal_system_archived_files_5m=$4
      journal_user_archived_files_5m=$5
      journal_file_scan_state=complete
    fi
  elif [ "$journal_file_status" -eq 42 ]; then
    journal_file_scan_state=truncated
  fi
fi

printf '%s\n' \
  'observation_schema=5' \
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
  "boundary_entry_age_seconds=${boundary_entry_age_seconds}" \
  'coverage_target_seconds=3000' \
  "journal_file_scan_state=${journal_file_scan_state}" \
  "journal_files=${journal_files}" \
  "journal_bytes=${journal_bytes}" \
  "journal_archived_files_5m=${journal_archived_files_5m}" \
  "journal_system_archived_files_5m=${journal_system_archived_files_5m}" \
  "journal_user_archived_files_5m=${journal_user_archived_files_5m}"
`

type journalBufferSample struct {
	journaldActive          string
	journaldActiveSeconds   int
	storage                 string
	maxUse                  string
	maxFileSize             string
	maxFiles                string
	maxFileSec              string
	maxRetention            string
	uptimeSeconds           int
	coverageChecked         bool
	coveragePresent         bool
	boundaryEntryAgeSeconds int
	coverageTargetSeconds   int
	journalFileScanState    string
	journalFiles            int
	journalBytes            uint64
	journalArchivedFiles5m  int
	journalSystemArchives5m int
	journalUserArchives5m   int
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
		"boundary_entry_age_seconds", "coverage_target_seconds", "journal_file_scan_state",
		"journal_files", "journal_bytes", "journal_archived_files_5m",
		"journal_system_archived_files_5m", "journal_user_archived_files_5m",
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
	if values["observation_schema"] != "5" {
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
	boundaryEntryAge, err := parseNonnegative("boundary_entry_age_seconds")
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
	if checked && present != (boundaryEntryAge >= coverageTarget) {
		return journalBufferSample{}, fmt.Errorf("journal buffer: inconsistent coverage result")
	}
	journalFileScanState := values["journal_file_scan_state"]
	if journalFileScanState != "complete" && journalFileScanState != "unavailable" && journalFileScanState != "truncated" {
		return journalBufferSample{}, fmt.Errorf("journal buffer: invalid journal file scan state")
	}
	journalFiles, err := parseNonnegative("journal_files")
	if err != nil {
		return journalBufferSample{}, err
	}
	journalBytes, err := strconv.ParseUint(values["journal_bytes"], 10, 64)
	if err != nil {
		return journalBufferSample{}, fmt.Errorf("journal buffer: invalid journal_bytes")
	}
	journalArchivedFiles5m, err := parseNonnegative("journal_archived_files_5m")
	if err != nil {
		return journalBufferSample{}, err
	}
	journalSystemArchives5m, err := parseNonnegative("journal_system_archived_files_5m")
	if err != nil {
		return journalBufferSample{}, err
	}
	journalUserArchives5m, err := parseNonnegative("journal_user_archived_files_5m")
	if err != nil {
		return journalBufferSample{}, err
	}
	if journalFileScanState != "complete" &&
		(journalFiles != 0 || journalBytes != 0 || journalArchivedFiles5m != 0 ||
			journalSystemArchives5m != 0 || journalUserArchives5m != 0) {
		return journalBufferSample{}, fmt.Errorf("journal buffer: incomplete journal file scan retained values")
	}
	if journalFileScanState == "complete" &&
		(journalFiles > 4096 || journalArchivedFiles5m > journalFiles ||
			journalSystemArchives5m > journalArchivedFiles5m ||
			journalUserArchives5m > journalArchivedFiles5m-journalSystemArchives5m) {
		return journalBufferSample{}, fmt.Errorf("journal buffer: inconsistent journal file counts")
	}
	return journalBufferSample{
		journaldActive: values["journald_active"], journaldActiveSeconds: activeSeconds,
		storage: values["storage"], maxUse: values["max_use"], maxFileSize: values["max_file_size"],
		maxFiles: values["max_files"], maxFileSec: values["max_file_sec"], maxRetention: values["max_retention"],
		uptimeSeconds: uptime, coverageChecked: checked, coveragePresent: present,
		boundaryEntryAgeSeconds: boundaryEntryAge, coverageTargetSeconds: coverageTarget,
		journalFileScanState: journalFileScanState, journalFiles: journalFiles,
		journalBytes: journalBytes, journalArchivedFiles5m: journalArchivedFiles5m,
		journalSystemArchives5m: journalSystemArchives5m,
		journalUserArchives5m:   journalUserArchives5m,
	}, nil
}

func evaluateJournalBuffer(target string, sample journalBufferSample) []finding {
	observed := fmt.Sprintf(
		"journald_active=%s journald_active_seconds=%d storage=%s max_use=%s max_file_size=%s max_files=%s max_file_sec=%s max_retention=%s uptime_seconds=%d coverage_checked=%t coverage_present=%t boundary_entry_age_seconds=%d coverage_target_seconds=%d",
		sample.journaldActive, sample.journaldActiveSeconds, sample.storage, sample.maxUse, sample.maxFileSize,
		sample.maxFiles, sample.maxFileSec, sample.maxRetention, sample.uptimeSeconds,
		sample.coverageChecked, sample.coveragePresent, sample.boundaryEntryAgeSeconds, sample.coverageTargetSeconds,
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

	if sample.journalFileScanState != "complete" {
		findings = append(findings, cannotObserveFinding(
			target+"/journal-buffer-files", fmt.Errorf("journal file metadata scan %s", sample.journalFileScanState),
		))
	} else if maxFiles, err := strconv.Atoi(sample.maxFiles); err != nil || maxFiles <= 0 {
		findings = append(findings, cannotObserveFinding(
			target+"/journal-buffer-file-headroom", fmt.Errorf("effective SystemMaxFiles is not a positive integer"),
		))
	} else {
		projectedArchives1h := sample.journalArchivedFiles5m * 12
		currentPercent := float64(sample.journalFiles) * 100 / float64(maxFiles)
		projectedPercent := float64(projectedArchives1h) * 100 / float64(maxFiles)
		fileObserved := fmt.Sprintf(
			"journal_files=%d system_max_files=%d current_file_capacity_pct=%.1f journal_bytes=%d journal_archived_files_5m=%d journal_system_archived_files_5m=%d journal_user_archived_files_5m=%d projected_archived_files_1h=%d projected_file_capacity_pct=%.1f",
			sample.journalFiles, maxFiles, currentPercent, sample.journalBytes,
			sample.journalArchivedFiles5m, sample.journalSystemArchives5m,
			sample.journalUserArchives5m, projectedArchives1h, projectedPercent,
		)
		if sample.journalFiles*4 > maxFiles || projectedArchives1h*4 > maxFiles {
			findings = append(findings, finding{
				probeId: "host/journal-buffer", tier: tierWarn,
				class: "journal-buffer-file-headroom", target: target, sustain: 2,
				symptom: fmt.Sprintf(
					"%s journal file count does not preserve the fourfold local recovery headroom",
					target,
				),
				mechanism: "Retained files or recent archive-file activity exceed the file-count headroom budget. SystemMaxFiles can constrain the recovery buffer independently of the byte ceiling; file counts alone do not identify the rotation trigger.",
				baseline:  "Current retained files and the five-minute archive-file count projected over one hour each use at most 25% of SystemMaxFiles, preserving at least fourfold file-count headroom alongside the byte cap.",
				observed:  fileObserved,
				evidence:  "The host reducer scans regular .journal and .journal~ files only in the validated local machine directory and returns allocated bytes and aggregate total/system/user counts. Machine identity, filenames, timestamps, entries, cursors, and workload identifiers never leave the host.",
				context:   "This is a local-capacity precursor. Recent archive mtimes approximate activity, not exact creation events or a future retention guarantee. Counts do not prove Fluent Bit loss, durable corruption, disk failure, or a responsible producer; correlate §11.14 before assigning data loss.",
				action:    "Resolve the rotation trigger with bounded journald reason evidence and the exact running systemd source/configuration before changing producer logging or capacity. Preserve the one-hour/100 GiB contract; evaluate SystemMaxFiles changes against inode cost, directory scan cost, and reader behavior. Do not delete journals, reboot, or restart the shipper to hide the count.",
				verify:    "Two consecutive five-minute observations keep both current and projected one-hour file use at or below 25% of SystemMaxFiles, the 50-minute boundary remains readable, §11.14 has zero iterator loss for ten minutes spanning ordinary rotation, and current-source Loki data stays fresh.",
				playbook:  "SIGNALS.md §8.5b and §11.14",
			})
		} else {
			findings = append(findings, healthyFinding(
				"host/journal-buffer", tierWarn, "journal-buffer-file-headroom", target,
			))
		}
	}

	if sample.coverageChecked && !sample.coveragePresent {
		findings = append(findings, finding{
			probeId: "host/journal-buffer", tier: tierWarn,
			class: "journal-buffer-short", target: target, sustain: 2,
			symptom:   fmt.Sprintf("%s retained less than 50 minutes of current-boot journal evidence", target),
			mechanism: "No readable current-boot record exists at or before the recovery cutoff even though both the host and journald have been active long enough to refill it. Size pressure, coarse whole-file rotation, or a journal failure removed the usable window.",
			baseline:  "After both host and journald have been active for 70 minutes, a readable current-boot record exists at or before the 50-minute cutoff while older records age into Loki.", observed: observed,
			evidence: fmt.Sprintf("boundary witness age=%ds, required=%ds", sample.boundaryEntryAgeSeconds, sample.coverageTargetSeconds),
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
