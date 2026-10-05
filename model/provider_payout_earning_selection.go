// Earning selection comes from the prepared public schedule, independently of
// an artifact publisher's proposed census or whole-work witness.
package model

import (
	"context"

	"github.com/urfoundation/sn/payoutartifact"
	"github.com/urnetwork/server"
)

// Companion and predecessor readers admit the same immutable asset boundary
// as the actual payout producer. A readiness-only edit cannot change this pin.
func GetProviderPayoutEarningSelection(ctx context.Context) (*payoutartifact.WholeWorkEarningSelection, error) {
	policy, err := server.LoadProviderPayoutEarningPolicy(ctx)
	if err != nil {
		return nil, err
	}
	return providerPayoutEarningSelection(policy)
}

// The caller already retained this exact policy after database admission.
func providerPayoutEarningSelection(policy *server.ProviderPayoutTransition) (*payoutartifact.WholeWorkEarningSelection, error) {
	if policy == nil {
		return nil, nil
	}
	digest, err := policy.EarningIdentitySha256()
	if err != nil {
		return nil, err
	}
	return &payoutartifact.WholeWorkEarningSelection{StartTime: policy.Cutoff, PolicyHash: "sha256:" + digest}, nil
}
