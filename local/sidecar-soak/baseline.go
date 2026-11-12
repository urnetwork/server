package main

// What the soak compares (§22.8's soak row, D-TEST-7; TEST-M2-14): each signal at the run's end against its
// baseline, both read after the load stops and a quiet period lets caches expire, so a cache's fill never reads as a
// leak. A phase's reading is the median of its samples, each taken after a collection, so one burst of garbage or a
// goroutine passing through moves nothing.
//
//	heap_bytes      live heap after a collection: back while the end's is at most the baseline's times
//	                (1 + HeapGrowth) plus HeapSlackBytes, which leaves room for caches that are bounded and full
//	goroutines      back within GoroutineSlack of the baseline
//	redis_keys      back within RedisKeySlack: soft state that rebuilds from its owner stays the same size
//	orphan_objects  segments past their cluster's horizon that no pidx/ entry introduces, by orphan GC's own dry
//	                count (OrphanGc.CountRepo, QD-677) per repo over the nightly pass's listings, deleting nothing,
//	                back within OrphanSlack. Under the horizon a segment is GC's to judge only 49 h or more after its
//	                upload, so this compares what GC would judge orphan at the baseline and at the end; an end whose
//	                count reaches nothing the load wrote is no evidence (soakReport.orphanCoverage)
//
// A signal its target cannot read honestly reads open, with why: never back, and never a pass's evidence. A count
// that cannot judge every repo, as when GC would skip one now, reads open.

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/evsservice"
	"github.com/urnetwork/sidecar/sidecartest/load"
)

type soakSignal string

const (
	soakSignalHeap       soakSignal = "heap_bytes"
	soakSignalGoroutines soakSignal = "goroutines"
	soakSignalRedisKeys  soakSignal = "redis_keys"
	soakSignalOrphans    soakSignal = "orphan_objects"
)

// Every signal, in the order a report lists them.
var soakSignals = []soakSignal{soakSignalHeap, soakSignalGoroutines, soakSignalRedisKeys, soakSignalOrphans}

type soakOutcome string

const (
	// Within its tolerance of the baseline.
	soakBack soakOutcome = "back"
	// Past it: a leak, which fails the run.
	soakLeaked soakOutcome = "leaked"
	// Not read, or not readable honestly on this target: never a pass.
	soakOpen soakOutcome = "open"
)

// How far each signal may stand above its baseline and still read back.
type soakTolerances struct {
	// The heap may grow by this share of its baseline, plus HeapSlackBytes.
	HeapGrowth     float64
	HeapSlackBytes int64
	GoroutineSlack int64
	RedisKeySlack  int64
	OrphanSlack    int64
}

// A tenth of the heap and 16 MiB, eight goroutines, sixteen keys and no orphan: room for bounded caches and the
// runtime's own, and none for growth that tracks the load.
func defaultSoakTolerances() *soakTolerances {
	return &soakTolerances{
		HeapGrowth:     0.1,
		HeapSlackBytes: 16 * 1024 * 1024,
		GoroutineSlack: 8,
		RedisKeySlack:  16,
		OrphanSlack:    0,
	}
}

// What one orphan count read.
type orphanCount struct {
	// Segments past their cluster's horizon that no pidx/ entry introduces, over every repo of every metadata
	// cluster: what orphan GC would judge orphan if it ran now.
	Orphans int64
	// The latest LastModified a counted segment can have: the earliest horizon the count read, less GC's adoption
	// bound. Nothing written after it is past a horizon yet, so the count says nothing of it.
	JudgedThrough time.Time
}

// Counts orphan objects by orphan GC's own definition, deleting nothing.
type orphanCounter interface {
	CountOrphans(ctx context.Context) (*orphanCount, error)
}

// Orphan GC's dry count over a host's stores (QD-677): per metadata cluster its horizon by the nightly pass's own
// OrphanHorizon (step 1), then per repo/ key on the cluster the repo's listings as the pass takes them
// (OrphanGc.ListRepo), judged by OrphanGc.CountRepo, which issues no DELETE. A repo GC would not judge now, for any
// reason, fails the count: what GC cannot judge is never counted as no orphan. Built once for a target, it starts
// no goroutine and keeps nothing in Redis, so its readings add nothing to what they measure.
type evsOrphanCounter struct {
	host *sidecar.ServiceHost
	pass *evsservice.NightlyPass
	gc   *evsservice.OrphanGc
}

// A count on host, which needs Blobs and a write budget as the pass does; nothing is written through either.
func newEvsOrphanCounter(host *sidecar.ServiceHost) (*evsOrphanCounter, error) {
	pass, err := evsservice.NewNightlyPass(host, nil)
	if err != nil {
		return nil, err
	}
	gc, err := evsservice.NewOrphanGc(host, nil)
	if err != nil {
		return nil, err
	}
	return &evsOrphanCounter{host: host, pass: pass, gc: gc}, nil
}

// Every metadata cluster's repos, one cluster after another: see the type.
func (self *evsOrphanCounter) CountOrphans(ctx context.Context) (*orphanCount, error) {
	clusterIds := self.host.Blobs.MetadataClusterIds()
	if len(clusterIds) == 0 {
		return nil, errors.New("no metadata cluster to count on")
	}
	count := &orphanCount{}
	for _, clusterId := range clusterIds {
		cluster, err := self.host.Blobs.Cluster(clusterId)
		if err != nil {
			return nil, err
		}
		// before any listing of the cluster's repos, as the pass takes it
		horizon, err := self.pass.OrphanHorizon(ctx, cluster)
		if err != nil {
			return nil, fmt.Errorf("cluster %s's horizon: %w", clusterId, err)
		}
		judgedThrough := horizon.Add(-self.gc.Settings().AdoptionBound)
		if count.JudgedThrough.IsZero() || judgedThrough.Before(count.JudgedThrough) {
			count.JudgedThrough = judgedThrough
		}
		whySkippedCounts := map[string]int{}
		startAfter := ""
		for {
			objects, more, err := cluster.List(ctx, sidecar.EvsRepoObjectKeyPrefix, startAfter, self.gc.Settings().ListLimit)
			if err != nil {
				return nil, err
			}
			for _, object := range objects {
				startAfter = object.Key
				repoId, err := sidecar.ParseId(strings.TrimPrefix(object.Key, sidecar.EvsRepoObjectKeyPrefix))
				if err != nil || evsservice.RepoKey(repoId) != object.Key {
					// not a repo's key: the pass judges nothing of it either
					continue
				}
				repo, err := self.gc.ListRepo(ctx, cluster, repoId, horizon)
				if err != nil {
					return nil, err
				}
				result, err := self.gc.CountRepo(ctx, repo)
				if err != nil {
					return nil, err
				}
				if result.Skipped != "" {
					whySkippedCounts[result.Skipped] += 1
					continue
				}
				count.Orphans += int64(result.OrphanCount)
			}
			if !more || len(objects) == 0 {
				break
			}
		}
		if 0 < len(whySkippedCounts) {
			return nil, fmt.Errorf("orphan gc would not judge every repo of cluster %s, repos by why: %v", clusterId, whySkippedCounts)
		}
	}
	return count, nil
}

// One phase's reading: its samples and its orphan count, -1 when unread with why.
type soakReading struct {
	Samples    []load.Sample `json:"samples"`
	Orphans    int64         `json:"orphans"`
	OrphansWhy string        `json:"orphans_why,omitempty"`
	// The count's orphanCount.JudgedThrough: zero when unread.
	OrphansJudgedThrough time.Time `json:"orphans_judged_through,omitzero"`
}

// The median of a signal over the reading's samples, false when no sample holds it.
func (self *soakReading) median(signal soakSignal) (int64, bool) {
	values := []int64{}
	for _, sample := range self.Samples {
		switch signal {
		case soakSignalHeap:
			values = append(values, int64(sample.HeapAllocBytes))
		case soakSignalGoroutines:
			values = append(values, int64(sample.Goroutines))
		case soakSignalRedisKeys:
			if 0 <= sample.RedisKeys {
				values = append(values, sample.RedisKeys)
			}
		}
	}
	if signal == soakSignalOrphans {
		return self.Orphans, 0 <= self.Orphans
	}
	if len(values) == 0 {
		return 0, false
	}
	slices.Sort(values)
	return values[len(values)/2], true
}

// One signal's verdict.
type soakVerdict struct {
	Signal   soakSignal  `json:"signal"`
	Baseline int64       `json:"baseline"`
	End      int64       `json:"end"`
	Limit    int64       `json:"limit"`
	Outcome  soakOutcome `json:"outcome"`
	Why      string      `json:"why,omitempty"`
}

// Each signal of end against baseline within tolerances; a signal in open, or one either reading lacks, reads open.
func compareSoak(baseline *soakReading, end *soakReading, tolerances *soakTolerances, open map[soakSignal]string) []*soakVerdict {
	verdicts := []*soakVerdict{}
	for _, signal := range soakSignals {
		verdict := &soakVerdict{Signal: signal}
		verdicts = append(verdicts, verdict)
		if why, isOpen := open[signal]; isOpen {
			verdict.Outcome = soakOpen
			verdict.Why = why
			continue
		}
		baselineValue, baselineRead := baseline.median(signal)
		endValue, endRead := end.median(signal)
		if !baselineRead || !endRead {
			verdict.Outcome = soakOpen
			verdict.Why = "not read"
			if signal == soakSignalOrphans {
				verdict.Why = firstNonEmpty(end.OrphansWhy, baseline.OrphansWhy, "not read")
			}
			continue
		}
		verdict.Baseline = baselineValue
		verdict.End = endValue
		switch signal {
		case soakSignalHeap:
			verdict.Limit = baselineValue + int64(float64(baselineValue)*tolerances.HeapGrowth) + tolerances.HeapSlackBytes
		case soakSignalGoroutines:
			verdict.Limit = baselineValue + tolerances.GoroutineSlack
		case soakSignalRedisKeys:
			verdict.Limit = baselineValue + tolerances.RedisKeySlack
		case soakSignalOrphans:
			verdict.Limit = baselineValue + tolerances.OrphanSlack
		}
		if verdict.Limit < endValue {
			verdict.Outcome = soakLeaked
			verdict.Why = fmt.Sprintf("%d at the end, past %d", endValue, verdict.Limit)
			continue
		}
		verdict.Outcome = soakBack
	}
	return verdicts
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}
