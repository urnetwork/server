// The independent original roster owns the provider universe before any
// wallet, reliability or binding query. Current SQL usage cannot enroll an SDK.
package controller

import (
	"context"
	"crypto/sha256"
	"errors"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/urfoundation/sn/v2026/payoutartifact"
	"github.com/urfoundation/sn/v2026/protocol"
	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
)

// One retained authority and independent pin set are used throughout a payout;
// a second mutable configuration read cannot silently select another universe.
type stProviderWorkAuthority struct {
	Raw         []byte
	Authority   payoutartifact.WholeWorkAuthority
	Expectation payoutartifact.WholeWorkExpectation
	// Only the checked operator/epoch identity is needed before payout signing.
	// It binds signed source lookup without deriving a deployment from a hash.
	Artifact *payoutartifact.Artifact
}

// Absence remains optional unknown. Returned authority must match the actual
// finalized epoch, operator domain, public request key and independent root.
func stLoadProviderWorkAuthority(ctx context.Context, cfg *StConfig, epoch *StPayoutEpochAuthority) (*stProviderWorkAuthority, error) {
	domainHash, approver, expected, err := LoadProviderWorkAuthorityPolicy()
	if errors.Is(err, server.ErrResourceNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	expected.EarningSelection, err = model.GetProviderPayoutEarningSelection(ctx)
	if err != nil {
		return nil, err
	}
	if cfg == nil || epoch == nil || cfg.Netuid == 0 || cfg.Netuid > 65535 || cfg.ArtifactKey == nil {
		return nil, model.ErrProviderWorkInvalid
	}
	domain := protocol.ClientKeyHistoryDomain{ChainID: cfg.ChainId, GenesisHash: cfg.GenesisHash, Netuid: uint16(cfg.Netuid), Coordinator: cfg.ContractAddress, SettlementVault: cfg.SettlementVault, DeploymentIDHash: sha256.Sum256([]byte(cfg.DeploymentId)), PolicyHash: epoch.PolicyHash, NoID: cfg.NoId}
	actual, err := domain.Digest()
	if err != nil || actual != domainHash || expected.AuthoritySigner == crypto.PubkeyToAddress(cfg.ArtifactKey.PublicKey) {
		return nil, errors.Join(model.ErrProviderWorkConflict, err)
	}
	raw, authority, err := model.GetProviderWorkAuthority(ctx, domainHash, epoch.Epoch, expected.AuthoritySigner)
	if errors.Is(err, model.ErrProviderWorkMissing) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	start := payoutartifact.Boundary{Number: epoch.Start.Block, Hash: common.Hash(epoch.Start.Hash).Hex()}
	end := payoutartifact.Boundary{Number: epoch.End.Block, Hash: common.Hash(epoch.End.Hash).Hex()}
	if authority.Domain != domain || authority.Start != start || authority.End != end || authority.RequestPublicKey != approver {
		return nil, model.ErrProviderWorkConflict
	}
	artifact := &payoutartifact.Artifact{DeploymentID: cfg.DeploymentId, ChainID: cfg.ChainId, GenesisHash: common.Hash(cfg.GenesisHash).Hex(), Netuid: uint16(cfg.Netuid), Coordinator: cfg.ContractAddress, SettlementVault: cfg.SettlementVault, Epoch: epoch.Epoch, NoID: cfg.NoId, PolicyHash: common.Hash(epoch.PolicyHash).Hex(), Start: start, End: end, Signer: crypto.PubkeyToAddress(cfg.ArtifactKey.PublicKey)}
	return &stProviderWorkAuthority{Raw: raw, Authority: authority, Expectation: expected, Artifact: artifact}, nil
}

// Add explicit zero rows only from an independently signed prospective roster.
// Every observed usage identity must be in that exact roster; missing original
// authority preserves the legacy unknown path without inventing completeness.
func stProviderWorkUsages(ctx context.Context, approved *stProviderWorkAuthority, usages []*model.StProviderUsage) ([]*model.StProviderUsage, error) {
	if approved == nil || approved.Authority.ExpectedProviders == nil {
		return usages, nil
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	observed := make(map[server.Id]*model.StProviderUsage, len(usages))
	for _, usage := range usages {
		if usage == nil || usage.ClientId == (server.Id{}) || usage.NetworkId == (server.Id{}) || usage.PayoutByteCount < 0 || observed[usage.ClientId] != nil {
			return nil, model.ErrProviderWorkConflict
		}
		observed[usage.ClientId] = usage
	}
	result := make([]*model.StProviderUsage, 0, len(approved.Authority.ExpectedProviders))
	for _, provider := range approved.Authority.ExpectedProviders {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		clientId, networkId := server.Id(provider.ClientId), server.Id(provider.NetworkId)
		usage := &model.StProviderUsage{ClientId: clientId, NetworkId: networkId}
		if prior := observed[clientId]; prior != nil {
			if prior.NetworkId != networkId {
				return nil, model.ErrProviderWorkConflict
			}
			usage.PayoutByteCount = prior.PayoutByteCount
			delete(observed, clientId)
		}
		result = append(result, usage)
	}
	if len(observed) != 0 {
		return nil, model.ErrProviderWorkConflict
	}
	return result, nil
}
