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

// 900 publishes exactly three dollar-quoted bodies. The source authorization
// is the reviewed-repository body it replaces with exactly the two published
// insertions, so the monitor's pin cannot accept a body 900 never wrote.
func TestTrustedInstrumentBodiesAreDelimited(t *testing.T) {
	migration := competitionTrustedInstrumentMigration()
	for _, tag := range []string{"$instrument_lock$", "$instrument$", "$source$"} {
		if count := strings.Count(migration, tag); count != 2 {
			t.Fatalf("900 tag %s occurs %d times", tag, count)
		}
	}
	if count := strings.Count(migration, "CREATE OR REPLACE FUNCTION"); count != 3 {
		t.Fatalf("900 creates %d functions, want the two instrument functions and the source authorization", count)
	}
	lockDigest, productIntent, sourceAuthorized := CompetitionTrustedInstrumentBodiesSql()
	if !strings.Contains(lockDigest, `"development_snapshot":false`) || strings.Contains(lockDigest, "BEGIN") {
		t.Fatalf("900 lock digest body: %q", lockDigest)
	}
	if !strings.HasPrefix(productIntent, "\nDECLARE previous jsonb;") || !strings.HasSuffix(productIntent, "RETURN projected;\nEND ") {
		t.Fatalf("900 product intent body is not the published function body")
	}
	projection := "\n intent:=competition_v2_instrument_product_intent(intent,env,competition,season);"
	predecessor := CompetitionRepositoryPatchSourceBodySql()
	if strings.Count(sourceAuthorized, projection) != 1 || strings.Count(sourceAuthorized, competitionTrustedInstrumentPathBaseSql) != 1 {
		t.Fatal("900 source authorization lacks its projection or path-base check")
	}
	restored := strings.Replace(strings.Replace(sourceAuthorized, projection, "", 1), competitionTrustedInstrumentPathBaseSql, "", 1)
	if sourceAuthorized == predecessor || restored != predecessor {
		t.Fatal("900 source authorization is not the reviewed-repository body with the two published insertions")
	}
}
