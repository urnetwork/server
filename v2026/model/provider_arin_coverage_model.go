// Read-only deployment attestation is separate from provider serving policy.
package model

import (
	"context"
	"time"

	"github.com/urnetwork/server/v2026"
)

// Aggregate rollout evidence only; absence of provenance is not a new serving
// exclusion. No provider identifier, connection hash, or address is returned.
type ProviderArinClassificationCoverage struct {
	Connections             int64 `json:"connections"`
	ClassifiedConnections   int64 `json:"classified_connections"`
	UnclassifiedConnections int64 `json:"unclassified_connections"`
	// A known database lookup from a different generation or before cutover.
	OutdatedConnections      int64 `json:"outdated_connections"`
	Providers                int64 `json:"providers"`
	FullyClassifiedProviders int64 `json:"fully_classified_providers"`
}

// All live public top-level provider connections must have an actual lookup
// against the canary's selected database epoch and after its rollout boundary.
func GetProviderArinClassificationCoverage(ctx context.Context, expectedBuildEpoch int64, minLookupAt time.Time) ProviderArinClassificationCoverage {
	coverage := ProviderArinClassificationCoverage{}
	server.Db(ctx, func(conn server.PgConn) {
		rows, err := conn.Query(ctx, `WITH connections AS (
			SELECT connection.client_id,
				COALESCE(location.arin_database_build_epoch > 0 AND location.arin_lookup_at IS NOT NULL,false) AS evaluated,
				COALESCE(location.arin_database_build_epoch=$1 AND $1>0 AND location.arin_lookup_at >= $2,false) AS selected
			FROM network_client_connection AS connection
			JOIN network_client AS client ON client.client_id=connection.client_id
			JOIN network_client_handler AS handler ON handler.handler_id=connection.handler_id
			LEFT JOIN network_client_location AS location ON location.connection_id=connection.connection_id
			WHERE connection.connected AND client.active AND client.source_client_id IS NULL
			AND handler.heartbeat_time >= $3
			AND EXISTS(SELECT 1 FROM provide_key WHERE provide_key.client_id=client.client_id AND provide_mode=$4)
		), providers AS (SELECT client_id,BOOL_AND(selected) AS selected FROM connections GROUP BY client_id)
		SELECT COUNT(*),COUNT(*) FILTER(WHERE selected),COUNT(*) FILTER(WHERE NOT evaluated),
			COUNT(*) FILTER(WHERE evaluated AND NOT selected),
			(SELECT COUNT(*) FROM providers),(SELECT COUNT(*) FROM providers WHERE selected)
		FROM connections`, expectedBuildEpoch, minLookupAt.UTC(), server.NowUtc().Add(-2*NetworkClientHandlerHeartbeatTimeout), ProvideModePublic)
		server.WithPgResult(rows, err, func() {
			if rows.Next() {
				server.Raise(rows.Scan(&coverage.Connections, &coverage.ClassifiedConnections, &coverage.UnclassifiedConnections,
					&coverage.OutdatedConnections, &coverage.Providers, &coverage.FullyClassifiedProviders))
			}
		})
	})
	return coverage
}
