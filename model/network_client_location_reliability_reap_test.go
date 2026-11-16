// A reaped client's location reliability row goes with the client, through
// each reap that hard deletes clients and their shared cascade
// (removeReapedNetworkClientDependents), on fixed clocks measured from the
// deactivation times the database stamped. These read only the reaps' public
// entry points, so they run unchanged against the code before the cascade
// deleted the row, where they fail.
package model

import (
	"testing"
	"time"

	"github.com/urnetwork/server"
)

// A location reliability row for the client, refreshed at updateBlockNumber,
// with the columns the retention reads.
func insertLocationReliabilityRetentionTestRow(t testing.TB, clientId server.Id, networkId server.Id, updateBlockNumber int64) {
	t.Helper()
	ctx := t.Context()
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(
			ctx,
			`INSERT INTO network_client_location_reliability (client_id, network_id, update_block_number) VALUES ($1, $2, $3)`,
			clientId,
			networkId,
			updateBlockNumber,
		))
	})
}

func locationReliabilityRetentionTestRowExists(t testing.TB, clientId server.Id) (exists bool) {
	t.Helper()
	ctx := t.Context()
	server.Db(ctx, func(conn server.PgConn) {
		server.Raise(conn.QueryRow(
			ctx,
			`SELECT EXISTS (SELECT 1 FROM network_client_location_reliability WHERE client_id = $1)`,
			clientId,
		).Scan(&exists))
	})
	return
}

// Requires each client's row to exist or not, by name.
func requireLocationReliabilityRetentionTestRows(t testing.TB, step string, nameClientIds map[string]server.Id, nameKept map[string]bool) {
	t.Helper()
	for name, clientId := range nameClientIds {
		if exists := locationReliabilityRetentionTestRowExists(t, clientId); exists != nameKept[name] {
			t.Fatalf("%s: the %s's location reliability row exists=%t, want %t", step, name, exists, nameKept[name])
		}
	}
}

// Each reap that hard deletes a client takes its location reliability row
// with it, the moment the client goes: the retired window reap a day after a
// hosted window's retirement, the hosted proxy reap two days after the
// proxy's, and the inactive reap 30 days after a top-level client's removal.
// Before, every row outlived its client by up to 31 days. A live client's row
// stays throughout, and the row of a client a reap passes over stays with it.
func TestReapedClientLocationReliabilityGoesWithItsClient(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		block := reliabilityBlockNumber(server.NowUtc())

		proxy := newHostedProxyReapTestProxy(t)
		windowId := insertRetiredHostedTestChild(t, proxy.networkId, proxy.clientId, proxy.deviceId)
		removedNetworkId, removedClientId := server.NewId(), server.NewId()
		Testing_CreateDevice(ctx, removedNetworkId, server.NewId(), removedClientId, "test", "test")
		liveNetworkId, liveClientId := server.NewId(), server.NewId()
		Testing_CreateDevice(ctx, liveNetworkId, server.NewId(), liveClientId, "test", "test")

		nameClientIds := map[string]server.Id{
			"hosted window":  windowId,
			"hosted proxy":   proxy.clientId,
			"removed client": removedClientId,
			"live client":    liveClientId,
		}
		insertLocationReliabilityRetentionTestRow(t, windowId, proxy.networkId, block)
		insertLocationReliabilityRetentionTestRow(t, proxy.clientId, proxy.networkId, block)
		insertLocationReliabilityRetentionTestRow(t, removedClientId, removedNetworkId, block)
		insertLocationReliabilityRetentionTestRow(t, liveClientId, liveNetworkId, block)
		kept := map[string]bool{"hosted window": true, "hosted proxy": true, "removed client": true, "live client": true}

		// the window, retired by its proxy, goes a day later
		windowRetired := retireRetiredHostedTestClient(t, proxy.networkId, windowId)
		removedCount, _, _ := RemoveRetiredHostedNetworkClients(ctx, windowRetired.Add(25*time.Hour), RetiredHostedNetworkClientCursor{}, DefaultRetiredHostedNetworkClientReapSettings())
		if removedCount != 1 || retiredHostedTestClientExists(t, windowId) {
			t.Fatalf("the retired window reap removed %d clients, want the window", removedCount)
		}
		kept["hosted window"] = false
		requireLocationReliabilityRetentionTestRows(t, "after the retired window reap", nameClientIds, kept)

		// the proxy, removed by its owner, goes two days later
		proxyRetired := retireRetiredHostedTestClient(t, proxy.networkId, proxy.clientId)
		removedCount, _, _, _ = RemoveRetiredHostedProxyClients(ctx, proxyRetired.Add(49*time.Hour), HostedProxyReapCursor{}, DefaultHostedProxyReapSettings())
		if removedCount != 1 || retiredHostedTestClientExists(t, proxy.clientId) {
			t.Fatalf("the hosted proxy reap removed %d clients, want the proxy", removedCount)
		}
		kept["hosted proxy"] = false
		requireLocationReliabilityRetentionTestRows(t, "after the hosted proxy reap", nameClientIds, kept)

		// the top-level client, removed by its owner, goes 30 days later; the
		// live client, seen since, is not marked abandoned
		clientRemoved := retireRetiredHostedTestClient(t, removedNetworkId, removedClientId)
		reapNow := clientRemoved.Add(NetworkClientReapAfterDeactivate + time.Minute)
		RemoveDisconnectedNetworkClients(
			ctx,
			reapNow.Add(-8*time.Hour),
			reapNow.Add(-NetworkClientReapAfterDeactivate),
			clientRemoved.Add(-TopLevelClientIdleExpiration),
		)
		if retiredHostedTestClientExists(t, removedClientId) || !retiredHostedTestClientExists(t, liveClientId) {
			t.Fatal("the inactive reap did not remove exactly the removed client")
		}
		kept["removed client"] = false
		requireLocationReliabilityRetentionTestRows(t, "after the inactive reap", nameClientIds, kept)
	})
}
