// Runway to the floor of every stateful volume, from node filesystem history.
// The MinIO drives are judged by minio-runway (§11.6b) with the same
// estimator and its longer thresholds, so they are not reported twice here.
package monitor

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
)

const storageRunwayProbeId = "storage/runway"

// SIGNALS.md §2.4f (storage-runway): one max-of-windows estimator for every
// stateful volume the inventory roles name.
func NewStorageRunwaySignal() Signal {
	return NewStorageRunwaySignalWithSettings(DefaultStorageRunwaySettings())
}

// The signal with explicit thresholds and volume rules.
func NewStorageRunwaySignalWithSettings(settings *StorageRunwaySettings) Signal {
	return &signalAdapter{
		number: "2.4f", key: "storage-runway", name: "Storage runway for every stateful volume",
		probe: &storageRunwayProbe{settings: settings},
	}
}

// The thresholds and volumes of SIGNALS.md §2.4f.
type StorageRunwaySettings struct {
	Estimator StorageRunwayEstimatorSettings
	// Normal sampling cadence, and the whole-run budget including gateway
	// failover.
	Cadence   time.Duration
	RunBudget time.Duration
	// WARN at or under WarnRunway and PAGE at or under PageRunway, each after
	// Sustain consecutive observations.
	WarnRunway time.Duration
	PageRunway time.Duration
	Sustain    int
	// PAGE when the free bytes above the floor are under this multiple of the
	// largest pace-window decline in the pace history: a floor scaled to the
	// volume's own pace. Non-positive disables it.
	PaceFloorMultiple float64
	// The stateful volumes, by inventory role.
	Volumes []StorageRunwayVolumeSettings
}

// One rule naming a stateful volume.
type StorageRunwayVolumeSettings struct {
	// Hosts carrying this inventory role hold the volume; empty means every
	// enabled inventory host.
	Role string
	// The exact node_filesystem mountpoint label.
	Mountpoint string
	// Bytes held back: runway counts down to this floor. When several rules
	// name one volume, the largest floor wins.
	FloorBytes float64
}

// The §2.4f defaults: WARN at 72 hours, PAGE at 24 hours, two observations
// at a 15-minute cadence; the database root keeps the §2.4b 100 GB line, and
// every other root and the Redis data volumes count down to full.
func DefaultStorageRunwaySettings() *StorageRunwaySettings {
	return &StorageRunwaySettings{
		Estimator:         DefaultStorageRunwayEstimatorSettings(),
		Cadence:           15 * time.Minute,
		RunBudget:         40 * time.Second,
		WarnRunway:        72 * time.Hour,
		PageRunway:        24 * time.Hour,
		Sustain:           2,
		PaceFloorMultiple: 2,
		Volumes: []StorageRunwayVolumeSettings{
			{Role: "", Mountpoint: "/", FloorBytes: 0},
			{Role: "pg-primary", Mountpoint: "/", FloorBytes: 100 * 1000 * 1000 * 1000},
			{Role: "redis-cluster", Mountpoint: "/mnt/data", FloorBytes: 0},
		},
	}
}

// The risk memory belongs to this signal instance and survives between runs
// so an unknown runway keeps the severity of a decline measured earlier.
type storageRunwayProbe struct {
	settings *StorageRunwaySettings
	risks    storageRunwayRiskMemory
}

func (self *storageRunwayProbe) id() string               { return storageRunwayProbeId }
func (self *storageRunwayProbe) tier() string             { return tierPage }
func (self *storageRunwayProbe) cadence() time.Duration   { return self.settings.Cadence }
func (self *storageRunwayProbe) runBudget() time.Duration { return self.settings.RunBudget }

// One configured volume and its validated history.
type storageRunwayVolume struct {
	host       string
	mountpoint string
	floor      float64
	target     string
	// the PostgreSQL primary's root, whose action is the §5.14 playbook
	database bool
	seen     bool
	samples  []storageRunwaySample
	reason   string
}

// <host>/root for "/", otherwise the host followed by the mountpoint.
func storageRunwayTarget(host, mountpoint string) string {
	if mountpoint == "/" {
		return host + "/root"
	}
	return host + mountpoint
}

// Raw selector over every configured host and mountpoint; series keep their
// own labels and the grid keeps original freshness explicit.
func storageRunwayQuery(environment string, hosts, mountpoints []string) string {
	return "node_filesystem_avail_bytes{env=" + strconv.Quote(environment) + `,job="node",host=~` +
		strconv.Quote(storageRunwayAlternation(hosts)) + ",mountpoint=~" + strconv.Quote(storageRunwayAlternation(mountpoints)) + "}"
}

// Every configured volume yields a measured, healthy or explicitly unknown
// finding. Cancellation keeps the parent verdict and the original source
// cause, and never publishes a completed observation.
func (self *storageRunwayProbe) check(ctx context.Context, env *probeEnv) ([]finding, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	settings := self.settings
	scoped, isScoped := env.runner.(*hostScopeRunner)
	volumeByKeys := map[[2]string]*storageRunwayVolume{}
	volumes := []*storageRunwayVolume{}
	hostNames, mountpoints := []string{}, []string{}
	seenHosts, seenMountpoints := map[string]bool{}, map[string]bool{}
	for _, configured := range env.cfg.hosts {
		rules := []StorageRunwayVolumeSettings{}
		for _, rule := range settings.Volumes {
			if rule.Mountpoint != "" && (rule.Role == "" || configured.hasRole(rule.Role)) {
				rules = append(rules, rule)
			}
		}
		if len(rules) == 0 {
			continue
		}
		if isScoped {
			// An excluded host's volumes stay unknown through the shared
			// host-scope coverage finding; its series are never read.
			if err := scoped.guardHost(ctx, configured); err != nil {
				if contextErr := ctx.Err(); contextErr != nil {
					return nil, contextErr
				}
				continue
			}
		}
		for _, rule := range rules {
			key := [2]string{configured.name, rule.Mountpoint}
			volume := volumeByKeys[key]
			if volume == nil {
				volume = &storageRunwayVolume{host: configured.name, mountpoint: rule.Mountpoint, floor: rule.FloorBytes,
					target: storageRunwayTarget(configured.name, rule.Mountpoint)}
				volumeByKeys[key] = volume
				volumes = append(volumes, volume)
			}
			volume.floor = max(volume.floor, rule.FloorBytes)
			volume.database = volume.database || (rule.Role == "pg-primary" && rule.Mountpoint == "/")
			if !seenHosts[configured.name] {
				seenHosts[configured.name] = true
				hostNames = append(hostNames, configured.name)
			}
			if !seenMountpoints[rule.Mountpoint] {
				seenMountpoints[rule.Mountpoint] = true
				mountpoints = append(mountpoints, rule.Mountpoint)
			}
		}
	}
	sort.Slice(volumes, func(i, j int) bool { return volumes[i].target < volumes[j].target })
	now := env.now().UTC()
	if len(volumes) == 0 {
		return []finding{self.unknown("storage/volumes", "no-configured-volume", storageRunwayEstimate{}, 0, now, nil)}, nil
	}
	unknownAll := func(reason string, err error) []finding {
		findings := make([]finding, 0, len(volumes))
		for _, volume := range volumes {
			findings = append(findings, self.unknown(volume.target, reason, storageRunwayEstimate{}, volume.floor, now, err))
		}
		return findings
	}
	gateways := env.cfg.hostsWithRole("services")
	if len(gateways) == 0 {
		return unknownAll("service-gateway-unavailable", nil), nil
	}
	grid := settings.Estimator.grid(now)
	command := storageRunwayRangeCommand("monitor-storage-runway", storageRunwayQuery(env.cfg.env, hostNames, mountpoints), grid, settings.Estimator)
	output, gateway, err := shellFirstServiceGateway(ctx, env.runner, gateways, nil, command)
	if ctx.Err() != nil {
		if err != nil {
			return nil, &signalContextError{contextErr: ctx.Err(), sourceErr: err}
		}
		return nil, ctx.Err()
	}
	if errors.Is(err, context.Canceled) {
		return nil, &signalContextError{contextErr: context.Canceled, sourceErr: err}
	}
	if err != nil {
		return unknownAll("source-command-unavailable", err), nil
	}
	seriesList, reason := parseStorageRunwayRange(output, grid, settings.Estimator)
	if reason != "" {
		return unknownAll(reason, nil), nil
	}
	for _, series := range seriesList {
		volume := volumeByKeys[[2]string{series.labels["host"], series.labels["mountpoint"]}]
		if volume == nil || volume.reason != "" {
			// A series outside the configured set is ignored and never
			// certifies a configured volume.
			continue
		}
		if volume.seen {
			volume.reason, volume.samples = "ambiguous-filesystem-source", nil
			continue
		}
		volume.seen = true
		for name := range series.labels {
			switch name {
			case "__name__", "env", "job", "host", "device", "fstype", "mountpoint", "instance":
			default:
				volume.reason = "unsupported-source-label"
			}
		}
		switch {
		case volume.reason != "":
		case series.labels["__name__"] != "node_filesystem_avail_bytes" || series.labels["env"] != env.cfg.env ||
			series.labels["job"] != "node" || series.labels["device"] == "" || series.labels["fstype"] == "":
			volume.reason = "unowned-filesystem-series"
		case series.invalid != "":
			volume.reason = series.invalid
		case !storageRunwayWholeBytes(series.samples):
			volume.reason = "invalid-byte-value"
		default:
			volume.samples = series.samples
		}
	}
	findings := []finding{}
	for _, volume := range volumes {
		if volume.reason == "" && !volume.seen {
			volume.reason = "missing-filesystem-series"
		}
		estimate := storageRunwayEstimate{}
		if volume.reason == "" {
			estimate = estimateStorageRunway(settings.Estimator, volume.samples, grid.end, volume.floor)
			if estimate.recentKnown {
				self.risks.remember(volume.target, estimate.recentRate, now)
			}
			switch {
			case !estimate.fresh:
				volume.reason = "stale-filesystem-series"
			case !estimate.complete:
				volume.reason = "incomplete-decline-windows"
			}
		}
		measured, banded := self.measured(volume, estimate, gateway.name)
		if banded {
			findings = append(findings, measured)
		}
		if volume.reason != "" {
			findings = append(findings, self.unknown(volume.target, volume.reason, estimate, volume.floor, now, nil))
			continue
		}
		if !banded {
			findings = append(findings, healthyFinding(storageRunwayProbeId, tierPage, "storage-runway", volume.target))
		}
		findings = append(findings, healthyFinding(storageRunwayProbeId, tierWarn, "storage-runway-unobservable", volume.target))
	}
	return findings, nil
}

// A violation needs a fresh newest sample. With windows missing the measured
// rate is a lower bound, so a band it already reaches still holds; only a
// complete estimate can be healthy.
func (self *storageRunwayProbe) measured(volume *storageRunwayVolume, estimate storageRunwayEstimate, gateway string) (finding, bool) {
	settings := self.settings
	if !estimate.present || !estimate.fresh {
		return finding{}, false
	}
	runway, declining := estimate.runway()
	band, tier := "", tierWarn
	switch {
	case declining && runway <= settings.PageRunway:
		band, tier = "page", tierPage
	case settings.PaceFloorMultiple > 0 && estimate.paceDecline > 0 && estimate.free < settings.PaceFloorMultiple*estimate.paceDecline:
		band, tier = "pace-floor", tierPage
	case declining && runway <= settings.WarnRunway:
		band = "warn"
	}
	if band == "" {
		return finding{}, false
	}
	volumeName := volume.mountpoint + " on " + volume.host
	symptom := fmt.Sprintf("Volume %s reaches its floor in %.1f hours at its fastest measured decline", volumeName, runway.Hours())
	if band == "pace-floor" {
		symptom = fmt.Sprintf("Volume %s has less free space above its floor than %.0f times its largest %s decline of the last %s",
			volumeName, settings.PaceFloorMultiple, storageRunwayWindowName(settings.Estimator.PaceWindow), storageRunwayWindowName(settings.Estimator.PaceHistory))
	}
	action := "Find what is growing before freeing space: compare the host's largest directories and services' own retention with the decline window, and remove avoidable growth at its source. Storage additions and retention changes are owner decisions; do not delete service data by hand."
	if volume.database {
		action = "Follow SIGNALS.md §5.14 in order: measure in decimal GB, attribute the growth (two relation-size snapshots four minutes apart, pg_ls_tmpdir(), pg_ls_waldir(), pg_replication_slots), then free space only by the guarded steps listed there with the owner's go, respecting the payout frontier (§2.4g). Treat the growth source (§14.8) afterwards; do not start VACUUM FULL, pg_repack or a mass delete from this alert."
	}
	return finding{
		probeId: storageRunwayProbeId, tier: tier, class: "storage-runway", target: volume.target, sustain: settings.Sustain,
		symptom: symptom,
		mechanism: "Available bytes are falling. The rate is the fastest decline over the " + storageRunwayWindowNames(settings.Estimator.Windows) +
			" windows ending at the newest sample, so an acceleration shows at once, while a cleanup or partition drop can only shorten the measured decline in the windows it falls in. Runway is the free space above the volume's floor divided by that rate.",
		baseline: fmt.Sprintf("WARN at runway <= %.0f hours and PAGE at <= %.0f hours, each after %d observations; independently PAGE when the free space above the floor is under %.0f times the largest %s decline of the last %s.",
			settings.WarnRunway.Hours(), settings.PageRunway.Hours(), settings.Sustain, settings.PaceFloorMultiple,
			storageRunwayWindowName(settings.Estimator.PaceWindow), storageRunwayWindowName(settings.Estimator.PaceHistory)),
		observed: "band=" + band + " " + estimate.observed(volume.floor) + fmt.Sprintf(" complete=%t", estimate.complete),
		evidence: "One bounded loopback Mimir range query of node_filesystem_avail_bytes for the inventory host and exact mountpoint through gateway " + gateway +
			"; the series is unique, carries env/job/host/device/fstype ownership, and its values are whole bytes on the query grid.",
		context:  "False-positive qualifiers: a temporary spill that recovers within the hour, a vacuum that holds back space reuse, and a REINDEX CONCURRENTLY or repack, which consumes its new copy before freeing the old one. False-negative qualifiers: growth on an unscraped mountpoint, a planned large write that arrives faster than the cadence, and missing history, which leaves the pace floor taken over less than the full history (pace_history_hours).",
		action:   action,
		verify:   "Two consecutive complete observations above both bands; a single quieter sample or one cleanup is not a trend.",
		playbook: "SIGNALS.md §2.4f and §5.14",
	}, true
}

// 1h, 6h and 24h.
func storageRunwayWindowNames(windows []time.Duration) string {
	names := make([]string, 0, len(windows))
	for _, window := range windows {
		names = append(names, storageRunwayWindowName(window))
	}
	return strings.Join(names, ", ")
}

// Unknown is not healthy for a safety property: the tier is the one the
// recent decline cannot rule out (SIGNALS.md §7, 2026-10-22 rule 1).
func (self *storageRunwayProbe) unknown(target, reason string, estimate storageRunwayEstimate, floor float64, now time.Time, err error) finding {
	settings := self.settings
	tier, basis := self.risks.unknownTier(target, estimate, now, settings.Estimator)
	observed := "reason=" + reason + " " + basis
	if estimate.present {
		observed += " newest_sample_age_s=" + strconv.FormatFloat(now.Add(-settings.Estimator.QueryLag).Sub(estimate.latest.at).Seconds(), 'f', 0, 64) +
			" " + estimate.observed(floor)
	}
	if err != nil {
		observed += " " + observationFailureDetails(err, classifyObservationError(err))
	}
	return finding{
		probeId: storageRunwayProbeId, tier: tier, class: "storage-runway-unobservable", target: target, sustain: settings.Sustain,
		symptom:   "The runway of " + target + " is unknown",
		mechanism: "The inventory, the gateway, the bounded Mimir answer or the volume's own fresh, unique and complete filesystem history could not be established. A missing or stale node-exporter series is unknown, never free space.",
		baseline:  fmt.Sprintf("One fresh unique available-bytes series per configured volume with every decline window measured. An unknown runway pages when the volume declined more than %.0f GB/h over the last %s, measured now or remembered from an earlier run; otherwise it warns.", gb(settings.Estimator.UnknownPageDeclineBytesPerHour), storageRunwayWindowName(settings.Estimator.UnknownRiskWindow)),
		observed:  observed,
		evidence:  "Only fixed reasons, byte and rate figures and error classes are retained; raw responses, labels, devices and source errors stay private.",
		context:   "Unknown is not healthy for a safety property. A runway alert raised earlier stays open while this lasts.",
		action:    observationFailureAction(err, classifyObservationError(err), "Restore the host's node filesystem metrics (Fluent Bit node_exporter_metrics) and the bounded Mimir read. While a paging unknown lasts, read free space on the host directly (df -B1) and treat the volume as filling at its last measured rate."),
		verify:    "A fresh complete history restores the estimate; only a complete estimate outside both bands is healthy.",
		playbook:  "SIGNALS.md §2.4f and §7",
	}
}
