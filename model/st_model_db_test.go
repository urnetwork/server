package model

// st_model_db_test.go — pg/redis-backed integration tests for the st
// settlement model (the serviceless pure tests live in st_model_test.go).
// Run with the local services from test.sh:
//
//	WARP_ENV=local WARP_SERVICE=test WARP_DOMAIN=bringyour.com WARP_BLOCK=test \
//	WARP_VERSION=0.0.0 BRINGYOUR_POSTGRES_HOSTNAME=local-pg.bringyour.com \
//	BRINGYOUR_REDIS_HOSTNAME=local-redis.bringyour.com go test ./model -run TestSt

import (
	"context"
	"encoding/hex"
	"fmt"
	"testing"
	"time"

	"github.com/urnetwork/server"
)

func TestStWalletRoundtrip(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		networkId := server.NewId()

		if GetStWallet(ctx, networkId) != nil {
			t.Fatal("unset wallet must be nil")
		}

		coldkeyA := testStCkey(1)
		SetStWallet(ctx, networkId, "5A...a", coldkeyA)
		wallet := GetStWallet(ctx, networkId)
		if wallet == nil || wallet.ColdkeyPubkey != coldkeyA || wallet.ColdkeySs58 != "5A...a" {
			t.Fatalf("wallet = %+v", wallet)
		}

		// upsert replaces (one wallet per network)
		coldkeyB := testStCkey(2)
		SetStWallet(ctx, networkId, "5B...b", coldkeyB)
		all := GetAllStWalletColdkeys(ctx)
		if len(all) != 1 || all[networkId] != coldkeyB {
			t.Fatalf("coldkeys = %v, want only the replacement", all)
		}
	})
}

func TestStEpochLifecycleMonotonic(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		UpsertStEpoch(ctx, &StEpoch{
			Epoch: 7, StartBlock: 100, CommitDeadlineBlock: 210,
			TrailsDeadlineBlock: 260, FinalizeBlock: 300, Status: StEpochStatusOpen,
		})
		SetStEpochStatus(ctx, 7, StEpochStatusCommitted)

		// a late window refresh must update the blocks but never regress status
		UpsertStEpoch(ctx, &StEpoch{
			Epoch: 7, StartBlock: 101, CommitDeadlineBlock: 211,
			TrailsDeadlineBlock: 261, FinalizeBlock: 301, Status: StEpochStatusOpen,
		})
		row := GetStEpoch(ctx, 7)
		if row.Status != StEpochStatusCommitted || row.StartBlock != 101 {
			t.Fatalf("after refresh: %+v (status must hold, blocks must update)", row)
		}

		// direct status regress is a no-op
		SetStEpochStatus(ctx, 7, StEpochStatusClosed)
		if row = GetStEpoch(ctx, 7); row.Status != StEpochStatusCommitted {
			t.Fatalf("status regressed to %s", row.Status)
		}

		// finalize records finalized_time exactly once
		SetStEpochStatus(ctx, 7, StEpochStatusFinalized)
		row = GetStEpoch(ctx, 7)
		if row.Status != StEpochStatusFinalized || row.FinalizedTime == nil {
			t.Fatalf("finalize: %+v", row)
		}
		finalizedTime := *row.FinalizedTime
		SetStEpochStatus(ctx, 7, StEpochStatusFinalized)
		if row = GetStEpoch(ctx, 7); !row.FinalizedTime.Equal(finalizedTime) {
			t.Fatal("finalized_time must be set once")
		}

		UpsertStEpoch(ctx, &StEpoch{Epoch: 9, Status: StEpochStatusOpen})
		if latest := GetLatestStEpoch(ctx); latest == nil || latest.Epoch != 9 {
			t.Fatalf("latest = %+v", latest)
		}
		if finalized := GetLatestFinalizedStEpoch(ctx); finalized == nil || finalized.Epoch != 7 {
			t.Fatalf("latest finalized = %+v", finalized)
		}
		open := GetStEpochsWithStatus(ctx, StEpochStatusOpen)
		if len(open) != 1 || open[0].Epoch != 9 {
			t.Fatalf("open epochs = %+v", open)
		}
	})
}

func TestStPayoutLeavesReplaceAndLookup(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		const epoch, noId = uint64(3), uint64(1)

		nA, nB := server.NewId(), server.NewId()
		SetStPayoutLeaves(ctx, epoch, noId, []*StPayoutLeaf{
			{Epoch: epoch, NoId: noId, NetworkId: nA, Coldkey: testStCkey(1), ShareBps: 4000, LeafIndex: 0},
			{Epoch: epoch, NoId: noId, NetworkId: nB, Coldkey: testStCkey(2), ShareBps: 6000, LeafIndex: 1},
		})

		// idempotent recompute before the on-chain commit: full replace
		SetStPayoutLeaves(ctx, epoch, noId, []*StPayoutLeaf{
			{Epoch: epoch, NoId: noId, NetworkId: nB, Coldkey: testStCkey(2), ShareBps: 10000, LeafIndex: 0},
		})

		leaves := GetStPayoutLeaves(ctx, epoch, noId)
		if len(leaves) != 1 || leaves[0].Coldkey != testStCkey(2) || leaves[0].ShareBps != 10000 {
			t.Fatalf("leaves = %+v, want the replacement set only", leaves)
		}

		if leaf := GetStPayoutLeafForColdkey(ctx, epoch, noId, testStCkey(2)); leaf == nil || leaf.ShareBps != 10000 {
			t.Fatalf("coldkey leaf = %+v", leaf)
		}
		if leaf := GetStPayoutLeafForColdkey(ctx, epoch, noId, testStCkey(1)); leaf != nil {
			t.Fatalf("replaced coldkey still has a leaf: %+v", leaf)
		}
	})
}

func TestStPublishLifecycle(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		publishId := AddStPublish(ctx, 4, StPublishKindCommit)
		txHash := "0xabc"
		UpdateStPublish(ctx, publishId, StPublishStatusConfirmed, &txHash, nil)
		AddStPublish(ctx, 4, StPublishKindDeposit)

		publishes := GetStPublishes(ctx, 4)
		if len(publishes) != 2 {
			t.Fatalf("publishes = %+v", publishes)
		}
		if publishes[0].Kind != StPublishKindCommit || publishes[0].Status != StPublishStatusConfirmed ||
			publishes[0].TxHash == nil || *publishes[0].TxHash != txHash {
			t.Fatalf("resolved publish = %+v", publishes[0])
		}
		if publishes[1].Status != StPublishStatusPending {
			t.Fatalf("second publish = %+v", publishes[1])
		}
	})
}

func TestStEventsDedupOrderAndHighWater(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		UpsertStEvents(ctx, []*StChainEvent{
			{BlockNumber: 5, LogIndex: 2, TxHash: "0x1", Kind: "HeadBound", DataJson: `{"a":1}`},
			{BlockNumber: 5, LogIndex: 0, TxHash: "0x1", Kind: "HeadUnbound", DataJson: `{}`},
			{BlockNumber: 3, LogIndex: 7, TxHash: "0x0", Kind: "OperatorCommitted", DataJson: `{}`},
		})
		// conservative re-scan: the duplicate (5, 2) must be ignored, first write wins
		UpsertStEvents(ctx, []*StChainEvent{
			{BlockNumber: 5, LogIndex: 2, TxHash: "0x1", Kind: "HeadBound", DataJson: `{"a":2}`},
		})

		events := GetStEvents(ctx, 0, 10)
		if len(events) != 3 {
			t.Fatalf("events = %+v", events)
		}
		// ordered by (block, log)
		if events[0].BlockNumber != 3 || events[1].LogIndex != 0 || events[2].LogIndex != 2 {
			t.Fatalf("order = %+v", events)
		}
		if events[2].DataJson != `{"a":1}` {
			t.Fatalf("dedup must keep the first write, got %s", events[2].DataJson)
		}

		SetStHighWaterBlock(ctx, 100)
		SetStHighWaterBlock(ctx, 50) // never moves backward
		if block := GetStHighWaterBlock(ctx); block != 100 {
			t.Fatalf("high water = %d, want 100", block)
		}
		SetStHighWaterBlock(ctx, 150)
		if block := GetStHighWaterBlock(ctx); block != 150 {
			t.Fatalf("high water = %d, want 150", block)
		}
	})
}

// testStHeadBindingRow reads the st_head_binding mirror row directly (the
// mirror is ops/debug only — production reads replay st_event instead).
func testStHeadBindingRow(ctx context.Context, ckey [32]byte) (active bool, updateBlock uint64, exists bool) {
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`SELECT active, update_block FROM st_head_binding WHERE ckey = $1`,
			ckey[:],
		)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				var block int64
				server.Raise(result.Scan(&active, &block))
				updateBlock = uint64(block)
				exists = true
			}
		})
	})
	return
}

func TestStHeadBindingUpsertGuard(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		ckey, hotkey := testStCkey(1), testStCkey(9)

		UpsertStHeadBinding(ctx, ckey, hotkey, 5, true, 10)
		if active, block, ok := testStHeadBindingRow(ctx, ckey); !ok || !active || block != 10 {
			t.Fatalf("bound row = %v %d %v", active, block, ok)
		}

		// an older event never regresses a newer state
		UpsertStHeadBinding(ctx, ckey, hotkey, 5, false, 5)
		if active, block, _ := testStHeadBindingRow(ctx, ckey); !active || block != 10 {
			t.Fatalf("older event applied: %v %d", active, block)
		}

		// same-block later log wins (the sync applies in (block, log) order)
		UpsertStHeadBinding(ctx, ckey, hotkey, 5, false, 10)
		if active, _, _ := testStHeadBindingRow(ctx, ckey); active {
			t.Fatal("same-block later event must win")
		}

		UpsertStHeadBinding(ctx, ckey, hotkey, 6, true, 20)
		if active, block, _ := testStHeadBindingRow(ctx, ckey); !active || block != 20 {
			t.Fatalf("newer event not applied: %v %d", active, block)
		}
	})
}

// testStHeadEventJson builds the data_json shape the event decoder writes for
// HeadBound/HeadUnbound (st_controller.go stEventDecoders).
func testStHeadEventJson(ckey [32]byte) string {
	hotkey := testStCkey(0xee)
	return fmt.Sprintf(
		`{"ckey":"0x%s","hotkey":"0x%s","uid":"7","registrant":"0x0"}`,
		hex.EncodeToString(ckey[:]),
		hex.EncodeToString(hotkey[:]),
	)
}

// TestGetHeadBoundCkeysInEpochFromEventLog is the SQL leg of the HF-1 fix
// (the replay core is unit-tested in st_model_test.go): kind filter,
// block <= close filter, and (block, log_index) ordering — including
// same-block sequences whose OUTCOME depends on log order.
func TestGetHeadBoundCkeysInEpochFromEventLog(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		const startBlock, closeBlock = uint64(500), uint64(1000)

		boundAll := testStCkey(1)       // bound pre-window, never unbound → excluded
		dodger := testStCkey(2)         // HF-1: unbind at close-1 → still excluded
		preWindow := testStCkey(3)      // bound+unbound before the window → not excluded
		bindThenUnbind := testStCkey(4) // same pre-window block: log0 Bound, log1 Unbound → NOT excluded iff log order applied
		unbindThenBind := testStCkey(5) // same pre-window block: log0 Unbound (no-op), log1 Bound → excluded iff log order applied
		postClose := testStCkey(6)      // bound only after closeBlock → filtered by the query
		malformed := testStCkey(7)      // malformed data_json row → skipped

		UpsertStEvents(ctx, []*StChainEvent{
			{BlockNumber: 100, LogIndex: 0, TxHash: "0x", Kind: "HeadBound", DataJson: testStHeadEventJson(boundAll)},
			{BlockNumber: 100, LogIndex: 1, TxHash: "0x", Kind: "HeadBound", DataJson: testStHeadEventJson(dodger)},
			{BlockNumber: closeBlock - 1, LogIndex: 0, TxHash: "0x", Kind: "HeadUnbound", DataJson: testStHeadEventJson(dodger)},
			{BlockNumber: 100, LogIndex: 2, TxHash: "0x", Kind: "HeadBound", DataJson: testStHeadEventJson(preWindow)},
			{BlockNumber: 400, LogIndex: 0, TxHash: "0x", Kind: "HeadUnbound", DataJson: testStHeadEventJson(preWindow)},
			{BlockNumber: 300, LogIndex: 0, TxHash: "0x", Kind: "HeadBound", DataJson: testStHeadEventJson(bindThenUnbind)},
			{BlockNumber: 300, LogIndex: 1, TxHash: "0x", Kind: "HeadUnbound", DataJson: testStHeadEventJson(bindThenUnbind)},
			{BlockNumber: 300, LogIndex: 2, TxHash: "0x", Kind: "HeadUnbound", DataJson: testStHeadEventJson(unbindThenBind)},
			{BlockNumber: 300, LogIndex: 3, TxHash: "0x", Kind: "HeadBound", DataJson: testStHeadEventJson(unbindThenBind)},
			{BlockNumber: closeBlock + 1, LogIndex: 0, TxHash: "0x", Kind: "HeadBound", DataJson: testStHeadEventJson(postClose)},
			{BlockNumber: 600, LogIndex: 0, TxHash: "0x", Kind: "HeadBound", DataJson: `{"ckey":"0x1234"}`},
			// a same-window non-head event kind must be ignored entirely
			{BlockNumber: 600, LogIndex: 1, TxHash: "0x", Kind: "OperatorCommitted", DataJson: testStHeadEventJson(malformed)},
		})

		got := GetHeadBoundCkeysInEpoch(ctx, startBlock, closeBlock)

		want := map[[32]byte]bool{boundAll: true, dodger: true, unbindThenBind: true}
		for ckey, in := range want {
			if got[ckey] != in {
				t.Fatalf("ckey %x: excluded=%v, want %v (full set %v)", ckey[0], got[ckey], in, got)
			}
		}
		if len(got) != len(want) {
			t.Fatalf("excluded set = %v, want exactly %d ckeys", got, len(want))
		}
	})
}

func TestStEpochSummaryCache(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		if GetStEpochSummaryCache(ctx) != nil {
			t.Fatal("fresh cache must miss")
		}
		summary := &StEpochSummary{
			Epoch: 12, StartBlock: 1200, CommitDeadlineBlock: 1310,
			TrailsDeadlineBlock: 1360, FinalizeBlock: 1400,
			TEpochBlocks: 100, ChainId: 964, ContractAddress: "0xdead",
		}
		SetStEpochSummaryCache(ctx, summary, time.Minute)
		got := GetStEpochSummaryCache(ctx)
		if got == nil || *got != *summary {
			t.Fatalf("cache roundtrip = %+v", got)
		}
	})
}

func TestGetStContributingClientCkeys(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		withKey, shortKey, noKey := server.NewId(), server.NewId(), server.NewId()
		ckey := testStCkey(5)
		SetClientPublicKey(ctx, withKey, ckey[:])
		SetClientPublicKey(ctx, shortKey, []byte("not-32-bytes"))

		got := GetStContributingClientCkeys(ctx, []server.Id{withKey, shortKey, noKey})
		if len(got) != 1 || got[withKey] != ckey {
			t.Fatalf("ckeys = %v, want only the 32-byte published key", got)
		}
		if len(GetStContributingClientCkeys(ctx, nil)) != 0 {
			t.Fatal("empty input must return empty")
		}
	})
}

func TestGetStEpochNetworkUsageWindow(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		start := time.Date(2026, 7, 1, 10, 0, 0, 0, time.UTC)
		end := start.Add(time.Hour)

		nA, nB := server.NewId(), server.NewId()
		insertSweep := func(networkId server.Id, byteCount int64, sweepTime time.Time) {
			server.Tx(ctx, func(tx server.PgTx) {
				server.RaisePgResult(tx.Exec(
					ctx,
					`
	                    INSERT INTO transfer_escrow_sweep (
	                        contract_id, balance_id, network_id,
	                        payout_byte_count, payout_net_revenue_nano_cents, sweep_time
	                    )
	                    VALUES ($1, $2, $3, $4, 0, $5)
	                `,
					server.NewId(), server.NewId(), networkId, byteCount, sweepTime,
				))
			})
		}
		insertSweep(nA, 600, start)                     // inclusive start
		insertSweep(nA, 400, start.Add(30*time.Minute)) // summed per network
		insertSweep(nB, 250, end.Add(-time.Second))
		insertSweep(nB, 999, end)                     // exclusive end
		insertSweep(nB, 999, start.Add(-time.Second)) // before the window

		got := map[server.Id]int64{}
		for _, usage := range GetStEpochNetworkUsage(ctx, start, end) {
			got[usage.NetworkId] = usage.PayoutByteCount
		}
		if len(got) != 2 || got[nA] != 1000 || got[nB] != 250 {
			t.Fatalf("usage = %v, want {A:1000, B:250}", got)
		}
	})
}

func TestGetStEpochClientReliabilityJoinAndWindow(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		networkId, userId := server.NewId(), server.NewId()
		Testing_CreateNetwork(ctx, networkId, fmt.Sprintf("st-rel-%s", networkId), userId)
		clientId := server.NewId()
		Testing_CreateDevice(ctx, networkId, server.NewId(), clientId, "d", "spec")
		orphanClientId := server.NewId() // no network_client row → dropped by the join

		p1 := time.Date(2026, 7, 1, 10, 0, 0, 0, time.UTC)
		p2 := p1.Add(15 * time.Minute)
		period := func(clientId server.Id, start time.Time, a int64, c int64) *VerifyProviderStatsRow {
			return &VerifyProviderStatsRow{
				PeriodStart: start, PeriodEnd: start.Add(15 * time.Minute),
				ClientId: clientId, Assignments: a, Confirmations: c,
			}
		}
		UpsertVerifyProviderStats(ctx, []*VerifyProviderStatsRow{
			period(clientId, p1, 10, 8),
			period(clientId, p2, 4, 4),
			period(orphanClientId, p1, 99, 99),
		})

		// window spanning both periods sums them
		rows := GetStEpochClientReliability(ctx, p1, p2.Add(15*time.Minute))
		if len(rows) != 1 {
			t.Fatalf("reliability rows = %+v, want the joined client only", rows)
		}
		if rows[0].ClientId != clientId || rows[0].NetworkId != networkId ||
			rows[0].Assignments != 14 || rows[0].Confirmations != 12 {
			t.Fatalf("summed row = %+v", rows[0])
		}

		// strict overlap: a period ending exactly at the window start is out
		if rows := GetStEpochClientReliability(ctx, p1.Add(15*time.Minute), p2.Add(15*time.Minute)); len(rows) != 1 || rows[0].Assignments != 4 {
			t.Fatalf("second-period-only rows = %+v", rows)
		}
	})
}
