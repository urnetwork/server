// Cleanup and admission must agree on trusted-operator runtime ancestry.
// Synthetic stat observations exercise ownership without root or host changes.
package competitionagent

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

// Directory metadata retains real FileInfo behavior except explicit test facts.
type cleanupSocketInfo struct {
	os.FileInfo
	mode os.FileMode
	uid  uint32
}

func (self cleanupSocketInfo) Mode() os.FileMode { return self.mode }
func (self cleanupSocketInfo) Sys() any          { return &syscall.Stat_t{Uid: self.uid} }

// Every ancestor is explicit; an unknown path fails rather than inheriting trust.
func cleanupSocketAuthorityFixture(t *testing.T) (Config, map[string]cleanupSocketInfo, func(string) (os.FileInfo, error)) {
	t.Helper()
	info, err := os.Stat(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	config := Config{BaseDirectory: "/synthetic/operator/runtime", StateDirectory: "/synthetic/operator/runtime/state", WorkDirectory: "/synthetic/operator/runtime/candidate/jobs", DockerSocketPath: "/synthetic/operator/runtime/control/docker.sock"}
	config.DockerRole.ScratchDirectory = "/synthetic/operator/runtime/candidate/scratch"
	facts := map[string]cleanupSocketInfo{}
	for path := filepath.Dir(config.DockerSocketPath); ; path = filepath.Dir(path) {
		facts[path] = cleanupSocketInfo{FileInfo: info, mode: os.ModeDir | 0751}
		if path == "/" {
			break
		}
	}
	operator := facts["/synthetic/operator"]
	operator.uid = 12345
	facts["/synthetic/operator"] = operator
	return config, facts, func(path string) (os.FileInfo, error) {
		if info, found := facts[path]; found {
			return info, nil
		}
		return nil, os.ErrNotExist
	}
}

// A named operator's non-writable home is explicitly permitted above the base.
// Requiring every ancestor to be root-owned contradicts the admitted topology.
func TestCleanupSocketAcceptsTrustedOperatorAncestor(t *testing.T) {
	config, _, statPath := cleanupSocketAuthorityFixture(t)
	if err := validateCleanupSocketDirectory(config, 12345, statPath); err != nil {
		t.Fatal("cleanup rejected an admitted runtime ownership topology", err)
	}
}

// Root-only layouts remain supported without requiring an operator-owned parent.
func TestCleanupSocketAcceptsRootOwnedAncestors(t *testing.T) {
	config, facts, statPath := cleanupSocketAuthorityFixture(t)
	operator := facts["/synthetic/operator"]
	operator.uid = 0
	facts["/synthetic/operator"] = operator
	if err := validateCleanupSocketDirectory(config, 12345, statPath); err != nil {
		t.Fatal(err)
	}
}

// Only ancestors above the base may use that identity; writable, symlinked or
// foreign ancestors and operator-owned control/base directories still refuse.
func TestCleanupSocketRejectsUntrustedRuntimeAncestry(t *testing.T) {
	for _, change := range []struct {
		path string
		uid  uint32
		mode os.FileMode
	}{
		{path: "/synthetic/operator", uid: 23456, mode: os.ModeDir | 0751},
		{path: "/synthetic/operator", uid: 12345, mode: os.ModeDir | 0771},
		{path: "/synthetic/operator", uid: 12345, mode: os.ModeDir | os.ModeSymlink | 0751},
		{path: "/synthetic/operator/runtime", uid: 12345, mode: os.ModeDir | 0751},
		{path: "/synthetic/operator/runtime/control", uid: 12345, mode: os.ModeDir | 0751},
	} {
		config, facts, statPath := cleanupSocketAuthorityFixture(t)
		info := facts[change.path]
		info.uid, info.mode = change.uid, change.mode
		facts[change.path] = info
		if err := validateCleanupSocketDirectory(config, 12345, statPath); err == nil {
			t.Fatal("untrusted runtime accepted", change.path, change.uid, change.mode)
		}
	}
}

// A valid-looking root leaf cannot redirect cleanup to candidate or other data.
func TestCleanupSocketRejectsAuthorityOutsideControlHierarchy(t *testing.T) {
	for _, socket := range []string{"/other/control/docker.sock", "/synthetic/operator/runtime/state/docker.sock", "/synthetic/operator/runtime/candidate/jobs/docker.sock", "/synthetic/operator/runtime/candidate/scratch/docker.sock", "/synthetic/operator/runtime/control/../docker.sock"} {
		config, _, statPath := cleanupSocketAuthorityFixture(t)
		config.DockerSocketPath = socket
		if err := validateCleanupSocketDirectory(config, 12345, statPath); err == nil {
			t.Fatal("cleanup accepted a foreign or writable endpoint", socket)
		}
	}
}
