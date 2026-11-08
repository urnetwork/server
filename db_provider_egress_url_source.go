// The catalog source and probation state of each URL outcome (D3,
// model/provider_egress_url_source.go), and the evidence and state of each
// country site's automatic graduation (model/provider_egress_country_site.go).
//
// A URL turn draws its destination from the general catalog or from the
// provider's country list, and a general destination promoted from the
// candidates is on probation until the pool refresh graduates it. Until now an
// outcome kept neither: the source was a taskworker metric label, and in URL
// mode nothing applied probation, so a promoted candidate counted at once,
// and a country site would have too.
//
// url_source holds where the destination came from: 0 the general catalog, 1
// the provider's country list. url_probation holds whether the destination
// had not graduated into scoring when the outcome was stored: a general
// destination the pool held on probation, or a country site the refresh had
// not graduated. The eight-hour window that every bucket and the Online rule
// read leaves a probationary outcome out; the rolling quota counts it as the
// measured run it is, so recording a site unscored costs no extra turns.
//
// provider_egress_country_site_tally pairs each measured country outcome with
// the same provider's general outcomes of the eight hours before it, one row
// per day, site and provider; the pool refresh judges each listed country site
// on a week of it and keeps the site's state, scored or on probation, in
// provider_egress_country_site, which the ingest reads for each outcome.
//
// The migration is additive and changes no stored outcome's meaning:
//
//   - The two tables are new, so creating them locks nothing that exists.
//     They are created before the ALTER, so the ALTER's lock is held for the
//     least time before the migration commits.
//   - Both columns are constant defaults (general, not on probation), so every
//     outcome stored before them, and every one an older release stores
//     during a rollout, reads as a scored general outcome, which is how it was
//     counted. Adding them is a catalog change: no partition is rewritten or
//     scanned (about 6.5 million rows a day, eight days kept).
//   - The ALTER still takes ACCESS EXCLUSIVE on the partitioned table and each
//     partition for that instant, and the table takes an insert for every URL
//     turn beside window reads that run for seconds, so the wait is bounded at
//     five seconds: a migration queued behind a long reader fails for a retry
//     rather than stall URL ingest behind it.
//   - Days made around it attach: the partition registry copies the parent's
//     columns and attaches in one transaction that holds the parent's lock
//     from the copy on, so the ALTER cannot come between; a day made by any
//     other path before the ALTER and not attached yet is given the columns
//     here, and the registry also adds, before every attach of a history day,
//     any column of the parent the day lacks (ReconcileLeafColumns).
//
// Old binaries ignore the columns and the tables: their inserts take the
// defaults, and their readers count every outcome as before until they are
// replaced. Apply before deploying the api, alt, taskworker and proxy that use
// it.
package server

// The migration's statement. Every step does nothing once done, so a failed
// lock wait is safe to retry.
const ProviderEgressUrlSourceSql = `
	DO $source$
	DECLARE
		leaf record;
	BEGIN
		PERFORM set_config('lock_timeout', '5s', true);
		CREATE TABLE IF NOT EXISTS provider_egress_country_site (
			name varchar(128) NOT NULL,
			country_code varchar(2) NOT NULL,
			url varchar(4096) NOT NULL,
			scored boolean NOT NULL DEFAULT false,
			probation_since timestamp NOT NULL,
			graduated_time timestamp NULL,
			demoted_time timestamp NULL,
			demote_count integer NOT NULL DEFAULT 0,
			demote_reason varchar(256) NOT NULL DEFAULT '',
			judged_time timestamp NULL,
			judgement varchar(512) NOT NULL DEFAULT '',
			network_count integer NOT NULL DEFAULT 0,
			provider_count integer NOT NULL DEFAULT 0,
			paired_load_count integer NOT NULL DEFAULT 0,
			paired_difference real NULL,
			lower_bound real NULL,
			upper_bound real NULL,
			update_time timestamp NOT NULL,
			PRIMARY KEY (name, country_code)
		);
		CREATE TABLE IF NOT EXISTS provider_egress_country_site_tally (
			tally_day date NOT NULL,
			name varchar(128) NOT NULL,
			country_code varchar(2) NOT NULL,
			client_id uuid NOT NULL,
			load_count integer NOT NULL DEFAULT 0,
			ok_count integer NOT NULL DEFAULT 0,
			paired_load_count integer NOT NULL DEFAULT 0,
			paired_ok_count integer NOT NULL DEFAULT 0,
			expected_ok_sum double precision NOT NULL DEFAULT 0,
			tls_failure_count integer NOT NULL DEFAULT 0,
			update_time timestamp NOT NULL,
			PRIMARY KEY (tally_day, name, country_code, client_id)
		);
		IF NOT EXISTS (
			SELECT 1 FROM pg_catalog.pg_attribute
			WHERE attrelid = 'public.provider_egress_health_history'::regclass
			  AND attname = 'url_source' AND NOT attisdropped
		) THEN
			ALTER TABLE provider_egress_health_history
				ADD COLUMN url_source smallint NOT NULL DEFAULT 0,
				ADD COLUMN url_probation boolean NOT NULL DEFAULT false;
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
			EXECUTE format('ALTER TABLE %s ADD COLUMN IF NOT EXISTS url_source smallint NOT NULL DEFAULT 0, ADD COLUMN IF NOT EXISTS url_probation boolean NOT NULL DEFAULT false', leaf.name);
		END LOOP;
	END
	$source$
`

var providerEgressUrlSourceMigration = newSqlMigration(ProviderEgressUrlSourceSql)

// The migration's version: its position in the list, from 1.
func ProviderEgressUrlSourceVersion() int {
	for i, migration := range migrations {
		if migration == providerEgressUrlSourceMigration {
			return i + 1
		}
	}
	return 0
}
