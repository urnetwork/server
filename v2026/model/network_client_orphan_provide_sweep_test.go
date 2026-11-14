// Actual migrated database and Redis controls cover page progress, waited
// updates, transaction retries and full-key access under competing indexes.
package model

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/urnetwork/server/v2026"
)

// Release fixture locks even after an assertion or expired test context.
func orphanProvideTestRollback(tx pgx.Tx) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = tx.Rollback(ctx)
}

// Retained pages and minimum/tied keys must advance without changing mirrors.
func TestSweepOrphanProvideKeyCursorAndRedis(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		type key struct {
			id   server.Id
			mode ProvideMode
		}
		keys := []key{{id: orphanSweepTestId(0), mode: -1}, {id: orphanSweepTestId(0), mode: 0}, {id: orphanSweepTestId(0), mode: 4},
			{id: orphanSweepTestId(1), mode: 1}, {id: orphanSweepTestId(1), mode: 4}, {id: orphanSweepTestId(2), mode: 1}, {id: orphanSweepTestId(3), mode: 3}}
		for _, n := range []int{0, 2} {
			statsInsertNetworkClient(ctx, server.NewId(), orphanSweepTestId(n))
		}
		server.Db(ctx, func(conn server.PgConn) {
			server.RaisePgResult(conn.Exec(ctx, `UPDATE network_client SET active=false WHERE client_id IN($1,$2)`, orphanSweepTestId(0), orphanSweepTestId(2)))
		})
		for _, k := range keys {
			statsInsertProvideKey(ctx, k.id, k.mode)
			server.Redis(ctx, func(r server.RedisClient) {
				server.Raise(r.Set(ctx, provideModesKey(k.id), "retained-modes", 0).Err())
				server.Raise(r.Set(ctx, provideModeSecretKeyKey(k.id, k.mode), "retained-secret", 0).Err())
			})
		}
		conn := acquireContractLifecycleTestConnection(t, ctx)
		defer conn.Release()
		cursor := key{}
		for i := 0; i <= len(keys); i++ {
			tx, err := conn.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
			server.Raise(err)
			defer orphanProvideTestRollback(tx)
			page := sweepOrphanProvideKeyPageInTx(ctx, tx, i == 0, cursor.id, cursor.mode, 1)
			server.Raise(tx.Rollback(ctx))
			if i == len(keys) {
				if page.gotBound || page.scanned != 0 || len(page.deleted) != 0 {
					t.Fatal("exact-full final page did not terminate on its empty successor")
				}
				break
			}
			if !page.gotBound || page.scanned != 1 || page.lastClientId != keys[i].id || page.lastProvideMode != keys[i].mode {
				t.Fatalf("cursor %d skipped the minimum key, a tied mode, or a retained page", i)
			}
			want := map[server.Id][]ProvideMode{}
			if keys[i].id == orphanSweepTestId(1) || keys[i].id == orphanSweepTestId(3) {
				want[keys[i].id] = []ProvideMode{keys[i].mode}
			}
			if len(page.deleted) != len(want) || (len(want) > 0 && !reflect.DeepEqual(page.deleted, want)) {
				t.Fatal("page returned keys other than its absent-parent deletion")
			}
			cursor = keys[i]
		}
		if removed := sweepOrphanProvideKeys(ctx, 2); removed != 3 {
			t.Fatalf("actual sweep deleted %d keys, expected 3", removed)
		}
		if sweepOrphanProvideKeys(ctx, 2) != 0 {
			t.Fatal("completed sweep repeated a deletion")
		}
		for _, k := range keys {
			retained := k.id == orphanSweepTestId(0) || k.id == orphanSweepTestId(2)
			var count int
			server.Raise(conn.QueryRow(ctx, `SELECT count(*) FROM provide_key WHERE client_id=$1 AND provide_mode=$2`, k.id, k.mode).Scan(&count))
			if (count == 1) != retained {
				t.Fatal("sweep changed retained or orphan membership")
			}
			server.Redis(ctx, func(r server.RedisClient) {
				for _, name := range []string{provideModesKey(k.id), provideModeSecretKeyKey(k.id, k.mode)} {
					n, err := r.Exists(ctx, name).Result()
					server.Raise(err)
					if (n == 1) != retained {
						t.Fatal("Redis cleanup differed from committed returned keys")
					}
				}
			})
		}
	})
}

// A real blocked update must yield one committed deletion and mirror cleanup.
func TestSweepOrphanProvideKeyFollowsConcurrentUpdate(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
		defer cancel()
		id := server.NewId()
		SetProvide(ctx, id, map[ProvideMode][]byte{ProvideModePublic: make([]byte, 32)})
		holder := acquireContractLifecycleTestConnection(t, ctx)
		defer holder.Release()
		held, err := holder.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
		server.Raise(err)
		var holderPid int
		server.Raise(held.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&holderPid))
		server.RaisePgResult(held.Exec(ctx, `UPDATE provide_key SET secret_key=$2 WHERE client_id=$1`, id, []byte{2}))
		type result struct {
			removed int64
			err     any
		}
		finished := make(chan result, 1)
		go func() {
			var out result
			out.err = server.HandleError(func() { out.removed = sweepOrphanProvideKeys(ctx, 1) })
			finished <- out
		}()
		joined := false
		defer func() {
			cancel()
			cleanup, stop := context.WithTimeout(context.Background(), 5*time.Second)
			defer stop()
			_ = held.Rollback(cleanup)
			if !joined {
				select {
				case <-finished:
				case <-cleanup.Done():
					t.Error("sweep worker did not join after cancellation")
				}
			}
		}()
		tick := time.NewTicker(5 * time.Millisecond)
		defer tick.Stop()
		for {
			var blocked bool
			server.Db(ctx, func(conn server.PgConn) {
				server.Raise(conn.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity
					WHERE datname=current_database() AND wait_event_type='Lock'
					AND query LIKE '%WITH slice%' AND query LIKE '%FROM provide_key%'
					AND $1::integer=ANY(pg_blocking_pids(pid)))`, holderPid).Scan(&blocked))
			})
			if blocked {
				break
			}
			select {
			case out := <-finished:
				joined = true
				t.Fatalf("sweep ended before its actual lock wait: %+v", out)
			case <-tick.C:
			case <-ctx.Done():
				t.Fatal("actual held-update wait was not observed")
			}
		}
		server.Raise(held.Commit(ctx))
		select {
		case out := <-finished:
			joined = true
			if out.err != nil || out.removed != 1 {
				t.Fatalf("waited update was not deleted exactly once: %+v", out)
			}
		case <-ctx.Done():
			t.Fatal("sweep did not finish after releasing its blocker")
		}
		server.Redis(ctx, func(r server.RedisClient) {
			n, err := r.Exists(ctx, provideModesKey(id), provideModeSecretKeyKey(id, ProvideModePublic)).Result()
			server.Raise(err)
			if n != 0 {
				t.Fatal("committed waited deletion did not clear its Redis mirrors")
			}
		})
	})
}

// Aborted results must not escape into the cursor, count or Redis cleanup.
func TestSweepOrphanProvideKeyRollbackAndRetry(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
		defer cancel()
		id := server.NewId()
		SetProvide(ctx, id, map[ProvideMode][]byte{ProvideModePublic: make([]byte, 32)})
		conn := acquireContractLifecycleTestConnection(t, ctx)
		defer conn.Release()
		tx, err := conn.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
		server.Raise(err)
		defer orphanProvideTestRollback(tx)
		page := sweepOrphanProvideKeyPageInTx(ctx, tx, true, server.Id{}, 0, 1)
		if !reflect.DeepEqual(page.deleted, map[server.Id][]ProvideMode{id: {ProvideModePublic}}) {
			t.Fatal("rollback control did not reach a real deletion")
		}
		_, err = tx.Exec(ctx, `SELECT 1/0`)
		var databaseError *pgconn.PgError
		if !errors.As(err, &databaseError) || databaseError.Code != "22012" {
			t.Fatal("post-delete fault did not occur")
		}
		server.Raise(tx.Rollback(ctx))
		var one int
		server.Raise(conn.QueryRow(ctx, `SELECT 1 FROM provide_key WHERE client_id=$1 FOR UPDATE NOWAIT`, id).Scan(&one))
		server.Redis(ctx, func(r server.RedisClient) {
			n, err := r.Exists(ctx, provideModesKey(id), provideModeSecretKeyKey(id, ProvideModePublic)).Result()
			server.Raise(err)
			if n != 2 {
				t.Fatal("rolled-back deletion changed Redis")
			}
		})
		// nextval is intentionally not rolled back: the first attempt fails
		// after selecting/locking, and the real MaintenanceTx retries once.
		server.RaisePgResult(conn.Exec(ctx, `CREATE SEQUENCE orphan_provide_retry_seq;
			CREATE FUNCTION orphan_provide_retry_once() RETURNS trigger LANGUAGE plpgsql AS $$
			BEGIN IF nextval('orphan_provide_retry_seq')=1 THEN
			 RAISE EXCEPTION 'synthetic retry' USING ERRCODE='40001'; END IF; RETURN OLD; END $$;
			CREATE TRIGGER orphan_provide_retry BEFORE DELETE ON provide_key
			FOR EACH ROW EXECUTE FUNCTION orphan_provide_retry_once()`))
		defer func() {
			cleanup, stop := context.WithTimeout(context.Background(), 5*time.Second)
			defer stop()
			_, _ = conn.Exec(cleanup, `DROP TRIGGER orphan_provide_retry ON provide_key;
				DROP FUNCTION orphan_provide_retry_once(); DROP SEQUENCE orphan_provide_retry_seq`)
		}()
		if removed := sweepOrphanProvideKeys(ctx, 1); removed != 1 {
			t.Fatalf("retry leaked a returned key or cursor: removed=%d", removed)
		}
		var calls, remaining int
		server.Raise(conn.QueryRow(ctx, `SELECT last_value FROM orphan_provide_retry_seq`).Scan(&calls))
		server.Raise(conn.QueryRow(ctx, `SELECT count(*) FROM provide_key WHERE client_id=$1`, id).Scan(&remaining))
		if calls != 2 || remaining != 0 || sweepOrphanProvideKeys(ctx, 1) != 0 {
			t.Fatal("actual retry or completed replay changed membership")
		}
		server.Redis(ctx, func(r server.RedisClient) {
			n, err := r.Exists(ctx, provideModesKey(id), provideModeSecretKeyKey(id, ProvideModePublic)).Result()
			server.Raise(err)
			if n != 0 {
				t.Fatal("successful retry did not clear exactly its committed Redis keys")
			}
		})
	})
}

// A parent committed between the two statements conservatively retains its key.
func TestSweepOrphanProvideKeyRechecksNewParent(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		id := server.NewId()
		statsInsertProvideKey(ctx, id, ProvideModePublic)
		conn := acquireContractLifecycleTestConnection(t, ctx)
		defer conn.Release()
		tx, err := conn.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
		server.Raise(err)
		defer orphanProvideTestRollback(tx)
		var scanned int64
		var locked []string
		var bound server.Id
		var mode ProvideMode
		server.Raise(tx.QueryRow(ctx, orphanProvideKeyFirstPageSql, 1).Scan(&scanned, &locked, &bound, &mode))
		if scanned != 1 || len(locked) != 1 || bound != id || mode != ProvideModePublic {
			t.Fatal("new-parent control did not lock its orphan")
		}
		statsInsertNetworkClient(ctx, server.NewId(), id)
		tag, err := tx.Exec(ctx, orphanProvideKeyDeleteSql, locked)
		server.Raise(err)
		if tag.RowsAffected() != 0 {
			t.Fatal("fresh parent membership was not rechecked")
		}
		server.Raise(tx.Commit(ctx))
		if sweepOrphanProvideKeys(ctx, 1) != 0 {
			t.Fatal("replay deleted a retained child")
		}
	})
}

// Generic/custom plans must bound both cursor and full-key membership work.
func TestSweepOrphanProvideKeyPlansStayInsidePage(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
		defer cancel()
		const parents, orphans, limit = 20000, 100, 128
		conn := acquireContractLifecycleTestConnection(t, ctx)
		defer conn.Release()
		server.RaisePgResult(conn.Exec(ctx, `INSERT INTO network_client(client_id,network_id,active)
			SELECT lpad(to_hex(n),32,'0')::uuid,$1,true FROM generate_series(0,$2::integer)n`, server.NewId(), parents))
		server.RaisePgResult(conn.Exec(ctx, `INSERT INTO provide_key(client_id,provide_mode,secret_key)
			SELECT lpad(to_hex(n),32,'0')::uuid,0,decode('01','hex') FROM generate_series(0,$1::integer)n`, parents+orphans))
		for _, leading := range []int{parents / 2, parents + 50} {
			server.RaisePgResult(conn.Exec(ctx, `INSERT INTO provide_key(client_id,provide_mode,secret_key)
				SELECT $1,n,decode('01','hex') FROM generate_series(1,20000)n`, orphanSweepTestId(leading)))
		}
		server.RaisePgResult(conn.Exec(ctx, `ANALYZE network_client; ANALYZE provide_key`))
		for _, mode := range []string{"force_custom_plan", "force_generic_plan"} {
			for _, page := range []struct {
				name  string
				first bool
				id    int
				mode  int
			}{
				{name: "first", first: true}, {name: "dense", id: parents - 200},
				{name: "tail", id: parents - 10}, {name: "terminal", id: parents + orphans},
				{name: "retained_suffix", id: parents / 2, mode: 19800},
				{name: "orphan_suffix", id: parents + 50, mode: 19800},
			} {
				func() {
					tx, err := conn.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
					server.Raise(err)
					defer func() {
						cleanup, stop := context.WithTimeout(context.Background(), 5*time.Second)
						defer stop()
						_ = tx.Rollback(cleanup)
						_, _ = conn.Exec(cleanup, `DEALLOCATE provide_page_plan`)
						_, _ = conn.Exec(cleanup, `DEALLOCATE provide_delete_plan`)
					}()
					server.RaisePgResult(tx.Exec(ctx, `SET LOCAL jit=off; SET LOCAL statement_timeout='20s'; SET LOCAL lock_timeout='3s'; SET LOCAL plan_cache_mode=`+mode))
					query, arguments := orphanProvideKeyFirstPageSql, fmt.Sprint(limit)
					if !page.first {
						query = orphanProvideKeyResumedPageSql
						arguments = fmt.Sprintf("'%s',%d,%d", orphanSweepTestId(page.id), page.mode, limit)
					}
					server.RaisePgResult(tx.Exec(ctx, `PREPARE provide_page_plan AS `+query))
					var raw []byte
					server.Raise(tx.QueryRow(ctx, `EXPLAIN(ANALYZE,BUFFERS,TIMING OFF,FORMAT JSON) EXECUTE provide_page_plan(`+arguments+`)`).Scan(&raw))
					var plans []struct{ Plan orphanSweepTestPlanNode }
					server.Raise(json.Unmarshal(raw, &plans))
					if len(plans) != 1 {
						t.Fatal("page plan missing")
					}
					var childWork, totalWork float64
					targetPoint, cursorRange, parentPoint, tidDelete := false, page.first, false, false
					var inspect func(orphanSweepTestPlanNode)
					inspect = func(node orphanSweepTestPlanNode) {
						if node.Relation != "" && node.NodeType != "ModifyTable" {
							if node.NodeType == "Seq Scan" || node.NodeType == "Parallel Seq Scan" {
								t.Fatalf("%s/%s used a whole-relation scan", mode, page.name)
							}
							work := (node.Rows + node.Removed + node.JoinRemoved + node.RecheckRemoved) * node.Loops
							totalWork += work
							condition := node.IndexCondition + " " + node.Recheck
							if node.Relation == "provide_key" {
								childWork += work
								both := strings.Contains(condition, "client_id") && strings.Contains(condition, "provide_mode")
								switch node.Alias {
								case "sweep_target":
									targetPoint = both && strings.Contains(condition, "=") && !strings.ContainsAny(condition, "<>")
								case "sweep_delete":
									tidDelete = node.NodeType == "Tid Scan"
								default:
									cursorRange = cursorRange || both && strings.Contains(condition, ">")
								}
							}
							if node.Relation == "network_client" {
								if node.Index != "network_client_pkey" || !strings.Contains(condition, "client_id =") || node.Rows+node.Removed > 1 || node.Loops > limit {
									t.Fatal("parent lookup lost its exact primary-key probe")
								}
								parentPoint = true
							}
						}
						for _, child := range node.Plans {
							inspect(child)
						}
					}
					inspect(plans[0].Plan)
					var scanned int64
					var locked []string
					var bound server.Id
					var boundMode ProvideMode
					err = tx.QueryRow(ctx, `EXECUTE provide_page_plan(`+arguments+`)`, pgx.QueryExecModeExec).Scan(&scanned, &locked, &bound, &boundMode)
					if err != nil && err != pgx.ErrNoRows {
						server.Raise(err)
					}
					if len(locked) > limit || int64(len(locked)) > scanned {
						t.Fatal("locked addresses exceeded the selected page")
					}
					if len(locked) == 0 {
						tidDelete = true
					} else {
						server.RaisePgResult(tx.Exec(ctx, `PREPARE provide_delete_plan(text[]) AS `+orphanProvideKeyDeleteSql))
						literal := "ARRAY['" + strings.Join(locked, "','") + "']::text[]"
						server.Raise(tx.QueryRow(ctx, `EXPLAIN(ANALYZE,BUFFERS,TIMING OFF,FORMAT JSON) EXECUTE provide_delete_plan(`+literal+`)`).Scan(&raw))
						plans = nil
						server.Raise(json.Unmarshal(raw, &plans))
						if len(plans) != 1 {
							t.Fatal("delete plan missing")
						}
						inspect(plans[0].Plan)
					}
					if childWork > 4*limit+16 || totalWork > 6*limit+32 || !targetPoint || !cursorRange || !parentPoint || !tidDelete {
						t.Fatalf("%s/%s exceeded its page: child=%.0f total=%.0f point=%t range=%t parent=%t tid=%t", mode, page.name, childWork, totalWork, targetPoint, cursorRange, parentPoint, tidDelete)
					}
					t.Logf("mode=%s page=%s child_work=%.0f total_work=%.0f", mode, page.name, childWork, totalWork)
				}()
			}
		}
	})
}
