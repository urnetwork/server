package model

import (
	"context"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/qualityprobe/egresshealth"
)

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
// (provider_egress_health_history url_source and url_probation). An outcome
// whose destination the storing host cannot place, in neither its catalog's
// country lists nor the pool, is recorded as unrecognized and on probation:
// the ingest fails closed rather than score it as general. An outcome on
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
	// A destination the storing host cannot place: neither one of the
	// country sites of its catalog nor in the general pool. A country site
	// arrives this way while a new catalog version has reached the host that
	// served the prober but not yet the host that stores the outcome (config
	// hosts lag each other by about two minutes), or while the storing host
	// cannot parse the current catalog. Such an outcome is stored on
	// probation, so it fails closed: it can never be scored as a general
	// outcome or reach the Online rule.
	ProviderEgressUrlSourceUnrecognized ProviderEgressUrlSource = 2
)

// Every source, in value order.
var ProviderEgressUrlSources = []ProviderEgressUrlSource{
	ProviderEgressUrlSourceGeneral,
	ProviderEgressUrlSourceCountry,
	ProviderEgressUrlSourceUnrecognized,
}

// The source's metric label.
func (self ProviderEgressUrlSource) String() string {
	switch self {
	case ProviderEgressUrlSourceGeneral:
		return "general"
	case ProviderEgressUrlSourceCountry:
		return "country"
	case ProviderEgressUrlSourceUnrecognized:
		return "unrecognized"
	default:
		return "unknown"
	}
}

// Whether the general pool holds the named destination, by the rule the pool
// is served by (controller ProviderEgressSites.configuredRows): its row's
// active flag (served scored or on probation), or, when it has no row yet,
// whether the storing host's catalog configures it as a destination
// (configured), since a configured destination is served as active until a
// row says otherwise. The seed fills only an empty table and the refresh syncs
// only the candidates, so a destination added to the catalog after the first
// seed has no row. While the catalog cannot be read nothing is configured,
// and rows alone place an outcome. A general outcome is placed by this; one
// that is not, and is not one of the host's country sites, is unrecognized
// (ProviderEgressUrlSourceUnrecognized).
func ProviderEgressDestinationInPool(ctx context.Context, name string, configured bool) bool {
	var inPool bool
	server.Db(ctx, func(conn server.PgConn) {
		server.Raise(conn.QueryRow(ctx, `SELECT COALESCE(
			(SELECT active FROM provider_egress_destination WHERE name = $1),
			$2::boolean
		)`, name, configured).Scan(&inPool))
	})
	return inPool
}

// Puts each destination in the general pool, active and scored, as the pool
// serves a configured destination, so the URL outcomes a test submits through
// the ingest for it are placed as general (ProviderEgressDestinationInPool).
func Testing_ServeProviderEgressDestinations(ctx context.Context, destinations ...egresshealth.Destination) {
	for _, destination := range destinations {
		SetProviderEgressDestination(ctx, &ProviderEgressDestination{
			Name:     destination.Name,
			Class:    string(destination.Class),
			Url:      destination.Url,
			Expect:   "body",
			Category: "reference",
			Region:   "global",
			Source:   ProviderEgressDestinationSourceBuiltin,
			Active:   true,
		})
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
