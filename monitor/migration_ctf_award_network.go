// The capture the flag award network migration (server ctf_schema.go,
// ctfAwardNetworkSql): the network each award was won for, and the index a
// network's wins are read by.
package monitor

import "github.com/urnetwork/server"

// One nullable uuid with no default on ctf_award, and its index, in the
// catalog's own text. A release from before the column names the columns it
// writes, so it inserts on and leaves the column null: unknown, never another
// network's. A default would name a network for every such award, and a
// required column would refuse its inserts. Follow the migration's position
// after merges instead of pinning a number.
var ctfAwardNetworkMigrationContract = migrationCurrentSuffix(server.CtfAwardNetworkMigrationVersion(), "capture the flag award network",
	snMainnetMigrationColumns("ctf_award",
		snMainnetMigrationColumn{name: "network_id", kind: "uuid"}),
	migrationFp2Index("ctf_award", "ctf_award_network", "deployment_key, no_id, network_id, epoch", "", false))
