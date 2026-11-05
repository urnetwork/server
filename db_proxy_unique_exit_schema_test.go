package server

import (
	"strings"
	"testing"
)

// The egress key columns go on a table find-providers reads, so their ADD
// COLUMN gives up after 2 s rather than queue the readers behind its lock.
func TestProxyUniqueExitEgressKeyMigrationBoundsItsLockWait(t *testing.T) {
	if !strings.Contains(proxyUniqueExitEgressKeyColumnsSql, "SET LOCAL lock_timeout = '2s';") {
		t.Fatalf("the migration does not bound its lock wait at 2 s: %s", proxyUniqueExitEgressKeyColumnsSql)
	}
	for _, sql := range []string{
		proxyUniqueExitEgressKeyColumnsSql,
		proxyUniqueExitTableSql,
		proxyUniqueExitNetworkIndexSql,
		proxyExitAssignmentTableSql,
		proxyExitAssignmentNetworkEgressKeyLiveIndexSql,
		proxyExitAssignmentProxyLiveIndexSql,
		proxyExitAssignmentProviderLiveIndexSql,
		proxyExitAssignmentAssignedAtLiveIndexSql,
		proxyExitAssignmentProxyReleasedIndexSql,
		proxyExitAssignmentNetworkReleasedIndexSql,
		proxyExitAssignmentReleasedAtIndexSql,
	} {
		upper := strings.ToUpper(sql)
		for _, destructive := range []string{"DROP ", "DELETE ", "UPDATE ", "TRUNCATE", "ALTER COLUMN", "RENAME"} {
			if strings.Contains(upper, destructive) {
				t.Fatalf("unique-exit migration is not additive (%q): %s", destructive, sql)
			}
		}
	}
}
