package controller

// st_ledger_v2_activation_test.go — the settlement v2 activation latch (SV-9)
// with the real database: the first observation of vault2's registration of
// the pool is recorded, and from then on the deployment's st subsystem runs
// only with the recorded v2 pair.

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
	})
}

// SV-9 (conflicting record): a later observation that differs from the
// recorded activation is refused, so the v1 cutoff and the v2 driver wait, and
// the record stays as it was.
func TestStLedgerV2ActivationRefusesAConflictingRecord(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		f := newLedgerV2Fixture(t)
		recorded := observeLedgerV2Activation(t, f)
		f.restart(f.cfg)
		f.chain.firstEpoch = 6
		const want = "st ledger v2: the deployment's activation is already recorded: pool 1 from epoch 5 with vault_v2_address " +
			testLedgerV2Vault + " and settlement_v2_address " + testLedgerV2Settlement
		if _, err := StV1EpochAllowed(f.ctx, 5); !errors.Is(err, model.ErrStLedgerV2ActivationRecorded) || err.Error() != want {
			t.Fatalf("v1 cutoff: %v", err)
		}
		if steps, err := StLedgerV2Sync(f.ctx, &StEpochState{Epoch: 7, HeadBlock: f.chain.end(6) + 10}); steps != nil || err == nil || err.Error() != want {
			t.Fatalf("v2 sync: %v, %v", steps, err)
		}
		if again := model.GetStLedgerV2Activation(f.ctx, f.cfg.DeploymentKey()); !reflect.DeepEqual(again, recorded) {
			t.Fatalf("the record changed from %+v to %+v", recorded, again)
		}
	})
}
