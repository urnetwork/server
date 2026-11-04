// Independent finalization custody never replaces the four-kind historical
// hold artifact. Old binaries can certify their original journal after DDL.
package monitor

import (
	"strings"

	"github.com/urnetwork/server"
)

// New tables and operative guards are independently checked, not inferred from
// schema head, names, an active hold, or a cooperative current application.
var competitionFinalizationHoldArtifactQuery = func() string {
	scope, hold, policy, roundScope, seal, decision, round := server.CompetitionFinalizationHoldBodiesSql()
	predicates := []string{
		migrationFp2Table("competition_finalization_hold"),
		snMainnetMigrationColumns("competition_finalization_hold",
			snMainnetMigrationColumn{name: "hold_id", kind: "uuid", notNull: true},
			snMainnetMigrationColumn{name: "environment", kind: "character varying(128)", notNull: true},
			snMainnetMigrationColumn{name: "competition_id", kind: "character varying(128)", notNull: true},
			snMainnetMigrationColumn{name: "season_id", kind: "character varying(128)", notNull: true},
			snMainnetMigrationColumn{name: "kind", kind: "character varying(32)", notNull: true},
			snMainnetMigrationColumn{name: "revision", kind: "integer", notNull: true},
			snMainnetMigrationColumn{name: "active", kind: "boolean", notNull: true},
			snMainnetMigrationColumn{name: "actor_id", kind: "character varying(128)", notNull: true},
			snMainnetMigrationColumn{name: "idempotency_key", kind: "character varying(128)", notNull: true},
			snMainnetMigrationColumn{name: "request_sha256", kind: "character(64)", notNull: true},
			snMainnetMigrationColumn{name: "hold_json", kind: "jsonb", notNull: true},
			snMainnetMigrationColumn{name: "created_at", kind: "timestamp without time zone", notNull: true}),
		snMainnetMigrationConstraints("competition_finalization_hold", "PRIMARY KEY (hold_id)",
			"UNIQUE (environment, competition_id, season_id, kind, revision)",
			"UNIQUE (environment, competition_id, season_id, actor_id, idempotency_key)",
			"CHECK (((kind)::text = 'finalization'::text))", "CHECK ((revision > 0))"),
		snMainnetMigrationForeignKeyCensus("competition_finalization_hold"),
		migrationFp2Table("competition_finalization_scope_fence"),
		snMainnetMigrationColumns("competition_finalization_scope_fence",
			snMainnetMigrationColumn{name: "environment", kind: "character varying(128)", notNull: true},
			snMainnetMigrationColumn{name: "competition_id", kind: "character varying(128)", notNull: true},
			snMainnetMigrationColumn{name: "season_id", kind: "character varying(128)", notNull: true},
			snMainnetMigrationColumn{name: "generation", kind: "bigint", notNull: true}),
		snMainnetMigrationConstraints("competition_finalization_scope_fence", "PRIMARY KEY (environment, competition_id, season_id)", "CHECK ((generation > 0))"),
		snMainnetMigrationForeignKeyCensus("competition_finalization_scope_fence"),
		snMainnetMigrationFunction("competition_finalization_touch_scope(text,text,text)", "void", "plpgsql", "v", false, scope, "env", "competition", "season"),
		snMainnetMigrationFunction("competition_finalization_hold_guard()", "trigger", "plpgsql", "v", false, hold),
		snMainnetMigrationFunction("competition_finalization_require_policy_unheld(jsonb,text)", "void", "plpgsql", "v", false, policy, "policy", "competition"),
		snMainnetMigrationFunction("competition_finalization_require_round_unheld(uuid)", "void", "plpgsql", "v", false, roundScope, "target_round"),
		snMainnetMigrationFunction("competition_finalization_seal_guard()", "trigger", "plpgsql", "v", false, seal),
		snMainnetMigrationFunction("competition_finalization_decision_guard()", "trigger", "plpgsql", "v", false, decision),
		snMainnetMigrationFunction("competition_finalization_round_guard()", "trigger", "plpgsql", "v", false, round),
		snMainnetMigrationTrigger("competition_automation_hold", "competition_00_finalization_hold_scope", "competition_finalization_hold_guard", 7),
		snMainnetMigrationTrigger("competition_finalization_hold", "competition_00_finalization_hold_scope", "competition_finalization_hold_guard", 7),
		snMainnetMigrationTrigger("competition_finalization_hold", "competition_finalization_hold_immutable", "competition_append_only_guard", 27),
		snMainnetMigrationTrigger("competition_finalization_hold", "competition_finalization_hold_no_truncate", "competition_append_only_guard", 34),
		snMainnetMigrationTrigger("competition_evaluation_seal", "competition_00_finalization_seal", "competition_finalization_seal_guard", 7),
		snMainnetMigrationTrigger("competition_reward_decision", "competition_00_finalization_decision", "competition_finalization_decision_guard", 7),
		snMainnetMigrationTrigger("competition_round", "competition_00_finalization_round", "competition_finalization_round_guard", 23),
	}
	return "(" + strings.Join(predicates, " AND ") + ")"
}()

// Append-only migration positioning leaves all published artifact bytes intact.
var competitionFinalizationHoldMigrationContract = migrationCurrentSuffix(server.CompetitionFinalizationHoldMigrationVersion(),
	"prospective finalization hold and stale-snapshot writer fence", competitionFinalizationHoldArtifactQuery)
