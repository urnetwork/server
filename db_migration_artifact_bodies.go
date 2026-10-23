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

// Version 856 publishes the contract id floor.
func ContractIdFloorBodySql() string {
	return strings.Split(contractIdFloorSql, "$$")[1]
}
