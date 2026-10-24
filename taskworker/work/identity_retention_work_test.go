// The retired hosted identity reap and the key history retention run as
// run-once chains whose Post hands the cursor to the next run.
package work

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
)

// Pure: a run that spends its whole budget still finishes inside its task's
// max time, so Post runs and hands on the cursor instead of the claim
// expiring and the run repeating.
func TestIdentityRetentionBudgetsFitUnderMaxTime(t *testing.T) {
	if budget := model.DefaultRetiredHostedNetworkClientReapSettings().RunBudget; removeRetiredHostedNetworkClientsMaxTime < 4*budget {
		t.Fatalf("retired hosted reap budget %v leaves no headroom under max time %v", budget, removeRetiredHostedNetworkClientsMaxTime)
	}
	if budget := model.DefaultStClientKeyHistoryRetentionSettings().RunBudget; removeGoneStClientKeyHistoryMaxTime < 4*budget {
		t.Fatalf("key history retention budget %v leaves no headroom under max time %v", budget, removeGoneStClientKeyHistoryMaxTime)
	}
}

// The args and run_at of the one pending row of a run-once key.
func readIdentityRetentionPending(ctx context.Context, t testing.TB, tx server.PgTx, runOnceKey string, args any) (runAt time.Time) {
	t.Helper()
	found := false
	result, err := tx.Query(ctx, `SELECT args_json, run_at FROM pending_task WHERE run_once_key = $1`, runOnceKey)
	server.WithPgResult(result, err, func() {
		if result.Next() {
			found = true
			var argsJson string
			server.Raise(result.Scan(&argsJson, &runAt))
			server.Raise(json.Unmarshal([]byte(argsJson), args))
		}
	})
	if !found {
		t.Fatalf("no %s row scheduled", runOnceKey)
	}
	return
}

// DB-backed: under the owner rule this runs only after the branch, with its
// migrations, is merged to main.
//
// A chain without a cursor starts at the horizon, so a window retired before
// the chain's first run stays for the 30-day reap; an explicit cursor before
// it deletes the window.
func TestRemoveRetiredHostedNetworkClientsStartsAtTheHorizon(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		clientSession := session.NewLocalClientSession(ctx, "0.0.0.0:0", nil)
		defer clientSession.Cancel()

		networkId, proxyClientId, proxyDeviceId := server.NewId(), server.NewId(), server.NewId()
		model.Testing_CreateDevice(ctx, networkId, proxyDeviceId, proxyClientId, "test", "test")
		proxyDeviceConfig := &model.ProxyDeviceConfig{}
		proxyDeviceConfig.ClientId = proxyClientId
		if err := model.CreateProxyDeviceConfig(ctx, proxyDeviceConfig); err != nil {
			t.Fatal(err)
		}
		windowId := server.NewId()
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(
				ctx,
				`
				INSERT INTO network_client (client_id, network_id, device_id, description, create_time, auth_time, source_client_id, active, deactivate_time)
				VALUES ($1, $2, $3, 'test', now(), now(), $4, false, (now() AT TIME ZONE 'UTC') - interval '2 days')
				`,
				windowId,
				networkId,
				proxyDeviceId,
				proxyClientId,
			))
		})
		windowExists := func() bool {
			return model.GetNetworkClientNetwork(ctx, windowId) != nil
		}

		before := server.NowUtc()
		result, err := RemoveRetiredHostedNetworkClients(&RemoveRetiredHostedNetworkClientsArgs{}, clientSession)
		if err != nil {
			t.Fatal(err)
		}
		if result.RemovedCount != 0 || !windowExists() {
			t.Fatal("a chain without a cursor deleted a window retired before it started")
		}
		horizon := before.Add(-model.DefaultRetiredHostedNetworkClientReapSettings().ReapAfterDeactivate)
		if result.Cursor.DeactivateTime.Before(horizon.Add(-time.Minute)) || horizon.Add(time.Minute).Before(result.Cursor.DeactivateTime) {
			t.Fatalf("a chain without a cursor started at %v, want the horizon %v", result.Cursor.DeactivateTime, horizon)
		}

		result, err = RemoveRetiredHostedNetworkClients(&RemoveRetiredHostedNetworkClientsArgs{Cursor: &model.RetiredHostedNetworkClientCursor{}}, clientSession)
		if err != nil {
			t.Fatal(err)
		}
		if result.RemovedCount != 1 || windowExists() {
			t.Fatal("a cursor before the window kept it")
		}
	})
}

// DB-backed: Post hands the run's cursor on, a minute later while work
// remains and on the idle cadence otherwise.
func TestRemoveRetiredHostedNetworkClientsPostKeepsTheCursor(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		clientSession := session.NewLocalClientSession(ctx, "0.0.0.0:0", nil)
		defer clientSession.Cancel()

		cursor := model.RetiredHostedNetworkClientCursor{
			DeactivateTime: server.NowUtc().Add(-30 * time.Hour).Truncate(time.Microsecond),
			ClientId:       server.NewId(),
		}
		for _, more := range []bool{true, false} {
			before := server.NowUtc()
			server.Tx(ctx, func(tx server.PgTx) {
				err := RemoveRetiredHostedNetworkClientsPost(
					&RemoveRetiredHostedNetworkClientsArgs{},
					&RemoveRetiredHostedNetworkClientsResult{Cursor: cursor, More: more},
					clientSession,
					tx,
				)
				if err != nil {
					t.Fatal(err)
				}
				var args RemoveRetiredHostedNetworkClientsArgs
				runAt := readIdentityRetentionPending(ctx, t, tx, `["remove_retired_hosted_network_clients"]`, &args)
				if args.Cursor == nil || args.Cursor.ClientId != cursor.ClientId || !args.Cursor.DeactivateTime.Equal(cursor.DeactivateTime) {
					t.Fatalf("Post scheduled cursor %+v, want %+v", args.Cursor, cursor)
				}
				wantDelay := 5 * time.Minute
				if more {
					wantDelay = time.Minute
				}
				if delay := runAt.Sub(before); delay < wantDelay-30*time.Second || wantDelay+30*time.Second < delay {
					t.Fatalf("Post with more=%t scheduled %v out, want %v", more, delay, wantDelay)
				}
				server.RaisePgResult(tx.Exec(ctx, `DELETE FROM pending_task WHERE run_once_key = '["remove_retired_hosted_network_clients"]'`))
			})
		}
	})
}

// DB-backed: Post hands the retention's cursor on, a minute later while work
// remains and on the idle cadence otherwise.
func TestRemoveGoneStClientKeyHistoryPostKeepsTheCursor(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		clientSession := session.NewLocalClientSession(ctx, "0.0.0.0:0", nil)
		defer clientSession.Cancel()

		afterClientId := server.NewId()
		cursor := model.StClientKeyHistoryRetentionCursor{DiscoverAfterClientId: &afterClientId}
		for _, more := range []bool{true, false} {
			before := server.NowUtc()
			server.Tx(ctx, func(tx server.PgTx) {
				err := RemoveGoneStClientKeyHistoryPost(
					&RemoveGoneStClientKeyHistoryArgs{},
					&RemoveGoneStClientKeyHistoryResult{Cursor: cursor, More: more},
					clientSession,
					tx,
				)
				if err != nil {
					t.Fatal(err)
				}
				var args RemoveGoneStClientKeyHistoryArgs
				runAt := readIdentityRetentionPending(ctx, t, tx, `["remove_gone_st_client_key_history"]`, &args)
				if args.Cursor.DiscoverAfterClientId == nil || *args.Cursor.DiscoverAfterClientId != afterClientId {
					t.Fatalf("Post scheduled cursor %+v, want %+v", args.Cursor, cursor)
				}
				wantDelay := 15 * time.Minute
				if more {
					wantDelay = time.Minute
				}
				if delay := runAt.Sub(before); delay < wantDelay-30*time.Second || wantDelay+30*time.Second < delay {
					t.Fatalf("Post with more=%t scheduled %v out, want %v", more, delay, wantDelay)
				}
				server.RaisePgResult(tx.Exec(ctx, `DELETE FROM pending_task WHERE run_once_key = '["remove_gone_st_client_key_history"]'`))
			})
		}
	})
}
