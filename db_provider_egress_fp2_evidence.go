// The two per-outcome facts capability-aware FindProviders2 reads from the URL
// history (FP2-PROTOCOL.md §4.1, §12.3):
//
//   - failed_later_legs: a smallint mask of the later legs of a failed URL
//     outcome that the ingest's place judgement held against the provider:
//     1 its IPv4 HTTP/3 leg was silent, 2 its IPv6 HTTPS leg failed, 4 its
//     IPv6 HTTP/3 leg was silent (egresshealth.UrlProbeEvidence.
//     FailedLaterLegs). Zero for every other outcome. It lets the export
//     classify a provider that fails only over UDP or only over IPv6 instead
//     of excluding it.
//   - pq_outcome: the outcome's post-quantum observation: 0 unmeasured, 1
//     positive, 2 negative (egresshealth.UrlProbeE2eEvidence). Strict
//     post-quantum selection requires a positive one in the eight-hour window.
//
// The migration is a catalog change only:
//
//   - Both columns have a constant default, so on PostgreSQL 18 adding them
//     rewrites and scans nothing (about 6 million rows a day, eight days
//     kept). Every outcome stored before them, and every one an older release
//     stores during a rollout, reads 0: a plain failure and no post-quantum
//     observation, the safe direction for both readers.
//   - Adding them still takes an access exclusive lock on the partitioned
//     table and each day for that instant, and the table takes an insert for
//     every URL turn beside window reads that run for seconds, so the wait is
//     bounded at five seconds: a migration queued behind a long reader fails
//     for a rerun rather than stall URL ingest behind it. Each step is skipped
//     once done, so a rerun takes no lock it does not need.
//   - Days made around it attach as for the URL source columns
//     (db_provider_egress_url_source.go): the partition registry adds any
//     parent column a day lacks before it attaches the day, and a day made
//     and not attached yet is given the columns here.
//   - No check constraint: validating one would scan every day. The store
//     writes only the masks and outcomes its tests pin.
//
// Old binaries name their columns, so they neither read nor write these. Apply
// before deploying the taskworker, api and alt that store URL outcomes with
// them; their readiness requires it (MinimumRuntimeMigrationVersion).
package server

// The migration's statement.
const ProviderEgressFp2EvidenceSql = `
	DO $fp2_evidence$
	DECLARE
		leaf record;
	BEGIN
		PERFORM set_config('lock_timeout', '5s', true);
		IF NOT EXISTS (
			SELECT 1 FROM pg_catalog.pg_attribute
			WHERE attrelid = 'public.provider_egress_health_history'::regclass
			  AND attname = 'failed_later_legs' AND NOT attisdropped
		) OR NOT EXISTS (
			SELECT 1 FROM pg_catalog.pg_attribute
			WHERE attrelid = 'public.provider_egress_health_history'::regclass
			  AND attname = 'pq_outcome' AND NOT attisdropped
		) THEN
			ALTER TABLE provider_egress_health_history
				ADD COLUMN IF NOT EXISTS failed_later_legs smallint NOT NULL DEFAULT 0,
				ADD COLUMN IF NOT EXISTS pq_outcome smallint NOT NULL DEFAULT 0;
		END IF;
		-- a day made like the history before this migration and not attached
		-- yet takes the columns too, so its attach still succeeds
		FOR leaf IN
			SELECT format('%I.%I', namespace.nspname, relation.relname) AS name
			FROM pg_catalog.pg_class AS relation
			JOIN pg_catalog.pg_namespace AS namespace ON namespace.oid = relation.relnamespace
			WHERE namespace.nspname = 'public' AND relation.relkind = 'r' AND NOT relation.relispartition
			AND relation.relname LIKE 'provider\_egress\_health\_history\_p%'
		LOOP
			EXECUTE format('ALTER TABLE %s ADD COLUMN IF NOT EXISTS failed_later_legs smallint NOT NULL DEFAULT 0, ADD COLUMN IF NOT EXISTS pq_outcome smallint NOT NULL DEFAULT 0', leaf.name);
		END LOOP;
	END
	$fp2_evidence$
`

var providerEgressFp2EvidenceMigration = newSqlMigration(ProviderEgressFp2EvidenceSql)

// The migration's position in the list, from 1, wherever merges place it.
func ProviderEgressFp2EvidenceMigrationVersion() int {
	return migrationVersion(providerEgressFp2EvidenceMigration)
}
