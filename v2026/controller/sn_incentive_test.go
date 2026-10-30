package controller

// The on-chain incentive reader (LEADERTOP.md §6.2, design question 10):
// stale and fallback detection on recorded-runtime censuses, the once-per-tempo
// cadence, and the join of the census's UID holders to the live binding
// census. No live chain; identities are synthetic.

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"slices"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
	"github.com/urnetwork/server/v2026/stmetagraph"
	"github.com/urnetwork/server/v2026/stmetagraph/stmetagraphtest"
)

// A census of eight UIDs at block 7,000,000: UIDs 1 and 5 hold permits, UID
// 1 applied a head row and set weights 100 blocks ago with full trust.
func snIncentiveTestCensus() *stmetagraph.Census {
	census := &stmetagraph.Census{
		Checkpoint: stmetagraph.Checkpoint{Number: 7_000_000, Hash: common.Hash{0x0c}, SpecName: "node-subtensor", SpecVersion: 486},
		Netuid:     25, SubnetworkN: 8, SubnetEpochIndex: 51234, Tempo: 360, ActivityCutoff: 5000,
		Incentive:       []uint16{0, 0, 30000, 0, 0, 0, 0, 35535},
		LastUpdate:      []uint64{0, 6_999_900, 0, 0, 0, 6_990_000, 0, 0},
		ValidatorPermit: []bool{false, true, false, false, false, true, false, false},
		ValidatorTrust:  []uint16{0, 65535, 0, 0, 0, 1000, 0, 0},
		Identities: map[uint16]stmetagraph.Identity{
			1: {Hotkey: stmetagraphtest.Hotkey(1), BlockAtRegistration: 1001},
			3: {Hotkey: stmetagraphtest.Hotkey(3), BlockAtRegistration: 1003},
			5: {Hotkey: stmetagraphtest.Hotkey(5), BlockAtRegistration: 1005},
		},
		ValidatorRows: []stmetagraph.ValidatorRow{
			{Uid: 1, Row: []stmetagraph.WeightEntry{{Uid: 2, Value: 65535}, {Uid: 7, Value: 30000}}},
			{Uid: 5, Row: []stmetagraph.WeightEntry{{Uid: 169, Value: 14563}, {Uid: 170, Value: 65535}, {Uid: 250, Value: 65535}}},
		},
		ValidatorRowsComplete: true, ResponseBytes: 12_345,
	}
	return census
}

// A census is stale for exactly the reasons the board must not trust its
// incentive: the reference validator's row is the stopgap fallback row, its
// LastUpdate is past the activity cutoff, its ValidatorTrust is well below 1,
// or no reference validator holds a permit. The reference is UID 1 unless
// st.yml lists validator hotkeys.
func TestSnIncentiveSnapshotStaleness(t *testing.T) {
	readAt := time.Date(2026, 10, 29, 18, 0, 0, 0, time.UTC)
	fallback := []stmetagraph.WeightEntry{{Uid: 169, Value: 14563}, {Uid: 170, Value: 65535}, {Uid: 250, Value: 65535}}
	for _, test := range []struct {
		name      string
		change    func(*stmetagraph.Census)
		hotkeys   [][32]byte
		reference *uint16
		reasons   []string
	}{
		{name: "healthy", change: func(*stmetagraph.Census) {}, reference: new(uint16(1)), reasons: []string{}},
		{name: "fallback row", change: func(census *stmetagraph.Census) { census.ValidatorRows[0].Row = fallback }, reference: new(uint16(1)), reasons: []string{model.SnIncentiveStaleFallbackRow}},
		{name: "a row like the fallback is not it", change: func(census *stmetagraph.Census) { census.ValidatorRows[0].Row = fallback[:2] }, reference: new(uint16(1)), reasons: []string{}},
		{name: "inactive", change: func(census *stmetagraph.Census) { census.LastUpdate[1] = 7_000_000 - 5_001 }, reference: new(uint16(1)), reasons: []string{model.SnIncentiveStaleReferenceInactive}},
		{name: "at the cutoff is active", change: func(census *stmetagraph.Census) { census.LastUpdate[1] = 7_000_000 - 5_000 }, reference: new(uint16(1)), reasons: []string{}},
		{name: "low trust", change: func(census *stmetagraph.Census) { census.ValidatorTrust[1] = 50_000 }, reference: new(uint16(1)), reasons: []string{model.SnIncentiveStaleLowValidatorTrust}},
		{name: "every reason at once", change: func(census *stmetagraph.Census) {
			census.ValidatorRows[0].Row, census.LastUpdate[1], census.ValidatorTrust[1] = fallback, 1, 0
		}, reference: new(uint16(1)), reasons: []string{model.SnIncentiveStaleFallbackRow, model.SnIncentiveStaleReferenceInactive, model.SnIncentiveStaleLowValidatorTrust}},
		{name: "no permit on UID 1", change: func(census *stmetagraph.Census) { census.ValidatorPermit[1] = false }, reasons: []string{model.SnIncentiveStaleNoReference}},
		// a configured hotkey makes its own permitted UID the reference: UID 5
		// applied the fallback row 10,000 blocks ago with low trust
		{name: "configured reference", change: func(*stmetagraph.Census) {}, hotkeys: [][32]byte{stmetagraphtest.Hotkey(5)}, reference: new(uint16(5)),
			reasons: []string{model.SnIncentiveStaleFallbackRow, model.SnIncentiveStaleReferenceInactive, model.SnIncentiveStaleLowValidatorTrust}},
		{name: "configured hotkey without a permit", change: func(*stmetagraph.Census) {}, hotkeys: [][32]byte{stmetagraphtest.Hotkey(3)}, reasons: []string{model.SnIncentiveStaleNoReference}},
	} {
		t.Run(test.name, func(t *testing.T) {
			census := snIncentiveTestCensus()
			test.change(census)
			snapshot := snIncentiveSnapshot(census, test.hotkeys, readAt)
			if !slices.Equal(snapshot.StaleReasons, test.reasons) || snapshot.Stale != (len(test.reasons) != 0) {
				t.Fatalf("stale %t %v, want %v", snapshot.Stale, snapshot.StaleReasons, test.reasons)
			}
			if (snapshot.ReferenceUid == nil) != (test.reference == nil) || snapshot.ReferenceUid != nil && *snapshot.ReferenceUid != *test.reference {
				t.Fatalf("reference %v, want %v", snapshot.ReferenceUid, test.reference)
			}
			if snapshot.FallbackRow != slices.Contains(test.reasons, model.SnIncentiveStaleFallbackRow) {
				t.Fatal("fallback flag", snapshot.FallbackRow)
			}
		})
	}
	// per-UID incentive with its holder and freshness
	snapshot := snIncentiveSnapshot(snIncentiveTestCensus(), nil, readAt)
	if snapshot.SubnetEpochIndex != 51234 || snapshot.Block != 7_000_000 || !snapshot.ReadAt.Equal(readAt) || len(snapshot.Uids) != 8 || snapshot.ResponseBytes != 12_345 {
		t.Fatal("snapshot", snapshot)
	}
	if row, ok := snapshot.IncentiveFor(7, stmetagraphtest.Hotkey(7)); ok || row != nil {
		t.Fatal("an unread holder's incentive was attributed")
	}
	if row, ok := snapshot.IncentiveFor(3, stmetagraphtest.Hotkey(3)); !ok || row.Incentive != 0 || row.BlockAtRegistration != 1003 {
		t.Fatal("UID 3", row, ok)
	}
	// the stored PerU16 over 65535 is the UID's share: UIDs 2 and 7 split it
	if row := snapshot.Uids[2]; row.Incentive != 30000 || row.IncentiveFraction() != 30000.0/65535 {
		t.Fatal("UID 2's share", row.IncentiveFraction())
	}
	if total := snapshot.Uids[2].IncentiveFraction() + snapshot.Uids[7].IncentiveFraction(); math.Abs(total-1) > 1e-12 {
		t.Fatal("the shares of a normalized vector sum to 1", total)
	}
	if _, ok := snapshot.IncentiveFor(3, stmetagraphtest.Hotkey(4)); ok {
		t.Fatal("another hotkey's UID was attributed")
	}
	if holders := snapshot.Holders(); len(holders.Holders) != 3 || holders.Holders[5].BlockAtRegistration != 1005 || holders.Block != 7_000_000 {
		t.Fatal("holders", holders)
	}
}

// Runs a native read against one synthetic chain.
type snIncentiveTestSource struct {
	chain *stmetagraphtest.Chain
}

func (self *snIncentiveTestSource) nativeRead(ctx context.Context, op func(context.Context, stmetagraph.Rpc) error) error {
	return op(ctx, self.chain)
}

// The reader reads one census per tempo: it makes no RPC until a tempo has
// nearly passed, then checks only the head and SubnetEpochIndex until the
// index advances, stores the census once, and waits after a failure. The
// census reads the holder of every bound fleet's UID, so a binding whose
// hotkey holds its UID is live with its registration block.
func TestSnReadIncentiveReadsOncePerTempo(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		cfg := &StConfig{ChainId: 945, ContractAddress: common.HexToAddress("0x2000000000000000000000000000000000000026"), Netuid: 25, GenesisHash: [32]byte(stmetagraphtest.Genesis), BlockSeconds: 12}
		deploymentKey := cfg.DeploymentKey()
		chain := stmetagraphtest.NewChain(t, stmetagraphtest.MetadataPath, 7_000_000)
		incentive := []uint16{0, 0, 30000, 0, 0, 0, 0, 35535}
		lastUpdate := []uint64{0, 6_999_900, 0, 0, 0, 6_990_000, 0, 0}
		permits := []bool{false, true, false, false, false, true, false, false}
		trust := []uint16{0, 65535, 0, 0, 0, 1000, 0, 0}
		rows := map[uint16][]stmetagraphtest.Weight{1: {{Uid: 3, Value: 65535}}}
		chain.Subnet(25, 100, incentive, lastUpdate, permits, trust, rows)

		// a fleet bound to UID 3 under its holder's hotkey, valid 40..50
		model.SetStEpochSummaryCache(ctx, deploymentKey, &model.StEpochSummary{Epoch: 42}, time.Hour)
		boundHotkey := stmetagraphtest.Hotkey(3)
		data, _ := json.Marshal(model.StFleetBoundEventData{
			ClientId: "0x51000000000000000000000000000000", FleetId: "0x" + common.Bytes2Hex(common.LeftPadBytes([]byte{0x61}, 32)),
			Hotkey: "0x" + common.Bytes2Hex(boundHotkey[:]), Uid: "3", Generation: "1", ValidFromEpoch: "40", ValidToEpoch: "50",
		})
		bound := &model.StChainEvent{BlockNumber: 500, BlockHash: common.Hash{0x05}.Hex(), LogIndex: 0, TxHash: common.Hash{0x0f}.Hex(), Kind: model.StFleetBoundEventKind, DataJson: string(data)}
		if err := model.ApplySnFleetBindingRange(ctx, deploymentKey, []*model.StChainEvent{bound}, model.SnFleetBindingCheckpoint{NextBlock: 501, BlockHash: common.Hash{0x05}.Hex()}); err != nil {
			t.Fatal(err)
		}

		state := &snIncentiveReaderState{reader: stmetagraph.NewReader()}
		source := &snIncentiveTestSource{chain: chain}
		start := time.Date(2026, 10, 29, 18, 0, 0, 0, time.UTC)
		result, err := snReadIncentive(ctx, cfg, source, state, start)
		if err != nil || result.Result != SnIncentiveReadStored || result.SubnetEpochIndex != 100 {
			t.Fatal("first read", result, err)
		}
		stored := model.GetLatestSnIncentiveSnapshot(ctx, deploymentKey)
		if stored == nil || stored.SubnetEpochIndex != 100 || stored.Block != 7_000_000 || !stored.ReadAt.Equal(start) || len(stored.Uids) != 8 || stored.Stale ||
			stored.ReferenceUid == nil || *stored.ReferenceUid != 1 || stored.ResponseBytes <= 0 || snIncentiveCensusBudget < stored.ResponseBytes {
			t.Fatal("stored census", stored)
		}
		if row, ok := stored.IncentiveFor(7, stmetagraphtest.Hotkey(7)); ok || row != nil {
			t.Fatal("UID 7 is neither permitted nor bound, so its holder was not read")
		}
		if row, ok := stored.IncentiveFor(3, stmetagraphtest.Hotkey(3)); !ok || row.BlockAtRegistration != 1003 {
			t.Fatal("the bound UID's holder was not read", row, ok)
		}
		if len(stored.ValidatorRows) != 2 || !slices.Equal(stored.ValidatorRows[0].Row, []model.SnWeightEntry{{Uid: 3, Value: 65535}}) {
			t.Fatal("validator rows", stored.ValidatorRows)
		}
		claimedFleets := snClaimedFleets(ctx, deploymentKey, 42)
		if live := claimedFleets.LiveFleets(); len(live) != 1 || live[0].Key.Uid != 3 || live[0].UidRegisteredBlock != 1003 || claimedFleets.UidSubnetEpochIndex != 100 {
			t.Fatal("the bound fleet is not live at the census", claimedFleets)
		}

		// within the tempo: no RPC at all
		calls := chain.TotalCalls()
		result, err = snReadIncentive(ctx, cfg, source, state, start.Add(30*time.Minute))
		if err != nil || result.Result != SnIncentiveReadSkipped || chain.TotalCalls() != calls {
			t.Fatal("a read inside the tempo reached the chain", result, err)
		}
		// near the tempo's end, the index has not advanced: one cheap check
		storageReads := chain.Calls("state_getStorage")
		result, err = snReadIncentive(ctx, cfg, source, state, start.Add(66*time.Minute))
		if err != nil || result.Result != SnIncentiveReadUnchanged || chain.Calls("state_getStorage") != storageReads+1 {
			t.Fatal("the index check read more than the index", result, err, chain.Calls("state_getStorage")-storageReads)
		}
		// the index advances: the next census is stored
		chain.SetHead(7_000_360)
		chain.Subnet(25, 101, incentive, lastUpdate, permits, trust, rows)
		result, err = snReadIncentive(ctx, cfg, source, state, start.Add(73*time.Minute))
		if err != nil || result.Result != SnIncentiveReadStored || result.SubnetEpochIndex != 101 {
			t.Fatal("the next tempo", result, err)
		}
		if index, _, tempo, ok := model.GetLatestSnIncentiveRead(ctx, deploymentKey); !ok || index != 101 || tempo != 360 {
			t.Fatal("latest read", index, tempo, ok)
		}
		// a failed read waits ten minutes before reaching the chain again
		chain.Fail(errors.New("archive unavailable"))
		if _, err := snReadIncentive(ctx, cfg, source, state, start.Add(150*time.Minute)); err == nil {
			t.Fatal("a failed read succeeded")
		}
		chain.Fail(nil)
		calls = chain.TotalCalls()
		result, err = snReadIncentive(ctx, cfg, source, state, start.Add(155*time.Minute))
		if err != nil || result.Result != SnIncentiveReadSkipped || chain.TotalCalls() != calls {
			t.Fatal("a read retried inside its delay", result, err)
		}
		result, err = snReadIncentive(ctx, cfg, source, state, start.Add(161*time.Minute))
		if err != nil || result.Result != SnIncentiveReadUnchanged {
			t.Fatal("a read after the delay", result, err)
		}
		// snapshots past the retention are removed with the next stored census
		chain.SetHead(7_000_720)
		chain.Subnet(25, 102, incentive, lastUpdate, permits, trust, rows)
		if result, err := snReadIncentive(ctx, cfg, source, state, start.Add(8*24*time.Hour)); err != nil || result.Result != SnIncentiveReadStored {
			t.Fatal(result, err)
		}
		if index, _, _, _ := model.GetLatestSnIncentiveRead(ctx, deploymentKey); index != 102 {
			t.Fatal("latest after retention", index)
		}
		if removed := model.RemoveSnIncentiveSnapshotsBefore(ctx, deploymentKey, start.Add(7*24*time.Hour)); removed != 0 {
			t.Fatal("the older snapshots outlived their retention", removed)
		}
	})
}
