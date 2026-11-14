//go:build sidecar_localstack

// FS-98's and FS-124's CLI cases on T3 (CORE-M3-04): the builders sidecartest/fault/fs98.go registers and names, on
// T3's Nets, so every statement a host's Sidecar signs goes through server's real sequencer over the local stack's
// MinIO (config_log.go). The registered cases run under every rule their rows take; the named clauses run fixed.
//
// Each schedule adds real Sidecars, which enroll through sidecartest's fake api until sidecart3 hosts server's
// enrollment (TEST-M1-08), so on T3's Nets each fails on the platform fakes until then, as every row with sidecars
// does (Q-TEST-1). The log's half runs today without the stack in server/api/fs98_db_test.go.

package sidecart3

import (
	"testing"

	"github.com/urnetwork/sidecar/sidecartest/fault"
)

func TestFs98CliOnT3(t *testing.T) {
	testEnvWithSchema().Run(t, func(tb testing.TB) {
		RunRow(tb, "FS-98", "cli", nil)
	})
}

func TestFs98CliMemberOnT3(t *testing.T) {
	testEnvWithSchema().Run(t, func(tb testing.TB) {
		Run(tb, fault.Fs98CliMember, nil)
	})
}

func TestFs98CliCompromiseOnT3(t *testing.T) {
	testEnvWithSchema().Run(t, func(tb testing.TB) {
		Run(tb, fault.Fs98CliCompromise, nil)
	})
}

func TestFs98CliProposalOnT3(t *testing.T) {
	testEnvWithSchema().Run(t, func(tb testing.TB) {
		Run(tb, fault.Fs98CliProposal, nil)
	})
}

func TestFs98CliProposalExpiresOnT3(t *testing.T) {
	testEnvWithSchema().Run(t, func(tb testing.TB) {
		Run(tb, fault.Fs98CliProposalExpires, nil)
	})
}

func TestFs124CliOnT3(t *testing.T) {
	testEnvWithSchema().Run(t, func(tb testing.TB) {
		RunRow(tb, "FS-124", "cli", nil)
	})
}
