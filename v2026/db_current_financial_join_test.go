// Published numeric migration history is immutable. Customer custody appends
// after the complete upstream debit, drain and Solana payment suffix.
package server

import "testing"

func TestCurrentFinancialJoinPreservesPublishedMigrationSuffix(t *testing.T) {
	markers := []string{
		"CREATE TABLE provider_payout_boundary",
		"CREATE TABLE transfer_debit_journal",
		"CREATE TABLE test_balance_drain",
		"CREATE INDEX test_balance_drain_network_id_end_time",
		"ADD COLUMN expected_amount_micro bigint",
		"CREATE TABLE circle_transfer_request",
	}
	previous := -1
	for _, marker := range markers {
		index := sqlMigrationIndex(t, marker)
		if previous >= 0 && index != previous+1 {
			t.Fatal("published migration suffix shifted for the financial join", marker, index, previous)
		}
		previous = index
	}
	if migrations[previous].(*SqlMigration).sql != circleTransferRequestSchemaSql {
		t.Fatal("customer custody body changed while moving its unpublished index")
	}
}
