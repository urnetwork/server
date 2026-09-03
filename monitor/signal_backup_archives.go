package monitor

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	backupArchiveMetricFreshness = 90 * time.Second
	backupArchiveHeartbeatAge    = 90 * time.Second
	backupArchiveMaximumAge      = 5 * 24 * time.Hour
	backupArchiveFutureTolerance = 5 * time.Minute
	backupArchiveStorageLookback = 30 * 24 * time.Hour
	backupArchiveTimerImminent   = 5 * time.Minute
)

const backupArchiveWriterCommand = `# monitor-signal-11.22-backup-archives
github_unit_state=$(systemctl is-active github-backup-archive.service 2>/dev/null || true)
github_main_pid=$(systemctl show github-backup-archive.service -p MainPID --value 2>/dev/null || true)
remote_unit_state=$(systemctl show remote-backup-archive.service -p ActiveState --value 2>/dev/null || true)
remote_unit_substate=$(systemctl show remote-backup-archive.service -p SubState --value 2>/dev/null || true)
remote_main_pid=$(systemctl show remote-backup-archive.service -p MainPID --value 2>/dev/null || true)
remote_result=$(systemctl show remote-backup-archive.service -p Result --value 2>/dev/null || true)
remote_restart=$(systemctl show remote-backup-archive.service -p Restart --value 2>/dev/null || true)
remote_restart_delay=$(systemctl show remote-backup-archive.service -p RestartUSec --value 2>/dev/null || true)
remote_exit_status=$(systemctl show remote-backup-archive.service -p ExecMainStatus --value 2>/dev/null || true)
remote_invocation_id=$(systemctl show remote-backup-archive.service -p InvocationID --value 2>/dev/null || true)
remote_exec_start_monotonic=$(systemctl show remote-backup-archive.service -p ExecMainStartTimestampMonotonic --value 2>/dev/null || true)
remote_timer_state=$(systemctl show remote-backup-archive.timer -p ActiveState --value 2>/dev/null || true)
remote_timer_next=$(systemctl show remote-backup-archive.timer -p NextElapseUSecRealtime --value 2>/dev/null || true)
remote_timer_next_epoch=$(date -d "${remote_timer_next}" +%s 2>/dev/null || true)
remote_boot_epoch=$(awk '$1 == "btime" {print $2}' /proc/stat 2>/dev/null)
remote_environment=$(systemctl show remote-backup-archive.service -p Environment --value 2>/dev/null || true)
remote_pg_source=$(printf '%s\n' "${remote_environment}" | tr ' ' '\n' | sed -n 's/^BRINGYOUR_BACKUP_PG_REMOTE=//p' | tail -n 1)
remote_pg_port=$(printf '%s\n' "${remote_environment}" | tr ' ' '\n' | sed -n 's/^BRINGYOUR_BACKUP_PG_PORT=//p' | tail -n 1)
remote_redis_source=$(printf '%s\n' "${remote_environment}" | tr ' ' '\n' | sed -n 's/^BRINGYOUR_BACKUP_REDIS_REMOTE=//p' | tail -n 1)
remote_redis_port=$(printf '%s\n' "${remote_environment}" | tr ' ' '\n' | sed -n 's/^BRINGYOUR_BACKUP_REDIS_PORT=//p' | tail -n 1)
remote_mount=$(printf '%s\n' "${remote_environment}" | tr ' ' '\n' | sed -n 's/^BRINGYOUR_BACKUP_MOUNT=//p' | tail -n 1)
remote_mount_present=0
remote_mount_source=unknown
remote_mount_fstype=unknown
remote_mount_options=unknown
remote_mount_lineage=unknown
if test -n "${remote_mount}" && mountpoint -q -- "${remote_mount}"; then
	remote_mount_present=1
	remote_mount_source=$(findmnt -rn -T "${remote_mount}" -o SOURCE 2>/dev/null | head -n 1)
	remote_mount_fstype=$(findmnt -rn -T "${remote_mount}" -o FSTYPE 2>/dev/null | head -n 1)
	remote_mount_options=$(findmnt -rn -T "${remote_mount}" -o OPTIONS 2>/dev/null | head -n 1)
	remote_mount_lineage=$(lsblk -srno KNAME -- "${remote_mount_source}" 2>/dev/null |
		awk '$1 ~ /^[A-Za-z0-9._+-]+$/ && !seen[$1]++ {if (out != "") out=out ","; out=out $1} END {print out}')
fi
case "${github_unit_state}" in '') github_unit_state=unknown ;; esac
case "${github_main_pid}" in ''|*[!0-9]*) github_main_pid=0 ;; esac
case "${remote_unit_state}" in '') remote_unit_state=unknown ;; esac
case "${remote_unit_substate}" in '') remote_unit_substate=unknown ;; esac
case "${remote_main_pid}" in ''|*[!0-9]*) remote_main_pid=0 ;; esac
case "${remote_result}" in '') remote_result=unknown ;; esac
case "${remote_restart}" in '') remote_restart=unknown ;; esac
case "${remote_restart_delay}" in ''|*[!0-9a-z]*) remote_restart_delay=unknown ;; esac
case "${remote_exit_status}" in ''|*[!0-9]*) remote_exit_status=0 ;; esac
case "${remote_invocation_id}" in '') remote_invocation_id=none ;; esac
case "${remote_exec_start_monotonic}" in ''|*[!0-9]*) remote_exec_start_monotonic=0 ;; esac
case "${remote_timer_state}" in '') remote_timer_state=unknown ;; esac
case "${remote_timer_next_epoch}" in ''|*[!0-9]*) remote_timer_next_epoch=0 ;; esac
case "${remote_boot_epoch}" in ''|*[!0-9]*) remote_boot_epoch=0 ;; esac
case "${remote_pg_source}" in '') remote_pg_source=unknown ;; esac
case "${remote_pg_port}" in ''|*[!0-9]*) remote_pg_port=0 ;; esac
case "${remote_redis_source}" in '') remote_redis_source=unknown ;; esac
case "${remote_redis_port}" in ''|*[!0-9]*) remote_redis_port=0 ;; esac
case "${remote_mount}" in '') remote_mount=unknown ;; esac
case "${remote_mount_source}" in '') remote_mount_source=unknown ;; esac
case "${remote_mount_fstype}" in '') remote_mount_fstype=unknown ;; esac
case "${remote_mount_options}" in '') remote_mount_options=unknown ;; esac
case "${remote_mount_lineage}" in '') remote_mount_lineage=unknown ;; esac
remote_storage_journal_source=unavailable
remote_storage_journal_probe=$(sudo -n journalctl --since '30 days ago' --no-pager -n 1 -o short-unix _TRANSPORT=kernel 2>/dev/null || true)
if test -n "${remote_storage_journal_probe}"; then
	remote_storage_journal_source=sudo
else
	remote_storage_journal_probe=$(journalctl --since '30 days ago' --no-pager -n 1 -o short-unix _TRANSPORT=kernel 2>/dev/null || true)
	if test -n "${remote_storage_journal_probe}"; then
		remote_storage_journal_source=direct
	fi
fi
remote_storage_journal_readable=0
case "${remote_storage_journal_source}" in sudo|direct) remote_storage_journal_readable=1 ;; esac
printf 'github_unit_state=%s\n' "${github_unit_state}"
printf 'github_main_pid=%s\n' "${github_main_pid}"
printf 'remote_unit_state=%s\n' "${remote_unit_state}"
printf 'remote_unit_substate=%s\n' "${remote_unit_substate}"
printf 'remote_main_pid=%s\n' "${remote_main_pid}"
printf 'remote_result=%s\n' "${remote_result}"
printf 'remote_restart=%s\n' "${remote_restart}"
printf 'remote_restart_delay=%s\n' "${remote_restart_delay}"
printf 'remote_exit_status=%s\n' "${remote_exit_status}"
printf 'remote_invocation_id=%s\n' "${remote_invocation_id}"
printf 'remote_exec_start_monotonic=%s\n' "${remote_exec_start_monotonic}"
printf 'remote_timer_state=%s\n' "${remote_timer_state}"
printf 'remote_timer_next_epoch=%s\n' "${remote_timer_next_epoch}"
printf 'remote_boot_epoch=%s\n' "${remote_boot_epoch}"
printf 'remote_pg_source=%s\n' "${remote_pg_source}"
printf 'remote_pg_port=%s\n' "${remote_pg_port}"
printf 'remote_redis_source=%s\n' "${remote_redis_source}"
printf 'remote_redis_port=%s\n' "${remote_redis_port}"
printf 'remote_mount=%s\n' "${remote_mount}"
printf 'remote_mount_present=%s\n' "${remote_mount_present}"
printf 'remote_mount_source=%s\n' "${remote_mount_source}"
printf 'remote_mount_fstype=%s\n' "${remote_mount_fstype}"
printf 'remote_mount_options=%s\n' "${remote_mount_options}"
printf 'remote_mount_lineage=%s\n' "${remote_mount_lineage}"
printf 'remote_storage_journal_readable=%s\n' "${remote_storage_journal_readable}"
read_archive_kernel_journal() {
	case "${remote_storage_journal_source}" in
		sudo) sudo -n journalctl "$@" ;;
		direct) command journalctl "$@" ;;
		*) return 1 ;;
	esac
}
if test "${remote_storage_journal_readable}" = 1; then
	read_archive_kernel_journal --since '30 days ago' --no-pager -n 512 -o short-unix \
		--grep='uas_eh_(abort|device_reset)_handler|device offlined|not ready after error recovery|Synchronize Cache.*failed|rejecting I/O to offline device|I/O error|Aborting journal|Remounting filesystem read-only|emergency_ro' \
		_TRANSPORT=kernel 2>/dev/null |
	awk '
	function clean_device(value) {
		sub(/^\[/, "", value); sub(/^\(/, "", value)
		sub(/\]$/, "", value); sub(/\):$/, "", value)
		sub(/[,:;.]$/, "", value)
		if (value ~ /^dm-[0-9]+-[0-9]+$/) sub(/-[0-9]+$/, "", value)
		if (value !~ /^[A-Za-z0-9._+-]+$/) return ""
		return value
	}
	function bracket_device( i, value) {
		for (i=1; i<=NF; i++) if ($i ~ /^\[[A-Za-z0-9._+-]+\][,:;.]?$/) {
			value=clean_device($i); if (value != "") return value
		}
		return ""
	}
	function parenthesized_device( i, value) {
		for (i=1; i<=NF; i++) if ($i ~ /^\([A-Za-z0-9._+-]+\):?$/) {
			value=clean_device($i); if (value != "") return value
		}
		return ""
	}
	function device_after(word, i, value) {
		for (i=1; i<NF; i++) if ($i == word) {
			value=clean_device($(i+1)); if (value != "") return value
		}
		return ""
	}
	function emit(kind, device, epoch) {
		if (device == "" || epoch !~ /^[0-9]+$/) return
		printf "remote_storage_event=%s,%s,%s\n", epoch, kind, device
	}
	{
		epoch=$1; sub(/[.][0-9]+$/, "", epoch)
		if ($0 ~ /Aborting journal|JBD2:.*I\/O error|Remounting filesystem read-only|emergency_ro/) {
			device=parenthesized_device()
			if (device == "") device=device_after("device")
			if (device == "") device=device_after("for")
			emit("journal", device, epoch); next
		}
		if ($0 ~ /uas_eh_(abort|device_reset)_handler|device offlined|not ready after error recovery|Synchronize Cache.*failed|rejecting I\/O to offline device/) {
			device=bracket_device()
			if (device == "") device=device_after("dev")
			emit("transport", device, epoch); next
		}
		if ($0 ~ /I\/O error/) {
			device=device_after("dev")
			if (device == "") device=bracket_device()
			emit("block-io", device, epoch)
		}
	}'
fi`

var backupArchiveNames = []string{
	"pg",
	"redis",
	"github-urnetwork",
	"github-urfoundation",
}

// Signal backup-archives implements SIGNALS.md §11.22. It reads the exact
// Planetoid textfile series through Mimir so a healthy node exporter collector
// or Grafana frontend cannot hide missing or stale completed backup archives.
func NewBackupArchivesSignal() Signal {
	return &signalAdapter{
		number: "11.22", key: "backup-archives", name: "Planetoid backup archive freshness",
		probe: backupArchivesProbe{},
	}
}

type backupArchivesProbe struct{}

func (backupArchivesProbe) id() string             { return "observability/backup-archives" }
func (backupArchivesProbe) tier() string           { return tierPage }
func (backupArchivesProbe) cadence() time.Duration { return time.Minute }

type backupArchiveLatestSample struct {
	generation string
	createdAt  time.Time
}

type backupArchiveObservation struct {
	host             string
	archive          string
	latest           []backupArchiveLatestSample
	progress         []float64
	heartbeats       []time.Time
	invalidLatest    []string
	invalidProgress  []string
	invalidHeartbeat []string
	staleScrapes     int
}

type backupArchiveWriterObservation struct {
	host               string
	unitState          string
	mainPID            int64
	remoteUnitState    string
	remoteUnitSubstate string
	remoteMainPID      int64
	remoteResult       string
	remoteRestart      string
	remoteRestartDelay string
	remoteExitStatus   int64
	remoteInvocationID string
	remoteExecStart    int64
	remoteTimerState   string
	remoteTimerNext    time.Time
	remoteBoot         time.Time
	remotePGSource     string
	remotePGPort       int64
	remoteRedisSource  string
	remoteRedisPort    int64
	remoteMount        string
	remoteMountPresent bool
	remoteMountState   string
	remoteMountSource  string
	remoteMountFSType  string
	remoteMountOptions string
	remoteMountLineage []string
	storageReadable    bool
	storageEvents      []backupArchiveStorageEvent
}

type backupArchiveStorageEvent struct {
	occurredAt time.Time
	kind       string
	device     string
}

func (backupArchivesProbe) check(ctx context.Context, env *probeEnv) ([]finding, error) {
	backupHosts := env.cfg.hostsWithRole("backup")
	if len(backupHosts) == 0 {
		return nil, fmt.Errorf("backup archives: no backup host in monitor inventory")
	}
	metricHosts := env.cfg.hostsWithRole("services")
	if len(metricHosts) == 0 {
		return nil, fmt.Errorf("backup archives: no services host in inventory for the loopback Mimir query")
	}
	for _, host := range backupHosts {
		if err := validateBackupArchiveSourceSettings(host.backup); err != nil {
			return nil, fmt.Errorf("backup archives: %s: %w", host.name, err)
		}
	}

	queryURL := "http://127.0.0.1:3100/prometheus/api/v1/query?query=" +
		url.QueryEscape(backupArchivesQuery(env.cfg.env, backupHosts))
	out, metricHost, err := shellFirstServiceGateway(
		ctx,
		env.runner,
		metricHosts,
		nil,
		"curl -fsS --max-time 15 '"+queryURL+"'",
	)
	if err != nil {
		return nil, fmt.Errorf("backup archives: query Mimir through service gateways: %w", err)
	}

	var response mimirInstantResponse
	if err := json.Unmarshal([]byte(out), &response); err != nil {
		return nil, fmt.Errorf("backup archives: decode Mimir response: %w", err)
	}
	if response.Status != "success" || response.Data.ResultType != "vector" {
		return nil, fmt.Errorf(
			"backup archives: Mimir status=%q result_type=%q error=%q",
			response.Status,
			response.Data.ResultType,
			response.Error,
		)
	}

	expectedHosts := make(map[string]bool, len(backupHosts))
	observations := map[string]*backupArchiveObservation{}
	for _, host := range backupHosts {
		expectedHosts[host.name] = true
		for _, archive := range backupArchiveNames {
			key := backupArchiveKey(host.name, archive)
			observations[key] = &backupArchiveObservation{host: host.name, archive: archive}
		}
	}

	now := env.now().UTC()
	writers := make(map[string]backupArchiveWriterObservation, len(backupHosts))
	for _, host := range backupHosts {
		output, err := env.runner.shell(ctx, host, backupArchiveWriterCommand)
		if err != nil {
			return nil, fmt.Errorf("backup archives: inspect writer on %s: %w", host.name, err)
		}
		writer, err := parseBackupArchiveWriterObservation(host.name, output)
		if err != nil {
			return nil, fmt.Errorf("backup archives: parse writer on %s: %w", host.name, err)
		}
		writers[host.name] = writer
	}
	for _, series := range response.Data.Result {
		hostName := series.Metric["host"]
		archive := series.Metric["archive"]
		if !expectedHosts[hostName] || !isBackupArchiveName(archive) {
			continue
		}
		observation := observations[backupArchiveKey(hostName, archive)]
		observedAt, value, err := mimirInstantValue(series.Value)
		if err != nil {
			return nil, fmt.Errorf(
				"backup archives: parse %s sample for %s/%s: %w",
				series.Metric["__name__"], hostName, archive, err,
			)
		}
		scrapeAge := now.Sub(observedAt)
		if scrapeAge > backupArchiveMetricFreshness {
			observation.staleScrapes++
			continue
		}
		if scrapeAge < -30*time.Second {
			observation.invalidLatest = append(
				observation.invalidLatest,
				fmt.Sprintf("future_scrape=%s", observedAt.Format(time.RFC3339)),
			)
			continue
		}

		switch series.Metric["__name__"] {
		case "urnetwork_backup_archive_latest_timestamp_seconds":
			generation := strings.TrimSpace(series.Metric["generation"])
			if generation == "" || math.IsNaN(value) || math.IsInf(value, 0) || value <= 0 {
				observation.invalidLatest = append(
					observation.invalidLatest,
					fmt.Sprintf("generation=%q value=%v", generation, value),
				)
				continue
			}
			createdAt := unixFloatTime(value)
			if createdAt.After(now.Add(backupArchiveFutureTolerance)) {
				observation.invalidLatest = append(
					observation.invalidLatest,
					fmt.Sprintf("generation=%q future_timestamp=%s", generation, createdAt.Format(time.RFC3339)),
				)
				continue
			}
			observation.latest = append(observation.latest, backupArchiveLatestSample{
				generation: generation,
				createdAt:  createdAt,
			})
		case "urnetwork_backup_archive_in_progress":
			if math.IsNaN(value) || math.IsInf(value, 0) || (value != 0 && value != 1) {
				observation.invalidProgress = append(
					observation.invalidProgress,
					fmt.Sprintf("value=%v", value),
				)
				continue
			}
			observation.progress = append(observation.progress, value)
		case "urnetwork_backup_archive_heartbeat_timestamp_seconds":
			if math.IsNaN(value) || math.IsInf(value, 0) || value <= 0 {
				observation.invalidHeartbeat = append(
					observation.invalidHeartbeat,
					fmt.Sprintf("value=%v", value),
				)
				continue
			}
			heartbeatAt := unixFloatTime(value)
			if heartbeatAt.After(now.Add(backupArchiveFutureTolerance)) {
				observation.invalidHeartbeat = append(
					observation.invalidHeartbeat,
					fmt.Sprintf("future_heartbeat=%s", heartbeatAt.Format(time.RFC3339)),
				)
				continue
			}
			observation.heartbeats = append(observation.heartbeats, heartbeatAt)
		}
	}

	keys := make([]string, 0, len(observations))
	for key := range observations {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	findings := make([]finding, 0, len(keys)*2)
	for _, key := range keys {
		observation := observations[key]
		findings = append(findings, evaluateBackupArchive(
			now,
			observation,
			metricHost.name,
			writers[observation.host].remoteMountState,
			backupArchiveQueuedBehind(observation, writers[observation.host], observations),
		)...)
	}
	for _, host := range backupHosts {
		findings = append(findings, evaluateBackupArchiveVolume(writers[host.name]))
		findings = append(findings, evaluateBackupArchiveVolumeRecovery(now, writers[host.name])...)
		findings = append(findings, evaluateBackupArchiveRecoveryIdle(now, writers[host.name], observations))
		findings = append(findings, evaluateBackupArchiveWriter(now, writers[host.name], observations))
		findings = append(findings, evaluateBackupArchiveRun(writers[host.name]))
		findings = append(findings, evaluateBackupArchiveRetry(writers[host.name]))
		findings = append(findings, evaluateBackupArchiveSourceRoute(
			writers[host.name],
			host.backup,
		))
	}
	return findings, nil
}

func validateBackupArchiveSourceSettings(settings *BackupHostSettings) error {
	if settings == nil {
		return fmt.Errorf("direct backup source settings are missing")
	}
	for name, source := range map[string]string{
		"PostgreSQL": settings.PGSource,
		"Redis":      settings.RedisSource,
	} {
		if matched, _ := regexp.MatchString(`^by@[A-Za-z0-9][A-Za-z0-9.:-]{0,252}$`, source); !matched {
			return fmt.Errorf("invalid %s direct SSH source", name)
		}
		if strings.HasPrefix(source, "by@172.28.") {
			return fmt.Errorf("%s bulk source uses the management VPN", name)
		}
	}
	for name, port := range map[string]int{"PostgreSQL": settings.PGPort, "Redis": settings.RedisPort} {
		if port < 1 || port > 65535 {
			return fmt.Errorf("invalid %s direct SSH port", name)
		}
	}
	if settings.PGSource == settings.RedisSource && settings.PGPort == settings.RedisPort {
		return fmt.Errorf("PostgreSQL and Redis direct SSH endpoints are identical")
	}
	return nil
}

func parseBackupArchiveWriterObservation(hostName, output string) (backupArchiveWriterObservation, error) {
	values := map[string]string{}
	storageEvents := []backupArchiveStorageEvent{}
	for _, line := range strings.Split(strings.TrimSpace(output), "\n") {
		key, value, ok := strings.Cut(strings.TrimSpace(line), "=")
		if !ok || key == "" || value == "" {
			return backupArchiveWriterObservation{}, fmt.Errorf("invalid property line %q", line)
		}
		if key == "remote_storage_event" {
			event, err := parseBackupArchiveStorageEvent(value)
			if err != nil {
				return backupArchiveWriterObservation{}, err
			}
			if len(storageEvents) >= 512 {
				return backupArchiveWriterObservation{}, fmt.Errorf("too many remote_storage_event properties")
			}
			storageEvents = append(storageEvents, event)
			continue
		}
		if _, duplicate := values[key]; duplicate {
			return backupArchiveWriterObservation{}, fmt.Errorf("duplicate property %q", key)
		}
		values[key] = value
	}
	required := []string{
		"github_unit_state",
		"github_main_pid",
		"remote_unit_state",
		"remote_unit_substate",
		"remote_main_pid",
		"remote_result",
		"remote_restart",
		"remote_restart_delay",
		"remote_exit_status",
		"remote_invocation_id",
		"remote_exec_start_monotonic",
		"remote_timer_state",
		"remote_timer_next_epoch",
		"remote_boot_epoch",
		"remote_pg_source",
		"remote_pg_port",
		"remote_redis_source",
		"remote_redis_port",
		"remote_mount",
		"remote_mount_present",
		"remote_mount_source",
		"remote_mount_fstype",
		"remote_mount_options",
		"remote_mount_lineage",
		"remote_storage_journal_readable",
	}
	if len(values) != len(required) {
		return backupArchiveWriterObservation{}, fmt.Errorf("expected %d properties; got %q", len(required), strings.TrimSpace(output))
	}
	for _, key := range required {
		if _, ok := values[key]; !ok {
			return backupArchiveWriterObservation{}, fmt.Errorf("missing property %q", key)
		}
	}
	for _, key := range []string{"github_unit_state", "remote_unit_state", "remote_unit_substate", "remote_result", "remote_restart", "remote_timer_state"} {
		if matched, _ := regexp.MatchString(`^[a-z-]+$`, values[key]); !matched {
			return backupArchiveWriterObservation{}, fmt.Errorf("invalid %s %q", key, values[key])
		}
	}
	if matched, _ := regexp.MatchString(`^(unknown|(?:/[A-Za-z0-9._-]+)+/?)$`, values["remote_mount"]); !matched {
		return backupArchiveWriterObservation{}, fmt.Errorf("invalid remote_mount %q", values["remote_mount"])
	}
	if matched, _ := regexp.MatchString(`^(unknown|(?:/[A-Za-z0-9._-]+)+)$`, values["remote_mount_source"]); !matched {
		return backupArchiveWriterObservation{}, fmt.Errorf("invalid remote_mount_source %q", values["remote_mount_source"])
	}
	if matched, _ := regexp.MatchString(`^(unknown|[A-Za-z0-9._+-]+)$`, values["remote_mount_fstype"]); !matched {
		return backupArchiveWriterObservation{}, fmt.Errorf("invalid remote_mount_fstype %q", values["remote_mount_fstype"])
	}
	if matched, _ := regexp.MatchString(`^(unknown|[A-Za-z0-9,._=:+/-]+)$`, values["remote_mount_options"]); !matched {
		return backupArchiveWriterObservation{}, fmt.Errorf("invalid remote_mount_options %q", values["remote_mount_options"])
	}
	remoteMountLineage, err := parseBackupArchiveMountLineage(values["remote_mount_lineage"])
	if err != nil {
		return backupArchiveWriterObservation{}, err
	}
	remoteMountPresent, err := strconv.ParseBool(values["remote_mount_present"])
	if err != nil {
		return backupArchiveWriterObservation{}, fmt.Errorf("invalid remote_mount_present %q", values["remote_mount_present"])
	}
	storageReadable, err := strconv.ParseBool(values["remote_storage_journal_readable"])
	if err != nil {
		return backupArchiveWriterObservation{}, fmt.Errorf("invalid remote_storage_journal_readable %q", values["remote_storage_journal_readable"])
	}
	if matched, _ := regexp.MatchString(`^(unknown|[0-9]+(?:us|ms|s|min|h|d|w))$`, values["remote_restart_delay"]); !matched {
		return backupArchiveWriterObservation{}, fmt.Errorf("invalid remote_restart_delay %q", values["remote_restart_delay"])
	}
	mainPID, err := strconv.ParseInt(values["github_main_pid"], 10, 64)
	if err != nil || mainPID < 0 {
		return backupArchiveWriterObservation{}, fmt.Errorf("invalid main PID %q", values["github_main_pid"])
	}
	remoteMainPID, err := strconv.ParseInt(values["remote_main_pid"], 10, 64)
	if err != nil || remoteMainPID < 0 {
		return backupArchiveWriterObservation{}, fmt.Errorf("invalid remote main PID %q", values["remote_main_pid"])
	}
	remoteExitStatus, err := strconv.ParseInt(values["remote_exit_status"], 10, 64)
	if err != nil || remoteExitStatus < 0 {
		return backupArchiveWriterObservation{}, fmt.Errorf("invalid remote exit status %q", values["remote_exit_status"])
	}
	if matched, _ := regexp.MatchString(`^(none|unknown|[0-9a-f]{32})$`, values["remote_invocation_id"]); !matched {
		return backupArchiveWriterObservation{}, fmt.Errorf("invalid remote_invocation_id %q", values["remote_invocation_id"])
	}
	remoteExecStart, err := parseBackupArchiveNonnegativeInt64("remote_exec_start_monotonic", values["remote_exec_start_monotonic"])
	if err != nil {
		return backupArchiveWriterObservation{}, err
	}
	remoteTimerNextEpoch, err := parseBackupArchiveNonnegativeInt64("remote_timer_next_epoch", values["remote_timer_next_epoch"])
	if err != nil {
		return backupArchiveWriterObservation{}, err
	}
	remoteBootEpoch, err := parseBackupArchiveNonnegativeInt64("remote_boot_epoch", values["remote_boot_epoch"])
	if err != nil {
		return backupArchiveWriterObservation{}, err
	}
	for _, key := range []string{"remote_pg_source", "remote_redis_source"} {
		if matched, _ := regexp.MatchString(`^(unknown|[a-z][a-z0-9_-]{0,31}@[A-Za-z0-9][A-Za-z0-9.:-]{0,252})$`, values[key]); !matched {
			return backupArchiveWriterObservation{}, fmt.Errorf("invalid %s %q", key, values[key])
		}
	}
	remotePGPort, err := strconv.ParseInt(values["remote_pg_port"], 10, 64)
	if err != nil || remotePGPort < 0 || remotePGPort > 65535 {
		return backupArchiveWriterObservation{}, fmt.Errorf("invalid remote PostgreSQL port %q", values["remote_pg_port"])
	}
	remoteRedisPort, err := strconv.ParseInt(values["remote_redis_port"], 10, 64)
	if err != nil || remoteRedisPort < 0 || remoteRedisPort > 65535 {
		return backupArchiveWriterObservation{}, fmt.Errorf("invalid remote Redis port %q", values["remote_redis_port"])
	}
	return backupArchiveWriterObservation{
		host:               hostName,
		unitState:          values["github_unit_state"],
		mainPID:            mainPID,
		remoteUnitState:    values["remote_unit_state"],
		remoteUnitSubstate: values["remote_unit_substate"],
		remoteMainPID:      remoteMainPID,
		remoteResult:       values["remote_result"],
		remoteRestart:      values["remote_restart"],
		remoteRestartDelay: values["remote_restart_delay"],
		remoteExitStatus:   remoteExitStatus,
		remoteInvocationID: values["remote_invocation_id"],
		remoteExecStart:    remoteExecStart,
		remoteTimerState:   values["remote_timer_state"],
		remoteTimerNext:    unixIntegerTime(remoteTimerNextEpoch),
		remoteBoot:         unixIntegerTime(remoteBootEpoch),
		remotePGSource:     values["remote_pg_source"],
		remotePGPort:       remotePGPort,
		remoteRedisSource:  values["remote_redis_source"],
		remoteRedisPort:    remoteRedisPort,
		remoteMount:        values["remote_mount"],
		remoteMountPresent: remoteMountPresent,
		remoteMountState: backupArchiveMountState(
			values["remote_mount"],
			remoteMountPresent,
			values["remote_mount_options"],
		),
		remoteMountSource:  values["remote_mount_source"],
		remoteMountFSType:  values["remote_mount_fstype"],
		remoteMountOptions: values["remote_mount_options"],
		remoteMountLineage: remoteMountLineage,
		storageReadable:    storageReadable,
		storageEvents:      storageEvents,
	}, nil
}

func parseBackupArchiveNonnegativeInt64(name, value string) (int64, error) {
	parsed, err := strconv.ParseInt(value, 10, 64)
	if err != nil || parsed < 0 {
		return 0, fmt.Errorf("invalid %s %q", name, value)
	}
	return parsed, nil
}

func unixIntegerTime(epoch int64) time.Time {
	if epoch == 0 {
		return time.Time{}
	}
	return time.Unix(epoch, 0).UTC()
}

func parseBackupArchiveMountLineage(value string) ([]string, error) {
	if value == "unknown" {
		return nil, nil
	}
	parts := strings.Split(value, ",")
	seen := map[string]bool{}
	lineage := make([]string, 0, len(parts))
	for _, part := range parts {
		if matched, _ := regexp.MatchString(`^[A-Za-z0-9._+-]+$`, part); !matched || seen[part] {
			return nil, fmt.Errorf("invalid remote_mount_lineage %q", value)
		}
		seen[part] = true
		lineage = append(lineage, part)
	}
	return lineage, nil
}

func parseBackupArchiveStorageEvent(value string) (backupArchiveStorageEvent, error) {
	parts := strings.Split(value, ",")
	if len(parts) != 3 {
		return backupArchiveStorageEvent{}, fmt.Errorf("invalid remote_storage_event %q", value)
	}
	epoch, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil || epoch <= 0 {
		return backupArchiveStorageEvent{}, fmt.Errorf("invalid remote_storage_event epoch %q", parts[0])
	}
	switch parts[1] {
	case "transport", "block-io", "journal":
	default:
		return backupArchiveStorageEvent{}, fmt.Errorf("invalid remote_storage_event kind %q", parts[1])
	}
	if matched, _ := regexp.MatchString(`^[A-Za-z0-9._+-]+$`, parts[2]); !matched {
		return backupArchiveStorageEvent{}, fmt.Errorf("invalid remote_storage_event device %q", parts[2])
	}
	return backupArchiveStorageEvent{
		occurredAt: time.Unix(epoch, 0).UTC(),
		kind:       parts[1],
		device:     parts[2],
	}, nil
}

func backupArchiveMountState(configuredMount string, present bool, options string) string {
	if configuredMount == "unknown" {
		return "unknown"
	}
	if !present {
		return "missing"
	}
	optionSet := map[string]bool{}
	for _, option := range strings.Split(options, ",") {
		optionSet[option] = true
	}
	if optionSet["ro"] || optionSet["emergency_ro"] {
		return "read-only"
	}
	if optionSet["rw"] {
		return "read-write"
	}
	return "unknown"
}

func evaluateBackupArchiveVolume(observation backupArchiveWriterObservation) finding {
	target := observation.host + "/archive-volume"
	if observation.remoteMountState == "read-write" {
		return healthyFinding(
			"observability/backup-archives", tierPage, "backup-archive-volume-unavailable", target,
		)
	}

	symptom := fmt.Sprintf("%s archive volume is unavailable", observation.host)
	mechanism := "The data-backup unit's configured archive mount is absent or cannot be proven writable, so no PostgreSQL or Redis generation can be published safely. A running unit or in-progress metric does not make that destination usable."
	if observation.remoteMountState == "read-only" {
		symptom = fmt.Sprintf("%s archive volume is mounted read-only", observation.host)
		mechanism = "The exact configured archive mount reports read-only or ext4 emergency_ro state. The kernel rejects archive writes after an explicit read-only mount or a protective filesystem remount; the unit's process and in-progress metric can remain present after the destination has stopped accepting data."
	} else if observation.remoteMountState == "missing" {
		symptom = fmt.Sprintf("%s archive volume is not mounted", observation.host)
		mechanism = "The exact mount configured on remote-backup-archive.service is absent. An external device that is offlined and later re-enumerated can leave the old LUKS mapper detached while the same physical volume appears under a new /dev name; an old process or progress gauge does not reconnect that storage."
	}

	return finding{
		probeId: "observability/backup-archives", tier: tierPage,
		class: "backup-archive-volume-unavailable", target: target, frame: "unit=remote-backup-archive.service", sustain: 1,
		symptom:   symptom,
		mechanism: mechanism,
		baseline:  "BRINGYOUR_BACKUP_MOUNT names a real mounted filesystem whose direct findmnt options include rw and exclude ro and emergency_ro before any archive writer is treated as active.",
		observed: fmt.Sprintf(
			"mount=%s mount_present=%t mount_state=%s source=%s fstype=%s options=%s unit_state=%s unit_substate=%s main_pid=%d",
			observation.remoteMount,
			observation.remoteMountPresent,
			observation.remoteMountState,
			observation.remoteMountSource,
			observation.remoteMountFSType,
			observation.remoteMountOptions,
			observation.remoteUnitState,
			observation.remoteUnitSubstate,
			observation.remoteMainPID,
		),
		evidence: "The probe reads the non-secret BRINGYOUR_BACKUP_MOUNT value from the effective systemd environment, verifies that exact path with mountpoint, and classifies its direct findmnt options. It does not infer volume health from free-space metrics or a live rsync PID.",
		context:  "This class needs operational and potentially hardware repair: software cannot reconnect an external SSD, repair a cable or enclosure, or replace failed media. Do not live-remount an ext4 filesystem read-write after an aborted journal, do not unlock by mutable /dev/sdX name, and do not start another writer against the bare mountpoint directory.",
		action:   "With explicit operator authorization, stop both archive writer units and verify their rsync/tar/xz children are gone. Identify the reappeared partition by its stable LUKS UUID, inspect bounded kernel USB/UAS and block-I/O evidence plus SMART data when the bridge exposes it, and repair or replace the proven cable, port, enclosure, or SSD fault. Close only the stale unmapped LUKS device after proving it has no mount or holders; unlock the current UUID, run e2fsck offline, then mount it normally. Before resuming one catch-up generation, perform a bounded write/read/delete check on the mounted archive and confirm no new transport, block-I/O, journal, or emergency-read-only event.",
		verify:   "Require three consecutive one-minute direct observations of the same LUKS-backed mount in read-write state, a clean offline filesystem check, a successful bounded write/read/delete check, no new USB/UAS reset, block-I/O, journal-abort, or read-only-remount event for 30 minutes, and one subsequent single-writer archive generation whose artifact and manifest validate.",
		playbook: "SIGNALS.md §11.22",
	}
}

func evaluateBackupArchiveVolumeRecovery(
	now time.Time,
	observation backupArchiveWriterObservation,
) []finding {
	target := observation.host + "/archive-volume"
	visibilityClass := "backup-archive-volume-history-unobservable"
	recoveryClass := "backup-archive-volume-recovery-unverified"
	if observation.remoteMountState != "read-write" {
		return []finding{
			healthyFinding("observability/backup-archives", tierWarn, visibilityClass, target),
			healthyFinding("observability/backup-archives", tierPage, recoveryClass, target),
		}
	}

	lineage := make(map[string]bool, len(observation.remoteMountLineage))
	for _, device := range observation.remoteMountLineage {
		lineage[device] = true
	}
	if !observation.storageReadable || len(lineage) == 0 {
		reason := "kernel-journal-unreadable"
		if observation.storageReadable {
			reason = "archive-block-lineage-unknown"
		}
		return []finding{
			{
				probeId: "observability/backup-archives", tier: tierWarn,
				class: visibilityClass, target: target, frame: "storage-history", sustain: 2,
				symptom:   fmt.Sprintf("%s recent storage-fault history cannot be bound to its mounted archive volume", observation.host),
				mechanism: "A current read-write mount is only a point-in-time state. Without a readable bounded kernel journal and the current mapper/backing-device lineage, the monitor cannot tell whether USB/UAS, block-I/O, or journal-abort evidence preceded that state, and it must not attribute an unrelated disk's errors to the archive.",
				baseline:  "The monitor can read the last 30 days of kernel records and resolve the configured archive mount through its current mapper, partition, and backing-disk kernel names.",
				observed: fmt.Sprintf(
					"reason=%s journal_readable=%t mount=%s mount_state=%s source=%s lineage=%s",
					reason,
					observation.storageReadable,
					observation.remoteMount,
					observation.remoteMountState,
					observation.remoteMountSource,
					backupArchiveLineageText(observation.remoteMountLineage),
				),
				evidence: "The host-side discriminator returns only journal readability, sanitized block names, and normalized event type/device/time triples; raw kernel text does not leave the backup host.",
				context:  "This is UNKNOWN storage history, not proof of a healthy volume and not permission to infer that another local disk failed. Kernel block names can also change across re-enumeration, so stable LUKS and device identity remain operator discriminators.",
				action:   "Restore read-only journal visibility or block-lineage resolution. Then rerun this signal before starting an archive writer; do not weaken the lineage filter or treat an unrelated device event as the archive cause.",
				verify:   "Two consecutive probes resolve the same mounted LUKS-backed lineage and return bounded normalized history; any matching event is handled by backup-archive-volume-recovery-unverified.",
				playbook: "SIGNALS.md §11.22",
			},
		}
	}

	counts := map[string]int{"transport": 0, "block-io": 0, "journal": 0}
	devices := map[string]bool{}
	latest := time.Time{}
	invalidFuture := 0
	windowStart := now.Add(-backupArchiveStorageLookback)
	for _, event := range observation.storageEvents {
		if !lineage[event.device] {
			continue
		}
		if event.occurredAt.After(now.Add(backupArchiveFutureTolerance)) {
			invalidFuture++
			continue
		}
		if event.occurredAt.Before(windowStart) {
			continue
		}
		counts[event.kind]++
		devices[event.device] = true
		if latest.IsZero() || event.occurredAt.After(latest) {
			latest = event.occurredAt
		}
	}
	visibility := healthyFinding(
		"observability/backup-archives", tierWarn, visibilityClass, target,
	)
	if invalidFuture > 0 {
		return []finding{visibility, {
			probeId: "observability/backup-archives", tier: tierWarn,
			class: visibilityClass, target: target, frame: "storage-history-clock", sustain: 1,
			symptom:   fmt.Sprintf("%s archive-bound kernel storage history has future timestamps", observation.host),
			mechanism: "The normalized event clock is too far ahead of the monitor clock, so the bounded recovery window cannot be evaluated safely.",
			baseline:  "Archive-bound kernel event timestamps are no more than five minutes in the future.",
			observed:  fmt.Sprintf("future_events=%d lineage=%s", invalidFuture, backupArchiveLineageText(observation.remoteMountLineage)),
			evidence:  "Only normalized event metadata was returned; raw kernel text remains on the host.",
			action:    "Repair the host or monitor clock and rerun the signal before using storage history as a recovery gate.",
			verify:    "Two consecutive probes contain no future normalized storage event.",
			playbook:  "SIGNALS.md §11.22",
		}}
	}
	if latest.IsZero() {
		return []finding{visibility, healthyFinding(
			"observability/backup-archives", tierPage, recoveryClass, target,
		)}
	}

	deviceNames := make([]string, 0, len(devices))
	for device := range devices {
		deviceNames = append(deviceNames, device)
	}
	sort.Strings(deviceNames)
	return []finding{visibility, {
		probeId: "observability/backup-archives", tier: tierPage,
		class: recoveryClass, target: target, frame: "recent-storage-fault", sustain: 1,
		symptom:   fmt.Sprintf("%s archive volume is read-write after recent storage-path failures", observation.host),
		mechanism: "The bounded kernel history contains USB/UAS transport, direct block-I/O, or ext4/JBD2 failure evidence naming a device in the currently mounted archive mapper/backing-device lineage. Journal replay and a fresh read-write mount restore present access; they do not prove an offline full-filesystem check or repair the cable, port, enclosure, bridge, or SSD that caused the I/O loss.",
		baseline:  "The archive volume has no lineage-bound transport, block-I/O, journal-abort, emergency-read-only, or read-only-remount event in the bounded 30-day evidence window.",
		observed: fmt.Sprintf(
			"mount=%s mount_state=%s source=%s lineage=%s matched_devices=%s transport_events=%d block_io_events=%d journal_events=%d latest_event=%s lookback=%s",
			observation.remoteMount,
			observation.remoteMountState,
			observation.remoteMountSource,
			backupArchiveLineageText(observation.remoteMountLineage),
			strings.Join(deviceNames, ","),
			counts["transport"],
			counts["block-io"],
			counts["journal"],
			latest.Format(time.RFC3339),
			backupArchiveStorageLookback,
		),
		evidence: "The probe resolves the configured mount through its current lsblk -s kernel lineage and joins only exact device tokens to normalized event type/device/time triples. Raw kernel text stays on the host; events naming other block devices are excluded.",
		context:  "A match to a current kernel name excludes an unrelated currently named device, but kernel names are mutable across detach/re-enumeration. Confirm the stable LUKS UUID and physical serial before replacing hardware. This page intentionally remains active for the full 30-day evidence window when repaired hardware retains the same kernel lineage. The 30-minute fault-free interval is only the minimum probation before a catch-up attempt, not an automated alert-clear condition; expiry of the bounded journal window is not a repair certificate.",
		action:   "Keep both archive writers stopped. With explicit operator authorization, bind the mounted mapper to the stable LUKS UUID and physical device, collect SMART/media evidence if the bridge supports it, and isolate or replace the proven cable, port, enclosure, bridge, or SSD fault. Unmount and run a full offline e2fsck; a boot-time journal replay is insufficient. After mounting normally, perform a bounded write/read/delete check before authorizing exactly one catch-up writer.",
		verify:   "Operational closure requires the stable LUKS-backed volume to pass a full offline filesystem check, three consecutive one-minute read-write observations, a bounded write/read/delete check, and 30 minutes with no new lineage-bound transport, block-I/O, journal-abort, emergency-read-only, or remount event. Then require one single-writer generation to complete, validate its artifact and manifest, and publish the same new generation on two direct Mimir reads. The page deliberately retains the event until it leaves the 30-day window (or a proven replacement has a distinct lineage); neither alert disappearance nor the 30-minute probation alone is closure.",
		playbook: "SIGNALS.md §11.22",
	}}
}

func backupArchiveLineageText(lineage []string) string {
	if len(lineage) == 0 {
		return "unknown"
	}
	return strings.Join(lineage, ",")
}

func evaluateBackupArchiveRecoveryIdle(
	now time.Time,
	writer backupArchiveWriterObservation,
	observations map[string]*backupArchiveObservation,
) finding {
	target := writer.host + "/remote"
	class := "backup-archive-recovery-idle"
	required := backupArchiveDataRecoveryRequired(now, writer.host, observations)
	if len(required) == 0 || writer.remoteMountState != "read-write" ||
		writer.remoteUnitState != "inactive" || writer.remoteUnitSubstate != "dead" ||
		writer.remoteMainPID != 0 || writer.remoteInvocationID != "none" || writer.remoteExecStart != 0 ||
		(!writer.remoteTimerNext.IsZero() && !writer.remoteTimerNext.After(now.Add(backupArchiveTimerImminent))) {
		return healthyFinding(
			"observability/backup-archives", tierPage, class, target,
		)
	}

	nextTrigger := "unknown"
	if !writer.remoteTimerNext.IsZero() {
		nextTrigger = writer.remoteTimerNext.Format(time.RFC3339)
	}
	boot := "unknown"
	if !writer.remoteBoot.IsZero() {
		boot = writer.remoteBoot.Format(time.RFC3339)
	}
	mechanism := "The PostgreSQL/Redis recovery objective is unresolved, but systemd has no current-boot invocation and no writer or on-failure retry is active. Result=success and ExecMainStatus=0 are manager defaults in this empty InvocationID state, not evidence that a post-reboot archive run succeeded. The persistent timer already consumed its earlier calendar trigger; its next trigger is still in the future. A reboot therefore discarded the pre-reboot retry/backoff without creating a replacement attempt."
	if writer.remoteTimerNext.IsZero() || writer.remoteTimerState != "active" {
		mechanism = "The PostgreSQL/Redis recovery objective is unresolved, but systemd has no current-boot invocation, no writer or on-failure retry is active, and the timer has no proven active future trigger. Result=success and ExecMainStatus=0 are manager defaults in this empty InvocationID state, not evidence that a post-reboot archive run succeeded."
	}
	return finding{
		probeId: "observability/backup-archives", tier: tierPage,
		class: class, target: target, frame: "post-reboot-no-invocation", sustain: 1,
		symptom:   fmt.Sprintf("%s data archive recovery is idle after reboot", target),
		mechanism: mechanism,
		baseline:  "When PostgreSQL or Redis completion is missing or older than five days and the archive mount is usable, the data unit is executing, waiting in a bounded on-failure restart, has a real current-boot invocation result, or has an imminent timer trigger.",
		observed: fmt.Sprintf(
			"recovery_required=%s unit_state=%s unit_substate=%s main_pid=%d invocation_id=%s exec_start_monotonic=%d result=%s exit_status=%d timer_state=%s timer_next=%s boot=%s mount_state=%s",
			strings.Join(required, ","),
			writer.remoteUnitState,
			writer.remoteUnitSubstate,
			writer.remoteMainPID,
			writer.remoteInvocationID,
			writer.remoteExecStart,
			writer.remoteResult,
			writer.remoteExitStatus,
			writer.remoteTimerState,
			nextTrigger,
			boot,
			writer.remoteMountState,
		),
		evidence: "The monitor joins raw Mimir completion state with the effective service InvocationID/start/result, timer state/next trigger, current boot time, and exact archive mount state. It does not call systemctl start or infer success from default zero-valued properties.",
		context:  "This is a post-recovery scheduling gap, separate from the physical storage cause and the erased off-volume completion rows. A direct SSH banner and an enp65s0 route prove only present network reachability; they do not repair the archive filesystem, authenticate the source listing, preserve a multi-hour session, or create a recovery point.",
		action:   "Do not wait for the next calendar trigger and do not start the data writer merely to clear this alert. First satisfy the simultaneous storage-recovery gates. With the volume proven safe and both writers still inactive, run only the bounded metrics refresh to reconstruct real completed rows from disk; validate those rows against the root-owned artifacts. Then obtain explicit operator authorization for exactly one catch-up data pull through the configured direct endpoints.",
		verify:   "The metrics refresh exposes the real stored PostgreSQL and Redis generations on two direct Mimir reads without starting a pull. After storage clearance and explicit authorization, exactly one current-boot InvocationID/MainPID runs; both direct source authentications succeed, the unit completes, artifacts and manifests validate, and two direct Mimir reads show both recovery points inside five days. The unit then becomes normally inactive with a real invocation result.",
		playbook: "SIGNALS.md §11.22",
	}
}

func backupArchiveDataRecoveryRequired(
	now time.Time,
	host string,
	observations map[string]*backupArchiveObservation,
) []string {
	required := []string{}
	for _, archive := range []string{"pg", "redis"} {
		observation := observations[backupArchiveKey(host, archive)]
		if observation == nil || len(observation.latest) == 0 {
			required = append(required, archive+":missing")
			continue
		}
		latest := observation.latest[0].createdAt
		for _, sample := range observation.latest[1:] {
			if sample.createdAt.After(latest) {
				latest = sample.createdAt
			}
		}
		if now.Sub(latest) > backupArchiveMaximumAge {
			required = append(required, archive+":stale")
		}
	}
	return required
}

func backupArchiveRemoteWriterRunning(observation backupArchiveWriterObservation) bool {
	switch observation.remoteUnitState {
	case "active", "activating", "reloading":
		return 0 < observation.remoteMainPID && observation.remoteUnitSubstate != "auto-restart"
	default:
		return false
	}
}

const backupArchiveNetworkPathGuidance = "A bounded TCP path trace that exposes carrier-private or ECMP hops makes upstream multi-egress NAT a candidate, but does not assign reset ownership. Resolve each source-observed public egress through the authoritative RIR: if sibling failures arrive through addresses owned by independent carriers and terminate against independent source hosts within seconds, one carrier and one source daemon are no longer the common fault domain. That cross-carrier control narrows the remaining shared stateful path to the offsite gateway/conntrack boundary or the destination public-forward gateway; it still does not choose between those two routers. A NetworkManager IPv6 route/DNS reselection is not an IPv4-reset cause by itself: an isolated reselection while the same transfer survives is a negative control. Repeated reselection bursts bracketing resets, with no link-carrier loss and no whole-site Internet transition, support a narrower router/WAN/NAT/RA lifecycle event, but are not proof that NetworkManager reset IPv4. Require paired UDM and destination-forward WAN-event/config/conntrack evidence plus carrier NAT/session evidence before choosing that owner. If the direct WAN cannot retain active multi-hour TCP state, the network closure is stable public/no-CGNAT egress or another approved direct WAN path, never the management VPN."

func evaluateBackupArchiveRun(observation backupArchiveWriterObservation) finding {
	target := observation.host + "/remote"
	if backupArchiveRemoteWriterRunning(observation) ||
		(observation.remoteUnitState != "failed" &&
			observation.remoteUnitSubstate != "auto-restart" &&
			observation.remoteResult == "success" &&
			observation.remoteExitStatus == 0) {
		return healthyFinding(
			"observability/backup-archives", tierPage, "backup-archive-run-failed", target,
		)
	}

	symptom := fmt.Sprintf("%s data archive pull failed", target)
	mechanism := "The last remote-backup-archive.service ExecStart terminated unsuccessfully, so no atomic PostgreSQL or Redis recovery point was published. A configured retry policy limits how long the failure persists; it does not make the failed attempt healthy."
	context := "Rsync's partial directory is intentionally resumable and incomplete files do not match the generation parser. Preserve that partial rather than renaming it or starting a concurrent writer. A simultaneous no-route failure to an unrelated public control is evidence about Planetoid's Internet path, not either backup source."
	action := "Read the bounded unit journal, verify the mounted archive and both direct SSH endpoints, then correlate the failure timestamp with Planetoid's NetworkManager connectivity state, an unrelated public control such as its VPN endpoint, and each source sshd journal before changing rsync. Concurrent no-route failures to the backup endpoint and the independent public control localize Planetoid's router or upstream Internet path. If independent Internet stayed healthy, a long session with no orderly source close plus a sibling flow that vanishes either before authentication or after authentication without an orderly source close localizes shared direct-path infrastructure, not either source host. Compare the source-observed public egress identity across same-endpoint retries; an identity change is Planetoid WAN/NAT evidence, while a stable identity does not distinguish Planetoid gateway policy from the Fremont public-forward edge. " + backupArchiveNetworkPathGuidance + " Obtain bounded router lifecycle/conntrack evidence before assigning either one. Repair the first proven dependency. If systemd has no retry scheduled, obtain operator authorization before starting one catch-up run."
	verify := "Exactly one subsequent unit generation runs with a nonzero MainPID, resumes the partial over the configured direct endpoint, exits successfully, validates both artifacts and manifests, and publishes fresh completed generations in two direct Mimir reads."
	if observation.remoteUnitSubstate == "auto-restart" {
		symptom = fmt.Sprintf("%s data archive pull failed and is waiting in systemd restart backoff", target)
		mechanism = "The last remote-backup-archive.service ExecStart exited unsuccessfully. Systemd keeps ActiveState=activating while SubState=auto-restart waits for RestartUSec, but no archive writer is active during the restart backoff and the failed attempt produced no atomic recovery point."
		action = "Do not manually start or duplicate the unit while its bounded on-failure retry is scheduled. Preserve the rsync partial, read the bounded journal, and correlate the failure timestamp with Planetoid's NetworkManager connectivity state, an unrelated public control such as its VPN endpoint, and both source sshd journals before changing rsync. Concurrent no-route failures to both the backup endpoint and the independent public control localize Planetoid's router or upstream Internet path. If independent Internet stayed healthy, a long session with no orderly source close plus a sibling flow that vanishes either before authentication or after authentication without an orderly source close localizes shared direct-path infrastructure, not either source host. Compare the source-observed public egress identity across same-endpoint retries; an identity change is Planetoid WAN/NAT evidence, while a stable identity does not distinguish Planetoid gateway policy from the Fremont public-forward edge. " + backupArchiveNetworkPathGuidance + " Obtain bounded router lifecycle/conntrack evidence before assigning either one. Verify both direct SSH forwards and observe the next systemd-owned attempt. Escalate the first proven network or source boundary if that attempt cannot sustain progress."
	}
	return finding{
		probeId: "observability/backup-archives", tier: tierPage,
		class: "backup-archive-run-failed", target: target,
		frame: "unit=remote-backup-archive.service", sustain: 1,
		symptom:   symptom,
		mechanism: mechanism,
		baseline:  "An active data pull has a nonzero MainPID. Otherwise the last completed remote-backup-archive.service invocation has Result=success and ExecMainStatus=0; auto-restart backoff is reported as a failed attempt, not live progress.",
		observed: fmt.Sprintf(
			"unit_state=%s unit_substate=%s main_pid=%d result=%s exit_status=%d restart=%s restart_delay=%s",
			observation.remoteUnitState,
			observation.remoteUnitSubstate,
			observation.remoteMainPID,
			observation.remoteResult,
			observation.remoteExitStatus,
			observation.remoteRestart,
			observation.remoteRestartDelay,
		),
		evidence: "ActiveState, SubState, MainPID, Result, ExecMainStatus, Restart, and RestartUSec are read directly from the effective unit on the configured backup host.",
		context:  context,
		action:   action,
		verify:   verify,
		playbook: "SIGNALS.md §11.22",
	}
}

func evaluateBackupArchiveSourceRoute(
	observation backupArchiveWriterObservation,
	expected *BackupHostSettings,
) finding {
	target := observation.host + "/remote-sources"
	if observation.remotePGSource == expected.PGSource && observation.remotePGPort == int64(expected.PGPort) &&
		observation.remoteRedisSource == expected.RedisSource && observation.remoteRedisPort == int64(expected.RedisPort) {
		return healthyFinding(
			"observability/backup-archives", tierPage, "backup-archive-source-route", target,
		)
	}
	return finding{
		probeId: "observability/backup-archives", tier: tierPage,
		class: "backup-archive-source-route", target: target,
		frame: "unit=remote-backup-archive.service", sustain: 1,
		symptom:   fmt.Sprintf("%s is not configured to pull both bulk backups through their dedicated direct SSH endpoints", observation.host),
		mechanism: "PostgreSQL and Redis generations are hundreds of GiB. Sending those bytes through Planetoid's management OpenVPN tunnel couples recovery to the tunnel's throughput and reconnects; a tunnel reset terminates rsync before an atomic generation can be published. The dedicated direct SSH forwards bypass that control-plane bottleneck.",
		baseline:  fmt.Sprintf("The effective unit uses PostgreSQL %s:%d and Redis %s:%d, matching the configured direct endpoints exactly.", expected.PGSource, expected.PGPort, expected.RedisSource, expected.RedisPort),
		observed: fmt.Sprintf(
			"pg_source=%s pg_port=%d redis_source=%s redis_port=%d",
			observation.remotePGSource,
			observation.remotePGPort,
			observation.remoteRedisSource,
			observation.remoteRedisPort,
		),
		evidence: "The monitor reads only the four non-secret BRINGYOUR_BACKUP_*_REMOTE/PORT values from the effective systemd Environment and compares them with the configured direct bulk-transfer endpoints.",
		context:  "The monitor itself may reach Planetoid over the management overlay; only archive payload traffic must bypass it. Installing a corrected unit does not change the already-running process environment, authorize interruption, or prove a copy completed.",
		action:   "Deploy the corrected Xops Planetoid unit with main/ansible/run-planetoid.sh. Do not restart or interrupt an active archive transfer without explicit operator authorization. Before the next attempt, require both direct ports to accept connections and the installed fleet key to authenticate read-only to the intended source directories.",
		verify:   "The effective unit matches both exact direct endpoints; public route lookup does not select tun0; authenticated source listings succeed on both forwarded ports; the next authorized oneshot exits successfully; and artifact, manifest, and two direct Mimir checks expose the new generations.",
		playbook: "SIGNALS.md §11.22",
	}
}

func evaluateBackupArchiveRetry(observation backupArchiveWriterObservation) finding {
	target := observation.host + "/remote"
	if observation.remoteRestart == "on-failure" && observation.remoteRestartDelay == "30min" {
		return healthyFinding(
			"observability/backup-archives", tierPage, "backup-archive-retry-disabled", target,
		)
	}
	return finding{
		probeId: "observability/backup-archives", tier: tierPage,
		class: "backup-archive-retry-disabled", target: target,
		frame: "unit=remote-backup-archive.service", sustain: 1,
		symptom:   fmt.Sprintf("%s cannot recover automatically when its encrypted archive mount appears after the daily trigger", target),
		mechanism: "The persistent timer starts the PostgreSQL/Redis archive pull once at 04:00. The oneshot waits 15 minutes for the udisks mount, but without an on-failure restart it remains failed after that deadline even if an operator mounts the encrypted disk later. On 2026-09-01 this left the August 20 recovery points unchanged for the rest of the day.",
		baseline:  "The effective remote-backup-archive.service policy is Restart=on-failure with RestartUSec=30min. A successful oneshot remains inactive and does not repeat.",
		observed: fmt.Sprintf(
			"unit_state=%s result=%s exit_status=%d restart=%s restart_delay=%s",
			observation.remoteUnitState,
			observation.remoteResult,
			observation.remoteExitStatus,
			observation.remoteRestart,
			observation.remoteRestartDelay,
		),
		evidence: "The monitor reads ActiveState, Result, ExecMainStatus, Restart, and RestartUSec from the effective systemd unit on the configured backup host; it does not infer policy from the source template or Grafana.",
		context:  "Xops commit 2311114 supplies the software retry. It cannot unlock LUKS, attach unavailable media, replace a failed disk, or create capacity; those remain operator or hardware work. Installing the unit does not authorize a catch-up pull.",
		action:   "After the active GitHub archive writer has safely completed, deploy a clean Xops descendant of 2311114 with main/ansible/run-planetoid.sh. Do not restart the healthy GitHub compression. Obtain operator authorization before starting a catch-up data pull, and unlock/mount the archive disk first.",
		verify:   "After daemon-reload, require direct systemctl properties Restart=on-failure and RestartUSec=30min. On the next genuine prerequisite failure, require systemd to schedule a delayed retry; after the disk is mounted, require exactly one pull to complete, validate its artifacts and manifests, and confirm the unit remains inactive rather than repeating.",
		playbook: "SIGNALS.md §11.22",
	}
}

func evaluateBackupArchiveWriter(
	now time.Time,
	writer backupArchiveWriterObservation,
	observations map[string]*backupArchiveObservation,
) finding {
	target := writer.host + "/github"
	progressTotal := float64(0)
	progressSeries := 0
	progressComplete := true
	heartbeatSeries := 0
	heartbeatComplete := true
	heartbeatAt := time.Time{}
	for _, archive := range []string{"github-urnetwork", "github-urfoundation"} {
		observation := observations[backupArchiveKey(writer.host, archive)]
		if observation == nil {
			progressComplete = false
			heartbeatComplete = false
			continue
		}
		if len(observation.progress) != 1 || len(observation.invalidProgress) != 0 {
			progressComplete = false
		} else {
			progressSeries++
			progressTotal += observation.progress[0]
		}
		if len(observation.heartbeats) != 1 || len(observation.invalidHeartbeat) != 0 {
			heartbeatComplete = false
		} else {
			heartbeatSeries++
			if heartbeatAt.IsZero() || observation.heartbeats[0].Before(heartbeatAt) {
				heartbeatAt = observation.heartbeats[0]
			}
		}
	}

	heartbeatTimestamp := "missing"
	heartbeatAge := "unknown"
	heartbeatAgeDuration := time.Duration(0)
	if !heartbeatAt.IsZero() {
		heartbeatTimestamp = heartbeatAt.Format(time.RFC3339)
		heartbeatAgeDuration = now.Sub(heartbeatAt)
		heartbeatAge = backupArchiveAge(heartbeatAgeDuration)
	}

	reasons := []string{}
	switch writer.unitState {
	case "active", "activating", "reloading":
		if writer.mainPID == 0 {
			reasons = append(reasons, "active-unit-without-main-pid")
		}
		if !heartbeatComplete {
			reasons = append(reasons, "github-heartbeat-incomplete")
		} else if heartbeatAgeDuration > backupArchiveHeartbeatAge {
			reasons = append(reasons, "metrics-heartbeat-stale")
		} else if heartbeatAgeDuration < -30*time.Second {
			reasons = append(reasons, "metrics-heartbeat-in-future")
		}
		if !progressComplete {
			reasons = append(reasons, "github-progress-incomplete")
		} else if progressTotal != 1 {
			reasons = append(reasons, "active-unit-progress-total-not-one")
		}
	case "inactive", "failed":
		if progressComplete && progressTotal != 0 {
			reasons = append(reasons, "idle-unit-progress-total-not-zero")
		}
	case "deactivating":
		// The owner may have published its final zeros before systemd finishes
		// moving the oneshot to inactive. Sustain gating absorbs this boundary.
	default:
		reasons = append(reasons, "unexpected-unit-state")
	}

	if len(reasons) == 0 {
		return healthyFinding(
			"observability/backup-archives", tierPage, "backup-archive-progress-stale", target,
		)
	}
	return finding{
		probeId: "observability/backup-archives", tier: tierPage,
		class: "backup-archive-progress-stale", target: target,
		frame: "unit=github-backup-archive.service", sustain: 2,
		symptom: fmt.Sprintf(
			"%s writer state and exported in-progress telemetry disagree",
			target,
		),
		mechanism: "Fluent Bit assigns a fresh scrape timestamp each time it rereads a Prometheus textfile, so Mimir sample freshness cannot prove that the file's Boolean phase is current. A standalone refresh overwrote both gauges with zero while the oneshot was active, and the long-running writer only rewrote metrics at organization transitions. The stale source file therefore looked freshly idle throughout a multi-hour compression. The fixed writer carries its own publication timestamp as a metric value so this distinction does not require privileged filesystem access.",
		baseline:  "While github-backup-archive.service is active, it has a nonzero MainPID, both heartbeat metric values are no more than 90 seconds old, and exactly one of the two fresh organization gauges is 1. When the unit is inactive or failed, both gauges are 0.",
		observed: fmt.Sprintf(
			"unit_state=%s main_pid=%d heartbeat_timestamp=%s heartbeat_age=%s fresh_github_heartbeat_series=%d heartbeat_complete=%t fresh_github_progress_series=%d progress_complete=%t published_progress_total=%s reasons=%s",
			writer.unitState,
			writer.mainPID,
			heartbeatTimestamp,
			heartbeatAge,
			heartbeatSeries,
			heartbeatComplete,
			progressSeries,
			progressComplete,
			strconv.FormatFloat(progressTotal, 'f', 0, 64),
			strings.Join(reasons, ","),
		),
		evidence: "The unit state and MainPID are read directly on the configured backup host and joined with the source-timestamp-filtered raw Mimir progress gauges plus their producer-owned heartbeat values.",
		context:  "This is a telemetry-owner defect, not evidence that the active archive stopped and not a completed recovery point. Preserve a healthy active tar/xz generation; changing the installed script does not alter the already-running shell.",
		action:   "Keep the current archive job running. Provenance-check the installed script against Xops commit 2733b0b. If it predates that commit, install it from an intentional local Xops checkout containing that change with run-planetoid.sh; its sole owning writer republishes the active phase and producer-owned heartbeat timestamp every 30 seconds, then cancels the helper before publishing final zeros. If the installed file is already current, do not rerun the playbook merely to clear this alert. The already-running pre-fix shell will not gain that behavior, so verify it on the next authorized archive generation rather than restarting this one or manually editing the .prom file.",
		verify:   "On the next generation using the fixed script, require two consecutive raw Mimir samples during a long phase with exactly one GitHub gauge at 1 and both producer heartbeat values no more than 90 seconds old; after successful atomic completion, require both gauges at 0, the helper gone, and the new tarball and manifest valid.",
		playbook: "SIGNALS.md §11.22",
	}
}

func backupArchivesQuery(environment string, hosts []*host) string {
	hostNames := make([]string, 0, len(hosts))
	for _, host := range hosts {
		hostNames = append(hostNames, regexp.QuoteMeta(host.name))
	}
	sort.Strings(hostNames)
	return fmt.Sprintf(
		`{__name__=~%s,env=%s,host=~%s}`,
		strconv.Quote(`urnetwork_backup_archive_(latest_timestamp_seconds|in_progress|heartbeat_timestamp_seconds)`),
		strconv.Quote(environment),
		strconv.Quote(strings.Join(hostNames, "|")),
	)
}

func backupArchiveKey(host, archive string) string { return host + "\x00" + archive }

func isBackupArchiveName(name string) bool {
	for _, expected := range backupArchiveNames {
		if name == expected {
			return true
		}
	}
	return false
}

type backupArchiveQueueObservation struct {
	activeArchive string
	unit          string
	unitState     string
	unitSubstate  string
	mainPID       int64
}

func backupArchiveQueuedBehind(
	observation *backupArchiveObservation,
	writer backupArchiveWriterObservation,
	observations map[string]*backupArchiveObservation,
) backupArchiveQueueObservation {
	if observation == nil || (observation.archive != "pg" && observation.archive != "redis") {
		return backupArchiveQueueObservation{}
	}
	if !backupArchiveRemoteWriterRunning(writer) {
		return backupArchiveQueueObservation{}
	}
	if writer.remoteMountState != "read-write" {
		return backupArchiveQueueObservation{}
	}
	progress, valid := backupArchiveProgress(observation)
	if !valid || progress != 0 {
		return backupArchiveQueueObservation{}
	}
	activeArchive := "pg"
	if observation.archive == "pg" {
		activeArchive = "redis"
	}
	active, valid := backupArchiveProgress(
		observations[backupArchiveKey(observation.host, activeArchive)],
	)
	if !valid || active != 1 {
		return backupArchiveQueueObservation{}
	}
	return backupArchiveQueueObservation{
		activeArchive: activeArchive,
		unit:          "remote-backup-archive.service",
		unitState:     writer.remoteUnitState,
		unitSubstate:  writer.remoteUnitSubstate,
		mainPID:       writer.remoteMainPID,
	}
}

func backupArchiveProgress(observation *backupArchiveObservation) (float64, bool) {
	if observation == nil || len(observation.progress) != 1 || len(observation.invalidProgress) != 0 {
		return 0, false
	}
	return observation.progress[0], true
}

func evaluateBackupArchive(
	now time.Time,
	observation *backupArchiveObservation,
	gateway string,
	volumeState string,
	queue backupArchiveQueueObservation,
) []finding {
	target := observation.host + "/" + observation.archive
	findings := []finding{}

	if len(observation.invalidLatest) > 0 || len(observation.invalidProgress) > 0 ||
		len(observation.invalidHeartbeat) > 0 || len(observation.progress) > 1 || len(observation.heartbeats) > 1 {
		invalid := append([]string(nil), observation.invalidLatest...)
		invalid = append(invalid, observation.invalidProgress...)
		invalid = append(invalid, observation.invalidHeartbeat...)
		if len(observation.progress) > 1 {
			invalid = append(invalid, fmt.Sprintf("progress_series=%d", len(observation.progress)))
		}
		if len(observation.heartbeats) > 1 {
			invalid = append(invalid, fmt.Sprintf("heartbeat_series=%d", len(observation.heartbeats)))
		}
		findings = append(findings, finding{
			probeId: "observability/backup-archives", tier: tierPage,
			class: "backup-archive-metrics-invalid", target: target, sustain: 1,
			symptom:   fmt.Sprintf("%s publishes an invalid or ambiguous backup archive metric", target),
			mechanism: "The textfile collector accepted a sample whose timestamp, generation, Boolean domain, or label cardinality cannot describe one completed archive, one active-state gauge, and at most one producer heartbeat. Treating it as fresh could conceal clock skew, a partial writer, or concurrent metric producers.",
			baseline:  "Each expected archive has exactly one fresh in-progress gauge in {0,1}; every completed-archive and producer-heartbeat value is finite, positive, and no more than five minutes in the future.",
			observed: fmt.Sprintf(
				"invalid=%s fresh_latest_samples=%d fresh_progress_samples=%d fresh_heartbeat_samples=%d stale_scrape_samples=%d metrics_gateway=%s",
				strings.Join(invalid, ";"), len(observation.latest), len(observation.progress), len(observation.heartbeats), observation.staleScrapes, gateway,
			),
			evidence: "Raw Mimir samples were source-timestamp filtered before their archive timestamp and Boolean value were validated.",
			context:  "This is a producer or collector contract failure, not proof that the archive media itself is corrupt.",
			action:   "Inspect the exact Planetoid .prom file and the writer that owns this archive. Restore atomic single-writer exposition and the host clock; do not coerce an invalid value in Grafana or add a second textfile producer.",
			verify:   "Two consecutive direct Mimir reads return one fresh in-progress series in {0,1}, one unambiguous newest completed generation when present, and no future or malformed value.",
			playbook: "SIGNALS.md §11.22",
		})
	} else {
		findings = append(findings, healthyFinding(
			"observability/backup-archives", tierPage, "backup-archive-metrics-invalid", target,
		))
	}

	if len(observation.progress) == 0 && len(observation.invalidProgress) == 0 {
		findings = append(findings, finding{
			probeId: "observability/backup-archives", tier: tierPage,
			class: "backup-archive-metrics-missing", target: target, frame: "textfile", sustain: 2,
			symptom:   fmt.Sprintf("%s has no fresh backup in-progress metric in Mimir", target),
			mechanism: "The archive writer, Fluent Bit textfile collector, authenticated remote-write route, or Mimir ingestion path is absent. In the 2026-09-01 incident, classic-config quotes made the boundary token `textfile\"`, so middle node metrics such as uname remained healthy while every archive series was silently omitted.",
			baseline:  "Every expected archive exports one in-progress gauge at least every 15 seconds, and Mimir returns a sample no more than 90 seconds old.",
			observed: fmt.Sprintf(
				"fresh_progress_samples=0 fresh_latest_samples=%d stale_scrape_samples=%d metrics_gateway=%s",
				len(observation.latest), observation.staleScrapes, gateway,
			),
			evidence: "The query reads raw Mimir rather than Grafana. A healthy node_uname_info series is only a transport control and does not prove the separately enabled textfile collector.",
			context:  "This is observation loss until the source .prom file and an isolated collector read are checked; it must not be interpreted as a successful backup or as an empty archive.",
			action:   "On the configured backup host, verify the .prom files as the fluent-bit user and run a bounded stdout-only textfile collector. Compare the deployed comma list's first and last tokens without printing credentials; wrapping quotes must not be present. If source collection is healthy, trace authenticated remote write and Mimir ingestion before changing dashboard queries.",
			verify:   "Both Grafana service gateways return fresh in-progress samples for all four archives on two consecutive probes, and the dashboard query sees the same labeled series.",
			playbook: "SIGNALS.md §11.22",
		})
	} else {
		findings = append(findings, healthyFinding(
			"observability/backup-archives", tierPage, "backup-archive-metrics-missing", target,
		))
	}

	if len(observation.latest) == 0 {
		if len(observation.invalidLatest) == 0 {
			progress := "unknown"
			if len(observation.progress) == 1 {
				progress = strconv.FormatFloat(observation.progress[0], 'f', 0, 64)
			}
			findings = append(findings, finding{
				probeId: "observability/backup-archives", tier: tierPage,
				class: "backup-archive-missing", target: target, sustain: 2,
				symptom:   fmt.Sprintf("%s has no observable completed archive generation", target),
				mechanism: "No fresh latest-timestamp series exists for this archive. This can mean no completed artifact exists, or that a pre-fix writer refreshed its off-volume metric while the archive mount was unavailable and erased the last-known completion row. Metric absence alone cannot distinguish those states.",
				baseline:  "Each of pg, redis, github-urnetwork, and github-urfoundation exposes one latest-timestamp row for a complete generation; a writer preserves that last-known row while its archive volume is unavailable.",
				observed: fmt.Sprintf(
					"completed_generations=0 in_progress=%s stale_scrape_samples=%d metrics_gateway=%s",
					progress, observation.staleScrapes, gateway,
				),
				evidence: "The raw Mimir query contains no fresh latest-timestamp row. Temporary and partial files never produce one, but this observation does not inspect the root-owned archive contents directly.",
				context:  "This is UNKNOWN completion state until the mounted media is inspected. An active first run is operationally pending, while a missing last-known metric is a producer defect; neither state can be repaired by inventing a timestamp. Software cannot create archive capacity or attach unavailable physical media.",
				action:   "First resolve any simultaneous archive-volume alert. Inspect the exact mounted latest tier and manifests through an authorized root-owned path. If a complete generation exists, deploy the writer that preserves last-known completion rows during volume loss and invoke only its bounded metrics refresh; do not hand-edit the .prom file. If none exists, restore the first failed prerequisite and authorize one single-writer catch-up run; never rename a partial artifact.",
				verify:   "A non-empty completed artifact and its manifest are present, and two fresh Mimir samples expose that real generation timestamp. Then induce the synthetic unavailable-volume boundary and prove a phase reset preserves the same completion row rather than turning it into apparent absence.",
				playbook: "SIGNALS.md §11.22",
			})
		}
		return findings
	}

	latest := observation.latest[0]
	for _, candidate := range observation.latest[1:] {
		if candidate.createdAt.After(latest.createdAt) {
			latest = candidate
		}
	}
	age := now.Sub(latest.createdAt)
	ageText := backupArchiveAge(age)
	if age > backupArchiveMaximumAge {
		progress := "unknown"
		if len(observation.progress) == 1 {
			progress = strconv.FormatFloat(observation.progress[0], 'f', 0, 64)
		}
		mechanism := "The source textfile is current, but its archive timestamp has not advanced inside the five-day recovery-point objective. The scheduled pull may have failed before its atomic final rename; on 2026-09-01 the data job waited 15 minutes for an absent udisks mount and exited without replacing the August 20 generation."
		context := "This alert is operational and may require persistent mount configuration, replacement media, or more archive capacity. A collector, Grafana, or code deploy alone cannot create a new recovery point."
		action := "Read the owning unit result and journal, verify the configured archive path is a real mounted filesystem with enough free space, then repair the first failed prerequisite. Start a catch-up run only with operator authorization; never refresh the metric without producing and validating a new archive."
		verify := "The exact unit exits successfully, the completed generation and manifest validate on mounted media, its timestamp is within five days, and two consecutive direct Mimir reads show the same new generation."
		frame := ""
		queueObserved := ""
		if progress == "1" && volumeState != "read-write" {
			frame = "archive-volume"
			mechanism = fmt.Sprintf("The producer still reports an active %s phase, but the direct archive-volume observation is %s. A stale phase gauge and live unit PID cannot prove that rsync still has a mounted, writable destination, so this is not healthy transfer progress.", observation.archive, volumeState)
			context = "The volume failure is the first broken prerequisite. Treat the transfer state as unsafe or stale until storage is recovered; do not attribute the unchanged completion time to source backlog or WAN capacity."
			action = "Follow the simultaneous backup-archive-volume-unavailable alert. With explicit operator authorization, stop the existing writer before closing a stale mapper or repairing the offline filesystem. Do not start a duplicate transfer, manufacture a completed timestamp, or remount an aborted ext4 journal read-write in place."
			verify = "After offline storage recovery, require the exact volume to remain read-write, then run one single-writer generation to a validated atomic artifact and observe two fresh Mimir reads with the new completion timestamp."
		} else if progress == "1" {
			mechanism = "The source textfile is current and a writer is active, but no new atomic recovery point exists until that transfer completes. A healthy process can remain outside the five-day objective when source backlog divided by sustained offsite throughput exceeds the available recovery window."
			context = "An active single-writer transfer is operationally pending, not fixed and not stalled merely because its final timestamp is unchanged. Software cannot create WAN bandwidth, attach seed media, or shorten the bytes that must cross the recovery boundary."
			action = "Preserve the active writer. Confirm one stable unit/PID, one source transfer, a read-write mounted archive, fresh progress telemetry, and increasing receive bytes; verify the effective source route is the dedicated direct SSH path, then compare source backlog with sustained direct-transfer throughput. If the projected completion still misses the recovery-point objective, operations must provision a faster path or an approved offline seed. Do not restart, duplicate, or manually finalize the transfer to make the timestamp move."
			verify = "The same authorized run completes atomically, its artifact and manifest validate, two direct Mimir reads expose the new generation, and a subsequent scheduled run proves the available offsite throughput can keep the archive inside the five-day objective."
		} else if queue.activeArchive != "" {
			frame = "queued-behind=" + queue.activeArchive
			queueObserved = fmt.Sprintf(
				" queued_behind=%s owner_unit=%s owner_unit_state=%s owner_unit_substate=%s owner_main_pid=%d owner_progress=1",
				queue.activeArchive,
				queue.unit,
				queue.unitState,
				queue.unitSubstate,
				queue.mainPID,
			)
			mechanism = fmt.Sprintf(
				"The source textfile is current and %s is idle only because the same single-writer data job is actively transferring %s first. The script processes PostgreSQL and Redis serially, so the second source cannot enter its in-progress phase until the first transfer and atomic archive rotation finish.",
				observation.archive,
				queue.activeArchive,
			)
			context = "This is queued work inside one healthy authorized writer, not an idle scheduler and not a second catch-up authorization. Its completion is still capacity-bound by the active source transfer. Software cannot create WAN bandwidth, attach seed media, or shorten the queued bytes."
			action = fmt.Sprintf(
				"Preserve the active %s phase and the owning %s process. Do not start, restart, or duplicate the data pull because %s reports in_progress=0. Track increasing receive bytes and source backlog for the active phase; if its projected completion misses the recovery-point objective, operations must provision a faster path or an approved offline seed.",
				queue.activeArchive,
				queue.unit,
				observation.archive,
			)
			verify = fmt.Sprintf(
				"The same authorized unit completes %s atomically, then publishes %s in_progress=1 without a second unit generation; both artifacts and manifests validate, and two direct Mimir reads expose completed generations inside the five-day objective.",
				queue.activeArchive,
				observation.archive,
			)
		}
		findings = append(findings, finding{
			probeId: "observability/backup-archives", tier: tierPage,
			class: "backup-archive-stale", target: target, frame: frame, sustain: 1,
			symptom: fmt.Sprintf(
				"%s newest completed generation is %s old",
				target, ageText,
			),
			mechanism: mechanism,
			baseline:  "Every completed archive timestamp is no more than five days old; current scrapes continue even when the stored generation is stale.",
			observed: fmt.Sprintf(
				"generation=%s completed_at=%s age=%s in_progress=%s archive_volume_state=%s fresh_latest_samples=%d metrics_gateway=%s%s",
				latest.generation, latest.createdAt.Format(time.RFC3339), ageText, progress, volumeState, len(observation.latest), gateway, queueObserved,
			),
			evidence: "Archive age comes from the producer's completed-file timestamp carried as the metric value, not from the fresh Mimir scrape timestamp.",
			context:  context,
			action:   action,
			verify:   verify,
			playbook: "SIGNALS.md §11.22",
		})
	} else {
		findings = append(findings, healthyFinding(
			"observability/backup-archives", tierPage, "backup-archive-stale", target,
		))
	}
	findings = append(findings, healthyFinding(
		"observability/backup-archives", tierPage, "backup-archive-missing", target,
	))
	return findings
}

func backupArchiveAge(age time.Duration) string {
	age = age.Round(time.Minute)
	if age < 24*time.Hour {
		return age.String()
	}
	days := age / (24 * time.Hour)
	remainder := age % (24 * time.Hour)
	if remainder == 0 {
		return fmt.Sprintf("%d days", days)
	}
	return fmt.Sprintf("%d days %s", days, remainder)
}
