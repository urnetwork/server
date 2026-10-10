package model

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/urnetwork/server"
)

// A completed later admission has already published the current exact amount
// at its durable revision. A delayed earlier mirror should not have to revisit
// historical escrow merely because its original revision was overtaken.
func TestNetEscrowDelayedCreateUsesCurrentDurableCacheAndAvoidsHistory(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
		defer cancel()
		f := seedAdmissionCacheHistory(t, ctx, 10001, 8)
		_, delayedPosts := createNetEscrowOrderingTestContract(ctx, f, 1)
		_, _ = createNetEscrowOrderingTestContract(ctx, f, 1)
		var exactCache bool
		server.Db(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(ctx, `
				SELECT cached.revision=revision.revision AND cached.reserved_byte_count=10003
				FROM transfer_balance_net_escrow_snapshot cached
				JOIN transfer_balance_net_escrow_revision revision USING(balance_id)
				WHERE balance_id=$1`, f.balanceId).Scan(&exactCache))
		})
		if !exactCache {
			t.Fatal("later admission did not commit a current exact cache snapshot")
		}

		blocker := acquireContractLifecycleTestConnection(t, ctx)
		defer blocker.Release()
		held, err := blocker.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer held.Rollback(context.Background())
		server.RaisePgResult(held.Exec(ctx, `LOCK TABLE transfer_escrow IN ACCESS EXCLUSIVE MODE`))
		done := make(chan struct{})
		go func() {
			defer close(done)
			server.RunPosts(ctx, delayedPosts...)
		}()

		observedHistoryWait := false
		finished := false
		limit := time.NewTimer(3 * time.Second)
		defer limit.Stop()
		tick := time.NewTicker(20 * time.Millisecond)
		defer tick.Stop()
	observe:
		for {
			select {
			case <-done:
				finished = true
				break observe
			case <-limit.C:
				break observe
			case <-tick.C:
				server.Db(ctx, func(conn server.PgConn) {
					server.Raise(conn.QueryRow(ctx, `
						SELECT EXISTS (
							SELECT 1 FROM pg_locks AS held
							JOIN pg_stat_activity AS activity ON activity.pid=held.pid
							WHERE held.relation='transfer_escrow'::regclass AND NOT held.granted
								AND activity.datname=current_database()
								AND activity.state='active' AND activity.wait_event='relation'
								AND position('SELECT requested_balance.balance_id,' IN activity.query)>0
						)`).Scan(&observedHistoryWait))
				})
				if observedHistoryWait {
					break observe
				}
			}
		}
		server.Raise(held.Rollback(ctx))
		select {
		case <-done:
		case <-ctx.Done():
			t.Fatal("delayed mirror did not join after releasing the history lock")
		}
		if got := Testing_NetEscrowByteCount(ctx, f.balanceId); got != 10003 {
			t.Fatalf("delayed mirror reserved %d, want current committed 10003", got)
		}
		t.Logf("exact_cache_current=%t history_relation_wait=%t mirror_finished_while_history_held=%t", exactCache, observedHistoryWait, finished)
		if observedHistoryWait || !finished {
			t.Fatal("delayed creation mirror revisited historical escrow despite a current durable cache snapshot")
		}
	})
}

// Overtaking admissions leave many delayed posts with old predicted revisions.
// Compare real concurrent mirror callbacks with and without the durable cache,
// including their Redis writes, rather than timing an isolated cache lookup.
func TestNetEscrowPostCacheConcurrentMirrorCost(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
		defer cancel()
		for _, missing := range []bool{true, false} {
			f := seedAdmissionCacheHistory(t, ctx, 10001, 32)
			allPosts := []func() any{}
			for range 20 {
				_, posts := createNetEscrowOrderingTestContract(ctx, f, 1)
				allPosts = append(allPosts, posts...)
			}
			if missing {
				server.Tx(ctx, func(tx server.PgTx) {
					server.RaisePgResult(tx.Exec(ctx, `DELETE FROM transfer_balance_net_escrow_snapshot WHERE balance_id=$1`, f.balanceId))
				})
			}
			beforeReload := testutil.ToFloat64(netEscrowCreationSnapshots.WithLabelValues("reloaded"))
			beforeReuse := testutil.ToFloat64(netEscrowCreationSnapshots.WithLabelValues("reused"))
			var wg sync.WaitGroup
			start := make(chan struct{})
			began := time.Now()
			for _, post := range allPosts {
				wg.Add(1)
				go func() {
					defer wg.Done()
					<-start
					server.RunPosts(ctx, post)
				}()
			}
			close(start)
			wg.Wait()
			reloads := testutil.ToFloat64(netEscrowCreationSnapshots.WithLabelValues("reloaded")) - beforeReload
			reused := testutil.ToFloat64(netEscrowCreationSnapshots.WithLabelValues("reused")) - beforeReuse
			// A cold concurrent reader may start before the first exact cache
			// publication. Only the final creation has a current prediction;
			// the sequential regression proves reuse after warming.
			validReloads := reloads == 0
			if missing {
				validReloads = 1 <= reloads && reloads <= 19
			}
			if !validReloads || reused+reloads != 20 {
				t.Fatalf("missing=%t completed reloads=%v reused=%v", missing, reloads, reused)
			}
			if got := Testing_NetEscrowByteCount(ctx, f.balanceId); got != 10021 {
				t.Fatalf("concurrent delayed mirrors published%d, want10021", got)
			}
			t.Logf("missing_cache=%t completed_mirrors=20 history_reloads=%v elapsed=%s", missing, reloads, time.Since(began))
		}
	})
}

// Older writers, terminal transitions and deleted/reused identities all keep
// exact fallback. A subsequent admission can publish another current cache
// independently of whether the original creation still exists or remains open.
func TestNetEscrowPostCacheInvalidationAndTerminalSource(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		for _, scenario := range []struct {
			name string
			want ByteCount
		}{
			{"missing", 5}, {"legacy_amount", 11}, {"settled", 4}, {"terminal", 4},
			{"contract_deleted", 4}, {"balance_deleted", 0}, {"balance_reused", 5}, {"current_after_terminal", 5},
		} {
			f := seedAdmissionCacheHistory(t, ctx, 3, 20)
			first, posts := createNetEscrowOrderingTestContract(ctx, f, 1)
			_, _ = createNetEscrowOrderingTestContract(ctx, f, 1)
			server.Tx(ctx, func(tx server.PgTx) {
				switch scenario.name {
				case "missing":
					server.RaisePgResult(tx.Exec(ctx, `DELETE FROM transfer_balance_net_escrow_snapshot WHERE balance_id=$1`, f.balanceId))
				case "legacy_amount":
					server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_escrow SET balance_byte_count=7 WHERE balance_id=$1 AND contract_id=md5($1::uuid::text||'-cache-contract-1')::uuid`, f.balanceId))
				case "settled":
					server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_escrow SET settled=true WHERE contract_id=$1`, first.ContractId))
				case "terminal", "current_after_terminal":
					server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_contract SET outcome='canceled',close_time=now() WHERE contract_id=$1`, first.ContractId))
				case "contract_deleted":
					server.RaisePgResult(tx.Exec(ctx, `DELETE FROM transfer_contract WHERE contract_id=$1`, first.ContractId))
				case "balance_deleted", "balance_reused":
					server.RaisePgResult(tx.Exec(ctx, `DELETE FROM transfer_balance WHERE balance_id=$1`, f.balanceId))
					if scenario.name == "balance_reused" {
						server.RaisePgResult(tx.Exec(ctx, `INSERT INTO transfer_balance(balance_id,network_id,end_time,start_balance_byte_count,balance_byte_count,net_revenue_nano_cents) VALUES($1,$2,now()+interval '1 hour',1000,1000,0)`, f.balanceId, f.sourceNetworkId))
					}
				}
			})
			if scenario.name == "current_after_terminal" {
				_, _ = createNetEscrowOrderingTestContract(ctx, f, 1)
			}
			before := testutil.ToFloat64(netEscrowCreationSnapshots.WithLabelValues("reloaded"))
			server.RunPosts(ctx, posts...)
			if got := Testing_NetEscrowByteCount(ctx, f.balanceId); got != scenario.want {
				t.Fatalf("%s delayed mirror=%d want%d", scenario.name, got, scenario.want)
			}
			wantReloads := float64(1)
			if scenario.name == "current_after_terminal" {
				wantReloads = 0
			}
			if delta := testutil.ToFloat64(netEscrowCreationSnapshots.WithLabelValues("reloaded")) - before; delta != wantReloads {
				t.Fatalf("%s exact fallbacks=%v want%v", scenario.name, delta, wantReloads)
			}
		}
	})
}

func TestNetEscrowPostCacheCustomGenericPointPlans(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		f := seedAdmissionCacheHistory(t, ctx, 3, 20)
		created, _ := createNetEscrowOrderingTestContract(ctx, f, 1)
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `ALTER TABLE transfer_balance_net_escrow_snapshot SET (autovacuum_enabled=false)`))
			server.RaisePgResult(tx.Exec(ctx, `ANALYZE transfer_balance_net_escrow_snapshot; ANALYZE transfer_balance; ANALYZE transfer_balance_net_escrow_revision; ANALYZE transfer_contract`))
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO transfer_balance_net_escrow_snapshot(balance_id,revision,reserved_byte_count)
				SELECT md5('unrelated-post-cache-'||n)::uuid,0,0 FROM generate_series(1,100000)n`))
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO transfer_balance(balance_id,network_id,start_time,end_time,start_balance_byte_count,balance_byte_count,net_revenue_nano_cents,subsidy_net_revenue_nano_cents,pro)
				SELECT md5('unrelated-post-cache-'||n)::uuid,$1,now()-interval '1 minute',now()+interval '1 hour',1,1,0,0,false FROM generate_series(1,100000)n`, f.sourceNetworkId))
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO transfer_balance_net_escrow_revision(balance_id,revision)
				SELECT md5('unrelated-post-cache-'||n)::uuid,1 FROM generate_series(1,100000)n`))
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO transfer_contract(contract_id,source_network_id,source_id,destination_network_id,destination_id,payer_network_id,transfer_byte_count)
				SELECT md5('unrelated-post-contract-'||n)::uuid,$1,$2,$3,$4,$1,1 FROM generate_series(1,100000)n`, f.sourceNetworkId, f.sourceId, f.destinationNetworkId, f.destinationId))
		})
		server.Db(ctx, func(conn server.PgConn) {
			server.RaisePgResult(conn.Exec(ctx, `PREPARE creation_cache_point AS `+netEscrowCreatedSnapshotSQL))
			defer conn.Exec(context.WithoutCancel(ctx), `DEALLOCATE creation_cache_point`)
			for _, mode := range []string{"force_custom_plan", "force_generic_plan"} {
				server.RaisePgResult(conn.Exec(ctx, `SET plan_cache_mode=`+mode))
				var raw []byte
				server.Raise(conn.QueryRow(ctx, fmt.Sprintf(`EXPLAIN (ANALYZE,BUFFERS,TIMING OFF,FORMAT JSON) EXECUTE creation_cache_point ('{%s}'::uuid[],'%s'::uuid)`, f.balanceId, created.ContractId)).Scan(&raw))
				var plan []map[string]any
				server.Raise(json.Unmarshal(raw, &plan))
				points := 0
				var visit func(map[string]any)
				visit = func(n map[string]any) {
					if relation, ok := n["Relation Name"].(string); ok {
						if relation == "transfer_escrow" {
							t.Fatal("creation cache hit planned escrow history access")
						}
						key := "balance_id ="
						if relation == "transfer_contract" {
							key = "contract_id ="
						}
						condition, _ := n["Index Cond"].(string)
						if !strings.Contains(condition, key) || n["Actual Loops"].(float64) != 1 || n["Actual Rows"].(float64) > 1 {
							t.Fatalf("%s post cache lookup escaped key for%s", mode, relation)
						}
						points++
					}
					if children, ok := n["Plans"].([]any); ok {
						for _, c := range children {
							visit(c.(map[string]any))
						}
					}
				}
				visit(plan[0]["Plan"].(map[string]any))
				if points != 4 {
					t.Fatalf("post cache points=%d want4", points)
				}
				t.Logf("%s post primary-key points=%d escrow_history_accesses=0 execution_ms=%v", mode, points, plan[0]["Execution Time"])
			}
		})
	})
}
