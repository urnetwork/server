// Epoch usage consumes immutable completed-work snapshots. Payment balances,
// escrow sweeps, and current provider membership never determine pool weight.
package model

import (
	"context"
	"fmt"
	"math"
	"slices"
	"time"

	"github.com/urnetwork/server"
)

// Reads one PostgreSQL snapshot and refuses partial pre-activation history.
// The half-open settlement window counts each contract once, regardless of
// how many balances funded it or whether it needed escrow at all.
func GetStEpochProviderUsage(ctx context.Context, startTime time.Time, endTime time.Time) ([]*StProviderUsage, error) {
	if !startTime.Before(endTime) {
		return nil, fmt.Errorf("invalid subnet usage window")
	}
	usagesByClientId := map[server.Id]*StProviderUsage{}
	var returnErr error
	server.Db(ctx, func(conn server.PgConn) {
		rows, err := conn.Query(ctx, `
			SELECT contract_id, provider_usage FROM transfer_contract
			WHERE $1 <= close_time AND close_time < $2 AND outcome IN ('settled','dispute_resolved_to_source','dispute_resolved_to_destination')
		`, startTime, endTime)
		if err != nil {
			returnErr = fmt.Errorf("read epoch provider usage: %w", err)
			return
		}
		defer rows.Close()
		for rows.Next() {
			var contractId server.Id
			var data []byte
			if err := rows.Scan(&contractId, &data); err != nil {
				returnErr = err
				return
			}
			snapshot, err := decodeContractUsageSnapshot(data)
			if err != nil {
				returnErr = fmt.Errorf("subnet contract %s: %w", contractId, err)
				return
			}
			for _, provider := range snapshot.Providers {
				usage := usagesByClientId[provider.ClientId]
				if usage == nil {
					usage = &StProviderUsage{ClientId: provider.ClientId, NetworkId: provider.NetworkId}
					usagesByClientId[provider.ClientId] = usage
				}
				if usage.NetworkId != provider.NetworkId || int64(provider.ByteCount) > math.MaxInt64-usage.PayoutByteCount {
					returnErr = fmt.Errorf("subnet provider usage has ambiguous network or overflowing total")
					return
				}
				usage.PayoutByteCount += int64(provider.ByteCount)
			}
		}
		returnErr = rows.Err()
	})
	if returnErr != nil {
		return nil, returnErr
	}
	usages := make([]*StProviderUsage, 0, len(usagesByClientId))
	for _, usage := range usagesByClientId {
		usages = append(usages, usage)
	}
	slices.SortFunc(usages, func(a, b *StProviderUsage) int { return a.ClientId.Cmp(b.ClientId) })
	return usages, nil
}
