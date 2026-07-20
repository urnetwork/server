package api

import (
	"context"
	"fmt"
	"time"

	"github.com/urnetwork/server"
)

const readinessCheckTimeout = 15 * time.Second

// ReadinessCheck runs the one-shot deep checks behind the /status readiness
// latch: pg and redis must both answer, or this container must not take over
// from the (working) old one. It runs once at startup by design — /status
// stays O(1) per poll, and runtime health remains the monitor's job
// (APIDRAIN1.md §2.1; same shape as the taskworker's check, TASKDRAIN1 §2.2).
//
// Redis is the check that earns its keep here: warmup already exercises pg,
// but a container with broken redis (vault drift, auth, network) would
// otherwise pass the health poll and take traffic while find-providers2,
// the handler caches, and api-key auth fail.
func ReadinessCheck(ctx context.Context) error {
	checkCtx, checkCancel := context.WithTimeout(ctx, readinessCheckTimeout)
	defer checkCancel()

	if r := server.HandleError(func() {
		server.Db(checkCtx, func(conn server.PgConn) {
			var one int
			result, err := conn.Query(checkCtx, "SELECT 1")
			server.WithPgResult(result, err, func() {
				for result.Next() {
					server.Raise(result.Scan(&one))
				}
			})
		})
	}); r != nil {
		return fmt.Errorf("pg: %s", r)
	}

	if r := server.HandleError(func() {
		server.Redis(checkCtx, func(client server.RedisClient) {
			server.Raise(client.Ping(checkCtx).Err())
		})
	}); r != nil {
		return fmt.Errorf("redis: %s", r)
	}

	return nil
}
