// §4.8's pins: server@sidecar's lock and build@sidecar's submodules pin the
// same commits. build's sidecar-branches.zsh repin writes both, so a sibling
// pinned to two commits means one of them was edited by hand or a re-pin
// stopped half way. Tracks never edit lock.yml by hand; the owner re-pins at
// each merge of main (REPO-M0-03).
package sourcegraph

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// The gitlinks at the top of a build branch, by path.
func readBuildGitlinks(ctx context.Context, buildDirectory string, branch string) (map[string]string, error) {
	command := exec.CommandContext(ctx, "git", "ls-tree", "refs/heads/"+branch)
	command.Dir = buildDirectory
	command.Env = append(os.Environ(), "GIT_NO_LAZY_FETCH=1", "GIT_TERMINAL_PROMPT=0")
	output, err := command.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("read build's %s branch at %s: %v\n%s", branch, buildDirectory, err, output)
	}
	gitlinks := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(string(output)), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 4 && fields[0] == "160000" && fields[1] == "commit" {
			gitlinks[fields[3]] = fields[2]
		}
	}
	return gitlinks, nil
}

// Each sibling the lock and build both pin to different commits, and a lock or
// build that pins no sidecar at all, which §4.8 is about.
func pinMismatches(lock sourceLock, gitlinks map[string]string) []string {
	mismatches := []string{}
	locked := []string{}
	for _, sibling := range lock.Siblings {
		locked = append(locked, sibling.Path)
		gitlink, found := gitlinks[sibling.Path]
		if found && gitlink != sibling.Ref {
			mismatches = append(mismatches, fmt.Sprintf("%s: the lock pins %s and build's sidecar branch %s; run build/all/sidecar-branches.zsh repin", sibling.Path, sibling.Ref, gitlink))
		}
	}
	if !slices.Contains(locked, "sidecar") {
		mismatches = append(mismatches, "the lock pins no sidecar")
	}
	if _, found := gitlinks["sidecar"]; !found {
		mismatches = append(mismatches, "build's sidecar branch pins no sidecar")
	}
	return mismatches
}

func TestSidecarPinsEqualBuildSubmodules(t *testing.T) {
	gitlinks, err := readBuildGitlinks(t.Context(), filepath.Join(serverDirectory(t), "..", "build"), "sidecar")
	if err != nil {
		t.Fatal(err)
	}
	for _, mismatch := range pinMismatches(readSourceLock(t), gitlinks) {
		t.Error(mismatch)
	}
}

// The rule reads a real build checkout: equal pins pass, and a gitlink moved on
// build's sidecar branch alone fails, naming the sibling.
func TestSidecarPinsEqualBuildSubmodulesFailsOnAMovedGitlink(t *testing.T) {
	build := t.TempDir()
	git := func(arguments ...string) {
		t.Helper()
		command := exec.CommandContext(t.Context(), "git", append([]string{"-c", "commit.gpgsign=false", "-c", "core.hooksPath=" + os.DevNull}, arguments...)...)
		command.Dir = build
		command.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL="+os.DevNull,
			"GIT_AUTHOR_NAME=Pin Fixture", "GIT_AUTHOR_EMAIL=pin-fixture@example.invalid",
			"GIT_COMMITTER_NAME=Pin Fixture", "GIT_COMMITTER_EMAIL=pin-fixture@example.invalid")
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", arguments, err, output)
		}
	}
	connect := strings.Repeat("1", 40)
	sidecar := strings.Repeat("2", 40)
	git("init", "-q", "-b", "sidecar")
	git("update-index", "--add", "--cacheinfo", "160000,"+connect+",connect")
	git("update-index", "--add", "--cacheinfo", "160000,"+sidecar+",sidecar")
	git("commit", "-q", "-m", "Pin the submodules")

	var lock sourceLock
	lock.Siblings = append(lock.Siblings,
		struct {
			Repository string `yaml:"repository"`
			Path       string `yaml:"path"`
			Ref        string `yaml:"ref"`
		}{Repository: "urnetwork/connect", Path: "connect", Ref: connect},
		struct {
			Repository string `yaml:"repository"`
			Path       string `yaml:"path"`
			Ref        string `yaml:"ref"`
		}{Repository: "urnetwork/sidecar", Path: "sidecar", Ref: sidecar},
	)
	gitlinks, err := readBuildGitlinks(t.Context(), build, "sidecar")
	if err != nil {
		t.Fatal(err)
	}
	if mismatches := pinMismatches(lock, gitlinks); len(mismatches) != 0 {
		t.Fatalf("equal pins fail: %v", mismatches)
	}

	moved := strings.Repeat("3", 40)
	git("update-index", "--cacheinfo", "160000,"+moved+",connect")
	git("commit", "-q", "-m", "Move connect's gitlink alone")
	gitlinks, err = readBuildGitlinks(t.Context(), build, "sidecar")
	if err != nil {
		t.Fatal(err)
	}
	mismatches := pinMismatches(lock, gitlinks)
	if len(mismatches) != 1 || !strings.HasPrefix(mismatches[0], "connect: the lock pins "+connect+" and build's sidecar branch "+moved) {
		t.Fatalf("a moved gitlink reads as %v", mismatches)
	}
}
