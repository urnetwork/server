// The fleet's URL outcomes by bucket of measured time
// (model/provider_egress_url_overload.go): per bucket, the scored URL outcomes
// of every provider, their failures, the same with the bucket's worst country
// and worst network left out, the baseline the bucket was judged against, and
// whether the judge found the bucket an overload of our own platform. The
// outcomes of a bucket judged an overload stay recorded in the history but
// leave every score and pool decision.
//
// The migration only creates the table, empty: no existing table is locked or
// rewritten. The five-second lock bound keeps a catalog wait from stalling
// behind a long reader, and a table that exists already is left as it is, so
// a failed lock wait is safe to retry. Old binaries never read it. Binaries
// with it read it in every URL window, so they require it: apply before
// deploying.
package server

// The migration's statement.
const ProviderEgressUrlFleetBucketSql = `
	DO $url_fleet_bucket$
	BEGIN
		IF to_regclass('public.provider_egress_url_fleet_bucket') IS NULL THEN
			PERFORM set_config('lock_timeout', '5s', true);
			CREATE TABLE provider_egress_url_fleet_bucket (
				bucket_start timestamp NOT NULL,
				bucket_end timestamp NOT NULL,
				outcome_count integer NOT NULL,
				error_count integer NOT NULL,
				country_outcome_count integer NOT NULL,
				country_error_count integer NOT NULL,
				network_outcome_count integer NOT NULL,
				network_error_count integer NOT NULL,
				baseline_outcome_count bigint NOT NULL,
				baseline_error_count bigint NOT NULL,
				discounted boolean NOT NULL,
				discounted_outcome_count integer NOT NULL,
				discounted_error_count integer NOT NULL,
				judge_time timestamp NOT NULL,
				PRIMARY KEY (bucket_start)
			);
		END IF;
	END
	$url_fleet_bucket$
`

var providerEgressUrlFleetBucketMigration = newSqlMigration(ProviderEgressUrlFleetBucketSql)

// The migration's version: its position in the list, from 1.
func ProviderEgressUrlFleetBucketVersion() int {
	for i, migration := range migrations {
		if migration == providerEgressUrlFleetBucketMigration {
			return i + 1
		}
	}
	return 0
}
