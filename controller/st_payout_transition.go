// Earnings activation is independent of chain epoch deadlines and existing
// transaction reconciliation. This gate authorizes no deployment by itself.
package controller

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/urnetwork/server"
)

func stPayoutIdentity(cfg *StConfig) server.ProviderPayoutMainnet {
	return server.ProviderPayoutMainnet{Profile: cfg.Profile, ChainId: cfg.ChainId,
		GenesisHash: fmt.Sprintf("0x%x", cfg.GenesisHash), Netuid: uint16(cfg.Netuid),
		DeploymentId: cfg.DeploymentId, Coordinator: cfg.ContractAddress.Hex(),
		SettlementVault: cfg.SettlementVault.Hex(), PolicyHash: fmt.Sprintf("0x%x", cfg.PolicyHash),
		ReadinessSha256: cfg.LaunchReadinessSha256}
}

func stPayoutAdmission(ctx context.Context, cfg *StConfig) error {
	return stPayoutAdmissionAt(ctx, cfg, server.NowUtc())
}

func stPayoutAdmissionAt(ctx context.Context, cfg *StConfig, now time.Time) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if cfg == nil {
		return errStNotConfigured
	}
	if cfg.Profile != "mainnet" {
		// A main environment cannot accidentally run the testnet payout writer.
		if env, _ := server.Env(); env == "main" {
			return fmt.Errorf("sn: main environment requires mainnet payout profile")
		}
		return nil
	}
	if !cfg.Enabled || cfg.Netuid != 25 {
		return fmt.Errorf("sn: mainnet payout is disabled or has wrong subnet")
	}
	policy, err := server.LoadProviderPayoutEarningPolicy(ctx)
	if err != nil {
		return err
	}
	if err := policy.MainnetAdmission(now, stPayoutIdentity(cfg)); err != nil {
		return err
	}
	return nil
}

// Public, redacted local status. This is loaded configuration and identity
// matching, never evidence that this binary/config is deployed or chain-ready.
type ProviderPayoutTransitionStatus struct {
	EarningBoundary                      *server.ProviderEarningBoundary  `json:"earning_boundary,omitempty"`
	EarningBoundaryReady                 bool                             `json:"earning_boundary_ready"`
	EarningBoundaryReason                string                           `json:"earning_boundary_reason"`
	ObservedAt                           time.Time                        `json:"observed_at"`
	Schedule                             *server.ProviderPayoutTransition `json:"schedule"`
	Selected                             *server.ProviderPayoutMainnet    `json:"selected,omitempty"`
	SelectedProfile                      string                           `json:"selected_profile"`
	Phase                                string                           `json:"phase"`
	NewMainnetWritesAdmitted             bool                             `json:"new_mainnet_writes_admitted"`
	LegacyPreCutoffPaymentsAllowed       bool                             `json:"legacy_pre_cutoff_payments_allowed"`
	NewLegacySubmissionsAdmitted         bool                             `json:"new_legacy_submissions_admitted"`
	ExistingAttemptReconciliationAllowed bool                             `json:"existing_attempt_reconciliation_allowed"`
	ReadinessReason                      string                           `json:"readiness_reason"`
	DeploymentVerified                   bool                             `json:"deployment_verified"`
	RetrospectiveClaimGuaranteed         bool                             `json:"retrospective_claim_guaranteed"`
	PayoutSchemaReady                    bool                             `json:"payout_schema_ready"`
	PayoutSchemaReason                   string                           `json:"payout_schema_reason"`
}

func GetProviderPayoutTransitionStatus(ctx context.Context) (*ProviderPayoutTransitionStatus, error) {
	policy, err := server.LoadProviderPayoutTransition(ctx)
	if err != nil {
		return nil, err
	}
	now := server.NowUtc()
	status := &ProviderPayoutTransitionStatus{ObservedAt: now, Schedule: policy, Phase: "legacy_unscheduled", LegacyPreCutoffPaymentsAllowed: true, ExistingAttemptReconciliationAllowed: true, SelectedProfile: os.Getenv("URNETWORK_ST_PROFILE")}
	if binding, err := server.RequireProviderPayoutBoundary(ctx, policy); err != nil {
		status.EarningBoundaryReason = err.Error()
		status.LegacyPreCutoffPaymentsAllowed = false
	} else {
		status.EarningBoundary = binding
		status.EarningBoundaryReady = policy == nil || binding != nil
	}
	if policy != nil {
		if err := server.RequireProviderPayoutSchema(ctx); err != nil {
			status.PayoutSchemaReason = err.Error()
		} else {
			status.PayoutSchemaReady = true
		}
		status.Phase = "scheduled"
		if !now.Before(policy.Cutoff) {
			status.Phase = "sn_earning_legacy_settlement"
		}
	}
	// This reports schedule/schema admission, not processor or database uptime.
	// Accepted attempts continue to use their original identity during a hold.
	status.NewLegacySubmissionsAdmitted = status.EarningBoundaryReady && (policy == nil || status.PayoutSchemaReady)
	cfg := stConfig()
	if cfg != nil {
		identity := stPayoutIdentity(cfg)
		status.Selected = &identity
	}
	if err := stPayoutAdmissionAt(ctx, cfg, now); err != nil {
		status.ReadinessReason = err.Error()
	} else if cfg.Profile == "mainnet" {
		status.NewMainnetWritesAdmitted = true
		status.ReadinessReason = "configured identity matches declared readiness; chain and deployment qualification remain separate"
	}
	return status, nil
}
