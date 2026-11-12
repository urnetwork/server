package controller

// sn_ctf_api_source_db_test.go — the capture the flag routes' view over the
// real records: the CTF pool's roots mirrored under no_id 2^32 + n, the epoch
// results, pots, awards, census and draw the driver and the census step write
// (model/ctf_model.go, keyed by n), and the pending draw.

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/holiman/uint256"

	"github.com/urfoundation/sn/ctf"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// snCtfDbFixture records the example view's epochs 8 and 9 as the driver and
// the census step would: root 1 (epoch 8) finalized, root 2 (epoch 9) pending,
// epoch 10 in progress, with the census of epoch 9 and its members.
type snCtfDbFixture struct {
	t     testing.TB
	ctx   context.Context
	cfg   *StConfig
	chain *fakeLedgerV2Chain
	view  *snCtfView
}

func newSnCtfDbFixture(t testing.TB) *snCtfDbFixture {
	f := &snCtfDbFixture{t: t, ctx: context.Background(), cfg: ledgerV2TestConfig(), view: snCtfTestView(t)}
	// the operator's vault2 is the example's, which the source names from the
	// configuration
	f.cfg.VaultV2 = f.view.Pool.Vault
	f.chain = newFakeLedgerV2Chain(&[]string{})
	f.chain.firstEpoch = 8
	SetStConfig(f.cfg)
	SetStLedgerV2Client(f.chain)
	SetStClient(newStubStClient(&StEpochState{Epoch: 10, EpochStartBlock: 9_700_828, TEpochBlocks: 50_400, HeadBlock: 9_700_828, HeadBlockTime: f.view.Clock.StartTime}))
	carryOf := snCtfCarryOf
	snCtfCarryOf = func(context.Context, *StConfig, uint64) (common.Address, error) {
		return f.view.Pool.CarryContract, nil
	}
	snCtfPoolRegistrations.Clear()
	snCtfFinalizedEpochs.Clear()
	t.Cleanup(func() {
		SetStConfig(nil)
		SetStLedgerV2Client(nil)
		SetStClient(nil)
		snCtfCarryOf = carryOf
		snCtfPoolRegistrations.Clear()
	})
	key := f.cfg.DeploymentKey()
	poolId := ctf.PoolID(f.cfg.NoId)
	for _, root := range f.view.Roots {
		mirrored := &model.StLedgerV2Root{NoId: poolId, Seq: root.Seq, Epoch: root.Epoch, Total: *uint256.NewInt(1), LeafCount: 1, LedgerHash: root.LedgerHash}
		model.AddStLedgerV2Commit(f.ctx, key, mirrored, root.CommitBlock, 0)
		if root.Finalized {
			mirrored.RootIndex = root.RootIndex
			model.SetStLedgerV2RootFinalized(f.ctx, key, mirrored, *root.FinalizedBlock)
		}
	}
	for _, epoch := range f.view.Epochs {
		result := &model.CtfEpochResult{
			DeploymentKey: key, NoId: f.cfg.NoId, Epoch: epoch.Epoch, LedgerEpoch: epoch.Root.Epoch, LedgerHash: epoch.Root.LedgerHash,
			Drawn: epoch.Drawn, Funding: epoch.Funding, Returned: epoch.Returned, Owed: epoch.Owed, Matured: epoch.Matured,
			Fresh: epoch.Fresh, SlotBase: epoch.SlotBase, PaidSlots: epoch.PaidSlots, PaidPots: epoch.PaidPots,
			PaidPending: epoch.PaidPending, PendingTotal: epoch.PendingTotal, Carried: epoch.Carried,
			AwardsRef: epoch.AwardsRef, PotsRef: epoch.PotsRef, PendingRef: epoch.PendingRef, Awards: len(epoch.Awards),
		}
		if census := epoch.Census; census != nil {
			hash, block := census.Hash, census.Block
			result.CensusHash, result.CensusBlock, result.Round = &hash, &block, census.Round
		}
		awards := []*model.CtfAward{}
		for _, award := range epoch.Awards {
			awards = append(awards, &model.CtfAward{Epoch: epoch.Epoch, Role: award.Role, CountryCode: award.Country, Rank: award.Rank, Unit: award.Unit, UnitFamily: award.UnitFamily, ClientId: award.ClientId, Coldkey: award.Coldkey, Slot: award.Slot, Pot: award.Pot, Deferred: award.Deferred})
		}
		pots := []*model.CtfPot{}
		censusPots := []*model.CtfCensusPot{}
		for index, country := range snCtfCountries() {
			for _, role := range []int{snCtfRoleProvider, snCtfRoleExtender} {
				pot := epoch.pot(index, role)
				pots = append(pots, &model.CtfPot{Epoch: epoch.Epoch, CountryCode: country.Code, Role: role, Units: int(pot.Units), Winners: pot.Winners, In: pot.In, Out: pot.Out})
				censusPots = append(censusPots, &model.CtfCensusPot{CountryCode: country.Code, Role: role, Units: int(pot.Units), Candidates: int(pot.Units), NearMisses: int(pot.NearMisses)})
			}
		}
		if !model.CreateCtfEpochResult(f.ctx, result, awards, pots, []*model.CtfPendingAward{}) {
			t.Fatalf("epoch %d result not created", epoch.Epoch)
		}
		if census := epoch.Census; census != nil {
			if !model.CreateCtfCensus(f.ctx, &model.CtfCensus{DeploymentKey: key, NoId: f.cfg.NoId, Epoch: epoch.Epoch, CensusHash: census.Hash, Header: []byte("{}"),
				Candidates: 4_217, ProviderCandidates: 4_217, RequiredSamples: 336, StartBlock: epoch.StartBlock, EndBlock: epoch.EndBlock,
				StartTime: f.view.Clock.StartTime, EndTime: f.view.Clock.StartTime, CaptureTime: f.view.Clock.StartTime, ProviderSourceTime: f.view.Clock.StartTime}, nil, nil, censusPots, nil) {
				t.Fatalf("epoch %d census not created", epoch.Epoch)
			}
		}
	}
	return f
}

// The view over the records answers what the example view answers: the same
// countries, history, epoch and amounts.
func TestDbSnCtfSourceView(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		f := newSnCtfDbFixture(t)
		source, err := newSnCtfCoreSource()
		if err != nil {
			t.Fatal(err)
		}
		view, err := source.View(f.ctx)
		if err != nil {
			t.Fatal(err)
		}
		// the routes add the page from ctf.yml's api block (snCtfApiConfigOf);
		// the source never reads it
		if view.PageUrl != "" {
			t.Fatalf("the source set the page %q", view.PageUrl)
		}
		view.PageUrl = f.view.PageUrl
		want := snCtfResultOf(f.view)
		got := snCtfResultOf(view)
		// the stub's block times are estimates; the rest is the records'
		want.AsOf, want.CurrentEpoch.DrawAt, got.AsOf, got.CurrentEpoch.DrawAt = "", "", "", ""
		wantJson, _ := json.Marshal(want)
		gotJson, _ := json.Marshal(got)
		if string(wantJson) != string(gotJson) {
			t.Fatalf("view\n%s\nwant\n%s", gotJson, wantJson)
		}
		iceland, _ := snCtfParseCountry("is")
		wantCountry, _ := json.Marshal(snCtfCountryResultOf(f.view, iceland).History)
		gotCountry, _ := json.Marshal(snCtfCountryResultOf(view, iceland).History)
		if string(wantCountry) != string(gotCountry) {
			t.Fatalf("history\n%s\nwant\n%s", gotCountry, wantCountry)
		}
		if len(view.Epochs) != 2 || view.Epochs[0].Root.Seq != 1 || !view.Epochs[0].Root.Finalized || view.Epochs[1].Root.Seq != 2 || view.Pending != nil {
			t.Fatalf("epochs %d, pending %+v", len(view.Epochs), view.Pending)
		}
		if view.AsOf.Before(view.Roots[1].CommitTime) {
			t.Fatalf("as of %s before the latest root's commitment %s", view.AsOf, view.Roots[1].CommitTime)
		}
	})
}

// After epoch 10 ends, before its root: the pending draw names its committed
// census and round; and the routes answer from the records.
func TestDbSnCtfSourcePendingDraw(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		f := newSnCtfDbFixture(t)
		SetStClient(newStubStClient(&StEpochState{Epoch: 11, EpochStartBlock: 9_751_228, TEpochBlocks: 50_400, HeadBlock: 9_751_228, HeadBlockTime: f.view.Clock.StartTime.Add(7 * 24 * time.Hour)}))
		block, blockTime, round := uint64(9_751_300), uint64(f.view.Clock.StartTime.Add(7*24*time.Hour+15*time.Minute).Unix()), uint64(34_939_000)
		model.SetCtfCensusCommitment(f.ctx, &model.CtfCensusCommitment{DeploymentKey: f.cfg.DeploymentKey(), NoId: f.cfg.NoId, Epoch: 10, CensusHash: [32]byte{0x10},
			Status: model.CtfCommitmentStatusCommitted, CommitBlock: &block, CommitBlockTime: &blockTime, Round: &round})
		source, err := newSnCtfCoreSource()
		if err != nil {
			t.Fatal(err)
		}
		view, err := source.View(f.ctx)
		if err != nil {
			t.Fatal(err)
		}
		if view.Pending == nil || view.Pending.Epoch != 10 || view.Pending.Census == nil || view.Pending.Census.Block != block || view.Pending.Census.Round != round {
			t.Fatalf("pending %+v", view.Pending)
		}
		if !view.AsOf.Equal(time.Unix(int64(blockTime), 0).UTC()) {
			t.Fatalf("as of %s", view.AsOf)
		}
		detail, err := source.EpochDetail(f.ctx, view, view.Epochs[1])
		if err != nil || detail.Candidates != 4_217 {
			t.Fatalf("detail %+v (%v)", detail, err)
		}
		withSnCtfTestSource(t, source)
		if response := serveSnCtf(SnCtf, "/sn/ctf"); response.Code != http.StatusOK {
			t.Fatalf("GET /sn/ctf: %d %s", response.Code, response.Body.String())
		}
	})
}
