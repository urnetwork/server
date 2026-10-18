package controller

// st_ledger_v2_activation_test.go — the settlement v2 activation latch (SV-9)
// with the real database: the first observation of vault2's registration of
// the pool is recorded, and from then on the deployment's st subsystem runs
// only with the recorded v2 pair. The tests from the corrected settlement on
// carry the independent review's probes (v2/launch-latch-review 06c0bf7c).

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/ethereum/go-ethereum/common"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// The refusals name the activation the fixture's chain registers: pool 1 from
// epoch 5, under the fixture's vault2 and STSettlement2.
const ledgerV2ActivationRefusal = "st: settlement v2 is active from epoch 5 (pool 1), but st.yml "

// ledgerV2ActivationStClient is the v1 chain of the latch tests. It records
// each mirror range it is asked for and answers it with events.
type ledgerV2ActivationStClient struct {
	*stubStClient
	ranges [][2]uint64
	events []*model.StChainEvent
}

func (self *ledgerV2ActivationStClient) SyncEvents(_ context.Context, fromBlock uint64, toBlock uint64) ([]*model.StChainEvent, uint64, error) {
	self.ranges = append(self.ranges, [2]uint64{fromBlock, toBlock})
	return self.events, toBlock + 1, nil
}

// The driver fixture with a v1 chain finalized at block 1,500 and the event
// mirror's checkpoint at block 1,000.
func newLedgerV2ActivationFixture(t testing.TB) (*ledgerV2Fixture, *ledgerV2ActivationStClient) {
	f := newLedgerV2Fixture(t)
	v1 := &ledgerV2ActivationStClient{stubStClient: newStubStClient(&StEpochState{Epoch: 6, PendingEpoch: 6, HeadBlock: 1_500, HeadBlockTime: server.NowUtc()})}
	SetStClient(v1)
	t.Cleanup(func() { SetStClient(nil) })
	model.SetStChainCheckpoint(f.ctx, f.cfg.DeploymentKey(), model.StChainCheckpoint{NextBlock: 1_000})
	return f, v1
}

// restart starts the server again under cfg, with empty in-process caches.
func (self *ledgerV2Fixture) restart(cfg *StConfig) {
	resetLedgerV2Caches()
	SetStConfig(cfg)
}

// observeLedgerV2Activation has the v1 cutoff read the pool's registration,
// which records the activation, and returns the record.
func observeLedgerV2Activation(t testing.TB, f *ledgerV2Fixture) *model.StLedgerV2Activation {
	t.Helper()
	if allowed, err := StV1EpochAllowed(f.ctx, 5); err != nil || allowed {
		t.Fatalf("epoch 5 of the registered pool: allowed %t, %v", allowed, err)
	}
	activation := model.GetStLedgerV2Activation(f.ctx, f.cfg.DeploymentKey())
	if activation == nil || activation.NoId != 1 || activation.FirstEpoch != 5 || activation.VaultV2Address != testLedgerV2Vault ||
		activation.SettlementV2Address != testLedgerV2Settlement || activation.CreateTime.IsZero() {
		t.Fatalf("recorded activation %+v", activation)
	}
	return activation
}

func requireLedgerV2Activated(t testing.TB, name string, err error, want string) {
	t.Helper()
	if !errors.Is(err, ErrStLedgerV2Activated) || err.Error() != want {
		t.Fatalf("%s: %v\nwant %s", name, err, want)
	}
}

// ledgerV2Recovered runs a call and returns what it panicked with, or nil.
func ledgerV2Recovered(run func()) (recovered any) {
	defer func() { recovered = recover() }()
	run()
	return nil
}

func ledgerV2Exec(t testing.TB, f *ledgerV2Fixture, sql string) {
	t.Helper()
	server.Tx(f.ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(f.ctx, sql))
	})
}

// SV-9 (record): the first read of vault2's registration of the pool records
// the deployment's activation with the configured pair; an unregistered pool
// records nothing, and reading the registration again after a restart, or
// recording the same activation again, changes nothing.
func TestStLedgerV2ActivationIsRecordedOnRegistration(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		f := newLedgerV2Fixture(t)
		key := f.cfg.DeploymentKey()
		f.chain.registered = false
		if allowed, err := StV1EpochAllowed(f.ctx, 5); err != nil || !allowed {
			t.Fatalf("unregistered pool: allowed %t, %v", allowed, err)
		}
		if activation := model.GetStLedgerV2Activation(f.ctx, key); activation != nil {
			t.Fatalf("an unregistered pool was recorded: %+v", activation)
		}

		f.restart(f.cfg)
		f.chain.registered = true
		recorded := observeLedgerV2Activation(t, f)

		f.restart(f.cfg)
		if first, registered, err := stLedgerV2FirstEpoch(f.ctx, f.cfg, f.chain); err != nil || !registered || first != 5 {
			t.Fatalf("after a restart: first epoch %d, registered %t, %v", first, registered, err)
		}
		repeat := *recorded
		if err := model.RecordStLedgerV2Activation(f.ctx, key, &repeat); err != nil {
			t.Fatalf("recording the same activation again: %v", err)
		}
		if again := model.GetStLedgerV2Activation(f.ctx, key); !reflect.DeepEqual(again, recorded) {
			t.Fatalf("a repeated observation changed %+v to %+v", recorded, again)
		}
	})
}

// SV-9 (missing pair): after the activation is recorded, a restart without the
// v2 pair refuses v1's close and commit of the first v2 epoch, the sync turn,
// the event mirror, whose checkpoint stays, and payout admission, which
// readiness reports; no v2 step runs.
func TestStLedgerV2ActivationRefusesAMissingPair(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		f, v1 := newLedgerV2ActivationFixture(t)
		observeLedgerV2Activation(t, f)
		off := *f.cfg
		off.SettlementV2, off.VaultV2 = common.Address{}, common.Address{}
		f.restart(&off)
		want := ledgerV2ActivationRefusal + "is missing the recorded settlement_v2_address " + testLedgerV2Settlement +
			" and vault_v2_address " + testLedgerV2Vault + " (st subsystem disabled)"

		_, err := StCloseOperatorEpoch(f.ctx, 5)
		requireLedgerV2Activated(t, "v1 close of epoch 5", err, want)
		_, err = StCommitEpochRoot(f.ctx, 5)
		requireLedgerV2Activated(t, "v1 commit of epoch 5", err, want)
		_, err = StSyncChainState(f.ctx)
		requireLedgerV2Activated(t, "sync turn", err, want)
		_, err = StSyncChainEvents(f.ctx, 0)
		requireLedgerV2Activated(t, "mirror turn", err, want)
		if checkpoint := model.GetStChainCheckpoint(f.ctx, off.DeploymentKey()); checkpoint.NextBlock != 1_000 || len(v1.ranges) != 0 {
			t.Fatalf("the mirror moved to %+v, reading %v", checkpoint, v1.ranges)
		}
		status, err := GetProviderPayoutTransitionStatus(f.ctx)
		if err != nil || status.ReadinessReason != want || status.NewMainnetWritesAdmitted {
			t.Fatalf("readiness %+v, %v", status, err)
		}
		if _, err := StLedgerV2RunStep(f.ctx, &StLedgerV2Step{Kind: StLedgerV2StepEpoch, Epoch: 5}); !errors.Is(err, errStLedgerV2NotConfigured) {
			t.Fatalf("a v2 step without the pair: %v", err)
		}
		if steps, err := StLedgerV2Sync(f.ctx, &StEpochState{Epoch: 6, HeadBlock: f.chain.end(5) + 10}); steps != nil || err != nil {
			t.Fatalf("v2 steps planned without the pair: %v, %v", steps, err)
		}
		if v1.commitCount != 0 || v1.rollCount != 0 {
			t.Fatalf("v1 wrote chain: %d commits, %d rolls", v1.commitCount, v1.rollCount)
		}
		requireLedgerV2Log(t, f.takeLog(), nil)
	})
}

// SV-9 (changed pair): a restart naming another STSettlement2, vault2 or both
// refuses the sync turn, the event mirror, v1's close of the first v2 epoch
// and every v2 step, naming each changed key with its recorded value.
func TestStLedgerV2ActivationRefusesChangedAddresses(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		f, v1 := newLedgerV2ActivationFixture(t)
		recorded := observeLedgerV2Activation(t, f)
		settlement, vault := common.HexToAddress(testLedgerV2Settlement), common.HexToAddress(testLedgerV2Vault)
		otherSettlement, otherVault := common.HexToAddress("0x00000000000000000000000000000000000000b1"), common.HexToAddress("0x00000000000000000000000000000000000000b2")
		for _, test := range []struct {
			name              string
			settlement, vault common.Address
			recorded          string
		}{
			{"STSettlement2", otherSettlement, vault, "settlement_v2_address " + testLedgerV2Settlement},
			{"vault2", settlement, otherVault, "vault_v2_address " + testLedgerV2Vault},
			{"both", otherSettlement, otherVault, "settlement_v2_address " + testLedgerV2Settlement + " and vault_v2_address " + testLedgerV2Vault},
		} {
			changed := *f.cfg
			changed.SettlementV2, changed.VaultV2 = test.settlement, test.vault
			f.restart(&changed)
			want := ledgerV2ActivationRefusal + "differs from the recorded " + test.recorded + " (st subsystem disabled)"
			_, err := StSyncChainState(f.ctx)
			requireLedgerV2Activated(t, test.name+": sync turn", err, want)
			_, err = StSyncChainEvents(f.ctx, 0)
			requireLedgerV2Activated(t, test.name+": mirror turn", err, want)
			_, err = StCloseOperatorEpoch(f.ctx, 5)
			requireLedgerV2Activated(t, test.name+": v1 close of epoch 5", err, want)
			_, err = StLedgerV2RunStep(f.ctx, &StLedgerV2Step{Kind: StLedgerV2StepEpoch, Epoch: 5})
			requireLedgerV2Activated(t, test.name+": v2 step", err, want)
		}
		if checkpoint := model.GetStChainCheckpoint(f.ctx, f.cfg.DeploymentKey()); checkpoint.NextBlock != 1_000 || len(v1.ranges) != 0 {
			t.Fatalf("the mirror moved to %+v, reading %v", checkpoint, v1.ranges)
		}
		if again := model.GetStLedgerV2Activation(f.ctx, f.cfg.DeploymentKey()); !reflect.DeepEqual(again, recorded) {
			t.Fatalf("the record changed from %+v to %+v", recorded, again)
		}
		requireLedgerV2Log(t, f.takeLog(), nil)
	})
}

// SV-9 (restored pair): restoring the v2 pair resumes everything. The mirror
// reads on from its checkpoint and applies the v2 logs it missed, v1 keeps
// only the epochs below the first v2 epoch, and the v2 driver runs.
func TestStLedgerV2ActivationResumesWhenThePairIsRestored(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		f, v1 := newLedgerV2ActivationFixture(t)
		key := f.cfg.DeploymentKey()
		observeLedgerV2Activation(t, f)
		off := *f.cfg
		off.SettlementV2, off.VaultV2 = common.Address{}, common.Address{}
		f.restart(&off)
		if _, err := StSyncChainEvents(f.ctx, 0); !errors.Is(err, ErrStLedgerV2Activated) {
			t.Fatalf("mirror turn without the pair: %v", err)
		}

		canonical := [32]byte{1}
		captured := ledgerV2MirrorEvent(t, 1_200, 0, "V2EmissionCaptured", map[string]any{"e": "5", "no_id": "1", "observed": "1001", "amount": "1000", "funded_through": "1000"})
		captured.BlockHash = common.BytesToHash(canonical[:]).Hex()
		v1.events = []*model.StChainEvent{captured}
		f.restart(f.cfg)
		if _, err := StSyncChainState(f.ctx); err != nil {
			t.Fatalf("sync turn with the pair restored: %v", err)
		}
		if synced, err := StSyncChainEvents(f.ctx, 0); err != nil || synced != 1 {
			t.Fatalf("mirror turn with the pair restored: %d events, %v", synced, err)
		}
		if !reflect.DeepEqual(v1.ranges, [][2]uint64{{1_000, 1_500}}) {
			t.Fatalf("mirror ranges %v, want the range from the checkpoint", v1.ranges)
		}
		if funding := model.GetStLedgerV2Funding(f.ctx, key, 1, 5); funding == nil || funding.FundedThrough.Uint64() != 1000 || funding.Amount.Uint64() != 1000 || funding.BlockNumber != 1_200 {
			t.Fatalf("mirrored funding %+v", funding)
		}
		if checkpoint := model.GetStChainCheckpoint(f.ctx, key); checkpoint.NextBlock != 1_501 {
			t.Fatalf("checkpoint %+v", checkpoint)
		}

		if allowed, err := StV1EpochAllowed(f.ctx, 4); err != nil || !allowed {
			t.Fatalf("v1 epoch 4: allowed %t, %v", allowed, err)
		}
		if _, err := StCloseOperatorEpoch(f.ctx, 5); !errors.Is(err, ErrStV1EpochSettledByV2) {
			t.Fatalf("v1 close of epoch 5: %v", err)
		}
		f.chain.emission[5], f.builder.earners[5] = 1000, [][32]byte{ledgerV2Coldkey(1)}
		f.chain.head = f.chain.end(5) + 10
		requireLedgerV2Outcome(t, f.run(StLedgerV2StepEpoch, 5, 0, 0), model.StPublishStatusConfirmed, false, "")
		if got := f.publishes(5); !reflect.DeepEqual(got, []string{"v2close confirmed", "v2commit confirmed"}) {
			t.Fatalf("epoch 5 publishes %v", got)
		}
	})
}

// SV-9 (not registered): until vault2 registers the pool nothing is recorded,
// and the configured pair and its removal both behave as before: v1 closes
// any epoch, the sync turn and the mirror run, and readiness has no reason.
// A configuration naming no deployment is checked against nothing.
func TestStLedgerV2ActivationChangesNothingBeforeRegistration(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		f, v1 := newLedgerV2ActivationFixture(t)
		f.chain.registered = false
		off := *f.cfg
		off.SettlementV2, off.VaultV2 = common.Address{}, common.Address{}
		for _, cfg := range []*StConfig{f.cfg, &off} {
			f.restart(cfg)
			if allowed, err := StV1EpochAllowed(f.ctx, 9); err != nil || !allowed {
				t.Fatalf("v2 %t: v1 epoch 9 allowed %t, %v", cfg.LedgerV2Enabled(), allowed, err)
			}
			if outcome, err := StCloseOperatorEpoch(f.ctx, 9); err != nil || outcome.Status != model.StPublishStatusConfirmed || outcome.TxHash != "0xstubclose" {
				t.Fatalf("v2 %t: v1 close of epoch 9: %+v, %v", cfg.LedgerV2Enabled(), outcome, err)
			}
			if _, err := StSyncChainState(f.ctx); err != nil {
				t.Fatalf("v2 %t: sync turn: %v", cfg.LedgerV2Enabled(), err)
			}
			if _, err := StSyncChainEvents(f.ctx, 0); err != nil {
				t.Fatalf("v2 %t: mirror turn: %v", cfg.LedgerV2Enabled(), err)
			}
			if status, err := GetProviderPayoutTransitionStatus(f.ctx); err != nil || status.ReadinessReason != "" {
				t.Fatalf("v2 %t: readiness %+v, %v", cfg.LedgerV2Enabled(), status, err)
			}
		}
		if !reflect.DeepEqual(v1.ranges, [][2]uint64{{1_000, 1_500}}) {
			t.Fatalf("mirror ranges %v", v1.ranges)
		}
		if checkpoint := model.GetStChainCheckpoint(f.ctx, f.cfg.DeploymentKey()); checkpoint.NextBlock != 1_501 {
			t.Fatalf("checkpoint %+v", checkpoint)
		}
		if activation := model.GetStLedgerV2Activation(f.ctx, f.cfg.DeploymentKey()); activation != nil {
			t.Fatalf("an unregistered pool was recorded: %+v", activation)
		}

		// A configuration naming no deployment has no activation to check.
		f.restart(&StConfig{Profile: "testnet"})
		if status, err := GetProviderPayoutTransitionStatus(f.ctx); err != nil || status.ReadinessReason != "" {
			t.Fatalf("no deployment: readiness %+v, %v", status, err)
		}
	})
}

// SV-9 (conflicting record): a later read of the registration that differs
// from the recorded activation is refused, so the v2 driver waits; the record
// stays as it was, and v1 keeps the recorded cutoff.
func TestStLedgerV2ActivationRefusesAConflictingRecord(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		f := newLedgerV2Fixture(t)
		recorded := observeLedgerV2Activation(t, f)
		f.restart(f.cfg)
		f.chain.firstEpoch = 6
		const want = "st ledger v2: the deployment's activation is already recorded: pool 1 from epoch 5 with vault_v2_address " +
			testLedgerV2Vault + " and settlement_v2_address " + testLedgerV2Settlement
		if _, _, err := stLedgerV2FirstEpoch(f.ctx, f.cfg, f.chain); !errors.Is(err, model.ErrStLedgerV2ActivationRecorded) || err.Error() != want {
			t.Fatalf("the registration read: %v", err)
		}
		if steps, err := StLedgerV2Sync(f.ctx, &StEpochState{Epoch: 7, HeadBlock: f.chain.end(6) + 10}); steps != nil || err == nil || err.Error() != want {
			t.Fatalf("v2 sync: %v, %v", steps, err)
		}
		if again := model.GetStLedgerV2Activation(f.ctx, f.cfg.DeploymentKey()); !reflect.DeepEqual(again, recorded) {
			t.Fatalf("the record changed from %+v to %+v", recorded, again)
		}
		for epoch, want := range map[uint64]bool{4: true, 5: false} {
			if allowed, err := StV1EpochAllowed(f.ctx, epoch); err != nil || allowed != want {
				t.Fatalf("v1 epoch %d: allowed %t, %v", epoch, allowed, err)
			}
		}
	})
}

// SV-9 (wrong settlement): vault2.pools answers the same whatever STSettlement2
// st.yml names, so a registration read under one that is not vault2's
// coordinator, or with the coordinator unreadable, is refused and records
// nothing. The corrected st.yml is then admitted and records the activation.
func TestStLedgerV2ActivationAdmitsTheCorrectedSettlement(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		f, _ := newLedgerV2ActivationFixture(t)
		key := f.cfg.DeploymentKey()
		mistyped := *f.cfg
		mistyped.SettlementV2 = common.HexToAddress("0x00000000000000000000000000000000000000ff")
		f.restart(&mistyped)
		const want = "st ledger v2: st.yml's settlement_v2_address is not vault2's coordinator: vault2 " + testLedgerV2Vault + " is bound to " +
			testLedgerV2Settlement + ", not 0x00000000000000000000000000000000000000ff; the pool's activation is not recorded"
		if _, err := StV1EpochAllowed(f.ctx, 5); !errors.Is(err, ErrStLedgerV2Coordinator) || err.Error() != want {
			t.Fatalf("v1 cutoff under the mistyped settlement: %v", err)
		}
		if steps, err := StLedgerV2Sync(f.ctx, &StEpochState{Epoch: 6, HeadBlock: f.chain.end(5) + 10}); steps != nil || err == nil || err.Error() != want {
			t.Fatalf("v2 sync under the mistyped settlement: %v, %v", steps, err)
		}
		if activation := model.GetStLedgerV2Activation(f.ctx, key); activation != nil {
			t.Fatalf("the mistyped settlement was recorded: %+v", activation)
		}

		// The operator corrects st.yml to the STSettlement2 the vault is bound to.
		f.restart(f.cfg)
		if _, err := StSyncChainState(f.ctx); err != nil {
			t.Fatalf("the corrected pair is refused: %v", err)
		}
		if _, err := StSyncChainEvents(f.ctx, 0); err != nil {
			t.Fatalf("the corrected pair's mirror is refused: %v", err)
		}
		f.chain.failNext("coordinator", errors.New("coordinator unreadable"))
		if _, err := StV1EpochAllowed(f.ctx, 5); err == nil || err.Error() != "coordinator unreadable" {
			t.Fatalf("v1 cutoff with the coordinator unreadable: %v", err)
		}
		if activation := model.GetStLedgerV2Activation(f.ctx, key); activation != nil {
			t.Fatalf("recorded without reading the coordinator: %+v", activation)
		}
		observeLedgerV2Activation(t, f)
	})
}

// SV-9 (v1 cutoff): once the activation is recorded its first epoch is the v1
// cutoff whatever vault2 is read to answer. After a restart, neither a read
// that lags behind the registration, nor its cached answer, nor a failing read
// returns the first v2 epoch to v1.
func TestStLedgerV2ActivationHoldsTheV1Cutoff(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		f, _ := newLedgerV2ActivationFixture(t)
		observeLedgerV2Activation(t, f)
		f.restart(f.cfg)
		f.chain.registered = false
		if _, registered, err := stLedgerV2FirstEpoch(f.ctx, f.cfg, f.chain); err != nil || registered {
			t.Fatalf("the lagging read: registered %t, %v", registered, err)
		}
		if _, err := StCloseOperatorEpoch(f.ctx, 5); !errors.Is(err, ErrStV1EpochSettledByV2) {
			t.Fatalf("v1 close of epoch 5, the recorded first v2 epoch: %v", err)
		}
		if _, err := StCommitEpochRoot(f.ctx, 5); !errors.Is(err, ErrStV1EpochSettledByV2) {
			t.Fatalf("v1 commit of epoch 5, the recorded first v2 epoch: %v", err)
		}
		f.restart(f.cfg)
		f.chain.failNext("pool", errors.New("vault2 unreadable"))
		for epoch, want := range map[uint64]bool{4: true, 5: false, 6: false} {
			if allowed, err := StV1EpochAllowed(f.ctx, epoch); err != nil || allowed != want {
				t.Fatalf("v1 epoch %d with vault2 unreadable: allowed %t, %v", epoch, allowed, err)
			}
		}
	})
}

// SV-9 (not armed): the latch arms at the first successful read of the pool's
// registration, and its status shows whether it has. While that read fails
// nothing is recorded, the status is not armed, and a restart without the pair
// is not refused: the residual the status lets the operator check for before
// the pair is ever removed. Once armed, the status carries the record, and the
// refusal of an st.yml that does not name its pair.
func TestStLedgerV2ActivationShowsWhetherItIsArmed(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		f, _ := newLedgerV2ActivationFixture(t)
		key := string(f.cfg.DeploymentKey())
		off := *f.cfg
		off.SettlementV2, off.VaultV2 = common.Address{}, common.Address{}
		unarmed := &StLedgerV2ActivationStatus{DeploymentKey: key}
		requireStatus := func(name string, want *StLedgerV2ActivationStatus) {
			t.Helper()
			if status := GetStLedgerV2ActivationStatus(f.ctx); !reflect.DeepEqual(status, want) {
				t.Fatalf("%s: status %+v, want %+v", name, status, want)
			}
			if transition, err := GetProviderPayoutTransitionStatus(f.ctx); err != nil || !reflect.DeepEqual(transition.LedgerV2Activation, want) || transition.ReadinessReason != want.Refusal {
				t.Fatalf("%s: readiness %+v, %v", name, transition, err)
			}
		}

		unavailable := errors.New("rpc unavailable")
		f.chain.fail["pool"] = []error{unavailable, unavailable}
		if _, err := StV1EpochAllowed(f.ctx, 5); !errors.Is(err, unavailable) {
			t.Fatalf("the registration read: %v", err)
		}
		if steps, err := StLedgerV2Sync(f.ctx, &StEpochState{Epoch: 6, HeadBlock: f.chain.end(5) + 10}); steps != nil || !errors.Is(err, unavailable) {
			t.Fatalf("v2 sync: %v, %v", steps, err)
		}
		requireStatus("registration unread", unarmed)
		f.restart(&off)
		requireStatus("registration unread, pair removed", unarmed)
		if outcome, err := StCloseOperatorEpoch(f.ctx, 5); err != nil || outcome.Status != model.StPublishStatusConfirmed {
			t.Fatalf("v1 close of epoch 5 while the latch is not armed: %+v, %v", outcome, err)
		}

		f.restart(f.cfg)
		recorded := observeLedgerV2Activation(t, f)
		armed := &StLedgerV2ActivationStatus{DeploymentKey: key, Armed: true, NoId: 1, FirstEpoch: 5,
			VaultV2Address: testLedgerV2Vault, SettlementV2Address: testLedgerV2Settlement, RecordedAt: &recorded.CreateTime}
		requireStatus("registration read", armed)
		f.restart(&off)
		refusing := *armed
		refusing.Refusal = ledgerV2ActivationRefusal + "is missing the recorded settlement_v2_address " + testLedgerV2Settlement +
			" and vault_v2_address " + testLedgerV2Vault + " (st subsystem disabled)"
		requireStatus("pair removed", &refusing)

		// A candidate st.yml is checked against a status without reading anything.
		other := *f.cfg
		other.ContractAddress = common.HexToAddress("0x2000000000000000000000000000000000000009")
		for _, test := range []struct {
			name   string
			cfg    *StConfig
			status *StLedgerV2ActivationStatus
			want   string
		}{
			{"the recorded pair against an armed latch", f.cfg, armed, ""},
			{"no pair against an armed latch", &off, armed, refusing.Refusal},
			{"no pair against a latch that is not armed", &off, unarmed, ""},
			{"another deployment's status", &other, armed, `st: the activation status is of deployment "` + key + `", not st.yml's "945:0x2000000000000000000000000000000000000009"`},
		} {
			if err := CheckStLedgerV2Activation(test.cfg, test.status); (err == nil) != (test.want == "") || err != nil && err.Error() != test.want {
				t.Fatalf("%s: %v", test.name, err)
			}
		}
	})
}

// SV-9 (no table): without the v2 pair, the sync turn, the mirror, a chain
// write and payout readiness run in a database without the activation table,
// as they did before the record existed. With the pair the record is required.
func TestStLedgerV2ActivationIsNotNeededWithoutThePair(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		f, v1 := newLedgerV2ActivationFixture(t)
		off := *f.cfg
		off.SettlementV2, off.VaultV2 = common.Address{}, common.Address{}
		f.restart(&off)
		ledgerV2Exec(t, f, `DROP TABLE st_ledger_v2_activation`)
		if _, err := StSyncChainState(f.ctx); err != nil {
			t.Fatalf("v1 turn without the table: %v", err)
		}
		if _, err := StSyncChainEvents(f.ctx, 0); err != nil || !reflect.DeepEqual(v1.ranges, [][2]uint64{{1_000, 1_500}}) {
			t.Fatalf("v1 mirror without the table: %v, ranges %v", err, v1.ranges)
		}
		if outcome, err := StCloseOperatorEpoch(f.ctx, 9); err != nil || outcome.Status != model.StPublishStatusConfirmed {
			t.Fatalf("v1 close without the table: %+v, %v", outcome, err)
		}
		if status, err := GetProviderPayoutTransitionStatus(f.ctx); err != nil || status.ReadinessReason != "" ||
			!reflect.DeepEqual(status.LedgerV2Activation, &StLedgerV2ActivationStatus{DeploymentKey: string(off.DeploymentKey())}) {
			t.Fatalf("readiness without the table: %+v, %v", status, err)
		}

		f.restart(f.cfg)
		if recovered := ledgerV2Recovered(func() { StSyncChainState(f.ctx) }); recovered == nil {
			t.Fatal("a turn with the v2 pair ran without the activation table")
		}
	})
}

// SV-9 (operator commands): under a v2 pair other than the recorded one the v2
// operator commands are refused like the turn, and write nothing: no
// declaration, no rebuild request and no withdrawal. Under the recorded pair
// they run.
func TestStLedgerV2ActivationOperatorCommandsNeedTheRecordedPair(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		f, _ := newLedgerV2ActivationFixture(t)
		observeLedgerV2Activation(t, f)
		key := f.cfg.DeploymentKey()
		for _, epoch := range []uint64{6, 7} {
			if err := model.RecordStLedgerV2EpochRows(f.ctx, key, 1, epoch, []string{}); err != nil {
				t.Fatal(err)
			}
		}
		if requested, err := model.RequestStLedgerV2EpochRebuild(f.ctx, key, 1, 6, "rebuilder@example"); !requested || err != nil {
			t.Fatalf("request: %t, %v", requested, err)
		}
		changed := *f.cfg
		changed.VaultV2 = common.HexToAddress("0x00000000000000000000000000000000000000b2")
		f.restart(&changed)
		want := ledgerV2ActivationRefusal + "differs from the recorded vault_v2_address " + testLedgerV2Vault + " (st subsystem disabled)"
		requireLedgerV2Activated(t, "v2-unrecoverable", StLedgerV2DeclareUnrecoverable(f.ctx, 5, "operator@example"), want)
		requireLedgerV2Activated(t, "v2-rebuild", StLedgerV2RebuildEpoch(f.ctx, 7, "rebuilder@example"), want)
		_, err := StLedgerV2WithdrawRebuild(f.ctx, 6, "withdrawer@example")
		requireLedgerV2Activated(t, "v2-rebuild-withdraw", err, want)
		records := model.GetStLedgerV2EpochRows(f.ctx, key, 1, 5, 7)
		if records[5] != nil || records[6].RebuildRequestedBy != "rebuilder@example" || records[7].RebuildRequestedBy != "" {
			t.Fatalf("a refused command wrote: %+v %+v %+v", records[5], records[6], records[7])
		}

		f.restart(f.cfg)
		if err := StLedgerV2DeclareUnrecoverable(f.ctx, 5, "operator@example"); err != nil {
			t.Fatalf("v2-unrecoverable under the recorded pair: %v", err)
		}
		if err := StLedgerV2RebuildEpoch(f.ctx, 7, "rebuilder@example"); err != nil {
			t.Fatalf("v2-rebuild under the recorded pair: %v", err)
		}
		if requestedBy, err := StLedgerV2WithdrawRebuild(f.ctx, 6, "withdrawer@example"); err != nil || requestedBy != "rebuilder@example" {
			t.Fatalf("v2-rebuild-withdraw under the recorded pair: %q, %v", requestedBy, err)
		}
	})
}

// SV-9 (clear): a record of a pair that is consistent on chain but is not the
// deployment's refuses the corrected st.yml until the operator clears it,
// which needs every recorded value named exactly and who clears it; anything
// else is refused and changes nothing. The corrected pair then runs and
// records its own activation.
func TestStLedgerV2ActivationIsClearedOnlyByItsExactValues(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		f, _ := newLedgerV2ActivationFixture(t)
		key := f.cfg.DeploymentKey()
		// Addresses whose checksummed form differs from the recorded lowercase one.
		const wrongSettlement, wrongVault = "0x8617e340b3d01fa5f11f306f4090fd50e238070d", "0x52908400098527886e0f7030069857d2e4169ee7"
		named := func(change func(*model.StLedgerV2Activation)) *model.StLedgerV2Activation {
			activation := &model.StLedgerV2Activation{NoId: 1, FirstEpoch: 5, VaultV2Address: wrongVault, SettlementV2Address: wrongSettlement}
			if change != nil {
				change(activation)
			}
			return activation
		}
		kept := func(name string, err error, reason string) {
			t.Helper()
			if !errors.Is(err, ErrStLedgerV2ActivationKept) || err.Error() != "st ledger v2: the activation is not cleared: "+reason {
				t.Fatalf("%s: %v", name, err)
			}
		}
		kept("nothing recorded", StLedgerV2ClearActivation(f.ctx, named(nil), "operator@example"), "deployment "+string(key)+" has none recorded")

		wrong := *f.cfg
		wrong.SettlementV2, wrong.VaultV2 = common.HexToAddress(wrongSettlement), common.HexToAddress(wrongVault)
		f.chain.coordinator = wrong.SettlementV2
		f.restart(&wrong)
		if allowed, err := StV1EpochAllowed(f.ctx, 5); err != nil || allowed {
			t.Fatalf("epoch 5 under the wrong pair: allowed %t, %v", allowed, err)
		}
		recorded := model.GetStLedgerV2Activation(f.ctx, key)
		if recorded == nil || recorded.VaultV2Address != wrongVault || recorded.SettlementV2Address != wrongSettlement {
			t.Fatalf("recorded %+v", recorded)
		}

		f.chain.coordinator = f.cfg.SettlementV2
		f.restart(f.cfg)
		_, err := StSyncChainState(f.ctx)
		requireLedgerV2Activated(t, "the corrected pair", err, ledgerV2ActivationRefusal+"differs from the recorded settlement_v2_address "+
			wrongSettlement+" and vault_v2_address "+wrongVault+" (st subsystem disabled)")

		if err := StLedgerV2ClearActivation(f.ctx, named(nil), ""); err == nil || err.Error() != "st ledger v2: clearing the activation must name who clears it" {
			t.Fatalf("an unnamed clear: %v", err)
		}
		exactly := "no_id, first_epoch, vault_v2_address and settlement_v2_address must name deployment " + string(key) + "'s record exactly"
		for name, change := range map[string]func(*model.StLedgerV2Activation){
			"another pool":          func(a *model.StLedgerV2Activation) { a.NoId = 2 },
			"another first epoch":   func(a *model.StLedgerV2Activation) { a.FirstEpoch = 6 },
			"another vault2":        func(a *model.StLedgerV2Activation) { a.VaultV2Address = testLedgerV2Vault },
			"another STSettlement2": func(a *model.StLedgerV2Activation) { a.SettlementV2Address = testLedgerV2Settlement },
			"a checksummed vault2":  func(a *model.StLedgerV2Activation) { a.VaultV2Address = common.HexToAddress(wrongVault).Hex() },
		} {
			kept(name, StLedgerV2ClearActivation(f.ctx, named(change), "operator@example"), exactly)
		}
		if again := model.GetStLedgerV2Activation(f.ctx, key); !reflect.DeepEqual(again, recorded) {
			t.Fatalf("a refused clear changed %+v to %+v", recorded, again)
		}
		SetStConfig(&StConfig{Enabled: true})
		if err := StLedgerV2ClearActivation(f.ctx, named(nil), "operator@example"); !errors.Is(err, errStNotConfigured) {
			t.Fatalf("a clear without a deployment: %v", err)
		}

		f.restart(f.cfg)
		if err := StLedgerV2ClearActivation(f.ctx, named(nil), "operator@example"); err != nil {
			t.Fatalf("the exact clear: %v", err)
		}
		if status := GetStLedgerV2ActivationStatus(f.ctx); status.Armed {
			t.Fatalf("status after the clear: %+v", status)
		}
		if _, err := StSyncChainState(f.ctx); err != nil {
			t.Fatalf("the corrected pair after the clear: %v", err)
		}
		observeLedgerV2Activation(t, f)
	})
}

// SV-9 (transient read): a failed read of the record fails only the call it
// serves. Nothing is cached, so the next turn runs, and a refusal ends with
// the turn that restores the pair.
func TestStLedgerV2ActivationReadFailureLastsOneTurn(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		f, v1 := newLedgerV2ActivationFixture(t)
		observeLedgerV2Activation(t, f)
		ledgerV2Exec(t, f, `ALTER TABLE st_ledger_v2_activation RENAME TO st_ledger_v2_activation_away`)
		if recovered := ledgerV2Recovered(func() { StSyncChainState(f.ctx) }); recovered == nil {
			t.Fatal("a turn whose record read failed ran")
		}
		if recovered := ledgerV2Recovered(func() { StSyncChainEvents(f.ctx, 0) }); recovered == nil || len(v1.ranges) != 0 {
			t.Fatalf("a mirror turn whose record read failed ran: %v, ranges %v", recovered, v1.ranges)
		}
		ledgerV2Exec(t, f, `ALTER TABLE st_ledger_v2_activation_away RENAME TO st_ledger_v2_activation`)
		if _, err := StSyncChainState(f.ctx); err != nil {
			t.Fatalf("the turn after the failed read: %v", err)
		}
		if synced, err := StSyncChainEvents(f.ctx, 0); err != nil || synced != 0 || len(v1.ranges) != 1 {
			t.Fatalf("the mirror turn after the failed read: %d, %v, ranges %v", synced, err, v1.ranges)
		}

		off := *f.cfg
		off.SettlementV2, off.VaultV2 = common.Address{}, common.Address{}
		for range 3 {
			f.restart(&off)
			if _, err := StSyncChainState(f.ctx); !errors.Is(err, ErrStLedgerV2Activated) {
				t.Fatalf("turn without the pair: %v", err)
			}
			f.restart(f.cfg)
			if _, err := StSyncChainState(f.ctx); err != nil {
				t.Fatalf("turn with the pair restored: %v", err)
			}
		}
	})
}

// SV-9 (several deployments): the record binds only its own deployment. The
// mainnet chain in the same database, and the same chain's replacement
// coordinator, run without the pair while the first deployment's activation is
// recorded, and a second deployment records its own.
func TestStLedgerV2ActivationBindsOnlyItsDeployment(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		f, _ := newLedgerV2ActivationFixture(t)
		recorded := observeLedgerV2Activation(t, f)

		mainnet := *f.cfg
		mainnet.ChainId = 964
		mainnet.SettlementV2, mainnet.VaultV2 = common.Address{}, common.Address{}
		replacement := *f.cfg
		replacement.ContractAddress = common.HexToAddress("0x2000000000000000000000000000000000000009")
		replacement.SettlementV2, replacement.VaultV2 = common.Address{}, common.Address{}
		for _, cfg := range []*StConfig{&mainnet, &replacement} {
			f.restart(cfg)
			if _, err := StSyncChainState(f.ctx); err != nil {
				t.Fatalf("deployment %s without the pair: %v", cfg.DeploymentKey(), err)
			}
		}

		const settlement, vault = "0x00000000000000000000000000000000000000c1", "0x00000000000000000000000000000000000000c2"
		other := *f.cfg
		other.ChainId = 964
		other.SettlementV2, other.VaultV2 = common.HexToAddress(settlement), common.HexToAddress(vault)
		f.restart(&other)
		f.chain.firstEpoch, f.chain.coordinator = 9, other.SettlementV2
		if first, registered, err := stLedgerV2FirstEpoch(f.ctx, &other, f.chain); err != nil || !registered || first != 9 {
			t.Fatalf("the second deployment's registration: %d, %t, %v", first, registered, err)
		}
		if again := model.GetStLedgerV2Activation(f.ctx, f.cfg.DeploymentKey()); !reflect.DeepEqual(again, recorded) {
			t.Fatalf("the first deployment's record changed to %+v", again)
		}
		if second := model.GetStLedgerV2Activation(f.ctx, other.DeploymentKey()); second == nil || second.FirstEpoch != 9 || second.VaultV2Address != vault || second.SettlementV2Address != settlement {
			t.Fatalf("the second deployment recorded %+v", second)
		}
	})
}

// SV-9 (address form): the pair is compared as addresses, never as st.yml
// text. The same pair parsed from lowercase, checksummed, uppercase and
// unprefixed hex is admitted against the recorded lowercase form. (The st.yml
// loader itself accepts only the lowercase form.)
func TestStLedgerV2ActivationComparesAddressesNotText(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		f, _ := newLedgerV2ActivationFixture(t)
		const settlement, vault = "0x52908400098527886e0f7030069857d2e4169ee7", "0xde709f2102306220921060314715629080e2fb77"
		cfg := *f.cfg
		cfg.SettlementV2, cfg.VaultV2 = common.HexToAddress(settlement), common.HexToAddress(vault)
		f.restart(&cfg)
		f.chain.coordinator = cfg.SettlementV2
		if _, _, err := stLedgerV2FirstEpoch(f.ctx, &cfg, f.chain); err != nil {
			t.Fatal(err)
		}
		if recorded := model.GetStLedgerV2Activation(f.ctx, cfg.DeploymentKey()); recorded == nil || recorded.SettlementV2Address != settlement || recorded.VaultV2Address != vault {
			t.Fatalf("recorded %+v, want the lowercase pair", recorded)
		}
		for _, form := range [][2]string{
			{"0x52908400098527886E0F7030069857D2E4169EE7", "0xde709f2102306220921060314715629080e2fb77"},
			{"0X52908400098527886E0F7030069857D2E4169EE7", "0XDE709F2102306220921060314715629080E2FB77"},
			{"52908400098527886e0f7030069857d2e4169ee7", "de709f2102306220921060314715629080e2fb77"},
		} {
			again := *f.cfg
			again.SettlementV2, again.VaultV2 = common.HexToAddress(form[0]), common.HexToAddress(form[1])
			f.restart(&again)
			if err := stLedgerV2ActivationAdmission(f.ctx, &again); err != nil {
				t.Fatalf("the pair written as %v: %v", form, err)
			}
		}
	})
}

// SV-9 (migration head): the record's table is part of the binary's migration
// head, which a fresh database reaches and the services' startup readiness
// requires, so a service that reads the table does not claim work from a
// database without it. The table has the columns the monitor contract lists.
func TestStLedgerV2ActivationMigrationIsTheRequiredHead(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		f := newLedgerV2Fixture(t)
		if required := server.MinimumRuntimeMigrationVersion(); required != server.MigrationCount() {
			t.Fatalf("startup readiness requires %d of the binary's %d migrations", required, server.MigrationCount())
		}
		var head int
		columns := []string{}
		server.Tx(f.ctx, func(tx server.PgTx) {
			server.Raise(tx.QueryRow(f.ctx, `SELECT COALESCE(MAX(end_version_number), 0) FROM migration_audit WHERE status = 'success'`).Scan(&head))
			result, err := tx.Query(f.ctx, `
				SELECT column_name || ' ' || data_type || COALESCE('(' || character_maximum_length || ')', '') || ' nullable ' || is_nullable
				FROM information_schema.columns
				WHERE table_schema = 'public' AND table_name = 'st_ledger_v2_activation'
				ORDER BY ordinal_position
			`)
			server.WithPgResult(result, err, func() {
				for result.Next() {
					var column string
					server.Raise(result.Scan(&column))
					columns = append(columns, column)
				}
			})
		})
		want := []string{
			"deployment_key character varying(96) nullable NO",
			"no_id bigint nullable NO",
			"first_epoch bigint nullable NO",
			"vault_v2_address character varying(42) nullable NO",
			"settlement_v2_address character varying(42) nullable NO",
			"create_time timestamp without time zone nullable NO",
		}
		if head != server.MigrationCount() || !reflect.DeepEqual(columns, want) {
			t.Fatalf("database head %d of %d, columns %v", head, server.MigrationCount(), columns)
		}
	})
}
