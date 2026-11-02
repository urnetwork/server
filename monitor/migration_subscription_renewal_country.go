// The subscription store country's migration (server
// subscription_renewal_country_schema.go): the country the App Store or Google
// Play sold each subscription row in, which the re-engagement program reads as
// a network's country when the network has no onboarding row.
package monitor

import "github.com/urnetwork/server"

// One nullable varchar(2) column on subscription_renewal, without a default.
// The migration publishes no index or check.
var subscriptionRenewalCountryMigrationContract = migrationCurrentSuffix(server.SubscriptionRenewalCountrySchemaMigrationVersion(), "subscription store country column",
	snMainnetMigrationColumns("subscription_renewal",
		snMainnetMigrationColumn{name: "country", kind: "character varying(2)"}))
