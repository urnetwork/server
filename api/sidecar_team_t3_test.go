//go:build sidecar_localstack

package api_test

// §22.7's suites in their Team forms on T3 (TEAM-M6-12): each Team row through server's real sequencer over the local
// stack's MinIO as api's first metadata cluster, in a bucket of the run's own (admissionT3Tier), as the admission
// suite's T3 rows run. FS-97's and FS-124's Team cases, S3b's and S4b's statements and the member form of the
// revocation suite run on sidecart3's Nets. Each needs the local stack (local/stack.sh up) and fails rather than skips
// without it.

import (
	"testing"

	"github.com/urnetwork/sidecar/sidecartest"
	"github.com/urnetwork/sidecar/sidecartest/fault"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/sidecart3"
)

func TestTeamKOfNEndToEndOnT3(t *testing.T) {
	sidecartest.RunTeamRow(t, "k of n end to end", admissionT3Tier(t), nil)
}

func TestTeamTwoAdminsForceKOfOneOnT3(t *testing.T) {
	sidecartest.RunTeamRow(t, "n at most 2 forces k = 1", admissionT3Tier(t), nil)
}

func TestTeamSeatsInTheDirectoryGroupOnT3(t *testing.T) {
	sidecartest.RunTeamRow(t, "seats in the directory group", admissionT3Tier(t), nil)
}

func TestTeamAdmissionsMeetNoMemberDeviceCapOnT3(t *testing.T) {
	sidecartest.RunTeamRow(t, "admissions meet no member-device cap", admissionT3Tier(t), nil)
}

func TestTeamEphemeralWritesNoConfigOnT3(t *testing.T) {
	sidecartest.RunTeamRow(t, "an ephemeral writes no config", admissionT3Tier(t), nil)
}

func TestFs97TeamOnT3(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(tb testing.TB) {
		sidecart3.RunRow(tb, "FS-97", "team", nil)
	})
}

func TestFs97TeamAdminSetMemberOnT3(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(tb testing.TB) {
		sidecart3.Run(tb, fault.Fs97TeamAdminSetMember, nil)
	})
}

func TestFs97TeamCleanupOnT3(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(tb testing.TB) {
		sidecart3.Run(tb, fault.Fs97TeamCleanup, nil)
	})
}

func TestFs124TeamOnT3(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(tb testing.TB) {
		sidecart3.RunRow(tb, "FS-124", "team", nil)
	})
}

func TestS3bMemberAdmitStatementsOnT3(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(tb testing.TB) {
		sidecart3.Run(tb, fault.TeamMemberAdmitStatements, nil)
	})
}

func TestS4bMemberRevokeStatementsOnT3(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(tb testing.TB) {
		sidecart3.Run(tb, fault.TeamMemberRevokeStatements, nil)
	})
}
