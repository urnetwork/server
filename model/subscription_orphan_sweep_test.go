package model

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/urnetwork/server"
)

// These are the persisted step identities, independent of the SQL builder.
var orphanSweepTestKeys = [][]string{
	{"contract_id", "party"},
	{"contract_id", "balance_id"},
	{"contract_id", "balance_id", "network_id"},
	{"stream_id", "client_id"},
	{"contract_id", "extender_id", "party"},
}

func orphanSweepTestId(n int) server.Id {
	var id server.Id
	binary.BigEndian.PutUint64(id[8:], uint64(n))
	return id
}

func orphanSweepTestKey(step, n int, minimum bool) []any {
	id, tail := orphanSweepTestId(n), orphanSweepTestId(1)
	party := "source"
	if minimum {
		tail, party = server.Id{}, ""
	}
	switch step {
	case 0:
		return []any{id, party}
	case 1, 3:
		return []any{id, tail}
	case 2:
		return []any{id, tail, tail}
	case 4:
		return []any{id, tail, party}
	default:
		panic("invalid orphan sweep test step")
	}
}

// Use the migrated tables, indexes and triggers. Zero-byte metadata keeps this
// query-work fixture separate from billing; real funded retention controls also
// run in the focused gate. A duplicate stream reference exercises EXISTS rather
// than assuming a unique parent stream.
func seedOrphanSweepTestHistory(t testing.TB, ctx context.Context, parents, orphans int) {
	t.Helper()
	clients := newEscrowSelectionTestClients(t, ctx)
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(ctx, `INSERT INTO transfer_contract
			(contract_id,source_network_id,source_id,destination_network_id,destination_id,
			 payer_network_id,transfer_byte_count,stream_id)
			SELECT lpad(to_hex(n),32,'0')::uuid,$1,$2,$3,$4,$1,0,lpad(to_hex(n),32,'0')::uuid
			FROM generate_series(0,$5::integer) n`, clients.payerNetworkId, clients.payerId,
			clients.providerNetworkId, clients.providerId, parents))
		server.RaisePgResult(tx.Exec(ctx, `INSERT INTO transfer_contract
			(contract_id,source_network_id,source_id,destination_network_id,destination_id,
			 payer_network_id,transfer_byte_count,stream_id)
			VALUES($1,$2,$3,$4,$5,$2,0,$6)`, orphanSweepTestId(parents+orphans+1000),
			clients.payerNetworkId, clients.payerId, clients.providerNetworkId, clients.providerId, orphanSweepTestId(1)))
		server.RaisePgResult(tx.Exec(ctx, `INSERT INTO contract_close(contract_id,party,used_transfer_byte_count)
			SELECT lpad(to_hex(n),32,'0')::uuid,'source',0 FROM generate_series(0,$1::integer) n`, parents+orphans))
		server.RaisePgResult(tx.Exec(ctx, `INSERT INTO transfer_escrow(contract_id,balance_id,balance_byte_count)
			SELECT lpad(to_hex(n),32,'0')::uuid,$2,0 FROM generate_series(0,$1::integer) n`, parents+orphans, orphanSweepTestId(1)))
		server.RaisePgResult(tx.Exec(ctx, `INSERT INTO transfer_escrow_sweep
			(contract_id,balance_id,network_id,payout_byte_count,payout_net_revenue_nano_cents)
			SELECT lpad(to_hex(n),32,'0')::uuid,$2,$2,0,0 FROM generate_series(0,$1::integer) n`, parents+orphans, orphanSweepTestId(1)))
		server.RaisePgResult(tx.Exec(ctx, `INSERT INTO contract_participant(stream_id,client_id,network_id)
			SELECT lpad(to_hex(n),32,'0')::uuid,$2,$3 FROM generate_series(0,$1::integer) n`, parents+orphans, orphanSweepTestId(1), clients.providerNetworkId))
		server.RaisePgResult(tx.Exec(ctx, `INSERT INTO contract_extender(contract_id,extender_id,party,client_id,network_id)
			SELECT lpad(to_hex(n),32,'0')::uuid,$2,'source',$3,$4 FROM generate_series(0,$1::integer) n`, parents+orphans,
			orphanSweepTestId(1), clients.providerId, clients.providerNetworkId))
		// Every first page must include the absolute minimum composite key;
		// a synthetic zero cursor with an unconditional '>' would lose it.
		server.RaisePgResult(tx.Exec(ctx, `INSERT INTO contract_close(contract_id,party,used_transfer_byte_count) VALUES($1,'',0)`, server.Id{}))
		server.RaisePgResult(tx.Exec(ctx, `INSERT INTO transfer_escrow(contract_id,balance_id,balance_byte_count) VALUES($1,$1,0)`, server.Id{}))
		server.RaisePgResult(tx.Exec(ctx, `INSERT INTO transfer_escrow_sweep
			(contract_id,balance_id,network_id,payout_byte_count,payout_net_revenue_nano_cents) VALUES($1,$1,$1,0,0)`, server.Id{}))
		server.RaisePgResult(tx.Exec(ctx, `INSERT INTO contract_participant(stream_id,client_id,network_id) VALUES($1,$1,$2)`, server.Id{}, clients.providerNetworkId))
		server.RaisePgResult(tx.Exec(ctx, `INSERT INTO contract_extender(contract_id,extender_id,party,client_id,network_id)
			VALUES($1,$1,'',$2,$3)`, server.Id{}, clients.providerId, clients.providerNetworkId))
	}, server.TxReadCommitted, server.OptNoRetry())
}

type orphanSweepTestPlanNode struct {
	NodeType       string                    `json:"Node Type"`
	Relation       string                    `json:"Relation Name"`
	Alias          string                    `json:"Alias"`
	Index          string                    `json:"Index Name"`
	IndexCondition string                    `json:"Index Cond"`
	Recheck        string                    `json:"Recheck Cond"`
	Rows           float64                   `json:"Actual Rows"`
	Removed        float64                   `json:"Rows Removed by Filter"`
	JoinRemoved    float64                   `json:"Rows Removed by Join Filter"`
	RecheckRemoved float64                   `json:"Rows Removed by Index Recheck"`
	Loops          float64                   `json:"Actual Loops"`
	Hits           float64                   `json:"Shared Hit Blocks"`
	Reads          float64                   `json:"Shared Read Blocks"`
	Plans          []orphanSweepTestPlanNode `json:"Plans"`
}

// Bound physical work, not elapsed time: both page selection and the DELETE
// target must stay within this page under custom and generic planning. An old
// bool-OR cursor or a hash join to the whole child history exceeds this oracle.
func TestSweepOrphanContractPlansStayInsideSelectedPage(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
		defer cancel()
		const parents, orphans, limit = 20000, 100, 128
		seedOrphanSweepTestHistory(t, ctx, parents, orphans)
		// Both a retained and an orphan leading key have a dense suffix. The
		// migrated sweep-time covering index previously matched only contract_id
		// and filtered these suffixes, despite a small returned page.
		clients := newEscrowSelectionTestClients(t, ctx)
		server.Tx(ctx, func(tx server.PgTx) {
			for _, leading := range []int{parents / 2, parents + 50} {
				id := orphanSweepTestId(leading)
				server.RaisePgResult(tx.Exec(ctx, `INSERT INTO contract_close(contract_id,party,used_transfer_byte_count)
					SELECT $1,'p' || lpad(n::text,8,'0'),0 FROM generate_series(2,20001) n`, id))
				server.RaisePgResult(tx.Exec(ctx, `INSERT INTO transfer_escrow(contract_id,balance_id,balance_byte_count)
					SELECT $1,lpad(to_hex(n),32,'0')::uuid,0 FROM generate_series(2,20001) n`, id))
				server.RaisePgResult(tx.Exec(ctx, `INSERT INTO transfer_escrow_sweep
					(contract_id,balance_id,network_id,payout_byte_count,payout_net_revenue_nano_cents)
					SELECT $1,lpad(to_hex(n),32,'0')::uuid,$2,0,0 FROM generate_series(2,20001) n`, id, orphanSweepTestId(1)))
				server.RaisePgResult(tx.Exec(ctx, `INSERT INTO contract_participant(stream_id,client_id,network_id)
					SELECT $1,lpad(to_hex(n),32,'0')::uuid,$2 FROM generate_series(2,20001) n`, id, clients.providerNetworkId))
				server.RaisePgResult(tx.Exec(ctx, `INSERT INTO contract_extender(contract_id,extender_id,party,client_id,network_id)
					SELECT $1,lpad(to_hex(n),32,'0')::uuid,'source',$2,$3 FROM generate_series(2,20001) n`, id, clients.providerId, clients.providerNetworkId))
			}
		}, server.TxReadCommitted, server.OptNoRetry())
		steps := sweepOrphanContractSteps()
		conn := acquireContractLifecycleTestConnection(t, ctx)
		defer conn.Release()
		server.RaisePgResult(conn.Exec(ctx, `ANALYZE transfer_contract; ANALYZE contract_close;
			ANALYZE transfer_escrow; ANALYZE transfer_escrow_sweep; ANALYZE contract_participant; ANALYZE contract_extender`))
		for stepIndex, step := range steps {
			keys := orphanSweepTestKeys[stepIndex]
			skewKey := func(leading int) []any {
				key := orphanSweepTestKey(stepIndex, leading, false)
				if stepIndex == 0 {
					key[1] = "p00019800"
				} else {
					key[1] = orphanSweepTestId(19800)
				}
				return key
			}
			for _, mode := range []string{"force_custom_plan", "force_generic_plan"} {
				for _, page := range []struct {
					name   string
					cursor []any
					first  bool
				}{
					{name: "first", first: true},
					{name: "dense_prefix", cursor: orphanSweepTestKey(stepIndex, parents-200, false)},
					{name: "orphan_tail", cursor: orphanSweepTestKey(stepIndex, parents-10, false)},
					{name: "terminal", cursor: orphanSweepTestKey(stepIndex, parents+orphans, false)},
					{name: "retained_suffix", cursor: skewKey(parents / 2)},
					{name: "orphan_suffix", cursor: skewKey(parents + 50)},
				} {
					func() {
						tx, err := conn.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted, AccessMode: pgx.ReadWrite})
						server.Raise(err)
						defer func() {
							_ = tx.Rollback(context.WithoutCancel(ctx))
							_, _ = conn.Exec(context.WithoutCancel(ctx), `DEALLOCATE orphan_page_plan`)
							_, _ = conn.Exec(context.WithoutCancel(ctx), `DEALLOCATE orphan_delete_plan`)
						}()
						server.RaisePgResult(tx.Exec(ctx, `SET LOCAL statement_timeout='20s'; SET LOCAL lock_timeout='3s'; SET LOCAL jit=off`))
						server.RaisePgResult(tx.Exec(ctx, `SET LOCAL plan_cache_mode=`+mode))
						query, arguments := step.firstSql, fmt.Sprint(limit)
						if !page.first {
							query = step.sql
							values := encodeSweepCursorKey(page.cursor)
							arguments = "'" + strings.Join(values, "','") + "'," + fmt.Sprint(limit)
						}
						server.RaisePgResult(tx.Exec(ctx, `PREPARE orphan_page_plan AS `+query))
						var raw []byte
						server.Raise(tx.QueryRow(ctx, `EXPLAIN (ANALYZE,BUFFERS,TIMING OFF,FORMAT JSON) EXECUTE orphan_page_plan(`+arguments+`)`).Scan(&raw))
						var plans []struct {
							Plan orphanSweepTestPlanNode `json:"Plan"`
						}
						server.Raise(json.Unmarshal(raw, &plans))
						if len(plans) != 1 {
							t.Fatal("orphan sweep plan count changed")
						}
						childWork, totalWork := float64(0), float64(0)
						targetPoint, cursorRange, parentPoint, tidDelete := false, page.first, false, false
						var inspect func(orphanSweepTestPlanNode)
						inspect = func(node orphanSweepTestPlanNode) {
							condition := node.IndexCondition + " " + node.Recheck
							if node.Relation != "" && node.NodeType != "ModifyTable" {
								if node.NodeType == "Seq Scan" || node.NodeType == "Parallel Seq Scan" {
									t.Fatalf("%s/%s/%s scanned a whole relation", step.table, mode, page.name)
								}
								work := (node.Rows + node.Removed + node.JoinRemoved + node.RecheckRemoved) * node.Loops
								totalWork += work
								if node.Relation == step.table {
									childWork += work
									completeKey := true
									for _, key := range keys {
										completeKey = completeKey && strings.Contains(condition, key)
									}
									if node.Alias == "sweep_target" {
										targetPoint = targetPoint || (completeKey && strings.Contains(condition, "=") &&
											!strings.Contains(condition, ">") && !strings.Contains(condition, "<"))
									} else if node.Alias == "sweep_delete" {
										tidDelete = tidDelete || node.NodeType == "Tid Scan"
									} else if completeKey && strings.Contains(condition, ">") {
										cursorRange = true
									}
								}
								if node.Relation == "transfer_contract" {
									index := "transfer_contract_pkey"
									if keys[0] == "stream_id" {
										index = "transfer_contract_stream_id"
									}
									if node.Index != index || !strings.Contains(condition, keys[0]+" =") ||
										node.Rows+node.Removed > 1 || node.Loops > limit {
										t.Fatalf("%s/%s/%s lost its parent point probe", step.table, mode, page.name)
									}
									parentPoint = true
								}
							}
							for _, child := range node.Plans {
								inspect(child)
							}
						}
						inspect(plans[0].Plan)
						// Materialize the actual selected addresses in this transaction,
						// then inspect the separately prepared deletion's fresh snapshot.
						var examined int64
						var locked []string
						targets := []any{&examined, &locked}
						targets = append(targets, step.newCursorTargets()...)
						// This manual prepared name is rebound to different table key
						// shapes. Its EXECUTE text cannot share a cached description.
						err = tx.QueryRow(ctx, `EXECUTE orphan_page_plan(`+arguments+`)`, pgx.QueryExecModeExec).Scan(targets...)
						if err != nil && err != pgx.ErrNoRows {
							server.Raise(err)
						}
						if len(locked) > limit || int64(len(locked)) > examined {
							t.Fatal("locked tuple count exceeded the selected page")
						}
						if len(locked) == 0 {
							tidDelete = true // production skips this statement
						} else {
							server.RaisePgResult(tx.Exec(ctx, `PREPARE orphan_delete_plan(text[]) AS `+step.deleteSql))
							literal := "ARRAY['" + strings.Join(locked, "','") + "']::text[]"
							server.Raise(tx.QueryRow(ctx, `EXPLAIN (ANALYZE,BUFFERS,TIMING OFF,FORMAT JSON)
								EXECUTE orphan_delete_plan(`+literal+`)`).Scan(&raw))
							plans = nil
							server.Raise(json.Unmarshal(raw, &plans))
							if len(plans) != 1 {
								t.Fatal("orphan deletion plan count changed")
							}
							inspect(plans[0].Plan)
						}
						if childWork > 4*limit+16 || totalWork > 6*limit+32 || !targetPoint || !cursorRange || !parentPoint || !tidDelete {
							t.Fatalf("%s/%s/%s exceeded the selected page: child=%.0f total=%.0f target_point=%t cursor_range=%t parent_point=%t tid_delete=%t",
								step.table, mode, page.name, childWork, totalWork, targetPoint, cursorRange, parentPoint, tidDelete)
						}
						t.Logf("table=%s mode=%s page=%s child_work=%.0f total_work=%.0f shared_hits=%.0f shared_reads=%.0f",
							step.table, mode, page.name, childWork, totalWork, plans[0].Plan.Hits, plans[0].Plan.Reads)
					}()
				}
			}
		}
	})
}

// Exercise actual first/resumed argument dispatch and persisted JSON cursors for
// all five key shapes, including the all-zero key and same-leading-key ties.
func TestSweepOrphanContractAllFiveCursorRoundTrips(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		const parents, orphans = 3, 2
		seedOrphanSweepTestHistory(t, ctx, parents, orphans)
		cursor := SweepOrphanCursor{}
		var removed int64
		emitted := make([]int, len(orphanSweepTestKeys))
		done := false
		for page := 0; !done; page++ {
			if page > 40 {
				t.Fatal("all-five cursor did not converge")
			}
			previous := cursor
			var count int64
			count, cursor, done = SweepOrphanContractData(ctx, cursor, 1, 1)
			removed += count
			if !done && (cursor.Step < previous.Step || reflect.DeepEqual(cursor, previous)) {
				t.Fatal("persisted cursor failed to advance")
			}
			if !done {
				if cursor.Step < 0 || len(emitted) <= cursor.Step || len(cursor.Key) == 0 {
					t.Fatal("row-budgeted sweep returned an invalid row cursor")
				}
				if emitted[cursor.Step] < 2 {
					want := encodeSweepCursorKey(orphanSweepTestKey(cursor.Step, 0, emitted[cursor.Step] == 0))
					if !reflect.DeepEqual(cursor.Key, want) {
						t.Fatalf("step %d omitted a minimum or tied composite key", cursor.Step)
					}
				}
				emitted[cursor.Step]++
			}
			raw, err := json.Marshal(cursor)
			server.Raise(err)
			cursor = SweepOrphanCursor{}
			server.Raise(json.Unmarshal(raw, &cursor))
		}
		if removed != 5*orphans || !reflect.DeepEqual(cursor, SweepOrphanCursor{}) {
			t.Fatalf("all-five cursor removed=%d, expected=%d, reset=%t", removed, 5*orphans, reflect.DeepEqual(cursor, SweepOrphanCursor{}))
		}
		for step, count := range emitted {
			if count != parents+orphans+2 {
				t.Fatalf("step %d emitted %d row cursors, expected %d", step, count, parents+orphans+2)
			}
		}
		server.Db(ctx, func(conn server.PgConn) {
			for i, step := range sweepOrphanContractSteps() {
				var count, orphanCount int
				server.Raise(conn.QueryRow(ctx, `SELECT count(*),count(*) FILTER(WHERE NOT EXISTS(
					SELECT 1 FROM transfer_contract p WHERE p.`+orphanSweepTestKeys[i][0]+`=c.`+orphanSweepTestKeys[i][0]+`)) FROM `+step.table+` c`).Scan(&count, &orphanCount))
				if count != parents+2 || orphanCount != 0 {
					t.Fatalf("%s lost retained rows or kept orphans: count=%d orphans=%d", step.table, count, orphanCount)
				}
			}
		})
		count, _, complete := SweepOrphanContractData(ctx, SweepOrphanCursor{}, 0, 2)
		if count != 0 || !complete {
			t.Fatal("completed all-five sweep was not replay-safe")
		}
	})
}

// A waited non-key update must still be deleted by the ordinary model path.
// This caught the rejected SELECT FOR UPDATE/ctid candidate: it returned one
// selected row but zero deletes after the updater committed a new tuple.
func TestSweepOrphanContractFollowsConcurrentPayloadUpdate(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
		defer cancel()
		id := server.NewId()
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO contract_close(contract_id,party,used_transfer_byte_count) VALUES($1,'source',0)`, id))
		})
		holder := acquireContractLifecycleTestConnection(t, ctx)
		defer holder.Release()
		held, err := holder.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
		server.Raise(err)
		defer held.Rollback(context.WithoutCancel(ctx))
		var holderPid int
		server.Raise(held.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&holderPid))
		server.RaisePgResult(held.Exec(ctx, `UPDATE contract_close SET used_transfer_byte_count=1 WHERE contract_id=$1 AND party='source'`, id))
		type result struct {
			removed int64
			done    bool
			err     any
		}
		finished := make(chan result, 1)
		go func() {
			var r result
			r.err = server.HandleError(func() { r.removed, _, r.done = SweepOrphanContractData(ctx, SweepOrphanCursor{}, 0, 1) })
			finished <- r
		}()
		joined := false
		defer func() {
			cancel()
			_ = held.Rollback(context.Background())
			if !joined {
				<-finished
			}
		}()
		tick := time.NewTicker(5 * time.Millisecond)
		defer tick.Stop()
		for {
			var blocked bool
			server.Db(ctx, func(conn server.PgConn) {
				server.Raise(conn.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity
					WHERE datname=current_database() AND wait_event_type='Lock'
					AND query LIKE '%WITH slice%' AND query LIKE '%FROM contract_close%'
					AND $1::integer=ANY(pg_blocking_pids(pid)))`, holderPid).Scan(&blocked))
			})
			if blocked {
				break
			}
			select {
			case r := <-finished:
				joined = true
				t.Fatalf("sweep ended before its concrete update wait: removed=%d done=%t err=%v", r.removed, r.done, r.err)
			case <-tick.C:
			case <-ctx.Done():
				t.Fatal("sweep update wait edge was not observed")
			}
		}
		server.Raise(held.Commit(ctx))
		r := <-finished
		joined = true
		if r.err != nil || r.removed != 1 || !r.done {
			t.Fatalf("waited orphan update changed deletion semantics: removed=%d done=%t err=%v", r.removed, r.done, r.err)
		}
		server.Db(ctx, func(conn server.PgConn) {
			var count int
			server.Raise(conn.QueryRow(ctx, `SELECT count(*) FROM contract_close WHERE contract_id=$1`, id).Scan(&count))
			if count != 0 {
				t.Fatal("updated orphan remained after the successful sweep")
			}
		})
	})
}

// A failure after the second statement has removed a row must roll back both
// stages and release the first statement's row locks. No partial cursor escapes.
func TestSweepOrphanContractTwoStageRollback(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
		defer cancel()
		seedOrphanSweepTestHistory(t, ctx, 3, 2)
		for i, step := range sweepOrphanContractSteps() {
			failed := step
			failed.deleteSql = `WITH removed AS (` + step.deleteSql + ` RETURNING 1)
				SELECT 1 / (count(*) - 1) FROM removed`
			panicValue := callWithForcedFailure(ctx, func(callCtx context.Context) {
				sweepOrphanCursor(callCtx, failed, orphanSweepTestKey(i, 4, false), 1, 1)
			})
			if !isForcedFailure(panicValue, "22012", "") {
				t.Fatalf("step %d did not reach the injected post-delete failure: %v", i, panicValue)
			}
			predicates := make([]string, len(orphanSweepTestKeys[i]))
			for j, key := range orphanSweepTestKeys[i] {
				predicates[j] = fmt.Sprintf("%s=$%d", key, j+1)
			}
			server.Tx(ctx, func(tx server.PgTx) {
				var one, count int
				server.Raise(tx.QueryRow(ctx, `SELECT 1 FROM `+step.table+` WHERE `+
					strings.Join(predicates, " AND ")+` FOR UPDATE NOWAIT`, orphanSweepTestKey(i, 5, false)...).Scan(&one))
				server.Raise(tx.QueryRow(ctx, `SELECT count(*) FROM `+step.table).Scan(&count))
				if one != 1 || count != 7 {
					t.Fatalf("step %d leaked a partial delete", i)
				}
			}, server.TxReadCommitted, server.OptNoRetry())
			removed, examined, key, done := sweepOrphanCursor(ctx, step, orphanSweepTestKey(i, 4, false), 1, 1)
			if removed != 1 || examined != 1 || done || !reflect.DeepEqual(key, orphanSweepTestKey(i, 5, false)) {
				t.Fatalf("step %d did not resume exactly after rollback", i)
			}
			removed, examined, _, done = sweepOrphanCursor(ctx, step, key, 1, 1)
			if removed != 0 || examined != 0 || !done {
				t.Fatalf("step %d repeated a completed delete", i)
			}
		}
	})
}

// A parent committed between the selection and deletion snapshots retains its
// child. This conservative read-committed outcome is explicit, not a claim of
// serializable membership against a parent inserted after the final recheck.
func TestSweepOrphanContractRechecksParentAfterLock(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
		defer cancel()
		seedOrphanSweepTestHistory(t, ctx, 3, 2)
		conn := acquireContractLifecycleTestConnection(t, ctx)
		defer conn.Release()
		for i, step := range sweepOrphanContractSteps() {
			func() {
				tx, err := conn.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
				server.Raise(err)
				defer tx.Rollback(context.WithoutCancel(ctx))
				var count int64
				var locked []string
				cursorTargets := step.newCursorTargets()
				targets := append([]any{&count, &locked}, cursorTargets...)
				args := append(orphanSweepTestKey(i, 4, false), 1)
				server.Raise(tx.QueryRow(ctx, step.sql, args...).Scan(targets...))
				if count != 1 || len(locked) != 1 || !reflect.DeepEqual(derefCursor(cursorTargets), orphanSweepTestKey(i, 5, false)) {
					t.Fatalf("step %d did not lock the selected orphan", i)
				}
				server.Tx(ctx, func(parentTx server.PgTx) {
					server.RaisePgResult(parentTx.Exec(ctx, `INSERT INTO transfer_contract
						(contract_id,source_network_id,source_id,destination_network_id,destination_id,
						 payer_network_id,transfer_byte_count,stream_id)
						SELECT $1,source_network_id,source_id,destination_network_id,destination_id,
						 payer_network_id,0,$1 FROM transfer_contract WHERE contract_id=$2`, orphanSweepTestId(5), orphanSweepTestId(1)))
				}, server.TxReadCommitted, server.OptNoRetry())
				tag, err := tx.Exec(ctx, step.deleteSql, locked)
				server.Raise(err)
				if tag.RowsAffected() != 0 {
					t.Fatalf("step %d deleted a child of the newly visible parent", i)
				}
				server.Raise(tx.Commit(ctx))
			}()
			server.Tx(ctx, func(tx server.PgTx) {
				var count int
				server.Raise(tx.QueryRow(ctx, `SELECT count(*) FROM `+step.table).Scan(&count))
				if count != 7 {
					t.Fatalf("step %d changed child membership", i)
				}
				server.RaisePgResult(tx.Exec(ctx, `DELETE FROM transfer_contract WHERE contract_id=$1`, orphanSweepTestId(5)))
			}, server.TxReadCommitted, server.OptNoRetry())
		}
	})
}
