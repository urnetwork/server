// Proposed close-owner artifacts are checked at deployment, not on task turns.
package monitor

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/urnetwork/server"
)

func contractCloseOwnerCatalogTestQuery(t testing.TB) string {
	t.Helper()
	var emitted string
	source := &syntheticSource{postgresFn: func(query string) ([]Row, error) {
		if strings.Contains(query, "FROM migration_catalog") {
			return syntheticMigrationCatalogRows(server.MigrationCount()), nil
		}
		emitted = query
		return []Row{syntheticMigrationArtifactRow(server.MigrationCount())}, nil
	}}
	if _, err := NewMigrationsSignal().Run(t.Context(), syntheticSettings(source)); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(emitted, strings.Join(contractCloseOwnerArtifactQueries, ",\n")) {
		t.Fatal("deployment signal omitted the actual close-owner schema contract")
	}
	_, indexQuery, found := strings.Cut(emitted, "index_artifact AS (")
	if !found {
		t.Fatal("deployment signal omitted its index catalog")
	}
	indexQuery, _, found = strings.Cut(indexQuery, "), constraint_artifact AS (")
	if !found {
		t.Fatal("deployment signal changed the index catalog boundary")
	}
	return "WITH index_artifact AS (" + indexQuery + ") SELECT ARRAY[" + strings.Join(contractCloseOwnerArtifactQueries, ",") + "]"
}

// PostgreSQL retains the whitespace inside the dollar quotes in prosrc. The
// deployment check must compare that complete published body byte for byte.
func TestMigrationsContractCloseOwnerPublishedGuardBody(t *testing.T) {
	_, bodyAndSuffix, found := strings.Cut(server.ContractCloseOwnerSchemaSql, "AS $body$")
	if !found {
		t.Fatal("published close-owner schema omitted the guard function body")
	}
	body, _, found := strings.Cut(bodyAndSuffix, "$body$;")
	if !found {
		t.Fatal("published close-owner guard function body is unterminated")
	}
	exactBody := "AND p.prosrc='" + strings.ReplaceAll(body, "'", "''") + "'"
	if !strings.Contains(contractCloseOwnerCatalogTestQuery(t), exactBody) {
		t.Fatal("deployment catalog check differs from the exact published guard function body")
	}
}

func TestMigrationsContractCloseOwnerPublishedArtifacts(t *testing.T) {
	contractCloseOwnerCatalogTestQuery(t)
	for version := 803; version <= 805; version++ {
		var artifact *migrationArtifact
		for index := range migrationArtifacts {
			if migrationArtifacts[index].requiredVersion == version {
				if artifact != nil {
					t.Fatal("close owner has duplicate migration artifact definitions")
				}
				artifact = &migrationArtifacts[index]
			}
		}
		if artifact == nil || artifact.rowColumn != version-597 {
			t.Fatal("close owner has no exact deployment artifact", version)
		}
		for _, head := range []int{version - 1, version} {
			row := syntheticMigrationArtifactRow(head)
			row[artifact.rowColumn] = "f"
			source := &syntheticSource{postgresFn: func(query string) ([]Row, error) {
				if strings.Contains(query, "FROM migration_catalog") {
					return syntheticMigrationCatalogRows(head), nil
				}
				return []Row{row}, nil
			}}
			alerts, err := NewMigrationsSignal().Run(t.Context(), syntheticSettings(source))
			if err != nil {
				t.Fatal(err)
			}
			if head < version {
				if len(alerts) != 1 || alerts[0].Class != "migration-behind" {
					t.Fatal("unapplied source schema was reported as drift")
				}
			} else if !strings.Contains(requireAlertClass(t, alerts, "migration-schema-drift").Markdown(), fmt.Sprintf("%s@v%d", artifact.name, version)) {
				t.Fatal("source schema drift lost its published owner")
			}
		}
	}
}

func TestMigrationsContractCloseOwnerExactSchemaPrefixes(t *testing.T) {
	(&server.TestEnv{ApplyDbMigrations: false, RerunCount: 0}).Run(t, func(t testing.TB) {
		ctx := t.Context()
		query := contractCloseOwnerCatalogTestQuery(t)
		check := func(head int) {
			server.Db(ctx, func(conn server.PgConn) {
				var matches []bool
				server.Raise(conn.QueryRow(ctx, query).Scan(&matches))
				if len(matches) != 3 {
					t.Fatal("deployment check returned an incomplete source owner shape")
				}
				for index, matched := range matches {
					if matched != (head >= 803+index) {
						t.Fatal("close-owner artifact disagrees with the actual migration prefix", head, index, matched)
					}
				}
			}, server.OptNoRetry())
		}
		check(0)
		for head := 802; head <= 805; head++ {
			server.ApplyDbMigrationsUpTo(ctx, head)
			check(head)
		}
	})
}

// Each corruption owns one transaction and rolls it back before the next
// connection is borrowed. No savepoint or nested database acquisition is used.
func TestMigrationsContractCloseOwnerCatalogFaults(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		query := contractCloseOwnerCatalogTestQuery(t)
		for _, fault := range []struct {
			index int
			sql   string
		}{
			{index: 0, sql: `ALTER TABLE legacy_settlement_intent ALTER COLUMN source_client_id SET NOT NULL`},
			{index: 0, sql: `ALTER TABLE legacy_settlement_intent ALTER COLUMN source_client_id SET DEFAULT '00000000-0000-0000-0000-000000000000'::uuid`},
			{index: 0, sql: `ALTER TABLE legacy_settlement_intent DISABLE TRIGGER legacy_settlement_intent_resolve_close_owner`},
			{index: 0, sql: `CREATE OR REPLACE FUNCTION assign_legacy_settlement_close_owner() RETURNS trigger LANGUAGE plpgsql AS $$BEGIN RETURN NEW; END;$$`},
			{index: 1, sql: `DROP INDEX legacy_settlement_intent_source_due`},
			{index: 2, sql: `DROP INDEX legacy_settlement_intent_owner_missing`},
		} {
			server.Db(ctx, func(conn server.PgConn) {
				tx, err := conn.Begin(ctx)
				server.Raise(err)
				defer tx.Rollback(context.Background())
				read := func() []bool {
					var matches []bool
					server.Raise(tx.QueryRow(ctx, query).Scan(&matches))
					return matches
				}
				for _, matched := range read() {
					if !matched {
						t.Fatal("healthy source schema failed the deployment contract")
					}
				}
				server.RaisePgResult(tx.Exec(ctx, fault.sql))
				if read()[fault.index] {
					t.Fatal("corrupted source-owner artifact passed deployment", fault.sql)
				}
				server.Raise(tx.Rollback(ctx))
			}, server.OptNoRetry())
		}
	})
}
