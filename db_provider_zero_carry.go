// The zero-carry verdicts (model/provider_zero_carry.go): providers that
// accepted contracts from several client networks and carried nothing through
// any of them, while every URL probe of theirs failed.
//
// One row per provider while its verdict holds. The taskworker's judge
// (JudgeProviderZeroCarry) inserts a row on the evidence it read and deletes
// it once the provider carries again: a URL probe succeeds, its app version
// changes, or the verdict reaches expire_time. Readers take only a row whose
// expire_time is still ahead, so a stopped judge cannot hold a verdict past
// it. client_version is the app version the provider's device reported when
// the verdict was made, compared on every run; the counts and bytes are the
// evidence the verdict rests on, kept for the operator tools and the
// dashboard.
//
// The migration only creates the table, empty: no existing table is locked or
// rewritten. The five-second lock bound keeps a catalog wait from stalling
// behind a long reader, and a table that exists already is left as it is, so
// a failed lock wait is safe to retry. Old binaries never read it. Apply
// before deploying the api, alt, proxy and taskworker that read it.
package server

// The migration's statement.
const ProviderZeroCarrySql = `
	DO $zero_carry$
	BEGIN
		IF to_regclass('public.provider_zero_carry') IS NULL THEN
			PERFORM set_config('lock_timeout', '5s', true);
			CREATE TABLE provider_zero_carry (
				client_id uuid NOT NULL,
				create_time timestamp NOT NULL,
				expire_time timestamp NOT NULL,
				client_version varchar(64) NULL,
				contract_count integer NOT NULL,
				network_count integer NOT NULL,
				carried_byte_count bigint NOT NULL,
				PRIMARY KEY (client_id)
			);
		END IF;
	END
	$zero_carry$
`

var providerZeroCarryMigration = newSqlMigration(ProviderZeroCarrySql)

// The migration's version: its position in the list, from 1.
func ProviderZeroCarryVersion() int {
	for i, migration := range migrations {
		if migration == providerZeroCarryMigration {
			return i + 1
		}
	}
	return 0
}
