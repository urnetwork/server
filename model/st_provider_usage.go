// Epoch usage expands immutable provider allocations while keeping account
// payments aggregated by network. A legacy fallback requires an immutable
// endpoint-backed contract without the stream aggregation marker.
package model

import (
	"context"
	"fmt"
	"time"

	"github.com/urnetwork/server"
)

// Reads one PostgreSQL snapshot, validates every allocation against its exact
// network sweep, and returns no partial credit if any historical row is
// ambiguous. Legacy stream membership can be republished after a network
// change, so it cannot prove which clients earned an older combined payment.
// NULL legacy allocations never fall back merely because modern
// allocation rows were empty, malformed, duplicated, or nonconserving.
func GetStEpochProviderUsage(ctx context.Context, startTime time.Time, endTime time.Time) ([]*StProviderUsage, error) {
	usages := []*StProviderUsage{}
	var returnErr error
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(ctx, `
            WITH payout_sweeps AS (
                SELECT * FROM transfer_escrow_sweep
                WHERE $1 <= sweep_time AND sweep_time < $2
            )
        `+contractProviderPayoutRowsSql+`
            SELECT COALESCE(client_id, '00000000-0000-0000-0000-000000000000'::uuid),
                network_id, COALESCE(SUM(payout_byte_count), 0)::bigint,
                BOOL_AND(valid)
            FROM provider_rows
            GROUP BY client_id, network_id
        `, startTime, endTime)
		if err != nil {
			returnErr = fmt.Errorf("read epoch provider attribution: %w", err)
			return
		}
		defer result.Close()
		for result.Next() {
			usage := &StProviderUsage{}
			var valid bool
			if err := result.Scan(&usage.ClientId, &usage.NetworkId, &usage.PayoutByteCount, &valid); err != nil {
				returnErr = err
				return
			}
			if !valid {
				returnErr = fmt.Errorf("subnet provider usage has missing, ambiguous, or nonconserving attribution for network %s", usage.NetworkId)
				return
			}
			usages = append(usages, usage)
		}
		returnErr = result.Err()
	})
	if returnErr != nil {
		return nil, returnErr
	}
	return usages, nil
}
