// The debit journal's custody contract holds on the plain table and on the
// parent the owner-run cutover partitions by contract day
// (server.MigrateTransferDebitJournalToPartitions), and still reports a fault
// in the partitioned layout: a leaf that lost the published fillfactor, a
// missing parent index, a trigger on the journal and a changed drop guard.
package monitor

import (
	"fmt"
	"strings"
	"testing"

	"github.com/urnetwork/server/v2026"
)

func TestTransferDebitJournalCustodyHoldsInBothLayouts(t *testing.T) {
	artifact := fmt.Sprintf("transfer debit journal custody in either partition layout with its drop guard@v%d", server.TransferDebitJournalDropGuardMigrationVersion())
	snMainnetMigrationTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		if _, drift := migrationPingDatabaseCheck(t, ctx); drift != "" {
			t.Fatalf("the plain journal reports drift: %s", drift)
		}
		cutoverDay := server.PartitionGranularityDaily.AddRanges(server.PartitionGranularityDaily.RangeStart(server.NowUtc()), -3)
		server.Testing_PartitionTransferDebitJournal(ctx, cutoverDay)
		if !server.IsTransferDebitJournalPartitioned(ctx) {
			t.Fatal("the journal is not partitioned")
		}
		if _, drift := migrationPingDatabaseCheck(t, ctx); drift != "" {
			t.Fatalf("the partitioned journal reports drift: %s", drift)
		}
		leaf := fmt.Sprintf("transfer_debit_journal_p%s", server.NowUtc().Format("20060102"))
		requireMigrationMergedFaults(t, ctx, []migrationMergedFault{
			{apply: `ALTER TABLE ` + leaf + ` RESET (fillfactor)`, restore: `ALTER TABLE ` + leaf + ` SET (fillfactor = 70)`, artifact: artifact},
			{apply: `DROP INDEX transfer_debit_journal_shard_create_time`, restore: `CREATE INDEX transfer_debit_journal_shard_create_time ON transfer_debit_journal(shard,create_time)`, artifact: artifact},
			{apply: `CREATE FUNCTION test_debit_journal_noop() RETURNS trigger LANGUAGE plpgsql AS $$BEGIN RETURN NEW; END$$; CREATE TRIGGER test_debit_journal_noop BEFORE INSERT ON transfer_debit_journal FOR EACH ROW EXECUTE FUNCTION test_debit_journal_noop()`, restore: `DROP TRIGGER test_debit_journal_noop ON transfer_debit_journal; DROP FUNCTION test_debit_journal_noop()`, artifact: artifact},
			{apply: `CREATE OR REPLACE FUNCTION transfer_debit_journal_drop_guard(leaf text) RETURNS void LANGUAGE plpgsql AS $$BEGIN END$$`, restore: `CREATE OR REPLACE FUNCTION transfer_debit_journal_drop_guard(leaf text) RETURNS void LANGUAGE plpgsql AS $debit_drop_guard$` + server.TransferDebitJournalDropGuardBodySql() + `$debit_drop_guard$`, artifact: artifact},
		})
		// the superseded contracts are retired at the guard's version
		for _, contract := range migrationCurrentSuffixContracts {
			if strings.HasPrefix(contract.artifact.name, "transfer debit journal") && contract.artifact.requiredVersion < server.TransferDebitJournalDropGuardMigrationVersion() &&
				contract.artifact.requiredVersion >= server.WritePathMigrationVersion(server.WritePathDebitJournalShardTimeIndex) &&
				contract.artifact.removedVersion != server.TransferDebitJournalDropGuardMigrationVersion() {
				t.Fatalf("%s@v%d is not superseded by the layout contract", contract.artifact.name, contract.artifact.requiredVersion)
			}
		}
	})
}
