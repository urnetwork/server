package monitor

import (
	"os"
	"strings"
	"testing"

	"github.com/urnetwork/server"
)

// The pinned body is byte for byte the function the location migration
// creates, so a healthy schema can never read as drift and an edited migration
// cannot pass under the old pin.
func TestArindbCanaryGuardBodyIsThePublishedMigrationBody(t *testing.T) {
	source, err := os.ReadFile("../db_arindb_canary.go")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(source), "AS $arin_old_quality$"+arinOldQualityGuardFunctionBody+"$arin_old_quality$;") {
		t.Fatal("the pinned old-slot guard body differs from the location migration's function body")
	}
}

// The canary's schema is two append-only migrations, each with one contract
// over the one table it locks: the rollup's slots, then the connection slots
// and their guard. Later appends do not change these published contracts.
func TestArindbCanaryContractsKeepPublishedOrder(t *testing.T) {
	rollupIndex := -1
	for index, contract := range migrationCurrentSuffixContracts {
		if contract.artifact.requiredVersion == arindbCanaryRollupMigrationVersion {
			rollupIndex = index
			break
		}
	}
	if rollupIndex < 0 || rollupIndex+1 >= len(migrationCurrentSuffixContracts) {
		t.Fatal("the published canary pair is absent from the suffix catalog")
	}
	rollup, location := migrationCurrentSuffixContracts[rollupIndex], migrationCurrentSuffixContracts[rollupIndex+1]
	if rollup.artifact.requiredVersion != arindbCanaryRollupMigrationVersion || location.artifact.requiredVersion != arindbCanaryLocationMigrationVersion ||
		rollup.artifact.removedVersion != 0 || location.artifact.removedVersion != 0 || server.MigrationCount() < arindbCanaryLocationMigrationVersion {
		t.Fatalf("canary contracts %+v and %+v at head %d", rollup.artifact, location.artifact, server.MigrationCount())
	}
	for _, c := range []struct {
		contract  snMainnetMigrationContract
		checks    []string
		notChecks []string
	}{
		{
			contract:  rollup,
			checks:    []string{"to_regclass('public.network_client_location_reliability')", "'arin_old_epoch'", "'arin_epoch'", "'arin_old_non_quality'"},
			notChecks: []string{"to_regclass('public.network_client_location')", "network_client_location_arin_old_quality_guard"},
		},
		{
			contract: location,
			checks: []string{
				"to_regclass('public.network_client_location')", "arin_old_quality_verified", "arin_old_database_build_epoch",
				"arin_old_quality_write_token", "network_client_location_arin_old_quality_guard", "actual.tgtype=23 AND actual.tgenabled='O'",
			},
			notChecks: []string{"to_regclass('public.network_client_location_reliability')"},
		},
	} {
		for _, want := range c.checks {
			if !strings.Contains(c.contract.query, want) {
				t.Errorf("canary contract %d does not check %s", c.contract.artifact.requiredVersion, want)
			}
		}
		for _, unwanted := range c.notChecks {
			if strings.Contains(c.contract.query, unwanted) {
				t.Errorf("canary contract %d checks %s, which its migration does not create", c.contract.artifact.requiredVersion, unwanted)
			}
		}
	}
}
