package model

import (
	"context"
	"testing"
	"time"

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
		swept := operatorBlockStart(block).Add(24 * time.Hour)
		contractId := contractIdMintedAt(wall)
		usage := &contractUsageSnapshot{Version: 1, ByteCount: gib, Providers: []contractProviderUsage{{ClientId: providerClient, NetworkId: provider, ByteCount: gib}}}
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO transfer_contract
			(contract_id,source_network_id,source_id,destination_network_id,destination_id,transfer_byte_count,create_time,close_time,outcome,provider_usage,usage_origin_is_source)
			VALUES($1,$2,$3,$4,$5,$6,$7,$8,'settled',$9,true)`, contractId, payer.network, payer.client, provider, providerClient, gib, swept.Add(-time.Hour), swept, usage))
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO transfer_escrow_sweep
			(contract_id,balance_id,network_id,destination_id,payout_byte_count,payout_net_revenue_nano_cents,sweep_time)
			VALUES($1,$2,$3,$4,$5,0,$6)`, contractId, payer.balance, provider, providerClient, gib, swept))
		})
	}
	return f
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
