// Compare statement-local column factoring with the unshared catalog view,
// retaining the complete dynamic row and existing versioned drift reducer.
package monitor

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
)

// Capture the real emitter independently of diagnostic helpers or fixed heads.
func migrationPublicColumnQueries(t testing.TB) (string, string, int) {
	t.Helper()
	head := server.MigrationCount()
	query := ""
	emitted := 0
	source := &syntheticSource{postgresFn: func(statement string) ([]Row, error) {
		if strings.Contains(statement, "FROM migration_catalog") {
			return syntheticMigrationCatalogRows(head), nil
		}
		emitted++
		query = statement
		return []Row{syntheticMigrationArtifactRow(head)}, nil
	}}
	if _, err := NewMigrationsSignal().Run(t.Context(), syntheticSettings(source)); err != nil || emitted != 1 {
		t.Fatal("migration column query emitter unavailable")
	}
	if strings.Count(query, migrationPublicColumnsQuery+", ") != 1 || !strings.Contains(query, "FROM public_columns") {
		t.Fatal("public column view lost its one statement-local owner")
	}
	baseline := strings.Replace(query, migrationPublicColumnsQuery+", ", "", 1)
	baseline = strings.ReplaceAll(baseline, "FROM public_columns", "FROM information_schema.columns")
	if strings.Contains(baseline, "public_columns") {
		t.Fatal("public column CTE name collides with another emitted expression")
	}
	return baseline, query, len(syntheticMigrationArtifactRow(head))
}

func TestMigrationsPublicColumnsKeepPermissionViewAndSingleOwner(t *testing.T) {
	baseline, query, columns := migrationPublicColumnQueries(t)
	if columns == 0 || baseline == query || !strings.Contains(migrationPublicColumnsQuery, "AS MATERIALIZED") ||
		!strings.Contains(migrationPublicColumnsQuery, "FROM information_schema.columns WHERE table_schema = 'public'") {
		t.Fatal("migration columns lost permission-aware public view semantics")
	}
	// Exercise the changed owning predicate in emitted SQL, without a source
	// file parser or a fixed count of references that future migrations add.
	normalized := strings.Join(strings.Fields(query), " ")
	if !strings.Contains(normalized, "FROM public_columns WHERE table_schema = 'public' AND table_name = 'account_payment' AND column_name = 'block_rollup_cursor' AND data_type = 'uuid' AND is_nullable = 'YES' AND column_default IS NULL") {
		t.Fatal("rollup cursor bypassed the shared permission-aware column view")
	}

}

func TestMigrationsPublicColumnsPreserveDynamicRowAndFaultVisibility(t *testing.T) {
	baseline, candidate, columns := migrationPublicColumnQueries(t)
	column := -1
	for _, artifact := range migrationArtifacts {
		if artifact.name == "account_payment.block_rollup_cursor" {
			column = artifact.rowColumn
		}
	}
	if column < 0 {
		t.Fatal("published rollup cursor owner unavailable")
	}
	(&server.TestEnv{ApplyDbMigrations: false, Warmup: false, RerunCount: 0}).Run(t, func(tb testing.TB) {
		read := func(query string) Row {
			ctx, cancel := context.WithTimeout(tb.Context(), 45*time.Second)
			defer cancel()
			observation := &migrationCensusObservation{settings: DefaultMigrationCensusSettings()}
			ctx = context.WithValue(ctx, migrationCensusContextKey{}, observation)
			rows, err := (&migrationPingDatabaseSource{requireCensusOwnership: true}).PostgreSQL(ctx, query)
			if err != nil || len(rows) != 1 || len(rows[0]) != columns || !observation.catalogComplete {
				tb.Fatal("complete migration row comparison unavailable")
			}
			return rows[0]
		}
		check := func(present *bool, drift bool) {
			before, after := read(baseline), read(candidate)
			if !slices.Equal(before, after) || present != nil && migrationBool(after[column]) != *present {
				tb.Fatal("shared public columns changed the complete migration row")
			}
			ctx, cancel := context.WithTimeout(tb.Context(), 45*time.Second)
			defer cancel()
			_, actual := migrationPingDatabaseCheck(tb, ctx)
			if drift {
				if !strings.Contains(actual, "account_payment.block_rollup_cursor@v687") {
					tb.Fatal("true column fault lost its original versioned drift signal")
				}
			} else if actual != "" {
				tb.Fatal("healthy or prepublication columns reported schema drift")
			}
		}
		for _, head := range []int{686, 687} {
			server.ApplyDbMigrationsUpTo(tb.Context(), head)
			present := head == 687
			check(&present, false)
		}
		exec := func(statement string) {
			ctx, cancel := context.WithTimeout(tb.Context(), 45*time.Second)
			defer cancel()
			server.Db(ctx, func(conn server.PgConn) {
				if _, err := conn.Exec(ctx, statement); err != nil {
					tb.Fatal("synthetic catalog column mutation failed")
				}
			}, server.OptNoRetry())
		}
		present := false
		exec(`ALTER TABLE account_payment RENAME COLUMN block_rollup_cursor TO synthetic_missing_rollup_cursor`)
		check(&present, true)
		exec(`ALTER TABLE account_payment RENAME COLUMN synthetic_missing_rollup_cursor TO block_rollup_cursor`)
		present = true
		check(&present, false)
		exec(`ALTER TABLE account_payment ALTER COLUMN block_rollup_cursor SET DEFAULT '00000000-0000-0000-0000-000000000000'::uuid`)
		present = false
		check(&present, true)
		exec(`ALTER TABLE account_payment ALTER COLUMN block_rollup_cursor DROP DEFAULT`)
		present = true
		check(&present, false)
		server.ApplyDbMigrationsUpTo(tb.Context(), server.MigrationCount())
		// A future append may legitimately retire the old cursor; every current
		// column must still match, without freezing the schema's head or width.
		check(nil, false)
	})
}
