// Observes selection intent and the exact empty-result boundary without target
// identities, extra storage reads, or changing provider eligibility.
package model

import (
	"context"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/internal/privateprovidercapture"
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

var findProviders2NativeSourceOutcomes = prometheus.NewCounterVec(prometheus.CounterOpts{
	Name: "urnetwork_findproviders2_native_source_outcomes_total",
	Help: "Visited native bucket outcomes after request filters; unavailable is degraded priority, never proven exhaustion, even when online fallback fills the response",
}, []string{"rank_mode", "source", "outcome"})

var findProviders2NativeSourceSchema = prometheus.NewGauge(prometheus.GaugeOpts{
	Name: "urnetwork_findproviders2_native_source_schema_version",
	Help: "Fixed native-source diagnostic schema supported even before the first provider request",
})

// Registers the schema even on a process with no requests; quiet and old
// producers remain distinguishable without a sentinel customer request.
func init() {
	findProviders2SelectionSchema.Set(2)
	findProviders2NativeSourceSchema.Set(1)
	prometheus.MustRegister(findProviders2SelectionOutcomes, findProviders2StageSeconds, findProviders2StageInflight, findProviders2SelectionSchema, findProviders2NativeSourceOutcomes, findProviders2NativeSourceSchema)
}

// Counts local observations of the pages actually requested, not unique fleet
// providers. Alternate-mode pages can overlap; only zero/nonzero is causal.
type findProviders2LoadObservation struct {
	missingTargets int
	missingPages   int
	privateCapture *privateprovidercapture.Recorder
	privateSource  string
	missingGroup   *findProviders2MissingGroup
}

// Owned by one request. No identity map or observation state outlives it.
type findProviders2SelectionObservation struct {
	args                 *FindProviders2Args
	targetKind           string
	requestClass         string
	rankMode             string
	stage                string
	stageStarted         time.Time
	outcome              string
	reason               string
	discovery            bool
	directRequested      bool
	backfillUnavailable  bool
	loaded               int
	eligible             int
	dropped              [4]int
	explicitSources      uint8
	requestedCount       int
	discoveryReturned    int
	explicitReturned     int
	primaryClientScores  map[server.Id]*ClientScore
	backfillClientScores map[server.Id]*ClientScore
	load                 findProviders2LoadObservation
	privateStarted       time.Time
	privateGroupCount    int
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

// One fixed outcome for each native tier the request actually visits. A
// successful online answer must not hide a native publication/read failure.
func (self *findProviders2SelectionObservation) nativeSource(mode RankMode, backfill bool, outcome string) {
	if mode != RankModeQuality && mode != RankModeSpeed {
		mode = "unknown"
	}
	source := "primary"
	if backfill {
		source = "alternate"
	}
	findProviders2NativeSourceOutcomes.WithLabelValues(mode, source, outcome).Inc()
}

// Explains the earliest proven zero boundary. Missing cache evidence is not
// proof of missing providers, and a filter-empty sample is not fleet scarcity.
func (self *findProviders2SelectionObservation) complete(resultCount int) {
	self.outcome, self.reason = "zero", "unclassified_zero"
	switch {
	case 0 < resultCount:
		self.outcome = "nonempty"
		switch {
		case 10 <= resultCount:
			self.reason = "returned_10_plus"
		case 3 <= resultCount:
			self.reason = "returned_3_9"
		case !self.discovery:
			self.reason = "returned_small_direct"
		case 0 < self.explicitReturned:
			self.reason = "returned_small_mixed_direct"
		case self.requestedCount <= self.discoveryReturned:
			self.reason = "returned_small_requested"
		case self.backfillUnavailable || 0 < self.load.missingPages || 0 < self.load.missingTargets:
			self.reason = "returned_small_cache_unknown"
		default:
			// Both independently sampled modes can contain the same provider.
			// Count their filtered union without another map or storage read.
			eligibleCount := len(self.primaryClientScores)
			for clientId := range self.backfillClientScores {
				if _, ok := self.primaryClientScores[clientId]; !ok {
					eligibleCount++
				}
			}
			if self.discoveryReturned < eligibleCount {
				self.reason = "returned_small_eligible"
			} else if reason := self.filterReason(); reason != "unclassified_zero" {
				self.reason = "returned_small_" + reason
			} else {
				self.reason = "returned_small_sample"
			}
		}
	case self.discovery && self.args.ForceCount && self.args.Count <= 0:
		self.reason = "intentional_zero"
	case !self.discovery && self.directRequested:
		self.reason = "direct_excluded"
	case self.targetKind == "none":
		self.reason = "no_specs"
	case !self.discovery:
		self.reason = "unresolved_target"
	case 0 < self.load.missingPages:
		self.reason = "cache_page_gap"
	case 0 < self.load.missingTargets:
		self.reason = "cache_missing"
	case self.backfillUnavailable:
		self.reason = "backfill_unavailable"
	case self.loaded == 0:
		self.reason = "cache_empty"
	case 0 < self.eligible:
		if self.rankMode == "unknown" {
			self.reason = "unsupported_rank"
		} else {
			self.reason = "eligible_not_selected"
		}
	default:
		self.reason = self.filterReason()
	}
}

// Classifies actual removed candidates, not the presence of an exclusion
// field. Destination tails may be live-window refills; neither field proves
// the client's current health or why it excluded a provider.
func (self *findProviders2SelectionObservation) filterReason() string {
	explicitReason := "filtered_explicit"
	switch self.explicitSources {
	case 1:
		explicitReason = "filtered_client_ids"
	case 2:
		explicitReason = "filtered_destinations"
	case 3:
		explicitReason = "filtered_explicit_mixed"
	}
	reason := "unclassified_zero"
	for i, next := range [4]string{"filtered_hard", "filtered_network", "filtered_family", explicitReason} {
		if self.dropped[i] == 0 {
			continue
		}
		if reason != "unclassified_zero" {
			return "filtered_mixed"
		}
		reason = next
	}
	return reason
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
	self.finishPrivateCapture(ctx)
}
