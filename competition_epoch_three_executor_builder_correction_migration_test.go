package server

import (
	"strings"
	"testing"
)

func TestEpochThreeExecutorBuilderCorrectionPreservesAmendmentGuards(t *testing.T) {
	sql := competitionEpochThreeExecutorBuilderCorrectionSql()
	for _, required := range []string{
		"CREATE OR REPLACE FUNCTION competition_executor_control_amendment_guard()",
		"epoch_round.epoch_number=3 AND NEW.round_id='01a1f4d0-47c9-ac2d-ce45-55ef159a9b41'::uuid",
		"host->>'builder_sha256' IS DISTINCT FROM host->>'previous_builder_sha256'",
		"clock_timestamp()<lease_expires_at", "EXISTS(SELECT 1 FROM competition_executor_qualification_run",
		"attempt_count<>0 OR started_at IS NOT NULL OR execution_deadline_at IS NOT NULL",
		"host->>'qualification_sha256' IS NOT DISTINCT FROM previous->>'qualification_sha256'",
	} {
		if !strings.Contains(sql, required) {
			t.Fatalf("missing executor amendment guard: %s", required)
		}
	}
	if strings.Count(sql, "CREATE OR REPLACE FUNCTION") != 1 || strings.Contains(sql, "DROP TRIGGER") {
		t.Fatal("migration affects more than the executor amendment guard")
	}
}
