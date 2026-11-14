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
	command := exec.CommandContext(t.Context(), "go", "build", "-mod=readonly", "-p", "2", "-trimpath", "-o", t.TempDir(), "./cli/api", "./cli/alt", "./cli/taskworker", "./cli/connect", "./cli/proxy", "./cli/dns", "./cli/evs", "./cli/gkm", "./cli/mxedge", "./cli/sockets", "./cli/tlsedge", "./bringyourctl")
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
	command := exec.CommandContext(t.Context(), "go", "build", "-mod=readonly", "-p", "2", "-trimpath", "-tags", "sidecar_pending", "-o", t.TempDir(), "./cli/api", "./cli/alt", "./cli/taskworker", "./cli/connect", "./cli/proxy", "./cli/dns", "./cli/evs", "./cli/gkm", "./cli/mxedge", "./cli/sockets", "./cli/tlsedge", "./bringyourctl")
	command.Dir = serverDirectory(t)
	command.Env = append(os.Environ(), "GOWORK=off", "GOTOOLCHAIN=local", "GOENV=off", "CGO_ENABLED=0", "GOFLAGS=-mod=readonly", "GOPROXY=off", "GOSUMDB=off")
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("compile runtime consumers with the pending groups linked: %v\n%s", err, output)
	}
}

// Every command above links the root, protocol and protocol/bridge through controller's
// sidecar identity routes, ./cli/dns links dnsservice and miekg/dns, ./cli/evs links
// evsservice and pushservice, ./cli/gkm links gkmservice, evsservice's write budget and
// pushservice, ./cli/mxedge links mxedgeservice, tlsedgeservice and
// sidecar's go-smtp fork, ./cli/sockets links socketsservice, tlsedgeservice's
// kill-hash reader and pushservice, and ./cli/tlsedge links tlsedgeservice and the root's edge client
// pool, but none links smtpproof. Each ./cli/<svc> hosting a sidecar engine joins the list above as it lands,
// with the requires its engine brings, and this compiles smtpproof, which the
// tools-tagged sidecar_require.go holds in the graph, beside the packages already linked.
func TestReleaseSourceGraphBuildsSidecarPackages(t *testing.T) {
	command := exec.CommandContext(t.Context(), "go", "build", "-mod=readonly", "-p", "2", "-trimpath",
		"github.com/urnetwork/sidecar", "github.com/urnetwork/sidecar/protocol", "github.com/urnetwork/sidecar/protocol/bridge", "github.com/urnetwork/sidecar/smtpproof")
	command.Dir = serverDirectory(t)
	command.Env = append(os.Environ(), "GOWORK=off", "GOTOOLCHAIN=local", "GOENV=off", "CGO_ENABLED=0", "GOFLAGS=-mod=readonly", "GOPROXY=off", "GOSUMDB=off")
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("compile sidecar's packages from the reviewed source graph: %v\n%s", err, output)
	}
}
