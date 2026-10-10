package work

import (
	"context"
	"fmt"
	"math"
	"runtime"
	"slices"
	"strings"
	"time"

	"github.com/urnetwork/connect/v2026"
	"github.com/urnetwork/glog/v2026"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/controller"
	"github.com/urnetwork/server/v2026/geo"
	"github.com/urnetwork/server/v2026/geo/solve"
	"github.com/urnetwork/server/v2026/model"
	"github.com/urnetwork/server/v2026/session"
	"github.com/urnetwork/server/v2026/task"
)

// The derive phase (connect/GEOMAP.md §5): every DeriveInterval of the
// ingest's settings (controller.ExtenderPingReportSettings) the day of
// co-signed pings the sweep leaves is solved for where every provider
// and extender that took part in one is, by the least squares of §5.2 with the
// source reputation of §5.5 (server/geo/solve), and every node whose derived
// point explains its pings better than its genesis did (§5.4) is published to
// `derived_location`, mapped to the nearest city of the place list (§6). The
// rest keep their genesis: a derivation is a whole answer, so the table after a
// run holds exactly the nodes that run published.
//
// The window is the ingest's Retention, the pings' own life (§5.7): the job
// bounds its read by create_time over that span, and the sweep keeps every
// row at least that long (it drops whole days, only past the ingest's keep
// span), so the two can never drift apart.
//
// Each node is solved from its genesis (§5.1): an extender's latest
// activation; a provider's egress probe while it is fresh, else the location
// its connection's own address lookup resolved to. A genesis is anchored with
// the lookup's accuracy radius, a probe's fixed radius, or for a row without a
// radius the width of the level it was placed at (solve.Settings). A location
// placed only in a region or a country carries no coordinates, so it stands at
// its region's or country's representative city and is anchored at least as
// wide as that region's or country's cities spread (geo.Representatives).
//
// How the job reads the day and projects its own cost is in
// derive_location_scale.go.

// The derive job's own settings, beside the solver's (solve.Settings): the
// task's cap, how the day is read, the sample sizes a node's coverage is
// measured against, the budget the planner's projection is judged against,
// and how much the log names.
type DeriveLocationsSettings struct {
	// The longest one derivation may run, the task's cap. A day of pings over
	// a few thousand nodes solves in seconds to a minute (§5.3), and the scale
	// target in minutes (§5.3, "Scale"); the rest is headroom for creating the
	// location rows of the places the nodes map to.
	MaxTime time.Duration

	// The concurrent cursors the day's co-signed pings are read over, one
	// pinger-id hash range each and each its own database connection, so the
	// taskworker's pool must hold them beside its other work.
	DeriveReadCursors int

	// The pingers' sample sizes (D26), which a node's coverage is measured
	// against: an extender pings up to ExtenderPeerSampleSize of the other
	// extenders, a provider probes up to ProviderProbeSampleSize extenders.
	// A provider's probe pass (connect's ExtenderNetworkClientSettings) stops
	// once ProbeWindowCount (4) candidates measure close enough, and reaches
	// ProbeMaxCandidateCount (16) only in a badly connected region, so a
	// well-connected provider measures four to six distinct extenders by
	// design: four distinct targets is the designed coverage, and more is not
	// better. An expectation of sixteen would read that as a quarter coverage
	// and mark every well-connected provider down.
	ExtenderPeerSampleSize  int
	ProviderProbeSampleSize int

	// The solve's budget on this host, which the projection of the next run
	// is judged against: the task's wall time and the process's memory.
	MaxSolveSeconds float64
	MaxSolveBytes   int64
	// the margin on the projected seconds for a run that takes longer than
	// the last, up to the sweep cap
	SweepSafetyFactor float64

	// The costs a projection assumes before any run has measured them: a
	// sweep's time per term and per node on one core, and the heap per term
	// and per node. A measured run splits its time and heap in the same
	// proportion between terms and nodes, so these also fix that proportion.
	SecondsPerTermSweep float64
	SecondsPerNodeSweep float64
	BytesPerTerm        float64
	BytesPerNode        float64
	// the sweeps a projection assumes before any run has taken some: about
	// twelve in each of three reputation rounds
	DefaultSweeps int

	// how often the heap is sampled for the run's peak
	MemorySampleInterval time.Duration

	// the most excluded sources, and the most probe-country refusals, one run
	// names in its log; the counts are always complete
	LoggedExcludedSourcesMax      int
	LoggedProbeCountryRefusalsMax int
}

// The defaults: sixteen cursors, the pingers' designed samples (D26), and the
// budget GEOMAP §5.3 sizes the solve to on the taskworker host.
func DefaultDeriveLocationsSettings() *DeriveLocationsSettings {
	return &DeriveLocationsSettings{
		MaxTime:                       30 * time.Minute,
		DeriveReadCursors:             16,
		ExtenderPeerSampleSize:        64,
		ProviderProbeSampleSize:       4,
		MaxSolveSeconds:               600,
		MaxSolveBytes:                 8 * 1024 * 1024 * 1024,
		SweepSafetyFactor:             3,
		SecondsPerTermSweep:           0.5e-6,
		SecondsPerNodeSweep:           3e-6,
		BytesPerTerm:                  100,
		BytesPerNode:                  2 * 1024,
		DefaultSweeps:                 36,
		MemorySampleInterval:          250 * time.Millisecond,
		LoggedExcludedSourcesMax:      20,
		LoggedProbeCountryRefusalsMax: 20,
	}
}

// The task's arguments: none, since a derivation reads the whole window.
type DeriveLocationsArgs struct {
}

// What one derivation did. The same numbers are logged, and the ones the
// dashboard shows are recorded for the stats collector
// (model.DeriveLocationsRun).
type DeriveLocationsResult struct {
	// why nothing was derived, empty for a derivation that ran
	Refused string `json:"refused,omitempty"`

	// the window's co-signed direct pings, and those that could not be a
	// sample (an unknown pinger kind, a node's ping to itself)
	CosignedPings int `json:"cosigned_pings"`
	DroppedPings  int `json:"dropped_pings"`
	// the refusals counted against a pinger's and a target's refusal rates,
	// and those that are not evidence (§5.5)
	Refusals        int `json:"refusals"`
	IgnoredRefusals int `json:"ignored_refusals"`

	// the parties of the co-signed pings; those solved, by kind; those with no
	// genesis to solve from; and those anchored at a region's or country's
	// representative
	NodesSeen           int `json:"nodes_seen"`
	Nodes               int `json:"nodes"`
	ProviderNodes       int `json:"provider_nodes"`
	ExtenderNodes       int `json:"extender_nodes"`
	NodesWithoutGenesis int `json:"nodes_without_genesis"`
	StandInGeneses      int `json:"stand_in_geneses"`

	// the terms solved on, and those the solver dropped (a term toward a node
	// without a genesis)
	Terms        int `json:"terms"`
	DroppedTerms int `json:"dropped_terms"`
	// the sources the final solve left out on reputation
	ExcludedSources int `json:"excluded_sources"`

	// the rows published; those of them with exactly MinDerivePeers peers;
	// those whose mapped place is outside the genesis region or country; the
	// publishable nodes no place or location row could be found for; and the
	// rows of the previous derivation removed
	Published           int `json:"published"`
	PublishedAtMinPeers int `json:"published_at_min_peers"`
	CrossedRegion       int `json:"crossed_region"`
	CrossedCountry      int `json:"crossed_country"`
	Unmapped            int `json:"unmapped"`
	Removed             int `json:"removed"`
	// the publishable providers whose mapped country contradicts the country a
	// fresh egress probe observed their exit in, which keep their genesis
	// (§5.4 as amended by §10.3)
	RefusedProbeCountry int `json:"refused_probe_country"`
	// the solved nodes the publish gates of §5.4 refused, by the first gate
	// each failed: too few pings, too few peers, still moving when the solve
	// stopped (PublishMaxLastStepKm), or no better than genesis
	PublishRefusals solve.PublishRefusals `json:"publish_refusals"`

	// the RMS ping residual over the final solve's terms at the derived
	// positions and at genesis, in km
	ResidualKm        float64 `json:"residual_km"`
	GenesisResidualKm float64 `json:"genesis_residual_km"`
	// the sweeps of each reputation round, whether the last one stopped
	// before the sweep cap, and whether that stop was the objective's
	// stagnation rather than the step tolerance
	Sweeps    []int `json:"sweeps"`
	Converged bool  `json:"converged"`
	Stagnated bool  `json:"stagnated"`
	// the wall time of reading the day over the cursors, and of solving and
	// mapping
	IngestSeconds float64 `json:"ingest_seconds"`
	SolveSeconds  float64 `json:"solve_seconds"`
}

// Schedules the next derivation a DeriveInterval from now. The chain is one
// pending task (RunOnce), re-armed by every run's Post.
func ScheduleDeriveLocations(clientSession *session.ClientSession, tx server.PgTx) {
	task.ScheduleTaskInTx(
		tx,
		DeriveLocations,
		&DeriveLocationsArgs{},
		clientSession,
		task.RunOnce("derive_locations"),
		task.RunAt(server.NowUtc().Add(controller.DefaultExtenderPingReportSettings().DeriveInterval)),
		task.MaxTime(DefaultDeriveLocationsSettings().MaxTime),
	)
}

// Runs one derivation over the window ending now, with the default settings.
// Without the place list nothing can be mapped, and nothing is derived.
func DeriveLocations(
	_ *DeriveLocationsArgs,
	clientSession *session.ClientSession,
) (*DeriveLocationsResult, error) {
	places := model.CurrentPlaces()
	if places == nil {
		// Nothing can be mapped without the place list, and no retry brings one
		// until a deploy does; the chain keeps its cadence, and the dashboard's
		// time since the last derivation shows the gap.
		glog.Errorf("[derive]no place list; nothing is derived until one is deployed\n")
		return &DeriveLocationsResult{
			Refused: "no place list",
		}, nil
	}
	return deriveLocations(clientSession.Ctx, places, solve.DefaultSettings(), DefaultDeriveLocationsSettings(), server.NowUtc())
}

// Re-arms the chain.
func DeriveLocationsPost(
	_ *DeriveLocationsArgs,
	_ *DeriveLocationsResult,
	clientSession *session.ClientSession,
	tx server.PgTx,
) error {
	ScheduleDeriveLocations(clientSession, tx)
	return nil
}

// One derivation over the window ending at `now`: the plan of its cost, the
// day's pings read over the cursors, the geneses, the solve and the mapping,
// the publication, and the record of the run.
func deriveLocations(
	ctx context.Context,
	places *geo.Places,
	settings *solve.Settings,
	jobSettings *DeriveLocationsSettings,
	now time.Time,
) (*DeriveLocationsResult, error) {
	minCreateTime := now.Add(-controller.DefaultExtenderPingReportSettings().Retention)
	// the cores the solve sweeps on: every one the process may use, unless
	// the solver's settings fix fewer workers
	cores := runtime.GOMAXPROCS(0)
	if 0 < settings.Workers {
		cores = min(cores, settings.Workers)
	}

	// the planning step: this run, projected from the last (GEOMAP §5.3,
	// "Scale"); a projection over budget is the capacity alert of SIGNALS.md
	// §2.19c, not a reason to run differently
	projection := planDeriveRun(jobSettings, model.GetDeriveLocationsRuns(ctx, 1), cores, func() (int64, int64) {
		return model.CountNetworkPingTermsAndNodes(ctx, minCreateTime)
	})
	logDeriveProjection("plan", jobSettings, projection, cores)

	memoryPeak := startDeriveMemoryPeak(ctx, jobSettings.MemorySampleInterval)
	defer memoryPeak.finish()

	ingestStart := time.Now()
	inputs := ingestDeriveInputs(ctx, settings, jobSettings, minCreateTime)
	ingestSeconds := time.Since(ingestStart).Seconds()
	extenderPeers, providerPeers := inputs.setExpectedPeers(jobSettings)
	glog.Infof(
		"[derive]read %d co-signed pings into %d terms over %d cursors in %.1fs; expected peers: %d per extender (of %d extenders, ExtenderPeerSampleSize=%d), %d per provider (ProviderProbeSampleSize=%d)\n",
		inputs.cosignedPings,
		len(inputs.terms),
		jobSettings.DeriveReadCursors,
		ingestSeconds,
		extenderPeers,
		inputs.extenderNodes(),
		jobSettings.ExtenderPeerSampleSize,
		providerPeers,
		jobSettings.ProviderProbeSampleSize,
	)

	geneses := loadDeriveGeneses(ctx, inputs.nodeKeys, places, settings, now)

	previousDerivedLocations := map[string]*model.DerivedLocation{}
	for _, derivedLocation := range model.GetDerivedLocations(ctx) {
		previousDerivedLocations[deriveNodeId(derivedLocation.NodeKind, derivedLocation.NodeId)] = derivedLocation
	}

	solveStart := time.Now()
	plan := planDerivation(inputs, geneses, previousDerivedLocations, places, settings)
	solveSeconds := time.Since(solveStart).Seconds()
	peakBytes := memoryPeak.finish()

	// Per node, why a publishable node was not published: its derived place is
	// in a country other than the one a fresh egress probe watched its exit
	// leave from. A rising count is a solver or genesis fault to look at, not
	// a provider fault -- the provider is reachable and exits where it was
	// probed.
	logProbeCountryRefusals := func() {
		refusals := plan.probeCountryRefusals
		loggedMax := max(0, jobSettings.LoggedProbeCountryRefusalsMax)
		for _, refusal := range refusals[:min(len(refusals), loggedMax)] {
			glog.Infof(
				"[derive]%s not published: it maps to %s, %s, %s, but a fresh egress probe observed its exit in %s; it keeps its genesis\n",
				refusal.nodeId,
				refusal.place.City,
				refusal.place.Region,
				refusal.place.CountryCode,
				refusal.probeCountryCode,
			)
		}
		if loggedMax < len(refusals) {
			glog.Infof("[derive]and %d more nodes not published against a fresh probe's country\n", len(refusals)-loggedMax)
		}
	}
	logProbeCountryRefusals()

	sweeps := 0
	for _, roundSweeps := range plan.result.Sweeps {
		sweeps += roundSweeps
	}
	costs := measureDeriveRunCosts(jobSettings, plan.result.TermCount, len(plan.result.Nodes), sweeps, cores, solveSeconds, peakBytes)
	next := projectDeriveRun(jobSettings, costs, cores)
	logDeriveProjection("next run", jobSettings, next, cores)

	// The location row of each mapped place, created once per city per
	// derivation, since many nodes map to one city: created as a lookup of the
	// city would create it, or nil when it cannot be -- a country the location
	// table cannot name, or a city that would not resolve to a full city row. A
	// published row always names a city with its region and country, so the
	// read path is one lookup (§6).
	cityLocations := map[uint32]*model.Location{}
	cityLocation := func(place *geo.Place) *model.Location {
		if location, ok := cityLocations[place.GeonameId]; ok {
			return location
		}
		location := &model.Location{
			LocationType:    model.LocationTypeCity,
			City:            place.City,
			Region:          place.Region,
			CountryCode:     place.CountryCode,
			Latitude:        place.Latitude,
			Longitude:       place.Longitude,
			Timezone:        place.TimeZone,
			CityGeonameId:   place.GeonameId,
			RegionGeonameId: place.RegionGeonameId,
		}
		if country := places.Country(place.CountryCode); country != nil {
			location.Country = country.Name
			location.CountryGeonameId = country.GeonameId
		}
		var resolved *model.Location
		if r := server.HandleError(func() {
			model.CreateLocation(ctx, location)
		}); r == nil &&
			location.LocationType == model.LocationTypeCity &&
			location.CityLocationId != (server.Id{}) &&
			location.RegionLocationId != (server.Id{}) &&
			location.CountryLocationId != (server.Id{}) {
			resolved = location
		} else if r == nil {
			glog.Infof("[derive]%s, %s, %s resolved to no city row; its nodes keep their genesis\n", place.City, place.Region, place.CountryCode)
		}
		cityLocations[place.GeonameId] = resolved
		return resolved
	}
	derivedLocations := make([]*model.DerivedLocation, 0, len(plan.publications))
	unmapped := plan.unmapped
	for _, publication := range plan.publications {
		location := cityLocation(publication.mapping.place)
		if location == nil {
			unmapped += 1
			continue
		}
		derivedLocations = append(derivedLocations, publication.derivedLocation(location, now))
	}
	// a cancelled run publishes nothing: a partial row set would remove every
	// node the run did not reach
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	written, removed := model.ReplaceDerivedLocations(ctx, derivedLocations)

	result := plan.summary(inputs)
	result.Published = written
	result.Unmapped = unmapped
	result.Removed = removed
	result.IngestSeconds = ingestSeconds
	result.SolveSeconds = solveSeconds
	for _, derivedLocation := range derivedLocations {
		if derivedLocation.PeerCount == settings.MinDerivePeers {
			result.PublishedAtMinPeers += 1
		}
		if derivedLocation.CrossedRegion {
			result.CrossedRegion += 1
		}
		if derivedLocation.CrossedCountry {
			result.CrossedCountry += 1
		}
	}

	logDerivation := func() {
		excluded := plan.result.Excluded
		loggedMax := max(0, jobSettings.LoggedExcludedSourcesMax)
		excludedNames := strings.Join(excluded[:min(len(excluded), loggedMax)], " ")
		if loggedMax < len(excluded) {
			excludedNames += fmt.Sprintf(" and %d more", len(excluded)-loggedMax)
		}
		glog.Infof(
			"[derive]%d nodes solved of %d seen (%d providers, %d extenders; %d without a genesis, %d at a region or country stand-in) on %d terms (%d dropped) from %d co-signed pings (%d dropped), %d refusals (%d not evidence); published %d (crossed region %d, crossed country %d), unmapped %d, refused %d for few pings, %d for few peers, %d still moving, %d for no improvement, %d against a fresh probe's country, removed %d; excluded %d sources [%s]; residual %.2f km against %.2f km at genesis; read in %.2fs, solved and mapped in %.2fs\n",
			result.Nodes,
			result.NodesSeen,
			result.ProviderNodes,
			result.ExtenderNodes,
			result.NodesWithoutGenesis,
			result.StandInGeneses,
			result.Terms,
			result.DroppedTerms,
			result.CosignedPings,
			result.DroppedPings,
			result.Refusals,
			result.IgnoredRefusals,
			result.Published,
			result.CrossedRegion,
			result.CrossedCountry,
			result.Unmapped,
			result.PublishRefusals.FewPings,
			result.PublishRefusals.FewPeers,
			result.PublishRefusals.StillMoving,
			result.PublishRefusals.NoImprovement,
			result.RefusedProbeCountry,
			result.Removed,
			result.ExcludedSources,
			excludedNames,
			result.ResidualKm,
			result.GenesisResidualKm,
			result.IngestSeconds,
			result.SolveSeconds,
		)
		// The two calibration findings of the solver (GEOMAP §9), kept in
		// view: about 1.5% of synthetic honest nodes with a genesis 170-500 km
		// off converged to a wrong place, the mirror ambiguity of a node with
		// two peers, which the peer gate of three (MinDerivePeers, D11) now
		// refuses as few peers -- the nodes published at exactly the gate are
		// the thinnest evidence left; and the later reputation rounds can
		// reach the sweep cap at a few thousand nodes.
		glog.Infof(
			"[derive]calibration: %d of %d published nodes have exactly MinDerivePeers=%d peers; sweeps per reputation round %v against the cap of %d (converged %t, on stagnation %t); %d nodes still moving past PublishMaxLastStepKm=%g km when the solve stopped\n",
			result.PublishedAtMinPeers,
			result.Published,
			settings.MinDerivePeers,
			result.Sweeps,
			settings.MaxIterations,
			result.Converged,
			result.Stagnated,
			result.PublishRefusals.StillMoving,
			settings.PublishMaxLastStepKm,
		)
	}
	logDerivation()

	// The record of this run, which the dashboard and the monitor read
	// (SIGNALS.md §2.19c); a failure to record it costs them one run's
	// numbers, never the derivation. Sources are the nodes with terms of
	// their own, the population reputation scores.
	sources := 0
	for i := range plan.result.Nodes {
		if 0 < plan.result.Nodes[i].SourceTermCount {
			sources += 1
		}
	}
	lastRoundSweeps := 0
	if 0 < len(plan.result.Sweeps) {
		lastRoundSweeps = plan.result.Sweeps[len(plan.result.Sweeps)-1]
	}
	run := &model.DeriveLocationsRun{
		RunTime:               now,
		Nodes:                 result.Nodes,
		Sources:               sources,
		Terms:                 result.Terms,
		CosignedPings:         result.CosignedPings,
		Published:             result.Published,
		ExcludedSources:       result.ExcludedSources,
		ResidualKm:            result.ResidualKm,
		GenesisResidualKm:     result.GenesisResidualKm,
		Converged:             plan.result.Converged,
		Stagnated:             plan.result.Stagnated,
		LastRoundSweeps:       lastRoundSweeps,
		SweepCap:              settings.MaxIterations,
		RefusedFewPings:       result.PublishRefusals.FewPings,
		RefusedFewPeers:       result.PublishRefusals.FewPeers,
		RefusedStillMoving:    result.PublishRefusals.StillMoving,
		RefusedNoImprovement:  result.PublishRefusals.NoImprovement,
		RefusedProbeCountry:   result.RefusedProbeCountry,
		Unmapped:              result.Unmapped,
		Sweeps:                costs.sweeps,
		Cores:                 costs.cores,
		ReadCursors:           jobSettings.DeriveReadCursors,
		IngestSeconds:         ingestSeconds,
		SolveSeconds:          costs.solveSeconds,
		PeakBytes:             costs.peakBytes,
		SecondsPerTermSweep:   costs.secondsPerTermSweep,
		SecondsPerNodeSweep:   costs.secondsPerNodeSweep,
		BytesPerTerm:          costs.bytesPerTerm,
		BytesPerNode:          costs.bytesPerNode,
		ProjectedSeconds:      next.seconds,
		ProjectedBytes:        next.bytes,
		MaxSolveSeconds:       jobSettings.MaxSolveSeconds,
		MaxSolveBytes:         jobSettings.MaxSolveBytes,
		ExpectedExtenderPeers: extenderPeers,
		ExpectedProviderPeers: providerPeers,
	}
	server.HandleError(func() {
		model.AddDeriveLocationsRun(ctx, run)
	})
	return result, nil
}

// Node ids. A provider and an extender are keyed by different id spaces, so
// the kind is part of the id and the two can never collide.
const (
	deriveProviderNodeIdPrefix = "p:"
	deriveExtenderNodeIdPrefix = "e:"
)

// The party behind a node id.
type deriveNodeKey struct {
	nodeKind int
	id       server.Id
}

// The node id of a party, "" for a node kind that is not a node.
func deriveNodeId(nodeKind int, id server.Id) string {
	switch nodeKind {
	case model.DerivedLocationNodeKindProvider:
		return deriveProviderNodeIdPrefix + id.String()
	case model.DerivedLocationNodeKindExtender:
		return deriveExtenderNodeIdPrefix + id.String()
	default:
		return ""
	}
}

// The party a node id names, the inverse of deriveNodeId; false for a string
// that is no node id.
func deriveNodeKeyOf(nodeId string) (deriveNodeKey, bool) {
	var nodeKind int
	var idString string
	switch {
	case strings.HasPrefix(nodeId, deriveProviderNodeIdPrefix):
		nodeKind = model.DerivedLocationNodeKindProvider
		idString = strings.TrimPrefix(nodeId, deriveProviderNodeIdPrefix)
	case strings.HasPrefix(nodeId, deriveExtenderNodeIdPrefix):
		nodeKind = model.DerivedLocationNodeKindExtender
		idString = strings.TrimPrefix(nodeId, deriveExtenderNodeIdPrefix)
	default:
		return deriveNodeKey{}, false
	}
	id, err := server.ParseId(idString)
	if err != nil {
		return deriveNodeKey{}, false
	}
	return deriveNodeKey{nodeKind: nodeKind, id: id}, true
}

// The node kind of a pinger kind, false for a pinger kind that is not a node.
func derivePingerNodeKind(pingerKind int) (int, bool) {
	switch pingerKind {
	case model.NetworkPingPingerKindProvider:
		return model.DerivedLocationNodeKindProvider, true
	case model.NetworkPingPingerKindExtender:
		return model.DerivedLocationNodeKindExtender, true
	default:
		return 0, false
	}
}

// Whether a refusal's reason speaks to either party's honesty (§5.5): a round
// trip below the one the target observed, a bad nonce, a claim bound to the
// wrong extender, or a bad signature. A pinger the target does not know is
// usually a directory that has not caught up, and a probe refused for rate is
// usually the shared address of an NLayer front (§2.9); neither is evidence
// against anyone, so neither counts as a refusal or as an attestation.
func derivePingRefusalIsEvidence(reason int) bool {
	switch reason {
	case int(connect.ExtenderProbeVerdictReasonRttBelowObserved),
		int(connect.ExtenderProbeVerdictReasonNonce),
		int(connect.ExtenderProbeVerdictReasonWrongExtender),
		int(connect.ExtenderProbeVerdictReasonBadSignature):
		return true
	default:
		return false
	}
}

// The window's pings as the solver takes them: one term per ordered pair, the
// nodes the terms name, and every party's attestation and refusal counts. The
// co-signed direct pings arrive from the cursors (mergeDeriveInputs); the
// relayed co-signatures and the refusals, a small share of the day, are then
// counted as one stream each delivers them. Not safe for concurrent use.
type deriveInputs struct {
	// ordered by source, then target
	terms []solve.Term
	// the party of every node: the sources and targets of the terms
	nodeKeys map[string]deriveNodeKey
	// every party's counts, a node or not: a refusal rate is over all of a
	// party's attestations, whoever its counterparts were
	refusals map[string]*solve.Refusals
	// the distinct peers a node of each kind was expected to measure (D26),
	// by node kind, 0 for a kind with no expectation (setExpectedPeers)
	nodeKindExpectedPeers map[int]int

	cosignedPings   int
	droppedPings    int
	refusalCount    int
	ignoredRefusals int
}

// A party's counts, created at zero.
func (self *deriveInputs) counts(nodeId string) *solve.Refusals {
	counts, ok := self.refusals[nodeId]
	if !ok {
		counts = &solve.Refusals{}
		self.refusals[nodeId] = counts
	}
	return counts
}

// Takes the co-signed pings of one pair that were relayed: attestations that
// were not refused, and not samples (§2.9).
func (self *deriveInputs) addRelayedCosigns(count *model.NetworkPingAttestationCount) {
	nodeKind, ok := derivePingerNodeKind(count.PingerKind)
	if !ok || count.Count <= 0 {
		return
	}
	source := deriveNodeId(nodeKind, count.PingerId)
	target := deriveNodeId(model.DerivedLocationNodeKindExtender, count.TargetExtenderId)
	self.counts(source).PingsAsPinger += count.Count
	self.counts(target).PingsAsTarget += count.Count
}

// Takes one refused ping: when its reason is evidence, a refusal of its
// pinger's attestation and one its target made.
func (self *deriveInputs) addRefusal(refusal *model.NetworkPingRefusal) {
	nodeKind, ok := derivePingerNodeKind(refusal.PingerKind)
	if !ok || !derivePingRefusalIsEvidence(refusal.Reason) {
		self.ignoredRefusals += 1
		return
	}
	source := deriveNodeId(nodeKind, refusal.PingerId)
	target := deriveNodeId(model.DerivedLocationNodeKindExtender, refusal.TargetExtenderId)
	self.refusalCount += 1
	pinger := self.counts(source)
	pinger.AsPinger += 1
	pinger.PingsAsPinger += 1
	refuser := self.counts(target)
	refuser.AsTarget += 1
	refuser.PingsAsTarget += 1
}

// The extenders the terms name: the extenders a pinger could have sampled
// over the day.
func (self *deriveInputs) extenderNodes() int {
	extenders := 0
	for _, key := range self.nodeKeys {
		if key.nodeKind == model.DerivedLocationNodeKindExtender {
			extenders += 1
		}
	}
	return extenders
}

// Sets the peers each kind of node is expected to measure (GEOMAP §5.5,
// "coverage"; D26): the sample its pinger draws from the extenders it can
// see, which is every extender of the window but itself, up to the pinger's
// sample size. A source is then never marked down for sampling by design.
// Returns the two expectations.
func (self *deriveInputs) setExpectedPeers(jobSettings *DeriveLocationsSettings) (extenderPeers int, providerPeers int) {
	extenders := self.extenderNodes()
	extenderPeers = min(max(0, extenders-1), jobSettings.ExtenderPeerSampleSize)
	providerPeers = min(extenders, jobSettings.ProviderProbeSampleSize)
	self.nodeKindExpectedPeers = map[int]int{
		model.DerivedLocationNodeKindExtender: extenderPeers,
		model.DerivedLocationNodeKindProvider: providerPeers,
	}
	return extenderPeers, providerPeers
}

// The counts of every node, as the solver takes them.
func (self *deriveInputs) solverRefusals() map[string]solve.Refusals {
	refusals := make(map[string]solve.Refusals, len(self.nodeKeys))
	for nodeId := range self.nodeKeys {
		if counts, ok := self.refusals[nodeId]; ok {
			refusals[nodeId] = *counts
		}
	}
	return refusals
}

// What placed a genesis, for the summary and the tests.
type deriveGenesisSource int

const (
	deriveGenesisSourceActivation deriveGenesisSource = iota
	deriveGenesisSourceProbe
	deriveGenesisSourceConnection
)

// One node's genesis as the solver anchors it (§5.1).
type deriveGenesis struct {
	source   deriveGenesisSource
	level    solve.GenesisLevel
	position solve.LatLon
	radiusKm float64
	// lower case
	countryCode string
	// the genesis region, empty for a genesis placed only in its country; its
	// GeoNames id is the list's when the row has none
	region          string
	regionGeonameId uint32
	// the position is the region's or country's representative, not a place
	// of the node's own
	standIn bool
	// the country a fresh egress probe observed the provider's exit in, lower
	// case, whatever the genesis was placed from; "" for an extender and for a
	// provider without a fresh probe
	probeCountryCode string
}

// The containment key of the genesis region (geo.ContainmentRegionKey), ""
// for a genesis placed only in its country.
func (self *deriveGenesis) regionKey() string {
	if self.region == "" {
		return ""
	}
	return geo.ContainmentRegionKey(self.countryCode, self.regionGeonameId, self.region)
}

// The containment key of the genesis country (geo.ContainmentCountryKey), ""
// for a genesis without one.
func (self *deriveGenesis) countryKey() string {
	if self.countryCode == "" {
		return ""
	}
	return geo.ContainmentCountryKey(self.countryCode)
}

// Reads the genesis of every node at once: the rows that place each node, and
// the location rows they name, in one query each.
func loadDeriveGeneses(
	ctx context.Context,
	nodeKeys map[string]deriveNodeKey,
	places *geo.Places,
	settings *solve.Settings,
	now time.Time,
) map[string]*deriveGenesis {
	extenderIds := []server.Id{}
	clientIds := []server.Id{}
	for _, key := range nodeKeys {
		switch key.nodeKind {
		case model.DerivedLocationNodeKindExtender:
			extenderIds = append(extenderIds, key.id)
		case model.DerivedLocationNodeKindProvider:
			clientIds = append(clientIds, key.id)
		}
	}
	activations := model.GetLatestNetworkExtenderActivations(ctx, extenderIds)
	probes := model.GetFreshProviderEgressLocations(ctx, clientIds, now.Add(-model.ProviderEgressLocationMaxAge))
	connections := model.GetConnectionGenesisLocations(ctx, clientIds)

	locationIds := []server.Id{}
	addLocationId := func(locationId *server.Id) {
		if locationId != nil {
			locationIds = append(locationIds, *locationId)
		}
	}
	for _, activation := range activations {
		addLocationId(activation.LocationId)
		addLocationId(activation.CityLocationId)
		addLocationId(activation.RegionLocationId)
		addLocationId(activation.CountryLocationId)
	}
	for _, probe := range probes {
		addLocationId(&probe.LocationId)
	}
	for _, connection := range connections {
		addLocationId(connection.GenesisLocationId)
		addLocationId(&connection.CityLocationId)
		addLocationId(&connection.RegionLocationId)
		addLocationId(&connection.CountryLocationId)
	}
	slices.SortFunc(locationIds, func(a server.Id, b server.Id) int {
		return a.Cmp(b)
	})
	locationIds = slices.Compact(locationIds)

	resolver := &deriveGenesisResolver{
		places:          places,
		representatives: geo.NewRepresentatives(places),
		settings:        settings,
		locations:       model.GetLocations(ctx, locationIds),
	}
	geneses := map[string]*deriveGenesis{}
	for nodeId, key := range nodeKeys {
		var genesis *deriveGenesis
		var ok bool
		switch key.nodeKind {
		case model.DerivedLocationNodeKindExtender:
			genesis, ok = resolver.extenderGenesis(activations[key.id])
		case model.DerivedLocationNodeKindProvider:
			genesis, ok = resolver.providerGenesis(probes[key.id], connections[key.id])
			if probe := probes[key.id]; ok && probe != nil {
				genesis.probeCountryCode = strings.ToLower(probe.CountryCode)
			}
		}
		if ok {
			geneses[nodeId] = genesis
		}
	}
	return geneses
}

// Places a node's genesis from the rows that describe it. It reads no
// database: the rows are loaded first, for every node at once.
type deriveGenesisResolver struct {
	places          *geo.Places
	representatives *geo.Representatives
	settings        *solve.Settings
	locations       map[server.Id]*model.Location
}

// An extender's latest activation, anchored with its lookup's radius.
func (self *deriveGenesisResolver) extenderGenesis(activation *model.NetworkExtenderActivationRecord) (*deriveGenesis, bool) {
	if activation == nil {
		return nil, false
	}
	// location_id is the most precise row the lookup resolved to; the others
	// are its hierarchy, each null past the granularity the lookup reached
	var location *model.Location
	for _, locationId := range []*server.Id{
		activation.LocationId,
		activation.CityLocationId,
		activation.RegionLocationId,
		activation.CountryLocationId,
	} {
		if locationId == nil {
			continue
		}
		if found, ok := self.locations[*locationId]; ok {
			location = found
			break
		}
	}
	if location == nil {
		return nil, false
	}
	return self.genesis(location, deriveGenesisSourceActivation, func(level solve.GenesisLevel) float64 {
		return self.settings.GenesisRadiusKm(deriveAccuracyKm(activation.AccuracyKm), level)
	})
}

// A provider's fresh egress probe, anchored with the probe's radius, else the
// location its connection's own lookup resolved to, anchored with the lookup's
// radius.
func (self *deriveGenesisResolver) providerGenesis(
	probe *model.ProviderEgressLocation,
	connection *model.ConnectionGenesisLocation,
) (*deriveGenesis, bool) {
	if probe != nil {
		if location, ok := self.locations[probe.LocationId]; ok {
			genesis, ok := self.genesis(location, deriveGenesisSourceProbe, func(solve.GenesisLevel) float64 {
				return self.settings.ProbedGenesisRadiusKm(probe.CityConfident)
			})
			if ok {
				return genesis, true
			}
		}
	}
	if connection == nil {
		return nil, false
	}
	var location *model.Location
	if connection.GenesisLocationId != nil {
		// The connection stores its genesis apart from where it is published.
		// When that row cannot be read the published columns are no
		// substitute: they may be a derived location, and a derivation must
		// never anchor to its own answer.
		location = self.locations[*connection.GenesisLocationId]
	} else {
		// a row written before the genesis column: the most precise location
		// it stores. A coarser location stores its coarsest id in the finer
		// columns (SetConnectionLocation), so the city column holds a city or
		// the country, and the region column a region or the country
		if city, ok := self.locations[connection.CityLocationId]; ok && city.LocationType == model.LocationTypeCity {
			location = city
		} else if region, ok := self.locations[connection.RegionLocationId]; ok && region.LocationType == model.LocationTypeRegion {
			location = region
		} else {
			location = self.locations[connection.CountryLocationId]
		}
	}
	if location == nil {
		return nil, false
	}
	return self.genesis(location, deriveGenesisSourceConnection, func(level solve.GenesisLevel) float64 {
		return self.settings.GenesisRadiusKm(deriveAccuracyKm(connection.AccuracyKm), level)
	})
}

// Places a location. A city is at its own coordinates, else at the place
// list's for its GeoNames id; anything coarser, or a city with neither, stands
// at its region's representative, else its country's, and is anchored at least
// as wide as that region's or country's cities spread: a lookup that could
// only place the node somewhere in the region is not evidence of where in it.
func (self *deriveGenesisResolver) genesis(
	location *model.Location,
	source deriveGenesisSource,
	radiusKm func(level solve.GenesisLevel) float64,
) (*deriveGenesis, bool) {
	level := solve.GenesisLevelCountry
	switch location.LocationType {
	case model.LocationTypeCity:
		level = solve.GenesisLevelCity
	case model.LocationTypeRegion:
		level = solve.GenesisLevelRegion
	}
	genesis := &deriveGenesis{
		source:      source,
		level:       level,
		countryCode: strings.ToLower(location.CountryCode),
	}
	if genesis.level != solve.GenesisLevelCountry && location.Region != "" {
		genesis.region = location.Region
		genesis.regionGeonameId = location.RegionGeonameId
		if genesis.regionGeonameId == 0 {
			// a row that predates the ids is keyed as the list keys its region
			genesis.regionGeonameId = self.places.RegionGeonameId(genesis.countryCode, location.Region)
		}
	}
	genesis.radiusKm = radiusKm(genesis.level)

	if genesis.level == solve.GenesisLevelCity {
		// the mmdb gives 0,0 for unknown coordinates and the rows store that
		// as NULL, which reads back as 0,0
		if location.Latitude != 0 || location.Longitude != 0 {
			genesis.position = solve.LatLon{Latitude: location.Latitude, Longitude: location.Longitude}
			valid := -90 <= genesis.position.Latitude && genesis.position.Latitude <= 90 &&
				-180 <= genesis.position.Longitude && genesis.position.Longitude <= 180
			return genesis, valid
		}
		if place := self.places.CityByGeonameId(location.CityGeonameId); location.CityGeonameId != 0 && place != nil {
			genesis.position = solve.LatLon{Latitude: place.Latitude, Longitude: place.Longitude}
			return genesis, true
		}
	}

	representative, ok := geo.Representative{}, false
	if genesis.region != "" {
		representative, ok = self.representatives.Region(genesis.countryCode, genesis.regionGeonameId, genesis.region)
	}
	if !ok {
		representative, ok = self.representatives.Country(genesis.countryCode)
	}
	if !ok {
		return nil, false
	}
	genesis.position = solve.LatLon{Latitude: representative.Place.Latitude, Longitude: representative.Place.Longitude}
	genesis.radiusKm = max(genesis.radiusKm, representative.SpreadKm)
	genesis.standIn = true
	return genesis, true
}

// A stored radius, 0 for none (Settings.GenesisRadiusKm).
func deriveAccuracyKm(accuracyKm *float32) float64 {
	if accuracyKm == nil {
		return 0
	}
	return float64(*accuracyKm)
}

// The place a derived point maps to (§6), and whether that place lies outside
// the genesis region or country. A crossing is not an error: the containment
// terms of §5.2 already made the solve pay for it, so one that survives is
// evidence -- of a wrong genesis, a bad k, or a colluding cluster -- and is
// counted and shown rather than refused.
type deriveMapping struct {
	place          *geo.Place
	crossedRegion  bool
	crossedCountry bool
}

// Maps a derived point to the nearest city anywhere; the genesis bias is the
// solve's, not the mapping's (D10).
func mapDerivedPosition(position solve.LatLon, genesis *deriveGenesis, places *geo.Places) (*deriveMapping, bool) {
	place, _ := places.NearestCity(position.Latitude, position.Longitude, "")
	if place == nil {
		return nil, false
	}
	mapping := &deriveMapping{
		place:          place,
		crossedCountry: place.CountryCode != genesis.countryCode,
	}
	// A genesis placed only in its country has no region to leave but by
	// leaving the country. A place is in the genesis region by the same
	// GeoNames id when both have one, as the list knows its regions' ids,
	// else by the same name, the key a region filed under no subdivision goes
	// by.
	sameRegion := func() bool {
		placeRegionGeonameId := places.RegionGeonameId(place.CountryCode, place.Region)
		if genesis.regionGeonameId != 0 && placeRegionGeonameId != 0 {
			return genesis.regionGeonameId == placeRegionGeonameId
		}
		return genesis.region == place.Region
	}
	mapping.crossedRegion = mapping.crossedCountry || (genesis.region != "" && !sameRegion())
	return mapping, true
}

// The longitude from `from` to `to` the short way round, in [-180, 180): a
// correction across the antimeridian is a few degrees, not nearly 360.
func deriveLongitudeDelta(from float64, to float64) float64 {
	delta := math.Mod(to-from+180, 360)
	if delta < 0 {
		delta += 360
	}
	return delta - 180
}

// One node to publish, before its place has a location row.
type derivePublication struct {
	key        deriveNodeKey
	genesis    *deriveGenesis
	nodeResult *solve.NodeResult
	mapping    *deriveMapping
}

// The published row, at the place's location row.
func (self *derivePublication) derivedLocation(location *model.Location, now time.Time) *model.DerivedLocation {
	position := self.nodeResult.Position
	return &model.DerivedLocation{
		NodeKind:          self.key.nodeKind,
		NodeId:            self.key.id,
		GenesisLatitude:   self.genesis.position.Latitude,
		GenesisLongitude:  self.genesis.position.Longitude,
		GenesisAccuracyKm: float32(self.genesis.radiusKm),
		DeltaLatitude:     position.Latitude - self.genesis.position.Latitude,
		DeltaLongitude:    deriveLongitudeDelta(self.genesis.position.Longitude, position.Longitude),
		Latitude:          position.Latitude,
		Longitude:         position.Longitude,
		PingCount:         self.nodeResult.PingCount,
		PeerCount:         self.nodeResult.PeerCount,
		ResidualKm:        float32(self.nodeResult.ResidualKm),
		Reputation:        float32(self.nodeResult.Q),
		CrossedRegion:     self.mapping.crossedRegion,
		CrossedCountry:    self.mapping.crossedCountry,
		LocationId:        location.LocationId,
		CityLocationId:    location.CityLocationId,
		RegionLocationId:  location.RegionLocationId,
		CountryLocationId: location.CountryLocationId,
		UpdateTime:        now,
	}
}

// A publishable node the probe-country gate kept at its genesis.
type deriveProbeCountryRefusal struct {
	nodeId           string
	place            *geo.Place
	probeCountryCode string
}

// One derivation's answer before anything is stored.
type derivePlan struct {
	nodes        []solve.Node
	result       *solve.Result
	publications []*derivePublication
	// publishable nodes whose point maps to no place
	unmapped int
	// publishable nodes whose place contradicts a fresh probe's country
	probeCountryRefusals []*deriveProbeCountryRefusal
	// nodes that have a genesis, by kind, and those that stand in
	providerNodes  int
	extenderNodes  int
	standInGeneses int
	nodesSeen      int
}

// Solves the nodes that have a genesis on the window's terms, each warm
// started from the position its previous row holds, and maps every
// publishable one (§5.4). It reads no database.
func planDerivation(
	inputs *deriveInputs,
	geneses map[string]*deriveGenesis,
	previousDerivedLocations map[string]*model.DerivedLocation,
	places *geo.Places,
	settings *solve.Settings,
) *derivePlan {
	plan := &derivePlan{
		nodesSeen: len(inputs.nodeKeys),
	}
	nodeIds := make([]string, 0, len(inputs.nodeKeys))
	for nodeId := range inputs.nodeKeys {
		nodeIds = append(nodeIds, nodeId)
	}
	slices.Sort(nodeIds)
	for _, nodeId := range nodeIds {
		genesis, ok := geneses[nodeId]
		if !ok {
			continue
		}
		key := inputs.nodeKeys[nodeId]
		node := solve.Node{
			Id:            nodeId,
			Genesis:       genesis.position,
			RadiusKm:      genesis.radiusKm,
			RegionKey:     genesis.regionKey(),
			CountryKey:    genesis.countryKey(),
			ExpectedPeers: inputs.nodeKindExpectedPeers[key.nodeKind],
		}
		if previousDerivedLocation, ok := previousDerivedLocations[nodeId]; ok {
			// start from where the last derivation put the node, whatever
			// genesis it was solved from then
			node.PreviousCorrection = solve.OffsetBetween(
				genesis.position,
				solve.LatLon{Latitude: previousDerivedLocation.Latitude, Longitude: previousDerivedLocation.Longitude},
			)
		}
		plan.nodes = append(plan.nodes, node)
		switch key.nodeKind {
		case model.DerivedLocationNodeKindProvider:
			plan.providerNodes += 1
		case model.DerivedLocationNodeKindExtender:
			plan.extenderNodes += 1
		}
		if genesis.standIn {
			plan.standInGeneses += 1
		}
	}

	plan.result = solve.Solve(
		plan.nodes,
		inputs.terms,
		inputs.solverRefusals(),
		geo.NewPlaceContainment(places),
		settings,
	)
	for i := range plan.result.Nodes {
		nodeResult := &plan.result.Nodes[i]
		if !solve.Publishable(nodeResult, settings) {
			continue
		}
		genesis := geneses[nodeResult.Id]
		mapping, ok := mapDerivedPosition(nodeResult.Position, genesis, places)
		if !ok {
			plan.unmapped += 1
			continue
		}
		// One more gate (§5.4 as amended by §10.3): a place in a country other
		// than the one a fresh egress probe watched the exit leave from is not
		// published. Without a derivation a fresh probe is where the provider
		// is published (§6), so the only way it could be listed away from its
		// observed exit is a derivation that crossed that border -- and the
		// ranking's country gate would then take it out of both buckets. The
		// probe is evidence of where the traffic exits; the pings, of where the
		// device sits. Where they disagree on the country, the listing keeps the
		// genesis, and the ranking gate stays the backstop.
		if genesis.probeCountryCode != "" && strings.ToLower(mapping.place.CountryCode) != genesis.probeCountryCode {
			plan.probeCountryRefusals = append(plan.probeCountryRefusals, &deriveProbeCountryRefusal{
				nodeId:           nodeResult.Id,
				place:            mapping.place,
				probeCountryCode: genesis.probeCountryCode,
			})
			continue
		}
		plan.publications = append(plan.publications, &derivePublication{
			key:        inputs.nodeKeys[nodeResult.Id],
			genesis:    genesis,
			nodeResult: nodeResult,
			mapping:    mapping,
		})
	}
	return plan
}

// The plan's part of the result; the caller adds what storing it did, and
// counts the published rows, which are the publications that also found a
// location row.
func (self *derivePlan) summary(inputs *deriveInputs) *DeriveLocationsResult {
	return &DeriveLocationsResult{
		CosignedPings:       inputs.cosignedPings,
		DroppedPings:        inputs.droppedPings,
		Refusals:            inputs.refusalCount,
		IgnoredRefusals:     inputs.ignoredRefusals,
		NodesSeen:           self.nodesSeen,
		Nodes:               len(self.result.Nodes),
		ProviderNodes:       self.providerNodes,
		ExtenderNodes:       self.extenderNodes,
		NodesWithoutGenesis: self.nodesSeen - len(self.nodes),
		StandInGeneses:      self.standInGeneses,
		Terms:               self.result.TermCount,
		DroppedTerms:        self.result.DroppedTermCount,
		ExcludedSources:     len(self.result.Excluded),
		Unmapped:            self.unmapped,
		RefusedProbeCountry: len(self.probeCountryRefusals),
		PublishRefusals:     self.result.PublishRefusals,
		ResidualKm:          self.result.ResidualKm,
		GenesisResidualKm:   self.result.GenesisResidualKm,
		Sweeps:              self.result.Sweeps,
		Converged:           self.result.Converged,
		Stagnated:           self.result.Stagnated,
	}
}
