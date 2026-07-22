package model

import (
	"context"
	"fmt"
	"time"

	"github.com/urnetwork/server"
)

// A single, deployment-wide hourly ceiling on how many clients can be
// deactivated through the bulk removal API (RemoveNetworkClients), counted
// across all networks and accounts together -- this does not distinguish
// who is calling it, only how much total bulk-removal work has been
// admitted in the trailing window. It guards against many networks each
// triggering large bulk-deletes at once placing an unbounded aggregate load
// on the database in a short window.
//
// This intentionally does NOT apply to the single-client removal API
// (RemoveNetworkClient) -- that path is a single-row update with no
// comparable blast radius, and callers should not have their normal,
// incremental client cleanup throttled by bulk-API abuse elsewhere.
const MaxBulkClientRemovalsPerHour = 1000000
const BulkClientRemovalWindow = time.Hour

func maxBulkClientRemovalsError() error {
	return fmt.Errorf(
		"The bulk client removal limit (%d per hour) has been reached for this deployment. Please try again later.",
		MaxBulkClientRemovalsPerHour,
	)
}

// CheckAndRecordBulkClientRemovalQuota atomically checks whether admitting
// `count` more removals would exceed MaxBulkClientRemovalsPerHour in the
// trailing BulkClientRemovalWindow, and if not, records the grant. Call this
// once per RemoveNetworkClients request (covering both its sync and async
// branches) after cheap validation (dedup, the per-request size cap) has
// already passed, so malformed requests don't burn the shared budget.
// networkId is stored only for observability -- this limit is global, not
// per-network.
func CheckAndRecordBulkClientRemovalQuota(
	ctx context.Context,
	networkId server.Id,
	count int,
) error {
	// the cutoff is computed here in Go, not via `now() - INTERVAL ...` in
	// SQL: create_time is a naive `timestamp` column holding UTC wall-clock
	// values (via server.NowUtc()), and `now()` returns `timestamptz`.
	// Comparing the two forces Postgres to cast one side using the
	// session's TimeZone setting -- on a non-UTC session (observed: BST)
	// that silently shifts the effective window by the zone offset, which
	// is large enough relative to this hour-long window to make it look
	// like nothing was ever recently recorded. Passing an explicit UTC
	// cutoff avoids any zone-dependent cast.
	cutoff := server.NowUtc().Add(-BulkClientRemovalWindow)

	var used int
	server.Tx(ctx, func(tx server.PgTx) {
		result, err := tx.Query(
			ctx,
			`
			SELECT COALESCE(SUM(client_count), 0) FROM bulk_client_removal_quota
			WHERE $1 <= create_time
			`,
			cutoff,
		)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				server.Raise(result.Scan(&used))
			}
		})
		if MaxBulkClientRemovalsPerHour < used+count {
			return
		}
		server.RaisePgResult(tx.Exec(
			ctx,
			`
			INSERT INTO bulk_client_removal_quota (bulk_client_removal_quota_id, network_id, client_count, create_time)
			VALUES ($1, $2, $3, $4)
			`,
			server.NewId(), networkId, count, server.NowUtc(),
		))
	})
	if MaxBulkClientRemovalsPerHour < used+count {
		return maxBulkClientRemovalsError()
	}
	return nil
}

// RemoveExpiredBulkClientRemovalQuota deletes quota ledger rows older than
// minTime. Rows older than BulkClientRemovalWindow no longer affect the
// trailing-window sum and are safe to delete.
func RemoveExpiredBulkClientRemovalQuota(ctx context.Context, minTime time.Time) {
	server.MaintenanceTx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(
			ctx,
			`DELETE FROM bulk_client_removal_quota WHERE create_time < $1`,
			minTime.UTC(),
		))
	})
}
