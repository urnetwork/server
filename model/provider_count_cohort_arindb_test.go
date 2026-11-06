package model

import (
	"testing"
)

// The cohort export tests build their own minimal schema
// (env.ApplyDbMigrations = false, cohortTestTables) without the ARIN canary's
// rollup slots (arin_old_risk, arin_old_non_quality, arin_epoch,
// arin_old_epoch). The score export reads those slots only when the canary
// compares an old database, and which databases a process compares comes from
// its config: config/all/arindb has shipped arin-old.mmdb beside arin.mmdb
// since 2026.10.27, so on a machine with that config the export read the
// slots and panicked with `column "arin_old_risk" does not exist`. These tests
// are about the cohort scope, not the canary (arindb_canary_db_test.go covers
// it), so they compare no old database whatever the config holds, and read no
// previous decision from the shared test Redis.
func useSingleArindbCanaryForCohortTest(t *testing.T) {
	t.Helper()
	useArindbCanaryPairForTest(t, testArindbDbLatestEpoch, 0)
	useTestArindbCanaryDecisionStore(t)
}
