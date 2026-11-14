// The durable per-network mail context (model/network_mail_context_model.go,
// controller/mail_context.go): what a network's own apps and browsers last
// reported about the reader, kept whatever the onboarding campaign's state, so
// the campaign emails can be placed in the reader's day and written in their
// language. One row per network: the newest device time zone, the newest
// ordered language list, and the newest app build per platform and
// distribution. Only the latest values are kept, never a history, and nothing
// here comes from an IP lookup.
//
// The migration only creates the table, empty: no existing table is locked
// or rewritten. The five-second lock bound keeps a catalog wait from stalling
// behind a long reader, and a table that exists already is left as it is, so
// a failed lock wait is safe to retry. Old binaries never read it. Apply
// before deploying the api and alt (auth-client, sign-up and sign-in write it)
// and the taskworker (the campaign sends read it).
package server

// The migration's statement.
const NetworkMailContextSql = `
	DO $mail_context$
	BEGIN
		PERFORM set_config('lock_timeout', '5s', true);
		IF to_regclass('public.network_mail_context') IS NULL THEN
			CREATE TABLE network_mail_context (
				network_id uuid NOT NULL,
				time_zone varchar(64) NOT NULL DEFAULT '',
				time_zone_time timestamp NULL,
				languages text[] NOT NULL DEFAULT '{}'::text[],
				languages_time timestamp NULL,
				clients jsonb NOT NULL DEFAULT '[]'::jsonb,
				update_time timestamp NOT NULL,
				PRIMARY KEY (network_id)
			);
		END IF;
	END
	$mail_context$
`

var networkMailContextMigration = newSqlMigration(NetworkMailContextSql)

// The migration's version: its position in the list, from 1.
func NetworkMailContextVersion() int {
	for i, migration := range migrations {
		if migration == networkMailContextMigration {
			return i + 1
		}
	}
	return 0
}
