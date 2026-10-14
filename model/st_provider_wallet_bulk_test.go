// Bulk epoch wallet selection selects exactly what v1 selects for every
// provider, from chains verified once per head (VERSION2-FINAL.md §8.2).
// Keys, networks and clients are synthetic.
package model

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/urfoundation/sn/payoutartifact"
	"github.com/urfoundation/sn/protocol"
	"github.com/urnetwork/server"
)

// The selection fixture: one provider per branch of v1's selection.
type stWalletBulkBranches struct {
	f        *stWalletBulkFixture
	observed []StProviderWalletIdentity
	// the providers by role
	own, rotated, future, lapsed, absent, legacy, moved, foreignRoot, lateBoundary, lateExpiry  StProviderWalletIdentity
	badSignature, badLink, badHead, networkRefused, globalFuture, delegationLate, globalMissing StProviderWalletIdentity
	sharedNetwork, sharedNetworkSibling, sharedHotkey, sharedHotkeySibling, movedDelegation     StProviderWalletIdentity
	replacedColdkey, delegationBad, networkBadLink, stranger                                    StProviderWalletIdentity
	networkBadSignature, networkBadHead, delegationBadLink, delegationBadHead                   StProviderWalletIdentity
	globalBadSignature, globalBadLink, globalBadHead                                            StProviderWalletIdentity
	// pinned heads, by provider or network
	providerHeads map[server.Id][][32]byte
	networkHeads  map[server.Id][][32]byte
	delegations   map[server.Id][][32]byte
}

func stWalletBulkProvider(networkId server.Id) StProviderWalletIdentity {
	return StProviderWalletIdentity{ClientId: server.NewId(), NetworkId: networkId}
}

// SV-2, SV-6: every branch of v1's selection. Coldkey seeds name the wallet
// each branch should select.
func newStWalletBulkBranches(t testing.TB) *stWalletBulkBranches {
	t.Helper()
	f := newStWalletBulkFixture(t)
	b := &stWalletBulkBranches{f: f, providerHeads: map[server.Id][][32]byte{}, networkHeads: map[server.Id][][32]byte{}, delegations: map[server.Id][][32]byte{}}
	provider := func(spec ...stWalletConsentSpec) StProviderWalletIdentity {
		identity := stWalletBulkProvider(server.NewId())
		if 0 < len(spec) {
			b.providerHeads[identity.ClientId] = f.providerChain(t, identity.ClientId, identity.NetworkId, spec...)
		}
		b.observed = append(b.observed, identity)
		return identity
	}
	network := func(identity StProviderWalletIdentity, spec ...stWalletConsentSpec) {
		b.networkHeads[identity.NetworkId] = f.networkChain(t, identity.NetworkId, spec...)
	}
	delegate := func(identity StProviderWalletIdentity, spec ...stWalletConsentSpec) {
		b.delegations[identity.NetworkId] = f.delegationChain(t, identity.NetworkId, spec...)
	}
	global := func(hotkey uint32, spec ...stWalletConsentSpec) stWalletConsentSpec {
		heads := f.hotkeyChain(t, hotkey, spec...)
		return stWalletConsentSpec{key: hotkey, from: 95, consentHead: heads[len(heads)-1], consentGeneration: uint64(len(heads))}
	}

	// the provider's own consent effective at the epoch
	b.own = provider(stWalletConsentSpec{key: 11, from: 95})
	// a later generation supersedes the first from epoch 101
	b.rotated = provider(stWalletConsentSpec{key: 12, from: 95}, stWalletConsentSpec{key: 13, from: 101})
	// an own consent effective only from 105 falls back to the network
	b.future = provider(stWalletConsentSpec{key: 14, from: 105})
	network(b.future, stWalletConsentSpec{key: 15, from: 95})
	// an own consent that lapsed at 99, no network consent, then the delegation
	b.lapsed = provider(stWalletConsentSpec{key: 16, from: 91, through: 99})
	delegate(b.lapsed, global(41, stWalletConsentSpec{key: 42, from: 95}))
	// no own chain and a network consent not yet effective: nothing effective
	b.absent = provider()
	network(b.absent, stWalletConsentSpec{key: 17, from: 105})
	// a first-schema own consent is unavailable and never falls back
	b.legacy = provider(stWalletConsentSpec{key: 18, from: 95, legacy: true})
	network(b.legacy, stWalletConsentSpec{key: 19, from: 95})
	// an own consent naming another network than the epoch observed
	other := server.NewId()
	b.moved = provider(stWalletConsentSpec{key: 20, from: 95, networkId: &other})
	// issued under another operator root
	b.foreignRoot = provider(stWalletConsentSpec{key: 21, from: 95, rootKey: f.otherKey})
	// issued at the start block of epoch 100, before epoch 101 starts
	b.lateBoundary = provider(stWalletConsentSpec{key: 22, from: 95, boundary: &protocol.ClientKeyEffectiveBoundary{Epoch: 90, Block: 1000, Hash: [32]byte{10}}})
	// acceptable until after epoch 100 starts, before epoch 101 starts
	b.lateExpiry = provider(stWalletConsentSpec{key: 23, from: 95, expiresAt: f.startTime.Unix() + 60})
	// chains the acceptance API refuses: the network consent never helps
	b.badSignature = provider(stWalletConsentSpec{key: 24, from: 95, badSignature: true})
	network(b.badSignature, stWalletConsentSpec{key: 25, from: 95})
	b.badLink = provider(stWalletConsentSpec{key: 26, from: 95}, stWalletConsentSpec{key: 27, from: 96, badLink: true})
	b.badHead = provider(stWalletConsentSpec{key: 28, from: 95, hashColumn: &[32]byte{28}})
	// a network consent under another root is refused, not passed over
	b.networkRefused = provider()
	network(b.networkRefused, stWalletConsentSpec{key: 29, from: 95, rootKey: f.otherKey})
	// a delegation whose global consent starts after the epoch
	b.globalFuture = provider()
	delegate(b.globalFuture, global(43, stWalletConsentSpec{key: 44, from: 110}))
	// a delegation acceptable until after epoch 100 starts
	b.delegationLate = provider()
	late := global(45, stWalletConsentSpec{key: 46, from: 95})
	late.expiresAt = f.startTime.Unix() + 60
	delegate(b.delegationLate, late)
	// a delegation pinning a global head that is not retained
	b.globalMissing = provider()
	missing := global(47, stWalletConsentSpec{key: 48, from: 95})
	missing.consentHead, missing.consentGeneration = [32]byte{47}, 2
	delegate(b.globalMissing, missing)
	// two providers of one network share its consent
	b.sharedNetwork = provider()
	network(b.sharedNetwork, stWalletConsentSpec{key: 49, from: 95})
	b.sharedNetworkSibling = StProviderWalletIdentity{ClientId: server.NewId(), NetworkId: b.sharedNetwork.NetworkId}
	b.observed = append(b.observed, b.sharedNetworkSibling)
	// two networks delegate to one hotkey and global head
	shared := global(50, stWalletConsentSpec{key: 51, from: 95})
	b.sharedHotkey = provider()
	delegate(b.sharedHotkey, shared)
	b.sharedHotkeySibling = provider()
	delegate(b.sharedHotkeySibling, shared)
	// the network moves to another hotkey from epoch 101
	b.movedDelegation = provider()
	first, second := global(52, stWalletConsentSpec{key: 53, from: 95}), global(54, stWalletConsentSpec{key: 55, from: 95})
	second.from = 101
	delegate(b.movedDelegation, first, second)
	// the global chain replaces its coldkey from epoch 101
	b.replacedColdkey = provider()
	delegate(b.replacedColdkey, global(56, stWalletConsentSpec{key: 57, from: 95}, stWalletConsentSpec{key: 58, from: 101}))
	// a delegation the hotkey did not sign
	b.delegationBad = provider()
	bad := global(59, stWalletConsentSpec{key: 60, from: 95})
	bad.badSignature = true
	delegate(b.delegationBad, bad)
	// a network chain whose second generation does not link the first
	b.networkBadLink = provider()
	network(b.networkBadLink, stWalletConsentSpec{key: 61, from: 95}, stWalletConsentSpec{key: 62, from: 96, badLink: true})
	// network, delegation and global chains the acceptance API refuses
	b.networkBadSignature = provider()
	network(b.networkBadSignature, stWalletConsentSpec{key: 64, from: 95, badSignature: true})
	b.networkBadHead = provider()
	network(b.networkBadHead, stWalletConsentSpec{key: 65, from: 95, hashColumn: &[32]byte{65}})
	b.delegationBadLink = provider()
	linked := global(66, stWalletConsentSpec{key: 67, from: 95})
	unlinked := linked
	unlinked.from, unlinked.badLink = 96, true
	delegate(b.delegationBadLink, linked, unlinked)
	b.delegationBadHead = provider()
	corrupt := global(68, stWalletConsentSpec{key: 69, from: 95})
	corrupt.hashColumn = &[32]byte{68}
	delegate(b.delegationBadHead, corrupt)
	b.globalBadSignature = provider()
	delegate(b.globalBadSignature, global(70, stWalletConsentSpec{key: 71, from: 95, badSignature: true}))
	b.globalBadLink = provider()
	delegate(b.globalBadLink, global(72, stWalletConsentSpec{key: 73, from: 95}, stWalletConsentSpec{key: 74, from: 96, badLink: true}))
	b.globalBadHead = provider()
	delegate(b.globalBadHead, global(75, stWalletConsentSpec{key: 76, from: 95, hashColumn: &[32]byte{75}}))
	// a provider with no evidence at all
	b.stranger = provider()
	f.flush(t)
	return b
}

// The mode and coldkey seed a branch selects at an epoch, or unmapped (mode "").
func requireStWalletBulkSelection(t testing.TB, label string, resolutions map[server.Id]*StPayoutWalletResolution, provider StProviderWalletIdentity, mode string, f *stWalletBulkFixture, coldkey uint32) {
	t.Helper()
	got := resolutions[provider.ClientId]
	if mode == "" {
		if got != nil {
			t.Fatalf("%s: unmapped provider selected %+v", label, got)
		}
		return
	}
	if got == nil || got.Mode != mode || got.Coldkey != f.public(t, coldkey) {
		t.Fatalf("%s: got %+v, want mode %s coldkey %d", label, got, mode, coldkey)
	}
}

// SV-2, SV-6: retained heads. At every epoch both v1 and the bulk selection
// map the same providers to the same resolutions, from a cold cache and again
// from the stored chains.
func TestStRetainedWalletResolutionsEqualV1(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		b := newStWalletBulkBranches(t)
		f := b.f
		selections := map[uint64]map[server.Id]*StPayoutWalletResolution{}
		for _, epoch := range []uint64{100, 101, 105} {
			scope := f.scopeAt(epoch)
			for pass := range 2 {
				v1, v1Err := GetStRetainedProviderWalletsForEpoch(ctx, f.root, scope, b.observed)
				bulk, bulkErr := GetStRetainedProviderWalletResolutionsForEpoch(ctx, f.root, scope, b.observed)
				requireStWalletResolutionsEqualV1(t, fmt.Sprintf("retained epoch %d pass %d", epoch, pass), v1, v1Err, bulk, bulkErr)
				selections[epoch] = bulk
			}
		}
		provider, network, hotkey := protocol.EarningWalletModeProvider, protocol.EarningWalletModeNetwork, protocol.EarningWalletModeHotkey
		at100, at101, at105 := selections[100], selections[101], selections[105]
		for _, want := range []struct {
			label      string
			selection  map[server.Id]*StPayoutWalletResolution
			provider   StProviderWalletIdentity
			mode       string
			coldkey    uint32
			generation uint64
		}{
			{"own", at100, b.own, provider, 11, 1},
			{"superseded before", at100, b.rotated, provider, 12, 1},
			{"superseded after", at101, b.rotated, provider, 13, 2},
			{"future own falls back", at100, b.future, network, 15, 1},
			{"future own effective", at105, b.future, provider, 14, 1},
			{"lapsed own delegates", at100, b.lapsed, hotkey, 42, 1},
			{"nothing effective", at100, b.absent, "", 0, 0},
			{"first schema", at100, b.legacy, "", 0, 0},
			{"another network", at100, b.moved, "", 0, 0},
			{"another root", at100, b.foreignRoot, "", 0, 0},
			{"boundary at start", at100, b.lateBoundary, "", 0, 0},
			{"boundary before start", at101, b.lateBoundary, provider, 22, 1},
			{"accepted after start", at100, b.lateExpiry, "", 0, 0},
			{"accepted before start", at101, b.lateExpiry, provider, 23, 1},
			{"bad signature", at100, b.badSignature, "", 0, 0},
			{"bad link", at100, b.badLink, "", 0, 0},
			{"bad head", at100, b.badHead, "", 0, 0},
			{"network refused", at100, b.networkRefused, "", 0, 0},
			{"global not effective", at100, b.globalFuture, "", 0, 0},
			{"delegation after start", at100, b.delegationLate, "", 0, 0},
			{"delegation before start", at101, b.delegationLate, hotkey, 46, 1},
			{"global head missing", at100, b.globalMissing, "", 0, 0},
			{"shared network", at100, b.sharedNetwork, network, 49, 1},
			{"shared network sibling", at100, b.sharedNetworkSibling, network, 49, 1},
			{"shared hotkey", at100, b.sharedHotkey, hotkey, 51, 1},
			{"shared hotkey sibling", at100, b.sharedHotkeySibling, hotkey, 51, 1},
			{"moved delegation before", at100, b.movedDelegation, hotkey, 53, 1},
			{"moved delegation after", at101, b.movedDelegation, hotkey, 55, 2},
			{"replaced coldkey before", at100, b.replacedColdkey, hotkey, 57, 1},
			{"replaced coldkey after", at101, b.replacedColdkey, hotkey, 58, 1},
			{"delegation bad signature", at100, b.delegationBad, "", 0, 0},
			{"network bad link", at100, b.networkBadLink, "", 0, 0},
			{"network bad signature", at100, b.networkBadSignature, "", 0, 0},
			{"network bad head", at100, b.networkBadHead, "", 0, 0},
			{"delegation bad link", at100, b.delegationBadLink, "", 0, 0},
			{"delegation bad head", at100, b.delegationBadHead, "", 0, 0},
			{"global bad signature", at100, b.globalBadSignature, "", 0, 0},
			{"global bad link", at100, b.globalBadLink, "", 0, 0},
			{"global bad head", at100, b.globalBadHead, "", 0, 0},
			{"stranger", at100, b.stranger, "", 0, 0},
		} {
			requireStWalletBulkSelection(t, want.label, want.selection, want.provider, want.mode, f, want.coldkey)
			if want.mode != "" && want.selection[want.provider.ClientId].ConsentGeneration != want.generation {
				t.Fatalf("%s: selected generation %d, want %d", want.label, want.selection[want.provider.ClientId].ConsentGeneration, want.generation)
			}
		}
		// a zero start block fails every prospective gate, in both
		scope := f.scopeAt(100)
		scope.Start.Number = 0
		v1, v1Err := GetStRetainedProviderWalletsForEpoch(ctx, f.root, scope, b.observed)
		bulk, bulkErr := GetStRetainedProviderWalletResolutionsForEpoch(ctx, f.root, scope, b.observed)
		requireStWalletResolutionsEqualV1(t, "zero start block", v1, v1Err, bulk, bulkErr)
		if len(bulk) != 0 {
			t.Fatal("a gated consent was selected without a start block", bulk)
		}
	})
}

// The roster's pins for the branches that resolve at epochs 100 and 101.
func (self *stWalletBulkBranches) resolvingPins() ([]payoutartifact.WholeWorkExpectedProvider, []payoutartifact.WholeWorkNetworkWallet, []payoutartifact.WholeWorkHotkeyDelegation) {
	providers := []payoutartifact.WholeWorkExpectedProvider{}
	for _, identity := range []StProviderWalletIdentity{self.own, self.rotated, self.future, self.lapsed, self.sharedNetwork, self.sharedNetworkSibling, self.sharedHotkey, self.sharedHotkeySibling, self.movedDelegation, self.replacedColdkey} {
		providers = append(providers, self.pin(identity))
	}
	networks := []payoutartifact.WholeWorkNetworkWallet{}
	for _, networkId := range []server.Id{self.future.NetworkId, self.sharedNetwork.NetworkId} {
		heads := self.networkHeads[networkId]
		networks = append(networks, payoutartifact.WholeWorkNetworkWallet{NetworkId: [16]byte(networkId), WalletHeadHash: hex.EncodeToString(heads[len(heads)-1][:]), WalletGeneration: uint64(len(heads))})
	}
	delegations := []payoutartifact.WholeWorkHotkeyDelegation{}
	for _, networkId := range []server.Id{self.lapsed.NetworkId, self.sharedHotkey.NetworkId, self.sharedHotkeySibling.NetworkId, self.movedDelegation.NetworkId, self.replacedColdkey.NetworkId} {
		heads := self.delegations[networkId]
		delegations = append(delegations, payoutartifact.WholeWorkHotkeyDelegation{NetworkId: [16]byte(networkId), DelegationHeadHash: hex.EncodeToString(heads[len(heads)-1][:]), DelegationGeneration: uint64(len(heads))})
	}
	return providers, networks, delegations
}

// The provider's newest head, or no pin.
func (self *stWalletBulkBranches) pin(identity StProviderWalletIdentity) payoutartifact.WholeWorkExpectedProvider {
	pinned := payoutartifact.WholeWorkExpectedProvider{ClientId: [16]byte(identity.ClientId), NetworkId: [16]byte(identity.NetworkId)}
	if heads := self.providerHeads[identity.ClientId]; 0 < len(heads) {
		pinned.WalletHeadHash, pinned.WalletGeneration = hex.EncodeToString(heads[len(heads)-1][:]), uint64(len(heads))
	}
	return pinned
}

// SV-2, SV-6: roster heads. A roster whose providers all resolve selects the
// same resolutions; a roster with one refused provider or malformed pin fails
// with v1's exact error.
func TestStRosterWalletResolutionsEqualV1(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		b := newStWalletBulkBranches(t)
		f := b.f
		providers, networks, delegations := b.resolvingPins()
		for _, epoch := range []uint64{100, 101} {
			scope := f.scopeAt(epoch)
			authority, expected := f.roster(t, scope, providers, networks, delegations)
			for range 2 {
				v1, v1Err := GetStProviderWalletsForEpoch(ctx, authority, expected, scope)
				bulk, bulkErr := GetStProviderWalletResolutionsForEpoch(ctx, authority, expected, scope)
				requireStWalletResolutionsEqualV1(t, "roster", v1, v1Err, bulk, bulkErr)
				if bulkErr != nil || len(bulk) != len(providers) {
					t.Fatal("the resolving roster did not map every provider", len(bulk), bulkErr)
				}
			}
		}
		// a provider outside the resolving roster, whose own pin is changed
		retained := func(mutate func(*payoutartifact.WholeWorkExpectedProvider)) payoutartifact.WholeWorkExpectedProvider {
			pinned := b.pin(b.foreignRoot)
			mutate(&pinned)
			return pinned
		}
		withNetwork := func(pinned payoutartifact.WholeWorkNetworkWallet) []payoutartifact.WholeWorkNetworkWallet {
			return append(append([]payoutartifact.WholeWorkNetworkWallet{}, networks...), pinned)
		}
		withDelegation := func(pinned payoutartifact.WholeWorkHotkeyDelegation) []payoutartifact.WholeWorkHotkeyDelegation {
			return append(append([]payoutartifact.WholeWorkHotkeyDelegation{}, delegations...), pinned)
		}
		unretained := hex.EncodeToString(make([]byte, 31)) + "01"
		for _, refused := range []struct {
			label       string
			provider    payoutartifact.WholeWorkExpectedProvider
			networks    []payoutartifact.WholeWorkNetworkWallet
			delegations []payoutartifact.WholeWorkHotkeyDelegation
			want        error
		}{
			{"nothing effective", b.pin(b.absent), networks, delegations, protocol.ErrWalletMappingAbsent},
			{"first schema", b.pin(b.legacy), networks, delegations, protocol.ErrWalletMappingUnavailable},
			{"bad signature", b.pin(b.badSignature), networks, delegations, protocol.ErrWalletMappingIntegrity},
			{"another network", b.pin(b.moved), networks, delegations, protocol.ErrWalletMappingIntegrity},
			{"global not effective", b.pin(b.globalFuture), networks, withDelegation(payoutartifact.WholeWorkHotkeyDelegation{NetworkId: [16]byte(b.globalFuture.NetworkId), DelegationHeadHash: hex.EncodeToString(b.delegations[b.globalFuture.NetworkId][0][:]), DelegationGeneration: 1}), protocol.ErrWalletMappingNotEffective},
			{"global head missing", b.pin(b.globalMissing), networks, withDelegation(payoutartifact.WholeWorkHotkeyDelegation{NetworkId: [16]byte(b.globalMissing.NetworkId), DelegationHeadHash: hex.EncodeToString(b.delegations[b.globalMissing.NetworkId][0][:]), DelegationGeneration: 1}), protocol.ErrWalletMappingUnavailable},
			{"head not retained", retained(func(pinned *payoutartifact.WholeWorkExpectedProvider) { pinned.WalletHeadHash = unretained }), networks, delegations, protocol.ErrWalletMappingUnavailable},
			{"generation not retained", retained(func(pinned *payoutartifact.WholeWorkExpectedProvider) { pinned.WalletGeneration = 2 }), networks, delegations, protocol.ErrWalletMappingUnavailable},
			{"network head not retained", b.pin(b.absent), withNetwork(payoutartifact.WholeWorkNetworkWallet{NetworkId: [16]byte(b.absent.NetworkId), WalletHeadHash: unretained, WalletGeneration: 1}), delegations, protocol.ErrWalletMappingUnavailable},
			{"delegation head not retained", b.pin(b.stranger), networks, withDelegation(payoutartifact.WholeWorkHotkeyDelegation{NetworkId: [16]byte(b.stranger.NetworkId), DelegationHeadHash: unretained, DelegationGeneration: 1}), protocol.ErrWalletMappingUnavailable},
		} {
			scope := f.scopeAt(100)
			authority, expected := f.roster(t, scope, append(append([]payoutartifact.WholeWorkExpectedProvider{}, providers...), refused.provider), refused.networks, refused.delegations)
			v1, v1Err := GetStProviderWalletsForEpoch(ctx, authority, expected, scope)
			bulk, bulkErr := GetStProviderWalletResolutionsForEpoch(ctx, authority, expected, scope)
			requireStWalletResolutionsEqualV1(t, refused.label, v1, v1Err, bulk, bulkErr)
			if bulk != nil || !errors.Is(bulkErr, refused.want) {
				t.Fatalf("%s: bulk %v, error %v, want %v", refused.label, bulk, bulkErr, refused.want)
			}
		}
	})
}

// SV-6: the consent cache per head hash. The first selection stores every
// generation of each chain that verified and none of a refused chain; a later
// epoch selects from the stored fields without reading the originals, which
// a changed stored field shows; a new head is verified and stored once.
func TestStWalletResolutionsVerifyEachHeadOnce(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		b := newStWalletBulkBranches(t)
		f := b.f
		stored := func(kind string, owner []byte) int {
			var count int
			server.Db(ctx, func(conn server.PgConn) {
				server.Raise(conn.QueryRow(ctx, `SELECT count(*) FROM st_verified_wallet_consent WHERE kind=$1 AND owner=$2`, kind, owner).Scan(&count))
			})
			return count
		}
		if _, err := GetStRetainedProviderWalletResolutionsForEpoch(ctx, f.root, f.scopeAt(100), b.observed); err != nil {
			t.Fatal(err)
		}
		for _, want := range []struct {
			label string
			kind  string
			owner []byte
			count int
		}{
			{"own", stWalletChainProvider, b.own.ClientId.Bytes(), 1},
			{"superseded", stWalletChainProvider, b.rotated.ClientId.Bytes(), 2},
			{"refused own", stWalletChainProvider, b.badSignature.ClientId.Bytes(), 0},
			{"refused head", stWalletChainProvider, b.badHead.ClientId.Bytes(), 0},
			{"network", stWalletChainNetwork, b.future.NetworkId.Bytes(), 1},
			{"network not read", stWalletChainNetwork, b.badSignature.NetworkId.Bytes(), 0},
			{"delegation", stWalletChainDelegation, b.movedDelegation.NetworkId.Bytes(), 2},
			{"global", stWalletChainHotkey, func() []byte { key := f.public(t, 56); return key[:] }(), 2},
		} {
			if count := stored(want.kind, want.owner); count != want.count {
				t.Fatalf("%s: %d stored generations, want %d", want.label, count, want.count)
			}
		}
		// a stored field is what a later epoch selects
		changed := f.public(t, 99)
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE st_verified_wallet_consent SET wallet=$1 WHERE kind=$2 AND owner=$3`, changed[:], stWalletChainProvider, b.own.ClientId.Bytes()))
		})
		bulk, err := GetStRetainedProviderWalletResolutionsForEpoch(ctx, f.root, f.scopeAt(101), b.observed)
		if err != nil || bulk[b.own.ClientId] == nil || bulk[b.own.ClientId].Coldkey != f.public(t, 99) {
			t.Fatal("a later epoch re-verified the originals instead of selecting the stored chain", bulk[b.own.ClientId], err)
		}
		// a pinned head that differs from the stored generation is read from
		// the originals, not taken from the store
		pinned := b.pin(b.foreignRoot)
		pinned.WalletHeadHash = hex.EncodeToString(make([]byte, 31)) + "01"
		if stored(stWalletChainProvider, b.foreignRoot.ClientId.Bytes()) != 1 {
			t.Fatal("the fixture did not store the chain the roster contradicts")
		}
		authority, expected := f.roster(t, f.scopeAt(101), []payoutartifact.WholeWorkExpectedProvider{pinned}, nil, nil)
		if bulk, err := GetStProviderWalletResolutionsForEpoch(ctx, authority, expected, f.scopeAt(101)); bulk != nil || !errors.Is(err, protocol.ErrWalletMappingUnavailable) {
			t.Fatal("a head contradicting the stored generation was taken from the store", bulk, err)
		}
		// a new head is verified from its originals, and stored once
		f.providerChain(t, b.stranger.ClientId, b.stranger.NetworkId, stWalletConsentSpec{key: 63, from: 95})
		f.flush(t)
		for range 2 {
			bulk, err = GetStRetainedProviderWalletResolutionsForEpoch(ctx, f.root, f.scopeAt(101), b.observed)
			if err != nil || bulk[b.stranger.ClientId] == nil || bulk[b.stranger.ClientId].Coldkey != f.public(t, 63) || stored(stWalletChainProvider, b.stranger.ClientId.Bytes()) != 1 {
				t.Fatal("a new head was not verified and stored once", bulk[b.stranger.ClientId], err)
			}
		}
	})
}

// A context whose Err is nil for its first calls and then canceled, so a
// test can stop a selection between two of its steps.
type stWalletStoppingContext struct {
	context.Context
	calls atomic.Int64
	after int64
}

func (self *stWalletStoppingContext) Err() error {
	if self.after < self.calls.Add(1) {
		return context.Canceled
	}
	return nil
}

// SV-6: each refusal of the retained bulk entry point, beside v1's.
func TestStRetainedWalletResolutionsRefusals(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		b := newStWalletBulkBranches(t)
		f := b.f
		scope := f.scopeAt(100)
		canceled, cancel := context.WithCancel(ctx)
		cancel()
		invalid := scope
		invalid.Domain.NoID = 0
		undated := scope
		undated.StartTime = undated.StartTime.AddDate(-100, 0, 0)
		zeroClient := []StProviderWalletIdentity{{NetworkId: server.NewId()}}
		zeroNetwork := []StProviderWalletIdentity{{ClientId: server.NewId()}}
		duplicate := []StProviderWalletIdentity{b.own, b.own}
		var missing context.Context
		for _, refused := range []struct {
			label     string
			ctx       context.Context
			root      common.Address
			scope     StProviderWalletEpochScope
			providers []StProviderWalletIdentity
			want      error
		}{
			{"no context", missing, f.root, scope, b.observed, protocol.ErrWalletMappingUnavailable},
			{"no root", ctx, common.Address{}, scope, b.observed, protocol.ErrWalletMappingUnavailable},
			{"no start time", ctx, f.root, undated, b.observed, protocol.ErrWalletMappingUnavailable},
			{"canceled", canceled, f.root, scope, b.observed, context.Canceled},
			{"invalid domain", ctx, f.root, invalid, b.observed, protocol.ErrWalletMappingIntegrity},
			{"zero client", ctx, f.root, scope, zeroClient, protocol.ErrWalletMappingIntegrity},
			{"zero network", ctx, f.root, scope, zeroNetwork, protocol.ErrWalletMappingIntegrity},
			{"duplicate client", ctx, f.root, scope, duplicate, protocol.ErrWalletMappingIntegrity},
		} {
			v1, v1Err := GetStRetainedProviderWalletsForEpoch(refused.ctx, refused.root, refused.scope, refused.providers)
			bulk, bulkErr := GetStRetainedProviderWalletResolutionsForEpoch(refused.ctx, refused.root, refused.scope, refused.providers)
			if v1 != nil || bulk != nil || !errors.Is(v1Err, refused.want) || !errors.Is(bulkErr, refused.want) {
				t.Fatalf("%s: v1 %v %v, bulk %v %v", refused.label, v1, v1Err, bulk, bulkErr)
			}
		}
		// stopped before the first chunk and after the last one
		counting := &stWalletStoppingContext{Context: ctx, after: 1 << 30}
		if _, err := GetStRetainedProviderWalletResolutionsForEpoch(counting, f.root, scope, b.observed); err != nil {
			t.Fatal(err)
		}
		counting.calls.Store(0)
		if _, err := GetStRetainedProviderWalletResolutionsForEpoch(counting, f.root, scope, b.observed); err != nil {
			t.Fatal(err)
		}
		for _, after := range []int64{1, counting.calls.Load() - 1} {
			stopping := &stWalletStoppingContext{Context: ctx, after: after}
			if bulk, err := GetStRetainedProviderWalletResolutionsForEpoch(stopping, f.root, scope, b.observed); bulk != nil || !errors.Is(err, context.Canceled) {
				t.Fatalf("a selection stopped after %d checks returned %v, %v", after, bulk, err)
			}
		}
		// a failed database read fails the selection whole
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `ALTER TABLE st_verified_wallet_consent RENAME TO st_verified_wallet_consent_moved`))
		})
		if bulk, err := GetStRetainedProviderWalletResolutionsForEpoch(ctx, f.root, scope, b.observed); bulk != nil || err == nil {
			t.Fatal("a failed stored-chain read returned a partial map", bulk, err)
		}
	})
}

// SV-6: each refusal of the roster bulk entry point returns v1's error.
func TestStRosterWalletResolutionsRefusals(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		b := newStWalletBulkBranches(t)
		f := b.f
		scope := f.scopeAt(100)
		providers, networks, delegations := b.resolvingPins()
		authority, expected := f.roster(t, scope, providers, networks, delegations)
		canceled, cancel := context.WithCancel(ctx)
		cancel()
		invalid := scope
		invalid.Domain.NoID = 0
		undated := scope
		undated.StartTime = undated.StartTime.AddDate(-100, 0, 0)
		noSigner, noRoot, otherHash := expected, expected, expected
		noSigner.AuthoritySigner = common.Address{}
		noRoot.ClientKeyRootSigner = common.Address{}
		otherHash.AuthorityHash = "sha256:" + strings.Repeat("0", 64)
		later := f.scopeAt(101)
		emptyAuthority, emptyExpected := f.roster(t, scope, nil, nil, nil)
		var missing context.Context
		for _, refused := range []struct {
			label     string
			ctx       context.Context
			authority []byte
			expected  payoutartifact.WholeWorkExpectation
			scope     StProviderWalletEpochScope
			want      error
		}{
			{"no context", missing, authority, expected, scope, protocol.ErrWalletMappingUnavailable},
			{"no authority", ctx, nil, expected, scope, protocol.ErrWalletMappingUnavailable},
			{"no roster signer", ctx, authority, noSigner, scope, protocol.ErrWalletMappingUnavailable},
			{"no root signer", ctx, authority, noRoot, scope, protocol.ErrWalletMappingUnavailable},
			{"no start time", ctx, authority, expected, undated, protocol.ErrWalletMappingUnavailable},
			{"canceled", canceled, authority, expected, scope, context.Canceled},
			{"invalid domain", ctx, authority, expected, invalid, protocol.ErrWalletMappingIntegrity},
			{"over capacity", ctx, make([]byte, payoutartifact.MaxWholeWorkAuthorityBytes+1), expected, scope, protocol.ErrWalletMappingCapacity},
			{"undecodable", ctx, []byte("{"), expected, scope, protocol.ErrWalletMappingIntegrity},
			{"another authority hash", ctx, authority, otherHash, scope, protocol.ErrWalletMappingIntegrity},
			{"another epoch", ctx, authority, expected, later, protocol.ErrWalletMappingIntegrity},
			{"no expected providers", ctx, emptyAuthority, emptyExpected, scope, protocol.ErrWalletMappingUnavailable},
		} {
			v1, v1Err := GetStProviderWalletsForEpoch(refused.ctx, refused.authority, refused.expected, refused.scope)
			bulk, bulkErr := GetStProviderWalletResolutionsForEpoch(refused.ctx, refused.authority, refused.expected, refused.scope)
			if v1 != nil || bulk != nil || !errors.Is(v1Err, refused.want) || !errors.Is(bulkErr, refused.want) {
				t.Fatalf("%s: v1 %v %v, bulk %v %v", refused.label, v1, v1Err, bulk, bulkErr)
			}
		}
		// canceled while the roster decodes (v1 decodes under its own timeout
		// context, which this context's Err does not reach)
		if bulk, err := GetStProviderWalletResolutionsForEpoch(&stWalletStoppingContext{Context: ctx, after: 1}, authority, expected, scope); bulk != nil || !errors.Is(err, context.Canceled) {
			t.Fatal("a roster canceled while decoding was accepted", bulk, err)
		}
		// a failed read of a chain's originals, inside a verifier, fails the
		// selection whole, as v1's read does
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `ALTER TABLE wallet_mapping_consent RENAME TO wallet_mapping_consent_moved`))
		})
		v1, v1Err := GetStProviderWalletsForEpoch(ctx, authority, expected, scope)
		bulk, bulkErr := GetStProviderWalletResolutionsForEpoch(ctx, authority, expected, scope)
		if v1 != nil || bulk != nil || v1Err == nil || bulkErr == nil || !strings.Contains(bulkErr.Error(), "wallet_mapping_consent") {
			t.Fatal("a failed original read returned a map", v1, v1Err, bulk, bulkErr)
		}
	})
}

// SV-6: a roster pin is read as v1 reads it. A signed roster already refuses
// half and malformed pins, so each refusal is triggered on the pin itself.
func TestStRosterProviderPinRefusals(t *testing.T) {
	head := strings.Repeat("ab", 32)
	clientId := [16]byte{1}
	for _, refused := range []struct {
		label    string
		provider payoutartifact.WholeWorkExpectedProvider
		want     error
	}{
		{"no pin", payoutartifact.WholeWorkExpectedProvider{ClientId: clientId}, protocol.ErrWalletMappingAbsent},
		{"head without generation", payoutartifact.WholeWorkExpectedProvider{ClientId: clientId, WalletHeadHash: head}, protocol.ErrWalletMappingIntegrity},
		{"generation without head", payoutartifact.WholeWorkExpectedProvider{ClientId: clientId, WalletGeneration: 1}, protocol.ErrWalletMappingIntegrity},
		{"uppercase head", payoutartifact.WholeWorkExpectedProvider{ClientId: clientId, WalletHeadHash: strings.ToUpper(head), WalletGeneration: 1}, protocol.ErrWalletMappingIntegrity},
		{"short head", payoutartifact.WholeWorkExpectedProvider{ClientId: clientId, WalletHeadHash: head[:62], WalletGeneration: 1}, protocol.ErrWalletMappingIntegrity},
		{"not hex", payoutartifact.WholeWorkExpectedProvider{ClientId: clientId, WalletHeadHash: "zz", WalletGeneration: 1}, protocol.ErrWalletMappingIntegrity},
	} {
		if pin := stRosterProviderPin(refused.provider); !errors.Is(pin.err, refused.want) {
			t.Fatalf("%s: pin %+v, want %v", refused.label, pin, refused.want)
		}
	}
	pin := stRosterProviderPin(payoutartifact.WholeWorkExpectedProvider{ClientId: clientId, WalletHeadHash: head, WalletGeneration: 3})
	if pin.err != nil || [16]byte(pin.head.owner[:16]) != clientId || [16]byte(pin.head.owner[16:]) != [16]byte{} || pin.head.generation != 3 || hex.EncodeToString(pin.head.hash[:]) != head {
		t.Fatal("a well-formed pin was read as", pin)
	}
}
