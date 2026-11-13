package model

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
)

// The deferred fault happens at COMMIT, after DELETE RETURNING has been read
// by the actual reaper. The existing context hook then connects the still-active
// child between attempts. Cleanup may own only identifiers from committed deletes.
// No production hook, timing sleep, retention change or mocked transaction is
// needed to distinguish a rolled-back candidate from a committed deletion.
func TestNetworkClientReapPublishesOnlyCommittedCleanup(t *testing.T) {
	for _, control := range []struct {
		name      string
		reconnect bool
		fault     bool
	}{
		{name: "healthy_committed_child"},
		{name: "healthy_reconnected_child", reconnect: true},
		{name: "rolled_back_reconnected_child", reconnect: true, fault: true},
	} {
		t.Run(control.name, func(t *testing.T) {
			env := server.DefaultTestEnv()
			env.RerunCount = 0
			env.Run(t, func(t testing.TB) {
				ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
				defer cancel()
				now := server.NowUtc().Truncate(time.Microsecond)
				networkId, parentId, clientId := server.NewId(), server.NewId(), server.NewId()
				statsInsertNetworkClient(ctx, networkId, parentId)
				statsInsertNetworkClient(ctx, networkId, clientId)
				statsInsertProvideKey(ctx, clientId, int(ProvideModePublic))
				server.Tx(ctx, func(tx server.PgTx) {
					tag := server.RaisePgResult(tx.Exec(ctx, `UPDATE network_client
						SET source_client_id=$2,create_time=$3,auth_time=$3
						WHERE client_id=$1 AND active`, clientId, parentId, now.Add(-NetworkClientReapAfterDeactivate-24*time.Hour)))
					if tag.RowsAffected() != 1 {
						panic("reap ownership fixture did not seed its active stale child")
					}
				}, server.TxReadCommitted, server.OptNoRetry())
				redisKeys := []string{
					provideModesKey(clientId),
					provideModeSecretKeyKey(clientId, ProvideModePublic),
					clientPublicKeyRedisKey(clientId),
				}
				server.Redis(ctx, func(r server.RedisClient) {
					server.Raise(r.Set(ctx, redisKeys[0], fmt.Sprintf("[%d]", ProvideModePublic), 0).Err())
					server.Raise(r.Set(ctx, redisKeys[1], []byte{0x01}, 0).Err())
					server.Raise(r.Set(ctx, redisKeys[2], "synthetic-reap-owner", 0).Err())
				})

				conn := acquireContractLifecycleTestConnection(t, ctx)
				defer conn.Release()
				var initialParents, initialKeys int
				server.Raise(conn.QueryRow(ctx, `SELECT
					(SELECT count(*) FROM network_client WHERE client_id=$1 AND active AND source_client_id IS NOT NULL),
					(SELECT count(*) FROM provide_key WHERE client_id=$1 AND provide_mode=$2)`, clientId, ProvideModePublic).
					Scan(&initialParents, &initialKeys))
				server.Redis(ctx, func(r server.RedisClient) {
					count, err := r.Exists(ctx, redisKeys...).Result()
					server.Raise(err)
					if initialParents != 1 || initialKeys != 1 || count != int64(len(redisKeys)) {
						t.Fatal("reap ownership fixture did not begin with a parent and all owned keys")
					}
				})
				var reruns, deferredCalls int
				workerCtx := ctx
				connectChild := func() {
					// The real connection path refreshes this active child's
					// auth_time and records its live connection. It never
					// reactivates an inactive identity or creates a new one.
					connectionId, _, _, _, err := ConnectNetworkClient(ctx, clientId, "192.0.2.1:20000", server.NewId())
					server.Raise(err)
					if connectionId == (server.Id{}) {
						panic("reap ownership control did not create a child connection")
					}
				}
				if control.reconnect && !control.fault {
					connectChild()
				}
				if control.fault {
					// The sequence is not rolled back, so the native COMMIT
					// failure is independently witnessed and happens once.
					server.RaisePgResult(conn.Exec(ctx, fmt.Sprintf(`
						CREATE SEQUENCE client_reap_ownership_retry_seq;
						CREATE FUNCTION client_reap_ownership_retry_once() RETURNS trigger LANGUAGE plpgsql AS $$
						BEGIN
						 IF OLD.client_id::text=TG_ARGV[0] AND nextval('client_reap_ownership_retry_seq')=1 THEN
						  RAISE EXCEPTION 'synthetic reap ownership retry' USING ERRCODE='40001';
						 END IF;
						 RETURN OLD;
						END $$;
						CREATE CONSTRAINT TRIGGER client_reap_ownership_retry
						AFTER DELETE ON network_client DEFERRABLE INITIALLY DEFERRED
						FOR EACH ROW EXECUTE FUNCTION client_reap_ownership_retry_once('%s')`, clientId.String())))
					defer func() {
						cleanup, stop := context.WithTimeout(context.Background(), 5*time.Second)
						defer stop()
						_, err := conn.Exec(cleanup, `DROP TRIGGER client_reap_ownership_retry ON network_client;
							DROP FUNCTION client_reap_ownership_retry_once(); DROP SEQUENCE client_reap_ownership_retry_seq`)
						if err != nil {
							t.Errorf("reap ownership fixture cleanup failed: %v", err)
						}
					}()
					workerCtx = server.Testing_WithTxRerunHook(ctx, func() {
						reruns++
						if reruns != 1 {
							panic("reap ownership control retried more than once")
						}
						server.Raise(conn.QueryRow(ctx, `SELECT last_value FROM client_reap_ownership_retry_seq`).Scan(&deferredCalls))
						if deferredCalls != 1 {
							panic("reap ownership retry was not caused by the deferred commit fault")
						}
						var active bool
						server.Raise(conn.QueryRow(ctx, `SELECT active FROM network_client WHERE client_id=$1`, clientId).Scan(&active))
						if !active {
							panic("rolled-back child was not still active before connection")
						}
						// Original context prevents recursive hook inheritance.
						connectChild()
					})
				}

				RemoveDisconnectedNetworkClients(workerCtx,
					now.Add(-8*time.Hour),
					now.Add(-NetworkClientReapAfterDeactivate),
					now.Add(-TopLevelClientIdleExpiration))

				var childExists, active, authFresh, sourceParentSurvives bool
				var provideRows, connectedRows int
				server.Raise(conn.QueryRow(ctx, `SELECT
					EXISTS(SELECT 1 FROM network_client WHERE client_id=$1),
					COALESCE((SELECT active FROM network_client WHERE client_id=$1),false),
					COALESCE((SELECT auth_time >= $3 FROM network_client WHERE client_id=$1),false),
					EXISTS(SELECT 1 FROM network_client WHERE client_id=$4 AND active),
					(SELECT count(*) FROM provide_key WHERE client_id=$1 AND provide_mode=$2),
					(SELECT count(*) FROM network_client_connection WHERE client_id=$1 AND connected)`, clientId, ProvideModePublic, now, parentId).
					Scan(&childExists, &active, &authFresh, &sourceParentSurvives, &provideRows, &connectedRows))
				var redisCount int64
				server.Redis(ctx, func(r server.RedisClient) {
					var err error
					redisCount, err = r.Exists(ctx, redisKeys...).Result()
					server.Raise(err)
				})
				if !sourceParentSurvives {
					t.Fatal("reap ownership control unexpectedly removed the fresh source parent")
				}
				if control.reconnect {
					// A failure here requires the real deferred fault and
					// positive surviving connected-child witnesses first.
					wantFaults := 0
					if control.fault {
						wantFaults = 1
					}
					if reruns != wantFaults || deferredCalls != wantFaults || !childExists || !active || !authFresh || connectedRows != 1 {
						t.Fatalf("reap retry control missed its causal boundary: reruns=%d deferred=%d child=%t active=%t fresh=%t connected=%d",
							reruns, deferredCalls, childExists, active, authFresh, connectedRows)
					}
					if provideRows != 1 || redisCount != int64(len(redisKeys)) {
						t.Fatalf("rolled-back reap escaped committed cleanup ownership: provide_rows=%d redis_keys=%d",
							provideRows, redisCount)
					}
				} else if reruns != 0 || childExists || active || authFresh || connectedRows != 0 || provideRows != 0 || redisCount != 0 {
					t.Fatalf("healthy committed reap did not clean its owned state: child=%t active=%t connected=%d provide_rows=%d redis_keys=%d",
						childExists, active, connectedRows, provideRows, redisCount)
				}
			})
		})
	}
}
