package controller

import (
	"fmt"
	"net"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"golang.org/x/net/publicsuffix"

	"github.com/urnetwork/glog/v2026"
	"github.com/urnetwork/server/v2026/qualityprobe/egresshealth"

	"github.com/urnetwork/server/v2026/model"
)

// The destination catalog's disjointness (connect/GEOMAP.md §11.4). A country
// list measures websites the general catalog does not: the prober refuses a
// whole pool whose country lists repeat a general entry's name or host
// (egresshealth.ValidatePool), so one stage owning the rule and another
// skipping it turned one colliding entry into a fleet-wide pool refusal. The
// parse, the served pool and the refresh apply the one rule below, and each
// keeps the colliding entry out rather than refusing the pool: absence of a
// country site is a coverage gap, never a reason to stop measuring.

// ProviderEgressSiteKey names the website a destination URL loads, the unit
// the catalog's lists are kept disjoint by: the registrable domain (the Public
// Suffix List's eTLD+1) of its lower-cased host, without a leading "www." or a
// trailing dot. www.example.co.uk, shop.example.co.uk and example.co.uk are
// one site; example.co.uk never collapses to co.uk; a.github.io and b.github.io
// are two. A host with no registrable domain -- an IP literal, or a public
// suffix itself -- is its own site. The country-site signal of SIGNALS.md
// §2.19d judges overlap by the same key.
//
// The key is a function of the prober's own overlap key (the host without
// "www."), so lists disjoint by it are disjoint by the prober's rule too, and
// a pool the server serves is never one the prober refuses for an overlap.
func ProviderEgressSiteKey(rawUrl string) string {
	u, err := url.Parse(rawUrl)
	if err != nil {
		return strings.ToLower(strings.TrimSpace(rawUrl))
	}
	host := strings.TrimSuffix(strings.TrimPrefix(strings.ToLower(u.Hostname()), "www."), ".")
	if host == "" || net.ParseIP(host) != nil {
		return host
	}
	if key, err := publicsuffix.EffectiveTLDPlusOne(host); err == nil {
		return key
	}
	return host
}

// The reasons a catalog entry is kept out of the served pool, the bounded
// reason label of urnetwork_egress_site_catalog_refusals_total.
const (
	// a candidate repeating a destination's name: the destination's contract
	// is authoritative and is never replaced
	ProviderEgressRefusalCandidateName = "candidate_name"
	// a country site repeating a general entry's name or website
	ProviderEgressRefusalCountryConflict = "country_conflict"
	// a country entry, or a whole list under a key that is not a country
	// code, the prober could not run; or a country_settings entry under such
	// a key, or repeating a country
	ProviderEgressRefusalCountryInvalid = "country_invalid"
)

// The stages that keep an entry out, the stage label.
const (
	// reading qualityprobe.yml (ParseProviderEgressSites)
	ProviderEgressRefusalStageParse = "parse"
	// building one served pool (GetProviderEgressDestinationPool)
	ProviderEgressRefusalStageServe = "serve"
	// the refresh holding a candidate back from promotion
	ProviderEgressRefusalStageRefresh = "refresh"
)

// One catalog entry kept out of the served pool, and why. Names and country
// codes are the catalog's own bounded identifiers.
type ProviderEgressCatalogRefusal struct {
	Reason string
	// Country is the country list the entry is in; empty for a candidate
	Country string
	Name    string
	// Detail says what the entry collides with or why it cannot run
	Detail string
	// List names the file's key the entry is under when that is neither the
	// candidates nor a country list: country_settings
	List string
}

func (self ProviderEgressCatalogRefusal) String() string {
	list := "candidates"
	if self.Country != "" {
		list = "countries." + self.Country
	}
	if self.List != "" {
		list = self.List
	}
	return fmt.Sprintf("%s entry %q %s", list, self.Name, self.Detail)
}

var providerEgressCatalogRefusalsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
	Namespace: "urnetwork",
	Subsystem: "egress_site",
	Name:      "catalog_refusals_total",
	Help:      "qualityprobe.yml entries kept out of the served destination pool, by stage (parse: refused reading the catalog, once per read; serve: dropped from one served pool; refresh: a candidate held back from promotion) and reason (candidate_name: a candidate repeating a destination's name; country_conflict: a country site repeating a general entry's name or registrable domain, or a candidate one would collide with; country_invalid: a country entry or list the prober could not run, or a country_settings entry under a key that is not a country code or that repeats a country)",
}, []string{"stage", "reason"})

// Registers the refusal counter with every label pair a stage can produce.
func init() {
	for _, reason := range []string{ProviderEgressRefusalCandidateName, ProviderEgressRefusalCountryConflict, ProviderEgressRefusalCountryInvalid} {
		providerEgressCatalogRefusalsTotal.WithLabelValues(ProviderEgressRefusalStageParse, reason)
	}
	providerEgressCatalogRefusalsTotal.WithLabelValues(ProviderEgressRefusalStageServe, ProviderEgressRefusalCountryConflict)
	providerEgressCatalogRefusalsTotal.WithLabelValues(ProviderEgressRefusalStageRefresh, ProviderEgressRefusalCountryConflict)
	prometheus.MustRegister(providerEgressCatalogRefusalsTotal)
}

// The most refusals one log line names; the count says how many more.
const providerEgressRefusalLogListed = 20

// How often an unchanged set of refusals is logged again. The api reads the
// catalog on every pool request, and the counter carries every one.
const providerEgressRefusalLogInterval = 10 * time.Minute

// The last refusal set logged per stage, and when.
var providerEgressRefusalLog = struct {
	sync.Mutex
	logged map[string]string
	at     map[string]time.Time
}{logged: map[string]string{}, at: map[string]time.Time{}}

// Counts each refusal under its stage and logs the set: when it changes, and
// otherwise at most once per providerEgressRefusalLogInterval. A set that
// empties is logged once, so the log says when a fix took effect.
func RecordProviderEgressCatalogRefusals(stage string, refusals []ProviderEgressCatalogRefusal) {
	for _, refusal := range refusals {
		providerEgressCatalogRefusalsTotal.WithLabelValues(stage, refusal.Reason).Inc()
	}
	listed := make([]string, 0, min(len(refusals), providerEgressRefusalLogListed))
	for i, refusal := range refusals {
		if providerEgressRefusalLogListed <= i {
			listed = append(listed, fmt.Sprintf("and %d more", len(refusals)-i))
			break
		}
		listed = append(listed, refusal.String())
	}
	text := strings.Join(listed, "; ")

	now := time.Now()
	providerEgressRefusalLog.Lock()
	previous, seen := providerEgressRefusalLog.logged[stage]
	due := text != previous || (text != "" && providerEgressRefusalLogInterval <= now.Sub(providerEgressRefusalLog.at[stage]))
	if due {
		providerEgressRefusalLog.logged[stage] = text
		providerEgressRefusalLog.at[stage] = now
	}
	providerEgressRefusalLog.Unlock()

	switch {
	case !due:
	case 0 < len(refusals):
		glog.Errorf("[egresssites]%s: %d qualityprobe.yml entries kept out of the served pool: %s\n", stage, len(refusals), text)
	case seen:
		glog.Infof("[egresssites]%s: no qualityprobe.yml entry is kept out of the served pool any longer\n", stage)
	}
}

// The general entries a country site must not repeat: their names, and the
// websites they load with the name of the entry that loads each.
type providerEgressGeneralIndex struct {
	names map[string]bool
	sites map[string]string
}

func newProviderEgressGeneralIndex() *providerEgressGeneralIndex {
	return &providerEgressGeneralIndex{names: map[string]bool{}, sites: map[string]string{}}
}

func (self *providerEgressGeneralIndex) add(name string, rawUrl string) {
	self.names[name] = true
	if key := ProviderEgressSiteKey(rawUrl); key != "" {
		if _, ok := self.sites[key]; !ok {
			self.sites[key] = name
		}
	}
}

// Why a country site collides with a general entry, or "".
func (self *providerEgressGeneralIndex) conflict(name string, rawUrl string) string {
	if self.names[name] {
		return fmt.Sprintf("repeats the name of general entry %q", name)
	}
	key := ProviderEgressSiteKey(rawUrl)
	if general, ok := self.sites[key]; ok {
		return fmt.Sprintf("shares the website %s with general entry %q", key, general)
	}
	return ""
}

// The country codes of lists, in order, so refusals and logs are stable.
func providerEgressCountryCodes[T any](lists map[string]T) []string {
	countries := make([]string, 0, len(lists))
	for country := range lists {
		countries = append(countries, country)
	}
	sort.Strings(countries)
	return countries
}

// Whether a country list key is one the prober matches a provider's country
// to: a lower-case alpha-2 code (egresshealth.ValidatePool).
func providerEgressCountryKeyValid(country string) bool {
	return len(country) == 2 && 'a' <= country[0] && country[0] <= 'z' && 'a' <= country[1] && country[1] <= 'z'
}

// The country lists served beside the general pool: every country site that
// collides with none of the served general entries, and the refusals of those
// that do. The parse has already kept out every site that collides with any
// configured destination or candidate, so this drops nothing unless the
// served general rows and the parsed catalog have come apart; it is what keeps
// a pool from being refused whole if they ever do.
func (self *ProviderEgressSites) servedCountries(general []egresshealth.Destination) (map[string][]egresshealth.Destination, []ProviderEgressCatalogRefusal) {
	index := newProviderEgressGeneralIndex()
	for _, destination := range general {
		index.add(destination.Name, destination.Url)
	}
	countries := map[string][]egresshealth.Destination{}
	refusals := []ProviderEgressCatalogRefusal{}
	for _, country := range providerEgressCountryCodes(self.Countries) {
		for _, destination := range self.Countries[country] {
			if conflict := index.conflict(destination.Name, destination.Url); conflict != "" {
				refusals = append(refusals, ProviderEgressCatalogRefusal{
					Reason:  ProviderEgressRefusalCountryConflict,
					Country: country,
					Name:    destination.Name,
					Detail:  conflict + " in the served pool",
				})
				continue
			}
			countries[country] = append(countries[country], destination)
		}
	}
	return countries, refusals
}

// Whether promoting a stored row would serve a general entry that collides
// with a country site, and with which: the refresh holds such a candidate
// back. The entry served under a promoted row's name is the one configuredRows
// serves -- the destination of that name, else the candidate -- and a name the
// file no longer lists is not served at all. Every country site the file
// lists counts, those the parse refused included, so a candidate is held back
// even while an api that predates the fail-small pool still serves the site.
func (self *ProviderEgressSites) PromotionConflict(row *model.ProviderEgressDestination) (ProviderEgressCatalogRefusal, bool) {
	var served *model.ProviderEgressDestination
	for _, rows := range [][]*model.ProviderEgressDestination{self.Destinations, self.Candidates} {
		for _, configured := range rows {
			if served == nil && configured.Name == row.Name {
				served = configured
			}
		}
	}
	if served == nil {
		return ProviderEgressCatalogRefusal{}, false
	}
	index := newProviderEgressGeneralIndex()
	index.add(served.Name, served.Url)
	conflicts := []string{}
	for _, country := range providerEgressCountryCodes(self.listedCountries) {
		for _, destination := range self.listedCountries[country] {
			if index.names[destination.Name] {
				conflicts = append(conflicts, fmt.Sprintf("countries.%s entry %q repeats its name", country, destination.Name))
			} else if key := ProviderEgressSiteKey(destination.Url); index.sites[key] != "" {
				conflicts = append(conflicts, fmt.Sprintf("countries.%s entry %q shares its website %s", country, destination.Name, key))
			}
		}
	}
	if len(conflicts) == 0 {
		return ProviderEgressCatalogRefusal{}, false
	}
	return ProviderEgressCatalogRefusal{
		Reason: ProviderEgressRefusalCountryConflict,
		Name:   row.Name,
		Detail: "is held back from promotion: " + strings.Join(conflicts, ", "),
	}, true
}
