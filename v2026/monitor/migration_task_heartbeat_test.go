// The task queue fillfactor contract is keyed by its migration's SQL, not by a
// version number a merge could make stale.
package monitor

import (
	"testing"

	"github.com/urnetwork/server/v2026"
)

// The binary publishes the migration, and the registry's contract at its
// version is the fillfactor artifact.
func TestTaskHeartbeatFillfactorContractFollowsItsMigration(t *testing.T) {
	version := server.TaskHeartbeatFillfactorMigrationVersion()
	if version <= 0 || server.MigrationCount() < version {
		t.Fatal("the task queue fillfactor migration is not published in this binary", version, server.MigrationCount())
	}
	contract := migrationCurrentSuffixTestContract(t, version)
	if contract.artifact.name != "task queue pages keep room for heartbeat versions" ||
		contract.query != pendingTaskFillfactorArtifactQuery || contract.artifact.removedVersion != 0 {
		t.Fatal("the contract at the fillfactor migration's version is not its artifact", contract.artifact, contract.query)
	}
}
