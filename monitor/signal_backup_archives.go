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
)

const backupArchiveWriterCommand = `# monitor-signal-11.22-backup-archives
github_unit_state=$(systemctl is-active github-backup-archive.service 2>/dev/null || true)
github_main_pid=$(systemctl show github-backup-archive.service -p MainPID --value 2>/dev/null || true)
remote_unit_state=$(systemctl show remote-backup-archive.service -p ActiveState --value 2>/dev/null || true)
remote_result=$(systemctl show remote-backup-archive.service -p Result --value 2>/dev/null || true)
remote_restart=$(systemctl show remote-backup-archive.service -p Restart --value 2>/dev/null || true)
remote_restart_delay=$(systemctl show remote-backup-archive.service -p RestartUSec --value 2>/dev/null || true)
remote_exit_status=$(systemctl show remote-backup-archive.service -p ExecMainStatus --value 2>/dev/null || true)
remote_environment=$(systemctl show remote-backup-archive.service -p Environment --value 2>/dev/null || true)
remote_pg_source=$(printf '%s\n' "${remote_environment}" | tr ' ' '\n' | sed -n 's/^BRINGYOUR_BACKUP_PG_REMOTE=//p' | tail -n 1)
remote_pg_port=$(printf '%s\n' "${remote_environment}" | tr ' ' '\n' | sed -n 's/^BRINGYOUR_BACKUP_PG_PORT=//p' | tail -n 1)
remote_redis_source=$(printf '%s\n' "${remote_environment}" | tr ' ' '\n' | sed -n 's/^BRINGYOUR_BACKUP_REDIS_REMOTE=//p' | tail -n 1)
remote_redis_port=$(printf '%s\n' "${remote_environment}" | tr ' ' '\n' | sed -n 's/^BRINGYOUR_BACKUP_REDIS_PORT=//p' | tail -n 1)
case "${github_unit_state}" in '') github_unit_state=unknown ;; esac
case "${github_main_pid}" in ''|*[!0-9]*) github_main_pid=0 ;; esac
case "${remote_unit_state}" in '') remote_unit_state=unknown ;; esac
case "${remote_result}" in '') remote_result=unknown ;; esac
case "${remote_restart}" in '') remote_restart=unknown ;; esac
case "${remote_restart_delay}" in ''|*[!0-9a-z]*) remote_restart_delay=unknown ;; esac
case "${remote_exit_status}" in ''|*[!0-9]*) remote_exit_status=0 ;; esac
case "${remote_pg_source}" in '') remote_pg_source=unknown ;; esac
case "${remote_pg_port}" in ''|*[!0-9]*) remote_pg_port=0 ;; esac
case "${remote_redis_source}" in '') remote_redis_source=unknown ;; esac
case "${remote_redis_port}" in ''|*[!0-9]*) remote_redis_port=0 ;; esac
printf 'github_unit_state=%s\n' "${github_unit_state}"
printf 'github_main_pid=%s\n' "${github_main_pid}"
printf 'remote_unit_state=%s\n' "${remote_unit_state}"
printf 'remote_result=%s\n' "${remote_result}"
printf 'remote_restart=%s\n' "${remote_restart}"
printf 'remote_restart_delay=%s\n' "${remote_restart_delay}"
printf 'remote_exit_status=%s\n' "${remote_exit_status}"
printf 'remote_pg_source=%s\n' "${remote_pg_source}"
printf 'remote_pg_port=%s\n' "${remote_pg_port}"
printf 'remote_redis_source=%s\n' "${remote_redis_source}"
printf 'remote_redis_port=%s\n' "${remote_redis_port}"`

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
	remoteResult       string
	remoteRestart      string
	remoteRestartDelay string
	remoteExitStatus   int64
	remotePGSource     string
	remotePGPort       int64
	remoteRedisSource  string
	remoteRedisPort    int64
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
	pgSource := env.cfg.hostByRole("pg-primary")
	redisSource := env.cfg.hostByRole("redis-cluster")
	if pgSource == nil || redisSource == nil || pgSource.overlayIp == "" || redisSource.overlayIp == "" {
		return nil, fmt.Errorf("backup archives: PostgreSQL and Redis management-VPN source identities are required")
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
		findings = append(findings, evaluateBackupArchive(now, observations[key], metricHost.name)...)
	}
	for _, host := range backupHosts {
		findings = append(findings, evaluateBackupArchiveWriter(now, writers[host.name], observations))
		findings = append(findings, evaluateBackupArchiveRetry(writers[host.name]))
		findings = append(findings, evaluateBackupArchiveSourceRoute(
			writers[host.name],
			"by@"+pgSource.overlayIp,
			"by@"+redisSource.overlayIp,
		))
	}
	return findings, nil
}

func parseBackupArchiveWriterObservation(hostName, output string) (backupArchiveWriterObservation, error) {
	values := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(output), "\n") {
		key, value, ok := strings.Cut(strings.TrimSpace(line), "=")
		if !ok || key == "" || value == "" {
			return backupArchiveWriterObservation{}, fmt.Errorf("invalid property line %q", line)
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
		"remote_result",
		"remote_restart",
		"remote_restart_delay",
		"remote_exit_status",
		"remote_pg_source",
		"remote_pg_port",
		"remote_redis_source",
		"remote_redis_port",
	}
	if len(values) != len(required) {
		return backupArchiveWriterObservation{}, fmt.Errorf("expected %d properties; got %q", len(required), strings.TrimSpace(output))
	}
	for _, key := range required {
		if _, ok := values[key]; !ok {
			return backupArchiveWriterObservation{}, fmt.Errorf("missing property %q", key)
		}
	}
	for _, key := range []string{"github_unit_state", "remote_unit_state", "remote_result", "remote_restart"} {
		if matched, _ := regexp.MatchString(`^[a-z-]+$`, values[key]); !matched {
			return backupArchiveWriterObservation{}, fmt.Errorf("invalid %s %q", key, values[key])
		}
	}
	if matched, _ := regexp.MatchString(`^(unknown|[0-9]+(?:us|ms|s|min|h|d|w))$`, values["remote_restart_delay"]); !matched {
		return backupArchiveWriterObservation{}, fmt.Errorf("invalid remote_restart_delay %q", values["remote_restart_delay"])
	}
	mainPID, err := strconv.ParseInt(values["github_main_pid"], 10, 64)
	if err != nil || mainPID < 0 {
		return backupArchiveWriterObservation{}, fmt.Errorf("invalid main PID %q", values["github_main_pid"])
	}
	remoteExitStatus, err := strconv.ParseInt(values["remote_exit_status"], 10, 64)
	if err != nil || remoteExitStatus < 0 {
		return backupArchiveWriterObservation{}, fmt.Errorf("invalid remote exit status %q", values["remote_exit_status"])
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
		remoteResult:       values["remote_result"],
		remoteRestart:      values["remote_restart"],
		remoteRestartDelay: values["remote_restart_delay"],
		remoteExitStatus:   remoteExitStatus,
		remotePGSource:     values["remote_pg_source"],
		remotePGPort:       remotePGPort,
		remoteRedisSource:  values["remote_redis_source"],
		remoteRedisPort:    remoteRedisPort,
	}, nil
}

func evaluateBackupArchiveSourceRoute(
	observation backupArchiveWriterObservation,
	expectedPGSource string,
	expectedRedisSource string,
) finding {
	target := observation.host + "/remote-sources"
	if observation.remotePGSource == expectedPGSource && observation.remotePGPort == 22 &&
		observation.remoteRedisSource == expectedRedisSource && observation.remoteRedisPort == 22 {
		return healthyFinding(
			"observability/backup-archives", tierPage, "backup-archive-source-route", target,
		)
	}
	return finding{
		probeId: "observability/backup-archives", tier: tierPage,
		class: "backup-archive-source-route", target: target,
		frame: "unit=remote-backup-archive.service", sustain: 1,
		symptom:   fmt.Sprintf("%s is not configured to pull both backup sources over their management-VPN identities", observation.host),
		mechanism: "Planetoid is offsite. A shared public address plus router-specific forwarded SSH ports couples recovery to an unrelated NAT boundary and can silently retain obsolete source routing after the rest of Planetoid moves to the VPN. Distinct inventory-derived overlay targets keep PostgreSQL and Redis source identity explicit.",
		baseline:  fmt.Sprintf("The effective unit uses PostgreSQL %s:22 and Redis %s:22, matching the monitor inventory exactly.", expectedPGSource, expectedRedisSource),
		observed: fmt.Sprintf(
			"pg_source=%s pg_port=%d redis_source=%s redis_port=%d",
			observation.remotePGSource,
			observation.remotePGPort,
			observation.remoteRedisSource,
			observation.remoteRedisPort,
		),
		evidence: "The monitor reads only the four non-secret BRINGYOUR_BACKUP_*_REMOTE/PORT values from the effective systemd Environment and compares them with the configured PostgreSQL and Redis overlay addresses.",
		context:  "This is deployment/routing identity, not proof that a copy completed. Changing the unit does not authorize an unscheduled transfer and cannot make absent media or insufficient capacity healthy.",
		action:   "Deploy a clean Xops descendant of fbd291a with main/ansible/run-planetoid.sh. Do not add a public-router fallback. Before the next scheduled pull, require both overlay routes to use tun0, TCP/22 to accept connections, and the installed fleet key to authenticate read-only to both source hosts. Obtain separate operator authorization for any unscheduled catch-up run.",
		verify:   "The effective unit matches both exact VPN targets on port 22; authenticated source listings succeed through tun0; the next scheduled oneshot exits successfully; and the completed artifacts, manifests, and two direct Mimir samples expose the new generations.",
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

func evaluateBackupArchive(now time.Time, observation *backupArchiveObservation, gateway string) []finding {
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
				symptom:   fmt.Sprintf("%s has no completed archive generation", target),
				mechanism: "No fresh latest-timestamp series exists for this archive. A first backup may still be running, but until its atomic final rename completes there is no recoverable generation for this source.",
				baseline:  "Each of pg, redis, github-urnetwork, and github-urfoundation has at least one complete generation on the mounted Planetoid archive.",
				observed: fmt.Sprintf(
					"completed_generations=0 in_progress=%s stale_scrape_samples=%d metrics_gateway=%s",
					progress, observation.staleScrapes, gateway,
				),
				evidence: "The latest metric is written only after a complete artifact exists; temporary and partial files never produce it.",
				context:  "An active first run is operationally pending, not fixed. Software cannot create archive capacity or attach unavailable physical media.",
				action:   "Inspect the owning systemd unit, mounted archive filesystem, free capacity, and bounded job journal. Restore the mount or failing source and allow one atomic run to finish; do not manufacture a latest timestamp or rename a partial archive.",
				verify:   "A non-empty completed artifact and its manifest are present, the writer exits successfully, and two fresh Mimir samples expose its real generation timestamp.",
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
		findings = append(findings, finding{
			probeId: "observability/backup-archives", tier: tierPage,
			class: "backup-archive-stale", target: target, sustain: 1,
			symptom: fmt.Sprintf(
				"%s newest completed generation is %s old",
				target, ageText,
			),
			mechanism: "The source textfile is current, but its archive timestamp has not advanced inside the five-day recovery-point objective. The scheduled pull may have failed before its atomic final rename; on 2026-09-01 the data job waited 15 minutes for an absent udisks mount and exited without replacing the August 20 generation.",
			baseline:  "Every completed archive timestamp is no more than five days old; current scrapes continue even when the stored generation is stale.",
			observed: fmt.Sprintf(
				"generation=%s completed_at=%s age=%s in_progress=%s fresh_latest_samples=%d metrics_gateway=%s",
				latest.generation, latest.createdAt.Format(time.RFC3339), ageText, progress, len(observation.latest), gateway,
			),
			evidence: "Archive age comes from the producer's completed-file timestamp carried as the metric value, not from the fresh Mimir scrape timestamp.",
			context:  "This alert is operational and may require persistent mount configuration, replacement media, or more archive capacity. A collector, Grafana, or code deploy alone cannot create a new recovery point.",
			action:   "Read the owning unit result and journal, verify the configured archive path is a real mounted filesystem with enough free space, then repair the first failed prerequisite. Start a catch-up run only with operator authorization; never refresh the metric without producing and validating a new archive.",
			verify:   "The exact unit exits successfully, the completed generation and manifest validate on mounted media, its timestamp is within five days, and two consecutive direct Mimir reads show the same new generation.",
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
