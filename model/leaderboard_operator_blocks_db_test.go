package model

import (
	"context"
	"testing"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/session"
)

// One payout window of the synthetic leaderboard fixtures: a legacy plan's
// subsidy epoch, or an awarded operator block.
func testingInsertLeaderboardLegacyWindow(t testing.TB, ctx context.Context, end time.Time) server.Id {
	t.Helper()
	planId := server.NewId()
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(ctx, `
			INSERT INTO subsidy_payment (
				payment_plan_id, start_time, end_time, active_user_count, paid_user_count,
				net_payout_byte_count_paid, net_payout_byte_count_unpaid, net_revenue_nano_cents, net_payout_nano_cents
			) VALUES ($1, $2, $3, 0, 0, 0, 0, 0, 0)
		`, planId, end.Add(-SubnetBlockDuration).UTC(), end.UTC()))
	})
	return planId
}

func testingInsertLeaderboardOperatorBlockAward(t testing.TB, ctx context.Context, block int64) server.Id {
	t.Helper()
	planId := OperatorBlockPointsPlanId(block)
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(ctx, `
			INSERT INTO payment_report (
				payment_plan_id, total_payout_nano_cents, total_nano_points, point_scale_factor, time_scale_factor, payout_points_per_payout
			) VALUES ($1, 0, 0, 1, 1, 0)
		`, planId))
	})
	return planId
}

// A provider network's payment in a window, carrying its provided bytes as a
// plan's payment or a block award's payment does.
func testingInsertLeaderboardPayment(t testing.TB, ctx context.Context, networkId server.Id, planId server.Id, byteCount ByteCount, at time.Time) {
	t.Helper()
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(ctx, `
			INSERT INTO account_payment (
				payment_id, payment_plan_id, network_id, wallet_id,
				payout_byte_count, payout_nano_cents, min_sweep_time,
				create_time, block_rollup_complete
			) VALUES ($1, $2, $3, NULL, $4, 0, $5, $5, true)
		`, server.NewId(), planId, networkId, byteCount, at.UTC()))
	})
}

// The root cause: the data leaderboard, the network ranking (/network/ranking
// and the epoch earnings email's rank) summed the payments of the 4 newest
// subsidy epochs only. Past the provider earning boundary no payout plan forms
// a subsidy epoch, so the windows stopped at the last legacy plans and the
// board froze: a network providing only past the boundary had no row at all,
// and networks of the last legacy weeks kept their rank forever. The windows
// now include every awarded operator block, newest first by their end, and
// still only the newest 4 count.
func TestDataLeaderboardWindowsIncludeAwardedOperatorBlocks(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		mib := ByteCount(1024 * 1024)

		// two completed operator blocks, and four legacy subsidy epochs that
		// ended before the older one opened, as the last plans ended at the
		// boundary
		current := int64(SubnetBlockNumber(server.NowUtc()))
		blockA := current - 2
		blockB := current - 1
		legacyEnd := operatorBlockStart(blockA)
		legacy := []server.Id{}
		for i := 4; 1 <= i; i -= 1 {
			legacy = append(legacy, testingInsertLeaderboardLegacyWindow(t, ctx, legacyEnd.Add(-time.Duration(i-1)*24*time.Hour)))
		}
		// legacy[0] is the oldest window and legacy[3] the newest
		awardA := testingInsertLeaderboardOperatorBlockAward(t, ctx, blockA)
		awardB := testingInsertLeaderboardOperatorBlockAward(t, ctx, blockB)

		networkIds := map[string]server.Id{}
		for _, name := range []string{"old-legacy", "older-legacy", "last-legacy", "past-boundary"} {
			networkId := server.NewId()
			Testing_CreateNetwork(ctx, networkId, "leaderboard-"+name, server.NewId())
			networkIds[name] = networkId
		}
		// only in the two oldest legacy windows: out of the newest 4
		testingInsertLeaderboardPayment(t, ctx, networkIds["old-legacy"], legacy[0], 900*mib, legacyEnd)
		testingInsertLeaderboardPayment(t, ctx, networkIds["older-legacy"], legacy[1], 800*mib, legacyEnd)
		// the two newest legacy windows
		testingInsertLeaderboardPayment(t, ctx, networkIds["last-legacy"], legacy[2], 100*mib, legacyEnd)
		testingInsertLeaderboardPayment(t, ctx, networkIds["last-legacy"], legacy[3], 100*mib, legacyEnd)
		// both awarded blocks: its only provided bytes
		testingInsertLeaderboardPayment(t, ctx, networkIds["past-boundary"], awardA, 300*mib, operatorBlockStart(blockA))
		testingInsertLeaderboardPayment(t, ctx, networkIds["past-boundary"], awardB, 400*mib, operatorBlockStart(blockB))
		// the open block is not a window until its award exists
		testingInsertLeaderboardPayment(t, ctx, networkIds["past-boundary"], OperatorBlockPointsPlanId(current), 5000*mib, operatorBlockStart(current))

		rankings, err := GetNetworkLeaderboardRankings(ctx)
		if err != nil {
			t.Fatal(err)
		}
		want := map[string]NetworkRanking{
			"past-boundary": {NetMiBCount: 700, LeaderboardRank: 1},
			"last-legacy":   {NetMiBCount: 200, LeaderboardRank: 2},
		}
		for name, networkId := range networkIds {
			got, ok := rankings[networkId]
			wantRanking, ranked := want[name]
			if ranked != ok {
				t.Fatalf("network %s ranked=%t, want ranked=%t (%+v)", name, ok, ranked, got)
			}
			if ranked && (got.NetMiBCount != wantRanking.NetMiBCount || got.LeaderboardRank != wantRanking.LeaderboardRank) {
				t.Fatalf("network %s ranking %+v, want %+v", name, got, wantRanking)
			}
		}
		if len(rankings) != len(want) {
			t.Fatalf("%d ranked networks, want %d: %+v", len(rankings), len(want), rankings)
		}

		earners, err := GetLeaderboard(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if len(earners) != 2 || earners[0].NetMiBCount != 700 || earners[1].NetMiBCount != 200 {
			t.Fatalf("leaderboard %+v, want 700 MiB then 200 MiB", earners)
		}
	})
}

// The same through the award itself: a network whose only traffic is past the
// boundary appears on the board and in the ranking with the bytes its block
// award recorded, beside a network paid by the last legacy plan.
func TestDataLeaderboardCountsAPostBoundaryBlockAward(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		block, now, cutoff := operatorBlockPointsTestBlock(server.NowUtc())
		useOperatorBlockPointsTransition(t, cutoff)

		config := operatorBlockPointsTestConfig()
		config.ReliabilityPointsPerPayout = 0
		config.ReliabilitySubsidyPerPayoutUsd = 0
		config.UsdPerActiveUser = 0
		config.SubscriptionNetRevenueFraction = 0
		config.MinWalletPayoutUsd = 0
		gib := ByteCount(1024 * 1024 * 1024)
		mib := ByteCount(1024 * 1024)

		payer := newOperatorBlockPointsPayer(t, ctx)
		providerA, clientA := operatorBlockPointsProvider(ctx)
		providerB, clientB := operatorBlockPointsProvider(ctx)
		for _, provider := range [][2]server.Id{{providerA, clientA}, {providerB, clientB}} {
			providerSession := session.Testing_CreateClientSession(ctx, &session.ByJwt{NetworkId: provider[0], ClientId: &provider[1]})
			SetNetworkLeaderboardPublic(true, providerSession)
			providerSession.Cancel()
		}

		// B: paid by the legacy plan, for traffic before the boundary
		legacyClose := cutoff.Add(-time.Hour)
		payer.contract(t, ctx, providerB, clientB, legacyClose, legacyClose, 5*mib, UsdToNanoCents(1))
		if _, err := CreatePaymentPlan(ctx, config, false, 0); err != nil {
			t.Fatal(err)
		}
		// A: only past the boundary, in the block
		inBlock := operatorBlockPointsTestTrafficTime(block, cutoff)
		payer.contract(t, ctx, providerA, clientA, inBlock, inBlock, 2*gib, 0)
		payer.contract(t, ctx, providerA, clientA, inBlock.Add(time.Hour), inBlock.Add(time.Hour), gib, 0)

		awards, err := AwardDueOperatorBlockPoints(ctx, config, now, block)
		if err != nil {
			t.Fatal(err)
		}
		awarded := false
		for _, award := range awards {
			awarded = awarded || award.Block == block
		}
		if !awarded {
			t.Fatalf("block %d has no award: %v", block, awards)
		}

		rankings, err := GetNetworkLeaderboardRankings(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if got := rankings[providerA]; got.NetMiBCount != float32(3*gib/mib) || got.LeaderboardRank != 1 {
			t.Fatalf("past-boundary provider ranking %+v, want %d MiB at rank 1", got, 3*gib/mib)
		}
		if got := rankings[providerB]; got.NetMiBCount != 5 || got.LeaderboardRank != 2 {
			t.Fatalf("legacy provider ranking %+v, want 5 MiB at rank 2", got)
		}

		earners, err := GetLeaderboard(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if len(earners) != 2 || earners[0].NetworkId != providerA.String() || earners[0].NetMiBCount != float32(3*gib/mib) || earners[1].NetworkId != providerB.String() {
			t.Fatalf("leaderboard %+v, want A (%d MiB) then B", earners, 3*gib/mib)
		}
	})
}
