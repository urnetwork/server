package server

import (
	"strings"
	"testing"
)

// The monitor pins these bodies byte for byte, so each must come from exactly
// one dollar-quoted function in its published migration.
func TestPublishedFunctionBodiesAreDelimited(t *testing.T) {
	migration := competitionWinnerReviewMigration()
	for _, tag := range []string{"$source$", "$guard$", "$winner_source$", "$winner_review$", "$winner_checkpoint$"} {
		if count := strings.Count(migration, tag); count != 2 {
			t.Fatalf("855 tag %s occurs %d times", tag, count)
		}
	}
	sourceAuthorized, roundInsertGuard, winnerSourceAuthorized, winnerReviewGuard, winnerCheckpointGuard := CompetitionWinnerReviewBodiesSql()
	for name, body := range map[string]string{
		"source authorized":        sourceAuthorized,
		"round insert guard":       roundInsertGuard,
		"winner source authorized": winnerSourceAuthorized,
		"winner review guard":      winnerReviewGuard,
		"winner checkpoint guard":  winnerCheckpointGuard,
	} {
		if !strings.Contains(body, "BEGIN") || !strings.Contains(body, "END") {
			t.Fatalf("855 %s body is not a function body: %q", name, body)
		}
	}
	if !strings.Contains(roundInsertGuard, "elected_winner_sol61max_v1") {
		t.Fatalf("855 round insert guard is not the replaced body")
	}
	if !strings.Contains(sourceAuthorized, "competition_v2_winner_source_authorized(intent,predecessor_id)") {
		t.Fatalf("855 source authorization is not the replaced body")
	}

	if count := strings.Count(contractIdFloorSql, "$$"); count != 2 {
		t.Fatalf("856 delimiter occurs %d times", count)
	}
	if body := ContractIdFloorBodySql(); !strings.Contains(body, "lpad(to_hex(floor(extract(epoch FROM t) * 1000)::bigint), 12, '0')") {
		t.Fatalf("856 body: %q", body)
	}

	if count := strings.Count(contractCloseReportEvidenceRetentionGuardSql, "$close_report_guard$"); count != 2 {
		t.Fatalf("857 delimiter occurs %d times", count)
	}
	if body := ContractCloseEvidenceRetentionGuardBodySql(); !strings.Contains(body, "interval '91 days'") || !strings.Contains(body, "urnetwork.close_evidence_retention") {
		t.Fatalf("857 body: %q", body)
	}
}
