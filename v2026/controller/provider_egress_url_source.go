package controller

import (
	"context"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/urnetwork/glog/v2026"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
	"github.com/urnetwork/server/v2026/qualityprobe/egresshealth"
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
// ParseProviderEgressSites), and one the pool holds is general, by the rule
// the pool is served by: an active row, or a destination the catalog
// configures that has no row yet (model.ProviderEgressDestinationInPool).
// One that is neither is unrecognized and stored on probation: this host
// cannot place it, so it fails closed rather than count as general (a country
// site from a catalog version this host has not received yet, or from one it
// cannot parse). The store decides probation from the pool and from each
// country site's graduation state (model/provider_egress_country_site.go).

// The country site names of one qualityprobe.yml version, each with the
// countries whose lists hold it, in order, and the names it configures as
// general destinations.
type providerEgressUrlSources struct {
	countrySites map[string][]string
	destinations map[string]bool
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

// Whether the catalog configures the name as a general destination. The pool
// serves a configured destination that has no pool row yet as active
// (ProviderEgressSites.configuredRows): the seed fills only an empty table,
// and the refresh syncs only the candidates, so a destination added to the
// catalog later has none. A catalog that cannot be read configures nothing.
func (self *providerEgressUrlSources) configured(name string) bool {
	return self != nil && self.destinations[name]
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
// from it; but a prober can still hold a pool drawn from an earlier version,
// so while it cannot be read only the pool places an outcome, and a country
// site's outcome is unrecognized, on probation.
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
		sources := &providerEgressUrlSources{countrySites: map[string][]string{}, destinations: map[string]bool{}}
		for _, countryCode := range providerEgressCountryCodes(sites.Countries) {
			for _, destination := range sites.Countries[countryCode] {
				sources.countrySites[destination.Name] = append(sources.countrySites[destination.Name], countryCode)
			}
		}
		for _, destination := range sites.Destinations {
			sources.destinations[destination.Name] = true
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
		glog.Errorf("[egress]the qualityprobe.yml catalog cannot be read (%v); until it can, only the pool places URL outcomes, and a country site's are stored unrecognized, on probation\n", err)
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
	Help:      "Measured URL outcomes the operator ingest stored, by the catalog their destination came from (source: general or country; unrecognized, in neither the storing host's country lists nor the pool, which fails closed on probation), how they counted (scoring: scored, in the eight-hour window; probation, a country site or a general site the refresh has not graduated, or an unrecognized destination, recorded and in the rolling quota but never scored; security_recheck, a recheck of a URL with an open TLS finding, out of both) and outcome (success or error)",
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
	if leg := health.UrlProbeEvidence.H3; leg != nil && leg.Verdict != "" {
		providerEgressUrlH3VerdictsTotal.WithLabelValues(leg.Verdict).Inc()
	}
	if leg := health.UrlProbeEvidence.Ipv6; leg != nil {
		if leg.Verdict != "" {
			providerEgressUrlIpv6VerdictsTotal.WithLabelValues(model.ProviderEgressIpv6PlaceTcp, leg.Verdict).Inc()
		}
		if leg.H3 != nil && leg.H3.Verdict != "" {
			providerEgressUrlIpv6VerdictsTotal.WithLabelValues(model.ProviderEgressIpv6PlaceUdp, leg.H3.Verdict).Inc()
		}
	}
	if mask := stored.FailedLaterLegs; mask != 0 {
		// the stored mask's legs, in bit order: v4udp, v6tcp, v6udp
		legs := []string{}
		for _, bit := range providerEgressUrlFailedLaterLegBits {
			if mask&bit.mask != 0 {
				legs = append(legs, bit.label)
			}
		}
		providerEgressUrlFailedLaterLegsTotal.WithLabelValues(strings.Join(legs, "_")).Inc()
	}
	if health.UrlProbeEvidence.E2e != nil {
		pq := egresshealth.UrlProbePqUnmeasured
		switch stored.PqOutcome {
		case egresshealth.UrlProbePqOutcomePositive:
			pq = egresshealth.UrlProbePqPositive
		case egresshealth.UrlProbePqOutcomeNegative:
			pq = egresshealth.UrlProbePqNegative
		}
		providerEgressUrlPqOutcomesTotal.WithLabelValues(pq).Inc()
	}
}

// Each failed later leg's bit and its label in the failed later legs counter.
var providerEgressUrlFailedLaterLegBits = []struct {
	mask  int16
	label string
}{
	{mask: egresshealth.UrlProbeFailedLaterLegIpv4Udp, label: "v4udp"},
	{mask: egresshealth.UrlProbeFailedLaterLegIpv6Tcp, label: "v6tcp"},
	{mask: egresshealth.UrlProbeFailedLaterLegIpv6Udp, label: "v6udp"},
}

// The stored failed outcomes whose later legs the ingest judged the
// provider's (FP2-PROTOCOL.md §3.2, §4.1).
var providerEgressUrlFailedLaterLegsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
	Namespace: "urnetwork",
	Subsystem: "egress_url",
	Name:      "failed_later_legs_total",
	Help:      "Stored URL outcomes that failed only on later legs the ingest's place verdict held against the provider, by the legs (failed_later_legs mask): v4udp, the IPv4 HTTP/3 leg was silent; v6tcp, the IPv6 HTTPS leg failed; v6udp, the IPv6 HTTP/3 leg was silent; v4udp_v6tcp and v4udp_v6udp, both. Each is negative evidence on the protocol axis (udp) or the IP axis (v6tcp), which capability-aware FindProviders2 classifies on instead of excluding",
}, []string{"legs"})

// The post-quantum outcomes of stored URL turns that opened an end-to-end
// session (FP2-PROTOCOL.md §12.3).
var providerEgressUrlPqOutcomesTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
	Namespace: "urnetwork",
	Subsystem: "egress_url",
	Name:      "pq_outcomes_total",
	Help:      "Stored URL outcomes of post-quantum turns, by the pq_outcome the store kept: positive, the session negotiated a post-quantum group, sealed before the IPv4 HTTPS leg's request and carried that passed leg; negative, a classical group, a post-quantum session unsealed by the run's end, or no responder for a whole establishment window; unmeasured, anything else, a security recheck or an outcome on probation included. Strict post-quantum selection needs a positive outcome in the eight-hour window",
}, []string{"pq"})

func init() {
	for _, legs := range []string{"v4udp", "v6tcp", "v6udp", "v4udp_v6tcp", "v4udp_v6udp"} {
		providerEgressUrlFailedLaterLegsTotal.WithLabelValues(legs)
	}
	for _, pq := range []string{egresshealth.UrlProbePqPositive, egresshealth.UrlProbePqNegative, egresshealth.UrlProbePqUnmeasured} {
		providerEgressUrlPqOutcomesTotal.WithLabelValues(pq)
	}
	prometheus.MustRegister(providerEgressUrlFailedLaterLegsTotal, providerEgressUrlPqOutcomesTotal)
}

// The place verdicts of stored failed IPv6 legs (model.JudgeProviderEgressLegs).
var providerEgressUrlIpv6VerdictsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
	Namespace: "urnetwork",
	Subsystem: "egress_url",
	Name:      "ipv6_verdicts_total",
	Help:      "Stored URL outcomes of dual-stack providers whose IPv6 leg failed (transport tcp: the HTTPS leg over the URL's AAAA record failed; udp: its HTTP/3 leg over IPv6 was silent), by the ingest's place verdict: provider, where at least three other established networks in the provider's country reached the same URL over IPv6 (over QUIC, for udp) that day or the day before, so the outcome fails; place, where they did not, so the outcome counts as its IPv4 legs",
}, []string{"transport", "verdict"})

// The place verdicts of stored silent HTTP/3 legs (model.JudgeProviderEgressH3).
var providerEgressUrlH3VerdictsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
	Namespace: "urnetwork",
	Subsystem: "egress_url",
	Name:      "h3_verdicts_total",
	Help:      "Stored URL outcomes whose HTTP/3 leg was silent (nothing answered its QUIC handshake), by the ingest's place verdict: provider, where at least three other established networks in the provider's country reached the same URL over QUIC that day or the day before, so the outcome fails; place, where they did not, so the outcome counts as its HTTPS leg",
}, []string{"verdict"})

func init() {
	for _, verdict := range []string{egresshealth.UrlProbeH3VerdictProvider, egresshealth.UrlProbeH3VerdictPlace} {
		providerEgressUrlH3VerdictsTotal.WithLabelValues(verdict)
		for _, transport := range []string{model.ProviderEgressIpv6PlaceTcp, model.ProviderEgressIpv6PlaceUdp} {
			providerEgressUrlIpv6VerdictsTotal.WithLabelValues(transport, verdict)
		}
	}
	prometheus.MustRegister(providerEgressUrlH3VerdictsTotal, providerEgressUrlIpv6VerdictsTotal)
}
