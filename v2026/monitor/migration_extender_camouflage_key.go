// The extender camouflage key (connect/EXTENDER.md P6; connect/EXTENDER-UPDATE2.md
// §3, phase 14b).
package monitor

import (
	"github.com/urnetwork/server/v2026"
)

// Two nullable columns on network_extender: the camouflage key an activation
// posted and the network that posted it. The version is the migration's
// position in the server's list, so the contract follows a re-sequencing.
var extenderCamouflageKeyMigrationContract = migrationCurrentSuffix(
	server.ExtenderCamouflageKeyMigrationVersion(),
	"extender camouflage key and the network that posted it",
	snMainnetMigrationColumns("network_extender",
		snMainnetMigrationColumn{name: "reality_public_key", kind: "bytea"},
		snMainnetMigrationColumn{name: "reality_public_key_network_id", kind: "uuid"}),
)
