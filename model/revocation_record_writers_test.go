// Every SQL change that revokes a credential writes the Redis revocation
// record before it commits (owner decision, 2026-10-26: Redis becomes the
// authority), and a change whose record write fails does not commit.
package model

import (
	"context"
	"errors"
	"strconv"
	"testing"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/session"
)

var errTestingRevocationStoreDown = errors.New("synthetic revocation store outage")

func assertRevocationField(t testing.TB, ctx context.Context, key string, field string, want string) {
	t.Helper()
	value, ok, err := session.Testing_RevocationRecordField(ctx, key, field)
	if err != nil {
		t.Fatal(err)
	}
	if !ok || value != want {
		t.Fatalf("record %s field %s = %q (present %v), want %q", key, field, value, ok, want)
	}
}

func assertNetworkRevocationField(t testing.TB, ctx context.Context, networkId server.Id, field string, check func(string) bool) {
	t.Helper()
	for _, key := range session.Testing_RevocationRecordShardKeys(networkId) {
		value, ok, err := session.Testing_RevocationRecordField(ctx, key, field)
		if err != nil {
			t.Fatal(err)
		}
		if !ok || !check(value) {
			t.Fatalf("record %s field %s = %q (present %v)", key, field, value, ok)
		}
	}
}

func testingClientActive(ctx context.Context, clientId server.Id) bool {
	var active bool
	server.Db(ctx, func(conn server.PgConn) {
		server.Raise(conn.QueryRow(ctx, `SELECT COALESCE((SELECT active FROM network_client WHERE client_id = $1), false)`, clientId).Scan(&active))
	})
	return active
}

// Runs f, which must raise, and returns what it raised.
func testingRaised(f func()) (raised error) {
	server.HandleError(f, func(err error) { raised = err })
	return
}

// A removal, by the client's own parent or by the network, tombstones the
// client in its root's record; a removed root also refuses its lineage.
func TestClientRemovalTombstonesRevocationRecordsBeforeCommit(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		networkId := server.NewId()
		parentId, childId, keptId := server.NewId(), server.NewId(), server.NewId()
		parentDeviceId := server.NewId()
		Testing_CreateDevice(ctx, networkId, parentDeviceId, parentId, "test", "test")
		Testing_CreateDevice(ctx, networkId, server.NewId(), keptId, "test", "test")
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO network_client (client_id, network_id, device_id, description, create_time, auth_time, source_client_id) VALUES ($1, $2, $3, 'test', now(), now(), $4)`, childId, networkId, parentDeviceId, parentId))
		})

		// with the store down, nothing is removed
		restore := session.Testing_FailRevocationRecordWrites(errTestingRevocationStoreDown)
		raised := testingRaised(func() {
			RemoveNetworkClient(&RemoveNetworkClientArgs{ClientId: keptId}, &session.ClientSession{Ctx: ctx, ByJwt: &session.ByJwt{NetworkId: networkId}})
		})
		restore()
		if !errors.Is(raised, errTestingRevocationStoreDown) {
			t.Fatalf("a removal without its record write was not refused: %v", raised)
		}
		if !testingClientActive(ctx, keptId) {
			t.Fatal("a removal committed without its record write")
		}

		owned, err := RemoveNetworkClient(&RemoveNetworkClientArgs{ClientId: childId}, &session.ClientSession{
			Ctx:   ctx,
			ByJwt: &session.ByJwt{NetworkId: networkId, ClientId: &parentId, DeviceId: &parentDeviceId},
		})
		if err != nil || owned.Error != nil {
			t.Fatal("owned removal failed", err, owned.Error)
		}
		rootKey := session.Testing_RevocationRecordKey(networkId, parentId)
		assertRevocationField(t, ctx, rootKey, session.Testing_RevocationClientField(childId), session.Testing_RevocationTombstone)

		network, err := RemoveNetworkClient(&RemoveNetworkClientArgs{ClientId: parentId}, &session.ClientSession{Ctx: ctx, ByJwt: &session.ByJwt{NetworkId: networkId}})
		if err != nil || network.Error != nil {
			t.Fatal("network removal failed", err, network.Error)
		}
		assertRevocationField(t, ctx, rootKey, session.Testing_RevocationClientField(parentId), session.Testing_RevocationTombstone)
		assertRevocationField(t, ctx, rootKey, session.Testing_RevocationRootField(parentId), session.Testing_RevocationTombstone)
	})
}

// Both reaps that end an active client, the abandoned top-level mark and the
// hard delete of stale children, tombstone what they end before committing.
func TestDisconnectedClientReapsTombstoneRevocationRecords(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		networkId := server.NewId()
		abandonedId, parentId, childId := server.NewId(), server.NewId(), server.NewId()
		parentDeviceId := server.NewId()
		Testing_CreateDevice(ctx, networkId, server.NewId(), abandonedId, "test", "test")
		Testing_CreateDevice(ctx, networkId, parentDeviceId, parentId, "test", "test")
		now := server.NowUtc()
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE network_client SET auth_time = $2 WHERE client_id = $1`, abandonedId, now.Add(-TopLevelClientIdleExpiration-time.Hour)))
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO network_client (client_id, network_id, device_id, description, create_time, auth_time, source_client_id) VALUES ($1, $2, $3, 'test', $4, $4, $5)`, childId, networkId, parentDeviceId, now.Add(-NetworkClientReapAfterDeactivate-time.Hour), parentId))
		})
		reap := func() {
			RemoveDisconnectedNetworkClients(ctx, now.Add(-8*time.Hour), now.Add(-NetworkClientReapAfterDeactivate), now.Add(-TopLevelClientIdleExpiration))
		}

		restore := session.Testing_FailRevocationRecordWrites(errTestingRevocationStoreDown)
		raised := testingRaised(reap)
		restore()
		if !errors.Is(raised, errTestingRevocationStoreDown) {
			t.Fatalf("a reap without its record write was not refused: %v", raised)
		}
		if !testingClientActive(ctx, abandonedId) {
			t.Fatal("the abandoned mark committed without its record write")
		}

		reap()
		if abandoned, child := testingClientActive(ctx, abandonedId), testingClientActive(ctx, childId); abandoned || child {
			t.Fatalf("the reaps did not end the stale clients: abandoned top level active %v, stale child active %v", abandoned, child)
		}
		abandonedKey := session.Testing_RevocationRecordKey(networkId, abandonedId)
		assertRevocationField(t, ctx, abandonedKey, session.Testing_RevocationClientField(abandonedId), session.Testing_RevocationTombstone)
		assertRevocationField(t, ctx, abandonedKey, session.Testing_RevocationRootField(abandonedId), session.Testing_RevocationTombstone)
		assertRevocationField(t, ctx, session.Testing_RevocationRecordKey(networkId, parentId), session.Testing_RevocationClientField(childId), session.Testing_RevocationTombstone)
	})
}

// A password reset raises every record's credential cutoff of the network
// before the rotation commits.
func TestPasswordResetRaisesEveryRevocationRecordCutoff(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		network, user := server.NewId(), server.NewId()
		userAuth := Testing_CreateNetwork(ctx, network, "revocation-reset", user)
		actor := session.Testing_CreateClientSession(ctx, session.NewByJwt(network, user, "revocation-reset", false, false))
		defer actor.Cancel()
		credentialChangeTime := func() time.Time {
			var value time.Time
			server.Db(ctx, func(conn server.PgConn) {
				server.Raise(conn.QueryRow(ctx, `SELECT credential_change_time FROM network_user WHERE user_id = $1`, user).Scan(&value))
			})
			return value
		}
		initial := credentialChangeTime()

		reset, err := AuthPasswordResetCreateCode(AuthPasswordResetCreateCodeArgs{UserAuth: userAuth}, actor)
		if err != nil || reset.ResetCode == nil {
			t.Fatal(reset, err)
		}
		restore := session.Testing_FailRevocationRecordWrites(errTestingRevocationStoreDown)
		raised := testingRaised(func() {
			AuthPasswordSet(AuthPasswordSetArgs{ResetCode: *reset.ResetCode, Password: "store-down-password"}, actor)
		})
		restore()
		if !errors.Is(raised, errTestingRevocationStoreDown) {
			t.Fatalf("a rotation without its record writes was not refused: %v", raised)
		}
		if !credentialChangeTime().Equal(initial) {
			t.Fatal("a rotation committed without its record writes")
		}

		reset, err = AuthPasswordResetCreateCode(AuthPasswordResetCreateCodeArgs{UserAuth: userAuth}, actor)
		if err != nil || reset.ResetCode == nil {
			t.Fatal(reset, err)
		}
		result, err := AuthPasswordSet(AuthPasswordSetArgs{ResetCode: *reset.ResetCode, Password: "rotated-password"}, actor)
		if err != nil || result == nil || result.Error != nil {
			t.Fatal("rotation failed", result, err)
		}
		cutoff := credentialChangeTime().UnixMicro()
		assertNetworkRevocationField(t, ctx, network, session.Testing_RevocationCredentialField, func(value string) bool {
			micros, err := strconv.ParseInt(value, 10, 64)
			return err == nil && cutoff <= micros
		})
	})
}

// Each network removal marks every record of the network deleted before the
// deletion commits: an account removal, a lag guest's, and a prober shard's.
func TestNetworkRemovalsRetireEveryRevocationRecord(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		deleted := func(value string) bool { return value == "1" }
		networkExists := func(networkId server.Id) bool {
			var exists bool
			server.Db(ctx, func(conn server.PgConn) {
				server.Raise(conn.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM network WHERE network_id = $1)`, networkId).Scan(&exists))
			})
			return exists
		}

		account, user := server.NewId(), server.NewId()
		Testing_CreateNetwork(ctx, account, "revocation-delete", user)
		restore := session.Testing_FailRevocationRecordWrites(errTestingRevocationStoreDown)
		raised := testingRaised(func() { RemoveNetworkWithStoreSnapshot(ctx, account, &user, nil) })
		restore()
		if !errors.Is(raised, errTestingRevocationStoreDown) || !networkExists(account) {
			t.Fatalf("an account removal without its record writes committed or was not refused: %v", raised)
		}
		if outcome, _ := RemoveNetworkWithStoreSnapshot(ctx, account, &user, nil); outcome != RemoveNetworkRemoved || networkExists(account) {
			t.Fatal("account removal failed", outcome)
		}
		assertNetworkRevocationField(t, ctx, account, session.Testing_RevocationDeletedField, deleted)

		now := server.NowUtc()
		guest := CreateLagGuestNetwork(ctx, now, now.Add(time.Hour))
		if !RemoveLagGuest(ctx, guest.NetworkId) || networkExists(guest.NetworkId) {
			t.Fatal("lag guest removal failed")
		}
		assertNetworkRevocationField(t, ctx, guest.NetworkId, session.Testing_RevocationDeletedField, deleted)

		owner := shardTestOwner(t, ctx, shardTestKey(0))
		if err := DrainProberShard(ctx, owner.Key); err != nil {
			t.Fatal(err)
		}
		if done, err := ReapProberShard(ctx, owner.Key); err != nil || !done {
			t.Fatal("prober shard reap failed", err)
		}
		assertNetworkRevocationField(t, ctx, owner.NetworkId, session.Testing_RevocationDeletedField, deleted)
	})
}
