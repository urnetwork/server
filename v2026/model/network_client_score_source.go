package model

import (
	"slices"

	"github.com/urnetwork/server/v2026"
)

// The score source needs the union of memberships at its three location
// levels. Aggregate once per location so joining them cannot multiply the
// provider/lookback rows. LEFT joins retain providers with no group, including
// clients first visible in this second source snapshot; their common-gate
// evidence and diagnostic population must still be captured.
const clientScoreLocationGroupSourceSQL = `
WITH location_group_memberships AS MATERIALIZED (
    SELECT location_id, array_agg(location_group_id) AS location_group_ids
    FROM location_group_member
    GROUP BY location_id
)
SELECT
    location_group_member_city.location_group_ids,
    location_group_member_region.location_group_ids,
    location_group_member_country.location_group_ids,
    network_client_location_reliability.client_id,
    network_client_location_reliability.network_id,
    network_client_location_reliability.max_net_type_score,
    network_client_location_reliability.max_net_type_score_speed,
    network_client_location_reliability.min_relative_latency_ms,
    network_client_location_reliability.max_bytes_per_second,
    network_client_location_reliability.has_latency_test,
    network_client_location_reliability.has_speed_test,
    COALESCE(client_connection_reliability_score.lookback_index, 0),  -- fix(beta): see LEFT JOIN comment below
    COALESCE(client_connection_reliability_score.reliability_weight, 1),
    COALESCE(client_connection_reliability_score.independent_reliability_weight, 1),
    -- publicly usable; see the per-location query above
    EXISTS (
        SELECT 1 FROM provide_key
        WHERE
            provide_key.client_id = network_client_location_reliability.client_id AND
            provide_key.provide_mode = $1
    ),
    COALESCE(provider_egress_health.reputation_failed_names, ''),
    network_client_location_reliability.ipv4_proven,
    network_client_location_reliability.ipv6_proven,
    -- the egress index and the published country; see the
    -- per-location query above
    network_client_location_reliability.egress_index,
    network_client_location_reliability.egress_quality,
    country_location.country_code

FROM network_client_location_reliability

INNER JOIN network_client ON
    network_client.client_id = network_client_location_reliability.client_id

LEFT JOIN location AS country_location ON
    country_location.location_id = network_client_location_reliability.country_location_id

-- fix(beta): same class of issue as UpdateClientLocations/the query
-- above this one -- treats an unscored client as neutral rather
-- than excluding it, since the reliability-scoring pipeline may
-- never populate at this env's small/cold-start scale
LEFT JOIN client_connection_reliability_score ON
    client_connection_reliability_score.client_id = network_client_location_reliability.client_id
LEFT JOIN provider_egress_health ON
        provider_egress_health.client_id = network_client_location_reliability.client_id AND
        provider_egress_health.measured_at >= $3

LEFT JOIN location_group_memberships location_group_member_city ON
    location_group_member_city.location_id = network_client_location_reliability.city_location_id

LEFT JOIN location_group_memberships location_group_member_region ON
    location_group_member_region.location_id = network_client_location_reliability.region_location_id

LEFT JOIN location_group_memberships location_group_member_country ON
    location_group_member_country.location_id = network_client_location_reliability.country_location_id

WHERE
    network_client.active = true AND
    network_client.source_client_id IS NULL AND
    network_client_location_reliability.connected = true AND
    network_client_location_reliability.valid = true AND
    -- same rule as the per-location query above: Public or
    -- Network. This one fills locationGroupClientScores -> the
    -- clientScoreLocationGroup* redis keys -> loadClientScores
    -- -> FindProviders2 whenever a spec carries a
    -- LocationGroupId, so a user who picks a promoted group
    -- (e.g. "Strong Privacy Laws") must be filtered by the same
    -- request-time network check as a plain location.
    EXISTS (
        SELECT 1 FROM provide_key
        WHERE
            provide_key.client_id = network_client_location_reliability.client_id AND
            provide_key.provide_mode IN ($1, $2)
    )`

func clientScoreDistinctGroupIds(memberships ...[]server.Id) []server.Id {
	var ids []server.Id
	for _, members := range memberships {
		for _, id := range members {
			if !slices.Contains(ids, id) {
				ids = append(ids, id)
			}
		}
	}
	return ids
}
