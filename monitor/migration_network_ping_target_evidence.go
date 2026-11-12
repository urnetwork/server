// The ping target evidence migration (server
// network_ping_target_evidence_schema.go): the refusals of each target's ping
// hour that are evidence against the target.
package monitor

import "github.com/urnetwork/server"

// One nullable bigint with no default on the per-target hour tally, in the
// catalog's own text. Without a default, a row a release from before the
// column creates reads null, its evidence unknown; a default would claim it
// held none, and a required column would refuse that release's inserts. The
// migration publishes no index or check. Follow the migration's position after
// merges instead of pinning a number.
var networkPingTargetEvidenceMigrationContract = migrationCurrentSuffix(server.NetworkPingTargetEvidenceSchemaMigrationVersion(), "ping target hour evidence refusals",
	snMainnetMigrationColumns("network_ping_target_hour_tally",
		snMainnetMigrationColumn{name: "evidence_rejection_count", kind: "bigint"}))
