package main

// `sim-latency reset` (and `run --reset`): clear the cross-run simulation
// state so runs are independent replicates.
//
// Reliability history is per-database and accumulates across runs; without a
// reset a second run's coveredBlockCount spans both runs and depresses every
// provider's weight (see README "Reliability history is per-database"). The
// statistical tooling assumes runs are iid draws of the same environment, so
// A/A replicates and A/B comparisons should both start from this reset.

import (
	"context"

	"github.com/urnetwork/server/v2026"
)

func resetLocalState(ctx context.Context) {
	logf("reset: truncating reliability/connection state")
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(
			ctx,
			`
			TRUNCATE
				client_reliability,
				client_reliability_running,
				client_reliability_running_window,
				client_reliability_sync,
				network_client_location_reliability,
				client_connection_reliability_score,
				network_client_connection,
				network_client_location,
				network_client_latency,
				network_client_speed
			CASCADE
			`,
		))
	})
	logf("reset: flushing redis")
	server.Redis(ctx, func(client server.RedisClient) {
		if err := client.FlushAll(ctx).Err(); err != nil {
			panic(err)
		}
	})
	logf("reset complete")
}
