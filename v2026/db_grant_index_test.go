package server

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

func grantAllocationIndexMigration(t testing.TB) *OnlineSqlMigration {
	t.Helper()
	index := migrationIndex(t, "transfer_balance_active_network_end_start_id")
	if index != 738 {
		t.Fatalf("grant index migration index=%d, want append-only index738", index)
	}
	migration, ok := migrations[index].(*OnlineSqlMigration)
	if !ok {
		t.Fatalf("grant index migration is %T, want online SQL", migrations[index])
	}
	return migration
}

func TestGrantAllocationIndexMigrationReplaysAgainstPostgres(t *testing.T) {
	(&TestEnv{ApplyDbMigrations: false}).Run(t, func(t testing.TB) {
		ctx := t.Context()
		migration := grantAllocationIndexMigration(t)
		MaintenanceDb(ctx, func(conn PgConn) {
			RaisePgResult(conn.Exec(ctx, `CREATE TABLE transfer_balance (
				balance_id uuid PRIMARY KEY, network_id uuid NOT NULL,
				start_time timestamp NOT NULL, end_time timestamp NOT NULL,
				active boolean NOT NULL
			); CREATE INDEX transfer_balance_active_network_end_start_id
			ON transfer_balance(network_id);`))
			assertIndex := func() {
				t.Helper()
				var definition string
				var valid, ready bool
				Raise(conn.QueryRow(ctx, `SELECT pg_get_indexdef(indexrelid),indisvalid,indisready
					FROM pg_index WHERE indexrelid='transfer_balance_active_network_end_start_id'::regclass`).Scan(&definition, &valid, &ready))
				if definition != "CREATE INDEX transfer_balance_active_network_end_start_id ON public.transfer_balance USING btree (network_id, end_time, start_time, balance_id) WHERE active" || !valid || !ready {
					t.Fatalf("grant index definition=%s valid=%t ready=%t", definition, valid, ready)
				}
			}
			for _, stage := range []string{"wrong-shape residue", "post-create pre-audit replay"} {
				steps := []string{}
				err := executeOnlineSqlMigration(ctx, migration, func(ctx context.Context, sql string) error {
					steps = append(steps, strings.Join(strings.Fields(sql), " "))
					_, err := conn.Exec(ctx, sql)
					return err
				})
				if err != nil {
					t.Fatalf("%s: %v", stage, err)
				}
				if len(steps) != 2 || !strings.HasPrefix(steps[0], "DROP INDEX CONCURRENTLY IF EXISTS ") || !strings.HasPrefix(steps[1], "CREATE INDEX CONCURRENTLY ") {
					t.Fatalf("%s did not use separate online recovery/create statements: %v", stage, steps)
				}
				assertIndex()
			}
			tx, err := conn.Begin(ctx)
			Raise(err)
			defer tx.Rollback(context.WithoutCancel(ctx))
			RaisePgResult(tx.Exec(ctx, migration.auditSql))
			Raise(tx.Commit(ctx))
			assertIndex()
		}, OptReadWrite(), OptNoRetry())
	})
}

// Counts actual buffer accesses for a controlled retained-snapshot workload.
// The SQL uses the migration's real table/index names and audit definition.
// No elapsed-time threshold or main-specific optimizer estimate is asserted.
func TestGrantAllocationIndexBoundsRetainedSnapshotPageWork(t *testing.T) {
	(&TestEnv{ApplyDbMigrations: false}).Run(t, func(t testing.TB) {
		ctx := t.Context()
		payer, otherPayer := NewId(), NewId()
		now := NowUtc()
		migration := grantAllocationIndexMigration(t)
		MaintenanceDb(ctx, func(writer PgConn) {
			RaisePgResult(writer.Exec(ctx, `CREATE TABLE transfer_balance (
				balance_id uuid PRIMARY KEY, network_id uuid NOT NULL,
				start_time timestamp NOT NULL, end_time timestamp NOT NULL,
				balance_byte_count bigint NOT NULL, paid boolean NOT NULL,
				active boolean GENERATED ALWAYS AS (balance_byte_count > 0) STORED
			); CREATE INDEX transfer_balance_active_network_id_start_end_time
			ON transfer_balance(active,network_id,start_time,end_time);`))
			RaisePgResult(writer.Exec(ctx, `INSERT INTO transfer_balance
				(balance_id,network_id,start_time,end_time,balance_byte_count,paid)
				SELECT md5('synthetic-grant-'||i)::uuid,
				 CASE WHEN i<=512 THEN $1::uuid ELSE $2::uuid END,
				 $3::timestamp-interval '1 hour', $3::timestamp+interval '1 hour'+i*interval '1 second',
				 1048576,i%2=0 FROM generate_series(1,4608) AS i`, payer, otherPayer, now))
			RaisePgResult(writer.Exec(ctx, `ANALYZE transfer_balance`))
			MaintenanceDb(ctx, func(reader PgConn) {
				old, err := reader.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
				Raise(err)
				defer old.Rollback(context.WithoutCancel(ctx))
				var count int
				Raise(old.QueryRow(ctx, `SELECT count(*) FROM transfer_balance WHERE network_id=$1`, payer).Scan(&count))
				if count != 512 {
					t.Fatal("retained snapshot lacks intended payer population")
				}
				for range 16 {
					RaisePgResult(writer.Exec(ctx, `UPDATE transfer_balance SET
					 end_time=end_time+interval '1 microsecond',balance_byte_count=balance_byte_count-1
					 WHERE network_id=$1`, payer))
				}
				RaisePgResult(writer.Exec(ctx, `SET enable_seqscan=off; SET enable_bitmapscan=off`))
				defer writer.Exec(context.WithoutCancel(ctx), `RESET enable_seqscan; RESET enable_bitmapscan`)
				base := `SELECT balance_id,paid,balance_byte_count,start_time,end_time FROM transfer_balance
				 WHERE network_id=$1 AND active AND start_time<=$2 AND $2<end_time`
				type observation struct {
					rows, buffers int
					sorted        bool
				}
				observe := func(query string) observation {
					var raw []byte
					Raise(writer.QueryRow(ctx, `EXPLAIN (ANALYZE,BUFFERS,TIMING OFF,FORMAT JSON) `+query, payer, now).Scan(&raw))
					var plans []map[string]any
					Raise(json.Unmarshal(raw, &plans))
					root := plans[0]["Plan"].(map[string]any)
					seen := observation{rows: int(root["Actual Rows"].(float64)), buffers: int(root["Shared Hit Blocks"].(float64) + root["Shared Read Blocks"].(float64))}
					var visit func(map[string]any)
					visit = func(node map[string]any) {
						seen.sorted = seen.sorted || node["Node Type"] == "Sort" || node["Node Type"] == "Incremental Sort"
						if children, ok := node["Plans"].([]any); ok {
							for _, child := range children {
								visit(child.(map[string]any))
							}
						}
					}
					visit(root)
					return seen
				}
				full := observe(base)
				pageSQL := base + ` ORDER BY end_time,start_time,balance_id LIMIT 64`
				unaligned := observe(pageSQL)
				RaisePgResult(writer.Exec(ctx, migration.auditSql))
				aligned := observe(pageSQL)
				fullWithIndex := observe(base)
				if full.rows != 512 || unaligned.rows != 64 || aligned.rows != 64 || !unaligned.sorted || aligned.sorted || aligned.buffers >= full.buffers || aligned.buffers >= unaligned.buffers {
					t.Fatalf("grant index did not bound retained-snapshot page: full=%+v unaligned=%+v aligned=%+v", full, unaligned, aligned)
				}
				var oldestEnd time.Time
				Raise(old.QueryRow(ctx, `SELECT min(end_time) FROM transfer_balance WHERE network_id=$1`, payer).Scan(&oldestEnd))
				if !oldestEnd.Equal(now.Add(time.Hour + time.Second)) {
					t.Fatal("old snapshot not retained throughout plan comparison")
				}
				t.Logf("held snapshot full=%+v unaligned-page=%+v aligned-page=%+v", full, unaligned, aligned)
				Raise(old.Rollback(ctx))
				// Release, ordinary vacuum, and the identical original query
				// isolate reclaimability from paging/index-order benefits.
				RaisePgResult(writer.Exec(ctx, `VACUUM (ANALYZE) transfer_balance`))
				reclaimed := observe(base)
				if reclaimed.rows != 512 || reclaimed.buffers >= fullWithIndex.buffers {
					t.Fatalf("released snapshot vacuum failed to reduce complete-read buffers: before=%+v after=%+v", fullWithIndex, reclaimed)
				}
				t.Logf("same index set: held snapshot complete-read=%+v; released snapshot vacuum complete-read=%+v", fullWithIndex, reclaimed)
			}, OptReadOnly(), OptNoRetry())
		}, OptReadWrite(), OptNoRetry())
	})
}
