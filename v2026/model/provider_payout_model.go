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

// The earning policy's payout read: the network's sweeps in the window whose
// contract closed settled before the cutoff.
//
// Such a contract was created before the cutoff, since a contract closes after
// its creation, and its id leads with its creation millisecond (connect.NewId),
// so the id is below the smallest id of the cutoff plus the creation margin
// (ContractIdCreateTimeRange). The sweep scan and the contract join each repeat
// that bound, because PostgreSQL does not carry an inequality across a join
// equality, and the bound is a literal, because PostgreSQL prunes partitions at
// plan time only by constants (contract_id_bound.go). The cutoff is fixed
// configuration, so the statement's text is too. The bound only narrows: the
// close time and the outcome still decide which sweeps earn.
//
// Without the bound, the read joined every sweep of the network in the window
// to transfer_contract for its close time, although every window the readers
// ask for now opens long after the cutoff and the read returns no row. On Main
// (2026-11-26) a call took 3 to 4 s, with about 3M buffer hits and 120k reads,
// its generic plan JIT compiled 132 functions (about 180 ms a call), and a
// custom plan for a large network hashed every settled contract from before
// the cutoff. With the bound, the scans keep to the partitions that can hold
// such a contract, and the sweep scan reads its window through the
// (sweep_time, contract_id) index with the bound in its condition.
func providerPayoutStatsPolicySql(cutoff time.Time) string {
	_, createdBefore := ContractIdCreateTimeRange(cutoff, cutoff)
	bound := contractIdLiteralSql(createdBefore)
	return `WITH payout_sweeps AS MATERIALIZED (
			SELECT s.* FROM transfer_escrow_sweep s JOIN transfer_contract c ON c.contract_id=s.contract_id
			WHERE s.network_id=$1 AND $2 <= s.sweep_time AND s.sweep_time < $3 AND c.close_time < $4
			AND s.contract_id < ` + bound + ` AND c.contract_id < ` + bound + `
			AND c.outcome IN ('settled','dispute_resolved_to_source','dispute_resolved_to_destination')
		)` + contractProviderPayoutRowsSql + `
		SELECT COALESCE(client_id,'00000000-0000-0000-0000-000000000000'::uuid),
		to_char(sweep_time,'YYYY-MM-DD'),COALESCE(SUM(payout_nano_cents),0)::bigint,BOOL_AND(valid)
		FROM provider_rows GROUP BY client_id,to_char(sweep_time,'YYYY-MM-DD')`
}

// The payout read queryProviderPayoutStats sends under the policy, or without
// one, and its arguments.
func providerPayoutStatsRead(
	policy *server.ProviderPayoutTransition,
	networkId server.Id,
	windowStart time.Time,
	windowEnd time.Time,
) (string, []any) {
	args := []any{networkId, windowStart, windowEnd}
	if policy == nil {
		return providerPayoutStatsSql, args
	}
	return providerPayoutStatsPolicySql(policy.Cutoff), append(args, policy.Cutoff)
}

// Reads every client/day in one snapshot and returns no partial totals if any
// scoped sweep is ambiguous, malformed, or fails either conservation check.
func queryProviderPayoutStats(
	ctx context.Context,
	conn server.PgConn,
	networkId server.Id,
	windowStart time.Time,
	windowEnd time.Time,
) (map[server.Id]map[string]NanoCents, error) {
	policy, err := server.LoadProviderPayoutEarningPolicyInConn(ctx, conn)
	if err != nil {
		return nil, err
	}
	query, args := providerPayoutStatsRead(policy, networkId, windowStart, windowEnd)
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
