package model

// A create_time window of contracts is read as its contract id range
// (ContractIdCreateTimeRange) with the create_time predicate kept as a filter.
// These tests hold the range to the margin it promises and every rewritten
// read to the result of its create_time-only original, on contracts created on
// and around window boundaries whose ids were minted anywhere within the
// margin of their creation.

import (
	"context"
	"fmt"
	"maps"
	"testing"
	"time"

	"github.com/urnetwork/server"
)

// One contract of a window fixture.
type contractCreateTimeFixture struct {
	id         server.Id
	createTime time.Time
	dispute    bool
}

// Contracts created on, just inside and just outside the boundaries of the
// hour from base, at its half hour and in the hours either side, each with ids
// minted across the margin around its creation: at its creation, the whole
// margin before it (the inclusive lower edge of ContractIdCreateTimeRange),
// almost the whole margin after it, and a millisecond either side. Two more
// carry the extreme ids of the hour's range: its lowest id exactly, created at
// the hour, and the highest id below its upper bound, created a microsecond
// before the hour ends.
func contractCreateTimeFixtures(base time.Time) []contractCreateTimeFixture {
	margin := ContractIdCreateTimeMargin
	createTimes := []time.Time{
		base.Add(-time.Hour),
		base.Add(-time.Microsecond),
		base,
		base.Add(time.Microsecond),
		base.Add(30*time.Minute - time.Microsecond),
		base.Add(30 * time.Minute),
		base.Add(time.Hour - time.Microsecond),
		base.Add(time.Hour),
		base.Add(time.Hour + time.Microsecond),
		base.Add(2*time.Hour - time.Microsecond),
	}
	offsets := []time.Duration{0, -margin, margin - time.Millisecond, -time.Millisecond, time.Millisecond}
	fixtures := []contractCreateTimeFixture{}
	for createIndex, createTime := range createTimes {
		for offsetIndex, offset := range offsets {
			fixtures = append(fixtures, contractCreateTimeFixture{
				id:         contractIdMintedAt(createTime.Add(offset)),
				createTime: createTime,
				dispute:    (createIndex+offsetIndex)%3 == 0,
			})
		}
	}
	lower, upper := ContractIdCreateTimeRange(base, base.Add(time.Hour))
	highest := server.ContractIdFloor(server.ContractIdTime(upper).Add(-time.Millisecond))
	for index := 6; index < len(highest); index += 1 {
		highest[index] = 0xff
	}
	fixtures = append(fixtures,
		contractCreateTimeFixture{id: lower, createTime: base, dispute: true},
		contractCreateTimeFixture{id: highest, createTime: base.Add(time.Hour - time.Microsecond)},
	)
	return fixtures
}

func insertContractCreateTimeFixtures(t testing.TB, ctx context.Context, fixtures []contractCreateTimeFixture) {
	t.Helper()
	server.Tx(ctx, func(tx server.PgTx) {
		for _, fixture := range fixtures {
			server.RaisePgResult(tx.Exec(
				ctx,
				`
				INSERT INTO transfer_contract (
					contract_id,
					source_network_id,
					source_id,
					destination_network_id,
					destination_id,
					transfer_byte_count,
					create_time,
					dispute
				)
				VALUES ($1, $2, $3, $4, $5, 1024, $6, $7)
				`,
				fixture.id,
				server.NewId(),
				server.NewId(),
				server.NewId(),
				server.NewId(),
				fixture.createTime.UTC(),
				fixture.dispute,
			))
		}
	})
}

// The windows each rewritten read is compared over: an hour bucket, a fill
// range of several, windows that start or end a microsecond off a boundary, a
// half hour and an empty window.
func contractCreateTimeWindows(base time.Time) [][2]time.Time {
	return [][2]time.Time{
		{base, base.Add(time.Hour)},
		{base.Add(-time.Hour), base.Add(2 * time.Hour)},
		{base.Add(time.Microsecond), base.Add(time.Hour)},
		{base, base.Add(time.Hour + time.Microsecond)},
		{base.Add(-time.Microsecond), base.Add(time.Microsecond)},
		{base, base.Add(30 * time.Minute)},
		{base.Add(30 * time.Minute), base.Add(time.Hour)},
		{base.Add(time.Hour), base.Add(time.Hour)},
	}
}

// Every id minted within the margin of a creation inside the window is inside
// the range, the range's own lower bound included, and an id minted beyond
// the margin is outside it.
func TestContractIdCreateTimeRangeHoldsMintedIds(t *testing.T) {
	start := time.Date(2026, 3, 15, 12, 0, 0, 0, time.UTC)
	end := start.Add(time.Hour)
	lower, upper := ContractIdCreateTimeRange(start, end)
	inside := func(id server.Id) bool {
		return lower.Cmp(id) <= 0 && id.Cmp(upper) < 0
	}
	margin := ContractIdCreateTimeMargin
	for _, test := range []struct {
		name     string
		mintedAt time.Time
		inside   bool
	}{
		{name: "created at the start, minted at it", mintedAt: start, inside: true},
		{name: "created at the start, minted the whole margin before", mintedAt: start.Add(-margin), inside: true},
		{name: "minted a millisecond beyond the margin before the start", mintedAt: start.Add(-margin - time.Millisecond)},
		{name: "created just before the end, minted almost the whole margin after", mintedAt: end.Add(margin - time.Millisecond), inside: true},
		{name: "minted the whole margin after the end", mintedAt: end.Add(margin)},
	} {
		if got := inside(contractIdMintedAt(test.mintedAt)); got != test.inside {
			t.Fatalf("%s: inside=%t, want %t", test.name, got, test.inside)
		}
	}
	if !inside(lower) || inside(upper) {
		t.Fatal("the range is not [lower, upper)")
	}
	if !server.ContractIdTime(lower).Equal(start.Add(-margin)) || !server.ContractIdTime(upper).Equal(end.Add(margin)) {
		t.Fatalf("range [%s, %s) is not the margin around [%s, %s)", lower, upper, start, end)
	}
}

// The SQL bounds equal the range Go computes, on a millisecond boundary, a
// microsecond before one and across a daylight saving change, in UTC and in a
// session time zone with daylight saving.
func TestContractIdCreateTimeBoundSqlMatchesRange(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		boundary := time.Date(2026, 10, 26, 0, 0, 0, 0, time.UTC)
		// America/Chicago leaves daylight saving at 2026-11-01 07:00 UTC
		fallBack := time.Date(2026, 11, 1, 7, 0, 0, 0, time.UTC)
		times := []time.Time{
			boundary,
			boundary.Add(-time.Microsecond),
			time.Date(2026, 10, 22, 13, 37, 5, 987654000, time.UTC),
			fallBack.Add(-time.Microsecond),
			fallBack.Add(500 * time.Microsecond),
		}
		for _, timeZone := range []string{"UTC", "America/Chicago"} {
			server.Tx(ctx, func(tx server.PgTx) {
				server.RaisePgResult(tx.Exec(ctx, fmt.Sprintf(`SET LOCAL TIME ZONE '%s'`, timeZone)))
				for _, at := range times {
					var lower, upper server.Id
					server.Raise(tx.QueryRow(
						ctx,
						`SELECT `+ContractIdCreatedFromSql("$1::timestamptz")+`, `+ContractIdCreatedBeforeSql("$1::timestamptz"),
						at,
					).Scan(&lower, &upper))
					wantLower, wantUpper := ContractIdCreateTimeRange(at, at)
					if lower != wantLower || upper != wantUpper {
						t.Fatalf("%s at %s: SQL bounds [%s, %s), want [%s, %s)", timeZone, at.Format(time.RFC3339Nano), lower, upper, wantLower, wantUpper)
					}
				}
			})
		}
	})
}

// The stats reads before the contract id range bounded them.
const contractRangeCountByCreateTimeSql = `
	SELECT COUNT(*), COUNT(*) FILTER (WHERE dispute)
	FROM transfer_contract
	WHERE $1 <= create_time AND create_time < $2
`

const contractHourBucketsByCreateTimeSql = `
	SELECT DATE_TRUNC('hour', create_time), COUNT(*), COUNT(*) FILTER (WHERE dispute)
	FROM transfer_contract
	WHERE $1 <= create_time AND create_time < $2
	GROUP BY 1
`

// The live bucket count and the hour bucket fill count exactly the contracts
// their create_time-only originals count, window by window.
func TestContractStatsWindowReadsMatchCreateTime(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		base := testContractStatsBase
		insertContractCreateTimeFixtures(t, ctx, contractCreateTimeFixtures(base))
		counted := false
		for _, window := range contractCreateTimeWindows(base) {
			start, end := window[0], window[1]
			original := ContractHourCounts{}
			originalBuckets := map[int64]ContractHourCounts{}
			server.Db(ctx, func(conn server.PgConn) {
				if start.Before(end) {
					server.Raise(conn.QueryRow(ctx, contractRangeCountByCreateTimeSql, start.UTC(), end.UTC()).Scan(&original.Contracts, &original.Disputes))
				}
				result, err := conn.Query(ctx, contractHourBucketsByCreateTimeSql, start.UTC(), end.UTC())
				server.WithPgResult(result, err, func() {
					for result.Next() {
						var bucketStart time.Time
						var bucket ContractHourCounts
						server.Raise(result.Scan(&bucketStart, &bucket.Contracts, &bucket.Disputes))
						originalBuckets[bucketStart.UTC().Unix()] = bucket
					}
				})
			})
			counted = counted || 0 < original.Contracts

			count := countContractRange(ctx, start, end)
			if count.Contracts != original.Contracts || count.Disputes != original.Disputes {
				t.Fatalf("[%s, %s): counted %+v, the create_time original %+v", start.Format(time.RFC3339Nano), end.Format(time.RFC3339Nano), count, original)
			}
			if buckets := fillContractHourBuckets(ctx, start, end); !maps.Equal(buckets, originalBuckets) {
				t.Fatalf("[%s, %s): buckets %+v, the create_time original %+v", start.Format(time.RFC3339Nano), end.Format(time.RFC3339Nano), buckets, originalBuckets)
			}
		}
		if !counted {
			t.Fatal("no window counted a contract")
		}
	})
}
