package server

import (
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

// Pure: the drop is appended after the key history migrations, bounds its
// lock wait, drops the one constraint only while the catalog still has it,
// and touches nothing else. The operator's statement names the same
// constraint.
func TestClientKeyHistoryRegistrationHashUniqueDropMigration(t *testing.T) {
	version := ClientKeyHistoryRegistrationHashUniqueDropMigrationVersion()
	if !(ClientKeyHistoryRetentionFloorMigrationVersion() < version && version <= MigrationCount()) {
		t.Fatalf("versions: key history floor %d, unique drop %d, count %d", ClientKeyHistoryRetentionFloorMigrationVersion(), version, MigrationCount())
	}
	migration, ok := migrations[version-1].(*SqlMigration)
	if !ok || !strings.HasPrefix(strings.TrimSpace(migration.sql), "SET LOCAL lock_timeout = '5s';") {
		t.Fatal("the unique drop does not bound its lock wait")
	}
	sql := clientKeyHistoryRegistrationHashUniqueDropSql
	if strings.Count(sql, "ALTER TABLE") != 1 || strings.Count(sql, "DROP CONSTRAINT st_client_key_history_registration_hash_key") != 1 {
		t.Fatalf("the unique drop changes more than the one constraint: %q", sql)
	}
	// the ALTER sits inside the catalog check, so a database whose constraint
	// is gone takes no table lock
	check := strings.Index(sql, "IF EXISTS (")
	alter := strings.Index(sql, "ALTER TABLE")
	end := strings.Index(sql, "END IF;")
	if !(0 < check && check < alter && alter < end) || !strings.Contains(sql[check:alter], "conname = 'st_client_key_history_registration_hash_key'") {
		t.Fatalf("the unique drop does not check the catalog before its ALTER: %q", sql)
	}
	for _, statement := range []string{"DROP INDEX", "DROP TABLE", "CREATE ", "UPDATE ", "DELETE FROM", "INSERT INTO", "TRUNCATE"} {
		if strings.Contains(sql, statement) {
			t.Fatalf("the unique drop runs %q", statement)
		}
	}
	if !strings.Contains(ClientKeyHistoryRegistrationHashUniqueDropOperatorSql, "DROP CONSTRAINT IF EXISTS st_client_key_history_registration_hash_key") ||
		!strings.Contains(ClientKeyHistoryRegistrationHashUniqueDropOperatorSql, "ALTER TABLE public.st_client_key_history") {
		t.Fatalf("the operator statement is not the same drop: %q", ClientKeyHistoryRegistrationHashUniqueDropOperatorSql)
	}
}

// DB-backed. Before the drop the unique constraint refuses a second row with
// another key and the same registration hash; the operator's statement drops
// the constraint and its index; the primary key still refuses a repeated key;
// the migration then records the drop as a catalog read, completing while
// another transaction holds a lock on the table, where taking the table's
// ACCESS EXCLUSIVE lock would wait out its lock timeout and fail.
func TestClientKeyHistoryRegistrationHashUniqueDropMigrationDropsOnlyThatConstraint(t *testing.T) {
	(&TestEnv{ApplyDbMigrations: false}).Run(t, func(t testing.TB) {
		ctx := t.Context()
		version := ClientKeyHistoryRegistrationHashUniqueDropMigrationVersion()
		ApplyDbMigrationsUpTo(ctx, version-1)
		domain := []byte(strings.Repeat("d", 32))
		insert := func(clientId Id, generation int, hashFill string) (err error) {
			HandleError(func() {
				MaintenanceDb(ctx, func(conn PgConn) {
					RaisePgResult(conn.Exec(ctx, `
						INSERT INTO st_client_key_history (client_id, generation, domain_hash, registration_hash, evidence_hash, evidence)
						VALUES ($1, $2, $3, $4, $5, '{}')
					`, clientId, generation, domain, []byte(strings.Repeat(hashFill, 32)), "sha256:"+strings.Repeat("e", 64)))
				}, OptReadWrite(), OptNoRetry())
			}, func(e error) { err = e })
			return
		}
		constraintName := func(err error) string {
			var pgErr *pgconn.PgError
			if errors.As(err, &pgErr) && pgErr.Code == "23505" {
				return pgErr.ConstraintName
			}
			return ""
		}
		present := func() (constraintCount int, indexCount int) {
			MaintenanceDb(ctx, func(conn PgConn) {
				Raise(conn.QueryRow(ctx, `
					SELECT
						(SELECT count(*) FROM pg_constraint WHERE conrelid = 'public.st_client_key_history'::regclass AND conname = 'st_client_key_history_registration_hash_key'),
						(SELECT count(*) FROM pg_class WHERE relname = 'st_client_key_history_registration_hash_key' AND relkind = 'i')
				`).Scan(&constraintCount, &indexCount))
			})
			return
		}

		firstId, secondId := NewId(), NewId()
		if err := insert(firstId, 1, "a"); err != nil {
			t.Fatal(err)
		}
		if name := constraintName(insert(secondId, 1, "a")); name != "st_client_key_history_registration_hash_key" {
			t.Fatalf("before the drop a repeated hash was refused by %q, want the unique constraint", name)
		}

		MaintenanceDb(ctx, func(conn PgConn) {
			RaisePgResult(conn.Exec(ctx, ClientKeyHistoryRegistrationHashUniqueDropOperatorSql))
		}, OptReadWrite(), OptNoRetry())
		if constraintCount, indexCount := present(); constraintCount != 0 || indexCount != 0 {
			t.Fatalf("after the operator's drop: %d constraints, %d indexes", constraintCount, indexCount)
		}
		if err := insert(secondId, 1, "a"); err != nil {
			t.Fatalf("after the drop a new key with a repeated hash was refused: %v", err)
		}
		if name := constraintName(insert(firstId, 1, "b")); name != "st_client_key_history_pkey" {
			t.Fatalf("after the drop a repeated key was refused by %q, want the primary key", name)
		}

		// a reader holds the table while the migration records the drop
		locked := make(chan struct{})
		release := make(chan struct{})
		holderDone := make(chan error, 1)
		go func() {
			var holderErr error
			HandleError(func() {
				MaintenanceTx(ctx, func(tx PgTx) {
					RaisePgResult(tx.Exec(ctx, `LOCK TABLE st_client_key_history IN ACCESS SHARE MODE`))
					close(locked)
					<-release
				}, OptNoRetry())
			}, func(e error) { holderErr = e })
			holderDone <- holderErr
		}()
		<-locked
		var migrateErr error
		HandleError(func() {
			ApplyDbMigrationsUpTo(ctx, version)
		}, func(e error) { migrateErr = e })
		close(release)
		if holderErr := <-holderDone; holderErr != nil {
			t.Fatal(holderErr)
		}
		if migrateErr != nil {
			t.Fatalf("recording the drop waited for the table: %v", migrateErr)
		}
		if DbVersion(ctx) != version {
			t.Fatalf("version %d after recording the drop, want %d", DbVersion(ctx), version)
		}
		ApplyDbMigrations(ctx)
		if DbVersion(ctx) != MigrationCount() {
			t.Fatal("the migrations after the unique drop did not apply")
		}
	})
}

// DB-backed. On a database that never had the operator's drop, such as a new
// one, the migration drops the constraint itself.
func TestClientKeyHistoryRegistrationHashUniqueDropMigrationDropsTheConstraintItself(t *testing.T) {
	(&TestEnv{ApplyDbMigrations: false}).Run(t, func(t testing.TB) {
		ctx := t.Context()
		version := ClientKeyHistoryRegistrationHashUniqueDropMigrationVersion()
		ApplyDbMigrationsUpTo(ctx, version-1)
		var before int
		MaintenanceDb(ctx, func(conn PgConn) {
			Raise(conn.QueryRow(ctx, `SELECT count(*) FROM pg_constraint WHERE conrelid = 'public.st_client_key_history'::regclass AND conname = 'st_client_key_history_registration_hash_key'`).Scan(&before))
		})
		if before != 1 {
			t.Fatalf("before the migration %d unique constraints, want 1", before)
		}
		ApplyDbMigrationsUpTo(ctx, version)
		var after int
		MaintenanceDb(ctx, func(conn PgConn) {
			Raise(conn.QueryRow(ctx, `SELECT count(*) FROM pg_constraint WHERE conrelid = 'public.st_client_key_history'::regclass AND conname = 'st_client_key_history_registration_hash_key'`).Scan(&after))
		})
		if after != 0 {
			t.Fatalf("the migration left %d unique constraints", after)
		}
	})
}
