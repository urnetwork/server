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

// One network chain's outcome at the epoch, shared by the network's providers.
type stNetworkEarningChain struct {
	mapping  *protocol.VerifiedNetworkWalletMapping
	original protocol.WalletMappingConsent
	err      error
}

// The signed roster selects each exact history head. Every retained original
// is verified before selecting the latest consent effective at the requested
// epoch; a future rotation never replaces the current epoch's wallet. A
// provider's own consent effective at the epoch wins; a provider chain that is
// absent from the roster or has no consent effective at the epoch falls back
// to its network's chain (protocol.SelectEarningWallet, shared with the
// independent verifier). Missing authority or any refused member returns no
// partial wallet map.
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
	// The provider's own chain at the epoch. The roster pinning no head is
	// absent; a verified chain without a consent effective at the epoch is not
	// effective; both may fall back. Every other refusal is final for the epoch.
	providerChain := func(provider payoutartifact.WholeWorkExpectedProvider) (*protocol.EarningWallet, protocol.WalletMappingConsent, error) {
		if provider.WalletHeadHash == "" && provider.WalletGeneration == 0 {
			return nil, protocol.WalletMappingConsent{}, protocol.WalletMappingAbsentError()
		}
		if provider.WalletHeadHash == "" || provider.WalletGeneration == 0 {
			return nil, protocol.WalletMappingConsent{}, protocol.ErrWalletMappingIntegrity
		}
		head, err := hex.DecodeString(provider.WalletHeadHash)
		if err != nil || len(head) != 32 || hex.EncodeToString(head) != provider.WalletHeadHash {
			return nil, protocol.WalletMappingConsent{}, protocol.ErrWalletMappingIntegrity
		}
		originals, err := ReadWalletMappingHistory(ctx, scope.Domain, server.Id(provider.ClientId), provider.WalletGeneration, [32]byte(head))
		if err != nil {
			return nil, protocol.WalletMappingConsent{}, err
		}
		mapping, err := protocol.VerifyWalletMappingHistory(ctx, originals, protocol.WalletMappingHistoryExpectation{Domain: scope.Domain, ClientId: provider.ClientId, HeadHash: [32]byte(head), Generation: provider.WalletGeneration, Epoch: scope.Epoch})
		if err != nil {
			return nil, protocol.WalletMappingConsent{}, err
		}
		if mapping.Statement.NetworkId != provider.NetworkId {
			return nil, protocol.WalletMappingConsent{}, protocol.ErrWalletMappingIntegrity
		}
		if err := protocol.VerifyProspectiveWalletMapping(ctx, mapping, expected.ClientKeyRootSigner, scope.Start.Number, scope.StartTime.Unix()); err != nil {
			return nil, protocol.WalletMappingConsent{}, err
		}
		return protocol.ProviderEarningWallet(mapping), originals[mapping.Statement.Generation-1], nil
	}
	// The roster's pinned network head, the complete retained chain through
	// it, the consent effective at the epoch and the same prospective gate as a
	// provider consent. A network the roster pins no chain for is absent.
	networkChain := func(networkId [16]byte) *stNetworkEarningChain {
		pinned, ok := authority.NetworkWallet(networkId)
		if !ok {
			return &stNetworkEarningChain{err: protocol.WalletMappingAbsentError()}
		}
		head, err := hex.DecodeString(pinned.WalletHeadHash)
		if err != nil || len(head) != 32 || hex.EncodeToString(head) != pinned.WalletHeadHash {
			return &stNetworkEarningChain{err: protocol.ErrWalletMappingIntegrity}
		}
		originals, err := ReadNetworkWalletMappingHistory(ctx, scope.Domain, server.Id(networkId), pinned.WalletGeneration, [32]byte(head))
		if err != nil {
			return &stNetworkEarningChain{err: err}
		}
		mapping, err := protocol.VerifyNetworkWalletMappingHistory(ctx, originals, protocol.NetworkWalletMappingHistoryExpectation{Domain: scope.Domain, NetworkId: networkId, HeadHash: [32]byte(head), Generation: pinned.WalletGeneration, Epoch: scope.Epoch})
		if err != nil {
			return &stNetworkEarningChain{err: err}
		}
		if err := protocol.VerifyProspectiveNetworkWalletMapping(ctx, mapping, expected.ClientKeyRootSigner, scope.Start.Number, scope.StartTime.Unix()); err != nil {
			return &stNetworkEarningChain{err: err}
		}
		return &stNetworkEarningChain{mapping: mapping, original: originals[mapping.Statement.Generation-1]}
	}
	wallets := make(map[server.Id]*StProviderWallet, len(authority.ExpectedProviders))
	// each network chain is read and verified at most once, and only when a
	// provider of the network falls back to it
	networkIdChains := map[[16]byte]*stNetworkEarningChain{}
	for _, provider := range authority.ExpectedProviders {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		own, ownOriginal, ownErr := providerChain(provider)
		var networkOriginal protocol.WalletMappingConsent
		earning, err := protocol.SelectEarningWallet(own, ownErr, func() (*protocol.EarningWallet, error) {
			chain := networkIdChains[provider.NetworkId]
			if chain == nil {
				chain = networkChain(provider.NetworkId)
				networkIdChains[provider.NetworkId] = chain
			}
			if chain.err != nil {
				return nil, chain.err
			}
			networkOriginal = chain.original
			return protocol.NetworkEarningWallet(provider.ClientId, chain.mapping), nil
		})
		if err != nil {
			return nil, err
		}
		original := ownOriginal
		if earning.Mode == protocol.EarningWalletModeNetwork {
			original = networkOriginal
		}
		if earning.ClientId != provider.ClientId || earning.NetworkId != provider.NetworkId {
			return nil, protocol.ErrWalletMappingIntegrity
		}
		address, err := ss58.Encode(earning.Coldkey, ss58.BittensorPrefix)
		if err != nil {
			return nil, errors.Join(protocol.ErrWalletMappingIntegrity, err)
		}
		message := original.Message
		signature := "0x" + hex.EncodeToString(original.Signature[:])
		clientId := server.Id(provider.ClientId)
		// SetTime stays unknown: issuance is not the original acceptance time,
		// and the account projection's timestamp has no selection authority.
		resolution := &StPayoutWalletResolution{
			ClientId:          clientId,
			NetworkId:         server.Id(provider.NetworkId),
			Mode:              earning.Mode,
			Coldkey:           earning.Coldkey,
			ConsentHash:       earning.OriginalHash,
			ConsentGeneration: earning.Generation,
			HeadHash:          earning.HeadHash,
			HeadGeneration:    earning.HeadGeneration,
		}
		wallets[clientId] = &StProviderWallet{ClientId: clientId, NetworkId: server.Id(provider.NetworkId), ColdkeySs58: address, ColdkeyPubkey: earning.Coldkey, OriginalMessage: &message, OriginalSignature: &signature, Resolution: resolution}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return wallets, nil
}
