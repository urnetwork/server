package model

import (
	"testing"
	"time"

	"github.com/urnetwork/server"
)

// Migration 947 (db_provider_egress_url_source.go) against days of the
// partitioned history made around it. A day the partition registry made, but
// had not attached, before the migration attaches after it with the new
// columns, and so does the next day the registry makes. A made day that
// missed the migration's own step still attaches, since the registry adds the
// parent's missing columns to a history day before every attach.
func TestProviderEgressHealthHistoryDaysMadeAroundTheUrlSourceMigrationAttach(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		now := server.NowUtc()
		testEgressHistoryCutover(t, ctx, now)
		family := providerEgressHealthHistoryPartitionFamily
		table := providerEgressHealthHistoryTable
		exec := func(sql string) {
			server.Tx(ctx, func(tx server.PgTx) {
				server.RaisePgResult(tx.Exec(ctx, sql))
			})
		}
		leafSql := func(r server.PartitionRange) (statements []string) {
			server.Db(ctx, func(conn server.PgConn) {
				var err error
				statements, err = family.CreateLeafSql(ctx, conn, table, r)
				server.Raise(err)
			})
			if len(statements) < 3 {
				t.Fatalf("the registry's create of a history day is %v, want the copy, the column reconcile and the attach", statements)
			}
			return
		}
		columns := func(leaf string) (count int) {
			server.Db(ctx, func(conn server.PgConn) {
				server.Raise(conn.QueryRow(ctx, `SELECT count(*) FROM pg_attribute
					WHERE attrelid = to_regclass($1) AND attname IN ('url_source', 'url_probation') AND NOT attisdropped
					AND attnotnull`, "public."+leaf).Scan(&count))
			})
			return
		}
		attached := func(leaf string) (partition bool) {
			server.Db(ctx, func(conn server.PgConn) {
				server.Raise(conn.QueryRow(ctx, `SELECT relispartition FROM pg_class WHERE oid = to_regclass($1)`, "public."+leaf).Scan(&partition))
			})
			return
		}
		day := family.RangeAt(now.AddDate(0, 0, 20))
		nextDay := family.RangeAt(now.AddDate(0, 0, 21))
		laterDay := family.RangeAt(now.AddDate(0, 0, 22))

		// the history as it was before the migration, and a day the registry
		// made then: the copy of the parent, not yet attached
		exec(`ALTER TABLE provider_egress_health_history DROP COLUMN url_source, DROP COLUMN url_probation`)
		dayStatements := leafSql(day)
		exec(dayStatements[0])
		dayLeaf := family.LeafName(table, day)
		if columns(dayLeaf) != 0 || attached(dayLeaf) {
			t.Fatalf("the day made before the migration has the columns or is attached")
		}

		exec(server.ProviderEgressUrlSourceSql)
		if columns(dayLeaf) != 2 {
			t.Fatalf("the migration left the made day without the columns")
		}
		for _, statement := range dayStatements[1:] {
			exec(statement)
		}
		if !attached(dayLeaf) {
			t.Fatal("the day made before the migration did not attach after it")
		}

		// the next day the registry makes has them and attaches
		if err := family.CreateRange(ctx, table, nextDay); err != nil {
			t.Fatalf("the next day: %v", err)
		}
		if nextLeaf := family.LeafName(table, nextDay); columns(nextLeaf) != 2 || !attached(nextLeaf) {
			t.Fatal("the next day lacks the columns or is not attached")
		}

		// a made day that lacks them anyway is given them before its attach
		laterStatements := leafSql(laterDay)
		exec(laterStatements[0])
		laterLeaf := family.LeafName(table, laterDay)
		exec(`ALTER TABLE public."` + laterLeaf + `" DROP COLUMN url_source, DROP COLUMN url_probation`)
		for _, statement := range laterStatements[1:] {
			exec(statement)
		}
		if columns(laterLeaf) != 2 || !attached(laterLeaf) {
			t.Fatal("a made day lacking the columns did not take them before its attach")
		}

		// an outcome stored in a day made around the migration reads as the
		// scored general outcome an older release stores
		var source int16
		var probation bool
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO provider_egress_health_history
				(run_id, client_id, measured_at, ok_count, total_count, class_results, tls_authentication_failure)
				VALUES ($1, $2, $3, 1, 1, '{}', false)`, server.NewId(), server.NewId(), day.Lower.Add(time.Hour)))
			server.Raise(tx.QueryRow(ctx, `SELECT url_source, url_probation FROM `+`"`+dayLeaf+`"`).Scan(&source, &probation))
		})
		if source != 0 || probation {
			t.Fatalf("an outcome in the made day reads source %d probation %t", source, probation)
		}
	})
}
