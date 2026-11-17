// The verify cohort's rolling census contract through the public migration
// signal: a code head ahead of the database is migration lag, never schema
// drift, and from the migration's version every published shape fault is
// drift it owns.
package monitor

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/urnetwork/server"
)

// A database one version behind the code reads the rolling census as absent
// and only warns that it is behind; once the database reaches its version, an
// absent rolling census pages, and a present one raises nothing but lag.
func TestMigrationsVerifyCohortRollingPendingWarnsWithoutPaging(t *testing.T) {
	version := server.VerifyCohortRollingSchemaMigrationVersion()
	if version == 0 {
		t.Fatal("the rolling census migration is not in the migration list")
	}
	artifact := migrationCurrentSuffixTestContract(t, version).artifact
	if artifact.rowColumn != version-589 || artifact.removedVersion != 0 {
		t.Fatalf("rolling census contract is %+v", artifact)
	}
	owner := fmt.Sprintf("%s@v%d", artifact.name, version)
	for _, test := range []struct {
		name           string
		head           int
		present, drift bool
	}{
		{name: "pending rolling census", head: version - 1},
		{name: "missing published rolling census", head: version, drift: true},
		{name: "published rolling census", head: version, present: true},
	} {
		row := syntheticMigrationArtifactRow(test.head)
		row[artifact.rowColumn] = "f"
		if test.present {
			row[artifact.rowColumn] = "t"
		}
		source := &syntheticSource{postgresFn: func(query string) ([]Row, error) {
			if strings.Contains(query, "FROM migration_catalog") {
				return syntheticMigrationCatalogRows(test.head), nil
			}
			return []Row{row}, nil
		}}
		alerts, err := NewMigrationsSignal().Run(context.Background(), syntheticSettings(source))
		if err != nil {
			t.Fatal(err)
		}
		verifyCohortCensusRequireAlerts(t, test.name, test.head, alerts, test.drift, owner)
	}
}

// The contract pins every column and the state table's key it publishes, so
// a rolled-back shape of any of them is drift the rolling census owns.
func TestMigrationsVerifyCohortRollingContractNamesItsArtifacts(t *testing.T) {
	query := verifyCohortRollingMigrationContract.query
	for _, want := range []string{
		"sn_head_summary", "'census_providers'", "'bound_clients'", "'bound_clients_observed'",
		"verify_cohort_census", "'last_epoch'",
		"verify_cohort_state", "'deployment_key'", "'rolling_from_epoch'", "'rolling_since'", "PRIMARY KEY (deployment_key)",
	} {
		if !strings.Contains(query, want) {
			t.Fatalf("the rolling census contract does not pin %s", want)
		}
	}
}
