package server

// The distribution each network's onboarding platform came with
// (model/onboarding_campaign_model.go, mmm/ONBOARDING-RETENTION.md P6): what
// the app reported in X-UR-ClientInfo at sign-up or when its device
// authenticated, which tells the App Store Mac from the direct-download Mac.
// One column with a constant default, so adding it is a catalog change with no
// rewrite of the table; the lock timeout keeps its brief exclusive lock from
// queueing the campaign's writers behind a long transaction.
const networkOnboardingDistributionSql = `
		SET LOCAL lock_timeout = '5s';
		ALTER TABLE network_onboarding
			ADD COLUMN distribution varchar(32) NOT NULL DEFAULT ''
	`

// The catalog monitor follows this immutable DDL wherever merges place it.
func NetworkOnboardingDistributionMigrationVersion() int {
	return sqlMigrationVersion(networkOnboardingDistributionSql)
}
