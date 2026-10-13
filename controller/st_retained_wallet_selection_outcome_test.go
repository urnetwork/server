// The retained wallet selection reports why each unmapped provider is
// unmapped: no signed evidence at all, or evidence the protocol refused.
// Keys, networks and clients are synthetic.
package controller

import (
	"errors"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/urfoundation/sn/protocol"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// The retained selection reports each unmapped provider as absent or refused,
// never both, and a failed read returns no partial selection.
func TestStRetainedProviderWalletSelectionSeparatesAbsentFromRefused(t *testing.T) {
	stPayoutWalletLayersRun(t, func(t testing.TB) {
		ctx := t.Context()
		f := newSnWalletProviderScopeFixture(t)
		provider := model.StProviderWalletIdentity{ClientId: *f.provider.ClientId, NetworkId: f.owner.NetworkId}
		sibling := model.StProviderWalletIdentity{ClientId: *f.sibling.ClientId, NetworkId: f.owner.NetworkId}
		selection, err := model.GetStRetainedProviderWalletSelectionForEpoch(ctx, f.rpc.root, f.scope, []model.StProviderWalletIdentity{provider, sibling})
		if err != nil || len(selection.Wallets) != 0 || len(selection.Refused) != 0 || len(selection.Absent) != 2 {
			t.Fatal("providers without any evidence were not reported absent", selection, err)
		}
		for _, absent := range selection.Absent {
			if !errors.Is(absent, protocol.ErrWalletMappingAbsent) {
				t.Fatal("an absent provider carries another outcome", absent)
			}
		}
		providerProof, _, _ := f.consent(t, f.provider, nil)
		f.accept(t, f.provider, providerProof)
		moved := model.StProviderWalletIdentity{ClientId: provider.ClientId, NetworkId: server.NewId()}
		selection, err = model.GetStRetainedProviderWalletSelectionForEpoch(ctx, f.rpc.root, f.scope, []model.StProviderWalletIdentity{moved, sibling})
		if err != nil || len(selection.Wallets) != 0 || len(selection.Refused) != 1 || selection.Refused[moved.ClientId] == nil || len(selection.Absent) != 1 || selection.Absent[sibling.ClientId] == nil {
			t.Fatal("a consent naming another network was not reported refused", selection, err)
		}
		if !errors.Is(selection.Refused[moved.ClientId], protocol.ErrWalletMappingIntegrity) {
			t.Fatal("the refusal lost its integrity verdict", selection.Refused[moved.ClientId])
		}
		selection, err = model.GetStRetainedProviderWalletSelectionForEpoch(ctx, common.Address{1}, f.scope, []model.StProviderWalletIdentity{provider, sibling})
		if err != nil || len(selection.Wallets) != 0 || selection.Refused[provider.ClientId] == nil || selection.Absent[sibling.ClientId] == nil {
			t.Fatal("a consent under another root was not reported refused", selection, err)
		}
		if selection, err := model.GetStRetainedProviderWalletSelectionForEpoch(ctx, f.rpc.root, f.scope, []model.StProviderWalletIdentity{provider, provider}); selection != nil || !errors.Is(err, protocol.ErrWalletMappingIntegrity) {
			t.Fatal("a duplicate observed provider returned a selection", selection, err)
		}
	})
}
