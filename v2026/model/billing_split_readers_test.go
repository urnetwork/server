// The billing split's remaining readers, pinned across the removal of the
// values the split wrote that nothing read (BILLING-SPLIT.md, phase 1): the
// lifetime provider totals (account_balance.provided_* with their queued
// projection rows, and the {account_balance_<id>} Redis counters) and the
// sweep's destination_id. One real multi-hop settlement feeds every reader a
// user or the SN pool sees, and each must return exactly what it returned
// while settlement still wrote those values. The reader test passes unchanged
// on the code before the removal; the removal test fails there.
package model

import (
	"context"
	"encoding/json"
	"maps"
	"slices"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/task"
)

// One settled contract of payer network P through three service hops: the
// egress client on network A and intermediaries on network B and on P itself.
// The client ids fix the participant order, so the even split of 121 bytes
// and of the 121 nanocents of provider revenue (a balance worth two
// nanocents a byte, half to providers) is 41/40/40, and P's own hop keeps its
// 40 out of the split while it stays provider usage.
type billingSplitReadersFixture struct {
	start                              time.Time
	payerNetworkId                     server.Id
	networkA, networkB                 server.Id
	originId, egressId, hopBId, hopPId server.Id
	contractId, balanceId              server.Id
	egressBytes, hopBBytes, hopPBytes  ByteCount
	egressRevenue, hopBRevenue         NanoCents
}

func newBillingSplitReadersFixture(t testing.TB, ctx context.Context) *billingSplitReadersFixture {
	t.Helper()
	f := &billingSplitReadersFixture{
		start:          server.NowUtc(),
		payerNetworkId: server.NewId(),
		networkA:       server.NewId(),
		networkB:       server.NewId(),
		originId:       contractPayoutTestId(1),
		egressId:       contractPayoutTestId(2),
		hopBId:         contractPayoutTestId(3),
		hopPId:         contractPayoutTestId(4),
		egressBytes:    41,
		hopBBytes:      40,
		hopPBytes:      40,
		egressRevenue:  41,
		hopBRevenue:    40,
	}
	const used = ByteCount(121)
	addContractPayoutTestClients(ctx, map[server.Id]server.Id{
		f.originId: f.payerNetworkId, f.egressId: f.networkA, f.hopBId: f.networkB, f.hopPId: f.payerNetworkId,
	})
	f.balanceId = addContractPayoutTestBalance(ctx, f.payerNetworkId, used).BalanceId
	escrow, err := CreateTransferEscrow(ctx, f.payerNetworkId, f.originId, f.networkA, f.egressId, used)
	if err != nil {
		t.Fatal(err)
	}
	f.contractId = escrow.ContractId
	intermediaries := []server.Id{f.hopBId, f.hopPId}
	streamId := AddToStream(ctx, f.contractId, f.originId, f.egressId, intermediaries)
	if err := SetContractStream(ctx, f.contractId, streamId, intermediaries); err != nil {
		t.Fatal(err)
	}
	if err := CloseContract(ctx, f.contractId, f.originId, used, false); err != nil {
		t.Fatal(err)
	}
	if err := CloseContract(ctx, f.contractId, f.egressId, used, false); err != nil {
		t.Fatal(err)
	}
	if _, closed := GetContractClose(ctx, f.contractId); !closed {
		t.Fatal("fixture contract did not settle")
	}
	return f
}

// What every remaining reader of the split returns for the fixture: the Unpaid
// Data the apps show, the provider payout stats, the operator block points
// traffic and the points an award gives it, and the SN pool's provider usage.
func TestBillingSplitReadersAfterSettlement(t *testing.T) {
	testEnv := server.DefaultTestEnv()
	testEnv.RerunCount = 0
	testEnv.Run(t, func(t testing.TB) {
		ctx := context.Background()
		f := newBillingSplitReadersFixture(t, ctx)
		windowStart, windowEnd := f.start.Add(-time.Hour), server.NowUtc().Add(time.Hour)

		// the sweeps: one per provider network outside the payer's
		wantSweeps := map[server.Id]contractPayoutTestAmount{
			f.networkA: {byteCount: f.egressBytes, payout: f.egressRevenue},
			f.networkB: {byteCount: f.hopBBytes, payout: f.hopBRevenue},
		}
		if got := contractPayoutTestAmounts(t, ctx, f.contractId); !maps.Equal(got, wantSweeps) {
			t.Fatalf("sweeps = %v, want %v", got, wantSweeps)
		}
		assertContractPayoutTestProviderAllocations(t, ctx, f.contractId, map[server.Id]int64{f.egressId: f.egressBytes, f.hopBId: f.hopBBytes})

		// /transfer/stats, the apps' Unpaid Data: none since USDC payouts
		// ended (owner decision 2026-11-13)
		for networkId, want := range map[server.Id]TransferStats{
			f.networkA:       {PaidBytesProvided: 0, UnpaidBytesProvided: 0},
			f.networkB:       {PaidBytesProvided: 0, UnpaidBytesProvided: 0},
			f.payerNetworkId: {PaidBytesProvided: 0, UnpaidBytesProvided: 0},
		} {
			if got := GetTransferStats(ctx, networkId); got == nil || *got != want {
				t.Fatalf("transfer stats for %s = %+v, want %+v", networkId, got, want)
			}
		}

		// the provider payout stats (/stats/providers and the per-provider views)
		var sweepTime time.Time
		server.Db(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(ctx, `SELECT min(sweep_time) FROM transfer_escrow_sweep WHERE contract_id=$1`, f.contractId).Scan(&sweepTime))
		})
		day := dayKey(sweepTime)
		for networkId, want := range map[server.Id]map[server.Id]map[string]NanoCents{
			f.networkA:       {f.egressId: {day: f.egressRevenue}},
			f.networkB:       {f.hopBId: {day: f.hopBRevenue}},
			f.payerNetworkId: {},
		} {
			var got map[server.Id]map[string]NanoCents
			server.Db(ctx, func(conn server.PgConn) {
				var err error
				got, err = queryProviderPayoutStats(ctx, conn, networkId, windowStart, windowEnd)
				server.Raise(err)
			})
			if len(got) != len(want) {
				t.Fatalf("provider payout stats for %s = %v, want %v", networkId, got, want)
			}
			for clientId, days := range want {
				if !maps.Equal(got[clientId], days) {
					t.Fatalf("provider payout stats for %s = %v, want %v", networkId, got, want)
				}
			}
		}

		// the operator block points traffic, past an earning boundary before it
		traffic, err := readOperatorBlockTraffic(ctx, windowStart, windowEnd, windowStart)
		if err != nil {
			t.Fatal(err)
		}
		if traffic.sweepCount != 2 || len(traffic.payees) != 2 || len(traffic.pairs) != 1 {
			t.Fatalf("operator block traffic shape %d sweeps, %d payees, %d payers", traffic.sweepCount, len(traffic.payees), len(traffic.pairs))
		}
		for networkId, want := range map[server.Id]struct {
			bytes   ByteCount
			revenue NanoCents
		}{
			f.networkA: {f.egressBytes, f.egressRevenue},
			f.networkB: {f.hopBBytes, f.hopBRevenue},
		} {
			// both sweeps of the one settlement share its sweep time
			payee := traffic.payees[networkId]
			if payee == nil || payee.payoutByteCount != want.bytes || payee.netRevenue != want.revenue ||
				!payee.minSweepTime.Equal(sweepTime) || payee.minPaidSweepTime == nil || !payee.minPaidSweepTime.Equal(sweepTime) {
				t.Fatalf("operator block payee %s = %+v, want %+v at %s", networkId, payee, want, sweepTime)
			}
			// the balance carries net revenue, so it is paid (transfer_balance.paid)
			pair := traffic.pairs[f.payerNetworkId][networkId]
			if pair == nil || pair.paidByteCount != want.bytes || pair.unpaidByteCount != 0 {
				t.Fatalf("operator block pair %s -> %s = %+v, want %d paid", f.payerNetworkId, networkId, pair, want.bytes)
			}
		}

		// the points an award over one fixed block gives that traffic, by the
		// plan's rule (planOperatorBlockPoints): the minimum subsidy of the one
		// paying network split over A and B by bytes, plus their net revenue,
		// weighs the block's points. Reliability, seekers and referrals are off.
		config := operatorBlockPointsTestConfig()
		config.ReliabilityPointsPerPayout = 0
		config.ReliabilitySubsidyPerPayoutUsd = 0
		config.UsdPerActiveUser = 0
		config.SubscriptionNetRevenueFraction = 0
		awardStart := time.Date(2026, 11, 8, 0, 0, 0, 0, time.UTC)
		awardEnd := awardStart.Add(SubnetBlockDuration)
		award, err := planOperatorBlockPoints(&operatorBlockPointsInputs{
			subsidyConfig:    config,
			block:            1,
			cutoff:           awardStart,
			windowStart:      awardStart,
			windowEnd:        awardEnd,
			seekerHolders:    map[server.Id]bool{},
			networkReferrals: map[server.Id][]server.Id{},
		}, traffic, &operatorBlockReliability{networkWeights: map[server.Id]float64{}})
		if err != nil {
			t.Fatal(err)
		}
		scale := min(float64(awardEnd.Sub(awardStart))/float64(config.Duration()), 1.0)
		subsidyUsd := scale * config.MinPayoutUsd
		trafficBytes := float64(f.egressBytes + f.hopBBytes)
		payoutA := f.egressRevenue + UsdToNanoCents(float64(f.egressBytes)/trafficBytes*subsidyUsd)
		payoutB := f.hopBRevenue + UsdToNanoCents(float64(f.hopBBytes)/trafficBytes*subsidyUsd)
		totalPoints := float64(config.AccountPointsPerPayout) * scale
		wantPoints := map[server.Id]NanoPoints{
			f.networkA: PointsToNanoPoints(totalPoints * (float64(payoutA) / float64(payoutA+payoutB))),
			f.networkB: PointsToNanoPoints(totalPoints * (float64(payoutB) / float64(payoutA+payoutB))),
		}
		gotPoints := map[server.Id]NanoPoints{}
		for _, point := range award.Points {
			if point.Event != string(AccountPointEventPayout) {
				t.Fatalf("award has an unexpected %s point for %s", point.Event, point.NetworkId)
			}
			gotPoints[point.NetworkId] += point.PointValue
		}
		if award.TotalComputedPayout != payoutA+payoutB || !maps.Equal(gotPoints, wantPoints) {
			t.Fatalf("operator block award computed %d nanocents and points %v, want %d and %v", award.TotalComputedPayout, gotPoints, payoutA+payoutB, wantPoints)
		}

		// the SN pool's provider usage, which counts the payer's own hop too
		usages, err := GetStEpochProviderUsage(ctx, windowStart, windowEnd)
		if err != nil {
			t.Fatal(err)
		}
		slices.SortFunc(usages, func(a, b *StProviderUsage) int { return a.ClientId.Cmp(b.ClientId) })
		wantUsage := []StProviderUsage{
			{ClientId: f.egressId, NetworkId: f.networkA, PayoutByteCount: f.egressBytes},
			{ClientId: f.hopBId, NetworkId: f.networkB, PayoutByteCount: f.hopBBytes},
			{ClientId: f.hopPId, NetworkId: f.payerNetworkId, PayoutByteCount: f.hopPBytes},
		}
		if len(usages) != len(wantUsage) {
			t.Fatalf("SN provider usage = %d rows, want %d", len(usages), len(wantUsage))
		}
		for index, usage := range usages {
			if *usage != wantUsage[index] {
				t.Fatalf("SN provider usage %d = %+v, want %+v", index, *usage, wantUsage[index])
			}
		}
	})
}

// The removal itself: the settlement queues no provider totals row, credits no
// lifetime provider totals in PostgreSQL or Redis, and writes each sweep with
// provider_payouts naming its clients and no destination_id. Before 2026-11-11
// it queued one ApplyLegacyProviderTotals row per contract and stamped each
// sweep's destination_id.
func TestBillingSplitSettlementWritesNoProviderTotals(t *testing.T) {
	testEnv := server.DefaultTestEnv()
	testEnv.RerunCount = 0
	testEnv.Run(t, func(t testing.TB) {
		ctx := context.Background()
		f := newBillingSplitReadersFixture(t, ctx)
		var queued, finished int
		server.Db(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(ctx, `SELECT
				(SELECT count(*) FROM pending_task WHERE function_name=$1 OR run_once_key=$2),
				(SELECT count(*) FROM finished_task WHERE function_name=$1)`,
				task.NewTaskTarget(ApplyLegacyProviderTotals).TargetFunctionName(),
				task.RunOnce("legacy_provider_totals", f.contractId).String()).Scan(&queued, &finished))
		})
		if queued != 0 || finished != 0 {
			t.Fatalf("settlement produced provider totals rows: %d pending, %d finished", queued, finished)
		}
		for _, networkId := range []server.Id{f.payerNetworkId, f.networkA, f.networkB} {
			if got := contractPayoutTestAccountAmount(t, ctx, networkId); got != (contractPayoutTestAmount{}) {
				t.Fatalf("settlement credited lifetime provider totals for %s: %v", networkId, got)
			}
		}
		server.Db(ctx, func(conn server.PgConn) {
			rows, err := conn.Query(ctx, `SELECT network_id, destination_id, provider_payouts FROM transfer_escrow_sweep WHERE contract_id=$1`, f.contractId)
			server.WithPgResult(rows, err, func() {
				count := 0
				for rows.Next() {
					count++
					var networkId server.Id
					var destinationId *server.Id
					var value []byte
					server.Raise(rows.Scan(&networkId, &destinationId, &value))
					var providers []contractProviderPayout
					if err := json.Unmarshal(value, &providers); err != nil || len(providers) != 1 {
						t.Fatalf("sweep for %s lacks its provider allocation: %s, %v", networkId, value, err)
					}
					if destinationId != nil {
						t.Fatalf("sweep for %s wrote destination_id %s", networkId, *destinationId)
					}
				}
				if count != 2 {
					t.Fatalf("settlement wrote %d sweeps, want 2", count)
				}
			})
		})
	})
}
