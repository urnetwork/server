//go:build linux

// Opt-in native diagnosis checks independent management admission without
// starting a service, reading a protocol frame, or claiming execution readiness.
package competitionagent

import (
	"os"
	"testing"
)

// The explicit protected configuration comes from the native test operator,
// never a repository fixture or an untrusted submission.
func TestManagementAdmissionNative(t *testing.T) {
	path := os.Getenv("COMPETITION_MANAGEMENT_NATIVE_CONFIG")
	if path == "" || os.Geteuid() != 0 {
		t.Skip("requires explicit root native management configuration")
	}
	config, err := LoadRecoveryConfig(path)
	if err != nil {
		t.Fatal("protected recovery loader", err)
	}
	settings := DefaultManagementSshSettings()
	if err := verifyManagementSshAmbientCapabilities(settings); err != nil {
		t.Fatal("native ambient capability profile", err)
	}
	if _, _, err := admitManagementSsh(t.Context(), config, settings); err != nil {
		t.Fatal("independent management admission", err)
	}
	t.Log("read-only management admission passed; framed control and evaluator qualification not exercised")
}
