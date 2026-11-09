package sidecart3

// The oracle's Postgres on T3 (TEST-M2-01): server's tables read, in a transaction that refuses a write.

import (
	"context"
	"testing"

	"github.com/urnetwork/server"
)

func TestPostgresReaderReadsAndRefusesWrites(t *testing.T) {
	// the private stores, a database of the test's own, and no migrations: nothing here reads server's schema
	(&server.TestEnv{ApplyDbMigrations: false}).Run(t, func(tb testing.TB) {
		ctx := context.Background()
		reader := PostgresReader()
		rows, err := reader.Rows(ctx, "SELECT $1::int + 1, 'a'", 41)
		if err != nil || len(rows) != 1 || len(rows[0]) != 2 {
			t.Fatalf("rows %v, %v", rows, err)
		}
		if got, ok := rows[0][0].(int32); !ok || got != 42 {
			t.Fatalf("the first column %#v", rows[0][0])
		}
		if _, err := reader.Rows(ctx, "CREATE TABLE store_view_write_refused (id int)"); err == nil {
			t.Fatal("a read-only read took a write")
		}
		if _, err := reader.Rows(ctx, "SELECT * FROM a_table_nobody_made"); err == nil {
			t.Fatal("a query on a missing table answered")
		}
	})
}
