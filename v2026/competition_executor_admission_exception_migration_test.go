// The operational exception does not rewrite scoring, evidence or clocks.
package server

import (
	"strings"
	"testing"
)

func TestExecutorAdmissionExceptionMigrationPreservesEvidenceAndReplacementClock(t *testing.T) {
	sql := CompetitionExecutorAdmissionExceptionMigrationSql()
	for _, forbidden := range []string{"UPDATE competition_round", "UPDATE competition_job", "UPDATE competition_executor_qualification_run", "UPDATE competition_executor_equivalence", "DROP TRIGGER", "DISABLE TRIGGER", "DELETE FROM", "JOIN competition_executor_qualification_run q"} {
		if strings.Contains(sql, forbidden) {
			t.Fatal("exception changes frozen authority", forbidden)
		}
	}
	for _, required := range []string{"PRIMARY KEY(round_id,host_id)", "approval_canonical", "receipt_canonical", "competition_executor_admission_exception_append_only", "competition_executor_admission_exception_no_truncate", "competition_executor_qualification_clock(e.round_id,e.host_id)", "receipt->'passed' IS DISTINCT FROM 'false'::jsonb", "throughput_p50_bytes_per_s", "clock_timestamp()<lease_expires_at", "NEW.created_at>q.deadline_at", "receipt_sha256=e.receipt_sha256", "control_amendment_sha256", "recovery_sha256"} {
		if !strings.Contains(sql, required) {
			t.Fatal("exception omitted an authority boundary", required)
		}
	}
}
