package model

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/urnetwork/server/v2026"
)

// This is the complete current public top-level connected population used by
// the existing ARIN rollout census. A missing location, old lookup or unknown
// policy classification never excludes a member. Native rank membership is a
// separate, generation-bound input; this SQL cannot reconstruct that authority.
const arinShadowPublicConnectionsScopeSQL = `
SELECT connection.client_id, connection.connection_id, connection.handler_id
FROM (
    SELECT client_id, connection_id, handler_id FROM network_client_connection
    WHERE connected = true ORDER BY client_id OFFSET 0
) AS connection
JOIN LATERAL (
    SELECT active, source_client_id FROM network_client
    WHERE client_id = connection.client_id OFFSET 0
) AS client ON client.active AND client.source_client_id IS NULL
JOIN LATERAL (
    SELECT true AS present FROM provide_key
    WHERE client_id = connection.client_id AND provide_mode = 3 OFFSET 0
) AS public_key ON public_key.present
JOIN LATERAL (
    SELECT heartbeat_time FROM network_client_handler
    WHERE handler_id = connection.handler_id OFFSET 0
) AS handler ON handler.heartbeat_time >= $1`

const arinShadowPublicConnectionsCountSQL = `
SELECT COUNT(DISTINCT client_id), COUNT(*) FROM (
` + arinShadowPublicConnectionsScopeSQL + ` LIMIT 2000001
) AS bounded`

const arinShadowPublicConnectionsCursorSQL = `
SELECT client_id, connection_id, handler_id,
       COUNT(*) OVER (PARTITION BY client_id) AS provider_connections
FROM (` + arinShadowPublicConnectionsScopeSQL + `) AS scoped
ORDER BY client_id, connection_id`

// A full current census must not silently scan disconnected history when
// false-zero statistics make that plan look cheap. Require the retained
// connected/client range and every point-join primary key before constraining
// this diagnostic transaction to index paths. These settings never escape it.
const arinShadowPublicIndexGuardSQL = `
SELECT
    (SELECT count(*)=4 FROM pg_index WHERE indisprimary AND indisvalid AND indisready
     AND indrelid IN ('network_client_connection'::regclass, 'network_client'::regclass,
                     'provide_key'::regclass, 'network_client_handler'::regclass))
    AND EXISTS (
        SELECT 1 FROM pg_index i
        JOIN pg_class index_class ON index_class.oid=i.indexrelid
        JOIN pg_am am ON am.oid=index_class.relam AND am.amname='btree'
        JOIN pg_attribute first_key ON first_key.attrelid=i.indrelid AND first_key.attnum=i.indkey[0]
        JOIN pg_attribute second_key ON second_key.attrelid=i.indrelid AND second_key.attnum=i.indkey[1]
        WHERE i.indrelid='network_client_connection'::regclass AND i.indisvalid AND i.indisready
          AND i.indpred IS NULL AND i.indexprs IS NULL
          AND first_key.attname='connected' AND second_key.attname='client_id'
    )`

// Private identities exist only in the bounded protected stream. Aggregate
// reports and ordinary JSON serialization cannot reveal these fields.
type ArinShadowPublicConnection struct {
	ClientId, ConnectionId, HandlerId server.Id `json:"-"`
	ProviderConnections               int       `json:"-"`
}

type ArinShadowPublicCohort struct {
	ObservedAt             time.Time `json:"observed_at"`
	Providers, Connections int64
	Complete               bool `json:"complete"`
}

// StreamArinShadowPublicCohort owns one read-only repeatable-read maintenance
// connection for at most90s. A separate primary-pool fact reader observes
// current facts between live-owner checks; it must not reuse this old snapshot.
// Counts and the full cursor share one snapshot. Every FETCH is <=256 rows,
// and only an exhausted cursor with matching exact totals emits Complete=true.
// It is opt-in: no production startup or scheduled path invokes this reader.
func StreamArinShadowPublicCohort(ctx context.Context,
	begin func(ArinShadowPublicCohort) error,
	consume func([]ArinShadowPublicConnection) error,
) (ArinShadowPublicCohort, error) {
	if ctx == nil || ctx.Err() != nil || begin == nil || consume == nil {
		return ArinShadowPublicCohort{}, server.ErrArinShadowInput
	}
	bounded, cancel := context.WithTimeout(ctx, server.ArinShadowCaptureMaxAge)
	defer cancel()
	return server.HandleError2(func() (ArinShadowPublicCohort, error) {
		cohort := ArinShadowPublicCohort{}
		var returnErr error
		server.MaintenanceDb(bounded, func(conn server.PgConn) {
			tx, err := conn.BeginTx(bounded, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
			if err != nil {
				returnErr = server.ErrArinShadowInput
				return
			}
			defer func() {
				cleanup, stop := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
				defer stop()
				if tx.Rollback(cleanup) != nil {
					cohort.Complete = false
					returnErr = server.ErrArinShadowInput
				}
			}()
			if _, err = tx.Exec(bounded, `SET LOCAL statement_timeout='15s'; SET LOCAL lock_timeout='500ms'; SET LOCAL work_mem='4MB'; SET LOCAL enable_seqscan=off; SET LOCAL enable_bitmapscan=off`); err != nil {
				returnErr = server.ErrArinShadowInput
				return
			}
			var indexesReady bool
			if err = tx.QueryRow(bounded, arinShadowPublicIndexGuardSQL).Scan(&indexesReady); err != nil || !indexesReady {
				returnErr = server.ErrArinShadowInput
				return
			}
			if err = tx.QueryRow(bounded, `SELECT transaction_timestamp() AT TIME ZONE 'UTC'`).Scan(&cohort.ObservedAt); err != nil {
				returnErr = server.ErrArinShadowInput
				return
			}
			cutoff := cohort.ObservedAt.Add(-2 * NetworkClientHandlerHeartbeatTimeout)
			if err = tx.QueryRow(bounded, arinShadowPublicConnectionsCountSQL, cutoff).Scan(&cohort.Providers, &cohort.Connections); err != nil || cohort.Connections > server.ArinShadowCapturePopulationLimit {
				returnErr = server.ErrArinShadowInput
				return
			}
			if err = begin(cohort); err != nil {
				returnErr = server.ErrArinShadowInput
				return
			}
			if _, err = tx.Exec(bounded, `DECLARE arin_shadow_current NO SCROLL CURSOR FOR `+arinShadowPublicConnectionsCursorSQL, cutoff); err != nil {
				returnErr = server.ErrArinShadowInput
				return
			}
			var providerCount, connectionCount int64
			var lastClient, lastConnection server.Id
			for bounded.Err() == nil {
				rows, err := tx.Query(bounded, `FETCH FORWARD 256 FROM arin_shadow_current`)
				if err != nil {
					returnErr = server.ErrArinShadowInput
					return
				}
				page := make([]ArinShadowPublicConnection, 0, server.ArinShadowCaptureBatchLimit)
				for rows.Next() {
					var row ArinShadowPublicConnection
					if err = rows.Scan(&row.ClientId, &row.ConnectionId, &row.HandlerId, &row.ProviderConnections); err != nil {
						break
					}
					if row.ClientId != lastClient {
						if !lastClient.Less(row.ClientId) {
							err = server.ErrArinShadowInput
							break
						}
						providerCount++
						lastClient = row.ClientId
						lastConnection = server.Id{}
					}
					if !lastConnection.Less(row.ConnectionId) || row.HandlerId == (server.Id{}) || row.ProviderConnections < 1 || row.ProviderConnections > server.ArinShadowCapturePopulationLimit {
						err = server.ErrArinShadowInput
						break
					}
					lastConnection = row.ConnectionId
					connectionCount++
					page = append(page, row)
				}
				rowErr := rows.Err()
				rows.Close()
				if err != nil || rowErr != nil || len(page) > server.ArinShadowCaptureBatchLimit || connectionCount > cohort.Connections {
					returnErr = server.ErrArinShadowInput
					return
				}
				if len(page) == 0 {
					cohort.Complete = providerCount == cohort.Providers && connectionCount == cohort.Connections
					if !cohort.Complete {
						returnErr = server.ErrArinShadowInput
					}
					return
				}
				if err = consume(page); err != nil {
					returnErr = server.ErrArinShadowInput
					return
				}
			}
			returnErr = server.ErrArinShadowInput
		}, server.OptNoRetry())
		if bounded.Err() != nil {
			cohort.Complete = false
			returnErr = server.ErrArinShadowInput
		}
		return cohort, returnErr
	}, func(error) (ArinShadowPublicCohort, error) {
		return ArinShadowPublicCohort{}, server.ErrArinShadowInput
	})
}
