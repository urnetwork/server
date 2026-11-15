package api_test

// FS-98's and FS-124's CLI cases against server's own sequencer (CORE-M3-04; §5.6's revocation, D-CORE-20,
// D-CORE-13): the schedules sidecartest/fault/fs98.go registers and names, never a second copy, on Nets whose
// authorization log is model.ConfigLogSequencer (sidecart3.ConfigLogApi) over the Net's first metadata cluster. A real
// Sidecar in a real daemon signs each revocation, proposal, Suspend, cosign and presence key statement the way ur's
// devices methods do, server's check judges it at the tip, and the suite's reference, new devices, gates and writer
// follower apply what server sequenced, compared after every entry.
//
// This is the log's half of T3, which needs no private store: the Sidecars enroll through sidecartest's fake api and
// their clients ride its platform, since sidecart3 does not host server's enrollment yet (TEST-M1-08). The whole T3
// run, on sidecart3's Nets over the local stack, is sidecart3's TestT3Rows and fs98_test.go, which fail on those
// platform fakes until it does.

import (
	"testing"

	"github.com/urnetwork/sidecar/sidecartest"
	"github.com/urnetwork/sidecar/sidecartest/fault"

	"github.com/urnetwork/server/sidecart3"
)

// A Net of T2's fakes whose authorization log is server's sequencer.
func fs98SequencerNetSettings() *sidecartest.NetSettings {
	settings := sidecartest.DefaultNetSettings()
	settings.ConfigLogApi = sidecart3.ConfigLogApi()
	settings.ConfigLogSettings = sidecart3.ConfigLogSuiteSettings()
	return settings
}

// A registered case under every rule its row takes, on server's sequencer.
func fs98SequencerRow(t *testing.T, rowId string, caseName string) {
	t.Helper()
	registration := fault.Lookup(rowId, caseName, fault.TierT2)
	if registration == nil {
		t.Fatalf("%s %q has no builder registered", rowId, caseName)
	}
	settings := fault.DefaultRuleSettings()
	settings.Net = fs98SequencerNetSettings()
	// a failing seed's trace is the sidecar repository's to keep
	settings.WriteTraces = false
	report := fault.CheckRow(t.Context(), registration, settings)
	t.Log(report.String())
	for _, failure := range report.Failures {
		t.Errorf("%s", failure)
	}
	for _, err := range report.Errors {
		t.Errorf("%v", err)
	}
}

// One named clause's schedule, fixed, on server's sequencer.
func fs98SequencerClause(t *testing.T, builder fault.Builder) {
	t.Helper()
	net := sidecartest.NewNet(t.Context(), fs98SequencerNetSettings())
	defer net.Close()
	result := fault.Run(t.Context(), builder(net), net)
	if result.Err != nil {
		t.Fatalf("%s", result)
	}
	t.Log(result.String())
}

func TestFs98CliOnServersSequencer(t *testing.T) {
	fs98SequencerRow(t, "FS-98", "cli")
}

func TestFs98CliMemberOnServersSequencer(t *testing.T) {
	fs98SequencerClause(t, fault.Fs98CliMember)
}

func TestFs98CliCompromiseOnServersSequencer(t *testing.T) {
	fs98SequencerClause(t, fault.Fs98CliCompromise)
}

func TestFs98CliProposalOnServersSequencer(t *testing.T) {
	fs98SequencerClause(t, fault.Fs98CliProposal)
}

func TestFs98CliProposalExpiresOnServersSequencer(t *testing.T) {
	fs98SequencerClause(t, fault.Fs98CliProposalExpires)
}

func TestFs124CliOnServersSequencer(t *testing.T) {
	fs98SequencerRow(t, "FS-124", "cli")
}

func TestFs124CliApprovalsOnServersSequencer(t *testing.T) {
	fs98SequencerClause(t, fault.Fs124CliApprovals)
}

func TestFs124CliCosignOnServersSequencer(t *testing.T) {
	fs98SequencerClause(t, fault.Fs124CliCosign)
}

func TestFs124CliRecoveryRotateOnServersSequencer(t *testing.T) {
	fs98SequencerClause(t, fault.Fs124CliRecoveryRotate)
}

// §22.7's revocation suite with X revoked by its own member as lost and as a compromise, CORE's check judging each way
// on server's sequencer.
func TestRevocationSuiteOwnDeviceFormOnServersSequencer(t *testing.T) {
	settings := sidecartest.DefaultRevocationSuiteSettings()
	settings.Net = fs98SequencerNetSettings()
	settings.Checks = []*sidecartest.RevocationCheck{fault.Fs98RevocationCheck}
	report := sidecartest.RunRevocationSuite(t, sidecartest.RevocationFormOwnDevice, settings)
	for _, way := range sidecartest.RevocationFormOwnDevice.Ways() {
		judged := false
		for _, result := range report.Of(way) {
			judged = judged || (result.CheckId == fault.Fs98RevocationCheck.Id && result.Err == nil && result.Open == "")
		}
		if !judged {
			t.Errorf("%s: CORE's check did not judge", way)
		}
	}
}
