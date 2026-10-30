// Catalog controls cover migration publication and exact wake metadata shape.
package monitor

import (
	"context"
	"strings"
	"testing"

	"github.com/urnetwork/server/v2026"
)

func TestMigrationsTaskRunOnceGenerationFollowsPublishedVersion(t *testing.T) {
	var artifact *migrationArtifact
	for index := range migrationArtifacts {
		if migrationArtifacts[index].requiredVersion == 796 {
			if artifact != nil {
				t.Fatal("generation schema has duplicate artifact contracts")
			}
			artifact = &migrationArtifacts[index]
		}
	}
	if artifact == nil || artifact.rowColumn != 796-589 {
		t.Fatal("generation schema has no exact artifact contract")
	}
	for _, head := range []int{795, 796} {
		row := syntheticMigrationArtifactRow(head)
		row[artifact.rowColumn] = "f"
		source := &syntheticSource{postgresFn: func(query string) ([]Row, error) {
			if strings.Contains(query, "FROM migration_catalog") {
				return syntheticMigrationCatalogRows(head), nil
			}
			if !strings.Contains(query, taskRunOnceGenerationArtifactQuery) {
				t.Fatal("actual migration probe omitted the generation predicate")
			}
			return []Row{row}, nil
		}}
		alerts, err := NewMigrationsSignal().Run(t.Context(), syntheticSettings(source))
		if err != nil {
			t.Fatal(err)
		}
		if head == 795 {
			if len(alerts) != 1 || alerts[0].Class != "migration-behind" {
				t.Fatal("future generation columns were labeled drift")
			}
		} else if !strings.Contains(requireAlertClass(t, alerts, "migration-schema-drift").Markdown(), artifact.name+"@v796") {
			t.Fatal("generation drift lost its schema owner")
		}
	}
}

func TestMigrationsTaskRunOnceGenerationCatalogFaults(t *testing.T) {
	snMainnetMigrationTestEnv().Run(t, func(t testing.TB) {
		snMainnetMigrationFaultTx(t, func(ctx context.Context, tx server.PgTx) {
			read := func() bool {
				var matched bool
				server.Raise(tx.QueryRow(ctx, "SELECT "+taskRunOnceGenerationArtifactQuery).Scan(&matched))
				return matched
			}
			if !read() {
				t.Fatal("healthy generation schema failed its catalog contract")
			}
			for _, fault := range []string{
				`ALTER TABLE pending_task DROP COLUMN run_once_generation`,
				`ALTER TABLE pending_task ALTER COLUMN run_once_generation TYPE integer`,
				`ALTER TABLE pending_task ALTER COLUMN run_once_generation DROP NOT NULL`,
				`ALTER TABLE pending_task ALTER COLUMN run_once_generation SET DEFAULT 1`,
				`ALTER TABLE pending_task DROP COLUMN claim_generation`,
				`ALTER TABLE pending_task ALTER COLUMN claim_generation DROP DEFAULT`,
				`ALTER TABLE pending_task ALTER COLUMN claim_generation DROP NOT NULL`,
				`ALTER TABLE pending_task DROP COLUMN run_once_wake_at`,
				`ALTER TABLE pending_task ALTER COLUMN run_once_wake_at SET DEFAULT now()`,
				`ALTER TABLE pending_task ALTER COLUMN run_once_wake_at TYPE timestamptz`,
				`ALTER TABLE pending_task ALTER COLUMN run_once_wake_at SET NOT NULL`,
			} {
				server.RaisePgResult(tx.Exec(ctx, `SAVEPOINT run_once_catalog_fault`))
				server.RaisePgResult(tx.Exec(ctx, fault))
				if read() {
					t.Fatal("changed generation schema cleared its contract", fault)
				}
				server.RaisePgResult(tx.Exec(ctx, `ROLLBACK TO SAVEPOINT run_once_catalog_fault`))
				if !read() {
					t.Fatal("catalog fault did not restore healthy schema")
				}
			}
		})
	})
}
