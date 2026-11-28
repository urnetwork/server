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

	"github.com/urnetwork/server/v2026"
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

// The degradation check's count of contracts created in its window before the
// contract id range bounded it.
const contractDegradationOpenCountByCreateTimeSql = `
	SELECT count(*)
	FROM transfer_contract
	WHERE $1 <= create_time AND create_time < $2
`

// The degradation check counts exactly the contracts its create_time-only
// original counts, for windows that end on, just after and between the
// fixture's boundaries.
func TestContractDegradationOpenCountMatchesCreateTime(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		base := testContractStatsBase
		insertContractCreateTimeFixtures(t, ctx, contractCreateTimeFixtures(base))
		counted := false
		for _, now := range []time.Time{
			base,
			base.Add(time.Microsecond),
			base.Add(30 * time.Minute),
			base.Add(time.Hour),
			base.Add(time.Hour + time.Microsecond),
			base.Add(2*time.Hour - time.Microsecond),
			base.Add(2 * time.Hour),
		} {
			var original int64
			server.Db(ctx, func(conn server.PgConn) {
				server.Raise(conn.QueryRow(ctx, contractDegradationOpenCountByCreateTimeSql, now.Add(-ContractDegradationWindow), now).Scan(&original))
			})
			counted = counted || 0 < original
			openCount, closeCount, err := MeasureContractDegradation(ctx, now)
			if err != nil {
				t.Fatal(err)
			}
			if openCount != original || closeCount != 0 {
				t.Fatalf("window ending %s: open=%d close=%d, the create_time original %d and no closes", now.Format(time.RFC3339Nano), openCount, closeCount, original)
			}
		}
		if !counted {
			t.Fatal("no window counted a contract")
		}
	})
}

// The straggler stamp before the contract id range bounded it.
var assignStragglerReapTimeByCreateTimeSql = `
WITH batch AS (
	SELECT transfer_contract.contract_id
	FROM transfer_contract
	WHERE
		transfer_contract.reap_time IS NULL AND
		transfer_contract.close_time IS NOT NULL AND
		transfer_contract.create_time < $1 AND
		NOT EXISTS (
			SELECT 1
			FROM transfer_escrow_sweep
			INNER JOIN account_payment ON
				account_payment.payment_id = transfer_escrow_sweep.payment_id
			WHERE
				transfer_escrow_sweep.contract_id = transfer_contract.contract_id AND
				` + server.ContractPaymentRetentionHoldSql + `
		)
	ORDER BY transfer_contract.create_time
	LIMIT $2
)
UPDATE transfer_contract
SET reap_time = now() AT TIME ZONE 'UTC'
FROM batch
WHERE transfer_contract.contract_id = batch.contract_id
`

// The contracts a stamp statement marks, in a transaction that rolls back.
func stragglerStampTestSelection(t testing.TB, ctx context.Context, sql string, args ...any) map[server.Id]bool {
	t.Helper()
	conn := acquireContractLifecycleTestConnection(t, ctx)
	defer conn.Release()
	tx, err := conn.Begin(ctx)
	server.Raise(err)
	defer func() {
		_ = tx.Rollback(context.WithoutCancel(ctx))
	}()
	selected := map[server.Id]bool{}
	result, err := tx.Query(ctx, sql+` RETURNING transfer_contract.contract_id`, args...)
	server.WithPgResult(result, err, func() {
		for result.Next() {
			var contractId server.Id
			server.Raise(result.Scan(&contractId))
			selected[contractId] = true
		}
	})
	return selected
}

// The straggler stamp marks exactly the contracts its create_time-only original
// marks: closed, never reaped, created before the horizon and held by no
// payment, among contracts created on and around the horizon whose ids were
// minted anywhere within the margin, open ones, already reaped ones and held
// ones. Batched below the eligible count, the stamp drains the same set.
func TestStragglerStampMatchesCreateTime(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		horizon := testContractStatsBase
		margin := ContractIdCreateTimeMargin
		networkId := server.NewId()
		usage := &contractUsageSnapshot{Version: 1, ByteCount: 1024,
			Providers: []contractProviderUsage{{ClientId: networkId, NetworkId: networkId, ByteCount: 1024}}}
		reaped := horizon.Add(time.Hour)
		paymentId := server.NewId()
		server.Tx(ctx, func(tx server.PgTx) {
			insert := func(contractId server.Id, created time.Time, closed bool, reapTime *time.Time) {
				var closeTime *time.Time
				var outcome *ContractOutcome
				var contractUsage *contractUsageSnapshot
				if closed {
					closeTime = &created
					settled := ContractOutcomeSettled
					outcome = &settled
					contractUsage = usage
				}
				server.RaisePgResult(tx.Exec(ctx, `INSERT INTO transfer_contract (
					contract_id,source_network_id,source_id,destination_network_id,destination_id,
					transfer_byte_count,create_time,close_time,outcome,provider_usage,usage_origin_is_source,reap_time
				) VALUES ($1,$2,$2,$2,$2,1024,$3,$4,$5,$6,TRUE,$7)`,
					contractId, networkId, created.UTC(), closeTime, outcome, contractUsage, reapTime))
			}
			// a pending payment holds its contracts
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO account_payment (
				payment_id,payment_plan_id,network_id,payout_byte_count,payout_nano_cents,min_sweep_time
			) VALUES ($1,$2,$3,1024,1,$4)`, paymentId, server.NewId(), networkId, horizon.UTC()))
			for index, created := range []time.Time{
				horizon.Add(-time.Hour),
				horizon.Add(-time.Microsecond),
				horizon,
				horizon.Add(time.Microsecond),
				horizon.Add(time.Hour),
			} {
				for _, offset := range []time.Duration{0, -margin, margin - time.Millisecond, -time.Millisecond, time.Millisecond} {
					insert(contractIdMintedAt(created.Add(offset)), created, true, nil)
				}
				insert(contractIdMintedAt(created), created, false, nil)
				insert(contractIdMintedAt(created), created, true, &reaped)
				held := contractIdMintedAt(created.Add(-time.Millisecond * time.Duration(index+2)))
				insert(held, created, true, nil)
				server.RaisePgResult(tx.Exec(ctx, `INSERT INTO transfer_escrow_sweep (
					contract_id,balance_id,network_id,payout_byte_count,payout_net_revenue_nano_cents,payment_id
				) VALUES ($1,$2,$3,1024,1,$4)`, held, server.NewId(), networkId, paymentId))
			}
			// the highest id below the horizon's id bound
			highest := server.ContractIdFloor(horizon.Add(margin - time.Millisecond))
			for index := 6; index < len(highest); index += 1 {
				highest[index] = 0xff
			}
			insert(highest, horizon.Add(-time.Microsecond), true, nil)
		})

		original := stragglerStampTestSelection(t, ctx, assignStragglerReapTimeByCreateTimeSql, horizon.UTC(), 1000)
		rewritten := stragglerStampTestSelection(t, ctx, assignStragglerReapTimeSQL, horizon.UTC(), 1000,
			server.ContractIdFloor(horizon.Add(stragglerContractIdMargin)))
		if len(original) == 0 || !maps.Equal(rewritten, original) {
			t.Fatalf("the stamp selects %d contracts, the create_time original %d: %v vs %v", len(rewritten), len(original), rewritten, original)
		}

		if assigned := assignStragglerReapTimeBatches(ctx, horizon, 3); assigned != int64(len(original)) {
			t.Fatalf("batches of 3 stamped %d contracts, want %d", assigned, len(original))
		}
		stamped := map[server.Id]bool{}
		server.Db(ctx, func(conn server.PgConn) {
			result, err := conn.Query(ctx, `SELECT contract_id FROM transfer_contract WHERE reap_time IS NOT NULL AND reap_time <> $1`, reaped.UTC())
			server.WithPgResult(result, err, func() {
				for result.Next() {
					var contractId server.Id
					server.Raise(result.Scan(&contractId))
					stamped[contractId] = true
				}
			})
		})
		if !maps.Equal(stamped, original) {
			t.Fatalf("the batches stamped %v, the create_time original %v", stamped, original)
		}
	})
}
