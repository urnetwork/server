// Connected client populations grouped by their last reported device version.
package model

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/urnetwork/server/v2026"
)

type DeviceVersionCount struct {
	Population string
	Version    string
	Count      int64
}

// ReadDeviceVersionStats counts client ids, not transports or devices. Providers
// are the connected, valid Public subset; clients include every connected role.
// The top 50 reported versions per population retain their full build strings.
// Unknown and the remaining versions (other) are always explicit buckets.
func ReadDeviceVersionStats(ctx context.Context) (counts []DeviceVersionCount, observedAt time.Time, returnErr error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	server.HandleError(func() {
		server.ReplicaDb(ctx, func(conn server.PgConn) {
			tx, err := conn.BeginTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly})
			server.Raise(err)
			defer func() {
				cleanupCtx, cleanupCancel := context.WithTimeout(context.WithoutCancel(ctx), time.Second)
				defer cleanupCancel()
				_ = tx.Rollback(cleanupCtx)
			}()
			_, err = tx.Exec(ctx, `SET LOCAL statement_timeout = '5s'`)
			server.Raise(err)
			counts, observedAt, returnErr = readDeviceVersionStats(ctx, tx)
		})
	}, func(err error) { returnErr = err })
	if ctx.Err() != nil {
		returnErr = ctx.Err()
	}
	if returnErr != nil {
		counts, observedAt = nil, time.Time{}
	}
	return
}

// A failed or canceled read never returns a publishable partial distribution.
func readDeviceVersionStats(ctx context.Context, query server.PgCanQuery) ([]DeviceVersionCount, time.Time, error) {
	if err := ctx.Err(); err != nil {
		return nil, time.Time{}, err
	}
	rows, err := query.Query(ctx, deviceVersionStatsSql, ProvideModePublic)
	if err != nil {
		return nil, time.Time{}, err
	}
	defer rows.Close()
	counts := []DeviceVersionCount{}
	var observedAt time.Time
	for rows.Next() {
		var count DeviceVersionCount
		var rowTime time.Time
		if err := rows.Scan(&count.Population, &count.Version, &count.Count, &rowTime); err != nil {
			return nil, time.Time{}, err
		}
		if count.Count < 0 || count.Version == "" || (count.Population != "provider" && count.Population != "client") || rowTime.IsZero() || (!observedAt.IsZero() && !observedAt.Equal(rowTime)) {
			return nil, time.Time{}, errors.New("invalid device-version snapshot")
		}
		observedAt = rowTime
		counts = append(counts, count)
	}
	if err := rows.Err(); err != nil {
		return nil, time.Time{}, err
	}
	if err := ctx.Err(); err != nil {
		return nil, time.Time{}, err
	}
	if observedAt.IsZero() {
		return nil, time.Time{}, errors.New("missing device-version snapshot")
	}
	return counts, observedAt, nil
}

// EXISTS prevents multiple live connections, keys or reliability rows from
// multiplying a client. LEFT JOIN retains devices that have not reported yet.
// Fold the long tail after counting so no clients disappear at the label cap.
const deviceVersionStatsSql = `
	WITH clients AS MATERIALIZED (
		SELECT network_client.client_id,
			COALESCE(NULLIF(BTRIM(device.client_version), ''), 'unknown') AS version,
			EXISTS (
				SELECT 1 FROM provide_key
				WHERE provide_key.client_id = network_client.client_id AND provide_mode = $1
			) AND EXISTS (
				SELECT 1 FROM network_client_location_reliability
				WHERE network_client_location_reliability.client_id = network_client.client_id
					AND connected AND valid
			) AS provider
		FROM network_client
		LEFT JOIN device ON device.device_id = network_client.device_id
		WHERE network_client.active AND network_client.source_client_id IS NULL
			AND EXISTS (
				SELECT 1 FROM network_client_connection
				WHERE network_client_connection.client_id = network_client.client_id AND connected
			)
	), populations AS (
		SELECT 'client' AS population, version FROM clients
		UNION ALL
		SELECT 'provider', version FROM clients WHERE provider
	), counts AS (
		SELECT population, version, count(*) AS count FROM populations GROUP BY population, version
	), ranked AS (
		SELECT *, row_number() OVER (PARTITION BY population ORDER BY count DESC, version) AS rank
		FROM counts WHERE version <> 'unknown'
	), folded AS (
		SELECT population, CASE WHEN rank <= 50 THEN version ELSE 'other' END AS version, count FROM ranked
		UNION ALL SELECT population, version, count FROM counts WHERE version = 'unknown'
		UNION ALL SELECT population, version, 0 FROM
			(VALUES ('client'), ('provider')) AS p(population),
			(VALUES ('unknown'), ('other')) AS v(version)
	)
	SELECT population, version, sum(count)::bigint, statement_timestamp() AT TIME ZONE 'UTC'
	FROM folded GROUP BY population, version ORDER BY population, version
`
