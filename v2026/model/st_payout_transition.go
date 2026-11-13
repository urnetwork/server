package model

import (
	"context"
	"fmt"
	"time"

	"github.com/urnetwork/server/v2026"
)

// Aggregate verification buckets cannot be proportionally split without raw
// event authority. Retain the epoch for repair instead of attributing a bucket
// spanning the earning boundary to both assets or silently losing its exposure.
func GetStEpochPayoutReliability(ctx context.Context, start, end time.Time, clientIds []server.Id) ([]*StClientReliability, error) {
	policy, err := server.LoadProviderPayoutEarningPolicy(ctx)
	if err != nil {
		return nil, err
	}
	start, end = policy.SnWindow(start, end)
	if !start.Before(end) {
		return []*StClientReliability{}, nil
	}
	if len(clientIds) == 0 {
		return []*StClientReliability{}, nil
	}
	if policy != nil {
		var ambiguous bool
		server.Db(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM verify_provider_stats
				WHERE period_start < $1 AND $1 < period_end AND $2 < period_end AND period_start < $3 AND client_id=ANY($4::uuid[]))`,
				policy.Cutoff, start, end, idStrings(clientIds)).Scan(&ambiguous))
		})
		if ambiguous {
			return nil, fmt.Errorf("sn: verification bucket straddles earnings cutoff; exact exposure attribution required")
		}
	}
	selected := map[server.Id]bool{}
	for _, id := range clientIds {
		selected[id] = true
	}
	rows := []*StClientReliability{}
	for _, row := range GetStEpochClientReliability(ctx, start, end) {
		if selected[row.ClientId] {
			rows = append(rows, row)
		}
	}
	return rows, nil
}
