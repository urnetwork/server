// The store notification flow (controller/store_notification_flow.go,
// model/store_notification_flow_model.go): per store, when the api last
// accepted a notification it verified, a Google Play real-time developer
// notification (google) or an App Store Server Notification (apple). The
// monitor's store-notification-silent alert (SIGNALS.md §2.22c) reads it
// against the store's purchases: from 2026-10-19 to 10-27 Main refused every
// Play notification and nothing noticed for eight days.
//
// The migration creates the table and one row per store whose clock starts
// at the migration (tracking starts as if a notification had just been
// accepted), so the alert can tell a store that has accepted nothing since
// from one never tracked. No existing table is locked or rewritten. The
// five-second lock bound keeps a catalog wait from stalling behind a long
// reader; a table that exists already is left as it is, so a failed lock
// wait is safe to retry. Old binaries never read it. Apply with the api and
// alt that write it: the monitor pages if neither writes it for a day while
// purchases continue.
package server

// The migration's statement.
const StoreNotificationFlowSql = `
	DO $store_notification_flow$
	BEGIN
		IF to_regclass('public.store_notification_flow') IS NULL THEN
			PERFORM set_config('lock_timeout', '5s', true);
			CREATE TABLE store_notification_flow (
				store varchar(32) NOT NULL,
				last_accepted_at timestamp NOT NULL,
				update_time timestamp NOT NULL,
				PRIMARY KEY (store)
			);
			INSERT INTO store_notification_flow (store, last_accepted_at, update_time)
			VALUES
				('apple', timezone('UTC', now()), timezone('UTC', now())),
				('google', timezone('UTC', now()), timezone('UTC', now()));
		END IF;
	END
	$store_notification_flow$
`

var storeNotificationFlowMigration = newSqlMigration(StoreNotificationFlowSql)

// The migration's version: its position in the list, from 1.
func StoreNotificationFlowVersion() int {
	for i, migration := range migrations {
		if migration == storeNotificationFlowMigration {
			return i + 1
		}
	}
	return 0
}
