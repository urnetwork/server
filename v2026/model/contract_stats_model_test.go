package model

import (
	"context"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
)

// The contract counts and the hour bucket cache (connect/EXTENDER.md M3).
//
// The clock is a parameter of every count, so nothing here sleeps or waits: a
// window is placed by naming the instant it ends at, and a bucket is made to
// settle by moving that instant past the settle timeout.
//
// Every id is generated, so nothing here names anything real.

// testContractStatsBase is the hour boundary every bucket in these tests is
// measured from. A fixed instant keeps date_trunc, the window arithmetic and
// the redis keys reproducible.
var testContractStatsBase = time.Date(2026, 3, 15, 12, 0, 0, 0, time.UTC)

// One transfer_contract row with an exact create_time, so it lands in a named
// hour bucket.
func testContractStatsContract(
	ctx context.Context,
	createTime time.Time,
	dispute bool,
	outcome *string,
) server.Id {
	contractId := server.NewId()
	server.Db(ctx, func(conn server.PgConn) {
		server.RaisePgResult(conn.Exec(
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
				dispute,
				outcome
			)
			VALUES ($1, $2, $3, $4, $5, 1024, $6, $7, $8)
			`,
			contractId,
			server.NewId(),
			server.NewId(),
			server.NewId(),
			server.NewId(),
			createTime.UTC(),
			dispute,
			outcome,
		))
	})
	return contractId
}

// One extender party of a contract, stamped with the contract's own
// create_time the way the production insert's default does.
func testContractStatsExtenderParty(
	ctx context.Context,
	contractId server.Id,
	party ContractParty,
	createTime time.Time,
) {
	server.Db(ctx, func(conn server.PgConn) {
		server.RaisePgResult(conn.Exec(
			ctx,
			`
			INSERT INTO contract_extender (
				contract_id,
				extender_id,
				party,
				client_id,
				network_id,
				create_time
			)
			VALUES ($1, $2, $3, $4, $5, $6)
			`,
			contractId,
			server.NewId(),
			party,
			server.NewId(),
			server.NewId(),
			createTime.UTC(),
		))
	})
}

// M3: the open counts read the three predicates. Open and disputed are
// disjoint by construction (open is generated as `dispute = false AND outcome
// IS NULL`), a decided dispute is no longer open, and a contract with an
// extender party is a subset of the open set.
func TestCountOpenContracts(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		now := server.NowUtc()
		success := "success"

		// three open contracts, one of them with two extender parties
		open1 := testContractStatsContract(ctx, now, false, nil)
		testContractStatsContract(ctx, now, false, nil)
		openWithExtender := testContractStatsContract(ctx, now, false, nil)
		testContractStatsExtenderParty(ctx, openWithExtender, ContractPartySource, now)
		testContractStatsExtenderParty(ctx, openWithExtender, ContractPartyDestination, now)
		_ = open1

		// closed: settled with an outcome, so neither open nor disputed
		closed := testContractStatsContract(ctx, now, false, &success)
		testContractStatsExtenderParty(ctx, closed, ContractPartySource, now)

		// an open dispute, and one already decided
		testContractStatsContract(ctx, now, true, nil)
		testContractStatsContract(ctx, now, true, &success)

		counts := CountOpenContracts(ctx)
		if counts.OpenContracts != 3 {
			t.Fatalf("open contracts = %d, want 3", counts.OpenContracts)
		}
		if counts.OpenContractsWithExtender != 1 {
			t.Fatalf(
				"open contracts with an extender = %d, want 1 (two parties of one contract)",
				counts.OpenContractsWithExtender,
			)
		}
		if counts.OpenDisputes != 1 {
			t.Fatalf("open disputes = %d, want 1 (the decided one is closed)", counts.OpenDisputes)
		}
	})
}

// M3: the trailing 24 hour window, its cold fill, its warm reads, and its
// boundary. The window is the 23 closed buckets before the current one plus
// the current partial one, so a contract created 24 hours ago is outside it.
func TestContractHourWindowCountsAndCache(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		base := testContractStatsBase
		// 15 minutes into the current hour: the previous hour has been over
		// long enough to be complete, so every closed bucket of the window is
		// cacheable
		now := base.Add(15 * time.Minute)
		success := "success"

		hour := func(offset int, minute int) time.Time {
			return base.Add(time.Duration(offset)*time.Hour + time.Duration(minute)*time.Minute)
		}

		// three in the hour that just ended, one of them disputed
		testContractStatsContract(ctx, hour(-1, 5), false, nil)
		testContractStatsContract(ctx, hour(-1, 30), false, nil)
		testContractStatsContract(ctx, hour(-1, 45), true, nil)

		// two five hours back, one with an extender party
		testContractStatsContract(ctx, hour(-5, 10), false, nil)
		fiveBack := testContractStatsContract(ctx, hour(-5, 20), false, &success)
		testContractStatsExtenderParty(ctx, fiveBack, ContractPartySource, hour(-5, 20))
		testContractStatsExtenderParty(ctx, fiveBack, ContractPartyDestination, hour(-5, 20))

		// one at the oldest edge of the window
		testContractStatsContract(ctx, hour(-23, 59), false, nil)

		// one just outside it: the 24th bucket back is not in the window
		outside := testContractStatsContract(ctx, hour(-24, 30), true, nil)
		testContractStatsExtenderParty(ctx, outside, ContractPartySource, hour(-24, 30))

		// two in the current partial bucket, one with an extender party
		testContractStatsContract(ctx, hour(0, 5), false, nil)
		current := testContractStatsContract(ctx, hour(0, 10), false, nil)
		testContractStatsExtenderParty(ctx, current, ContractPartySource, hour(0, 10))

		// cold cache: one grouped query per table over the whole missing
		// range, whatever that range holds
		window := CountContractHourWindow(ctx, now)
		stats := Testing_ContractHourCacheStats()
		if stats.CachedBuckets != 0 || stats.FilledBuckets != 23 || stats.FillQueries != 2 {
			t.Fatalf("a cold cache did %+v, want 23 buckets filled by 2 queries", stats)
		}
		if stats.LiveBuckets != 1 || stats.LiveQueries != 2 {
			t.Fatalf("a cold cache counted %+v live, want only the partial bucket", stats)
		}
		if window.Contracts != 8 {
			t.Fatalf("contracts = %d, want 8 (the 24th bucket back is outside)", window.Contracts)
		}
		if window.Disputes != 1 {
			t.Fatalf("disputes = %d, want 1", window.Disputes)
		}
		if window.WithExtender != 2 {
			t.Fatalf(
				"contracts with an extender = %d, want 2 (two parties of one contract count once)",
				window.WithExtender,
			)
		}

		// warm cache: every closed bucket comes from redis and only the
		// partial one is counted live
		warm := CountContractHourWindow(ctx, now)
		stats = Testing_ContractHourCacheStats()
		if stats.CachedBuckets != 23 || stats.FilledBuckets != 0 || stats.FillQueries != 0 {
			t.Fatalf("a warm cache did %+v, want 23 buckets from redis and no fill", stats)
		}
		if stats.LiveBuckets != 1 {
			t.Fatalf("a warm cache counted %+v live, want only the partial bucket", stats)
		}
		if warm != window {
			t.Fatalf("the warm window = %+v, want the cold one %+v", warm, window)
		}

		// a complete bucket is read from the cache, not rescanned: a row that
		// appears in one after it was cached does not change the sum
		testContractStatsContract(ctx, hour(-5, 40), false, nil)
		cached := CountContractHourWindow(ctx, now)
		if cached != window {
			t.Fatalf("a cached bucket was rescanned: %+v, want %+v", cached, window)
		}

		// the partial bucket is always live, so a row in it does
		partial := testContractStatsContract(ctx, hour(0, 12), false, nil)
		testContractStatsExtenderParty(ctx, partial, ContractPartySource, hour(0, 12))
		live := CountContractHourWindow(ctx, now)
		if live.Contracts != window.Contracts+1 || live.WithExtender != window.WithExtender+1 {
			t.Fatalf("the partial bucket was not recounted: %+v, want one more than %+v", live, window)
		}

		// and CountContracts carries the same three numbers through
		counts := CountContracts(ctx, now)
		if counts.Contracts24h != live.Contracts ||
			counts.Disputes24h != live.Disputes ||
			counts.ContractsWithExtender24h != live.WithExtender {
			t.Fatalf("CountContracts = %+v, want the window %+v", counts, live)
		}
	})
}

// M3: a bucket is complete only once its hour has been over for a minute, so
// an insert that was in flight when the hour turned can never be frozen out of
// the cache. Until then the just-ended hour is counted live like the current
// one.
func TestContractHourBucketSettles(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		base := testContractStatsBase

		// one contract in the hour that just ended
		testContractStatsContract(ctx, base.Add(-30*time.Minute), false, nil)

		// 30 seconds after the hour turned: the previous bucket has not
		// settled, so it is counted live along with the current partial one
		unsettled := base.Add(30 * time.Second)
		window := CountContractHourWindow(ctx, unsettled)
		stats := Testing_ContractHourCacheStats()
		if stats.LiveBuckets != 2 {
			t.Fatalf("live buckets = %d, want 2 (the unsettled hour and the partial one)", stats.LiveBuckets)
		}
		if stats.FilledBuckets != 22 {
			t.Fatalf("filled buckets = %d, want 22 (the unsettled hour is not cached)", stats.FilledBuckets)
		}
		if window.Contracts != 1 {
			t.Fatalf("contracts = %d, want the unsettled hour counted live", window.Contracts)
		}

		// the insert that was in flight when the hour turned commits into the
		// hour it belongs to
		testContractStatsContract(ctx, base.Add(-10*time.Minute), false, nil)

		// two minutes after the hour turned it has settled, and the bucket is
		// computed and cached with both rows
		settled := base.Add(2 * time.Minute)
		window = CountContractHourWindow(ctx, settled)
		stats = Testing_ContractHourCacheStats()
		if stats.LiveBuckets != 1 {
			t.Fatalf("live buckets = %d, want only the partial one", stats.LiveBuckets)
		}
		if window.Contracts != 2 {
			t.Fatalf("contracts = %d, want both rows of the settled hour", window.Contracts)
		}

		// and the settled bucket is now warm
		CountContractHourWindow(ctx, settled)
		stats = Testing_ContractHourCacheStats()
		if stats.FilledBuckets != 0 || stats.CachedBuckets != 23 {
			t.Fatalf("the settled bucket was not cached: %+v", stats)
		}
	})
}

// The cached buckets are per bucket start, so two windows an hour apart share
// every bucket they have in common and only the new one is filled.
func TestContractHourBucketsAreSharedAcrossWindows(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		base := testContractStatsBase

		first := base.Add(15 * time.Minute)
		CountContractHourWindow(ctx, first)
		if stats := Testing_ContractHourCacheStats(); stats.FilledBuckets != 23 {
			t.Fatalf("the first window filled %+v, want 23", stats)
		}

		// an hour later the window has moved by one bucket: 22 of its closed
		// buckets are already cached and only the newly closed one is filled
		second := first.Add(time.Hour)
		CountContractHourWindow(ctx, second)
		stats := Testing_ContractHourCacheStats()
		if stats.CachedBuckets != 22 || stats.FilledBuckets != 1 || stats.FillQueries != 2 {
			t.Fatalf("the next window did %+v, want 22 cached and 1 filled", stats)
		}

		// clearing the cache makes the same window cold again, which is what
		// a 26 hour expiry eventually does
		Testing_ClearContractHourBuckets(ctx, second)
		CountContractHourWindow(ctx, second)
		if stats := Testing_ContractHourCacheStats(); stats.CachedBuckets != 0 || stats.FilledBuckets != 23 {
			t.Fatalf("a cleared cache did %+v, want a cold fill", stats)
		}
	})
}
