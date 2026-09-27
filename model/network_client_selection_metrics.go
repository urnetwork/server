// Observes selection intent and the exact empty-result boundary without target
// identities, extra storage reads, or changing provider eligibility.
package model

import (
	"context"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/urnetwork/server"
)

var findProviders2SelectionOutcomes = prometheus.NewCounterVec(prometheus.CounterOpts{
	Name: "urnetwork_findproviders2_selection_outcomes_total",
	Help: "Request-local provider-selection outcomes by fixed intent and terminal explanation; no target or caller identifiers",
}, []string{"target_kind", "request_class", "ip_family", "rank_mode", "outcome", "reason"})

var findProviders2StageSeconds = prometheus.NewHistogramVec(prometheus.HistogramOpts{
	Name: "urnetwork_findproviders2_stage_seconds", Help: "Provider-selection stage residence, including failed and canceled work",
	Buckets: prometheus.DefBuckets,
}, []string{"stage"})

var findProviders2StageInflight = prometheus.NewGaugeVec(prometheus.GaugeOpts{
	Name: "urnetwork_findproviders2_stage_inflight", Help: "Provider-selection requests currently inside each fixed stage",
}, []string{"stage"})

var findProviders2SelectionSchema = prometheus.NewGauge(prometheus.GaugeOpts{
	Name: "urnetwork_findproviders2_selection_schema_version", Help: "Fixed selection-diagnostic schema supported by this process",
})

// Registers the schema even on a process with no requests; quiet and old
// producers remain distinguishable without a sentinel customer request.
func init() {
	findProviders2SelectionSchema.Set(1)
	prometheus.MustRegister(findProviders2SelectionOutcomes, findProviders2StageSeconds, findProviders2StageInflight, findProviders2SelectionSchema)
}

// Counts local observations of the pages actually requested, not unique fleet
// providers. Alternate-mode pages can overlap; only zero/nonzero is causal.
type findProviders2LoadObservation struct {
	missingTargets int
	missingPages   int
}

// Owned by one request. No identity map or observation state outlives it.
type findProviders2SelectionObservation struct {
	args                *FindProviders2Args
	targetKind          string
	requestClass        string
	rankMode            string
	stage               string
	stageStarted        time.Time
	outcome             string
	reason              string
	discovery           bool
	directRequested     bool
	backfillUnavailable bool
	loaded              int
	eligible            int
	dropped             [4]int
	load                findProviders2LoadObservation
}

// Captures only bounded request intent; resolving target types later reuses
// metadata the selector already loaded rather than starting a new query.
func newFindProviders2SelectionObservation(args *FindProviders2Args) *findProviders2SelectionObservation {
	rankMode := args.RankMode
	if rankMode == "" {
		rankMode = RankModeQuality
	} else if rankMode != RankModeQuality && rankMode != RankModeSpeed {
		rankMode = "unknown"
	}
	requestClass := "default_minimum"
	if args.ForceMinimum {
		requestClass = "forced_minimum"
	} else if args.ForceCount {
		switch {
		case args.Count <= 0:
			requestClass = "count_zero"
		case args.Count <= 2:
			requestClass = "count_small"
		default:
			requestClass = "count_positive"
		}
	}
	observation := &findProviders2SelectionObservation{
		args: args, targetKind: findProviders2TargetKind(args, nil, nil), requestClass: requestClass, rankMode: rankMode,
	}
	for _, spec := range args.Specs {
		if spec != nil && spec.ClientId != nil {
			observation.directRequested = true
		}
	}
	observation.enter("validate")
	return observation
}

// Uses fixed target types only. Missing/legacy directory entries stay unknown;
// a caller's country is never substituted for the requested target's kind.
func findProviders2TargetKind(args *FindProviders2Args, countries map[string]server.Id, directory map[server.Id]*locationDirectoryEntry) string {
	kind := "none"
	add := func(next string) {
		if kind == "none" {
			kind = next
		} else if kind != next {
			kind = "mixed"
		}
	}
	for _, spec := range args.Specs {
		if spec == nil {
			continue
		}
		if spec.LocationId != nil {
			next := "location_unknown"
			if entry := directory[*spec.LocationId]; entry != nil {
				switch entry.LocationType {
				case LocationTypeCountry, LocationTypeRegion, LocationTypeCity:
					next = entry.LocationType
				}
			}
			if next == "location_unknown" {
				for _, countryId := range countries {
					if countryId == *spec.LocationId {
						next = "country"
						break
					}
				}
			}
			add(next)
		}
		if spec.LocationGroupId != nil {
			add("group")
		}
		if spec.BestAvailable {
			add("best_available")
		}
		if spec.ClientId != nil {
			add("direct")
		}
	}
	return kind
}

// Ends the previous stage before starting the next; each request owns exactly
// one inflight increment, which finish also releases on error or panic.
func (self *findProviders2SelectionObservation) enter(stage string) {
	if self.stage != "" {
		findProviders2StageInflight.WithLabelValues(self.stage).Dec()
		findProviders2StageSeconds.WithLabelValues(self.stage).Observe(time.Since(self.stageStarted).Seconds())
	}
	self.stage, self.stageStarted = stage, time.Now()
	findProviders2StageInflight.WithLabelValues(stage).Inc()
}

// Explains the earliest proven zero boundary. Missing cache evidence is not
// proof of missing providers, and a filter-empty sample is not fleet scarcity.
func (self *findProviders2SelectionObservation) complete(resultCount int) {
	self.outcome, self.reason = "zero", "unclassified_zero"
	switch {
	case 0 < resultCount:
		self.outcome, self.reason = "nonempty", "returned"
	case self.discovery && self.args.ForceCount && self.args.Count <= 0:
		self.reason = "intentional_zero"
	case !self.discovery && self.directRequested:
		self.reason = "direct_excluded"
	case self.targetKind == "none":
		self.reason = "no_specs"
	case !self.discovery:
		self.reason = "unresolved_target"
	case self.backfillUnavailable:
		self.reason = "backfill_unavailable"
	case 0 < self.load.missingPages:
		self.reason = "cache_page_gap"
	case 0 < self.load.missingTargets:
		self.reason = "cache_missing"
	case self.loaded == 0:
		self.reason = "cache_empty"
	case 0 < self.eligible:
		if self.rankMode == "unknown" {
			self.reason = "unsupported_rank"
		} else {
			self.reason = "eligible_not_selected"
		}
	default:
		reasons := [4]string{"filtered_hard", "filtered_network", "filtered_family", "filtered_explicit"}
		for i, count := range self.dropped {
			if count == 0 {
				continue
			}
			if self.reason == "unclassified_zero" {
				self.reason = reasons[i]
			} else {
				self.reason = "filtered_mixed"
			}
		}
	}
}

// Runs for every exit, including canceled/failed requests omitted by the
// legacy completed-list metric. Arbitrary errors never become label values.
func (self *findProviders2SelectionObservation) finish(ctx context.Context) {
	findProviders2StageInflight.WithLabelValues(self.stage).Dec()
	findProviders2StageSeconds.WithLabelValues(self.stage).Observe(time.Since(self.stageStarted).Seconds())
	if self.outcome == "" {
		self.outcome, self.reason = "error", self.stage
		if ctx.Err() != nil {
			self.outcome = "canceled"
		}
	}
	findProviders2SelectionOutcomes.WithLabelValues(self.targetKind, self.requestClass, findProviders2OutcomeIpFamily(self.args.IpFamily), self.rankMode, self.outcome, self.reason).Inc()
}
