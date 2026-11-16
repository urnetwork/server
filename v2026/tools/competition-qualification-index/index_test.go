// Real Git fixtures exercise the source-owned materializer predicates without
// reading deployment data, using a database, or materializing production trees.
package competitionqualificationindex

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Each fixture owns its complete Git database, worktree and private index.
type qualificationIndexFixture struct {
	directory string
	index     string
	commit    string
	predicate string
	env       []string
	options   []string
}

// Ambient Git configuration, credentials and repository overrides are absent.
func newQualificationIndexFixture(t *testing.T) *qualificationIndexFixture {
	t.Helper()
	root := t.TempDir()
	directory := filepath.Join(root, "synthetic-source")
	if err := os.Mkdir(directory, 0700); err != nil {
		t.Fatal(err)
	}
	workingDirectory, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	env := []string{}
	for _, entry := range os.Environ() {
		if !strings.HasPrefix(entry, "GIT_") {
			env = append(env, entry)
		}
	}
	env = append(env, "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_TERMINAL_PROMPT=0",
		"GIT_AUTHOR_NAME=Synthetic Fixture", "GIT_AUTHOR_EMAIL=fixture@example.invalid",
		"GIT_COMMITTER_NAME=Synthetic Fixture", "GIT_COMMITTER_EMAIL=fixture@example.invalid")
	fixture := &qualificationIndexFixture{directory: directory, index: filepath.Join(root, "private-verification.index"),
		predicate: filepath.Join(workingDirectory, "predicate.sh"), env: env,
		options: []string{"--no-optional-locks", "-c", "safe.directory=" + directory, "-c", "protocol.allow=never",
			"-c", "core.hooksPath=/dev/null", "-c", "core.fsmonitor=false", "-c", "core.fileMode=true",
			"-c", "core.autocrlf=false", "-c", "core.attributesFile=/dev/null", "-c", "core.sparseCheckout=false",
			"-c", "maintenance.auto=false", "-c", "gc.auto=0"}}
	fixture.git(t, "", "init", "--initial-branch=synthetic")
	for name, content := range map[string]string{
		"alpha.txt": "original alpha\n", "beta.txt": "original beta\n",
		"approved-baseline/payload.dat":       "synthetic retained baseline\n",
		"approved-baseline-neighbor/keep.dat": "synthetic neighboring source\n",
		"approved-baseline-marker.txt":        "synthetic prefix sibling\n",
	} {
		path := filepath.Join(directory, name)
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
	}
	fixture.git(t, "", "add", "--", ".")
	fixture.git(t, "", "commit", "--no-gpg-sign", "-m", "synthetic source fixture")
	fixture.commit = strings.TrimSpace(string(fixture.git(t, "", "rev-parse", "HEAD")))
	fixture.git(t, fixture.index, "read-tree", fixture.commit)
	return fixture
}

// Each native child has its own finite context and exec-owned drain join.
func (self *qualificationIndexFixture) command(t *testing.T, index, name string, arguments ...string) ([]byte, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, name, arguments...)
	command.Env = append([]string(nil), self.env...)
	if index != "" {
		command.Env = append(command.Env, "GIT_INDEX_FILE="+index)
	}
	command.WaitDelay = time.Second
	return command.CombinedOutput()
}

// Real Git is never substituted by a shell imitation of stat-cache behavior.
func (self *qualificationIndexFixture) gitResult(t *testing.T, index string, arguments ...string) ([]byte, error) {
	t.Helper()
	actual := append([]string(nil), self.options...)
	actual = append(actual, "-C", self.directory)
	actual = append(actual, arguments...)
	return self.command(t, index, "git", actual...)
}

// Setup failures are distinct from expected native predicate rejections.
func (self *qualificationIndexFixture) git(t *testing.T, index string, arguments ...string) []byte {
	t.Helper()
	output, err := self.gitResult(t, index, arguments...)
	if err != nil {
		t.Fatalf("synthetic Git setup failed: %v: %s", err, output)
	}
	return output
}

// Only the actual sourced functions are under test. This wrapper substitutes
// the actor's finite execution/log owner, not either predicate's commands.
func (self *qualificationIndexFixture) predicateResult(t *testing.T, function, excluded, outputPath string) ([]byte, error) {
	t.Helper()
	program := `set -euo pipefail
source "$1"
shift
run_owned() {
  shift 2
  if [[ -n ${command_output:-} ]]; then "$@" > "$command_output"; else "$@"; fi
}
io_seconds=15
metadata_seconds=15
function=$1; index=$2; destination=$3; excluded=$4; output=$5
shift 5
case "$function" in
  qualification_authenticate_present) qualification_authenticate_present synthetic "$index" "$destination" "$@" ;;
  qualification_select_missing) qualification_select_missing synthetic "$index" "$destination" "$excluded" "$output" "$@" ;;
  *) exit 64 ;;
esac
`
	arguments := []string{"-c", program, "synthetic-predicate", self.predicate, function, self.index, self.directory, excluded, outputPath}
	arguments = append(arguments, self.options...)
	return self.command(t, "", "/bin/bash", arguments...)
}

// A positive result requires the new source-owned predicate, not a quiet skip.
func (self *qualificationIndexFixture) authenticate(t *testing.T) {
	t.Helper()
	output, err := self.predicateResult(t, "qualification_authenticate_present", "", "")
	if err != nil {
		t.Fatalf("unchanged synthetic source rejected: %v: %s", err, output)
	}
}

// Native exit1 is a rejection, unlike a killed command or setup error.
func requireQualificationExitOne(t *testing.T, err error) {
	t.Helper()
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 1 {
		t.Fatalf("expected native rejection exit1, got %v", err)
	}
}

// The old predicate is deterministically red on identical bytes because
// read-tree has no stat cache; refresh must authenticate, not suppress, it.
func TestQualificationIndexFreshStatCacheRequiresContentRefresh(t *testing.T) {
	fixture := newQualificationIndexFixture(t)
	expected := strings.TrimSpace(string(fixture.git(t, "", "rev-parse", fixture.commit+":alpha.txt")))
	actual := strings.TrimSpace(string(fixture.git(t, "", "hash-object", "--no-filters", "--", "alpha.txt")))
	if expected != actual {
		t.Fatal("synthetic fixture did not start with identical native blob bytes")
	}
	debug := fixture.git(t, fixture.index, "ls-files", "--debug", "--", "alpha.txt")
	if !bytes.Contains(debug, []byte("ctime: 0:0")) || !bytes.Contains(debug, []byte("size: 0")) {
		t.Fatal("fixture did not preserve the actual fresh-index root cause")
	}
	_, oldErr := fixture.gitResult(t, fixture.index, "diff-files", "--no-ext-diff", "--quiet", "--diff-filter=ACMRTUXB")
	requireQualificationExitOne(t, oldErr)
	fixture.authenticate(t)
	fixture.git(t, fixture.index, "diff-files", "--no-ext-diff", "--exit-code")
	if after := strings.TrimSpace(string(fixture.git(t, "", "hash-object", "--no-filters", "--", "alpha.txt"))); after != expected {
		t.Fatal("private stat refresh changed source bytes")
	}
	fixture.git(t, fixture.index, "diff-index", "--cached", "--exit-code", fixture.commit)
}

// Same-length tampering must not be blessed as an innocuous stat difference.
func TestQualificationIndexRejectsPresentContentChange(t *testing.T) {
	fixture := newQualificationIndexFixture(t)
	path := filepath.Join(fixture.directory, "alpha.txt")
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	changed := append([]byte(nil), before...)
	changed[0] ^= 1
	if err := os.WriteFile(path, changed, 0600); err != nil {
		t.Fatal(err)
	}
	_, err = fixture.predicateResult(t, "qualification_authenticate_present", "", "")
	requireQualificationExitOne(t, err)
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(after, changed) {
		t.Fatal("rejection overwrote or lost the changed fixture evidence")
	}
	fixture.git(t, fixture.index, "diff-index", "--cached", "--exit-code", fixture.commit)
}

// Byte identity never authorizes a different executable mode.
func TestQualificationIndexRejectsPresentModeChange(t *testing.T) {
	fixture := newQualificationIndexFixture(t)
	if err := os.Chmod(filepath.Join(fixture.directory, "alpha.txt"), 0700); err != nil {
		t.Fatal(err)
	}
	_, err := fixture.predicateResult(t, "qualification_authenticate_present", "", "")
	requireQualificationExitOne(t, err)
	info, err := os.Stat(filepath.Join(fixture.directory, "alpha.txt"))
	if err != nil || info.Mode().Perm() != 0700 {
		t.Fatal("mode rejection altered the preserved fixture file")
	}
	fixture.git(t, fixture.index, "diff-index", "--cached", "--exit-code", fixture.commit)
}

// Ignore-missing admits a partial checkout only; the complete-tree proof still
// rejects until the exact missing pinned path is populated without force.
func TestQualificationIndexMissingOnlyRequiresSubsequentPopulation(t *testing.T) {
	fixture := newQualificationIndexFixture(t)
	if err := os.Remove(filepath.Join(fixture.directory, "beta.txt")); err != nil {
		t.Fatal(err)
	}
	fixture.authenticate(t)
	missingPath := filepath.Join(t.TempDir(), "missing.z")
	output, err := fixture.predicateResult(t, "qualification_select_missing", "", missingPath)
	if err != nil {
		t.Fatalf("missing-only selector failed: %v: %s", err, output)
	}
	missing, err := os.ReadFile(missingPath)
	if err != nil || !bytes.Equal(missing, []byte("beta.txt\x00")) {
		t.Fatal("selector did not preserve exact missing-only identity")
	}
	_, err = fixture.gitResult(t, fixture.index, "diff-files", "--no-ext-diff", "--exit-code")
	requireQualificationExitOne(t, err)
	fixture.git(t, fixture.index, "checkout-index", "-u", "--", "beta.txt")
	fixture.git(t, fixture.index, "diff-files", "--no-ext-diff", "--exit-code")
}

// Exact skip-worktree exclusions do not swallow similarly named siblings.
func TestQualificationIndexExclusionKeepsMissingPrefixSiblings(t *testing.T) {
	fixture := newQualificationIndexFixture(t)
	fixture.git(t, fixture.index, "update-index", "--skip-worktree", "--", "approved-baseline/payload.dat")
	for _, relative := range []string{"approved-baseline/payload.dat", "approved-baseline-neighbor/keep.dat", "approved-baseline-marker.txt"} {
		if err := os.Remove(filepath.Join(fixture.directory, relative)); err != nil {
			t.Fatal(err)
		}
	}
	fixture.authenticate(t)
	missingPath := filepath.Join(t.TempDir(), "missing.z")
	output, err := fixture.predicateResult(t, "qualification_select_missing", "approved-baseline/", missingPath)
	if err != nil {
		t.Fatalf("closed exclusion selector failed: %v: %s", err, output)
	}
	missing, err := os.ReadFile(missingPath)
	expected := []byte("approved-baseline-marker.txt\x00approved-baseline-neighbor/keep.dat\x00")
	if err != nil || !bytes.Equal(missing, expected) {
		t.Fatal("directory exclusion leaked into its prefix siblings or retained excluded work")
	}
	_, err = fixture.gitResult(t, fixture.index, "diff-files", "--no-ext-diff", "--exit-code")
	requireQualificationExitOne(t, err)
}

// An excluded baseline cannot hide a modified, present prefix-sibling file.
func TestQualificationIndexExclusionRejectsPresentPrefixSiblingChange(t *testing.T) {
	fixture := newQualificationIndexFixture(t)
	fixture.git(t, fixture.index, "update-index", "--skip-worktree", "--", "approved-baseline/payload.dat")
	if err := os.Remove(filepath.Join(fixture.directory, "approved-baseline/payload.dat")); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(fixture.directory, "approved-baseline-neighbor/keep.dat")
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	content[0] ^= 1
	if err := os.WriteFile(path, content, 0600); err != nil {
		t.Fatal(err)
	}
	_, err = fixture.predicateResult(t, "qualification_authenticate_present", "", "")
	requireQualificationExitOne(t, err)
}

// Really-refresh must not inherit an assume-unchanged bypass from its owner.
func TestQualificationIndexAssumeUnchangedCannotHidePresentChange(t *testing.T) {
	fixture := newQualificationIndexFixture(t)
	fixture.git(t, fixture.index, "update-index", "--assume-unchanged", "--", "alpha.txt")
	if err := os.WriteFile(filepath.Join(fixture.directory, "alpha.txt"), []byte("tampered alpha\n"), 0600); err != nil {
		t.Fatal(err)
	}
	_, err := fixture.predicateResult(t, "qualification_authenticate_present", "", "")
	requireQualificationExitOne(t, err)
	fixture.git(t, fixture.index, "diff-index", "--cached", "--exit-code", fixture.commit)
}
