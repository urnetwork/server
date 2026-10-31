package server

// Replica provenance is additive so older readers and writers retain their
// numeric columns. Existing scores have no authenticated checkpoint and remain
// unavailable to the corrected reader until signed evidence is replayed.
const snTopReplicaSchemaSql = `
    SET LOCAL lock_timeout = '5s';
    ALTER TABLE sn_top_snapshot
        ADD COLUMN replica_validator_hotkey bytea,
        ADD COLUMN replica_native_epoch bigint,
        ADD COLUMN replica_measurement_hash text,
        ADD COLUMN replica_complete bool NOT NULL DEFAULT false;
    ALTER TABLE sn_top_fleet
        ADD COLUMN replica_available bool NOT NULL DEFAULT false;
    ALTER TABLE sn_head_summary
        ADD COLUMN replica_genesis bool NOT NULL DEFAULT false;
    CREATE INDEX sn_head_summary_replica_genesis
        ON sn_head_summary (netuid, validator_hotkey, native_epoch)
        WHERE replica_genesis = true;
`

// The catalog monitor follows this immutable DDL wherever merges place it.
func SnTopReplicaSchemaMigrationVersion() int {
	return sqlMigrationVersion(snTopReplicaSchemaSql)
}
