// The census read exactly as it was before whole-window usage reads (origin/main
// 99131b70b, model/st_provider_usage.go). It is a test control only: every
// statement, transaction and row rule is the pre-change code, so equivalence
// tests compare the current read against it rather than against itself.
package model

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/urfoundation/sn/payoutartifact"
	"github.com/urnetwork/server"
)

// GetStEpochProviderUsageWholeCensus before the change.
func stEpochProviderUsageWholeCensusControl(ctx context.Context, epoch uint64, startTime, endTime time.Time) ([]*StProviderUsage, *payoutartifact.ClosedWorkCensus, *payoutartifact.ClosedWorkWindow, error) {
	census := &payoutartifact.ClosedWorkCensus{Schema: payoutartifact.ClosedWorkSchema}
	var window *payoutartifact.ClosedWorkWindow
	usages, err := stEpochProviderUsageOriginalControl(ctx, epoch, startTime, endTime, census, &window)
	if err != nil {
		return nil, nil, nil, err
	}
	if census.Records == nil {
		return usages, nil, nil, nil
	}
	census.Sort()
	return usages, census, window, nil
}

func stEpochProviderUsageOriginalControl(ctx context.Context, epoch uint64, startTime time.Time, endTime time.Time, census *payoutartifact.ClosedWorkCensus, window **payoutartifact.ClosedWorkWindow) ([]*StProviderUsage, error) {
	if !startTime.Before(endTime) {
		return nil, fmt.Errorf("invalid subnet usage window")
	}
	transition, err := server.LoadProviderPayoutEarningPolicy(ctx)
	if err != nil {
		return nil, err
	}
	earningStart, earningEnd := transition.SnWindow(startTime, endTime)
	if !earningStart.Before(earningEnd) {
		return []*StProviderUsage{}, nil
	}
	// Whole-work evidence retains the original epoch interval, including every
	// pre-cutoff original. Only the earning aggregate uses the asset boundary.
	queryStart, queryEnd := earningStart, earningEnd
	originalBytes := 0
	if census != nil {
		queryStart, queryEnd = startTime, endTime
		census.WindowStart, census.WindowEnd = startTime.UTC().Format(time.RFC3339Nano), endTime.UTC().Format(time.RFC3339Nano)
		if transition != nil {
			census.EarningPolicyHash = "sha256:" + transition.ConfigSha256
			selection, err := providerPayoutEarningSelection(transition)
			if err != nil {
				return nil, err
			}
			census.EarningStart = selection.StartTime.UTC().Format(time.RFC3339Nano)
			census.EarningSelectionHash = selection.PolicyHash
		}
		census.Records = []payoutartifact.ClosedWorkRecord{}
	}
	usagesByClientId := map[server.Id]*StProviderUsage{}
	var returnErr error
	read := func(conn server.PgCanQuery) {
		query := stEpochProviderUsageSql
		if census != nil {
			query = stEpochProviderOriginalUsageSql
		}
		rows, err := conn.Query(ctx, query, queryStart, queryEnd)
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
			if closedAt.Before(earningStart) || !closedAt.Before(earningEnd) {
				continue
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
	}
	// One statement reads the whole epoch in one snapshot, so the read cannot
	// be paged, and a seven-day epoch outlasts the application role's
	// 15-minute transaction bound (30-55 minutes projected at epoch 0's read
	// rates). It runs on a direct maintenance session that lifts the bound for
	// itself; the epoch close's MaxTime still bounds it, and its cancel reaches
	// that session. A replica takes this read once one exists.
	if census == nil {
		server.MaintenanceDb(ctx, func(conn server.PgConn) {
			server.WithoutTransactionTimeout(ctx, conn, func() {
				read(conn)
			})
		})
	} else {
		server.MaintenanceDb(ctx, func(conn server.PgConn) {
			server.WithoutTransactionTimeout(ctx, conn, func() {
				// The census and the check for close evidence that a partition
				// cutover holds outside the evidence table read one snapshot, so
				// the census never misses a moved report: it waits for the swap.
				tx, err := conn.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
				if err != nil {
					returnErr = fmt.Errorf("read epoch provider usage: %w", err)
					return
				}
				// read only, so a rollback ends it
				defer tx.Rollback(context.WithoutCancel(ctx))
				moved, err := server.CloseEvidenceMovedOutsideTableInTx(ctx, tx)
				if err != nil {
					returnErr = fmt.Errorf("read epoch provider usage: %w", err)
					return
				}
				if moved {
					returnErr = server.ErrCloseEvidenceCutoverMoving
					return
				}
				read(tx)
			})
		})
	}
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
