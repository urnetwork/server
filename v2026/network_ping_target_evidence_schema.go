package server

// The refusals of each target's ping hour that are evidence against the target
// (model.NetworkPingRefusalCountsAgainstTarget, connect/GEOMAP.md §5.5), beside
// every refusal (rejection_count). The per-target ranking of refusing targets
// reads them, and so leaves out the refusals that speak to no one: a pinger
// the target does not know, which is a stale directory, and a probe refused
// for rate. From 2026-11-07 stale directories refused most extender pingers,
// and the ranking listed extenders whose only fault was that.
//
// The migration is additive and bounded:
//
//   - One nullable bigint with no default: a catalog change only. No row of
//     the partitioned table or of its day partitions is rewritten or scanned,
//     and every row stored before it reads null: its evidence is unknown,
//     never zero.
//   - The ALTER takes ACCESS EXCLUSIVE on the partitioned table and on each day
//     partition for that instant. Every ping report writes the table, so each
//     lock wait is bounded at five seconds: a migration queued behind a long
//     transaction fails, to be run again, rather than hold the ingest behind
//     it.
//   - It reads the catalog first and does nothing once the column exists, so
//     running it again takes no lock and changes nothing.
//   - A day partition is always made as a partition of the table (the model's
//     createNetworkPingTablePartition, CREATE TABLE ... PARTITION OF), never
//     made apart and attached, so every day made after it has the column.
//
// A release from before the column names the columns it writes, so it stores
// on after the migration and leaves the column null in the rows it creates. A
// row the newer release creates and an older one adds to counts only the newer
// release's evidence, for the hours of a rollout. Apply before deploying the
// api and alt that write it and the taskworker that reads it.
const networkPingTargetEvidenceSchemaSql = `
	DO $evidence$
	BEGIN
		PERFORM set_config('lock_timeout', '5s', true);
		IF NOT EXISTS (
			SELECT 1 FROM pg_catalog.pg_attribute
			WHERE attrelid = 'public.network_ping_target_hour_tally'::regclass
				AND attname = 'evidence_rejection_count' AND NOT attisdropped
		) THEN
			ALTER TABLE network_ping_target_hour_tally
				ADD COLUMN IF NOT EXISTS evidence_rejection_count bigint NULL;
		END IF;
	END
	$evidence$
`

// The catalog monitor follows this immutable DDL wherever merges place it.
func NetworkPingTargetEvidenceSchemaMigrationVersion() int {
	return sqlMigrationVersion(networkPingTargetEvidenceSchemaSql)
}
