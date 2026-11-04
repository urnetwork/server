// The unresolved expression statistics through the public migration signal:
// an unsampled published object is healthy, lag remains lag, and shape faults
// at a source-known head belong to this migration.
package monitor

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/urnetwork/server"
)

func TestMigrationsTransferContractUnresolvedStatisticsPublicationBoundary(t *testing.T) {
	version := server.TransferContractUnresolvedPredicateStatisticsVersion()
	head := server.MigrationCount()
	if version < 1 || head < version {
		t.Fatalf("unresolved expression statistics migration is not published: version=%d head=%d", version, head)
	}
	artifact := migrationCurrentSuffixTestContract(t, version).artifact
	contract := migrationCurrentSuffixTestContract(t, version)
	if contract.query != transferContractUnresolvedStatisticsArtifactQuery ||
		artifact.name != "transfer_contract unresolved predicate expression statistics" ||
		artifact.rowColumn != version-589 || artifact.removedVersion != 0 {
		t.Fatalf("expression statistics contract is %+v", artifact)
	}
	for _, test := range []struct {
		version int
		present bool
		class   string
	}{
		{version: version - 1, class: "migration-behind"},
		{version: head, present: true},
		{version: head, class: "migration-schema-drift"},
		{version: head + 1, present: true, class: "cannot-observe"},
		{version: head + 1, class: "cannot-observe"},
	} {
		row := syntheticMigrationArtifactRow(test.version)
		row[artifact.rowColumn] = "f"
		if test.present {
			row[artifact.rowColumn] = "t"
		}
		source := &syntheticSource{postgresFn: func(query string) ([]Row, error) {
			if strings.Contains(query, "FROM migration_catalog") {
				if test.version > head {
					return migrationFutureSchemaCatalogRows(), nil
				}
				return syntheticMigrationCatalogRows(test.version), nil
			}
			return []Row{row}, nil
		}}
		alerts, err := NewMigrationsSignal().Run(context.Background(), syntheticSettings(source))
		if err != nil {
			t.Fatal(err)
		}
		if test.class == "" {
			if len(alerts) != 0 {
				t.Fatalf("a present expression statistics object produced %d alerts", len(alerts))
			}
			continue
		}
		if len(alerts) != 1 {
			t.Fatalf("version %d produced %d alerts, want exactly one %s", test.version, len(alerts), test.class)
		}
		alert := requireAlertClass(t, alerts, test.class)
		if test.class == "migration-schema-drift" && !strings.Contains(alert.Markdown(), fmt.Sprintf("%s@v%d", artifact.name, artifact.requiredVersion)) {
			t.Fatal("published expression statistics drift lost its owning artifact")
		}
		if test.class == "cannot-observe" && (!alert.unobservable || alert.Severity != SeverityWarn) {
			t.Fatal("an unknown successor certified the expression statistics")
		}
	}
}

// A failed or absent metadata result never becomes a healthy statistics object.
func TestMigrationsTransferContractUnresolvedStatisticsUnavailableSource(t *testing.T) {
	private := errors.New("synthetic statistics catalog unavailable")
	for _, sourceError := range []error{nil, private} {
		reads := 0
		source := &syntheticSource{postgresFn: func(query string) ([]Row, error) {
			reads++
			if !strings.Contains(query, transferContractUnresolvedStatisticsArtifactQuery) {
				t.Fatal("the owning probe omitted the expression statistics predicate")
			}
			return nil, sourceError
		}}
		alerts, err := NewMigrationsSignal().Run(context.Background(), syntheticSettings(source))
		if reads != 1 || err == nil || len(alerts) != 0 {
			t.Fatal("an unavailable statistics catalog certified health or continued reading")
		}
		if sourceError != nil && !errors.Is(err, sourceError) {
			t.Fatal("the statistics catalog lost its source error identity")
		}
	}
}

// The real schema before and at publication, without ANALYZE, then rolled-back
// name, namespace, relation, expression and statistics-kind faults.
func TestMigrationsTransferContractUnresolvedStatisticsPublishedSchemaAndFaults(t *testing.T) {
	version := server.TransferContractUnresolvedPredicateStatisticsVersion()
	artifact := migrationCurrentSuffixTestContract(t, version).artifact
	owner := fmt.Sprintf("%s@v%d", artifact.name, version)
	faults := []struct{ name, sql string }{
		{"object absent", `DROP STATISTICS public.transfer_contract_unresolved_predicate`},
		{"object outside public", `CREATE SCHEMA monitor_statistics_fault; ALTER STATISTICS public.transfer_contract_unresolved_predicate SET SCHEMA monitor_statistics_fault`},
		{"object on a different table", `DROP STATISTICS public.transfer_contract_unresolved_predicate; CREATE STATISTICS public.transfer_contract_unresolved_predicate ON (reschedule_error_count = 0) FROM public.pending_task`},
		{"legacy outcome expression", `DROP STATISTICS public.transfer_contract_unresolved_predicate; CREATE STATISTICS public.transfer_contract_unresolved_predicate ON (outcome IS NULL) FROM public.transfer_contract`},
		{"inverted dispute expression", `DROP STATISTICS public.transfer_contract_unresolved_predicate; CREATE STATISTICS public.transfer_contract_unresolved_predicate ON (CASE WHEN outcome IS NULL THEN dispute = true ELSE false END) FROM public.transfer_contract`},
		{"extra expression", `DROP STATISTICS public.transfer_contract_unresolved_predicate; CREATE STATISTICS public.transfer_contract_unresolved_predicate ON (CASE WHEN outcome IS NULL THEN dispute = false ELSE false END), (outcome IS NULL) FROM public.transfer_contract`},
		{"extra column and statistics kinds", `DROP STATISTICS public.transfer_contract_unresolved_predicate; CREATE STATISTICS public.transfer_contract_unresolved_predicate ON (CASE WHEN outcome IS NULL THEN dispute = false ELSE false END), dispute FROM public.transfer_contract`},
	}
	type prefix struct {
		row    Row
		alerts []Alert
	}
	prefixes := map[int]prefix{}
	var observations [][]Alert
	(&server.TestEnv{ApplyDbMigrations: false, RerunCount: 0}).Run(t, func(t testing.TB) {
		for _, head := range []int{version - 1, version} {
			server.ApplyDbMigrationsUpTo(t.Context(), head)
			source := &migrationPingDatabaseSource{}
			alerts, err := NewMigrationsSignal().Run(t.Context(), syntheticSettings(source))
			if err != nil {
				t.Fatal(err)
			}
			prefixes[head] = prefix{row: source.artifactRow, alerts: alerts}
		}
		for _, fault := range faults {
			snMainnetMigrationFaultTx(t, func(ctx context.Context, tx server.PgTx) {
				if _, err := tx.Exec(ctx, fault.sql); err != nil {
					t.Fatalf("apply %s: %v", fault.name, err)
				}
				alerts, err := NewMigrationsSignal().Run(ctx, syntheticSettings(&snMainnetMigrationTxSource{tx: tx}))
				if err != nil {
					t.Fatalf("inspect %s: %v", fault.name, err)
				}
				observations = append(observations, alerts)
			})
		}
		if _, drift := migrationPingDatabaseCheck(t, t.Context()); drift != "" {
			t.Fatalf("rolled-back expression statistics faults left drift: %s", drift)
		}
	})
	pending, published := prefixes[version-1], prefixes[version]
	if pending.row == nil || published.row == nil {
		t.Fatal("a published prefix was not observed")
	}
	if pending.row[artifact.rowColumn] != "f" || published.row[artifact.rowColumn] != "t" {
		t.Fatalf("the expression statistics reads %q one version behind and %q at its version", pending.row[artifact.rowColumn], published.row[artifact.rowColumn])
	}
	// behind or at its version the object is never drift; only the lag
	// warning while the database is behind the source
	for _, head := range []int{version - 1, version} {
		wantLag := 0
		if head < server.MigrationCount() {
			wantLag = 1
		}
		alerts := prefixes[head].alerts
		if len(alerts) != wantLag || (wantLag == 1 && alerts[0].Class != "migration-behind") {
			t.Fatalf("at head %d: %d alerts, want %d migration-behind", head, len(alerts), wantLag)
		}
	}
	if len(observations) != len(faults) {
		t.Fatalf("observed %d expression statistics faults, want %d", len(observations), len(faults))
	}
	for index, alerts := range observations {
		if alert := requireAlertClass(t, alerts, "migration-schema-drift"); alert.Severity != SeverityPage || !strings.Contains(alert.Markdown(), owner) {
			t.Fatalf("%s lost its owning migration: %s", faults[index].name, alert.Markdown())
		}
	}
}
