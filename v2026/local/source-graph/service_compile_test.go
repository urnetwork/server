// A reviewed graph must compile each service that consumes runtime capabilities.
package sourcegraph

import (
	"os"
	"os/exec"
	"testing"
)

// Actual service entry points expose stale sibling APIs that module pins and
// dependency-only tests cannot detect, including ALT's admission constructor.
func TestReleaseSourceGraphBuildsRuntimeConsumers(t *testing.T) {
	command := exec.CommandContext(t.Context(), "go", "build", "-mod=readonly", "-p", "2", "-trimpath", "-o", t.TempDir(), "./cli/api", "./cli/alt", "./cli/taskworker", "./cli/connect", "./cli/proxy", "./bringyourctl")
	command.Dir = serverDirectory(t)
	command.Env = append(os.Environ(), "GOWORK=off", "GOTOOLCHAIN=local", "GOENV=off", "CGO_ENABLED=0", "GOFLAGS=-mod=readonly", "GOPROXY=off", "GOSUMDB=off")
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("compile runtime consumers from the reviewed source graph: %v\n%s", err, output)
	}
}
