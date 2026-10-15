package main

// `sim-latency reset` (and `run --reset`): clear the cross-run simulation
// state so runs are independent replicates.
//
// Reliability history is per-database and accumulates across runs; without a
// reset a second run's coveredBlockCount spans both runs and depresses every
// provider's weight (see README "Reliability history is per-database"). The
// statistical tooling assumes runs are iid draws of the same environment, so
// A/A replicates and A/B comparisons should both start from this reset.
//
// The reset truncates tables with CASCADE and runs FLUSHALL on the client's
// logical database, db 0, which sends no SELECT. Before it sends anything,
// server.RequireAttestedLocalEndpoints must accept every PostgreSQL and Redis
// endpoint the pools dial: the process must carry its launcher's attestation,
// every endpoint must be attested, and none may be loopback outside the
// private portable root. A local service name that fell through to 127.0.0.1,
// where a production data tunnel can listen, stops the reset there.

import (
	"context"
	"fmt"

	"github.com/urnetwork/server"
)

func resetLocalState(ctx context.Context) error {
	truncate := func(ctx context.Context) {
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
	}
	flush := func(ctx context.Context) {
		server.Redis(ctx, func(client server.RedisClient) {
			if err := client.FlushAll(ctx).Err(); err != nil {
				panic(err)
			}
		})
	}
	return resetLocalStateWith(ctx, server.RequireAttestedLocalEndpoints, truncate, flush)
}

// Runs the destructive steps only after the gate accepts every endpoint; a
// refusal returns before either step can send a command.
func resetLocalStateWith(
	ctx context.Context,
	gate func(context.Context) error,
	truncate func(context.Context),
	flush func(context.Context),
) error {
	if err := gate(ctx); err != nil {
		return fmt.Errorf("reset refused: %w", err)
	}
	logf("reset: truncating reliability/connection state")
	truncate(ctx)
	logf("reset: flushing redis")
	flush(ctx)
	logf("reset complete")
	return nil
}
