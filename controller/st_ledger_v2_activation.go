package controller

// st_ledger_v2_activation.go — the settlement v2 activation latch (SV-9). The
// first time the server observes vault2's registration of the configured pool,
// it records the deployment's activation (stLedgerV2FirstEpoch). Registration
// is permanent, while the v2 pair in st.yml is not: without it the v1 driver
// would again close and commit the epochs v2 settles, and the event mirror
// would move its one checkpoint past v2 logs it never read. So once the
// activation is recorded, every sync turn, mirror turn and chain write of the
// deployment is refused unless st.yml names the recorded pair, and payout
// readiness reports why. The mirror's checkpoint stays where it was, so
// restoring the pair resumes from it.

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/urnetwork/server/model"
)

// ErrStLedgerV2Activated refuses the st subsystem of a deployment whose v2
// activation is recorded while st.yml lacks the recorded v2 pair or names
// another one.
var ErrStLedgerV2Activated = errors.New("st: settlement v2 is active")

// stLedgerV2ActivationAdmission refuses with ErrStLedgerV2Activated, naming
// the first v2 epoch and each missing or mismatched st.yml key with its
// recorded value, when the configured v2 pair is not the one recorded with the
// deployment's activation. Without a recorded activation nothing is refused.
func stLedgerV2ActivationAdmission(ctx context.Context, cfg *StConfig) error {
	deploymentKey := cfg.DeploymentKey()
	if deploymentKey == "" {
		return nil
	}
	activation := model.GetStLedgerV2Activation(ctx, deploymentKey)
	if activation == nil {
		return nil
	}
	var differing []string
	if stLedgerV2Address(cfg.SettlementV2) != activation.SettlementV2Address {
		differing = append(differing, "settlement_v2_address "+activation.SettlementV2Address)
	}
	if stLedgerV2Address(cfg.VaultV2) != activation.VaultV2Address {
		differing = append(differing, "vault_v2_address "+activation.VaultV2Address)
	}
	if len(differing) == 0 {
		return nil
	}
	state := "differs from"
	if !cfg.LedgerV2Enabled() {
		state = "is missing"
	}
	return fmt.Errorf("%w from epoch %d (pool %d), but st.yml %s the recorded %s (st subsystem disabled)",
		ErrStLedgerV2Activated, activation.FirstEpoch, activation.NoId, state, strings.Join(differing, " and "))
}
