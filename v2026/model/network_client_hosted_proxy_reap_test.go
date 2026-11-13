package model

// The hosted proxy reap, on fixed clocks: every call passes the reap's now,
// measured from the deactivation time the database stamped.

import (
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/session"
)

// A hosted proxy as auth-client writes one: a top-level client with a
// proxy_device_config and a proxy_client row on some (host, block).
type hostedProxyReapTestProxy struct {
	networkId server.Id
	clientId  server.Id
	deviceId  server.Id
	proxyId   server.Id
	proxyHost string
	block     string
}

func newHostedProxyReapTestProxy(t testing.TB) *hostedProxyReapTestProxy {
	t.Helper()
	ctx := t.Context()
	proxy := &hostedProxyReapTestProxy{
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
	proxyClient, err := CreateProxyClient(
		ctx,
		proxyDeviceConfig.ProxyId,
		proxy.clientId,
		proxyDeviceConfig.InstanceId,
		CreateProxyClientOptions{},
	)
	if err != nil || proxyClient == nil {
		t.Fatal("proxy client", err)
	}
	proxy.proxyId = proxyDeviceConfig.ProxyId
	proxy.proxyHost = proxyClient.ProxyHost
	proxy.block = proxyClient.Block
	return proxy
}

// The reaps the taskworker runs, at now: the 30-day inactive reap, the retired
// window reap and the hosted proxy reap, each from its start.
func runHostedReapsAt(t testing.TB, now time.Time) {
	t.Helper()
	ctx := t.Context()
	RemoveDisconnectedNetworkClients(
		ctx,
		now.Add(-8*time.Hour),
		now.Add(-NetworkClientReapAfterDeactivate),
		now.Add(-TopLevelClientIdleExpiration),
	)
	RemoveRetiredHostedNetworkClients(ctx, now, RetiredHostedNetworkClientCursor{}, DefaultRetiredHostedNetworkClientReapSettings())
	RemoveRetiredHostedProxyClients(ctx, now, HostedProxyReapCursor{}, DefaultHostedProxyReapSettings())
}

// Whether the proxy is in its (host, block) set, which every proxy instance
// restores as WireGuard clients at startup and re-reads on each reconcile.
func hostedProxyReapTestInWireGuardSet(t testing.TB, proxy *hostedProxyReapTestProxy) bool {
	t.Helper()
	proxyClients, _, err := GetProxyClientsSince(t.Context(), proxy.proxyHost, proxy.block, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, ok := proxyClients[proxy.proxyId]
	return ok
}

func hostedProxyReapTestRowCount(t testing.TB, sql string, id server.Id) int {
	t.Helper()
	ctx := t.Context()
	var count int
	server.Db(ctx, func(conn server.PgConn) {
		server.Raise(conn.QueryRow(ctx, sql, id).Scan(&count))
	})
	return count
}

// Root cause (2026-11-05): a removed or expired proxy's proxy_client row
// lived until the 30-day inactive reap, so every proxy instance kept restoring
// it as a WireGuard client; on Main 93% of the configured WireGuard clients
// were such dead proxies. Before the fix this failed with "a proxy removed 49h
// ago is still in the (host, block) set every proxy instance restores as
// WireGuard clients: client exists=true".
func TestHostedProxyRemovedTwoDaysAgoLeavesTheWireGuardSet(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		proxy := newHostedProxyReapTestProxy(t)
		if !hostedProxyReapTestInWireGuardSet(t, proxy) {
			t.Fatal("a new proxy is not in its (host, block) set")
		}

		deactivateTime := retireRetiredHostedTestClient(t, proxy.networkId, proxy.clientId)
		runHostedReapsAt(t, deactivateTime.Add(49*time.Hour))

		if hostedProxyReapTestInWireGuardSet(t, proxy) {
			t.Fatalf("a proxy removed 49h ago is still in the (host, block) set every proxy instance restores as WireGuard clients: client exists=%t", retiredHostedTestClientExists(t, proxy.clientId))
		}
		if retiredHostedTestClientExists(t, proxy.clientId) {
			t.Fatal("the removed proxy's client row is still there")
		}
		if n := hostedProxyReapTestRowCount(t, `SELECT count(*) FROM proxy_device_config WHERE proxy_id = $1`, proxy.proxyId); n != 0 {
			t.Fatalf("the removed proxy kept %d proxy_device_config rows", n)
		}
		if n := hostedProxyReapTestRowCount(t, `SELECT count(*) FROM proxy_client_change WHERE proxy_id = $1`, proxy.proxyId); n != 0 {
			t.Fatalf("the removed proxy kept %d proxy_client_change rows", n)
		}
	})
}

// The boundary, on the reap alone: a proxy removed 47 hours ago keeps its rows,
// though it left the WireGuard set at its removal (GetProxyClientsSince reads
// active clients only); at 49 hours it is deleted.
func TestHostedProxyReapKeepsARemovedProxyFor47Hours(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		proxy := newHostedProxyReapTestProxy(t)
		deactivateTime := retireRetiredHostedTestClient(t, proxy.networkId, proxy.clientId)
		settings := DefaultHostedProxyReapSettings()

		removedCount, cursor, passComplete, more := RemoveRetiredHostedProxyClients(ctx, deactivateTime.Add(47*time.Hour), HostedProxyReapCursor{}, settings)
		if removedCount != 0 || more || !passComplete || cursor != (HostedProxyReapCursor{}) {
			t.Fatalf("at 47h: removed=%d more=%t pass_complete=%t cursor=%+v", removedCount, more, passComplete, cursor)
		}
		if !retiredHostedTestClientExists(t, proxy.clientId) {
			t.Fatal("a proxy removed 47h ago was reaped")
		}
		if n := hostedProxyReapTestRowCount(t, `SELECT count(*) FROM proxy_client WHERE proxy_id = $1`, proxy.proxyId); n != 1 {
			t.Fatalf("a proxy removed 47h ago has %d proxy_client rows, want its row kept", n)
		}
		if hostedProxyReapTestInWireGuardSet(t, proxy) {
			t.Fatal("a removed proxy is still in the WireGuard set")
		}

		removedCount, cursor, passComplete, more = RemoveRetiredHostedProxyClients(ctx, deactivateTime.Add(49*time.Hour), HostedProxyReapCursor{}, settings)
		if removedCount != 1 || more || !passComplete || cursor != (HostedProxyReapCursor{}) {
			t.Fatalf("at 49h: removed=%d more=%t pass_complete=%t cursor=%+v", removedCount, more, passComplete, cursor)
		}
		if retiredHostedTestClientExists(t, proxy.clientId) || hostedProxyReapTestInWireGuardSet(t, proxy) {
			t.Fatal("a proxy removed 49h ago was kept")
		}
	})
}

// An active proxy, however old, is never reaped; an inactive client without a
// proxy config keeps the 30-day reap; and a config whose client row is gone is
// skipped (the orphan sweep owns it).
func TestHostedProxyReapKeepsActiveProxiesAndOtherClients(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		activeProxy := newHostedProxyReapTestProxy(t)

		networkId := server.NewId()
		clientId := server.NewId()
		Testing_CreateDevice(ctx, networkId, server.NewId(), clientId, "test", "test")
		deactivateTime := retireRetiredHostedTestClient(t, networkId, clientId)

		orphanConfig := &ProxyDeviceConfig{}
		orphanConfig.ClientId = server.NewId()
		if err := CreateProxyDeviceConfig(ctx, orphanConfig); err != nil {
			t.Fatal(err)
		}

		removedCount, _, passComplete, more := RemoveRetiredHostedProxyClients(ctx, deactivateTime.Add(60*24*time.Hour), HostedProxyReapCursor{}, DefaultHostedProxyReapSettings())
		if removedCount != 0 || more || !passComplete {
			t.Fatalf("removed=%d more=%t pass_complete=%t", removedCount, more, passComplete)
		}
		if !retiredHostedTestClientExists(t, activeProxy.clientId) || !hostedProxyReapTestInWireGuardSet(t, activeProxy) {
			t.Fatal("an active proxy was reaped")
		}
		if !retiredHostedTestClientExists(t, clientId) {
			t.Fatal("an inactive client without a proxy config was reaped by the hosted proxy reap")
		}
		if n := hostedProxyReapTestRowCount(t, `SELECT count(*) FROM proxy_device_config WHERE proxy_id = $1`, orphanConfig.ProxyId); n != 1 {
			t.Fatalf("the orphan config has %d rows, want it left for the orphan sweep", n)
		}
	})
}

// The proxy's windows go first: removing the proxy retires its window too, the
// window reap deletes the window at 25 hours while the proxy's config still
// marks it hosted, and the proxy reap deletes the proxy and then the device
// they shared at 49 hours.
func TestHostedProxyReapRemovesTheProxyAfterItsWindows(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		proxy := newHostedProxyReapTestProxy(t)
		windowId := insertRetiredHostedTestChild(t, proxy.networkId, proxy.clientId, proxy.deviceId)
		deactivateTime := retireRetiredHostedTestClient(t, proxy.networkId, proxy.clientId)
		if !retiredHostedTestClientExists(t, windowId) {
			t.Fatal("the window is gone at the removal")
		}

		removedCount, _, _ := RemoveRetiredHostedNetworkClients(ctx, deactivateTime.Add(25*time.Hour), RetiredHostedNetworkClientCursor{}, DefaultRetiredHostedNetworkClientReapSettings())
		if removedCount != 1 || retiredHostedTestClientExists(t, windowId) {
			t.Fatalf("the window reap at 25h removed %d, want the proxy's window", removedCount)
		}
		if !retiredHostedTestClientExists(t, proxy.clientId) {
			t.Fatal("the window reap removed the proxy")
		}

		proxyRemovedCount, _, _, _ := RemoveRetiredHostedProxyClients(ctx, deactivateTime.Add(49*time.Hour), HostedProxyReapCursor{}, DefaultHostedProxyReapSettings())
		if proxyRemovedCount != 1 || retiredHostedTestClientExists(t, proxy.clientId) {
			t.Fatalf("the proxy reap at 49h removed %d, want the proxy", proxyRemovedCount)
		}
		if n := hostedProxyReapTestRowCount(t, `SELECT count(*) FROM device WHERE device_id = $1`, proxy.deviceId); n != 0 {
			t.Fatalf("the proxy's device is still there (%d rows)", n)
		}
	})
}

// MaxRemoveCount ends a run with more to do, the next run resumes at the
// cursor, and the run that reaches the end of proxy_device_config completes
// the pass with the zero cursor.
func TestHostedProxyReapPacesARunAndEndsAPass(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		var deactivateTime time.Time
		proxies := []*hostedProxyReapTestProxy{}
		for range 3 {
			proxy := newHostedProxyReapTestProxy(t)
			deactivateTime = retireRetiredHostedTestClient(t, proxy.networkId, proxy.clientId)
			proxies = append(proxies, proxy)
		}
		settings := DefaultHostedProxyReapSettings()
		settings.PageCount = 2
		settings.MaxRemoveCount = 2
		now := deactivateTime.Add(49 * time.Hour)

		removedCount, cursor, passComplete, more := RemoveRetiredHostedProxyClients(ctx, now, HostedProxyReapCursor{}, settings)
		if removedCount != 2 || !more || passComplete || cursor == (HostedProxyReapCursor{}) {
			t.Fatalf("first run: removed=%d more=%t pass_complete=%t cursor=%+v", removedCount, more, passComplete, cursor)
		}

		removedCount, cursor, passComplete, more = RemoveRetiredHostedProxyClients(ctx, now, cursor, settings)
		if removedCount != 1 || more || !passComplete || cursor != (HostedProxyReapCursor{}) {
			t.Fatalf("second run: removed=%d more=%t pass_complete=%t cursor=%+v", removedCount, more, passComplete, cursor)
		}
		for _, proxy := range proxies {
			if retiredHostedTestClientExists(t, proxy.clientId) {
				t.Fatalf("proxy %s was kept", proxy.proxyId)
			}
		}
	})
}

// The proxy outlives its windows' reap, whose hosted test reads the proxy's
// config, and goes well before the 30-day reap.
func TestHostedProxyReapSettingsOrder(t *testing.T) {
	proxyReap := DefaultHostedProxyReapSettings().ReapAfterDeactivate
	windowReap := DefaultRetiredHostedNetworkClientReapSettings().ReapAfterDeactivate
	if proxyReap <= windowReap {
		t.Fatalf("the proxy reap (%s) must come after the window reap (%s)", proxyReap, windowReap)
	}
	if NetworkClientReapAfterDeactivate <= proxyReap {
		t.Fatalf("the proxy reap (%s) must come before the 30-day reap (%s)", proxyReap, NetworkClientReapAfterDeactivate)
	}
}

// The chain continues soon while a pass is unfinished, and waits a pass
// interval after a pass ends.
func TestHostedProxyReapNextRunAt(t *testing.T) {
	settings := DefaultHostedProxyReapSettings()
	now := time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)
	if got := HostedProxyReapNextRunAt(now, false, settings); !got.Equal(now.Add(settings.ContinueInterval)) {
		t.Fatalf("unfinished pass: next run at %s", got)
	}
	if got := HostedProxyReapNextRunAt(now, true, settings); !got.Equal(now.Add(settings.PassInterval)) {
		t.Fatalf("finished pass: next run at %s", got)
	}
}

// The revocation record guard exempts removeHostedProxyClientsSql because it
// ends no live credential (revocation_record_writer_guard_test.go). Handed the
// ids directly, the delete removes only the client that was deactivated before
// the horizon, and that client's deactivation had already tombstoned it; an
// active proxy and one deactivated at the horizon stay.
func TestHostedProxyReapDeleteEndsNoLiveCredential(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		activeProxy := newHostedProxyReapTestProxy(t)
		expiredProxy := newHostedProxyReapTestProxy(t)
		recentProxy := newHostedProxyReapTestProxy(t)
		expiredDeactivateTime := retireRetiredHostedTestClient(t, expiredProxy.networkId, expiredProxy.clientId)
		recentDeactivateTime := retireRetiredHostedTestClient(t, recentProxy.networkId, recentProxy.clientId)
		if !expiredDeactivateTime.Before(recentDeactivateTime) {
			t.Fatalf("deactivations out of order: %s, %s", expiredDeactivateTime, recentDeactivateTime)
		}
		// the deactivation tombstoned the client before the delete runs
		assertRevocationField(
			t,
			ctx,
			session.Testing_RevocationRecordKey(expiredProxy.networkId, expiredProxy.clientId),
			session.Testing_RevocationClientField(expiredProxy.clientId),
			"x",
		)

		// the horizon is the recent proxy's deactivation: the expired proxy is
		// before it, the recent proxy is not, and the active proxy was created
		// before it but is active
		deletedClientIds := []server.Id{}
		server.Tx(ctx, func(tx server.PgTx) {
			result, err := tx.Query(
				ctx,
				removeHostedProxyClientsSql,
				[]server.Id{activeProxy.clientId, expiredProxy.clientId, recentProxy.clientId},
				recentDeactivateTime,
			)
			server.WithPgResult(result, err, func() {
				for result.Next() {
					var clientId server.Id
					var deviceId *server.Id
					server.Raise(result.Scan(&clientId, &deviceId))
					deletedClientIds = append(deletedClientIds, clientId)
				}
			})
		})
		if len(deletedClientIds) != 1 || deletedClientIds[0] != expiredProxy.clientId {
			t.Fatalf("the delete removed %v, want only the proxy deactivated before the horizon %s", deletedClientIds, expiredProxy.clientId)
		}
		if !retiredHostedTestClientExists(t, activeProxy.clientId) || !testingClientActive(ctx, activeProxy.clientId) {
			t.Fatal("the delete removed or ended an active proxy")
		}
		if !retiredHostedTestClientExists(t, recentProxy.clientId) {
			t.Fatal("the delete removed a proxy deactivated at the horizon")
		}
	})
}
