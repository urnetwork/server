package api_test

// §22.7's suites in their Team forms against server's own sequencer (TEAM-M6-12; sidecartest/suite_team.go): each
// Team row its own top-level test, on a Team network whose authorization log is model.ConfigLogSequencer over a fake
// first metadata cluster, every entry read back and applied by the suite's reference, a new device and a gate at each
// base and the writer's follower. The log's half of T3, which needs no private store; the rows over the local stack's
// MinIO are sidecar_team_t3_test.go's. The forged clause of the ephemeral row runs on T2 alone, which that row logs.

import (
	"testing"

	"github.com/urnetwork/sidecar/sidecartest"

	"github.com/urnetwork/server/sidecart3"
)

// server's sequencer over a fake first metadata cluster of the test's own.
func teamSequencerTier(t *testing.T) *sidecartest.AdmissionTier {
	t.Helper()
	blobs := sidecartest.NewFakeBlobs(sidecartest.NewFakeClock(sidecartest.DefaultClockStart()), sidecartest.NewFaultQueue(), nil, sidecartest.DefaultFakeBlobsSettings())
	cluster, err := blobs.FirstCluster()
	if err != nil {
		t.Fatal(err)
	}
	return &sidecartest.AdmissionTier{Cluster: cluster, NewApi: sidecart3.ConfigLogApi(), Settings: sidecart3.ConfigLogSuiteSettings()}
}

func TestTeamKOfNEndToEndOnServersSequencer(t *testing.T) {
	sidecartest.RunTeamRow(t, "k of n end to end", teamSequencerTier(t), nil)
}

func TestTeamTwoAdminsForceKOfOneOnServersSequencer(t *testing.T) {
	sidecartest.RunTeamRow(t, "n at most 2 forces k = 1", teamSequencerTier(t), nil)
}

func TestTeamSeatsInTheDirectoryGroupOnServersSequencer(t *testing.T) {
	sidecartest.RunTeamRow(t, "seats in the directory group", teamSequencerTier(t), nil)
}

func TestTeamAdmissionsMeetNoMemberDeviceCapOnServersSequencer(t *testing.T) {
	sidecartest.RunTeamRow(t, "admissions meet no member-device cap", teamSequencerTier(t), nil)
}

func TestTeamEphemeralWritesNoConfigOnServersSequencer(t *testing.T) {
	sidecartest.RunTeamRow(t, "an ephemeral writes no config", teamSequencerTier(t), nil)
}
