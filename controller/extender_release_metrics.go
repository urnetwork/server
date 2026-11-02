package controller

import (
	"github.com/prometheus/client_golang/prometheus"

	"github.com/urnetwork/connect"
)

// The release metrics (connect/EXTENDER-UPDATE2.md §1.1, rollout gates): how
// each `POST /network/extender-release` ended, by the families the client can
// dial and whether it declared the camouflage capability, and separately by
// the caller's country. They are what the gated-preference rollout reads for
// release denials, fleet capacity by country and family, and compatible
// client coverage.
//
// Every label value is a constant here, or a country code reduced to the
// two-letter set, never something a caller sent. The first counter's series
// are created at zero, so a rate is defined before the first release.

// The families a release request asked for (ExtenderReleaseArgs.IpVersions).
const (
	// empty, or both families
	extenderReleaseFamilyAny  = "any"
	extenderReleaseFamilyIpv4 = "ipv4"
	extenderReleaseFamilyIpv6 = "ipv6"
	// refused before its families were read
	extenderReleaseFamilyUnknown = "unknown"
)

// Every outcome a release can end in, connect's and the route's.
var extenderReleaseOutcomes = []string{
	connect.ExtenderReleaseOutcomeOk,
	connect.ExtenderReleaseOutcomeEmpty,
	connect.ExtenderReleaseOutcomeEpochSpent,
	connect.ExtenderReleaseOutcomeProbation,
	connect.ExtenderReleaseOutcomeCountryCapacity,
	connect.ExtenderReleaseOutcomeRateLimited,
	connect.ExtenderReleaseOutcomeIncompatible,
	connect.ExtenderReleaseOutcomeUnavailable,
}

// The country label of an unlocated caller, or a request refused before its
// caller was located.
const extenderReleaseCountryUnknown = "unknown"

type extenderReleaseMetrics struct {
	releases        *prometheus.CounterVec
	countryOutcomes *prometheus.CounterVec
}

func newExtenderReleaseMetrics(registerer prometheus.Registerer) *extenderReleaseMetrics {
	metrics := &extenderReleaseMetrics{
		releases: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "urnetwork_extender_releases_total",
			Help: "Gated extender releases (POST /network/extender-release) by outcome, the address families the client can dial (any, ipv4, ipv6; unknown when refused before they were read) and whether it declared tcp-camouflage-v1 (capable). " +
				"Outcomes: ok, records released; empty, nothing eligible; epoch_spent and probation, the identity's records for the epoch are spent and none is eligible now; country_capacity, every eligible record is at its per-country limit; " +
				"rate_limited, an identity or vantage request limit; incompatible, only records requiring an undeclared capability were eligible; unavailable, the configuration, the ledger or the request.",
		}, []string{"outcome", "family", "capable"}),
		countryOutcomes: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "urnetwork_extender_release_country_outcomes_total",
			Help: "Gated extender releases by outcome and the caller's country, a lower case ISO 3166 alpha-2 code (unknown when unlocated or refused before it was located).",
		}, []string{"outcome", "country"}),
	}
	for _, outcome := range extenderReleaseOutcomes {
		for _, family := range []string{
			extenderReleaseFamilyAny,
			extenderReleaseFamilyIpv4,
			extenderReleaseFamilyIpv6,
			extenderReleaseFamilyUnknown,
		} {
			for _, capable := range []string{"false", "true"} {
				metrics.releases.WithLabelValues(outcome, family, capable)
			}
		}
	}
	registerer.MustRegister(metrics.releases, metrics.countryOutcomes)
	return metrics
}

var defaultExtenderReleaseMetrics = newExtenderReleaseMetrics(prometheus.DefaultRegisterer)

// Counts one release. Nil counts nothing.
func (self *extenderReleaseMetrics) observe(outcome string, family string, capable bool, countryCode string) {
	if self == nil {
		return
	}
	capableLabel := "false"
	if capable {
		capableLabel = "true"
	}
	self.releases.WithLabelValues(outcome, family, capableLabel).Inc()
	self.countryOutcomes.WithLabelValues(outcome, extenderReleaseCountryLabel(countryCode)).Inc()
}

// The family label of the requested families, which have been checked to be 4
// or 6.
func extenderReleaseFamily(ipVersions []int) string {
	ipv4 := false
	ipv6 := false
	for _, ipVersion := range ipVersions {
		switch ipVersion {
		case 4:
			ipv4 = true
		case 6:
			ipv6 = true
		}
	}
	switch {
	case ipv4 && !ipv6:
		return extenderReleaseFamilyIpv4
	case ipv6 && !ipv4:
		return extenderReleaseFamilyIpv6
	default:
		return extenderReleaseFamilyAny
	}
}

// A country code as a label: two lower case ascii letters, or unknown, so the
// set is bounded whatever a location source returns.
func extenderReleaseCountryLabel(countryCode string) string {
	if len(countryCode) != 2 {
		return extenderReleaseCountryUnknown
	}
	label := []byte(countryCode)
	for i, c := range label {
		switch {
		case 'a' <= c && c <= 'z':
		case 'A' <= c && c <= 'Z':
			label[i] = c - 'A' + 'a'
		default:
			return extenderReleaseCountryUnknown
		}
	}
	return string(label)
}
