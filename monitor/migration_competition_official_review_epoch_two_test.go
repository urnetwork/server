// Exact catalog ownership preserves the complete predecessor while retiring
// only its old bodies at the appended reviewer-only migration boundary.
package monitor

import (
	"context"
	"strings"
	"testing"

	"github.com/urnetwork/server"
)

// Only this registered append may take over the reviewed recovery lineage.
func migrationEpochTwoOfficialTestOwner(t testing.TB) migrationArtifact {
	t.Helper()
	version := server.CompetitionOfficialReviewEpochTwoMigrationVersion()
	previous := server.CompetitionSecondRecoveryMigrationVersion()
	current := migrationCurrentSuffixTestContract(t, version).artifact
	if version <= previous || migrationCurrentSuffixTestContract(t, previous).artifact.removedVersion != version ||
		current.name != "epoch-two official reviewer substitution with retained complete custody" ||
		current.removedVersion != server.CompetitionOfficialReviewEpochTwoAstra62MigrationVersion() {
		t.Fatal("epoch-two reviewer catalog owner has an unreviewed boundary")
	}
	return current
}

func migrationEpochTwoAstra62TestOwner(t testing.TB) migrationArtifact {
	t.Helper()
	version := server.CompetitionOfficialReviewEpochTwoAstra62MigrationVersion()
	previous := migrationEpochTwoOfficialTestOwner(t)
	current := migrationCurrentSuffixTestContract(t, version).artifact
	if version <= previous.requiredVersion || previous.removedVersion != version ||
		current.name != "epoch-two Astra 6.2 reviewer provenance with retained complete custody" || current.removedVersion != 0 {
		t.Fatal("epoch-two Astra 6.2 catalog owner has an unreviewed boundary")
	}
	return current
}

// The successor retains all byte-pinned recovery/source/native predicates and
// gains only the new epoch-two functions and five exact replacement bodies.
func TestMigrationsEpochTwoOfficialReviewRetainsCompleteCustody(t *testing.T) {
	version := server.CompetitionOfficialReviewEpochTwoMigrationVersion()
	prior := migrationCurrentSuffixTestContract(t, server.CompetitionSecondRecoveryMigrationVersion())
	current := migrationCurrentSuffixTestContract(t, version)
	if prior.artifact.removedVersion != version || current.artifact.removedVersion != server.CompetitionOfficialReviewEpochTwoAstra62MigrationVersion() {
		t.Fatal("reviewer amendment has incorrect custody lifetime")
	}
	old := server.CompetitionOfficialReviewBodiesSql()
	bodies := server.CompetitionOfficialReviewEpochTwoBodiesSql()
	expected := competitionSecondRecoveryMigrationContract.query
	for _, replacement := range []struct{ old, next string }{
		{old: old.RequestAuthorized, next: bodies.RequestAuthorized}, {old: old.AuthorizationGuard, next: bodies.AuthorizationGuard},
		{old: old.ReviewIdentityGuard, next: bodies.ReviewIdentityGuard}, {old: old.WinnerSourceAuthorized, next: bodies.WinnerSourceAuthorized},
		{old: old.WinnerReviewGuard, next: bodies.WinnerReviewGuard},
	} {
		pin := snMainnetMigrationLiteral(replacement.old)
		if strings.Count(expected, pin) != 1 {
			t.Fatal("missing inherited body")
		}
		expected = strings.Replace(expected, pin, snMainnetMigrationLiteral(replacement.next), 1)
	}
	if !strings.Contains(current.query, expected) {
		t.Fatal("new catalog owner omitted inherited authority")
	}
	for _, body := range []string{old.FindingsValid, bodies.FindingsValid, bodies.PrereviewBinding} {
		if !strings.Contains(current.query, "p.prosrc="+snMainnetMigrationLiteral(body)) {
			t.Fatal("new or legacy body is not byte pinned")
		}
	}
	for _, name := range []string{"competition_second_recovery_commit_guard()", "competition_v2_source_authorized", "competition_review_handoff_completion_guard()", "competition_v2_winner_checkpoint_guard()", "competition_v2_production_benefit_review_guard()"} {
		if !strings.Contains(current.query, name) {
			t.Fatal("inherited boundary absent", name)
		}
	}
}

func TestMigrationsEpochTwoOfficialAstra62ChangesOnlyFindingsCustody(t *testing.T) {
	previous := migrationCurrentSuffixTestContract(t, migrationEpochTwoOfficialTestOwner(t).requiredVersion)
	current := migrationCurrentSuffixTestContract(t, migrationEpochTwoAstra62TestOwner(t).requiredVersion)
	old := "p.prosrc=" + snMainnetMigrationLiteral(server.CompetitionOfficialReviewEpochTwoBodiesSql().FindingsValid)
	next := "p.prosrc=" + snMainnetMigrationLiteral(server.CompetitionOfficialReviewEpochTwoAstra62FindingsBodySql())
	if strings.Count(current.query, next) != 1 || strings.Contains(current.query, old) || strings.Replace(current.query, next, old, 1) != previous.query {
		t.Fatal("reviewer ID append changed or omitted unrelated catalog custody")
	}
}

// Healthy complete prefixes and rollback-contained function drift exercise the
// actual monitor, not an always-true synthetic catalog result.
func TestMigrationsEpochTwoOfficialReviewPostgresBoundariesAndDrift(t *testing.T) {
	(&server.TestEnv{ApplyDbMigrations: false, RerunCount: 0}).Run(t, func(tb testing.TB) {
		prior := migrationEpochTwoOfficialTestOwner(tb)
		current := migrationEpochTwoAstra62TestOwner(tb)
		checkHead := func(head int) {
			tb.Helper()
			server.ApplyDbMigrationsUpTo(tb.Context(), head)
			if _, drift := migrationPingDatabaseCheck(tb, tb.Context()); drift != "" {
				tb.Fatal("healthy epoch-two review prefix reports drift", head, drift)
			}
		}
		checkFault := func(owner migrationArtifact, statement string) {
			tb.Helper()
			snMainnetMigrationFaultTx(tb, func(ctx context.Context, tx server.PgTx) {
				if _, err := tx.Exec(ctx, statement); err != nil {
					tb.Fatal(err)
				}
				alerts, err := NewMigrationsSignal().Run(ctx, syntheticSettings(&snMainnetMigrationTxSource{tx: tx}))
				if err != nil {
					tb.Fatal(err)
				}
				if alert := requireAlertClass(tb, alerts, "migration-schema-drift"); !strings.Contains(alert.Markdown(), owner.name) {
					tb.Fatal("new or inherited drift lost its exact catalog owner")
				}
			})
		}
		checkHead(prior.requiredVersion - 1)
		checkHead(prior.requiredVersion)
		for _, statement := range []string{
			`CREATE OR REPLACE FUNCTION competition_official_review_epoch_two_binding(round_uuid uuid,request jsonb,imported_at timestamp) RETURNS boolean LANGUAGE sql STABLE AS 'SELECT true'`,
			`CREATE OR REPLACE FUNCTION competition_official_review_epoch_two_findings_valid(request jsonb) RETURNS boolean LANGUAGE plpgsql IMMUTABLE PARALLEL SAFE AS 'BEGIN RETURN true; END'`,
			`CREATE OR REPLACE FUNCTION competition_v2_winner_checkpoint_guard() RETURNS trigger LANGUAGE plpgsql AS 'BEGIN RETURN NEW; END'`,
			`ALTER TABLE competition_job_reevaluation DISABLE TRIGGER competition_job_reevaluation_append_only`,
		} {
			checkFault(prior, statement)
		}
		checkHead(current.requiredVersion - 1)
		checkHead(current.requiredVersion)
		checkFault(current, `CREATE OR REPLACE FUNCTION competition_official_review_epoch_two_findings_valid(request jsonb) RETURNS boolean LANGUAGE plpgsql IMMUTABLE PARALLEL SAFE AS $legacy_findings$`+
			server.CompetitionOfficialReviewEpochTwoBodiesSql().FindingsValid+`$legacy_findings$`)
	})
}
