// Eligibility-monitor custody is tested before and after its appended migration,
// including restored old bodies, weakened new guards and inherited authority.
package monitor

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/urnetwork/server"
)

// Neither an early waiver nor a missing successor can hide exact function
// drift. Each replaced predecessor remains required until its registered boundary.
func TestMigrationsProductionBenefitSuccessorContractCustody(t *testing.T) {
	version := server.CompetitionProductionBenefitMigrationVersion()
	contract := migrationCurrentSuffixTestContract(t, version)
	retirement := server.CompetitionIntakeFirstMigrationVersion()
	if version <= 900 || retirement <= version || contract.artifact.rowColumn != version-589 || contract.artifact.removedVersion != retirement || migrationCurrentSuffixTestContract(t, 900).artifact.removedVersion != version {
		t.Fatal("production-benefit custody has wrong version or lifetime")
	}
	valid, guard, findingsV1, findingsV2 := server.CompetitionProductionBenefitBodiesSql()
	for _, pin := range []string{"public.competition_v2_production_benefit_review_valid(jsonb)", "public.competition_v2_production_benefit_review_guard()", "competition_v2_production_benefit_review", "public.competition_v2_instrument_product_intent(jsonb,text,text,text)", "competition_v2_winner_review_before_insert", "competition_v2_winner_checkpoint_before_insert", "competition_v2_source_authorized", "p.proparallel='s'", "p.prosrc=" + snMainnetMigrationLiteral(valid), "p.prosrc=" + snMainnetMigrationLiteral(guard), "p.prosrc=" + snMainnetMigrationLiteral(findingsV1), "p.prosrc=" + snMainnetMigrationLiteral(findingsV2)} {
		if !strings.Contains(contract.query, pin) {
			t.Fatalf("production-benefit custody omitted %.100q", pin)
		}
	}
	for _, fixture := range []struct {
		head, bad int
		drift     bool
	}{
		{head: version - 1, bad: 900, drift: true}, {head: version, bad: 900}, {head: version, bad: version, drift: true},
	} {
		row := syntheticMigrationArtifactRow(fixture.head)
		for len(row) <= fixture.bad-589 {
			row = append(row, "t")
		}
		row[fixture.bad-589] = "f"
		source := &syntheticSource{postgresFn: func(query string) ([]Row, error) {
			if strings.Contains(query, "FROM migration_catalog") {
				return syntheticMigrationCatalogRows(fixture.head), nil
			}
			return []Row{row}, nil
		}}
		alerts, err := NewMigrationsSignal().Run(context.Background(), syntheticSettings(source))
		if err != nil || (requireAlertClassCount(alerts, "migration-schema-drift") != 0) != fixture.drift {
			t.Fatalf("head=%d bad=%d drift expected=%t: %v", fixture.head, fixture.bad, fixture.drift, err)
		}
	}
}

// The benefit predecessor remains active immediately before intake. At intake,
// its retired row cannot page; the successor's inherited custody still can.
func TestMigrationsProductionBenefitRetirementBoundaryRetainsSuccessorCustody(t *testing.T) {
	version := server.CompetitionProductionBenefitMigrationVersion()
	retirement := server.CompetitionIntakeFirstMigrationVersion()
	predecessor := migrationCurrentSuffixTestContract(t, version)
	successor := migrationCurrentSuffixTestContract(t, retirement)
	if retirement <= version || predecessor.artifact.removedVersion != retirement || (successor.artifact.removedVersion != 0 && successor.artifact.removedVersion <= retirement) {
		t.Fatal("benefit retirement must match an intake successor active at its boundary")
	}
	for _, fixture := range []struct {
		head, bad int
		drift     bool
	}{
		{head: retirement - 1, bad: version, drift: true},
		{head: retirement, bad: version},
		{head: retirement, bad: retirement, drift: true},
	} {
		row := syntheticMigrationArtifactRow(fixture.head)
		badContract := migrationCurrentSuffixTestContract(t, fixture.bad)
		row[badContract.artifact.rowColumn] = "f"
		source := &syntheticSource{postgresFn: func(query string) ([]Row, error) {
			if strings.Contains(query, "FROM migration_catalog") {
				return syntheticMigrationCatalogRows(fixture.head), nil
			}
			return []Row{row}, nil
		}}
		alerts, err := NewMigrationsSignal().Run(context.Background(), syntheticSettings(source))
		want := 0
		if fixture.drift {
			want = 1
		}
		if err != nil || requireAlertClassCount(alerts, "migration-schema-drift") != want {
			t.Fatalf("head=%d fault=%d want=%d migration drift alerts: %v", fixture.head, fixture.bad, want, err)
		}
		if fixture.drift {
			owner := fmt.Sprintf("%s@v%d", badContract.artifact.name, fixture.bad)
			for _, alert := range alerts {
				if alert.Class == "migration-schema-drift" && !strings.Contains(alert.Markdown(), owner) {
					t.Fatalf("head=%d fault=%d lost exact owning custody", fixture.head, fixture.bad)
				}
			}
		}
	}
}

// Actual migrated catalogs, not a name-only mock, establish the healthy
// controls. Every fault rolls back inside the existing private fixture.
func TestMigrationsProductionBenefitPublishedPrefixesAndGuardFaults(t *testing.T) {
	version := server.CompetitionProductionBenefitMigrationVersion()
	contract := migrationCurrentSuffixTestContract(t, version)
	_, _, _, _, _, oldV1, oldV2, _ := server.CompetitionSnowTwoMemberWinnerReviewBodiesSql()
	owner := fmt.Sprintf("%s@v%d", contract.artifact.name, version)
	(&server.TestEnv{ApplyDbMigrations: false, RerunCount: 0}).Run(t, func(tb testing.TB) {
		for _, head := range []int{version - 1, version} {
			server.ApplyDbMigrationsUpTo(tb.Context(), head)
			row, drift := migrationPingDatabaseCheck(tb, tb.Context())
			want := "f"
			if head == version {
				want = "t"
			}
			if drift != "" || row[contract.artifact.rowColumn] != want {
				tb.Fatalf("eligibility prefix %d=%q want=%q drift=%s", head, row[contract.artifact.rowColumn], want, drift)
			}
		}
		for _, fault := range []struct{ name, sql string }{
			{name: "old v1 findings restored", sql: `CREATE OR REPLACE FUNCTION competition_v2_snow_review_findings_valid(request jsonb) RETURNS boolean LANGUAGE plpgsql IMMUTABLE PARALLEL SAFE AS $old$` + oldV1 + `$old$`},
			{name: "old v2 findings restored", sql: `CREATE OR REPLACE FUNCTION competition_v2_snow_review_findings_valid_v2(request jsonb) RETURNS boolean LANGUAGE plpgsql IMMUTABLE PARALLEL SAFE AS $old$` + oldV2 + `$old$`},
			{name: "eligibility helper weakened", sql: `CREATE OR REPLACE FUNCTION competition_v2_production_benefit_review_valid(request jsonb) RETURNS boolean LANGUAGE plpgsql IMMUTABLE PARALLEL SAFE AS $$ BEGIN RETURN true; END $$`},
			{name: "eligibility metadata changed", sql: `ALTER FUNCTION competition_v2_production_benefit_review_valid(jsonb) PARALLEL UNSAFE`},
			{name: "eligibility privilege changed", sql: `ALTER FUNCTION competition_v2_production_benefit_review_guard() SECURITY DEFINER`},
			{name: "eligibility trigger disabled", sql: `ALTER TABLE competition_integrity_review DISABLE TRIGGER competition_v2_production_benefit_review`},
			{name: "eligibility trigger narrowed", sql: `DROP TRIGGER competition_v2_production_benefit_review ON competition_integrity_review; CREATE TRIGGER competition_v2_production_benefit_review BEFORE UPDATE ON competition_integrity_review FOR EACH ROW EXECUTE FUNCTION competition_v2_production_benefit_review_guard()`},
			{name: "retained source authority weakened", sql: `CREATE OR REPLACE FUNCTION competition_v2_source_authorized(intent jsonb,env text,competition text,season text) RETURNS void LANGUAGE plpgsql AS $$ BEGIN RETURN; END $$`},
			{name: "retained winner checkpoint disabled", sql: `ALTER TABLE competition_supervisor_transition DISABLE TRIGGER competition_v2_winner_checkpoint_before_insert`},
		} {
			snMainnetMigrationFaultTx(tb, func(ctx context.Context, tx server.PgTx) {
				if _, err := tx.Exec(ctx, fault.sql); err != nil {
					tb.Fatalf("apply %s: %v", fault.name, err)
				}
				alerts, err := NewMigrationsSignal().Run(ctx, syntheticSettings(&snMainnetMigrationTxSource{tx: tx}))
				if err != nil {
					tb.Fatal(err)
				}
				matched := false
				for _, alert := range alerts {
					matched = matched || alert.Class == "migration-schema-drift" && strings.Contains(alert.Markdown(), owner)
				}
				if !matched {
					tb.Fatalf("%s lost production-benefit successor custody", fault.name)
				}
			})
		}
		if _, drift := migrationPingDatabaseCheck(tb, tb.Context()); drift != "" {
			tb.Fatal("rolled-back eligibility faults left drift", drift)
		}
	})
}
