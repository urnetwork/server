package server

// The win-back offers the re-engagement program issues (model/winback_offer_model.go,
// mmm/reengagement/PLAN.md "THE STORES" 6): one row per offer, keyed by its
// id, with the network's offers in time order through the second index, so
// "one offer per network per 365 days" is one indexed read.
//
//   - store is where the offer is redeemed: stripe, play or apple. Each store
//     names its own handle: the per-customer Stripe promotion code, the Play
//     offer tag on the monthly base plan, or the App Store win-back offer's
//     identifier. A store never carries another store's handle.
//   - expires_at is the offer's real deadline. The Stripe promotion code
//     expires with it; the plan response stops showing the offer at it.
//   - redeemed_at is when the purchase that used the offer was made, stamped
//     by the purchase path of the offer's store.
//
// A new table: nothing is rewritten, and older binaries never read it.
const networkWinbackOfferSchemaSql = `
	CREATE TABLE network_winback_offer (
		offer_id uuid NOT NULL,
		network_id uuid NOT NULL,
		store varchar(16) NOT NULL,
		plan varchar(16) NOT NULL,
		percent_off integer NOT NULL CHECK (0 < percent_off AND percent_off < 100),
		issued_at timestamp NOT NULL,
		expires_at timestamp NOT NULL,
		redeemed_at timestamp NULL,
		stripe_promotion_code_id varchar(128) NULL,
		play_offer_tag varchar(64) NULL,
		apple_offer_id varchar(128) NULL,

		PRIMARY KEY (offer_id)
	);

	CREATE INDEX network_winback_offer_network_issued
		ON network_winback_offer (network_id, issued_at);
`

// The version of the migration that creates the offers in this binary, found
// by its SQL, so the monitor's contract follows the migration to whatever
// position a merge gives it. Zero when the binary lacks it.
func NetworkWinbackOfferSchemaMigrationVersion() int {
	return sqlMigrationVersion(networkWinbackOfferSchemaSql)
}
