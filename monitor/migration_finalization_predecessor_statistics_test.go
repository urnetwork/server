// A known successor cannot certify a missing predecessor. Native object
// faults and unknown future heads retain their separate authority boundaries.
package monitor

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/urnetwork/server"
)

// The predecessor has an operative exact predicate, not a TRUE placeholder.
// Omitting it must still stop authority before 926 even when 927 is present.
func TestMigrationsUnresolvedStatisticsClosesOnlyItsCatalogGap(t *testing.T) {
	version := server.TransferContractUnresolvedPredicateStatisticsVersion()
	contract := migrationCurrentSuffixTestContract(t, version)
	if version != 926 || contract.query != transferContractUnresolvedStatisticsArtifactQuery || contract.artifact.rowColumn != version-589 || contract.artifact.removedVersion != 0 {
		t.Fatal("published statistics contract lost its exact position", version, contract.artifact)
	}
	if head := migrationCatalogAuthorityHead(migrationCurrentSuffixContracts); head != server.MigrationCount() {
		t.Fatal("qualified predecessor and successor did not form a complete catalog", head)
	}
	without := make([]snMainnetMigrationContract, 0, len(migrationCurrentSuffixContracts)-1)
	for _, candidate := range migrationCurrentSuffixContracts {
		if candidate.artifact.requiredVersion != version {
			without = append(without, candidate)
		}
	}
	if head := migrationCatalogAuthorityHead(without); head != version-1 {
		t.Fatal("later finalization contract silently bridged an unknown predecessor", head)
	}
	// Exercise the public verdict as well as the contiguous-head census:
	// only a complete known catalog can promote a live definition mismatch.
	head := server.MigrationCount()
	for _, test := range []struct {
		name                string
		dbHead, catalogHead int
		contracts           []snMainnetMigrationContract
		wantClass           string
	}{
		{name: "internal predecessor gap", dbHead: head, catalogHead: version - 1, contracts: without, wantClass: "cannot-observe"},
		{name: "known statistics drift", dbHead: head, catalogHead: head, wantClass: "migration-schema-drift"},
		{name: "unknown future head", dbHead: head + 1, catalogHead: head, wantClass: "cannot-observe"},
	} {
		row := syntheticMigrationArtifactRow(test.dbHead)
		row[contract.artifact.rowColumn] = "f"
		catalog := syntheticMigrationCatalogRows(head)
		if head < test.dbHead {
			catalog = migrationFutureSchemaCatalogRows()
		}
		signal := NewMigrationsSignal().(*signalAdapter)
		signal.probe = migrationsProbe{catalogContracts: test.contracts}
		alerts := migrationFutureSchemaAlerts(t, signal, test.name, row, catalog)
		alert := requireAlertClass(t, alerts, test.wantClass)
		if len(alerts) != 1 {
			t.Fatal("catalog authority emitted an unrelated verdict", test.name, alerts)
		}
		if test.wantClass == "cannot-observe" {
			if !alert.unobservable || alert.Severity != SeverityWarn || !strings.Contains(alert.Observed, fmt.Sprintf("catalog_known_version=%d", test.catalogHead)) {
				t.Fatal("catalog gap or unknown future head gained live-schema authority", test.name, alert)
			}
		} else if alert.Severity != SeverityPage || !strings.Contains(alert.Observed, fmt.Sprintf("%s@v%d", contract.artifact.name, version)) {
			t.Fatal("known statistics corruption lost its exact owning page", alert)
		}
	}
}

// Fresh published prefixes contain no ANALYZE sample. Only the object identity,
// relation, expression census and exact opaque predicate grant schema authority.
func TestMigrationsUnresolvedStatisticsPublishedPrefixesAndNativeFaults(t *testing.T) {
	version := server.TransferContractUnresolvedPredicateStatisticsVersion()
	contract := migrationCurrentSuffixTestContract(t, version)
	owner := fmt.Sprintf("%s@v%d", contract.artifact.name, version)
	definition := `CREATE STATISTICS public.transfer_contract_unresolved_predicate ON (CASE WHEN outcome IS NULL THEN dispute = false ELSE false END) FROM public.transfer_contract`
	faults := []struct{ name, sql string }{
		{"object absent", `DROP STATISTICS public.transfer_contract_unresolved_predicate`},
		{"same name transparent predicate", `DROP STATISTICS public.transfer_contract_unresolved_predicate; CREATE STATISTICS public.transfer_contract_unresolved_predicate ON (outcome IS NULL AND dispute=false) FROM public.transfer_contract`},
		{"same name different CASE", `DROP STATISTICS public.transfer_contract_unresolved_predicate; CREATE STATISTICS public.transfer_contract_unresolved_predicate ON (CASE WHEN outcome IS NULL THEN dispute=true ELSE false END) FROM public.transfer_contract`},
		{"wrong table", `DROP STATISTICS public.transfer_contract_unresolved_predicate; CREATE TABLE synthetic_statistics_contract(outcome varchar,dispute boolean); CREATE STATISTICS public.transfer_contract_unresolved_predicate ON (CASE WHEN outcome IS NULL THEN dispute=false ELSE false END) FROM public.synthetic_statistics_contract`},
		{"wrong namespace", `CREATE SCHEMA synthetic_statistics_scope; ALTER STATISTICS public.transfer_contract_unresolved_predicate SET SCHEMA synthetic_statistics_scope`},
		{"column statistics substitute", `DROP STATISTICS public.transfer_contract_unresolved_predicate; CREATE STATISTICS public.transfer_contract_unresolved_predicate ON outcome,dispute FROM public.transfer_contract`},
		{"extra expression", `DROP STATISTICS public.transfer_contract_unresolved_predicate; CREATE STATISTICS public.transfer_contract_unresolved_predicate ON (CASE WHEN outcome IS NULL THEN dispute=false ELSE false END),(dispute IS NULL) FROM public.transfer_contract`},
	}
	(&server.TestEnv{ApplyDbMigrations: false, RerunCount: 0}).Run(t, func(t testing.TB) {
		for _, head := range []int{version - 1, version, server.CompetitionFinalizationHoldMigrationVersion()} {
			server.ApplyDbMigrationsUpTo(t.Context(), head)
			row, drift := migrationPingDatabaseCheck(t, t.Context())
			want := "f"
			if version <= head {
				want = "t"
			}
			if drift != "" || row[contract.artifact.rowColumn] != want {
				t.Fatalf("prefix%d statistics=%q want=%q drift=%s", head, row[contract.artifact.rowColumn], want, drift)
			}
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
				alert := requireAlertClass(t, alerts, "migration-schema-drift")
				if alert.Severity != SeverityPage || !strings.Contains(alert.Observed, owner) || requireAlertClassCount(alerts, "cannot-observe") != 0 {
					t.Fatalf("%s lost exact known-head statistics authority", fault.name)
				}
			})
		}
		// Recreating the exact definition is a healthy control in the same
		// private transaction, without reading or filling statistics samples.
		snMainnetMigrationFaultTx(t, func(ctx context.Context, tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `DROP STATISTICS public.transfer_contract_unresolved_predicate; `+definition))
			alerts, err := NewMigrationsSignal().Run(ctx, syntheticSettings(&snMainnetMigrationTxSource{tx: tx}))
			if err != nil || requireAlertClassCount(alerts, "migration-schema-drift") != 0 || requireAlertClassCount(alerts, "cannot-observe") != 0 {
				t.Fatal("exact unsampled statistics definition did not remain healthy", alerts, err)
			}
		})
	})
}
