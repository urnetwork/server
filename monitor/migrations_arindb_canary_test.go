package monitor

import (
	"os"
	"strings"
	"testing"

	"github.com/urnetwork/server"
)

// The pinned body is byte for byte the function the 837 migration creates, so
// a healthy schema can never read as drift and an edited migration cannot
// pass under the old pin.
func TestArindbCanaryGuardBodyIsThePublishedMigrationBody(t *testing.T) {
	source, err := os.ReadFile("../db_arindb_canary.go")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(source), "AS $arin_old_quality$"+arinOldQualityGuardFunctionBody+"$arin_old_quality$;") {
		t.Fatal("the pinned old-slot guard body differs from the 837 migration's function body")
	}
}

// The canary's schema is one append-only migration with one contract, at the
// head this source tree requires.
func TestArindbCanaryContractIsTheCurrentHead(t *testing.T) {
	contract := migrationCurrentSuffixContracts[len(migrationCurrentSuffixContracts)-1]
	if contract.artifact.requiredVersion != 837 || contract.artifact.removedVersion != 0 || server.MigrationCount() < 837 {
		t.Fatalf("canary contract %+v at head %d", contract.artifact, server.MigrationCount())
	}
	for _, want := range []string{
		"arin_old_quality_verified", "arin_old_database_build_epoch", "arin_old_quality_write_token",
		"to_regclass('public.network_client_location_reliability')", "'arin_old_epoch'", "'arin_epoch'",
		"network_client_location_arin_old_quality_guard", "actual.tgtype=23 AND actual.tgenabled='O'",
	} {
		if !strings.Contains(contract.query, want) {
			t.Errorf("canary contract does not check %s", want)
		}
	}
}
