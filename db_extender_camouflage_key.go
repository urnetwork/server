package server

// The camouflage key an extender's activation posts, and the network that
// posted it (connect/EXTENDER.md P6; connect/EXTENDER-UPDATE2.md §3, phase
// 14b). A record carries the key only while that network still holds the
// extender's identity key, so a key never follows an identity to another
// network, whatever moves the row.
//
// Two nullable columns with no default: a catalog-only change that rewrites
// nothing, under a short lock timeout. The running code names its columns in
// every read and write of network_extender, so it tolerates the columns before
// it reads them.
var extenderCamouflageKeyMigration = newSqlMigration(`
	SET LOCAL lock_timeout = '5s';
	ALTER TABLE network_extender
		ADD COLUMN reality_public_key bytea NULL,
		ADD COLUMN reality_public_key_network_id uuid NULL
`)

// The version that adds the extender camouflage key columns: the migration's
// position in the list, which follows any re-sequencing of the list.
func ExtenderCamouflageKeyMigrationVersion() int {
	return migrationVersion(extenderCamouflageKeyMigration)
}
