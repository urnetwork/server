// MinIO drive runway in bytes and in inodes, raw-versus-logical accounting
// divergence, PutObject surge and base-backup headroom: what MinIO's own
// percentage and quota accounting (§11.6a) cannot see. Both drive runways use
// the shared §2.4f estimator (storage_runway.go) with this section's longer
// thresholds.
package monitor

import (
	"context"
	"fmt"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

const minioRunwayProbeId = "minio/runway"

// SIGNALS.md §11.6b (minio-runway): the drives' runway and the data beside
// MinIO on them.
func NewMinioRunwaySignal() Signal {
	return NewMinioRunwaySignalWithSettings(DefaultMinioRunwaySettings())
}

// The signal with explicit thresholds.
func NewMinioRunwaySignalWithSettings(settings *MinioRunwaySettings) Signal {
	return &signalAdapter{
		number: "11.6b", key: "minio-runway", name: "MinIO drive runway and accounting divergence",
		probe: &minioRunwayProbe{settings: settings},
	}
}

// The thresholds of SIGNALS.md §11.6b.
type MinioRunwaySettings struct {
	Estimator StorageRunwayEstimatorSettings
	// Normal sampling cadence, and the whole-run budget including the
	// gateway, backup-unit and replication-slot reads.
	Cadence   time.Duration
	RunBudget time.Duration
	// Consecutive observations before a runway, divergence or surge alert.
	Sustain int
	// Drive runway: warn at or under WarnRunway, page at or under PageRunway.
	// Longer than §2.4f's because adding drives takes days.
	WarnRunway time.Duration
	PageRunway time.Duration
	// node_filesystem mountpoints of the MinIO drives, as a fully anchored
	// regular expression.
	DriveMountpointPattern string
	// MinIO refuses new objects once free space after a write would be at
	// most this fraction of the total (diskFillFraction 0.99): each drive's
	// runway counts down to this share of its size.
	DriveFloorFraction float64
	// Raw bytes per logical byte of the erasure coding: the drives of the
	// erasure set over its data shards, 8/6 for 6+2 (two for 2+2).
	ErasureOverhead float64
	// Warn when raw used bytes exceed ErasureOverhead times MinIO's logical
	// bucket bytes by more than this.
	DivergenceWarnBytes float64
	// Warn when the PutObject rate over PutRateWindow exceeds PutSurgeMultiple
	// times its median over PutSurgeHistory and is at least
	// PutSurgeMinimumRate per second. The median needs PutSurgeMinimumHistory
	// of measured history.
	PutRateWindow          time.Duration
	PutSurgeMultiple       float64
	PutSurgeMinimumRate    float64
	PutSurgeHistory        time.Duration
	PutSurgeMinimumHistory time.Duration
	// The base backup beside MinIO on the same drives (xops
	// playbook-pg-basebackup.yml): its units on the MinIO host, its
	// replication-slot prefix on the primary, the per-drive reserve it keeps
	// for MinIO, the least space its own admission requires above that
	// reserve, and how soon a timer start counts as about to write.
	BackupServiceUnit          string
	BackupTimerUnit            string
	BackupSlotPrefix           string
	BackupReserveBytesPerDrive float64
	BackupMinimumFreeBytes     float64
	BackupTimerHorizon         time.Duration
	// Bounds on the unit read: each systemctl call (whole seconds, at least
	// one), the whole command, and its output.
	BackupUnitTimeout    time.Duration
	BackupCommandTimeout time.Duration
	BackupMaxOutputBytes int
	// Inode runway. XFS refuses every new file or directory with ENOSPC once
	// a drive's inodes reach its imaxpct ceiling, however many bytes are
	// free, and every object puts a shard on every drive. Each drive's free
	// inodes count down to zero with the same estimator: warn at or under
	// InodeWarnRunway or below InodeWarnFree free inodes on any drive, page at
	// or under InodePageRunway or below InodePageFree. The floors are counts,
	// not a share of the ceiling, so they hold at any imaxpct.
	InodeWarnRunway time.Duration
	InodePageRunway time.Duration
	InodeWarnFree   float64
	InodePageFree   float64
	// An unknown inode layer pages when a drive lost more than this many
	// inodes an hour over the estimator's unknown-risk window, measured now
	// or remembered from a run inside it (emission rule 1).
	InodeUnknownPageDeclinePerHour float64
}

// The §11.6b defaults on the main store: since 2026-10-24 one 8-drive erasure
// set at EC:2 (6 data + 2 parity shards, xops MINIO-REBUILD-8.md) of 3.84 TB
// XFS drives; the backup's inventory settings (pg_basebackup_reserve_bytes
// 1.5 TiB, pg_basebackup_min_free_bytes 4 TB); and inode floors set after
// the drives reached 89% of their 375.07M-inode ceiling on 2026-11-07.
func DefaultMinioRunwaySettings() *MinioRunwaySettings {
	return &MinioRunwaySettings{
		Estimator:                  DefaultStorageRunwayEstimatorSettings(),
		Cadence:                    15 * time.Minute,
		RunBudget:                  60 * time.Second,
		Sustain:                    2,
		WarnRunway:                 14 * 24 * time.Hour,
		PageRunway:                 3 * 24 * time.Hour,
		DriveMountpointPattern:     "/mnt/blob[0-9]+",
		DriveFloorFraction:         0.01,
		ErasureOverhead:            8.0 / 6.0,
		DivergenceWarnBytes:        1000 * 1000 * 1000 * 1000,
		PutRateWindow:              time.Hour,
		PutSurgeMultiple:           3,
		PutSurgeMinimumRate:        10,
		PutSurgeHistory:            7 * 24 * time.Hour,
		PutSurgeMinimumHistory:     3 * 24 * time.Hour,
		BackupServiceUnit:          "pg-basebackup.service",
		BackupTimerUnit:            "pg-basebackup.timer",
		BackupSlotPrefix:           "pgbackup_",
		BackupReserveBytesPerDrive: 1536 * 1024 * 1024 * 1024,
		BackupMinimumFreeBytes:     4 * 1000 * 1000 * 1000 * 1000,
		BackupTimerHorizon:         24 * time.Hour,
		BackupUnitTimeout:          5 * time.Second,
		BackupCommandTimeout:       15 * time.Second,
		BackupMaxOutputBytes:       4 * 1024,
		InodeWarnRunway:            7 * 24 * time.Hour,
		InodePageRunway:            3 * 24 * time.Hour,
		InodeWarnFree:              30 * 1000 * 1000,
		InodePageFree:              15 * 1000 * 1000,
		// above the 7-14 day average before 2026-11-07 (17-20M a day) and
		// below the 2026-11-01 peak (35M a day)
		InodeUnknownPageDeclinePerHour: 1000 * 1000,
	}
}

// The risk memory is keyed by host and survives between runs.
type minioRunwayProbe struct {
	settings *MinioRunwaySettings
	risks    storageRunwayRiskMemory
}

func (self *minioRunwayProbe) id() string               { return minioRunwayProbeId }
func (self *minioRunwayProbe) tier() string             { return tierPage }
func (self *minioRunwayProbe) cadence() time.Duration   { return self.settings.Cadence }
func (self *minioRunwayProbe) runBudget() time.Duration { return self.settings.RunBudget }

// One drive's filesystem history for one layer: available and size bytes, or
// free and total inodes.
type minioRunwayDrive struct {
	mountpoint string
	avail      []storageRunwaySample
	size       []storageRunwaySample
	reason     string
}

// One MinIO host's reduced range answer. The drives' bytes and inodes are
// separate layers, so a gap in one never clears or masks the other.
type minioRunwayHost struct {
	name       string
	drives     map[string]*minioRunwayDrive
	inodes     map[string]*minioRunwayDrive
	gauges     map[string][]storageRunwaySample
	gaugeFault map[string]string
	driveFault string
	inodeFault string
}

// One range query: drive filesystem bytes and inodes by mountpoint, MinIO's
// drive count, logical bucket bytes, usage snapshot age and the hourly
// PutObject rate.
func minioRunwayQuery(environment string, hosts []string, settings *MinioRunwaySettings) string {
	env := strconv.Quote(environment)
	hostMatcher := strconv.Quote(storageRunwayAlternation(hosts))
	tag := func(expression, check string) string {
		return "label_replace(" + expression + `,"monitor_check",` + strconv.Quote(check) + `,"__name__",".*")`
	}
	node := func(metric string) string {
		return metric + "{env=" + env + `,job="node",host=~` + hostMatcher + ",mountpoint=~" + strconv.Quote(settings.DriveMountpointPattern) + "}"
	}
	minio := func(metric, extra string) string {
		return metric + "{env=" + env + `,job="minio",host=~` + hostMatcher + extra + "}"
	}
	window := "[" + strconv.FormatInt(int64(settings.PutRateWindow/time.Second), 10) + "s]"
	return strings.Join([]string{
		tag(node("node_filesystem_avail_bytes"), "drive_avail"),
		tag(node("node_filesystem_size_bytes"), "drive_size"),
		tag(node("node_filesystem_files_free"), "drive_files_free"),
		tag(node("node_filesystem_files"), "drive_files"),
		tag("max by (host) ("+minio("minio_system_drive_count", "")+")", "drive_count"),
		tag("sum by (host) ("+minio("minio_cluster_usage_buckets_total_bytes", "")+")", "logical_bytes"),
		// The objects path reports seconds; this MinIO release writes the
		// buckets-path gauge of the same name in nanoseconds (§11.6a).
		tag("max by (host) ("+minio("minio_cluster_usage_objects_since_last_update_seconds", "")+")", "usage_age"),
		tag("sum by (host) (rate("+minio("minio_api_requests_total", `,name="PutObject"`)+window+"))", "put_rate"),
	}, " or ")
}

// Group the range answer by inventory host. Series of other hosts, and
// partitions outside the fixed set, are discarded or fail the host's layer;
// none can certify a configured host.
func reduceMinioRunway(seriesList []storageRunwayRangeSeries, environment string, hosts []string, mountpointPattern *regexp.Regexp) map[string]*minioRunwayHost {
	observations := map[string]*minioRunwayHost{}
	for _, name := range hosts {
		observations[name] = &minioRunwayHost{name: name, drives: map[string]*minioRunwayDrive{}, inodes: map[string]*minioRunwayDrive{},
			gauges: map[string][]storageRunwaySample{}, gaugeFault: map[string]string{}}
	}
	for _, series := range seriesList {
		observation := observations[series.labels["host"]]
		if observation == nil {
			continue
		}
		check := series.labels["monitor_check"]
		switch check {
		case "drive_avail", "drive_size", "drive_files_free", "drive_files":
			drives, fault, valueReason := observation.drives, &observation.driveFault, "invalid-byte-value"
			if check == "drive_files_free" || check == "drive_files" {
				drives, fault, valueReason = observation.inodes, &observation.inodeFault, "invalid-inode-value"
			}
			for name := range series.labels {
				switch name {
				case "__name__", "env", "job", "host", "device", "fstype", "mountpoint", "instance", "monitor_check":
				default:
					*fault = "unsupported-drive-label"
				}
			}
			mountpoint := series.labels["mountpoint"]
			if !mountpointPattern.MatchString(mountpoint) || series.labels["env"] != environment || series.labels["job"] != "node" ||
				series.labels["device"] == "" || series.labels["fstype"] == "" {
				*fault = "unowned-drive-series"
				continue
			}
			drive := drives[mountpoint]
			if drive == nil {
				drive = &minioRunwayDrive{mountpoint: mountpoint}
				drives[mountpoint] = drive
			}
			slot := &drive.avail
			if check == "drive_size" || check == "drive_files" {
				slot = &drive.size
			}
			switch {
			case *slot != nil:
				drive.reason = "ambiguous-drive-series"
			case series.invalid != "":
				drive.reason = series.invalid
			case !storageRunwayWholeBytes(series.samples):
				// inode counts are whole numbers below 2^53 too
				drive.reason = valueReason
			default:
				*slot = series.samples
			}
		case "drive_count", "logical_bytes", "usage_age", "put_rate":
			for name := range series.labels {
				if name != "host" && name != "monitor_check" {
					observation.gaugeFault[check] = "unsupported-gauge-label"
				}
			}
			switch {
			case observation.gauges[check] != nil:
				observation.gaugeFault[check] = "duplicate-gauge-series"
			case series.invalid != "":
				observation.gaugeFault[check] = series.invalid
			default:
				observation.gauges[check] = series.samples
			}
		default:
			observation.driveFault = "unexpected-partition"
		}
	}
	return observations
}

// The gauge's value at the grid end, when fresh and unambiguous.
func (self *minioRunwayHost) gauge(check string, end time.Time) (float64, string) {
	if reason := self.gaugeFault[check]; reason != "" {
		return 0, reason
	}
	samples := self.gauges[check]
	if len(samples) == 0 {
		return 0, "missing-" + strings.ReplaceAll(check, "_", "-")
	}
	if latest := samples[len(samples)-1]; latest.at.Equal(end) {
		return latest.value, ""
	}
	return 0, "stale-" + strings.ReplaceAll(check, "_", "-")
}

// One drive's estimate with its floor, from the newest size sample.
type minioRunwayDriveEstimate struct {
	drive     *minioRunwayDrive
	estimate  storageRunwayEstimate
	size      float64
	floor     float64
	sizeKnown bool
}

// Each layer is reduced independently: one layer's gap never clears or
// masks another layer's measured state.
func (self *minioRunwayProbe) check(ctx context.Context, env *probeEnv) ([]finding, error) {
	if probeEnded(ctx) {
		return nil, ctx.Err()
	}
	settings := self.settings
	minioHosts := []*host{}
	scoped, isScoped := env.runner.(*hostScopeRunner)
	for _, configured := range env.cfg.hostsWithRole("minio") {
		if isScoped {
			if err := scoped.guardHost(ctx, configured); err != nil {
				if probeEnded(ctx) {
					return nil, ctx.Err()
				}
				continue
			}
		}
		minioHosts = append(minioHosts, configured)
	}
	if len(minioHosts) == 0 {
		// No enabled MinIO host has drives to judge (§11.6a's dark inventory).
		return nil, nil
	}
	now := env.now().UTC()
	pattern, err := regexp.Compile("^(?:" + settings.DriveMountpointPattern + ")$")
	if err != nil || settings.DriveMountpointPattern == "" {
		return []finding{self.unknown(minioHosts[0].name+"/drives", "drives", "invalid-drive-pattern", tierWarn, "", nil)}, nil
	}
	hostNames := []string{}
	for _, configured := range minioHosts {
		hostNames = append(hostNames, configured.name)
	}
	unknownAll := func(reason string, err error) []finding {
		findings := []finding{}
		for _, configured := range minioHosts {
			tier, basis := self.risks.unknownTier(configured.name+"/drives", storageRunwayEstimate{}, now, settings.Estimator)
			inodeTier, inodeBasis := self.inodeUnknownTier(configured.name+"/inodes", 0, false, now)
			findings = append(findings,
				self.unknown(configured.name+"/drives", "drives", reason, tier, basis, err),
				self.unknown(configured.name+"/inodes", "inodes", reason, inodeTier, inodeBasis, err),
				self.unknown(configured.name+"/accounting", "accounting", reason, tierWarn, "", err),
				self.unknown(configured.name+"/puts", "puts", reason, tierWarn, "", err),
				self.unknown(configured.name+"/backup", "backup", reason, tierWarn, "", err))
		}
		return findings
	}
	gateways := env.cfg.hostsWithRole("services")
	if len(gateways) == 0 {
		return unknownAll("service-gateway-unavailable", nil), nil
	}
	grid := settings.Estimator.grid(now)
	command := storageRunwayRangeCommand("monitor-minio-runway", minioRunwayQuery(env.cfg.env, hostNames, settings), grid, settings.Estimator)
	output, gateway, err := mimirShellFirstServiceGatewayParsed(ctx, env.runner, gateways, nil, command, settings.Estimator.MaxResponseBytes)
	if lifecycleErr := probeLifecycleError(ctx, err); lifecycleErr != nil {
		return nil, lifecycleErr
	}
	if err != nil {
		return unknownAll("source-command-unavailable", err), nil
	}
	seriesList, reason := parseStorageRunwayRange(output, grid, settings.Estimator)
	if reason != "" {
		return unknownAll(reason, nil), nil
	}
	observations := reduceMinioRunway(seriesList, env.cfg.env, hostNames, pattern)

	findings := []finding{}
	pendingRiskSamples := map[string]storageRunwaySample{}
	for _, configured := range minioHosts {
		observation := observations[configured.name]
		drives, census, censusReason := self.driveEstimates(observation, grid.end)
		findings = append(findings, self.driveRunway(configured.name, drives, census, censusReason, now, gateway.name, pendingRiskSamples)...)
		inodes, inodeCensus, inodeCensusReason := self.inodeEstimates(observation, grid.end)
		findings = append(findings, self.inodeRunway(configured.name, inodes, inodeCensus, inodeCensusReason, now, gateway.name, pendingRiskSamples)...)
		findings = append(findings, self.accounting(configured.name, observation, drives, census, censusReason, grid.end, gateway.name)...)
		findings = append(findings, self.putSurge(configured.name, observation, grid, gateway.name)...)
		backup, err := self.backup(ctx, env, configured, drives, census, censusReason)
		if err != nil {
			return nil, err
		}
		findings = append(findings, backup...)
	}
	// Later source callbacks may end the owner after drive reduction. Only a
	// completed live observation can replace its remembered risk.
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	for target, sample := range pendingRiskSamples {
		self.risks.remember(target, sample.value, sample.at)
	}
	return findings, nil
}

// Estimate every drive. The census is complete when MinIO's own fresh drive
// count equals the number of drive filesystems and each one has a fresh,
// complete history and size; otherwise a reason says what is missing.
func (self *minioRunwayProbe) driveEstimates(observation *minioRunwayHost, end time.Time) ([]minioRunwayDriveEstimate, bool, string) {
	settings := self.settings
	mountpoints := make([]string, 0, len(observation.drives))
	for mountpoint := range observation.drives {
		mountpoints = append(mountpoints, mountpoint)
	}
	sort.Strings(mountpoints)
	drives := []minioRunwayDriveEstimate{}
	reason := observation.driveFault
	for _, mountpoint := range mountpoints {
		drive := observation.drives[mountpoint]
		if drive.reason != "" {
			reason = drive.reason
			continue
		}
		if drive.avail == nil {
			reason = "missing-drive-available-series"
			continue
		}
		// Without a fresh size the floor is unknown; a zero floor keeps any
		// violation valid (a higher floor only shortens the runway).
		estimate := minioRunwayDriveEstimate{drive: drive}
		if len(drive.size) > 0 && drive.size[len(drive.size)-1].at.Equal(end) {
			estimate.size, estimate.sizeKnown = drive.size[len(drive.size)-1].value, true
			estimate.floor = settings.DriveFloorFraction * estimate.size
		}
		estimate.estimate = estimateStorageRunway(settings.Estimator, drive.avail, end, estimate.floor)
		switch {
		case reason != "":
		case !estimate.estimate.fresh:
			reason = "stale-drive-series"
		case !estimate.estimate.complete:
			reason = "incomplete-decline-windows"
		case !estimate.sizeKnown:
			reason = "missing-or-stale-drive-size"
		case estimate.estimate.latest.value > estimate.size:
			reason = "inconsistent-drive-bytes"
		}
		drives = append(drives, estimate)
	}
	count, countReason := observation.gauge("drive_count", end)
	switch {
	case reason != "":
	case countReason != "":
		reason = countReason
	case count < 1 || count != math.Trunc(count) || int(count) != len(observation.drives):
		reason = fmt.Sprintf("drive-filesystems=%d minio-drive-count=%.0f", len(observation.drives), count)
	}
	return drives, reason == "", reason
}

// The shortest runway among the drives, since one full drive fails every
// write. A violation needs only a fresh drive; healthy needs the full census.
func (self *minioRunwayProbe) driveRunway(hostName string, drives []minioRunwayDriveEstimate, census bool, censusReason string, now time.Time, gateway string, pendingRiskSamples map[string]storageRunwaySample) []finding {
	settings := self.settings
	target := hostName + "/drives"
	var worst, leastFree *minioRunwayDriveEstimate
	worstRunway := time.Duration(math.MaxInt64)
	recentRate, recentKnown := 0.0, false
	for index := range drives {
		drive := &drives[index]
		if drive.estimate.recentKnown {
			recentRate, recentKnown = max(recentRate, drive.estimate.recentRate), true
		}
		if !drive.estimate.fresh {
			continue
		}
		if leastFree == nil || drive.estimate.latest.value < leastFree.estimate.latest.value {
			leastFree = drive
		}
		if runway, declining := drive.estimate.runway(); declining && runway < worstRunway {
			worst, worstRunway = drive, runway
		}
	}
	if recentKnown {
		pendingRiskSamples[target] = storageRunwaySample{at: now, value: recentRate}
	}
	findings := []finding{}
	band, tier := "", tierWarn
	switch {
	case worst != nil && worstRunway <= settings.PageRunway:
		band, tier = "page", tierPage
	case worst != nil && worstRunway <= settings.WarnRunway:
		band = "warn"
	}
	if band != "" {
		parts := []string{}
		for _, drive := range drives {
			if drive.estimate.fresh {
				parts = append(parts, drive.drive.mountpoint+" available_bytes="+strconv.FormatFloat(drive.estimate.latest.value, 'f', 0, 64))
			}
		}
		findings = append(findings, finding{
			probeId: minioRunwayProbeId, tier: tier, class: "minio-drive-runway", target: target, sustain: settings.Sustain,
			symptom: fmt.Sprintf("MinIO drive %s on %s reaches MinIO's free-space floor in %.1f days at its fastest measured decline", worst.drive.mountpoint, hostName, worstRunway.Hours()/24),
			mechanism: "MinIO fails every write once one drive is full, so the drive with the shortest runway bounds the whole store. The rate is the fastest decline over the " + storageRunwayWindowNames(settings.Estimator.Windows) +
				" windows of the drive's own filesystem, which counts the base backup and other data beside MinIO as well as small-object overhead that MinIO's bucket accounting omits.",
			baseline: fmt.Sprintf("Every drive's runway to %.0f%% of its size is over %.0f days; WARN at <= %.0f days and PAGE at <= %.0f days, each after %d observations.",
				100*settings.DriveFloorFraction, settings.WarnRunway.Hours()/24, settings.WarnRunway.Hours()/24, settings.PageRunway.Hours()/24, settings.Sustain),
			observed: "band=" + band + " worst_drive=" + worst.drive.mountpoint + " " + worst.estimate.observed(worst.floor) +
				fmt.Sprintf(" least_free_drive=%s census_complete=%t drives=[%s]", leastFree.drive.mountpoint, census, strings.Join(parts, "; ")),
			evidence: "One bounded loopback Mimir range query of node_filesystem_avail_bytes and _size_bytes for the inventory MinIO host's drive mountpoints, joined with MinIO's own minio_system_drive_count, through gateway " + gateway + ".",
			context:  "False-positive qualifiers: a scheduled backup's transient second generation, and scanner lag right after a MinIO restart. False-negative qualifiers: healing and replication write without S3 puts, and raw cost per object varies with object size and filesystem.",
			action:   "Cut object growth at its source (§14.8, skipping the history-key copy for client-key evidence and a lifecycle rule for existing copies), run base backups only with room for two generations above the reserve, and add drives: a new pool needs at least four drives to keep 2+2 parity. Do not delete files under a drive's minio directory by hand.",
			verify:   "Two consecutive complete observations with every drive's runway over the warning band.",
			playbook: "SIGNALS.md §11.6b and §2.4f",
		})
	}
	if !census {
		tier, basis := self.risks.unknownTier(target, storageRunwayEstimate{recentRate: recentRate, recentKnown: recentKnown}, now, settings.Estimator)
		return append(findings, self.unknown(target, "drives", censusReason, tier, basis, nil))
	}
	if band == "" {
		findings = append(findings, healthyFinding(minioRunwayProbeId, tierPage, "minio-drive-runway", target))
	}
	return append(findings, healthyFinding(minioRunwayProbeId, tierWarn, "minio-runway-unobservable", target))
}

// Estimate every drive's free inodes, counted down to zero. The inode census
// is complete when MinIO's own fresh drive count equals the number of drive
// filesystems with inode series, and each has a fresh, complete free-inode
// history and a fresh total; otherwise a reason says what is missing.
func (self *minioRunwayProbe) inodeEstimates(observation *minioRunwayHost, end time.Time) ([]minioRunwayDriveEstimate, bool, string) {
	settings := self.settings
	mountpoints := make([]string, 0, len(observation.inodes))
	for mountpoint := range observation.inodes {
		mountpoints = append(mountpoints, mountpoint)
	}
	sort.Strings(mountpoints)
	drives := []minioRunwayDriveEstimate{}
	reason := observation.inodeFault
	for _, mountpoint := range mountpoints {
		drive := observation.inodes[mountpoint]
		if drive.reason != "" {
			reason = drive.reason
			continue
		}
		if drive.avail == nil {
			reason = "missing-drive-free-inode-series"
			continue
		}
		if count := len(drive.size); count > 0 && drive.size[count-1].value == 0 {
			// A filesystem without a fixed inode table reports no inodes at
			// all: zero free is no wall there, and this layer cannot judge it.
			reason = "drive-reports-no-inodes"
			continue
		}
		estimate := minioRunwayDriveEstimate{drive: drive}
		if len(drive.size) > 0 && drive.size[len(drive.size)-1].at.Equal(end) {
			estimate.size, estimate.sizeKnown = drive.size[len(drive.size)-1].value, true
		}
		// The runway counts down to zero; the floors are judged apart.
		estimate.estimate = estimateStorageRunway(settings.Estimator, drive.avail, end, 0)
		switch {
		case reason != "":
		case !estimate.estimate.fresh:
			reason = "stale-drive-inode-series"
		case !estimate.estimate.complete:
			reason = "incomplete-decline-windows"
		case !estimate.sizeKnown:
			reason = "missing-or-stale-drive-inode-total"
		case estimate.estimate.latest.value > estimate.size:
			reason = "inconsistent-drive-inodes"
		}
		drives = append(drives, estimate)
	}
	count, countReason := observation.gauge("drive_count", end)
	switch {
	case reason != "":
	case countReason != "":
		reason = countReason
	case count < 1 || count != math.Trunc(count) || int(count) != len(observation.inodes):
		reason = fmt.Sprintf("drive-filesystems=%d minio-drive-count=%.0f", len(observation.inodes), count)
	}
	return drives, reason == "", reason
}

// Free inodes per drive against the fixed floors, and the shortest days to
// zero at the fastest measured decline. Every object puts a shard on every
// drive, so the drives run out together and the first one out fails every
// write. A violation needs only a fresh drive; healthy needs the full census.
func (self *minioRunwayProbe) inodeRunway(hostName string, drives []minioRunwayDriveEstimate, census bool, censusReason string, now time.Time, gateway string, pendingRiskSamples map[string]storageRunwaySample) []finding {
	settings := self.settings
	target := hostName + "/inodes"
	var worst, leastFree *minioRunwayDriveEstimate
	worstRunway := time.Duration(math.MaxInt64)
	recentRate, recentKnown := 0.0, false
	for index := range drives {
		drive := &drives[index]
		if drive.estimate.recentKnown {
			recentRate, recentKnown = max(recentRate, drive.estimate.recentRate), true
		}
		if !drive.estimate.fresh {
			continue
		}
		if leastFree == nil || drive.estimate.latest.value < leastFree.estimate.latest.value {
			leastFree = drive
		}
		if runway, declining := drive.estimate.runway(); declining && runway < worstRunway {
			worst, worstRunway = drive, runway
		}
	}
	if recentKnown {
		pendingRiskSamples[target] = storageRunwaySample{at: now, value: recentRate}
	}
	freeBand, runwayBand := "none", "none"
	if leastFree != nil {
		switch free := leastFree.estimate.latest.value; {
		case free < settings.InodePageFree:
			freeBand = "page"
		case free < settings.InodeWarnFree:
			freeBand = "warn"
		}
	}
	if worst != nil {
		switch {
		case worstRunway <= settings.InodePageRunway:
			runwayBand = "page"
		case worstRunway <= settings.InodeWarnRunway:
			runwayBand = "warn"
		}
	}
	findings := []finding{}
	band, tier := "", tierWarn
	switch {
	case freeBand == "page" || runwayBand == "page":
		band, tier = "page", tierPage
	case freeBand == "warn" || runwayBand == "warn":
		band = "warn"
	}
	if band != "" {
		warnDays, pageDays := settings.InodeWarnRunway.Hours()/24, settings.InodePageRunway.Hours()/24
		floor := settings.InodeWarnFree
		if band == "page" {
			floor = settings.InodePageFree
		}
		shown := leastFree
		symptom := fmt.Sprintf("MinIO drive %s on %s has %s free inodes, under the %s %s floor",
			leastFree.drive.mountpoint, hostName, minioRunwayInodeCount(leastFree.estimate.latest.value), minioRunwayInodeCount(floor), band)
		if runwayBand == band {
			shown = worst
			symptom = fmt.Sprintf("MinIO drive %s on %s runs out of inodes in %.1f days at its fastest measured decline, with %s free",
				worst.drive.mountpoint, hostName, worstRunway.Hours()/24, minioRunwayInodeCount(worst.estimate.latest.value))
		}
		parts := []string{}
		for _, drive := range drives {
			if drive.estimate.fresh {
				parts = append(parts, drive.drive.mountpoint+" free_inodes="+strconv.FormatFloat(drive.estimate.latest.value, 'f', 0, 64))
			}
		}
		findings = append(findings, finding{
			probeId: minioRunwayProbeId, tier: tier, class: "minio-inode-runway", target: target, sustain: settings.Sustain,
			symptom: symptom,
			mechanism: "XFS allocates inodes only up to its imaxpct ceiling (node_filesystem_files). Once a drive has no free inode, every new file or directory on it fails with ENOSPC however many bytes are free, and MinIO fails every write: Loki chunks, Mimir blocks and blob objects (SN25 artifacts and evidence). Every object puts a shard, a directory and an xl.meta, on every drive, so the drives run out together, and neither MinIO's bucket accounting nor the byte runway sees it. The rate is the fastest decline over the " +
				storageRunwayWindowNames(settings.Estimator.Windows) + " windows of the drive's own free-inode count, and the runway counts down to zero.",
			baseline: fmt.Sprintf("Every drive keeps at least %s free inodes and more than %.0f days of them at its fastest measured decline; WARN under %s or at <= %.0f days, PAGE under %s or at <= %.0f days, each after %d observations.",
				minioRunwayInodeCount(settings.InodeWarnFree), warnDays, minioRunwayInodeCount(settings.InodeWarnFree), warnDays, minioRunwayInodeCount(settings.InodePageFree), pageDays, settings.Sustain),
			observed: "band=" + band + " free_band=" + freeBand + " runway_band=" + runwayBand + " worst_drive=" + shown.drive.mountpoint + " " + minioRunwayInodeObserved(shown) +
				fmt.Sprintf(" least_free_drive=%s least_free_inodes=%.0f census_complete=%t drives=[%s]", leastFree.drive.mountpoint, leastFree.estimate.latest.value, census, strings.Join(parts, "; ")),
			evidence: "One bounded loopback Mimir range query of node_filesystem_files_free and node_filesystem_files for the inventory MinIO host's drive mountpoints, joined with MinIO's own minio_system_drive_count, through gateway " + gateway + ".",
			context:  "On 2026-11-07 each of edge-5's eight XFS drives (imaxpct=5, a 375.07M-inode ceiling) had about 42.85M free inodes (89% used) beside terabytes of free bytes, losing about 8M inodes a day and up to 17-20M a day over the two weeks before. False-positive qualifiers: a bulk delete or lifecycle expiry returns inodes in bursts, which shortens the measured decline only in the windows it falls in. False-negative qualifiers: a raised ceiling (xfs_growfs -m) adds free inodes at once, so for up to a day only the windows after it measure the decline, and a burst of small objects can outrun the 15-minute cadence.",
			action:   "Cut object growth at its source first (§14.8: the client-key evidence history copies and other small-object prefixes, and lifecycle rules for expired copies). Raising the ceiling is the owner's decision: `xfs_growfs -m <percent> /mnt/blobN` on every drive works online and moves no data (15% gives each 3.84 TB drive about 1.13B inodes). Read the drives with `df -i /mnt/blob*`. Do not delete files under a drive's minio directory by hand.",
			verify:   fmt.Sprintf("Two consecutive complete observations with every drive at or above %s free inodes and over %.0f days of runway.", minioRunwayInodeCount(settings.InodeWarnFree), warnDays),
			playbook: "SIGNALS.md §11.6b and §2.4f",
		})
	}
	if !census {
		tier, basis := self.inodeUnknownTier(target, recentRate, recentKnown, now)
		return append(findings, self.unknown(target, "inodes", censusReason, tier, basis, nil))
	}
	if band == "" {
		findings = append(findings, healthyFinding(minioRunwayProbeId, tierPage, "minio-inode-runway", target))
	}
	return append(findings, healthyFinding(minioRunwayProbeId, tierWarn, "minio-runway-unobservable", target))
}

// Emission rule 1 for the inode layer, as storageRunwayRiskMemory.unknownTier
// is for bytes: an unknown inode layer pages when a drive lost more than the
// page line of inodes an hour over the unknown-risk window, measured now or
// remembered from a run inside it.
func (self *minioRunwayProbe) inodeUnknownTier(target string, recentRate float64, recentKnown bool, now time.Time) (string, string) {
	settings := self.settings
	rate, known, basis := recentRate, recentKnown, "measured"
	if !known {
		rate, known = self.risks.recall(target, now, settings.Estimator.UnknownRiskWindow)
		basis = "remembered"
	}
	if !known {
		return tierWarn, "recent_decline=unknown"
	}
	text := "recent_decline_basis=" + basis + " recent_decline_inodes_per_hour=" + strconv.FormatFloat(rate*3600, 'f', 0, 64)
	if rate*3600 > settings.InodeUnknownPageDeclinePerHour {
		return tierPage, text
	}
	return tierWarn, text
}

// Fixed public fields of one drive's inode estimate: counts, inodes a day
// per decline window and days to zero. No device or label value enters them.
func minioRunwayInodeObserved(drive *minioRunwayDriveEstimate) string {
	estimate := drive.estimate
	parts := []string{"free_inodes=" + strconv.FormatFloat(estimate.latest.value, 'f', 0, 64)}
	if drive.sizeKnown {
		parts = append(parts, "total_inodes="+strconv.FormatFloat(drive.size, 'f', 0, 64),
			"used_pct="+strconv.FormatFloat(100*(1-estimate.latest.value/drive.size), 'f', 2, 64))
	}
	for _, decline := range estimate.declines {
		name := "decline_" + storageRunwayWindowName(decline.window) + "_inodes_per_day="
		if !decline.measured {
			parts = append(parts, name+"unknown")
			continue
		}
		parts = append(parts, name+strconv.FormatFloat(decline.rate*86400, 'f', 0, 64))
	}
	if runway, declining := estimate.runway(); declining {
		parts = append(parts, "rate_window="+storageRunwayWindowName(estimate.rateWindow),
			"runway_days="+strconv.FormatFloat(runway.Hours()/24, 'f', 1, 64))
	} else {
		parts = append(parts, "runway_days=none-no-decline")
	}
	return strings.Join(parts, " ")
}

// Inode counts in millions with at most two decimals: 42.85M, 15M.
func minioRunwayInodeCount(count float64) string {
	return strconv.FormatFloat(math.Round(count/1e4)/100, 'f', -1, 64) + "M"
}

// Raw used bytes over the drives minus the erasure-coded logical bytes:
// foreign data, scanner lag or small-object overhead.
func (self *minioRunwayProbe) accounting(hostName string, observation *minioRunwayHost, drives []minioRunwayDriveEstimate, census bool, censusReason string, end time.Time, gateway string) []finding {
	settings := self.settings
	target := hostName + "/accounting"
	if !census {
		return []finding{self.unknown(target, "accounting", censusReason, tierWarn, "", nil)}
	}
	logical, reason := observation.gauge("logical_bytes", end)
	if reason != "" {
		return []finding{self.unknown(target, "accounting", reason, tierWarn, "", nil)}
	}
	rawUsed := 0.0
	for _, drive := range drives {
		rawUsed += drive.size - drive.estimate.latest.value
	}
	divergence := rawUsed - settings.ErasureOverhead*logical
	// 1.333 for 6+2, 2 for 2+2
	overhead := strconv.FormatFloat(settings.ErasureOverhead, 'g', 4, 64)
	usageAge := "unknown"
	if age, ageReason := observation.gauge("usage_age", end); ageReason == "" {
		usageAge = strconv.FormatFloat(age, 'f', 0, 64)
	}
	if divergence <= settings.DivergenceWarnBytes {
		return []finding{
			healthyFinding(minioRunwayProbeId, tierWarn, "minio-accounting-divergence", target),
			healthyFinding(minioRunwayProbeId, tierWarn, "minio-runway-unobservable", target),
		}
	}
	return []finding{
		{
			probeId: minioRunwayProbeId, tier: tierWarn, class: "minio-accounting-divergence", target: target, sustain: settings.Sustain,
			symptom:   fmt.Sprintf("MinIO's drives on %s hold %.2f TB more raw data than %s times its logical bucket bytes", hostName, divergence/1e12, overhead),
			mechanism: "Raw used space on the drive filesystems should be about the erasure overhead (the set's drives over its data shards) times MinIO's logical bucket bytes. The excess is data beside MinIO on the same drives (the PostgreSQL base backup), scanner lag in a large flat prefix, or small-object overhead: each small object costs a directory and an xl.meta on every drive.",
			baseline:  fmt.Sprintf("raw_used_bytes - %s * logical_bytes <= %.0f bytes.", overhead, settings.DivergenceWarnBytes),
			observed: fmt.Sprintf("raw_used_bytes=%.0f logical_bytes=%.0f erasure_overhead=%s divergence_bytes=%.0f divergence_tb=%.2f drives=%d usage_snapshot_age_s=%s",
				rawUsed, logical, overhead, divergence, divergence/1e12, len(drives), usageAge),
			evidence: "Fresh drive filesystem size and available bytes for every drive MinIO counts, and MinIO's own minio_cluster_usage_buckets_total_bytes, through the loopback Mimir gateway " + gateway + ".",
			context:  "False-positive qualifiers: a scheduled backup's transient second generation, and scanner lag right after a MinIO restart, which leaves the logical figure low. The logical bytes are cluster-wide, so the comparison holds for a single-node store like main's; on a multi-node store each host's drives hold only its share. usage_snapshot_age_s is the objects-path gauge in seconds; this release writes the buckets-path gauge of the same name in nanoseconds.",
			action:   "Name the excess before acting: the base backup's own journal and inventory (`pg_basebackup_retain`), the scanner's progress (`mc admin scanner status` as root), and the object-count trend of the client-key evidence prefix. The monitor's user cannot read MinIO's or the backup's directories, so `du` from it reads near zero; use the owner's privileged du.",
			verify:   "Two consecutive complete observations with the divergence under the warning line.",
			playbook: "SIGNALS.md §11.6b",
		},
		healthyFinding(minioRunwayProbeId, tierWarn, "minio-runway-unobservable", target),
	}
}

// The newest PutObject rate against the median of its own history.
func (self *minioRunwayProbe) putSurge(hostName string, observation *minioRunwayHost, grid storageRunwayGrid, gateway string) []finding {
	settings := self.settings
	target := hostName + "/puts"
	current, reason := observation.gauge("put_rate", grid.end)
	if reason != "" {
		return []finding{self.unknown(target, "puts", reason, tierWarn, "", nil)}
	}
	history := []float64{}
	earliest := time.Time{}
	for _, sample := range observation.gauges["put_rate"] {
		if sample.at.Before(grid.end.Add(-settings.PutSurgeHistory)) || sample.at.After(grid.end.Add(-settings.PutRateWindow)) {
			continue
		}
		history = append(history, sample.value)
		if earliest.IsZero() {
			earliest = sample.at
		}
	}
	if len(history) == 0 || grid.end.Sub(earliest) < settings.PutSurgeMinimumHistory {
		return []finding{self.unknown(target, "puts", "insufficient-put-history", tierWarn, "", nil)}
	}
	median := medianFloat64(history)
	if current < settings.PutSurgeMinimumRate || current <= settings.PutSurgeMultiple*median {
		return []finding{
			healthyFinding(minioRunwayProbeId, tierWarn, "minio-put-surge", target),
			healthyFinding(minioRunwayProbeId, tierWarn, "minio-runway-unobservable", target),
		}
	}
	return []finding{
		{
			probeId: minioRunwayProbeId, tier: tierWarn, class: "minio-put-surge", target: target, sustain: settings.Sustain,
			symptom:   fmt.Sprintf("MinIO on %s takes %.0f PutObject requests per second, over %.0f times its %s median", hostName, current, settings.PutSurgeMultiple, storageRunwayWindowName(settings.PutSurgeHistory)),
			mechanism: "Every small object costs far more raw space than its logical size, and MinIO's scanner lags behind a flat prefix, so a put surge fills the drives faster than bucket accounting shows. On 10-22 client-key evidence wrote about two puts per registration under identity churn.",
			baseline:  fmt.Sprintf("The PutObject rate over %s stays at or under %.0f times its median over %s, or under %.0f per second.", storageRunwayWindowName(settings.PutRateWindow), settings.PutSurgeMultiple, storageRunwayWindowName(settings.PutSurgeHistory), settings.PutSurgeMinimumRate),
			observed: fmt.Sprintf("put_rate_per_second=%.1f median_per_second=%.1f multiple=%.1f history_hours=%.1f history_points=%d",
				current, median, current/max(median, 1e-9), grid.end.Sub(earliest).Hours(), len(history)),
			evidence: "MinIO's own minio_api_requests_total{name=\"PutObject\"} rate, summed by inventory host, through the loopback Mimir gateway " + gateway + ".",
			context:  "The median includes a long surge once it has lasted days, so the multiple falls while the surge continues; read the rate itself as well. A MinIO restart can extrapolate a counter increase.",
			action:   "Find the writer before acting: correlate §14.8 identity churn and the client-key evidence prefixes, and cut the growth at its source. Do not raise drive capacity alerts or quotas to absorb it.",
			verify:   "Two consecutive observations with the PutObject rate back under the multiple of its median.",
			playbook: "SIGNALS.md §11.6b and §14.8",
		},
		healthyFinding(minioRunwayProbeId, tierWarn, "minio-runway-unobservable", target),
	}
}

// The base backup's units on one host, reduced to fixed states.
type minioRunwayBackupUnits struct {
	serviceLoaded bool
	serviceActive bool
	timerLoaded   bool
	timerActive   bool
	timerNext     time.Time
}

// Fixed lines from the configured backup units behind a hostname guard. No
// path, argv or journal text leaves the host.
const minioRunwayBackupCommandTemplate = `[ "$(hostname -s)" = {host} ] || exit 74
printf 'monitor-unit service\n'
timeout {unit_timeout}s systemctl show --no-pager --timestamp=unix -p LoadState -p ActiveState -- {service_unit}
printf 'monitor-unit timer\n'
timeout {unit_timeout}s systemctl show --no-pager --timestamp=unix -p LoadState -p ActiveState -p NextElapseUSecRealtime -- {timer_unit}
printf 'monitor-unit end\n'
`

// The backup-unit read for one inventory host.
func minioRunwayBackupCommand(hostName string, settings *MinioRunwaySettings) string {
	shortName, _, _ := strings.Cut(hostName, ".")
	return strings.NewReplacer(
		"{host}", shellSingleQuote(shortName),
		"{unit_timeout}", strconv.Itoa(max(int(settings.BackupUnitTimeout/time.Second), 1)),
		"{service_unit}", shellSingleQuote(settings.BackupServiceUnit),
		"{timer_unit}", shellSingleQuote(settings.BackupTimerUnit),
	).Replace(minioRunwayBackupCommandTemplate)
}

// Exactly the framed properties, once each, in the framed units.
func parseMinioRunwayBackupUnits(output string, maxBytes int) (minioRunwayBackupUnits, string) {
	units := minioRunwayBackupUnits{}
	if len(output) > maxBytes || !strings.HasSuffix(output, "monitor-unit end\n") {
		return units, "incomplete-unit-frame"
	}
	section := ""
	properties := map[string]map[string]string{"service": {}, "timer": {}}
	for _, line := range strings.Split(strings.TrimSuffix(output, "\n"), "\n") {
		switch line {
		case "monitor-unit service", "monitor-unit timer":
			next := strings.TrimPrefix(line, "monitor-unit ")
			if (next == "service" && section != "") || (next == "timer" && section != "service") {
				return units, "unordered-unit-frame"
			}
			section = next
			continue
		case "monitor-unit end":
			if section != "timer" {
				return units, "unordered-unit-frame"
			}
			section = "end"
			continue
		}
		key, value, found := strings.Cut(line, "=")
		allowed := key == "LoadState" || key == "ActiveState" || (section == "timer" && key == "NextElapseUSecRealtime")
		if !found || (section != "service" && section != "timer") || !allowed {
			return units, "unexpected-unit-line"
		}
		if _, duplicate := properties[section][key]; duplicate {
			return units, "duplicate-unit-property"
		}
		properties[section][key] = value
	}
	if section != "end" {
		return units, "incomplete-unit-frame"
	}
	state := func(section string) (bool, bool, bool) {
		load, active := properties[section]["LoadState"], properties[section]["ActiveState"]
		switch {
		case load == "not-found" || load == "masked":
			return false, false, true
		case load != "loaded":
			return false, false, false
		}
		switch active {
		case "active", "activating", "deactivating", "reloading":
			return true, true, true
		case "inactive", "failed":
			return true, false, true
		}
		return false, false, false
	}
	var valid bool
	units.serviceLoaded, units.serviceActive, valid = state("service")
	if !valid {
		return units, "unknown-service-state"
	}
	units.timerLoaded, units.timerActive, valid = state("timer")
	if !valid {
		return units, "unknown-timer-state"
	}
	switch next := properties["timer"]["NextElapseUSecRealtime"]; {
	case next == "" || next == "n/a" || next == "0":
	case strings.HasPrefix(next, "@") && len(next) > 1 && len(next) <= 20 && strings.Trim(next[1:], "0123456789") == "":
		seconds, err := strconv.ParseInt(next[1:], 10, 64)
		if err != nil {
			return units, "invalid-timer-elapse"
		}
		units.timerNext = time.Unix(seconds, 0).UTC()
	default:
		return units, "invalid-timer-elapse"
	}
	return units, ""
}

// The primary's backup replication slots, as counts only.
const minioRunwayBackupSlotTemplate = `{hostname_guard}
SELECT 'slots', :'monitor_primary_hostname'::text, (count(*) FILTER (WHERE slot_name LIKE '{slot_pattern}'))::text,
       CASE WHEN pg_is_in_recovery() THEN '1' ELSE '0' END
FROM pg_replication_slots;
`

// The slot count query for a validated prefix; its underscores match only
// themselves.
func minioRunwayBackupSlotQuery(prefix string) string {
	return strings.NewReplacer(
		"{hostname_guard}", pgPrimaryHostnameMetaCommand,
		"{slot_pattern}", strings.ReplaceAll(prefix, "_", `\_`)+"%",
	).Replace(minioRunwayBackupSlotTemplate)
}

var minioRunwayBackupSlotPrefixPattern = regexp.MustCompile(`^[a-z][a-z0-9_]{0,40}$`)

// A backup that is running, holds its slot or is due on its timer while the
// drives lack the room its own admission needs above the reserve warns
// before it writes. Source failures stay unknown for this layer only.
func (self *minioRunwayProbe) backup(ctx context.Context, env *probeEnv, configured *host, drives []minioRunwayDriveEstimate, census bool, censusReason string) ([]finding, error) {
	settings := self.settings
	target := configured.name + "/backup"
	output, err := env.runner.sshTimeout(ctx, configured, minioRunwayBackupCommand(configured.name, settings), "", settings.BackupCommandTimeout)
	if lifecycleErr := probeLifecycleError(ctx, err); lifecycleErr != nil {
		return nil, lifecycleErr
	}
	if err != nil {
		return []finding{self.unknown(target, "backup", "backup-unit-unavailable", tierWarn, "", err)}, nil
	}
	units, reason := parseMinioRunwayBackupUnits(output, settings.BackupMaxOutputBytes)
	if reason != "" {
		return []finding{self.unknown(target, "backup", reason, tierWarn, "", nil)}, nil
	}
	if !units.serviceLoaded && !units.timerLoaded {
		// This host has no base-backup writer.
		return []finding{
			healthyFinding(minioRunwayProbeId, tierWarn, "minio-backup-reserve", target),
			healthyFinding(minioRunwayProbeId, tierWarn, "minio-runway-unobservable", target),
		}, nil
	}
	if !minioRunwayBackupSlotPrefixPattern.MatchString(settings.BackupSlotPrefix) {
		return []finding{self.unknown(target, "backup", "invalid-slot-prefix", tierWarn, "", nil)}, nil
	}
	slots := int64(0)
	primaries := env.cfg.hostsWithRole("pg-primary")
	slotReason := ""
	if len(primaries) != 1 {
		slotReason = "primary-inventory-unavailable-or-ambiguous"
	} else {
		rows, err := env.runner.pg(ctx, minioRunwayBackupSlotQuery(settings.BackupSlotPrefix))
		if lifecycleErr := probeLifecycleError(ctx, err); lifecycleErr != nil {
			return nil, lifecycleErr
		}
		shortName, _, _ := strings.Cut(primaries[0].name, ".")
		switch {
		case err != nil:
			return []finding{self.unknown(target, "backup", "replication-slots-unavailable", tierWarn, "", err)}, nil
		case len(rows) != 1 || len(rows[0]) != 4 || rows[0][0] != "slots" || rows[0][1] != shortName || rows[0][3] != "0":
			slotReason = "replication-slot-frame-invalid"
		default:
			count, valid := pgFreezeAgeInteger(rows[0][2], 0, math.MaxInt32)
			if !valid {
				slotReason = "replication-slot-frame-invalid"
			}
			slots = count
		}
	}
	if slotReason != "" {
		return []finding{self.unknown(target, "backup", slotReason, tierWarn, "", nil)}, nil
	}
	now := env.now().UTC()
	triggers := []string{}
	if units.serviceActive {
		triggers = append(triggers, "service-active")
	}
	if units.serviceLoaded && slots > 0 {
		triggers = append(triggers, "replication-slot")
	}
	if units.timerActive && !units.timerNext.IsZero() && !units.timerNext.After(now.Add(settings.BackupTimerHorizon)) {
		triggers = append(triggers, "timer-due")
	}
	if len(triggers) == 0 {
		return []finding{
			healthyFinding(minioRunwayProbeId, tierWarn, "minio-backup-reserve", target),
			healthyFinding(minioRunwayProbeId, tierWarn, "minio-runway-unobservable", target),
		}, nil
	}
	if !census {
		return []finding{self.unknown(target, "backup", censusReason, tierWarn, "", nil)}, nil
	}
	usable := 0.0
	leastFree := drives[0]
	for _, drive := range drives {
		usable += max(0, drive.estimate.latest.value-settings.BackupReserveBytesPerDrive)
		if drive.estimate.latest.value < leastFree.estimate.latest.value {
			leastFree = drive
		}
	}
	if usable >= settings.BackupMinimumFreeBytes {
		return []finding{
			healthyFinding(minioRunwayProbeId, tierWarn, "minio-backup-reserve", target),
			healthyFinding(minioRunwayProbeId, tierWarn, "minio-runway-unobservable", target),
		}, nil
	}
	return []finding{
		{
			probeId: minioRunwayProbeId, tier: tierWarn, class: "minio-backup-reserve", target: target, sustain: 1,
			symptom:   "A PostgreSQL base backup is about to write beside MinIO on " + configured.name + " without the room its own admission needs above the per-drive reserve",
			mechanism: "The base backup writes its pieces and WAL chunks to directories at the roots of MinIO's drives and keeps a per-drive reserve free for MinIO. Its own admission needs at least the configured minimum (and 1.25 times the last generation) usable above that reserve across the drives; with less, the run fails, or it consumes the headroom MinIO's runway depends on.",
			baseline:  fmt.Sprintf("While a backup runs, holds its replication slot or is due within %s, the drives keep at least %.0f bytes usable above the %.0f-byte per-drive reserve.", storageRunwayWindowName(settings.BackupTimerHorizon), settings.BackupMinimumFreeBytes, settings.BackupReserveBytesPerDrive),
			observed: fmt.Sprintf("triggers=%s usable_above_reserve_bytes=%.0f required_at_least_bytes=%.0f reserve_per_drive_bytes=%.0f least_free_drive=%s least_free_available_bytes=%.0f backup_slots=%d",
				strings.Join(triggers, ","), usable, settings.BackupMinimumFreeBytes, settings.BackupReserveBytesPerDrive, leastFree.drive.mountpoint, leastFree.estimate.latest.value, slots),
			evidence: "Fixed systemd unit states read on the MinIO host behind a hostname guard, the primary's replication-slot count for the configured prefix behind its own hostname guard, and the drives' fresh filesystem bytes from Mimir.",
			context:  "The last generation's size is in backup-info.json inside directories only the backup account can read, so the required figure is the configured minimum, a lower bound. A run that cannot fit fails before writing a piece below the reserve; the warning is that the run is about to start or is running without its own required room.",
			action:   "Have the backup owner stop or defer the run until there is room for two generations above the reserve, or free space at its source first (§11.6b). Do not lower the reserve or delete MinIO data to make room.",
			verify:   "No backup trigger remains, or the drives keep at least the required room above the reserve on the next observation.",
			playbook: "SIGNALS.md §11.6b",
		},
		healthyFinding(minioRunwayProbeId, tierWarn, "minio-runway-unobservable", target),
	}, nil
}

// A gap in one layer is unknown for that layer; the drive and inode layers'
// tiers follow emission rule 1 (SIGNALS.md §7).
func (self *minioRunwayProbe) unknown(target, frame, reason, tier, basis string, err error) finding {
	settings := self.settings
	observed := "layer=" + frame + " reason=" + reason
	if basis != "" {
		observed += " " + basis
	}
	if err != nil {
		observed += " " + observationFailureDetails(err, classifyObservationError(err))
	}
	sustain := settings.Sustain
	return finding{
		probeId: minioRunwayProbeId, tier: tier, class: "minio-runway-unobservable", target: target, sustain: sustain,
		symptom:   "MinIO's " + frame + " layer on " + strings.SplitN(target, "/", 2)[0] + " is unknown",
		mechanism: "The gateway, the bounded Mimir answer, MinIO's own drive count, a fresh complete drive history of bytes or of inodes, the logical or PutObject series, or the backup unit and slot reads could not be established. A missing series is unknown, never headroom.",
		baseline: fmt.Sprintf("Every drive MinIO counts has fresh complete filesystem history and size, in bytes and in inodes; logical bytes and the PutObject rate are fresh with at least %s of history; the backup units and slots read cleanly. An unknown drive runway pages when a drive declined more than %.0f GB/h over the last %s, and an unknown inode runway when a drive lost more than %s inodes an hour over it.",
			storageRunwayWindowName(settings.PutSurgeMinimumHistory), gb(settings.Estimator.UnknownPageDeclineBytesPerHour), storageRunwayWindowName(settings.Estimator.UnknownRiskWindow), minioRunwayInodeCount(settings.InodeUnknownPageDeclinePerHour)),
		observed: observed,
		evidence: "Only fixed reasons, byte, inode and rate figures and error classes are retained; raw answers, labels, devices, unit text and source errors stay private.",
		context:  "Unknown is not healthy for a safety property. Correlate §11.14 log-shipper and §11.6a minio-storage before treating a dark store as quiet.",
		action:   observationFailureAction(err, classifyObservationError(err), "Restore the node and MinIO metric shipping and the bounded Mimir, unit and slot reads. While a paging unknown lasts, read the drives' free space and inodes on the host directly (df -B1 /mnt/blob* and df -i /mnt/blob*) and treat them as filling at the last measured rate."),
		verify:   "A fresh complete observation of the layer restores it; only a complete layer outside its bands is healthy.",
		playbook: "SIGNALS.md §11.6b and §7",
	}
}
