package model

// These readers are diagnostic capabilities, not serving-policy hooks. Nothing
// invokes them during startup, score publication or a connection announcement.
import (
	"context"
	"time"

	"github.com/urnetwork/server/v2026"
)

// The requested identities drive both primary-key probes. OFFSET 0 keeps a
// false-zero catalog estimate from substituting a whole location-table scan.
// No address hash is read: it is a masked bucket, not an exact-IP authority.
const arinShadowCaptureFactsSQL = `
WITH requested AS MATERIALIZED (
    SELECT connection_id, ordinal
    FROM unnest($1::uuid[]) WITH ORDINALITY AS input(connection_id, ordinal)
    LIMIT 257
)
SELECT requested.connection_id,
       statement_timestamp() AT TIME ZONE 'UTC',
       connection.client_id, connection.handler_id, connection.connected,
       location.client_id, location.arin_database_build_epoch,
       location.arin_lookup_at, location.arin_risk, location.arin_non_quality,
       location.arin_quality_verified
FROM requested
LEFT JOIN LATERAL (
    SELECT client_id, handler_id, connected FROM network_client_connection
    WHERE connection_id = requested.connection_id OFFSET 0
) AS connection ON true
LEFT JOIN LATERAL (
    SELECT client_id, arin_database_build_epoch, arin_lookup_at,
           arin_risk, arin_non_quality, arin_quality_verified
    FROM network_client_location
    WHERE connection_id = requested.connection_id OFFSET 0
) AS location ON true
ORDER BY requested.ordinal`

// ReadArinShadowCaptureFacts returns one row for every requested key, including
// absent/disconnected/misbound rows. A missing location never inherits default
// false flags as proof of a lookup. Its original timestamp is never refreshed.
func ReadArinShadowCaptureFacts(ctx context.Context, ids []server.Id) ([]server.ArinShadowCaptureFacts, error) {
	if ctx == nil || ctx.Err() != nil || len(ids) == 0 || len(ids) > server.ArinShadowCaptureBatchLimit {
		return nil, server.ErrArinShadowInput
	}
	seen := make(map[server.Id]bool, len(ids))
	for _, id := range ids {
		if id == (server.Id{}) || seen[id] {
			return nil, server.ErrArinShadowInput
		}
		seen[id] = true
	}
	bounded, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	return server.HandleError2(func() ([]server.ArinShadowCaptureFacts, error) {
		facts := make([]server.ArinShadowCaptureFacts, 0, len(ids))
		server.Db(bounded, func(conn server.PgConn) {
			rows, err := conn.Query(bounded, arinShadowCaptureFactsSQL, ids)
			server.WithPgResult(rows, err, func() {
				for rows.Next() {
					var fact server.ArinShadowCaptureFacts
					var client, handler, locationClient *server.Id
					var connected, risk, nonQuality, verified *bool
					var epoch *int64
					var lookup *time.Time
					server.Raise(rows.Scan(&fact.ConnectionId, &fact.ObservedAt,
						&client, &handler, &connected, &locationClient, &epoch, &lookup,
						&risk, &nonQuality, &verified))
					if client != nil {
						fact.ClientId = *client
					}
					if handler != nil {
						fact.HandlerId = *handler
					}
					fact.Connected = connected != nil && *connected
					fact.Present = client != nil && handler != nil && locationClient != nil && *client == *locationClient &&
						epoch != nil && lookup != nil && risk != nil && nonQuality != nil && verified != nil
					if fact.Present {
						fact.Actual = server.ArinShadowActiveFacts{Epoch: *epoch, At: lookup.UTC(),
							Risk: *risk, NonQuality: *nonQuality, Verified: *verified}
					}
					facts = append(facts, fact)
				}
			})
		})
		if bounded.Err() != nil || len(facts) != len(ids) {
			return nil, server.ErrArinShadowInput
		}
		return facts, nil
	}, func(error) ([]server.ArinShadowCaptureFacts, error) {
		// Database errors can contain parameters. The capture boundary returns
		// only this finite failure, never private identities or query details.
		return nil, server.ErrArinShadowInput
	})
}
