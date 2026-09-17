package model

// the contract counts behind the internal contract gauges
// (connect/EXTENDER.md M3; controller/stats_collector.go)
//
// A gauge is a point in time, so each number comes in two forms: open now, and
// created in the trailing 24 hours.
//
// The open counts are three reads of the partial indexes on transfer_contract
// (`open`, and `dispute AND outcome IS NULL`), plus an existence probe of
// contract_extender over the open set, which is cheap because the open set is
// small.
//
// The 24 hour counts are bucketized in one hour blocks of create_time. A
// complete bucket never changes, so it is computed once — by whichever
// collector host needs it first — and cached in redis for 26 hours under its
// start time; a cold cache is filled with one grouped query per table over the
// whole missing range. A bucket counts as complete only once its hour has been
// over for a minute, so an insert still in flight when the hour turned can
// never leave a short count in the cache; until then that bucket is counted
// live, as the current partial one always is. The window is the 23 closed
// buckets before the current one plus the current partial one, so it is at
// most 24 hours long and no contract is ever counted twice.

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/urnetwork/server/v2026"
)

// the cached hour bucket keys
const statsContractHourRedisKeyPrefix = "stats.contract_hour."

// a cached bucket outlives the 24 hour window it belongs to and then
// self-expires
const statsContractHourTtl = 26 * time.Hour

// how long an hour must have been over before its bucket may be cached. An
// insert whose transaction opened before the hour turned commits with a
// create_time inside it, so a bucket read the instant its hour ended could
// miss that row and freeze the short count forever
const statsContractHourSettleTimeout = time.Minute

// the closed buckets of the window, before the current partial one
const statsContractHourWindowBuckets = 23

// ContractCounts is what the contract gauges publish (M3): each number open
// now, and created in the trailing 24 hours.
type ContractCounts struct {
	OpenContracts             int64
	OpenContractsWithExtender int64
	OpenDisputes              int64
	Contracts24h              int64
	ContractsWithExtender24h  int64
	Disputes24h               int64
}

// ContractHourCounts is one hour bucket of create_time, and equally the sum of
// a window of them. The json field names are the redis value's contract: a
// bucket written by one deploy is read by the next.
type ContractHourCounts struct {
	Contracts    int64 `json:"contracts"`
	Disputes     int64 `json:"disputes"`
	WithExtender int64 `json:"with_extender"`
}

func (self *ContractHourCounts) add(other ContractHourCounts) {
	self.Contracts += other.Contracts
	self.Disputes += other.Disputes
	self.WithExtender += other.WithExtender
}

// ContractHourCacheStats is what the last CountContracts call did with the
// hour buckets, so a test can tell a consulted cache from a rescanned range.
type ContractHourCacheStats struct {
	// complete buckets served from redis
	CachedBuckets int
	// complete buckets computed and written to redis by this process
	FilledBuckets int
	// grouped range queries the fill took (one per table, or none)
	FillQueries int
	// buckets counted live: the current partial one, plus a just-ended hour
	// that has not settled yet
	LiveBuckets int
	// range queries the live count took (one per table)
	LiveQueries int
}

var contractHourCacheStatsMutex sync.Mutex
var contractHourCacheStats ContractHourCacheStats

func setContractHourCacheStats(stats ContractHourCacheStats) {
	contractHourCacheStatsMutex.Lock()
	defer contractHourCacheStatsMutex.Unlock()
	contractHourCacheStats = stats
}

// Testing_ContractHourCacheStats reads the hour bucket work of the last
// CountContractHourWindow call in this process.
func Testing_ContractHourCacheStats() ContractHourCacheStats {
	contractHourCacheStatsMutex.Lock()
	defer contractHourCacheStatsMutex.Unlock()
	return contractHourCacheStats
}

func statsContractHourRedisKey(bucketStart time.Time) string {
	return fmt.Sprintf("%s%d", statsContractHourRedisKeyPrefix, bucketStart.Unix())
}

// contractHourBucketStart truncates to the hour the bucket a time belongs to
// starts at. Truncate
// is defined on the duration since the zero instant, so it is an exact hour
// boundary in utc and never depends on the location the caller's clock
// carries.
func contractHourBucketStart(t time.Time) time.Time {
	return t.UTC().Truncate(time.Hour)
}

// CountContracts returns every contract number the collector publishes, with
// the trailing 24 hour window measured against now (M3). now is a parameter
// rather than a call to the clock so a test can place the window exactly.
func CountContracts(ctx context.Context, now time.Time) ContractCounts {
	counts := CountOpenContracts(ctx)
	window := CountContractHourWindow(ctx, now)
	counts.Contracts24h = window.Contracts
	counts.Disputes24h = window.Disputes
	counts.ContractsWithExtender24h = window.WithExtender
	return counts
}

// CountOpenContracts counts what is open right now: open contracts, open
// contracts with at least one extender party, and open disputes.
//
// "Open" and "disputed" are disjoint — the open column is generated as
// `dispute = false AND outcome IS NULL` — so the two counts are separate reads
// of their own partial indexes rather than one filtered scan. An open dispute
// is a dispute no one has decided yet.
func CountOpenContracts(ctx context.Context) ContractCounts {
	var counts ContractCounts
	server.ReplicaDb(ctx, func(conn server.PgConn) {
		count := func(sql string) int64 {
			var value int64
			result, err := conn.Query(ctx, sql)
			server.WithPgResult(result, err, func() {
				if result.Next() {
					server.Raise(result.Scan(&value))
				}
			})
			return value
		}
		counts.OpenContracts = count(`
			SELECT COUNT(*) FROM transfer_contract WHERE open
		`)
		counts.OpenDisputes = count(`
			SELECT COUNT(*) FROM transfer_contract WHERE dispute AND outcome IS NULL
		`)
		counts.OpenContractsWithExtender = count(`
			SELECT COUNT(*)
			FROM transfer_contract
			WHERE
				transfer_contract.open AND
				EXISTS (
					SELECT 1
					FROM contract_extender
					WHERE contract_extender.contract_id = transfer_contract.contract_id
				)
		`)
	})
	return counts
}

// CountContractHourWindow sums the trailing 24 hour window from its hour
// buckets: the 23 closed buckets before the current one, each read from redis
// when it is complete and counted live when it is not, plus the current
// partial bucket, always counted live.
func CountContractHourWindow(ctx context.Context, now time.Time) ContractHourCounts {
	stats := ContractHourCacheStats{}
	defer func() {
		setContractHourCacheStats(stats)
	}()

	currentStart := contractHourBucketStart(now)
	// a closed bucket is complete once its hour has been over long enough that
	// nothing can still be committed into it. Completeness only grows with
	// age, so the live buckets are always the newest ones
	settled := func(bucketStart time.Time) bool {
		return !now.Before(bucketStart.Add(time.Hour).Add(statsContractHourSettleTimeout))
	}

	completeStarts := []time.Time{}
	liveStart := currentStart
	for i := statsContractHourWindowBuckets; 1 <= i; i -= 1 {
		bucketStart := currentStart.Add(-time.Duration(i) * time.Hour)
		if settled(bucketStart) {
			completeStarts = append(completeStarts, bucketStart)
		} else {
			liveStart = bucketStart
			break
		}
	}
	// the closed buckets that were not settled, plus the current partial one
	stats.LiveBuckets = 1 + int(currentStart.Sub(liveStart)/time.Hour)

	cached := getContractHourBuckets(ctx, completeStarts)
	stats.CachedBuckets = len(cached)

	missing := []time.Time{}
	for _, bucketStart := range completeStarts {
		if _, ok := cached[bucketStart.Unix()]; !ok {
			missing = append(missing, bucketStart)
		}
	}
	if 0 < len(missing) {
		// one grouped query per table over the whole missing range, however
		// many buckets of it are already cached: a cold cache is 23 buckets
		// and two queries, not 46
		filled := fillContractHourBuckets(ctx, missing[0], missing[len(missing)-1].Add(time.Hour))
		stats.FillQueries = 2
		setContractHourBuckets(ctx, missing, filled)
		for _, bucketStart := range missing {
			cached[bucketStart.Unix()] = filled[bucketStart.Unix()]
		}
		stats.FilledBuckets = len(missing)
	}

	window := ContractHourCounts{}
	for _, bucketStart := range completeStarts {
		window.add(cached[bucketStart.Unix()])
	}
	// the live buckets are contiguous and end at now, so they are one range.
	// exactly on the hour the partial bucket is empty and there is nothing to
	// scan at all
	if liveStart.Before(now) {
		window.add(countContractRange(ctx, liveStart, now))
		stats.LiveQueries = 2
	}

	return window
}

// countContractRange counts one half-open range of create_time directly.
func countContractRange(ctx context.Context, start time.Time, end time.Time) ContractHourCounts {
	bucket := ContractHourCounts{}
	if !start.Before(end) {
		return bucket
	}
	server.ReplicaDb(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
			SELECT
				COUNT(*),
				COUNT(*) FILTER (WHERE dispute)
			FROM transfer_contract
			WHERE $1 <= create_time AND create_time < $2
			`,
			start.UTC(),
			end.UTC(),
		)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				server.Raise(result.Scan(&bucket.Contracts, &bucket.Disputes))
			}
		})

		// the extender party count is a range scan of the small table by its
		// own create_time index, never a probe per contract. Every party row of
		// one contract copies the contract's create_time at insert
		// (contractExtenderInsertSql), so a contract never straddles two
		// buckets
		result, err = conn.Query(
			ctx,
			`
			SELECT COUNT(DISTINCT contract_id)
			FROM contract_extender
			WHERE $1 <= create_time AND create_time < $2
			`,
			start.UTC(),
			end.UTC(),
		)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				server.Raise(result.Scan(&bucket.WithExtender))
			}
		})
	})
	return bucket
}

// fillContractHourBuckets computes every hour bucket in the half-open range
// with one grouped query per table.
func fillContractHourBuckets(
	ctx context.Context,
	start time.Time,
	end time.Time,
) map[int64]ContractHourCounts {
	buckets := map[int64]ContractHourCounts{}
	server.ReplicaDb(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
			SELECT
				DATE_TRUNC('hour', create_time),
				COUNT(*),
				COUNT(*) FILTER (WHERE dispute)
			FROM transfer_contract
			WHERE $1 <= create_time AND create_time < $2
			GROUP BY 1
			`,
			start.UTC(),
			end.UTC(),
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var bucketStart time.Time
				var contracts int64
				var disputes int64
				server.Raise(result.Scan(&bucketStart, &contracts, &disputes))
				key := bucketStart.UTC().Unix()
				bucket := buckets[key]
				bucket.Contracts = contracts
				bucket.Disputes = disputes
				buckets[key] = bucket
			}
		})

		result, err = conn.Query(
			ctx,
			`
			SELECT
				DATE_TRUNC('hour', create_time),
				COUNT(DISTINCT contract_id)
			FROM contract_extender
			WHERE $1 <= create_time AND create_time < $2
			GROUP BY 1
			`,
			start.UTC(),
			end.UTC(),
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var bucketStart time.Time
				var withExtender int64
				server.Raise(result.Scan(&bucketStart, &withExtender))
				key := bucketStart.UTC().Unix()
				bucket := buckets[key]
				bucket.WithExtender = withExtender
				buckets[key] = bucket
			}
		})
	})
	return buckets
}

// getContractHourBuckets reads the cached buckets, keyed by unix bucket start.
// A bucket that is absent, expired or unparseable is simply not in the result,
// which sends it to the fill.
//
// The gets go through one pipeline rather than one MGet: the keys carry no
// hash tag, so in a cluster they span slots, which MGet refuses and a pipeline
// routes.
func getContractHourBuckets(
	ctx context.Context,
	bucketStarts []time.Time,
) map[int64]ContractHourCounts {
	buckets := map[int64]ContractHourCounts{}
	if len(bucketStarts) == 0 {
		return buckets
	}
	server.Redis(ctx, func(client server.RedisClient) {
		values := make([]*redis.StringCmd, len(bucketStarts))
		_, err := client.Pipelined(ctx, func(pipe redis.Pipeliner) error {
			for i, bucketStart := range bucketStarts {
				values[i] = pipe.Get(ctx, statsContractHourRedisKey(bucketStart))
			}
			return nil
		})
		if err != nil && err != redis.Nil {
			// a cache that cannot be read is a cold cache, not a failure: the
			// fill below answers with the same numbers at a scan's cost
			return
		}
		for i, value := range values {
			body, err := value.Result()
			if err != nil {
				continue
			}
			var bucket ContractHourCounts
			if json.Unmarshal([]byte(body), &bucket) != nil {
				continue
			}
			buckets[bucketStarts[i].Unix()] = bucket
		}
	})
	return buckets
}

// setContractHourBuckets caches the given complete buckets. A bucket with no
// contracts is cached like any other: a zero that is stored is a bucket that
// is never rescanned.
func setContractHourBuckets(
	ctx context.Context,
	bucketStarts []time.Time,
	buckets map[int64]ContractHourCounts,
) {
	if len(bucketStarts) == 0 {
		return
	}
	server.Redis(ctx, func(client server.RedisClient) {
		_, err := client.Pipelined(ctx, func(pipe redis.Pipeliner) error {
			for _, bucketStart := range bucketStarts {
				bucket := buckets[bucketStart.Unix()]
				body, err := json.Marshal(bucket)
				if err != nil {
					continue
				}
				pipe.Set(
					ctx,
					statsContractHourRedisKey(bucketStart),
					body,
					statsContractHourTtl,
				)
			}
			return nil
		})
		// a bucket that could not be cached is recomputed on the next refresh;
		// the value the caller already has is correct either way
		_ = err
	})
}

// Testing_ClearContractHourBuckets drops the cached buckets of the window
// ending at now, so a test can start from a cold cache.
func Testing_ClearContractHourBuckets(ctx context.Context, now time.Time) {
	currentStart := contractHourBucketStart(now)
	server.Redis(ctx, func(client server.RedisClient) {
		_, err := client.Pipelined(ctx, func(pipe redis.Pipeliner) error {
			for i := 0; i <= statsContractHourWindowBuckets; i += 1 {
				bucketStart := currentStart.Add(-time.Duration(i) * time.Hour)
				pipe.Del(ctx, statsContractHourRedisKey(bucketStart))
			}
			return nil
		})
		server.Raise(err)
	})
}
