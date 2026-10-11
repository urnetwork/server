// Production payout wallet selection shares the independently retained roster
// and authenticated epoch used by the complete work census.
package controller

import (
	"context"
	"crypto/sha256"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/urfoundation/sn/payoutartifact"
	"github.com/urfoundation/sn/protocol"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// Missing independent wallet authority holds this payout only. It cannot
// borrow the current directory, a legacy login or a later projection; startup
// and the existing account-display and legacy-payment APIs remain separate.
func stProviderWalletsForEpoch(ctx context.Context, cfg *StConfig, approved *stProviderWorkAuthority, epoch *StPayoutEpochAuthority) (map[server.Id]*model.StProviderWallet, error) {
	if ctx == nil || cfg == nil || cfg.RootKey == nil || approved == nil || epoch == nil {
		return nil, protocol.ErrWalletMappingUnavailable
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if cfg.Netuid == 0 || cfg.Netuid > 65535 || cfg.PolicyHash != epoch.PolicyHash || approved.Expectation.ClientKeyRootSigner != crypto.PubkeyToAddress(cfg.RootKey.PublicKey) {
		return nil, protocol.ErrWalletMappingIntegrity
	}
	return model.GetStProviderWalletsForEpoch(ctx, approved.Raw, approved.Expectation, stProviderWalletEpochScope(cfg, epoch))
}

// Before provider_work.yml is provisioned there is no roster to pin heads. The
// observed providers' newest retained chains, each signed by its coldkey and
// issued under the configured root key, select wallets under the roster's
// precedence, verification and prospective gate. A provider without accepted
// evidence stays unmapped and is excluded rather than holding the payout; the
// account projection still selects nothing.
func stRetainedProviderWalletsForEpoch(ctx context.Context, cfg *StConfig, epoch *StPayoutEpochAuthority, usages []*model.StProviderUsage) (map[server.Id]*model.StProviderWallet, error) {
	if ctx == nil || cfg == nil || cfg.RootKey == nil || epoch == nil {
		return nil, protocol.ErrWalletMappingUnavailable
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if cfg.Netuid == 0 || cfg.Netuid > 65535 || cfg.PolicyHash != epoch.PolicyHash {
		return nil, protocol.ErrWalletMappingIntegrity
	}
	providers := make([]model.StProviderWalletIdentity, len(usages))
	for index, usage := range usages {
		if usage == nil {
			return nil, protocol.ErrWalletMappingIntegrity
		}
		providers[index] = model.StProviderWalletIdentity{ClientId: usage.ClientId, NetworkId: usage.NetworkId}
	}
	return model.GetStRetainedProviderWalletsForEpoch(ctx, crypto.PubkeyToAddress(cfg.RootKey.PublicKey), stProviderWalletEpochScope(cfg, epoch), providers)
}

// The configured operator domain under the authenticated epoch's policy and
// boundaries, shared by both head sources.
func stProviderWalletEpochScope(cfg *StConfig, epoch *StPayoutEpochAuthority) model.StProviderWalletEpochScope {
	domain := protocol.ClientKeyHistoryDomain{ChainID: cfg.ChainId, GenesisHash: cfg.GenesisHash, Netuid: uint16(cfg.Netuid), Coordinator: cfg.ContractAddress, SettlementVault: cfg.SettlementVault, DeploymentIDHash: sha256.Sum256([]byte(cfg.DeploymentId)), PolicyHash: epoch.PolicyHash, NoID: cfg.NoId}
	return model.StProviderWalletEpochScope{Domain: domain, Epoch: epoch.Epoch, Start: payoutartifact.Boundary{Number: epoch.Start.Block, Hash: common.Hash(epoch.Start.Hash).Hex()}, End: payoutartifact.Boundary{Number: epoch.End.Block, Hash: common.Hash(epoch.End.Hash).Hex()}, StartTime: epoch.StartTime}
}
