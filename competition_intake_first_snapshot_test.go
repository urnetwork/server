// Physical scope writes prevent an older repeatable-read view from retaining
// authority after another reviewer/operator has advanced the shared fence.
package server

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Explicit snapshot and commit barriers force the stale ordering. The test
// invokes the new ledger trigger, not a substituted admission helper.
func TestCompetitionIntakeFirstLedgerFenceRejectsStaleRepeatableRead(t *testing.T) {
	(&TestEnv{ApplyDbMigrations: true, RerunCount: 0}).Run(t, func(tb testing.TB) {
		ctx, cancel := context.WithTimeout(tb.Context(), 30*time.Second)
		defer cancel()
		var actualHead int
		Db(ctx, func(conn PgConn) {
			Raise(conn.QueryRow(ctx, `SELECT COALESCE(max(end_version_number),0) FROM migration_audit WHERE status='success'`).Scan(&actualHead))
		})
		if actualHead != MigrationCount() {
			tb.Fatalf("intake snapshot fixture schema=%d want=%d", actualHead, MigrationCount())
		}
		tb.Logf("intake_snapshot_fixture_schema_head=%d", actualHead)
		if CompetitionIntakeFirstMigrationVersion() == 0 {
			Tx(ctx, func(tx PgTx) { RaisePgResult(tx.Exec(ctx, CompetitionIntakeFirstMigrationSql())) }, OptReadWrite())
		}
		Tx(ctx, func(tx PgTx) {
			RaisePgResult(tx.Exec(ctx, `SELECT competition_finalization_touch_scope('synthetic-intake','synthetic-competition','synthetic-season')`))
		}, OptReadWrite())
		snapshotReady, writerCommitted := make(chan struct{}), make(chan struct{})
		outcome := make(chan error, 1)
		go func() {
			Db(ctx, func(conn PgConn) {
				tx, err := conn.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead})
				if err != nil {
					close(snapshotReady)
					outcome <- err
					return
				}
				defer tx.Rollback(context.Background())
				_, err = tx.Exec(ctx, `CREATE TEMP TABLE competition_round AS SELECT * FROM public.competition_round WITH NO DATA;
 INSERT INTO pg_temp.competition_round(round_id,competition_id,policy_json) VALUES('00000000-0000-0000-0000-000000000001','synthetic-competition','{"automation_policy":{"enabled":true,"environment":"synthetic-intake","season_id":"synthetic-season"}}');
 CREATE TEMP TABLE synthetic_intake_ledger(round_id uuid);
 CREATE TRIGGER synthetic_intake_fence BEFORE INSERT ON synthetic_intake_ledger FOR EACH ROW EXECUTE FUNCTION public.competition_v2_intake_ledger_fence_guard();
 SELECT generation FROM public.competition_finalization_scope_fence WHERE environment='synthetic-intake' AND competition_id='synthetic-competition' AND season_id='synthetic-season';`)
				close(snapshotReady)
				if err != nil {
					outcome <- err
					return
				}
				select {
				case <-writerCommitted:
				case <-ctx.Done():
					outcome <- ctx.Err()
					return
				}
				_, err = tx.Exec(ctx, `INSERT INTO pg_temp.synthetic_intake_ledger VALUES('00000000-0000-0000-0000-000000000001')`)
				outcome <- err
			})
		}()
		select {
		case <-snapshotReady:
		case <-ctx.Done():
			tb.Fatal("stale snapshot fixture did not reach its barrier")
		}
		Tx(ctx, func(tx PgTx) {
			RaisePgResult(tx.Exec(ctx, `SELECT competition_finalization_touch_scope('synthetic-intake','synthetic-competition','synthetic-season')`))
		}, OptReadWrite())
		close(writerCommitted)
		var pgError *pgconn.PgError
		if err := <-outcome; !errors.As(err, &pgError) || pgError.Code != "40001" {
			tb.Fatalf("stale ledger write reused prior authority instead of serialization refusal: %v", err)
		}
	})
}
