package server

// The country a store sold each subscription row in
// (model/subscription_store_country_model.go): the App Store storefront or the
// Google Play region, upper-case ISO alpha-2, recorded beside the row's price
// tier from now on. A nullable column without a default, so adding it rewrites
// nothing; the lock timeout keeps its brief exclusive lock from queueing the
// table's writers behind a long transaction. The migration list's text,
// unchanged, so its published identity stays the same.
const subscriptionRenewalCountrySchemaSql = `
		SET LOCAL lock_timeout = '5s';
		ALTER TABLE subscription_renewal ADD COLUMN country varchar(2) NULL
	`

// The catalog monitor follows this immutable DDL wherever merges place it.
func SubscriptionRenewalCountrySchemaMigrationVersion() int {
	return sqlMigrationVersion(subscriptionRenewalCountrySchemaSql)
}
