// The carrier ports of each extender address (server
// network_extender_address_ports_schema.go, connect/EXTENDER.md S10).
package monitor

import "github.com/urnetwork/server"

// Two nullable integer columns on network_extender_address with no default, in
// the catalog's own text on PostgreSQL 18. The migration publishes no index or
// check. The version is the migration's position in the server's list, so the
// contract follows a re-sequencing.
var extenderAddressPortsMigrationContract = migrationCurrentSuffix(
	server.NetworkExtenderAddressPortsSchemaMigrationVersion(),
	"extender address tcp and udp ports",
	snMainnetMigrationColumns("network_extender_address",
		snMainnetMigrationColumn{name: "tcp_port", kind: "integer"},
		snMainnetMigrationColumn{name: "udp_port", kind: "integer"}),
)
