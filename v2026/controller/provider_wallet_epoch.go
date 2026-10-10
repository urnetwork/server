// Production payout wallet selection shares the independently retained roster
// and authenticated epoch used by the complete work census.
package controller

import (
	"context"
	"crypto/sha256"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/urfoundation/sn/v2026/payoutartifact"
	"github.com/urfoundation/sn/v2026/protocol"
	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
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
	domain := protocol.ClientKeyHistoryDomain{ChainID: cfg.ChainId, GenesisHash: cfg.GenesisHash, Netuid: uint16(cfg.Netuid), Coordinator: cfg.ContractAddress, SettlementVault: cfg.SettlementVault, DeploymentIDHash: sha256.Sum256([]byte(cfg.DeploymentId)), PolicyHash: epoch.PolicyHash, NoID: cfg.NoId}
	scope := model.StProviderWalletEpochScope{Domain: domain, Epoch: epoch.Epoch, Start: payoutartifact.Boundary{Number: epoch.Start.Block, Hash: common.Hash(epoch.Start.Hash).Hex()}, End: payoutartifact.Boundary{Number: epoch.End.Block, Hash: common.Hash(epoch.End.Hash).Hex()}, StartTime: epoch.StartTime}
	return model.GetStProviderWalletsForEpoch(ctx, approved.Raw, approved.Expectation, scope)
}
