package model

import (
	"context"
	"slices"
	"testing"

	"github.com/urnetwork/server"
)

// The clients the provider status's data totals read
// (ListProviderStatusProvidedClientIds) against the database: every
// top-level client of the network, removed ones included, never a child
// client or another network's client, up to its limit. Run with
// `cd server/model && ../test.sh -run ProviderStatusProvidedClientIds`.
func TestProviderStatusProvidedClientIdsReadTheNetworksTopLevelClients(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		networkId := server.NewId()
		otherNetworkId := server.NewId()
		newClient := func(networkId server.Id) server.Id {
			clientId := server.NewId()
			Testing_CreateDevice(ctx, networkId, server.NewId(), clientId, "", "")
			return clientId
		}
		providerClientId := newClient(networkId)
		SetProvide(ctx, providerClientId, egressTestPublicAndNetwork)
		idleClientId := newClient(networkId)
		removedClientId := newClient(networkId)
		childClientId := newClient(networkId)
		newClient(otherNetworkId)
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE network_client SET active = false WHERE client_id = $1`, removedClientId))
			server.RaisePgResult(tx.Exec(ctx, `UPDATE network_client SET source_client_id = $2 WHERE client_id = $1`, childClientId, providerClientId))
		})

		want := []server.Id{providerClientId, idleClientId, removedClientId}
		slices.SortFunc(want, server.Id.Cmp)
		clientIds, truncated := ListProviderStatusProvidedClientIds(ctx, networkId, ProviderStatusProvidedMaxClients)
		slices.SortFunc(clientIds, server.Id.Cmp)
		if truncated || !slices.Equal(clientIds, want) {
			t.Fatalf("clients %v truncated %t, want %v and not truncated", clientIds, truncated, want)
		}

		// past the limit: the limit's count, and truncated
		clientIds, truncated = ListProviderStatusProvidedClientIds(ctx, networkId, 2)
		if !truncated || len(clientIds) != 2 {
			t.Fatalf("limit 2: %v truncated %t", clientIds, truncated)
		}
		for _, clientId := range clientIds {
			if !slices.Contains(want, clientId) {
				t.Fatalf("limit 2 read %s, not a top-level client of the network", clientId)
			}
		}

		clientIds, truncated = ListProviderStatusProvidedClientIds(ctx, server.NewId(), ProviderStatusProvidedMaxClients)
		if truncated || len(clientIds) != 0 {
			t.Fatalf("a network without clients: %v truncated %t", clientIds, truncated)
		}
	})
}
