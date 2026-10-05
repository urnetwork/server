package model

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgerrcode"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/urnetwork/server"
)

// This is the exact qualified parent census, before only the metadata fences.
const unfencedNetEscrowCensusForTest = `
    SELECT requested_balance.balance_id,
        COALESCE(revision.revision, 0),
        CASE WHEN balance.balance_id IS NULL THEN 0 ELSE reserved.byte_count END,
        balance.end_time
    FROM unnest($1::uuid[]) AS requested_balance(balance_id)
    LEFT JOIN transfer_balance_net_escrow_revision AS revision USING (balance_id)
    LEFT JOIN transfer_balance AS balance USING (balance_id)
    CROSS JOIN LATERAL (
        SELECT COALESCE(SUM(selected_escrow.balance_byte_count), 0) AS byte_count
        FROM (
            SELECT transfer_escrow.contract_id, transfer_escrow.balance_byte_count
            FROM transfer_escrow
            WHERE transfer_escrow.balance_id = requested_balance.balance_id AND
                transfer_escrow.settled = false AND
                transfer_escrow.balance_byte_count <> 0 AND NOT transfer_escrow.redis_reserved
            OFFSET 0
        ) AS selected_escrow
        INNER JOIN LATERAL (
            SELECT outcome FROM transfer_contract
            WHERE contract_id = selected_escrow.contract_id
            OFFSET 0
        ) AS transfer_contract ON transfer_contract.outcome IS NULL
    ) AS reserved
`

type censusBoundsPlanNode struct {
	NodeType       string                 `json:"Node Type"`
	Relation       string                 `json:"Relation Name"`
	IndexCondition string                 `json:"Index Cond"`
	Rows           float64                `json:"Actual Rows"`
	Removed        float64                `json:"Rows Removed by Filter"`
	Loops          float64                `json:"Actual Loops"`
	Hit            float64                `json:"Shared Hit Blocks"`
	Read           float64                `json:"Shared Read Blocks"`
	Plans          []censusBoundsPlanNode `json:"Plans"`
}

type censusBoundsPlan struct {
	Plan          censusBoundsPlanNode `json:"Plan"`
	ExecutionTime float64              `json:"Execution Time"`
	JIT           struct {
		Functions int `json:"Functions"`
	} `json:"JIT"`
}

// A late page must not walk unrelated metadata after an unanalyzed backfill.
// PostgreSQL can still infer the parent cardinality from physical table growth;
// record that plan rather than assuming that stale statistics force a scan.
// Healthy full-page plans also report cost without a wall-clock speedup claim.
func TestNetEscrowCensusMetadataPointPlans(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
		defer cancel()
		f := seedAdmissionCacheHistory(t, ctx, 3, 20)
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `ALTER TABLE transfer_balance SET (autovacuum_enabled=false)`))
			server.RaisePgResult(tx.Exec(ctx, `ALTER TABLE transfer_balance_net_escrow_revision SET (autovacuum_enabled=false)`))
			server.RaisePgResult(tx.Exec(ctx, `ANALYZE transfer_balance; ANALYZE transfer_balance_net_escrow_revision`))
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO transfer_balance(balance_id,network_id,start_time,end_time,start_balance_byte_count,balance_byte_count,net_revenue_nano_cents,subsidy_net_revenue_nano_cents,pro)
                SELECT md5('unrelated-cache-'||n)::uuid,$1,now()-interval '1 minute',now()+interval '1 hour',1,1,0,0,false FROM generate_series(1,100000)n`, f.sourceNetworkId))
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO transfer_balance_net_escrow_revision(balance_id,revision)
                SELECT md5('unrelated-cache-'||n)::uuid,1 FROM generate_series(1,100000)n`))
		}, server.TxReadCommitted, server.OptNoRetry())

		inspect := func(label string, count int) {
			server.Db(ctx, func(conn server.PgConn) {
				var parentJIT string
				server.Raise(conn.QueryRow(ctx, `SHOW jit`).Scan(&parentJIT))
				ids := []server.Id{}
				rows, err := conn.Query(ctx, `SELECT balance_id FROM transfer_balance ORDER BY balance_id DESC LIMIT $1`, count)
				server.WithPgResult(rows, err, func() {
					for rows.Next() {
						var id server.Id
						server.Raise(rows.Scan(&id))
						ids = append(ids, id)
					}
				})
				if len(ids) != count {
					t.Fatal("metadata fixture page is incomplete")
				}
				terms := make([]string, len(ids))
				for i, id := range ids {
					terms[i] = fmt.Sprintf("'%s'", id)
				}
				argument := "ARRAY[" + strings.Join(terms, ",") + "]::uuid[]"
				for _, mode := range []string{"force_custom_plan", "force_generic_plan"} {
					for _, candidate := range []bool{false, true} {
						func() {
							tx, err := conn.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted, AccessMode: pgx.ReadOnly})
							server.Raise(err)
							defer tx.Rollback(context.WithoutCancel(ctx))
							server.RaisePgResult(tx.Exec(ctx, `SELECT set_config('plan_cache_mode', $1, true)`, mode))
							query := unfencedNetEscrowCensusForTest
							if candidate {
								query = netEscrowReservationPageSQL
								configureNetEscrowReservationPageTimeout(ctx, tx, netEscrowReservationPageStatementTimeout)
							} else {
								server.RaisePgResult(tx.Exec(ctx, `SELECT set_config('statement_timeout', $1, true)`, "120000ms"))
							}
							server.RaisePgResult(tx.Exec(ctx, "PREPARE census_bounds_control(uuid[]) AS "+query))
							defer tx.Exec(context.WithoutCancel(ctx), "DEALLOCATE census_bounds_control")
							var raw []byte
							server.Raise(tx.QueryRow(ctx, "EXPLAIN (ANALYZE,BUFFERS,FORMAT JSON) EXECUTE census_bounds_control("+argument+")").Scan(&raw))
							var plans []censusBoundsPlan
							server.Raise(json.Unmarshal(raw, &plans))
							if len(plans) != 1 {
								t.Fatal("unexpected census plan count")
							}
							metadataRows, metadataLoops, metadataNodes := float64(0), float64(0), 0
							pointScoped := true
							var visit func(censusBoundsPlanNode)
							visit = func(n censusBoundsPlanNode) {
								if n.Relation == "transfer_balance" || n.Relation == "transfer_balance_net_escrow_revision" {
									metadataNodes++
									metadataRows += (n.Rows + n.Removed) * n.Loops
									metadataLoops += n.Loops
									if !strings.Contains(n.IndexCondition, "balance_id") ||
										(n.NodeType != "Index Scan" && n.NodeType != "Index Only Scan") ||
										n.Rows+n.Removed > 1 || n.Loops > float64(count) {
										pointScoped = false
									}
								}
								for _, child := range n.Plans {
									visit(child)
								}
							}
							visit(plans[0].Plan)
							if candidate && (!pointScoped || metadataNodes != 2 || metadataRows > float64(2*count)) {
								t.Fatalf("metadata probes lost their key bound: nodes=%d rows=%.0f loops=%.0f", metadataNodes, metadataRows, metadataLoops)
							}
							if candidate && plans[0].JIT.Functions != 0 {
								t.Fatal("standalone census unexpectedly compiled JIT functions")
							}
							t.Logf("case=%s mode=%s candidate=%t parent_jit=%s requested=%d metadata_rows=%.0f metadata_loops=%.0f buffers=%.0f jit_functions=%d execution_ms=%.3f",
								label, mode, candidate, parentJIT, count, metadataRows, metadataLoops,
								plans[0].Plan.Hit+plans[0].Plan.Read, plans[0].JIT.Functions, plans[0].ExecutionTime)
						}()
					}
				}
			})
		}
		inspect("late_page_unanalyzed_backfill", 10)
		server.Db(ctx, func(conn server.PgConn) {
			server.RaisePgResult(conn.Exec(ctx, `ANALYZE transfer_balance; ANALYZE transfer_balance_net_escrow_revision`))
		})
		inspect("healthy_full_page", 10000)
	})
}

func TestNetEscrowCensusSettingsStayTransactionLocal(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
		defer cancel()
		server.Db(ctx, func(conn server.PgConn) {
			var originalJIT, originalTimeout string
			server.Raise(conn.QueryRow(ctx, `SELECT current_setting('jit'), current_setting('statement_timeout')`).Scan(&originalJIT, &originalTimeout))
			server.RaisePgResult(conn.Exec(ctx, `SELECT set_config('jit', 'on', false), set_config('statement_timeout', '5s', false)`))
			defer conn.Exec(context.WithoutCancel(ctx), `SELECT set_config('jit', $1, false), set_config('statement_timeout', $2, false)`, originalJIT, originalTimeout)
			for _, finish := range []string{"commit", "rollback", "query_canceled"} {
				func() {
					tx, err := conn.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted, AccessMode: pgx.ReadOnly})
					server.Raise(err)
					defer tx.Rollback(context.WithoutCancel(ctx))
					configureNetEscrowReservationPageTimeout(ctx, tx, 25*time.Millisecond)
					var jit, timeout, readonly string
					server.Raise(tx.QueryRow(ctx, `SELECT current_setting('jit'), current_setting('statement_timeout'), current_setting('transaction_read_only')`).Scan(&jit, &timeout, &readonly))
					if jit != "off" || timeout != "25ms" || readonly != "on" {
						t.Fatal("census transaction did not receive its local read-only settings")
					}
					if finish == "query_canceled" {
						_, err := tx.Exec(ctx, `SELECT pg_sleep(1)`)
						var pgErr *pgconn.PgError
						if !errors.As(err, &pgErr) || pgErr.Code != pgerrcode.QueryCanceled {
							t.Fatal("local statement deadline did not cancel the control query")
						}
					}
					if finish == "commit" {
						server.Raise(tx.Commit(ctx))
					} else {
						server.Raise(tx.Rollback(ctx))
					}
					server.Raise(conn.QueryRow(ctx, `SELECT current_setting('jit'), current_setting('statement_timeout')`).Scan(&jit, &timeout))
					if jit != "on" || timeout != "5s" {
						t.Fatalf("census settings escaped the %s boundary", finish)
					}
				}()
			}
		})
	})
}

// Reconciliation is an independent census. A durable cache row with the right
// revision but the wrong amount must never become its source of truth.
func TestNetEscrowCensusIgnoresPoisonedDurableCache(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
		defer cancel()
		f := newNetEscrowOrderingTestFixture(t, ctx)
		_, posts := createNetEscrowOrderingTestContract(ctx, f, 17)
		server.RunPosts(ctx, posts...)
		missingID := server.NewId()
		snapshots := openEscrowReservedForBalances(ctx, []server.Id{f.balanceId, missingID})
		snapshot, ok := snapshots[f.balanceId]
		missing, missingOK := snapshots[missingID]
		if !ok || snapshot.reserved != 17 || snapshot.endTime == nil || !missingOK ||
			missing.reserved != 0 || missing.revision != 0 || missing.endTime != nil {
			t.Fatal("exact census changed known or missing balance semantics")
		}
		const poison = ByteCount(999)
		server.Db(ctx, func(conn server.PgConn) {
			server.RaisePgResult(conn.Exec(ctx, `INSERT INTO transfer_balance_net_escrow_snapshot(balance_id, revision, reserved_byte_count)
                VALUES ($1,$2,$3) ON CONFLICT(balance_id) DO UPDATE
                SET revision=EXCLUDED.revision, reserved_byte_count=EXCLUDED.reserved_byte_count`, f.balanceId, snapshot.revision, poison))
		})
		server.Redis(ctx, func(r server.RedisClient) {
			// Keep the correct revision fence; only the approximate counter drifts.
			server.Raise(r.Set(ctx, netEscrowKey(f.balanceId), int64(poison), time.Hour).Err())
		})
		assertState := func(wantCounter ByteCount) {
			t.Helper()
			server.Redis(ctx, func(r server.RedisClient) {
				got, err := r.Get(ctx, netEscrowKey(f.balanceId)).Int64()
				server.Raise(err)
				if ByteCount(got) != wantCounter {
					t.Fatal("unexpected legacy mirror amount")
				}
			})
			server.Db(ctx, func(conn server.PgConn) {
				var amount int64
				server.Raise(conn.QueryRow(ctx, `SELECT reserved_byte_count FROM transfer_balance_net_escrow_snapshot WHERE balance_id=$1`, f.balanceId).Scan(&amount))
				if ByteCount(amount) != poison {
					t.Fatal("reconciliation unexpectedly rewrote the durable cache")
				}
			})
		}
		drift, count := ReconcileNetEscrowForNetwork(ctx, f.sourceNetworkId, false)
		if count != 1 || drift != poison-17 {
			t.Fatal("dry-run did not report exact source drift")
		}
		assertState(poison)
		drift, count = ReconcileNetEscrowForNetwork(ctx, f.sourceNetworkId, true)
		if count != 1 || drift != poison-17 {
			t.Fatal("repair did not use the independent source census")
		}
		assertState(17)
	})
}

// Either open row is still the last witness for an expired balance. Its exact
// legacy sum is zero, but visiting it must repair stale legacy mirror debt
// without releasing a live Redis reservation or changing database accounting.
func TestNetEscrowCensusRepairsExpiredLastWitness(t *testing.T) {
	for _, redisReserved := range []bool{false, true} {
		t.Run(fmt.Sprintf("redis_reserved_%t", redisReserved), func(t *testing.T) {
			env := server.DefaultTestEnv()
			env.RerunCount = 0
			env.Run(t, func(t testing.TB) {
				ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
				defer cancel()
				f := newNetEscrowOrderingTestFixture(t, ctx)
				var contract *TransferEscrow
				var liveRedis ByteCount
				if redisReserved {
					liveRedis = 17
					contract = createRedisAdmissionTest(ctx, f, liveRedis)
				} else {
					contract, _ = createNetEscrowOrderingTestContract(ctx, f, 0)
				}
				keys := redisContractReservationKeys(f.balanceId)
				var tokenExpiry float64
				if redisReserved {
					server.Redis(ctx, func(r server.RedisClient) {
						var err error
						tokenExpiry, err = r.ZScore(ctx, keys[2], contract.ContractId.String()).Result()
						server.Raise(err)
					})
				}
				server.Db(ctx, func(conn server.PgConn) {
					server.RaisePgResult(conn.Exec(ctx, `UPDATE transfer_balance SET end_time=$2 WHERE balance_id=$1`, f.balanceId, server.NowUtc().Add(-time.Minute)))
				})
				ids := []server.Id{f.balanceId}
				before := openEscrowReservedForBalances(ctx, ids)[f.balanceId]
				if before.reserved != 0 || before.endTime == nil {
					t.Fatal("last witness did not produce an exact zero legacy snapshot")
				}
				reconcileNetEscrowBatch(ctx, map[server.Id]netEscrowSnapshot{f.balanceId: before}, ids, true)
				assertUnchanged := func(wantLegacy ByteCount) {
					t.Helper()
					if got := Testing_NetEscrowByteCount(ctx, f.balanceId); got != wantLegacy+liveRedis {
						t.Fatalf("legacy mirror repair changed total reservation: got=%d want=%d", got, wantLegacy+liveRedis)
					}
					server.Db(ctx, func(conn server.PgConn) {
						var balance, escrow ByteCount
						var reserved bool
						var outcome *string
						server.Raise(conn.QueryRow(ctx, `SELECT b.balance_byte_count,e.balance_byte_count,e.redis_reserved,c.outcome
                            FROM transfer_balance b JOIN transfer_escrow e USING(balance_id)
                            JOIN transfer_contract c USING(contract_id) WHERE b.balance_id=$1`, f.balanceId).Scan(&balance, &escrow, &reserved, &outcome))
						if balance != 1000 || escrow != liveRedis || reserved != redisReserved || outcome != nil {
							t.Fatal("census changed last-witness accounting")
						}
					})
					after := openEscrowReservedForBalances(ctx, ids)[f.balanceId]
					if after.revision != before.revision || after.reserved != 0 {
						t.Fatal("reconciliation changed the exact source snapshot")
					}
					if redisReserved {
						server.Redis(ctx, func(r server.RedisClient) {
							amount, err := r.HGet(ctx, keys[1], contract.ContractId.String()).Int64()
							server.Raise(err)
							expiry, err := r.ZScore(ctx, keys[2], contract.ContractId.String()).Result()
							server.Raise(err)
							if ByteCount(amount) != liveRedis || expiry != tokenExpiry {
								t.Fatal("legacy census changed the live Redis token or lease")
							}
						})
					}
				}
				const poison = ByteCount(999)
				for _, targeted := range []bool{false, true} {
					server.Redis(ctx, func(r server.RedisClient) {
						server.Raise(r.Set(ctx, netEscrowKey(f.balanceId), int64(poison), time.Hour).Err())
					})
					for _, apply := range []bool{false, true} {
						var drift ByteCount
						var count int
						if targeted {
							drift, count = ReconcileNetEscrowForNetwork(ctx, f.sourceNetworkId, apply)
						} else {
							var all map[server.Id]ByteCount
							all, count = ReconcileNetEscrow(ctx, apply)
							drift = all[f.sourceNetworkId]
						}
						if count != 1 || drift != poison {
							t.Fatalf("last witness omitted: targeted=%t apply=%t count=%d drift=%d", targeted, apply, count, drift)
						}
						if apply {
							assertUnchanged(0)
						} else {
							assertUnchanged(poison)
						}
					}
				}
			})
		})
	}
}
