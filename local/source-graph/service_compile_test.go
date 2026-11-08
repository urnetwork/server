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

// The same consumers as local and canary build them, with the sidecar's pending
// migration groups linked (cli/sidecar-tags.mk, §19.7), so a group that compiles
// on one leg alone fails here rather than at a local run or a canary deploy.
func TestReleaseSourceGraphBuildsThePendingLeg(t *testing.T) {
	command := exec.CommandContext(t.Context(), "go", "build", "-mod=readonly", "-p", "2", "-trimpath", "-tags", "sidecar_pending", "-o", t.TempDir(), "./cli/api", "./cli/alt", "./cli/taskworker", "./cli/connect", "./cli/proxy", "./bringyourctl")
	command.Dir = serverDirectory(t)
	command.Env = append(os.Environ(), "GOWORK=off", "GOTOOLCHAIN=local", "GOENV=off", "CGO_ENABLED=0", "GOFLAGS=-mod=readonly", "GOPROXY=off", "GOSUMDB=off")
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("compile runtime consumers with the pending groups linked: %v\n%s", err, output)
	}
}

// No ./cli/<svc> links sidecar yet, so the list above cannot show that sidecar builds
// on server's module versions. Each service's command joins that list as it lands, and
// until then this compiles the packages those commands will import. The tools-tagged
// sidecar_require.go holds the requires and links nothing into any service.
func TestReleaseSourceGraphBuildsSidecarPackages(t *testing.T) {
	command := exec.CommandContext(t.Context(), "go", "build", "-mod=readonly", "-p", "2", "-trimpath",
		"github.com/urnetwork/sidecar", "github.com/urnetwork/sidecar/protocol", "github.com/urnetwork/sidecar/protocol/bridge", "github.com/urnetwork/sidecar/smtpproof")
	command.Dir = serverDirectory(t)
	command.Env = append(os.Environ(), "GOWORK=off", "GOTOOLCHAIN=local", "GOENV=off", "CGO_ENABLED=0", "GOFLAGS=-mod=readonly", "GOPROXY=off", "GOSUMDB=off")
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("compile sidecar's packages from the reviewed source graph: %v\n%s", err, output)
	}
}
