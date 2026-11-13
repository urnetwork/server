// Private real-Git fixtures verify comment-only readiness patches and source
// discovery without launching an evaluator or generating workload identities.
package controller

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Every Git invocation supplies a synthetic local author and disables ambient
// hooks, filters, credentials and background maintenance; no network is used.
func competitionQualificationFixtureGit(t testing.TB, directory string, arguments ...string) []byte {
	t.Helper()
	options := []string{"-c", "user.name=Synthetic qualification fixture", "-c", "user.email=qualification@example.invalid",
		"-c", "core.hooksPath=/dev/null", "-c", "core.fsmonitor=false", "-c", "commit.gpgsign=false",
		"-c", "gc.auto=0", "-c", "maintenance.auto=false", "-c", "protocol.allow=never", "-C", directory}
	command := exec.CommandContext(t.Context(), "/usr/bin/git", append(options, arguments...)...)
	for _, value := range os.Environ() {
		if !strings.HasPrefix(value, "GIT_") {
			command.Env = append(command.Env, value)
		}
	}
	command.Env = append(command.Env, "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1", "GIT_TERMINAL_PROMPT=0", "LANG=C", "LC_ALL=C")
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("synthetic Git %v: %v\n%s", arguments, err, output)
	}
	return output
}

// A valid Go file need not end in LF. Both path modes preserve every original
// byte except adding its terminal LF, blank line and fixed trusted comment.
func TestCompetitionHostQualificationPatchPreservesNonFinalLf(t *testing.T) {
	source := []byte("package synthetic\n\nfunc Example() int { return 1 }")
	for _, mode := range []string{"", competitionMonoRootPathBase} {
		root := t.TempDir()
		if err := os.Chmod(root, 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(filepath.Join(root, "connect"), 0700); err != nil {
			t.Fatal(err)
		}
		fileMode := os.FileMode(0644)
		if mode != "" {
			fileMode = 0755
		}
		path := filepath.Join(root, "connect", "example.go")
		if err := os.WriteFile(path, source, fileMode); err != nil {
			t.Fatal(err)
		}
		competitionQualificationFixtureGit(t, root, "init", "--quiet")
		competitionQualificationFixtureGit(t, root, "add", "--", "connect/example.go")
		competitionQualificationFixtureGit(t, root, "commit", "--quiet", "-m", "synthetic baseline")
		policy, patchPath := validSettings().PatchPolicy, "connect/example.go"
		if mode != "" {
			policy.PathBase = mode
			policy.AllowedPaths = []string{"server/**"}
			policy.ForbiddenPaths = []string{"server/connect/sim-latency/**"}
			patchPath = "server/" + patchPath
		}
		patch, err := competitionQualificationPatch(patchPath, source, policy)
		if err != nil {
			t.Fatalf("mode %q: %v", mode, err)
		}
		if !bytes.Contains(patch.Bytes, []byte("-func Example() int { return 1 }\n\\ No newline at end of file\n+func Example() int { return 1 }\n")) ||
			!bytes.HasSuffix(patch.Bytes, []byte("\n")) || bytes.Contains(patch.Bytes, []byte{'\r'}) {
			t.Fatalf("mode %q lost the canonical old-EOF replacement", mode)
		}
		routed, patchError := SplitCompetitionRepositoryPatch(string(patch.Bytes), policy)
		if patchError != nil || len(routed) != 1 {
			t.Fatalf("mode %q routing: %v", mode, patchError)
		}
		patchFile := filepath.Join(t.TempDir(), "qualification.patch")
		if err := os.WriteFile(patchFile, routed["server"], 0600); err != nil {
			t.Fatal(err)
		}
		competitionQualificationFixtureGit(t, root, "apply", "--check", "--index", "--", patchFile)
		competitionQualificationFixtureGit(t, root, "apply", "--index", "--", patchFile)
		expected := append(append([]byte{}, source...), []byte("\n\n"+competitionHostQualificationComment+"\n")...)
		staged := competitionQualificationFixtureGit(t, root, "show", ":connect/example.go")
		working, err := os.ReadFile(path)
		if err != nil || !bytes.Equal(staged, expected) || !bytes.Equal(working, expected) {
			t.Fatalf("mode %q changed the complete expected Go blob: %v", mode, err)
		}
		competitionQualificationFixtureGit(t, root, "diff", "--cached", "--check")
	}
}

// Earlier ignored fixtures deliberately contain non-Go text; the selector must
// choose the later actual regular source from the committed tree instead.
func TestCompetitionHostQualificationSkipsGoIgnoredDirectories(t *testing.T) {
	root := t.TempDir()
	if err := os.Chmod(root, 0700); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"aaa/.fixtures/example.go", "aaa/_fixtures/example.go", "aaa/testdata/example.go",
		"connect/sim-latency/example.go", "denied/example.go", "zreal/genuine.go"} {
		full := filepath.Join(root, filepath.FromSlash(path))
		if err := os.MkdirAll(filepath.Dir(full), 0700); err != nil {
			t.Fatal(err)
		}
		content := []byte("not Go syntax; retained fixture only\n")
		if path == "zreal/genuine.go" {
			content = []byte("package synthetic\n")
		}
		if err := os.WriteFile(full, content, 0644); err != nil {
			t.Fatal(err)
		}
	}
	competitionQualificationFixtureGit(t, root, "init", "--quiet")
	competitionQualificationFixtureGit(t, root, "add", "--", ".")
	competitionQualificationFixtureGit(t, root, "commit", "--quiet", "-m", "synthetic committed fixtures")
	tree := competitionQualificationFixtureGit(t, root, "ls-tree", "-r", "-z", "--full-tree", "HEAD")
	policy := PatchPolicy{PathBase: competitionMonoRootPathBase, AllowedPaths: []string{"server/**"},
		ForbiddenPaths: []string{"server/connect/sim-latency/**", "server/denied/**"}}
	path, sourcePath, err := competitionQualificationSourcePaths(policy, tree, defaultCompetitionQualificationSourceSettings())
	if err != nil || path != "server/zreal/genuine.go" || sourcePath != "zreal/genuine.go" {
		t.Fatalf("ignored directory source selected: path=%q source=%q error=%v", path, sourcePath, err)
	}
}
