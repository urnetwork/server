package controller

// st_ledger_v2_answer_test.go — demands name the finalized root their account
// is proven against (the contract review's H1 fix to G2), so the driver
// answers a demand on any finalized root, not only the latest
// (VERSION2-FINAL.md §8.1), and treats a revert showing the write already done
// as done (SPEC A6).

import (
	"crypto/sha256"
	"errors"
	"slices"
	"testing"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// Roots of epochs 5 and 6 finalized at indexes 0 and 1, and epoch 7's root
// pending as seq 3, mirrored. Accounts: root 0 a:500 b:500; root 1 a:500
// b:2000 c:1500; pending a:500 b:2000 c:2500.
func commitThreeRoots(t testing.TB, f *ledgerV2Fixture) [3][32]byte {
	t.Helper()
	a, b, c := ledgerV2Coldkey(1), ledgerV2Coldkey(2), ledgerV2Coldkey(3)
	f.chain.emission[5], f.chain.emission[6], f.chain.emission[7] = 1000, 3000, 1000
	f.builder.earners[5], f.builder.earners[6], f.builder.earners[7] = [][32]byte{b, a}, [][32]byte{b, c}, [][32]byte{c}
	for epoch := uint64(5); epoch <= 7; epoch++ {
		f.chain.head = f.chain.end(epoch) + 10
		requireLedgerV2Outcome(t, f.run(StLedgerV2StepEpoch, epoch, 0, 0), model.StPublishStatusConfirmed, false, "")
		f.chain.approvals = 1
	}
	f.takeLog()
	p := f.chain.pending
	if len(f.chain.roots) != 2 || p == nil || p.Seq != 3 {
		t.Fatalf("roots %d pending %+v", len(f.chain.roots), p)
	}
	model.AddStLedgerV2Commit(f.ctx, f.cfg.DeploymentKey(), &model.StLedgerV2Root{NoId: 1, Seq: p.Seq, Epoch: p.Epoch, Root: p.Root, Total: p.Total, LeafCount: p.LeafCount, LedgerHash: p.LedgerHash}, p.CommittedBlock, 0)
	return [3][32]byte{sha256.Sum256(f.builder.builds[0].Header), sha256.Sum256(f.builder.builds[1].Header), sha256.Sum256(f.builder.builds[2].Header)}
}

func (self *ledgerV2Fixture) runAnswer(seq uint64, rootIndex uint64, index uint64) *StPublishOutcome {
	self.t.Helper()
	outcome, err := StLedgerV2RunStep(self.ctx, &StLedgerV2Step{Kind: StLedgerV2StepAnswer, Epoch: 7, Seq: seq, RootIndex: rootIndex, Index: index})
	if err != nil {
		self.t.Fatalf("answer step: %v", err)
	}
	return outcome
}

// SV-1 (demand answering), H1: demands on the same account index of an older
// finalized root and of the latest are planned and answered separately, each
// with that root's account and its proof against that root, read back through
// the builder; an older root is read from vault2. A failed root read is
// retried, and a demand naming no finalized root is not answered. checkPath
// has no block window, so an answer never reads the latest block.
func TestStLedgerV2DriverAnswersDemandsOnAnyFinalizedRoot(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		f := newLedgerV2Fixture(t)
		hashes := commitThreeRoots(t, f)
		key := f.cfg.DeploymentKey()
		f.chain.demandAt(0, 1)
		f.chain.demandAt(1, 1)
		for rootIndex := range uint64(2) {
			model.AddStLedgerV2Demand(f.ctx, key, &model.StLedgerV2Demand{NoId: 1, Seq: 3, RootIndex: rootIndex, Index: 1, Demander: "0xd1", DemandBlock: f.chain.head})
		}
		steps := ledgerV2Sync(t, f, 8)
		for rootIndex := range uint64(2) {
			if !slices.Contains(steps, StLedgerV2Step{Kind: StLedgerV2StepAnswer, Epoch: 7, Seq: 3, RootIndex: rootIndex, Index: 1}) {
				t.Fatalf("the demand on root %d is not planned: %+v", rootIndex, steps)
			}
		}

		resetLedgerV2Caches()
		f.chain.failNext("root", errors.New("root unreadable"))
		requireLedgerV2Outcome(t, f.runAnswer(3, 0, 1), model.StPublishStatusFailed, true, "root unreadable")
		f.chain.failNext("latest", errors.New("latest unreadable"))
		requireLedgerV2Outcome(t, f.runAnswer(3, 0, 1), model.StPublishStatusConfirmed, false, "")
		requireLedgerV2Outcome(t, f.runAnswer(3, 1, 1), model.StPublishStatusConfirmed, false, "")
		if len(f.chain.fail["latest"]) != 1 {
			t.Fatal("an answer read the latest block")
		}
		requireLedgerV2Log(t, f.takeLog(), []string{"accounts " + hexPrefix(hashes[0]), "accounts " + hexPrefix(hashes[2]), "answer 3 0 1", "accounts " + hexPrefix(hashes[1]), "answer 3 1 1"})
		for rootIndex := range uint64(2) {
			if f.chain.demands[[3]uint64{3, rootIndex, 1}] != StLedgerV2DemandAnswered {
				t.Fatalf("the demand on root %d is %d", rootIndex, f.chain.demands[[3]uint64{3, rootIndex, 1}])
			}
		}
		if f.chain.pending.OpenDemands != 0 {
			t.Fatalf("%d demands open", f.chain.pending.OpenDemands)
		}

		f.chain.demands[[3]uint64{3, 2, 1}] = StLedgerV2DemandOpen
		requireLedgerV2Outcome(t, f.runAnswer(3, 2, 1), model.StPublishStatusSkipped, false, "seq 3 is not pending over finalized root 2")
		requireLedgerV2Log(t, f.takeLog(), nil)
	})
}

// SV-1, SPEC A6: an answer or finalize whose send reverts NoFault or
// NoPendingRoot was done by someone else and ends the step without a retry;
// any other refusal is retried.
func TestStLedgerV2DriverTreatsFrontRunWritesAsDone(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		f := newLedgerV2Fixture(t)
		commitThreeRoots(t, f)
		f.chain.demandAt(0, 0)
		for _, name := range []string{"NoFault", "NoPendingRoot"} {
			f.chain.failNext("answer", fakeLedgerV2Revert(name))
			requireLedgerV2Outcome(t, f.runAnswer(3, 0, 0), model.StPublishStatusSkipped, false, "already done: "+name)
		}
		f.chain.failNext("answer", fakeLedgerV2Revert("InvalidProof"))
		requireLedgerV2Outcome(t, f.runAnswer(3, 0, 0), model.StPublishStatusFailed, true, "execution reverted")
		if got := f.publishes(7); !slices.Equal(got[len(got)-3:], []string{"v2answer skipped", "v2answer skipped", "v2answer failed"}) {
			t.Fatalf("epoch 7 publishes %v", got)
		}

		f.chain.head = f.chain.pending.CommittedBlock + f.chain.challenge
		f.chain.pending.OpenDemands = 0
		f.chain.failNext("finalize", fakeLedgerV2Revert("NoPendingRoot"))
		requireLedgerV2Outcome(t, f.run(StLedgerV2StepFinalize, 7, 0, 0), model.StPublishStatusSkipped, false, "already done: NoPendingRoot")
		if got := f.publishes(7); got[len(got)-1] != "v2finalize skipped" {
			t.Fatalf("epoch 7 publishes %v", got)
		}
	})
}
