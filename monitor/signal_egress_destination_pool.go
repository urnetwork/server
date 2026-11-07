package monitor

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/urnetwork/server/controller"
)

// SIGNALS.md §2.19h maps to signal_egress_destination_pool.go and
// signal_egress_destination_pool_test.go. Every URL pass starts by fetching
// the destination pool (GET /network/provider-egress-destinations, or the
// in-process operator); a pass without one measures no URL, and with every
// pass failing the Quality and Speed buckets empty within eight hours. The
// pool is built from qualityprobe.yml, whose entries must keep the country
// lists disjoint from the general catalog: the server refuses a colliding
// entry alone rather than the pool, which keeps measuring but serves less
// than the catalog says. The signal reads the catalog through the API's own
// parser for the entries it refuses, and the Taskworker pool-fetch counter and
// the fleet's serve and refresh refusal counters from Mimir. Entry names,
// country codes, reasons and counts are the only output: never a URL or a
// website.
func NewEgressDestinationPoolSignal() Signal {
	return NewEgressDestinationPoolSignalWithSettings(DefaultEgressDestinationPoolSettings())
}

// The signal with explicit thresholds.
func NewEgressDestinationPoolSignalWithSettings(settings *EgressDestinationPoolSettings) Signal {
	return &signalAdapter{
		number: "2.19h", key: "egress-destination-pool", name: "Destination pool availability and catalog refusals",
		probe: egressDestinationPoolProbe{settings: settings},
	}
}

const (
	egressDestinationPoolProbeId   = "runtime/egress-destination-pool"
	egressPoolFetchesMetric        = "urnetwork_egress_probe_pool_fetches_total"
	egressCatalogRefusalsMetric    = "urnetwork_egress_site_catalog_refusals_total"
	egressDestinationPoolPlaybook  = "SIGNALS.md §2.19h"
	egressDestinationPoolTargetKey = "/egress-destination-pool"
)

// The thresholds and bounds of §2.19h.
type EgressDestinationPoolSettings struct {
	// the trailing window the fleet's fetches and refusals are read over
	Window time.Duration
	// the fewest unavailable fetches in the window that alert: a single
	// failed fetch is a blip, while a pool the server cannot serve fails every
	// shard's every pass
	MinUnavailableFetches float64
	// consecutive failing cadences after which an unavailable pool pages
	UnavailablePageSustain int
	// the Taskworker service that runs the passes and the refresh, and the
	// services that serve the pool
	TaskworkerService string
	PoolServices      []string
	// services that serve the pool when placed, with no requirement otherwise
	OptionalPoolServices []string
	// the one fleet read
	FleetReadSettings
	// the most entry names and slot gaps one finding lists
	MaxListed int
}

// The defaults of SIGNALS.md §2.19h.
func DefaultEgressDestinationPoolSettings() *EgressDestinationPoolSettings {
	return &EgressDestinationPoolSettings{
		Window:                 15 * time.Minute,
		MinUnavailableFetches:  2,
		UnavailablePageSustain: 6,
		TaskworkerService:      "taskworker",
		PoolServices:           []string{"api"},
		OptionalPoolServices:   []string{"alt"},
		FleetReadSettings: FleetReadSettings{
			EvaluationLag:    2 * time.Minute,
			Freshness:        3 * time.Minute,
			FutureSkew:       30 * time.Second,
			QueryTimeout:     15 * time.Second,
			MaxResponseBytes: 256 * 1024,
			MaxRows:          1024,
		},
		MaxListed: 12,
	}
}

type egressDestinationPoolProbe struct {
	settings *EgressDestinationPoolSettings
	// nil reads the deployment's catalog
	loadCatalog func() (*controller.ProviderEgressSites, error)
}

func (egressDestinationPoolProbe) id() string             { return egressDestinationPoolProbeId }
func (egressDestinationPoolProbe) tier() string           { return tierWarn }
func (egressDestinationPoolProbe) cadence() time.Duration { return 5 * time.Minute }

// The fleet's fetches and refusals over the window, and the slot coverage of
// the counters they come from.
type egressDestinationPoolFleet struct {
	served      float64
	unavailable float64
	// serve and refresh refusals by "stage/reason"
	refusals map[string]float64
	// every desired Taskworker and pool-serving slot exports both counters
	complete bool
	coverage string
	gateway  string
}

// One instant read: the window's increases, and per-slot freshness of each
// counter on the slots that must export it.
func egressDestinationPoolQuery(environment string, settings *EgressDestinationPoolSettings, taskworkers fleetSlotInventory, servers fleetSlotInventory) string {
	window := promDuration(settings.Window)
	base := "env=" + strconv.Quote(environment)
	parts := []string{
		fleetPart("fetches", "sum by (result) (increase("+egressPoolFetchesMetric+"{"+base+",job="+strconv.Quote(settings.TaskworkerService)+"}["+window+"]))"),
		fleetPart("refusals", "sum by (stage, reason) (increase("+egressCatalogRefusalsMetric+"{"+base+",stage=~\"serve|refresh\"}["+window+"]))"),
	}
	if selector := taskworkers.selector(environment); selector != "" {
		parts = append(parts,
			fleetSlotFreshnessPart("fetches", egressPoolFetchesMetric, selector),
			fleetSlotFreshnessPart("refusals", egressCatalogRefusalsMetric, selector),
		)
	}
	if selector := servers.selector(environment); selector != "" {
		parts = append(parts, fleetSlotFreshnessPart("refusals", egressCatalogRefusalsMetric, selector))
	}
	return strings.Join(parts, " or ")
}

func (self egressDestinationPoolProbe) check(ctx context.Context, env *probeEnv) ([]finding, error) {
	settings := self.settings
	if settings == nil {
		settings = DefaultEgressDestinationPoolSettings()
	}
	loadCatalog := self.loadCatalog
	if loadCatalog == nil {
		loadCatalog = controller.LoadProviderEgressSites
	}
	target := env.cfg.env + egressDestinationPoolTargetKey
	reasons := []string{}

	catalogRead := false
	var refused []controller.ProviderEgressCatalogRefusal
	if sites, err := loadCatalog(); err != nil || sites == nil {
		reasons = append(reasons, "catalog_unavailable")
	} else {
		catalogRead = true
		refused = append([]controller.ProviderEgressCatalogRefusal{}, sites.Refused...)
	}

	var fleet *egressDestinationPoolFleet
	at := fleetEvaluationTime(env.now(), settings.EvaluationLag)
	taskworkers := newFleetSlotInventory(env, []string{settings.TaskworkerService}, nil)
	servers := newFleetSlotInventory(env, settings.PoolServices, settings.OptionalPoolServices)
	out, gateway, err := readFleetMimir(ctx, env, egressDestinationPoolQuery(env.cfg.env, settings, taskworkers, servers), at, settings.FleetReadSettings)
	if err != nil {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}
		reasons = append(reasons, "fleet_unavailable "+observationFailureDetails(err, classifyObservationError(err)))
	} else if samples, err := parseFleetSamples(out, at, settings.FleetReadSettings); err != nil {
		reasons = append(reasons, "fleet_invalid_response")
	} else {
		fleet = &egressDestinationPoolFleet{refusals: map[string]float64{}, gateway: gateway.name}
		fetchResults := map[string]bool{}
		for _, sample := range samples {
			if math.IsNaN(sample.value) || math.IsInf(sample.value, 0) || sample.value < 0 {
				if sample.part == "fetches" || sample.part == "refusals" {
					reasons = append(reasons, "invalid_fleet_sample")
				}
				continue
			}
			switch sample.part {
			case "fetches":
				result := sample.labels["result"]
				if len(sample.labels) != 1 || fetchResults[result] {
					reasons = append(reasons, "invalid_fleet_sample")
					continue
				}
				fetchResults[result] = true
				switch result {
				case "server":
					fleet.served += sample.value
				// builtin is the result a Taskworker built before the
				// rename reports for the same unavailable fetch
				case "unavailable", "builtin":
					fleet.unavailable += sample.value
				default:
					reasons = append(reasons, "unrecognized_fetch_result")
				}
			case "refusals":
				stage, reason := sample.labels["stage"], sample.labels["reason"]
				switch {
				case len(sample.labels) != 2 || (stage != controller.ProviderEgressRefusalStageServe && stage != controller.ProviderEgressRefusalStageRefresh):
					reasons = append(reasons, "invalid_fleet_sample")
				case reason != controller.ProviderEgressRefusalCandidateName && reason != controller.ProviderEgressRefusalCountryConflict && reason != controller.ProviderEgressRefusalCountryInvalid:
					reasons = append(reasons, "unrecognized_refusal_reason")
				default:
					fleet.refusals[stage+"/"+reason] += sample.value
				}
			}
		}
		taskworkerCoverage := newFleetSlotCoverage(taskworkers, samples, []string{"fetches", "refusals"}, at, settings.FleetReadSettings, nil)
		serverCoverage := newFleetSlotCoverage(servers, samples, []string{"refusals"}, at, settings.FleetReadSettings, nil)
		fleet.complete = taskworkerCoverage.full() && serverCoverage.full() && fetchResults["server"]
		fleet.coverage = "taskworker_" + taskworkerCoverage.observed(settings.MaxListed) + " pool_service_" + serverCoverage.observed(settings.MaxListed)
		if !fleet.complete {
			reasons = append(reasons, "slot_coverage_incomplete")
		}
	}
	return evaluateEgressDestinationPool(settings, target, catalogRead, refused, fleet, reasons), nil
}

func evaluateEgressDestinationPool(
	settings *EgressDestinationPoolSettings,
	target string,
	catalogRead bool,
	refused []controller.ProviderEgressCatalogRefusal,
	fleet *egressDestinationPoolFleet,
	reasons []string,
) []finding {
	findings := []finding{}
	window := settings.Window.String()
	fleetText := "fleet=unknown"
	if fleet != nil {
		fleetText = fmt.Sprintf("window=%s served_fetches=%.0f unavailable_fetches=%.0f metrics_gateway=%s %s", window, fleet.served, fleet.unavailable, fleet.gateway, fleet.coverage)
	}
	newFinding := func(class string, sustain int) finding {
		return finding{
			probeId: egressDestinationPoolProbeId, tier: tierWarn, class: class, target: target, sustain: sustain,
			playbook: egressDestinationPoolPlaybook,
		}
	}

	// the pool could not be had
	switch {
	case fleet != nil && settings.MinUnavailableFetches <= fleet.unavailable:
		f := newFinding("egress-destination-pool-unavailable", 2)
		f.pageSustain = settings.UnavailablePageSustain
		share := "unknown"
		if total := fleet.served + fleet.unavailable; 0 < total {
			share = strconv.FormatFloat(100*fleet.unavailable/total, 'f', 1, 64) + "%"
		}
		f.symptom = fmt.Sprintf("%.0f URL and probe passes got no destination pool over %s, %s of the passes that fetched one", fleet.unavailable, window, share)
		f.mechanism = "Every pass starts by fetching the destination pool from the api route or the in-process operator. There is no built-in table: a pass without a pool measures no URL, so while the route answers 500 or serves a pool the prober refuses, the shards measure nothing and every provider's URL evidence ages out of the eight-hour window, emptying the Quality and Speed buckets."
		f.baseline = fmt.Sprintf("Fewer than %.0f unavailable pool fetches over %s; every pass loads the served pool.", settings.MinUnavailableFetches, window)
		f.observed = fleetText + " unavailable_share=" + share
		f.evidence = "The Taskworker urnetwork_egress_probe_pool_fetches_total increases by result (server, unavailable; builtin from a Taskworker built before the rename), summed over the env's Taskworker slots through one loopback Mimir read."
		f.context = "A whole-catalog parse failure in qualityprobe.yml (a destination or candidate the prober cannot run), a database failure under the pool route, or an api or alt build that refuses a pool for an overlap all produce this. A colliding country site alone never does on a build that carries §2.19h's fail-small pool."
		f.action = "Read the api's and alt's [pegd] \"could not build the egress destination pool\" lines and the taskworker's \"URL probe destination catalog\" pass errors for the cause. Repair qualityprobe.yml through the ordinary configuration release if it no longer parses; do not substitute a hand-made pool."
		f.verify = "Unavailable fetches fall under the threshold for a window, served fetches advance, and URL outcomes resume in §2.19f."
		findings = append(findings, f)
	case fleet != nil && fleet.complete:
		findings = append(findings, healthyFinding(egressDestinationPoolProbeId, tierWarn, "egress-destination-pool-unavailable", target))
	}

	// entries kept out of the pool
	fleetRefusals := 0.0
	refusalText := []string{}
	if fleet != nil {
		keys := make([]string, 0, len(fleet.refusals))
		for key := range fleet.refusals {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			fleetRefusals += fleet.refusals[key]
			if 0 < fleet.refusals[key] {
				refusalText = append(refusalText, fmt.Sprintf("%s:%.0f", key, fleet.refusals[key]))
			}
		}
	}
	switch {
	case 0 < len(refused) || 0 < fleetRefusals:
		byReason := map[string]int{}
		entries := []string{}
		for _, refusal := range refused {
			byReason[refusal.Reason]++
			list := "candidates"
			if refusal.Country != "" {
				list = refusal.Country
			}
			entries = append(entries, list+"/"+refusal.Name)
		}
		reasonText := []string{}
		for _, reason := range []string{controller.ProviderEgressRefusalCandidateName, controller.ProviderEgressRefusalCountryConflict, controller.ProviderEgressRefusalCountryInvalid} {
			reasonText = append(reasonText, fmt.Sprintf("%s:%d", reason, byReason[reason]))
		}
		listed := strings.Join(entries, ",")
		if settings.MaxListed < len(entries) {
			listed = strings.Join(entries[:settings.MaxListed], ",") + fmt.Sprintf(",and %d more", len(entries)-settings.MaxListed)
		}
		if listed == "" {
			listed = "none"
		}
		fleetRefusalText := "none"
		if 0 < len(refusalText) {
			fleetRefusalText = strings.Join(refusalText, ",")
		}
		f := newFinding("egress-destination-catalog-refused", 1)
		f.symptom = fmt.Sprintf("qualityprobe.yml has %d entries the parse refuses, and the fleet kept %.0f more out of a served pool or a promotion over %s", len(refused), fleetRefusals, window)
		f.mechanism = "The country lists must be disjoint from every destination and candidate by name and by registrable domain, and a candidate may not take a destination's name, or the prober refuses the whole pool. The server keeps each colliding or unrunnable entry out alone -- the parse refuses it, a served pool drops a country site that collides with a served entry, and the refresh holds back a candidate that would -- so measurement continues, but what is measured is not the catalog that was written."
		f.baseline = "The configured catalog refuses no entry, and no serve or refresh refusal is counted over the window."
		f.observed = fmt.Sprintf("catalog_refused=%d by_reason=%s entries=%s fleet_refusals=%s %s", len(refused), strings.Join(reasonText, ","), listed, fleetRefusalText, fleetText)
		f.evidence = "The configured qualityprobe.yml through the API's own parser (controller.LoadProviderEgressSites), and the urnetwork_egress_site_catalog_refusals_total serve and refresh increases summed over the env's processes. Entry names and country codes only; the [egresssites] log line on the api and taskworker names what each entry collides with."
		f.context = "candidate_name: a candidate repeating a destination's name; the destination's contract is served, and a site retired for good returns as a candidate alone with a higher revision. country_conflict: a country site sharing a name or registrable domain with a destination or candidate. country_invalid: a country entry or list the prober could not run. A serve or refresh refusal with a clean catalog means the served rows and the parsed catalog have come apart."
		f.action = "Remove or replace the named entries in config/<env>/qualityprobe.yml and release it through the ordinary configuration release; check the file first with the opt-in readbacks (FP2_QUALITY_CATALOG_PATH, TestQualityProbeCatalogFromConfiguredPath*)."
		f.verify = "The next cadence reads the catalog with no refusal, and the serve and refresh refusal increases return to zero."
		findings = append(findings, f)
	case catalogRead && fleet != nil && fleet.complete:
		findings = append(findings, healthyFinding(egressDestinationPoolProbeId, tierWarn, "egress-destination-catalog-refused", target))
	}

	if 0 < len(reasons) {
		unique := []string{}
		seen := map[string]bool{}
		for _, reason := range reasons {
			if !seen[reason] {
				seen[reason] = true
				unique = append(unique, reason)
			}
		}
		f := newFinding("egress-destination-pool-unobservable", 2)
		f.symptom = "Destination pool availability or catalog refusals cannot be fully established"
		f.mechanism = "The configured catalog, the Mimir read, or a desired Taskworker or pool-serving slot's pool-fetch or refusal counter could not be read, so a failing pool fetch or a refused entry cannot be ruled out from what was read."
		f.baseline = "The catalog parses, and every desired Taskworker slot exports fresh pool-fetch and refusal counters and every desired api slot a fresh refusal counter, with served fetches present."
		f.observed = "visibility_reason=" + strings.Join(unique, ";") + " " + fleetText
		f.evidence = "Desired slots come from active services.yml placement, not from surviving series. Findings proven from what was read are reported beside this."
		f.context = "A process built before §2.19h exports no refusal counter, so a fleet partly on an older build stays unobservable until every slot carries it."
		f.action = "Restore the configuration resource or the Mimir path, or finish the rollout of the api, alt and taskworker build that exports the counters, and rerun."
		f.verify = "Every source reads on two cadences."
		findings = append(findings, f)
	}
	return findings
}
