package controller

// st_ledger_v2_activation.go — the settlement v2 activation latch (SV-9).
//
// Arming. The first time the server reads vault2's registration of the
// configured pool, and vault2 names st.yml's STSettlement2 as its coordinator,
// it records the deployment's activation (stLedgerV2FirstEpoch). Registration
// is permanent, while the v2 pair in st.yml is not: without it the v1 driver
// would again close and commit the epochs v2 settles, and the event mirror
// would move its one checkpoint past v2 logs it never read.
//
// Armed. Every sync turn, mirror turn, chain write and v2 operator command of
// the deployment is refused unless st.yml names the recorded pair, and payout
// readiness reports why. The v1 cutoff is the recorded first epoch, whatever a
// later read of vault2 answers. The mirror's checkpoint stays where it was, so
// restoring the pair resumes from it.
//
// Not armed. Until that first read succeeds nothing is recorded, and a
// deployment without the v2 pair runs as it did before settlement v2, without
// the record's table. So the latch cannot refuse removing the pair between the
// pool's registration and the server's first read of it: within the minute an
// unregistered answer is cached, while that read fails, or while the server is
// stopped. It does not arm on an unreadable registration, because removing the
// pair is also how a failed switch is undone before the pool registers. The
// operator instead checks that the latch is armed before the pair is ever
// removed: GetStLedgerV2ActivationStatus, shown by `bringyourctl
// sn-transition-status` and `bringyourctl st v2-activation` and checked
// against a candidate st.yml by `sn25golive st-check --v2-activation`.
//
// A wrong record, of a pair that is consistent on chain but is not the
// deployment's, is cleared by `bringyourctl st v2-activation-clear`, which
// must name the recorded values exactly (StLedgerV2ClearActivation).

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/urnetwork/glog"

	"github.com/urnetwork/server/model"
)

var (
	// ErrStLedgerV2Activated refuses the st subsystem of a deployment whose v2
	// activation is recorded while st.yml lacks the recorded v2 pair or names
	// another one.
	ErrStLedgerV2Activated = errors.New("st: settlement v2 is active")
	// ErrStLedgerV2Coordinator refuses to record an activation under an
	// STSettlement2 that vault2 does not name as its coordinator.
	ErrStLedgerV2Coordinator = errors.New("st ledger v2: st.yml's settlement_v2_address is not vault2's coordinator")
	// ErrStLedgerV2ActivationKept refuses to clear a recorded activation.
	ErrStLedgerV2ActivationKept = errors.New("st ledger v2: the activation is not cleared")
)

// stLedgerV2RecordActivation records vault2's registration of the configured
// pool as the deployment's activation. vault2.pools answers the same whatever
// STSettlement2 st.yml names, so the pair is recorded only if vault2 names that
// settlement as its coordinator: a record of any other would refuse the
// corrected st.yml for good.
func stLedgerV2RecordActivation(ctx context.Context, cfg *StConfig, client StLedgerV2Client, firstEpoch uint64) error {
	coordinator, err := client.LedgerV2Coordinator(ctx)
	if err != nil {
		return err
	}
	if coordinator != cfg.SettlementV2 {
		return fmt.Errorf("%w: vault2 0x%x is bound to 0x%x, not 0x%x; the pool's activation is not recorded",
			ErrStLedgerV2Coordinator, cfg.VaultV2, coordinator, cfg.SettlementV2)
	}
	return model.RecordStLedgerV2Activation(ctx, cfg.DeploymentKey(), &model.StLedgerV2Activation{
		NoId: cfg.NoId, FirstEpoch: firstEpoch, VaultV2Address: stLedgerV2Address(cfg.VaultV2), SettlementV2Address: stLedgerV2Address(cfg.SettlementV2),
	})
}

// stLedgerV2RecordedActivation reads the deployment's recorded activation, or
// nil. Without the v2 pair it runs for deployments that never use v2, whose
// database may predate the record's table: no table, no activation.
func stLedgerV2RecordedActivation(ctx context.Context, cfg *StConfig) *model.StLedgerV2Activation {
	deploymentKey := cfg.DeploymentKey()
	if deploymentKey == "" {
		return nil
	}
	if cfg.LedgerV2Enabled() {
		return model.GetStLedgerV2Activation(ctx, deploymentKey)
	}
	return model.GetStLedgerV2ActivationIfInstalled(ctx, deploymentKey)
}

// stLedgerV2ActivationRefusal is ErrStLedgerV2Activated, naming the first v2
// epoch and each missing or mismatched st.yml key with its recorded value,
// when cfg's v2 pair is not the one recorded with the activation.
func stLedgerV2ActivationRefusal(cfg *StConfig, activation *model.StLedgerV2Activation) error {
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

// stLedgerV2ActivationAdmission refuses with stLedgerV2ActivationRefusal when
// the deployment's activation is recorded and the configured v2 pair is not
// the recorded one. Without a recorded activation nothing is refused.
func stLedgerV2ActivationAdmission(ctx context.Context, cfg *StConfig) error {
	if activation := stLedgerV2RecordedActivation(ctx, cfg); activation != nil {
		return stLedgerV2ActivationRefusal(cfg, activation)
	}
	return nil
}

// StLedgerV2ActivationStatus shows operators a deployment's activation latch:
// not armed until the activation is recorded, then armed with the recorded
// pool, first epoch and pair. Refusal is the latch's reason against the loaded
// st.yml, and empty while it admits it.
type StLedgerV2ActivationStatus struct {
	DeploymentKey       string     `json:"deployment_key"`
	Armed               bool       `json:"armed"`
	NoId                uint64     `json:"no_id,omitempty"`
	FirstEpoch          uint64     `json:"first_epoch,omitempty"`
	VaultV2Address      string     `json:"vault_v2_address,omitempty"`
	SettlementV2Address string     `json:"settlement_v2_address,omitempty"`
	RecordedAt          *time.Time `json:"recorded_at,omitempty"`
	Refusal             string     `json:"refusal,omitempty"`
}

// GetStLedgerV2ActivationStatus reads the latch of the deployment the loaded
// st.yml selects.
func GetStLedgerV2ActivationStatus(ctx context.Context) *StLedgerV2ActivationStatus {
	cfg := stConfig()
	status := &StLedgerV2ActivationStatus{DeploymentKey: string(cfg.DeploymentKey())}
	activation := stLedgerV2RecordedActivation(ctx, cfg)
	if activation == nil {
		return status
	}
	status.Armed, status.NoId, status.FirstEpoch = true, activation.NoId, activation.FirstEpoch
	status.VaultV2Address, status.SettlementV2Address, status.RecordedAt = activation.VaultV2Address, activation.SettlementV2Address, &activation.CreateTime
	if err := stLedgerV2ActivationRefusal(cfg, activation); err != nil {
		status.Refusal = err.Error()
	}
	return status
}

// CheckStLedgerV2Activation checks a candidate st.yml against the activation
// status of the server it is for, reading nothing: the refusal that server
// would give it, or nil. A status of another deployment is refused.
func CheckStLedgerV2Activation(cfg *StConfig, status *StLedgerV2ActivationStatus) error {
	if deploymentKey := string(cfg.DeploymentKey()); status.DeploymentKey != deploymentKey {
		return fmt.Errorf("st: the activation status is of deployment %q, not st.yml's %q", status.DeploymentKey, deploymentKey)
	}
	if !status.Armed {
		return nil
	}
	return stLedgerV2ActivationRefusal(cfg, &model.StLedgerV2Activation{
		NoId: status.NoId, FirstEpoch: status.FirstEpoch, VaultV2Address: status.VaultV2Address, SettlementV2Address: status.SettlementV2Address,
	})
}

// StLedgerV2ClearActivation is the operator's explicit command removing the
// recorded activation of the deployment the loaded st.yml selects, for a
// record that names a wrong pool or pair. It must name the recorded values
// exactly and who clears it, which is logged; without a record, or with any
// other value, it is refused and nothing changes. The server records the
// activation again the next time it reads the registration, which a process
// that already read it does after a restart.
func StLedgerV2ClearActivation(ctx context.Context, named *model.StLedgerV2Activation, clearedBy string) error {
	deploymentKey := stConfig().DeploymentKey()
	if deploymentKey == "" {
		return errStNotConfigured
	}
	if clearedBy == "" {
		return errors.New("st ledger v2: clearing the activation must name who clears it")
	}
	if model.GetStLedgerV2Activation(ctx, deploymentKey) == nil {
		return fmt.Errorf("%w: deployment %s has none recorded", ErrStLedgerV2ActivationKept, deploymentKey)
	}
	if !model.ClearStLedgerV2Activation(ctx, deploymentKey, named) {
		return fmt.Errorf("%w: no_id, first_epoch, vault_v2_address and settlement_v2_address must name deployment %s's record exactly", ErrStLedgerV2ActivationKept, deploymentKey)
	}
	glog.Infof("[st]v2 activation of deployment %s cleared by %s: pool %d from epoch %d, vault_v2_address %s, settlement_v2_address %s\n",
		deploymentKey, clearedBy, named.NoId, named.FirstEpoch, named.VaultV2Address, named.SettlementV2Address)
	return nil
}
