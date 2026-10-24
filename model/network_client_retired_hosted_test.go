package model

// The retired hosted identity reap, on fixed clocks: every call passes the
// reap's now, measured from the deactivation time the database stamped.
//
// DB-backed: under the owner rule these run only after the branch, with its
// migrations, is merged to main.

import (
	"testing"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/session"
)

// A hosted proxy device and its network: the proxy's client has a
// proxy_device_config, as NewProxyDevice requires.
type retiredHostedTestProxy struct {
	networkId server.Id
	clientId  server.Id
	deviceId  server.Id
}

func newRetiredHostedTestProxy(t testing.TB) *retiredHostedTestProxy {
	t.Helper()
	ctx := t.Context()
	proxy := &retiredHostedTestProxy{
		networkId: server.NewId(),
		clientId:  server.NewId(),
		deviceId:  server.NewId(),
	}
	Testing_CreateDevice(ctx, proxy.networkId, proxy.deviceId, proxy.clientId, "test", "test")
	proxyDeviceConfig := &ProxyDeviceConfig{}
	proxyDeviceConfig.ClientId = proxy.clientId
	if err := CreateProxyDeviceConfig(ctx, proxyDeviceConfig); err != nil {
		t.Fatal(err)
	}
	return proxy
}

// A child of parentId on deviceId, as a window mint writes it.
func insertRetiredHostedTestChild(t testing.TB, networkId server.Id, parentId server.Id, deviceId server.Id) server.Id {
	t.Helper()
	ctx := t.Context()
	childId := server.NewId()
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(
			ctx,
			`
			INSERT INTO network_client (client_id, network_id, device_id, description, create_time, auth_time, source_client_id)
			VALUES ($1, $2, $3, 'test', now(), now(), $4)
			`,
			childId,
			networkId,
			deviceId,
			parentId,
		))
	})
	return childId
}

// Retires the client as its owner does and returns the deactivation time the
// database stamped.
func retireRetiredHostedTestClient(t testing.TB, networkId server.Id, clientId server.Id) time.Time {
	t.Helper()
	ctx := t.Context()
	removed, err := RemoveNetworkClient(&RemoveNetworkClientArgs{ClientId: clientId}, &session.ClientSession{
		Ctx:   ctx,
		ByJwt: &session.ByJwt{NetworkId: networkId},
	})
	if err != nil || removed.Error != nil {
		t.Fatal("removal failed", err, removed.Error)
	}
	var deactivateTime time.Time
	server.Db(ctx, func(conn server.PgConn) {
		server.Raise(conn.QueryRow(ctx, `SELECT deactivate_time FROM network_client WHERE client_id = $1`, clientId).Scan(&deactivateTime))
	})
	return deactivateTime
}

func retiredHostedTestClientExists(t testing.TB, clientId server.Id) bool {
	t.Helper()
	return GetNetworkClientNetwork(t.Context(), clientId) != nil
}

// One page per call, so a test can watch the cursor move.
func retiredHostedTestSettings() *RetiredHostedNetworkClientReapSettings {
	settings := DefaultRetiredHostedNetworkClientReapSettings()
	settings.RunBudget = 0
	return settings
}

// A hosted window retired 23 hours ago stays; at 25 hours it is deleted with
// its dependents. Its provide keys already went at the deactivation.
func TestRetiredHostedClientKeptAt23HoursDeletedAt25Hours(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		proxy := newRetiredHostedTestProxy(t)
		windowId := insertRetiredHostedTestChild(t, proxy.networkId, proxy.clientId, proxy.deviceId)
		SetProvide(ctx, windowId, map[ProvideMode][]byte{
			ProvideModeStream: provideRetirementTestKey(0xa1),
		})
		SetClientTlsCertificateWithSignature(ctx, windowId, []byte("synthetic-pem"), []byte("synthetic-signature"))
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO network_client_role (client_id, role) VALUES ($1, 'synthetic-role')`, windowId))
		})

		deactivateTime := retireRetiredHostedTestClient(t, proxy.networkId, windowId)
		if secretKeys := loadProvideSecretKeys(ctx, windowId); len(secretKeys) != 0 {
			t.Fatalf("the retired window kept provide keys %v", secretKeys)
		}

		settings := DefaultRetiredHostedNetworkClientReapSettings()
		removedCount, cursor, more := RemoveRetiredHostedNetworkClients(ctx, deactivateTime.Add(23*time.Hour), RetiredHostedNetworkClientCursor{}, settings)
		if removedCount != 0 || more || !retiredHostedTestClientExists(t, windowId) {
			t.Fatalf("a window retired 23h ago was reaped: removed=%d more=%t", removedCount, more)
		}
		if cursor != (RetiredHostedNetworkClientCursor{}) {
			t.Fatalf("the cursor passed a window that was not yet due: %+v", cursor)
		}

		removedCount, cursor, more = RemoveRetiredHostedNetworkClients(ctx, deactivateTime.Add(25*time.Hour), cursor, settings)
		if removedCount != 1 || more || retiredHostedTestClientExists(t, windowId) {
			t.Fatalf("a window retired 25h ago was kept: removed=%d more=%t", removedCount, more)
		}
		if cursor.ClientId != windowId || !cursor.DeactivateTime.Equal(deactivateTime) {
			t.Fatalf("the cursor %+v did not end at the reaped window", cursor)
		}

		tlsCertificatePem, _, err := GetClientTlsCertificateAndSignature(ctx, windowId)
		if err != nil || len(tlsCertificatePem) != 0 {
			t.Fatal("the reaped window kept its tls certificate", err)
		}
		server.Db(ctx, func(conn server.PgConn) {
			var roleCount, deviceCount int
			server.Raise(conn.QueryRow(ctx, `SELECT count(*) FROM network_client_role WHERE client_id = $1`, windowId).Scan(&roleCount))
			server.Raise(conn.QueryRow(ctx, `SELECT count(*) FROM device WHERE device_id = $1`, proxy.deviceId).Scan(&deviceCount))
			if roleCount != 0 {
				t.Fatalf("the reaped window kept %d roles", roleCount)
			}
			if deviceCount != 1 {
				t.Fatal("the reap deleted the device the proxy still uses")
			}
		})
		if !retiredHostedTestClientExists(t, proxy.clientId) {
			t.Fatal("the reap deleted the proxy")
		}
	})
}

// A retired top-level client and a retired child of a client that is not a
// hosted proxy keep the 30-day reap.
func TestRetiredHostedReapKeepsOtherClientsToTheirOwnReap(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		networkId := server.NewId()
		topLevelId, topLevelDeviceId := server.NewId(), server.NewId()
		Testing_CreateDevice(ctx, networkId, topLevelDeviceId, topLevelId, "test", "test")
		childId := insertRetiredHostedTestChild(t, networkId, topLevelId, topLevelDeviceId)
		retiredId := server.NewId()
		Testing_CreateDevice(ctx, networkId, server.NewId(), retiredId, "test", "test")

		childTime := retireRetiredHostedTestClient(t, networkId, childId)
		retiredTime := retireRetiredHostedTestClient(t, networkId, retiredId)
		now := retiredTime.Add(25 * time.Hour)

		removedCount, cursor, _ := RemoveRetiredHostedNetworkClients(ctx, now, RetiredHostedNetworkClientCursor{}, DefaultRetiredHostedNetworkClientReapSettings())
		if removedCount != 0 || !retiredHostedTestClientExists(t, childId) || !retiredHostedTestClientExists(t, retiredId) {
			t.Fatal("the hosted reap deleted a client that is not a hosted window")
		}
		if cursor.ClientId != childId || !cursor.DeactivateTime.Equal(childTime) {
			t.Fatalf("the cursor %+v did not pass the child it examined", cursor)
		}

		// their own reap keeps them inside its 30 days, and deletes them after
		RemoveDisconnectedNetworkClients(ctx, now.Add(-8*time.Hour), now.Add(-NetworkClientReapAfterDeactivate), time.Time{})
		if !retiredHostedTestClientExists(t, childId) || !retiredHostedTestClientExists(t, retiredId) {
			t.Fatal("the 30-day reap deleted a client retired a day ago")
		}
		later := retiredTime.Add(NetworkClientReapAfterDeactivate + time.Hour)
		RemoveDisconnectedNetworkClients(ctx, later.Add(-8*time.Hour), later.Add(-NetworkClientReapAfterDeactivate), time.Time{})
		if retiredHostedTestClientExists(t, childId) || retiredHostedTestClientExists(t, retiredId) {
			t.Fatal("the 30-day reap kept a client retired more than 30 days ago")
		}
		if !retiredHostedTestClientExists(t, topLevelId) {
			t.Fatal("a reap deleted an active client")
		}
	})
}

// A retired window that still holds a connected row is passed, and left to
// the 30-day reap; once disconnected, a fresh pass deletes it.
func TestRetiredHostedReapPassesConnectedWindow(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		proxy := newRetiredHostedTestProxy(t)
		windowId := insertRetiredHostedTestChild(t, proxy.networkId, proxy.clientId, proxy.deviceId)
		connectionId, _, _, _, err := ConnectNetworkClient(ctx, windowId, "192.0.2.10:20000", CreateNetworkClientHandler(ctx))
		if err != nil {
			t.Fatal(err)
		}
		deactivateTime := retireRetiredHostedTestClient(t, proxy.networkId, windowId)
		now := deactivateTime.Add(25 * time.Hour)
		settings := DefaultRetiredHostedNetworkClientReapSettings()

		removedCount, cursor, _ := RemoveRetiredHostedNetworkClients(ctx, now, RetiredHostedNetworkClientCursor{}, settings)
		if removedCount != 0 || !retiredHostedTestClientExists(t, windowId) {
			t.Fatal("the reap deleted a window that is still connected")
		}
		if cursor.ClientId != windowId {
			t.Fatalf("the cursor %+v did not pass the connected window", cursor)
		}

		if err := DisconnectNetworkClient(ctx, connectionId); err != nil {
			t.Fatal(err)
		}
		if removedCount, _, _ := RemoveRetiredHostedNetworkClients(ctx, now, cursor, settings); removedCount != 0 {
			t.Fatal("the reap examined a window behind its cursor again")
		}
		if removedCount, _, _ := RemoveRetiredHostedNetworkClients(ctx, now, RetiredHostedNetworkClientCursor{}, settings); removedCount != 1 || retiredHostedTestClientExists(t, windowId) {
			t.Fatal("a fresh pass kept the disconnected window")
		}
	})
}

// With no time budget a call examines one page, and the next call resumes at
// its cursor, in deactivation order.
func TestRetiredHostedReapResumesAtItsCursor(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		proxy := newRetiredHostedTestProxy(t)
		windowIds := []server.Id{}
		for range 3 {
			windowId := insertRetiredHostedTestChild(t, proxy.networkId, proxy.clientId, proxy.deviceId)
			retireRetiredHostedTestClient(t, proxy.networkId, windowId)
			windowIds = append(windowIds, windowId)
		}
		// distinct deactivation times, newest last
		start := server.NowUtc().Add(-48 * time.Hour).Truncate(time.Second)
		server.Tx(ctx, func(tx server.PgTx) {
			for index, windowId := range windowIds {
				server.RaisePgResult(tx.Exec(
					ctx,
					`UPDATE network_client SET deactivate_time = $2 WHERE client_id = $1`,
					windowId,
					start.Add(time.Duration(index)*time.Minute),
				))
			}
		})
		now := start.Add(25 * time.Hour)
		settings := retiredHostedTestSettings()
		settings.BatchCount = 1

		cursor := RetiredHostedNetworkClientCursor{}
		for index, windowId := range windowIds {
			removedCount, next, more := RemoveRetiredHostedNetworkClients(ctx, now, cursor, settings)
			if removedCount != 1 || !more || retiredHostedTestClientExists(t, windowId) {
				t.Fatalf("page %d: removed=%d more=%t", index, removedCount, more)
			}
			if next.ClientId != windowId {
				t.Fatalf("page %d ended at %s, want %s", index, next.ClientId, windowId)
			}
			for _, laterId := range windowIds[index+1:] {
				if !retiredHostedTestClientExists(t, laterId) {
					t.Fatalf("page %d deleted a window past its page", index)
				}
			}
			cursor = next
		}
		removedCount, next, more := RemoveRetiredHostedNetworkClients(ctx, now, cursor, settings)
		if removedCount != 0 || more || next != cursor {
			t.Fatalf("a caught-up reap moved: removed=%d more=%t cursor=%+v", removedCount, more, next)
		}
	})
}
