package model

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/session"
)

// MintEdgeClient on Postgres (TLS-M5-11, QD-812): the first ask creates the derived client and its device in the
// urnetwork-edge network, a later ask reuses them, so a block's restart piles nothing up; the jwt is a strict_exp
// client token connect accepts, ending at its exp, staggered by index; and a client that was removed, a derived
// id that is another network's, and a network with no row are each refused with nothing created.
func TestMintEdgeClientReusesItsDerivedClient(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		networkId := server.NewId()
		adminUserId := server.NewId()
		Testing_CreateNetwork(ctx, networkId, "urnetwork-edge-test", adminUserId)
		settings := DefaultEdgeClientMintSettings()

		clientCount := func() int {
			count := 0
			server.Db(ctx, func(conn server.PgConn) {
				server.Raise(conn.QueryRow(ctx, `SELECT COUNT(*) FROM network_client WHERE network_id = $1`, networkId).Scan(&count))
			})
			return count
		}
		before := clientCount()

		key := &EdgeClientKey{Environment: "test", Block: "tlsedge", Host: "lb-a.example", Index: 1}
		startTime := server.NowUtc()
		first, err := MintEdgeClient(ctx, networkId, key, settings)
		if err != nil {
			t.Fatal(err)
		}
		wantClientId := server.DeriveEdgeClientId("test", "tlsedge", "lb-a.example", 1)
		wantDeviceId := server.DeriveEdgeDeviceId("test", "tlsedge", "lb-a.example", 1)
		if first.ClientId != wantClientId || first.ByClientJwt == "" {
			t.Fatalf("minted %s, want %s", first.ClientId, wantClientId)
		}
		if clientCount() != before+1 {
			t.Fatalf("%d clients, want %d", clientCount(), before+1)
		}

		// the jwt as connect reads it: this client on its derived device, strict_exp with its root, and an exp
		// of the life plus the index's stagger
		byJwt, err := session.ParseByJwtForAudience(ctx, first.ByClientJwt, session.ByJwtAudienceConnect)
		if err != nil {
			t.Fatal(err)
		}
		if byJwt.NetworkId != networkId || byJwt.UserId != adminUserId || byJwt.ClientId == nil || *byJwt.ClientId != wantClientId ||
			byJwt.DeviceId == nil || *byJwt.DeviceId != wantDeviceId || byJwt.RootClientId == nil || *byJwt.RootClientId != wantClientId ||
			!byJwt.StrictExp || byJwt.SessionId != nil || byJwt.ExpiresAt == nil {
			t.Fatalf("jwt %+v", byJwt)
		}
		life := settings.JwtLife + settings.JwtIndexStagger
		if exp := byJwt.ExpiresAt.Time; exp.Before(startTime.Add(life-2*time.Second)) || exp.After(server.NowUtc().Add(life+2*time.Second)) || !exp.Equal(first.ExpireTime) {
			t.Fatalf("exp %s, want about %s from %s", exp, life, startTime)
		}
		// and its state holds: an active client of the network on that device
		if err := session.ValidateByJwtState(ctx, byJwt, true); err != nil {
			t.Fatalf("the minted jwt's state: %v", err)
		}

		// a restart asks again: the same client, a new jwt, no new row
		second, err := MintEdgeClient(ctx, networkId, key, settings)
		if err != nil {
			t.Fatal(err)
		}
		if second.ClientId != first.ClientId || second.ByClientJwt == first.ByClientJwt || clientCount() != before+1 {
			t.Fatalf("a second ask made another client or the same jwt: %d clients", clientCount())
		}

		// another index is another client
		other, err := MintEdgeClient(ctx, networkId, &EdgeClientKey{Environment: "test", Block: "tlsedge", Host: "lb-a.example", Index: 0}, settings)
		if err != nil {
			t.Fatal(err)
		}
		if other.ClientId == first.ClientId || clientCount() != before+2 {
			t.Fatalf("index 0 shares index 1's client")
		}

		// a removed client stays removed
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE network_client SET active = false WHERE client_id = $1`, first.ClientId))
		})
		if _, err := MintEdgeClient(ctx, networkId, key, settings); !errors.Is(err, ErrEdgeClientRetired) {
			t.Fatalf("a removed client was minted for: %v", err)
		}
	})
}

func TestMintEdgeClientRefusesWhatIsNotItsNetworks(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		settings := DefaultEdgeClientMintSettings()
		key := &EdgeClientKey{Environment: "test", Block: "tlsedge", Host: "lb-b.example", Index: 0}

		// no network row: nothing is created
		missingNetworkId := server.NewId()
		if _, err := MintEdgeClient(ctx, missingNetworkId, key, settings); !errors.Is(err, ErrEdgeClientNetworkMissing) {
			t.Fatalf("a network with no row: %v", err)
		}
		created := 0
		server.Db(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(ctx, `SELECT COUNT(*) FROM network_client WHERE client_id = $1`, server.DeriveEdgeClientId("test", "tlsedge", "lb-b.example", 0)).Scan(&created))
		})
		if created != 0 {
			t.Fatal("a client was created with no network")
		}

		// the derived id already another network's: refused, never taken over
		networkId := server.NewId()
		Testing_CreateNetwork(ctx, networkId, "urnetwork-edge-test-b", server.NewId())
		otherNetworkId := server.NewId()
		Testing_CreateNetwork(ctx, otherNetworkId, "urnetwork-other-test-b", server.NewId())
		if _, err := MintEdgeClient(ctx, otherNetworkId, key, settings); err != nil {
			t.Fatal(err)
		}
		if _, err := MintEdgeClient(ctx, networkId, key, settings); !errors.Is(err, ErrEdgeClientRetired) {
			t.Fatalf("another network's client was minted for: %v", err)
		}

		// arguments no client has
		for _, bad := range []*EdgeClientKey{
			nil,
			{Environment: "", Block: "tlsedge", Host: "lb-b.example", Index: 0},
			{Environment: "test", Block: "", Host: "lb-b.example", Index: 0},
			{Environment: "test", Block: "tlsedge", Host: "", Index: 0},
			{Environment: "test", Block: "tlsedge", Host: "lb-b.example", Index: -1},
		} {
			if _, err := MintEdgeClient(ctx, networkId, bad, settings); err == nil {
				t.Errorf("%+v minted", bad)
			}
		}
		if _, err := MintEdgeClient(ctx, server.Id{}, key, settings); err == nil {
			t.Error("the zero network minted")
		}
	})
}
