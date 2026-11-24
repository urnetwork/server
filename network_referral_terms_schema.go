package server

// The referral terms (owner 2026-11-24; model/network_referral_terms.go): a
// referral made from now on must be claimed by the referred network's app use
// within a claim window, and then pays both sides for life; one that is not
// claimed in time ends. A link that existed before pays for life as before.
//
//   - network_referral.existing_link is true for every link that exists when
//     this applies and false for every link made after it, whichever release
//     makes it: the column is added with the default true, which every
//     existing row takes, and the default then changes to false. An existing
//     link pays without a claim. A re-point to another referrer makes the
//     link new.
//   - network_referral.end_time is when a new link's claim window ended
//     unclaimed; the link stays as a record, never pays, and no longer counts
//     toward its referrer's cap. Null for every other link.
//   - network_referral_referee holds, per referred network, the time of its
//     first link, kept across a re-point and an unlink so the claim window
//     counts from the first link, and the time its app use claimed it.
//   - network_referral_app_day holds, per referred network that still has to
//     claim, its app use per UTC day, which the usage rollup adds.
//
// Columns with a constant default or none and a default change are catalog
// changes: nothing is rewritten, under a five-second lock bound. The two
// tables are new. The running release writes network_referral without naming
// the columns, so its rows take the defaults, and it reads neither table.
const networkReferralTermsSchemaSql = `
	SET LOCAL lock_timeout = '5s';
	ALTER TABLE network_referral
		ADD COLUMN IF NOT EXISTS existing_link boolean NOT NULL DEFAULT true,
		ADD COLUMN IF NOT EXISTS end_time timestamp NULL;
	ALTER TABLE network_referral ALTER COLUMN existing_link SET DEFAULT false;
	CREATE TABLE IF NOT EXISTS network_referral_referee (
		network_id uuid NOT NULL,
		first_link_time timestamp NOT NULL,
		qualified_time timestamp NULL,
		PRIMARY KEY (network_id)
	);
	CREATE TABLE IF NOT EXISTS network_referral_app_day (
		network_id uuid NOT NULL,
		day date NOT NULL,
		app_byte_count bigint NOT NULL,
		update_time timestamp NOT NULL,
		PRIMARY KEY (network_id, day)
	);
`

// The position of the migration, for the monitor's contract.
func NetworkReferralTermsSchemaMigrationVersion() int {
	return sqlMigrationVersion(networkReferralTermsSchemaSql)
}
