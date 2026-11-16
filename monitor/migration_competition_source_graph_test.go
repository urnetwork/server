// The real v850 transition transfers exact function custody while retaining
// v847 lifecycle and index guards. Every fault is rolled back independently.
package monitor

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/urnetwork/server"
)

// Old bodies must still be enforced immediately before their true successor.
func TestMigrationsSourceGraphHistoricalAuthorityCustody(t *testing.T) {
	var observations [][]Alert
	(&server.TestEnv{ApplyDbMigrations: false, RerunCount: 0}).Run(t, func(t testing.TB) {
		server.ApplyDbMigrationsUpTo(t.Context(), 849)
		if _, drift := migrationPingDatabaseCheck(t, t.Context()); drift != "" {
			t.Fatalf("published pre-successor source graph reports drift: %s", drift)
		}
		for _, statement := range []string{
			`CREATE OR REPLACE FUNCTION competition_v2_source_mode(intent jsonb) RETURNS void LANGUAGE plpgsql AS 'BEGIN RETURN; END'`,
			`CREATE OR REPLACE FUNCTION competition_v2_source_authorized(intent jsonb, env text, competition text, season text) RETURNS void LANGUAGE plpgsql AS 'BEGIN RETURN; END'`,
		} {
			snMainnetMigrationFaultTx(t, func(ctx context.Context, tx server.PgTx) {
				if _, err := tx.Exec(ctx, statement); err != nil {
					t.Fatal(err)
				}
				alerts, err := NewMigrationsSignal().Run(ctx, syntheticSettings(&snMainnetMigrationTxSource{tx: tx}))
				if err != nil {
					t.Fatal(err)
				}
				observations = append(observations, alerts)
			})
		}
	})
	if len(observations) != 2 {
		t.Fatalf("observed %d historical authority faults", len(observations))
	}
	for _, alerts := range observations {
		if alert := requireAlertClass(t, alerts, "migration-schema-drift"); !strings.Contains(alert.Markdown(), "isolated automation staging and current lifecycle guards@v847") {
			t.Fatal("pre-successor authority fault lost v847 custody")
		}
	}
}

// The historical v900 owner remains exact at its own published prefix.
// Current-head faults belong to its explicitly registered successors.
const competitionSourceGuardsHeadOwner = "trusted instrument overlay with two-member review, identity and retained source custody@v900"

// Validate the registered 900-to-benefit-to-intake-to-handoff-to-official
// review-to-executor control-to-qualification recovery-to-admission exception
// to-admitted claim-to-distributed recovery-to-operator patch retry
// transfers before selecting the current owner. This is the known lineage,
// never a generic waiver for an unknown successor or an expired predicate.
func competitionSourceGuardsCurrentOwner(t testing.TB) string {
	t.Helper()
	benefitVersion := server.CompetitionProductionBenefitMigrationVersion()
	intakeVersion := server.CompetitionIntakeFirstMigrationVersion()
	handoffVersion := server.CompetitionReviewHandoffMigrationVersion()
	officialVersion := server.CompetitionOfficialReviewMigrationVersion()
	controlVersion := server.CompetitionExecutorControlAmendmentMigrationVersion()
	recoveryVersion := server.CompetitionExecutorQualificationRecoveryMigrationVersion()
	exceptionVersion := server.CompetitionExecutorAdmissionExceptionMigrationVersion()
	claimVersion := server.CompetitionExecutorAdmissionClaimMigrationVersion()
	distributedVersion := server.CompetitionDistributedReevaluationMigrationVersion()
	patchRetryVersion := server.CompetitionOperatorRequestedPatchRetryMigrationVersion()
	if benefitVersion <= 900 || intakeVersion <= benefitVersion || handoffVersion <= intakeVersion ||
		officialVersion <= handoffVersion || controlVersion <= officialVersion || recoveryVersion <= controlVersion ||
		exceptionVersion <= recoveryVersion || claimVersion <= exceptionVersion || distributedVersion <= claimVersion || patchRetryVersion <= distributedVersion || server.MigrationCount() < patchRetryVersion {
		t.Fatal("source guard successors are not published in order")
	}
	for _, expected := range []struct {
		version   int
		removedAt int
		name      string
	}{
		{900, benefitVersion, "trusted instrument overlay with two-member review, identity and retained source custody"},
		{benefitVersion, intakeVersion, "production-benefit eligibility with retained review and instrument custody"},
		{intakeVersion, handoffVersion, "separate published-source intake and qualified grading custody"},
		{handoffVersion, officialVersion, "pending review handoff ledger with retained intake and winner-review custody"},
		{officialVersion, controlVersion, "epoch-one official review authorization with retained review handoff custody"},
		{controlVersion, recoveryVersion, "operator executor control amendment with retained official review custody"},
		{recoveryVersion, exceptionVersion, "single-use executor qualification recovery with retained executor control amendment custody"},
		{exceptionVersion, claimVersion, "retained host admission exception with retained qualification recovery custody"},
		{version: claimVersion, removedAt: distributedVersion, name: "executor claims honor admitted evidence with retained admission custody"},
		{version: distributedVersion, removedAt: patchRetryVersion, name: "separately authorized distributed infrastructure recovery"},
		{version: patchRetryVersion, removedAt: 0, name: "separately authorized unchanged-patch retry"},
	} {
		contract := migrationCurrentSuffixTestContract(t, expected.version)
		if contract.artifact.name != expected.name || contract.artifact.removedVersion != expected.removedAt {
			t.Fatalf("source guard owner %d has an unexpected name or retirement", expected.version)
		}
	}
	return fmt.Sprintf("separately authorized unchanged-patch retry@v%d", patchRetryVersion)
}

// The intake-first owner, current from its version until the review handoff.
func competitionSourceGuardsIntakeOwner() string {
	return fmt.Sprintf("separate published-source intake and qualified grading custody@v%d", server.CompetitionIntakeFirstMigrationVersion())
}

// The review handoff owner, current from its version until the official review.
func competitionSourceGuardsHandoffOwner() string {
	return fmt.Sprintf("pending review handoff ledger with retained intake and winner-review custody@v%d", server.CompetitionReviewHandoffMigrationVersion())
}

// The official review owner, current from its version until the executor
// control amendment.
func competitionSourceGuardsOfficialReviewOwner() string {
	return fmt.Sprintf("epoch-one official review authorization with retained review handoff custody@v%d", server.CompetitionOfficialReviewMigrationVersion())
}

// The executor control amendment owner, current from its version until the
// qualification recovery.
func competitionSourceGuardsExecutorControlOwner() string {
	return fmt.Sprintf("operator executor control amendment with retained official review custody@v%d", server.CompetitionExecutorControlAmendmentMigrationVersion())
}

// The qualification recovery owner, current from its version until the
// admission exception.
func competitionSourceGuardsQualificationRecoveryOwner() string {
	return fmt.Sprintf("single-use executor qualification recovery with retained executor control amendment custody@v%d", server.CompetitionExecutorQualificationRecoveryMigrationVersion())
}

// Historical exception custody remains exact until the claim correction.
func competitionSourceGuardsAdmissionExceptionOwner() string {
	return fmt.Sprintf("retained host admission exception with retained qualification recovery custody@v%d", server.CompetitionExecutorAdmissionExceptionMigrationVersion())
}

// Historical admitted claims retain custody up to the explicit distributed append.
func competitionSourceGuardsClaimOwner() string {
	return fmt.Sprintf("executor claims honor admitted evidence with retained admission custody@v%d", server.CompetitionExecutorAdmissionClaimMigrationVersion())
}

// The separately reviewed infrastructure authority remains exact until the
// additive unchanged-patch capability takes over complete catalog custody.
func competitionSourceGuardsDistributedOwner() string {
	return fmt.Sprintf("separately authorized distributed infrastructure recovery@v%d", server.CompetitionDistributedReevaluationMigrationVersion())
}

// Before a registered transfer the old artifact remains required; at the
// transfer the successor owns drift. Missing old or future artifact booleans
// cannot move that boundary, and every result uses the public signal reducer.
func TestMigrationsSourceGuardRegisteredSuccessorCustody(t *testing.T) {
	currentOwner := competitionSourceGuardsCurrentOwner(t)
	benefitVersion := server.CompetitionProductionBenefitMigrationVersion()
	intakeVersion := server.CompetitionIntakeFirstMigrationVersion()
	benefitOwner := fmt.Sprintf("production-benefit eligibility with retained review and instrument custody@v%d", benefitVersion)
	intakeOwner := competitionSourceGuardsIntakeOwner()
	for _, transfer := range []struct {
		oldVersion  int
		nextVersion int
		oldOwner    string
		nextOwner   string
	}{
		{900, benefitVersion, competitionSourceGuardsHeadOwner, benefitOwner},
		{benefitVersion, intakeVersion, benefitOwner, intakeOwner},
		{intakeVersion, server.CompetitionReviewHandoffMigrationVersion(), intakeOwner, competitionSourceGuardsHandoffOwner()},
		{server.CompetitionReviewHandoffMigrationVersion(), server.CompetitionOfficialReviewMigrationVersion(), competitionSourceGuardsHandoffOwner(), competitionSourceGuardsOfficialReviewOwner()},
		{server.CompetitionOfficialReviewMigrationVersion(), server.CompetitionExecutorControlAmendmentMigrationVersion(), competitionSourceGuardsOfficialReviewOwner(), competitionSourceGuardsExecutorControlOwner()},
		{server.CompetitionExecutorControlAmendmentMigrationVersion(), server.CompetitionExecutorQualificationRecoveryMigrationVersion(), competitionSourceGuardsExecutorControlOwner(), competitionSourceGuardsQualificationRecoveryOwner()},
		{server.CompetitionExecutorQualificationRecoveryMigrationVersion(), server.CompetitionExecutorAdmissionExceptionMigrationVersion(), competitionSourceGuardsQualificationRecoveryOwner(), competitionSourceGuardsAdmissionExceptionOwner()},
		{oldVersion: server.CompetitionExecutorAdmissionExceptionMigrationVersion(), nextVersion: server.CompetitionExecutorAdmissionClaimMigrationVersion(), oldOwner: competitionSourceGuardsAdmissionExceptionOwner(), nextOwner: competitionSourceGuardsClaimOwner()},
		{oldVersion: server.CompetitionExecutorAdmissionClaimMigrationVersion(), nextVersion: server.CompetitionDistributedReevaluationMigrationVersion(), oldOwner: competitionSourceGuardsClaimOwner(), nextOwner: competitionSourceGuardsDistributedOwner()},
		{oldVersion: server.CompetitionDistributedReevaluationMigrationVersion(), nextVersion: server.CompetitionOperatorRequestedPatchRetryMigrationVersion(), oldOwner: competitionSourceGuardsDistributedOwner(), nextOwner: currentOwner},
	} {
		for _, control := range []struct {
			head           int
			missingVersion int
			wantOwner      string
		}{
			{transfer.nextVersion - 1, transfer.oldVersion, transfer.oldOwner},
			{transfer.nextVersion - 1, transfer.nextVersion, ""},
			{transfer.nextVersion, transfer.oldVersion, ""},
			{transfer.nextVersion, transfer.nextVersion, transfer.nextOwner},
		} {
			row := syntheticMigrationArtifactRow(control.head)
			artifact := migrationCurrentSuffixTestContract(t, control.missingVersion).artifact
			row[artifact.rowColumn] = "f"
			source := &syntheticSource{postgresFn: func(query string) ([]Row, error) {
				if strings.Contains(query, "FROM migration_catalog") {
					return syntheticMigrationCatalogRows(control.head), nil
				}
				return []Row{row}, nil
			}}
			alerts, err := NewMigrationsSignal().Run(context.Background(), syntheticSettings(source))
			if err != nil {
				t.Fatal("source guard boundary did not reach the public reducer")
			}
			wantDrift := 0
			if control.wantOwner != "" {
				wantDrift = 1
			}
			if requireAlertClassCount(alerts, "migration-schema-drift") != wantDrift || requireAlertClassCount(alerts, "cannot-observe") != 0 {
				t.Fatal("source guard publication or retirement boundary changed")
			}
			if wantDrift != 0 {
				alert := requireAlertClass(t, alerts, "migration-schema-drift")
				if alert.Severity != SeverityPage || !strings.Contains(alert.Markdown(), control.wantOwner) {
					t.Fatal("source guard fault lost its exact applied owner")
				}
			}
		}
	}
}

// Numeric head, matching names, old legitimate bodies and unchanged source
// functions cannot substitute for the exact published successor graph.
func TestMigrationsSourceGraphSchemaFaultsReachSignal(t *testing.T) {
	currentOwner := competitionSourceGuardsCurrentOwner(t)
	faults := []struct {
		name string
		sql  string
		want string
	}{
		{name: "legacy source mode restored", sql: `CREATE OR REPLACE FUNCTION competition_v2_source_mode(intent jsonb) RETURNS void LANGUAGE plpgsql AS $regression$` + competitionMigration847CompetitionV2SourceModeBody + `$regression$`, want: currentOwner},
		{name: "legacy source authority restored", sql: `CREATE OR REPLACE FUNCTION competition_v2_source_authorized(intent jsonb, env text, competition text, season text) RETURNS void LANGUAGE plpgsql AS $regression$` + competitionMigration847CompetitionV2SourceAuthorizedBody + `$regression$`, want: currentOwner},
		{name: "graph validator always accepts", sql: `CREATE OR REPLACE FUNCTION competition_v2_source_graph_valid(commits jsonb, staging_mode boolean) RETURNS boolean LANGUAGE plpgsql IMMUTABLE AS 'BEGIN RETURN true; END'`, want: currentOwner},
		{name: "graph security privilege changed", sql: `ALTER FUNCTION competition_v2_source_graph_valid(jsonb,boolean) SECURITY DEFINER`, want: currentOwner},
		{name: "graph volatility changed", sql: `ALTER FUNCTION competition_v2_source_graph_valid(jsonb,boolean) VOLATILE`, want: currentOwner},
		{name: "retained seal guard changed", sql: `CREATE OR REPLACE FUNCTION competition_v2_seal_guard() RETURNS trigger LANGUAGE plpgsql AS 'BEGIN RETURN NEW; END'`, want: "isolated automation staging and current lifecycle guards@v847"},
		{name: "retained staging index removed", sql: `DROP INDEX competition_round_v2_staging_epoch`, want: "isolated automation staging and current lifecycle guards@v847"},
	}
	var observations [][]Alert
	snMainnetMigrationTestEnv().Run(t, func(t testing.TB) {
		if _, drift := migrationPingDatabaseCheck(t, t.Context()); drift != "" {
			t.Fatalf("published source graph schema reports drift: %s", drift)
		}
		artifact := migrationCurrentSuffixTestContract(t, 850).artifact
		if artifact.rowColumn != 261 {
			t.Fatal("source graph lost its exact versioned artifact row")
		}
		for _, fault := range faults {
			snMainnetMigrationFaultTx(t, func(ctx context.Context, tx server.PgTx) {
				if _, err := tx.Exec(ctx, fault.sql); err != nil {
					t.Fatalf("apply %s: %v", fault.name, err)
				}
				alerts, err := NewMigrationsSignal().Run(ctx, syntheticSettings(&snMainnetMigrationTxSource{tx: tx}))
				if err != nil {
					t.Fatal(err)
				}
				observations = append(observations, alerts)
			})
		}
		if _, drift := migrationPingDatabaseCheck(t, t.Context()); drift != "" {
			t.Fatalf("rolled-back source graph faults left drift: %s", drift)
		}
	})
	if len(observations) != len(faults) {
		t.Fatalf("observed %d source graph faults, want %d", len(observations), len(faults))
	}
	for i, alerts := range observations {
		if alert := requireAlertClass(t, alerts, "migration-schema-drift"); !strings.Contains(alert.Markdown(), faults[i].want) {
			t.Fatalf("%s lost its published migration owner", faults[i].name)
		}
	}
}

// Exact-body custody transfers only when the real successor migration applies.
// Each published prefix must be healthy and still detect every retained guard.
func TestMigrationsSourceGuardPublishedOwnerTransfers(t *testing.T) {
	currentOwner := competitionSourceGuardsCurrentOwner(t)
	benefitVersion := server.CompetitionProductionBenefitMigrationVersion()
	intakeVersion := server.CompetitionIntakeFirstMigrationVersion()
	prefixes := []struct {
		head      int
		removedAt int
		owner     string
	}{
		{855, 858, "elected-winner review and successor source guards@v855"},
		{858, 859, "production and staging source authorization of the ten live repositories@v858"},
		{859, 891, "scheduled round opening bound to its intent@v859"},
		{891, 892, "provider-aware winner review and scheduled round guards@v891"},
		{892, 893, "reviewed repository patch source and winner guard custody@v892"},
		{893, 897, "provider union review, identity and retained source custody@v893"},
		{897, 900, "two-member provider union review, identity and retained source custody@v897"},
		{900, benefitVersion, competitionSourceGuardsHeadOwner},
		{benefitVersion, intakeVersion, fmt.Sprintf("production-benefit eligibility with retained review and instrument custody@v%d", benefitVersion)},
		{intakeVersion, server.CompetitionReviewHandoffMigrationVersion(), competitionSourceGuardsIntakeOwner()},
		{server.CompetitionReviewHandoffMigrationVersion(), server.CompetitionOfficialReviewMigrationVersion(), competitionSourceGuardsHandoffOwner()},
		{server.CompetitionOfficialReviewMigrationVersion(), server.CompetitionExecutorControlAmendmentMigrationVersion(), competitionSourceGuardsOfficialReviewOwner()},
		{server.CompetitionExecutorControlAmendmentMigrationVersion(), server.CompetitionExecutorQualificationRecoveryMigrationVersion(), competitionSourceGuardsExecutorControlOwner()},
		{server.CompetitionExecutorQualificationRecoveryMigrationVersion(), server.CompetitionExecutorAdmissionExceptionMigrationVersion(), competitionSourceGuardsQualificationRecoveryOwner()},
		{server.CompetitionExecutorAdmissionExceptionMigrationVersion(), server.CompetitionExecutorAdmissionClaimMigrationVersion(), competitionSourceGuardsAdmissionExceptionOwner()},
		{head: server.CompetitionExecutorAdmissionClaimMigrationVersion(), removedAt: server.CompetitionDistributedReevaluationMigrationVersion(), owner: competitionSourceGuardsClaimOwner()},
		{head: server.CompetitionDistributedReevaluationMigrationVersion(), removedAt: server.CompetitionOperatorRequestedPatchRetryMigrationVersion(), owner: competitionSourceGuardsDistributedOwner()},
		{head: server.CompetitionOperatorRequestedPatchRetryMigrationVersion(), removedAt: 0, owner: currentOwner},
	}
	faults := []struct {
		name string
		sql  string
	}{
		{"source authority accepts everything", `CREATE OR REPLACE FUNCTION competition_v2_source_authorized(intent jsonb, env text, competition text, season text) RETURNS void LANGUAGE plpgsql AS 'BEGIN RETURN; END'`},
		{"round insertion accepts everything", `CREATE OR REPLACE FUNCTION competition_v2_round_insert_guard() RETURNS trigger LANGUAGE plpgsql AS 'BEGIN RETURN NEW; END'`},
		{"source graph accepts everything", `CREATE OR REPLACE FUNCTION competition_v2_source_graph_valid(commits jsonb, staging_mode boolean) RETURNS boolean LANGUAGE plpgsql IMMUTABLE AS 'BEGIN RETURN true; END'`},
	}
	type observation struct {
		head   int
		name   string
		owner  string
		alerts []Alert
	}
	var observations []observation
	(&server.TestEnv{ApplyDbMigrations: false, RerunCount: 0}).Run(t, func(t testing.TB) {
		for _, prefix := range prefixes {
			contract := migrationCurrentSuffixTestContract(t, prefix.head)
			if contract.artifact.removedVersion != prefix.removedAt {
				t.Fatalf("published owner %d retires at %d, want %d", prefix.head, contract.artifact.removedVersion, prefix.removedAt)
			}
			server.ApplyDbMigrationsUpTo(t.Context(), prefix.head)
			if _, drift := migrationPingDatabaseCheck(t, t.Context()); drift != "" {
				t.Fatalf("published owner prefix %d reports drift: %s", prefix.head, drift)
			}
			for _, fault := range faults {
				snMainnetMigrationFaultTx(t, func(ctx context.Context, tx server.PgTx) {
					if _, err := tx.Exec(ctx, fault.sql); err != nil {
						t.Fatalf("apply %s at %d: %v", fault.name, prefix.head, err)
					}
					alerts, err := NewMigrationsSignal().Run(ctx, syntheticSettings(&snMainnetMigrationTxSource{tx: tx}))
					if err != nil {
						t.Fatalf("inspect %s at %d: %v", fault.name, prefix.head, err)
					}
					observations = append(observations, observation{prefix.head, fault.name, prefix.owner, alerts})
				})
			}
			if _, drift := migrationPingDatabaseCheck(t, t.Context()); drift != "" {
				t.Fatalf("rolled-back owner prefix %d reports drift: %s", prefix.head, drift)
			}
		}
	})
	if len(observations) != len(prefixes)*len(faults) {
		t.Fatalf("observed %d published-owner faults, want %d", len(observations), len(prefixes)*len(faults))
	}
	for _, observed := range observations {
		alert := requireAlertClass(t, observed.alerts, "migration-schema-drift")
		if !strings.Contains(alert.Markdown(), observed.owner) {
			t.Fatalf("%s at %d lost its published migration owner", observed.name, observed.head)
		}
	}
}
