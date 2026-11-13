package monitor

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/url"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	providerCountRange     = "5m"
	providerCountFreshness = 90 * time.Second
	providerCountPageMin   = 20.0
	providerCountWarnMin   = 50.0
	// the label value the query tags the empty-outcome explanations with
	providerCountEmptyPart = "empty"
)

// SIGNALS.md §2.9a maps to signal_provider_count.go and
// signal_provider_count_test.go. It measures completed API responses, not a
// Redis score-cache surrogate, so a real empty provider list remains visible.
func NewProviderCountSignal() Signal {
	return &signalAdapter{number: "2.9a", key: "provider-count", name: "User-visible provider-count degradation", probe: &providerCountProbe{recoveryRuns: map[providerCountRecoveryKey]int{}}}
}

// The two classes whose incidents wait on their own cohorts' recovery
// evidence.
var providerCountMeasuredClasses = []string{"provider-count-effective-empty", "provider-count-small-list"}

// The consecutive qualified executions a reporting cohort shows before its
// class stops waiting on it: the notifier's two complete absent runs.
const providerCountRecoveryRuns = 2

// One cohort an open incident of a measured class was reported for.
type providerCountRecoveryKey struct {
	measuredClass string
	key           providerCountKey
}

// Remembers, between executions, every cohort a measured class was reported
// for until that cohort's own evidence shows recovery. The notifier matches
// an incident on class and target without the cohort, so this memory is what
// lets a cohort below the request floor hold only the incidents it reported.
type providerCountProbe struct {
	stateLock sync.Mutex
	// each reporting cohort and its consecutive qualified executions since
	// it last reported the class
	recoveryRuns map[providerCountRecoveryKey]int
}

func (*providerCountProbe) id() string             { return "mimir/provider-count" }
func (*providerCountProbe) tier() string           { return tierPage }
func (*providerCountProbe) cadence() time.Duration { return time.Minute }

// The zero-share cohorts count capability-blind requests only. A request
// that keeps only modern or only post-quantum providers (capability modern,
// post_quantum or modern_post_quantum, FP2-MODERN.md) can rightly answer
// short where the capable supply is small, so its answers never count toward
// an empty or small list. The negative match keeps capability none and the
// series without the label, which older processes export.
const providerCountCapabilitySelector = `capability!~"modern.*|post_quantum"`

// The result bands, and with them, read at the same instant and tagged apart,
// the empty answers' explanations: urnetwork_findproviders2_empty_outcomes_total
// counts every zero-band answer again under the same request-class labels,
// with the requested target's kind, the target country of a supply-empty
// country or region, and the request's terminal selection reason. API
// processes that predate it export no explanations, and their zeros stay
// unexplained. The admitted supply the taskworker's score export measured per
// country and family (model network_client_score_supply.go) comes with them:
// each process's latest measurement and its time, the lookback maximum and
// the oldest measurement within it, and the fleet totals with their longer
// lookback maximum.
func providerCountQuery(environment string) string {
	environmentSelector := `env=` + strconv.Quote(environment)
	selector := environmentSelector + `,` + providerCountCapabilitySelector
	tag := func(expression, part string) string {
		return `label_replace(` + expression + `,"monitor_part",` + strconv.Quote(part) + `,"","")`
	}
	admitted := `urnetwork_client_score_admitted_providers{` + environmentSelector + `}`
	measured := `urnetwork_client_score_supply_measured_timestamp_seconds{` + environmentSelector + `}`
	fleet := `urnetwork_client_score_admitted_providers_fleet{` + environmentSelector + `}`
	return strings.Join([]string{
		`sum by (ip_family,location_kind,caller_country,rank_mode,force_minimum,result_count) (increase(urnetwork_findproviders2_outcomes_total{` + selector + `}[` + providerCountRange + `]))`,
		tag(`sum by (ip_family,location_kind,caller_country,rank_mode,force_minimum,target_kind,target_country,reason) (increase(urnetwork_findproviders2_empty_outcomes_total{`+selector+`}[`+providerCountRange+`]))`, providerCountEmptyPart),
		tag(admitted, "supply"),
		tag(measured, "supply_time"),
		tag(`max by (country,ip_family) (max_over_time(`+admitted+`[`+providerCountSupplyLookback+`]))`, "supply_lookback"),
		tag(`min(min_over_time(`+measured+`[`+providerCountSupplyLookback+`]))`, "supply_coverage"),
		tag(fleet, "fleet"),
		tag(`max by (ip_family) (max_over_time(`+fleet+`[`+providerCountFleetLookback+`]))`, "fleet_lookback"),
	}, ` or `)
}

type providerCountKey struct {
	ipFamily      string
	locationKind  string
	callerCountry string
	rankMode      string
	forceMinimum  string
}

func (k providerCountKey) frame() string {
	return strings.Join([]string{k.ipFamily, k.locationKind, k.callerCountry, k.rankMode}, "/")
}

type providerCountCohort struct {
	key   providerCountKey
	bands map[string]float64
	// the cohort's zeros its request explains: the exits its window already
	// holds were every candidate, or it asked for none
	requestExplained float64
	// the cohort's zeros from a city or region the score export holds no
	// admitted provider for
	targetSupply float64
	// the cohort's zeros from a country whose admitted supply in the
	// cohort's family is below the well-supplied floor and was not lost
	unsupplied float64
	// how the response's explanations were read: absent, joined or invalid
	explanations string
	// how the response's supply was read: absent, joined, invalid or stale
	supply string
	// the supply verdict of every target country the cohort answered empty
	// from supply
	supplyTargets map[providerCountSupplyKey]*providerCountSupplyTarget
	// the fleet's IPv6 supply fell below half its lookback maximum
	fleetV6Dropped bool
}

// One target country of a cohort's supply-empty answers.
type providerCountSupplyTarget struct {
	zeros    float64
	current  float64
	lookback float64
	verdict  string
}

// The selection reasons (model network_client_selection_metrics.go) of a zero
// the request itself explains. filtered_destinations: ExcludeDestinations
// removed every candidate, and the standard window passes there only the
// healthy exits it already holds, so the answer is a refill finding nothing
// new. intentional_zero: it asked for zero. ExcludeClientIds also carries the
// client's runtime removals of providers that failed it, so a zero it explains
// (filtered_client_ids, or filtered_explicit_mixed and the schema-1
// filtered_explicit with it) can be a user who has nothing and still counts.
var providerCountRequestExplainedReasons = []string{"filtered_destinations", "intentional_zero"}

// A city's or region's cache_empty zero is target supply, not a platform
// empty: most cities and regions hold one or two providers, so the export
// legitimately writes them without an admitted provider while their supply
// fails the shared gates or has left. A region whose country lost all of its
// supply pages (joinSupply). A country pages unless its own admitted supply
// explains it; a group, best-available, mixed or unknown target the export
// wrote empty still counts toward the PAGE.

// Every zero-reason and target-kind value the producer can export; anything
// else makes the explanations untrusted.
var providerCountZeroReasons = []string{"intentional_zero", "direct_excluded", "no_specs", "unresolved_target", "cache_page_gap", "cache_missing", "backfill_unavailable", "filtered_capability", "cache_empty", "unsupported_rank", "eligible_not_selected", "filtered_hard", "filtered_network", "filtered_family", "filtered_explicit", "filtered_client_ids", "filtered_destinations", "filtered_explicit_mixed", "filtered_mixed", "unclassified_zero"}
var providerCountTargetKinds = []string{"none", "location_unknown", "country", "region", "city", "group", "best_available", "direct", "mixed"}

// The cohort's explained zeros, clamped to its zeros: each explanation was
// counted at the same response as a zero, so their sum cannot exceed the
// zeros, but increase() extrapolates the two counters apart slightly.
func (self providerCountCohort) explained() (requestExplained, targetSupply, unsupplied float64) {
	zero := self.bands["0"]
	requestExplained = min(self.requestExplained, zero)
	targetSupply = min(self.targetSupply, zero-requestExplained)
	unsupplied = min(self.unsupplied, zero-requestExplained-targetSupply)
	return
}

// The cohort's answers the thresholds judge, of its total, and the zeros among
// them: every answer but the explained zeros.
func (self providerCountCohort) considered(total float64) (considered, unexplained float64) {
	requestExplained, targetSupply, unsupplied := self.explained()
	explained := requestExplained + targetSupply + unsupplied
	return total - explained, self.bands["0"] - explained
}

func (self *providerCountProbe) check(ctx context.Context, env *probeEnv) ([]finding, error) {
	metricHosts := env.cfg.hostsWithRole("services")
	if len(metricHosts) == 0 {
		return nil, fmt.Errorf("provider count: no services host is configured for the loopback Mimir query")
	}
	queryURL := "http://127.0.0.1:3100/prometheus/api/v1/query?query=" + url.QueryEscape(providerCountQuery(env.cfg.env))
	out, metricHost, err := mimirShellFirstServiceGatewayWithRejection(ctx, env.runner, metricHosts, nil, "curl -fsS --max-time 15 '"+queryURL+"'", mimirShellResponseMaxBytes, func(body string) error {
		_, sourceErr := parseProviderCountCohorts(body, env.now())
		return sourceErr
	})
	if contextErr := ctx.Err(); contextErr != nil {
		// A retirement after a completed read still retains only a safe
		// parser failure; successful cohorts never become findings.
		if err == nil {
			_, sourceErr := parseProviderCountCohorts(out, env.now())
			return nil, mimirShellParentError(contextErr, sourceErr)
		}
		return nil, err
	}
	if findings, overflow := mimirShellOverflowFindings(err, "mimir/provider-count"); overflow && ctx.Err() == nil {
		return findings, nil
	}
	if err != nil {
		return nil, fmt.Errorf("provider count: query Mimir through service gateways: %w", err)
	}
	cohorts, err := parseProviderCountCohorts(out, env.now())
	if err != nil {
		return nil, err
	}
	findings, assessed := self.assess(ctx, cohorts, metricHost.name)
	if !assessed {
		// a retired execution commits no recovery memory and no findings
		return nil, ctx.Err()
	}
	return findings, nil
}

// Judges one successful response and advances the recovery memory. Returns
// false, committing nothing, when the execution was retired meanwhile.
func (self *providerCountProbe) assess(ctx context.Context, cohorts []providerCountCohort, metricHost string) ([]finding, bool) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	findings := make([]finding, 0, len(cohorts))
	observedDiscovery := false
	// some ordinary cohort has the requests to judge: without one the
	// response is missing or partial, and no incident may recover from it
	recoverySample := false
	// per measured class, the cohorts that report it now and the cohorts
	// whose own requests show it no longer holds
	reporting := map[providerCountRecoveryKey]bool{}
	qualified := map[providerCountRecoveryKey]bool{}
	for _, cohort := range cohorts {
		if cohort.key.forceMinimum != "false" {
			continue
		}
		total := 0.0
		for _, value := range cohort.bands {
			total += value
		}
		// A zero the request or its narrow target explains is no platform
		// empty: it leaves both the zeros and the denominator the thresholds
		// read, so neither can dilute the cohort's remaining answers.
		considered, unexplained := cohort.considered(total)
		cohortFindings := []finding{}
		unclassified := false
		small := unexplained + cohort.bands["1-2"]
		if considered >= providerCountPageMin && unexplained/considered >= .8 {
			cohortFindings = append(cohortFindings, providerCountFinding(cohort, tierPage, metricHost, "effective-empty"))
		} else {
			if considered >= providerCountWarnMin && small/considered >= .5 {
				cohortFindings = append(cohortFindings, providerCountFinding(cohort, tierWarn, metricHost, "small-list"))
			} else if cohort.key.locationKind != "direct" && considered >= providerCountPageMin && considered < providerCountWarnMin && small/considered >= .5 {
				// Falling below the empty-list PAGE ratio does not establish
				// recovery when a traffic-bearing cohort remains mostly small.
				// The sample is below the ordinary WARN floor; retain only an
				// intent-unknown observation, never a lower-threshold scarcity
				// rule.
				cohortFindings = append(cohortFindings, providerCountFinding(cohort, tierWarn, metricHost, "small-list-unclassified"))
				unclassified = true
			}
			_, targetSupply, unsupplied := cohort.explained()
			// Explained supply gaps never page, but one at product scale
			// stays visible: at the WARN floor a single client's refill loop,
			// four requests a minute per rank mode, cannot raise it.
			if cohort.key.locationKind != "direct" && total >= providerCountWarnMin && targetSupply/total >= .5 {
				cohortFindings = append(cohortFindings, providerCountTargetSupplyFinding(cohort, metricHost))
			}
			// A cohort that would page if its countries without supply
			// counted warns instead, carrying their supply.
			if withUnsupplied := considered + unsupplied; 0 < unsupplied && providerCountPageMin <= withUnsupplied && (unexplained+unsupplied)/withUnsupplied >= .8 {
				cohortFindings = append(cohortFindings, providerCountCountryUnsuppliedFinding(cohort, metricHost))
			}
		}
		findings = append(findings, cohortFindings...)
		// Direct specs append caller-chosen IDs without running discovery.
		// Neither their natural small size nor their absence proves capacity,
		// so they report no measured class and supply no recovery evidence.
		if cohort.key.locationKind == "direct" {
			continue
		}
		if total > 0 {
			observedDiscovery = true
		}
		// At least the PAGE's request floor, in a shape the reducer can
		// judge: the cohort's own evidence. Explained answers count, since
		// they too show the cohort no longer meets the class.
		sufficient := providerCountPageMin <= total && !unclassified
		if sufficient {
			recoverySample = true
		}
		for _, class := range providerCountMeasuredClasses {
			recoveryKey := providerCountRecoveryKey{measuredClass: class, key: cohort.key}
			for _, f := range cohortFindings {
				if f.class == class {
					reporting[recoveryKey] = true
				}
			}
			if sufficient && !reporting[recoveryKey] {
				qualified[recoveryKey] = true
			}
		}
	}
	// A cohort that reports a class waits until its own qualified streak
	// completes. One below the floor, in the intent-unknown shape or missing
	// from this response holds its class and restarts the streak. A cohort
	// that never reported the class holds nothing.
	recoveryRuns := map[providerCountRecoveryKey]int{}
	held := map[string]bool{}
	for recoveryKey := range reporting {
		recoveryRuns[recoveryKey] = 0
	}
	for recoveryKey, runs := range self.recoveryRuns {
		switch {
		case reporting[recoveryKey]:
		case qualified[recoveryKey]:
			if runs+1 < providerCountRecoveryRuns {
				recoveryRuns[recoveryKey] = runs + 1
			}
		default:
			recoveryRuns[recoveryKey] = 0
			held[recoveryKey.measuredClass] = true
		}
	}
	if ctx.Err() != nil {
		return nil, false
	}
	self.recoveryRuns = recoveryRuns
	for _, class := range providerCountMeasuredClasses {
		// Unknown or excluded-only responses retain measured history without
		// raising a replacement incident. These exact class/target holds do
		// not suppress current PAGE/WARN findings or change their thresholds.
		if held[class] || !recoverySample {
			findings = append(findings, finding{probeId: "mimir/provider-count", class: class, target: "api-fleet", clearHold: true})
		}
	}
	if len(findings) == 0 && observedDiscovery {
		return []finding{healthyFinding("mimir/provider-count", tierPage, "provider-count-degraded", "api-fleet")}, true
	}
	return findings, true
}

// The cohort's bounded counts as every finding reports them: the raw response
// shape, then how its zeros were explained and the answers the thresholds
// judged.
type providerCountCounts struct {
	total, zero, small, zeroPercent, smallPercent float64
	considered                                    float64
	observed                                      string
}

func newProviderCountCounts(cohort providerCountCohort, metricHost string) providerCountCounts {
	counts := providerCountCounts{zero: cohort.bands["0"]}
	for _, value := range cohort.bands {
		counts.total += value
	}
	counts.small = counts.zero + cohort.bands["1-2"]
	counts.zeroPercent = 100 * counts.zero / counts.total
	counts.smallPercent = 100 * counts.small / counts.total
	requestExplained, targetSupply, unsupplied := cohort.explained()
	considered, unexplained := cohort.considered(counts.total)
	counts.considered = considered
	counts.observed = fmt.Sprintf("completed_requests=%.0f zero=%.0f zero_percent=%.1f zero_or_1_2=%.0f zero_or_1_2_percent=%.1f ip_family=%s location_kind=%s caller_country=%s rank_mode=%s force_minimum=false range=%s metrics_gateway=%s zero_explained_by_request=%.0f zero_explained_by_target_supply=%.0f zero_explained_by_country_supply=%.0f zero_unexplained=%.0f considered_requests=%.0f zero_explanations=%s country_supply=%s", counts.total, counts.zero, counts.zeroPercent, counts.small, counts.smallPercent, cohort.key.ipFamily, cohort.key.locationKind, cohort.key.callerCountry, cohort.key.rankMode, providerCountRange, metricHost, requestExplained, targetSupply, unsupplied, unexplained, considered, cohort.explanations, cohort.supply)
	if targets := cohort.supplyTargetSummary(); targets != "" {
		counts.observed += " country_supply_targets=" + targets
	}
	if cohort.fleetV6Dropped {
		counts.observed += " fleet_v6_dropped=true"
	}
	return counts
}

// At most eight target countries by their zeros, each with its current and
// lookback supply and verdict.
func (self providerCountCohort) supplyTargetSummary() string {
	keys := make([]providerCountSupplyKey, 0, len(self.supplyTargets))
	for key := range self.supplyTargets {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool {
		a, b := self.supplyTargets[keys[i]], self.supplyTargets[keys[j]]
		if a.zeros != b.zeros {
			return a.zeros > b.zeros
		}
		return keys[i].country+keys[i].family < keys[j].country+keys[j].family
	})
	parts := []string{}
	for _, key := range keys[:min(len(keys), 8)] {
		target := self.supplyTargets[key]
		if target.verdict == providerCountSupplyUnknown {
			parts = append(parts, fmt.Sprintf("%s/%s:%s", key.country, key.family, target.verdict))
			continue
		}
		parts = append(parts, fmt.Sprintf("%s/%s:now=%.0f:max_%s=%.0f:%s", key.country, key.family, target.current, providerCountSupplyLookback, target.lookback, target.verdict))
	}
	return strings.Join(parts, ",")
}

func providerCountFinding(cohort providerCountCohort, tier, metricHost, class string) finding {
	counts := newProviderCountCounts(cohort, metricHost)
	total, zeroPercent, smallPercent, observed := counts.total, counts.zeroPercent, counts.smallPercent, counts.observed
	if cohort.key.locationKind == "direct" {
		return finding{
			probeId: "mimir/provider-count", tier: tierWarn, class: "provider-count-direct-unclassified",
			target: "api-fleet", frame: cohort.key.frame(), sustain: 1,
			symptom:   fmt.Sprintf("Direct-provider response intent is unclassified for %s: %.0f completed requests, zero=%.1f%%, zero-or-1-2=%.1f%%", cohort.key.frame(), total, zeroPercent, smallPercent),
			mechanism: "The direct request class has no location, group or best-available selector. FindProviders2 appends each explicit ClientId unless that final destination is explicitly or hard-egress excluded; it does not run discovery score, reliability, network-only or IP-family filtering. An empty specification or all-excluded explicit destinations can return zero, and one or two requested IDs can legitimately return one or two. This count does not prove supply depletion or a provider failure.",
			baseline:  "Direct responses are interpreted against requested and nonexcluded explicit IDs, not discovery-pool size. The existing cohort thresholds identify material response traffic needing request-intent evidence; they do not establish a scarcity baseline.",
			observed:  observed,
			evidence:  "The completed-response metric retains only bounded request labels and result-count bands. It has no specification count, exclusion count, expected explicit-result count, client, network, provider, location ID, address or request identifier.",
			context:   "Intent remains unknown, not healthy. The default quality label does not prove quality ranking ran, and caller_country=unknown is expected when the direct branch skips caller-country lookup. Do not attribute this cohort to probe admission, DoH, TLS, quality gates or general capacity. A returned explicit ID is not proof that the destination is available.",
			action:    "Verify the emitting API artifact and correlate naturally occurring direct requests with bounded counts of requested explicit IDs, nonexcluded final IDs and returned entries at the same request boundary. Keep any identifiers private. Distinguish empty or intentionally excluded requests from request-construction failures; do not clear score keys, relax gates or fabricate traffic.",
			verify:    "Establish request intent and the expected nonexcluded explicit-result count before judging direct responses. Require independent traffic-bearing discovery cohorts and successful routes for supply recovery; disappearance of this warning or a quiet direct cohort is not recovery.",
			playbook:  "SIGNALS.md §2.9a and §5.9",
		}
	}
	if class == "small-list-unclassified" {
		return finding{
			probeId: "mimir/provider-count", tier: tierWarn, class: "provider-count-small-list-unclassified",
			target: "api-fleet", frame: cohort.key.frame(), sustain: 1,
			symptom:   fmt.Sprintf("Small provider-list response intent is unclassified for %s: %.0f completed responses, zero=%.1f%%, zero-or-1-2=%.1f%%", cohort.key.frame(), total, zeroPercent, smallPercent),
			mechanism: "The cohort has completed traffic and at least half its responses contain zero, one or two entries, but its sample is below the ordinary small-list WARN floor. This records response shape only; request intent and the established baseline remain unknown. It does not establish supply scarcity, provider failure or recovery.",
			baseline:  "Retain a nonpaging intent-unknown observation for at least 20 but fewer than 50 completed ordinary discovery responses in five minutes with at least 50% zero-or-one-to-two entries. The existing effective-empty PAGE takes precedence; the ordinary provisional WARN still requires 50 completed responses.",
			observed:  observed + " qualification=unclassified sample_state=below_warn_min request_intent=unknown baseline_state=unobserved completed_requests_for_threshold=" + strconv.FormatFloat(counts.considered, 'g', -1, 64),
			evidence:  "The completed-response counter omits ForceCount, requested/effective Count, target location and caller exclusions. ForceMinimum=false does not exclude an intentionally capped request. Caller country is not target/provider country, and repeated calls are not a unique-caller denominator.",
			context:   "A zero share falling below 80% can remove the PAGE while all responses remain small. That threshold movement is not recovery. Direct explicit-destination requests retain their separate interpretation, and ForceMinimum diagnostics remain excluded. The same-label cohort can mix target restrictions, count caps and exclusions; positive returned entries do not prove successful routes.",
			action:    "Retain the exact response counts and inspect already-available bounded request-intent evidence before attributing a cause. Correlate independent cache completeness, eligibility, egress and route results. Do not relax gates, clear score keys or manufacture traffic to cross an alert threshold.",
			verify:    "Establish request intent and sufficient same-cohort baseline evidence separately from successful routes. Falling below an alert ratio or volume floor, disappearance of a class, and a quiet cohort are not recovery evidence.",
			playbook:  "SIGNALS.md §2.9a and §5.9",
		}
	}
	symptom := fmt.Sprintf("provider lists are materially degraded for %s: %.0f completed ordinary requests, zero=%.1f%%, zero-or-1-2=%.1f%%", cohort.key.frame(), total, zeroPercent, smallPercent)
	if class == "effective-empty" {
		symptom = fmt.Sprintf("provider lists are effectively empty for %s: %.0f completed ordinary requests, zero=%.1f%%", cohort.key.frame(), total, zeroPercent)
	}
	if counts.considered < total {
		symptom += fmt.Sprintf(", judged on the %.0f answers neither the request nor a narrow empty target explains", counts.considered)
	}
	f := finding{
		probeId: "mimir/provider-count", tier: tier, class: "provider-count-" + class,
		target: "api-fleet", frame: cohort.key.frame(), sustain: 1,
		symptom:   symptom,
		mechanism: "This counts the final FindProviders2 connection-selection response after network-only, IP-family, and explicit-destination exclusion filters. A nonempty Redis score cache can therefore remain healthy while the product-visible list is empty or materially small for this request cohort. The app location picker uses different endpoints and is independently covered by provider-picker (§2.9b); this counter cannot prove picker health.",
		baseline:  "At least 20 ordinary discovery requests in five minutes with 80% zero providers is an effective-empty page; this absolute page does not require a learned baseline. Zeros explained by a window refill (filtered_destinations), a zero-count request or an empty city or region target leave both counts first.",
		observed:  observed,
		evidence:  "Mimir aggregates only the API counter's fixed-vocabulary request class and result-count bands, joined on the same labels to the empty answers' fixed target kind and selection reason. Both are incremented at the completed response boundary; no client, network, provider, location ID, address, request identifier, or provider list leaves the API.",
		context:   "Direct explicit-destination cohorts are reported separately as request-intent unknown, and ForceMinimum diagnostics are excluded. A refill zero whose every candidate was an exit the window already holds, a zero-count request, or a city or region the score export holds no admitted provider for does not count; a country, group, best-available, mixed or unknown target the export wrote empty still does, and so does a zero ExcludeClientIds explains, which can be the client's own removals of providers that failed it. Network-only access outside its network and restrictive location or group requests can still legitimately make a discovery cohort small. A returned candidate can still fail later, so pair this with missing-origin, stale-destination, egress admission, and end-to-end connection success. Missing outcome telemetry is a cannot-observe condition, never a healthy zero, and absent or untrusted explanations explain nothing.",
		action:    "Treat repeated empty connection-selection lists as a product availability failure. Identify whether the reported screen is the location picker (§2.9b) or FindProviders2; caller country is not selected target country, and ForceCount/requested Count and exclusions remain unobserved. Correlate the same cohort with §2.9 cache population, §2.15 reliability integrity, §2.19a egress admission, and §2.17/§2.18 lifecycle failures. Restore the owning selection or lifecycle boundary; do not clear score keys, relax gates, or fabricate requests to make the list count recover.",
		verify:    "Two consecutive complete five-minute windows show every materially used ordinary cohort below the alert band, §2.9 current cache documents remain complete, and successful provider routes recover without manual state changes.",
		playbook:  "SIGNALS.md §2.9a and §5.9",
	}
	if class == "small-list" {
		f.symptom = fmt.Sprintf("Provisional five-minute small provider lists for %s: %.0f completed ordinary requests, zero=%.1f%%, zero-or-1-2=%.1f%%", cohort.key.frame(), total, zeroPercent, smallPercent)
		f.mechanism += " This fixed five-minute observation has no established rolling baseline and does not prove a regression from normal request behavior."
		f.baseline = "The current reducer retains a provisional WARN at 50 completed requests with at least 50% zero or one-to-two responses in five minutes. The desired catalog rule requires 15 minutes and an established materially lower cohort baseline; that qualification is not implemented."
		f.observed += " qualification=provisional baseline_state=unobserved"
		f.action = "Preserve this bounded observation and establish the same cohort's 15-minute distribution and pre-incident baseline before diagnosing a regression. Correlate request restrictions and independent cache, reliability, egress and route controls; do not clear score keys, relax gates or fabricate traffic from this provisional count."
		f.verify = "Require the catalog's separate 15-minute and established-baseline authority before claiming a qualified regression or its recovery. Clearing the provisional five-minute WARN alone does not satisfy that contract; retain independent traffic-bearing discovery and successful-route checks."
	}
	return f
}

// A traffic-bearing cohort whose answers are mostly a narrow target without
// admitted supply: a supply gap, not a selection failure, and never a page.
func providerCountTargetSupplyFinding(cohort providerCountCohort, metricHost string) finding {
	counts := newProviderCountCounts(cohort, metricHost)
	_, targetSupply, _ := cohort.explained()
	return finding{
		probeId: "mimir/provider-count", tier: tierWarn, class: "provider-count-target-supply-empty",
		target: "api-fleet", frame: cohort.key.frame(), sustain: 1,
		symptom:   fmt.Sprintf("Requested cities or regions hold no admitted provider for %s: %.0f of %.0f completed ordinary requests answered empty from a target without supply", cohort.key.frame(), targetSupply, counts.total),
		mechanism: "These empty answers carry the selection reason cache_empty for a city or region target: the score export published the target, so it still has connected source rows or lost its last one within the cache ttl, but no admitted provider, because none passes the shared reliability, URL and hard-exclusion gates. FindProviders2 answered correctly for the target the request named; this is not a selection or cache failure, and these zeros are left out of the effective-empty PAGE.",
		baseline:  "At least 50 completed ordinary requests in five minutes, at least half of them answered empty by a city or region target without admitted supply. A single client's refill loop for one such target, four requests a minute per rank mode, stays below this floor.",
		observed:  counts.observed,
		evidence:  "urnetwork_findproviders2_empty_outcomes_total counts each empty answer again under the outcome counter's request-class labels with its fixed target kind and selection reason. No target identity, client, network, provider, address or request identifier leaves the API, so the targets themselves are not named here.",
		context:   "Most cities and regions hold one or two providers, so a target can lose all of its admitted supply while every country around it keeps answering; caller country is not the requested target's country. A country, group, best-available, mixed or unknown target the export wrote empty is not this class and still counts toward the effective-empty PAGE.",
		action:    "Check whether the requested targets are still offered to users (§2.9b picker) and why their providers fail admission: §2.15 reliability and §2.19a egress admission. Whether an app or the platform should fall back to the target's region or country is a product decision. Do not relax gates, clear score keys or widen targets to make answers nonempty.",
		verify:    "The cohort's share of empty answers from narrow targets without supply falls below half for two consecutive complete five-minute windows, or the targets' providers are admitted again.",
		playbook:  "SIGNALS.md §2.9a and §5.9",
	}
}

func parseProviderCountCohorts(raw string, now time.Time) ([]providerCountCohort, error) {
	var response struct {
		mimirInstantResponse
		Infos []string `json:"infos"`
	}
	if err := json.Unmarshal([]byte(raw), &response); err != nil {
		return nil, fmt.Errorf("provider count: decode Mimir response")
	}
	if response.Status != "success" || response.Data.ResultType != "vector" {
		return nil, fmt.Errorf("provider count: Mimir response status or result type is unsupported")
	}
	// These aggregate ratios need the whole response, including their
	// denominators. An annotation leaves every returned cohort unqualified.
	if len(response.Warnings) != 0 || len(response.Infos) != 0 {
		return nil, fmt.Errorf("provider count: invalid response annotations")
	}
	byKey := map[providerCountKey]map[string]float64{}
	explanationRows := []int{}
	supplyRows := map[string][]int{}
	untrustedParts := false
	for index, series := range response.Data.Result {
		if part, tagged := series.Metric["monitor_part"]; tagged {
			switch {
			case part == providerCountEmptyPart:
				explanationRows = append(explanationRows, index)
			case slices.Contains(providerCountSupplyParts, part):
				supplyRows[part] = append(supplyRows[part], index)
			default:
				// a part this probe never asked for: neither optional
				// source can be trusted, and the bands alone decide
				untrustedParts = true
			}
			continue
		}
		key, band, err := providerCountSeriesKey(series.Metric)
		if err != nil {
			return nil, err
		}
		observedAt, value, err := mimirInstantValue(series.Value)
		if err != nil {
			return nil, fmt.Errorf("provider count: parse Mimir outcome value")
		}
		age := now.Sub(observedAt)
		if age > providerCountFreshness || age < -30*time.Second {
			return nil, fmt.Errorf("provider count: stale outcome sample for %s age=%s", key.frame(), age.Round(time.Second))
		}
		if math.IsNaN(value) || math.IsInf(value, 0) || value < 0 {
			return nil, fmt.Errorf("provider count: invalid outcome value for %s", key.frame())
		}
		bands := byKey[key]
		if bands == nil {
			bands = map[string]float64{}
			byKey[key] = bands
		}
		if _, duplicate := bands[band]; duplicate {
			return nil, fmt.Errorf("provider count: duplicate result band %s for %s", band, key.frame())
		}
		bands[band] = value
	}
	if len(byKey) == 0 {
		return nil, fmt.Errorf("provider count: outcome metric is absent")
	}
	explanations, explanationState := parseProviderCountExplanations(response.Data.Result, explanationRows, now)
	supply := parseProviderCountSupply(response.Data.Result, supplyRows, now)
	if untrustedParts {
		explanations, explanationState = nil, "invalid"
		supply = providerCountSupplyEvidence{state: "invalid"}
	}
	keys := make([]providerCountKey, 0, len(byKey))
	for key := range byKey {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i].frame() < keys[j].frame() })
	cohorts := make([]providerCountCohort, 0, len(keys))
	for _, key := range keys {
		cohort := providerCountCohort{key: key, bands: byKey[key], explanations: explanationState, supply: supply.state, supplyTargets: map[providerCountSupplyKey]*providerCountSupplyTarget{}}
		for explanation, value := range explanations[key] {
			switch {
			case slices.Contains(providerCountRequestExplainedReasons, explanation.reason):
				cohort.requestExplained += value
			case explanation.reason != "cache_empty":
			case explanation.targetKind == "city":
				cohort.targetSupply += value
			case explanation.targetKind == "region" || explanation.targetKind == "country":
				cohort.joinSupply(supply, explanation, value)
			}
		}
		cohorts = append(cohorts, cohort)
	}
	return cohorts, nil
}

// Judges one cohort's supply-empty answers for a country or region target
// against that country's admitted supply in the cohort's family. A region's
// country that lost its supply, or IPv6 supply lost across the fleet, pages;
// otherwise a region stays the narrow target it was. A country pages unless
// its supply is below the well-supplied floor and was not lost. Without a
// trusted verdict each keeps the treatment it had without supply data.
func (self *providerCountCohort) joinSupply(supply providerCountSupplyEvidence, explanation providerCountExplanationKey, zeros float64) {
	key := providerCountSupplyKey{country: explanation.targetCountry, family: self.key.ipFamily}
	verdict, current, lookback := supply.verdict(key)
	if supply.fleetV6Dropped() && key.family == "v6" {
		self.fleetV6Dropped = true
	}
	if supply.state == "joined" && explanation.targetCountry != "" {
		target := self.supplyTargets[key]
		if target == nil {
			target = &providerCountSupplyTarget{current: current, lookback: lookback, verdict: verdict}
			self.supplyTargets[key] = target
		}
		target.zeros += zeros
	}
	switch {
	case explanation.targetKind == "region":
		if verdict != providerCountSupplyLost && verdict != providerCountSupplyFleetDropped {
			self.targetSupply += zeros
		}
	case verdict == providerCountSupplyUnsupplied || verdict == providerCountSupplyLow:
		self.unsupplied += zeros
	}
}

type providerCountExplanationKey struct{ targetKind, targetCountry, reason string }

// Reads the tagged empty-outcome rows. Explanations can only lower a verdict,
// so any row that fails the outcome rows' checks, or one the fixed
// vocabularies do not hold, discards all of them, leaving every cohort judged
// on its result bands alone; such a row never fails the outcome read.
func parseProviderCountExplanations(result []struct {
	Metric map[string]string `json:"metric"`
	Value  []json.RawMessage `json:"value"`
}, rows []int, now time.Time) (map[providerCountKey]map[providerCountExplanationKey]float64, string) {
	if len(rows) == 0 {
		return nil, "absent"
	}
	explanations := map[providerCountKey]map[providerCountExplanationKey]float64{}
	for _, index := range rows {
		series := result[index]
		key, err := providerCountCohortKey(series.Metric)
		explanation := providerCountExplanationKey{targetKind: series.Metric["target_kind"], targetCountry: series.Metric["target_country"], reason: series.Metric["reason"]}
		if err != nil || series.Metric["monitor_part"] != providerCountEmptyPart || !slices.Contains(providerCountTargetKinds, explanation.targetKind) || !slices.Contains(providerCountZeroReasons, explanation.reason) || (explanation.targetCountry != "" && !providerCountCountryCode(explanation.targetCountry)) {
			return nil, "invalid"
		}
		observedAt, value, err := mimirInstantValue(series.Value)
		if age := now.Sub(observedAt); err != nil || age > providerCountFreshness || age < -30*time.Second || math.IsNaN(value) || math.IsInf(value, 0) || value < 0 {
			return nil, "invalid"
		}
		cohort := explanations[key]
		if cohort == nil {
			cohort = map[providerCountExplanationKey]float64{}
			explanations[key] = cohort
		}
		if _, duplicate := cohort[explanation]; duplicate {
			return nil, "invalid"
		}
		cohort[explanation] = value
	}
	return explanations, "joined"
}

// The request class every outcome and explanation row carries, in its fixed
// vocabulary.
func providerCountCohortKey(metric map[string]string) (providerCountKey, error) {
	value := func(name string) string { return metric[name] }
	key := providerCountKey{ipFamily: value("ip_family"), locationKind: value("location_kind"), callerCountry: value("caller_country"), rankMode: value("rank_mode"), forceMinimum: value("force_minimum")}
	valid := func(actual string, allowed ...string) bool { return slices.Contains(allowed, actual) }
	if !valid(key.ipFamily, "any", "v4", "v6", "dualstack", "unknown") || !valid(key.locationKind, "location", "group", "best-available", "mixed", "direct") || !valid(key.rankMode, "quality", "speed", "online", "unknown") || !valid(key.forceMinimum, "true", "false") || !(key.callerCountry == "unknown" || (len(key.callerCountry) == 2 && key.callerCountry == strings.ToLower(key.callerCountry))) {
		return providerCountKey{}, fmt.Errorf("provider count: invalid bounded metric labels")
	}
	return key, nil
}

func providerCountSeriesKey(metric map[string]string) (providerCountKey, string, error) {
	key, err := providerCountCohortKey(metric)
	if err != nil {
		return providerCountKey{}, "", err
	}
	band := metric["result_count"]
	if !slices.Contains([]string{"0", "1-2", "3-9", "10+"}, band) {
		return providerCountKey{}, "", fmt.Errorf("provider count: invalid result count band")
	}
	return key, band, nil
}
