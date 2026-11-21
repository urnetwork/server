//go:build sidecar_pending

package model

import (
	"context"
	"testing"

	"github.com/urnetwork/server"
)

// §5.15's lookup for the hint writers (plan-KEYS-hint-addressing): each sidecar's newest binding's client while its
// identity is active, across networks; a revoked sidecar and one api never bound are absent alike.
func TestSidecarCurrentClientsAreTheNewestBindingsOfActiveIdentities(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		now := server.NowUtc()
		networkId := server.NewId()
		otherNetworkId := server.NewId()

		moved := server.NewId()
		movedKey := testSidecarKey(t)
		BindSidecarIdentity(ctx, testSidecarBindingRecord(t, networkId, moved, movedKey, 1), now)
		newest := testSidecarBindingRecord(t, networkId, moved, movedKey, 2)
		if outcome, _ := BindSidecarIdentity(ctx, newest, now); outcome != SidecarBindAppended {
			t.Fatalf("the second generation: %s", outcome)
		}
		other := server.NewId()
		otherRecord := testSidecarBindingRecord(t, otherNetworkId, other, testSidecarKey(t), 1)
		BindSidecarIdentity(ctx, otherRecord, now)
		revoked := server.NewId()
		BindSidecarIdentity(ctx, testSidecarBindingRecord(t, networkId, revoked, testSidecarKey(t), 1), now)
		if !SetSidecarIdentityRevoked(ctx, revoked, now) {
			t.Fatal("not revoked")
		}
		unknown := server.NewId()

		clients, err := GetSidecarCurrentClients(ctx, []server.Id{moved, other, revoked, unknown})
		if err != nil {
			t.Fatal(err)
		}
		if len(clients) != 2 {
			t.Fatalf("%d clients, want the two active identities'", len(clients))
		}
		if client := clients[moved]; client == nil || client.ClientId != newest.ClientId || client.Generation != 2 {
			t.Errorf("a moved sidecar: %+v, want its newest generation's client", client)
		}
		if client := clients[other]; client == nil || client.ClientId != otherRecord.ClientId || client.Generation != 1 {
			t.Errorf("another network's sidecar: %+v", client)
		}
		if empty, err := GetSidecarCurrentClients(ctx, nil); err != nil || len(empty) != 0 {
			t.Errorf("no ids: %v %v", empty, err)
		}
	})
}
