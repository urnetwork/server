package controller

// st_ledger_v2_driver_test.go — the settlement v2 driver (VERSION2-FINAL.md
// §8.1) against an in-memory chain and builder, with the real database:
// SV-1 and SV-4.

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/holiman/uint256"

	"github.com/urfoundation/sn/merkle"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/startifact"
)

// The log of publishing a build: its chunks, then its header.
func ledgerV2BuildPuts(build *StLedgerV2Build) []string {
	puts := []string{}
	for _, chunk := range build.Chunks {
		puts = append(puts, ledgerV2PutEntry(chunk))
	}
	return append(puts, ledgerV2PutEntry(build.Header))
}

func ledgerV2Join(parts ...[]string) []string {
	return slices.Concat(parts...)
}

func requireLedgerV2Log(t testing.TB, got []string, want []string) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("calls %q\nwant  %q", got, want)
	}
}

func requireLedgerV2Outcome(t testing.TB, outcome *StPublishOutcome, status string, retry bool, reason string) {
	t.Helper()
	if outcome.Status != status || outcome.Retry != retry || !strings.Contains(outcome.Reason, reason) {
		t.Fatalf("outcome %+v, want %s retry=%t reason containing %q", outcome, status, retry, reason)
	}
}

func ledgerV2Accounts(entries ...any) []merkle.LedgerAccount {
	accounts := []merkle.LedgerAccount{}
	for i := 0; i < len(entries); i += 2 {
		accounts = append(accounts, merkle.LedgerAccount{Coldkey: entries[i].([32]byte), Cumulative: *uint256.NewInt(uint64(entries[i+1].(int)))})
	}
	return accounts
}

func ledgerV2Sync(t testing.TB, f *ledgerV2Fixture, epoch uint64) []StLedgerV2Step {
	t.Helper()
	steps, err := StLedgerV2Sync(f.ctx, &StEpochState{Epoch: epoch, HeadBlock: f.chain.head, HeadBlockTime: time.Now().UTC()})
	if err != nil {
		t.Fatal(err)
	}
	out := []StLedgerV2Step{}
	for _, step := range steps {
		out = append(out, StLedgerV2Step{Kind: step.Kind, Epoch: step.Epoch, Seq: step.Seq, RootIndex: step.RootIndex, Index: step.Index})
	}
	return out
}

// SV-1 (order): per epoch the driver closes inside the window, finalizes the
// previous root, reads the latest finalized root and the root count, builds,
// publishes the chunks and then the header, and commits with that root count.
func TestStLedgerV2DriverRunsEpochsInOrder(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		f := newLedgerV2Fixture(t)
		a, b, c := ledgerV2Coldkey(1), ledgerV2Coldkey(2), ledgerV2Coldkey(3)
		f.chain.emission[5], f.chain.emission[6] = 1000, 3000
		f.builder.earners[5], f.builder.earners[6] = [][32]byte{b, a}, [][32]byte{b, c}

		f.chain.head = f.chain.end(5) + 10
		requireLedgerV2Outcome(t, f.run(StLedgerV2StepEpoch, 5, 0, 0), model.StPublishStatusConfirmed, false, "")
		build5 := f.builder.builds[0]
		requireLedgerV2Log(t, f.takeLog(), ledgerV2Join([]string{"close 5", "build 5"}, ledgerV2BuildPuts(build5), []string{"commit 5 prev 0"}))
		if !reflect.DeepEqual(build5.Accounts, ledgerV2Accounts(a, 500, b, 500)) {
			t.Fatalf("epoch 5 accounts %v", build5.Accounts)
		}
		tree, err := merkle.BuildLedgerSumTree(f.ctx, build5.Accounts)
		if err != nil {
			t.Fatal(err)
		}
		root, total := tree.Root()
		if p := f.chain.pending; p == nil || p.Epoch != 5 || p.Root != root || !p.Total.Eq(&total) || p.Total.Uint64() != 1000 || p.LeafCount != 2 || p.LedgerHash != sha256.Sum256(build5.Header) {
			t.Fatalf("committed root %+v", p)
		}
		if records := model.GetStLedgerV2EpochRows(f.ctx, f.cfg.DeploymentKey(), 1, 5, 5); len(records) != 1 || !reflect.DeepEqual(records[5].ProviderChunks, build5.Epochs[0].ProviderChunks) || len(records[5].ProviderChunks) != 1 {
			t.Fatalf("epoch 5 record %+v", records[5])
		}
		if header, err := GetStLedgerV2Object(f.ctx, startifact.LedgerObjectRef(build5.Header)); err != nil || string(header) != string(build5.Header) {
			t.Fatalf("published header %q, %v", header, err)
		}
		if got := f.publishes(5); !reflect.DeepEqual(got, []string{"v2close confirmed", "v2commit confirmed"}) {
			t.Fatalf("epoch 5 publishes %v", got)
		}

		f.chain.approvals = 1
		f.chain.head = f.chain.end(6) + 10
		requireLedgerV2Outcome(t, f.run(StLedgerV2StepEpoch, 6, 0, 0), model.StPublishStatusConfirmed, false, "")
		build6 := f.builder.builds[1]
		requireLedgerV2Log(t, f.takeLog(), ledgerV2Join([]string{"close 6", "finalize 5", "build 6"}, ledgerV2BuildPuts(build6), []string{"commit 6 prev 1"}))
		input := f.builder.inputs[1]
		if input.NoId != 1 || input.Epoch != 6 || input.Previous == nil || input.Previous.Index != 0 || input.Previous.Epoch != 5 || len(input.Epochs) != 1 || input.Epochs[0].Epoch != 6 || input.Epochs[0].Funding.Uint64() != 3000 || input.Epochs[0].Rows != nil {
			t.Fatalf("epoch 6 build input %+v", input)
		}
		if !reflect.DeepEqual(build6.Accounts, ledgerV2Accounts(a, 500, b, 2000, c, 1500)) {
			t.Fatalf("epoch 6 accounts %v", build6.Accounts)
		}
		if p := f.chain.pending; p == nil || p.Epoch != 6 || p.Total.Uint64() != 4000 || p.LeafCount != 3 || len(f.chain.roots) != 1 || f.chain.roots[0].Epoch != 5 {
			t.Fatalf("pending %+v roots %+v", p, f.chain.roots)
		}
		if got := f.publishes(5); !reflect.DeepEqual(got, []string{"v2close confirmed", "v2commit confirmed", "v2finalize confirmed"}) {
			t.Fatalf("epoch 5 publishes %v", got)
		}
		if got := f.publishes(6); !reflect.DeepEqual(got, []string{"v2close confirmed", "v2commit confirmed"}) {
			t.Fatalf("epoch 6 publishes %v", got)
		}
		if len(*f.alerts) != 0 {
			t.Fatalf("alerts %v", *f.alerts)
		}
	})
}

// SV-1 (retries): a failed read, send or build is retried; a repeated step
// skips what is done, rebuilds byte-identically, and stops once committed;
// pause and an unended epoch are waited out.
func TestStLedgerV2DriverRetriesFailedSteps(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		f := newLedgerV2Fixture(t)
		a := ledgerV2Coldkey(1)
		f.chain.emission[5], f.chain.emission[6] = 1000, 2000
		f.builder.earners[5], f.builder.earners[6] = [][32]byte{a}, [][32]byte{a}

		f.chain.head = f.chain.end(5) - 1
		outcome := f.run(StLedgerV2StepEpoch, 5, 0, 0)
		requireLedgerV2Outcome(t, outcome, model.StPublishStatusSkipped, true, "epoch 5 ends at block 252000")
		if until := time.Until(outcome.RetryAt); until <= 0 || 13*time.Second < until {
			t.Fatalf("retry at %s for a block one ahead", outcome.RetryAt)
		}

		f.chain.head = f.chain.end(5) + 10
		f.chain.failNext("state", errors.New("rpc unavailable"))
		requireLedgerV2Outcome(t, f.run(StLedgerV2StepEpoch, 5, 0, 0), model.StPublishStatusFailed, true, "rpc unavailable")
		f.chain.failNext("close", errors.New("broadcast failed"))
		requireLedgerV2Outcome(t, f.run(StLedgerV2StepEpoch, 5, 0, 0), model.StPublishStatusFailed, true, "broadcast failed")
		f.builder.fail = errors.New("census unavailable")
		requireLedgerV2Outcome(t, f.run(StLedgerV2StepEpoch, 5, 0, 0), model.StPublishStatusFailed, true, "census unavailable")
		requireLedgerV2Log(t, f.takeLog(), []string{"close 5", "close 5", "build 5"})

		f.chain.failNext("commit", errors.New("nonce gap"))
		requireLedgerV2Outcome(t, f.run(StLedgerV2StepEpoch, 5, 0, 0), model.StPublishStatusFailed, true, "nonce gap")
		first := f.builder.builds[0]
		requireLedgerV2Log(t, f.takeLog(), ledgerV2Join([]string{"build 5"}, ledgerV2BuildPuts(first), []string{"commit 5 prev 0"}))
		if steps := ledgerV2Sync(t, f, 6); !slices.Contains(steps, StLedgerV2Step{Kind: StLedgerV2StepEpoch, Epoch: 5}) {
			t.Fatalf("a failed epoch is not planned again: %+v", steps)
		}

		requireLedgerV2Outcome(t, f.run(StLedgerV2StepEpoch, 5, 0, 0), model.StPublishStatusConfirmed, false, "")
		second := f.builder.builds[1]
		requireLedgerV2Log(t, f.takeLog(), ledgerV2Join([]string{"build 5"}, ledgerV2BuildPuts(second), []string{"commit 5 prev 0"}))
		// The retry lists the epoch's recorded rows instead of building them
		// again, and commits the identical header.
		if string(first.Header) != string(second.Header) || !reflect.DeepEqual(second.Chunks, first.Chunks[1:]) {
			t.Fatal("a retried build differs")
		}
		if got := f.publishes(5); !reflect.DeepEqual(got, []string{"v2close failed", "v2close confirmed", "v2commit failed", "v2commit confirmed"}) {
			t.Fatalf("epoch 5 publishes %v", got)
		}
		if steps := ledgerV2Sync(t, f, 6); slices.Contains(steps, StLedgerV2Step{Kind: StLedgerV2StepEpoch, Epoch: 5}) {
			t.Fatalf("a committed epoch is planned again: %+v", steps)
		}

		f.chain.approvals = 1
		f.chain.head = f.chain.end(6) + 10
		f.chain.paused = true
		requireLedgerV2Outcome(t, f.run(StLedgerV2StepEpoch, 6, 0, 0), model.StPublishStatusSkipped, true, "settlement is paused")
		requireLedgerV2Log(t, f.takeLog(), nil)
		f.chain.paused = false
		requireLedgerV2Outcome(t, f.run(StLedgerV2StepEpoch, 6, 0, 0), model.StPublishStatusConfirmed, false, "")
		requireLedgerV2Log(t, f.takeLog(), ledgerV2Join([]string{"close 6", "finalize 5", "build 6"}, ledgerV2BuildPuts(f.builder.builds[2]), []string{"commit 6 prev 1"}))
	})
}

// SV-1 (deferral on a missed close): a close missed beyond its window is
// alerted and never sent; the next close defers the epoch, which the next root
// covers with zero funding while the emission it left behind funds the later
// epoch.
func TestStLedgerV2DriverDefersAMissedClose(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		f := newLedgerV2Fixture(t)
		a, b := ledgerV2Coldkey(1), ledgerV2Coldkey(2)
		f.chain.emission[5], f.chain.emission[6], f.chain.emission[7] = 1000, 2000, 4000
		f.builder.earners[5], f.builder.earners[6], f.builder.earners[7] = [][32]byte{a}, [][32]byte{b}, [][32]byte{a}
		f.chain.head = f.chain.end(5) + 10
		f.run(StLedgerV2StepEpoch, 5, 0, 0)
		f.takeLog()
		f.chain.approvals = 1

		f.chain.head = f.chain.end(6) + f.chain.window + 1
		requireLedgerV2Outcome(t, f.run(StLedgerV2StepEpoch, 6, 0, 0), model.StPublishStatusMissed, false, "close window closed at block 303600")
		requireLedgerV2Log(t, f.takeLog(), nil)
		if got := f.publishes(6); !reflect.DeepEqual(got, []string{"v2commit missed"}) {
			t.Fatalf("epoch 6 publishes %v", got)
		}
		if !reflect.DeepEqual(*f.alerts, []string{StLedgerV2AlertMissedClose + " epoch 6"}) {
			t.Fatalf("alerts %v", *f.alerts)
		}

		f.chain.head = f.chain.end(7) + 10
		requireLedgerV2Outcome(t, f.run(StLedgerV2StepEpoch, 7, 0, 0), model.StPublishStatusConfirmed, false, "")
		build := f.builder.builds[len(f.builder.builds)-1]
		requireLedgerV2Log(t, f.takeLog(), ledgerV2Join([]string{"close 7", "defer 6", "finalize 5", "build 7"}, ledgerV2BuildPuts(build), []string{"commit 7 prev 1"}))
		input := f.builder.inputs[len(f.builder.inputs)-1]
		if len(input.Epochs) != 2 || input.Epochs[0].Epoch != 6 || !input.Epochs[0].Funding.IsZero() || input.Epochs[1].Epoch != 7 || input.Epochs[1].Funding.Uint64() != 6000 {
			t.Fatalf("covered epochs %+v %+v", input.Epochs[0], input.Epochs[1])
		}
		if p := f.chain.pending; p == nil || p.Epoch != 7 || p.Total.Uint64() != 7000 || !reflect.DeepEqual(build.Accounts, ledgerV2Accounts(a, 7000)) {
			t.Fatalf("root covering the deferred epoch %+v, accounts %v", p, build.Accounts)
		}

		// The mirror alerts on every deferred epoch, including one the
		// driver never saw.
		*f.alerts = nil
		stLedgerV2Alerted.Clear()
		stApplyLedgerV2Events(f.ctx, f.cfg, []*model.StChainEvent{ledgerV2MirrorEvent(t, f.chain.head, 0, "V2EmissionDeferred", map[string]any{"e": "6", "no_id": "1", "funded_through": "1000"})})
		if !reflect.DeepEqual(*f.alerts, []string{StLedgerV2AlertMissedClose + " epoch 6"}) {
			t.Fatalf("mirror alerts %v", *f.alerts)
		}
	})
}

// commitTwoRoots commits epochs 5 and 6, finalizing 5, and mirrors the
// pending root of epoch 6 (seq 2).
func commitTwoRoots(t testing.TB, f *ledgerV2Fixture) (*StLedgerV2Build, *StLedgerV2Build) {
	t.Helper()
	a, b, c := ledgerV2Coldkey(1), ledgerV2Coldkey(2), ledgerV2Coldkey(3)
	f.chain.emission[5], f.chain.emission[6] = 1000, 3000
	f.builder.earners[5], f.builder.earners[6] = [][32]byte{b, a}, [][32]byte{b, c}
	f.chain.head = f.chain.end(5) + 10
	f.run(StLedgerV2StepEpoch, 5, 0, 0)
	f.chain.approvals = 1
	f.chain.head = f.chain.end(6) + 10
	f.run(StLedgerV2StepEpoch, 6, 0, 0)
	f.takeLog()
	p := f.chain.pending
	root := &model.StLedgerV2Root{NoId: 1, Seq: p.Seq, Epoch: p.Epoch, Root: p.Root, Total: p.Total, LeafCount: p.LeafCount, LedgerHash: p.LedgerHash}
	model.AddStLedgerV2Commit(f.ctx, f.cfg.DeploymentKey(), root, p.CommittedBlock, 0)
	return f.builder.builds[0], f.builder.builds[1]
}

// SV-1 (demand answering): an open demand on the pending root is answered
// with a clean opening built from the stored account rows; an answered, absent
// or stale demand is already done (SPEC A6); an opening that would show a fault
// or does not rebuild the committed root is never sent.
func TestStLedgerV2DriverAnswersDemands(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		f := newLedgerV2Fixture(t)
		build5, build6 := commitTwoRoots(t, f)
		hash5, hash6 := sha256.Sum256(build5.Header), sha256.Sum256(build6.Header)
		f.chain.demand(1)
		model.AddStLedgerV2Demand(f.ctx, f.cfg.DeploymentKey(), &model.StLedgerV2Demand{NoId: 1, Seq: 2, Index: 1, Demander: "0xd1", DemandBlock: f.chain.head})
		if steps := ledgerV2Sync(t, f, 7); !slices.Contains(steps, StLedgerV2Step{Kind: StLedgerV2StepAnswer, Epoch: 6, Seq: 2, Index: 1}) {
			t.Fatalf("the open demand is not planned: %+v", steps)
		}

		resetLedgerV2Caches()
		requireLedgerV2Outcome(t, f.run(StLedgerV2StepAnswer, 6, 2, 1), model.StPublishStatusConfirmed, false, "")
		requireLedgerV2Log(t, f.takeLog(), []string{"accounts " + hexPrefix(hash5), "accounts " + hexPrefix(hash6), "answer 2 0 1"})
		if f.chain.demands[[3]uint64{2, 0, 1}] != StLedgerV2DemandAnswered || f.chain.pending.OpenDemands != 0 {
			t.Fatalf("demand %d open %d", f.chain.demands[[3]uint64{2, 0, 1}], f.chain.pending.OpenDemands)
		}
		if got := f.publishes(6); got[len(got)-1] != "v2answer confirmed" {
			t.Fatalf("epoch 6 publishes %v", got)
		}

		for _, done := range []struct{ seq, index uint64 }{{2, 1}, {2, 0}, {1, 1}} {
			requireLedgerV2Outcome(t, f.run(StLedgerV2StepAnswer, 6, done.seq, done.index), model.StPublishStatusSkipped, false, "")
		}
		requireLedgerV2Log(t, f.takeLog(), nil)

		// Stored accounts that do not rebuild the finalized root.
		f.chain.demand(0)
		honest := slices.Clone(f.builder.ledgers[hash5])
		f.builder.ledgers[hash5][0].Cumulative.AddUint64(&f.builder.ledgers[hash5][0].Cumulative, 1)
		resetLedgerV2Caches()
		requireLedgerV2Outcome(t, f.run(StLedgerV2StepAnswer, 6, 2, 0), model.StPublishStatusFailed, true, "do not rebuild its root")
		requireLedgerV2Log(t, f.takeLog(), []string{"accounts " + hexPrefix(hash5)})
		f.builder.ledgers[hash5] = honest

		// A pending root that lowers the demanded account opens with a fault.
		lowered := ledgerV2Accounts(ledgerV2Coldkey(1), 499, ledgerV2Coldkey(2), 2000, ledgerV2Coldkey(3), 1501)
		tree, err := merkle.BuildLedgerSumTree(f.ctx, lowered)
		if err != nil {
			t.Fatal(err)
		}
		f.chain.pending.Root, f.chain.pending.Total = tree.Root()
		f.builder.ledgers[hash6] = lowered
		resetLedgerV2Caches()
		requireLedgerV2Outcome(t, f.run(StLedgerV2StepAnswer, 6, 2, 0), model.StPublishStatusFailed, true, "with fault lower_value")
		requireLedgerV2Log(t, f.takeLog(), []string{"accounts " + hexPrefix(hash5), "accounts " + hexPrefix(hash6)})
	})
}

// The fake builder's log name for a ledger.
func hexPrefix(hash [32]byte) string {
	return fmt.Sprintf("%x", hash[:4])
}

// SV-1 (finalize): the pending root is finalized once its window has passed
// with a quorum and no open demand; a root whose approvals closed short of the
// quorum is left to the next commit; a root someone else finalized is done.
func TestStLedgerV2DriverFinalizesWhenDue(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		f := newLedgerV2Fixture(t)
		f.chain.emission[5], f.chain.emission[6] = 1000, 1000
		f.builder.earners[5], f.builder.earners[6] = [][32]byte{ledgerV2Coldkey(1)}, [][32]byte{ledgerV2Coldkey(1)}
		f.chain.head = f.chain.end(5) + 10
		f.run(StLedgerV2StepEpoch, 5, 0, 0)
		f.takeLog()
		committed := f.chain.pending.CommittedBlock

		requireLedgerV2Outcome(t, f.run(StLedgerV2StepFinalize, 4, 0, 0), model.StPublishStatusSkipped, false, "no pending root for epoch 4")
		requireLedgerV2Outcome(t, f.run(StLedgerV2StepFinalize, 5, 0, 0), model.StPublishStatusSkipped, true, "the challenge window ends at block 266410")
		f.chain.head = committed + f.chain.challenge
		requireLedgerV2Outcome(t, f.run(StLedgerV2StepFinalize, 5, 0, 0), model.StPublishStatusSkipped, true, "0 of 1 approvals")
		f.chain.approvals = 1
		f.chain.demand(0)
		requireLedgerV2Outcome(t, f.run(StLedgerV2StepFinalize, 5, 0, 0), model.StPublishStatusSkipped, true, "1 open demands")
		f.chain.pending.OpenDemands = 0
		f.chain.paused = true
		requireLedgerV2Outcome(t, f.run(StLedgerV2StepFinalize, 5, 0, 0), model.StPublishStatusSkipped, true, "settlement is paused")
		f.chain.paused = false
		requireLedgerV2Log(t, f.takeLog(), nil)

		// A send that reverts because someone else finalized first is retried,
		// and the retry finds the root already finalized (SPEC A6).
		f.chain.failNext("finalize", errors.New("execution reverted: NoPendingRoot"))
		requireLedgerV2Outcome(t, f.run(StLedgerV2StepFinalize, 5, 0, 0), model.StPublishStatusFailed, true, "NoPendingRoot")
		f.chain.roots = append(f.chain.roots, &StLedgerV2FinalizedRoot{Root: f.chain.pending.Root, Total: f.chain.pending.Total, LeafCount: f.chain.pending.LeafCount, Epoch: 5, LedgerHash: f.chain.pending.LedgerHash})
		f.chain.pending = nil
		requireLedgerV2Outcome(t, f.run(StLedgerV2StepFinalize, 5, 0, 0), model.StPublishStatusSkipped, false, "no pending root for epoch 5")
		requireLedgerV2Log(t, f.takeLog(), []string{"finalize 5"})

		f.chain.head = f.chain.end(6) + 10
		f.run(StLedgerV2StepEpoch, 6, 0, 0)
		f.takeLog()
		f.chain.head = f.chain.pending.CommittedBlock + f.chain.challenge
		f.chain.approvals = 1
		requireLedgerV2Outcome(t, f.run(StLedgerV2StepFinalize, 6, 0, 0), model.StPublishStatusConfirmed, false, "")
		requireLedgerV2Log(t, f.takeLog(), []string{"finalize 6"})
		if len(f.chain.roots) != 2 || f.chain.pending != nil {
			t.Fatalf("roots %d pending %+v", len(f.chain.roots), f.chain.pending)
		}

		f.chain.emission[7] = 1000
		f.builder.earners[7] = [][32]byte{ledgerV2Coldkey(2)}
		f.chain.head = f.chain.end(7) + 10
		f.run(StLedgerV2StepEpoch, 7, 0, 0)
		f.chain.head = f.chain.end(8)
		requireLedgerV2Outcome(t, f.run(StLedgerV2StepFinalize, 7, 0, 0), model.StPublishStatusMissed, false, "approvals closed at block 403200 with 0 of 1")
		if got := f.publishes(7); got[len(got)-1] != "v2finalize missed" {
			t.Fatalf("epoch 7 publishes %v", got)
		}
	})
}

// SV-1 (providers: null only by explicit command): a build that lists an
// epoch as null without the operator's declaration, or changes a recorded
// epoch's rows, is refused before anything is published; the operator command
// declares an epoch null, naming who, and later roots keep every recorded epoch.
func TestStLedgerV2DriverNullOnlyByCommand(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		f := newLedgerV2Fixture(t)
		a := ledgerV2Coldkey(1)
		key := f.cfg.DeploymentKey()
		f.chain.emission[5], f.chain.emission[6], f.chain.emission[7], f.chain.emission[8] = 1000, 2000, 3000, 500
		f.builder.earners[5], f.builder.earners[6], f.builder.earners[7], f.builder.earners[8] = [][32]byte{a}, [][32]byte{a}, [][32]byte{a}, [][32]byte{a}

		f.chain.head = f.chain.end(5) + 10
		f.builder.nullUnrecorded = true
		requireLedgerV2Outcome(t, f.run(StLedgerV2StepEpoch, 5, 0, 0), model.StPublishStatusFailed, true, "epoch 5 is null without the operator's declaration")
		requireLedgerV2Log(t, f.takeLog(), []string{"close 5", "build 5"})
		if records := model.GetStLedgerV2EpochRows(f.ctx, key, 1, 5, 5); len(records) != 0 {
			t.Fatalf("refused build recorded %+v", records)
		}
		f.builder.nullUnrecorded = false
		delete(f.builder.earners, 5)
		requireLedgerV2Outcome(t, f.run(StLedgerV2StepEpoch, 5, 0, 0), model.StPublishStatusSkipped, false, ErrStLedgerV2NoRoot.Error())

		if err := StLedgerV2DeclareUnrecoverable(f.ctx, 6, "operator@example"); err != nil {
			t.Fatal(err)
		}
		f.chain.head = f.chain.end(6) + 10
		requireLedgerV2Outcome(t, f.run(StLedgerV2StepEpoch, 6, 0, 0), model.StPublishStatusSkipped, false, ErrStLedgerV2NoRoot.Error())
		f.chain.head = f.chain.end(7) + 10
		requireLedgerV2Outcome(t, f.run(StLedgerV2StepEpoch, 7, 0, 0), model.StPublishStatusConfirmed, false, "")
		build7 := f.builder.builds[len(f.builder.builds)-1]
		if p := f.chain.pending; p == nil || p.Total.Uint64() != 6000 || !reflect.DeepEqual(build7.Accounts, ledgerV2Accounts(a, 6000)) {
			t.Fatalf("root 7 %+v accounts %v", p, build7.Accounts)
		}
		records := model.GetStLedgerV2EpochRows(f.ctx, key, 1, 5, 7)
		if len(records) != 3 || records[5].Null() || len(records[5].ProviderChunks) != 0 || !records[6].Null() || records[6].DeclaredBy != "operator@example" || len(records[7].ProviderChunks) != 1 {
			t.Fatalf("records %+v %+v %+v", records[5], records[6], records[7])
		}
		if !build7.Epochs[1].Null || build7.Epochs[1].Epoch != 6 {
			t.Fatalf("epoch 6 listed as %+v", build7.Epochs[1])
		}

		if err := StLedgerV2DeclareUnrecoverable(f.ctx, 7, "operator@example"); !errors.Is(err, model.ErrStLedgerV2EpochRowsFixed) {
			t.Fatalf("declaring a recorded epoch: %v", err)
		}
		if err := StLedgerV2DeclareUnrecoverable(f.ctx, 8, ""); err == nil || !strings.Contains(err.Error(), "must name who makes it") {
			t.Fatalf("anonymous declaration: %v", err)
		}

		// Root 7 is never approved, so the next root covers 5 … 8 again and
		// must list the recorded epochs unchanged.
		f.takeLog()
		f.chain.head = f.chain.end(8) + 10
		f.builder.changeRecorded = true
		requireLedgerV2Outcome(t, f.run(StLedgerV2StepEpoch, 8, 0, 0), model.StPublishStatusFailed, true, "the build changes epoch 5's recorded provider rows")
		requireLedgerV2Log(t, f.takeLog(), []string{"close 8", "build 8"})
		f.builder.changeRecorded = false
		requireLedgerV2Outcome(t, f.run(StLedgerV2StepEpoch, 8, 0, 0), model.StPublishStatusConfirmed, false, "")
		input := f.builder.inputs[len(f.builder.inputs)-1]
		if input.Previous != nil || len(input.Epochs) != 4 || input.Epochs[0].Rows == nil || input.Epochs[1].Rows == nil || !input.Epochs[1].Rows.Null() || input.Epochs[2].Rows == nil || input.Epochs[3].Rows != nil {
			t.Fatalf("epoch 8 input %+v", input)
		}
		build8 := f.builder.builds[len(f.builder.builds)-1]
		for i := range 3 {
			if !reflect.DeepEqual(build8.Epochs[i], build7.Epochs[i]) {
				t.Fatalf("epoch %d listed as %+v, then %+v", 5+i, build7.Epochs[i], build8.Epochs[i])
			}
		}
		if p := f.chain.pending; p == nil || p.Epoch != 8 || p.Total.Uint64() != 6500 {
			t.Fatalf("root 8 %+v", p)
		}

		SetStConfig(&StConfig{})
		if err := StLedgerV2DeclareUnrecoverable(f.ctx, 9, "operator@example"); !errors.Is(err, errStLedgerV2NotConfigured) {
			t.Fatalf("declaration without v2: %v", err)
		}
	})
}

// SV-1 (no commit unless the total grows): a total that does not exceed the
// finalized total, an epoch that does not allocate, and a build whose total
// differs from the funding are never committed.
func TestStLedgerV2DriverCommitsOnlyGrowingTotals(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		f := newLedgerV2Fixture(t)
		a := ledgerV2Coldkey(1)
		f.chain.emission[6], f.chain.emission[8], f.chain.emission[9] = 1000, 500, 700
		f.builder.earners[6], f.builder.earners[8], f.builder.earners[9] = [][32]byte{a}, [][32]byte{a}, [][32]byte{a}

		f.chain.head = f.chain.end(5) + 10
		requireLedgerV2Outcome(t, f.run(StLedgerV2StepEpoch, 5, 0, 0), model.StPublishStatusSkipped, false, "the total 0 does not exceed the finalized total 0")
		requireLedgerV2Log(t, f.takeLog(), []string{"close 5"})
		if got := f.publishes(5); !reflect.DeepEqual(got, []string{"v2close confirmed", "v2commit skipped"}) {
			t.Fatalf("epoch 5 publishes %v", got)
		}

		f.chain.head = f.chain.end(6) + 10
		requireLedgerV2Outcome(t, f.run(StLedgerV2StepEpoch, 6, 0, 0), model.StPublishStatusConfirmed, false, "")
		f.chain.approvals = 1
		f.chain.head = f.chain.end(7) + 10
		requireLedgerV2Outcome(t, f.run(StLedgerV2StepEpoch, 7, 0, 0), model.StPublishStatusSkipped, false, "the total 1000 does not exceed the finalized total 1000")
		requireLedgerV2Log(t, f.takeLog(), ledgerV2Join([]string{"close 6", "build 6"}, ledgerV2BuildPuts(f.builder.builds[0]), []string{"commit 6 prev 0", "close 7", "finalize 6"}))

		f.chain.head = f.chain.end(8) + 10
		f.builder.shortTotal = true
		requireLedgerV2Outcome(t, f.run(StLedgerV2StepEpoch, 8, 0, 0), model.StPublishStatusFailed, true, "the built total 1499 differs from fundedThrough 1500")
		requireLedgerV2Log(t, f.takeLog(), []string{"close 8", "build 8"})
		f.builder.shortTotal = false

		delete(f.builder.earners, 9)
		f.chain.head = f.chain.end(9) + 10
		requireLedgerV2Outcome(t, f.run(StLedgerV2StepEpoch, 9, 0, 0), model.StPublishStatusSkipped, false, ErrStLedgerV2NoRoot.Error())
		requireLedgerV2Log(t, f.takeLog(), []string{"close 9", "build 9"})
		if p := f.chain.pending; p != nil {
			t.Fatalf("an unallocated epoch was committed: %+v", p)
		}
	})
}

// SV-1 (alerts): a missed close, a missed commit window, a demand open for
// more than a day and a root unfinalized at end(e) + 14,400 + 1,200, each
// raised once and only when its condition holds.
func TestStLedgerV2DriverRaisesEveryAlert(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		f := newLedgerV2Fixture(t)
		key := f.cfg.DeploymentKey()
		a := ledgerV2Coldkey(1)
		f.chain.emission[6], f.chain.emission[7] = 1000, 1000
		f.builder.earners[6], f.builder.earners[7] = [][32]byte{a}, [][32]byte{a}

		// Epoch 5: nobody closes it. Nothing is alerted while the window is
		// open or while the mirror has not passed the window's end.
		f.chain.head = f.chain.end(5) + f.chain.window
		if steps := ledgerV2Sync(t, f, 6); !slices.Contains(steps, StLedgerV2Step{Kind: StLedgerV2StepEpoch, Epoch: 5}) {
			t.Fatalf("plan %+v", steps)
		}
		f.chain.head++
		model.SetStChainCheckpoint(f.ctx, key, model.StChainCheckpoint{NextBlock: f.chain.end(5) + f.chain.window})
		if steps := ledgerV2Sync(t, f, 6); slices.Contains(steps, StLedgerV2Step{Kind: StLedgerV2StepEpoch, Epoch: 5}) || len(*f.alerts) != 0 {
			t.Fatalf("plan %+v alerts %v before the mirror passed the window", steps, *f.alerts)
		}
		model.SetStChainCheckpoint(f.ctx, key, model.StChainCheckpoint{NextBlock: f.chain.head + 1})
		ledgerV2Sync(t, f, 6)
		ledgerV2Sync(t, f, 6)
		if !reflect.DeepEqual(*f.alerts, []string{StLedgerV2AlertMissedClose + " epoch 5", StLedgerV2AlertMissedCommitWindow + " epoch 5"}) {
			t.Fatalf("alerts %v", *f.alerts)
		}

		// Epoch 6: the close lands but the commit window passes before the
		// commit can be sent.
		*f.alerts = nil
		f.chain.head = f.chain.end(6) + 10
		f.chain.afterClose = f.chain.window
		requireLedgerV2Outcome(t, f.run(StLedgerV2StepEpoch, 6, 0, 0), model.StPublishStatusMissed, false, "commit window closed at block 303600")
		if got := f.publishes(6); !reflect.DeepEqual(got, []string{"v2close confirmed", "v2commit missed"}) || !reflect.DeepEqual(*f.alerts, []string{StLedgerV2AlertMissedCommitWindow + " epoch 6"}) {
			t.Fatalf("epoch 6 publishes %v alerts %v", got, *f.alerts)
		}
		f.chain.afterClose = 0

		// Epoch 7: committed; then a demand older than a day, and the root
		// still pending at end(7) + 14,400 + 1,200.
		*f.alerts = nil
		f.chain.head = f.chain.end(7) + 10
		requireLedgerV2Outcome(t, f.run(StLedgerV2StepEpoch, 7, 0, 0), model.StPublishStatusConfirmed, false, "")
		p := f.chain.pending
		for epoch := range uint64(3) {
			model.SetStLedgerV2Funding(f.ctx, key, &model.StLedgerV2Funding{NoId: 1, Epoch: 5 + epoch, FundedThrough: f.chain.funded[5+epoch], Deferred: epoch == 0, BlockNumber: f.chain.end(5 + epoch)})
		}
		model.AddStLedgerV2Commit(f.ctx, key, &model.StLedgerV2Root{NoId: 1, Seq: p.Seq, Epoch: 7, Root: p.Root, Total: p.Total, LeafCount: p.LeafCount, LedgerHash: p.LedgerHash}, p.CommittedBlock, 0)
		model.AddStLedgerV2Demand(f.ctx, key, &model.StLedgerV2Demand{NoId: 1, Seq: p.Seq, Index: 0, Demander: "0xd1", DemandBlock: p.CommittedBlock + 100})
		model.SetStChainCheckpoint(f.ctx, key, model.StChainCheckpoint{NextBlock: f.chain.end(8) + 1})
		f.chain.head = p.CommittedBlock + 100 + 7_200
		ledgerV2Sync(t, f, 8)
		if len(*f.alerts) != 0 {
			t.Fatalf("alerts %v at exactly one day", *f.alerts)
		}
		f.chain.head++
		ledgerV2Sync(t, f, 8)
		if !reflect.DeepEqual(*f.alerts, []string{StLedgerV2AlertOpenDemand + " seq 1 root 0 index 0"}) {
			t.Fatalf("alerts %v", *f.alerts)
		}
		*f.alerts = nil
		f.chain.head = f.chain.end(7) + 14_400 + 1_200
		ledgerV2Sync(t, f, 8)
		if len(*f.alerts) != 0 {
			t.Fatalf("alerts %v at the finalization deadline", *f.alerts)
		}
		f.chain.head++
		ledgerV2Sync(t, f, 8)
		if !reflect.DeepEqual(*f.alerts, []string{StLedgerV2AlertUnfinalizedRoot + " seq 1"}) {
			t.Fatalf("alerts %v", *f.alerts)
		}

		// A finalized root raises nothing.
		*f.alerts = nil
		stLedgerV2Alerted.Clear()
		index := uint64(0)
		model.SetStLedgerV2RootFinalized(f.ctx, key, &model.StLedgerV2Root{NoId: 1, Seq: p.Seq, Epoch: 7, Root: p.Root, Total: p.Total, LeafCount: p.LeafCount, LedgerHash: p.LedgerHash, RootIndex: &index}, f.chain.head)
		model.SetStLedgerV2DemandAnswered(f.ctx, key, 1, p.Seq, 0, 0, "0xa1", f.chain.head)
		ledgerV2Sync(t, f, 8)
		if len(*f.alerts) != 0 {
			t.Fatalf("alerts %v for a finalized root", *f.alerts)
		}
	})
}

// Finalized roots are rebuilt from their recorded inputs and recorded for
// readers once, in root order; a rebuild that differs is never recorded.
// SV-5 (per-epoch amounts for readers)
func TestStLedgerV2DriverRecordsFinalizedRoots(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		f := newLedgerV2Fixture(t)
		key := f.cfg.DeploymentKey()
		build5, _ := commitTwoRoots(t, f)
		root0 := f.chain.roots[0]
		finalized := &model.StLedgerV2Root{NoId: 1, Seq: 1, Epoch: 5, Root: root0.Root, Total: root0.Total, LeafCount: root0.LeafCount, LedgerHash: root0.LedgerHash}
		model.AddStLedgerV2Commit(f.ctx, key, finalized, f.chain.end(5)+10, 0)
		index := uint64(0)
		finalized.RootIndex = &index
		model.SetStLedgerV2RootFinalized(f.ctx, key, finalized, f.chain.end(6))
		if steps := ledgerV2Sync(t, f, 7); !slices.Contains(steps, StLedgerV2Step{Kind: StLedgerV2StepRecord, Epoch: 5, Seq: 1}) {
			t.Fatalf("plan %+v", steps)
		}

		// A rebuild that differs from the finalized root is not recorded.
		f.chain.funded[5] = *uint256.NewInt(999)
		requireLedgerV2Outcome(t, f.run(StLedgerV2StepRecord, 5, 1, 0), model.StPublishStatusFailed, true, "the built total 999 differs from fundedThrough 1000")
		f.chain.funded[5] = *uint256.NewInt(1000)

		requireLedgerV2Outcome(t, f.run(StLedgerV2StepRecord, 5, 1, 0), model.StPublishStatusConfirmed, false, "root 0 recorded")
		accounts := []*model.StLedgerV2Account{}
		for _, account := range build5.Accounts {
			accounts = append(accounts, model.GetStLedgerV2AccountsForColdkey(f.ctx, key, account.Coldkey)...)
		}
		if len(accounts) != 2 || accounts[0].Index != 0 || accounts[1].Index != 1 || accounts[0].Coldkey != ledgerV2Coldkey(1) || accounts[1].Coldkey != ledgerV2Coldkey(2) {
			t.Fatalf("accounts %+v", accounts)
		}
		amounts := model.GetStLedgerV2Amounts(f.ctx, key, 1, 5)
		if len(amounts) != 2 || amounts[0].Amount.Uint64() != 500 || amounts[1].Amount.Uint64() != 500 || amounts[0].RootIndex != 0 {
			t.Fatalf("amounts %+v", amounts)
		}
		requireLedgerV2Outcome(t, f.run(StLedgerV2StepRecord, 5, 1, 0), model.StPublishStatusSkipped, false, "not an unrecorded finalized root")
		if steps := ledgerV2Sync(t, f, 7); slices.Contains(steps, StLedgerV2Step{Kind: StLedgerV2StepRecord, Epoch: 5, Seq: 1}) {
			t.Fatalf("a recorded root is planned: %+v", steps)
		}

		// Root 1 cannot be recorded without root 0 mirrored first, nor with
		// a covered epoch whose rows are not recorded.
		second := &model.StLedgerV2Root{NoId: 1, Seq: 7, Epoch: 7, Root: root0.Root, Total: *uint256.NewInt(5000), LeafCount: 3, LedgerHash: root0.LedgerHash}
		two := uint64(2)
		second.RootIndex = &two
		model.SetStLedgerV2RootFinalized(f.ctx, key, second, f.chain.end(8))
		requireLedgerV2Outcome(t, f.run(StLedgerV2StepRecord, 7, 7, 0), model.StPublishStatusFailed, true, "root 1 is not mirrored")
		third := &model.StLedgerV2Root{NoId: 1, Seq: 8, Epoch: 7, Root: root0.Root, Total: *uint256.NewInt(5000), LeafCount: 3, LedgerHash: root0.LedgerHash}
		one := uint64(1)
		third.RootIndex = &one
		model.SetStLedgerV2RootFinalized(f.ctx, key, third, f.chain.end(8))
		requireLedgerV2Outcome(t, f.run(StLedgerV2StepRecord, 7, 8, 0), model.StPublishStatusFailed, true, "fundedThrough decreases at epoch 7")
		f.chain.funded[7] = *uint256.NewInt(5000)
		requireLedgerV2Outcome(t, f.run(StLedgerV2StepRecord, 7, 8, 0), model.StPublishStatusFailed, true, "epoch 7 of root 1 has no recorded provider rows")
	})
}

// SV-4: the v1 driver keeps every epoch below the pool's first v2 epoch and
// never closes or commits one at or after it, except the phase C sweep epoch.
func TestStV1DriverStopsAtFirstV2Epoch(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		f := newLedgerV2Fixture(t)
		state := &StEpochState{Epoch: 8, PendingEpoch: 8, EpochStartBlock: 400_000, TEpochBlocks: 50_400, CommitWindowBlocks: 1_200, HeadBlock: 400_010, HeadBlockTime: time.Now().UTC()}
		SetStClient(newStubStClient(state))
		defer SetStClient(nil)

		// Until the registration has been read once its cutoff is unknown; from
		// then on the recorded activation answers without vault2 (SV-9).
		f.chain.failNext("pool", errors.New("vault2 unreadable"))
		if _, err := StV1EpochAllowed(f.ctx, 4); err == nil || err.Error() != "vault2 unreadable" {
			t.Fatalf("unknown registration: %v", err)
		}
		f.chain.failNext("pool", errors.New("vault2 unreadable"))
		if _, err := StCloseOperatorEpoch(f.ctx, 4); err == nil || err.Error() != "vault2 unreadable" {
			t.Fatalf("close with unknown registration: %v", err)
		}

		for _, test := range []struct {
			name       string
			registered bool
			epoch      uint64
			allowed    bool
		}{
			{"unregistered pool", false, 9, true},
			{"before the first epoch", true, 4, true},
			{"the first epoch", true, 5, false},
			{"after the first epoch", true, 6, false},
		} {
			resetLedgerV2Caches()
			f.chain.registered = test.registered
			allowed, err := StV1EpochAllowed(f.ctx, test.epoch)
			if err != nil || allowed != test.allowed {
				t.Fatalf("%s: allowed %t, %v", test.name, allowed, err)
			}
			_, closeErr := StCloseOperatorEpoch(f.ctx, test.epoch)
			_, commitErr := StCommitEpochRoot(f.ctx, test.epoch)
			if refused := errors.Is(closeErr, ErrStV1EpochSettledByV2) && errors.Is(commitErr, ErrStV1EpochSettledByV2); refused == test.allowed {
				t.Fatalf("%s: close %v commit %v", test.name, closeErr, commitErr)
			}
		}

		resetLedgerV2Caches()
		f.chain.registered = true
		sweep := stLedgerV1SweepEpoch
		stLedgerV1SweepEpoch = func(context.Context, *StConfig) (uint64, bool) { return 7, true }
		defer func() { stLedgerV1SweepEpoch = sweep }()
		for epoch, want := range map[uint64]bool{6: false, 7: true, 8: false} {
			if allowed, err := StV1EpochAllowed(f.ctx, epoch); err != nil || allowed != want {
				t.Fatalf("sweep hook: epoch %d allowed %t, %v", epoch, allowed, err)
			}
		}

		// Without v2 every epoch stays with v1.
		SetStConfig(&StConfig{Enabled: true, ChainId: 945, ContractAddress: f.cfg.ContractAddress})
		if allowed, err := StV1EpochAllowed(f.ctx, 50); err != nil || !allowed {
			t.Fatalf("v2 off: %t, %v", allowed, err)
		}
	})
}

// The pool's registration is read until vault2 reports it, then cached; an
// unregistered answer is read again after a minute. The registered answer is
// also recorded (SV-9), so this runs with the database.
// SV-4
func TestStLedgerV2FirstEpochCache(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		f := newLedgerV2Fixture(t)
		f.chain.registered = false
		if _, registered, err := stLedgerV2FirstEpoch(f.ctx, f.cfg, f.chain); err != nil || registered {
			t.Fatalf("registered %t, %v", registered, err)
		}
		f.chain.registered = true
		if _, registered, _ := stLedgerV2FirstEpoch(f.ctx, f.cfg, f.chain); registered {
			t.Fatal("an unregistered answer was read again within a minute")
		}
		stLedgerV2Pools.Range(func(key, value any) bool {
			pool := value.(stLedgerV2Pool)
			pool.checked = pool.checked.Add(-time.Minute)
			stLedgerV2Pools.Store(key, pool)
			return true
		})
		if first, registered, err := stLedgerV2FirstEpoch(f.ctx, f.cfg, f.chain); err != nil || !registered || first != 5 {
			t.Fatalf("after a minute: %d %t %v", first, registered, err)
		}
		f.chain.failNext("pool", errors.New("unreadable"))
		if _, registered, err := stLedgerV2FirstEpoch(f.ctx, f.cfg, f.chain); err != nil || !registered {
			t.Fatalf("a registered pool was read again: %t %v", registered, err)
		}
	})
}
