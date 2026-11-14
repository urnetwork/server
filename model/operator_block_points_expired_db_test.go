package model

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/urnetwork/server"
)

// Three whole operator blocks after the wall clock's open block, with the
// earning boundary at the first one's start: previous, the block that expires,
// and next. Every retention check of their awards is then the award clock's,
// which a test sets: the check an award makes on the wall clock after its reads
// passes for a window that starts after the wall clock. Each block carries one
// GiB of a provider's traffic a day after it opens. The contracts' ids are
// minted at the wall clock, as contract partitions start at the current week.
type operatorBlockPointsExpiryFixture struct {
	previous int64
	block    int64
	next     int64
	cutoff   time.Time
	provider server.Id
	config   *SubsidyConfig
}

func newOperatorBlockPointsExpiryFixture(t testing.TB, ctx context.Context) *operatorBlockPointsExpiryFixture {
	t.Helper()
	wall := server.NowUtc()
	previous := int64(SubnetBlockNumber(wall)) + 2
	f := &operatorBlockPointsExpiryFixture{
		previous: previous,
		block:    previous + 1,
		next:     previous + 2,
		cutoff:   operatorBlockStart(previous),
		config:   operatorBlockPointsTestConfig(),
	}
	f.config.ReliabilityPointsPerPayout = 0
	useOperatorBlockPointsTransition(t, f.cutoff)

	payer := newOperatorBlockPointsPayer(t, ctx)
	provider, providerClient := operatorBlockPointsProvider(ctx)
	f.provider = provider
	gib := ByteCount(1024 * 1024 * 1024)
	for _, block := range []int64{f.previous, f.block, f.next} {
		payer.contractMintedAt(t, ctx, wall, provider, providerClient, operatorBlockStart(block).Add(24*time.Hour), gib)
	}
	return f
}

// A settled contract of the payer with the provider, minted at `minted`,
// opened an hour before it closed at `swept`, and its sweep then.
func (self *operatorBlockPointsPayer) contractMintedAt(
	t testing.TB,
	ctx context.Context,
	minted time.Time,
	providerNetwork server.Id,
	providerClient server.Id,
	swept time.Time,
	bytes ByteCount,
) server.Id {
	t.Helper()
	contractId := contractIdMintedAt(minted)
	usage := &contractUsageSnapshot{Version: 1, ByteCount: bytes, Providers: []contractProviderUsage{{ClientId: providerClient, NetworkId: providerNetwork, ByteCount: bytes}}}
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(ctx, `INSERT INTO transfer_contract
		(contract_id,source_network_id,source_id,destination_network_id,destination_id,transfer_byte_count,create_time,close_time,outcome,provider_usage,usage_origin_is_source)
		VALUES($1,$2,$3,$4,$5,$6,$7,$8,'settled',$9,true)`, contractId, self.network, self.client, providerNetwork, providerClient, bytes, swept.Add(-time.Hour), swept, usage))
		server.RaisePgResult(tx.Exec(ctx, `INSERT INTO transfer_escrow_sweep
		(contract_id,balance_id,network_id,destination_id,payout_byte_count,payout_net_revenue_nano_cents,sweep_time)
		VALUES($1,$2,$3,$4,$5,0,$6)`, contractId, self.balance, providerNetwork, providerClient, bytes, swept))
	})
	return contractId
}

// Every expiry record, in block order.
func operatorBlockPointsTestExpiries(t testing.TB, ctx context.Context) (expiries []*OperatorBlockPointsExpiry) {
	t.Helper()
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(ctx, `
			SELECT block_number, window_start, window_end, reliability_min_block_number, retention_min_block_number, expire_time
			FROM operator_block_points_expired
			ORDER BY block_number
		`)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				expiry := &OperatorBlockPointsExpiry{}
				server.Raise(result.Scan(
					&expiry.Block,
					&expiry.WindowStart,
					&expiry.WindowEnd,
					&expiry.ReliabilityMinBlockNumber,
					&expiry.RetentionMinBlockNumber,
					&expiry.ExpireTime,
				))
				expiries = append(expiries, expiry)
			}
		})
	})
	return
}

// The record is the expiry the award clock found.
func operatorBlockPointsRequireExpiry(t testing.TB, got *OperatorBlockPointsExpiry, block int64, cutoff time.Time, now time.Time) {
	t.Helper()
	want, ok := OperatorBlockPointsExpiryAt(block, cutoff, now)
	if !ok {
		t.Fatalf("block %d is not expired at %s", block, now)
	}
	if got.Block != want.Block || !got.WindowStart.Equal(want.WindowStart) || !got.WindowEnd.Equal(want.WindowEnd) ||
		got.ReliabilityMinBlockNumber != want.ReliabilityMinBlockNumber || got.RetentionMinBlockNumber != want.RetentionMinBlockNumber ||
		!got.ExpireTime.Equal(want.ExpireTime) {
		t.Fatalf("expiry record %+v, want %+v", got, want)
	}
}

// The Sunday the block's last settlements are in: the first award clock at
// which it is due.
func operatorBlockPointsSettled(block int64) time.Time {
	return operatorBlockStart(block + 1).Add(OperatorBlockPointsSettleTime)
}

// The root cause of the stall: the daily task awards its blocks oldest first
// and stops at the first error, and a block whose reliability evidence is past
// client_reliability retention (10 days after it opens) fails closed on every
// run. So once one block passed its deadline unawarded (its award failed for
// three days, or no taskworker ran it), every run stopped at it: the next
// block, due a week later, was never awarded either, until it too passed
// retention, and so on for good. Here the previous block is awarded on time,
// the block's award never runs before its deadline, and the daily runs after
// the next block closes each try the block first. The next block must be
// awarded by the first of them, and the expired block never.
func TestOperatorBlockPointsExpiredBlockDoesNotStallLaterBlocks(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		f := newOperatorBlockPointsExpiryFixture(t, ctx)

		// on time, by a task first scheduled while the previous block was open
		awards, err := AwardDueOperatorBlockPoints(ctx, f.config, operatorBlockPointsSettled(f.previous), f.previous)
		if err != nil || len(awards) != 1 || awards[0].Block != f.previous {
			t.Fatalf("the previous block %d was not awarded on time: %v %v", f.previous, awards, err)
		}

		// the next block is due from its settle time until its own window
		// passes retention, three days later
		firstRun := operatorBlockPointsSettled(f.next)
		nextStart, _, _ := OperatorBlockPointsWindow(f.next, f.cutoff)
		nextMinBlockNumber, _ := operatorBlockReliabilityBlockWindow(nextStart, nextStart.Add(SubnetBlockDuration))
		runErrs := []error{}
		lastRun := firstRun
		for run := firstRun; run.Before(firstRun.Add(4 * 24 * time.Hour)); run = run.Add(24 * time.Hour) {
			lastRun = run
			if _, err := AwardDueOperatorBlockPoints(ctx, f.config, run, f.previous); err != nil {
				runErrs = append(runErrs, err)
			}
		}
		if operatorBlockReliabilityRetained(nextMinBlockNumber, lastRun) {
			t.Fatalf("the last daily run at %s is inside the next block's retention", lastRun)
		}

		var firstErr error
		if 0 < len(runErrs) {
			firstErr = runErrs[0]
		}
		awarded := GetOperatorBlockPointsAwarded(ctx, []int64{f.previous, f.block, f.next})
		if !awarded[f.next] {
			t.Fatalf("block %d was never awarded: %d of the daily runs from %s to %s failed, the first with %v, and it then passed client_reliability retention itself", f.next, len(runErrs), firstRun.Format(time.RFC3339), lastRun.Format(time.RFC3339), firstErr)
		}
		if awarded[f.block] {
			t.Fatalf("block %d was awarded past client_reliability retention", f.block)
		}
		if firstErr != nil {
			t.Fatalf("%d daily runs failed, the first with %v", len(runErrs), firstErr)
		}
	})
}

// The first daily run after the next block closes records the expired block
// once, with the window and retention it found, counts it once, and awards
// the next block. Later runs, through the next block's own deadline, and a
// second commit of its award write nothing: no block is awarded twice, an
// awarded block is never recorded expired, and the expired block's traffic
// earns nothing.
func TestOperatorBlockPointsExpiryIsRecordedOnceAndNoBlockIsAwardedTwice(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		f := newOperatorBlockPointsExpiryFixture(t, ctx)
		if _, err := AwardDueOperatorBlockPoints(ctx, f.config, operatorBlockPointsSettled(f.previous), f.previous); err != nil {
			t.Fatal(err)
		}

		countedBefore := testutil.ToFloat64(operatorBlockPointsExpiredBlocksCounter)
		firstRun := operatorBlockPointsSettled(f.next)
		awards, err := AwardDueOperatorBlockPoints(ctx, f.config, firstRun, f.previous)
		if err != nil || len(awards) != 1 || awards[0].Block != f.next {
			t.Fatalf("the first run after block %d closed: %v %v", f.next, awards, err)
		}
		expiries := operatorBlockPointsTestExpiries(t, ctx)
		if len(expiries) != 1 {
			t.Fatalf("%d expiry records, want block %d's: %+v", len(expiries), f.block, expiries)
		}
		operatorBlockPointsRequireExpiry(t, expiries[0], f.block, f.cutoff, firstRun)
		if counted := testutil.ToFloat64(operatorBlockPointsExpiredBlocksCounter) - countedBefore; counted != 1 {
			t.Fatalf("the expiry was counted %v times", counted)
		}

		before := operatorBlockPointsRowCounts(t, ctx)
		for run := firstRun; run.Before(firstRun.Add(4 * 24 * time.Hour)); run = run.Add(24 * time.Hour) {
			again, err := AwardDueOperatorBlockPoints(ctx, f.config, run, f.previous)
			if err != nil || len(again) != 0 {
				t.Fatalf("the run at %s awarded %v: %v", run, again, err)
			}
		}
		committed, err := CommitOperatorBlockPoints(ctx, awards[0])
		if err != nil || committed {
			t.Fatalf("a second commit of block %d: %t %v", f.next, committed, err)
		}
		if after := operatorBlockPointsRowCounts(t, ctx); after != before {
			t.Fatalf("later runs wrote rows: %v -> %v", before, after)
		}
		if expiries := operatorBlockPointsTestExpiries(t, ctx); len(expiries) != 1 || expiries[0].Block != f.block || !expiries[0].ExpireTime.Equal(firstRun) {
			t.Fatalf("later runs changed the expiry records: %+v", expiries)
		}
		if counted := testutil.ToFloat64(operatorBlockPointsExpiredBlocksCounter) - countedBefore; counted != 1 {
			t.Fatalf("the expiry was counted %v times", counted)
		}

		// the provider's points are those of the previous and next blocks,
		// one award each
		planBlocks := map[server.Id]int64{
			OperatorBlockPointsPlanId(f.previous): f.previous,
			OperatorBlockPointsPlanId(f.next):     f.next,
		}
		pointBlocks := map[int64]int{}
		for _, row := range operatorBlockPointsTestRows(t, ctx, f.provider) {
			block, ok := planBlocks[row.planId]
			if !ok || !slices.Equal(row.blocks, []int64{block}) {
				t.Fatalf("a point of plan %s in blocks %v", row.planId, row.blocks)
			}
			pointBlocks[block] += 1
		}
		if pointBlocks[f.previous] != 1 || pointBlocks[f.next] != 1 {
			t.Fatalf("payout points by block %v", pointBlocks)
		}
		server.Db(ctx, func(conn server.PgConn) {
			var reports int
			server.Raise(conn.QueryRow(ctx, `SELECT COUNT(*) FROM payment_report WHERE payment_plan_id = ANY($1::uuid[])`,
				[]server.Id{OperatorBlockPointsPlanId(f.previous), OperatorBlockPointsPlanId(f.block), OperatorBlockPointsPlanId(f.next)}).Scan(&reports))
			if reports != 2 {
				t.Fatalf("%d payment reports for the three blocks, want 2", reports)
			}
		})
	})
}

// An expired block is never awarded: once the daily run after its deadline
// records it, a commit of its award computed in time, as a backfill's dry
// run made before the deadline would hold it, is refused and writes nothing,
// and an award computed now fails closed. A block with an award is never
// recorded expired.
func TestOperatorBlockPointsNeverAwardsAnExpiredBlock(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		f := newOperatorBlockPointsExpiryFixture(t, ctx)
		if _, err := AwardDueOperatorBlockPoints(ctx, f.config, operatorBlockPointsSettled(f.previous), f.previous); err != nil {
			t.Fatal(err)
		}
		inTime, err := ComputeOperatorBlockPoints(ctx, f.config, f.block, operatorBlockPointsSettled(f.block))
		if err != nil || len(inTime.Points) == 0 {
			t.Fatalf("block %d computed in time: %+v %v", f.block, inTime, err)
		}

		// past the block's deadline, before the next block is due
		late := operatorBlockStart(f.block).Add(ClientReliabilityRetention + time.Hour)
		if !late.Before(operatorBlockPointsSettled(f.next)) {
			t.Fatalf("the late run at %s is after block %d is due", late, f.next)
		}
		awards, err := AwardDueOperatorBlockPoints(ctx, f.config, late, f.previous)
		if err != nil || len(awards) != 0 {
			t.Fatalf("the late run: %v %v", awards, err)
		}
		expiries := operatorBlockPointsTestExpiries(t, ctx)
		if len(expiries) != 1 {
			t.Fatalf("%d expiry records, want block %d's: %+v", len(expiries), f.block, expiries)
		}
		operatorBlockPointsRequireExpiry(t, expiries[0], f.block, f.cutoff, late)

		before := operatorBlockPointsRowCounts(t, ctx)
		committed, err := CommitOperatorBlockPoints(ctx, inTime)
		if committed || !errors.Is(err, ErrOperatorBlockPointsExpired) {
			t.Fatalf("a commit of expired block %d: %t %v", f.block, committed, err)
		}
		if _, err := ComputeOperatorBlockPoints(ctx, f.config, f.block, late); !errors.Is(err, ErrOperatorBlockPointsReliabilityExpired) {
			t.Fatalf("an award of expired block %d computed: %v", f.block, err)
		}
		if after := operatorBlockPointsRowCounts(t, ctx); after != before {
			t.Fatalf("a refused commit wrote rows: %v -> %v", before, after)
		}
		if GetOperatorBlockPointsAwarded(ctx, []int64{f.block})[f.block] {
			t.Fatalf("expired block %d has an award", f.block)
		}

		// the previous block is past its deadline too, and has its award
		expiry, ok := OperatorBlockPointsExpiryAt(f.previous, f.cutoff, late)
		if !ok {
			t.Fatalf("block %d is not expired at %s", f.previous, late)
		}
		recorded, err := recordOperatorBlockPointsExpiry(ctx, expiry)
		if err != nil || recorded {
			t.Fatalf("awarded block %d was recorded expired: %t %v", f.previous, recorded, err)
		}
		if expired := GetOperatorBlockPointsExpired(ctx, []int64{f.previous, f.block}); expired[f.previous] || !expired[f.block] {
			t.Fatalf("expiry records %v", expired)
		}
	})
}

// Any error other than an expiry still stops the run at its block, and the
// retry awards the blocks in order: the task owns only the blocks after its
// last award, so a block passed over on such an error would never be tried
// again. Two blocks are due with the first still retained only when the
// boundary is late in the first, so its window is its last day. The first
// block's two providers serve more bytes together than a ByteCount holds, so
// its award fails until one sweep is gone, while the second's would succeed.
// Neither run records an expiry.
func TestOperatorBlockPointsOtherErrorsStillStopTheRunInOrder(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		wall := server.NowUtc()
		first := int64(SubnetBlockNumber(wall)) + 2
		second := first + 1
		cutoff := operatorBlockStart(second).Add(-24 * time.Hour)
		useOperatorBlockPointsTransition(t, cutoff)
		config := operatorBlockPointsTestConfig()
		config.ReliabilityPointsPerPayout = 0

		payer := newOperatorBlockPointsPayer(t, ctx)
		providerA, clientA := operatorBlockPointsProvider(ctx)
		providerB, clientB := operatorBlockPointsProvider(ctx)
		half := ByteCount(4 * 1024 * 1024 * 1024 * 1024 * 1024 * 1024)
		overflow := payer.contractMintedAt(t, ctx, wall, providerA, clientA, cutoff.Add(time.Hour), half)
		payer.contractMintedAt(t, ctx, wall, providerB, clientB, cutoff.Add(2*time.Hour), half)
		payer.contractMintedAt(t, ctx, wall, providerA, clientA, operatorBlockStart(second).Add(24*time.Hour), 1024*1024*1024)

		run := operatorBlockPointsSettled(second)
		if due := OperatorBlockPointsDueBlocks(cutoff, run); !slices.Equal(due, []int64{first, second}) {
			t.Fatalf("due blocks at %s: %v", run, due)
		}
		if _, expired := OperatorBlockPointsExpiryAt(first, cutoff, run); expired {
			t.Fatalf("block %d is expired at %s", first, run)
		}
		countedBefore := testutil.ToFloat64(operatorBlockPointsExpiredBlocksCounter)

		awards, err := AwardDueOperatorBlockPoints(ctx, config, run, first)
		if err == nil || len(awards) != 0 {
			t.Fatalf("the run went past failing block %d: %v %v", first, awards, err)
		}
		if awarded := GetOperatorBlockPointsAwarded(ctx, []int64{first, second}); awarded[first] || awarded[second] {
			t.Fatalf("awards after a failing block: %v", awarded)
		}

		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `DELETE FROM transfer_escrow_sweep WHERE contract_id = $1`, overflow))
		})
		awards, err = AwardDueOperatorBlockPoints(ctx, config, run, first)
		if err != nil || len(awards) != 2 || awards[0].Block != first || awards[1].Block != second {
			t.Fatalf("the retry: %v %v", awards, err)
		}
		if expiries := operatorBlockPointsTestExpiries(t, ctx); len(expiries) != 0 {
			t.Fatalf("on-time runs recorded expiries: %+v", expiries)
		}
		if counted := testutil.ToFloat64(operatorBlockPointsExpiredBlocksCounter) - countedBefore; counted != 0 {
			t.Fatalf("on-time runs counted %v expiries", counted)
		}
	})
}
