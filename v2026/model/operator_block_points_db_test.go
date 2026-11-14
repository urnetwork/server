package model

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/session"
)

// An earning boundary at `cutoff`, prepared as Main's was.
func useOperatorBlockPointsTransition(t testing.TB, cutoff time.Time) {
	t.Helper()
	data := fmt.Sprintf("schema: urnetwork-provider-payout-transition-v1\ncutoff_utc: %q\nattribution: settled_contract_close_time\nlegacy_usdc: finish_pre_cutoff_obligations\nmainnet:\n  profile: mainnet\n  chain_id: 964\n  genesis_hash: %q\n  netuid: 25\n  activation: blocked\n", cutoff.UTC().Format(time.RFC3339), "0x"+strings.Repeat("11", 32))
	t.Cleanup(server.Config.PushSimpleResource("sn.yml", []byte(data)))
	policy, err := server.LoadProviderPayoutTransition(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := server.PrepareProviderPayoutBoundary(context.Background(), policy.ConfigSha256); err != nil {
		t.Fatal(err)
	}
}

// One paying network and its balance, and the providers it uses.
type operatorBlockPointsPayer struct {
	network, client, balance server.Id
}

func newOperatorBlockPointsPayer(t testing.TB, ctx context.Context) *operatorBlockPointsPayer {
	t.Helper()
	payer := &operatorBlockPointsPayer{network: server.NewId(), client: server.NewId()}
	testingCreatePaymentClient(ctx, payer.network, payer.client)
	code, err := CreateBalanceCode(ctx, 1024*1024*1024*1024, 365*24*time.Hour, UsdToNanoCents(100), "operator-block-points-"+server.NewId().String(), "", "")
	if err != nil {
		t.Fatal(err)
	}
	testingRedeemPaymentBalanceCode(t, ctx, payer.network, code.Secret)
	balances := GetActiveTransferBalances(ctx, payer.network)
	if len(balances) != 1 {
		t.Fatal("missing fixture balance")
	}
	payer.balance = balances[0].BalanceId
	return payer
}

// A settled contract of the payer with the provider, opened an hour before it
// closed, and its sweep. Its id is minted when it opened, as connect.NewId
// mints every contract's id: a payout plan reads unpaid sweeps by contract id.
func (self *operatorBlockPointsPayer) contract(
	t testing.TB,
	ctx context.Context,
	providerNetwork server.Id,
	providerClient server.Id,
	closed time.Time,
	swept time.Time,
	bytes ByteCount,
	revenue NanoCents,
) server.Id {
	t.Helper()
	contractId := contractIdMintedAt(closed.Add(-time.Hour))
	settled := "settled"
	usage := &contractUsageSnapshot{Version: 1, ByteCount: bytes, Providers: []contractProviderUsage{{ClientId: providerClient, NetworkId: providerNetwork, ByteCount: bytes}}}
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(ctx, `INSERT INTO transfer_contract
		(contract_id,source_network_id,source_id,destination_network_id,destination_id,transfer_byte_count,create_time,close_time,outcome,provider_usage,usage_origin_is_source)
		VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,true)`, contractId, self.network, self.client, providerNetwork, providerClient, bytes, closed.Add(-time.Hour), closed, settled, usage))
		server.RaisePgResult(tx.Exec(ctx, `INSERT INTO transfer_escrow_sweep
		(contract_id,balance_id,network_id,destination_id,payout_byte_count,payout_net_revenue_nano_cents,sweep_time)
		VALUES($1,$2,$3,$4,$5,$6,$7)`, contractId, self.balance, providerNetwork, providerClient, bytes, revenue, swept))
	})
	return contractId
}

func operatorBlockPointsProvider(ctx context.Context) (network server.Id, client server.Id) {
	network, client = server.NewId(), server.NewId()
	testingCreatePaymentClient(ctx, network, client)
	return
}

// The points rows of a network, with their payment's state and blocks.
type operatorBlockPointsTestRow struct {
	event, paymentState string
	value               NanoPoints
	linked              *server.Id
	planId              server.Id
	blocks              []int64
}

func operatorBlockPointsTestRows(t testing.TB, ctx context.Context, networkId server.Id) (rows []*operatorBlockPointsTestRow) {
	t.Helper()
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(ctx, `
			SELECT point.event, point.point_value, point.linked_network_id, point.payment_plan_id,
				CASE WHEN payment.payment_id IS NULL THEN 'missing'
					ELSE format('canceled=%s completed=%s rollup=%s payout=%s subsidy=%s reliability=%s wallet=%s marker=%s',
						payment.canceled, payment.completed, payment.block_rollup_complete,
						payment.payout_nano_cents, payment.subsidy_payout_nano_cents, payment.reliability_subsidy_nano_cents,
						payment.wallet_id IS NOT NULL,
						EXISTS (SELECT 1 FROM audit_account_payment marker WHERE marker.payment_id = payment.payment_id AND marker.event_type = $2))
				END,
				ARRAY(SELECT block.block_number FROM account_payment_block block WHERE block.payment_id = point.account_payment_id ORDER BY block.block_number)
			FROM account_point point
			LEFT JOIN account_payment payment ON payment.payment_id = point.account_payment_id
			WHERE point.network_id = $1
			ORDER BY point.event, point.point_value
		`, networkId, OperatorBlockPointsEventType)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				row := &operatorBlockPointsTestRow{}
				server.Raise(result.Scan(&row.event, &row.value, &row.linked, &row.planId, &row.paymentState, &row.blocks))
				rows = append(rows, row)
			}
		})
	})
	return
}

func operatorBlockPointsRowCounts(t testing.TB, ctx context.Context) (counts [6]int64) {
	t.Helper()
	server.Db(ctx, func(conn server.PgConn) {
		server.Raise(conn.QueryRow(ctx, `SELECT
			(SELECT COUNT(*) FROM account_point),
			(SELECT COUNT(*) FROM account_payment),
			(SELECT COUNT(*) FROM account_payment_block),
			(SELECT COUNT(*) FROM payment_report),
			(SELECT COUNT(*) FROM audit_account_payment),
			(SELECT COUNT(*) FROM transfer_escrow_sweep WHERE payment_id IS NOT NULL)
		`).Scan(&counts[0], &counts[1], &counts[2], &counts[3], &counts[4], &counts[5]))
	})
	return
}

func operatorBlockPointsUsage(t testing.TB, ctx context.Context, start time.Time, end time.Time) string {
	t.Helper()
	usages, err := GetStEpochProviderUsage(ctx, start, end)
	if err != nil {
		t.Fatal(err)
	}
	slices.SortFunc(usages, func(a *StProviderUsage, b *StProviderUsage) int { return a.ClientId.Cmp(b.ClientId) })
	data, err := json.Marshal(usages)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// The root cause: past the earning boundary a payout plan applied no points,
// so a completed operator block's traffic earned none. The block's award
// gives them, the payout points by bytes served, stores the payments'
// operator block so the leaderboard's Blocks count it, and changes neither the
// SN pool's usage, the sweeps nor the provider's stats; a legacy obligation
// before the boundary is no traffic of the block. A second run awards nothing
// more.
func TestOperatorBlockPointsAwardPostCutoffBlock(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		// a completed block whose reliability evidence is retained, after a
		// boundary inside the block before it, or late in the open block inside
		// the block itself (operatorBlockPointsTestBlock)
		block, now, cutoff := operatorBlockPointsTestBlock(server.NowUtc())
		blockStart := operatorBlockStart(block)
		blockEnd := blockStart.Add(SubnetBlockDuration)
		windowStart, windowEnd, _ := OperatorBlockPointsWindow(block, cutoff)
		useOperatorBlockPointsTransition(t, cutoff)

		config := operatorBlockPointsTestConfig()
		config.ReliabilityPointsPerPayout = 0
		gib := ByteCount(1024 * 1024 * 1024)

		payer := newOperatorBlockPointsPayer(t, ctx)
		providerA, clientA := operatorBlockPointsProvider(ctx)
		providerB, clientB := operatorBlockPointsProvider(ctx)
		parentD, childE := server.NewId(), server.NewId()
		Testing_CreateNetwork(ctx, parentD, "operator-block-parent", server.NewId())
		Testing_CreateNetwork(ctx, childE, "operator-block-child", server.NewId())
		CreateNetworkReferral(ctx, providerA, CreateNetworkReferralCode(ctx, parentD).ReferralCode)
		CreateNetworkReferral(ctx, childE, CreateNetworkReferralCode(ctx, providerA).ReferralCode)
		sessionA := session.Testing_CreateClientSession(ctx, &session.ByJwt{NetworkId: providerA, ClientId: &clientA})
		defer sessionA.Cancel()
		if err := MarkWalletSeekerHolder("0x00000000000000000000000000000000000000a1", sessionA); err != nil {
			t.Fatal(err)
		}

		// a legacy obligation: closed and swept before the boundary
		legacyClose := cutoff.Add(-time.Hour)
		payer.contract(t, ctx, providerB, clientB, legacyClose, legacyClose, 5*1024*1024, UsdToNanoCents(1))
		// the block's traffic: three times as much through A as through B,
		// swept on both sides of a settlement that closed in the block
		inBlock := operatorBlockPointsTestTrafficTime(block, cutoff)
		postCutoff := []server.Id{
			payer.contract(t, ctx, providerA, clientA, inBlock, inBlock, 2*gib, 0),
			payer.contract(t, ctx, providerA, clientA, inBlock.Add(time.Hour), inBlock.Add(time.Hour), gib, 0),
			payer.contract(t, ctx, providerB, clientB, inBlock.Add(2*time.Hour), inBlock.Add(2*time.Hour), gib, 0),
		}
		// traffic of the next block is not this block's
		payer.contract(t, ctx, providerB, clientB, blockEnd.Add(time.Hour), blockEnd.Add(time.Hour), 7*gib, 0)

		usageBefore := operatorBlockPointsUsage(t, ctx, blockStart, blockEnd)
		transferStatsBefore := *GetTransferStats(ctx, providerA)

		awards, err := AwardDueOperatorBlockPoints(ctx, config, now, block)
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

		// the block's payout points, over its window past the boundary (the
		// whole block unless the boundary is inside it), shared 3:1 by bytes
		scale := min(float64(windowEnd.Sub(windowStart))/float64(config.Duration()), 1.0)
		payoutNanoPoints := PointsToNanoPoints(float64(config.AccountPointsPerPayout) * scale)
		shares := operatorBlockPayoutPointShares(payoutNanoPoints, map[server.Id]ByteCount{providerA: 3 * gib, providerB: gib})
		pointsA, pointsB := shares[providerA], shares[providerB]
		operatorBlockPointsRequireByteShares(t, map[server.Id]NanoPoints{providerA: pointsA, providerB: pointsB}, map[server.Id]ByteCount{providerA: 3 * gib, providerB: gib}, payoutNanoPoints)
		multipliedA := NanoPoints(float64(pointsA) * config.SeekerHolderMultiplier)
		if award.PayoutNanoPoints != payoutNanoPoints || award.ByteCount != 4*gib || len(award.Payments) != 2 || award.SweepCount != 3 {
			t.Fatalf("award %+v", award)
		}

		// format renders booleans as t and f
		marker := "canceled=t completed=f rollup=t payout=0 subsidy=0 reliability=0 wallet=f marker=t"
		planId := OperatorBlockPointsPlanId(block)
		check := func(networkId server.Id, want map[string]NanoPoints) {
			t.Helper()
			rows := operatorBlockPointsTestRows(t, ctx, networkId)
			got := map[string]NanoPoints{}
			for _, row := range rows {
				if row.planId != planId {
					continue
				}
				if row.paymentState != marker || !slices.Equal(row.blocks, []int64{block}) {
					t.Fatalf("network %s point %s: payment %q blocks %v", networkId, row.event, row.paymentState, row.blocks)
				}
				got[row.event] += row.value
			}
			if !maps.Equal(got, want) {
				t.Fatalf("network %s points %v, want %v", networkId, got, want)
			}
		}
		check(providerA, map[string]NanoPoints{
			string(AccountPointEventPayout):           pointsA,
			string(AccountPointEventPayoutMultiplier): multipliedA - pointsA,
		})
		check(providerB, map[string]NanoPoints{
			string(AccountPointEventPayout): pointsB,
		})
		check(parentD, map[string]NanoPoints{
			string(AccountPointEventPayoutLinkedAccount): NanoPoints(float64(multipliedA) * config.ReferralParentPayoutFraction),
		})
		check(childE, map[string]NanoPoints{
			string(AccountPointEventPayoutLinkedAccount): NanoPoints(float64(multipliedA) * config.ReferralChildPayoutFraction),
		})

		// the leaderboard counts the block for every network with its points
		inputs, err := loadPointsNetworkInputs(ctx, []PointsEpochWindow{{Epoch: uint64(block), Start: blockStart, End: blockEnd}})
		if err != nil {
			t.Fatal(err)
		}
		for _, input := range inputs {
			if slices.Contains([]server.Id{providerA, providerB, parentD, childE}, input.NetworkId) && !input.EpochsWithPoints[uint64(block)] {
				t.Fatalf("network %s has no points in block %d on the leaderboard", input.NetworkId, block)
			}
		}

		// the SN pool's usage, the sweeps and the provider's stats are unchanged
		if usageAfter := operatorBlockPointsUsage(t, ctx, blockStart, blockEnd); usageAfter != usageBefore {
			t.Fatalf("SN usage changed: %s -> %s", usageBefore, usageAfter)
		}
		server.Db(ctx, func(conn server.PgConn) {
			var assigned int
			server.Raise(conn.QueryRow(ctx, `SELECT COUNT(*) FROM transfer_escrow_sweep WHERE contract_id = ANY($1::uuid[]) AND payment_id IS NOT NULL`, postCutoff).Scan(&assigned))
			if assigned != 0 {
				t.Fatalf("%d post-cutoff sweeps were assigned to a payment", assigned)
			}
		})
		if transferStatsAfter := *GetTransferStats(ctx, providerA); transferStatsAfter != transferStatsBefore {
			t.Fatalf("transfer stats changed: %+v -> %+v", transferStatsBefore, transferStatsAfter)
		}
		payments, err := GetNetworkPayments(sessionA)
		if err != nil {
			t.Fatal(err)
		}
		for _, payment := range payments {
			if payment.PaymentPlanId == planId {
				t.Fatal("the points payment is listed as a payout")
			}
		}

		// idempotent: nothing more on a second run or commit
		before := operatorBlockPointsRowCounts(t, ctx)
		again, err := AwardDueOperatorBlockPoints(ctx, config, now, block)
		if err != nil || len(again) != 0 {
			t.Fatal("second run awarded", again, err)
		}
		committed, err := CommitOperatorBlockPoints(ctx, award)
		if err != nil || committed {
			t.Fatal("second commit wrote", committed, err)
		}
		if after := operatorBlockPointsRowCounts(t, ctx); after != before {
			t.Fatalf("second run wrote rows: %v -> %v", before, after)
		}
	})
}

// The award's reliability is the payout plan's: the award's read-only fold of
// the window equals the fold the plan ran into network_connection_reliability_score,
// computed here by the plan's statement, multiplied by the country multipliers
// as the plan read them, so reliability points are the plan's for the window.
func TestOperatorBlockPointsReliabilityMatchesThePlanFold(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		now := server.NowUtc()
		networkA, networkB, countries := seedReliabilityWindowTestClients(t, ctx, now)
		// a country multiplier, which the plan reads with the scores
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO network_client_location_reliability_multiplier (country_location_id, reliability_multiplier) VALUES ($1, 1.5)`, countries["de"]))
		})

		config := EnvSubsidyConfig()
		previous := *config
		defer func() { *config = previous }()
		config.ReliabilityPointsPerPayout = 500_000
		scale := 0.25

		// the plan's window [start, end] in reliability blocks is the award's
		// half-open [start, end+1us)
		start := now.Add(-4 * ReliabilityBlockDuration)
		end := now
		UpdateClientLocationReliabilities(ctx, start, end)
		planWeights := map[server.Id]float64{}
		for _, weight := range planFoldTestWeights(t, ctx, start, end.Add(time.Microsecond)) {
			multiplier := 1.0
			if weight.key.hasCountry && weight.key.countryLocationId == countries["de"] {
				multiplier = 1.5
			}
			planWeights[weight.key.networkId] += float64(weight.weight * multiplier)
		}
		planPoints := operatorBlockReliabilityPoints(planWeights, scale, config)

		reliability, err := readOperatorBlockReliability(ctx, start, end.Add(time.Microsecond), now)
		if err != nil {
			t.Fatal(err)
		}
		if len(reliability.networkWeights) != 2 || reliability.effectiveBlockCount <= 0 {
			t.Fatalf("reliability %+v", reliability)
		}
		awardPoints := operatorBlockReliabilityPoints(reliability.networkWeights, scale, config)
		if !maps.Equal(planPoints, awardPoints) || len(awardPoints) != 2 || awardPoints[networkA] <= 0 || awardPoints[networkB] <= 0 {
			t.Fatalf("plan %v, award %v", planPoints, awardPoints)
		}
	})
}
