//go:build sidecar_pending

package model

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"testing"
	"time"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/sidecar"

	"github.com/urnetwork/server"
)

// §18.8's lease and §7.7's ephemeral reap (TEAM-M1-10), which need MG1.

// An ephemeral client: a client of the fixture's network with a lease seen at seenTime, and the close
// capability its mint returned.
type testEphemeralClient struct {
	clientId   server.Id
	capability string
}

func newTestEphemeralClient(t testing.TB, self *clientOwnerFixture, seenTime time.Time) *testEphemeralClient {
	t.Helper()
	client := self.mint(t, self.root, &AuthNetworkClientArgs{Description: "page", DeviceSpec: "test"})
	ephemeral := &testEphemeralClient{clientId: *client.ClientId}
	server.Tx(self.ctx, func(tx server.PgTx) {
		ephemeral.capability = InsertClientLeaseInTx(self.ctx, tx, ephemeral.clientId, seenTime)
	})
	return ephemeral
}

// The client's lease seen_time, false for no lease.
func testLeaseSeenTime(t testing.TB, ctx context.Context, clientId server.Id) (seenTime time.Time, found bool) {
	t.Helper()
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(ctx, `SELECT seen_time FROM network_client_lease WHERE client_id = $1`, clientId)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				server.Raise(result.Scan(&seenTime))
				found = true
			}
		})
	})
	return
}

func testClientActive(t testing.TB, ctx context.Context, clientId server.Id) (active bool) {
	t.Helper()
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(ctx, `SELECT active FROM network_client WHERE client_id = $1`, clientId)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				server.Raise(result.Scan(&active))
			}
		})
	})
	return
}

// A connection of the client that opened at connectTime and, unless disconnectTime is nil, closed then.
func testClientConnection(t testing.TB, ctx context.Context, clientId server.Id, connectTime time.Time, disconnectTime *time.Time) {
	t.Helper()
	handlerId := CreateNetworkClientHandler(ctx)
	connectionId, _, _, _, err := ConnectNetworkClient(ctx, clientId, "192.0.2.7:1", handlerId)
	if err != nil {
		t.Fatal(err)
	}
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(
			ctx,
			`
				UPDATE network_client_connection
				SET connect_time = $2, connected = $3, disconnect_time = $4
				WHERE connection_id = $1
			`,
			connectionId,
			connectTime,
			disconnectTime == nil,
			disconnectTime,
		))
	})
}

func testReapSettings() *NetworkClientLeaseSettings {
	settings := DefaultNetworkClientLeaseSettings()
	settings.ReapPassTimeout = time.Minute
	return settings
}

// A pass reaps until it has looked at every row past the threshold.
func testReapAll(t testing.TB, ctx context.Context, now time.Time, settings *NetworkClientLeaseSettings) (reapedCount int) {
	t.Helper()
	var after *ReapEphemeralClientsCursor
	for range 100 {
		result, err := ReapEphemeralClients(ctx, now, after, settings)
		if err != nil {
			t.Fatal(err)
		}
		reapedCount += result.ReapedCount
		if result.Cursor == nil {
			return
		}
		after = result.Cursor
	}
	t.Fatal("the pass never finished")
	return
}

// The predicate on the pass's clock: a lease 30 and 35 min old keeps its client, one 36 min old with no
// connection reaps it, and the reap removes the lease with the client.
func TestEphemeralReapAtThirtyThirtyFiveAndThirtySixMinutes(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		self := newClientOwnerFixture(t)
		seen := server.NowUtc().Truncate(time.Second)
		ephemeral := newTestEphemeralClient(t, self, seen)
		settings := testReapSettings()

		for _, minutes := range []int{30, 35} {
			if reaped := testReapAll(t, self.ctx, seen.Add(time.Duration(minutes)*time.Minute), settings); reaped != 0 {
				t.Fatalf("at %d min reaped %d", minutes, reaped)
			}
			connect.AssertEqual(t, testClientActive(t, self.ctx, ephemeral.clientId), true)
		}
		connect.AssertEqual(t, testReapAll(t, self.ctx, seen.Add(36*time.Minute), settings), 1)
		connect.AssertEqual(t, testClientActive(t, self.ctx, ephemeral.clientId), false)
		_, found := testLeaseSeenTime(t, self.ctx, ephemeral.clientId)
		connect.AssertEqual(t, found, false)
	})
}

// A client connected for hours survives a disconnect under 35 min, and an open connection keeps any
// client; once its newest disconnect is over 35 min old too, it is reaped.
func TestEphemeralReapSparesARecentDisconnectAndAnOpenConnection(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		self := newClientOwnerFixture(t)
		now := server.NowUtc().Truncate(time.Second).Add(2 * time.Hour)
		settings := testReapSettings()

		agent := newTestEphemeralClient(t, self, now.Add(-5*time.Hour))
		disconnected := now.Add(-10 * time.Minute)
		testClientConnection(t, self.ctx, agent.clientId, now.Add(-5*time.Hour), &disconnected)

		open := newTestEphemeralClient(t, self, now.Add(-5*time.Hour))
		testClientConnection(t, self.ctx, open.clientId, now.Add(-5*time.Hour), nil)

		connect.AssertEqual(t, testReapAll(t, self.ctx, now, settings), 0)
		connect.AssertEqual(t, testClientActive(t, self.ctx, agent.clientId), true)
		connect.AssertEqual(t, testClientActive(t, self.ctx, open.clientId), true)

		// 36 min after the disconnect the agent goes; the open connection still keeps the other
		connect.AssertEqual(t, testReapAll(t, self.ctx, disconnected.Add(36*time.Minute), settings), 1)
		connect.AssertEqual(t, testClientActive(t, self.ctx, agent.clientId), false)
		connect.AssertEqual(t, testClientActive(t, self.ctx, open.clientId), true)
	})
}

// A poll's touch moves seen_time, which keeps the client past the time its old lease would have reaped
// it; a touch never moves seen_time back, and a client with no lease writes nothing.
func TestEphemeralLeaseTouchKeepsTheClient(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		self := newClientOwnerFixture(t)
		seen := server.NowUtc().Truncate(time.Second)
		ephemeral := newTestEphemeralClient(t, self, seen)

		touched := seen.Add(20 * time.Minute)
		if err := touchClientLeaseAt(self.ctx, ephemeral.clientId, touched); err != nil {
			t.Fatal(err)
		}
		if err := touchClientLeaseAt(self.ctx, ephemeral.clientId, seen.Add(time.Minute)); err != nil {
			t.Fatal(err)
		}
		seenTime, found := testLeaseSeenTime(t, self.ctx, ephemeral.clientId)
		connect.AssertEqual(t, found, true)
		connect.AssertEqual(t, seenTime.Equal(touched), true)

		connect.AssertEqual(t, testReapAll(t, self.ctx, seen.Add(40*time.Minute), testReapSettings()), 0)
		connect.AssertEqual(t, testReapAll(t, self.ctx, touched.Add(36*time.Minute), testReapSettings()), 1)

		legacy := self.mint(t, self.root, &AuthNetworkClientArgs{Description: "device", DeviceSpec: "test"})
		if err := TouchClientLease(self.ctx, connect.Id(*legacy.ClientId)); err != nil {
			t.Fatal(err)
		}
		_, found = testLeaseSeenTime(t, self.ctx, *legacy.ClientId)
		connect.AssertEqual(t, found, false)
	})
}

// The close capability is 128 bits, returned once and kept only as its SHA-256; it reaps its client at
// once, once; a wrong or malformed one does nothing.
func TestEphemeralCloseCapabilityReapsOnce(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		self := newClientOwnerFixture(t)
		ephemeral := newTestEphemeralClient(t, self, server.NowUtc())
		other := newTestEphemeralClient(t, self, server.NowUtc())

		capabilityBytes, err := base64.RawURLEncoding.DecodeString(ephemeral.capability)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, len(capabilityBytes), 16)
		var stored []byte
		server.Db(self.ctx, func(conn server.PgConn) {
			result, err := conn.Query(self.ctx, `SELECT close_sha256 FROM network_client_lease WHERE client_id = $1`, ephemeral.clientId)
			server.WithPgResult(result, err, func() {
				if result.Next() {
					server.Raise(result.Scan(&stored))
				}
			})
		})
		sum := sha256.Sum256(capabilityBytes)
		connect.AssertEqual(t, bytes.Equal(stored, sum[:]), true)
		connect.AssertEqual(t, bytes.Contains(stored, capabilityBytes), false)

		wrongBytes := bytes.Clone(capabilityBytes)
		wrongBytes[0] ^= 1
		for _, wrong := range []string{
			base64.RawURLEncoding.EncodeToString(wrongBytes),
			"",
			ephemeral.capability + "=",
			ephemeral.capability[:21],
			base64.StdEncoding.EncodeToString(capabilityBytes),
		} {
			reaped, err := CloseEphemeralClient(self.ctx, wrong)
			connect.AssertEqual(t, err, nil)
			connect.AssertEqual(t, reaped, false)
		}
		connect.AssertEqual(t, testClientActive(t, self.ctx, ephemeral.clientId), true)

		reaped, err := CloseEphemeralClient(self.ctx, ephemeral.capability)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, reaped, true)
		connect.AssertEqual(t, testClientActive(t, self.ctx, ephemeral.clientId), false)
		reaped, err = CloseEphemeralClient(self.ctx, ephemeral.capability)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, reaped, false)
		connect.AssertEqual(t, testClientActive(t, self.ctx, other.clientId), true)
	})
}

// One pass looks at no more than its count and hands on its cursor, so every reapable client goes over
// a chain of passes; a lease outliving a removed client is cleared.
func TestEphemeralReapPassIsBounded(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		self := newClientOwnerFixture(t)
		seen := server.NowUtc().Truncate(time.Second)
		clients := []*testEphemeralClient{}
		for i := range 5 {
			clients = append(clients, newTestEphemeralClient(t, self, seen.Add(time.Duration(i)*time.Second)))
		}
		// a lease whose client was removed by a binary that did not run the lease effect
		server.Tx(self.ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(self.ctx, `UPDATE network_client SET active = false WHERE client_id = $1`, clients[4].clientId))
		})

		settings := testReapSettings()
		settings.ReapPassCount = 2
		now := seen.Add(time.Hour)
		first, err := ReapEphemeralClients(self.ctx, now, nil, settings)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, first.LookedCount, 2)
		connect.AssertEqual(t, first.ReapedCount, 2)
		connect.AssertEqual(t, first.Cursor != nil && first.Cursor.ClientId == clients[1].clientId, true)

		second, err := ReapEphemeralClients(self.ctx, now, first.Cursor, settings)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, second.LookedCount, 2)
		third, err := ReapEphemeralClients(self.ctx, now, second.Cursor, settings)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, third.LookedCount, 1)
		connect.AssertEqual(t, third.ClearedCount, 1)
		connect.AssertEqual(t, third.Cursor == nil, true)
		connect.AssertEqual(t, first.ReapedCount+second.ReapedCount+third.ReapedCount, 4)
		for _, client := range clients {
			_, found := testLeaseSeenTime(t, self.ctx, client.clientId)
			connect.AssertEqual(t, found, false)
		}
	})
}

// A reap deactivates through §7.10's chain with the reason deactivated, and a removal keeps removed.
func TestEphemeralReapRecordsDeactivated(t *testing.T) {
	connect.AssertEqual(t, removalReasonOf(context.Background()), sidecar.RevocationReasonRemoved)
	connect.AssertEqual(t, removalReasonOf(withRemovalReason(context.Background(), sidecar.RevocationReasonDeactivated)), sidecar.RevocationReasonDeactivated)

	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		self := newClientOwnerFixture(t)
		ephemeral := newTestEphemeralClient(t, self, server.NowUtc())
		reasons := []sidecar.RevocationReason{}
		RegisterRemovalEffect("test_reason", func(ctx context.Context, tx server.PgTx, removal *Removal) error {
			reasons = append(reasons, removal.Reason)
			return nil
		})
		t.Cleanup(func() { Testing_UnregisterRemovalEffect("test_reason") })

		reaped, err := ReapEphemeralClient(self.ctx, ephemeral.clientId)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, reaped, true)
		removed := self.mint(t, self.root, &AuthNetworkClientArgs{Description: "device", DeviceSpec: "test"})
		result, err := RemoveNetworkClient(&RemoveNetworkClientArgs{ClientId: *removed.ClientId}, self.root)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, result.Error == nil, true)
		connect.AssertEqual(t, reasons, []sidecar.RevocationReason{sidecar.RevocationReasonDeactivated, sidecar.RevocationReasonRemoved})
	})
}
