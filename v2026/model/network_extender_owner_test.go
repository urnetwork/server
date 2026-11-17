package model

// An extender key belongs to the network that first activated it
// (ActivateNetworkExtender). The activation probes cannot tell the key's
// owner from a post relayed through its extender (connect/EXTENDER.md A4, A5),
// so the store refuses to move a key between networks.

import (
	"context"
	"testing"
	"time"

	"github.com/urnetwork/connect/v2026"

	"github.com/urnetwork/server/v2026"
)

// An activation of a stored key by another network, on the same family or the
// other one, stores nothing: no row change, no address, no history row, no
// signature and no publish. Before, the upsert moved the row's network and
// client to the poster's.
func TestActivateNetworkExtenderNeverMovesAKeyToAnotherNetwork(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		publicKey := []byte("extender-public-key-owner-00001")
		sign, signed := testRecordSigner()

		owner := testExtenderActivation(publicKey, 4, "192.0.2.61")
		activated := ActivateNetworkExtender(ctx, owner, sign)
		extenderId := activated.Extender.ExtenderId
		publishCount := len(Testing_GetNetworkExtenderPublishes(ctx))
		historyCount := len(GetNetworkExtenderActivations(ctx, extenderId, time.Time{}))
		connect.AssertEqual(t, len(signed()), 1)

		for _, other := range []*NetworkExtenderActivation{
			testExtenderActivation(publicKey, 4, "198.51.100.61"),
			testExtenderActivation(publicKey, 6, "2001:db8::61"),
		} {
			other.NetworkId = server.NewId()
			other.TcpPort = 8443
			other.CountryCode = "DE"
			moved := ActivateNetworkExtender(ctx, other, sign)
			if moved != nil {
				t.Fatalf(
					"another network's activation of the key was stored: network moved %t, client moved %t",
					moved.Extender.NetworkId == other.NetworkId,
					moved.Extender.ClientId == other.ClientId,
				)
			}
		}

		stored := Testing_GetNetworkExtender(ctx, extenderId)
		connect.AssertEqual(t, stored.Extender.NetworkId, owner.NetworkId)
		connect.AssertEqual(t, stored.Extender.ClientId, owner.ClientId)
		connect.AssertEqual(t, stored.Extender.TcpPort, owner.TcpPort)
		connect.AssertEqual(t, stored.Extender.CountryCode, owner.CountryCode)
		connect.AssertEqual(t, stored.Extender.RecordIssueTime.Equal(*activated.Extender.RecordIssueTime), true)
		connect.AssertEqual(t, len(stored.Addresses), 1)
		connect.AssertEqual(t, stored.Addresses[0].Ip.String(), "192.0.2.61")
		connect.AssertEqual(t, len(Testing_GetNetworkExtenderPublishes(ctx)), publishCount)
		connect.AssertEqual(t, len(GetNetworkExtenderActivations(ctx, extenderId, time.Time{})), historyCount)
		connect.AssertEqual(t, len(signed()), 1)

		// the owner's own re-activation still stores and publishes
		reactivated := ActivateNetworkExtender(ctx, owner, sign)
		connect.AssertEqual(t, reactivated.Extender.ExtenderId, extenderId)
		connect.AssertEqual(t, len(Testing_GetNetworkExtenderPublishes(ctx)), publishCount+1)
	})
}

// Another client of the owning network re-activates the key and becomes its
// client, with the ports it now serves: a reinstall on the same account keeps
// its extender.
func TestActivateNetworkExtenderKeepsAKeyInItsNetworkForAnotherClient(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		publicKey := []byte("extender-public-key-owner-00002")
		sign, _ := testRecordSigner()

		first := testExtenderActivation(publicKey, 4, "192.0.2.62")
		activated := ActivateNetworkExtender(ctx, first, sign)

		second := testExtenderActivation(publicKey, 4, "192.0.2.63")
		second.NetworkId = first.NetworkId
		second.ClientId = server.NewId()
		second.TcpPort = 8443
		reactivated := ActivateNetworkExtender(ctx, second, sign)
		if reactivated == nil {
			t.Fatal("another client of the owning network was refused")
		}
		connect.AssertEqual(t, reactivated.Extender.ExtenderId, activated.Extender.ExtenderId)

		stored := Testing_GetNetworkExtender(ctx, activated.Extender.ExtenderId)
		connect.AssertEqual(t, stored.Extender.NetworkId, first.NetworkId)
		connect.AssertEqual(t, stored.Extender.ClientId, second.ClientId)
		connect.AssertEqual(t, stored.Extender.TcpPort, 8443)
		connect.AssertEqual(t, stored.Addresses[0].Ip.String(), "192.0.2.63")
	})
}
