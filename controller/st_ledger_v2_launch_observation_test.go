// Synthetic controller selection proves the hook is read without chain or DB
// admission. Global test seams are restored; these tests do not run in parallel.
package controller

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/ethereum/go-ethereum/common"
)

func TestStLedgerV1LaunchObservationExportsOnlyPublicPolicy(t *testing.T) {
	previousConfig, previousSweep := stConfigInstance, stLedgerV1SweepEpoch
	t.Cleanup(func() { stConfigInstance, stLedgerV1SweepEpoch = previousConfig, previousSweep })
	cfg := &StConfig{Profile: "synthetic", Enabled: true, ChainId: 945, NoId: 7, ContractAddress: common.Address{1}, SettlementV2: common.Address{2}, VaultV2: common.Address{3}}
	SetStConfig(cfg)
	calls := 0
	stLedgerV1SweepEpoch = func(ctx context.Context, actual *StConfig) (uint64, bool) {
		calls++
		if actual != cfg || ctx != t.Context() {
			t.Fatal("sweep hook source context changed")
		}
		return 0, false
	}
	got, err := ObserveStLedgerV1LaunchPolicy(t.Context())
	if err != nil || got == nil || calls != 1 || got.SweepEnabled || got.SweepEpoch != 0 || !got.LedgerV2Enabled || got.Profile != cfg.Profile || got.NoId != cfg.NoId || got.Scope != "this-helper-compiled-controller-policy" {
		t.Fatalf("public policy projection: %+v %v calls=%d", got, err, calls)
	}
	raw, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"private", "root_key", "deposit_key", "ops_key", "artifact_key"} {
		if strings.Contains(string(raw), forbidden) {
			t.Fatalf("private configuration field %s was exported", forbidden)
		}
	}
}

func TestStLedgerV1LaunchObservationReportsActivePhaseCSweep(t *testing.T) {
	previousConfig, previousSweep := stConfigInstance, stLedgerV1SweepEpoch
	t.Cleanup(func() { stConfigInstance, stLedgerV1SweepEpoch = previousConfig, previousSweep })
	SetStConfig(&StConfig{Profile: "synthetic", Enabled: true, ChainId: 945, NoId: 7, ContractAddress: common.Address{1}, SettlementV2: common.Address{2}, VaultV2: common.Address{3}})
	stLedgerV1SweepEpoch = func(context.Context, *StConfig) (uint64, bool) { return 4, true }
	got, err := ObserveStLedgerV1LaunchPolicy(t.Context())
	if err != nil || got == nil || !got.SweepEnabled || got.SweepEpoch != 4 {
		t.Fatalf("active phase C was hidden behind the v2 pair: %+v %v", got, err)
	}
}

func TestStLedgerV1LaunchObservationCancellationDoesNotLoadPolicy(t *testing.T) {
	previousSweep := stLedgerV1SweepEpoch
	t.Cleanup(func() { stLedgerV1SweepEpoch = previousSweep })
	stLedgerV1SweepEpoch = func(context.Context, *StConfig) (uint64, bool) {
		t.Fatal("canceled observation reached the hook")
		return 0, false
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	for _, invalid := range []context.Context{nil, ctx} {
		got, err := ObserveStLedgerV1LaunchPolicy(invalid)
		if err == nil || got != nil {
			t.Fatalf("missing/canceled context produced policy: %+v %v", got, err)
		}
	}
}
