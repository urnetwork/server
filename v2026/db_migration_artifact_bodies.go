// Read-only catalog consumers compare the exact published migration bodies.
package server

import "strings"

func ProviderPaymentBonusGuardBodiesSql() (string, string) {
	bodies := strings.Split(providerPaymentBonusSchemaSql, "$$")
	return bodies[1], bodies[3]
}
func ProviderPayoutBoundaryGuardBodySql() string {
	return strings.Split(providerPayoutBoundarySchemaSql, "$$")[1]
}
func TransferDebitGuardBodySql() string {
	return strings.Split(transferDebitJournalSchemaSql, "$guard$")[1]
}

// The monitor compares exact immutable evidence custody without rewriting it.
func ContractCloseEvidenceGuardBodySql() string {
	return strings.Split(contractCloseReportEvidenceSchemaSql, "$close_report_guard$")[1]
}

// Version 857 admits only age-bounded retention deletes of close evidence.
func ContractCloseEvidenceRetentionGuardBodySql() string {
	return strings.Split(contractCloseReportEvidenceRetentionGuardSql, "$close_report_guard$")[1]
}

// The close evidence guard that also admits moves between partitions.
func ContractCloseEvidenceMoveGuardBodySql() string {
	return strings.Split(contractCloseReportEvidenceMoveGuardSql, "$close_report_guard$")[1]
}

// The version of the migration that publishes the move guard, found by its
// SQL: versions are positional, so it follows the migration to wherever the
// migration list places it. Zero when the list lacks it.
func ContractCloseEvidenceMoveGuardMigrationVersion() int {
	return sqlMigrationVersion(contractCloseReportEvidenceMoveGuardSql)
}

// The version (slice index plus one) of the plain SQL migration whose SQL is
// exactly sql, or zero.
func sqlMigrationVersion(sql string) int {
	for i, migration := range migrations {
		if sqlMigration, ok := migration.(*SqlMigration); ok && sqlMigration.sql == sql {
			return i + 1
		}
	}
	return 0
}

// Version 855 replaces the 850 source authorization and 851 round insert
// bodies and adds the elected-winner functions.
func CompetitionWinnerReviewBodiesSql() (sourceAuthorized, roundInsertGuard, winnerSourceAuthorized, winnerReviewGuard, winnerCheckpointGuard string) {
	migration := competitionWinnerReviewMigration()
	return strings.Split(migration, "$source$")[1],
		strings.Split(migration, "$guard$")[1],
		strings.Split(migration, "$winner_source$")[1],
		strings.Split(migration, "$winner_review$")[1],
		strings.Split(migration, "$winner_checkpoint$")[1]
}

// Version 858 retires operator-proxy from production's source requirement:
// it replaces the source graph and source authorization bodies.
func CompetitionSourceRetirementBodiesSql() (sourceGraphValid, sourceAuthorized string) {
	migration := competitionSourceRetirementMigration()
	return strings.Split(migration, "$graph$")[1], strings.Split(migration, "$source$")[1]
}

// Version 859 binds a scheduled round's opening to its intent: it replaces the
// round guard body and republishes the winner guards unchanged.
func CompetitionScheduledOpeningRoundGuardBodySql() string {
	return strings.Split(competitionScheduledOpeningMigration(), "$guard$")[1]
}

// Version 856 publishes the contract id floor.
func ContractIdFloorBodySql() string {
	return strings.Split(contractIdFloorSql, "$$")[1]
}

// The session journal's partition preparation (PARTITION-MAINTENANCE-PLAN.md
// C4): the retire lookup's floor, the append that uses it, the receipt
// reference and the drop guard.
func ProviderWorkSessionPartitionBodiesSql() (admitFloor, sessionAppend, receiptReference, dropGuard string) {
	return strings.Split(providerWorkSessionAdmitFloorSql, "$admit_floor$")[1],
		strings.Split(providerWorkSessionPartitionAppendSql, "$session_append$")[1],
		strings.Split(providerWorkSessionReceiptReferenceSql, "$receipt_reference$")[1],
		strings.Split(providerWorkSessionEventDropGuardSql, "$event_drop_guard$")[1]
}

// Read-only consumers pin the provider successor's exact bodies without
// changing any historical migration or reproducing its replacement logic.
func CompetitionWinnerReviewProviderBodiesSql() (provider, roundInsertGuard, winnerSourceAuthorized, winnerReviewGuard string) {
	migration := competitionWinnerReviewProviderMigration()
	return strings.Split(migration, "$provider$")[1],
		strings.Split(migration, "$guard$")[1],
		strings.Split(migration, "$winner_source$")[1],
		strings.Split(migration, "$winner_review$")[1]
}

// The reviewed repository successor changes only the source authorization.
func CompetitionRepositoryPatchSourceBodySql() string {
	return strings.Split(competitionRepositoryPatchMigration(), "$source$")[1]
}

// Read-only catalog consumers use the actual published Snow replacement
// bodies; the retained mono-root source body remains owned by its migration.
func CompetitionSnowWinnerReviewBodiesSql() (provider, roundInsertGuard, winnerSourceAuthorized, winnerReviewGuard, identityGuard, findings string) {
	migration := competitionSnowWinnerReviewMigration()
	guards := strings.Split(migration, "$guard$")
	return strings.Split(migration, "$provider$")[1], guards[1],
		strings.Split(migration, "$winner_source$")[1],
		strings.Split(migration, "$winner_review$")[1], guards[3],
		strings.Split(migration, "$snow_findings$")[1]
}

// Extracts the actual successor DDL, including both preserved findings versions.
// This reads published source only; it does not apply a migration.
func CompetitionSnowTwoMemberWinnerReviewBodiesSql() (provider, roundInsertGuard, winnerSourceAuthorized, winnerReviewGuard, identityGuard, findingsV1, findingsV2, findingsPolicy string) {
	migration := competitionSnowTwoMemberWinnerReviewMigration()
	guards := strings.Split(migration, "$guard$")
	findings := strings.Split(migration, "$snow_findings$")
	return strings.Split(migration, "$provider$")[1], guards[1],
		strings.Split(migration, "$winner_source$")[1], strings.Split(migration, "$winner_review$")[1], guards[3],
		findings[3], findings[1], strings.Split(migration, "$snow_policy_findings$")[1]
}
