// Release payout wallet layers without a roster: signed consent first, the
// account-history wallet only for a provider with no signed evidence, and
// never for refused evidence. A configured roster stays the only authority.
// Keys, networks, clients and wallets are synthetic.
package controller

import (
	"context"
	"errors"
	"maps"
	"strings"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/urfoundation/sn/protocol"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// Once, so a refusal cannot be hidden by a retry, and with more time than
// the scope fixture's own runner for the slower shared database.
func stPayoutWalletLayersRun(t *testing.T, run func(testing.TB)) {
	t.Helper()
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 5*time.Minute)
		defer cancel()
		run(&snWalletProviderScopeTest{TB: t, ctx: ctx})
	})
}

// The epoch whose wallet scope is exactly the fixture's consent scope.
func stPayoutWalletLayersEpoch(t testing.TB, f *snWalletProviderScopeFixture, cfg *StConfig) *StPayoutEpochAuthority {
	t.Helper()
	epoch := &StPayoutEpochAuthority{
		Epoch:      f.scope.Epoch,
		PolicyHash: cfg.PolicyHash,
		Start:      protocol.ClientKeyEffectiveBoundary{Block: f.scope.Start.Number, Hash: [32]byte(common.HexToHash(f.scope.Start.Hash))},
		End:        protocol.ClientKeyEffectiveBoundary{Block: f.scope.End.Number, Hash: [32]byte(common.HexToHash(f.scope.End.Hash))},
		StartTime:  f.scope.StartTime,
		EndTime:    f.scope.StartTime.Add(time.Hour),
	}
	if scope := stProviderWalletEpochScope(cfg, epoch); scope != f.scope {
		t.Fatal("the payout epoch does not name the fixture's consent scope", scope, f.scope)
	}
	return epoch
}

// Current counts of every wallet source, for deltas.
func stPayoutWalletProviderCounts() map[string]float64 {
	counts := map[string]float64{}
	for _, source := range StPayoutWalletSources {
		counts[source] = testutil.ToFloat64(stPayoutWalletProviders.WithLabelValues(source))
	}
	return counts
}

func stPayoutWalletProviderDeltas(before map[string]float64) map[string]float64 {
	deltas := map[string]float64{}
	for source, count := range stPayoutWalletProviderCounts() {
		if delta := count - before[source]; delta != 0 {
			deltas[source] = delta
		}
	}
	return deltas
}

// The provider holds an accepted own consent and a newer account-history
// wallet; its sibling has only an account-history wallet. The consent beats
// the account history, the sibling falls back to its account-history wallet,
// and once the network consents, that consent beats the sibling's account
// history too.
func TestStPayoutWalletLayersPreferSignedConsentOverAccountHistory(t *testing.T) {
	stPayoutWalletLayersRun(t, func(t testing.TB) {
		ctx := t.Context()
		f := newSnWalletProviderScopeFixture(t)
		cfg := stConfig()
		epoch := stPayoutWalletLayersEpoch(t, f, cfg)
		providerProof, _, _ := f.consent(t, f.provider, nil)
		f.accept(t, f.provider, providerProof)
		consentColdkey := f.key.Public().Encode()
		// written after the consent's own projection, so each is the newest row
		providerAccountColdkey := stPayoutAccountWalletRow(t, *f.provider.ClientId, f.owner.NetworkId, 61, server.NowUtc().Add(time.Minute))
		siblingAccountColdkey := stPayoutAccountWalletRow(t, *f.sibling.ClientId, f.owner.NetworkId, 71, server.NowUtc().Add(time.Minute))
		if providerAccountColdkey == consentColdkey || siblingAccountColdkey == consentColdkey {
			t.Fatal("synthetic account-history wallets collide with the consent")
		}
		provider := &model.StProviderUsage{ClientId: *f.provider.ClientId, NetworkId: f.owner.NetworkId}
		sibling := &model.StProviderUsage{ClientId: *f.sibling.ClientId, NetworkId: f.owner.NetworkId}
		usages := []*model.StProviderUsage{provider, sibling}

		before := stPayoutWalletProviderCounts()
		wallets, selection, err := stSelectReleasePayoutWallets(ctx, cfg, nil, false, epoch, usages)
		if err != nil {
			t.Fatal(err)
		}
		if own := wallets[provider.ClientId]; own == nil || own.Resolution == nil || own.Resolution.Mode != protocol.EarningWalletModeProvider || own.ColdkeyPubkey != consentColdkey {
			t.Fatal("the provider's account-history wallet replaced its signed consent", own)
		}
		if fallback := wallets[sibling.ClientId]; fallback == nil || fallback.Resolution != nil || fallback.ColdkeyPubkey != siblingAccountColdkey {
			t.Fatal("the sibling without signed evidence was not paid to its account-history wallet", fallback)
		}
		if want := map[server.Id]string{provider.ClientId: StPayoutWalletSourceProviderConsent, sibling.ClientId: StPayoutWalletSourceAccountHistory}; !maps.Equal(selection.Sources, want) || len(selection.Refused) != 0 || selection.RosterConfigured {
			t.Fatal("wallet sources were misreported", selection.Sources, selection.Refused)
		}
		if deltas := stPayoutWalletProviderDeltas(before); !maps.Equal(deltas, map[string]float64{StPayoutWalletSourceProviderConsent: 1, StPayoutWalletSourceAccountHistory: 1}) {
			t.Fatal("wallet source metrics were miscounted", deltas)
		}

		// the network consent is signed evidence for the sibling
		networkKey, networkAddress := snNetworkWalletTestKey(t, 215)
		networkSet, _, _ := f.networkConsent(t, f.owner, networkKey, networkAddress, f.scope.Epoch)
		f.accept(t, f.owner, networkSet)
		stPayoutAccountWalletRow(t, sibling.ClientId, sibling.NetworkId, 73, server.NowUtc().Add(2*time.Minute))
		wallets, selection, err = stSelectReleasePayoutWallets(ctx, cfg, nil, false, epoch, usages)
		if err != nil {
			t.Fatal(err)
		}
		if network := wallets[sibling.ClientId]; network == nil || network.Resolution == nil || network.Resolution.Mode != protocol.EarningWalletModeNetwork || network.ColdkeySs58 != networkAddress {
			t.Fatal("the sibling's account-history wallet replaced its network's signed consent", network)
		}
		if own := wallets[provider.ClientId]; own == nil || own.ColdkeyPubkey != consentColdkey {
			t.Fatal("the network consent replaced the provider's own consent", own)
		}
		if want := map[server.Id]string{provider.ClientId: StPayoutWalletSourceProviderConsent, sibling.ClientId: StPayoutWalletSourceNetworkConsent}; !maps.Equal(selection.Sources, want) {
			t.Fatal("wallet sources were misreported", selection.Sources)
		}
	})
}

// Refused signed evidence is never paid from account history, even when the
// newest account-history wallet names the provider's observed network: a
// consent observed on a network it does not name, and a consent issued under
// another root. The sibling without evidence still falls back.
func TestStPayoutRefusedWalletEvidenceNeverFallsBackToAccountHistory(t *testing.T) {
	stPayoutWalletLayersRun(t, func(t testing.TB) {
		ctx := t.Context()
		f := newSnWalletProviderScopeFixture(t)
		cfg := stConfig()
		epoch := stPayoutWalletLayersEpoch(t, f, cfg)
		providerProof, _, _ := f.consent(t, f.provider, nil)
		f.accept(t, f.provider, providerProof)
		siblingAccountColdkey := stPayoutAccountWalletRow(t, *f.sibling.ClientId, f.owner.NetworkId, 71, server.NowUtc().Add(time.Minute))
		sibling := &model.StProviderUsage{ClientId: *f.sibling.ClientId, NetworkId: f.owner.NetworkId}

		// observed on a network its own consent does not name; the newest
		// account-history row names that network, so only the refusal stops it
		moved := &model.StProviderUsage{ClientId: *f.provider.ClientId, NetworkId: server.NewId()}
		stPayoutAccountWalletRow(t, moved.ClientId, moved.NetworkId, 81, server.NowUtc().Add(2*time.Minute))
		before := stPayoutWalletProviderCounts()
		wallets, selection, err := stSelectReleasePayoutWallets(ctx, cfg, nil, false, epoch, []*model.StProviderUsage{moved, sibling})
		if err != nil {
			t.Fatal(err)
		}
		if wallets[moved.ClientId] != nil || selection.Sources[moved.ClientId] != StPayoutWalletSourceRefused || !errors.Is(selection.Refused[moved.ClientId], protocol.ErrWalletMappingIntegrity) || len(selection.Refused) != 1 {
			t.Fatal("a provider whose consent names another network was paid or not reported refused", wallets[moved.ClientId], selection.Sources, selection.Refused)
		}
		if fallback := wallets[sibling.ClientId]; fallback == nil || fallback.ColdkeyPubkey != siblingAccountColdkey {
			t.Fatal("a refused provider changed its sibling's fallback", fallback)
		}
		if deltas := stPayoutWalletProviderDeltas(before); !maps.Equal(deltas, map[string]float64{StPayoutWalletSourceRefused: 1, StPayoutWalletSourceAccountHistory: 1}) {
			t.Fatal("refused metrics were miscounted", deltas)
		}

		// the same consent checked under another root key is refused, with an
		// account-history row naming the provider's own network
		otherRoot, err := crypto.HexToECDSA(strings.Repeat("57", 32))
		if err != nil {
			t.Fatal(err)
		}
		other := *cfg
		other.RootKey = otherRoot
		provider := &model.StProviderUsage{ClientId: *f.provider.ClientId, NetworkId: f.owner.NetworkId}
		stPayoutAccountWalletRow(t, provider.ClientId, provider.NetworkId, 91, server.NowUtc().Add(3*time.Minute))
		wallets, selection, err = stSelectReleasePayoutWallets(ctx, &other, nil, false, epoch, []*model.StProviderUsage{provider, sibling})
		if err != nil {
			t.Fatal(err)
		}
		if wallets[provider.ClientId] != nil || selection.Sources[provider.ClientId] != StPayoutWalletSourceRefused || selection.Refused[provider.ClientId] == nil {
			t.Fatal("a consent issued under another root was replaced by an account-history wallet", wallets[provider.ClientId], selection.Sources, selection.Refused)
		}
		if fallback := wallets[sibling.ClientId]; fallback == nil || fallback.ColdkeyPubkey != siblingAccountColdkey {
			t.Fatal("another root changed the sibling's fallback", fallback)
		}
	})
}

// Without a roster the sibling falls back to its account-history wallet. A
// configured roster is the only wallet authority: its missing epoch original
// holds the payout without consulting account history, and an approved roster
// pays exactly its pinned consent, not the provider's newer account-history
// wallet.
func TestStPayoutConfiguredRosterTakesPrecedenceOverAccountHistoryAndHolds(t *testing.T) {
	stPayoutWalletLayersRun(t, func(t testing.TB) {
		ctx := t.Context()
		f := newSnWalletProviderScopeFixture(t)
		cfg := stConfig()
		epoch := stPayoutWalletLayersEpoch(t, f, cfg)
		providerProof, _, _ := f.consent(t, f.provider, nil)
		accepted := f.accept(t, f.provider, providerProof)
		consentColdkey := f.key.Public().Encode()
		stPayoutAccountWalletRow(t, *f.provider.ClientId, f.owner.NetworkId, 101, server.NowUtc().Add(time.Minute))
		siblingAccountColdkey := stPayoutAccountWalletRow(t, *f.sibling.ClientId, f.owner.NetworkId, 111, server.NowUtc().Add(time.Minute))
		provider := &model.StProviderUsage{ClientId: *f.provider.ClientId, NetworkId: f.owner.NetworkId}
		sibling := &model.StProviderUsage{ClientId: *f.sibling.ClientId, NetworkId: f.owner.NetworkId}

		// the control: without a roster the sibling is paid from account history
		wallets, _, err := stSelectReleasePayoutWallets(ctx, cfg, nil, false, epoch, []*model.StProviderUsage{provider, sibling})
		if err != nil || wallets[sibling.ClientId] == nil || wallets[sibling.ClientId].ColdkeyPubkey != siblingAccountColdkey {
			t.Fatal("without a roster the sibling was not paid to its account-history wallet", wallets, err)
		}

		before := stPayoutWalletProviderCounts()
		if wallets, selection, err := stSelectReleasePayoutWallets(ctx, cfg, nil, true, epoch, []*model.StProviderUsage{provider, sibling}); wallets != nil || selection != nil || !errors.Is(err, protocol.ErrWalletMappingUnavailable) {
			t.Fatal("a configured roster without its epoch original did not hold", wallets, selection, err)
		}
		if deltas := stPayoutWalletProviderDeltas(before); len(deltas) != 0 {
			t.Fatal("a held roster payout counted a wallet source", deltas)
		}

		raw, expected := f.approve(t, f.provider, accepted.MappingHash, accepted.MappingGeneration)
		approved := &stProviderWorkAuthority{Raw: raw, Expectation: expected}
		wallets, selection, err := stSelectReleasePayoutWallets(ctx, cfg, approved, true, epoch, []*model.StProviderUsage{provider})
		if err != nil || len(wallets) != 1 {
			t.Fatal("the approved roster did not select its pinned provider", wallets, err)
		}
		if roster := wallets[provider.ClientId]; roster == nil || roster.Resolution == nil || roster.ColdkeyPubkey != consentColdkey {
			t.Fatal("an account-history wallet replaced the roster's pinned consent", roster)
		}
		if want := map[server.Id]string{provider.ClientId: StPayoutWalletSourceRoster}; !selection.RosterConfigured || !maps.Equal(selection.Sources, want) {
			t.Fatal("the roster selection misreported its sources", selection)
		}
		if deltas := stPayoutWalletProviderDeltas(before); !maps.Equal(deltas, map[string]float64{StPayoutWalletSourceRoster: 1}) {
			t.Fatal("a roster selection counted a fallback source", deltas)
		}
	})
}
