// Service commands and release tests share a reviewed source graph. Resolve it
// without service imports so missing, stale or dirty siblings fail clearly.
package sourcegraph

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// Only the effective module identity matters; local replacements can silently
// override a correct requirement and must remain visible to the assertions.
type sourceModule struct {
	Path    string
	Version string
	Dir     string
	Replace *sourceModule
}

func serverDirectory(t *testing.T) string {
	t.Helper()
	directory, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	return directory
}

// Resolve the checked-in server main module with no workspace or network fallback.
// Qualification downloads dependencies first and runs with a frozen go.sum.
func resolvedSourceModule(t *testing.T, modulePath string) sourceModule {
	t.Helper()
	return resolvedSourceModuleIn(t, serverDirectory(t), modulePath)
}

func resolvedSourceModuleIn(t *testing.T, directory, modulePath string) sourceModule {
	t.Helper()
	command := exec.CommandContext(t.Context(), "go", "list", "-mod=readonly", "-m", "-json", modulePath)
	command.Dir = directory
	command.Env = append(os.Environ(), "GOWORK=off", "GOFLAGS=-mod=readonly", "GOPROXY=off", "GOSUMDB=off")
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("resolve %s without changing source or downloading modules: %v\n%s", modulePath, err, output)
	}
	var module sourceModule
	if err := json.Unmarshal(output, &module); err != nil {
		t.Fatalf("decode resolved %s: %v\n%s", modulePath, err, output)
	}
	return module
}

// lock.yml beside this file is the source lock a clean checkout follows. Read
// its exact revision instead of maintaining a second, eventually contradictory
// pin list.
func reviewedRevision(t *testing.T, sibling, repository string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(serverDirectory(t), "local", "source-graph", "lock.yml"))
	if err != nil {
		t.Fatal(err)
	}
	var lock struct {
		Siblings []struct {
			Repository string `yaml:"repository"`
			Path       string `yaml:"path"`
			Ref        string `yaml:"ref"`
		} `yaml:"siblings"`
	}
	if err := yaml.Unmarshal(raw, &lock); err != nil {
		t.Fatal(err)
	}
	var revisions []string
	for _, checkout := range lock.Siblings {
		if checkout.Path == sibling {
			if checkout.Repository != repository || !regexp.MustCompile(`^[0-9a-f]{40}$`).MatchString(checkout.Ref) {
				t.Fatalf("%s checkout must pin a complete %s commit: %+v", sibling, repository, checkout)
			}
			revisions = append(revisions, checkout.Ref)
		}
	}
	if len(revisions) != 1 {
		t.Fatalf("expected one reviewed %s checkout, got %d", sibling, len(revisions))
	}
	return revisions[0]
}

func requireReviewedSibling(t *testing.T, modulePath, sibling, repository, subdirectory string) {
	t.Helper()
	module := resolvedSourceModule(t, modulePath)
	wantPath := filepath.ToSlash(filepath.Join("..", sibling, subdirectory))
	wantDirectory := filepath.Join(serverDirectory(t), wantPath)
	if module.Path != modulePath || module.Replace == nil || module.Replace.Path != wantPath || module.Replace.Version != "" || module.Replace.Dir != wantDirectory {
		t.Fatalf("%s resolved outside its reviewed sibling: %+v replacement=%+v", modulePath, module, module.Replace)
	}
	requireReviewedCheckout(t, sibling, repository)
}

func requireReviewedCheckout(t *testing.T, sibling, repository string) {
	t.Helper()
	revision := reviewedRevision(t, sibling, repository)
	root := filepath.Join(serverDirectory(t), "..", sibling)
	gitOutput := func(arguments ...string) string {
		t.Helper()
		command := exec.CommandContext(t.Context(), "git", arguments...)
		command.Dir = root
		output, err := command.CombinedOutput()
		if err != nil {
			t.Fatalf("inspect %s source: %v\n%s", sibling, err, output)
		}
		return strings.TrimSpace(string(output))
	}
	canonicalRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	actualRoot, err := filepath.EvalSymlinks(gitOutput("rev-parse", "--show-toplevel"))
	if err != nil || actualRoot != canonicalRoot {
		t.Fatalf("%s must be its own checkout: root=%q want=%q err=%v", sibling, actualRoot, canonicalRoot, err)
	}
	if actual := gitOutput("rev-parse", "HEAD"); actual != revision {
		t.Fatalf("%s HEAD = %s, reviewed service graph requires %s", sibling, actual, revision)
	}
	// Untracked and ignored Go or embedded files can alter the effective graph
	// even when HEAD matches. Qualification uses a complete clean checkout.
	if status := gitOutput("status", "--porcelain=v1", "--untracked-files=all", "--ignored=matching"); status != "" {
		t.Fatalf("%s has source changes outside reviewed commit %s:\n%s", sibling, revision, status)
	}
}

func TestReleaseSourceGraphPinsSdkWithoutSiblingOverride(t *testing.T) {
	requireReviewedSibling(t, "github.com/urnetwork/sdk", "sdk", "urnetwork/sdk", "")
}

func TestReleaseSourceGraphPinsConnectWithoutSiblingOverride(t *testing.T) {
	requireReviewedSibling(t, "github.com/urnetwork/connect", "connect", "urnetwork/connect", "")
}

// Dependency replacements are not inherited: SCTP must resolve to the tracked
// subtree of the same reviewed Connect revision as the service transport.
func TestReleaseSourceGraphPinsReviewedSctpFork(t *testing.T) {
	requireReviewedSibling(t, "github.com/pion/sctp", "connect", "urnetwork/connect", "sctp")
}

func TestReleaseSourceGraphPinsNativeFeeOwner(t *testing.T) {
	requireReviewedSibling(t, "github.com/urfoundation/sn", "sn", "urfoundation/sn", "")
	requireReviewedSibling(t, "github.com/centrifuge/go-substrate-rpc-client/v4", "sn", "urfoundation/sn", "third_party/go-substrate-rpc-client")
}

func TestReleaseSourceGraphPinsRemainingServiceSiblings(t *testing.T) {
	for _, sibling := range []string{"proxy", "glog", "goidenticons", "userwireguard"} {
		t.Run(sibling, func(t *testing.T) {
			requireReviewedSibling(t, "github.com/urnetwork/"+sibling, sibling, "urnetwork/"+sibling, "")
		})
	}
}

// Server and the SN fixture generator must consume the same reviewed sibling.
// A cached module at that revision would bypass the frozen PERF arm's source.
func TestReleaseSourceGraphPinsSharedGvisorFork(t *testing.T) {
	requireReviewedSibling(t, "gvisor.dev/gvisor", "gvisor", "urnetwork/gvisor", "")
	snDirectory := filepath.Join(serverDirectory(t), "..", "sn")
	module := resolvedSourceModuleIn(t, snDirectory, "gvisor.dev/gvisor")
	if module.Path != "gvisor.dev/gvisor" || module.Replace == nil || module.Replace.Path != "../gvisor" || module.Replace.Version != "" || module.Replace.Dir != filepath.Join(snDirectory, "..", "gvisor") {
		t.Fatalf("SN's fixture generator resolved a different gVisor source: %+v replacement=%+v", module, module.Replace)
	}
}
