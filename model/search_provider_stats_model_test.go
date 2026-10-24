package model

import (
	"context"
	"testing"
	"time"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/server"
)

// The rollup re-upserts every row of the open hour each minute. An unchanged
// count leaves the row version (xmin) as it was; a changed count, and a new
// row, are written.
func TestUpsertSearchProviderStatsSkipsUnchangedRows(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		periodStart := time.Date(2026, 10, 24, 6, 0, 0, 0, time.UTC)
		periodEnd := periodStart.Add(SearchStatsPeriod)
		unchangedClientId := server.NewId()
		changedClientId := server.NewId()
		newClientId := server.NewId()
		row := func(clientId server.Id, matchCount int64) *SearchProviderStatsRow {
			return &SearchProviderStatsRow{
				PeriodStart: periodStart,
				PeriodEnd:   periodEnd,
				ClientId:    clientId,
				MatchCount:  matchCount,
			}
		}
		state := func(clientId server.Id) (xmin string, matchCount int64, found bool) {
			server.Db(ctx, func(conn server.PgConn) {
				result, err := conn.Query(
					ctx,
					`SELECT xmin::text, match_count FROM search_provider_stats WHERE period_start = $1 AND client_id = $2`,
					periodStart,
					clientId,
				)
				server.WithPgResult(result, err, func() {
					if result.Next() {
						server.Raise(result.Scan(&xmin, &matchCount))
						found = true
					}
				})
			})
			return
		}

		UpsertSearchProviderStats(ctx, []*SearchProviderStatsRow{
			row(unchangedClientId, 3),
			row(changedClientId, 5),
		})
		unchangedXmin, _, _ := state(unchangedClientId)
		changedXmin, _, _ := state(changedClientId)

		// the next minute's rollup of the same open hour
		UpsertSearchProviderStats(ctx, []*SearchProviderStatsRow{
			row(unchangedClientId, 3),
			row(changedClientId, 8),
			row(newClientId, 1),
		})

		xmin, matchCount, found := state(unchangedClientId)
		if !found || xmin != unchangedXmin || matchCount != 3 {
			t.Fatalf("unchanged row: found=%t xmin %s -> %s count=%d, want the row as it was", found, unchangedXmin, xmin, matchCount)
		}
		xmin, matchCount, found = state(changedClientId)
		if !found || xmin == changedXmin || matchCount != 8 {
			t.Fatalf("changed row: found=%t xmin %s -> %s count=%d, want the new count written", found, changedXmin, xmin, matchCount)
		}
		_, matchCount, found = state(newClientId)
		connect.AssertEqual(t, found, true)
		connect.AssertEqual(t, matchCount, int64(1))
	})
}
