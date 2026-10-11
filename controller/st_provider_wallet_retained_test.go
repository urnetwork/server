// Before provider_work.yml is provisioned, the newest retained signed consents
// select payout wallets for observed providers. Keys, networks and clients are
// synthetic.
package controller

import (
	"context"
	"errors"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/urfoundation/sn/payoutartifact"
	"github.com/urfoundation/sn/protocol"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/startifact"
)

// The retained heads select exactly what a roster pinning those heads would:
// the provider's own consent, then the network's, then the delegation.
func TestStRetainedProviderWalletsForEpochMatchesRosterPrecedence(t *testing.T) {
	snWalletProviderScopeRun(t, func(t testing.TB) {
		f := newSnHotkeyWalletFixture(t)
		hotkey, hotkeySs58 := snNetworkWalletTestKey(t, 211)
		coldkey, _ := snNetworkWalletTestKey(t, 212)
		chain := snHotkeyWalletConsentAppend(t, nil, f.rpc.domain.HotkeySubnet(), hotkey, coldkey, f.scope.Epoch, "retained")
		f.retainHotkeyChain(t, f.owner, chain)
		delegationSet, _, _ := f.hotkeyDelegation(t, f.owner, hotkey, hotkeySs58, chain, f.scope.Epoch)
		delegationAccepted := f.accept(t, f.owner, delegationSet)
		// the network consent is effective one epoch after the delegation
		networkKey, networkSs58 := snNetworkWalletTestKey(t, 213)
		networkSet, _, _ := f.networkConsent(t, f.owner, networkKey, networkSs58, f.scope.Epoch+1)
		networkAccepted := f.accept(t, f.owner, networkSet)
		providerProof, _, _ := f.consent(t, f.provider, nil)
		providerAccepted := f.accept(t, f.provider, providerProof)
		networkId := [16]byte(f.owner.NetworkId)
		pinned := []payoutartifact.WholeWorkExpectedProvider{
			{ClientId: [16]byte(*f.provider.ClientId), NetworkId: networkId, WalletHeadHash: providerAccepted.MappingHash, WalletGeneration: providerAccepted.MappingGeneration},
			{ClientId: [16]byte(*f.sibling.ClientId), NetworkId: networkId},
		}
		pinnedNetwork := []payoutartifact.WholeWorkNetworkWallet{{NetworkId: networkId, WalletHeadHash: networkAccepted.MappingHash, WalletGeneration: networkAccepted.MappingGeneration}}
		pinnedDelegation := []payoutartifact.WholeWorkHotkeyDelegation{{NetworkId: networkId, DelegationHeadHash: delegationAccepted.MappingHash, DelegationGeneration: delegationAccepted.MappingGeneration}}
		stranger := model.StProviderWalletIdentity{ClientId: server.NewId(), NetworkId: server.NewId()}
		observed := []model.StProviderWalletIdentity{
			{ClientId: *f.provider.ClientId, NetworkId: f.owner.NetworkId},
			{ClientId: *f.sibling.ClientId, NetworkId: f.owner.NetworkId},
			stranger,
		}
		later := f.scope
		later.Epoch += 1
		for _, scope := range []model.StProviderWalletEpochScope{f.scope, later} {
			authority, expected := f.hotkeyRoster(t, scope, pinned, pinnedNetwork, pinnedDelegation)
			roster, err := model.GetStProviderWalletsForEpoch(t.Context(), authority, expected, scope)
			if err != nil || len(roster) != 2 {
				t.Fatal("the pinned roster did not resolve both providers", scope.Epoch, roster, err)
			}
			retained, err := model.GetStRetainedProviderWalletsForEpoch(t.Context(), f.rpc.root, scope, observed)
			if err != nil || len(retained) != 2 || retained[stranger.ClientId] != nil {
				t.Fatal("retained heads did not resolve exactly the consenting providers", scope.Epoch, retained, err)
			}
			for clientId, want := range roster {
				got := retained[clientId]
				if got == nil || got.Resolution == nil || *got.Resolution != *want.Resolution || got.ColdkeySs58 != want.ColdkeySs58 || got.ColdkeyPubkey != want.ColdkeyPubkey || *got.OriginalMessage != *want.OriginalMessage || *got.OriginalSignature != *want.OriginalSignature {
					t.Fatal("retained heads selected another wallet than the roster", scope.Epoch, clientId, got, want)
				}
			}
		}
		if own := mustRetainedWallet(t, f, observed, f.scope, *f.provider.ClientId); own.Resolution.Mode != protocol.EarningWalletModeProvider {
			t.Fatal("the provider's own effective consent did not win", own.Resolution)
		}
		if delegated := mustRetainedWallet(t, f, observed, f.scope, *f.sibling.ClientId); delegated.Resolution.Mode != protocol.EarningWalletModeHotkey || delegated.ColdkeyPubkey != coldkey.Public().Encode() {
			t.Fatal("an absent own chain did not fall back to the delegation", delegated.Resolution)
		}
		if network := mustRetainedWallet(t, f, observed, later, *f.sibling.ClientId); network.Resolution.Mode != protocol.EarningWalletModeNetwork || network.ColdkeySs58 != networkSs58 {
			t.Fatal("the effective network consent did not win over the delegation", network.Resolution)
		}
	})
}

func mustRetainedWallet(t testing.TB, f *snWalletProviderScopeFixture, observed []model.StProviderWalletIdentity, scope model.StProviderWalletEpochScope, clientId server.Id) *model.StProviderWallet {
	t.Helper()
	wallets, err := model.GetStRetainedProviderWalletsForEpoch(t.Context(), f.rpc.root, scope, observed)
	if err != nil || wallets[clientId] == nil || wallets[clientId].Resolution == nil {
		t.Fatal("retained heads did not resolve the provider", clientId, wallets, err)
	}
	return wallets[clientId]
}

// Absent or refused evidence leaves only that provider unmapped, where a
// roster would hold every provider. A failed read never yields a partial map.
func TestStRetainedProviderWalletsForEpochUnmapsOnlyRefusedProviders(t *testing.T) {
	snWalletProviderScopeRun(t, func(t testing.TB) {
		f := newSnWalletProviderScopeFixture(t)
		provider := model.StProviderWalletIdentity{ClientId: *f.provider.ClientId, NetworkId: f.owner.NetworkId}
		sibling := model.StProviderWalletIdentity{ClientId: *f.sibling.ClientId, NetworkId: f.owner.NetworkId}
		observed := []model.StProviderWalletIdentity{provider, sibling}
		wallets, err := model.GetStRetainedProviderWalletsForEpoch(t.Context(), f.rpc.root, f.scope, observed)
		if err != nil || wallets == nil || len(wallets) != 0 {
			t.Fatal("providers without consent held the epoch or gained a wallet", wallets, err)
		}
		networkKey, networkAddress := snNetworkWalletTestKey(t, 214)
		set, _, networkHead := f.networkConsent(t, f.owner, networkKey, networkAddress, f.scope.Epoch)
		f.accept(t, f.owner, set)
		providerProof, _, _ := f.consent(t, f.provider, nil)
		f.accept(t, f.provider, providerProof)
		// the provider observed on a network its own consent does not name is
		// refused and never falls back; the sibling still earns to the network
		moved := model.StProviderWalletIdentity{ClientId: provider.ClientId, NetworkId: server.NewId()}
		wallets, err = model.GetStRetainedProviderWalletsForEpoch(t.Context(), f.rpc.root, f.scope, []model.StProviderWalletIdentity{moved, sibling})
		if err != nil || len(wallets) != 1 || wallets[moved.ClientId] != nil {
			t.Fatal("a refused provider was mapped or held the epoch", wallets, err)
		}
		if fallback := wallets[sibling.ClientId]; fallback == nil || fallback.Resolution == nil || fallback.Resolution.Mode != protocol.EarningWalletModeNetwork || fallback.Resolution.HeadHash != networkHead || fallback.ColdkeySs58 != networkAddress {
			t.Fatal("an absent own chain did not fall back to the network consent", fallback)
		}
		// consent issued under another root is refused for every provider
		if wallets, err := model.GetStRetainedProviderWalletsForEpoch(t.Context(), common.Address{1}, f.scope, observed); err != nil || len(wallets) != 0 {
			t.Fatal("consent issued under another root selected a wallet", wallets, err)
		}
		if wallets, err := model.GetStRetainedProviderWalletsForEpoch(t.Context(), f.rpc.root, f.scope, []model.StProviderWalletIdentity{provider, provider}); wallets != nil || !errors.Is(err, protocol.ErrWalletMappingIntegrity) {
			t.Fatal("a duplicate observed provider was accepted", wallets, err)
		}
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		if wallets, err := model.GetStRetainedProviderWalletsForEpoch(ctx, f.rpc.root, f.scope, observed); wallets != nil || !errors.Is(err, context.Canceled) {
			t.Fatal("a canceled selection returned a map", wallets, err)
		}
	})
}

// With no provider_work.yml a fresh epoch publishes from retained consents
// instead of holding; the artifact claims no whole-work evidence.
func TestStRetainedPayoutPublishesWithoutRosterSigner(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(tb testing.TB) {
		if _, _, _, err := LoadProviderWorkAuthorityPolicy(); !errors.Is(err, server.ErrResourceNotFound) {
			tb.Fatal("the test vault already provisions provider_work.yml", err)
		}
		controllerPayoutSchedule(tb, stPayoutPolicyTime(100))
		fixture, _, cfg, restart := newStPayoutPolicyFixture(tb, 1)
		client := restart()
		fixture.configure(275, "")
		if _, _, err := stComputeReleasePayout(tb.Context(), cfg, client, 1, stPayoutPolicyTime(200), stPayoutPolicyTime(250), 200, 250, nil); err != nil {
			tb.Fatal("a payout without a roster signer was held", err)
		}
		record := model.GetStPayoutArtifact(tb.Context(), cfg.DeploymentKey(), 1, cfg.NoId)
		store, ok := server.LoadBlobStore()
		if record == nil || !ok {
			tb.Fatal("the retained-consent payout was not published")
		}
		artifact, _, err := startifact.Read(tb.Context(), store, record.ContentHash)
		if err != nil || artifact == nil || artifact.ClosedWork != nil && artifact.ClosedWork.WholeInventory != nil {
			tb.Fatal("the retained-consent payout claimed whole-work evidence", err)
		}
	})
}
