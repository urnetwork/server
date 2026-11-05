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

// The unique-exit migrations are in the list, in order, one contiguous block:
// the monitor's contracts version each by its position.
func TestProxyUniqueExitMigrationVersionsAreContiguous(t *testing.T) {
	v := ProxyUniqueExitMigrationVersion()
	versions := []int{
		v.EgressKeyColumns,
		v.UniqueExitTable,
		v.UniqueExitNetworkIndex,
		v.AssignmentTable,
		v.AssignmentNetworkEgressKeyLiveIndex,
		v.AssignmentProxyLiveIndex,
		v.AssignmentProviderLiveIndex,
		v.AssignmentAssignedAtLiveIndex,
		v.AssignmentProxyReleasedIndex,
		v.AssignmentNetworkReleasedIndex,
		v.AssignmentReleasedAtIndex,
	}
	for i, version := range versions {
		if version == 0 {
			t.Fatalf("unique-exit migration %d is not in the migration list", i)
		}
		if 0 < i && version != versions[i-1]+1 {
			t.Fatalf("unique-exit migrations are not contiguous: %v", versions)
		}
	}
}
