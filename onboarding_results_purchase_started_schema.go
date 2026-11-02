package server

// purchase_started_7d on the onboarding results (onboarding/results.go
// ComputeOutcomes): the networks with a purchase.started in the 7 days after
// their cohort time. It is the in-app offer experiment's leading metric,
// because both arms can start a purchase: the offer arm from the offer screen,
// the holdout from the regular plan picker (mmm/onboarding/REVIEW.md finding
// 4). A constant default: no table rewrite, and the rollup of an older binary,
// which does not name the column, writes 0.
const onboardingResultsPurchaseStartedSql = `
		SET LOCAL lock_timeout = '5s';
		ALTER TABLE onboarding_results_daily
			ADD COLUMN purchase_started_7d integer NOT NULL DEFAULT 0
	`

// The version of the migration that adds the column in this binary, found by
// its SQL, so the monitor's contract follows the migration to whatever
// position a merge gives it. Zero when the binary lacks it.
func OnboardingResultsPurchaseStartedMigrationVersion() int {
	return sqlMigrationVersion(onboardingResultsPurchaseStartedSql)
}
