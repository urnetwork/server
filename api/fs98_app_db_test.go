package api_test

// FS-98's and FS-124's app-host cases against server's own sequencer (CORE-M8-03; §5.8's two calls, D-CORE-20,
// D-SDK-30): the schedules sidecartest/fault/fs98_app.go registers and names, never a second copy, on Nets whose
// authorization log is model.ConfigLogSequencer, as fs98_db_test.go runs the CLI case's. A phone's app signs in process
// under its presence signer and a desktop app in remote mode by the daemon's two calls, server's check judges each
// countersigned statement at the tip, and the suite's readers apply what server sequenced, compared after every
// entry. The log's half of T3, which needs no private store, as fs98_db_test.go says; the sdk's adapters are §18's T4.

import (
	"testing"

	"github.com/urnetwork/sidecar/sidecartest/fault"
)

func TestFs98AppHostOnServersSequencer(t *testing.T) {
	fs98SequencerRow(t, "FS-98", "app-host")
}

func TestFs98AppHostPhoneOnServersSequencer(t *testing.T) {
	fs98SequencerClause(t, fault.Fs98AppHostPhone)
}

func TestFs98AppHostRemoteOnServersSequencer(t *testing.T) {
	fs98SequencerClause(t, fault.Fs98AppHostRemote)
}

func TestFs124AppHostOnServersSequencer(t *testing.T) {
	fs98SequencerRow(t, "FS-124", "app-host")
}
