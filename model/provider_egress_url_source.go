package model

// Where a URL outcome's destination came from, and whether it scores (D3,
// REVIEW-INDEPENDENT.md; db_provider_egress_url_source.go).
//
// A URL turn draws its one destination from the general catalog, or, on half
// of its normal turns, from the provider's country list when one is served. A
// general destination promoted from the candidates serves on probation until
// the pool refresh graduates it: it is loaded and recorded, but it has to earn
// the right to cost a provider a tier (GEOMAP §11.4). A country site is in the
// same position until the pool refresh graduates it on paired evidence, and
// returns to it when demoted (provider_egress_country_site.go).
//
// The operator ingest decides both at the store, from the catalog, the pool
// and the country sites' states as they stand, and records them beside the
// outcome
// (provider_egress_health_history url_source and url_probation). An outcome on
// probation is a measured run like any other: it is in the history with its
// outcome and in the rolling quota, which paces turns by measured runs. What
// it may not do is count: the eight-hour window every bucket, the index and
// the Online rule read leaves it out (providerEgressHealthWindowSql), and so do
// the scored loads the fleet gauges and the pool refresh read.

// The catalog a URL outcome's destination came from, as
// provider_egress_health_history.url_source stores it.
type ProviderEgressUrlSource int16

const (
	// The general catalog: the configured destinations and the promoted
	// candidates the pool serves to every provider. Every outcome stored
	// before the source was recorded, and every one a release that does not
	// record it stores, is general, which is how it was counted.
	ProviderEgressUrlSourceGeneral ProviderEgressUrlSource = 0
	// The country list served for the provider's published country.
	ProviderEgressUrlSourceCountry ProviderEgressUrlSource = 1
)

// Every source, in value order.
var ProviderEgressUrlSources = []ProviderEgressUrlSource{
	ProviderEgressUrlSourceGeneral,
	ProviderEgressUrlSourceCountry,
}

// The source's metric label.
func (self ProviderEgressUrlSource) String() string {
	switch self {
	case ProviderEgressUrlSourceGeneral:
		return "general"
	case ProviderEgressUrlSourceCountry:
		return "country"
	default:
		return "unknown"
	}
}

// How the store counted one measured URL outcome.
type ProviderEgressHealthStored struct {
	// Stored is false for a run id already stored: the replay stored and
	// counted nothing.
	Stored bool
	// Recheck is a security recheck of a URL with an open TLS finding: a
	// security-only receipt, out of the eight-hour window and the rolling
	// quota alike (provider_egress_tls_verdict.go).
	Recheck bool
	// Probation is an outcome on a destination that had not graduated into
	// scoring: in the history and the rolling quota, out of the eight-hour
	// window and every scored total.
	Probation bool
}
