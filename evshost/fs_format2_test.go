//go:build sidecar_localstack && sidecar_testhooks

// Format 2's commit-side schedules on T3 (EVS2-M10-31; §22.8's FS-68, FS-91, FS-94 and FS-95): the versioning turns,
// rules, kinds and lock, the reuse copy racing a DROP, the shared inline record and the refused drops that
// sidecartest/fault registers and builds, on T3's Nets, where sidecart3 names this package's block as each world's
// instances, so every push, dry run, DROP and read goes through server/cli/evs's handler on the private Redis and the
// local stack's MinIO. The drop job, its dry runs and the evacuation run as the world runs them on T2, with the
// engine settings the tier gives taskworker's jobs; taskworker's own process is not hosted here.
//
// What T3 proves is the commit side. A drop job's start and a purge sweep's deletes wait on each cluster's settle flag
// (FileCluster.SettlePassed, FS-114), and the local stack's sidecar.yml names no settle_passed_build for its
// clusters, so here every schedule holds the job to that wait, with no drop moved and no segment deleted
// (fault's fsDropSettled), and the deletes, the job's refusals and what readers see after them run on T2 alone until
// T3's tier declares its clusters settled.
//
// The schedules hold instances at barriers, so this needs sidecar_testhooks; the stack must be up
// (sidecar/local/stack.sh), and these fail rather than skip without it.

package evshost_test

import (
	"testing"

	"github.com/urnetwork/sidecar/protocol"
	"github.com/urnetwork/sidecar/sidecartest/fault"

	"github.com/urnetwork/server/sidecart3"
)

func TestFs68OnT3(t *testing.T) {
	fsCommitTestEnv().Run(t, func(tb testing.TB) {
		sidecart3.RunRow(tb, "FS-68", "", nil)
	})
}

func TestFs68RulesOnT3(t *testing.T) {
	runClauseOnT3(t, "FS-68", "rules", fault.Fs68Rules)
}

func TestFs68PlatformRuleOnT3(t *testing.T) {
	runClauseOnT3(t, "FS-68", "platform rule", fault.Fs68PlatformRule)
}

func TestFs68ConfigOnT3(t *testing.T) {
	runClauseOnT3(t, "FS-68", "config", fault.Fs68Kind(protocol.RepoKind_REPO_KIND_CONFIG))
}

func TestFs68MailcfgOnT3(t *testing.T) {
	runClauseOnT3(t, "FS-68", "mailcfg", fault.Fs68Kind(protocol.RepoKind_REPO_KIND_MAILCFG))
}

func TestFs68LockOnT3(t *testing.T) {
	runClauseOnT3(t, "FS-68", "lock", fault.Fs68LockedPastTheWindow)
}

func TestFs91OnT3(t *testing.T) {
	fsCommitTestEnv().Run(t, func(tb testing.TB) {
		sidecart3.RunRow(tb, "FS-91", "", nil)
	})
}

// The copy released within §11.8's window after the DROP (QD-1105), with the store clock the row's case is flagged
// for.
func TestFs91WithinTheWindowOnT3(t *testing.T) {
	fsCommitTestEnv().Run(t, func(tb testing.TB) {
		settings := sidecart3.DefaultSettings()
		if fault.DefaultCatalog().Case("FS-91", "").StoreClock {
			settings.StoreClock = sidecart3.DefaultStoreClockSettings()
		}
		fault.RunClause(tb, "FS-91", "within the window", fault.Fs91ReleasedWithinTheWindow, sidecart3.RuleSettings(tb, settings))
	})
}

func TestFs94OnT3(t *testing.T) {
	fsCommitTestEnv().Run(t, func(tb testing.TB) {
		sidecart3.RunRow(tb, "FS-94", "", nil)
	})
}

func TestFs94RecordClassOnT3(t *testing.T) {
	runClauseOnT3(t, "FS-94", "record class", fault.Fs94RecordClass)
}

func TestFs95OnT3(t *testing.T) {
	fsCommitTestEnv().Run(t, func(tb testing.TB) {
		sidecart3.RunRow(tb, "FS-95", "", nil)
	})
}

func TestFs95LockedOnT3(t *testing.T) {
	runClauseOnT3(t, "FS-95", "locked", fault.Fs95Locked)
}

func TestFs95ConfigOnT3(t *testing.T) {
	runClauseOnT3(t, "FS-95", "config", fault.Fs95Kind(protocol.RepoKind_REPO_KIND_CONFIG))
}

func TestFs95MailcfgOnT3(t *testing.T) {
	runClauseOnT3(t, "FS-95", "mailcfg", fault.Fs95Kind(protocol.RepoKind_REPO_KIND_MAILCFG))
}

func TestFs95OwnerBoundOnT3(t *testing.T) {
	runClauseOnT3(t, "FS-95", "owner-bound", fault.Fs95OwnerBound)
}

func TestFs95ListMissingAVersionOnT3(t *testing.T) {
	runClauseOnT3(t, "FS-95", "list missing a version", fault.Fs95ListMismatch(false))
}

func TestFs95ListAddingAVersionOnT3(t *testing.T) {
	runClauseOnT3(t, "FS-95", "list adding a version", fault.Fs95ListMismatch(true))
}
