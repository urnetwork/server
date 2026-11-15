//go:build sidecar_pending

package model

import (
	"bytes"
	"context"
	"crypto/rand"
	"testing"
	"time"

	"github.com/urnetwork/sidecar"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/session"
)

// §5.15's identity tables, which need MG1. The model does not verify
// signatures, the controller does, so these records carry synthetic bytes in
// place of a signed binding.

// A synthetic 32-byte key.
func testSidecarKey(t testing.TB) []byte {
	t.Helper()
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatal(err)
	}
	return key
}

// A record of generation for sidecarId's identity key, under a new client, with
// bytes that name it.
func testSidecarBindingRecord(t testing.TB, networkId server.Id, sidecarId server.Id, identityPublicKey []byte, generation int64) *SidecarBindingRecord {
	t.Helper()
	clientId := server.NewId()
	return &SidecarBindingRecord{
		NetworkId:         networkId,
		SidecarId:         sidecarId,
		ClientId:          clientId,
		Generation:        generation,
		ClientPublicKey:   testSidecarKey(t),
		IdentityPublicKey: identityPublicKey,
		SignedBinding:     append([]byte("synthetic binding of "), clientId.Bytes()...),
	}
}

// The number of binding rows sidecarId has.
func countSidecarBindings(t testing.TB, ctx context.Context, sidecarId server.Id) int {
	t.Helper()
	count := 0
	server.Db(ctx, func(conn server.PgConn) {
		server.Raise(conn.QueryRow(ctx, `SELECT COUNT(*) FROM sidecar_identity_binding WHERE sidecar_id = $1`, sidecarId).Scan(&count))
	})
	return count
}

// Whether sidecarId's identity row exists.
func sidecarIdentityExists(t testing.TB, ctx context.Context, sidecarId server.Id) bool {
	t.Helper()
	exists := false
	server.Db(ctx, func(conn server.PgConn) {
		server.Raise(conn.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM sidecar_identity WHERE sidecar_id = $1)`, sidecarId).Scan(&exists))
	})
	return exists
}

// A first binding creates the identity at generation 1, each later one is the
// newest plus one under the same identity key, and anything else conflicts.
func TestBindAppendsGeneration(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		networkId := server.NewId()
		sidecarId := server.NewId()
		identityKey := testSidecarKey(t)
		now := server.NowUtc()

		outcome, generation := BindSidecarIdentity(ctx, testSidecarBindingRecord(t, networkId, sidecarId, identityKey, 1), now)
		if outcome != SidecarBindAppended || generation != 1 {
			t.Fatalf("first: %s %d", outcome, generation)
		}
		outcome, generation = BindSidecarIdentity(ctx, testSidecarBindingRecord(t, networkId, sidecarId, identityKey, 2), now)
		if outcome != SidecarBindAppended || generation != 2 {
			t.Fatalf("second: %s %d", outcome, generation)
		}
		for name, record := range map[string]*SidecarBindingRecord{
			"a skipped generation":  testSidecarBindingRecord(t, networkId, sidecarId, identityKey, 4),
			"a repeated generation": testSidecarBindingRecord(t, networkId, sidecarId, identityKey, 2),
			"generation one again":  testSidecarBindingRecord(t, networkId, sidecarId, identityKey, 1),
			"another identity key":  testSidecarBindingRecord(t, networkId, sidecarId, testSidecarKey(t), 3),
			"another network":       testSidecarBindingRecord(t, server.NewId(), sidecarId, identityKey, 3),
		} {
			if outcome, _ := BindSidecarIdentity(ctx, record, now); outcome != SidecarBindGenerationConflict {
				t.Errorf("%s: %s", name, outcome)
			}
		}
		if count := countSidecarBindings(t, ctx, sidecarId); count != 2 {
			t.Fatalf("%d bindings", count)
		}
		view := GetSidecarIdentity(ctx, networkId, sidecarId, 0)
		if view == nil || view.Status != SidecarIdentityStatusActive || view.IdentityAlg != SidecarIdentityAlgEd25519 ||
			!bytes.Equal(view.IdentityPublicKey, identityKey) || len(view.Bindings) != 2 {
			t.Fatalf("view: %+v", view)
		}
	})
}

// A client binds exactly once, under any sidecar_id: a copied token cannot bind
// a key of its own and refresh under it (§5.7, plan-CORE-binding-wire #2).
func TestBindRefusesSecondBindOfClient(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		networkId := server.NewId()
		now := server.NowUtc()
		first := testSidecarBindingRecord(t, networkId, server.NewId(), testSidecarKey(t), 1)
		if outcome, _ := BindSidecarIdentity(ctx, first, now); outcome != SidecarBindAppended {
			t.Fatalf("%s", outcome)
		}
		// the same client under another sidecar, and under its own with other bytes
		other := testSidecarBindingRecord(t, networkId, server.NewId(), testSidecarKey(t), 1)
		other.ClientId = first.ClientId
		if outcome, _ := BindSidecarIdentity(ctx, other, now); outcome != SidecarBindClientBound {
			t.Errorf("another sidecar: %s", outcome)
		}
		again := *first
		again.Generation = 2
		again.SignedBinding = []byte("other synthetic bytes")
		if outcome, _ := BindSidecarIdentity(ctx, &again, now); outcome != SidecarBindClientBound {
			t.Errorf("other bytes: %s", outcome)
		}
		if sidecarIdentityExists(t, ctx, other.SidecarId) {
			t.Error("a refused bind created an identity")
		}
	})
}

// A byte-identical retry answers the stored generation and writes nothing
// (plan-CORE-binding-wire #5).
func TestBindRetryIsIdempotent(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		networkId := server.NewId()
		sidecarId := server.NewId()
		identityKey := testSidecarKey(t)
		now := server.NowUtc()
		BindSidecarIdentity(ctx, testSidecarBindingRecord(t, networkId, sidecarId, identityKey, 1), now)
		record := testSidecarBindingRecord(t, networkId, sidecarId, identityKey, 2)
		if outcome, _ := BindSidecarIdentity(ctx, record, now); outcome != SidecarBindAppended {
			t.Fatalf("%s", outcome)
		}
		for range 2 {
			outcome, generation := BindSidecarIdentity(ctx, record, now.Add(time.Minute))
			if outcome != SidecarBindRetried || generation != 2 {
				t.Fatalf("retry: %s %d", outcome, generation)
			}
		}
		if count := countSidecarBindings(t, ctx, sidecarId); count != 2 {
			t.Fatalf("%d bindings", count)
		}
	})
}

// A revoked identity binds nothing more, and its clients lose their proof key.
func TestBindRefusesRevokedSidecar(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		networkId := server.NewId()
		sidecarId := server.NewId()
		identityKey := testSidecarKey(t)
		now := server.NowUtc()
		first := testSidecarBindingRecord(t, networkId, sidecarId, identityKey, 1)
		BindSidecarIdentity(ctx, first, now)
		if !SetSidecarIdentityRevoked(ctx, sidecarId, now) {
			t.Fatal("not revoked")
		}
		if SetSidecarIdentityRevoked(ctx, sidecarId, now) {
			t.Fatal("revoked twice")
		}
		if outcome, _ := BindSidecarIdentity(ctx, testSidecarBindingRecord(t, networkId, sidecarId, identityKey, 2), now); outcome != SidecarBindSidecarRevoked {
			t.Fatalf("%s", outcome)
		}
		if _, err := GetClientBindingKey(ctx, first.ClientId); err != ErrClientBindingKeyNotFound {
			t.Fatalf("a revoked identity's key: %v", err)
		}
	})
}

// After a restore lost the identity row, a binding past generation 1 is taken
// only once the projection admits its identity key (§5.6, §19.10, CORE-M2-07):
// with no sequencer installed the default seam refuses, and a projection
// admitting another key refuses, both as generation_conflict, creating nothing.
func TestRestoreBindWaitsForProjection(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		networkId := server.NewId()
		now := server.NowUtc()

		restoreSequencer := configLogSequencer.Swap(nil)
		refused := testSidecarBindingRecord(t, networkId, server.NewId(), testSidecarKey(t), 3)
		outcome, _ := BindSidecarIdentity(ctx, refused, now)
		configLogSequencer.Store(restoreSequencer)
		if outcome != SidecarBindGenerationConflict || sidecarIdentityExists(t, ctx, refused.SidecarId) {
			t.Fatalf("a re-registration with no projection: %s", outcome)
		}

		admittedKey := testSidecarKey(t)
		restore := Testing_SetSidecarReregistrationAdmits(func(ctx context.Context, seamNetworkId server.Id, sidecarId server.Id, identityPublicKey []byte) bool {
			return seamNetworkId == networkId && bytes.Equal(identityPublicKey, admittedKey)
		})
		defer restore()
		otherKey := testSidecarBindingRecord(t, networkId, server.NewId(), testSidecarKey(t), 3)
		if outcome, _ := BindSidecarIdentity(ctx, otherKey, now); outcome != SidecarBindGenerationConflict || sidecarIdentityExists(t, ctx, otherKey.SidecarId) {
			t.Fatalf("a key the projection does not admit: %s", outcome)
		}
		admitted := testSidecarBindingRecord(t, networkId, server.NewId(), admittedKey, 3)
		outcome, generation := BindSidecarIdentity(ctx, admitted, now)
		if outcome != SidecarBindAppended || generation != 3 {
			t.Fatalf("an admitted re-registration: %s %d", outcome, generation)
		}
		next := testSidecarBindingRecord(t, networkId, admitted.SidecarId, admitted.IdentityPublicKey, 4)
		if outcome, _ := BindSidecarIdentity(ctx, next, now); outcome != SidecarBindAppended {
			t.Fatalf("the generation after: %s", outcome)
		}
	})
}

// A landed SidecarRevoke's removal marks the identity revoked through the
// removal chain (CORE-M2-07), once: a later bind answers sidecar_revoked and
// the client's proof key is gone, and the effect applied again changes nothing.
func TestBindRefusesRevoked(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		networkId := server.NewId()
		sidecarId := server.NewId()
		identityKey := testSidecarKey(t)
		now := server.NowUtc()
		first := testSidecarBindingRecord(t, networkId, sidecarId, identityKey, 1)
		if outcome, _ := BindSidecarIdentity(ctx, first, now); outcome != SidecarBindAppended {
			t.Fatalf("bind: %s", outcome)
		}
		removal := &Removal{
			Subjects: []*RemovalSubject{{NetworkId: networkId, Kind: sidecar.RevocationKindSidecar, SubjectId: sidecarId}},
			Reason:   sidecar.RevocationReasonRevoked,
			Time:     now,
		}
		for range 2 {
			server.Tx(ctx, func(tx server.PgTx) {
				server.Raise(applySidecarIdentityRemovalEffectInTx(ctx, tx, removal))
			})
		}
		status := ""
		var revokeTime time.Time
		server.Db(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(ctx, `SELECT status, revoke_time FROM sidecar_identity WHERE sidecar_id = $1`, sidecarId).Scan(&status, &revokeTime))
		})
		if status != SidecarIdentityStatusRevoked || !revokeTime.Equal(now) {
			t.Fatalf("status %q at %v", status, revokeTime)
		}
		if outcome, _ := BindSidecarIdentity(ctx, testSidecarBindingRecord(t, networkId, sidecarId, identityKey, 2), now); outcome != SidecarBindSidecarRevoked {
			t.Fatalf("a revoked sidecar's bind: %s", outcome)
		}
		if _, err := GetClientBindingKey(ctx, first.ClientId); err != ErrClientBindingKeyNotFound {
			t.Fatalf("a revoked identity's key: %v", err)
		}
		// a client's removal names no sidecar, and leaves another identity active
		otherId := server.NewId()
		other := testSidecarBindingRecord(t, networkId, otherId, testSidecarKey(t), 1)
		BindSidecarIdentity(ctx, other, now)
		server.Tx(ctx, func(tx server.PgTx) {
			server.Raise(applySidecarIdentityRemovalEffectInTx(ctx, tx, NewClientRemoval(networkId, []server.Id{other.ClientId}, sidecar.RevocationReasonRemoved, now)))
		})
		if _, err := GetClientBindingKey(ctx, other.ClientId); err != nil {
			t.Fatalf("a client's removal revoked its identity: %v", err)
		}
	})
}

// §7.4's proof key is the client's binding key only while that binding is its
// sidecar's newest: an older generation's key is refused (TEAM-M1-02).
func TestClientBindingKeyIsTheNewestGenerations(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		networkId := server.NewId()
		sidecarId := server.NewId()
		identityKey := testSidecarKey(t)
		now := server.NowUtc()
		first := testSidecarBindingRecord(t, networkId, sidecarId, identityKey, 1)
		BindSidecarIdentity(ctx, first, now)
		key, err := GetClientBindingKey(ctx, first.ClientId)
		if err != nil || !bytes.Equal(key, first.ClientPublicKey) {
			t.Fatalf("the newest: %v", err)
		}
		second := testSidecarBindingRecord(t, networkId, sidecarId, identityKey, 2)
		BindSidecarIdentity(ctx, second, now)
		if _, err := GetClientBindingKey(ctx, first.ClientId); err != ErrClientBindingKeyNotFound {
			t.Fatalf("an older generation's key: %v", err)
		}
		if key, err := GetClientBindingKey(ctx, second.ClientId); err != nil || !bytes.Equal(key, second.ClientPublicKey) {
			t.Fatalf("the newer: %v", err)
		}
		if _, err := GetClientBindingKey(ctx, server.NewId()); err != ErrClientBindingKeyNotFound {
			t.Fatalf("an unbound client: %v", err)
		}
		if key, err := ClientProofPublicKey(ctx, second.ClientId); err != nil || !bytes.Equal(key, second.ClientPublicKey) {
			t.Fatalf("the proof key: %v", err)
		}
	})
}

// The reaper removes clients and never an identity or a binding: the identity
// is durable across client generations (§5.5, §5.15).
func TestIdentityRowsSurviveReaper(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		networkId := server.NewId()
		userId := server.NewId()
		Testing_CreateNetwork(ctx, networkId, "sidecar-reaper", userId)
		userSession := session.Testing_CreateClientSession(ctx, &session.ByJwt{NetworkId: networkId, UserId: userId, NetworkName: "sidecar-reaper"})
		authResult, err := AuthNetworkClient(&AuthNetworkClientArgs{Description: "synthetic", DeviceSpec: "synthetic"}, userSession)
		if err != nil || authResult.Error != nil || authResult.ByClientJwt == nil {
			t.Fatalf("client: %v %+v", err, authResult)
		}
		byJwt, err := session.ParseByJwt(ctx, *authResult.ByClientJwt)
		if err != nil {
			t.Fatal(err)
		}
		sidecarId := server.NewId()
		record := testSidecarBindingRecord(t, networkId, sidecarId, testSidecarKey(t), 1)
		record.ClientId = *byJwt.ClientId
		if outcome, _ := BindSidecarIdentity(ctx, record, server.NowUtc()); outcome != SidecarBindAppended {
			t.Fatalf("%s", outcome)
		}
		// the client deactivated long ago, so the reaper deletes it
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE network_client SET active = false, deactivate_time = $2 WHERE client_id = $1`, record.ClientId, server.NowUtc().Add(-time.Hour)))
		})
		RemoveDisconnectedNetworkClients(ctx, server.NowUtc(), server.NowUtc(), time.Time{})
		clientExists := true
		server.Db(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM network_client WHERE client_id = $1)`, record.ClientId).Scan(&clientExists))
		})
		if clientExists {
			t.Fatal("the reaper did not reach the client, so this proves nothing")
		}
		if !sidecarIdentityExists(t, ctx, sidecarId) || countSidecarBindings(t, ctx, sidecarId) != 1 {
			t.Fatal("the reaper took an identity row")
		}
	})
}

// A network's deletion takes its identities and bindings, and nobody else's.
func TestIdentityDeletedWithNetwork(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		networkId := server.NewId()
		userId := server.NewId()
		Testing_CreateNetwork(ctx, networkId, "sidecar-delete", userId)
		otherNetworkId := server.NewId()
		Testing_CreateNetwork(ctx, otherNetworkId, "sidecar-keep", server.NewId())
		now := server.NowUtc()

		sidecarId := server.NewId()
		identityKey := testSidecarKey(t)
		BindSidecarIdentity(ctx, testSidecarBindingRecord(t, networkId, sidecarId, identityKey, 1), now)
		BindSidecarIdentity(ctx, testSidecarBindingRecord(t, networkId, sidecarId, identityKey, 2), now)
		otherSidecarId := server.NewId()
		BindSidecarIdentity(ctx, testSidecarBindingRecord(t, otherNetworkId, otherSidecarId, testSidecarKey(t), 1), now)

		if removed, _ := RemoveNetwork(ctx, networkId, nil); !removed {
			t.Fatal("not removed")
		}
		if sidecarIdentityExists(t, ctx, sidecarId) || countSidecarBindings(t, ctx, sidecarId) != 0 {
			t.Fatal("the deleted network's identity stayed")
		}
		if !sidecarIdentityExists(t, ctx, otherSidecarId) || countSidecarBindings(t, ctx, otherSidecarId) != 1 {
			t.Fatal("another network's identity went")
		}
	})
}

// The lookups serve only the caller's network, page the bindings newest first,
// and find a client of any generation.
func TestIdentityLookupsServeTheCallersNetwork(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		networkId := server.NewId()
		sidecarId := server.NewId()
		identityKey := testSidecarKey(t)
		now := server.NowUtc()
		records := []*SidecarBindingRecord{}
		for generation := int64(1); generation <= SidecarIdentityBindingPageCount+2; generation += 1 {
			record := testSidecarBindingRecord(t, networkId, sidecarId, identityKey, generation)
			if outcome, _ := BindSidecarIdentity(ctx, record, now); outcome != SidecarBindAppended {
				t.Fatalf("generation %d: %s", generation, outcome)
			}
			records = append(records, record)
		}
		newest := records[len(records)-1]
		view := GetSidecarIdentity(ctx, networkId, sidecarId, 0)
		if view == nil || !bytes.Equal(view.Binding, newest.SignedBinding) || len(view.Bindings) != SidecarIdentityBindingPageCount ||
			!bytes.Equal(view.Bindings[0], newest.SignedBinding) {
			t.Fatal("the first page")
		}
		page := GetSidecarIdentity(ctx, networkId, sidecarId, 3)
		if page == nil || len(page.Bindings) != 2 || !bytes.Equal(page.Bindings[0], records[1].SignedBinding) ||
			!bytes.Equal(page.Bindings[1], records[0].SignedBinding) || !bytes.Equal(page.Binding, newest.SignedBinding) {
			t.Fatal("the page below generation 3")
		}
		if GetSidecarIdentity(ctx, server.NewId(), sidecarId, 0) != nil {
			t.Fatal("another network read the identity")
		}
		byClient := GetSidecarIdentityByClient(ctx, networkId, records[0].ClientId)
		if byClient == nil || byClient.SidecarId != sidecarId || !bytes.Equal(byClient.Binding, records[0].SignedBinding) {
			t.Fatal("a client of the first generation")
		}
		if GetSidecarIdentityByClient(ctx, server.NewId(), records[0].ClientId) != nil {
			t.Fatal("another network read a client's binding")
		}
	})
}

// restore_required while api lacks the client's binding or identity, or a
// registration an area's check misses, and not otherwise.
func TestRestoreRequiredReadsBindingsAndRegistrations(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		networkId := server.NewId()
		record := testSidecarBindingRecord(t, networkId, server.NewId(), testSidecarKey(t), 1)
		if !SidecarRestoreRequired(ctx, networkId, record.ClientId) {
			t.Fatal("no binding is no restore")
		}
		BindSidecarIdentity(ctx, record, server.NowUtc())
		if SidecarRestoreRequired(ctx, networkId, record.ClientId) {
			t.Fatal("a bound client must re-register")
		}
		if !SidecarRestoreRequired(ctx, server.NewId(), record.ClientId) {
			t.Fatal("a binding of another network counts")
		}
		registered := false
		AddSidecarRegistrationCheck("synthetic", func(context.Context, server.Id, server.Id) (bool, error) { return registered, nil })
		defer Testing_RemoveSidecarRegistrationCheck("synthetic")
		if !SidecarRestoreRequired(ctx, networkId, record.ClientId) {
			t.Fatal("a missing registration is no restore")
		}
		registered = true
		if SidecarRestoreRequired(ctx, networkId, record.ClientId) {
			t.Fatal("a present registration must re-register")
		}
	})
}

// §5.15's caller rule across §8.11's shares (CORE-M6-04): a network reads
// another's identities, by sidecar_id and by client, only while a share binds
// the two, accepted with status active, whichever of them shares. An invite
// not yet accepted, an accept refused for its size, a third network and a
// share whose row went at its revocation read nothing, as though api held
// none; the identity's own network always reads it.
func TestIdentityReadableAcrossActiveShareOnly(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		bridgeShareTestAllowOutbound(t)
		x := newBridgeShareTestSide(t, ctx)
		y := newBridgeShareTestSide(t, ctx)
		z := newBridgeShareTestSide(t, ctx)
		// readable answers whether reader's network reads owner's identity by
		// both lookups, failing the test when they disagree
		readable := func(name string, reader *bridgeShareTestSide, owner *bridgeShareTestSide) bool {
			t.Helper()
			bySidecar := GetSidecarIdentity(ctx, reader.networkId, owner.sidecarId, 0)
			byClient := GetSidecarIdentityByClient(ctx, reader.networkId, *owner.clientSession.ByJwt.ClientId)
			if (bySidecar == nil) != (byClient == nil) {
				t.Fatalf("%s: the lookups disagree, by sidecar_id %t, by client %t", name, bySidecar != nil, byClient != nil)
			}
			if bySidecar != nil && (bySidecar.SidecarId != owner.sidecarId || bySidecar.NetworkId != owner.networkId ||
				len(bySidecar.Binding) == 0 || len(bySidecar.Bindings) != 1 || byClient.SidecarId != owner.sidecarId) {
				t.Fatalf("%s: the view %+v, by client %+v", name, bySidecar, byClient)
			}
			return bySidecar != nil
		}
		for _, side := range []*bridgeShareTestSide{x, y, z} {
			if !readable("its own network", side, side) {
				t.Fatal("a network does not read its own identity")
			}
		}
		if readable("no share", y, x) || readable("no share, the other way", x, y) {
			t.Fatal("a network reads another's identity with no share between them")
		}

		// x files a share and y has not accepted it
		pendingId := server.NewId()
		server.Tx(ctx, func(tx server.PgTx) {
			if err := CreateNetworkBridgeShareInTx(ctx, tx, pendingId, x.networkId, bridgeShareTestSeal(t, []byte("targets")), 1); err != nil {
				t.Fatalf("create: %v", err)
			}
		})
		if readable("an invite not accepted", y, x) {
			t.Fatal("an unaccepted share binds the networks")
		}
		// an accept refused for its size binds nothing
		refusedId, _, _ := bridgeShareTestAccepted(t, ctx, x, z, NetworkBridgeShareAcceptStatusRefusedTooLarge)
		if readable("a refused accept", z, x) || readable("a refused accept, the other way", x, z) {
			t.Fatal("a refused share binds the networks")
		}

		// once y accepts with status active, each network reads the other's
		shareId, _, _ := bridgeShareTestAccepted(t, ctx, x, y, NetworkBridgeShareAcceptStatusActive)
		if !readable("the invited network", y, x) || !readable("the sharing network", x, y) {
			t.Fatal("an active share does not bind its networks")
		}
		if readable("a third network", z, y) || readable("a third network, the other way", y, z) {
			t.Fatal("a share binds a network that is no side of it")
		}

		// the share's revocation removes its row: nothing reads across it after
		if !RemoveNetworkBridgeShare(ctx, shareId) {
			t.Fatal("the share's row did not go")
		}
		if readable("a revoked share", y, x) || readable("a revoked share, the other way", x, y) {
			t.Fatal("a revoked share still binds the networks")
		}
		RemoveNetworkBridgeShare(ctx, pendingId)
		RemoveNetworkBridgeShare(ctx, refusedId)
	})
}
