//go:build sidecar_pending

package server

// MG2's §20 part (QUOTA-M2-02): §20.14's four tables and service_debit_journal's delete
// guard, under -tags sidecar_pending, on the private stack through .stack/server-test.sh.

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

// CODESTYLE's migration rules for MG2's §20 part: its four tables, indexes on them, and
// the guard, whose only touch of an existing table is a delete trigger with its lock
// wait bounded.
func TestSidecarMg2QuotaPartIsAdditive(t *testing.T) {
	ownTables := `(network_quota_state|network_service_usage|service_debit_journal|service_usage_batch)`
	statement := regexp.MustCompile(`(?is)^\s*(CREATE TABLE ` + ownTables + `\b|CREATE INDEX ` + ownTables + `_\w+ ON ` + ownTables + `\b|SET LOCAL lock_timeout = '2s';\s*CREATE FUNCTION protect_pending_service_debit\(\))`)
	refused := regexp.MustCompile(`(?i)\b(DROP|ALTER|RENAME|NOT VALID|TRUNCATE|UPDATE|REFERENCES|transaction_timeout)\b`)
	migrations := sidecarMg2QuotaMigrations()
	if len(migrations) == 0 {
		t.Fatal("MG2's §20 part is empty")
	}
	for index, migration := range migrations {
		switch value := migration.(type) {
		case *SqlMigration:
			if !statement.MatchString(value.sql) {
				t.Errorf("entry %d is not an addition: %s", index, value.sql)
			}
			if refused.MatchString(value.sql) {
				t.Errorf("entry %d changes what the running release uses: %s", index, value.sql)
			}
		default:
			t.Fatalf("entry %d is a %T", index, migration)
		}
	}
	// the guard's one trigger fires on deletes alone
	if !strings.Contains(serviceDebitJournalGuardSql, "BEFORE DELETE ON transfer_balance") || strings.Count(serviceDebitJournalGuardSql, "CREATE TRIGGER") != 1 {
		t.Errorf("the guard is not one delete trigger: %s", serviceDebitJournalGuardSql)
	}
}

// §20.14's columns, exactly: a further column needs a metadata row first (I-18).
func TestSidecarMg2QuotaColumns(t *testing.T) {
	DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		columns := map[string]string{}
		MaintenanceDb(ctx, func(conn PgConn) {
			result, err := conn.Query(ctx, `
				SELECT table_name || '.' || column_name, data_type || ':' || is_nullable
				FROM information_schema.columns
				WHERE table_schema = current_schema() AND table_name IN (
					'network_quota_state', 'network_service_usage', 'service_debit_journal', 'service_usage_batch'
				)
			`)
			WithPgResult(result, err, func() {
				for result.Next() {
					var column, shape string
					Raise(result.Scan(&column, &shape))
					columns[column] = shape
				}
			})
		})
		want := map[string]string{
			"network_quota_state.network_id":         "uuid:NO",
			"network_quota_state.stored_byte_count":  "bigint:NO",
			"network_quota_state.storage_over":       "boolean:NO",
			"network_quota_state.spent_time":         "timestamp without time zone:YES",
			"network_service_usage.network_id":       "uuid:NO",
			"network_service_usage.usage_day":        "date:NO",
			"network_service_usage.meter":            "character varying:NO",
			"network_service_usage.quantity":         "bigint:NO",
			"service_debit_journal.usage_id":         "uuid:NO",
			"service_debit_journal.balance_id":       "uuid:NO",
			"service_debit_journal.meter":            "character varying:NO",
			"service_debit_journal.debit_byte_count": "bigint:NO",
			"service_debit_journal.batch_id":         "uuid:YES",
			"service_debit_journal.applied":          "boolean:NO",
			"service_usage_batch.batch_id":           "uuid:NO",
			"service_usage_batch.producer":           "character varying:NO",
			"service_usage_batch.window_start":       "timestamp without time zone:NO",
			"service_usage_batch.applied_time":       "timestamp without time zone:NO",
		}
		if len(columns) != len(want) {
			t.Errorf("%d columns, want %d: %v", len(columns), len(want), columns)
		}
		for column, shape := range want {
			if columns[column] != shape {
				t.Errorf("%s is %q, want %q", column, columns[column], shape)
			}
		}
	})
}

// Inserts a transfer_balance as the running release writes one, returning its id.
func testingInsertGuardBalance(t testing.TB, ctx context.Context) Id {
	t.Helper()
	balanceId := NewId()
	now := NowUtc()
	MaintenanceTx(ctx, func(tx PgTx) {
		RaisePgResult(tx.Exec(ctx, `
			INSERT INTO transfer_balance (
				balance_id, network_id, start_time, end_time, start_balance_byte_count,
				balance_byte_count, net_revenue_nano_cents, pro
			) VALUES ($1, $2, $3, $4, 1024, 1024, 0, false)
		`, balanceId, NewId(), now.Add(-time.Hour), now.Add(time.Hour)))
	})
	return balanceId
}

// Deletes a balance as retention does, returning the error, if any.
func testingDeleteGuardBalance(ctx context.Context, balanceId Id) (returnErr error) {
	HandleError(func() {
		MaintenanceTx(ctx, func(tx PgTx) {
			RaisePgResult(tx.Exec(ctx, `DELETE FROM transfer_balance WHERE balance_id = $1`, balanceId))
		}, OptNoRetry())
	}, func(err error) {
		returnErr = err
	})
	return
}

// A balance with a service debit row, pending or applied, is never deleted; with its
// rows gone it is. transfer_debit_journal's guard stands beside it unchanged.
func TestServiceDebitJournalGuardRefusesDeletingABalanceWithRows(t *testing.T) {
	DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		for _, applied := range []bool{false, true} {
			balanceId := testingInsertGuardBalance(t, ctx)
			usageId := NewId()
			MaintenanceTx(ctx, func(tx PgTx) {
				RaisePgResult(tx.Exec(ctx, `
					INSERT INTO service_debit_journal (usage_id, balance_id, meter, debit_byte_count, batch_id, applied)
					VALUES ($1, $2, 'evs', 512, $3, $4)
				`, usageId, balanceId, NewId(), applied))
			})

			err := testingDeleteGuardBalance(ctx, balanceId)
			var pgErr *pgconn.PgError
			if !errors.As(err, &pgErr) || pgErr.Code != "55000" || !strings.Contains(pgErr.Message, "pending service debits") {
				t.Fatalf("applied %t: delete answered %v, want the guard's 55000", applied, err)
			}

			MaintenanceTx(ctx, func(tx PgTx) {
				RaisePgResult(tx.Exec(ctx, `DELETE FROM service_debit_journal WHERE balance_id = $1 AND usage_id = $2`, balanceId, usageId))
			})
			if err := testingDeleteGuardBalance(ctx, balanceId); err != nil {
				t.Fatalf("applied %t: a balance with no rows was kept: %v", applied, err)
			}
		}

		triggers := []string{}
		MaintenanceDb(ctx, func(conn PgConn) {
			result, err := conn.Query(ctx, `
				SELECT tgname FROM pg_trigger
				WHERE tgrelid = 'transfer_balance'::regclass AND NOT tgisinternal AND tgname LIKE 'transfer_balance_pending_%'
				ORDER BY tgname
			`)
			WithPgResult(result, err, func() {
				for result.Next() {
					var name string
					Raise(result.Scan(&name))
					triggers = append(triggers, name)
				}
			})
		})
		if strings.Join(triggers, ",") != "transfer_balance_pending_debit_guard,transfer_balance_pending_service_debit_guard" {
			t.Fatalf("triggers = %v", triggers)
		}
	})
}

// FS-55 for MG2's §20 part: the running release keeps writing and deleting balances on
// the schema before the group and after it, since the guard refuses nothing while the
// journal is empty.
func TestSidecarMg2QuotaRunsBesideTheRunningRelease(t *testing.T) {
	(&TestEnv{ApplyDbMigrations: false, RerunCount: 0}).Run(t, func(t testing.TB) {
		ctx := context.Background()
		running := MigrationCount() - len(sidecarPendingMigrations())
		ApplyDbMigrationsUpTo(ctx, running)

		var journal *string
		MaintenanceDb(ctx, func(conn PgConn) {
			Raise(conn.QueryRow(ctx, `SELECT to_regclass('service_debit_journal')::text`).Scan(&journal))
		})
		if journal != nil {
			t.Fatal("the running release's schema already holds MG2")
		}
		before := testingInsertGuardBalance(t, ctx)
		if err := testingDeleteGuardBalance(ctx, before); err != nil {
			t.Fatalf("the running release's delete failed before MG2: %v", err)
		}

		ApplyDbMigrations(ctx)
		if got := DbVersion(ctx); got != MigrationCount() {
			t.Fatalf("version = %d of %d", got, MigrationCount())
		}
		after := testingInsertGuardBalance(t, ctx)
		if err := testingDeleteGuardBalance(ctx, after); err != nil {
			t.Fatalf("the running release's delete failed after MG2: %v", err)
		}
	})
}

// The flush pages a shard by balance key on the expression index, since the journal has
// no shard column: a page read uses service_debit_journal_shard, with sequential scans
// off as CODESTYLE's compatibility proof plans them.
func TestServiceDebitJournalShardIndexServesAPage(t *testing.T) {
	DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		balanceId := testingInsertGuardBalance(t, ctx)
		MaintenanceTx(ctx, func(tx PgTx) {
			for i := 0; i < 32; i += 1 {
				RaisePgResult(tx.Exec(ctx, `
					INSERT INTO service_debit_journal (usage_id, balance_id, meter, debit_byte_count)
					VALUES ($1, $2, 'evs', 1)
				`, NewId(), balanceId))
			}
		})
		var plan string
		MaintenanceTx(ctx, func(tx PgTx) {
			RaisePgResult(tx.Exec(ctx, `SET LOCAL enable_seqscan = off`))
			result, err := tx.Query(ctx, `
				EXPLAIN
				SELECT balance_id, usage_id
				FROM service_debit_journal
				WHERE get_byte(uuid_send(balance_id), 15) % 16 = $1 AND $2 < balance_id
				ORDER BY balance_id, usage_id
				LIMIT 64
			`, 3, Id{})
			WithPgResult(result, err, func() {
				lines := []string{}
				for result.Next() {
					var line string
					Raise(result.Scan(&line))
					lines = append(lines, line)
				}
				plan = strings.Join(lines, "\n")
			})
		})
		if !strings.Contains(plan, "service_debit_journal_shard") {
			t.Fatalf("a shard page does not use the shard index:\n%s", plan)
		}
		MaintenanceTx(ctx, func(tx PgTx) {
			RaisePgResult(tx.Exec(ctx, `DELETE FROM service_debit_journal WHERE balance_id = $1`, balanceId))
		})
	})
}

// The guard's body as published, for a catalog check.
func TestServiceDebitGuardBodySql(t *testing.T) {
	body := ServiceDebitGuardBodySql()
	if !strings.Contains(body, "FROM service_debit_journal WHERE balance_id=OLD.balance_id") || strings.Contains(body, "$guard$") {
		t.Fatalf("body = %q", body)
	}
}
