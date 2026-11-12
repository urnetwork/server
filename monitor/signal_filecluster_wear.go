package monitor

// Signal filecluster-wear implements SIGNALS.md §29.4 (EVSB-M3-01; §19.6's wear and counters rows, §19.8's
// business and warn rows, R-OPS-12, D-OPS-19, owner decisions 140 and 190). Every 5 min it reads the metadata
// drives' wear from Mimir:
//
//   - the busiest drive's urnetwork_filecluster_drive_dwpd{cluster,window}, which the budget loop publishes: its
//     last 24 h past 0.8 DWPD warns (filecluster-drive-dwpd-day), its 7-day mean over 0.5 warns
//     (filecluster-drive-dwpd-week) and over 0.75 pages in business hours (filecluster-drive-dwpd-business);
//   - each NVMe drive's hourly SMART log (xops's smart-textfile.sh): media errors (filecluster-drive-media-errors)
//     or available spare under 10% (filecluster-drive-spare-low) warn, and a drive whose percentage used, at its
//     30-day rate, spends its whole rated endurance in under five years of service pages in business hours
//     (filecluster-drive-rating-short);
//   - urnetwork_filecluster_drive_rating_days{cluster}, which this publishes: the days until the cluster's first
//     drive's percentage used reaches 100 at its 30-day rate, +Inf while none wears. A drive joins its cluster
//     where its filesystem's mountpoint (node_filesystem_size_bytes) is a drive path admin info lists for the
//     cluster, and a drive with no reading 30 days old is not yet rated. P0's forgiven excess over the same 30
//     days, in drive-writes, is shown against the ~420 five years at the refill leave under the rating (§19.6's
//     wear row);
//   - filecluster-wear-unobservable reports what it could not read: Mimir, admin info, a metadata cluster's
//     drive-write series, or its drives' SMART log.
//
// §19.8's business-hours pages are raised at page severity, and the pager holds each until the business calendar
// opens (QD-754; page_classes.go's business rows). Slack carries them at once.

import (
	"context"
	"fmt"
	"math"
	"net/url"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/urnetwork/server"
)

func NewFileClusterWearSignal() Signal {
	return &signalAdapter{
		number: "29.4", key: "filecluster-wear", name: "File-cluster drive writes and wear",
		probe: fileClusterWearProbe{},
	}
}

const fileClusterWearProbeId = "filecluster/wear"

// SIGNALS.md §29.4's classes.
const (
	fileClusterWearClassDwpdDay       = "filecluster-drive-dwpd-day"
	fileClusterWearClassDwpdWeek      = "filecluster-drive-dwpd-week"
	fileClusterWearClassDwpdBusiness  = "filecluster-drive-dwpd-business"
	fileClusterWearClassMediaErrors   = "filecluster-drive-media-errors"
	fileClusterWearClassSpareLow      = "filecluster-drive-spare-low"
	fileClusterWearClassRatingShort   = "filecluster-drive-rating-short"
	fileClusterWearClassUnobservable  = "filecluster-wear-unobservable"
	fileClusterDwpdWindowDay          = "24h"
	fileClusterDwpdWindowWeek         = "7d"
	fileClusterSmartJob               = "file_cluster"
	fileClusterWearCheckDwpd          = "dwpd"
	fileClusterWearCheckUsed          = "percentage_used"
	fileClusterWearCheckUsedThen      = "percentage_used_window_start"
	fileClusterWearCheckSpare         = "available_spare"
	fileClusterWearCheckMediaErrors   = "media_errors"
	fileClusterWearCheckFilesystem    = "filesystem"
	fileClusterWearCheckForgivenBytes = "forgiven_bytes"
)

// A partition's name, which names its disk: nvme0n1p2 or sdb1 (as file_cluster_counters.go).
var fileClusterWearPartitionPattern = regexp.MustCompile(`^(nvme[0-9]+n[0-9]+)p[0-9]+$|^((?:s|v|xv|h)d[a-z]+)[0-9]+$`)

type fileClusterWearProbe struct {
	// nil queries Mimir
	query sidecarStorageQuery
}

func (fileClusterWearProbe) id() string             { return fileClusterWearProbeId }
func (fileClusterWearProbe) tier() string           { return tierWarn }
func (fileClusterWearProbe) cadence() time.Duration { return 5 * time.Minute }

// Where admin info says a drive is mounted: its path, else its endpoint's path.
func fileClusterWearDrivePath(drivePath string, endpoint string) string {
	if drivePath != "" {
		return path.Clean(drivePath)
	}
	if parsed, err := url.Parse(endpoint); err == nil && parsed.Path != "" {
		return path.Clean(parsed.Path)
	}
	return ""
}

// A filesystem's device as the SMART log names it: without /dev/, and a partition taken back to its disk.
func fileClusterWearDevice(device string) string {
	device = strings.TrimPrefix(device, "/dev/")
	if match := fileClusterWearPartitionPattern.FindStringSubmatch(device); match != nil {
		return match[1] + match[2]
	}
	return device
}

// The one query: each partition's fresh samples. paths are every metadata drive path admin info lists.
func fileClusterWearQuery(environment string, settings *SidecarStorageSettings, paths []string) string {
	fresh := func(metric string, check string, matchers ...string) string {
		return sidecarStorageTag(sidecarStorageFresh(sidecarStorageSelector(environment, metric, matchers...), settings.MetricFreshness), check)
	}
	smart := `job="` + fileClusterSmartJob + `"`
	quoted := make([]string, 0, len(paths))
	for _, drivePath := range paths {
		quoted = append(quoted, regexp.QuoteMeta(drivePath))
	}
	window := sidecarStorageRange(settings.RatingWindow)
	expressions := []string{
		fresh("urnetwork_filecluster_drive_dwpd", fileClusterWearCheckDwpd),
		fresh("urnetwork_filecluster_smart_percentage_used", fileClusterWearCheckUsed, smart),
		sidecarStorageTag(sidecarStorageSelector(environment, "urnetwork_filecluster_smart_percentage_used", smart)+" offset "+strings.Trim(window, "[]"), fileClusterWearCheckUsedThen),
		fresh("urnetwork_filecluster_smart_available_spare", fileClusterWearCheckSpare, smart),
		fresh("urnetwork_filecluster_smart_media_errors", fileClusterWearCheckMediaErrors, smart),
		sidecarStorageTag("sum by (cluster) (increase("+sidecarStorageSelector(environment, "urnetwork_evs_budget_p0_forgiven_bytes_total")+window+"))", fileClusterWearCheckForgivenBytes),
	}
	if 0 < len(quoted) {
		expressions = append(expressions, fresh("node_filesystem_size_bytes", fileClusterWearCheckFilesystem, "mountpoint=~"+strconv.Quote(strings.Join(quoted, "|"))))
	}
	return sidecarStorageJoin(expressions...)
}

func (self fileClusterWearProbe) check(ctx context.Context, env *probeEnv) ([]finding, error) {
	settings := env.cfg.sidecarStorage
	if gate, ok := fileClusterSignalGate(settings, fileClusterWearProbeId, fileClusterWearClassUnobservable); ok {
		return []finding{gate}, nil
	}
	findings := []finding{}
	clusterIds := fileClusterIdsOfKind(settings.AdminSource, server.FileClusterKindMetadata)
	pathClusters := map[string]string{}
	paths := []string{}
	for _, clusterId := range clusterIds {
		info, err := settings.AdminSource.ReadAdminInfo(ctx, clusterId)
		if err != nil {
			findings = append(findings, self.wearFinding(settings, fileClusterWearClassUnobservable, clusterId, "admin-info",
				"the cluster's admin info did not read, so its drives cannot be told from others", err.Error()))
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
	samples, err := query(ctx, env, fileClusterWearQuery(env.cfg.env, settings, paths))
	if err != nil {
		return append(findings, self.wearFinding(settings, fileClusterWearClassUnobservable, "mimir", "mimir",
			"the drives' writes and SMART log did not read from Mimir", err.Error())), nil
	}
	return append(findings, self.evaluate(settings, clusterIds, pathClusters, samples)...), nil
}

// One drive's SMART readings.
type fileClusterWearDrive struct {
	host   string
	device string
	// its cluster, "" until its filesystem joins one
	clusterId   string
	used        float64
	hasUsed     bool
	usedThen    float64
	hasUsedThen bool
	spare       float64
	hasSpare    bool
	mediaErrors float64
	hasErrors   bool
}

// Its rate in percentage points a day over the window, ok false without a reading the window's age.
func (self *fileClusterWearDrive) dailyRate(window time.Duration) (float64, bool) {
	if !self.hasUsed || !self.hasUsedThen {
		return 0, false
	}
	return (self.used - self.usedThen) / (window.Hours() / 24), true
}

// Every finding from one query's samples, publishing each cluster's rating days.
func (self fileClusterWearProbe) evaluate(settings *SidecarStorageSettings, clusterIds []string, pathClusters map[string]string, samples []*sidecarStorageSample) []finding {
	clusterDwpds := map[string]map[string]float64{}
	clusterForgivenBytes := map[string]float64{}
	hostDeviceClusters := map[string]string{}
	drives := map[string]*fileClusterWearDrive{}
	drive := func(sample *sidecarStorageSample) *fileClusterWearDrive {
		key := sample.label("host") + "/" + sample.label("device")
		found, ok := drives[key]
		if !ok {
			found = &fileClusterWearDrive{host: sample.label("host"), device: sample.label("device")}
			drives[key] = found
		}
		return found
	}
	for _, sample := range samples {
		switch sample.check {
		case fileClusterWearCheckDwpd:
			clusterId := sample.label("cluster")
			if clusterDwpds[clusterId] == nil {
				clusterDwpds[clusterId] = map[string]float64{}
			}
			// two lease holders' series overlap at a change: take the higher
			window := sample.label("window")
			clusterDwpds[clusterId][window] = math.Max(clusterDwpds[clusterId][window], sample.value)
		case fileClusterWearCheckForgivenBytes:
			clusterForgivenBytes[sample.label("cluster")] = sample.value
		case fileClusterWearCheckFilesystem:
			if clusterId, ok := pathClusters[path.Clean(sample.label("mountpoint"))]; ok {
				hostDeviceClusters[sample.label("host")+"/"+fileClusterWearDevice(sample.label("device"))] = clusterId
			}
		case fileClusterWearCheckUsed:
			found := drive(sample)
			found.used, found.hasUsed = sample.value, true
		case fileClusterWearCheckUsedThen:
			found := drive(sample)
			found.usedThen, found.hasUsedThen = sample.value, true
		case fileClusterWearCheckSpare:
			found := drive(sample)
			found.spare, found.hasSpare = sample.value, true
		case fileClusterWearCheckMediaErrors:
			found := drive(sample)
			found.mediaErrors, found.hasErrors = sample.value, true
		}
	}

	findings := []finding{}
	forgiven := func(clusterId string) string {
		driveWrites := clusterForgivenBytes[clusterId] / settings.DriveByteCount
		return fmt.Sprintf("forgiven_drive_writes_%s=%.3f of ~%.0f", sidecarStorageRange(settings.RatingWindow), driveWrites, settings.ForgivenDriveWritesHeadroom)
	}
	for _, clusterId := range clusterIds {
		dwpds := clusterDwpds[clusterId]
		day, hasDay := dwpds[fileClusterDwpdWindowDay]
		week, hasWeek := dwpds[fileClusterDwpdWindowWeek]
		if !hasDay || !hasWeek {
			findings = append(findings, self.wearFinding(settings, fileClusterWearClassUnobservable, clusterId, "dwpd",
				"the busiest drive's writes are not published: the cluster's budget loop is not running", ""))
		} else {
			observed := fmt.Sprintf("dwpd_24h=%.3f dwpd_7d=%.3f %s", day, week, forgiven(clusterId))
			if settings.DriveDwpdDayWarn < day {
				findings = append(findings, self.wearFinding(settings, fileClusterWearClassDwpdDay, clusterId, "",
					"the busiest metadata drive's last 24 h is past 0.8 DWPD", observed))
			} else {
				findings = append(findings, healthyFinding(fileClusterWearProbeId, tierWarn, fileClusterWearClassDwpdDay, clusterId))
			}
			switch {
			case settings.DriveDwpdWeekBusiness < week:
				findings = append(findings,
					healthyFinding(fileClusterWearProbeId, tierWarn, fileClusterWearClassDwpdWeek, clusterId),
					self.wearFinding(settings, fileClusterWearClassDwpdBusiness, clusterId, "",
						"the busiest metadata drive's 7-day mean is over 0.75 DWPD: the budget holds 0.8 but for P0's excess", observed))
			case settings.DriveDwpdWeekWarn < week:
				findings = append(findings,
					self.wearFinding(settings, fileClusterWearClassDwpdWeek, clusterId, "",
						"the busiest metadata drive's 7-day mean is over 0.5 DWPD", observed),
					healthyFinding(fileClusterWearProbeId, tierPage, fileClusterWearClassDwpdBusiness, clusterId))
			default:
				findings = append(findings,
					healthyFinding(fileClusterWearProbeId, tierWarn, fileClusterWearClassDwpdWeek, clusterId),
					healthyFinding(fileClusterWearProbeId, tierPage, fileClusterWearClassDwpdBusiness, clusterId))
			}
		}
	}

	clusterSmartCounts := map[string]int{}
	clusterRatingDays := map[string]float64{}
	keys := []string{}
	for key, found := range drives {
		found.clusterId = hostDeviceClusters[key]
		keys = append(keys, key)
	}
	slices.Sort(keys)
	for _, key := range keys {
		found := drives[key]
		if found.clusterId == "" {
			// a drive no metadata cluster's path names: boot media, or another cluster's
			continue
		}
		clusterSmartCounts[found.clusterId] += 1
		target := found.host + "/" + found.device
		if found.hasErrors && 0 < found.mediaErrors {
			findings = append(findings, self.wearFinding(settings, fileClusterWearClassMediaErrors, target, "",
				"a metadata drive's SMART log counts media errors", fmt.Sprintf("cluster=%s media_errors=%.0f", found.clusterId, found.mediaErrors)))
		} else if found.hasErrors {
			findings = append(findings, healthyFinding(fileClusterWearProbeId, tierWarn, fileClusterWearClassMediaErrors, target))
		}
		if found.hasSpare && found.spare < settings.DriveSpareWarnPercent {
			findings = append(findings, self.wearFinding(settings, fileClusterWearClassSpareLow, target, "",
				"a metadata drive's available spare is under 10%", fmt.Sprintf("cluster=%s available_spare=%.0f", found.clusterId, found.spare)))
		} else if found.hasSpare {
			findings = append(findings, healthyFinding(fileClusterWearProbeId, tierWarn, fileClusterWearClassSpareLow, target))
		}
		rate, ok := found.dailyRate(settings.RatingWindow)
		if !ok {
			continue
		}
		ratingDays := math.Inf(1)
		if 0 < rate {
			ratingDays = math.Max(0, 100-found.used) / rate
		}
		if current, ok := clusterRatingDays[found.clusterId]; !ok || ratingDays < current {
			clusterRatingDays[found.clusterId] = ratingDays
		}
		// at this rate the drive's whole rating lasts 100/rate days: under its service life, it spends its
		// rating before five years of service
		if 0 < rate && 100/rate < settings.ServiceLife.Hours()/24 {
			findings = append(findings, self.wearFinding(settings, fileClusterWearClassRatingShort, target, "",
				"a metadata drive's percentage used, at its 30-day rate, spends its rated endurance before five years of service",
				fmt.Sprintf("cluster=%s percentage_used=%.0f rate_per_day=%.4f rating_days=%.0f %s", found.clusterId, found.used, rate, ratingDays, forgiven(found.clusterId))))
		} else {
			findings = append(findings, healthyFinding(fileClusterWearProbeId, tierPage, fileClusterWearClassRatingShort, target))
		}
	}
	for _, clusterId := range clusterIds {
		if clusterSmartCounts[clusterId] == 0 {
			findings = append(findings, self.wearFinding(settings, fileClusterWearClassUnobservable, clusterId, "smart",
				"no SMART log reached Mimir for the cluster's drives, so their spare, media errors and rating cannot be seen", ""))
		}
		if settings.Gauges != nil {
			ratingDays, known := clusterRatingDays[clusterId]
			settings.Gauges.PublishDriveRatingDays(clusterId, ratingDays, known)
		}
	}
	return findings
}

// A finding of class at target, sustained once: a warn, or a page for §19.8's business classes.
func (self fileClusterWearProbe) wearFinding(settings *SidecarStorageSettings, class string, target string, frame string, symptom string, observed string) finding {
	finding := finding{
		probeId:  fileClusterWearProbeId,
		tier:     tierWarn,
		class:    class,
		target:   target,
		frame:    frame,
		symptom:  symptom,
		observed: observed,
		sustain:  1,
		playbook: "SIGNALS.md §29.4; §19.10's file cluster row (DESIGN.md)",
	}
	switch class {
	case fileClusterWearClassDwpdBusiness, fileClusterWearClassRatingShort:
		// §19.8's business row: a page the pager holds for business hours
		finding.tier = tierPage
	}
	switch class {
	case fileClusterWearClassDwpdDay, fileClusterWearClassDwpdWeek, fileClusterWearClassDwpdBusiness:
		finding.action = "Confirm the counters, check for a heal or a runaway job, then order the next metadata cluster (§19.9), or on a cluster place_new no longer names act on §19.9's cohorts row (§19.10)."
	case fileClusterWearClassMediaErrors, fileClusterWearClassSpareLow:
		finding.action = "Plan the drive's replacement from the PM9A3 cold spares; a set stays homogeneous (owner decision 139)."
	case fileClusterWearClassRatingShort:
		finding.action = "Record the drive's wear against its rating (§19.6's wear row) and read P0's forgiven excess; order the next metadata cluster (§19.9) before the drives' rating runs out."
	case fileClusterWearClassUnobservable:
		finding.unobservable = true
	}
	return finding
}
