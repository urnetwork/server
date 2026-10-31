// SIGNALS.md §8.9: a compiled append cannot silently extend schema authority.
package monitor

import (
	"fmt"
	"strings"
	"testing"

	"github.com/urnetwork/server"
)

func TestMigrationsCatalogAuthorityRequiresContiguousExplicitContracts(t *testing.T) {
	first := migrationCurrentSuffixFirstVersion
	contracts := []snMainnetMigrationContract{
		migrationCurrentSuffix(first+2, "later", "true"),
		migrationCurrentSuffix(first, "first", "true"),
	}
	if got := migrationCatalogAuthorityHead(contracts); got != first {
		t.Fatalf("catalog gap certified head %d", got)
	}
	contracts = append(contracts, migrationCurrentSuffix(first+1, "middle", "true"))
	if got := migrationCatalogAuthorityHead(contracts); got != first+2 {
		t.Fatalf("complete reordered catalog head %d", got)
	}
	if got := migrationCatalogAuthorityHead(nil); got != first-1 {
		t.Fatalf("absent contracts certified head %d", got)
	}
	if got := migrationCatalogAuthorityHead(migrationCurrentSuffixContracts); got != server.MigrationCount() {
		t.Fatalf("current source catalog head %d differs from published source", got)
	}
}

// A private immutable probe catalog simulates the next compiled append whose
// schema contract was omitted. The public adapter and real reducers stay used;
// no global catalog, server migration list or live source is changed.
func TestMigrationsCompiledHeadBeyondCatalogStaysUnknown(t *testing.T) {
	head := server.MigrationCount()
	contracts := make([]snMainnetMigrationContract, 0, len(migrationCurrentSuffixContracts))
	for _, contract := range migrationCurrentSuffixContracts {
		if contract.artifact.requiredVersion < head {
			contracts = append(contracts, contract)
		}
	}
	if migrationCatalogAuthorityHead(contracts) != head-1 {
		t.Fatal("fixture did not remove exactly the catalog successor")
	}
	for _, control := range []struct {
		name                          string
		failedArtifact, identityFault bool
	}{
		{name: "older predicates pass"},
		{name: "successor legitimately replaces old artifact", failedArtifact: true},
		{name: "immutable prefix fault remains independently actionable", identityFault: true},
	} {
		t.Run(control.name, func(t *testing.T) {
			signal := NewMigrationsSignal().(*signalAdapter)
			signal.probe = migrationsProbe{catalogContracts: contracts}
			row := syntheticMigrationArtifactRow(head)
			if control.failedArtifact {
				row = syntheticMigrationMissingArtifactRow(t, head, migrationFutureSchemaArtifact)
			}
			catalog := syntheticMigrationCatalogRows(head)
			if control.identityFault {
				catalog[0][1] = strings.Repeat("f", 64)
			}
			alerts := migrationFutureSchemaAlerts(t, signal, control.name, row, catalog)
			unknown := requireAlertClass(t, alerts, "cannot-observe")
			want := fmt.Sprintf("db_version=%d code_required_version=%d catalog_known_version=%d", head, head, head-1)
			if !strings.Contains(unknown.Observed, want) || unknown.Severity != SeverityWarn || !unknown.unobservable {
				t.Fatal("compiled source head falsely extended catalog authority")
			}
			wantDrift := 0
			if control.identityFault {
				wantDrift = 1
			}
			if requireAlertClassCount(alerts, "migration-schema-drift") != wantDrift {
				t.Fatal("live artifact ambiguity or immutable fault lost its authority")
			}
			if requireAlertClassCount(alerts, "migration-behind") != 0 {
				t.Fatal("catalog gap fabricated numeric migration lag")
			}
		})
	}
	// A genuinely lagging database retains the compiled server requirement even
	// when its own live schema is inside the conservative catalog boundary.
	signal := NewMigrationsSignal().(*signalAdapter)
	signal.probe = migrationsProbe{catalogContracts: contracts}
	alerts := migrationFutureSchemaAlerts(t, signal, "real numeric lag", syntheticMigrationArtifactRow(head-1), syntheticMigrationCatalogRows(head-1))
	if requireAlertClassCount(alerts, "migration-behind") != 1 || requireAlertClassCount(alerts, "cannot-observe") != 0 {
		t.Fatal("catalog authority changed real compiled migration lag")
	}
}
