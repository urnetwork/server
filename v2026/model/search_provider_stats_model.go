package model

// search_provider_stats_model.go — provider "search interest" (the
// `search_interest` field of the /stats/providers* APIs).
//
// FindProviders2 is a hot, high-QPS path, so it must never write postgres.
// Instead it increments a per-provider match counter in a per-hour redis hash
// (RecordProviderSearchMatches). RollupSearchProviderStats periodically drains
// completed hours into `search_provider_stats`, and the stats APIs read from
// that table. This mirrors the verify_provider_stats rollup (redis counters ->
// periodic task -> pg rollup).

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/urnetwork/server/v2026"
)

// SearchStatsPeriod is the rollup granularity. Match counts accumulate in a
// per-hour redis hash and roll up to one `search_provider_stats` row per
// (hour, provider client).
const SearchStatsPeriod = time.Hour

// searchStatsRedisTtl is a memory backstop for the per-period redis counters.
// Set to 2x the rollup period so a bucket's counts survive at least one full
// period past its close for the rollup to drain them; in normal operation the
// rollup deletes the bucket well before this.
const searchStatsRedisTtl = 2 * SearchStatsPeriod

// SearchStatsRetention is how long rolled-up search_provider_stats rows are
// kept. RemoveOldSearchProviderStats deletes anything older. Note this bounds
// the search_interest history; other stats metrics read live from their source
// tables and are unaffected.
const SearchStatsRetention = 30 * 24 * time.Hour

// searchProviderMatchPeriodsKey is a redis SET of period-start unix seconds
// that currently have pending (un-rolled-up or still-open) counts.
const searchProviderMatchPeriodsKey = "search_provider_match_periods"

// searchProviderMatchKey is the per-period redis hash: field = provider client
// id, value = match count in that hour.
func searchProviderMatchKey(periodStart time.Time) string {
	return fmt.Sprintf("search_provider_match.%d", periodStart.Unix())
}

func searchStatsPeriodStart(now time.Time) time.Time {
	return now.UTC().Truncate(SearchStatsPeriod)
}

// RecordProviderSearchMatches counts one search match for each provider client
// that appeared in a FindProviders2 result. It is best-effort and hot-path
// safe: it writes only redis (never postgres) and never fails the caller — a
// redis error is swallowed so provider discovery is unaffected.
func RecordProviderSearchMatches(
	ctx context.Context,
	clientIds []server.Id,
	now time.Time,
) {
	if len(clientIds) == 0 {
		return
	}
	// swallow any panic (e.g. redis unavailable) so search is never impacted
	server.HandleError(func() {
		periodStart := searchStatsPeriodStart(now)
		statsKey := searchProviderMatchKey(periodStart)
		server.Redis(ctx, func(r server.RedisClient) {
			// every HIncrBy targets the same hash key (one slot), so batching in
			// a transaction is cluster-safe
			r.TxPipelined(ctx, func(pipe redis.Pipeliner) error {
				for _, clientId := range clientIds {
					pipe.HIncrBy(ctx, statsKey, clientId.String(), 1)
				}
				// backstop TTL only — the rollup normally deletes the bucket right
				// after its hour closes (see searchStatsRedisTtl)
				pipe.Expire(ctx, statsKey, searchStatsRedisTtl)
				return nil
			})
			// the periods set is a different key (different slot), so it must be a
			// separate command, not part of the transaction above
			r.SAdd(ctx, searchProviderMatchPeriodsKey, strconv.FormatInt(periodStart.Unix(), 10))
			r.Expire(ctx, searchProviderMatchPeriodsKey, searchStatsRedisTtl)
		})
	})
}

// SearchProviderStatsRow is one `search_provider_stats` rollup row.
type SearchProviderStatsRow struct {
	PeriodStart time.Time
	PeriodEnd   time.Time
	ClientId    server.Id
	MatchCount  int64
}

// RollupSearchProviderStats drains the per-hour redis match counters into
// `search_provider_stats`. It upserts absolute per-period counts, so it is
// idempotent and can run any number of times per period. A redis hour bucket
// is only deleted once that hour has fully elapsed (its count is final); the
// current open hour is left in place so later matches keep accumulating and
// get re-rolled on the next run.
func RollupSearchProviderStats(ctx context.Context, now time.Time) {
	var periodStrs []string
	server.Redis(ctx, func(r server.RedisClient) {
		periodStrs, _ = r.SMembers(ctx, searchProviderMatchPeriodsKey).Result()
	})

	for _, periodStr := range periodStrs {
		unixSec, err := strconv.ParseInt(periodStr, 10, 64)
		if err != nil {
			server.Redis(ctx, func(r server.RedisClient) {
				r.SRem(ctx, searchProviderMatchPeriodsKey, periodStr)
			})
			continue
		}
		periodStart := time.Unix(unixSec, 0).UTC()
		periodEnd := periodStart.Add(SearchStatsPeriod)
		statsKey := searchProviderMatchKey(periodStart)

		var counts map[string]string
		server.Redis(ctx, func(r server.RedisClient) {
			counts, _ = r.HGetAll(ctx, statsKey).Result()
		})

		rows := []*SearchProviderStatsRow{}
		for clientIdStr, countStr := range counts {
			clientId, err := server.ParseId(clientIdStr)
			if err != nil {
				continue
			}
			var count int64
			fmt.Sscanf(countStr, "%d", &count)
			rows = append(rows, &SearchProviderStatsRow{
				PeriodStart: periodStart,
				PeriodEnd:   periodEnd,
				ClientId:    clientId,
				MatchCount:  count,
			})
		}
		if 0 < len(rows) {
			UpsertSearchProviderStats(ctx, rows)
		}

		if !periodEnd.After(now) {
			// hour has fully elapsed: the count is final, stop tracking it
			server.Redis(ctx, func(r server.RedisClient) {
				r.Del(ctx, statsKey)
				r.SRem(ctx, searchProviderMatchPeriodsKey, periodStr)
			})
		}
	}
}

// UpsertSearchProviderStats writes rollup rows, overwriting the row for
// (period_start, client_id) with the latest absolute per-period count.
func UpsertSearchProviderStats(ctx context.Context, rows []*SearchProviderStatsRow) {
	if len(rows) == 0 {
		return
	}
	server.Tx(ctx, func(tx server.PgTx) {
		server.BatchInTx(ctx, tx, func(batch server.PgBatch) {
			for _, row := range rows {
				batch.Queue(
					`
					INSERT INTO search_provider_stats (
						period_start,
						period_end,
						client_id,
						match_count
					)
					VALUES ($1, $2, $3, $4)
					ON CONFLICT (period_start, client_id) DO UPDATE
					SET
						period_end = $2,
						match_count = $4
					`,
					row.PeriodStart.UTC(),
					row.PeriodEnd.UTC(),
					row.ClientId,
					row.MatchCount,
				)
			}
		})
	})
}

// RemoveOldSearchProviderStats deletes rolled-up rows older than
// SearchStatsRetention, in bounded batches so it never holds a long lock.
// Driven by the RemoveOldSearchProviderStats task.
func RemoveOldSearchProviderStats(ctx context.Context, maxTime time.Time, limit int) {
	minTime := maxTime.Add(-SearchStatsRetention)
	server.MaintenanceTx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(
			ctx,
			`
			DELETE FROM search_provider_stats
			USING (
			    SELECT period_start, client_id
			    FROM search_provider_stats
			    WHERE period_start < $1
			    ORDER BY period_start
			    LIMIT $2
			) t
			WHERE
			    search_provider_stats.period_start = t.period_start AND
			    search_provider_stats.client_id = t.client_id
			`,
			minTime.UTC(),
			limit,
		))
	})
}
