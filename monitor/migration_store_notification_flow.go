// The store notification flow (server store_notification_flow_schema.go,
// controller/store_notification_flow.go) belongs to the migration catalog.
package monitor

import "github.com/urnetwork/server"

// Follow the migration's position after merges instead of pinning a number.
var storeNotificationFlowMigrationContract = migrationCurrentSuffix(
	server.StoreNotificationFlowVersion(),
	"store notification flow",
	migrationFp2Table("store_notification_flow"),
	snMainnetMigrationColumns("store_notification_flow",
		snMainnetMigrationColumn{name: "store", kind: "character varying(32)", notNull: true},
		snMainnetMigrationColumn{name: "last_accepted_at", kind: "timestamp without time zone", notNull: true},
		snMainnetMigrationColumn{name: "update_time", kind: "timestamp without time zone", notNull: true}),
	snMainnetMigrationConstraints("store_notification_flow", "PRIMARY KEY (store)"))
