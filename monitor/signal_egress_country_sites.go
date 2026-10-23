package monitor

import (
	"context"
	"fmt"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/urnetwork/server/controller"
	"github.com/urnetwork/server/qualityprobe/egresshealth"
)

// SIGNALS.md §2.19d maps to signal_egress_country_sites.go and
// signal_egress_country_sites_test.go. The url-only scheduler draws half of
// its normal url turns from the provider's country list when one is served,
// so the measurement system needs a usable list for every country. This
// signal monitors the measurement system, not the countries or their
// providers. It reads the configured qualityprobe.yml catalog (the global
// destinations and the per-country lists the api publishes with the pool
// generation), the GeoLite2-derived country rows of the location table as the
// denominator, and the Taskworker url-draw source counter. Country codes and
// counts are the only output: never a visited url, site name, provider id or
// raw failure. The producer publishes no provenance or verification records
// and no per-country or generation-linked draw receipts, so source freshness
// and per-country skips remain an explicit, alerted coverage defect.
func NewEgressCountrySitesSignal() Signal {
	return NewEgressCountrySitesSignalWithSettings(DefaultEgressCountrySitesSettings())
}

// The signal with explicit thresholds.
func NewEgressCountrySitesSignalWithSettings(settings *EgressCountrySitesSettings) Signal {
	return &signalAdapter{
		number: "2.19d", key: "egress-country-sites", name: "Country-specific egress site coverage",
		probe: egressCountrySitesProbe{settings: settings},
	}
}

const (
	egressCountrySitesProbeId = "pg/egress-country-sites"
	urlProbeSourcesMetric     = "urnetwork_url_probe_sources_total"
)

var egressCountryCodePattern = regexp.MustCompile(`^[a-z]{2}$`)

// The thresholds and bounds of §2.19d.
type EgressCountrySitesSettings struct {
	// sites every country list and the global list should hold
	TargetSites int
	// url-draw receipts are read over ReceiptWindow; the general/country split
	// is judged once MinSplitDraws country-eligible normal draws arrive, and
	// alerts only beyond SplitSigma standard deviations of an equal split
	ReceiptWindow time.Duration
	MinSplitDraws float64
	SplitSigma    float64
	// the most denominator countries one read accepts
	MaxCountries int
	// the Taskworker service whose slots must report the draw counter
	Service string
	// the one receipt read, and the counter's freshness on every slot
	FleetReadSettings
	// the most country codes and slot gaps one alert lists
	MaxListed int
}

// The defaults of SIGNALS.md §2.19d.
func DefaultEgressCountrySitesSettings() *EgressCountrySitesSettings {
	return &EgressCountrySitesSettings{
		TargetSites:   100,
		ReceiptWindow: 6 * time.Hour,
		MinSplitDraws: 400,
		SplitSigma:    6,
		MaxCountries:  512,
		Service:       "taskworker",
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

// The parts of the configured catalog this signal judges.
type egressCountryCatalog struct {
	globalUrls []string
	countries  map[string][]egressCountrySite
}

type egressCountrySite struct {
	url          string
	incompatible []egresshealth.Place
}

// Reads the deployment's qualityprobe.yml through the same parser the api
// serves the pool from; an unusable file is unknown, never an empty catalog.
func loadEgressCountryCatalog() (*egressCountryCatalog, error) {
	sites, err := controller.LoadProviderEgressSites()
	if err != nil {
		return nil, err
	}
	catalog := &egressCountryCatalog{countries: map[string][]egressCountrySite{}}
	for _, destination := range sites.Destinations {
		catalog.globalUrls = append(catalog.globalUrls, destination.Url)
	}
	for country, destinations := range sites.Countries {
		list := []egressCountrySite{}
		for _, destination := range destinations {
			list = append(list, egressCountrySite{url: destination.Url, incompatible: destination.Incompatible})
		}
		catalog.countries[country] = list
	}
	return catalog, nil
}

type egressCountrySitesProbe struct {
	settings *EgressCountrySitesSettings
	// nil reads the deployment's catalog and url scheduler configuration
	loadCatalog func() (*egressCountryCatalog, error)
	loadDesired func() (urlProbeCoverageDesired, error)
}

func (egressCountrySitesProbe) id() string             { return egressCountrySitesProbeId }
func (egressCountrySitesProbe) tier() string           { return tierWarn }
func (egressCountrySitesProbe) cadence() time.Duration { return 15 * time.Minute }

// The GeoLite2 export's countries as the location seeder stored them.
func egressCountryDenominatorQuery(settings *EgressCountrySitesSettings) string {
	return fmt.Sprintf(`/* monitor-egress-country-sites-denominator */
SELECT lower(country_code) FROM location WHERE location_type = 'country' GROUP BY 1 ORDER BY 1 LIMIT %d;
`, settings.MaxCountries+1)
}

func readEgressCountryDenominator(ctx context.Context, env *probeEnv, settings *EgressCountrySitesSettings) ([]string, error) {
	rows, err := env.runner.pg(ctx, egressCountryDenominatorQuery(settings))
	if err != nil {
		return nil, err
	}
	if settings.MaxCountries < len(rows) {
		return nil, fmt.Errorf("country denominator exceeds its bound")
	}
	countries := []string{}
	for _, row := range rows {
		if len(row) != 1 || !egressCountryCodePattern.MatchString(row.str(0)) {
			return nil, fmt.Errorf("country denominator has an invalid row")
		}
		countries = append(countries, row.str(0))
	}
	if len(countries) == 0 {
		return nil, fmt.Errorf("country denominator is empty")
	}
	return countries, nil
}

func egressCountryReceiptQuery(environment string, settings *EgressCountrySitesSettings, inventory fleetSlotInventory) string {
	base := "env=" + strconv.Quote(environment) + ",job=" + strconv.Quote(settings.Service)
	parts := []string{
		fleetPart("sources", "sum by (source) (increase("+urlProbeSourcesMetric+"{"+base+"}["+promDuration(settings.ReceiptWindow)+"]))"),
	}
	if selector := inventory.selector(environment); selector != "" {
		parts = append(parts, fleetSlotFreshnessPart("sources", urlProbeSourcesMetric, selector))
	}
	return strings.Join(parts, " or ")
}

// The url-draw receipts over the window, by draw source.
type egressCountryReceipts struct {
	enabled  bool
	sources  map[string]float64
	coverage fleetSlotCoverage
	gateway  string
}

func (self egressCountrySitesProbe) check(ctx context.Context, env *probeEnv) ([]finding, error) {
	settings := self.settings
	if settings == nil {
		settings = DefaultEgressCountrySitesSettings()
	}
	loadCatalog, loadDesired := self.loadCatalog, self.loadDesired
	if loadCatalog == nil {
		loadCatalog = loadEgressCountryCatalog
	}
	if loadDesired == nil {
		loadDesired = loadUrlProbeCoverageDesired
	}
	target := env.cfg.env + "/egress-country-sites"
	reasons := []string{}
	catalog, err := loadCatalog()
	if err != nil {
		catalog = nil
		reasons = append(reasons, "catalog_unavailable")
	}
	denominator, err := readEgressCountryDenominator(ctx, env, settings)
	if err != nil {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}
		denominator = nil
		reasons = append(reasons, "denominator_unavailable")
	}
	var receipts *egressCountryReceipts
	desired, err := loadDesired()
	switch {
	case err != nil:
		reasons = append(reasons, "url_workflow_unknown")
	case !desired.enabled:
		receipts = &egressCountryReceipts{}
	default:
		at := fleetEvaluationTime(env.now(), settings.EvaluationLag)
		inventory := newFleetSlotInventory(env, []string{settings.Service}, nil)
		out, gateway, err := readFleetMimir(ctx, env, egressCountryReceiptQuery(env.cfg.env, settings, inventory), at, settings.FleetReadSettings)
		if err != nil {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			default:
			}
			reasons = append(reasons, "receipts_unavailable "+observationFailureDetails(err, classifyObservationError(err)))
			break
		}
		samples, err := parseFleetSamples(out, at, settings.FleetReadSettings)
		if err != nil {
			reasons = append(reasons, "receipts_invalid_response")
			break
		}
		receipts = &egressCountryReceipts{enabled: true, sources: map[string]float64{}, gateway: gateway.name}
		receipts.coverage = newFleetSlotCoverage(inventory, samples, []string{"sources"}, at, settings.FleetReadSettings, nil)
		for _, sample := range samples {
			if sample.part != "sources" {
				continue
			}
			source := sample.labels["source"]
			switch source {
			case "general", "country", "general_country_unavailable", "security_recheck":
			default:
				reasons = append(reasons, "unrecognized_draw_source")
				continue
			}
			if len(sample.labels) != 1 {
				reasons = append(reasons, "unrecognized_draw_source")
				continue
			}
			if _, duplicate := receipts.sources[source]; duplicate || math.IsNaN(sample.value) || math.IsInf(sample.value, 0) || sample.value < 0 {
				reasons = append(reasons, "invalid_draw_receipt")
				continue
			}
			receipts.sources[source] = sample.value
		}
		if len(receipts.sources) != 4 {
			reasons = append(reasons, "draw_sources_incomplete")
		}
		if !receipts.coverage.full() {
			reasons = append(reasons, "receipt_slot_coverage_incomplete "+receipts.coverage.observed(settings.MaxListed))
		}
	}
	return evaluateEgressCountrySites(settings, target, catalog, denominator, receipts, reasons), nil
}

func evaluateEgressCountrySites(
	settings *EgressCountrySitesSettings,
	target string,
	catalog *egressCountryCatalog,
	denominator []string,
	receipts *egressCountryReceipts,
	reasons []string,
) []finding {
	listed := func(values []string) string {
		sort.Strings(values)
		if len(values) == 0 {
			return "none"
		}
		if len(values) <= settings.MaxListed {
			return strings.Join(values, ",")
		}
		return strings.Join(values[:settings.MaxListed], ",") + fmt.Sprintf(",and %d more", len(values)-settings.MaxListed)
	}
	// active use of the country path, from the aggregate draw receipts
	receiptText := "url_workflow=unknown"
	if receipts != nil && !receipts.enabled {
		receiptText = "url_workflow=disabled"
	} else if receipts != nil {
		general, country := receipts.sources["general"], receipts.sources["country"]
		unavailable, security := receipts.sources["general_country_unavailable"], receipts.sources["security_recheck"]
		normal := general + country + unavailable
		fallback := "unknown"
		if 0 < normal {
			fallback = strconv.FormatFloat(100*unavailable/normal, 'f', 1, 64) + "%"
		}
		receiptText = fmt.Sprintf("receipt_window=%s general_draws=%.0f country_draws=%.0f country_unavailable_draws=%.0f security_recheck_draws=%.0f normal_country_fallback_share=%s metrics_gateway=%s",
			settings.ReceiptWindow, general, country, unavailable, security, fallback, receipts.gateway)
	}
	findings := []finding{}
	countryFinding := func(class string, symptom string, observed string) finding {
		return finding{
			probeId: egressCountrySitesProbeId, tier: tierWarn, class: class, target: target, sustain: 1,
			symptom:   symptom,
			mechanism: "The URL-only scheduler draws half of a provider's normal URL turns from its country's list when the served pool has a compatible one, and falls back to the general list when it has none (general_country_unavailable). A missing, short or overlapping list therefore narrows or duplicates what is measured in that country. This is a pool-quality and coverage finding about the measurement system, not a provider or country fault, and not an FP2 admission gate: general URL measurements remain valid.",
			baseline:  fmt.Sprintf("Every GeoLite2 country has a list of at least %d compatible sites, the global list holds at least %d, no country site shares a registrable domain with the global list or another site of its list, and country-eligible normal draws split evenly between general and country.", settings.TargetSites, settings.TargetSites),
			observed:  observed + " " + receiptText,
			evidence:  "The configured qualityprobe.yml catalog through the API's own parser, the location table's GeoLite2-derived country rows as the denominator, and the Taskworker urnetwork_url_probe_sources_total increases by draw source. Registrable domains use the Public Suffix List. Only country codes and counts leave the monitor.",
			context:   "Some territories have no defensible country-ranked data; that is an explicit coverage gap, never invented sites. Region-specific hard exclusions can leave a provider few compatible sites even when its country has a full list. A site that is country-ranked but inaccessible from the prober host is a curation candidate, not proof of provider failure. Security rechecks are counted apart and are not country sampling failures.",
			action:    "Curate the named countries' lists from Radar first and CrUX second through the country-site helpers (canonical registrable domains, no global overlap, no padding), verify each site loads, and publish the catalog through the ordinary configuration release. Do not invent sites for territories without a ranked source.",
			verify:    fmt.Sprintf("The next cadence reads every named list at %d or more compatible sites without overlap, and later receipts show country draws advancing for those countries' providers.", settings.TargetSites),
			playbook:  "SIGNALS.md §2.19d",
		}
	}
	if catalog != nil {
		globalKeys := map[string]bool{}
		invalidGlobal := 0
		for _, rawUrl := range catalog.globalUrls {
			key, err := CanonicalSiteKey(rawUrl)
			if err != nil {
				invalidGlobal++
				continue
			}
			globalKeys[key] = true
		}
		underfilled, overlapping, outside, emptyLists := []string{}, []string{}, []string{}, map[string]bool{}
		denominatorSet := map[string]bool{}
		for _, country := range denominator {
			denominatorSet[country] = true
		}
		overlapSites, duplicateSites, invalidSites := 0, 0, 0
		for country, sites := range catalog.countries {
			if denominator != nil && !denominatorSet[country] {
				outside = append(outside, country)
			}
			seen := map[string]bool{}
			compatible, overlap, duplicate, invalid := 0, 0, 0, 0
			for _, site := range sites {
				key, err := CanonicalSiteKey(site.url)
				switch {
				case err != nil:
					invalid++
				case globalKeys[key]:
					overlap++
				case seen[key]:
					duplicate++
				}
				if err == nil {
					seen[key] = true
				}
				wholeCountry := false
				for _, place := range site.incompatible {
					if strings.EqualFold(strings.TrimSpace(place.Country), country) && strings.TrimSpace(place.Region) == "" {
						wholeCountry = true
					}
				}
				if !wholeCountry {
					compatible++
				}
			}
			if compatible == 0 {
				emptyLists[country] = true
			} else if compatible < settings.TargetSites {
				underfilled = append(underfilled, fmt.Sprintf("%s:%d", country, compatible))
			}
			if 0 < overlap+duplicate+invalid {
				overlapping = append(overlapping, country)
			}
			overlapSites += overlap
			duplicateSites += duplicate
			invalidSites += invalid
		}
		if denominator != nil {
			missing := []string{}
			for _, country := range denominator {
				if _, present := catalog.countries[country]; !present || emptyLists[country] {
					missing = append(missing, country)
				}
			}
			if len(missing) != 0 {
				findings = append(findings, countryFinding("egress-country-list-missing",
					fmt.Sprintf("%d of %d GeoLite2 countries have no usable country site list", len(missing), len(denominator)),
					fmt.Sprintf("denominator_countries=%d countries_with_lists=%d missing=%d missing_countries=%s lists_outside_denominator=%d outside_countries=%s",
						len(denominator), len(denominator)-len(missing), len(missing), listed(missing), len(outside), listed(outside))))
			} else {
				findings = append(findings, healthyFinding(egressCountrySitesProbeId, tierWarn, "egress-country-list-missing", target))
			}
		}
		if len(catalog.globalUrls) < settings.TargetSites || len(underfilled) != 0 {
			findings = append(findings, countryFinding("egress-country-list-underfilled",
				fmt.Sprintf("%d of %d country lists fall short of the %d-site target, and the global list holds %d sites", len(underfilled), len(catalog.countries), settings.TargetSites, len(catalog.globalUrls)),
				fmt.Sprintf("global_sites=%d target_sites=%d underfilled_lists=%d underfilled=%s", len(catalog.globalUrls), settings.TargetSites, len(underfilled), listed(underfilled))))
		} else {
			findings = append(findings, healthyFinding(egressCountrySitesProbeId, tierWarn, "egress-country-list-underfilled", target))
		}
		if len(overlapping) != 0 {
			findings = append(findings, countryFinding("egress-country-list-overlap",
				fmt.Sprintf("%d country lists share registrable domains with the global list or repeat a site", len(overlapping)),
				fmt.Sprintf("global_overlap_sites=%d duplicate_sites=%d invalid_country_urls=%d invalid_global_urls=%d countries=%s",
					overlapSites, duplicateSites, invalidSites, invalidGlobal, listed(overlapping))))
		} else {
			findings = append(findings, healthyFinding(egressCountrySitesProbeId, tierWarn, "egress-country-list-overlap", target))
		}
		// the producer contract has no fields for these records at all
		provenance := countryFinding("egress-country-provenance-missing",
			"Country site lists carry no provenance, verification or per-country draw records, so their freshness and per-country use cannot be measured",
			fmt.Sprintf("country_lists=%d missing_records=source_identity,source_period,curation_time,load_verification_time,pool_generation_receipts,per_country_draw_receipts", len(catalog.countries)))
		provenance.mechanism = "The qualityprobe.yml country catalog lists destinations only: it has no source identity, source period, curation or independent load-verification time for any list, and the URL-draw counter has neither a country nor a pool-generation label. The Radar (seven days), CrUX (two monthly releases) and verification (30 days) freshness bands and the per-country skip page of §2.19d therefore cannot be evaluated. This is a standing producer gap, not healthy freshness."
		provenance.action = "Add the provenance and verification fields to the country catalog and a bounded per-country draw and skip counter linked to the served pool generation in the producer, then extend this signal to judge them. Until then treat country-list freshness as unknown."
		provenance.verify = "The catalog carries provenance for every list and receipts carry country and pool generation; this class then gives way to the freshness bands."
		findings = append(findings, provenance)
	}
	if receipts != nil && receipts.enabled && len(receipts.sources) == 4 {
		general, country := receipts.sources["general"], receipts.sources["country"]
		eligible := general + country
		if settings.MinSplitDraws <= eligible {
			deviation := math.Abs(country-eligible/2) / math.Sqrt(eligible/4)
			if settings.SplitSigma <= deviation {
				split := countryFinding("egress-country-draw-split",
					"Country-eligible normal URL draws do not split evenly between the general and country lists",
					fmt.Sprintf("country_share=%.1f%% deviation_sigma=%.1f split_sigma=%.0f", 100*country/eligible, deviation, settings.SplitSigma))
				split.mechanism = "When a provider's country has a compatible list, the normal half of its URL turns is an even coin between the general and country lists. Receipts this far from an even split over this many draws are not finite random imbalance: the draw rule, the served pool or the receipt counter is wrong. The aggregate counter has no pool-generation label, so a mixed rollout of the draw rule cannot be told apart from a defect."
				split.action = "Compare the running Taskworker artifact's URL draw rule with the catalog contract, and check that the served pool carries the country lists; do not resample or reweight to hide the split."
				split.verify = "A later receipt window of the same size falls within the split bound."
				findings = append(findings, split)
			} else {
				findings = append(findings, healthyFinding(egressCountrySitesProbeId, tierWarn, "egress-country-draw-split", target))
			}
		}
	}
	if len(reasons) != 0 {
		unique := []string{}
		seenReasons := map[string]bool{}
		for _, reason := range reasons {
			if !seenReasons[reason] {
				seenReasons[reason] = true
				unique = append(unique, reason)
			}
		}
		findings = append(findings, finding{
			probeId: egressCountrySitesProbeId, tier: tierWarn, class: "egress-country-sites-unobservable",
			target: target, sustain: 2,
			symptom:   "Country site coverage cannot be fully established",
			mechanism: "The configured catalog, the GeoLite2-derived country denominator, the URL scheduler configuration or the Taskworker draw receipts could not be read, or a desired Taskworker slot lacks a fresh draw counter. A missing or underfilled list cannot be ruled out from what was read.",
			baseline:  "The catalog parses, the denominator reads, and every desired Taskworker slot exports a fresh urnetwork_url_probe_sources_total for all four draw sources.",
			observed:  "visibility_reason=" + strings.Join(unique, ";") + " " + receiptText,
			evidence:  "Desired slots come from active services.yml placement, not from surviving series. Findings proven from what was read are reported beside this.",
			context:   "An unreadable manifest, pool, GeoLite2 export or receipt cannot emit healthy evidence or resolve an existing alert.",
			action:    "Restore the configuration resource, the PostgreSQL read or the Mimir path, and rerun.",
			verify:    "Every source reads on two cadences.",
			playbook:  "SIGNALS.md §2.19d",
		})
	}
	return findings
}
