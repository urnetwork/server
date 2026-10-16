// The reads the capability probe (server/capprobe) selects its providers
// with: a bounded page of its sweep over the connected providers, and which
// of a page's due providers the egress rules admit to Quality or Speed. The
// probe measures end-to-end post-quantum encryption and resizable transfer
// windows for the modern provider gate (egress_modern_provider.go) and
// records what it saw with RecordProviderModernEvidence; it never reads or
// writes URL evidence, the URL due queue or its quota.
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

// Of clientIds, in their order, those the egress rules admit to Quality or to
// Speed before the modern provider gate: the providers whose evidence the gate
// reads. It is the gate's own population, so a provider the gate takes out
// for want of evidence stays measured and can return. The native URL-run
// minimums (egress_native_min_runs.go) come first, as they do for the gate:
// a provider with one passing URL run is in neither bucket and is first
// measured after its second run admits it to Speed, so a one-sample fleet
// batch costs no tunnels. The bulk evidence is read for clientIds alone, as
// provider status reads it.
func FilterProviderCapabilityProbeEligible(ctx context.Context, clientIds []server.Id) []server.Id {
	if len(clientIds) == 0 {
		return []server.Id{}
	}
	return providerCapabilityProbeEligible(
		newProviderCountFilterForClients(ctx, clientIds),
		clientIds,
		egressIndexSettings(),
		providerEgressTestEnabled(),
	)
}

// The decision every pass makes, over the filter's evidence. No URL-success
// guard is consulted: a provider the ratio admits has a success, so that rule
// never holds for one.
func providerCapabilityProbeEligible(
	filter providerCountFilter,
	clientIds []server.Id,
	settings *EgressIndexSettings,
	egressTestEnabled bool,
) []server.Id {
	eligibleClientIds := []server.Id{}
	for _, clientId := range clientIds {
		decision := decideProviderEgress(filter.egressFacts(clientId, nil, nil, nil, settings), egressTestEnabled)
		if decision.qualityBeforeModernGate || decision.speedBeforeModernGate {
			eligibleClientIds = append(eligibleClientIds, clientId)
		}
	}
	return eligibleClientIds
}
