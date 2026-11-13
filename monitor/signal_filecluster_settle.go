package monitor

// Signal filecluster-settle implements SIGNALS.md §29.3 (EVSB-M3-01; §19.6's settle and clocks rows, §19.10's
// held-key and clock rows, D-OPS-20, D-OPS-9, FS-50). Every 30 s one Mimir query reads:
//
//   - each MinIO host's oldest held key, urnetwork_filecluster_held_key_oldest_seconds, which the fork exports
//     on its node metrics (EVSB-M2-18): a key held past 1 min by a write that cannot settle pages §19.8's rota
//     at any hour from M3, as filecluster-key-held on its cluster;
//   - every clock §19.6 bounds: each evs, gkm and taskworker process's urnetwork_clock_offset_seconds
//     (server/clock_offset.go, +Inf while unknown), each Redis host's (xops's chrony-offset-textfile.sh, -1
//     while unknown) and each MinIO host's urnetwork_filecluster_clock_offset_seconds beside
//     urnetwork_filecluster_clock_known: past 25 ms or unknown, filecluster-clock-offset warns;
//   - filecluster-settle-unobservable reports what it could not read: Mimir itself, a cluster with no MinIO
//     series, or no process clock at all.
//
// The MinIO series reach Mimir through xops's scrape of each cluster's node metrics on every storage host
// (playbook-file-clusters.yml), which labels them job="file_cluster_minio" and cluster="<cluster id>". No evs,
// gkm or taskworker host constructs its clock check yet (EVSB-M2-03): until one does this signal reports the
// process clocks blind, which holds its clock incidents open.

import (
	"context"
	"fmt"
	"math"
	"slices"
	"strings"
	"time"
)

func NewFileClusterSettleSignal() Signal {
	return &signalAdapter{
		number: "29.3", key: "filecluster-settle", name: "File-cluster held keys and clocks",
		probe: fileClusterSettleProbe{},
	}
}

const fileClusterSettleProbeId = "filecluster/settle"

// SIGNALS.md §29.3's classes.
const (
	fileClusterSettleClassKeyHeld      = "filecluster-key-held"
	fileClusterSettleClassClockOffset  = "filecluster-clock-offset"
	fileClusterSettleClassUnobservable = "filecluster-settle-unobservable"
)

// The job a scrape of a cluster's MinIO node metrics carries in Mimir.
const fileClusterMinioJob = "file_cluster_minio"

type fileClusterSettleProbe struct {
	// nil queries Mimir
	query sidecarStorageQuery
}

func (fileClusterSettleProbe) id() string             { return fileClusterSettleProbeId }
func (fileClusterSettleProbe) tier() string           { return tierPage }
func (fileClusterSettleProbe) cadence() time.Duration { return 30 * time.Second }

// The query's partitions.
const (
	fileClusterSettleCheckHeld        = "held_key_oldest"
	fileClusterSettleCheckMinioKnown  = "minio_clock_known"
	fileClusterSettleCheckMinioOffset = "minio_clock_offset"
	fileClusterSettleCheckOffset      = "clock_offset"
)

// The one query: each partition's fresh samples.
func fileClusterSettleQuery(environment string, freshness time.Duration) string {
	minio := func(metric string, check string) string {
		return sidecarStorageTag(sidecarStorageFresh(sidecarStorageSelector(environment, metric, "job="+fmt.Sprintf("%q", fileClusterMinioJob)), freshness), check)
	}
	return sidecarStorageJoin(
		minio("urnetwork_filecluster_held_key_oldest_seconds", fileClusterSettleCheckHeld),
		minio("urnetwork_filecluster_clock_known", fileClusterSettleCheckMinioKnown),
		minio("urnetwork_filecluster_clock_offset_seconds", fileClusterSettleCheckMinioOffset),
		sidecarStorageTag(sidecarStorageFresh(sidecarStorageSelector(environment, "urnetwork_clock_offset_seconds"), freshness), fileClusterSettleCheckOffset),
	)
}

func (self fileClusterSettleProbe) check(ctx context.Context, env *probeEnv) ([]finding, error) {
	settings := env.cfg.sidecarStorage
	if gate, ok := fileClusterSignalGate(settings, fileClusterSettleProbeId, fileClusterSettleClassUnobservable); ok {
		return []finding{gate}, nil
	}
	query := self.query
	if query == nil {
		query = sidecarStorageMimirQuery
	}
	samples, err := query(ctx, env, fileClusterSettleQuery(env.cfg.env, settings.MetricFreshness))
	if err != nil {
		return []finding{self.settleFinding(settings, fileClusterSettleClassUnobservable, "mimir", "mimir",
			"the held keys and clocks did not read from Mimir", err.Error())}, nil
	}
	return self.evaluate(settings, settings.AdminSource.ClusterIds(), samples), nil
}

// A clock's offset reads past the line or unknown: +Inf (a process with no bound), below zero (a Redis host
// with none), or not a number.
func fileClusterClockBad(offset float64, line time.Duration) bool {
	return math.IsNaN(offset) || math.IsInf(offset, 0) || offset < 0 || line.Seconds() < offset
}

// The worse of two readings of one clock: one past the line or unknown over a good one, else the larger.
func fileClusterWorseOffset(a float64, b float64, line time.Duration) float64 {
	aBad := fileClusterClockBad(a, line)
	bBad := fileClusterClockBad(b, line)
	switch {
	case aBad:
		return a
	case bBad:
		return b
	}
	return max(a, b)
}

// Every finding from one query's samples, for the clusters the vault file names.
func (self fileClusterSettleProbe) evaluate(settings *SidecarStorageSettings, clusterIds []string, samples []*sidecarStorageSample) []finding {
	findings := []finding{}
	clusterSeen := map[string]bool{}
	clusterHeld := map[string][]string{}
	// a MinIO host's clock: its known flag and offset
	type minioClock struct {
		known     float64
		hasKnown  bool
		offset    float64
		hasOffset bool
	}
	minioClocks := map[string]*minioClock{}
	// the worst offset of each process or Redis host
	clockOffsets := map[string]float64{}
	processClockCount := 0
	for _, sample := range samples {
		switch sample.check {
		case fileClusterSettleCheckHeld, fileClusterSettleCheckMinioKnown, fileClusterSettleCheckMinioOffset:
			clusterId := sample.label("cluster")
			host := sample.label("host")
			clusterSeen[clusterId] = true
			key := clusterId + " minio " + host
			clock, ok := minioClocks[key]
			if !ok {
				clock = &minioClock{}
				minioClocks[key] = clock
			}
			switch sample.check {
			case fileClusterSettleCheckHeld:
				if !sidecarStorageCount(sample.value) || settings.HeldKeyPage.Seconds() < sample.value {
					clusterHeld[clusterId] = append(clusterHeld[clusterId], fmt.Sprintf("%s held_key_oldest_s=%g", host, sample.value))
				}
			case fileClusterSettleCheckMinioKnown:
				clock.known, clock.hasKnown = sample.value, true
			case fileClusterSettleCheckMinioOffset:
				clock.offset, clock.hasOffset = sample.value, true
			}
		case fileClusterSettleCheckOffset:
			var target string
			switch {
			case sample.label("service") != "":
				processClockCount += 1
				target = sample.label("service") + " " + sample.label("block") + "@" + sample.label("host")
			case sample.label("role") == "redis":
				target = "redis@" + sample.label("host")
			default:
				continue
			}
			if previous, ok := clockOffsets[target]; ok {
				clockOffsets[target] = fileClusterWorseOffset(previous, sample.value, settings.ClockWarnOffset)
			} else {
				clockOffsets[target] = sample.value
			}
		}
	}

	for _, clusterId := range clusterIds {
		if !clusterSeen[clusterId] {
			findings = append(findings, self.settleFinding(settings, fileClusterSettleClassUnobservable, clusterId, "minio-series",
				"no fresh MinIO settle series reached Mimir for the cluster, so a held key or a host's clock cannot be seen",
				"job="+fileClusterMinioJob))
			continue
		}
		if held := clusterHeld[clusterId]; 0 < len(held) {
			slices.Sort(held)
			findings = append(findings, self.settleFinding(settings, fileClusterSettleClassKeyHeld, clusterId, "",
				"a file-cluster key is held past 1 min by a write that cannot settle: requests on it wait, then fail 503", strings.Join(held, "; ")))
		} else {
			findings = append(findings, healthyFinding(fileClusterSettleProbeId, tierPage, fileClusterSettleClassKeyHeld, clusterId))
		}
	}
	for target, clock := range minioClocks {
		// an offset the host does not call known is no bound
		offset := clock.offset
		if !clock.hasKnown || clock.known != 1 || !clock.hasOffset {
			offset = math.Inf(1)
		}
		clockOffsets[target] = offset
	}
	if processClockCount == 0 {
		findings = append(findings, self.settleFinding(settings, fileClusterSettleClassUnobservable, "process-clocks", "process-clocks",
			"no evs, gkm or taskworker process exports its clock offset, so a clock past §19.6's bound cannot be seen", ""))
	}
	targets := []string{}
	for target := range clockOffsets {
		targets = append(targets, target)
	}
	slices.Sort(targets)
	for _, target := range targets {
		offset := clockOffsets[target]
		if fileClusterClockBad(offset, settings.ClockWarnOffset) {
			observed := fmt.Sprintf("offset_s=%g", offset)
			if math.IsInf(offset, 1) || offset < 0 || math.IsNaN(offset) {
				observed = "offset unknown"
			}
			findings = append(findings, self.settleFinding(settings, fileClusterSettleClassClockOffset, target, "",
				"a clock §19.6 bounds is past 25 ms or unknown: past 50 ms, or unknown, its deadlines and fences stop", observed))
		} else {
			findings = append(findings, healthyFinding(fileClusterSettleProbeId, tierWarn, fileClusterSettleClassClockOffset, target))
		}
	}
	return findings
}

// A finding of class at target, sustained and tiered as the class is.
func (self fileClusterSettleProbe) settleFinding(settings *SidecarStorageSettings, class string, target string, frame string, symptom string, observed string) finding {
	finding := finding{
		probeId:  fileClusterSettleProbeId,
		tier:     tierWarn,
		class:    class,
		target:   target,
		frame:    frame,
		symptom:  symptom,
		observed: observed,
		playbook: "SIGNALS.md §29.3; §19.10's held-key and clock rows (DESIGN.md)",
	}
	switch class {
	case fileClusterSettleClassKeyHeld:
		finding.tier = tierPage
		finding.sustain = 1
		finding.action = "Bring the host back, or replace its drives (the cold spare chassis or spare drives), after which the settler reads what they hold and releases the key (§19.10)."
	case fileClusterSettleClassClockOffset:
		finding.sustain = settings.WarnSustain
		finding.action = "Fix chrony's sources on the host, or drain it (§19.10): past 50 ms or unknown, evs there answers pushes 503, taskworker raises no fence and a storage host begins no drive commit."
	case fileClusterSettleClassUnobservable:
		finding.sustain = 1
		finding.unobservable = true
	}
	return finding
}
