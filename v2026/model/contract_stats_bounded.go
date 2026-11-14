// Collector-only snapshots keep exact open-set gauges honest when the live
// population outgrows a bounded read. Explicit exact-count APIs stay separate.
package model

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/urnetwork/server/v2026"
)

// A 1001-row newest-first sentinel has a direct one-second Main control. Keep
// the count authority conservative instead of spending the whole query budget.
const openContractStatsLimit int64 = 1000
const openContractStatsTimeout = 5 * time.Second

// Counts are lower bounds unless their exact flag is true. The extender count
// shares the open-set completeness flag; a capped zero is not an exact zero.
// Every count belongs to ObservedAt's one PostgreSQL statement snapshot.
type OpenContractStatsSnapshot struct {
	OpenContracts             int64
	OpenContractsWithExtender int64
	OpenDisputes              int64
	OpenContractsExact        bool
	OpenDisputesExact         bool
	ObservedAt                time.Time
}

// Bounds acquisition as well as query work. Unavailable evidence never returns
// a publishable partial snapshot, including cancellation after row decoding.
func ReadOpenContractStats(ctx context.Context) (snapshot OpenContractStatsSnapshot, returnErr error) {
	ctx, cancel := context.WithTimeout(ctx, openContractStatsTimeout)
	defer cancel()
	server.HandleError(func() {
		server.ReplicaDb(ctx, func(conn server.PgConn) {
			tx, err := conn.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted, AccessMode: pgx.ReadOnly})
			if err != nil {
				returnErr = err
				return
			}
			defer func() {
				cleanupCtx, cleanupCancel := context.WithTimeout(context.WithoutCancel(ctx), time.Second)
				defer cleanupCancel()
				_ = tx.Rollback(cleanupCtx)
			}()
			// The server bound survives a lost client cancellation packet. A
			// local setting cannot escape this read-only transaction into the pool.
			if _, err := tx.Exec(ctx, `SET LOCAL statement_timeout = '5s'`); err != nil {
				returnErr = err
				return
			}
			snapshot, returnErr = readOpenContractStats(ctx, tx, openContractStatsLimit)
		})
	}, func(err error) { returnErr = err })
	if ctx.Err() != nil {
		returnErr = ctx.Err()
	}
	if returnErr != nil {
		snapshot = OpenContractStatsSnapshot{}
	}
	return
}

// The query owner is explicit so local SQL fixtures and cancellation controls
// exercise the same implementation without process-wide database hooks.
func readOpenContractStats(ctx context.Context, query server.PgCanQuery, limit int64) (snapshot OpenContractStatsSnapshot, returnErr error) {
	if limit < 1 || openContractStatsLimit < limit {
		return OpenContractStatsSnapshot{}, errors.New("invalid open-contract stats limit")
	}
	ctx, cancel := context.WithTimeout(ctx, openContractStatsTimeout)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return OpenContractStatsSnapshot{}, err
	}
	rows, err := query.Query(ctx, openContractStatsSql, limit+1)
	if err != nil {
		return OpenContractStatsSnapshot{}, err
	}
	defer rows.Close()
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return OpenContractStatsSnapshot{}, err
		}
		return OpenContractStatsSnapshot{}, errors.New("missing open-contract stats snapshot")
	}
	if err := rows.Scan(&snapshot.OpenContracts, &snapshot.OpenContractsWithExtender, &snapshot.OpenDisputes, &snapshot.ObservedAt); err != nil {
		return OpenContractStatsSnapshot{}, err
	}
	if rows.Next() {
		return OpenContractStatsSnapshot{}, errors.New("duplicate open-contract stats snapshot")
	}
	if err := rows.Err(); err != nil {
		return OpenContractStatsSnapshot{}, err
	}
	if err := ctx.Err(); err != nil {
		return OpenContractStatsSnapshot{}, err
	}
	if snapshot.ObservedAt.IsZero() || snapshot.OpenContracts < 0 || limit+1 < snapshot.OpenContracts ||
		snapshot.OpenDisputes < 0 || limit+1 < snapshot.OpenDisputes || snapshot.OpenContractsWithExtender < 0 || snapshot.OpenContracts < snapshot.OpenContractsWithExtender {
		return OpenContractStatsSnapshot{}, errors.New("invalid open-contract stats snapshot")
	}
	snapshot.OpenContractsExact = snapshot.OpenContracts <= limit
	snapshot.OpenDisputesExact = snapshot.OpenDisputes <= limit
	return
}

// Materialize the cap before testing extender membership. A limit outside a
// rare EXISTS filter could still inspect the entire open table to find a page.
// Start at new arrivals, not an old prefix with snapshot-retained dead entries.
// Order only selects a lower-bound sample; exact counts still exhaust the set.
// Row bounds do not bound dead-version visibility work; the deadline does.
const openContractStatsSql = `
	WITH open_contracts AS MATERIALIZED (
		SELECT contract_id FROM transfer_contract WHERE open
		ORDER BY create_time DESC LIMIT $1
	), disputes AS MATERIALIZED (
		SELECT 1 FROM transfer_contract WHERE dispute AND outcome IS NULL LIMIT $1
	)
	SELECT
		(SELECT count(*) FROM open_contracts),
		(SELECT count(*) FROM open_contracts WHERE EXISTS (
			SELECT 1 FROM contract_extender
			WHERE contract_extender.contract_id = open_contracts.contract_id
		)),
		(SELECT count(*) FROM disputes),
		statement_timestamp() AT TIME ZONE 'UTC'
`
