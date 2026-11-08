// The catalog source and probation state of each URL outcome (D3,
// model/provider_egress_url_source.go).
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
// destination the pool held on probation, or a country site. The eight-hour
// window that every bucket and the Online rule read leaves a probationary
// outcome out; the rolling quota counts it as the measured run it is, so
// recording a site unscored costs no extra turns.
//
// The migration is additive and changes no stored outcome's meaning:
//
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
//
// Old binaries ignore the columns: their inserts take the defaults, and their
// readers count every outcome as before until they are replaced. Apply before
// deploying the api, alt, taskworker and proxy that use it.
package server

// The migration's statement. It does nothing when the column exists, so a
// failed lock wait is safe to retry.
const ProviderEgressUrlSourceSql = `
	DO $source$
	BEGIN
		IF NOT EXISTS (
			SELECT 1 FROM pg_catalog.pg_attribute
			WHERE attrelid = 'public.provider_egress_health_history'::regclass
			  AND attname = 'url_source' AND NOT attisdropped
		) THEN
			PERFORM set_config('lock_timeout', '5s', true);
			ALTER TABLE provider_egress_health_history
				ADD COLUMN url_source smallint NOT NULL DEFAULT 0,
				ADD COLUMN url_probation boolean NOT NULL DEFAULT false;
		END IF;
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
