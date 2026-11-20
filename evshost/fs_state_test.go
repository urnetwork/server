//go:build sidecar_localstack && sidecar_testhooks

// EVS1-M2-41's fold, epoch, read and lifecycle rows on T3 (§22.4, §22.8), as fs_commit_test.go runs EVS1-M2-40's: the
// builders sidecartest/fault registers, on T3's Nets, through this package's block on the private Redis and the local
// stack's MinIO, under every rule each row takes with the nightly seeds.

package evshost_test

import (
	"testing"

	"github.com/urnetwork/sidecar/sidecartest/fault"

	"github.com/urnetwork/server/sidecart3"
)

func TestFs4OnT3(t *testing.T) {
	fsCommitTestEnv().Run(t, func(tb testing.TB) {
		sidecart3.RunRow(tb, "FS-4", "", nil)
	})
}

func TestFs58OnT3(t *testing.T) {
	fsCommitTestEnv().Run(t, func(tb testing.TB) {
		sidecart3.RunRow(tb, "FS-58", "", nil)
	})
}

func TestFs58FoldsKilledAndRacedOnT3(t *testing.T) {
	runClauseOnT3(t, "FS-58", "folds killed and raced")
}

func TestFs63OnT3(t *testing.T) {
	fsCommitTestEnv().Run(t, func(tb testing.TB) {
		sidecart3.RunRow(tb, "FS-63", "", nil)
	})
}

func TestFs62OnT3(t *testing.T) {
	fsCommitTestEnv().Run(t, func(tb testing.TB) {
		sidecart3.RunRow(tb, "FS-62", "", nil)
	})
}

func TestFs62LeaseHolderResyncOnT3(t *testing.T) {
	runClauseOnT3(t, "FS-62", "lease holder resync")
}

func TestFs4TwoSourcesOnT3(t *testing.T) {
	runClauseOnT3(t, "FS-4", "two sources")
}

func TestFs6OnT3(t *testing.T) {
	fsCommitTestEnv().Run(t, func(tb testing.TB) {
		sidecart3.RunRow(tb, "FS-6", "", nil)
	})
}

func TestFs6SevenDayStartOnT3(t *testing.T) {
	runClauseOnT3(t, "FS-6", "seven-day start")
}

func TestFs6KilledAtEachPhaseOnT3(t *testing.T) {
	runClauseOnT3(t, "FS-6", "killed at each phase")
}

func TestFs6StaleJobOnT3(t *testing.T) {
	runClauseOnT3(t, "FS-6", "stale job")
}

// FS-10 forges server state, so its cases run on T2 alone and their twins, which must succeed, here (§22.8's cadence
// rule): the registered case's twin under the name M2's sweep gives it, and the rollback clause's by the convention,
// which runs a forged case's clause's twin on T3.
func TestFs10TwinOnT3(t *testing.T) {
	fsCommitTestEnv().Run(t, func(tb testing.TB) {
		fault.RunClause(tb, "FS-10", "twin", fault.Fs10WriterChange, sidecart3.RuleSettings(tb, nil))
	})
}

func TestFs10RollbackRefusedOnT3(t *testing.T) {
	runClauseOnT3(t, "FS-10", "rollback refused")
}

func TestFs12OnT3(t *testing.T) {
	fsCommitTestEnv().Run(t, func(tb testing.TB) {
		sidecart3.RunRow(tb, "FS-12", "", nil)
	})
}

func TestFs4BatchedEpochOnT3(t *testing.T) {
	runClauseOnT3(t, "FS-4", "batched epoch")
}
