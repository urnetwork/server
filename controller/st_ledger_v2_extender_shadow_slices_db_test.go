package controller

// st_ledger_v2_extender_shadow_slices_db_test.go — a shadow run reads its
// epoch's contracts in short slices (sn WHITEPAPER.md §8.6.7; server CPUOPT5.md
// #1: the read in one snapshot pinned Main's primary for the whole epoch), to
// exactly the rows and extender pool of the read in one snapshot.

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/holiman/uint256"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// Epoch 2 of the extender pool scenario, a 20-minute window, read in 2-minute
// slices: ten slices, each in its own snapshot, to the rows and pool of the
// read in one, and a shadow report the validator's recompute approves.
func TestStLedgerV2ExtenderShadowReadsTheEpochInShortSlices(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		s := newStExtenderPoolScenario(t, 4)
		f := s.f
		stExtenderShadowConcluded.Clear()
		t.Cleanup(stExtenderShadowConcluded.Clear)
		build, err := (stLedgerV2Builder{}).Build(f.ctx, s.input)
		if err != nil {
			t.Fatal(err)
		}
		f.publish(s.input, build, 0)
		pool, err := stExtenderPoolSettings(f.cfg)
		if err != nil {
			t.Fatal(err)
		}
		amendment := model.StExtenderAmendment{FirstEpoch: 2, PlatformEndpoints: model.StPlatformEndpointSet{}}
		var sliceStarts []time.Time
		t.Cleanup(model.Testing_SetStEpochUsageSliceHook(func(sliceStart time.Time, attempt int, read bool) error {
			if !read {
				sliceStarts = append(sliceStarts, sliceStart)
			}
			return nil
		}))
		wholeRows, wholePool, err := stLedgerV2AmendedEpochRows(f.ctx, 2, amendment, pool, false, model.StEpochUsageSlices{})
		if err != nil {
			t.Fatal(err)
		}
		if len(sliceStarts) != 0 {
			t.Fatalf("the read in one snapshot read %d slices", len(sliceStarts))
		}
		settings := DefaultStLedgerV2ExtenderShadowSettings()
		settings.ReadSliceSpan = 2 * time.Minute
		slicedRows, slicedPool, err := stLedgerV2AmendedEpochRows(f.ctx, 2, amendment, pool, false, settings.slicing())
		if err != nil {
			t.Fatal(err)
		}
		authority := ledgerV2BuildAuthority(2)
		if len(sliceStarts) != 10 || !sliceStarts[0].Equal(authority.StartTime) || !sliceStarts[9].Equal(authority.StartTime.Add(18*time.Minute)) {
			t.Fatalf("slices from %v", sliceStarts)
		}
		if !slices.Equal(slicedRows, wholeRows) || slicedPool.Bytes() != wholePool.Bytes() || slicedPool.UnfundedBytes != wholePool.UnfundedBytes ||
			slicedPool.MeasurementBytes != wholePool.MeasurementBytes || slicedPool.Contracts != wholePool.Contracts || slicedPool.Payers != wholePool.Payers {
			t.Fatalf("sliced %+v %+v, want %+v %+v", slicedRows, slicedPool, wholeRows, wholePool)
		}
		if wholePool.IntraNetwork != 500 || wholePool.UnfundedBytes != 300 || wholePool.MeasurementBytes != 200 {
			t.Fatalf("pool %+v", wholePool)
		}

		// the run itself, in those slices
		if outcome := stLedgerV2CaptureExtenderCensus(f.ctx, f.cfg, f.chain, 2); outcome.Status != model.StPublishStatusConfirmed {
			t.Fatalf("census capture %+v", outcome)
		}
		sliceStarts = nil
		chain := &stExtenderShadowTestChain{state: StLedgerV2State{Registered: true, FirstEpoch: 1, LastFundedEpoch: 2},
			funded: map[uint64]uint256.Int{1: *uint256.NewInt(1_500), 2: *uint256.NewInt(3_800)}}
		outcome := stLedgerV2RunExtenderShadow(f.ctx, f.cfg, chain, 2, settings)
		if outcome.Status != model.StPublishStatusConfirmed || len(sliceStarts) != 10 {
			t.Fatalf("shadow %+v in %d slices", outcome, len(sliceStarts))
		}
		recorded := model.GetStLedgerV2ExtenderShadow(f.ctx, f.cfg.DeploymentKey(), 1, 2)
		if recorded == nil || !strings.Contains(outcome.Reason, recorded.ReportRef) {
			t.Fatalf("recorded %+v", recorded)
		}
	})
}
