// Payout wallets come from original consent effective in the independently
// approved epoch. The current account projection cannot select an earning key.
package model

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/urfoundation/sn/payoutartifact"
	"github.com/urfoundation/sn/protocol"
	"github.com/urfoundation/sn/ss58"
	"github.com/urnetwork/server"
)

// The caller obtains these values from its authenticated closed-epoch read,
// independently of the roster, wallet history and mutable account directory.
type StProviderWalletEpochScope struct {
	Domain    protocol.ClientKeyHistoryDomain
	Epoch     uint64
	Start     payoutartifact.Boundary
	End       payoutartifact.Boundary
	StartTime time.Time
}

// The signed roster selects each exact history head. Every retained original
// is verified before selecting the latest consent effective at the requested
// epoch; a future rotation never replaces the current epoch's wallet. Missing
// authority or any refused member returns no partial wallet map.
func GetStProviderWalletsForEpoch(ctx context.Context, authorityOriginal []byte, expected payoutartifact.WholeWorkExpectation, scope StProviderWalletEpochScope) (walletClientIds map[server.Id]*StProviderWallet, resultErr error) {
	defer providerWorkRecover(&resultErr)
	if ctx == nil || len(authorityOriginal) == 0 || expected.AuthoritySigner == (common.Address{}) || expected.ClientKeyRootSigner == (common.Address{}) || scope.StartTime.Unix() <= 0 {
		return nil, protocol.ErrWalletMappingUnavailable
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := scope.Domain.Validate(); err != nil {
		return nil, errors.Join(protocol.ErrWalletMappingIntegrity, err)
	}
	ctx, cancel := context.WithTimeout(ctx, 300*time.Second)
	defer cancel()
	authority, err := payoutartifact.DecodeWholeWorkAuthority(ctx, authorityOriginal, expected.AuthoritySigner)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return nil, err
		}
		if errors.Is(err, payoutartifact.ErrClosedWorkCapacity) {
			return nil, errors.Join(protocol.ErrWalletMappingCapacity, err)
		}
		return nil, errors.Join(protocol.ErrWalletMappingIntegrity, err)
	}
	hash := sha256.Sum256(authorityOriginal)
	if expected.AuthorityHash != "" && expected.AuthorityHash != "sha256:"+hex.EncodeToString(hash[:]) || authority.Domain != scope.Domain || authority.Epoch != scope.Epoch || authority.Start != scope.Start || authority.End != scope.End {
		return nil, protocol.ErrWalletMappingIntegrity
	}
	if authority.ExpectedProviders == nil {
		return nil, protocol.ErrWalletMappingUnavailable
	}
	wallets := make(map[server.Id]*StProviderWallet, len(authority.ExpectedProviders))
	for _, provider := range authority.ExpectedProviders {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if provider.WalletHeadHash == "" || provider.WalletGeneration == 0 {
			return nil, protocol.ErrWalletMappingUnavailable
		}
		head, err := hex.DecodeString(provider.WalletHeadHash)
		if err != nil || len(head) != 32 || hex.EncodeToString(head) != provider.WalletHeadHash {
			return nil, protocol.ErrWalletMappingIntegrity
		}
		clientId := server.Id(provider.ClientId)
		originals, err := ReadWalletMappingHistory(ctx, scope.Domain, clientId, provider.WalletGeneration, [32]byte(head))
		if err != nil {
			return nil, err
		}
		mapping, err := protocol.VerifyWalletMappingHistory(ctx, originals, protocol.WalletMappingHistoryExpectation{Domain: scope.Domain, ClientId: provider.ClientId, HeadHash: [32]byte(head), Generation: provider.WalletGeneration, Epoch: scope.Epoch})
		if err != nil {
			return nil, err
		}
		if mapping.Statement.NetworkId != provider.NetworkId {
			return nil, protocol.ErrWalletMappingIntegrity
		}
		if err := protocol.VerifyProspectiveWalletMapping(ctx, mapping, expected.ClientKeyRootSigner, scope.Start.Number, scope.StartTime.Unix()); err != nil {
			return nil, err
		}
		address, err := ss58.Encode(mapping.Statement.Coldkey, ss58.BittensorPrefix)
		if err != nil {
			return nil, errors.Join(protocol.ErrWalletMappingIntegrity, err)
		}
		original := originals[mapping.Statement.Generation-1]
		signature := "0x" + hex.EncodeToString(original.Signature[:])
		// SetTime stays unknown: issuance is not the original acceptance time,
		// and the account projection's timestamp has no selection authority.
		wallets[clientId] = &StProviderWallet{ClientId: clientId, NetworkId: server.Id(provider.NetworkId), ColdkeySs58: address, ColdkeyPubkey: mapping.Statement.Coldkey, OriginalMessage: &original.Message, OriginalSignature: &signature}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return wallets, nil
}
