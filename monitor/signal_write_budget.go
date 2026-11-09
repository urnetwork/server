package monitor

// Signal write-budget implements SIGNALS.md §29.5 (EVSB-M3-01; §11.13's write budget and hdd rows, §19.8's page
// and warn rows, R-OPS-12, D-EVS-33, owner decisions 186 and 190, FS-50). Each minute one Mimir query reads the
// series the budget loop, the hdd loop and the processes publish, per cluster and with no network or repo label:
//
//   - P0 past the refill: urnetwork_evs_budget_p0_refill_ratio, P0's measured bytes over the last hour against
//     that hour's refill, above 1 pages §19.8's rota at any hour from M3 (write-budget-p0-past-refill). P0 is
//     never paced or refused (owner decision 186): the page asks for capacity, placement and the networks;
//   - P0 past its warn line for an hour (write-budget-p0-warn-line): D-TEST-11 sets the line at four times the
//     hourly share its nvme run measures, at most half the refill, and until that run the line is half;
//   - the bucket B empty for 5 min, P0's excess forgiven throughout (write-budget-bucket-empty), or under 25% of
//     its depth (write-budget-bucket-low);
//   - shedding, P1 refused as the heaviest network's (write-budget-shedding), from
//     urnetwork_evs_budget_shed_total;
//   - HDD admission refusals past 0.1% of a day's uploads (write-budget-hdd-refusals);
//   - write-budget-unobservable: Mimir did not read, or a metadata cluster's loop publishes no bucket or ratio.

import (
	"context"
	"fmt"
	"math"
	"strconv"
	"time"

	"github.com/urnetwork/server"
)

func NewWriteBudgetSignal() Signal {
	return &signalAdapter{
		number: "29.5", key: "write-budget", name: "Write budget, P0 and HDD admission",
		probe: writeBudgetProbe{},
	}
}

const writeBudgetProbeId = "filecluster/write-budget"

// SIGNALS.md §29.5's classes.
const (
	writeBudgetClassP0PastRefill = "write-budget-p0-past-refill"
	writeBudgetClassP0WarnLine   = "write-budget-p0-warn-line"
	writeBudgetClassBucketEmpty  = "write-budget-bucket-empty"
	writeBudgetClassBucketLow    = "write-budget-bucket-low"
	writeBudgetClassShedding     = "write-budget-shedding"
	writeBudgetClassHddRefusals  = "write-budget-hdd-refusals"
	writeBudgetClassUnobservable = "write-budget-unobservable"
)

// The query's partitions.
const (
	writeBudgetCheckRatio      = "p0_refill_ratio"
	writeBudgetCheckRatioMin   = "p0_refill_ratio_min"
	writeBudgetCheckBucket     = "bucket"
	writeBudgetCheckBucketMax  = "bucket_max"
	writeBudgetCheckShed       = "shed"
	writeBudgetCheckHddRefused = "hdd_refused"
	writeBudgetCheckUploads    = "uploads"
)

// evs's segment upload route, as urnetwork_evs_requests_total names it (evsservice/upload_segment.go).
const writeBudgetUploadRoute = "PUT /evs/v1/repos/{repo_id}/segments/{segment_id}"

type writeBudgetProbe struct {
	// nil queries Mimir
	query sidecarStorageQuery
}

func (writeBudgetProbe) id() string             { return writeBudgetProbeId }
func (writeBudgetProbe) tier() string           { return tierPage }
func (writeBudgetProbe) cadence() time.Duration { return time.Minute }

// The one query: each partition per cluster.
func writeBudgetQuery(environment string, settings *SidecarStorageSettings) string {
	selector := func(metric string, matchers ...string) string {
		return sidecarStorageSelector(environment, metric, matchers...)
	}
	return sidecarStorageJoin(
		sidecarStorageTag("max by (cluster) "+sidecarStorageFresh(selector("urnetwork_evs_budget_p0_refill_ratio"), settings.MetricFreshness), writeBudgetCheckRatio),
		sidecarStorageTag("min by (cluster) (min_over_time("+selector("urnetwork_evs_budget_p0_refill_ratio")+sidecarStorageRange(settings.P0WarnLineWindow)+"))", writeBudgetCheckRatioMin),
		sidecarStorageTag("max by (cluster) "+sidecarStorageFresh(selector("urnetwork_evs_budget_bucket_bytes"), settings.MetricFreshness), writeBudgetCheckBucket),
		sidecarStorageTag("max by (cluster) (max_over_time("+selector("urnetwork_evs_budget_bucket_bytes")+sidecarStorageRange(settings.BucketEmptyWindow)+"))", writeBudgetCheckBucketMax),
		sidecarStorageTag("sum by (cluster) (increase("+selector("urnetwork_evs_budget_shed_total")+sidecarStorageRange(settings.ShedWindow)+"))", writeBudgetCheckShed),
		sidecarStorageTag("sum by (cluster) (increase("+selector("urnetwork_evs_hdd_admission_refused_total")+sidecarStorageRange(settings.HddRefusalWindow)+"))", writeBudgetCheckHddRefused),
		sidecarStorageTag("sum (increase("+selector("urnetwork_evs_requests_total", "route="+strconv.Quote(writeBudgetUploadRoute))+sidecarStorageRange(settings.HddRefusalWindow)+"))", writeBudgetCheckUploads),
	)
}

func (self writeBudgetProbe) check(ctx context.Context, env *probeEnv) ([]finding, error) {
	settings := env.cfg.sidecarStorage
	if gate, ok := fileClusterSignalGate(settings, writeBudgetProbeId, writeBudgetClassUnobservable); ok {
		return []finding{gate}, nil
	}
	query := self.query
	if query == nil {
		query = sidecarStorageMimirQuery
	}
	samples, err := query(ctx, env, writeBudgetQuery(env.cfg.env, settings))
	if err != nil {
		return []finding{self.budgetFinding(settings, writeBudgetClassUnobservable, "mimir", "mimir",
			"the write budget's series did not read from Mimir", err.Error())}, nil
	}
	metadataClusterIds := fileClusterIdsOfKind(settings.AdminSource, server.FileClusterKindMetadata)
	segmentClusterIds := fileClusterIdsOfKind(settings.AdminSource, server.FileClusterKindSegment)
	return self.evaluate(settings, metadataClusterIds, segmentClusterIds, samples), nil
}

// Every finding from one query's samples.
func (self writeBudgetProbe) evaluate(settings *SidecarStorageSettings, metadataClusterIds []string, segmentClusterIds []string, samples []*sidecarStorageSample) []finding {
	checkClusterValues := map[string]map[string]float64{}
	uploads := 0.0
	for _, sample := range samples {
		if sample.check == writeBudgetCheckUploads {
			uploads = sample.value
			continue
		}
		if checkClusterValues[sample.check] == nil {
			checkClusterValues[sample.check] = map[string]float64{}
		}
		checkClusterValues[sample.check][sample.label("cluster")] = sample.value
	}
	value := func(check string, clusterId string) (float64, bool) {
		value, ok := checkClusterValues[check][clusterId]
		return value, ok && !math.IsNaN(value)
	}

	findings := []finding{}
	for _, clusterId := range metadataClusterIds {
		ratio, hasRatio := value(writeBudgetCheckRatio, clusterId)
		bucket, hasBucket := value(writeBudgetCheckBucket, clusterId)
		if !hasRatio || !hasBucket {
			findings = append(findings, self.budgetFinding(settings, writeBudgetClassUnobservable, clusterId, "budget-loop",
				"the cluster's budget loop publishes no bucket or P0 refill ratio, so P0 past the refill cannot be seen", ""))
			continue
		}
		ratioMin, _ := value(writeBudgetCheckRatioMin, clusterId)
		bucketMax, hasBucketMax := value(writeBudgetCheckBucketMax, clusterId)
		shed, _ := value(writeBudgetCheckShed, clusterId)
		observed := fmt.Sprintf("p0_refill_ratio=%.3f p0_refill_ratio_min_%s=%.3f bucket_bytes=%.0f bucket_share=%.3f shed_%s=%.0f",
			ratio, sidecarStorageRange(settings.P0WarnLineWindow), ratioMin, bucket, bucket/settings.BucketDepthByteCount, sidecarStorageRange(settings.ShedWindow), shed)

		if 1 < ratio {
			findings = append(findings, self.budgetFinding(settings, writeBudgetClassP0PastRefill, clusterId, "",
				"P0's bytes over the last hour are above that hour's refill on the cluster: its drives wear past the budget while it lasts", observed))
		} else {
			findings = append(findings, healthyFinding(writeBudgetProbeId, tierPage, writeBudgetClassP0PastRefill, clusterId))
		}
		if settings.P0WarnLineShare < ratioMin {
			findings = append(findings, self.budgetFinding(settings, writeBudgetClassP0WarnLine, clusterId, "",
				"P0 has stood past its warn line for an hour", observed))
		} else {
			findings = append(findings, healthyFinding(writeBudgetProbeId, tierWarn, writeBudgetClassP0WarnLine, clusterId))
		}
		if hasBucketMax && bucketMax <= 0 {
			findings = append(findings, self.budgetFinding(settings, writeBudgetClassBucketEmpty, clusterId, "",
				"the write budget's bucket has been empty for 5 minutes, P0's excess forgiven throughout", observed))
		} else {
			findings = append(findings, healthyFinding(writeBudgetProbeId, tierWarn, writeBudgetClassBucketEmpty, clusterId))
		}
		if bucket < settings.BucketLowShare*settings.BucketDepthByteCount {
			findings = append(findings, self.budgetFinding(settings, writeBudgetClassBucketLow, clusterId, "",
				"the write budget's bucket is under 25% of its depth", observed))
		} else {
			findings = append(findings, healthyFinding(writeBudgetProbeId, tierWarn, writeBudgetClassBucketLow, clusterId))
		}
		if 0 < shed {
			findings = append(findings, self.budgetFinding(settings, writeBudgetClassShedding, clusterId, "",
				"the write budget is shedding: P1 work of the heaviest network is refused 429 RESOURCE_EXHAUSTED", observed))
		} else {
			findings = append(findings, healthyFinding(writeBudgetProbeId, tierWarn, writeBudgetClassShedding, clusterId))
		}
	}
	for _, clusterId := range segmentClusterIds {
		refused, _ := value(writeBudgetCheckHddRefused, clusterId)
		share := 0.0
		switch {
		case 0 < uploads:
			share = refused / uploads
		case 0 < refused:
			share = math.Inf(1)
		}
		if settings.HddRefusalShare < share {
			findings = append(findings, self.budgetFinding(settings, writeBudgetClassHddRefusals, clusterId, "",
				"HDD admission refused more than 0.1% of the day's uploads",
				fmt.Sprintf("refused_%s=%.0f uploads_%s=%.0f share=%.5f", sidecarStorageRange(settings.HddRefusalWindow), refused, sidecarStorageRange(settings.HddRefusalWindow), uploads, share)))
		} else {
			findings = append(findings, healthyFinding(writeBudgetProbeId, tierWarn, writeBudgetClassHddRefusals, clusterId))
		}
	}
	return findings
}

// A finding of class at target, sustained and tiered as the class is.
func (self writeBudgetProbe) budgetFinding(settings *SidecarStorageSettings, class string, target string, frame string, symptom string, observed string) finding {
	finding := finding{
		probeId:  writeBudgetProbeId,
		tier:     tierWarn,
		class:    class,
		target:   target,
		frame:    frame,
		symptom:  symptom,
		observed: observed,
		sustain:  settings.WarnSustain,
		playbook: "SIGNALS.md §29.5; §19.10's P0 and file cluster rows (DESIGN.md)",
	}
	switch class {
	case writeBudgetClassP0PastRefill:
		finding.tier = tierPage
		finding.sustain = settings.PageSustain
		finding.action = "Read §19.8's P0 bytes by kind; never pace or refuse P0. Order the next metadata cluster (§19.9) and point place_new away from this one; contact the driving networks, found in the request logs, referring one that does it deliberately to §21.8's review; record the drives' wear against their rating (§19.10)."
	case writeBudgetClassP0WarnLine, writeBudgetClassBucketEmpty, writeBudgetClassBucketLow:
		finding.action = "Read P0's bytes by kind and the drives' writes: a budget shedding for an hour or a drive over 0.75 DWPD asks for the counters confirmed, a heal or runaway job ruled out, then the next metadata cluster (§19.10)."
	case writeBudgetClassShedding:
		finding.action = "A budget shedding for an hour: confirm the counters, check for a heal or a runaway job, then order the next metadata cluster (§19.10)."
	case writeBudgetClassHddRefusals:
		finding.action = "The segment cluster's HDD operations are past half their measured capacity: §19.9's hdd trigger asks for the segment placement and added HDD capacity (Q-OPS-2)."
	case writeBudgetClassUnobservable:
		finding.sustain = 1
		finding.unobservable = true
	}
	return finding
}
