// Read only this compiled controller's loaded public selection and phase C
// hook. A caller must separately bind deployed process/source identities and
// actual database observations; this cannot attest another process's memory.
package controller

import (
	"context"
	"errors"
	"strings"
)

// No keys or mutable configuration pointers cross the projection boundary.
type StLedgerV1LaunchPolicyObservation struct {
	Profile             string `json:"profile"`
	DeploymentKey       string `json:"deployment_key"`
	ChainId             uint64 `json:"chain_id"`
	NoId                uint64 `json:"no_id"`
	LedgerV2Enabled     bool   `json:"ledger_v2_enabled"`
	SettlementV2Address string `json:"settlement_v2_address"`
	VaultV2Address      string `json:"vault_v2_address"`
	SweepEnabled        bool   `json:"sweep_enabled"`
	SweepEpoch          uint64 `json:"sweep_epoch"`
	Scope               string `json:"scope"`
}

// This does not read/record the activation, call admission, schedule a task or
// access the database. The real compiled sweep hook is observed without an
// assumption that a recorded first epoch alone excludes phase C.
func ObserveStLedgerV1LaunchPolicy(ctx context.Context) (*StLedgerV1LaunchPolicyObservation, error) {
	if ctx == nil {
		return nil, errors.New("v1 launch policy observation needs a context")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	cfg := stConfig()
	if cfg == nil || !cfg.Enabled || cfg.DeploymentKey() == "" || cfg.NoId == 0 {
		return nil, errors.New("v1 launch policy has no enabled managed deployment")
	}
	epoch, enabled := stLedgerV1SweepEpoch(ctx, cfg)
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return &StLedgerV1LaunchPolicyObservation{Profile: cfg.Profile, DeploymentKey: string(cfg.DeploymentKey()), ChainId: cfg.ChainId, NoId: cfg.NoId,
		LedgerV2Enabled: cfg.LedgerV2Enabled(), SettlementV2Address: strings.ToLower(cfg.SettlementV2.Hex()), VaultV2Address: strings.ToLower(cfg.VaultV2.Hex()),
		SweepEnabled: enabled, SweepEpoch: epoch, Scope: "this-helper-compiled-controller-policy"}, nil
}
