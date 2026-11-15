package api_test

// FS-98's Team cases against server's own sequencer (CORE-M6-04; §5.6's revocation, D-CORE-20, §7.9's scoped, authority
// and cascade rows): the schedules sidecartest/fault/fs98_team.go registers and names, never a second copy, on Nets
// whose authorization log is model.ConfigLogSequencer, as fs98_db_test.go runs the CLI case's. A real Sidecar in a real
// daemon signs each revocation, proposal and Suspend, server's check judges it at the tip, and the suite's readers
// apply what server sequenced, compared after every entry. The log's half of T3, which needs no private store, as
// fs98_db_test.go says.

import (
	"testing"

	"github.com/urnetwork/sidecar/sidecartest/fault"
)

func TestFs98TeamOnServersSequencer(t *testing.T) {
	fs98SequencerRow(t, "FS-98", "team")
}

func TestFs98TeamSelfAdmitOnOnServersSequencer(t *testing.T) {
	fs98SequencerClause(t, fault.Fs98TeamSelfAdmitOn)
}

func TestFs98TeamSelfAdmitOffOnServersSequencer(t *testing.T) {
	fs98SequencerClause(t, fault.Fs98TeamSelfAdmitOff)
}

func TestFs98TeamSelfAdmitOffBoundsOnServersSequencer(t *testing.T) {
	fs98SequencerClause(t, fault.Fs98TeamSelfAdmitOffBounds)
}

func TestFs98TeamCompromiseSelfAdmitOffOnServersSequencer(t *testing.T) {
	fs98SequencerClause(t, fault.Fs98TeamCompromiseSelfAdmitOff)
}

func TestFs98TeamSuspendedTargetRevokedAtCosignOnServersSequencer(t *testing.T) {
	fs98SequencerClause(t, fault.Fs98TeamSuspendedTargetRevokedAtCosign)
}
