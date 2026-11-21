package model

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

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

		if first.AuthTime.IsZero() || second.AuthTime.UnixMilli() <= first.AuthTime.UnixMilli() {
			t.Fatalf("edge authentication time did not advance: %s after %s", second.AuthTime, first.AuthTime)
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

// The edge rev effect's world on Postgres, with rev/ on T2's FakeBlobs: an edge network and another network, each with
// its admin's session. Neither is urnetwork_edge until a test's sidecar.yml names one. The tests on it pop each
// sidecar.yml they push when their attempt returns, not at t's cleanup, which the env's rerun of a failed attempt
// runs only after the last attempt, so a rerun starts from the file the env holds.
type edgeRevTestWorld struct {
	revStore       *testRevStore
	edgeNetworkId  server.Id
	edgeSession    *session.ClientSession
	otherNetworkId server.Id
	otherSession   *session.ClientSession
}

// Installs the world's rev/ writer as the process's until t's cleanup.
func newEdgeRevTestWorld(ctx context.Context, t testing.TB) *edgeRevTestWorld {
	t.Helper()
	revStore := newTestRevStore(t)
	// the removal's own clock is real time, so the fake starts at it
	revStore.clock.AdvanceTo(time.Now())
	SetSidecarRevStore(revStore.store)
	t.Cleanup(func() { SetSidecarRevStore(nil) })

	edgeNetworkId := server.NewId()
	edgeUserId := server.NewId()
	Testing_CreateNetwork(ctx, edgeNetworkId, "urnetwork-edge-rev-world-test", edgeUserId)
	otherNetworkId := server.NewId()
	otherUserId := server.NewId()
	Testing_CreateNetwork(ctx, otherNetworkId, "other-rev-world-test", otherUserId)
	return &edgeRevTestWorld{
		revStore:       revStore,
		edgeNetworkId:  edgeNetworkId,
		edgeSession:    session.Testing_CreateClientSession(ctx, &session.ByJwt{NetworkId: edgeNetworkId, UserId: edgeUserId, NetworkName: "urnetwork-edge-rev-world-test"}),
		otherNetworkId: otherNetworkId,
		otherSession:   session.Testing_CreateClientSession(ctx, &session.ByJwt{NetworkId: otherNetworkId, UserId: otherUserId, NetworkName: "other-rev-world-test"}),
	}
}

// An edge client of networkId, minted as api's edge route mints one.
func (self *edgeRevTestWorld) mint(ctx context.Context, t testing.TB, networkId server.Id, slot int) server.Id {
	t.Helper()
	mint, err := MintEdgeClient(ctx, networkId, &EdgeClientKey{Environment: "test", Block: "smtpedge", Host: "lb-a.example", Slot: slot, Index: 0}, nil)
	if err != nil {
		t.Fatal(err)
	}
	return mint.ClientId
}

// A user's client of the other network, on a device of deviceSpec.
func (self *edgeRevTestWorld) authOther(t testing.TB, deviceSpec string) server.Id {
	t.Helper()
	result, err := AuthNetworkClient(&AuthNetworkClientArgs{Description: "synthetic", DeviceSpec: deviceSpec}, self.otherSession)
	if err != nil || result.Error != nil || result.ByClientJwt == nil {
		t.Fatalf("client: %v %+v", err, result)
	}
	byJwt, err := session.ParseByJwt(self.otherSession.Ctx, *result.ByClientJwt)
	if err != nil {
		t.Fatal(err)
	}
	return *byJwt.ClientId
}

// Removes the client through api's removal, as its network's admin.
func (self *edgeRevTestWorld) remove(t testing.TB, clientSession *session.ClientSession, clientId server.Id) {
	t.Helper()
	result, err := RemoveNetworkClient(&RemoveNetworkClientArgs{ClientId: clientId}, clientSession)
	if err != nil || result.Error != nil {
		t.Fatalf("remove: %v %+v", err, result)
	}
}

// The client rev/ object rev/ holds for the client in networkId; nil for none.
func (self *edgeRevTestWorld) held(t testing.TB, networkId server.Id, clientId server.Id) *sidecar.RevocationRecord {
	t.Helper()
	return self.revStore.held(t, &SidecarRevocation{NetworkId: networkId, Kind: sidecar.RevocationKindClient, SubjectId: clientId})
}

// A transaction that records each statement sent through it, then sends it on.
type edgeRevStatementTx struct {
	server.PgTx
	statements []string
}

func (self *edgeRevStatementTx) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	self.statements = append(self.statements, sql)
	return self.PgTx.Exec(ctx, sql, args...)
}

func (self *edgeRevStatementTx) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	self.statements = append(self.statements, sql)
	return self.PgTx.Query(ctx, sql, args...)
}

func (self *edgeRevStatementTx) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	self.statements = append(self.statements, sql)
	return self.PgTx.QueryRow(ctx, sql, args...)
}

func (self *edgeRevStatementTx) SendBatch(ctx context.Context, batch *pgx.Batch) pgx.BatchResults {
	for _, query := range batch.QueuedQueries {
		self.statements = append(self.statements, query.SQL)
	}
	return self.PgTx.SendBatch(ctx, batch)
}

// A process that starts while sidecar.yml is there but broken (QD-1114): its first read is marked and names no
// urnetwork_edge, though the file names the edge network before its broken key, as the good push sockets still holds
// did. A removal then owes the rev/ object of each client on an edge device, in the client's own network, so sockets'
// reader refuses the removed edge client's token from not_before on. A removed ordinary client of another network owes
// none, and a user's client on a device that names itself edge owes one in its own network, the harmless extra the
// answer accepts.
func TestRemoveEdgeClientOwesItsRevocationUnderAFailedFirstRead(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		world := newEdgeRevTestWorld(ctx, t)
		edgeClientId := world.mint(ctx, t, world.edgeNetworkId, 0)
		ordinaryClientId := world.authOther(t, "synthetic")
		namedEdgeClientId := world.authOther(t, EdgeClientDeviceSpec)

		defer server.Testing_PushSidecarConfig(fmt.Sprintf(
			"sidecar:\n  urnetwork_edge: %q\n  evs: {enabled: true, percent: fifty}\n",
			world.edgeNetworkId,
		))()
		config := server.CurrentSidecarConfig()
		if !config.FailedFirstRead() {
			t.Fatal("the broken file's read is not a failed first read")
		}
		if _, ok := config.UrnetworkEdge(); ok {
			t.Fatal("the failed first read names an urnetwork_edge")
		}

		issuedAt := time.Now().Truncate(time.Second)
		world.remove(t, world.edgeSession, edgeClientId)
		record := world.held(t, world.edgeNetworkId, edgeClientId)
		if record == nil {
			t.Fatal("an edge client removed under a failed first read has no rev/ object")
		}
		if record.Reason != sidecar.RevocationReasonRemoved {
			t.Fatalf("reason %s", record.Reason)
		}
		var deactivateTime time.Time
		server.Db(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(ctx, `SELECT deactivate_time FROM network_client WHERE client_id = $1`, edgeClientId).Scan(&deactivateTime))
		})
		if record.NotBefore.Before(deactivateTime) {
			t.Fatalf("not_before %v before the deactivation %v", record.NotBefore, deactivateTime)
		}
		// sockets' reader refuses a token the route issued the client before the removal
		settings := sidecar.DefaultRevocationSettings()
		settings.Clock = world.revStore.clock
		checker := sidecar.NewRevocationChecker(world.revStore.blobs, testRevVerify, settings)
		err := checker.Check(ctx, &sidecar.Principal{
			NetworkId: [16]byte(world.edgeNetworkId),
			ClientId:  [16]byte(edgeClientId),
			Audience:  "sockets",
			IssuedAt:  issuedAt,
			ExpiresAt: issuedAt.Add(15 * time.Minute),
		})
		if !errors.Is(err, sidecar.ErrRevocationVoidsToken) {
			t.Fatalf("the removed edge client's token is not void: %v", err)
		}

		world.remove(t, world.otherSession, ordinaryClientId)
		if record := world.held(t, world.otherNetworkId, ordinaryClientId); record != nil {
			t.Fatalf("an ordinary client's removal under a failed first read owes a rev/ object: %+v", record)
		}

		world.remove(t, world.otherSession, namedEdgeClientId)
		if record := world.held(t, world.otherNetworkId, namedEdgeClientId); record == nil {
			t.Fatal("a client on a device that names itself edge owes no rev/ object in its own network")
		}
		if record := world.held(t, world.edgeNetworkId, namedEdgeClientId); record != nil {
			t.Fatalf("a client of the other network owes a rev/ object in the edge network: %+v", record)
		}
	})
}

// With a good read, sidecar.yml stays the only source (QD-1114): a removal owes the edge effect's object only for a
// client of the urnetwork_edge it names, so an edge client minted in another network owes none, and a failed reread
// keeps that good read and its answers. An absent file is no failed read and names no edge network, so it owes none,
// even for a client on an edge device.
func TestRemoveEdgeClientUnderAGoodReadOwesByUrnetworkEdgeAlone(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		world := newEdgeRevTestWorld(ctx, t)
		edgeClientIds := []server.Id{
			world.mint(ctx, t, world.edgeNetworkId, 0),
			world.mint(ctx, t, world.edgeNetworkId, 1),
			world.mint(ctx, t, world.edgeNetworkId, 2),
		}
		// a derived id names no network, so these take slots of their own
		otherEdgeClientIds := []server.Id{
			world.mint(ctx, t, world.otherNetworkId, 3),
			world.mint(ctx, t, world.otherNetworkId, 4),
		}

		// the test env's config home holds no sidecar.yml, and this read is the process's first
		if _, err := server.Config.SimpleResource("sidecar.yml"); !errors.Is(err, server.ErrResourceNotFound) {
			t.Fatalf("the test env's sidecar.yml is not absent: %v", err)
		}
		server.Testing_ForgetSidecarConfig()
		defer server.Testing_ForgetSidecarConfig()
		if config := server.CurrentSidecarConfig(); config.FailedFirstRead() {
			t.Fatal("the absent file's read is a failed first read")
		}
		world.remove(t, world.edgeSession, edgeClientIds[0])
		if record := world.held(t, world.edgeNetworkId, edgeClientIds[0]); record != nil {
			t.Fatalf("an edge client's removal under an absent file owes a rev/ object: %+v", record)
		}

		defer server.Testing_PushSidecarConfig(fmt.Sprintf("sidecar:\n  urnetwork_edge: %q\n", world.edgeNetworkId))()
		goodRead := server.CurrentSidecarConfig()
		world.remove(t, world.otherSession, otherEdgeClientIds[0])
		if record := world.held(t, world.otherNetworkId, otherEdgeClientIds[0]); record != nil {
			t.Fatalf("an edge client of a network the good read does not name owes a rev/ object: %+v", record)
		}
		world.remove(t, world.edgeSession, edgeClientIds[1])
		if record := world.held(t, world.edgeNetworkId, edgeClientIds[1]); record == nil {
			t.Fatal("a removed client of the named edge network has no rev/ object")
		}

		// a broken push lands under the running process: the reread keeps the good read
		defer server.Testing_RereadSidecarConfig("sidecar: [unterminated\n")()
		if config := server.CurrentSidecarConfig(); config != goodRead || config.FailedFirstRead() {
			t.Fatal("the failed reread did not keep the good read")
		}
		world.remove(t, world.otherSession, otherEdgeClientIds[1])
		if record := world.held(t, world.otherNetworkId, otherEdgeClientIds[1]); record != nil {
			t.Fatalf("after a failed reread an edge client of another network owes a rev/ object: %+v", record)
		}
		world.remove(t, world.edgeSession, edgeClientIds[2])
		if record := world.held(t, world.edgeNetworkId, edgeClientIds[2]); record == nil {
			t.Fatal("after a failed reread a removed client of the named edge network has no rev/ object")
		}
	})
}

// The effect's device query runs only under a failed first read (QD-1114). A removal of a client on an edge device and
// an ordinary client, of a network no read names, sends no statement through the effect under an absent file, a good
// read naming no urnetwork_edge, a good read naming another network, or a failed reread that keeps that good read.
// Under a failed first read a removal of two ordinary clients sends one, the query, and owes nothing; and once the
// first good read clears the mark it sends none again.
func TestEdgeClientRevocationEffectQueriesOnlyUnderAFailedFirstRead(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		world := newEdgeRevTestWorld(ctx, t)
		removal := NewClientRemoval(
			world.otherNetworkId,
			[]server.Id{world.authOther(t, EdgeClientDeviceSpec), world.authOther(t, "synthetic")},
			sidecar.RevocationReasonRemoved,
			server.NowUtc(),
		)
		ordinaryRemoval := NewClientRemoval(
			world.otherNetworkId,
			[]server.Id{world.authOther(t, "synthetic"), world.authOther(t, "synthetic")},
			sidecar.RevocationReasonRemoved,
			server.NowUtc(),
		)
		statements := func(removal *Removal) []string {
			t.Helper()
			var effectErr error
			tx := &edgeRevStatementTx{}
			server.Tx(ctx, func(pgTx server.PgTx) {
				tx.PgTx = pgTx
				tx.statements = nil
				effectErr = applyEdgeClientRevocationEffectInTx(ctx, tx, removal)
			})
			if effectErr != nil {
				t.Fatal(effectErr)
			}
			return tx.statements
		}
		requireNone := func(name string) {
			t.Helper()
			if config := server.CurrentSidecarConfig(); config.FailedFirstRead() {
				t.Fatalf("%s: the read is a failed first read", name)
			}
			if sent := statements(removal); len(sent) != 0 {
				t.Fatalf("%s: the effect sent %d statements: %q", name, len(sent), sent)
			}
		}

		// the test env's config home holds no sidecar.yml, and this read is the process's first
		if _, err := server.Config.SimpleResource("sidecar.yml"); !errors.Is(err, server.ErrResourceNotFound) {
			t.Fatalf("the test env's sidecar.yml is not absent: %v", err)
		}
		server.Testing_ForgetSidecarConfig()
		defer server.Testing_ForgetSidecarConfig()
		requireNone("an absent file")
		defer server.Testing_PushSidecarConfig("sidecar:\n  sockets: {enabled: false}\n")()
		requireNone("a good read naming no urnetwork_edge")
		defer server.Testing_PushSidecarConfig(fmt.Sprintf("sidecar:\n  urnetwork_edge: %q\n", world.edgeNetworkId))()
		requireNone("a good read naming another network")
		defer server.Testing_RereadSidecarConfig("sidecar: [unterminated\n")()
		requireNone("a failed reread after a good read")

		defer server.Testing_PushSidecarConfig("sidecar: [unterminated\n")()
		if config := server.CurrentSidecarConfig(); !config.FailedFirstRead() {
			t.Fatal("the broken file's read is not a failed first read")
		}
		if sent := statements(ordinaryRemoval); len(sent) != 1 {
			t.Fatalf("under the mark a removal of two ordinary clients sent %d statements, want the one query: %q", len(sent), sent)
		}

		// the fixed push lands: the reread, the failed read as its previous, is the first good read
		defer server.Testing_RereadSidecarConfig(fmt.Sprintf("sidecar:\n  urnetwork_edge: %q\n", world.edgeNetworkId))()
		requireNone("the first good read after a failed one")
	})
}
