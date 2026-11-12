package controller

// st_ctf_driver_db_test.go — the capture the flag driver's records in the
// database (stCtfDbRecords): the same epoch as TestStCtfDriverRunsAnEpochInOrder,
// with the publish rows, the commitment, the draw, the fixed result and the
// ledger written to and read back from the real tables.

import (
	"testing"

	"github.com/urfoundation/sn/ledger"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

func TestStCtfDriverRecordsInTheDatabase(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		f := newCtfFixture(t, StCtfModeLive)
		setStCtfRecords(nil)
		key := f.cfg.DeploymentKey()
		f.census[8] = ctfTestCensusOf(t, 8, ctfTestCandidates()...)
		f.commitRound(8, 1000)
		f.emission(8, 1_000_000_000_000)
		f.chain.head = f.chain.end(8)

		requireLedgerV2Outcome(t, f.run(StCtfDriverStepCensus, 8), model.StPublishStatusConfirmed, false, "")
		requireLedgerV2Outcome(t, f.run(StCtfDriverStepCensus, 8), model.StPublishStatusSkipped, false, "is committed at block")
		requireLedgerV2Outcome(t, f.run(StCtfDriverStepEpoch, 8), model.StPublishStatusConfirmed, false, "")
		requireCtfLog(t, f.takeLog(), "census 8", "close 8", "commit 8 prev 0")
		requireCtfAudit(t, f.verify(nil))

		records := stCtfRecordsFor(f.cfg)
		if status, _ := records.Outcome(f.ctx, 8, model.StPublishKindCtfCommit); status != model.StPublishStatusConfirmed {
			t.Fatalf("commit outcome %q", status)
		}
		commitment := model.GetCtfCensusCommitment(f.ctx, key, ctfTestNoId, 8)
		if commitment == nil || commitment.Status != model.CtfCommitmentStatusCommitted || commitment.CensusHash != f.census[8].Hash ||
			commitment.Round == nil || *commitment.Round != 1000 || commitment.CommitBlock == nil || *commitment.CommitBlock != f.chain.end(8)+1 {
			t.Fatalf("commitment %+v", commitment)
		}
		if draw := model.GetCtfDraw(f.ctx, key, ctfTestNoId, 8); draw == nil || draw.Round != 1000 || len(draw.Signature) != 48 {
			t.Fatalf("draw %+v", draw)
		}
		pending := f.chain.pending
		result := model.GetCtfEpochResult(f.ctx, key, ctfTestNoId, 8)
		if result == nil || result.LedgerHash != pending.LedgerHash || !result.Drawn || result.Awards != 6 {
			t.Fatalf("result %+v", result)
		}
		if awards := model.GetCtfAwards(f.ctx, key, ctfTestNoId, 8); len(awards) != 6 {
			t.Fatalf("%d awards", len(awards))
		}
		if pots := model.GetCtfPots(f.ctx, key, ctfTestNoId, 8); len(pots) != 498 {
			t.Fatalf("%d pots", len(pots))
		}
		built := model.GetCtfLedger(f.ctx, key, ctfTestNoId, 8, pending.LedgerHash)
		if built == nil || built.Status != model.CtfLedgerStatusCommitted || built.Shadow || built.FirstEpoch != 8 {
			t.Fatalf("ledger %+v", built)
		}

		// a rebuild reads the fixed result back and reproduces it
		fixed := records.EpochResults(f.ctx, 8, 8)[8]
		if fixed == nil || ledger.Ref(fixed.AwardsRef) != ledger.Ref(result.AwardsRef) {
			t.Fatalf("fixed %+v", fixed)
		}

		// the mirror's root moves the ledger to finalized once recorded
		commitBlock := pending.CommittedBlock
		model.AddStLedgerV2Commit(f.ctx, key, &model.StLedgerV2Root{NoId: ctfTestPoolId, Seq: pending.Seq, Epoch: 8, Root: pending.Root, Total: pending.Total,
			LeafCount: pending.LeafCount, LedgerHash: pending.LedgerHash}, commitBlock, 0)
		f.chain.approvals = 1
		f.chain.head += f.chain.challenge
		requireLedgerV2Outcome(t, f.run(StCtfDriverStepFinalize, 8), model.StPublishStatusConfirmed, false, "")
		rootIndex := uint64(0)
		model.SetStLedgerV2RootFinalized(f.ctx, key, &model.StLedgerV2Root{NoId: ctfTestPoolId, Seq: pending.Seq, Epoch: 8, Root: pending.Root, Total: pending.Total,
			LeafCount: pending.LeafCount, LedgerHash: pending.LedgerHash, RootIndex: &rootIndex}, f.chain.head)
		requireLedgerV2Outcome(t, f.runStep(&StCtfDriverStep{Kind: StCtfDriverStepRecord, Seq: pending.Seq}), model.StPublishStatusConfirmed, false, "recorded")
		if built := model.GetCtfLedger(f.ctx, key, ctfTestNoId, 8, pending.LedgerHash); built.Status != model.CtfLedgerStatusFinalized || built.RootIndex == nil || *built.RootIndex != 0 {
			t.Fatalf("finalized ledger %+v", built)
		}
		if count := model.GetStLedgerV2AccountCount(f.ctx, key, ctfTestPoolId); count != pending.LeafCount {
			t.Fatalf("%d accounts recorded of %d", count, pending.LeafCount)
		}
		// served by the ledger account route, as any pool's account
		awards := model.GetCtfAwards(f.ctx, key, ctfTestNoId, 8)
		SetStLedgerBuilder(stLedgerV2Builder{})
		t.Cleanup(func() { SetStLedgerBuilder(nil) })
		accounts, err := StLedgerV2Accounts(f.ctx, awards[0].Coldkey)
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for _, account := range accounts.Accounts {
			found = found || account.NoId == ctfTestPoolId
		}
		if !found {
			t.Fatalf("the winner's capture the flag account is not served: %+v", accounts.Accounts)
		}
	})
}
