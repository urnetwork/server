package server

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

const migration810TestIdentity = "6a622e890406cd7984ad51a0a5dc964c7b7f1c912ecf3cef7d3eb04a00df5cd7"
const migration810TestDefinition = "CREATE INDEX transfer_contract_audit_closed_null_day ON public.transfer_contract USING btree (close_time, contract_id) WHERE ((outcome IS NULL) AND (close_time IS NOT NULL))"

// This is the published 809 ledger plus the minimal table needed by 810. Only
// the actual normal migrator may produce the next success/identity record.
func migration810ReplayTestFixture(t testing.TB, ctx context.Context) *OnlineSqlMigration {
	t.Helper()
	migration, ok := migrations[809].(*OnlineSqlMigration)
	if !ok {
		t.Fatal("migration810 lost its online schema-audit type")
	}
	identity, err := MigrationIdentity(809)
	Raise(err)
	if identity != migration810TestIdentity {
		t.Fatal("published migration810 production/recovery/audit bytes changed", identity)
	}
	if DbVersion(ctx) != 0 {
		t.Fatal("replay fixture must start with an empty ledger")
	}
	indices, identities := []int32{}, []string{}
	for index := range 809 {
		identity, err := MigrationIdentity(index)
		Raise(err)
		indices, identities = append(indices, int32(index)), append(identities, identity)
	}
	MaintenanceTx(ctx, func(tx PgTx) {
		RaisePgResult(tx.Exec(ctx, `CREATE TABLE transfer_contract (
			contract_id uuid PRIMARY KEY, close_time timestamp, outcome varchar(32));
			INSERT INTO transfer_contract SELECT md5('migration810-'||n)::uuid,
			'2026-01-02'::timestamp,NULL FROM generate_series(1,32) n`))
		RaisePgResult(tx.Exec(ctx, migrationCatalogSchemaSQL))
		RaisePgResult(tx.Exec(ctx, `INSERT INTO migration_catalog(migration_index,identity_sha256)
			SELECT * FROM unnest($1::integer[],$2::text[])`, indices, identities))
		RaisePgResult(tx.Exec(ctx, `INSERT INTO migration_audit(start_version_number,end_version_number,status)
			VALUES(0,809,'success')`))
	})
	return migration
}

// Read actual durable shape and identity; no name-only readiness evidence.
func migration810ReplayTestIndex(t testing.TB, ctx context.Context, wantValid bool) uint32 {
	t.Helper()
	var oid uint32
	MaintenanceDb(ctx, func(conn PgConn) {
		var definition string
		var valid, ready, live bool
		Raise(conn.QueryRow(ctx, `SELECT catalog.indexrelid,pg_get_indexdef(catalog.indexrelid),
			catalog.indisvalid,catalog.indisready,catalog.indislive
			FROM pg_index AS catalog WHERE catalog.indexrelid=to_regclass('public.transfer_contract_audit_closed_null_day')
			AND catalog.indrelid='public.transfer_contract'::regclass`).Scan(&oid, &definition, &valid, &ready, &live))
		if definition != migration810TestDefinition || valid != wantValid || !ready || !live {
			t.Fatalf("index oid=%d definition=%q valid/ready/live=%t/%t/%t", oid, definition, valid, ready, live)
		}
	}, OptReadOnly(), OptNoRetry())
	return oid
}

// Success, identity, source rows and released ownership must agree durably.
func migration810ReplayTestLedger(t testing.TB, ctx context.Context, version int) {
	t.Helper()
	if actual := DbVersion(ctx); actual != version {
		t.Fatalf("ledger head=%d, want%d", actual, version)
	}
	MaintenanceDb(ctx, func(conn PgConn) {
		var count, rows int
		var identity string
		Raise(conn.QueryRow(ctx, `SELECT count(*),COALESCE(max(identity_sha256) FILTER(WHERE migration_index=809),'') FROM migration_catalog`).Scan(&count, &identity))
		Raise(conn.QueryRow(ctx, `SELECT count(*) FROM transfer_contract WHERE close_time='2026-01-02'::timestamp AND outcome IS NULL`).Scan(&rows))
		if count != version || (version == 810 && identity != migration810TestIdentity) || (version == 809 && identity != "") || rows != 32 {
			t.Fatalf("ledger count=%d identity=%q retained rows=%d", count, identity, rows)
		}
		var locks int
		Raise(conn.QueryRow(ctx, `SELECT count(*) FROM pg_locks WHERE locktype='advisory'
			AND classid=$1::oid AND objid=810 AND objsubid=2 AND granted`, int64(0x4d494752)).Scan(&locks))
		if locks != 0 {
			t.Fatal("migration recovery admission remained held after return", locks)
		}
	}, OptReadOnly(), OptNoRetry())
}

// RED on the old runner: a correct already-built index receives a new OID.
// GREEN must retain that exact artifact and use the normal ledger transaction.
func TestMigration810CompletedIndexRetainsOidAndRecordsLedger(t *testing.T) {
	(&TestEnv{ApplyDbMigrations: false}).Run(t, func(t testing.TB) {
		ctx := t.Context()
		migration := migration810ReplayTestFixture(t, ctx)
		MaintenanceTx(ctx, func(tx PgTx) { RaisePgResult(tx.Exec(ctx, migration.auditSql)) })
		before := migration810ReplayTestIndex(t, ctx, true)
		ApplyDbMigrationsUpTo(ctx, 810)
		if after := migration810ReplayTestIndex(t, ctx, true); before != after {
			t.Fatalf("completed index rebuilt before ledger completion: oid%d became%d", before, after)
		}
		migration810ReplayTestLedger(t, ctx, 810)
		ApplyDbMigrationsUpTo(ctx, 810)
		if after := migration810ReplayTestIndex(t, ctx, true); before != after {
			t.Fatal("completed migration replay changed index identity")
		}
	})
}

// Same-name usable mistakes and a failed concurrent uniqueness build must
// recover through the registered DROP/CREATE CONCURRENTLY statements.
func TestMigration810AbsentWrongAndInvalidIndexRepair(t *testing.T) {
	for _, kind := range []string{"absent", "wrong-columns", "wrong-predicate", "wrong-table", "invalid"} {
		t.Run(kind, func(t *testing.T) {
			(&TestEnv{ApplyDbMigrations: false}).Run(t, func(t testing.TB) {
				ctx := t.Context()
				migration810ReplayTestFixture(t, ctx)
				var before uint32
				MaintenanceDb(ctx, func(conn PgConn) {
					var sql string
					switch kind {
					case "wrong-columns":
						sql = `CREATE INDEX transfer_contract_audit_closed_null_day ON transfer_contract(contract_id)`
					case "wrong-predicate":
						sql = `CREATE INDEX transfer_contract_audit_closed_null_day ON transfer_contract(close_time,contract_id) WHERE outcome IS NULL`
					case "wrong-table":
						sql = `CREATE TABLE other_contract (contract_id uuid,close_time timestamp,outcome varchar(32)); CREATE INDEX transfer_contract_audit_closed_null_day ON other_contract(close_time,contract_id) WHERE outcome IS NULL AND close_time IS NOT NULL`
					case "invalid":
						if _, err := conn.Exec(ctx, `CREATE UNIQUE INDEX CONCURRENTLY transfer_contract_audit_closed_null_day ON transfer_contract(close_time)`); err == nil {
							t.Fatal("concurrent uniqueness control did not fail")
						}
						var valid bool
						Raise(conn.QueryRow(ctx, `SELECT indisvalid FROM pg_index WHERE indexrelid='transfer_contract_audit_closed_null_day'::regclass`).Scan(&valid))
						if valid {
							t.Fatal("failed concurrent build lacks real invalid residue")
						}
					}
					if sql != "" {
						RaisePgResult(conn.Exec(ctx, sql))
					}
					Raise(conn.QueryRow(ctx, `SELECT COALESCE(to_regclass('public.transfer_contract_audit_closed_null_day')::oid,0)`).Scan(&before))
				}, OptReadWrite(), OptNoRetry())
				ApplyDbMigrationsUpTo(ctx, 810)
				if after := migration810ReplayTestIndex(t, ctx, true); before == after {
					t.Fatal("wrong/absent/invalid artifact was admitted unchanged", kind, after)
				}
				migration810ReplayTestLedger(t, ctx, 810)
			})
		})
	}
}

// An independent repeatable-read actor pins the builder's old-snapshot phase.
// The coordinator never acquires a second connection inside its own callback.
func migration810HoldOldSnapshot(t testing.TB, ctx context.Context) func() {
	t.Helper()
	ready, release := make(chan struct{}), make(chan struct{})
	holder := startOwnedTransactionTest(func() {
		MaintenanceDb(ctx, func(conn PgConn) {
			tx, err := conn.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
			Raise(err)
			defer tx.Rollback(context.WithoutCancel(ctx))
			var count int
			Raise(tx.QueryRow(ctx, `SELECT count(*) FROM transfer_contract`).Scan(&count))
			close(ready)
			select {
			case <-release:
			case <-ctx.Done():
			}
		}, OptReadOnly(), OptNoRetry())
	})
	stop := sync.OnceFunc(func() {
		close(release)
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		if caught := holder.join(t, cleanup); caught != nil {
			t.Fatal("snapshot actor failed", caught)
		}
	})
	select {
	case <-ready:
		return stop
	case <-holder.done:
		t.Fatal("snapshot actor failed before readiness", holder.recovered)
	case <-ctx.Done():
		stop()
		t.Fatal("snapshot actor did not become ready", ctx.Err())
	}
	return stop
}

// Native state, rather than a sleep, proves the intended surviving builder.
func migration810WaitBuilder(t testing.TB, ctx context.Context, wantActive bool) {
	t.Helper()
	MaintenanceDb(ctx, func(conn PgConn) {
		ticker := time.NewTicker(10 * time.Millisecond)
		defer ticker.Stop()
		for {
			var active, waiting bool
			Raise(conn.QueryRow(ctx, `SELECT count(*)>0,COALESCE(bool_or(phase='waiting for old snapshots'),false)
				FROM pg_stat_progress_create_index WHERE relid='public.transfer_contract'::regclass`).Scan(&active, &waiting))
			if (wantActive && waiting) || (!wantActive && !active) {
				return
			}
			select {
			case <-ticker.C:
			case <-ctx.Done():
				t.Fatal("builder did not reach expected native state", wantActive, ctx.Err())
			}
		}
	}, OptReadOnly(), OptNoRetry())
}

// Both the old uncoordinated builder and the new migration owner must retain
// custody while active. Cancellation leaves repairable, exact invalid residue.
func TestMigration810ActiveBuilderCustodyAndCanceledRecovery(t *testing.T) {
	for _, legacy := range []bool{true, false} {
		t.Run(fmt.Sprintf("legacy=%t", legacy), func(t *testing.T) {
			(&TestEnv{ApplyDbMigrations: false}).Run(t, func(t testing.TB) {
				ctx, cancel := context.WithTimeout(t.Context(), 45*time.Second)
				defer cancel()
				migration := migration810ReplayTestFixture(t, ctx)
				stopSnapshot := migration810HoldOldSnapshot(t, ctx)
				defer stopSnapshot()
				buildCtx, cancelBuild := context.WithCancel(ctx)
				builder := startOwnedTransactionTest(func() {
					if legacy {
						MaintenanceDb(buildCtx, func(conn PgConn) { RaisePgResult(conn.Exec(buildCtx, migration.sql)) }, OptReadWrite(), OptNoRetry())
					} else {
						ApplyDbMigrationsUpTo(buildCtx, 810)
					}
				})
				defer func() {
					cancelBuild()
					stopSnapshot()
					cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
					defer cancel()
					_ = builder.join(t, cleanup)
				}()
				migration810WaitBuilder(t, ctx, true)
				before := migration810ReplayTestIndex(t, ctx, false)
				probeCtx, cancelProbe := context.WithTimeout(ctx, 2*time.Second)
				caught := captureDbErrorPanic(func() { ApplyDbMigrationsUpTo(probeCtx, 810) })
				cancelProbe()
				if caught == nil || !strings.Contains(fmt.Sprint(caught), "active") {
					t.Fatal("active builder was not explicitly refused before recovery", caught)
				}
				if after := migration810ReplayTestIndex(t, ctx, false); after != before || DbVersion(ctx) != 809 {
					t.Fatal("refused replay changed builder identity or migration head", before, after)
				}
				select {
				case <-builder.done:
					t.Fatal("replay interrupted the builder", builder.recovered)
				default:
				}
				cancelBuild()
				if caught := builder.join(t, ctx); caught == nil {
					t.Fatal("canceled builder reported success")
				}
				stopSnapshot()
				migration810WaitBuilder(t, ctx, false)
				if after := migration810ReplayTestIndex(t, ctx, false); after != before {
					t.Fatal("canceled build lost its expected invalid residue")
				}
				migration810ReplayTestLedger(t, ctx, 809)
				ApplyDbMigrationsUpTo(ctx, 810)
				if after := migration810ReplayTestIndex(t, ctx, true); after == before {
					t.Fatal("abandoned exact invalid index was accepted without repair")
				}
				migration810ReplayTestLedger(t, ctx, 810)
			})
		})
	}
}

// A correct artifact alone never authorizes a canceled caller to record success.
func TestMigration810CanceledOwnerCannotRecordCompletion(t *testing.T) {
	(&TestEnv{ApplyDbMigrations: false}).Run(t, func(t testing.TB) {
		ctx := t.Context()
		migration := migration810ReplayTestFixture(t, ctx)
		MaintenanceTx(ctx, func(tx PgTx) { RaisePgResult(tx.Exec(ctx, migration.auditSql)) })
		before := migration810ReplayTestIndex(t, ctx, true)
		canceled, cancel := context.WithCancel(ctx)
		cancel()
		if caught := captureDbErrorPanic(func() { ApplyDbMigrationsUpTo(canceled, 810) }); caught == nil {
			t.Fatal("canceled migration owner reported completion")
		}
		migration810ReplayTestLedger(t, ctx, 809)
		if after := migration810ReplayTestIndex(t, ctx, true); after != before {
			t.Fatal("canceled owner changed the completed artifact")
		}
	})
}
