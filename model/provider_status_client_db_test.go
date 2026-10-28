package model

import (
	"context"
	"testing"

	"github.com/urnetwork/server"
)

// GetProviderStatusClient against the database: it reads a client id exactly
// as the status decides one. Run with
// `cd server/model && ../test.sh -run ProviderStatusClient`.

// A provider client of the network is a client and a provider; a client of
// the network without a provide key is a client only; a removed client, a
// child client, another network's provider client and an unknown id are
// neither. A client is exactly one GetClientProviderStatus decides.
func TestProviderStatusClientMatchesDecidedClients(t *testing.T) {
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
		providerClientId := newClient(networkId, true)
		idleClientId := newClient(networkId, false)
		removedClientId := newClient(networkId, true)
		childClientId := newClient(networkId, true)
		otherClientId := newClient(otherNetworkId, true)
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE network_client SET active = false WHERE client_id = $1`, removedClientId))
			server.RaisePgResult(tx.Exec(ctx, `UPDATE network_client SET source_client_id = $2 WHERE client_id = $1`, childClientId, providerClientId))
		})

		for name, testCase := range map[string]struct {
			clientId server.Id
			client   bool
			provider bool
		}{
			"provider":      {clientId: providerClientId, client: true, provider: true},
			"idle":          {clientId: idleClientId, client: true, provider: false},
			"removed":       {clientId: removedClientId},
			"child":         {clientId: childClientId},
			"other_network": {clientId: otherClientId},
			"unknown":       {clientId: server.NewId()},
		} {
			client, provider := GetProviderStatusClient(ctx, networkId, testCase.clientId)
			if client != testCase.client || provider != testCase.provider {
				t.Fatalf("%s: client %t provider %t, want %t %t", name, client, provider, testCase.client, testCase.provider)
			}
			if decided := GetClientProviderStatus(ctx, networkId, testCase.clientId) != nil; decided != client {
				t.Fatalf("%s: client %t, but a status was decided: %t", name, client, decided)
			}
		}

		// its own network reads the other network's provider client
		if client, provider := GetProviderStatusClient(ctx, otherNetworkId, otherClientId); !client || !provider {
			t.Fatalf("its own network: client %t provider %t", client, provider)
		}
	})
}
