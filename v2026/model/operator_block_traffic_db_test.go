package model

// The bytes served per operator block of GET /account/epochs
// (operator_block_traffic.go): the rollup counts a block's bytes with the
// award's own statement, the award's bytes replace it once the award commits,
// and a window counts the blocks that end in it, as its points do.

import (
	"context"
	"maps"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
)

// Bounds a test run reads a whole block within: a quarter day a statement.
func operatorBlockTrafficTestSettings() *OperatorBlockTrafficRollupSettings {
	settings := DefaultOperatorBlockTrafficRollupSettings()
	settings.ChunkDuration = 6 * time.Hour
	settings.MaxChunks = 1000
	settings.RunBudget = 10 * time.Minute
	return settings
}

// A contract of the payer with the provider, opened an hour before it closed
// with the outcome. Its sweep at swept carries bytes and belongs to the USDC
// payment paymentId when that is set. Without swept the contract has no
// sweep, as the degradation valve settles a contract without escrow.
func operatorBlockTrafficContract(
	t testing.TB,
	ctx context.Context,
	payer *operatorBlockPointsPayer,
	providerNetwork server.Id,
	providerClient server.Id,
	outcome string,
	closed time.Time,
	swept *time.Time,
	bytes ByteCount,
	paymentId *server.Id,
) {
	t.Helper()
	contractId := contractIdMintedAt(closed.Add(-time.Hour))
	usage := &contractUsageSnapshot{Version: 1, ByteCount: bytes, Providers: []contractProviderUsage{{ClientId: providerClient, NetworkId: providerNetwork, ByteCount: bytes}}}
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(ctx, `INSERT INTO transfer_contract
		(contract_id,source_network_id,source_id,destination_network_id,destination_id,transfer_byte_count,create_time,close_time,outcome,provider_usage,usage_origin_is_source)
		VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,true)`, contractId, payer.network, payer.client, providerNetwork, providerClient, bytes, closed.Add(-time.Hour), closed, outcome, usage))
		if swept != nil {
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO transfer_escrow_sweep
			(contract_id,balance_id,network_id,destination_id,payout_byte_count,payout_net_revenue_nano_cents,sweep_time,payment_id)
			VALUES($1,$2,$3,$4,$5,0,$6,$7)`, contractId, payer.balance, providerNetwork, providerClient, bytes, swept.UTC(), paymentId))
		}
	})
}

// The rollup's bytes of a block, per network.
func operatorBlockTrafficTestRows(t testing.TB, ctx context.Context, block int64) map[server.Id]ByteCount {
	t.Helper()
	byteCounts := map[server.Id]ByteCount{}
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(ctx, `SELECT network_id, payout_byte_count FROM operator_block_network_traffic WHERE block_number = $1`, block)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var networkId server.Id
				var byteCount ByteCount
				server.Raise(result.Scan(&networkId, &byteCount))
				byteCounts[networkId] = byteCount
			}
		})
	})
	return byteCounts
}

// The rollup's cursor of a block, or nil without a row.
func operatorBlockTrafficTestReadThrough(t testing.TB, ctx context.Context, block int64) *time.Time {
	t.Helper()
	var readThrough *time.Time
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(ctx, `SELECT read_through FROM operator_block_traffic WHERE block_number = $1`, block)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				var value time.Time
				server.Raise(result.Scan(&value))
				readThrough = &value
			}
		})
	})
	return readThrough
}

// The bytes the award's own read gives each provider network in the window.
func operatorBlockTrafficTestAwardRead(t testing.TB, ctx context.Context, start time.Time, end time.Time, cutoff time.Time) map[server.Id]ByteCount {
	t.Helper()
	traffic, err := readOperatorBlockTraffic(ctx, start, end, cutoff)
	if err != nil {
		t.Fatal(err)
	}
	byteCounts := map[server.Id]ByteCount{}
	for networkId, payee := range traffic.payees {
		byteCounts[networkId] = payee.payoutByteCount
	}
	return byteCounts
}

// One network's bytes served in each window.
func operatorBlockTrafficTestServed(ctx context.Context, networkId server.Id, now time.Time, windows ...[2]time.Time) []*AccountServedBytes {
	epochWindows := []PointsEpochWindow{}
	for i, window := range windows {
		epochWindows = append(epochWindows, PointsEpochWindow{Epoch: uint64(i + 1), Start: window[0], End: window[1]})
	}
	return GetAccountServedBytesInWindows(ctx, networkId, epochWindows, now)
}

// A completed block's bytes are the award's bytes: the sweeps no USDC payment
// owns with payout bytes, of contracts with a credit-bearing outcome that
// closed at or after the cutoff, by sweep time in the block's window, per
// provider network. A canceled contract's sweep, a contract settled without
// escrow, a sweep of no bytes, a contract that closed before the cutoff, a
// sweep USDC paid and the next block's sweep count nothing, and each network
// keeps its own bytes. A window counts the blocks whose end is in it, as its
// points do: a block's end is in exactly one of two adjacent windows.
func TestOperatorBlockTrafficCountsTheAwardsSweeps(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		block := int64(SubnetBlockNumber(server.NowUtc())) - 1
		blockStart := operatorBlockStart(block)
		blockEnd := blockStart.Add(SubnetBlockDuration)
		cutoff := blockStart.Add(-2 * 24 * time.Hour)
		useOperatorBlockPointsTransition(t, cutoff)
		windowStart, windowEnd, ok := OperatorBlockPointsWindow(block, cutoff)
		if !ok || !windowStart.Equal(blockStart) || !windowEnd.Equal(blockEnd) {
			t.Fatalf("block %d window [%s, %s)", block, windowStart, windowEnd)
		}
		gib := ByteCount(1024 * 1024 * 1024)
		day := 24 * time.Hour
		at := func(offset time.Duration) *time.Time {
			swept := blockStart.Add(offset)
			return &swept
		}

		payer := newOperatorBlockPointsPayer(t, ctx)
		providerA, clientA := operatorBlockPointsProvider(ctx)
		providerB, clientB := operatorBlockPointsProvider(ctx)
		canceledC, clientC := operatorBlockPointsProvider(ctx)
		unsweptD, clientD := operatorBlockPointsProvider(ctx)
		emptyE, clientE := operatorBlockPointsProvider(ctx)
		preCutoffF, clientF := operatorBlockPointsProvider(ctx)
		paidG, clientG := operatorBlockPointsProvider(ctx)
		usdcPaymentId := server.NewId()

		// the block's traffic
		operatorBlockTrafficContract(t, ctx, payer, providerA, clientA, ContractOutcomeSettled, *at(day), at(day), 2*gib, nil)
		operatorBlockTrafficContract(t, ctx, payer, providerA, clientA, ContractOutcomeSettled, *at(2 * day), at(2*day), gib, nil)
		operatorBlockTrafficContract(t, ctx, payer, providerB, clientB, ContractOutcomeSettled, *at(3 * day), at(3*day), gib, nil)
		operatorBlockTrafficContract(t, ctx, payer, providerB, clientB, ContractOutcomeDisputeResolvedToSource, *at(4 * day), at(4*day), gib/2, nil)
		// none of these counts
		operatorBlockTrafficContract(t, ctx, payer, canceledC, clientC, "canceled", *at(day), at(day), 4*gib, nil)
		operatorBlockTrafficContract(t, ctx, payer, unsweptD, clientD, ContractOutcomeSettled, *at(2 * day), nil, 4*gib, nil)
		operatorBlockTrafficContract(t, ctx, payer, emptyE, clientE, ContractOutcomeSettled, *at(2 * day), at(2*day), 0, nil)
		operatorBlockTrafficContract(t, ctx, payer, preCutoffF, clientF, ContractOutcomeDisputeResolvedToDestination, cutoff.Add(-time.Hour), at(2*day), 8*gib, nil)
		operatorBlockTrafficContract(t, ctx, payer, paidG, clientG, ContractOutcomeSettled, *at(2 * day), at(2*day), 4*gib, &usdcPaymentId)
		// the next block's traffic
		operatorBlockTrafficContract(t, ctx, payer, providerB, clientB, ContractOutcomeSettled, blockEnd.Add(time.Hour), at(SubnetBlockDuration+time.Hour), 16*gib, nil)

		// the block's last settlements are in
		now := blockEnd.Add(OperatorBlockPointsSettleTime).Add(time.Minute)
		chunkCount, err := AdvanceOperatorBlockTrafficRollup(ctx, operatorBlockTrafficTestSettings(), now)
		if err != nil {
			t.Fatal(err)
		}
		if chunkCount < 28 {
			t.Fatalf("the rollup read %d quarter days of the block", chunkCount)
		}
		want := map[server.Id]ByteCount{providerA: 3 * gib, providerB: gib + gib/2}
		if got := operatorBlockTrafficTestRows(t, ctx, block); !maps.Equal(got, want) {
			t.Fatalf("the rollup counted %v bytes, want %v", got, want)
		}
		if award := operatorBlockTrafficTestAwardRead(t, ctx, windowStart, windowEnd, cutoff); !maps.Equal(award, want) {
			t.Fatalf("the award reads %v bytes, the rollup %v", award, want)
		}
		if readThrough := operatorBlockTrafficTestReadThrough(t, ctx, block); readThrough == nil || !readThrough.Equal(blockEnd) {
			t.Fatalf("block %d is read through %v, want its end %s", block, readThrough, blockEnd)
		}

		// an epoch from two days before the block's end, and the one before
		epoch := [2]time.Time{blockEnd.Add(-2 * day), blockEnd.Add(5 * day)}
		before := [2]time.Time{blockEnd.Add(-9 * day), blockEnd.Add(-2 * day)}
		for networkId, byteCount := range map[server.Id]ByteCount{providerA: 3 * gib, providerB: gib + gib/2, canceledC: 0, unsweptD: 0, emptyE: 0, preCutoffF: 0, paidG: 0} {
			served := operatorBlockTrafficTestServed(ctx, networkId, now, epoch, before)
			if served[0].ByteCount != byteCount || served[0].AsOf == nil || !served[0].AsOf.Equal(blockEnd) {
				t.Fatalf("network %s served %d bytes as of %v in the epoch, want %d as of %s", networkId, served[0].ByteCount, served[0].AsOf, byteCount, blockEnd)
			}
			if served[1].ByteCount != 0 {
				t.Fatalf("network %s served %d bytes in the epoch before", networkId, served[1].ByteCount)
			}
		}
		// a block counts once: its end is in exactly one of two adjacent windows
		served := operatorBlockTrafficTestServed(ctx, providerA, now, [2]time.Time{blockEnd.Add(-time.Hour), blockEnd}, [2]time.Time{blockEnd, blockEnd.Add(time.Hour)})
		if served[0].ByteCount != 0 || served[0].AsOf != nil || served[1].ByteCount != 3*gib {
			t.Fatalf("block %d at its end: %d bytes before and %d from, want 0 and %d", block, served[0].ByteCount, served[1].ByteCount, 3*gib)
		}
	})
}

// The running figure: before a block completes, the rollup counts its bytes
// through the settled sweep time, which is the figure's as-of, for the epoch
// that holds the block's end and for the block itself; a block that has not
// started counts nothing. A run at the same time reads nothing more, a write
// from a cursor another run has moved adds nothing, and a later run adds the
// rest once, so the completed block's bytes are the award's.
func TestOperatorBlockTrafficRunsAheadOfTheAward(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		block := int64(SubnetBlockNumber(server.NowUtc())) - 1
		blockStart := operatorBlockStart(block)
		blockEnd := blockStart.Add(SubnetBlockDuration)
		cutoff := blockStart.Add(-2 * 24 * time.Hour)
		useOperatorBlockPointsTransition(t, cutoff)
		gib := ByteCount(1024 * 1024 * 1024)
		day := 24 * time.Hour
		at := func(offset time.Duration) *time.Time {
			swept := blockStart.Add(offset)
			return &swept
		}

		payer := newOperatorBlockPointsPayer(t, ctx)
		providerA, clientA := operatorBlockPointsProvider(ctx)
		providerB, clientB := operatorBlockPointsProvider(ctx)
		operatorBlockTrafficContract(t, ctx, payer, providerA, clientA, ContractOutcomeSettled, *at(day), at(day), gib, nil)
		operatorBlockTrafficContract(t, ctx, payer, providerB, clientB, ContractOutcomeSettled, *at(2 * day), at(2*day), gib, nil)
		// past the settled time of the first runs
		operatorBlockTrafficContract(t, ctx, payer, providerA, clientA, ContractOutcomeSettled, *at(4 * day), at(4*day), 2*gib, nil)

		settings := operatorBlockTrafficTestSettings()
		epoch := [2]time.Time{blockEnd.Add(-2 * day), blockEnd.Add(5 * day)}
		requireServed := func(now time.Time, networkId server.Id, byteCount ByteCount, asOf time.Time) {
			t.Helper()
			served := operatorBlockTrafficTestServed(ctx, networkId, now, epoch)[0]
			if served.ByteCount != byteCount || served.AsOf == nil || !served.AsOf.Equal(asOf) {
				t.Fatalf("network %s served %d bytes as of %v, want %d as of %s", networkId, served.ByteCount, served.AsOf, byteCount, asOf)
			}
		}

		// three days into the block: the block before has no award either,
		// so the rollup reads its window past the cutoff first
		settled := blockStart.Add(3 * day)
		now := settled.Add(settings.SettleTime)
		if _, err := AdvanceOperatorBlockTrafficRollup(ctx, settings, now); err != nil {
			t.Fatal(err)
		}
		if readThrough := operatorBlockTrafficTestReadThrough(t, ctx, block-1); readThrough == nil || !readThrough.Equal(blockStart) {
			t.Fatalf("the block before is read through %v, want %s", readThrough, blockStart)
		}
		requireServed(now, providerA, gib, settled)
		requireServed(now, providerB, gib, settled)
		// the open block's own figure is the same bytes, as of the same time;
		// the next block has not started and counts nothing
		windowBytes, blockBytes := GetAccountServedBytes(ctx, providerA, []PointsEpochWindow{{Epoch: 1, Start: epoch[0], End: epoch[1]}}, []int64{block, block + 1}, now)
		if len(windowBytes) != 1 || windowBytes[0].ByteCount != gib || len(blockBytes) != 2 {
			t.Fatalf("served %+v in the epoch and %+v in the blocks", windowBytes, blockBytes)
		}
		if blockBytes[0].ByteCount != gib || blockBytes[0].AsOf == nil || !blockBytes[0].AsOf.Equal(settled) {
			t.Fatalf("block %d served %d bytes as of %v, want %d as of %s", block, blockBytes[0].ByteCount, blockBytes[0].AsOf, gib, settled)
		}
		if blockBytes[1].ByteCount != 0 || blockBytes[1].AsOf != nil {
			t.Fatalf("block %d, not started, served %d bytes as of %v", block+1, blockBytes[1].ByteCount, blockBytes[1].AsOf)
		}

		// a second run at the same time reads nothing more
		chunkCount, err := AdvanceOperatorBlockTrafficRollup(ctx, settings, now)
		if err != nil || chunkCount != 0 {
			t.Fatalf("a second run read %d statements: %v", chunkCount, err)
		}
		requireServed(now, providerA, gib, settled)

		// a run that read from where the cursor was before adds nothing
		_, windowEnd, _ := OperatorBlockPointsWindow(block, cutoff)
		stale := &operatorBlockTrafficCursor{block: block, windowEnd: windowEnd, readThrough: blockStart, existing: true}
		traffic := newOperatorBlockTraffic()
		traffic.add(&operatorBlockSweepRow{payeeNetworkId: providerA, payoutByteCount: 5 * gib, minSweepTime: blockStart.Add(day), sweepCount: 1})
		if writeOperatorBlockTraffic(ctx, stale, settled, traffic, now) {
			t.Fatal("a stale cursor moved")
		}
		requireServed(now, providerA, gib, settled)

		// once the block's last settlements are in, the rest of it, once
		now = blockEnd.Add(settings.SettleTime)
		if _, err := AdvanceOperatorBlockTrafficRollup(ctx, settings, now); err != nil {
			t.Fatal(err)
		}
		requireServed(now, providerA, 3*gib, blockEnd)
		requireServed(now, providerB, gib, blockEnd)
		windowStart, _, _ := OperatorBlockPointsWindow(block, cutoff)
		award := operatorBlockTrafficTestAwardRead(t, ctx, windowStart, windowEnd, cutoff)
		if rollup := operatorBlockTrafficTestRows(t, ctx, block); !maps.Equal(award, rollup) {
			t.Fatalf("the award reads %v bytes, the rollup %v", award, rollup)
		}
	})
}

// Once the award commits, the bytes are the award's: the same bytes the
// rollup counted, which the award shared the block's payout points by, so a
// provider's bytes and points line up. The rollup then deletes the block's
// rows and never reads an awarded block again; the bytes stay.
func TestOperatorBlockTrafficAwardSupersedesTheRollup(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		block, awardNow, cutoff := operatorBlockPointsTestBlock(server.NowUtc())
		useOperatorBlockPointsTransition(t, cutoff)
		config := operatorBlockPointsTestConfig()
		config.ReliabilityPointsPerPayout = 0
		blockEnd := operatorBlockStart(block).Add(SubnetBlockDuration)
		gib := ByteCount(1024 * 1024 * 1024)

		payer := newOperatorBlockPointsPayer(t, ctx)
		providerA, clientA := operatorBlockPointsProvider(ctx)
		providerB, clientB := operatorBlockPointsProvider(ctx)
		inBlock := operatorBlockPointsTestTrafficTime(block, cutoff)
		payer.contract(t, ctx, providerA, clientA, inBlock, inBlock, 2*gib, 0)
		payer.contract(t, ctx, providerA, clientA, inBlock.Add(time.Hour), inBlock.Add(time.Hour), gib, 0)
		payer.contract(t, ctx, providerB, clientB, inBlock.Add(2*time.Hour), inBlock.Add(2*time.Hour), gib, 0)

		settings := operatorBlockTrafficTestSettings()
		// past the block's last settlements and into the next block, so the
		// awarded block is not the rollup's newest
		later := server.MaxTime(awardNow, blockEnd.Add(settings.SettleTime)).Add(time.Hour)
		if _, err := AdvanceOperatorBlockTrafficRollup(ctx, settings, later); err != nil {
			t.Fatal(err)
		}
		epoch := [2]time.Time{blockEnd.Add(-2 * 24 * time.Hour), blockEnd.Add(5 * 24 * time.Hour)}
		want := map[server.Id]ByteCount{providerA: 3 * gib, providerB: gib}
		requireServed := func() {
			t.Helper()
			for networkId, byteCount := range want {
				served := operatorBlockTrafficTestServed(ctx, networkId, later, epoch)[0]
				if served.ByteCount != byteCount || served.AsOf == nil || !served.AsOf.Equal(blockEnd) {
					t.Fatalf("network %s served %d bytes as of %v, want %d as of %s", networkId, served.ByteCount, served.AsOf, byteCount, blockEnd)
				}
			}
		}
		requireServed()

		awards, err := AwardDueOperatorBlockPoints(ctx, config, awardNow, block)
		if err != nil {
			t.Fatal(err)
		}
		var award *OperatorBlockPoints
		for _, a := range awards {
			if a.Block == block {
				award = a
			}
		}
		if award == nil {
			t.Fatalf("block %d has no award: %v", block, awards)
		}
		awardByteCounts := map[server.Id]ByteCount{}
		for _, payment := range award.Payments {
			awardByteCounts[payment.NetworkId] = payment.PayoutByteCount
		}
		if !maps.Equal(awardByteCounts, want) {
			t.Fatalf("the award shared its points by %v bytes, the rollup counted %v", awardByteCounts, want)
		}
		requireServed()
		// the epoch's points are the payout points the award shared by
		// exactly the bytes the epoch reports
		points := map[server.Id]NanoPoints{}
		for networkId := range want {
			points[networkId] = GetAccountNanoPointsInWindow(ctx, networkId, epoch[0], epoch[1])
		}
		operatorBlockPointsRequireByteShares(t, points, want, operatorBlockPointsConfiguredPayout(config, block, cutoff))

		// the next run deletes the awarded block's rollup rows
		if _, err := AdvanceOperatorBlockTrafficRollup(ctx, settings, later); err != nil {
			t.Fatal(err)
		}
		if rows := operatorBlockTrafficTestRows(t, ctx, block); len(rows) != 0 {
			t.Fatalf("the awarded block keeps rollup rows %v", rows)
		}
		if readThrough := operatorBlockTrafficTestReadThrough(t, ctx, block); readThrough != nil {
			t.Fatalf("the awarded block keeps its cursor at %s", readThrough)
		}
		requireServed()

		// an empty rollup starts after the awarded block
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `DELETE FROM operator_block_network_traffic`))
			server.RaisePgResult(tx.Exec(ctx, `DELETE FROM operator_block_traffic`))
		})
		if _, err := AdvanceOperatorBlockTrafficRollup(ctx, settings, later); err != nil {
			t.Fatal(err)
		}
		if readThrough := operatorBlockTrafficTestReadThrough(t, ctx, block); readThrough != nil {
			t.Fatalf("the rollup read the awarded block through %s", readThrough)
		}
		if readThrough := operatorBlockTrafficTestReadThrough(t, ctx, block+1); readThrough == nil {
			t.Fatal("the rollup did not start the block after the awarded one")
		}
		requireServed()
	})
}
