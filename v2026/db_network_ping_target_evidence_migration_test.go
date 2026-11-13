package server

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgerrcode"
	"github.com/jackc/pgx/v5/pgconn"
)

// The evidence column (network_ping_target_evidence_schema.go) is an addition
// the running release does not notice, made under a bound on its lock waits.
// Migrations apply while that release still serves, and its per-target tally
// upsert names the columns it writes, so it keeps storing after the migration,
// and every row it creates or adds to reads null: unknown evidence, never zero.
// Behind a transaction that holds the tally, as a long reader would, the
// migration fails at its five-second lock bound rather than queue the ingest
// behind it, and leaves nothing changed. The column is a nullable bigint with
// no default on the partitioned table and on every day partition, a day made
// after the migration included, and running the migration's statement again
// takes no lock and changes nothing.
func TestNetworkPingTargetEvidenceMigrationIsAdditive(t *testing.T) {
	(&TestEnv{ApplyDbMigrations: false, RerunCount: 0}).Run(t, func(t testing.TB) {
		ctx := context.Background()
		version := NetworkPingTargetEvidenceSchemaMigrationVersion()
		if version == 0 {
			t.Fatal("the evidence column's migration is not in the migration list")
		}

		// the per-target tally upsert of the release that predates the column,
		// at the database's current hour, which a day partition covers
		upsertRunningRelease := func(target Id) {
			MaintenanceTx(ctx, func(tx PgTx) {
				RaisePgResult(tx.Exec(
					ctx,
					`
					INSERT INTO network_ping_target_hour_tally (
						hour,
						target_extender_id,
						pinger_kind,
						ping_count,
						rejection_count
					)
					VALUES (date_trunc('hour', now() AT TIME ZONE 'utc'), $1, 2, 3, 1)
					ON CONFLICT (hour, target_extender_id, pinger_kind) DO UPDATE SET
						ping_count = network_ping_target_hour_tally.ping_count + excluded.ping_count,
						rejection_count = network_ping_target_hour_tally.rejection_count + excluded.rejection_count
					`,
					target,
				))
			})
		}
		// the table's relations, the partitioned table and each day, and how
		// many of them carry the column as the monitor's contract pins it
		columnShape := func() (relations int, shaped int) {
			MaintenanceDb(ctx, func(conn PgConn) {
				Raise(conn.QueryRow(
					ctx,
					`
					WITH relation AS (
						SELECT 'public.network_ping_target_hour_tally'::regclass::oid AS oid
						UNION ALL
						SELECT inhrelid FROM pg_inherits
						WHERE inhparent = 'public.network_ping_target_hour_tally'::regclass
					)
					SELECT
						count(*),
						count(*) FILTER (WHERE EXISTS (
							SELECT 1
							FROM pg_attribute AS attribute
							LEFT JOIN pg_attrdef AS attribute_default
								ON attribute_default.adrelid = attribute.attrelid
								AND attribute_default.adnum = attribute.attnum
							WHERE attribute.attrelid = relation.oid
								AND attribute.attname = 'evidence_rejection_count'
								AND NOT attribute.attisdropped
								AND attribute.atttypid = 'bigint'::regtype
								AND NOT attribute.attnotnull
								AND attribute_default.adbin IS NULL
						))
					FROM relation
					`,
				).Scan(&relations, &shaped))
			}, OptReadOnly(), OptNoRetry())
			return relations, shaped
		}
		evidenceCounts := func() (rows int, unknown int) {
			MaintenanceDb(ctx, func(conn PgConn) {
				Raise(conn.QueryRow(
					ctx,
					`
					SELECT count(*), count(*) FILTER (WHERE evidence_rejection_count IS NULL)
					FROM network_ping_target_hour_tally
					`,
				).Scan(&rows, &unknown))
			}, OptReadOnly(), OptNoRetry())
			return rows, unknown
		}
		// holds ACCESS SHARE on the tally in another session, as a long
		// reader would, until the returned release is called
		holdTally := func() (release func()) {
			locked := make(chan error, 1)
			released := make(chan struct{})
			done := make(chan struct{})
			go func() {
				defer close(done)
				defer func() {
					if r := recover(); r != nil {
						select {
						case locked <- fmt.Errorf("hold the tally: %v", r):
						default:
						}
					}
				}()
				MaintenanceDb(ctx, func(conn PgConn) {
					tx, err := conn.Begin(ctx)
					if err != nil {
						locked <- err
						return
					}
					defer tx.Rollback(context.Background())
					if _, err := tx.Exec(ctx, `LOCK TABLE network_ping_target_hour_tally IN ACCESS SHARE MODE`); err != nil {
						locked <- err
						return
					}
					locked <- nil
					<-released
				}, OptNoRetry())
			}()
			if err := <-locked; err != nil {
				close(released)
				<-done
				t.Fatal(err)
			}
			return func() {
				close(released)
				<-done
			}
		}
		// the migration's statement in a transaction of its own, as the
		// migration runs it, once: its error and how long it took
		runStatement := func() (statementErr error, elapsed time.Duration) {
			start := time.Now()
			MaintenanceDb(ctx, func(conn PgConn) {
				tx, err := conn.Begin(ctx)
				if err != nil {
					statementErr = err
					return
				}
				defer tx.Rollback(context.Background())
				if _, err := tx.Exec(ctx, networkPingTargetEvidenceSchemaSql); err != nil {
					statementErr = err
					return
				}
				statementErr = tx.Commit(ctx)
			}, OptNoRetry())
			return statementErr, time.Since(start)
		}

		ApplyDbMigrationsUpTo(ctx, version-1)
		relations, shaped := columnShape()
		if relations < 2 || shaped != 0 {
			t.Fatalf("before its migration: %d relations, %d with the evidence column; want days and none", relations, shaped)
		}
		before := NewId()
		upsertRunningRelease(before)

		// behind a held tally the migration gives up at its bound
		release := holdTally()
		statementErr, elapsed := runStatement()
		release()
		var pgErr *pgconn.PgError
		if !errors.As(statementErr, &pgErr) || pgErr.Code != pgerrcode.LockNotAvailable {
			t.Fatalf("behind a held tally the migration returned %v after %s, want a lock timeout", statementErr, elapsed)
		}
		if elapsed < 4*time.Second || 30*time.Second < elapsed {
			t.Fatalf("behind a held tally the migration gave up after %s, want its five-second bound", elapsed)
		}
		if _, shaped := columnShape(); shaped != 0 {
			t.Fatalf("a migration that timed out left the column on %d relations", shaped)
		}

		ApplyDbMigrationsUpTo(ctx, version)
		relations, shaped = columnShape()
		if shaped != relations {
			t.Fatalf("after its migration: %d of %d relations carry a nullable bigint evidence column without a default", shaped, relations)
		}
		// the running release stores on: one more increment of the row it made
		// before the migration, and a row of its own
		upsertRunningRelease(before)
		upsertRunningRelease(NewId())
		if rows, unknown := evidenceCounts(); rows != 2 || unknown != 2 {
			t.Fatalf("rows = %d, of them unknown = %d; want two rows, both unknown", rows, unknown)
		}

		// the statement again, behind a held tally: it reads the catalog,
		// takes no lock and changes nothing
		release = holdTally()
		statementErr, elapsed = runStatement()
		release()
		if statementErr != nil || 4*time.Second <= elapsed {
			t.Fatalf("run again behind a held tally: %v after %s, want no wait", statementErr, elapsed)
		}
		if againRelations, againShaped := columnShape(); againRelations != relations || againShaped != relations {
			t.Fatalf("run again: %d of %d relations carry the column, want %d of %d", againShaped, againRelations, relations, relations)
		}

		// a day made after the migration, as the sweep makes one, has the column
		MaintenanceTx(ctx, func(tx PgTx) {
			RaisePgResult(tx.Exec(
				ctx,
				`
				DO $day$
				DECLARE
					day timestamp := date_trunc('day', now() AT TIME ZONE 'utc') + interval '10 days';
				BEGIN
					EXECUTE format(
						'CREATE TABLE %I PARTITION OF network_ping_target_hour_tally FOR VALUES FROM (%L) TO (%L)',
						'network_ping_target_hour_tally_p' || to_char(day, 'YYYYMMDD'),
						to_char(day, 'YYYY-MM-DD'),
						to_char(day + interval '1 day', 'YYYY-MM-DD')
					);
				END
				$day$
				`,
			))
		})
		if dayRelations, dayShaped := columnShape(); dayRelations != relations+1 || dayShaped != dayRelations {
			t.Fatalf("with a new day: %d of %d relations carry the column, want %d of %d", dayShaped, dayRelations, relations+1, relations+1)
		}

		// and the head still applies from here
		ApplyDbMigrations(ctx)
	})
}
