package monitor

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/urnetwork/server"
)

// Signal migrations implements SIGNALS.md §8.9. Migration versions are an
// append-only production protocol: the recorded head and the schema artifacts
// published at each version must agree before dependent services roll.
func NewMigrationsSignal() Signal {
	return &signalAdapter{
		number: "8.9",
		key:    "migrations",
		name:   "Database migration and schema coherence",
		probe:  migrationsProbe{},
	}
}

type migrationsProbe struct{}

func (migrationsProbe) id() string             { return "pg/migration-coherence" }
func (migrationsProbe) tier() string           { return tierWarn }
func (migrationsProbe) cadence() time.Duration { return time.Minute }

type migrationArtifact struct {
	name            string
	requiredVersion int
	rowColumn       int
}

var migrationArtifacts = []migrationArtifact{
	{name: "competition_round", requiredVersion: 588, rowColumn: 1},
	{name: "competition_job_immutable_guard", requiredVersion: 589, rowColumn: 2},
	{name: "competition_round.providers_sha256", requiredVersion: 590, rowColumn: 3},
	{name: "competition_round.epoch_number", requiredVersion: 591, rowColumn: 4},
	{name: "competition_job.api_image_digest", requiredVersion: 592, rowColumn: 5},
	{name: "competition_candidate_review", requiredVersion: 593, rowColumn: 6},
	{name: "transfer_escrow_balance_contract", requiredVersion: 594, rowColumn: 7},
	{name: "account_payment.contract_retention_cursor+pending", requiredVersion: 595, rowColumn: 8},
	{name: "account_payment_contract_retention_pending", requiredVersion: 596, rowColumn: 9},
	{name: "transfer_escrow_sweep_payment_contract", requiredVersion: 597, rowColumn: 10},
	{name: "migration_catalog", requiredVersion: 599, rowColumn: 11},
}

func (migrationsProbe) check(ctx context.Context, env *probeEnv) ([]finding, error) {
	rows, err := env.runner.pg(ctx, `
		WITH version AS (
			SELECT coalesce(max(end_version_number), 0)::int AS value
			FROM migration_audit
			WHERE status = 'success'
		)
		SELECT version.value,
		       to_regclass('public.competition_round') IS NOT NULL,
		       to_regprocedure('public.competition_job_immutable_guard()') IS NOT NULL,
		       EXISTS (
		           SELECT 1 FROM information_schema.columns
		           WHERE table_schema = 'public' AND table_name = 'competition_round'
		             AND column_name = 'providers_sha256'
		       ),
		       EXISTS (
		           SELECT 1 FROM information_schema.columns
		           WHERE table_schema = 'public' AND table_name = 'competition_round'
		             AND column_name = 'epoch_number'
		       ),
		       EXISTS (
		           SELECT 1 FROM information_schema.columns
		           WHERE table_schema = 'public' AND table_name = 'competition_job'
		             AND column_name = 'api_image_digest'
		       ),
		       to_regclass('public.competition_candidate_review') IS NOT NULL,
		       to_regclass('public.transfer_escrow_balance_contract') IS NOT NULL,
		       (
		           SELECT count(*) = 2 FROM information_schema.columns
		           WHERE table_schema = 'public' AND table_name = 'account_payment'
		             AND column_name IN ('contract_retention_cursor', 'contract_retention_pending')
		       ),
		       to_regclass('public.account_payment_contract_retention_pending') IS NOT NULL,
		       to_regclass('public.transfer_escrow_sweep_payment_contract') IS NOT NULL,
		       to_regclass('public.migration_catalog') IS NOT NULL
		FROM version;
	`)
	if err != nil {
		return nil, err
	}
	if len(rows) != 1 {
		return nil, fmt.Errorf("migration coherence query returned %d rows, want 1", len(rows))
	}

	target := pgTarget(env)
	dbVersion := atoiRow(rows[0], 0)
	requiredHead := server.MigrationCount()
	missing := make([]string, 0)
	for _, artifact := range migrationArtifacts {
		if artifact.requiredVersion <= dbVersion && !migrationBool(rows[0].str(artifact.rowColumn)) {
			missing = append(missing, fmt.Sprintf("%s@v%d", artifact.name, artifact.requiredVersion))
		}
	}
	if 600 <= dbVersion {
		if !migrationBool(rows[0].str(11)) {
			missing = append(missing, "migration_catalog identities@v600")
		} else {
			catalogRows, catalogErr := env.runner.pg(ctx, `
				SELECT count(*)::int,
				       coalesce(min(migration_index), -1)::int,
				       coalesce(max(migration_index), -1)::int
				FROM migration_catalog;
			`)
			if catalogErr != nil {
				return nil, catalogErr
			}
			if len(catalogRows) != 1 || atoiRow(catalogRows[0], 0) != dbVersion ||
				atoiRow(catalogRows[0], 1) != 0 || atoiRow(catalogRows[0], 2) != dbVersion-1 {
				missing = append(missing, "migration_catalog identities@v600")
			}
		}
	}

	findings := make([]finding, 0, 2)
	if len(missing) > 0 {
		findings = append(findings, finding{
			probeId: "pg/migration-coherence", tier: tierPage,
			class: "migration-schema-drift", target: target, sustain: 1,
			symptom:   fmt.Sprintf("database migration audit is at version %d but %d published schema artifact(s) are absent", dbVersion, len(missing)),
			mechanism: "A migration version was reordered, skipped, removed, or marked successful without leaving its published schema. A service that trusts only the numeric head can then execute code against missing columns or indexes, or replay an older non-idempotent migration into objects that already exist.",
			baseline:  fmt.Sprintf("Every published artifact through recorded database version %d exists; migration versions never move after release.", dbVersion),
			observed:  fmt.Sprintf("db_version=%d code_required_version=%d missing=%s", dbVersion, requiredHead, strings.Join(missing, ",")),
			action:    "Stop dependent service activation. Restore every published migration to its original index, append new migrations after the published sequence, and apply that corrected stream. Do not edit migration_audit or create production objects by hand merely to clear this alert.",
			verify:    "The recorded head advances only through the corrected append-only stream, every required artifact exists at its published version, and a fresh migration-coherence run has no schema-drift alert.",
			playbook:  "SIGNALS.md §8.9",
		})
	} else {
		findings = append(findings, healthyFinding("pg/migration-coherence", tierPage, "migration-schema-drift", target))
	}

	if dbVersion < requiredHead {
		findings = append(findings, finding{
			probeId: "pg/migration-coherence", tier: tierWarn,
			class: "migration-behind", target: target, sustain: 1,
			symptom:   fmt.Sprintf("database migration head %d is %d version(s) behind the code-required head %d", dbVersion, requiredHead-dbVersion, requiredHead),
			mechanism: "The checked source tree contains schema-dependent code newer than the production database. Starting that code before its append-only migrations finish can turn a safe online rollout into missing-column, missing-index, or duplicate-object failures.",
			baseline:  fmt.Sprintf("The database is at migration version %d before dependent services from this source tree become active.", requiredHead),
			observed:  fmt.Sprintf("db_version=%d code_required_version=%d lag=%d", dbVersion, requiredHead, requiredHead-dbVersion),
			action:    "Run the database migration phase from the exact service commit and require it to reach the code-required head before activating dependent taskworkers or APIs. If the numeric head advances while an artifact remains absent, treat migration-schema-drift as the blocking incident.",
			verify:    fmt.Sprintf("migration_audit reaches version %d and every versioned artifact in this signal exists before dependent services start.", requiredHead),
			playbook:  "SIGNALS.md §8.9",
		})
	} else {
		findings = append(findings, healthyFinding("pg/migration-coherence", tierWarn, "migration-behind", target))
	}

	return findings, nil
}

func migrationBool(value string) bool {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "1", "t", "true", "yes":
		return true
	default:
		return false
	}
}
