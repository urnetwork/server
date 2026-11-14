package controller

// GET /account/epochs reports the bytes served next to each finalized epoch's
// points, by the same operator blocks, and a running figure for the open epoch
// (server#451; model/operator_block_traffic.go).

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
)

// An earning boundary at cutoff, as sn.yml states it. prepare records it, as
// Main's was recorded; without that the boundary is not in force.
func pushServedBytesEarningBoundary(t testing.TB, ctx context.Context, cutoff time.Time, prepare bool) {
	t.Helper()
	data := fmt.Sprintf("schema: urnetwork-provider-payout-transition-v1\ncutoff_utc: %q\nattribution: settled_contract_close_time\nlegacy_usdc: finish_pre_cutoff_obligations\nmainnet:\n  profile: mainnet\n  chain_id: 964\n  genesis_hash: %q\n  netuid: 25\n  activation: blocked\n", cutoff.UTC().Format(time.RFC3339), "0x"+strings.Repeat("11", 32))
	t.Cleanup(server.Config.PushSimpleResource("sn.yml", []byte(data)))
	if !prepare {
		return
	}
	policy, err := server.LoadProviderPayoutTransition(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := server.PrepareProviderPayoutBoundary(ctx, policy.ConfigSha256); err != nil {
		t.Fatal(err)
	}
}

// Clears the chain state and epoch windows earlier tests cached.
func resetSnEarningsCaches() {
	snChainStateCache.lock.Lock()
	snChainStateCache.state = nil
	snChainStateCache.expiry = time.Time{}
	snChainStateCache.lock.Unlock()
	snEpochWindowCache.lock.Lock()
	snEpochWindowCache.windows = map[snEpochWindowCacheKey][2]time.Time{}
	snEpochWindowCache.lock.Unlock()
}

// Finalized epoch 5 holds the end of operator block b-1, whose award has
// committed, and open epoch 6 holds the end of block b, which the rollup has
// read a day past the epoch's start. The open block is b+1, which the rollup
// has read up to the settle time and which ends after epoch 6. Epoch 5 lists
// the award's bytes beside the award's points and nothing of the network's
// USDC payment; the current epoch carries block b's bytes so far, as of where
// the rollup read it, within the open epoch's window, which starts where
// epoch 5 ends; the current block carries block b+1's bytes so far and counts
// in epoch 7. A network with no traffic lists zeros, and without an open
// epoch past the newest finalized one the result has neither running figure.
func TestAccountEpochsReportServedBytesAndTheRunningFigures(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		gib := model.ByteCount(1024 * 1024 * 1024)
		day := 24 * time.Hour

		block := int64(model.SubnetBlockNumber(server.NowUtc())) - 1
		blockStart := model.SubnetBlockGenesis.Add(time.Duration(block-1) * model.SubnetBlockDuration)
		blockEnd := blockStart.Add(model.SubnetBlockDuration)
		previousEnd := blockStart
		// epoch 6 starts two days before block b ends; epoch 5 is the week before
		epochBlocks := uint64(50_400)
		epochLength := time.Duration(epochBlocks) * 12 * time.Second
		start6 := blockEnd.Add(-2 * day)
		start5 := start6.Add(-epochLength)
		if !previousEnd.After(start5) || !previousEnd.Before(start6) || !blockEnd.After(start6) || !blockEnd.Before(start6.Add(epochLength)) {
			t.Fatalf("blocks %d and %d do not end in epochs 5 and 6", block-1, block)
		}

		cfg := ledgerV2TestConfig()
		cfg.DeploymentId = "account-epochs-served-bytes"
		cfg.ContractAddress = common.HexToAddress("0x20000000000000000000000000000000000000a1")
		cfg.SettlementV2, cfg.VaultV2 = common.Address{}, common.Address{}
		startBlock6 := uint64(3_000_000)
		head := startBlock6 + 1_000
		stub := newStubStClient(&StEpochState{
			Epoch:           6,
			PendingEpoch:    6,
			EpochStartBlock: startBlock6,
			TEpochBlocks:    epochBlocks,
			HeadBlock:       head,
			HeadBlockTime:   start6.Add(time.Duration(head-startBlock6) * 12 * time.Second),
		})
		stub.epochCloseBlocks[5] = startBlock6
		SetStConfig(cfg)
		SetStClient(stub)
		resetSnEarningsCaches()
		t.Cleanup(func() {
			SetStConfig(nil)
			SetStClient(nil)
			resetSnEarningsCaches()
		})
		key := cfg.DeploymentKey()
		model.UpsertStEpoch(ctx, key, &model.StEpoch{Epoch: 5, StartBlock: startBlock6 - epochBlocks, Status: model.StEpochStatusFinalized})
		model.UpsertStEpoch(ctx, key, &model.StEpoch{Epoch: 6, StartBlock: startBlock6, Status: model.StEpochStatusOpen})
		model.SetStEpochSummaryCache(ctx, key, &model.StEpochSummary{Epoch: 6, StartBlock: startBlock6, TEpochBlocks: epochBlocks}, time.Hour)

		networkA, networkB := server.NewId(), server.NewId()
		model.Testing_CreateNetwork(ctx, networkA, "epoch-bytes-a", server.NewId())
		model.Testing_CreateNetwork(ctx, networkB, "epoch-bytes-b", server.NewId())
		// block b-1's award: its payment row carries the bytes it shared the
		// points by, and its point row the network's points
		awardPlanId := model.OperatorBlockPointsPlanId(block - 1)
		awardPaymentId := server.NewId()
		readThrough := start6.Add(day)
		// the open block b+1, read up to the settle time
		openReadThrough := server.MaxTime(blockEnd, server.NowUtc().Add(-2*time.Hour)).Truncate(time.Second)
		mib := model.ByteCount(1024 * 1024)
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `
				INSERT INTO payment_report (payment_plan_id, total_payout_nano_cents, total_nano_points, point_scale_factor, payout_points_per_payout, time_scale_factor)
				VALUES ($1, 0, 5000000, 1, 1, 1)
			`, awardPlanId))
			server.RaisePgResult(tx.Exec(ctx, `
				INSERT INTO account_payment (payment_id, payment_plan_id, network_id, payout_byte_count, payout_nano_cents, min_sweep_time, create_time, canceled, cancel_time, block_rollup_complete)
				VALUES ($1, $2, $3, $4, 0, $5, $6, true, $6, true)
			`, awardPaymentId, awardPlanId, networkA, 3*gib, blockStart.Add(-day), blockStart.Add(12*time.Hour)))
			server.RaisePgResult(tx.Exec(ctx, `
				INSERT INTO account_point (account_point_id, network_id, event, point_value, payment_plan_id, account_payment_id, create_time)
				VALUES ($1, $2, $3, 5000000, $4, $5, $6)
			`, server.NewId(), networkA, model.AccountPointEventPayout, awardPlanId, awardPaymentId, blockStart.Add(12*time.Hour)))
			// a USDC payment of the network is no operator block's traffic
			server.RaisePgResult(tx.Exec(ctx, `
				INSERT INTO account_payment (payment_id, payment_plan_id, network_id, payout_byte_count, payout_nano_cents, min_sweep_time, create_time)
				VALUES ($1, $2, $3, $4, 0, $5, $5)
			`, server.NewId(), server.NewId(), networkA, 9*gib, start5.Add(day)))
			// block b has no award yet: the rollup's bytes so far
			server.RaisePgResult(tx.Exec(ctx, `
				INSERT INTO operator_block_traffic (block_number, read_through, update_time) VALUES ($1, $2, $2)
			`, block, readThrough))
			server.RaisePgResult(tx.Exec(ctx, `
				INSERT INTO operator_block_network_traffic (block_number, network_id, payout_byte_count) VALUES ($1, $2, $3)
			`, block, networkA, gib))
			server.RaisePgResult(tx.Exec(ctx, `
				INSERT INTO operator_block_traffic (block_number, read_through, update_time) VALUES ($1, $2, $2)
			`, block+1, openReadThrough))
			server.RaisePgResult(tx.Exec(ctx, `
				INSERT INTO operator_block_network_traffic (block_number, network_id, payout_byte_count) VALUES ($1, $2, $3)
			`, block+1, networkA, 512*mib))
		})

		read := func(networkId server.Id) (*AccountEpochsResult, map[string]any) {
			t.Helper()
			clientSession := session.Testing_CreateClientSession(ctx, &session.ByJwt{NetworkId: networkId})
			defer clientSession.Cancel()
			result, err := AccountEpochs(&AccountEpochsArgs{}, clientSession)
			if err != nil {
				t.Fatal(err)
			}
			encoded, err := json.Marshal(result)
			if err != nil {
				t.Fatal(err)
			}
			decoded := map[string]any{}
			if err := json.Unmarshal(encoded, &decoded); err != nil {
				t.Fatal(err)
			}
			return result, decoded
		}

		result, decoded := read(networkA)
		if len(result.Epochs) != 1 {
			t.Fatalf("epochs %+v", result.Epochs)
		}
		epoch := result.Epochs[0]
		if epoch.Epoch != 5 || epoch.StartMillis != start5.UnixMilli() || epoch.EndMillis != start6.UnixMilli() || epoch.Points != 5 || epoch.ServedByteCount != 3*gib {
			t.Fatalf("epoch 5 %+v, want [%s, %s) with 5 points and %d bytes", epoch, start5, start6, 3*gib)
		}
		want := &AccountCurrentEpoch{
			Epoch:           6,
			StartMillis:     start6.UnixMilli(),
			EndMillis:       start6.Add(epochLength).UnixMilli(),
			ServedByteCount: gib,
			AsOfMillis:      readThrough.UnixMilli(),
		}
		if result.CurrentEpoch == nil || *result.CurrentEpoch != *want {
			t.Fatalf("current epoch %+v, want %+v", result.CurrentEpoch, want)
		}
		epochs := decoded["epochs"].([]any)
		if served := epochs[0].(map[string]any)["served_byte_count"]; served != float64(3*gib) {
			t.Fatalf("epoch 5 encodes served_byte_count %v", served)
		}
		current, ok := decoded["current_epoch"].(map[string]any)
		if !ok {
			t.Fatalf("no current_epoch in %v", decoded)
		}
		if keys := slices.Sorted(maps.Keys(current)); !slices.Equal(keys, []string{"as_of_millis", "end_millis", "epoch", "served_byte_count", "start_millis"}) {
			t.Fatalf("current_epoch encodes %v", keys)
		}
		// block b+1 ends seven days after block b, past epoch 6's end
		wantBlock := &AccountCurrentBlock{
			BlockNumber:     block + 1,
			StartMillis:     blockEnd.UnixMilli(),
			EndMillis:       blockEnd.Add(model.SubnetBlockDuration).UnixMilli(),
			ServedByteCount: 512 * mib,
			AsOfMillis:      openReadThrough.UnixMilli(),
			Epoch:           7,
		}
		if result.CurrentBlock == nil || *result.CurrentBlock != *wantBlock {
			t.Fatalf("current block %+v, want %+v", result.CurrentBlock, wantBlock)
		}
		currentBlock, ok := decoded["current_block"].(map[string]any)
		if !ok {
			t.Fatalf("no current_block in %v", decoded)
		}
		if keys := slices.Sorted(maps.Keys(currentBlock)); !slices.Equal(keys, []string{"as_of_millis", "block_number", "end_millis", "epoch", "served_byte_count", "start_millis"}) {
			t.Fatalf("current_block encodes %v", keys)
		}

		// a network with no traffic
		result, _ = read(networkB)
		if len(result.Epochs) != 1 || result.Epochs[0].ServedByteCount != 0 || result.Epochs[0].Points != 0 {
			t.Fatalf("network B epochs %+v", result.Epochs)
		}
		if result.CurrentEpoch == nil || result.CurrentEpoch.ServedByteCount != 0 || result.CurrentEpoch.AsOfMillis != readThrough.UnixMilli() {
			t.Fatalf("network B current epoch %+v", result.CurrentEpoch)
		}
		if result.CurrentBlock == nil || result.CurrentBlock.ServedByteCount != 0 || result.CurrentBlock.AsOfMillis != openReadThrough.UnixMilli() || result.CurrentBlock.Epoch != 7 {
			t.Fatalf("network B current block %+v", result.CurrentBlock)
		}

		// no epoch is open past the newest finalized one
		model.SetStEpochSummaryCache(ctx, key, &model.StEpochSummary{Epoch: 5, StartBlock: startBlock6 - epochBlocks, TEpochBlocks: epochBlocks}, time.Hour)
		result, decoded = read(networkA)
		if _, ok := decoded["current_epoch"]; ok || result.CurrentEpoch != nil {
			t.Fatalf("a current epoch without an open one: %v", decoded)
		}
		if _, ok := decoded["current_block"]; ok || result.CurrentBlock != nil {
			t.Fatalf("a current block without an open epoch: %v", decoded)
		}
		if len(result.Epochs) != 1 || result.Epochs[0].ServedByteCount != 3*gib {
			t.Fatalf("epochs without an open one %+v", result.Epochs)
		}
	})
}

// The hourly points leaderboard rebuild keeps the bytes served of blocks
// without an award moving (model.AdvanceOperatorBlockTrafficRollup): a run
// reads the settled traffic of the completed block and reports the
// statements it read. A rollup that cannot run, under an earning boundary
// that differs from the recorded one, is logged, and the rebuild still
// succeeds.
func TestRebuildPointsLeaderboardAdvancesTheServedBytesRollup(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		gib := model.ByteCount(1024 * 1024 * 1024)
		now := server.NowUtc()
		block := int64(model.SubnetBlockNumber(now)) - 1
		blockStart := model.SubnetBlockGenesis.Add(time.Duration(block-1) * model.SubnetBlockDuration)
		blockEnd := blockStart.Add(model.SubnetBlockDuration)
		// the block before has no window past the cutoff, so the rollup's first
		// run starts at this block, wherever the wall clock is in the next one
		pushServedBytesEarningBoundary(t, ctx, blockStart, true)

		providerNetworkId, providerClientId := server.NewId(), server.NewId()
		model.Testing_CreateNetwork(ctx, providerNetworkId, "served-bytes-provider", server.NewId())
		contractId := server.ContractIdFloor(blockStart)
		contractId[15] = 1
		swept := blockStart.Add(time.Hour)
		usage := fmt.Sprintf(`{"version":1,"byte_count":%d,"providers":[{"client_id":%q,"network_id":%q,"byte_count":%d}]}`, gib, providerClientId, providerNetworkId, gib)
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO transfer_contract
				(contract_id,source_network_id,source_id,destination_network_id,destination_id,transfer_byte_count,create_time,close_time,outcome,provider_usage,usage_origin_is_source)
				VALUES($1,$2,$3,$4,$5,$6,$7,$8,'settled',$9::jsonb,true)`,
				contractId, server.NewId(), server.NewId(), providerNetworkId, providerClientId, gib, blockStart, swept, usage))
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO transfer_escrow_sweep
				(contract_id,balance_id,network_id,destination_id,payout_byte_count,payout_net_revenue_nano_cents,sweep_time)
				VALUES($1,$2,$3,$4,$5,0,$6)`,
				contractId, server.NewId(), providerNetworkId, providerClientId, gib, swept))
		})

		anonymous := session.NewLocalClientSession(ctx, "0.0.0.0:0", nil)
		defer anonymous.Cancel()
		result, err := RebuildPointsLeaderboard(&RebuildPointsLeaderboardArgs{}, anonymous)
		if err != nil {
			t.Fatal(err)
		}
		if result.TrafficChunks <= 0 {
			t.Fatalf("the rebuild read %d traffic statements", result.TrafficChunks)
		}
		window := []model.PointsEpochWindow{{Epoch: 1, Start: blockEnd.Add(-time.Hour), End: blockEnd.Add(time.Hour)}}
		if served := model.GetAccountServedBytesInWindows(ctx, providerNetworkId, window, now)[0]; served.ByteCount != gib {
			t.Fatalf("the provider served %d bytes in block %d, want %d", served.ByteCount, block, gib)
		}

		// a boundary other than the recorded one stops the rollup, not the rebuild
		pushServedBytesEarningBoundary(t, ctx, blockStart.Add(-24*time.Hour), false)
		result, err = RebuildPointsLeaderboard(&RebuildPointsLeaderboardArgs{}, anonymous)
		if err != nil {
			t.Fatalf("a rollup that cannot run failed the rebuild: %s", err)
		}
		if result.TrafficChunks != 0 {
			t.Fatalf("the rollup read %d statements under a boundary not in force", result.TrafficChunks)
		}
	})
}
