// This file keeps the egress destination pool representative
// (connect/GEOMAP.md §11.4): once a day it judges every active site by our own
// load tally, retires the sites that fail exits known to work, promotes
// candidates in their place, and learns the places a working site is blocked
// from.
package work

import (
	"context"
	"crypto/tls"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/urnetwork/glog"
	"github.com/urnetwork/server/qualityprobe/egresshealth"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/controller"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
	"github.com/urnetwork/server/task"
)

var egressSiteFailureShare = prometheus.NewGaugeVec(prometheus.GaugeOpts{
	Namespace: "urnetwork",
	Subsystem: "egress_site",
	Name:      "failure_share",
	Help:      "The latest pool refresh's failure share of each active site, over healthy exits where enough qualified, else over all exits; per site name and class, both bounded by the pool",
}, []string{"site", "class"})

var egressSitePoolSize = prometheus.NewGaugeVec(prometheus.GaugeOpts{
	Namespace: "urnetwork",
	Subsystem: "egress_site",
	Name:      "pool_size",
	Help:      "Destination pool rows per class after the latest refresh, by state: scored, probation (active, not yet counted), candidates (promotable now)",
}, []string{"class", "state"})

var egressSiteChangesTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
	Namespace: "urnetwork",
	Subsystem: "egress_site",
	Name:      "changes_total",
	Help:      "Pool changes the refresh made, by change (retired, promoted, graduated, marked, unmarked) and class",
}, []string{"change", "class"})

var egressSiteRefreshRunsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
	Namespace: "urnetwork",
	Subsystem: "egress_site",
	Name:      "refresh_runs_total",
	Help:      "Pool refreshes by result: refreshed, skipped (the fleet-wide failure share was above the prober-fault line), failed",
}, []string{"result"})

var egressSiteRefreshLastRunTimestamp = prometheus.NewGauge(prometheus.GaugeOpts{
	Namespace: "urnetwork",
	Subsystem: "egress_site",
	Name:      "refresh_last_run_timestamp_seconds",
	Help:      "Unix time this taskworker last completed a pool refresh, refreshed or skipped",
})

// Registers the refresh's metrics with every bounded label set seeded.
func init() {
	for _, result := range []string{"refreshed", "skipped", "failed"} {
		egressSiteRefreshRunsTotal.WithLabelValues(result)
	}
	for _, change := range []string{"retired", "promoted", "graduated", "marked", "unmarked"} {
		for _, class := range model.ProviderEgressSiteClasses {
			egressSiteChangesTotal.WithLabelValues(change, class)
		}
	}
	prometheus.MustRegister(
		egressSiteFailureShare,
		egressSitePoolSize,
		egressSiteChangesTotal,
		egressSiteRefreshRunsTotal,
		egressSiteRefreshLastRunTimestamp,
	)
}

// The refresh takes no arguments: everything it decides on is read at run time.
type RefreshEgressDestinationsArgs struct{}

// What one refresh did; the same numbers are logged.
type RefreshEgressDestinationsResult struct {
	// Skipped says why nothing was judged, empty for a refresh that ran.
	Skipped   string `json:"skipped,omitempty"`
	Seeded    bool   `json:"seeded,omitempty"`
	Synced    int    `json:"synced"`
	Judged    int    `json:"judged"`
	Retired   int    `json:"retired"`
	Promoted  int    `json:"promoted"`
	Graduated int    `json:"graduated"`
	Marked    int    `json:"marked"`
	Unmarked  int    `json:"unmarked"`
	// Unfilled counts openings no checked candidate loaded cleanly for.
	Unfilled int `json:"unfilled"`
}

// Arms the daily refresh one interval out: a freshly seeded pool is the
// prober's own table, and there is nothing to judge until the prober has
// loaded it.
func ScheduleRefreshEgressDestinations(clientSession *session.ClientSession, tx server.PgTx) {
	settings, err := model.GetProviderEgressSiteSettings()
	if err != nil {
		// the refresh itself reports the unusable file every run; the chain
		// keeps the default cadence until it is fixed
		settings = model.DefaultProviderEgressSiteSettings()
	}
	task.ScheduleTaskInTx(
		tx,
		RefreshEgressDestinations,
		&RefreshEgressDestinationsArgs{},
		clientSession,
		task.RunOnce("refresh_egress_destinations"),
		task.RunAt(server.NowUtc().Add(settings.SiteRefreshInterval())),
		task.MaxTime(settings.SiteRefreshMaxTime()),
	)
}

// One refresh of the pool. An unusable egress-sites.yml fails the run (and so
// the monitor's refresh-not-running finding) rather than judging the pool on
// numbers nobody meant.
func RefreshEgressDestinations(
	_ *RefreshEgressDestinationsArgs,
	clientSession *session.ClientSession,
) (*RefreshEgressDestinationsResult, error) {
	result, err := refreshEgressDestinations(clientSession.Ctx, server.NowUtc(), checkEgressCandidateFromHost)
	if err != nil {
		egressSiteRefreshRunsTotal.WithLabelValues("failed").Inc()
		return nil, err
	}
	if result.Skipped != "" {
		egressSiteRefreshRunsTotal.WithLabelValues("skipped").Inc()
	} else {
		egressSiteRefreshRunsTotal.WithLabelValues("refreshed").Inc()
	}
	egressSiteRefreshLastRunTimestamp.Set(float64(time.Now().Unix()))
	return result, nil
}

// Schedules the next refresh one interval out, whatever this one did.
func RefreshEgressDestinationsPost(
	_ *RefreshEgressDestinationsArgs,
	_ *RefreshEgressDestinationsResult,
	clientSession *session.ClientSession,
	tx server.PgTx,
) error {
	ScheduleRefreshEgressDestinations(clientSession, tx)
	return nil
}

// Loads one candidate from the taskworker host itself: nil when it loaded
// cleanly.
type egressCandidateCheck func(ctx context.Context, candidate egresshealth.Destination, profile egresshealth.RequestProfile, attempts int, meanInterval time.Duration) error

// Loads a candidate the way the prober loads it -- the module's own
// browser-shaped, retried fetch (egresshealth.Check) -- but from this host,
// with no provider in the path. A site that fails from a datacenter host with
// a clean route will fail through every tunnel too, and promoting it would
// only burn a probation. The client refuses redirects and speaks HTTP/1.1, as
// the provider tunnel's client does, so a 3xx contract and a bot manager see
// the same request they will see through a provider.
func checkEgressCandidateFromHost(
	ctx context.Context,
	candidate egresshealth.Destination,
	profile egresshealth.RequestProfile,
	attempts int,
	meanInterval time.Duration,
) error {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.ForceAttemptHTTP2 = false
	transport.TLSNextProto = map[string]func(string, *tls.Conn) http.RoundTripper{}
	transport.DisableKeepAlives = true
	client := &http.Client{
		Transport: transport,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	result, err := egresshealth.Check(ctx, client, egresshealth.Options{
		Destinations:          []egresshealth.Destination{candidate},
		AllDestinations:       true,
		Concurrency:           1,
		Profile:               &profile,
		LoadAttempts:          attempts,
		LoadRetryMeanInterval: meanInterval,
	})
	if err != nil {
		return err
	}
	if result.Total != 1 || result.OkCount != 1 {
		reason := "failed every attempt"
		if 0 < len(result.Checks) && result.Checks[0].Err != "" {
			reason = result.Checks[0].Err
		}
		return fmt.Errorf("%s did not load cleanly from the taskworker host: %s", candidate.Name, reason)
	}
	return nil
}

// One active site's share, and what it stands on.
type egressSiteJudgement struct {
	name    string
	class   string
	share   *float64
	samples int
	// basis is healthy (enough healthy exits), all (too few healthy exits,
	// judged on every exit and logged as such), or too_few (no judgement)
	basis string
}

// The share a site is judged on: its failure share among healthy exits when at
// least minSamples qualify, else -- when the bad sites themselves drag every
// exit under the healthy line -- its share over all exits, which the caller
// logs. Under minSamples either way there is no judgement.
func judgeEgressSite(healthyLoads, healthyFailures, loads, failures, minSamples int) (share *float64, samples int, basis string) {
	ratio := func(failed int, total int) *float64 {
		value := float64(failed) / float64(total)
		return &value
	}
	switch {
	case minSamples <= healthyLoads:
		return ratio(healthyFailures, healthyLoads), healthyLoads, "healthy"
	case minSamples <= loads:
		return ratio(failures, loads), loads, "all"
	case 0 < healthyLoads:
		return ratio(healthyFailures, healthyLoads), healthyLoads, "too_few"
	case 0 < loads:
		return ratio(failures, loads), loads, "too_few"
	default:
		return nil, 0, "too_few"
	}
}

// One change the refresh made, for the log.
type egressRefreshChange struct {
	name     string
	class    string
	category string
	detail   string
}

// One opening to promote a candidate into.
type egressPromotionNeed struct {
	class string
	// category is the category the opening prefers ("" any): a retired site
	// is replaced by one of its own kind.
	category string
	// place, when set, is a place the class is too thin at: the candidate
	// must not be known to fail there.
	place  *model.ProviderEgressPlace
	reason string
}

// Everything one refresh decides on.
type egressRefreshInput struct {
	now          time.Time
	settings     *model.ProviderEgressSiteSettings
	destinations []*model.ProviderEgressDestination
	// windowTallies are the site tally over the window, per site and place
	windowTallies []model.ProviderEgressSiteTally
	// probationTotals are the per-day totals of the sites on probation, from
	// their earliest promotion day on
	probationTotals []model.ProviderEgressSiteDayTotal
	// recent is the fleet's scored loads within the prober-fault window
	recent model.ProviderEgressClassTotal
}

// What one refresh changes. rows holds a copy of every row it changes, keyed
// by name, ready to write.
type egressRefreshPlan struct {
	skipped    string
	rows       map[string]*model.ProviderEgressDestination
	judgements []egressSiteJudgement
	retired    []egressRefreshChange
	graduated  []egressRefreshChange
	marked     []egressRefreshChange
	unmarked   []egressRefreshChange
	fallbacks  []string
	needs      []egressPromotionNeed
}

// The plan's copy of d, made on first use, so the input rows are never changed.
func (self *egressRefreshPlan) row(d *model.ProviderEgressDestination) *model.ProviderEgressDestination {
	if row, ok := self.rows[d.Name]; ok {
		return row
	}
	row := *d
	row.Incompatible = append([]model.ProviderEgressDestinationPlace(nil), d.Incompatible...)
	self.rows[d.Name] = &row
	return &row
}

// Names a place in a log line: "de", or "us/California".
func egressPlaceKey(place model.ProviderEgressPlace) string {
	if place.Region == "" {
		return place.CountryCode
	}
	return place.CountryCode + "/" + place.Region
}

// One site's summed tally at one place, or overall.
type egressPlaceCounts struct {
	loads, failures, healthyLoads, healthyFailures, canaryLoads, canaryPasses int
}

// Adds one tally row.
func (self *egressPlaceCounts) add(t model.ProviderEgressSiteTally) {
	self.loads += t.LoadCount
	self.failures += t.FailureCount
	self.healthyLoads += t.HealthyLoadCount
	self.healthyFailures += t.HealthyFailureCount
	self.canaryLoads += t.CanaryLoadCount
	self.canaryPasses += t.CanaryPassCount
}

// Decides one refresh (GEOMAP §11.4), with no I/O, in this order:
//
//  1. skip everything while the fleet-wide failure share -- recent, or over
//     the window -- is above the prober-fault line: a prober fault fails every
//     site at once, and retiring sites then would empty the pool for nothing;
//  2. judge every active site on its failure share among healthy exits
//     (judgeEgressSite), and every site on probation on its loads since its
//     promotion: past SiteProbationShare of SiteMinSamples it counts from
//     now on, under it it is retired;
//  3. retire, per class, at most SiteMaxRetirePerRun of the scored sites above
//     SiteRetireShare over at least SiteMinSamples, worst first, each opening
//     a promotion of its own category;
//  4. mark a place incompatible for a site that works elsewhere but fails at
//     least SiteRegionFailShare of the healthy exits there over at least
//     SiteRegionMinSamples -- a whole country when the country qualifies, else
//     each region that does -- but only in places where most sites pass: a
//     place where everything fails is the route or the echo, never the sites;
//  5. unmark a place the refresh itself learned once its canaries have passed
//     for SiteRegionCooldown;
//  6. open promotions for a class under SitePoolSize, and for a class whose
//     compatible pool at a place fell under its sample size, each bounded by
//     SiteMaxRetirePerRun per class.
func planEgressDestinationRefresh(input egressRefreshInput) *egressRefreshPlan {
	settings := input.settings
	now := input.now
	plan := &egressRefreshPlan{rows: map[string]*model.ProviderEgressDestination{}}
	// what a judgement's samples were, for the log
	exitsJudged := func(basis string) string {
		if basis == "all" {
			return "exits, healthy or not (too few healthy exits to judge on)"
		}
		return "healthy exits"
	}

	// 1. the prober-fault line
	windowLoads, windowFailures := 0, 0
	for _, t := range input.windowTallies {
		windowLoads += t.LoadCount
		windowFailures += t.FailureCount
	}
	if 0 < input.recent.Total {
		share := float64(input.recent.Total-input.recent.Ok) / float64(input.recent.Total)
		if settings.SiteProberFaultShare < share {
			plan.skipped = fmt.Sprintf("the fleet-wide failure share of the last %s is %.3f, above the prober-fault line %.3f", settings.SiteProberFaultWindow(), share, settings.SiteProberFaultShare)
			return plan
		}
	}
	if 0 < windowLoads {
		share := float64(windowFailures) / float64(windowLoads)
		if settings.SiteProberFaultShare < share {
			plan.skipped = fmt.Sprintf("the fleet-wide failure share over the window is %.3f, above the prober-fault line %.3f", share, settings.SiteProberFaultShare)
			return plan
		}
	}

	byName := map[string]*model.ProviderEgressDestination{}
	for _, d := range input.destinations {
		byName[d.Name] = d
	}
	siteTotals := map[string]*egressPlaceCounts{}
	sitePlaces := map[string]map[model.ProviderEgressPlace]*egressPlaceCounts{}
	for _, t := range input.windowTallies {
		if _, ok := siteTotals[t.Name]; !ok {
			siteTotals[t.Name] = &egressPlaceCounts{}
			sitePlaces[t.Name] = map[model.ProviderEgressPlace]*egressPlaceCounts{}
		}
		siteTotals[t.Name].add(t)
		// every tally row counts toward its country, and a row with a region
		// toward that (country, region) too
		places := []model.ProviderEgressPlace{{CountryCode: t.Place.CountryCode}}
		if t.Place.Region != "" {
			places = append(places, t.Place)
		}
		for _, place := range places {
			if place.CountryCode == "" {
				continue
			}
			counts, ok := sitePlaces[t.Name][place]
			if !ok {
				counts = &egressPlaceCounts{}
				sitePlaces[t.Name][place] = counts
			}
			counts.add(t)
		}
	}

	// 2. judgement and probation
	sort.Slice(input.destinations, func(i, j int) bool { return input.destinations[i].Name < input.destinations[j].Name })
	retireCandidates := map[string][]egressSiteJudgement{}
	failedProbation := map[string][]egressRefreshChange{}
	probationSince := map[string]*egressPlaceCounts{}
	for _, total := range input.probationTotals {
		d, ok := byName[total.Name]
		if !ok || !d.Probation || d.PromotedTime == nil || total.Day.Before(d.PromotedTime.UTC().Truncate(24*time.Hour)) {
			continue
		}
		counts, ok := probationSince[total.Name]
		if !ok {
			counts = &egressPlaceCounts{}
			probationSince[total.Name] = counts
		}
		counts.loads += total.LoadCount
		counts.failures += total.FailureCount
		counts.healthyLoads += total.HealthyLoadCount
		counts.healthyFailures += total.HealthyFailureCount
	}
	for _, d := range input.destinations {
		if !d.Active {
			continue
		}
		counts := siteTotals[d.Name]
		if counts == nil {
			counts = &egressPlaceCounts{}
		}
		share, samples, basis := judgeEgressSite(counts.healthyLoads, counts.healthyFailures, counts.loads, counts.failures, settings.SiteMinSamples)
		judgement := egressSiteJudgement{name: d.Name, class: d.Class, share: share, samples: samples, basis: basis}
		plan.judgements = append(plan.judgements, judgement)
		if basis == "all" {
			plan.fallbacks = append(plan.fallbacks, d.Name)
		}
		row := plan.row(d)
		row.FailureShare = share
		row.SampleCount = samples
		judgedTime := now
		row.JudgedTime = &judgedTime
		aboveRetire := basis != "too_few" && share != nil && settings.SiteRetireShare < *share
		if aboveRetire {
			if row.AboveRetireSince == nil {
				row.AboveRetireSince = &judgedTime
			}
		} else {
			row.AboveRetireSince = nil
		}

		if d.Probation {
			since := probationSince[d.Name]
			if since == nil {
				since = &egressPlaceCounts{}
			}
			probationShare, probationSamples, probationBasis := judgeEgressSite(since.healthyLoads, since.healthyFailures, since.loads, since.failures, settings.SiteMinSamples)
			if probationBasis == "too_few" || probationShare == nil {
				continue
			}
			passShare := 1 - *probationShare
			if settings.SiteProbationShare <= passShare {
				row.Probation = false
				plan.graduated = append(plan.graduated, egressRefreshChange{
					name: d.Name, class: d.Class, category: d.Category,
					detail: fmt.Sprintf("passed %.1f%% of %d %s since its promotion", 100*passShare, probationSamples, exitsJudged(probationBasis)),
				})
			} else {
				failedProbation[d.Class] = append(failedProbation[d.Class], egressRefreshChange{
					name: d.Name, class: d.Class, category: d.Category,
					detail: fmt.Sprintf("failed probation: passed %.1f%% of %d %s since its promotion, under the %.1f%% it needed", 100*passShare, probationSamples, exitsJudged(probationBasis), 100*settings.SiteProbationShare),
				})
			}
			continue
		}
		if aboveRetire {
			retireCandidates[d.Class] = append(retireCandidates[d.Class], judgement)
		}
	}

	// 3. retirement, bounded per class
	retiredNames := map[string]bool{}
	for _, class := range model.ProviderEgressSiteClasses {
		changes := failedProbation[class]
		judgements := retireCandidates[class]
		sort.Slice(judgements, func(i, j int) bool {
			if *judgements[i].share != *judgements[j].share {
				return *judgements[i].share > *judgements[j].share
			}
			return judgements[i].name < judgements[j].name
		})
		for _, judgement := range judgements {
			d := byName[judgement.name]
			changes = append(changes, egressRefreshChange{
				name: d.Name, class: d.Class, category: d.Category,
				detail: fmt.Sprintf("failed %.1f%% of %d %s over %s", 100**judgement.share, judgement.samples, exitsJudged(judgement.basis), settings.SiteWindow()),
			})
		}
		for i, change := range changes {
			if settings.SiteMaxRetirePerRun <= i {
				break
			}
			d := byName[change.name]
			row := plan.row(d)
			retiredTime := now
			row.Active = false
			row.Probation = false
			row.RetiredTime = &retiredTime
			row.RetireReason = change.detail
			if 256 < len(row.RetireReason) {
				row.RetireReason = row.RetireReason[:256]
			}
			row.RetireCount++
			row.AboveRetireSince = nil
			retiredNames[d.Name] = true
			plan.retired = append(plan.retired, change)
			plan.needs = append(plan.needs, egressPromotionNeed{class: d.Class, category: d.Category, reason: "replaces " + d.Name})
		}
	}

	// 4. regional incompatibility, only where most sites pass
	placeSites := map[model.ProviderEgressPlace]int{}
	placeFailingSites := map[model.ProviderEgressPlace]int{}
	for name, places := range sitePlaces {
		d, ok := byName[name]
		if !ok || !d.Scored() {
			continue
		}
		for place, counts := range places {
			if counts.healthyLoads < 1 {
				continue
			}
			placeSites[place]++
			if settings.SiteRegionFailShare*float64(counts.healthyLoads) <= float64(counts.healthyFailures) {
				placeFailingSites[place]++
			}
		}
	}
	mostSitesPass := func(place model.ProviderEgressPlace) bool {
		return 0 < placeSites[place] && 2*placeFailingSites[place] < placeSites[place]
	}
	blocked := func(counts *egressPlaceCounts) bool {
		return counts != nil &&
			settings.SiteRegionMinSamples <= counts.healthyLoads &&
			settings.SiteRegionFailShare*float64(counts.healthyLoads) <= float64(counts.healthyFailures)
	}
	markedPlaces := map[string]map[model.ProviderEgressPlace]bool{}
	for _, d := range input.destinations {
		if !d.Active || retiredNames[d.Name] {
			continue
		}
		row := plan.row(d)
		if row.FailureShare != nil && settings.SiteRetireShare < *row.FailureShare {
			// failing everyone is the retire line's case, not a place's
			continue
		}
		places := sitePlaces[d.Name]
		// countries first, so a qualifying country is one mark rather than
		// one per region
		ordered := make([]model.ProviderEgressPlace, 0, len(places))
		for place := range places {
			ordered = append(ordered, place)
		}
		sort.Slice(ordered, func(i, j int) bool {
			if (ordered[i].Region == "") != (ordered[j].Region == "") {
				return ordered[i].Region == ""
			}
			return egressPlaceKey(ordered[i]) < egressPlaceKey(ordered[j])
		})
		for _, place := range ordered {
			if !blocked(places[place]) || !mostSitesPass(place) || row.IncompatibleWith(place) {
				continue
			}
			markedAt := now
			row.Incompatible = append(row.Incompatible, model.ProviderEgressDestinationPlace{
				Country:  place.CountryCode,
				Region:   place.Region,
				MarkedAt: &markedAt,
			})
			counts := places[place]
			plan.marked = append(plan.marked, egressRefreshChange{
				name: d.Name, class: d.Class, category: d.Category,
				detail: fmt.Sprintf("%s: failed %d of %d healthy exits there", egressPlaceKey(place), counts.healthyFailures, counts.healthyLoads),
			})
			if markedPlaces[d.Class] == nil {
				markedPlaces[d.Class] = map[model.ProviderEgressPlace]bool{}
			}
			markedPlaces[d.Class][place] = true
		}
	}

	// 5. canaries: a learned mark whose canaries keep passing is lifted
	for _, d := range input.destinations {
		if !d.Active || retiredNames[d.Name] {
			continue
		}
		row := plan.row(d)
		kept := make([]model.ProviderEgressDestinationPlace, 0, len(row.Incompatible))
		for _, mark := range row.Incompatible {
			if mark.MarkedAt == nil || !mark.MarkedAt.Before(now) {
				kept = append(kept, mark)
				continue
			}
			// the canaries pass while at least SiteProbationShare of the
			// window's canary loads there pass -- what a new site needs to
			// earn scoring -- and a mark is lifted only on current evidence:
			// a window with no canary loads there neither lifts it nor ends
			// the passing streak
			counts := sitePlaces[d.Name][model.ProviderEgressPlace{CountryCode: mark.Country, Region: mark.Region}]
			passing := false
			if counts != nil && 0 < counts.canaryLoads {
				if settings.SiteProbationShare*float64(counts.canaryLoads) <= float64(counts.canaryPasses) {
					passing = true
					if mark.CanaryPassingSince == nil {
						passingSince := now
						mark.CanaryPassingSince = &passingSince
					}
				} else {
					mark.CanaryPassingSince = nil
				}
			}
			if passing && !now.Before(mark.CanaryPassingSince.Add(settings.SiteRegionCooldown())) {
				plan.unmarked = append(plan.unmarked, egressRefreshChange{
					name: d.Name, class: d.Class, category: d.Category,
					detail: fmt.Sprintf("%s: canaries passed for %s", egressPlaceKey(model.ProviderEgressPlace{CountryCode: mark.Country, Region: mark.Region}), settings.SiteRegionCooldown()),
				})
				continue
			}
			kept = append(kept, mark)
		}
		row.Incompatible = kept
	}

	// 6. openings beyond the retirements
	for _, class := range model.ProviderEgressSiteClasses {
		active := 0
		for _, d := range input.destinations {
			row := d
			if changed, ok := plan.rows[d.Name]; ok {
				row = changed
			}
			if row.Class == class && row.Active {
				active++
			}
		}
		opened := 0
		for _, need := range plan.needs {
			if need.class == class {
				opened++
			}
		}
		deficit := settings.SitePoolSize[class] - active - opened
		for i := 0; i < deficit && i < settings.SiteMaxRetirePerRun; i++ {
			plan.needs = append(plan.needs, egressPromotionNeed{class: class, reason: fmt.Sprintf("the class holds %d of %d", active, settings.SitePoolSize[class])})
		}
		placeNeeds := 0
		places := make([]model.ProviderEgressPlace, 0, len(markedPlaces[class]))
		for place := range markedPlaces[class] {
			places = append(places, place)
		}
		sort.Slice(places, func(i, j int) bool { return egressPlaceKey(places[i]) < egressPlaceKey(places[j]) })
		for _, place := range places {
			if settings.SiteMaxRetirePerRun <= placeNeeds {
				break
			}
			compatible := 0
			for _, d := range input.destinations {
				row := d
				if changed, ok := plan.rows[d.Name]; ok {
					row = changed
				}
				if row.Class == class && row.Active && !row.IncompatibleWith(place) {
					compatible++
				}
			}
			if compatible < settings.SiteSampleSize[class] {
				place := place
				plan.needs = append(plan.needs, egressPromotionNeed{
					class: class, place: &place,
					reason: fmt.Sprintf("%s has %d compatible %s sites, under the sample of %d", egressPlaceKey(place), compatible, class, settings.SiteSampleSize[class]),
				})
				placeNeeds++
			}
		}
	}
	return plan
}

// The promotion order for one opening: candidates of its class that may be
// promoted now and are not known to fail at its place; the opening's category
// first, then the candidates compatible with the most of the class's thin
// places, then those never retired, then the oldest.
func orderEgressCandidates(
	destinations []*model.ProviderEgressDestination,
	need egressPromotionNeed,
	thinPlaces []model.ProviderEgressPlace,
	now time.Time,
	settings *model.ProviderEgressSiteSettings,
	taken map[string]bool,
) []*model.ProviderEgressDestination {
	candidates := []*model.ProviderEgressDestination{}
	for _, d := range destinations {
		if taken[d.Name] || d.Class != need.class || !d.IsCandidate(now, settings.SiteRetireCooldown(), settings.SiteMaxRetirements) {
			continue
		}
		if need.place != nil && d.IncompatibleWith(*need.place) {
			continue
		}
		candidates = append(candidates, d)
	}
	compatibleThin := func(d *model.ProviderEgressDestination) int {
		n := 0
		for _, place := range thinPlaces {
			if !d.IncompatibleWith(place) {
				n++
			}
		}
		return n
	}
	sort.SliceStable(candidates, func(i, j int) bool {
		a, b := candidates[i], candidates[j]
		if need.category != "" && (a.Category == need.category) != (b.Category == need.category) {
			return a.Category == need.category
		}
		if ca, cb := compatibleThin(a), compatibleThin(b); ca != cb {
			return ca > cb
		}
		if (a.RetiredTime == nil) != (b.RetiredTime == nil) {
			return a.RetiredTime == nil
		}
		if !a.AddedTime.Equal(b.AddedTime) {
			return a.AddedTime.Before(b.AddedTime)
		}
		return a.Name < b.Name
	})
	return candidates
}

// One refresh at now: seed, sync the candidates, plan, fill the openings
// through host checks, write, and report.
func refreshEgressDestinations(ctx context.Context, now time.Time, check egressCandidateCheck) (*RefreshEgressDestinationsResult, error) {
	sites, err := controller.LoadProviderEgressSites()
	if err != nil {
		glog.Errorf("[egresssites]egress-sites.yml is unusable, the pool is not refreshed: %s\n", err)
		return nil, err
	}
	settings := sites.Settings
	result := &RefreshEgressDestinationsResult{}
	// the host checks load with the probe's own load rules, so a candidate
	// earns its place with the retries every site gets; they run at once, so
	// one check's retry schedule is what the task's max time has to cover
	attempts, meanInterval := egresshealth.DefaultLoadAttempts, egresshealth.DefaultLoadRetryMeanInterval
	if probeSettings, err := getProviderEgressProbeSettings(); err == nil && 0 < probeSettings.LoadAttempts && 0 < probeSettings.LoadRetryMeanIntervalSeconds {
		attempts = probeSettings.LoadAttempts
		meanInterval = time.Duration(probeSettings.LoadRetryMeanIntervalSeconds) * time.Second
	}
	checkBudget := egresshealth.Options{Concurrency: 1, LoadAttempts: attempts, LoadRetryMeanInterval: meanInterval}.RunBudget(1)
	if settings.SiteRefreshMaxTime() <= checkBudget {
		err := fmt.Errorf("egress sites: site_refresh_max_time_seconds %d does not cover one candidate host check at the probe's load rules (%s)", settings.SiteRefreshMaxTimeSeconds, checkBudget)
		glog.Errorf("[egresssites]%s; the pool is not refreshed\n", err)
		return nil, err
	}

	seeded, err := controller.EnsureProviderEgressDestinationsSeeded(ctx)
	if err != nil {
		return nil, err
	}
	result.Seeded = seeded
	result.Synced = len(model.SyncProviderEgressDestinationCandidates(ctx, sites.Candidates))

	destinations := model.GetProviderEgressDestinations(ctx)
	windowStart := model.ProviderEgressSiteTallyWindowStart(now, settings.SiteWindow())
	probationStart := now
	probationNames := []string{}
	for _, d := range destinations {
		if d.Active && d.Probation && d.PromotedTime != nil {
			probationNames = append(probationNames, d.Name)
			if d.PromotedTime.Before(probationStart) {
				probationStart = *d.PromotedTime
			}
		}
	}
	retentionStart := model.ProviderEgressSiteTallyWindowStart(now, settings.SiteTallyRetention())
	if probationStart.Before(retentionStart) {
		probationStart = retentionStart
	}
	recent := model.GetProviderEgressHealthClassTotals(ctx, now.Add(-settings.SiteProberFaultWindow()))[""]
	plan := planEgressDestinationRefresh(egressRefreshInput{
		now:             now,
		settings:        settings,
		destinations:    destinations,
		windowTallies:   model.GetProviderEgressSiteTallies(ctx, windowStart),
		probationTotals: model.GetProviderEgressSiteDayTotals(ctx, probationStart, probationNames),
		recent:          recent,
	})
	if plan.skipped != "" {
		glog.Errorf("[egresssites]refresh skipped: %s; retiring sites during a prober fault would empty the pool for nothing\n", plan.skipped)
		result.Skipped = plan.skipped
		model.RemoveExpiredProviderEgressTallies(ctx, retentionStart)
		recordEgressSitePoolMetrics(destinations, nil, now, settings)
		return result, nil
	}

	// the openings, filled from host checks run at once: each opening takes its
	// own candidates in its own order, and no candidate serves two openings
	thin := map[string][]model.ProviderEgressPlace{}
	for _, need := range plan.needs {
		if need.place != nil {
			thin[need.class] = append(thin[need.class], *need.place)
		}
	}
	current := make([]*model.ProviderEgressDestination, 0, len(destinations))
	for _, d := range destinations {
		if row, ok := plan.rows[d.Name]; ok {
			current = append(current, row)
		} else {
			current = append(current, d)
		}
	}
	taken := map[string]bool{}
	// one opening and the candidates checked for it, in promotion order
	type opening struct {
		need       egressPromotionNeed
		candidates []*model.ProviderEgressDestination
	}
	openings := []*opening{}
	for _, need := range plan.needs {
		ordered := orderEgressCandidates(current, need, thin[need.class], now, settings, taken)
		if settings.SiteCandidateChecksPerPromotion < len(ordered) {
			ordered = ordered[:settings.SiteCandidateChecksPerPromotion]
		}
		for _, candidate := range ordered {
			taken[candidate.Name] = true
		}
		openings = append(openings, &opening{need: need, candidates: ordered})
	}
	// every candidate is converted before any check starts: a row the prober
	// could not run is refused here, on this goroutine, so no write to the
	// results ever races the checks' own
	checked := map[string]error{}
	candidateDestinations := []egresshealth.Destination{}
	for _, o := range openings {
		for _, candidate := range o.candidates {
			destination, err := controller.ProviderEgressDestinationToWire(candidate)
			if err != nil {
				checked[candidate.Name] = err
				continue
			}
			candidateDestinations = append(candidateDestinations, destination)
		}
	}
	var stateLock sync.Mutex
	var checks sync.WaitGroup
	for _, destination := range candidateDestinations {
		checks.Add(1)
		go func() {
			defer checks.Done()
			err := check(ctx, destination, sites.Profile, attempts, meanInterval)
			func() {
				stateLock.Lock()
				defer stateLock.Unlock()
				checked[destination.Name] = err
			}()
		}()
	}
	checks.Wait()
	if err := ctx.Err(); err != nil {
		// a drain mid-check proves nothing about the candidates; the judgement
		// is not written half-applied, and the next run starts over
		return nil, err
	}

	promoted := []egressRefreshChange{}
	for _, o := range openings {
		filled := false
		for _, candidate := range o.candidates {
			if err := checked[candidate.Name]; err != nil {
				glog.Infof("[egresssites]candidate %s (%s/%s) not promoted: %s\n", candidate.Name, candidate.Class, candidate.Category, err)
				continue
			}
			row := plan.row(candidate)
			promotedTime := now
			row.Active = true
			row.Probation = true
			row.PromotedTime = &promotedTime
			row.FailureShare = nil
			row.SampleCount = 0
			row.JudgedTime = nil
			row.AboveRetireSince = nil
			promoted = append(promoted, egressRefreshChange{name: candidate.Name, class: candidate.Class, category: candidate.Category, detail: o.need.reason})
			filled = true
			break
		}
		if !filled {
			result.Unfilled++
			glog.Errorf("[egresssites]no candidate for an opening in %s (%s): %d checked, none loaded cleanly; the candidate list needs sites for it\n", o.need.class, o.need.reason, len(o.candidates))
		}
	}

	names := make([]string, 0, len(plan.rows))
	for name := range plan.rows {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		model.SetProviderEgressDestination(ctx, plan.rows[name])
	}
	model.RemoveExpiredProviderEgressTallies(ctx, retentionStart)

	for _, change := range plan.retired {
		glog.Infof("[egresssites]retired %s (%s/%s): %s\n", change.name, change.class, change.category, change.detail)
		egressSiteChangesTotal.WithLabelValues("retired", change.class).Inc()
	}
	for _, change := range promoted {
		glog.Infof("[egresssites]promoted %s (%s/%s) on probation: %s\n", change.name, change.class, change.category, change.detail)
		egressSiteChangesTotal.WithLabelValues("promoted", change.class).Inc()
	}
	for _, change := range plan.graduated {
		glog.Infof("[egresssites]%s (%s/%s) passed probation: %s\n", change.name, change.class, change.category, change.detail)
		egressSiteChangesTotal.WithLabelValues("graduated", change.class).Inc()
	}
	for _, change := range plan.marked {
		glog.Infof("[egresssites]marked %s (%s) incompatible at %s\n", change.name, change.class, change.detail)
		egressSiteChangesTotal.WithLabelValues("marked", change.class).Inc()
	}
	for _, change := range plan.unmarked {
		glog.Infof("[egresssites]unmarked %s (%s) at %s\n", change.name, change.class, change.detail)
		egressSiteChangesTotal.WithLabelValues("unmarked", change.class).Inc()
	}
	if 0 < len(plan.fallbacks) {
		glog.Infof("[egresssites]too few healthy exits to judge %s; judged on the share over all exits\n", strings.Join(plan.fallbacks, ","))
	}

	final := model.GetProviderEgressDestinations(ctx)
	recordEgressSitePoolMetrics(final, plan.judgements, now, settings)
	for _, class := range model.ProviderEgressSiteClasses {
		scored, probation, candidates := egressSitePoolCounts(final, class, now, settings)
		glog.Infof(
			"[egresssites]%s: %d scored, %d on probation, %d candidates remaining (pool size %d)\n",
			class, scored, probation, candidates, settings.SitePoolSize[class],
		)
	}

	result.Judged = len(plan.judgements)
	result.Retired = len(plan.retired)
	result.Promoted = len(promoted)
	result.Graduated = len(plan.graduated)
	result.Marked = len(plan.marked)
	result.Unmarked = len(plan.unmarked)
	return result, nil
}

// One class's scored, probationary and promotable rows.
func egressSitePoolCounts(
	destinations []*model.ProviderEgressDestination,
	class string,
	now time.Time,
	settings *model.ProviderEgressSiteSettings,
) (scored int, probation int, candidates int) {
	for _, d := range destinations {
		if d.Class != class {
			continue
		}
		switch {
		case d.Scored():
			scored++
		case d.Active:
			probation++
		case d.IsCandidate(now, settings.SiteRetireCooldown(), settings.SiteMaxRetirements):
			candidates++
		}
	}
	return scored, probation, candidates
}

// Publishes the pool sizes and, after a refresh that judged, each active
// site's share. The share series are reset first, so a retired site's series
// ends rather than repeating its last share.
func recordEgressSitePoolMetrics(
	destinations []*model.ProviderEgressDestination,
	judgements []egressSiteJudgement,
	now time.Time,
	settings *model.ProviderEgressSiteSettings,
) {
	for _, class := range model.ProviderEgressSiteClasses {
		scored, probation, candidates := egressSitePoolCounts(destinations, class, now, settings)
		egressSitePoolSize.WithLabelValues(class, "scored").Set(float64(scored))
		egressSitePoolSize.WithLabelValues(class, "probation").Set(float64(probation))
		egressSitePoolSize.WithLabelValues(class, "candidates").Set(float64(candidates))
	}
	if judgements == nil {
		return
	}
	egressSiteFailureShare.Reset()
	active := map[string]bool{}
	for _, d := range destinations {
		active[d.Name] = d.Active
	}
	for _, judgement := range judgements {
		if judgement.share != nil && active[judgement.name] {
			egressSiteFailureShare.WithLabelValues(judgement.name, judgement.class).Set(*judgement.share)
		}
	}
}
