// The disconnected-client task through its own entry point and windows: its
// idle top-level marking holds while sidecar.yml is there but the process has
// never read it (QD-1079).
package work

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
)

// A taskworker that starts during a broken sidecar.yml push leaves urnetwork-
// edge's idle slot active, so the next deploy into it is minted its client, and
// marks no other idle client either; once the push is fixed, the next run's read
// is the first good one and marks the ordinary idle client, while the edge
// client, whose network that read names, stays active.
func TestRemoveDisconnectedNetworkClientsHoldsMarkingUntilSidecarYmlReads(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		clientSession := session.NewLocalClientSession(ctx, "0.0.0.0:0", nil)
		defer clientSession.Cancel()
		now := server.NowUtc()

		setAuthTime := func(clientId server.Id, authTime time.Time) {
			server.Tx(ctx, func(tx server.PgTx) {
				server.RaisePgResult(tx.Exec(ctx, `UPDATE network_client SET auth_time = $2 WHERE client_id = $1`, clientId, authTime))
			})
		}
		requireState := func(name string, clientId server.Id, wantActive bool) {
			t.Helper()
			exists := false
			active := false
			server.Db(ctx, func(conn server.PgConn) {
				result, err := conn.Query(ctx, `SELECT active FROM network_client WHERE client_id = $1`, clientId)
				server.WithPgResult(result, err, func() {
					if result.Next() {
						exists = true
						server.Raise(result.Scan(&active))
					}
				})
			})
			if !exists || active != wantActive {
				t.Fatalf("%s: exists %t, active %t, want active %t", name, exists, active, wantActive)
			}
		}
		run := func() {
			if _, err := RemoveDisconnectedNetworkClients(&RemoveDisconnectedNetworkClientsArgs{}, clientSession); err != nil {
				t.Fatal(err)
			}
		}

		idleAuthTime := now.Add(-model.TopLevelClientIdleExpiration - 24*time.Hour)
		edgeNetworkId := server.NewId()
		model.Testing_CreateNetwork(ctx, edgeNetworkId, "urnetwork-edge-task-test", server.NewId())
		edgeKey := &model.EdgeClientKey{Environment: "test", Block: "smtpedge", Host: "lb-a.example", Slot: 3, Index: 0}
		mint, err := model.MintEdgeClient(ctx, edgeNetworkId, edgeKey, nil)
		if err != nil {
			t.Fatal(err)
		}
		edgeClientId := mint.ClientId
		setAuthTime(edgeClientId, idleAuthTime)
		networkId := server.NewId()
		model.Testing_CreateNetwork(ctx, networkId, "ordinary-task-test", server.NewId())
		idleClientId := server.NewId()
		model.Testing_CreateDevice(ctx, networkId, server.NewId(), idleClientId, "synthetic", "synthetic")
		setAuthTime(idleClientId, idleAuthTime)

		// the broken push names the edge before the key that fails
		t.Cleanup(server.Testing_PushSidecarConfig(fmt.Sprintf(
			"sidecar:\n  urnetwork_edge: %q\n  evs: {enabled: true, percent: fifty}\n",
			edgeNetworkId,
		)))
		run()
		requireState("held: the idle edge client", edgeClientId, true)
		requireState("held: the idle ordinary client", idleClientId, true)
		mint, err = model.MintEdgeClient(ctx, edgeNetworkId, edgeKey, nil)
		if err != nil {
			t.Fatalf("held: the next deploy into the edge slot is refused: %v", err)
		}
		if mint.ClientId != edgeClientId {
			t.Fatalf("held: the edge slot minted %s, want %s", mint.ClientId, edgeClientId)
		}

		t.Cleanup(server.Testing_RereadSidecarConfig(fmt.Sprintf("sidecar:\n  urnetwork_edge: %q\n", edgeNetworkId)))
		run()
		requireState("released: the idle edge client", edgeClientId, true)
		requireState("released: the idle ordinary client", idleClientId, false)
	})
}
