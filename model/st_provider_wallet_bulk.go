// Bulk epoch wallet selection for the settlement v2 ledger build
// (VERSION2-FINAL.md §8.2). These are the counterparts of
// GetStProviderWalletsForEpoch and GetStRetainedProviderWalletsForEpoch at a
// million providers: every chain is verified once per head and stored
// (st_wallet_consent_verified.go), and an epoch selects from the stored fields
// in bulk under the same precedence, gates and refusals. Each mapped provider
// returns the resolution v1 records for it; the selected original stays
// retained under the resolution's consent hash.
package model

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"

	"github.com/ethereum/go-ethereum/common"
	"github.com/urfoundation/sn/payoutartifact"
	"github.com/urfoundation/sn/protocol"
	"github.com/urnetwork/glog"
	"github.com/urnetwork/server"
)

// Providers per selection chunk. Network and delegation outcomes carry across
// chunks, so a chunk bounds only the provider chains held at once.
const stProviderWalletBulkChunk = 16384

// One pinned head, or the refusal for a missing or malformed pin.
type stWalletPin struct {
	head stWalletChainHead
	err  error
}

// Where one selection reads its pinned heads: a signed roster, or the newest
// retained originals. Providers are pinned in input order, from the input
// index start; a network without a pin is absent.
type stWalletPins struct {
	providers   func(ctx context.Context, start int, providers []StProviderWalletIdentity) []stWalletPin
	networks    func(ctx context.Context, networkIds []server.Id) map[server.Id]stWalletPin
	delegations func(ctx context.Context, networkIds []server.Id) map[server.Id]stWalletPin
}

// GetStProviderWalletsForEpoch at a million providers: the signed roster is
// decoded and its pinned heads read and refused as v1 reads them, and every
// provider's resolution is returned, or the first refusal in roster order.
// The caller's context bounds the selection; v1's 300-second bound, which a
// million chains outgrow, does not apply.
func GetStProviderWalletResolutionsForEpoch(ctx context.Context, authorityOriginal []byte, expected payoutartifact.WholeWorkExpectation, scope StProviderWalletEpochScope) (resolutions map[server.Id]*StPayoutWalletResolution, resultErr error) {
	defer providerWorkRecover(&resultErr)
	if ctx == nil || len(authorityOriginal) == 0 || expected.AuthoritySigner == (common.Address{}) || expected.ClientKeyRootSigner == (common.Address{}) || scope.StartTime.Unix() <= 0 {
		return nil, protocol.ErrWalletMappingUnavailable
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	scopeHashes, err := newStWalletScopeHashes(scope.Domain)
	if err != nil {
		return nil, err
	}
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
	providers := make([]StProviderWalletIdentity, len(authority.ExpectedProviders))
	for index, provider := range authority.ExpectedProviders {
		providers[index] = StProviderWalletIdentity{ClientId: server.Id(provider.ClientId), NetworkId: server.Id(provider.NetworkId)}
	}
	// the first pin of a network wins, as the roster's own lookups return it
	networkPins := map[server.Id]stWalletPin{}
	for _, wallet := range authority.NetworkWallets {
		if _, ok := networkPins[server.Id(wallet.NetworkId)]; !ok {
			networkPins[server.Id(wallet.NetworkId)] = stRosterWalletPin(wallet.NetworkId, wallet.WalletHeadHash, wallet.WalletGeneration)
		}
	}
	delegationPins := map[server.Id]stWalletPin{}
	for _, delegation := range authority.HotkeyDelegations {
		if _, ok := delegationPins[server.Id(delegation.NetworkId)]; !ok {
			delegationPins[server.Id(delegation.NetworkId)] = stRosterWalletPin(delegation.NetworkId, delegation.DelegationHeadHash, delegation.DelegationGeneration)
		}
	}
	pins := stWalletPins{
		providers: func(_ context.Context, start int, batch []StProviderWalletIdentity) []stWalletPin {
			pinned := make([]stWalletPin, len(batch))
			for index := range batch {
				pinned[index] = stRosterProviderPin(authority.ExpectedProviders[start+index])
			}
			return pinned
		},
		networks:    stMappedWalletPins(networkPins),
		delegations: stMappedWalletPins(delegationPins),
	}
	return selectStProviderWalletResolutionsForEpoch(ctx, expected.ClientKeyRootSigner, scope, scopeHashes, providers, pins, false)
}

// GetStRetainedProviderWalletsForEpoch at a million providers: the observed
// providers' newest retained originals stand in for a roster's pins, and a
// provider without accepted evidence, or whose evidence is refused, is
// unmapped. The caller's context bounds the selection.
func GetStRetainedProviderWalletResolutionsForEpoch(ctx context.Context, rootSigner common.Address, scope StProviderWalletEpochScope, providers []StProviderWalletIdentity) (resolutions map[server.Id]*StPayoutWalletResolution, resultErr error) {
	defer providerWorkRecover(&resultErr)
	if ctx == nil || rootSigner == (common.Address{}) || scope.StartTime.Unix() <= 0 {
		return nil, protocol.ErrWalletMappingUnavailable
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	scopeHashes, err := newStWalletScopeHashes(scope.Domain)
	if err != nil {
		return nil, err
	}
	clients := make(map[server.Id]bool, len(providers))
	for _, provider := range providers {
		if provider.ClientId == (server.Id{}) || provider.NetworkId == (server.Id{}) || clients[provider.ClientId] {
			return nil, protocol.ErrWalletMappingIntegrity
		}
		clients[provider.ClientId] = true
	}
	pins := stWalletPins{
		providers: func(ctx context.Context, _ int, batch []StProviderWalletIdentity) []stWalletPin {
			clientIds := make([]server.Id, len(batch))
			for index, provider := range batch {
				clientIds[index] = provider.ClientId
			}
			heads := readStRetainedWalletHeads(ctx, stRetainedProviderWalletHeadsSql, scopeHashes.domain, clientIds)
			pinned := make([]stWalletPin, len(batch))
			for index, provider := range batch {
				pinned[index] = stRetainedWalletPin(provider.ClientId, heads)
			}
			return pinned
		},
		networks: func(ctx context.Context, networkIds []server.Id) map[server.Id]stWalletPin {
			return stRetainedWalletPins(networkIds, readStRetainedWalletHeads(ctx, stRetainedNetworkWalletHeadsSql, scopeHashes.domain, networkIds))
		},
		delegations: func(ctx context.Context, networkIds []server.Id) map[server.Id]stWalletPin {
			return stRetainedWalletPins(networkIds, readStRetainedWalletHeads(ctx, stRetainedHotkeyDelegationHeadsSql, scopeHashes.domain, networkIds))
		},
	}
	return selectStProviderWalletResolutionsForEpoch(ctx, rootSigner, scope, scopeHashes, providers, pins, true)
}

// The stored-consent scopes of one operator domain: its digest for provider,
// network and delegation chains, and its subnet's digest for global hotkey
// chains.
type stWalletScopeHashes struct {
	domain [32]byte
	subnet [32]byte
}

func newStWalletScopeHashes(domain protocol.ClientKeyHistoryDomain) (stWalletScopeHashes, error) {
	domainHash, err := domain.Digest()
	subnetHash, subnetErr := hotkeyWalletMappingSubnetHash(domain.HotkeySubnet())
	if err := errors.Join(err, subnetErr); err != nil {
		return stWalletScopeHashes{}, errors.Join(protocol.ErrWalletMappingIntegrity, err)
	}
	return stWalletScopeHashes{domain: domainHash, subnet: subnetHash}, nil
}

// A roster's provider pin as v1 reads it: no head is absent and half a pin is
// malformed. A decoded roster already refuses half and malformed pins; the
// checks keep v1's refusals if that validation changes.
func stRosterProviderPin(provider payoutartifact.WholeWorkExpectedProvider) stWalletPin {
	if provider.WalletHeadHash == "" && provider.WalletGeneration == 0 {
		return stWalletPin{err: protocol.WalletMappingAbsentError()}
	}
	if provider.WalletHeadHash == "" || provider.WalletGeneration == 0 {
		return stWalletPin{err: protocol.ErrWalletMappingIntegrity}
	}
	return stRosterWalletPin(provider.ClientId, provider.WalletHeadHash, provider.WalletGeneration)
}

// A roster's hex head pin: malformed unless it is the exact lowercase spelling
// of 32 bytes. A generation the reader refuses is refused there.
func stRosterWalletPin(owner [16]byte, headHash string, generation uint64) stWalletPin {
	hash, err := hex.DecodeString(headHash)
	if err != nil || len(hash) != 32 || hex.EncodeToString(hash) != headHash {
		return stWalletPin{err: protocol.ErrWalletMappingIntegrity}
	}
	pin := stWalletPin{head: stWalletChainHead{generation: generation, hash: [32]byte(hash)}}
	copy(pin.head.owner[:], owner[:])
	return pin
}

func stMappedWalletPins(pins map[server.Id]stWalletPin) func(context.Context, []server.Id) map[server.Id]stWalletPin {
	return func(_ context.Context, networkIds []server.Id) map[server.Id]stWalletPin {
		mapped := make(map[server.Id]stWalletPin, len(networkIds))
		for _, networkId := range networkIds {
			if pin, ok := pins[networkId]; ok {
				mapped[networkId] = pin
			} else {
				mapped[networkId] = stWalletPin{err: protocol.WalletMappingAbsentError()}
			}
		}
		return mapped
	}
}

// The newest retained original of the owner, or absent.
func stRetainedWalletPin(owner server.Id, heads map[server.Id]stRetainedWalletHead) stWalletPin {
	head, ok := heads[owner]
	if !ok {
		return stWalletPin{err: protocol.WalletMappingAbsentError()}
	}
	pin := stWalletPin{head: stWalletChainHead{generation: head.generation, hash: head.hash}}
	copy(pin.head.owner[:], owner[:])
	return pin
}

func stRetainedWalletPins(owners []server.Id, heads map[server.Id]stRetainedWalletHead) map[server.Id]stWalletPin {
	pins := make(map[server.Id]stWalletPin, len(owners))
	for _, owner := range owners {
		pins[owner] = stRetainedWalletPin(owner, heads)
	}
	return pins
}

// A provider chain that is absent or verified with no consent effective at the
// epoch falls back; protocol.SelectEarningWallet makes the same choice.
func stWalletFallback(err error) bool {
	return errors.Is(err, protocol.ErrWalletMappingAbsent) || errors.Is(err, protocol.ErrWalletMappingNotEffective)
}

// One network's earning outcome, shared by its providers. The wallet is copied
// for each provider with that provider as its client.
type stWalletOutcome struct {
	wallet *protocol.EarningWallet
	err    error
}

// One epoch's selection inputs.
type stWalletSelection struct {
	scope       StProviderWalletEpochScope
	scopeHashes stWalletScopeHashes
	rootSigner  common.Address
	startBlock  uint64
	startUnix   int64
}

// protocol.VerifyProspective{Wallet,NetworkWallet}Mapping and
// VerifyProspectiveHotkeyNetworkDelegation on a verified consent: the exact
// operator signer, an issuance boundary before the window and an acceptance
// that expired before the window started. Each entry point has refused a zero
// signer and start time; the prospective signature verified with the chain.
func (self *stWalletSelection) gate(consent *stVerifiedWalletConsent) error {
	if self.startBlock == 0 || consent.prospectiveSigner == (common.Address{}) {
		return protocol.ErrWalletMappingUnavailable
	}
	if consent.prospectiveSigner != self.rootSigner || consent.prospectiveBlock >= self.startBlock || consent.fromEpoch <= consent.prospectiveEpoch {
		return protocol.ErrWalletMappingIntegrity
	}
	if consent.expiresAt > self.startUnix {
		return protocol.ErrWalletMappingUnavailable
	}
	return nil
}

// The chains through the pins that have heads.
func (self *stWalletSelection) chains(ctx context.Context, kind string, pins []stWalletPin) map[stWalletChainHead]*stWalletChain {
	heads := make([]stWalletChainHead, 0, len(pins))
	for _, pin := range pins {
		if pin.err == nil {
			heads = append(heads, pin.head)
		}
	}
	scopeHash := self.scopeHashes.domain
	if kind == stWalletChainHotkey {
		scopeHash = self.scopeHashes.subnet
	}
	return getStWalletChains(ctx, self.scope.Domain, scopeHash, kind, heads)
}

// The provider's own chain: the consent effective at the epoch, the network
// the epoch observed the provider on, then the prospective gate.
func (self *stWalletSelection) providerWallet(provider StProviderWalletIdentity, pin stWalletPin, chains map[stWalletChainHead]*stWalletChain) (*protocol.EarningWallet, error) {
	if pin.err != nil {
		return nil, pin.err
	}
	consent, err := chains[pin.head].at(self.scope.Epoch)
	if err != nil {
		return nil, err
	}
	if consent.networkId != [16]byte(provider.NetworkId) {
		return nil, protocol.ErrWalletMappingIntegrity
	}
	if err := self.gate(consent); err != nil {
		return nil, err
	}
	return &protocol.EarningWallet{Mode: protocol.EarningWalletModeProvider, ClientId: [16]byte(provider.ClientId), NetworkId: consent.networkId, Coldkey: consent.wallet,
		OriginalHash: consent.originalHash, Generation: consent.generation, HeadHash: pin.head.hash, HeadGeneration: pin.head.generation}, nil
}

// The network's chain: the consent effective at the epoch, then the
// prospective gate.
func (self *stWalletSelection) networkWallet(networkId server.Id, pin stWalletPin, chains map[stWalletChainHead]*stWalletChain) *stWalletOutcome {
	if pin.err != nil {
		return &stWalletOutcome{err: pin.err}
	}
	consent, err := chains[pin.head].at(self.scope.Epoch)
	if err != nil {
		return &stWalletOutcome{err: err}
	}
	if err := self.gate(consent); err != nil {
		return &stWalletOutcome{err: err}
	}
	return &stWalletOutcome{wallet: &protocol.EarningWallet{Mode: protocol.EarningWalletModeNetwork, NetworkId: [16]byte(networkId), Coldkey: consent.wallet,
		OriginalHash: consent.originalHash, Generation: consent.generation, HeadHash: pin.head.hash, HeadGeneration: pin.head.generation}}
}

// The global chain head the delegation effective at the epoch pins.
func stDelegationConsentHead(delegation *stVerifiedWalletConsent) stWalletChainHead {
	return stWalletChainHead{owner: delegation.wallet, generation: delegation.consentGeneration, hash: delegation.consentHeadHash}
}

// The network's hotkey delegation, in v1's order: the delegation effective at
// the epoch, the read of the global chain it pins, the delegation's
// prospective gate, then the global consent effective at the epoch
// (protocol.ResolveHotkeyEarningWallet). Both entry points refuse a zero
// client, which that function also refuses.
func (self *stWalletSelection) hotkeyWallet(pin stWalletPin, delegations map[stWalletChainHead]*stWalletChain, consents map[stWalletChainHead]*stWalletChain) *stWalletOutcome {
	if pin.err != nil {
		return &stWalletOutcome{err: pin.err}
	}
	delegation, err := delegations[pin.head].at(self.scope.Epoch)
	if err != nil {
		return &stWalletOutcome{err: err}
	}
	chain := consents[stDelegationConsentHead(delegation)]
	if chain.readErr != nil {
		return &stWalletOutcome{err: chain.readErr}
	}
	if err := self.gate(delegation); err != nil {
		return &stWalletOutcome{err: err}
	}
	consent, err := chain.at(self.scope.Epoch)
	if err != nil {
		return &stWalletOutcome{err: err}
	}
	return &stWalletOutcome{wallet: &protocol.EarningWallet{Mode: protocol.EarningWalletModeHotkey, NetworkId: [16]byte(pin.head.owner[:16]), Coldkey: consent.wallet,
		OriginalHash: delegation.originalHash, Generation: delegation.generation, HeadHash: pin.head.hash, HeadGeneration: pin.head.generation,
		Hotkey: delegation.wallet, ConsentOriginalHash: consent.originalHash, ConsentGeneration: consent.generation,
		ConsentHeadHash: delegation.consentHeadHash, ConsentHeadGeneration: delegation.consentGeneration}}
}

// selectStProviderWalletsForEpoch over stored verified chains, chunk by chunk
// in input order. Each chunk evaluates its providers' own chains, then the
// network chains of providers that fall back, then the delegations of
// networks that fall back, so a chain is read only when v1 would read it.
// With unmapRefusals a refused provider is unmapped; otherwise the first
// refusal in input order fails the selection.
func selectStProviderWalletResolutionsForEpoch(ctx context.Context, rootSigner common.Address, scope StProviderWalletEpochScope, scopeHashes stWalletScopeHashes, providers []StProviderWalletIdentity, pins stWalletPins, unmapRefusals bool) (map[server.Id]*StPayoutWalletResolution, error) {
	selection := &stWalletSelection{scope: scope, scopeHashes: scopeHashes, rootSigner: rootSigner, startBlock: scope.Start.Number, startUnix: scope.StartTime.Unix()}
	unmapped := func(clientId server.Id, err error) bool {
		if !unmapRefusals || ctx.Err() != nil || !stWalletMappingRefusal(err) {
			return false
		}
		if !errors.Is(err, protocol.ErrWalletMappingAbsent) && !errors.Is(err, protocol.ErrWalletMappingNotEffective) {
			glog.Infof("[st]epoch %d provider %s wallet evidence refused: %v\n", scope.Epoch, clientId, err)
		}
		return true
	}
	resolutions := make(map[server.Id]*StPayoutWalletResolution, len(providers))
	networks := map[server.Id]*stWalletOutcome{}
	hotkeys := map[server.Id]*stWalletOutcome{}
	for start := 0; start < len(providers); start += stProviderWalletBulkChunk {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		batch := providers[start:min(start+stProviderWalletBulkChunk, len(providers))]
		providerPins := pins.providers(ctx, start, batch)
		providerChains := selection.chains(ctx, stWalletChainProvider, providerPins)
		own := make([]stWalletOutcome, len(batch))
		networkIds := []server.Id{}
		for index, provider := range batch {
			own[index].wallet, own[index].err = selection.providerWallet(provider, providerPins[index], providerChains)
			if own[index].err != nil && stWalletFallback(own[index].err) && networks[provider.NetworkId] == nil {
				// a network first needed in this chunk is evaluated once
				networks[provider.NetworkId] = &stWalletOutcome{}
				networkIds = append(networkIds, provider.NetworkId)
			}
		}
		if 0 < len(networkIds) {
			networkPins := pins.networks(ctx, networkIds)
			pinList := make([]stWalletPin, len(networkIds))
			for index, networkId := range networkIds {
				pinList[index] = networkPins[networkId]
			}
			networkChains := selection.chains(ctx, stWalletChainNetwork, pinList)
			delegationIds := []server.Id{}
			for index, networkId := range networkIds {
				networks[networkId] = selection.networkWallet(networkId, pinList[index], networkChains)
				if stWalletFallback(networks[networkId].err) {
					delegationIds = append(delegationIds, networkId)
				}
			}
			if 0 < len(delegationIds) {
				delegationPins := pins.delegations(ctx, delegationIds)
				delegationList := make([]stWalletPin, len(delegationIds))
				for index, networkId := range delegationIds {
					delegationList[index] = delegationPins[networkId]
				}
				delegationChains := selection.chains(ctx, stWalletChainDelegation, delegationList)
				consentPins := []stWalletPin{}
				for _, pin := range delegationList {
					if pin.err != nil {
						continue
					}
					if delegation, err := delegationChains[pin.head].at(scope.Epoch); err == nil {
						consentPins = append(consentPins, stWalletPin{head: stDelegationConsentHead(delegation)})
					}
				}
				consentChains := selection.chains(ctx, stWalletChainHotkey, consentPins)
				for index, networkId := range delegationIds {
					hotkeys[networkId] = selection.hotkeyWallet(delegationList[index], delegationChains, consentChains)
				}
			}
		}
		for index, provider := range batch {
			earning, err := protocol.SelectEarningWalletWithHotkey(own[index].wallet, own[index].err, func() (*protocol.EarningWallet, error) {
				return stProviderEarningWallet(networks[provider.NetworkId], provider.ClientId)
			}, func() (*protocol.EarningWallet, error) {
				return stProviderEarningWallet(hotkeys[provider.NetworkId], provider.ClientId)
			})
			if err != nil {
				if unmapped(provider.ClientId, err) {
					continue
				}
				return nil, err
			}
			// v1's identity and ss58 checks hold by construction: every wallet
			// names this client and network, and the Bittensor prefix encodes
			// every 32-byte key
			resolutions[provider.ClientId] = &StPayoutWalletResolution{
				ClientId:                    provider.ClientId,
				NetworkId:                   provider.NetworkId,
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
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return resolutions, nil
}

// A network-level outcome as one provider's wallet.
func stProviderEarningWallet(outcome *stWalletOutcome, clientId server.Id) (*protocol.EarningWallet, error) {
	if outcome.err != nil {
		return nil, outcome.err
	}
	wallet := *outcome.wallet
	wallet.ClientId = [16]byte(clientId)
	return &wallet, nil
}
