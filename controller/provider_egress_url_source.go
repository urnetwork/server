package controller

import (
	"context"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/urnetwork/glog"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/qualityprobe/egresshealth"
)

// The catalog source of each URL outcome, decided at ingest (D3,
// model/provider_egress_url_source.go).
//
// The prober knows which list a turn drew from, but the wire carries only the
// destination, and the operator API refuses unknown fields: a prober that sent
// a new field would be refused by every server not yet rolled. The source is
// therefore derived here, from the served catalog, which is what decided the
// draw: a destination named in a country list is a country site (the parse
// keeps country sites disjoint from every general entry by name and website,
// ParseProviderEgressSites), anything else general. The store decides
// probation from the pool and from each country site's graduation state
// (model/provider_egress_country_site.go).

// The country site names of one qualityprobe.yml version, each with the
// countries whose lists hold it, in order.
type providerEgressUrlSources struct {
	countrySites map[string][]string
}

// The catalog's source for the outcome's destination, and for a country site
// the countries whose lists hold it.
func (self *providerEgressUrlSources) of(destination egresshealth.Destination) (model.ProviderEgressUrlSource, []string) {
	if self != nil {
		if countryCodes := self.countrySites[destination.Name]; 0 < len(countryCodes) {
			return model.ProviderEgressUrlSourceCountry, countryCodes
		}
	}
	return model.ProviderEgressUrlSourceGeneral, nil
}

// The country a country outcome's destination came from: its one list, or
// for a site several countries list, the provider's published country when it
// is one of them (the list the provider was served), else the first.
func providerEgressUrlCountryOf(ctx context.Context, clientId server.Id, countryCodes []string) string {
	switch len(countryCodes) {
	case 0:
		return ""
	case 1:
		return countryCodes[0]
	}
	published := strings.ToLower(strings.TrimSpace(model.GetProviderEgressPlaces(ctx, []server.Id{clientId})[clientId].CountryCode))
	if slices.Contains(countryCodes, published) {
		return published
	}
	return countryCodes[0]
}

// One qualityprobe.yml version's parse: its country sites, or when it failed
// to parse.
type providerEgressUrlSourcesLoad struct {
	sources  *providerEgressUrlSources
	failedAt time.Time
}

// How long a catalog version that failed to parse is left before it is parsed
// again. Every URL outcome reads the catalog, so a version that does not
// parse must not be parsed again for each of them.
const providerEgressUrlSourcesRetry = time.Minute

// The parse of the current qualityprobe.yml, reused while its version is
// unchanged, so the ingest parses the catalog once per change rather than once
// per outcome.
var providerEgressUrlSourcesCache server.ResourceValueCache[server.ResourceVersion, providerEgressUrlSourcesLoad]

// The catalog parse; a test counts its calls.
var providerEgressUrlSourcesParse = ParseProviderEgressSites

// When a catalog that could not be read was last logged; one line a minute.
var providerEgressUrlSourcesUnreadable = struct {
	sync.Mutex
	loggedAt time.Time
}{}

// The country sites of the served catalog. A catalog that cannot be read
// serves no pool either (GetProviderEgressDestinationPool), so no prober draws
// a country site from it; its outcomes are general, as every outcome was
// before the source was recorded.
func getProviderEgressUrlSources() *providerEgressUrlSources {
	resource, err := server.Config.SimpleResource(model.ProviderEgressSitesResourceName)
	if err != nil {
		logProviderEgressUrlSourcesUnreadable(err)
		return nil
	}
	current := func(load providerEgressUrlSourcesLoad) bool {
		return load.sources != nil || time.Since(load.failedAt) < providerEgressUrlSourcesRetry
	}
	return providerEgressUrlSourcesCache.Get(resource.Version(), current, func() (providerEgressUrlSourcesLoad, bool) {
		// the pool's own parse, without counting its refusals again: the
		// pool request records those for the same version
		sites, err := providerEgressUrlSourcesParse(resource.UnmarshalYamlE)
		if err != nil {
			logProviderEgressUrlSourcesUnreadable(err)
			return providerEgressUrlSourcesLoad{failedAt: time.Now()}, true
		}
		sources := &providerEgressUrlSources{countrySites: map[string][]string{}}
		for _, countryCode := range providerEgressCountryCodes(sites.Countries) {
			for _, destination := range sites.Countries[countryCode] {
				sources.countrySites[destination.Name] = append(sources.countrySites[destination.Name], countryCode)
			}
		}
		return providerEgressUrlSourcesLoad{sources: sources}, true
	}).sources
}

func logProviderEgressUrlSourcesUnreadable(err error) {
	providerEgressUrlSourcesUnreadable.Lock()
	due := time.Minute <= time.Since(providerEgressUrlSourcesUnreadable.loggedAt)
	if due {
		providerEgressUrlSourcesUnreadable.loggedAt = time.Now()
	}
	providerEgressUrlSourcesUnreadable.Unlock()
	if due {
		glog.Errorf("[egress]the qualityprobe.yml catalog cannot be read (%v); URL outcomes are stored as general until it can\n", err)
	}
}

// How the store counted a measured URL outcome, as the outcome counter labels
// it.
const (
	providerEgressUrlScoringScored          = "scored"
	providerEgressUrlScoringProbation       = "probation"
	providerEgressUrlScoringSecurityRecheck = "security_recheck"
)

var providerEgressUrlOutcomesTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
	Namespace: "urnetwork",
	Subsystem: "egress_url",
	Name:      "outcomes_total",
	Help:      "Measured URL outcomes the operator ingest stored, by the catalog their destination came from (source: general or country), how they counted (scoring: scored, in the eight-hour window; probation, a country site or a general site the refresh has not graduated, recorded and in the rolling quota but never scored; security_recheck, a recheck of a URL with an open TLS finding, out of both) and outcome (success or error)",
}, []string{"source", "scoring", "outcome"})

// Registers the outcome counter with every label set it can produce.
func init() {
	for _, source := range model.ProviderEgressUrlSources {
		for _, scoring := range []string{providerEgressUrlScoringScored, providerEgressUrlScoringProbation, providerEgressUrlScoringSecurityRecheck} {
			for _, outcome := range []string{"success", "error"} {
				providerEgressUrlOutcomesTotal.WithLabelValues(source.String(), scoring, outcome)
			}
		}
	}
	prometheus.MustRegister(providerEgressUrlOutcomesTotal)
}

// Counts one stored URL outcome under its source and how the store counted
// it. A replay stored nothing and counts nothing; a security-only receipt has
// no outcome.
func recordProviderEgressUrlOutcome(health *model.ProviderEgressHealth, stored model.ProviderEgressHealthStored) {
	if !stored.Stored || health.UrlProbeEvidence == nil || health.Total != 1 {
		return
	}
	scoring := providerEgressUrlScoringScored
	switch {
	case stored.Recheck:
		scoring = providerEgressUrlScoringSecurityRecheck
	case stored.Probation:
		scoring = providerEgressUrlScoringProbation
	}
	outcome := "error"
	if 0 < health.OKCount {
		outcome = "success"
	}
	providerEgressUrlOutcomesTotal.WithLabelValues(health.UrlSource.String(), scoring, outcome).Inc()
}
