// A reviewed graph must compile payout commands and their regression packages.
// Compilation exercises actual public interfaces without loading service config.
package sourcegraph

import (
	"os"
	"os/exec"
	"testing"
)

// Stale sibling pins fail through the actual commands, not a mirrored symbol list.
func TestReleaseSourceGraphBuildsPayoutCommands(t *testing.T) {
	command := exec.CommandContext(t.Context(), "go", "build", "-mod=readonly", "-p", "2", "-trimpath", "-o", t.TempDir(), "./cli/taskworker", "./bringyourctl")
	command.Dir = serverDirectory(t)
	command.Env = append(os.Environ(), "GOWORK=off", "GOTOOLCHAIN=local", "CGO_ENABLED=0", "GOEXPERIMENT=greenteagc", "GOFLAGS=-mod=readonly", "GOPROXY=off", "GOSUMDB=off")
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("compile payout commands from the reviewed immutable graph: %v\n%s", err, output)
	}
}

// Compile the payout regression packages without starting their integration fixtures.
func TestReleaseSourceGraphCompilesPayoutTests(t *testing.T) {
	command := exec.CommandContext(t.Context(), "go", "test", "-mod=readonly", "-p", "2", "-c", "-o", t.TempDir(), "./taskworker/work", "./controller")
	command.Dir = serverDirectory(t)
	command.Env = append(os.Environ(), "GOWORK=off", "GOTOOLCHAIN=local", "CGO_ENABLED=0", "GOEXPERIMENT=greenteagc", "GOFLAGS=-mod=readonly", "GOPROXY=off", "GOSUMDB=off")
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("compile payout tests from the reviewed immutable graph: %v\n%s", err, output)
	}
}
