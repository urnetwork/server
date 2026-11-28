// The campaigns' consent evidence and the re-engagement program
// (mmm/onboarding/REVIEW.md finding 12, mmm/reengagement/PLAN.md "Server").
package monitor

// 905 adds four columns to account_preferences with constant defaults; 906
// and 908 create the program's two tables, keyed by their primary keys only;
// 907 and 909 index them. The default expressions and index definitions are
// the catalog's own text on PostgreSQL 18.
var reengagementMigrationContracts = []snMainnetMigrationContract{
	// How the product-updates preference was set and when, and the one
	// marketing suppression every campaign respects.
	migrationCurrentSuffix(905, "campaign consent source and marketing suppression columns",
		snMainnetMigrationColumns("account_preferences",
			snMainnetMigrationColumn{name: "product_updates_source", kind: "character varying(32)", notNull: true, defaultExpression: "''::character varying"},
			snMainnetMigrationColumn{name: "product_updates_source_time", kind: "timestamp without time zone"},
			snMainnetMigrationColumn{name: "marketing_suppressed_at", kind: "timestamp without time zone"},
			snMainnetMigrationColumn{name: "marketing_suppressed_reason", kind: "character varying(32)", notNull: true, defaultExpression: "''::character varying"})),
	// One re-engagement episode row per network.
	migrationCurrentSuffix(906, "re-engagement episode per network",
		migrationFp2Table("network_reengagement"),
		snMainnetMigrationColumns("network_reengagement",
			snMainnetMigrationColumn{name: "network_id", kind: "uuid", notNull: true},
			snMainnetMigrationColumn{name: "stream", kind: "character varying(32)", notNull: true},
			snMainnetMigrationColumn{name: "episode", kind: "integer", notNull: true, defaultExpression: "1"},
			snMainnetMigrationColumn{name: "started_at", kind: "timestamp without time zone", notNull: true},
			snMainnetMigrationColumn{name: "last_connect_at", kind: "timestamp without time zone"},
			snMainnetMigrationColumn{name: "platform", kind: "character varying(16)", notNull: true, defaultExpression: "''::character varying"},
			snMainnetMigrationColumn{name: "locale", kind: "character varying(32)", notNull: true, defaultExpression: "''::character varying"},
			snMainnetMigrationColumn{name: "time_zone", kind: "character varying(64)", notNull: true, defaultExpression: "''::character varying"},
			snMainnetMigrationColumn{name: "country", kind: "character varying(2)", notNull: true, defaultExpression: "''::character varying"},
			snMainnetMigrationColumn{name: "experiment_id", kind: "character varying(64)", notNull: true, defaultExpression: "''::character varying"},
			snMainnetMigrationColumn{name: "variant", kind: "character varying(64)", notNull: true, defaultExpression: "''::character varying"},
			snMainnetMigrationColumn{name: "last_step", kind: "character varying(8)", notNull: true, defaultExpression: "''::character varying"},
			snMainnetMigrationColumn{name: "next_step", kind: "character varying(8)", notNull: true, defaultExpression: "''::character varying"},
			snMainnetMigrationColumn{name: "next_send_at", kind: "timestamp without time zone"},
			snMainnetMigrationColumn{name: "last_sent_at", kind: "timestamp without time zone"},
			snMainnetMigrationColumn{name: "reason", kind: "character varying(32)", notNull: true, defaultExpression: "''::character varying"},
			snMainnetMigrationColumn{name: "reason_at", kind: "timestamp without time zone"},
			snMainnetMigrationColumn{name: "fix_reason", kind: "character varying(32)", notNull: true, defaultExpression: "''::character varying"},
			snMainnetMigrationColumn{name: "fix_reason_at", kind: "timestamp without time zone"},
			snMainnetMigrationColumn{name: "fix_platform", kind: "character varying(16)", notNull: true, defaultExpression: "''::character varying"},
			snMainnetMigrationColumn{name: "fix_sent_at", kind: "timestamp without time zone"},
			snMainnetMigrationColumn{name: "quiet_until", kind: "timestamp without time zone"},
			snMainnetMigrationColumn{name: "keep_until", kind: "timestamp without time zone"},
			snMainnetMigrationColumn{name: "exit_reason", kind: "character varying(32)", notNull: true, defaultExpression: "''::character varying"},
			snMainnetMigrationColumn{name: "exited_at", kind: "timestamp without time zone"},
			snMainnetMigrationColumn{name: "send_failures", kind: "integer", notNull: true, defaultExpression: "0"},
			snMainnetMigrationColumn{name: "last_send_error", kind: "character varying(256)", notNull: true, defaultExpression: "''::character varying"}),
		snMainnetMigrationConstraints("network_reengagement", "PRIMARY KEY (network_id)")),
	// The scan's pending fix follow-ups: answered, not yet sent.
	migrationCurrentSuffix(907, "re-engagement pending fix follow-up index",
		snMainnetMigrationPartialIndex("network_reengagement", "network_reengagement_fix_pending", "fix_reason_at",
			"((fix_reason_at IS NOT NULL) AND (fix_sent_at IS NULL))")),
	// Each re-engagement send, keyed by Brevo's message id.
	migrationCurrentSuffix(908, "re-engagement sends by message id",
		migrationFp2Table("network_reengagement_email"),
		snMainnetMigrationColumns("network_reengagement_email",
			snMainnetMigrationColumn{name: "message_id", kind: "character varying(256)", notNull: true},
			snMainnetMigrationColumn{name: "network_id", kind: "uuid", notNull: true},
			snMainnetMigrationColumn{name: "step", kind: "character varying(8)", notNull: true},
			snMainnetMigrationColumn{name: "template", kind: "character varying(32)", notNull: true},
			snMainnetMigrationColumn{name: "variant", kind: "character varying(32)", notNull: true},
			snMainnetMigrationColumn{name: "experiment", kind: "character varying(64)", notNull: true, defaultExpression: "''::character varying"},
			snMainnetMigrationColumn{name: "experiment_variant", kind: "character varying(64)", notNull: true, defaultExpression: "''::character varying"},
			snMainnetMigrationColumn{name: "template_id", kind: "integer", notNull: true, defaultExpression: "0"},
			snMainnetMigrationColumn{name: "locale", kind: "character varying(32)", notNull: true, defaultExpression: "''::character varying"},
			snMainnetMigrationColumn{name: "sent_at", kind: "timestamp without time zone", notNull: true}),
		snMainnetMigrationConstraints("network_reengagement_email", "PRIMARY KEY (message_id)")),
	// A network's sends in time order: the yearly cap and the last send.
	migrationCurrentSuffix(909, "re-engagement sends by network and time",
		migrationFp2Index("network_reengagement_email", "network_reengagement_email_network_sent", "network_id, sent_at", "", false)),
}
