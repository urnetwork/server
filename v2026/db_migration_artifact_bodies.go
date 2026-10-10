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
