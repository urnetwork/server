package monitor

// Signal filecluster-cohort implements SIGNALS.md §29.7 (EVSB-M3-02; §19.9's cohorts, first and triggers rows,
// D-OPS-17, D-OPS-22, owner decisions 136 and 145, FS-119). A metadata cluster's repos (its cohort) keep growing after
// place_new moves on, and a placed repo never moves, so each cluster is projected from its own slopes and the estate's
// free space moves nothing. Every 30 min, per metadata cluster:
//
//   - raw space and inodes now from admin info, the line evs's 507 CLUSTER_FULL reads, and their 30-day slopes from
//     each drive's filesystem (node_filesystem_avail_bytes and node_filesystem_files_free now and 30 days back; a
//     drive joins its cluster where its mountpoint is a drive path admin info lists, as §29.4's do);
//   - the busiest drive's 7-day DWPD (urnetwork_filecluster_drive_dwpd{window="7d"}) now and 30 days back;
//   - copy load, path-epoch copies' operations a day against half the cluster's 21.6M: no process publishes it yet,
//     so copy is never projected and each finding says so; it is not a standing visibility finding, which would hold
//     every other incident of the signal open;
//   - the first cluster's control plane apart, urnetwork_filecluster_control_plane_bytes (the nightly start's), with
//     its 30-day slope, shown beside the cluster's own projection.
//
// It publishes urnetwork_filecluster_exhaustion_days{cluster,resource} for raw, inodes and wear (and hdd_ops on a
// segment cluster: its drive operations' 30-day trend against half its measured capacity,
// g0-recheck-0-hdd-trigger), the days until the resource reaches its line at its slope, 0 at or past it and +Inf
// while it does not grow; a resource with no 30-day reading is not projected. Classes:
//
//   - filecluster-cohort-limit, §19.8's business-hours page: the cluster stands at its cohort limit, 80% raw or 70%
//     inodes, and evs refuses its repos' payload 507 CLUSTER_FULL;
//   - filecluster-cohort-exhaustion, §19.8's business-hours page: the cluster's earliest exhaustion (80% raw, 70%
//     inodes, a 0.75 DWPD 7-day mean) falls inside the move tool's lead time; the finding names the estate's
//     earliest;
//   - filecluster-cohort-trigger, a warn: D-OPS-17's marks, 50% raw or 40% inodes, reached or projected inside the
//     next metadata cluster's lead time (the 0.5 DWPD mark reached is §29.4's filecluster-drive-dwpd-week, and only
//     its projection is here);
//   - filecluster-hdd-trigger, §19.8's business-hours page: a segment cluster's projected hdd_ops crossing inside the
//     segment placement's lead time;
//   - filecluster-cohort-unobservable reports what could not be read.
//
// §19.8's business-hours pages wait for a business-hours path in the pager (OPS); until it has one they warn on
// Slack, as §29.4's do. No series or finding names a network or repo.

import (
	"context"
	"fmt"
	"math"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/urnetwork/server"
)

func NewFileClusterCohortSignal() Signal {
	return &signalAdapter{
		number: "29.7", key: "filecluster-cohort", name: "File-cluster cohorts and their exhaustion",
		probe: fileClusterCohortProbe{},
	}
}

const fileClusterCohortProbeId = "filecluster/cohort"

// SIGNALS.md §29.7's classes.
const (
	fileClusterCohortClassLimit        = "filecluster-cohort-limit"
	fileClusterCohortClassExhaustion   = "filecluster-cohort-exhaustion"
	fileClusterCohortClassTrigger      = "filecluster-cohort-trigger"
	fileClusterCohortClassHddTrigger   = "filecluster-hdd-trigger"
	fileClusterCohortClassUnobservable = "filecluster-cohort-unobservable"
)

// urnetwork_filecluster_exhaustion_days's resource label (§19.8; g0-recheck-0-hdd-trigger adds hdd_ops).
const (
	fileClusterResourceRaw    = "raw"
	fileClusterResourceInodes = "inodes"
	fileClusterResourceWear   = "wear"
	fileClusterResourceCopy   = "copy"
	fileClusterResourceHddOps = "hdd_ops"
)

// The query's partitions.
const (
	fileClusterCohortCheckSize          = "filesystem_size"
	fileClusterCohortCheckAvail         = "filesystem_avail"
	fileClusterCohortCheckAvailThen     = "filesystem_avail_then"
	fileClusterCohortCheckFiles         = "filesystem_files"
	fileClusterCohortCheckFilesFree     = "filesystem_files_free"
	fileClusterCohortCheckFilesFreeThen = "filesystem_files_free_then"
	fileClusterCohortCheckDwpd          = "dwpd_7d"
	fileClusterCohortCheckDwpdThen      = "dwpd_7d_then"
	fileClusterCohortCheckControl       = "control_plane"
	fileClusterCohortCheckControlThen   = "control_plane_then"
	fileClusterCohortCheckHddOps        = "hdd_ops"
	fileClusterCohortCheckHddOpsThen    = "hdd_ops_then"
	fileClusterCohortCheckHddCapacity   = "hdd_capacity"
)

type fileClusterCohortProbe struct {
	// nil queries Mimir
	query sidecarStorageQuery
}

func (fileClusterCohortProbe) id() string             { return fileClusterCohortProbeId }
func (fileClusterCohortProbe) tier() string           { return tierWarn }
func (fileClusterCohortProbe) cadence() time.Duration { return 30 * time.Minute }

// The one query: each listed drive's filesystem now and a window back, the busiest drives' 7-day DWPD, the first
// cluster's control plane and the segment clusters' drive operations, each now and a window back.
func fileClusterCohortQuery(environment string, settings *SidecarStorageSettings, paths []string) string {
	fresh := func(metric string, check string, matchers ...string) string {
		return sidecarStorageTag(sidecarStorageFresh(sidecarStorageSelector(environment, metric, matchers...), settings.MetricFreshness), check)
	}
	then := func(metric string, check string, matchers ...string) string {
		return sidecarStorageTag(sidecarStorageSelector(environment, metric, matchers...)+" offset "+strings.Trim(sidecarStorageRange(settings.CohortTrendWindow), "[]"), check)
	}
	expressions := []string{
		fresh("urnetwork_filecluster_drive_dwpd", fileClusterCohortCheckDwpd, `window="7d"`),
		then("urnetwork_filecluster_drive_dwpd", fileClusterCohortCheckDwpdThen, `window="7d"`),
		fresh("urnetwork_filecluster_control_plane_bytes", fileClusterCohortCheckControl),
		then("urnetwork_filecluster_control_plane_bytes", fileClusterCohortCheckControlThen),
		fresh("urnetwork_filecluster_hdd_drive_ops", fileClusterCohortCheckHddOps),
		then("urnetwork_filecluster_hdd_drive_ops", fileClusterCohortCheckHddOpsThen),
		fresh("urnetwork_filecluster_hdd_capacity", fileClusterCohortCheckHddCapacity),
	}
	if 0 < len(paths) {
		quoted := make([]string, 0, len(paths))
		for _, drivePath := range paths {
			quoted = append(quoted, regexp.QuoteMeta(drivePath))
		}
		mountpoint := "mountpoint=~" + strconv.Quote(strings.Join(quoted, "|"))
		expressions = append(expressions,
			fresh("node_filesystem_size_bytes", fileClusterCohortCheckSize, mountpoint),
			fresh("node_filesystem_avail_bytes", fileClusterCohortCheckAvail, mountpoint),
			then("node_filesystem_avail_bytes", fileClusterCohortCheckAvailThen, mountpoint),
			fresh("node_filesystem_files", fileClusterCohortCheckFiles, mountpoint),
			fresh("node_filesystem_files_free", fileClusterCohortCheckFilesFree, mountpoint),
			then("node_filesystem_files_free", fileClusterCohortCheckFilesFreeThen, mountpoint),
		)
	}
	return sidecarStorageJoin(expressions...)
}

// One cluster's inputs, as admin info and one query gave them.
type fileClusterCohortInput struct {
	clusterId string
	kind      string
	// admin info's raw use and inodes now, the line evs's 507 reads
	info *server.FileClusterAdminInfo
	// its drives' filesystems summed, now and a window back
	sizeByteCount      float64
	availByteCount     float64
	availThenByteCount float64
	hasAvailThen       bool
	fileCount          float64
	filesFree          float64
	filesFreeThen      float64
	hasFilesFreeThen   bool
	filesystemCount    int
	// the busiest drive's 7-day DWPD now and a window back
	dwpd        float64
	hasDwpd     bool
	dwpdThen    float64
	hasDwpdThen bool
	// the control plane, the first cluster's alone
	controlBytes       float64
	hasControl         bool
	controlBytesThen   float64
	hasControlThen     bool
	hddOps, hddOpsThen float64
	hasHddOps          bool
	hasHddOpsThen      bool
	hddCapacity        float64
	hasHddCapacity     bool
}

// A resource's projection: its share now, its slope a day, and the days until it reaches line; known false
// without a reading a window old.
type fileClusterProjection struct {
	resource string
	now      float64
	perDay   float64
	line     float64
	days     float64
	known    bool
}

// The days until value, growing perDay, reaches line: 0 at or past it, +Inf while it does not grow.
func fileClusterDaysTo(value float64, perDay float64, line float64) float64 {
	switch {
	case line <= value:
		return 0
	case perDay <= 0:
		return math.Inf(1)
	}
	return (line - value) / perDay
}

// A projection of value, a window after then, toward line. At or past its line a resource is exhausted, 0 days,
// whatever its slope.
func fileClusterProject(resource string, value float64, then float64, hasThen bool, window time.Duration, line float64) *fileClusterProjection {
	projection := &fileClusterProjection{resource: resource, now: value, line: line}
	if 0 < line && line <= value {
		projection.known = true
		if hasThen {
			projection.perDay = (value - then) / (window.Hours() / 24)
		}
		return projection
	}
	if !hasThen {
		return projection
	}
	projection.perDay = (value - then) / (window.Hours() / 24)
	projection.days = fileClusterDaysTo(value, projection.perDay, line)
	projection.known = true
	return projection
}

func (self fileClusterCohortProbe) check(ctx context.Context, env *probeEnv) ([]finding, error) {
	settings := env.cfg.sidecarStorage
	if gate, ok := fileClusterSignalGate(settings, fileClusterCohortProbeId, fileClusterCohortClassUnobservable); ok {
		return []finding{gate}, nil
	}
	findings := []finding{}
	inputs := []*fileClusterCohortInput{}
	pathClusters := map[string]string{}
	paths := []string{}
	for _, clusterId := range settings.AdminSource.ClusterIds() {
		input := &fileClusterCohortInput{clusterId: clusterId, kind: settings.AdminSource.ClusterKind(clusterId)}
		info, err := settings.AdminSource.ReadAdminInfo(ctx, clusterId)
		if err != nil {
			findings = append(findings, self.cohortFinding(fileClusterCohortClassUnobservable, clusterId, "admin-info",
				"the cluster's admin info did not read, so its fill and its drives are unknown", err.Error()))
			continue
		}
		input.info = info
		inputs = append(inputs, input)
		if input.kind != server.FileClusterKindMetadata {
			continue
		}
		for _, drive := range info.Drives {
			if drivePath := fileClusterWearDrivePath(drive.Path, drive.Endpoint); drivePath != "" && pathClusters[drivePath] == "" {
				pathClusters[drivePath] = clusterId
				paths = append(paths, drivePath)
			}
		}
	}
	slices.Sort(paths)
	query := self.query
	if query == nil {
		query = sidecarStorageMimirQuery
	}
	samples, err := query(ctx, env, fileClusterCohortQuery(env.cfg.env, settings, paths))
	if err != nil {
		return append(findings, self.cohortFinding(fileClusterCohortClassUnobservable, "mimir", "mimir",
			"the clusters' filesystems, drive writes and drive operations did not read from Mimir", err.Error())), nil
	}
	return append(findings, self.evaluate(settings, inputs, pathClusters, samples)...), nil
}

// Every finding from the clusters' inputs and one query's samples, publishing each projection.
func (self fileClusterCohortProbe) evaluate(settings *SidecarStorageSettings, inputs []*fileClusterCohortInput, pathClusters map[string]string, samples []*sidecarStorageSample) []finding {
	clusterInputs := map[string]*fileClusterCohortInput{}
	firstMetadataId := ""
	for _, input := range inputs {
		clusterInputs[input.clusterId] = input
		if firstMetadataId == "" && input.kind == server.FileClusterKindMetadata {
			firstMetadataId = input.clusterId
		}
	}
	for _, sample := range samples {
		clusterId := sample.label("cluster")
		if mountpoint := sample.label("mountpoint"); mountpoint != "" {
			clusterId = pathClusters[path.Clean(mountpoint)]
		}
		input := clusterInputs[clusterId]
		if input == nil || !sidecarStorageCount(sample.value) {
			continue
		}
		switch sample.check {
		case fileClusterCohortCheckSize:
			input.sizeByteCount += sample.value
			input.filesystemCount += 1
		case fileClusterCohortCheckAvail:
			input.availByteCount += sample.value
		case fileClusterCohortCheckAvailThen:
			input.availThenByteCount += sample.value
			input.hasAvailThen = true
		case fileClusterCohortCheckFiles:
			input.fileCount += sample.value
		case fileClusterCohortCheckFilesFree:
			input.filesFree += sample.value
		case fileClusterCohortCheckFilesFreeThen:
			input.filesFreeThen += sample.value
			input.hasFilesFreeThen = true
		case fileClusterCohortCheckDwpd:
			// two lease holders' series overlap at a change: take the higher
			input.dwpd, input.hasDwpd = math.Max(input.dwpd, sample.value), true
		case fileClusterCohortCheckDwpdThen:
			input.dwpdThen, input.hasDwpdThen = math.Max(input.dwpdThen, sample.value), true
		case fileClusterCohortCheckControl:
			input.controlBytes, input.hasControl = sample.value, true
		case fileClusterCohortCheckControlThen:
			input.controlBytesThen, input.hasControlThen = sample.value, true
		case fileClusterCohortCheckHddOps:
			input.hddOps, input.hasHddOps = sample.value, true
		case fileClusterCohortCheckHddOpsThen:
			input.hddOpsThen, input.hasHddOpsThen = sample.value, true
		case fileClusterCohortCheckHddCapacity:
			input.hddCapacity, input.hasHddCapacity = sample.value, true
		}
	}

	findings := []finding{}
	publish := func(clusterId string, projection *fileClusterProjection) {
		if gauges, ok := settings.Gauges.(FileClusterExhaustionGauges); ok {
			gauges.PublishExhaustionDays(clusterId, projection.resource, projection.days, projection.known)
		}
	}
	type clusterEarliest struct {
		clusterId string
		resource  string
		days      float64
	}
	clusterProjections := map[string][]*fileClusterProjection{}
	var earliest *clusterEarliest
	for _, input := range inputs {
		switch input.kind {
		case server.FileClusterKindMetadata:
			projections, blind := self.metadataProjections(settings, input)
			for _, frame := range blind {
				findings = append(findings, self.cohortFinding(fileClusterCohortClassUnobservable, input.clusterId, frame,
					fileClusterCohortBlindSymptom(frame), ""))
			}
			clusterProjections[input.clusterId] = projections
			for _, projection := range projections {
				publish(input.clusterId, projection)
				if projection.known && (earliest == nil || projection.days < earliest.days) {
					earliest = &clusterEarliest{clusterId: input.clusterId, resource: projection.resource, days: projection.days}
				}
			}
		case server.FileClusterKindSegment:
			projection := fileClusterProject(fileClusterResourceHddOps, input.hddOps, input.hddOpsThen, input.hasHddOps && input.hasHddOpsThen,
				settings.CohortTrendWindow, settings.HddTriggerShare*input.hddCapacity)
			if !input.hasHddCapacity || input.hddCapacity <= 0 {
				// no measured capacity: §22.8's hdd run has not set one, so nothing is projected
				projection.known = false
			}
			publish(input.clusterId, projection)
			if projection.known && projection.days <= settings.HddTriggerLeadTime.Hours()/24 {
				findings = append(findings, self.cohortFinding(fileClusterCohortClassHddTrigger, input.clusterId, "",
					"the segment cluster's drive operations, at their 30-day trend, reach half its measured capacity inside the segment placement's lead time",
					fmt.Sprintf("hdd_ops=%.1f capacity=%.1f per_day=%.3f days=%.0f", input.hddOps, input.hddCapacity, projection.perDay, projection.days)))
			} else {
				findings = append(findings, healthyFinding(fileClusterCohortProbeId, tierWarn, fileClusterCohortClassHddTrigger, input.clusterId))
			}
		}
	}
	earliestText := "earliest: none projected"
	if earliest != nil {
		earliestText = fmt.Sprintf("earliest: %s %s in %s days", earliest.clusterId, earliest.resource, fileClusterDaysText(earliest.days))
	}

	for _, input := range inputs {
		if input.kind != server.FileClusterKindMetadata {
			continue
		}
		projections := clusterProjections[input.clusterId]
		observed := []string{}
		for _, projection := range projections {
			if projection.known {
				observed = append(observed, fmt.Sprintf("%s=%.3f per_day=%.5f days=%s", projection.resource, projection.now, projection.perDay, fileClusterDaysText(projection.days)))
			} else {
				observed = append(observed, fmt.Sprintf("%s=%.3f unprojected", projection.resource, projection.now))
			}
		}
		if input.clusterId == firstMetadataId {
			observed = append(observed, fileClusterControlPlaneText(settings, input))
		}
		observed = append(observed, earliestText)
		observedText := strings.Join(observed, " ")

		rawShare, inodeShare := fileClusterAdminShares(input.info)
		if settings.CohortRawLimit <= rawShare || settings.CohortInodeLimit <= inodeShare {
			findings = append(findings, self.cohortFinding(fileClusterCohortClassLimit, input.clusterId, "",
				"the metadata cluster stands at its cohort limit, 80% raw or 70% inodes: evs refuses its repos' payload 507 CLUSTER_FULL", observedText))
		} else {
			findings = append(findings, healthyFinding(fileClusterCohortProbeId, tierWarn, fileClusterCohortClassLimit, input.clusterId))
		}

		exhaustionDays := math.Inf(1)
		exhaustionKnown := false
		for _, projection := range projections {
			if projection.known && projection.days < exhaustionDays {
				exhaustionDays, exhaustionKnown = projection.days, true
			}
		}
		if exhaustionKnown && exhaustionDays <= settings.MoveToolLeadTime.Hours()/24 {
			findings = append(findings, self.cohortFinding(fileClusterCohortClassExhaustion, input.clusterId, "",
				"the cluster's own growth reaches 80% raw, 70% inodes or a 0.75 DWPD 7-day mean inside the move tool's lead time", observedText))
		} else {
			findings = append(findings, healthyFinding(fileClusterCohortProbeId, tierWarn, fileClusterCohortClassExhaustion, input.clusterId))
		}

		trigger := ""
		switch {
		case settings.TriggerRawShare <= rawShare:
			trigger = fmt.Sprintf("raw at %.3f", rawShare)
		case settings.TriggerInodeShare <= inodeShare:
			trigger = fmt.Sprintf("inodes at %.3f", inodeShare)
		}
		leadDays := settings.NextClusterLeadTime.Hours() / 24
		for _, projection := range self.triggerProjections(settings, input) {
			if trigger == "" && projection.known && projection.days <= leadDays {
				trigger = fmt.Sprintf("%s projected to its mark in %s days", projection.resource, fileClusterDaysText(projection.days))
			}
		}
		if trigger != "" {
			findings = append(findings, self.cohortFinding(fileClusterCohortClassTrigger, input.clusterId, "",
				"D-OPS-17's mark for the next metadata cluster is reached or projected inside its lead time: "+trigger, observedText))
		} else {
			findings = append(findings, healthyFinding(fileClusterCohortProbeId, tierWarn, fileClusterCohortClassTrigger, input.clusterId))
		}
	}
	return findings
}

// The cluster's raw and inode shares as admin info gives them, the line the budget loop's at_limit flag reads.
func fileClusterAdminShares(info *server.FileClusterAdminInfo) (rawShare float64, inodeShare float64) {
	if info == nil {
		return 0, 0
	}
	if 0 < info.RawCapacityByteCount {
		rawShare = float64(info.RawUsageByteCount) / float64(info.RawCapacityByteCount)
	}
	if inodeCount := info.UsedInodeCount + info.FreeInodeCount; 0 < inodeCount {
		inodeShare = float64(info.UsedInodeCount) / float64(inodeCount)
	}
	return rawShare, inodeShare
}

// A metadata cluster's projections toward its cohort lines (raw, inodes, wear), and the inputs it could not read,
// copy's always until a series carries it. Shares now are admin info's; slopes are the filesystems' over the window.
func (self fileClusterCohortProbe) metadataProjections(settings *SidecarStorageSettings, input *fileClusterCohortInput) ([]*fileClusterProjection, []string) {
	return self.projectionsToward(settings, input, settings.CohortRawLimit, settings.CohortInodeLimit, settings.CohortWearLimit)
}

// The same projections toward D-OPS-17's marks.
func (self fileClusterCohortProbe) triggerProjections(settings *SidecarStorageSettings, input *fileClusterCohortInput) []*fileClusterProjection {
	projections, _ := self.projectionsToward(settings, input, settings.TriggerRawShare, settings.TriggerInodeShare, settings.TriggerWearDwpd)
	return projections
}

func (self fileClusterCohortProbe) projectionsToward(settings *SidecarStorageSettings, input *fileClusterCohortInput, rawLine float64, inodeLine float64, wearLine float64) ([]*fileClusterProjection, []string) {
	blind := []string{}
	rawShare, inodeShare := fileClusterAdminShares(input.info)
	projections := []*fileClusterProjection{}

	// the filesystems' used shares a window back, against the same sizes
	rawThen, hasRawThen := 0.0, 0 < input.sizeByteCount && input.hasAvailThen
	if hasRawThen {
		rawThen = rawShare - (input.availThenByteCount-input.availByteCount)/input.sizeByteCount
	}
	if input.filesystemCount == 0 {
		blind = append(blind, "filesystem")
	}
	projections = append(projections, fileClusterProject(fileClusterResourceRaw, rawShare, rawThen, hasRawThen, settings.CohortTrendWindow, rawLine))

	inodeThen, hasInodeThen := 0.0, 0 < input.fileCount && input.hasFilesFreeThen
	if hasInodeThen {
		inodeThen = inodeShare - (input.filesFreeThen-input.filesFree)/input.fileCount
	}
	projections = append(projections, fileClusterProject(fileClusterResourceInodes, inodeShare, inodeThen, hasInodeThen, settings.CohortTrendWindow, inodeLine))

	if !input.hasDwpd {
		blind = append(blind, "dwpd")
		projections = append(projections, &fileClusterProjection{resource: fileClusterResourceWear, line: wearLine})
	} else {
		projections = append(projections, fileClusterProject(fileClusterResourceWear, input.dwpd, input.dwpdThen, input.hasDwpdThen, settings.CohortTrendWindow, wearLine))
	}

	// path-epoch copies' load against half the cluster's 21.6M operations a day: no process publishes it yet, so it
	// stays unprojected (see the file header)
	projections = append(projections, &fileClusterProjection{resource: fileClusterResourceCopy})
	return projections, blind
}

// The first cluster's control plane, apart, with its slope.
func fileClusterControlPlaneText(settings *SidecarStorageSettings, input *fileClusterCohortInput) string {
	if !input.hasControl {
		return "control_plane=unpublished"
	}
	if !input.hasControlThen {
		return fmt.Sprintf("control_plane_bytes=%.0f unprojected", input.controlBytes)
	}
	perDay := (input.controlBytes - input.controlBytesThen) / (settings.CohortTrendWindow.Hours() / 24)
	return fmt.Sprintf("control_plane_bytes=%.0f per_day=%.0f", input.controlBytes, perDay)
}

// days for a finding, to the nearest whole day, or inf.
func fileClusterDaysText(days float64) string {
	if math.IsInf(days, 1) {
		return "inf"
	}
	return strconv.FormatFloat(days, 'f', 0, 64)
}

func fileClusterCohortBlindSymptom(frame string) string {
	switch frame {
	case "filesystem":
		return "no filesystem of the cluster's drives reached Mimir, so its raw and inode slopes cannot be taken"
	case "dwpd":
		return "the busiest drive's 7-day writes are not published: the cluster's budget loop is not running"
	}
	return "an input of the cluster's projection did not read"
}

// A finding of class at target, sustained once.
func (self fileClusterCohortProbe) cohortFinding(class string, target string, frame string, symptom string, observed string) finding {
	finding := finding{
		probeId:  fileClusterCohortProbeId,
		tier:     tierWarn,
		class:    class,
		target:   target,
		frame:    frame,
		symptom:  symptom,
		observed: observed,
		sustain:  1,
		playbook: "SIGNALS.md §29.7; §19.9's cohorts row and §19.10's file cluster row (DESIGN.md)",
	}
	switch class {
	case fileClusterCohortClassLimit:
		finding.action = "The cohort takes only what the storage limit accepts until it falls under the line: move a cohort with the move tool once it exists (§19.9's cohorts row); meanwhile confirm deletions and erasures pass."
	case fileClusterCohortClassExhaustion:
		finding.action = "Date the move tool by the earliest projection (§19.9's cohorts row); the estate's free space does not take a placed cohort."
	case fileClusterCohortClassTrigger:
		finding.action = "Order the next metadata cluster (§19.9's triggers row, owner decision 145) and name it in place_new once it passes §22's suite."
	case fileClusterCohortClassHddTrigger:
		finding.action = "Order the segment placement and its HDD capacity before the projected crossing (§19.9's hdd row; g0-recheck-0-hdd-trigger)."
	case fileClusterCohortClassUnobservable:
		finding.unobservable = true
	}
	return finding
}
