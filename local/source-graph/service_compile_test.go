// A reviewed graph must compile api/payout commands and their regression packages.
// Compilation exercises actual public interfaces without loading service config.
package sourcegraph

import (
	"os"
	"os/exec"
	"testing"
)

// Stale sibling pins fail through the actual commands, not a mirrored symbol list.
func TestReleaseSourceGraphBuildsRuntimeConsumers(t *testing.T) {
	command := exec.CommandContext(t.Context(), "go", "build", "-mod=readonly", "-p", "2", "-trimpath", "-o", t.TempDir(), "./cli/api", "./cli/taskworker", "./bringyourctl")
	command.Dir = serverDirectory(t)
	command.Env = append(os.Environ(), "GOWORK=off", "GOTOOLCHAIN=local", "CGO_ENABLED=0", "GOEXPERIMENT=greenteagc", "GOFLAGS=-mod=readonly", "GOPROXY=off", "GOSUMDB=off")
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("compile runtime consumers from the reviewed immutable graph: %v\n%s", err, output)
	}
}

// Compile api and payout regression packages without starting integration fixtures.
func TestReleaseSourceGraphCompilesRuntimeConsumerTests(t *testing.T) {
	command := exec.CommandContext(t.Context(), "go", "test", "-mod=readonly", "-p", "2", "-c", "-o", t.TempDir(), "./api", "./taskworker/work", "./controller")
	command.Dir = serverDirectory(t)
	command.Env = append(os.Environ(), "GOWORK=off", "GOTOOLCHAIN=local", "CGO_ENABLED=0", "GOEXPERIMENT=greenteagc", "GOFLAGS=-mod=readonly", "GOPROXY=off", "GOSUMDB=off")
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("compile runtime consumer tests from the reviewed immutable graph: %v\n%s", err, output)
	}
}
