package server

import (
	"strings"
	"testing"
)

// Rendering must preserve all legacy guards and add only explicit handoff
// branches. The full native controller fixture exercises the appended DDL.
func TestCompetitionReviewHandoffMigrationKeepsLegacyAndIndependentAuthority(t *testing.T) {
	sql := competitionReviewHandoffMigration()
	if !strings.Contains(sql, "NOT BETWEEN 2 AND 384") || strings.Contains(sql, "NOT BETWEEN 2 AND 4096") {
		t.Fatal("database does not enforce approved 384-call ceiling")
	}
	for _, predicate := range []string{
		"NOT COALESCE(proposal->>'reward_disposition' IN ('approved','disqualified'),false)",
		"NOT COALESCE(competition_v2_winner_review_provider('elected_winner_snow_union_v2',proposal->>'reviewer_model',proposal->>'reviewer_reasoning'),false)",
		"NOT COALESCE(competition_v2_snow_review_findings_valid_v2(proposal),false)",
	} {
		if !strings.Contains(sql, predicate) {
			t.Fatal("pending proposal has a nullable authority predicate", predicate)
		}
	}
	for _, required := range []string{"CREATE TABLE competition_review_handoff (", "UNIQUE(round_id,decision_sha256)", "UNIQUE(handoff_id,call_id,kind)", "competition_review_handoff_observed", "competition_review_operator_actor", "timezone('UTC',clock_timestamp())", "competition_review_handoff_clock()>=owner.deadline", "claims<>receipts", "'call_ids'->>claims IS DISTINCT FROM NEW.call_id", "deadline cannot reset", "competition_v2_snow_review_findings_valid_for_policy", "competition_v2_winner_review_provider", "checkpoint IS DISTINCT FROM actual_checkpoint", "make_interval(secs=>(policy->>'winner_review_timeout_seconds')::integer)", "completion.completion_json->'proposal' IS DISTINCT FROM request", "interval '24 hours'"} {
		if !strings.Contains(sql, required) {
			t.Fatal("handoff migration omitted authority boundary", required)
		}
	}
	for _, forbidden := range []string{"UPDATE competition_", "DELETE FROM competition_", "DROP TABLE", "ALTER TABLE", "CREATE OR REPLACE FUNCTION competition_v2_source_authorized", "CREATE OR REPLACE FUNCTION competition_v2_snow_review_findings_valid"} {
		if strings.Contains(sql, forbidden) {
			t.Fatal("handoff migration broadened scope", forbidden)
		}
	}
	if strings.Contains(competitionWinnerReviewGuards, "handoff_id") || strings.Contains(competitionSnowTwoMemberWinnerReviewMigration(), "handoff_id") {
		t.Fatal("published legacy migration bytes changed")
	}
	count := 0
	for _, migration := range migrations {
		if item, ok := migration.(*SqlMigration); ok && item.sql == sql {
			count++
		}
	}
	if count != 1 {
		t.Fatal("additive migration not registered exactly once", count)
	}
}

// The upstream migration already owns its published ordinal. Integration may
// append the review schema, never insert before or silently renumber that owner.
func TestCompetitionReviewHandoffMigrationPreservesPublishedTlsPosition(t *testing.T) {
	if ProviderEgressTlsVerdictVersion() != 945 {
		t.Fatal("published upstream TLS verdict migration was reordered")
	}
	want := competitionReviewHandoffMigration()
	version := 0
	for index, migration := range migrations {
		if sql, ok := migration.(*SqlMigration); ok && sql.sql == want {
			version = index + 1
		}
	}
	if version != 946 {
		t.Fatal("review handoff must append after the existing upstream migration", version)
	}
}
