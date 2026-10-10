package model

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/urnetwork/server/v2026"
)

type admissionCacheTestTx struct {
	server.PgTx
	exact     *atomic.Int64
	forceMiss bool
}

func (tx admissionCacheTestTx) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	if sql == netEscrowReservationPageSQL && tx.exact != nil {
		tx.exact.Add(1)
	}
	if sql == netEscrowAdmissionCacheSQL && tx.forceMiss {
		sql = strings.Replace(sql, "snapshot.revision = COALESCE(revision.revision, 0)", "false", 1)
	}
	return tx.PgTx.Query(ctx, sql, args...)
}

func seedAdmissionCacheHistory(t testing.TB, ctx context.Context, count, credit int) netEscrowOrderingTestFixture {
	t.Helper()
	f := newNetEscrowOrderingTestFixture(t, ctx)
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_balance SET start_balance_byte_count=$2,balance_byte_count=$2 WHERE balance_id=$1`, f.balanceId, count+credit))
		server.RaisePgResult(tx.Exec(ctx, `INSERT INTO transfer_contract(contract_id,source_network_id,source_id,destination_network_id,destination_id,payer_network_id,transfer_byte_count)
   SELECT md5($5::uuid::text||'-cache-contract-'||n)::uuid,$1,$2,$3,$4,$1,1 FROM generate_series(1,$6)n`, f.sourceNetworkId, f.sourceId, f.destinationNetworkId, f.destinationId, f.balanceId, count))
		server.RaisePgResult(tx.Exec(ctx, `INSERT INTO transfer_escrow(contract_id,balance_id,balance_byte_count)
   SELECT md5($1::uuid::text||'-cache-contract-'||n)::uuid,$1,1 FROM generate_series(1,$2)n`, f.balanceId, count))
	})
	return f
}

func cacheTestCreate(ctx context.Context, f netEscrowOrderingTestFixture, count *atomic.Int64, forceMiss bool) (escrow *TransferEscrow, err error) {
	defer func() {
		if v := recover(); v != nil {
			if e, ok := v.(error); ok {
				err = e
			} else {
				err = fmt.Errorf("unexpected panic type %T", v)
			}
		}
	}()
	server.Tx(ctx, func(tx server.PgTx) {
		escrow, _, err = createTransferEscrowInTx(ctx, admissionCacheTestTx{tx, count, forceMiss}, f.sourceNetworkId, f.sourceId, f.destinationNetworkId, f.destinationId, f.sourceNetworkId, 1, nil)
	}, server.TxReadCommitted, server.OptNoRetry())
	return
}

func TestNetEscrowAdmissionCacheContendedOpenHistory(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
		defer cancel()
		for _, forceMiss := range []bool{true, false} {
			f := seedAdmissionCacheHistory(t, ctx, 10001, 8)
			var census atomic.Int64
			var accepted atomic.Int64
			var rejected atomic.Int64
			errs := make(chan error, 20)
			var wg sync.WaitGroup
			start := make(chan struct{})
			began := time.Now()
			for range 20 {
				wg.Add(1)
				go func() {
					defer wg.Done()
					<-start
					escrow, err := cacheTestCreate(ctx, f, &census, forceMiss)
					if err == nil && escrow != nil {
						accepted.Add(1)
					} else if err != nil && strings.Contains(err.Error(), "Insufficient balance") {
						rejected.Add(1)
					} else {
						errs <- fmt.Errorf("unexpected admission result: %v", err)
					}
				}()
			}
			close(start)
			wg.Wait()
			close(errs)
			for err := range errs {
				t.Error(err)
			}
			if accepted.Load() != 8 || rejected.Load() != 12 {
				t.Fatalf("accepted=%d rejected=%d, want8/12", accepted.Load(), rejected.Load())
			}
			if exact := openEscrowReservedForBalances(ctx, []server.Id{f.balanceId})[f.balanceId].reserved; exact != 10009 {
				t.Fatalf("concurrent admissions reserved %d, want10009", exact)
			}
			want := int64(1)
			if forceMiss {
				want = 20
			}
			if census.Load() != want {
				t.Fatalf("forceMiss=%t exact censuses=%d want%d", forceMiss, census.Load(), want)
			}
			t.Logf("forceMiss=%t requests20 accepted8 rejected12 exact_history_reads=%d elapsed=%s", forceMiss, census.Load(), time.Since(began))
		}
	})
}

func TestNetEscrowAdmissionCacheHitDoesNotReadEscrowHistory(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
		defer cancel()
		f := seedAdmissionCacheHistory(t, ctx, 3, 20)
		// Prime through real admission, whose insert+contract+cache share one commit.
		if _, err := cacheTestCreate(ctx, f, nil, false); err != nil {
			t.Fatal(err)
		}
		blocker := acquireContractLifecycleTestConnection(t, ctx)
		defer blocker.Release()
		held, err := blocker.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer held.Rollback(context.Background())
		server.RaisePgResult(held.Exec(ctx, `LOCK TABLE transfer_escrow IN ACCESS EXCLUSIVE MODE`))
		conn := acquireContractLifecycleTestConnection(t, ctx)
		defer conn.Release()
		tx, err := conn.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(context.Background())
		server.RaisePgResult(tx.Exec(ctx, `SET LOCAL lock_timeout='150ms'`))
		server.RaisePgResult(tx.Exec(ctx, `SELECT 1 FROM transfer_balance WHERE balance_id=$1 FOR UPDATE NOWAIT`, f.balanceId))
		// Reading cached authority succeeds while any historical table access is
		// impossible. Full admission would still need its normal escrow INSERT.
		got := readLockedNetEscrowSnapshots(ctx, tx, []server.Id{f.balanceId})[f.balanceId]
		if got.reserved != 4 {
			t.Fatalf("cached amount=%d, want4", got.reserved)
		}
		rows, err := tx.Query(ctx, netEscrowReservationPageSQL, []server.Id{f.balanceId})
		if err == nil {
			rows.Close()
			err = rows.Err()
		}
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != "55P03" {
			t.Fatalf("exact baseline did not hit the held table lock: %v", err)
		}
	})
}

func TestNetEscrowAdmissionCacheLegacyMutationAndRollback(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 45*time.Second)
		defer cancel()
		f := seedAdmissionCacheHistory(t, ctx, 3, 20)
		if _, err := cacheTestCreate(ctx, f, nil, false); err != nil {
			t.Fatal(err)
		}
		var before netEscrowSnapshot
		server.Db(ctx, func(c server.PgConn) {
			server.Raise(c.QueryRow(ctx, `SELECT revision,reserved_byte_count FROM transfer_balance_net_escrow_snapshot WHERE balance_id=$1`, f.balanceId).Scan(&before.revision, &before.reserved))
		})
		// Simulates an older writer: it knows no cache, but mandatory triggers
		// advance the durable revision atomically with the escrow amount.
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_escrow SET balance_byte_count=5 WHERE balance_id=$1 AND contract_id=md5($1::uuid::text||'-cache-contract-1')::uuid`, f.balanceId))
		})
		var reads atomic.Int64
		if _, err := cacheTestCreate(ctx, f, &reads, false); err != nil {
			t.Fatal(err)
		}
		if reads.Load() != 1 {
			t.Fatalf("legacy writer did not invalidate cached authority: reads=%d", reads.Load())
		}
		if exact := openEscrowReservedForBalances(ctx, []server.Id{f.balanceId})[f.balanceId].reserved; exact != 9 {
			t.Fatalf("legacy mutation admission reserved%d, want9", exact)
		}
		server.Tx(ctx, func(tx server.PgTx) {
			tag := server.RaisePgResult(tx.Exec(ctx, netEscrowPublishAdmissionCacheSQL, netEscrowAdmissionCacheArgs(map[server.Id]netEscrowSnapshot{f.balanceId: before}, []server.Id{f.balanceId})...))
			if tag.RowsAffected() != 0 {
				t.Fatal("stale publisher overwrote newer cache")
			}
		}, server.TxReadCommitted)
		sentinel := errors.New("rollback cache and financial writes")
		func() {
			defer func() {
				if v := recover(); v != sentinel {
					t.Fatalf("unexpected rollback result %v", v)
				}
			}()
			server.Tx(ctx, func(tx server.PgTx) {
				_, _, err := createTransferEscrowInTx(ctx, tx, f.sourceNetworkId, f.sourceId, f.destinationNetworkId, f.destinationId, f.sourceNetworkId, 1, nil)
				server.Raise(err)
				panic(sentinel)
			}, server.TxReadCommitted, server.OptNoRetry())
		}()
		reads.Store(0)
		if _, err := cacheTestCreate(ctx, f, &reads, false); err != nil {
			t.Fatal(err)
		}
		if reads.Load() != 0 {
			t.Fatal("rollback lost matching prior committed cache")
		}
		if exact := openEscrowReservedForBalances(ctx, []server.Id{f.balanceId})[f.balanceId].reserved; exact != 10 {
			t.Fatalf("rollback replay reserved%d, want10", exact)
		}
	})
}

func lockedAdmissionCacheTestRead(ctx context.Context, f netEscrowOrderingTestFixture) (got netEscrowSnapshot, reads int64) {
	var count atomic.Int64
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(ctx, `SELECT 1 FROM transfer_balance WHERE balance_id=$1 FOR UPDATE`, f.balanceId))
		got = readLockedNetEscrowSnapshots(ctx, admissionCacheTestTx{tx, &count, false}, []server.Id{f.balanceId})[f.balanceId]
	}, server.TxReadCommitted)
	return got, count.Load()
}

func TestNetEscrowAdmissionCacheOutcomeDeleteAndIdentityReuse(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 45*time.Second)
		defer cancel()
		f := seedAdmissionCacheHistory(t, ctx, 3, 20)
		if got, n := lockedAdmissionCacheTestRead(ctx, f); got.reserved != 3 || n != 1 {
			t.Fatal("initial exact cache was not warmed")
		}
		for _, c := range []struct {
			sql  string
			want ByteCount
		}{
			{`UPDATE transfer_escrow SET settled=true WHERE balance_id=$1 AND contract_id=md5($1::uuid::text||'-cache-contract-1')::uuid`, 2},
			{`UPDATE transfer_contract SET outcome='canceled',close_time=now() WHERE contract_id=md5($1::uuid::text||'-cache-contract-2')::uuid`, 1},
			{`DELETE FROM transfer_escrow WHERE balance_id=$1 AND contract_id=md5($1::uuid::text||'-cache-contract-3')::uuid`, 0},
		} {
			server.Tx(ctx, func(tx server.PgTx) { server.RaisePgResult(tx.Exec(ctx, c.sql, f.balanceId)) })
			if got, n := lockedAdmissionCacheTestRead(ctx, f); got.reserved != c.want || n != 1 {
				t.Fatalf("mutation cache amount%d exactreads%d want%d/1", got.reserved, n, c.want)
			}
		}
		// Contract insertion after an orphaned escrow must invalidate a zero cache.
		orphan := server.NewId()
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO transfer_escrow(contract_id,balance_id,balance_byte_count) VALUES($1,$2,7)`, orphan, f.balanceId))
		})
		if got, n := lockedAdmissionCacheTestRead(ctx, f); got.reserved != 0 || n != 1 {
			t.Fatal("orphan insert was not revalidated")
		}
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO transfer_contract(contract_id,source_network_id,source_id,destination_network_id,destination_id,payer_network_id,transfer_byte_count)
   VALUES($1,$2,$3,$4,$5,$2,7)`, orphan, f.sourceNetworkId, f.sourceId, f.destinationNetworkId, f.destinationId))
		})
		if got, n := lockedAdmissionCacheTestRead(ctx, f); got.reserved != 7 || n != 1 {
			t.Fatal("orphan contract activation reused stale zero")
		}
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `DELETE FROM transfer_balance WHERE balance_id=$1`, f.balanceId))
		})
		if got, n := lockedAdmissionCacheTestRead(ctx, f); got.reserved != 0 || got.endTime != nil || n != 1 {
			t.Fatal("deleted balance was not a zero tombstone")
		}
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO transfer_balance(balance_id,network_id,start_time,end_time,start_balance_byte_count,balance_byte_count,net_revenue_nano_cents,subsidy_net_revenue_nano_cents,pro)
   VALUES($1,$2,now()-interval '1 minute',now()+interval '1 hour',23,23,0,0,false)`, f.balanceId, f.sourceNetworkId))
		})
		if got, n := lockedAdmissionCacheTestRead(ctx, f); got.reserved != 7 || got.endTime == nil || n != 1 {
			t.Fatal("balance identity reuse bypassed revision tombstone")
		}
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `DELETE FROM transfer_contract WHERE contract_id=$1`, orphan))
		})
		if got, n := lockedAdmissionCacheTestRead(ctx, f); got.reserved != 0 || n != 1 {
			t.Fatal("contract deletion failed to invalidate cache")
		}
	})
}

type admissionCacheThirdMutationTx struct {
	server.PgTx
	balanceId server.Id
}

func (tx admissionCacheThirdMutationTx) SendBatch(ctx context.Context, b *pgx.Batch) pgx.BatchResults {
	server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_escrow SET balance_byte_count=balance_byte_count+4 WHERE balance_id=$1 AND contract_id=md5($1::uuid::text||'-cache-contract-1')::uuid`, tx.balanceId))
	return tx.PgTx.SendBatch(ctx, b)
}

func TestNetEscrowAdmissionCacheThirdMutationCannotPublishWrongRevision(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		f := seedAdmissionCacheHistory(t, ctx, 3, 20)
		lockedAdmissionCacheTestRead(ctx, f)
		var posts []func() any
		server.Tx(ctx, func(tx server.PgTx) {
			var err error
			_, posts, err = createTransferEscrowInTx(ctx, admissionCacheThirdMutationTx{tx, f.balanceId}, f.sourceNetworkId, f.sourceId, f.destinationNetworkId, f.destinationId, f.sourceNetworkId, 1, nil)
			server.Raise(err)
		}, server.TxReadCommitted)
		// The unexpected third revision means the predicted+2 amount cannot be
		// made authoritative. Its delayed mirror must also take the exact fallback.
		server.RunPosts(ctx, posts...)
		if got, n := lockedAdmissionCacheTestRead(ctx, f); got.reserved != 8 || n != 1 {
			t.Fatalf("third mutation yielded amount%d reads%d, want8/1", got.reserved, n)
		}
		if mirror := Testing_NetEscrowByteCount(ctx, f.balanceId); mirror != 8 {
			t.Fatalf("delayed mirror used stale cache publication: %d", mirror)
		}
	})
}

func TestNetEscrowAdmissionCacheCustomGenericPointPlans(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		f := seedAdmissionCacheHistory(t, ctx, 3, 20)
		lockedAdmissionCacheTestRead(ctx, f)
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `ALTER TABLE transfer_balance_net_escrow_snapshot SET (autovacuum_enabled=false)`))
			server.RaisePgResult(tx.Exec(ctx, `ANALYZE transfer_balance_net_escrow_snapshot`))
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO transfer_balance_net_escrow_snapshot(balance_id,revision,reserved_byte_count)
    SELECT md5('unrelated-cache-'||n)::uuid,0,0 FROM generate_series(1,100000)n`))
			server.RaisePgResult(tx.Exec(ctx, `ANALYZE transfer_balance; ANALYZE transfer_balance_net_escrow_revision`))
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO transfer_balance(balance_id,network_id,start_time,end_time,start_balance_byte_count,balance_byte_count,net_revenue_nano_cents,subsidy_net_revenue_nano_cents,pro)
                SELECT md5('unrelated-cache-'||n)::uuid,$1,now()-interval '1 minute',now()+interval '1 hour',1,1,0,0,false FROM generate_series(1,100000)n`, f.sourceNetworkId))
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO transfer_balance_net_escrow_revision(balance_id,revision)
                SELECT md5('unrelated-cache-'||n)::uuid,1 FROM generate_series(1,100000)n`))
		})
		server.Db(ctx, func(conn server.PgConn) {
			server.RaisePgResult(conn.Exec(ctx, `PREPARE admission_cache_point AS `+netEscrowAdmissionCacheSQL))
			defer conn.Exec(context.WithoutCancel(ctx), `DEALLOCATE admission_cache_point`)
			for _, mode := range []string{"force_custom_plan", "force_generic_plan"} {
				server.RaisePgResult(conn.Exec(ctx, `SET plan_cache_mode=`+mode))
				var raw []byte
				server.Raise(conn.QueryRow(ctx, fmt.Sprintf(`EXPLAIN (ANALYZE,BUFFERS,TIMING OFF,FORMAT JSON) EXECUTE admission_cache_point ('{%s}'::uuid[])`, f.balanceId)).Scan(&raw))
				var plan []map[string]any
				server.Raise(json.Unmarshal(raw, &plan))
				points := 0
				var visit func(map[string]any)
				visit = func(n map[string]any) {
					if relation, ok := n["Relation Name"].(string); ok {
						if relation == "transfer_escrow" || relation == "transfer_contract" {
							t.Fatal("cache hit planned history access")
						}
						cond, _ := n["Index Cond"].(string)
						if !strings.Contains(cond, "balance_id =") || n["Actual Loops"].(float64) != 1 || n["Actual Rows"].(float64) > 1 {
							t.Fatalf("%s cache point plan escaped bound for%s", mode, relation)
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
				if points != 3 {
					t.Fatalf("point lookups%d want3", points)
				}
				t.Logf("%s cache primary-key points=%d history_accesses=0 execution_ms=%v", mode, points, plan[0]["Execution Time"])
			}
		})
	})
}
