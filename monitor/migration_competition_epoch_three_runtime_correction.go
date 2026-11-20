// SIGNALS.md §8.9. The epoch-three runtime correction replaces three function
// bodies and adds a fenced, append-only journal. The catalog reads schema only.
package monitor

import "github.com/urnetwork/server"

const (
	competitionIntakeRuntimeReadySignature  = "competition_v2_intake_runtime_ready(uuid)"
	competitionIntakeBaselineValidSignature = "competition_v2_intake_baseline_valid(uuid,bytea,text,text,text,text,text,text,text)"
)

var competitionEpochThreeRuntimeCorrectionVersion = server.CompetitionEpochThreeRuntimeCorrectionMigrationVersion()

// Require the exact installed executable bodies, journal storage and enabled
// trigger wiring from the same registered version that releases the old bodies.
var competitionEpochThreeRuntimeCorrectionMigrationContract = func() snMainnetMigrationContract {
	const table = "competition_epoch_three_runtime_correction"
	const roundKey = "FOREIGN KEY (round_id) REFERENCES competition_round(round_id)"
	bodies := server.CompetitionEpochThreeRuntimeCorrectionBodiesSql()
	return migrationCurrentSuffix(competitionEpochThreeRuntimeCorrectionVersion, "epoch-three runtime correction journal and readiness guards",
		migrationFp2Table(table),
		snMainnetMigrationColumns(table,
			snMainnetMigrationColumn{name: "round_id", kind: "uuid", notNull: true},
			snMainnetMigrationColumn{name: "correction_sha256", kind: "character(64)", notNull: true},
			snMainnetMigrationColumn{name: "correction_canonical", kind: "bytea", notNull: true},
			snMainnetMigrationColumn{name: "correction_json", kind: "jsonb", notNull: true},
			snMainnetMigrationColumn{name: "created_at", kind: "timestamp without time zone", notNull: true}),
		snMainnetMigrationConstraints(table,
			"PRIMARY KEY (round_id)",
			"CHECK ((correction_sha256 ~ '^[0-9a-f]{64}$'::text))",
			"CHECK (((octet_length(correction_canonical) >= 1) AND (octet_length(correction_canonical) <= 8192)))",
			"CHECK ((jsonb_typeof(correction_json) = 'object'::text))",
			"CHECK (((convert_from(correction_canonical, 'UTF8'::name))::jsonb = correction_json))",
			"CHECK ((encode(sha256(correction_canonical), 'hex'::text) = (correction_sha256)::text))"),
		snMainnetMigrationForeignKey(table, "competition_round", roundKey),
		snMainnetMigrationForeignKeyCensus(table, roundKey),
		snMainnetMigrationFunction("competition_epoch_three_runtime_correction_guard()", "trigger", "plpgsql", "v", false, bodies.CorrectionGuard),
		snMainnetMigrationFunction("competition_epoch_three_runtime_receipt_valid(jsonb,text,jsonb)", "boolean", "sql", "s", false, bodies.ReceiptValid, "intent", "role_name", "receipt"),
		snMainnetMigrationFunction("competition_v2_source_readiness_guard()", "trigger", "plpgsql", "v", false, bodies.SourceReadinessGuard),
		snMainnetMigrationFunction(competitionIntakeRuntimeReadySignature, "boolean", "plpgsql", "v", false, bodies.IntakeRuntimeReady, "target"),
		snMainnetMigrationFunction(competitionIntakeBaselineValidSignature, "boolean", "plpgsql", "v", false, bodies.IntakeBaselineValid, "target", "content", "digest", "base", "providers", "image", "scorer", "hardware", "qualification"),
		snMainnetMigrationTrigger(table, "competition_epoch_three_runtime_correction_identity", "competition_epoch_three_runtime_correction_guard", 7),
		snMainnetMigrationTrigger(table, "competition_epoch_three_runtime_correction_append_only", "competition_append_only_guard", 27),
		snMainnetMigrationTrigger(table, "competition_epoch_three_runtime_correction_no_truncate", "competition_append_only_guard", 34))
}()
