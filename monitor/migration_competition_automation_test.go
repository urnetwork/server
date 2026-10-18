// Published automation successors must replace obsolete checks without losing
// the historical schema or the replacement's operative custody predicates.
package monitor

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/urnetwork/server"
)

// The actual v847 DDL replaces v652's active-staging predicate. A coherent
// prefix must remain healthy on both sides of that exact transition.
func TestMigrationsAutomationStagingPublishedTransition(t *testing.T) {
	(&server.TestEnv{ApplyDbMigrations: false, RerunCount: 0}).Run(t, func(t testing.TB) {
		for _, head := range []int{846, 847, 848} {
			server.ApplyDbMigrationsUpTo(t.Context(), head)
			_, drift := migrationPingDatabaseCheck(t, t.Context())
			if drift != "" {
				t.Fatalf("published automation transition at %d reports schema drift: %s", head, drift)
			}
		}
	})
}

// Earlier bodies remain mandatory until their own published successor, not
// until whichever migration head happens to be compiled into this binary.
func TestMigrationsAutomationHistoricalFunctionCustody(t *testing.T) {
	faults := []struct {
		version int
		name    string
	}{
		{version: 842, name: "competition_v2_decision_guard"},
		{version: 843, name: "competition_v2_decision_guard"},
		{version: 844, name: "competition_v2_source_insert_or_activate_guard"},
		{version: 845, name: "competition_v2_source_abandonment_guard"},
		{version: 846, name: "competition_v2_source_target_guard"},
		{version: 847, name: "competition_v2_seal_guard"},
	}
	var observed []snMainnetMigrationAlertObservation
	(&server.TestEnv{ApplyDbMigrations: false, RerunCount: 0}).Run(t, func(t testing.TB) {
		for _, fault := range faults {
			server.ApplyDbMigrationsUpTo(t.Context(), fault.version)
			row, drift := migrationPingDatabaseCheck(t, t.Context())
			if drift != "" {
				t.Fatalf("published automation prefix %d reports schema drift: %s", fault.version, drift)
			}
			for version := 842; version <= 848; version++ {
				artifact := migrationCurrentSuffixTestContract(t, version).artifact
				if artifact.rowColumn != version-589 || len(row) <= artifact.rowColumn {
					t.Fatalf("automation version %d lost its positional artifact column", version)
				}
				if present := migrationBool(row[artifact.rowColumn]); present != (version <= fault.version) {
					t.Fatalf("automation prefix %d artifact %d=%t", fault.version, version, present)
				}
			}
			snMainnetMigrationFaultTx(t, func(ctx context.Context, tx server.PgTx) {
				statement := fmt.Sprintf("CREATE OR REPLACE FUNCTION %s() RETURNS trigger LANGUAGE plpgsql AS 'BEGIN RETURN NEW; END'", fault.name)
				if _, err := tx.Exec(ctx, statement); err != nil {
					t.Fatalf("replace published guard at %d: %v", fault.version, err)
				}
				alerts, err := NewMigrationsSignal().Run(ctx, syntheticSettings(&snMainnetMigrationTxSource{tx: tx}))
				if err != nil {
					t.Fatalf("inspect historical guard at %d: %v", fault.version, err)
				}
				artifact := migrationCurrentSuffixTestContract(t, fault.version).artifact
				observed = append(observed, snMainnetMigrationAlertObservation{
					alerts: alerts, want: fmt.Sprintf("%s@v%d", artifact.name, fault.version),
				})
			})
		}
	})
	if len(observed) != len(faults) {
		t.Fatalf("observed %d historical guards, want %d", len(observed), len(faults))
	}
	for index, observation := range observed {
		if alert := requireAlertClass(t, observation.alerts, "migration-schema-drift"); !strings.Contains(alert.Markdown(), observation.want) {
			t.Fatalf("version %d lost historical guard custody: %s", faults[index].version, alert.Markdown())
		}
	}
}

// Physical faults execute the complete owning signal and roll back before the
// next case. A matching name or a numeric migration head cannot clear them.
func TestMigrationsAutomationSchemaFaultsReachSignal(t *testing.T) {
	faults := []struct {
		name    string
		version int
		sql     string
	}{
		{name: "missing publication timestamp", version: 842, sql: `ALTER TABLE competition_round DROP COLUMN automation_publication_at`},
		{name: "nullable seal digest", version: 842, sql: `ALTER TABLE competition_evaluation_seal ALTER COLUMN seal_sha256 DROP NOT NULL`},
		{name: "missing seal check", version: 842, sql: snMainnetMigrationDropConstraint("competition_evaluation_seal", "c", "CHECK ((sealed_at <= publication_at))")},
		{name: "weakened fixed-width seal digest", version: 842, sql: snMainnetMigrationDropConstraint("competition_evaluation_seal", "c", "CHECK ((seal_sha256 ~ '^[0-9a-f]{64}$'::text))") + `ALTER TABLE competition_evaluation_seal ADD CHECK (seal_sha256 ~ '^[0-9a-f]{1,64}$')`},
		{name: "seal digest text-cast lookalike", version: 842, sql: snMainnetMigrationDropConstraint("competition_evaluation_seal", "c", "CHECK ((seal_sha256 ~ '^[0-9a-f]{64}$'::text))") + `ALTER TABLE competition_evaluation_seal ADD CHECK (seal_sha256::text ~ '^[0-9a-f]{64}$')`},
		{name: "weakened fixed-width decision digest", version: 842, sql: snMainnetMigrationDropConstraint("competition_reward_decision", "c", "CHECK ((decision_sha256 ~ '^[0-9a-f]{64}$'::text))") + `ALTER TABLE competition_reward_decision ADD CHECK (decision_sha256 ~ '^[0-9a-f]{1,64}$')`},
		{name: "decision digest text-cast lookalike", version: 842, sql: snMainnetMigrationDropConstraint("competition_reward_decision", "c", "CHECK ((decision_sha256 ~ '^[0-9a-f]{64}$'::text))") + `ALTER TABLE competition_reward_decision ADD CHECK (decision_sha256::text ~ '^[0-9a-f]{64}$')`},
		{name: "missing seal foreign key", version: 842, sql: snMainnetMigrationDropConstraint("competition_evaluation_seal", "f", "FOREIGN KEY (round_id) REFERENCES competition_round(round_id)")},
		{name: "non-durable transitions", version: 842, sql: `ALTER TABLE competition_supervisor_transition SET UNLOGGED`},
		{name: "changed eligibility body", version: 842, sql: `CREATE OR REPLACE FUNCTION competition_v2_score_eligible(score jsonb, baseline_raw double precision, margin double precision) RETURNS boolean LANGUAGE sql IMMUTABLE AS 'SELECT true'`},
		{name: "disabled evidence fence", version: 842, sql: `ALTER TABLE competition_job DISABLE TRIGGER competition_v2_job_fence`},
		{name: "sequence detached from cursor", version: 842, sql: `ALTER SEQUENCE competition_reward_outbox_event_cursor_seq OWNED BY NONE`},
		{name: "missing hold revision", version: 843, sql: snMainnetMigrationDropConstraint("competition_automation_hold", "u", "UNIQUE (environment, competition_id, season_id, kind, revision)")},
		{name: "mutable integrity incidents", version: 843, sql: `ALTER TABLE competition_automation_incident DISABLE TRIGGER competition_automation_incident_immutable`},
		{name: "changed incident-aware decision", version: 843, sql: `CREATE OR REPLACE FUNCTION competition_v2_decision_guard() RETURNS trigger LANGUAGE plpgsql AS 'BEGIN RETURN NEW; END'`},
		{name: "source lease guard privilege change", version: 844, sql: `ALTER FUNCTION competition_v2_require_source_lease(text,text,text) SECURITY DEFINER`},
		{name: "outbox cursor allocated before its scope lock", version: 844, sql: `ALTER TABLE competition_reward_outbox ALTER COLUMN event_cursor SET DEFAULT nextval('competition_reward_outbox_event_cursor_seq'::regclass)`},
		{name: "atomic outbox guard made immediate", version: 844, sql: `DROP TRIGGER competition_v2_decision_outbox ON competition_reward_decision; CREATE CONSTRAINT TRIGGER competition_v2_decision_outbox AFTER INSERT ON competition_reward_decision NOT DEFERRABLE FOR EACH ROW EXECUTE FUNCTION competition_v2_decision_outbox_guard()`},
		{name: "missing abandonment timestamp", version: 845, sql: `ALTER TABLE competition_source_activation DROP COLUMN abandoned_at CASCADE`},
		{name: "obsolete unconditional target identity", version: 845, sql: `ALTER TABLE competition_source_activation ADD UNIQUE(environment,competition_id,season_id,target_epoch)`},
		{name: "review cursor allocated before its scope lock", version: 845, sql: `ALTER TABLE competition_integrity_review ALTER COLUMN review_cursor SET DEFAULT nextval('competition_integrity_review_review_cursor_seq'::regclass)`},
		{name: "missing deferred review custody", version: 845, sql: `DROP TRIGGER competition_v2_review_decision_committed ON competition_integrity_review`},
		{name: "missing prospective round identity", version: 846, sql: `DROP INDEX competition_source_reserved_round`},
		{name: "prospective target guard changed", version: 846, sql: `CREATE OR REPLACE FUNCTION competition_v2_source_target_guard() RETURNS trigger LANGUAGE plpgsql AS 'BEGIN RETURN NEW; END'`},
		{name: "legacy staging predicate restored", version: 847, sql: `DROP INDEX competition_round_one_active_staging; CREATE UNIQUE INDEX competition_round_one_active_staging ON competition_round(competition_id) WHERE staging AND NOT canceled AND finalized_at IS NULL`},
		{name: "missing isolated staging epoch", version: 847, sql: `DROP INDEX competition_round_v2_staging_epoch`},
		{name: "missing retained epoch constraint", version: 847, sql: `ALTER TABLE competition_round DROP CONSTRAINT competition_round_epoch_kind`},
		{name: "current seal guard changed", version: 847, sql: `CREATE OR REPLACE FUNCTION competition_v2_seal_guard() RETURNS trigger LANGUAGE plpgsql AS 'BEGIN RETURN NEW; END'`},
		{name: "missing network feedback lookup", version: 848, sql: `DROP INDEX account_feedback_network_id_feedback_time`},
		{name: "feedback lookup wrong leading key", version: 848, sql: `DROP INDEX account_feedback_network_id_feedback_time; CREATE INDEX account_feedback_network_id_feedback_time ON account_feedback(feedback_time,network_id)`},
	}
	var observed []snMainnetMigrationAlertObservation
	snMainnetMigrationTestEnv().Run(t, func(t testing.TB) {
		if _, drift := migrationPingDatabaseCheck(t, t.Context()); drift != "" {
			t.Fatalf("published automation schema reports drift: %s", drift)
		}
		for _, fault := range faults {
			snMainnetMigrationFaultTx(t, func(ctx context.Context, tx server.PgTx) {
				if _, err := tx.Exec(ctx, fault.sql); err != nil {
					t.Fatalf("apply %s: %v", fault.name, err)
				}
				alerts, err := NewMigrationsSignal().Run(ctx, syntheticSettings(&snMainnetMigrationTxSource{tx: tx}))
				if err != nil {
					t.Fatalf("inspect %s: %v", fault.name, err)
				}
				artifact := migrationCurrentSuffixTestContract(t, fault.version).artifact
				observed = append(observed, snMainnetMigrationAlertObservation{
					alerts: alerts, want: fmt.Sprintf("%s@v%d", artifact.name, fault.version),
				})
			})
		}
		if _, drift := migrationPingDatabaseCheck(t, t.Context()); drift != "" {
			t.Fatalf("rolled-back automation faults left schema drift: %s", drift)
		}
	})
	if len(observed) != len(faults) {
		t.Fatalf("observed %d automation faults, want %d", len(observed), len(faults))
	}
	for index, observation := range observed {
		if alert := requireAlertClass(t, observation.alerts, "migration-schema-drift"); !strings.Contains(alert.Markdown(), observation.want) {
			t.Fatalf("%s lost its owning migration: %s", faults[index].name, alert.Markdown())
		}
	}
}
