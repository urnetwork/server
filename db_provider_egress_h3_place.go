// The HTTP/3 place evidence (model/provider_egress_h3_place.go): per URL,
// country and UTC day, the established networks whose exits there reached the
// URL over QUIC, at most a handful of them. It is what judges a silent HTTP/3
// leg: the silence is the provider's only where other established networks
// in the same country reach the same URL over QUIC; elsewhere the place does
// not carry it, and the URL is judged by its HTTPS leg.
//
// The migration only creates the table, empty: no existing table is locked or
// rewritten. The five-second lock bound keeps a catalog wait from stalling
// behind a long reader, and a table that exists already is left as it is, so
// a failed lock wait is safe to retry. Old binaries never read it. Apply
// before deploying the taskworker (its in-process operator ingest writes and
// reads it), and the api and alt, which run the same ingest for a standalone
// prober.
package server

// The migration's statement.
const ProviderEgressH3PlaceSql = `
	DO $h3_place$
	BEGIN
		IF to_regclass('public.provider_egress_h3_place') IS NULL THEN
			PERFORM set_config('lock_timeout', '5s', true);
			CREATE TABLE provider_egress_h3_place (
				name varchar(256) NOT NULL,
				country_code varchar(2) NOT NULL,
				tally_day date NOT NULL,
				network_ids uuid[] NOT NULL DEFAULT '{}',
				update_time timestamp NOT NULL,
				PRIMARY KEY (name, country_code, tally_day)
			);
		END IF;
	END
	$h3_place$
`

var providerEgressH3PlaceMigration = newSqlMigration(ProviderEgressH3PlaceSql)

// The migration's version: its position in the list, from 1.
func ProviderEgressH3PlaceVersion() int {
	for i, migration := range migrations {
		if migration == providerEgressH3PlaceMigration {
			return i + 1
		}
	}
	return 0
}
