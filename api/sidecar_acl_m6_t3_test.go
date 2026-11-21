//go:build sidecar_localstack

package api_test

// ACL-M6-05's schedules on T3 (sidecartest/fault/fs_acl_m6.go; §22.8's FS-19, FS-21 and FS-85 at M6-out, S13's grant
// steps, S3b's and S4b's acl checks): a Team's access rules on server's sequencer over the local stack's MinIO, so
// api's real verifier judges every append: the directory's RepoCreate under the team tier, which the schedule's world
// names at the sequencer (model.ConfigLogSettings.TeamTier), and every GrantActivate under §16.6's spend check, which
// the world installs there as at every reader (model.ConfigLogSettings.AclSpendCheck). The forged clauses run on T2
// alone, as each schedule says. FS-21's team runs its clients on server's exchange under server's contract gate, so it
// needs the schema. Each needs the local stack (local/stack.sh up) and fails rather than skips without it.

import (
	"testing"

	"github.com/urnetwork/sidecar/sidecartest/fault"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/sidecart3"
)

func TestFs19TeamOnT3(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(tb testing.TB) {
		sidecart3.RunRow(tb, "FS-19", "team", nil)
	})
}

func TestFs19InviteOnT3(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(tb testing.TB) {
		sidecart3.RunRow(tb, "FS-19", "invite", nil)
	})
}

// The fixed schedule, FI-10 after every step; sidecart3's TestT3Rows runs the case under §22.8's rules.
func TestFs21TeamOnT3(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(tb testing.TB) {
		sidecart3.Run(tb, fault.Fs21Team, nil)
	})
}

func TestFs85AclRepoRequestOnT3(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(tb testing.TB) {
		sidecart3.RunRegisteredClause(tb, "FS-85", "acl repo request", nil)
	})
}

func TestFs85AclOwnerBoundRequestOnT3(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(tb testing.TB) {
		sidecart3.RunRegisteredClause(tb, "FS-85", "acl owner-bound request", nil)
	})
}

func TestFs85AclWorkerRequestOnT3(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(tb testing.TB) {
		sidecart3.RunRegisteredClause(tb, "FS-85", "acl worker request", nil)
	})
}

func TestFs85AclCopiedDecisionOnT3(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(tb testing.TB) {
		sidecart3.RunRegisteredClause(tb, "FS-85", "acl copied decision", nil)
	})
}

func TestAclS13GrantStepsOnT3(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(tb testing.TB) {
		sidecart3.Run(tb, fault.AclS13GrantSteps, nil)
	})
}

func TestAclMemberAddsOnlyForReadersOnT3(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(tb testing.TB) {
		sidecart3.Run(tb, fault.AclMemberAddsOnlyForReaders, nil)
	})
}

func TestAclMemberRemovalDropsEveryRightOnT3(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(tb testing.TB) {
		sidecart3.Run(tb, fault.AclMemberRemovalDropsEveryRight, nil)
	})
}
