package taskworker

import (
	"testing"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/controller"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
)

// A deleted account leaves no active client and none of its own rows once the
// work its deletion queues has run. Before 2026-11-16 the deletion
// (controller.RemoveNetworkAndContacts, behind /auth/network-delete and
// `bringyourctl network remove`) removed the network, its users and their
// sign-ins and queued nothing else: its clients stayed active and every other
// row of the account stayed (support item 8364 left 406 clients). The test
// reads only entry points that predate the change, so it runs unchanged
// against the code before it, where it fails.
func TestAccountDeletionLeavesNoActiveClientOrAccountRows(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		networkId := server.NewId()
		userId := server.NewId()
		model.Testing_CreateGuestNetwork(ctx, networkId, "synthetic-deleted-"+networkId.String()[:8], userId)
		deviceId := server.NewId()
		clientId := server.NewId()
		model.Testing_CreateDevice(ctx, networkId, deviceId, clientId, "synthetic", "synthetic")
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO account_preferences (network_id, product_updates) VALUES ($1, true)`, networkId))
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO account_api_key (api_key_id, network_id, api_key, name, create_time) VALUES ($1, $2, $3, 'synthetic', now())`, server.NewId(), networkId, "synthetic-"+server.NewId().String()))
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO network_referral_code (network_id, referral_code) VALUES ($1, $2)`, networkId, "SYN"+server.NewId().String()[:8]))
		})
		// a live network beside it keeps everything
		liveNetworkId := server.NewId()
		model.Testing_CreateGuestNetwork(ctx, liveNetworkId, "synthetic-live-"+liveNetworkId.String()[:8], server.NewId())
		model.Testing_CreateDevice(ctx, liveNetworkId, server.NewId(), server.NewId(), "synthetic", "synthetic")
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO account_preferences (network_id, product_updates) VALUES ($1, true)`, liveNetworkId))
		})

		clientSession := session.NewLocalClientSession(ctx, "", nil)
		defer clientSession.Cancel()
		outcome, _ := controller.RemoveNetworkAndContacts(clientSession, networkId, &userId, &model.RemoveNetworkStoreSnapshot{})
		if outcome != model.RemoveNetworkRemoved {
			t.Fatalf("the deletion = %s", outcome)
		}

		// the work the deletion queued, as the worker finds it once its block
		// has passed, until none is due
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE pending_task SET run_at = run_at - interval '10 minutes'`))
		})
		worker := InitTaskWorker(ctx)
		defer worker.Close()
		for range 10 {
			finished, retried, postRetried, err := worker.EvalTasks(10)
			if err != nil || 0 < len(retried) || 0 < len(postRetried) {
				t.Fatalf("queued work failed: retried %v, post retried %v: %v", retried, postRetried, err)
			}
			if len(finished) == 0 {
				break
			}
		}

		count := func(sql string, args ...any) (n int64) {
			server.Db(ctx, func(conn server.PgConn) {
				server.Raise(conn.QueryRow(ctx, sql, args...).Scan(&n))
			})
			return
		}
		if n := count(`SELECT count(*) FROM network_client WHERE network_id = $1 AND active`, networkId); n != 0 {
			t.Fatalf("the deleted account still has %d active clients", n)
		}
		for _, table := range []string{"account_preferences", "account_api_key", "network_referral_code"} {
			if n := count(`SELECT count(*) FROM `+table+` WHERE network_id = $1`, networkId); n != 0 {
				t.Fatalf("the deleted account still has %d %s rows", n, table)
			}
		}
		// its inactive client waits for the client delay, with its cleanup queued
		if n := count(`SELECT count(*) FROM pending_task WHERE args_json::jsonb->'deleted_network'->>'network_id' = $1 AND (now() AT TIME ZONE 'UTC') < run_at`, networkId.String()); n != 1 {
			t.Fatalf("%d cleanup runs queued for the deleted account's clients, want 1", n)
		}
		if n := count(`SELECT count(*) FROM network_client WHERE network_id = $1 AND active`, liveNetworkId); n != 1 {
			t.Fatal("the live network's client changed")
		}
		if n := count(`SELECT count(*) FROM account_preferences WHERE network_id = $1`, liveNetworkId); n != 1 {
			t.Fatal("the live network's preferences changed")
		}
	})
}
