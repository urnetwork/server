package model

import (
	"testing"

	"github.com/urnetwork/server"
)

// Root cause (2026-11-05): every proxy instance restores the proxy_client rows
// of its (host, block) as WireGuard clients at startup and reads them all again
// on every reconcile, and the read did not look at the proxy's client. A
// removed or expired proxy's client is deactivated for good (nothing sets
// network_client.active back to true), but its row lives until the hosted proxy
// reap, so on Main about 760k of the 823k configured WireGuard clients were
// such dead proxies. Before the fix this failed with "a proxy whose client is
// deactivated is restored as a WireGuard client".
func TestProxyWithAnInactiveClientIsNotRestoredAsAWireGuardClient(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		active := newHostedProxyReapTestProxy(t)
		inactive := newHostedProxyReapTestProxy(t)
		retireRetiredHostedTestClient(t, inactive.networkId, inactive.clientId)
		if n := hostedProxyReapTestRowCount(t, `SELECT count(*) FROM proxy_client WHERE proxy_id = $1`, inactive.proxyId); n != 1 {
			t.Fatalf("the removed proxy has %d proxy_client rows, want its row kept until the reap", n)
		}
		var inactiveChangeId int64
		server.Db(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(ctx, `SELECT max(change_id) FROM proxy_client_change WHERE proxy_id = $1`, inactive.proxyId).Scan(&inactiveChangeId))
		})

		// the startup restore and every reconcile
		proxyClients, _, err := GetProxyClientsSince(ctx, active.proxyHost, active.block, 0)
		if err != nil {
			t.Fatal(err)
		}
		if _, ok := proxyClients[active.proxyId]; !ok {
			t.Fatal("an active proxy is not restored as a WireGuard client")
		}
		proxyClients, maxChangeId, err := GetProxyClientsSince(ctx, inactive.proxyHost, inactive.block, 0)
		if err != nil {
			t.Fatal(err)
		}
		if _, ok := proxyClients[inactive.proxyId]; ok {
			t.Fatal("a proxy whose client is deactivated is restored as a WireGuard client")
		}
		// the read still covers the inactive proxy's change, so a watch moves
		// past it instead of reading it again on every poll
		if maxChangeId < inactiveChangeId {
			t.Fatalf("the read covered changes up to %d, short of the inactive proxy's change %d", maxChangeId, inactiveChangeId)
		}

		// a poll that starts at the inactive proxy's change delivers nothing and
		// reports the change
		proxyClients, maxChangeId, err = GetProxyClientsSince(ctx, inactive.proxyHost, inactive.block, inactiveChangeId)
		if err != nil {
			t.Fatal(err)
		}
		if _, ok := proxyClients[inactive.proxyId]; ok {
			t.Fatal("a poll delivered a proxy whose client is deactivated")
		}
		if maxChangeId < inactiveChangeId {
			t.Fatalf("a poll from change %d reported %d", inactiveChangeId, maxChangeId)
		}
	})
}
