package controller

// st_ledger_v2_edge_test.go — every refusal and failure branch of the
// settlement v2 driver, each triggered alone: a failure is retried without a
// chain write, and a refusal never publishes or commits what it refused.

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"math/big"
	"reflect"
	"strings"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/holiman/uint256"

	"github.com/urfoundation/sn/merkle"
	"github.com/urfoundation/sn/stabi"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// Steps and sync turns refuse without settlement v2, without a chain client,
// without payout admission, and for an unknown step. Payout admission reads
// the activation record (SV-9), so this runs with the database.
// SV-1
func TestStLedgerV2StepRefusals(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		resetLedgerV2Caches()
		defer resetLedgerV2Caches()
		defer SetStConfig(nil)
		defer SetStClient(nil)
		defer SetStLedgerV2Client(nil)
		step := &StLedgerV2Step{Kind: StLedgerV2StepEpoch, Epoch: 5}

		off := ledgerV2TestConfig()
		off.SettlementV2, off.VaultV2 = common.Address{}, common.Address{}
		SetStConfig(off)
		if _, err := StLedgerV2RunStep(ctx, step); !errors.Is(err, errStLedgerV2NotConfigured) {
			t.Fatalf("v2 off: %v", err)
		}
		if steps, err := StLedgerV2Sync(ctx, &StEpochState{Epoch: 9}); steps != nil || err != nil {
			t.Fatalf("v2 off sync: %v %v", steps, err)
		}

		cfg := ledgerV2TestConfig()
		SetStConfig(cfg)
		SetStClient(newStubStClient(&StEpochState{}))
		if _, err := StLedgerV2RunStep(ctx, step); !errors.Is(err, errStLedgerV2NotConfigured) {
			t.Fatalf("no v2 client: %v", err)
		}
		core := &CoreStClient{cfg: cfg}
		SetStClient(core)
		if client := stLedgerV2Client(); client != core {
			t.Fatalf("the core client does not serve v2: %v", client)
		}
		if _, err := StV1EpochAllowed(ctx, 5); err != nil && !strings.Contains(err.Error(), "vault2 pools(1)") {
			t.Fatalf("v1 cutoff reads vault2 through the core client: %v", err)
		}
		SetStClient(newStubStClient(&StEpochState{}))
		if _, err := StV1EpochAllowed(ctx, 5); !errors.Is(err, errStLedgerV2NotConfigured) {
			t.Fatalf("v1 cutoff without a v2 client: %v", err)
		}

		SetStLedgerV2Client(newFakeLedgerV2Chain(&[]string{}))
		if _, err := StLedgerV2RunStep(ctx, &StLedgerV2Step{Kind: "sweep"}); err == nil || err.Error() != `st ledger v2: unknown step "sweep"` {
			t.Fatalf("unknown step: %v", err)
		}
		mainnet := ledgerV2TestConfig()
		mainnet.Profile = "mainnet"
		SetStConfig(mainnet)
		if _, err := StLedgerV2RunStep(ctx, step); err == nil || err.Error() != "sn: mainnet payout is disabled or has wrong subnet" {
			t.Fatalf("payout admission: %v", err)
		}
		if seconds := stLedgerV2BlockSeconds(&StConfig{}); seconds != stDefaultBlockSeconds {
			t.Fatalf("default block seconds %d", seconds)
		}
	})
}

// The tree cache keeps the four most recent trees and each tree once.
// SV-1 (demand answering)
func TestStLedgerV2TreeCacheKeepsRecentTrees(t *testing.T) {
	resetLedgerV2Caches()
	defer resetLedgerV2Caches()
	for marker := range byte(5) {
		stLedgerV2CacheTree([32]byte{marker}, &stLedgerV2Tree{})
		stLedgerV2CacheTree([32]byte{marker}, &stLedgerV2Tree{})
	}
	if len(stLedgerV2Trees) != stLedgerV2TreeCacheSize || stLedgerV2Trees[[32]byte{0}] != nil || stLedgerV2Trees[[32]byte{4}] == nil {
		t.Fatalf("cached %d trees: %v", len(stLedgerV2Trees), stLedgerV2TreeOrder)
	}
}

// Each failure of the epoch step is retried without a chain write, and each
// refused build is never published or committed.
// SV-1 (retries)
func TestStLedgerV2EpochStepFailures(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		f := newLedgerV2Fixture(t)
		a, b := ledgerV2Coldkey(1), ledgerV2Coldkey(2)
		for epoch := uint64(5); epoch <= 8; epoch++ {
			f.chain.emission[epoch] = 1000
			f.builder.earners[epoch] = [][32]byte{a, b}
		}
		f.chain.head = f.chain.end(5) + 10
		requireLedgerV2Outcome(t, f.run(StLedgerV2StepEpoch, 4, 0, 0), model.StPublishStatusSkipped, false, "epoch 4 is not a v2 epoch of pool 1")

		retried := func(reason string, log ...string) {
			t.Helper()
			requireLedgerV2Outcome(t, f.run(StLedgerV2StepEpoch, 5, 0, 0), model.StPublishStatusFailed, true, reason)
			requireLedgerV2Log(t, f.takeLog(), log)
		}
		f.chain.failNext("window", errors.New("window unreadable"))
		retried("window unreadable")
		f.chain.closeLag = true
		retried("the close of epoch 5 is not visible at block 252010", "close 5")
		f.chain.closeLag = false
		f.chain.fail["state"] = []error{nil, errors.New("state after close")}
		retried("state after close", "close 5")
		f.chain.failNext("funded", errors.New("funded unreadable"))
		retried("funded unreadable")
		f.chain.fail["funded"] = []error{nil, errors.New("funded by epoch unreadable")}
		retried("funded by epoch unreadable")
		SetStLedgerBuilder(nil)
		retried("no ledger builder is installed")
		SetStLedgerBuilder(f.builder)
		for _, refusal := range []struct {
			mutate func(*StLedgerV2Build)
			reason string
		}{
			{func(build *StLedgerV2Build) { build.Accounts = nil }, "merkle: ledger tree needs 1 to 2^29 accounts"},
			{func(build *StLedgerV2Build) { build.Epochs = nil }, "the build lists 0 epochs, not 1"},
			{func(build *StLedgerV2Build) { build.Epochs[0].Epoch = 9 }, "the build lists epoch 9 in place of 5"},
		} {
			f.builder.mutate = refusal.mutate
			retried(refusal.reason, "build 5")
		}
		f.builder.mutate = nil
		SetStLedgerV2ObjectStore(&refusingBlobStore{BlobStore: f.store})
		retried("put refused", "build 5")
		SetStLedgerV2ObjectStore(f.store)
		if records := model.GetStLedgerV2EpochRows(f.ctx, f.cfg.DeploymentKey(), 1, 5, 5); len(records) != 0 || f.chain.pending != nil {
			t.Fatalf("a refused build was recorded %+v or committed %+v", records, f.chain.pending)
		}
		requireLedgerV2Outcome(t, f.run(StLedgerV2StepEpoch, 5, 0, 0), model.StPublishStatusConfirmed, false, "")
		requireLedgerV2Outcome(t, f.run(StLedgerV2StepEpoch, 5, 0, 0), model.StPublishStatusSkipped, false, "a root for epoch 5 is already committed")
		f.takeLog()

		// Epoch 6: the previous root has two accounts.
		f.chain.approvals = 1
		f.chain.head = f.chain.end(6) + 10
		f.chain.failNext("finalize", errors.New("finalize failed"))
		requireLedgerV2Outcome(t, f.run(StLedgerV2StepEpoch, 6, 0, 0), model.StPublishStatusFailed, true, "finalize failed")
		requireLedgerV2Log(t, f.takeLog(), []string{"close 6", "finalize 5"})
		f.chain.fail["state"] = []error{nil, errors.New("state after finalize")}
		requireLedgerV2Outcome(t, f.run(StLedgerV2StepEpoch, 6, 0, 0), model.StPublishStatusFailed, true, "state after finalize")
		requireLedgerV2Log(t, f.takeLog(), []string{"finalize 5"})

		f.chain.fail["funded"] = []error{nil, errors.New("funded before unreadable")}
		requireLedgerV2Outcome(t, f.run(StLedgerV2StepEpoch, 6, 0, 0), model.StPublishStatusFailed, true, "funded before unreadable")
		requireLedgerV2Log(t, f.takeLog(), nil)
		retried6 := func(status string, retry bool, reason string, log ...string) {
			t.Helper()
			requireLedgerV2Outcome(t, f.run(StLedgerV2StepEpoch, 6, 0, 0), status, retry, reason)
			got := f.takeLog()
			if len(got) < len(log) || !reflect.DeepEqual(got[:len(log)], log) || (len(log) != 0 && len(got) > len(log) && !strings.HasPrefix(got[len(log)], "put ")) {
				t.Fatalf("calls %q, want %q then puts", got, log)
			}
		}
		f.builder.mutate = func(build *StLedgerV2Build) {
			merged := build.Accounts[0]
			merged.Cumulative.Add(&merged.Cumulative, &build.Accounts[1].Cumulative)
			build.Accounts = []merkle.LedgerAccount{merged}
		}
		retried6(model.StPublishStatusFailed, true, "the build has 1 accounts, fewer than the previous root's 2", "build 6")
		f.builder.mutate = nil
		f.builder.onBuild = func() { f.chain.roots = append(f.chain.roots, &StLedgerV2FinalizedRoot{Epoch: 5}) }
		retried6(model.StPublishStatusFailed, true, "the root count moved from 1 to 2 during the build", "build 6")
		f.chain.roots = f.chain.roots[:1]
		f.builder.onBuild = func() { f.chain.paused = true }
		retried6(model.StPublishStatusSkipped, true, "settlement is paused", "build 6")
		f.chain.paused = false
		f.builder.onBuild = func() { f.chain.fail["state"] = []error{errors.New("state before commit")} }
		retried6(model.StPublishStatusFailed, true, "state before commit", "build 6")
		f.builder.onBuild = nil
		header := strings.TrimPrefix(startifactRef(f.builder.builds[len(f.builder.builds)-1].Header), "sha256:")
		SetStLedgerV2ObjectStore(&refusingBlobStore{BlobStore: f.store, only: header})
		retried6(model.StPublishStatusFailed, true, "put refused", "build 6")
		SetStLedgerV2ObjectStore(f.store)
		committed := f.chain.lastCommit
		f.chain.lastCommit = f.chain.head - 10
		retried6(model.StPublishStatusSkipped, true, "vault2 accepts the pool's next commit at block 316800", "build 6")
		f.chain.lastCommit = committed
		if f.chain.pending != nil {
			t.Fatalf("a refused epoch 6 root was committed: %+v", f.chain.pending)
		}
		f.chain.failNext("commit", fmt.Errorf("%w: synthetic", ErrStLedgerV2CommitSpent))
		requireLedgerV2Outcome(t, f.run(StLedgerV2StepEpoch, 6, 0, 0), model.StPublishStatusMissed, false, "synthetic")
		if got := f.publishes(6); got[len(got)-1] != "v2commit missed" {
			t.Fatalf("epoch 6 publishes %v", got)
		}

		// Epoch 7: the window passes during the build.
		f.takeLog()
		f.chain.head = f.chain.end(7) + 10
		f.builder.onBuild = func() { f.chain.head = f.chain.end(7) + f.chain.window + 1 }
		requireLedgerV2Outcome(t, f.run(StLedgerV2StepEpoch, 7, 0, 0), model.StPublishStatusMissed, false, "commit window closed at block 354000")
		if !strings.Contains(strings.Join(*f.alerts, ","), StLedgerV2AlertMissedCommitWindow+" epoch 7") {
			t.Fatalf("alerts %v", *f.alerts)
		}

		// Epoch 8: the operator declares the epoch null while it is built.
		f.chain.head = f.chain.end(8) + 10
		f.builder.onBuild = func() {
			if err := model.DeclareStLedgerV2EpochUnrecoverable(f.ctx, f.cfg.DeploymentKey(), 1, 8, "operator@example"); err != nil {
				t.Fatal(err)
			}
		}
		requireLedgerV2Outcome(t, f.run(StLedgerV2StepEpoch, 8, 0, 0), model.StPublishStatusFailed, true, "the epoch's provider rows are already recorded: epoch 8")
		if f.chain.pending != nil {
			t.Fatalf("committed %+v over a declaration", f.chain.pending)
		}
	})
}

func startifactRef(object []byte) string {
	return fmt.Sprintf("sha256:%x", sha256.Sum256(object))
}

// Ledger objects go to the configured blob store unless a test overrides it.
// SV-3 (objects)
func TestStLedgerV2ObjectsUseTheConfiguredStore(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		SetStLedgerV2ObjectStore(nil)
		ref, err := PutStLedgerV2Object(context.Background(), []byte("header"))
		if err != nil {
			t.Fatal(err)
		}
		if object, err := GetStLedgerV2Object(context.Background(), ref); err != nil || string(object) != "header" {
			t.Fatalf("object %q, %v", object, err)
		}
	})
}

// Chain amounts convert to uint256 only when they fit, and so does a
// finalized root's total.
// SV-1
func TestStLedgerV2ChainAmounts(t *testing.T) {
	for _, value := range []*big.Int{nil, big.NewInt(-1), new(big.Int).Lsh(big.NewInt(1), 256)} {
		if _, err := stUint256(value); err == nil {
			t.Fatalf("%v converted", value)
		}
	}
	if value, err := stUint256(big.NewInt(7)); err != nil || value.Uint64() != 7 {
		t.Fatalf("7 converted to %s, %v", value.Dec(), err)
	}
	if root, err := stLedgerV2FinalizedRootOf(2, stabi.ISTSettlementVault2Root{Total: big.NewInt(-1)}); root != nil || err == nil || err.Error() != "st: chain value -1 is not a uint256" {
		t.Fatalf("a negative root total converted: %+v %v", root, err)
	}
	root, err := stLedgerV2FinalizedRootOf(2, stabi.ISTSettlementVault2Root{Root: [32]byte{1}, Total: big.NewInt(9), LeafCount: 3, Epoch: 5, LedgerHash: [32]byte{2}})
	if want := (&StLedgerV2FinalizedRoot{Index: 2, Root: [32]byte{1}, Total: *uint256.NewInt(9), LeafCount: 3, Epoch: 5, LedgerHash: [32]byte{2}}); err != nil || !reflect.DeepEqual(root, want) {
		t.Fatalf("root %+v %v", root, err)
	}
}

// A root never covers more than 96 epochs.
// SV-1 (§6.6 bound)
func TestStLedgerV2RootCoversAtMost96Epochs(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		f := newLedgerV2Fixture(t)
		f.chain.emission[101] = 1000
		f.chain.head = f.chain.end(101) + 10
		requireLedgerV2Outcome(t, f.run(StLedgerV2StepEpoch, 101, 0, 0), model.StPublishStatusFailed, true, "a root for epoch 101 cannot cover epochs 5 to 101")
		if f.chain.lastFunded != 101 || f.chain.pending != nil {
			t.Fatalf("funded through %d, pending %+v", f.chain.lastFunded, f.chain.pending)
		}
	})
}

// Each failure of the finalize and answer steps is retried without a send.
// SV-1 (retries)
func TestStLedgerV2FinalizeAndAnswerFailures(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		f := newLedgerV2Fixture(t)
		build5, build6 := commitTwoRoots(t, f)
		hash5, hash6 := sha256.Sum256(build5.Header), sha256.Sum256(build6.Header)
		f.chain.failNext("state", errors.New("state unreadable"))
		requireLedgerV2Outcome(t, f.run(StLedgerV2StepFinalize, 6, 0, 0), model.StPublishStatusFailed, true, "state unreadable")
		f.chain.head = f.chain.pending.CommittedBlock + f.chain.challenge
		f.chain.failNext("window", errors.New("window unreadable"))
		requireLedgerV2Outcome(t, f.run(StLedgerV2StepFinalize, 6, 0, 0), model.StPublishStatusFailed, true, "window unreadable")

		f.chain.demand(1)
		f.chain.failNext("state", errors.New("state unreadable"))
		requireLedgerV2Outcome(t, f.run(StLedgerV2StepAnswer, 6, 2, 1), model.StPublishStatusFailed, true, "state unreadable")
		f.chain.failNext("demand", errors.New("demand unreadable"))
		requireLedgerV2Outcome(t, f.run(StLedgerV2StepAnswer, 6, 2, 1), model.StPublishStatusFailed, true, "demand unreadable")
		f.chain.demands[[3]uint64{2, 0, 5}] = StLedgerV2DemandOpen
		requireLedgerV2Outcome(t, f.run(StLedgerV2StepAnswer, 6, 2, 5), model.StPublishStatusFailed, true, "index 5 is outside the finalized root's 2 accounts")

		resetLedgerV2Caches()
		SetStLedgerBuilder(nil)
		requireLedgerV2Outcome(t, f.run(StLedgerV2StepAnswer, 6, 2, 1), model.StPublishStatusFailed, true, "no ledger builder is installed")
		SetStLedgerBuilder(f.builder)
		pending := f.builder.ledgers[hash6]
		delete(f.builder.ledgers, hash6)
		requireLedgerV2Outcome(t, f.run(StLedgerV2StepAnswer, 6, 2, 1), model.StPublishStatusFailed, true, "fake builder: unknown ledger")
		f.builder.ledgers[hash6] = pending
		finalized := f.builder.ledgers[hash5]
		f.builder.ledgers[hash5] = nil
		resetLedgerV2Caches()
		requireLedgerV2Outcome(t, f.run(StLedgerV2StepAnswer, 6, 2, 1), model.StPublishStatusFailed, true, "merkle: ledger tree needs 1 to 2^29 accounts")
		f.builder.ledgers[hash5] = finalized
		if log := f.takeLog(); strings.Contains(strings.Join(log, ","), "answer") || strings.Contains(strings.Join(log, ","), "finalize") {
			t.Fatalf("a failed step sent: %v", log)
		}
		requireLedgerV2Outcome(t, f.run(StLedgerV2StepAnswer, 6, 2, 1), model.StPublishStatusConfirmed, false, "")
	})
}

// Each failure of the record step is retried and writes nothing; roots are
// recorded in root order, the second appending only its new accounts.
// SV-5
func TestStLedgerV2RecordFailures(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		f := newLedgerV2Fixture(t)
		key := f.cfg.DeploymentKey()
		commitTwoRoots(t, f)
		f.chain.head = f.chain.pending.CommittedBlock + f.chain.challenge
		f.chain.approvals = 1
		f.run(StLedgerV2StepFinalize, 6, 0, 0)
		mirror := func(seq uint64, index uint64, root *StLedgerV2FinalizedRoot) {
			finalized := &model.StLedgerV2Root{NoId: 1, Seq: seq, Epoch: root.Epoch, Root: root.Root, Total: root.Total, LeafCount: root.LeafCount, LedgerHash: root.LedgerHash, RootIndex: &index}
			model.SetStLedgerV2RootFinalized(f.ctx, key, finalized, f.chain.head)
		}
		mirror(1, 0, f.chain.roots[0])
		mirror(2, 1, f.chain.roots[1])

		retried := func(seq uint64, reason string) {
			t.Helper()
			requireLedgerV2Outcome(t, f.run(StLedgerV2StepRecord, 0, seq, 0), model.StPublishStatusFailed, true, reason)
			if count := model.GetStLedgerV2AccountCount(f.ctx, key, 1); count != 0 {
				t.Fatalf("%s wrote %d accounts", reason, count)
			}
		}
		resetLedgerV2Caches()
		f.chain.failNext("pool", errors.New("pool unreadable"))
		retried(1, "pool unreadable")
		f.chain.failNext("funded", errors.New("funded unreadable"))
		retried(1, "funded unreadable")
		SetStLedgerBuilder(nil)
		retried(1, "no ledger builder is installed")
		SetStLedgerBuilder(f.builder)
		f.builder.fail = errors.New("rows unreadable")
		retried(1, "rows unreadable")
		f.builder.mutate = func(build *StLedgerV2Build) {
			build.Accounts[0], build.Accounts[1] = build.Accounts[1], build.Accounts[0]
		}
		retried(1, "the rebuilt root 0 differs from the finalized one")
		f.builder.mutate = nil
		retried(2, "an earlier root of pool 1 is not recorded")

		requireLedgerV2Outcome(t, f.run(StLedgerV2StepRecord, 0, 1, 0), model.StPublishStatusConfirmed, false, "root 0 recorded")
		requireLedgerV2Outcome(t, f.run(StLedgerV2StepRecord, 0, 2, 0), model.StPublishStatusConfirmed, false, "root 1 recorded")
		accounts := model.GetStLedgerV2AccountsForColdkey(f.ctx, key, ledgerV2Coldkey(3))
		if count := model.GetStLedgerV2AccountCount(f.ctx, key, 1); count != 3 || len(accounts) != 1 || accounts[0].Index != 2 || accounts[0].RootIndex != 1 {
			t.Fatalf("%d accounts, coldkey 3 %+v", count, accounts)
		}
		amounts := model.GetStLedgerV2Amounts(f.ctx, key, 1, 6)
		if len(amounts) != 2 || amounts[0].Coldkey != ledgerV2Coldkey(2) || amounts[0].Amount.Uint64() != 1500 || amounts[1].Amount.Uint64() != 1500 || amounts[1].RootIndex != 1 {
			t.Fatalf("epoch 6 amounts %+v", amounts)
		}
	})
}

// A sync turn that cannot read the pool or an epoch window plans nothing.
// SV-1
func TestStLedgerV2SyncFailures(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		f := newLedgerV2Fixture(t)
		state := &StEpochState{Epoch: 8, HeadBlock: f.chain.end(8) + 10}
		f.chain.failNext("pool", errors.New("pool unreadable"))
		if steps, err := StLedgerV2Sync(f.ctx, state); err == nil || err.Error() != "pool unreadable" || steps != nil {
			t.Fatalf("pool failure: %v %v", steps, err)
		}
		f.chain.failNext("window", errors.New("window unreadable"))
		if steps, err := StLedgerV2Sync(f.ctx, state); err == nil || err.Error() != "window unreadable" || steps != nil {
			t.Fatalf("window failure: %v %v", steps, err)
		}
		root := &model.StLedgerV2Root{NoId: 1, Seq: 1, Epoch: 6, Total: *uint256.NewInt(1)}
		model.AddStLedgerV2Commit(f.ctx, f.cfg.DeploymentKey(), root, f.chain.end(6)+10, 0)
		f.chain.fail["window"] = []error{nil, errors.New("root window unreadable")}
		if steps, err := StLedgerV2Sync(f.ctx, state); err == nil || err.Error() != "root window unreadable" || steps != nil {
			t.Fatalf("root window failure: %v %v", steps, err)
		}
	})
}

// Readers get a committed ledger's tree and accounts, checked against the
// committed root.
// SV-3 (account proofs)
func TestStLedgerV2TreeForReaders(t *testing.T) {
	resetLedgerV2Caches()
	defer resetLedgerV2Caches()
	builder := newFakeLedgerBuilder(&[]string{})
	SetStLedgerBuilder(builder)
	defer SetStLedgerBuilder(nil)
	accounts := ledgerV2Accounts(ledgerV2Coldkey(1), 500, ledgerV2Coldkey(2), 700)
	builder.ledgers[[32]byte{7}] = accounts
	built, err := merkle.BuildLedgerSumTree(context.Background(), accounts)
	if err != nil {
		t.Fatal(err)
	}
	root, total := built.Root()
	tree, got, err := StLedgerV2Tree(context.Background(), [32]byte{7}, root, &total, 2)
	if err != nil || !reflect.DeepEqual(got, accounts) || tree.LeafCount() != 2 {
		t.Fatalf("tree %v accounts %v, %v", tree, got, err)
	}
	if _, _, err := StLedgerV2Tree(context.Background(), [32]byte{7}, root, &total, 3); err == nil || !strings.Contains(err.Error(), "do not rebuild its root") {
		t.Fatalf("a different account count: %v", err)
	}
	if _, _, err := StLedgerV2Tree(context.Background(), [32]byte{8}, root, &total, 2); err == nil || err.Error() != "fake builder: unknown ledger" {
		t.Fatalf("an unknown ledger: %v", err)
	}
}
