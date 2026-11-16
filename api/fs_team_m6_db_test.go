package api_test

// FS-97's and FS-124's Team cases, S3b's and S4b's statements and the revocation suite's member form against server's
// own sequencer (TEAM-M6-12; §7.9's presence, several and counted rows, §7.10's stale and cleanup rows): the schedules
// sidecartest/fault/fs_team_m6.go registers and names, never a second copy, on Nets whose authorization log is
// model.ConfigLogSequencer (sidecart3.ConfigLogApi), as fs98_db_test.go runs CORE's. Each statement a schedule's device
// signs is judged by server's check at the tip, and the suite's readers apply what server sequenced, compared after
// every entry. The log's half of T3, which needs no private store; the forged clauses run on T2 alone, so here a
// schedule that holds one logs it and runs the rest. sidecart3's runs over the local stack are fs_team_m6_t3_test.go.

import (
	"testing"

	"github.com/urnetwork/sidecar/sidecartest"
	"github.com/urnetwork/sidecar/sidecartest/fault"
)

func TestFs97TeamOnServersSequencer(t *testing.T) {
	fs98SequencerRow(t, "FS-97", "team")
}

func TestFs97TeamAdminSetMemberOnServersSequencer(t *testing.T) {
	fs98SequencerClause(t, fault.Fs97TeamAdminSetMember)
}

func TestFs97TeamCleanupOnServersSequencer(t *testing.T) {
	fs98SequencerClause(t, fault.Fs97TeamCleanup)
}

func TestFs124TeamOnServersSequencer(t *testing.T) {
	fs98SequencerRow(t, "FS-124", "team")
}

func TestS3bMemberAdmitStatementsOnServersSequencer(t *testing.T) {
	fs98SequencerClause(t, fault.TeamMemberAdmitStatements)
}

func TestS4bMemberRevokeStatementsOnServersSequencer(t *testing.T) {
	fs98SequencerClause(t, fault.TeamMemberRevokeStatements)
}

// §22.7's revocation suite in its member form, TEAM's check judging each way on server's sequencer.
func TestRevocationSuiteMemberFormOnServersSequencer(t *testing.T) {
	settings := sidecartest.DefaultRevocationSuiteSettings()
	settings.Net = fs98SequencerNetSettings()
	settings.Checks = []*sidecartest.RevocationCheck{fault.TeamMemberRevocationCheck}
	report := sidecartest.RunRevocationSuite(t, sidecartest.RevocationFormMember, settings)
	for _, way := range sidecartest.RevocationFormMember.Ways() {
		judged := false
		for _, result := range report.Of(way) {
			judged = judged || (result.CheckId == fault.TeamMemberRevocationCheck.Id && result.Err == nil && result.Open == "")
		}
		if !judged {
			t.Errorf("%s: TEAM's check did not judge", way)
		}
	}
}
