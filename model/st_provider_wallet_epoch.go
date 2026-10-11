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
	"github.com/urnetwork/glog"
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

// One hotkey delegation's outcome at the epoch, shared by the network's
// providers: the resolved wallet of the first provider that fell back to it,
// and the global consent original that names its coldkey.
type stHotkeyEarningChain struct {
	wallet   *protocol.EarningWallet
	original protocol.HotkeyWalletMappingConsent
	err      error
}

// The signed roster selects each exact history head. Every retained original
// is verified before selecting the latest consent effective at the requested
// epoch; a future rotation never replaces the current epoch's wallet. A
// provider's own consent effective at the epoch wins; a provider chain that is
// absent from the roster or has no consent effective at the epoch falls back
// to its network's chain, and a network chain that is absent or not effective
// falls back to the network's hotkey delegation
// (protocol.SelectEarningWalletWithHotkey, shared with the independent
// verifier). Missing authority or any refused member returns no partial
// wallet map.
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
	heads := stProviderWalletHeads{providers: authority.ExpectedProviders, network: authority.NetworkWallet, delegation: authority.HotkeyDelegation}
	return selectStProviderWalletsForEpoch(ctx, expected.ClientKeyRootSigner, scope, heads, false)
}

// The exact history heads one selection reads: the signed roster's pins, or,
// before a roster signer is provisioned, the newest retained originals.
type stProviderWalletHeads struct {
	providers  []payoutartifact.WholeWorkExpectedProvider
	network    func(networkId [16]byte) (payoutartifact.WholeWorkNetworkWallet, bool)
	delegation func(networkId [16]byte) (payoutartifact.WholeWorkHotkeyDelegation, bool)
}

// The protocol's own verdicts on retained evidence. A database or transport
// failure carries none of them and fails the whole selection for a retry.
func stWalletMappingRefusal(err error) bool {
	return errors.Is(err, protocol.ErrWalletMappingUnavailable) || errors.Is(err, protocol.ErrWalletMappingIntegrity) || errors.Is(err, protocol.ErrWalletMappingCapacity) || errors.Is(err, protocol.ErrWalletMappingNotEffective)
}

// Every head is verified the same way whichever source pinned it. With
// unmapRefusals, a provider whose evidence is absent, not effective or refused
// is left out of the map (unmapped); otherwise any refusal fails the selection.
func selectStProviderWalletsForEpoch(ctx context.Context, rootSigner common.Address, scope StProviderWalletEpochScope, heads stProviderWalletHeads, unmapRefusals bool) (map[server.Id]*StProviderWallet, error) {
	unmapped := func(clientId [16]byte, err error) bool {
		if !unmapRefusals || ctx.Err() != nil || !stWalletMappingRefusal(err) {
			return false
		}
		if !errors.Is(err, protocol.ErrWalletMappingAbsent) && !errors.Is(err, protocol.ErrWalletMappingNotEffective) {
			glog.Infof("[st]epoch %d provider %s wallet evidence refused: %v\n", scope.Epoch, server.Id(clientId), err)
		}
		return true
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
		if err := protocol.VerifyProspectiveWalletMapping(ctx, mapping, rootSigner, scope.Start.Number, scope.StartTime.Unix()); err != nil {
			return nil, protocol.WalletMappingConsent{}, err
		}
		return protocol.ProviderEarningWallet(mapping), originals[mapping.Statement.Generation-1], nil
	}
	// The roster's pinned network head, the complete retained chain through
	// it, the consent effective at the epoch and the same prospective gate as a
	// provider consent. A network the roster pins no chain for is absent.
	networkChain := func(networkId [16]byte) *stNetworkEarningChain {
		pinned, ok := heads.network(networkId)
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
		if err := protocol.VerifyProspectiveNetworkWalletMapping(ctx, mapping, rootSigner, scope.Start.Number, scope.StartTime.Unix()); err != nil {
			return &stNetworkEarningChain{err: err}
		}
		return &stNetworkEarningChain{mapping: mapping, original: originals[mapping.Statement.Generation-1]}
	}
	// The roster's pinned delegation head, the complete delegation chain
	// through it and the global chain through the consent head of the
	// delegation effective at the epoch, resolved by the shared protocol
	// function with the same prospective gate. A network the roster pins no
	// delegation for is absent.
	hotkeyChain := func(networkId [16]byte, clientId [16]byte) *stHotkeyEarningChain {
		pinned, ok := heads.delegation(networkId)
		if !ok {
			return &stHotkeyEarningChain{err: protocol.WalletMappingAbsentError()}
		}
		head, err := hex.DecodeString(pinned.DelegationHeadHash)
		if err != nil || len(head) != 32 || hex.EncodeToString(head) != pinned.DelegationHeadHash {
			return &stHotkeyEarningChain{err: protocol.ErrWalletMappingIntegrity}
		}
		delegations, err := ReadHotkeyNetworkDelegationHistory(ctx, scope.Domain, server.Id(networkId), pinned.DelegationGeneration, [32]byte(head))
		if err != nil {
			return &stHotkeyEarningChain{err: err}
		}
		expectation := protocol.HotkeyNetworkDelegationHistoryExpectation{Domain: scope.Domain, NetworkId: networkId, HeadHash: [32]byte(head), Generation: pinned.DelegationGeneration, Epoch: scope.Epoch}
		// the delegation effective at the epoch names the global head to read
		delegation, err := protocol.VerifyHotkeyNetworkDelegationHistory(ctx, delegations, expectation)
		if err != nil {
			return &stHotkeyEarningChain{err: err}
		}
		consents, err := ReadHotkeyWalletMappingHistory(ctx, scope.Domain.HotkeySubnet(), delegation.Statement.Hotkey, delegation.Statement.ConsentGeneration, delegation.Statement.ConsentHeadHash)
		if err != nil {
			return &stHotkeyEarningChain{err: err}
		}
		wallet, err := protocol.ResolveHotkeyEarningWallet(ctx, clientId, protocol.HotkeyEarningWalletEvidence{Delegations: delegations, DelegationExpected: expectation, Consents: consents}, rootSigner, scope.Start.Number, scope.StartTime.Unix())
		if err != nil {
			return &stHotkeyEarningChain{err: err}
		}
		if wallet == nil || wallet.ConsentGeneration == 0 || uint64(len(consents)) < wallet.ConsentGeneration {
			return &stHotkeyEarningChain{err: protocol.ErrWalletMappingIntegrity}
		}
		return &stHotkeyEarningChain{wallet: wallet, original: consents[wallet.ConsentGeneration-1]}
	}
	wallets := make(map[server.Id]*StProviderWallet, len(heads.providers))
	// each network chain and delegation is read and verified at most once, and
	// only when a provider of the network falls back to it
	networkIdChains := map[[16]byte]*stNetworkEarningChain{}
	hotkeyNetworkIdChains := map[[16]byte]*stHotkeyEarningChain{}
	for _, provider := range heads.providers {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		own, ownOriginal, ownErr := providerChain(provider)
		var networkOriginal protocol.WalletMappingConsent
		var hotkeyOriginal protocol.HotkeyWalletMappingConsent
		earning, err := protocol.SelectEarningWalletWithHotkey(own, ownErr, func() (*protocol.EarningWallet, error) {
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
		}, func() (*protocol.EarningWallet, error) {
			chain := hotkeyNetworkIdChains[provider.NetworkId]
			if chain == nil {
				chain = hotkeyChain(provider.NetworkId, provider.ClientId)
				hotkeyNetworkIdChains[provider.NetworkId] = chain
			}
			if chain.err != nil {
				return nil, chain.err
			}
			hotkeyOriginal = chain.original
			// the resolution names its client only; the rest is the network's
			wallet := *chain.wallet
			wallet.ClientId = provider.ClientId
			return &wallet, nil
		})
		if err != nil {
			if unmapped(provider.ClientId, err) {
				continue
			}
			return nil, err
		}
		original := ownOriginal
		switch earning.Mode {
		case protocol.EarningWalletModeNetwork:
			original = networkOriginal
		case protocol.EarningWalletModeHotkey:
			// the global consent the coldkey signed names the earning wallet
			original = protocol.WalletMappingConsent{Message: hotkeyOriginal.Message, Signature: hotkeyOriginal.ColdkeySignature}
		}
		if earning.ClientId != provider.ClientId || earning.NetworkId != provider.NetworkId {
			if unmapped(provider.ClientId, protocol.ErrWalletMappingIntegrity) {
				continue
			}
			return nil, protocol.ErrWalletMappingIntegrity
		}
		address, err := ss58.Encode(earning.Coldkey, ss58.BittensorPrefix)
		if err != nil {
			err = errors.Join(protocol.ErrWalletMappingIntegrity, err)
			if unmapped(provider.ClientId, err) {
				continue
			}
			return nil, err
		}
		message := original.Message
		signature := "0x" + hex.EncodeToString(original.Signature[:])
		clientId := server.Id(provider.ClientId)
		// SetTime stays unknown: issuance is not the original acceptance time,
		// and the account projection's timestamp has no selection authority.
		resolution := &StPayoutWalletResolution{
			ClientId:                    clientId,
			NetworkId:                   server.Id(provider.NetworkId),
			Mode:                        earning.Mode,
			Coldkey:                     earning.Coldkey,
			ConsentHash:                 earning.OriginalHash,
			ConsentGeneration:           earning.Generation,
			HeadHash:                    earning.HeadHash,
			HeadGeneration:              earning.HeadGeneration,
			Hotkey:                      earning.Hotkey,
			HotkeyConsentHash:           earning.ConsentOriginalHash,
			HotkeyConsentGeneration:     earning.ConsentGeneration,
			HotkeyConsentHeadHash:       earning.ConsentHeadHash,
			HotkeyConsentHeadGeneration: earning.ConsentHeadGeneration,
		}
		wallets[clientId] = &StProviderWallet{ClientId: clientId, NetworkId: server.Id(provider.NetworkId), ColdkeySs58: address, ColdkeyPubkey: earning.Coldkey, OriginalMessage: &message, OriginalSignature: &signature, Resolution: resolution}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return wallets, nil
}
