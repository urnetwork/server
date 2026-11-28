// Migration replay tests retain completed index artifacts and prove that active
// builders keep custody until every independently owned test actor has exited.
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

// An absent artifact is created through the registered concurrent DDL.
func TestMigration810AbsentIndexRepair(t *testing.T) {
	migration810TestIndexRepair(t, "absent")
}

// A usable same-name index with incorrect columns must be replaced.
func TestMigration810WrongColumnsIndexRepair(t *testing.T) {
	migration810TestIndexRepair(t, "wrong-columns")
}

// A weaker same-name predicate cannot satisfy the published migration.
func TestMigration810WrongPredicateIndexRepair(t *testing.T) {
	migration810TestIndexRepair(t, "wrong-predicate")
}

// A same-name index on a different parent table must not count as completion.
func TestMigration810WrongTableIndexRepair(t *testing.T) {
	migration810TestIndexRepair(t, "wrong-table")
}

// A real failed concurrent uniqueness build leaves residue requiring recovery.
func TestMigration810InvalidIndexRepair(t *testing.T) {
	migration810TestIndexRepair(t, "invalid")
}

// Incorrect or missing artifacts use the registered recovery statements and
// the normal ledger completion transaction without changing source rows.
func migration810TestIndexRepair(t *testing.T, kind string) {
	t.Helper()
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
}

// An independent repeatable-read actor pins the builder's old-snapshot phase.
// The coordinator never acquires a second connection inside its own callback.
func migration810HoldOldSnapshot(t testing.TB, ctx context.Context) (int32, func()) {
	t.Helper()
	holdCtx, cancelHold := context.WithCancel(ctx)
	ready, release := make(chan struct{}), make(chan struct{})
	var holderPid int32
	holder := startOwnedTransactionTest(func() {
		MaintenanceDb(holdCtx, func(conn PgConn) {
			tx, err := conn.BeginTx(holdCtx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
			Raise(err)
			defer tx.Rollback(context.WithoutCancel(holdCtx))
			var count int
			Raise(tx.QueryRow(holdCtx, `SELECT count(*) FROM transfer_contract`).Scan(&count))
			holderPid = int32(conn.Conn().PgConn().PID())
			close(ready)
			select {
			case <-release:
			case <-holdCtx.Done():
			}
		}, OptReadOnly(), OptNoRetry())
	})
	stop := sync.OnceFunc(func() {
		close(release)
		cancelHold()
		<-holder.done
		if holder.recovered != nil {
			t.Error("snapshot actor failed", holder.recovered)
		}
	})
	select {
	case <-ready:
		return holderPid, stop
	case <-holder.done:
		stop()
		t.Fatal("snapshot actor failed before readiness", holder.recovered)
	case <-ctx.Done():
		stop()
		t.Fatal("snapshot actor did not become ready", ctx.Err())
	}
	return holderPid, stop
}

// A builder generation is tied to its exact backend, query and index artifact.
// The test keeps its independently owned old snapshot until that builder exits.
type migration810BuilderTestState struct {
	pid          int32
	backendStart time.Time
	queryStart   time.Time
	indexOid     uint32
}

// A real virtual-xid wait on our held snapshot, not a progress phase label
// alone, proves custody of the still-invalid concurrent build.
func migration810WaitBuilderBlocked(t testing.TB, ctx context.Context, holderPid int32) migration810BuilderTestState {
	t.Helper()
	var state migration810BuilderTestState
	MaintenanceDb(ctx, func(conn PgConn) {
		ticker := time.NewTicker(10 * time.Millisecond)
		defer ticker.Stop()
		for {
			err := conn.QueryRow(ctx, `SELECT progress.pid,builder.backend_start,builder.query_start,progress.index_relid
				FROM pg_stat_progress_create_index AS progress
				JOIN pg_stat_activity AS builder ON builder.pid=progress.pid
				JOIN pg_stat_activity AS holder ON holder.pid=$1
				WHERE progress.relid=to_regclass('public.transfer_contract')
				AND progress.index_relid=to_regclass('public.transfer_contract_audit_closed_null_day')
				AND progress.phase='waiting for old snapshots' AND progress.current_locker_pid=$1
				AND $1=ANY(pg_blocking_pids(progress.pid))
				AND builder.state='active' AND builder.wait_event_type='Lock' AND builder.wait_event='virtualxid'
				AND holder.state='idle in transaction' AND holder.backend_xmin IS NOT NULL`, holderPid).
				Scan(&state.pid, &state.backendStart, &state.queryStart, &state.indexOid)
			if err == nil {
				return
			}
			if err != pgx.ErrNoRows {
				Raise(err)
			}
			select {
			case <-ticker.C:
			case <-ctx.Done():
				t.Fatal("builder did not block on its owned old snapshot", ctx.Err())
			}
		}
	}, OptReadOnly(), OptNoRetry())
	return state
}

// Client cancellation can return before PostgreSQL processes its cancellation
// or socket closure. Keep the old snapshot held until the exact server build
// disappears, so releasing the fixture cannot let that build finish instead.
func migration810WaitBuilderStopped(t testing.TB, ctx context.Context, state migration810BuilderTestState) {
	t.Helper()
	MaintenanceDb(ctx, func(conn PgConn) {
		ticker := time.NewTicker(10 * time.Millisecond)
		defer ticker.Stop()
		for {
			var stopped bool
			Raise(conn.QueryRow(ctx, `SELECT
				NOT EXISTS(SELECT 1 FROM pg_stat_progress_create_index WHERE pid=$1 AND index_relid=$2)
				AND NOT EXISTS(SELECT 1 FROM pg_stat_activity WHERE pid=$1 AND backend_start=$3
					AND query_start=$4 AND state='active')`, state.pid, state.indexOid, state.backendStart, state.queryStart).Scan(&stopped))
			if stopped {
				return
			}
			select {
			case <-ticker.C:
			case <-ctx.Done():
				t.Fatal("canceled server builder did not exit while its old snapshot remained held", ctx.Err())
			}
		}
	}, OptReadOnly(), OptNoRetry())
}

// A surviving legacy builder retains custody until it is canceled and joined.
func TestMigration810LegacyBuilderCustodyAndCanceledRecovery(t *testing.T) {
	migration810TestActiveBuilderCustody(t, true)
}

// An enrolled recovery owner excludes a second migrator throughout the build.
func TestMigration810OwnedBuilderCustodyAndCanceledRecovery(t *testing.T) {
	migration810TestActiveBuilderCustody(t, false)
}

// Both old and enrolled builders leave an exact invalid artifact after owned
// cancellation. Only after they exit may the normal migrator repair it.
func migration810TestActiveBuilderCustody(t *testing.T, legacy bool) {
	t.Helper()
	(&TestEnv{ApplyDbMigrations: false}).Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 45*time.Second)
		defer cancel()
		migration := migration810ReplayTestFixture(t, ctx)
		holderPid, stopSnapshot := migration810HoldOldSnapshot(t, ctx)
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
			// Cleanup must join both actors before the disposable database is
			// torn down, including assertion failure and cancellation paths.
			<-builder.done
		}()
		state := migration810WaitBuilderBlocked(t, ctx, holderPid)
		t.Logf("before refused replay: owned snapshot blocks builder pid=%d index=%d", state.pid, state.indexOid)
		before := migration810ReplayTestIndex(t, ctx, false)
		probeCtx, cancelProbe := context.WithTimeout(ctx, 2*time.Second)
		caught := captureDbErrorPanic(func() { ApplyDbMigrationsUpTo(probeCtx, 810) })
		cancelProbe()
		if caught == nil || !strings.Contains(fmt.Sprint(caught), "active") {
			t.Fatal("active builder was not explicitly refused before recovery", caught)
		}
		t.Log("after refused replay: checking retained invalid artifact")
		if after := migration810ReplayTestIndex(t, ctx, false); after != before || DbVersion(ctx) != 809 {
			t.Fatal("refused replay changed builder identity or migration head", before, after)
		}
		select {
		case <-builder.done:
			t.Fatal("replay interrupted the builder", builder.recovered)
		default:
		}
		stillBlocked := migration810WaitBuilderBlocked(t, ctx, holderPid)
		if stillBlocked.pid != state.pid || stillBlocked.indexOid != state.indexOid ||
			!stillBlocked.backendStart.Equal(state.backendStart) || !stillBlocked.queryStart.Equal(state.queryStart) {
			t.Fatal("refused replay changed the blocked builder generation", state, stillBlocked)
		}
		cancelBuild()
		if caught := builder.join(t, ctx); caught == nil {
			t.Fatal("canceled builder reported success")
		}
		migration810WaitBuilderStopped(t, ctx, state)
		t.Log("after server cancellation, before snapshot release: checking retained invalid artifact")
		if after := migration810ReplayTestIndex(t, ctx, false); after != before {
			t.Fatal("canceled build lost its expected invalid residue")
		}
		stopSnapshot()
		migration810ReplayTestLedger(t, ctx, 809)
		ApplyDbMigrationsUpTo(ctx, 810)
		if after := migration810ReplayTestIndex(t, ctx, true); after == before {
			t.Fatal("abandoned exact invalid index was accepted without repair")
		}
		migration810ReplayTestLedger(t, ctx, 810)
	})
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
