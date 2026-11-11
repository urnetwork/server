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

func TestFs58FoldKilledAndRacedOnT3(t *testing.T) {
	runClauseOnT3(t, "FS-58", "folds killed and raced", fault.Fs58FoldKilledAndRaced)
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

func TestFs4ThreeSourcesOnT3(t *testing.T) {
	runClauseOnT3(t, "FS-4", "three sources", fault.Fs4ThreeSources)
}

func TestFs6OnT3(t *testing.T) {
	fsCommitTestEnv().Run(t, func(tb testing.TB) {
		sidecart3.RunRow(tb, "FS-6", "", nil)
	})
}

func TestFs6SevenDayStartOnT3(t *testing.T) {
	runClauseOnT3(t, "FS-6", "seven-day start", fault.Fs6SevenDayStart)
}

func TestFs6KilledAtEachPhaseOnT3(t *testing.T) {
	runClauseOnT3(t, "FS-6", "killed at each phase", fault.Fs6KilledAtEachPhase)
}

func TestFs6StaleJobOnT3(t *testing.T) {
	runClauseOnT3(t, "FS-6", "stale job", fault.Fs6StaleJob)
}

// FS-10 forges server state, so its cases run on T2 alone and their twins, which must succeed, here (§22.8's cadence
// rule).
func TestFs10TwinOnT3(t *testing.T) {
	runClauseOnT3(t, "FS-10", "twin", fault.Fs10WriterChange)
}

func TestFs10RollbackTwinOnT3(t *testing.T) {
	runClauseOnT3(t, "FS-10", "rollback refused twin", fault.Fs10RollbackTwin)
}

func TestFs12OnT3(t *testing.T) {
	fsCommitTestEnv().Run(t, func(tb testing.TB) {
		sidecart3.RunRow(tb, "FS-12", "", nil)
	})
}
