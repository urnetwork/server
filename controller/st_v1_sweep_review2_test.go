// Second review of the phase C sweep (server-b3, 2026-11-01): a refused sweep
// config is shown, the failure count abandons only past the commit cutoff
// with no commit intent reserved, and the Safe calls ignore absorb dust.
package controller

import (
	"context"
	"errors"
	"math/big"
	"strings"
	"testing"

	"github.com/urnetwork/server/model"
)

// A refused sweep config shows its reason even though the sweep is off;
// runbook D3 reads config_error.
func TestStV1SweepStatusShowsARefusedConfig(t *testing.T) {
	cfg := sweepTestConfig()
	cfg.V1SweepEpoch, cfg.V1SweepRecoveryEpochs = 0, nil
	cfg.V1SweepConfigError = "st.yml testnet sweep keys refused, the sweep is disabled: v1_sweep_recovery_epochs [4] must ascend strictly below the sweep epoch 4"
	SetStConfig(cfg)
	t.Cleanup(func() { SetStConfig(nil) })
	view, err := GetStV1SweepStatus(context.Background())
	if err != nil || view.Configured || view.ConfigError != cfg.V1SweepConfigError {
		t.Fatalf("view %+v err %v", view, err)
	}
}

// sweepCommitFactsClient answers the abandon gate's chain and intent facts.
type sweepCommitFactsClient struct {
	sweepFakeClient
	latest, cutoff uint64
	reserved       bool
	factsErr       error
}

func (self *sweepCommitFactsClient) V1SweepCommitFacts(context.Context, uint64, uint64) (uint64, uint64, bool, error) {
	return self.latest, self.cutoff, self.reserved, self.factsErr
}

// The failure count abandons only once the latest block is at or past the
// commit cutoff and no root:E:N commit intent is reserved; before that, or
// with a commit intent reserved (a commit whose send timed out), or when
// the facts cannot be read, it only alerts.
func TestStV1SweepAfterTurnAbandonsOnlyPastTheCommitCutoff(t *testing.T) {
	SetStConfig(sweepTestConfig())
	t.Cleanup(func() { SetStConfig(nil) })
	prior := stV1SweepPublishes
	stV1SweepPublishes = func(context.Context, *StConfig, uint64) []*model.StPublish { return nil }
	t.Cleanup(func() { stV1SweepPublishes = prior })
	cutoff := uint64(sweepTestEnd + sweepTestWindow - stV1SweepCommitMarginBlocks)
	failed := &StPublishOutcome{Status: model.StPublishStatusFailed, Reason: "rpc down", Retry: true}
	for _, test := range []struct {
		name    string
		client  *sweepCommitFactsClient
		abandon bool
	}{
		{"an open commit window", &sweepCommitFactsClient{latest: sweepTestEnd + 600, cutoff: cutoff}, false},
		{"the block before the cutoff", &sweepCommitFactsClient{latest: cutoff - 1, cutoff: cutoff}, false},
		{"a reserved commit intent", &sweepCommitFactsClient{latest: cutoff + 50, cutoff: cutoff, reserved: true}, false},
		{"unreadable facts", &sweepCommitFactsClient{latest: cutoff + 50, cutoff: cutoff, factsErr: errors.New("rpc down")}, false},
		{"past the cutoff, nothing reserved", &sweepCommitFactsClient{latest: cutoff, cutoff: cutoff}, true},
	} {
		capture := captureSweep(t)
		SetStV1SweepClient(test.client)
		failures, stop := StV1SweepAfterTurn(context.Background(), sweepTestEpoch, stV1SweepAbandonFailures-1, failed, nil)
		SetStV1SweepClient(nil)
		abandoned := false
		for _, outcome := range capture.outcomes {
			abandoned = abandoned || strings.Contains(outcome, StPublishKindV1SweepAbandon)
		}
		if stop != test.abandon || abandoned != test.abandon || failures != stV1SweepAbandonFailures {
			t.Fatalf("%s: stop %t abandoned %t failures %d outcomes %v", test.name, stop, abandoned, failures, capture.outcomes)
		}
		if len(capture.alerts) == 0 {
			t.Fatalf("%s: no alert", test.name)
		}
	}
}

// Dust on the v1 escrow below the absorb threshold does not hide the Safe's
// assignSurplus call, as it does not block the planner.
func TestStV1SweepSafeCallsIgnoreAbsorbDust(t *testing.T) {
	chain := sweepFundedChain()
	chain.Sweep.Root, chain.Sweep.Status, chain.Sweep.Total, chain.LeafClaimed = StV1SweepRoot(sweepTestVault2Coldkey), stV1SweepFinalized, sweepRao(sweepTestTotal), true
	chain.AssignSelector, chain.V2NextEpoch, chain.RecordedEpoch, chain.RecordedArtifact = true, 5, 4, [32]byte{0xaa}
	chain.Sweep.RootArtifact, chain.RecordedRoot = [32]byte{0xaa}, chain.Sweep.Root
	for _, dust := range []int64{1, stV1SweepMinimumAbsorbRao - 1} {
		chain.Absorbable = sweepRao(dust)
		calls := stV1SweepSafeCalls(sweepTestConfig(), sweepTestEpoch, chain, big.NewInt(sweepTestTotal))
		if len(calls) != 1 || calls[0].Name != "assignSurplus" {
			t.Fatalf("dust %d rao: %+v", dust, calls)
		}
	}
	chain.Absorbable = sweepRao(stV1SweepMinimumAbsorbRao)
	if calls := stV1SweepSafeCalls(sweepTestConfig(), sweepTestEpoch, chain, big.NewInt(sweepTestTotal)); len(calls) != 0 {
		t.Fatalf("an absorbable amount still offered %+v", calls)
	}
}
