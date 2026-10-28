// The reads the capability probe (server/capprobe) selects its providers
// with: a bounded page of its sweep over the connected providers, and which
// of a page's due providers the egress rules keep in Online, as the score
// export decides it. The probe measures end-to-end post-quantum encryption
// and resizable transfer windows, the evidence a provider is modern by
// (egress_modern_provider.go): modern-first priority and modern-only requests
// read it (FP2-MODERN.md), and so does the modern provider gate while
// provider.yml switches it on. It records what it saw with
// RecordProviderModernEvidence; it never reads or writes URL evidence, the
// URL due queue or its quota.
package model

import (
	"context"
	"time"

	"github.com/urnetwork/server"
)

// The most providers one sweep page reads.
const ProviderCapabilityProbeMaxPageSize = 4096

// One provider of a sweep page.
type ProviderCapabilityProbeCandidate struct {
	ClientId server.Id
	// An active top-level client holding a public provide key, with no
	// evidence row or one measured before its refresh time. The rest of the
	// page is read so the sweep can move past it.
	Due bool
}

// The page reads the sweep's population in client id order through
// network_client_location_reliability's (valid, connected, client_id) index,
// so it costs at most pageSize index entries and three primary key probes
// each, however much of the fleet is fresh. A modern row (post-quantum and
// resizable windows) is due once measured before $4, any other row once
// measured before $5.
const providerCapabilityProbePageSql = `
	WITH page AS MATERIALIZED (
		SELECT client_id
		FROM network_client_location_reliability
		WHERE valid AND connected AND client_id > $1
		ORDER BY client_id
		LIMIT $2
	)
	SELECT
		page.client_id,
		COALESCE(
			network_client.active AND
			network_client.source_client_id IS NULL AND
			EXISTS (
				SELECT 1 FROM provide_key
				WHERE provide_key.client_id = page.client_id AND provide_key.provide_mode = $3
			) AND (
				provider_modern_evidence.client_id IS NULL OR
				provider_modern_evidence.measured_at < CASE
					WHEN provider_modern_evidence.post_quantum AND provider_modern_evidence.window_resizable THEN $4::timestamp
					ELSE $5::timestamp
				END
			),
			false
		) AS due
	FROM page
	LEFT JOIN network_client ON network_client.client_id = page.client_id
	LEFT JOIN provider_modern_evidence ON provider_modern_evidence.client_id = page.client_id
	ORDER BY page.client_id
`

// Up to pageSize connected, valid providers after afterClientId, in client id
// order, each with whether its evidence is due: a modern row measured before
// modernRefreshBefore, any other row measured before otherRefreshBefore, or
// no row. Fewer than pageSize rows means the sweep reached the population's
// end. The zero id starts at the beginning.
func GetProviderCapabilityProbePage(
	ctx context.Context,
	afterClientId server.Id,
	pageSize int,
	modernRefreshBefore time.Time,
	otherRefreshBefore time.Time,
) []ProviderCapabilityProbeCandidate {
	pageSize = max(1, min(pageSize, ProviderCapabilityProbeMaxPageSize))
	candidates := make([]ProviderCapabilityProbeCandidate, 0, pageSize)
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			providerCapabilityProbePageSql,
			afterClientId,
			pageSize,
			ProvideModePublic,
			modernRefreshBefore.UTC(),
			otherRefreshBefore.UTC(),
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var candidate ProviderCapabilityProbeCandidate
				server.Raise(result.Scan(&candidate.ClientId, &candidate.Due))
				candidates = append(candidates, candidate)
			}
		})
	})
	return candidates
}

// Which providers of a cohort the probe measures, and how they were judged.
type ProviderCapabilityProbeEligibility struct {
	// in the cohort's order
	ClientIds []server.Id
	// no trusted URL-success guard was published (none, or one past its max
	// age), so every country was exempt from the rule, as provider status
	// reads it: providers the export keeps out of every bucket for no_success
	// or low_success were judged eligible too
	Unguarded bool
}

// Of clientIds, in their order, those the egress rules keep in Online, judged
// with the score export's Online inputs as provider status judges them: each
// provider's published country, the country of its location rollup, and the
// URL-success guard the export last published. Online is the population
// modern-first priority and modern-only requests draw from (FP2-MODERN.md),
// Quality and Speed included, so a provider without a URL run, one short of
// the native URL-run minimums (egress_native_min_runs.go) and one whose
// failing ratio keeps it Online are measured. A provider the export places in
// no bucket is not: a hard exclusion, and one the URL-success rule or the
// Online URL ratio takes out of every bucket (no_success, low_success).
// Without a trusted guard every country is exempt, so those providers are
// measured too, and the result says so.
//
// The bulk evidence is read for clientIds alone, as provider status reads it.
// Quality's ARIN facts follow each row's latest slot rather than the
// published serving epoch, which Online does not read. The guard is read from
// Redis before any database connection is taken, and each database read takes
// a connection of its own, none while another is held.
func FilterProviderCapabilityProbeEligible(ctx context.Context, clientIds []server.Id) ProviderCapabilityProbeEligibility {
	if len(clientIds) == 0 {
		return ProviderCapabilityProbeEligibility{ClientIds: []server.Id{}}
	}
	onlineSuccessBaseline := getProviderOnlineSuccessBaseline(ctx)
	clientIdPublishedCountryCodes := getProviderCapabilityProbePublishedCountryCodes(ctx, clientIds)
	filter := newProviderCountFilterForClients(ctx, clientIds)
	filter.onlineSuccessBaseline = onlineSuccessBaseline
	return ProviderCapabilityProbeEligibility{
		ClientIds: providerCapabilityProbeEligible(
			filter,
			clientIdPublishedCountryCodes,
			clientIds,
			egressIndexSettings(),
			providerEgressTestEnabled(),
		),
		Unguarded: onlineSuccessBaseline == nil,
	}
}

// The published country of each of clientIds with a location rollup row, as
// the score export reads it: the country code of the rollup's country
// location, nil when that location is unknown. A provider without a row is
// absent. Read at most a sweep page of providers a query, the bound of the
// page read, whatever the cohort.
func getProviderCapabilityProbePublishedCountryCodes(ctx context.Context, clientIds []server.Id) map[server.Id]*string {
	clientIdPublishedCountryCodes := make(map[server.Id]*string, len(clientIds))
	server.Db(ctx, func(conn server.PgConn) {
		for start := 0; start < len(clientIds); start += ProviderCapabilityProbeMaxPageSize {
			result, err := conn.Query(
				ctx,
				`
				SELECT
					network_client_location_reliability.client_id,
					country_location.country_code
				FROM network_client_location_reliability
				LEFT JOIN location AS country_location ON
					country_location.location_id = network_client_location_reliability.country_location_id
				WHERE network_client_location_reliability.client_id = ANY($1)
				`,
				clientIds[start:min(start+ProviderCapabilityProbeMaxPageSize, len(clientIds))],
			)
			server.WithPgResult(result, err, func() {
				for result.Next() {
					var clientId server.Id
					var publishedCountryCode *string
					server.Raise(result.Scan(&clientId, &publishedCountryCode))
					clientIdPublishedCountryCodes[clientId] = publishedCountryCode
				}
			})
		}
	})
	return clientIdPublishedCountryCodes
}

// The decision every pass makes, over the filter's evidence and its guard,
// with each provider's published country (absent or nil for none): whether
// the provider is in the Online bucket, whatever its native buckets and the
// modern provider gate.
func providerCapabilityProbeEligible(
	filter providerCountFilter,
	clientIdPublishedCountryCodes map[server.Id]*string,
	clientIds []server.Id,
	settings *EgressIndexSettings,
	egressTestEnabled bool,
) []server.Id {
	eligibleClientIds := []server.Id{}
	for _, clientId := range clientIds {
		decision := decideProviderEgress(
			filter.egressFacts(clientId, clientIdPublishedCountryCodes[clientId], nil, nil, settings),
			egressTestEnabled,
		)
		if decision.online {
			eligibleClientIds = append(eligibleClientIds, clientId)
		}
	}
	return eligibleClientIds
}
