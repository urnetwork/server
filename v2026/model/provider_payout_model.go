// Provider statistics read the same validated settlement shares as subnet
// usage, with the original sweep network and a half-open reporting window.
package model

import (
	"context"
	"fmt"
	"time"

	"github.com/urnetwork/server/v2026"
)

// The network and time filter precedes expansion. No client filter may hide
// an invalid allocation; the caller selects visible clients after validation.
// Materializing that selective scope keeps legacy joins from broadening the
// sweep scan and uses the existing network index independently of expansion.
const providerPayoutStatsSql = `
    WITH payout_sweeps AS MATERIALIZED (
        SELECT * FROM transfer_escrow_sweep
        WHERE network_id = $1 AND $2 <= sweep_time AND sweep_time < $3
    )
` + contractProviderPayoutRowsSql + `
    SELECT COALESCE(client_id, '00000000-0000-0000-0000-000000000000'::uuid),
        to_char(sweep_time, 'YYYY-MM-DD') AS day,
        COALESCE(SUM(payout_nano_cents), 0)::bigint, BOOL_AND(valid)
    FROM provider_rows
    GROUP BY client_id, day
`

// Reads every client/day in one snapshot and returns no partial totals if any
// scoped sweep is ambiguous, malformed, or fails either conservation check.
func queryProviderPayoutStats(
	ctx context.Context,
	conn server.PgConn,
	networkId server.Id,
	windowStart time.Time,
	windowEnd time.Time,
) (map[server.Id]map[string]NanoCents, error) {
	policy, err := server.LoadProviderPayoutEarningPolicy(ctx)
	if err != nil {
		return nil, err
	}
	query := providerPayoutStatsSql
	args := []any{networkId, windowStart, windowEnd}
	if policy != nil {
		query = `WITH payout_sweeps AS MATERIALIZED (
			SELECT s.* FROM transfer_escrow_sweep s JOIN transfer_contract c ON c.contract_id=s.contract_id
			WHERE s.network_id=$1 AND $2 <= s.sweep_time AND s.sweep_time < $3 AND c.close_time < $4
			AND c.outcome IN ('settled','dispute_resolved_to_source','dispute_resolved_to_destination')
		)` + contractProviderPayoutRowsSql + `
		SELECT COALESCE(client_id,'00000000-0000-0000-0000-000000000000'::uuid),
		to_char(sweep_time,'YYYY-MM-DD'),COALESCE(SUM(payout_nano_cents),0)::bigint,BOOL_AND(valid)
		FROM provider_rows GROUP BY client_id,to_char(sweep_time,'YYYY-MM-DD')`
		args = append(args, policy.Cutoff)
	}
	result, err := conn.Query(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("read provider payout attribution: %w", err)
	}
	defer result.Close()
	clientDayPayouts := map[server.Id]map[string]NanoCents{}
	for result.Next() {
		var clientId server.Id
		var day string
		var payout NanoCents
		var valid bool
		if err := result.Scan(&clientId, &day, &payout, &valid); err != nil {
			return nil, fmt.Errorf("read provider payout attribution: %w", err)
		}
		if !valid {
			return nil, fmt.Errorf("provider payout has missing, ambiguous, or nonconserving attribution for network %s", networkId)
		}
		dayPayouts, ok := clientDayPayouts[clientId]
		if !ok {
			dayPayouts = map[string]NanoCents{}
			clientDayPayouts[clientId] = dayPayouts
		}
		dayPayouts[day] = payout
	}
	if err := result.Err(); err != nil {
		return nil, fmt.Errorf("read provider payout attribution: %w", err)
	}
	return clientDayPayouts, nil
}
