package model

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/urnetwork/sidecar"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/session"
)

// MintEdgeClient on Postgres (TLS-M5-11, QD-812, QD-873): the first ask creates the derived client and its device
// in the urnetwork-edge network, a later ask reuses them, so a block's restart piles nothing up; another slot or
// index is another client; the jwt is a strict_exp client token connect accepts, ending at its exp, staggered by
// index; and a client that was removed, a derived id that is another network's, and a network with no row are
// each refused with nothing created.
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

		key := &EdgeClientKey{Environment: "test", Block: "tlsedge", Host: "lb-a.example", Slot: 1, Index: 1}
		startTime := server.NowUtc()
		first, err := MintEdgeClient(ctx, networkId, key, settings)
		if err != nil {
			t.Fatal(err)
		}
		wantClientId := server.DeriveEdgeClientId("test", "lb-a.example", "tlsedge", 1, 1)
		wantDeviceId := server.DeriveEdgeDeviceId("test", "lb-a.example", "tlsedge", 1, 1)
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
		other, err := MintEdgeClient(ctx, networkId, &EdgeClientKey{Environment: "test", Block: "tlsedge", Host: "lb-a.example", Slot: 1, Index: 0}, settings)
		if err != nil {
			t.Fatal(err)
		}
		if other.ClientId == first.ClientId || clientCount() != before+2 {
			t.Fatalf("index 0 shares index 1's client")
		}

		// the other slot is another client on another device, for the deploy's other process (QD-873)
		otherSlot, err := MintEdgeClient(ctx, networkId, &EdgeClientKey{Environment: "test", Block: "tlsedge", Host: "lb-a.example", Slot: 0, Index: 1}, settings)
		if err != nil {
			t.Fatal(err)
		}
		otherSlotByJwt, err := session.ParseByJwtForAudience(ctx, otherSlot.ByClientJwt, session.ByJwtAudienceConnect)
		if err != nil {
			t.Fatal(err)
		}
		if otherSlot.ClientId == first.ClientId || otherSlot.ClientId != server.DeriveEdgeClientId("test", "lb-a.example", "tlsedge", 0, 1) ||
			otherSlotByJwt.DeviceId == nil || *otherSlotByJwt.DeviceId == wantDeviceId || clientCount() != before+3 {
			t.Fatalf("slot 0 shares slot 1's client or device")
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
		key := &EdgeClientKey{Environment: "test", Block: "tlsedge", Host: "lb-b.example", Slot: 0, Index: 0}

		// no network row: nothing is created
		missingNetworkId := server.NewId()
		if _, err := MintEdgeClient(ctx, missingNetworkId, key, settings); !errors.Is(err, ErrEdgeClientNetworkMissing) {
			t.Fatalf("a network with no row: %v", err)
		}
		created := 0
		server.Db(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(ctx, `SELECT COUNT(*) FROM network_client WHERE client_id = $1`, server.DeriveEdgeClientId("test", "lb-b.example", "tlsedge", 0, 0)).Scan(&created))
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
			{Environment: "test", Block: "tlsedge", Host: "lb-b.example", Slot: -1, Index: 0},
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

// An edge client's removal owes its client rev/ object (QD-1054), though it is bound to no sidecar: the sockets token
// api's route answered it is refused at sockets from not_before on, by sockets' own reader of rev/. A removed
// unbound client of another network owes none here, and with no urnetwork_edge in sidecar.yml an edge client's
// removal owes none either, since sockets then takes no edge caller's token. On Postgres, with rev/ on T2's
// FakeBlobs.
func TestRemoveEdgeClientOwesItsRevocation(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		revStore := newTestRevStore(t)
		// the removal's own clock is real time, so the fake starts at it
		revStore.clock.AdvanceTo(time.Now())
		SetSidecarRevStore(revStore.store)
		t.Cleanup(func() { SetSidecarRevStore(nil) })

		edgeNetworkId := server.NewId()
		adminUserId := server.NewId()
		Testing_CreateNetwork(ctx, edgeNetworkId, "urnetwork-edge-rev-test", adminUserId)
		t.Cleanup(server.Testing_PushSidecarConfig("sidecar:\n  urnetwork_edge: " + edgeNetworkId.String() + "\n"))
		edgeSession := session.Testing_CreateClientSession(ctx, &session.ByJwt{NetworkId: edgeNetworkId, UserId: adminUserId, NetworkName: "urnetwork-edge-rev-test"})
		removeClient := func(clientSession *session.ClientSession, clientId server.Id) {
			t.Helper()
			result, err := RemoveNetworkClient(&RemoveNetworkClientArgs{ClientId: clientId}, clientSession)
			if err != nil || result.Error != nil {
				t.Fatalf("remove: %v %+v", err, result)
			}
		}
		held := func(networkId server.Id, clientId server.Id) *sidecar.RevocationRecord {
			t.Helper()
			return revStore.held(t, &SidecarRevocation{NetworkId: networkId, Kind: sidecar.RevocationKindClient, SubjectId: clientId})
		}

		mint, err := MintEdgeClient(ctx, edgeNetworkId, &EdgeClientKey{Environment: "test", Block: "smtpedge", Host: "lb-a.example", Slot: 0, Index: 0}, nil)
		if err != nil {
			t.Fatal(err)
		}
		issuedAt := time.Now().Truncate(time.Second)
		removeClient(edgeSession, mint.ClientId)
		record := held(edgeNetworkId, mint.ClientId)
		if record == nil {
			t.Fatal("a removed edge client has no rev/ object")
		}
		if record.Reason != sidecar.RevocationReasonRemoved {
			t.Fatalf("reason %s", record.Reason)
		}
		var deactivateTime time.Time
		server.Db(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(ctx, `SELECT deactivate_time FROM network_client WHERE client_id = $1`, mint.ClientId).Scan(&deactivateTime))
		})
		if record.NotBefore.Before(deactivateTime) {
			t.Fatalf("not_before %v before the deactivation %v", record.NotBefore, deactivateTime)
		}
		// sockets' reader refuses a token the route issued the client before the removal
		settings := sidecar.DefaultRevocationSettings()
		settings.Clock = revStore.clock
		checker := sidecar.NewRevocationChecker(revStore.blobs, testRevVerify, settings)
		err = checker.Check(ctx, &sidecar.Principal{
			NetworkId: [16]byte(edgeNetworkId),
			ClientId:  [16]byte(mint.ClientId),
			Audience:  "sockets",
			IssuedAt:  issuedAt,
			ExpiresAt: issuedAt.Add(15 * time.Minute),
		})
		if !errors.Is(err, sidecar.ErrRevocationVoidsToken) {
			t.Fatalf("the removed edge client's token is not void: %v", err)
		}

		// another network's unbound client owes nothing here
		otherNetworkId := server.NewId()
		otherUserId := server.NewId()
		Testing_CreateNetwork(ctx, otherNetworkId, "other-rev-test", otherUserId)
		otherSession := session.Testing_CreateClientSession(ctx, &session.ByJwt{NetworkId: otherNetworkId, UserId: otherUserId, NetworkName: "other-rev-test"})
		authResult, err := AuthNetworkClient(&AuthNetworkClientArgs{Description: "synthetic", DeviceSpec: "synthetic"}, otherSession)
		if err != nil || authResult.Error != nil || authResult.ByClientJwt == nil {
			t.Fatalf("client: %v %+v", err, authResult)
		}
		otherByJwt, err := session.ParseByJwt(ctx, *authResult.ByClientJwt)
		if err != nil {
			t.Fatal(err)
		}
		removeClient(otherSession, *otherByJwt.ClientId)
		if record := held(otherNetworkId, *otherByJwt.ClientId); record != nil {
			t.Fatalf("an unbound client of another network owes a rev/ object: %+v", record)
		}

		// sidecar.yml naming no urnetwork_edge: an edge client's removal owes nothing
		unnamed, err := MintEdgeClient(ctx, edgeNetworkId, &EdgeClientKey{Environment: "test", Block: "smtpedge", Host: "lb-a.example", Slot: 1, Index: 0}, nil)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(server.Testing_PushSidecarConfig("sidecar:\n  sockets: {enabled: false}\n"))
		removeClient(edgeSession, unnamed.ClientId)
		if record := held(edgeNetworkId, unnamed.ClientId); record != nil {
			t.Fatalf("an edge client's removal with no urnetwork_edge owes a rev/ object: %+v", record)
		}
	})
}
