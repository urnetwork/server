// The durable per-network mail context (server
// network_mail_context_schema.go, controller/mail_context.go) belongs to the
// migration catalog.
package monitor

import "github.com/urnetwork/server"

// Follow the migration's position after merges instead of pinning a number.
var networkMailContextMigrationContract = migrationCurrentSuffix(
	server.NetworkMailContextVersion(),
	"network mail context",
	migrationFp2Table("network_mail_context"),
	snMainnetMigrationColumns("network_mail_context",
		snMainnetMigrationColumn{name: "network_id", kind: "uuid", notNull: true},
		snMainnetMigrationColumn{name: "time_zone", kind: "character varying(64)", notNull: true, defaultExpression: "''::character varying"},
		snMainnetMigrationColumn{name: "time_zone_time", kind: "timestamp without time zone"},
		snMainnetMigrationColumn{name: "languages", kind: "text[]", notNull: true, defaultExpression: "'{}'::text[]"},
		snMainnetMigrationColumn{name: "languages_time", kind: "timestamp without time zone"},
		snMainnetMigrationColumn{name: "clients", kind: "jsonb", notNull: true, defaultExpression: "'[]'::jsonb"},
		snMainnetMigrationColumn{name: "update_time", kind: "timestamp without time zone", notNull: true}),
	snMainnetMigrationConstraints("network_mail_context", "PRIMARY KEY (network_id)"))
