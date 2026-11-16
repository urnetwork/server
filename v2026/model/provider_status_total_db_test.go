package model

import (
	"context"
	"slices"
	"testing"

	"github.com/urnetwork/server/v2026"
)

// The network's provider client count (total_clients) against the database:
// it counts exactly the clients the answer draws from, past the ones it
// decides, and its count stops at its bound. Run with
// `cd server/model && ../test.sh -run ProviderStatusTotalClients`.

// A network with two provider clients more than one answer decides: the
// answer decides the lowest ProviderStatusMaxClients client ids, is truncated,
// and counts every provider client, but not a client without a provide key,
// a removed client, a child client or another network's client.
func TestProviderStatusTotalClientsCountsEveryProviderClient(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		networkId := server.NewId()
		otherNetworkId := server.NewId()
		newClient := func(networkId server.Id, provider bool) server.Id {
			clientId := server.NewId()
			Testing_CreateDevice(ctx, networkId, server.NewId(), clientId, "", "")
			if provider {
				SetProvide(ctx, clientId, egressTestPublicAndNetwork)
			}
			return clientId
		}
		providerCount := ProviderStatusMaxClients + 2
		providerClientIds := []server.Id{}
		for range providerCount {
			providerClientIds = append(providerClientIds, newClient(networkId, true))
		}
		slices.SortFunc(providerClientIds, func(a server.Id, b server.Id) int { return a.Cmp(b) })
		newClient(networkId, false)
		removedClientId := newClient(networkId, true)
		childClientId := newClient(networkId, true)
		otherClientId := newClient(otherNetworkId, true)
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE network_client SET active = false WHERE client_id = $1`, removedClientId))
			server.RaisePgResult(tx.Exec(ctx, `UPDATE network_client SET source_client_id = $2 WHERE client_id = $1`, childClientId, providerClientIds[0]))
		})

		result := GetNetworkProviderStatuses(ctx, networkId)
		if !result.Truncated || len(result.Providers) != ProviderStatusMaxClients || result.TotalClients != providerCount {
			t.Fatalf("truncated %t, %d decided, total_clients %d; want true, %d, %d", result.Truncated, len(result.Providers), result.TotalClients, ProviderStatusMaxClients, providerCount)
		}
		for i, status := range result.Providers {
			if status.ClientId != providerClientIds[i] {
				t.Fatalf("decided %d is %s, want the lowest client ids %v", i, status.ClientId, providerClientIds[:ProviderStatusMaxClients])
			}
		}

		// within one answer, the count is the list
		other := GetNetworkProviderStatuses(ctx, otherNetworkId)
		if other.Truncated || len(other.Providers) != 1 || other.Providers[0].ClientId != otherClientId || other.TotalClients != 1 {
			t.Fatalf("another network: %+v", other)
		}
		empty := GetNetworkProviderStatuses(ctx, server.NewId())
		if empty.Truncated || len(empty.Providers) != 0 || empty.TotalClients != 0 {
			t.Fatalf("a network without providers: %+v", empty)
		}

		// the count stops at its bound, the list at its limit, and both are
		// read in one statement
		clientIds, truncated, total := listNetworkProviderClientIds(ctx, networkId, 4, 10)
		if !slices.Equal(clientIds, providerClientIds[:4]) || !truncated || total != 10 {
			t.Fatalf("limit 4, count limit 10: %v %t %d", clientIds, truncated, total)
		}
		clientIds, truncated, total = listNetworkProviderClientIds(ctx, networkId, providerCount, ProviderStatusMaxCountedClients)
		if !slices.Equal(clientIds, providerClientIds) || truncated || total != providerCount {
			t.Fatalf("limit %d: %d listed, %t, %d", providerCount, len(clientIds), truncated, total)
		}
	})
}
