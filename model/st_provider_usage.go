// Epoch usage consumes immutable completed-work snapshots. Payment balances,
// escrow sweeps, and current provider membership never determine pool weight.
package model

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math"
	"slices"
	"time"

	"github.com/urfoundation/sn/payoutartifact"
	"github.com/urnetwork/server"
)

// Reads live and archived work in one PostgreSQL snapshot and refuses partial
// pre-activation history. A reused live/archive identity fails the whole window.
// The half-open settlement window counts each contract once, regardless of
// how many balances funded it or whether it needed escrow at all.
func GetStEpochProviderUsage(ctx context.Context, startTime time.Time, endTime time.Time) ([]*StProviderUsage, error) {
	return getStEpochProviderUsage(ctx, 0, startTime, endTime)
}

// Only the payout owner's explicit epoch can admit an exact historical debt
// receipt. Generic time-window readers retain strict complete-usage admission.
func GetStEpochProviderUsageAtEpoch(ctx context.Context, epoch uint64, startTime time.Time, endTime time.Time) ([]*StProviderUsage, error) {
	return getStEpochProviderUsage(ctx, epoch, startTime, endTime)
}

// A historical terminal row with no close time cannot be assigned outside the
// requested epoch. Probe at most one such identity per store in the same
// statement snapshot as live/archive usage, including while retention moves it.
// The live partial index excludes ordinary open/canceled rows; archive custody
// contains only credit-bearing outcomes and uses its existing close-time index.
const stEpochProviderUsageSql = `
	WITH missing_time AS MATERIALIZED (
		(SELECT contract_id, NULL::jsonb AS provider_usage, close_time, false AS duplicate
		 FROM transfer_contract WHERE close_time IS NULL
			AND outcome IN ('settled','dispute_resolved_to_source','dispute_resolved_to_destination')
		 ORDER BY contract_id LIMIT 1)
		UNION ALL
		(SELECT contract_id, NULL::jsonb AS provider_usage, close_time, false AS duplicate
		 FROM st_provider_usage_archive WHERE close_time IS NULL
		 ORDER BY close_time, contract_id LIMIT 1)
	)
	SELECT contract_id, provider_usage, close_time, duplicate FROM missing_time
	UNION ALL
	SELECT source.contract_id, source.provider_usage, source.close_time,
		archive.contract_id IS NOT NULL AS duplicate
	FROM transfer_contract AS source
	LEFT JOIN st_provider_usage_archive AS archive ON archive.contract_id=source.contract_id
	WHERE NOT EXISTS (SELECT 1 FROM missing_time)
		AND $1 <= source.close_time AND source.close_time < $2
		AND source.outcome IN ('settled','dispute_resolved_to_source','dispute_resolved_to_destination')
	UNION ALL
	SELECT contract_id, provider_usage, close_time, false FROM st_provider_usage_archive
	WHERE NOT EXISTS (SELECT 1 FROM missing_time)
		AND $1 <= close_time AND close_time < $2
		AND outcome IN ('settled','dispute_resolved_to_source','dispute_resolved_to_destination')
`

// A component census comes from the exact same statement snapshot as usage.
// Existing payout processing can retain unknown evidence when the optional
// original-row component exceeds capacity; it never publishes a partial census.
func GetStEpochProviderUsageCensus(ctx context.Context, epoch uint64, startTime, endTime time.Time) ([]*StProviderUsage, *payoutartifact.ClosedWorkCensus, error) {
	usages, census, _, err := GetStEpochProviderUsageWholeCensus(ctx, epoch, startTime, endTime)
	return usages, census, err
}

// A separate sentinel retains the complete same-statement window even when no
// credited row exists. No invented earning row carries known-empty evidence.
func GetStEpochProviderUsageWholeCensus(ctx context.Context, epoch uint64, startTime, endTime time.Time) ([]*StProviderUsage, *payoutartifact.ClosedWorkCensus, *payoutartifact.ClosedWorkWindow, error) {
	census := &payoutartifact.ClosedWorkCensus{Schema: payoutartifact.ClosedWorkSchema}
	var window *payoutartifact.ClosedWorkWindow
	usages, err := getStEpochProviderUsageWithCensus(ctx, epoch, startTime, endTime, census, &window)
	if err != nil {
		return nil, nil, nil, err
	}
	if census.Records == nil {
		return usages, nil, nil, nil
	}
	census.Sort()
	return usages, census, window, nil
}

func getStEpochProviderUsage(ctx context.Context, epoch uint64, startTime time.Time, endTime time.Time) ([]*StProviderUsage, error) {
	return getStEpochProviderUsageWithCensus(ctx, epoch, startTime, endTime, nil, nil)
}

// The optional recorder borrows each validated row only until it clones the
// original jsonb bytes. No second query can race retention or terminal writers.
func getStEpochProviderUsageWithCensus(ctx context.Context, epoch uint64, startTime time.Time, endTime time.Time, census *payoutartifact.ClosedWorkCensus, window **payoutartifact.ClosedWorkWindow) ([]*StProviderUsage, error) {
	if !startTime.Before(endTime) {
		return nil, fmt.Errorf("invalid subnet usage window")
	}
	transition, err := server.LoadProviderPayoutEarningPolicy(ctx)
	if err != nil {
		return nil, err
	}
	startTime, endTime = transition.SnWindow(startTime, endTime)
	if !startTime.Before(endTime) {
		return []*StProviderUsage{}, nil
	}
	originalBytes := 0
	if census != nil {
		census.WindowStart, census.WindowEnd = startTime.UTC().Format(time.RFC3339Nano), endTime.UTC().Format(time.RFC3339Nano)
		if transition != nil {
			census.EarningPolicyHash = "sha256:" + transition.ConfigSha256
		}
		census.Records = []payoutartifact.ClosedWorkRecord{}
	}
	usagesByClientId := map[server.Id]*StProviderUsage{}
	var returnErr error
	server.Db(ctx, func(conn server.PgConn) {
		query := stEpochProviderUsageSql
		if census != nil {
			query = stEpochProviderOriginalUsageSql
		}
		rows, err := conn.Query(ctx, query, startTime, endTime)
		if err != nil {
			returnErr = fmt.Errorf("read epoch provider usage: %w", err)
			return
		}
		defer rows.Close()
		for rows.Next() {
			var contractId server.Id
			var data []byte
			var closedAt *time.Time
			var duplicate bool
			var originalReports []byte
			var windowOnly bool
			columns := []any{&contractId, &data, &closedAt, &duplicate}
			if census != nil {
				columns = append(columns, &originalReports, &windowOnly)
			}
			if err := rows.Scan(columns...); err != nil {
				returnErr = err
				return
			}
			if windowOnly {
				if window != nil && len(originalReports) != 0 {
					if err := json.Unmarshal(originalReports, window); err != nil {
						returnErr = err
						return
					}
				}
				continue
			}
			if closedAt == nil {
				returnErr = fmt.Errorf("subnet contract %s has terminal usage without a close time; epoch completeness is unknown", contractId)
				return
			}
			if duplicate {
				returnErr = fmt.Errorf("subnet contract %s has both live and archived usage", contractId)
				return
			}
			snapshot, err := decodeContractUsageSnapshot(data)
			if err != nil {
				returnErr = fmt.Errorf("subnet contract %s: %w", contractId, err)
				return
			}
			if legacy := snapshot.LegacyExclusion; legacy != nil && (legacy.ContractId != contractId || !legacy.ClosedAt.Equal(*closedAt) || legacy.Epoch != epoch) {
				returnErr = fmt.Errorf("subnet contract %s: legacy usage exclusion differs from its terminal owner", contractId)
				return
			}
			if census != nil {
				census.Count++
				if census.Records != nil {
					if len(originalReports) > payoutartifact.MaxClosedWorkRecordBytes {
						originalReports = nil // Preserve original usage, never a truncated proof.
					}
					if len(census.Records) == payoutartifact.MaxClosedWorkRecords || len(data) > payoutartifact.MaxClosedWorkRecordBytes || len(data)+len(originalReports) > payoutartifact.MaxClosedWorkOriginalBytes-originalBytes {
						census.Records = nil // No prefix may claim the complete query.
					} else {
						originalBytes += len(data) + len(originalReports)
						census.Records = append(census.Records, payoutartifact.ClosedWorkRecord{ContractId: [16]byte(contractId), ClosedAt: closedAt.UTC().Format(time.RFC3339Nano), Original: bytes.Clone(data), OriginalReports: bytes.Clone(originalReports)})
					}
				}
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
