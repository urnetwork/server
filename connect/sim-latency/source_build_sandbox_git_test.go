// Native Git tests use its deterministic ownership guard on real read-only
// fixtures, not a claim of different OS UIDs or a privileged container run.
package main

import (
	"bytes"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// Decode the actual Docker argv, never a separately maintained test policy.
func sourceSandboxGitArgumentEnvironment(t *testing.T, phase string) (map[string]string, string) {
	t.Helper()
	input := sourceSandboxFixture()
	input.Phase = phase
	input.MetadataDirectory = filepath.Join(filepath.Dir(input.SourceDirectory), phase, "metadata")
	input.OutputDirectory = filepath.Join(filepath.Dir(input.SourceDirectory), phase, "output")
	args, err := sourceBuildSandboxArguments(input, sourceSandboxTestSettings())
	if err != nil {
		t.Fatal(err)
	}
	environment := map[string]string{}
	for _, argument := range args {
		if entry, found := strings.CutPrefix(argument, "--env="); found {
			name, value, found := strings.Cut(entry, "=")
			if !found {
				t.Fatal("guest environment lacks an explicit value", argument)
			}
			if _, duplicate := environment[name]; duplicate {
				t.Fatal("guest environment repeats a value", name)
			}
			environment[name] = value
		}
	}
	return environment, args[len(args)-1]
}

// Each Git exec, including one spawned by Go, gets the same protected command scope.
func TestSourceBuildSandboxGitPolicyScopesEveryPhaseAndVcsChild(t *testing.T) {
	for _, phase := range []string{"metadata", "check", "compile", "correctness"} {
		environment, script := sourceSandboxGitArgumentEnvironment(t, phase)
		for name, expected := range map[string]string{
			"GIT_CONFIG_NOSYSTEM": "1", "GIT_CONFIG_SYSTEM": "/dev/null", "GIT_CONFIG_GLOBAL": "/dev/null",
			"GIT_CONFIG_PARAMETERS": "", "GIT_OPTIONAL_LOCKS": "0", "GIT_TERMINAL_PROMPT": "0", "GIT_NO_LAZY_FETCH": "1",
			"GIT_ATTR_NOSYSTEM": "1", "GIT_ASKPASS": "/bin/false", "SSH_ASKPASS": "/bin/false", "GIT_PAGER": "/bin/cat",
		} {
			if actual, found := environment[name]; !found || actual != expected {
				t.Errorf("%s lost %s: %q", phase, name, actual)
			}
		}
		count, err := strconv.Atoi(environment["GIT_CONFIG_COUNT"])
		if err != nil || count < 2 {
			t.Fatal("guest command config is absent", phase, err)
		}
		safe := []string{}
		config := map[string]string{}
		for index := 0; index < count; index++ {
			key, keyFound := environment["GIT_CONFIG_KEY_"+strconv.Itoa(index)]
			value, valueFound := environment["GIT_CONFIG_VALUE_"+strconv.Itoa(index)]
			if !keyFound || !valueFound {
				t.Fatal("guest command config is incomplete", phase, index)
			}
			if key == "safe.directory" {
				safe = append(safe, value)
			}
			config[key] = value
		}
		if !slices.Equal(safe, []string{"", "/workspace/server"}) {
			t.Fatal("guest trust was inherited, widened or not reset", phase, safe)
		}
		for key, expected := range map[string]string{"core.hooksPath": "/dev/null", "core.fsmonitor": "false",
			"protocol.allow": "never", "credential.helper": "", "credential.interactive": "false", "maintenance.auto": "false"} {
			if value, found := config[key]; !found || value != expected {
				t.Fatal("guest execution guard missing", phase, key)
			}
		}
		if strings.Contains(script, "buildvcs=false") || environment["GOFLAGS"] != "-mod=readonly" {
			t.Fatal("Git ownership was bypassed instead of retaining production VCS stamping", phase)
		}
		if phase == "compile" && (!strings.Contains(script, "go build -trimpath") || !strings.Contains(script, "vcs.modified=")) {
			t.Fatal("production binary lost clean revision verification")
		}
	}
}

// Stable environment serialization also makes subprocess diagnostics reproducible.
func sourceSandboxGitTestEnvironment(values map[string]string) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	environment := make([]string, 0, len(keys))
	for _, key := range keys {
		environment = append(environment, key+"="+values[key])
	}
	return environment
}

// A native foreign-owner refusal must become exact-root success, not sibling trust.
func TestSourceBuildSandboxNativeGitTrustIsExactInheritedAndReadOnly(t *testing.T) {
	root := t.TempDir()
	commit := sourceTestRepository(t, root, "server")
	sourceTestRepository(t, root, "sibling")
	repository, sibling := filepath.Join(root, "server"), filepath.Join(root, "sibling")
	marker, program := filepath.Join(t.TempDir(), "executed"), filepath.Join(root, "fsmonitor")
	if err := os.WriteFile(program, []byte("#!/bin/sh\n: > \"$SOURCE_BUILD_TEST_GIT_EXECUTED\"\nexit 1\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	sourceTestGit(t, repository, "config", "core.fsmonitor", program)
	sourceTestGit(t, repository, "config", "core.hooksPath", root)
	global := filepath.Join(root, "global-config")
	if err := os.WriteFile(global, []byte("[safe]\n\tdirectory = *\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	configBefore, err := os.ReadFile(filepath.Join(repository, ".git", "config"))
	if err != nil {
		t.Fatal(err)
	}
	indexBefore, err := os.ReadFile(filepath.Join(repository, ".git", "index"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if entry.IsDir() {
				return os.Chmod(path, 0o700)
			}
			return nil
		}); err != nil {
			t.Error(err)
		}
	})
	if err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		mode := os.FileMode(0o444)
		if entry.IsDir() || info.Mode().Perm()&0o111 != 0 {
			mode = 0o555
		}
		return os.Chmod(path, mode)
	}); err != nil {
		t.Fatal(err)
	}
	gitPath, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	run := func(environment map[string]string, executable string, arguments ...string) ([]byte, error) {
		command := exec.CommandContext(t.Context(), executable, arguments...)
		command.Env = sourceSandboxGitTestEnvironment(environment)
		command.WaitDelay = defaultSourceGitExecutionSettings().WaitDelay
		return command.CombinedOutput()
	}
	// The native test guard forces Git's ownership path without chmod/chown of foreign data.
	untrusted := map[string]string{"PATH": os.Getenv("PATH"), "LANG": "C", "LC_ALL": "C", "GIT_TEST_ASSUME_DIFFERENT_OWNER": "1",
		"GIT_CONFIG_NOSYSTEM": "1", "GIT_CONFIG_SYSTEM": "/dev/null", "GIT_CONFIG_GLOBAL": "/dev/null", "GIT_CONFIG_COUNT": "0", "GIT_CONFIG_PARAMETERS": ""}
	if output, err := run(untrusted, gitPath, "-C", repository, "rev-parse", "HEAD"); err == nil || !bytes.Contains(output, []byte("detected dubious ownership")) {
		t.Fatalf("native ownership refusal not reproduced: %v %s", err, output)
	}
	scoped := map[string]string{"PATH": os.Getenv("PATH"), "LANG": "C", "LC_ALL": "C", "GIT_TEST_ASSUME_DIFFERENT_OWNER": "1",
		"GIT_CONFIG_NOSYSTEM": "0", "GIT_CONFIG_SYSTEM": global, "GIT_CONFIG_GLOBAL": global,
		"GIT_CONFIG_PARAMETERS": "'safe.directory=*'", "GIT_CONFIG_COUNT": "1", "GIT_CONFIG_KEY_0": "safe.directory", "GIT_CONFIG_VALUE_0": "*",
		"SOURCE_BUILD_TEST_GIT_EXECUTED": marker}
	guest, _ := sourceSandboxGitArgumentEnvironment(t, "compile")
	for name, value := range guest {
		if strings.HasPrefix(name, "GIT_") || name == "SSH_ASKPASS" {
			scoped[name] = value
		}
	}
	if scoped["GIT_CONFIG_KEY_1"] != "safe.directory" || scoped["GIT_CONFIG_VALUE_1"] != "/workspace/server" {
		t.Fatal("test mapping no longer matches the exact production mount ABI")
	}
	// Only map the immutable guest mount name to this private native fixture.
	scoped["GIT_CONFIG_VALUE_1"] = repository
	if output, err := run(scoped, gitPath, "-C", repository, "rev-parse", "HEAD"); err != nil || strings.TrimSpace(string(output)) != commit {
		t.Fatalf("exact root did not receive native ownership trust: %v %s", err, output)
	}
	if output, err := run(scoped, gitPath, "-C", repository, "status", "--porcelain", "--untracked-files=all"); err != nil || len(output) != 0 {
		t.Fatalf("sealed tree could not be checked without writes/helpers: %v %s", err, output)
	}
	// A fresh exec child models the environment inherited by Go's Git VCS subprocess.
	if output, err := run(scoped, "/bin/sh", "-c", `exec "$1" -C "$2" rev-parse HEAD`, "_", gitPath, repository); err != nil || strings.TrimSpace(string(output)) != commit {
		t.Fatalf("VCS-style child lost the scoped process environment: %v %s", err, output)
	}
	if output, err := run(scoped, gitPath, "-C", sibling, "rev-parse", "HEAD"); err == nil || !bytes.Contains(output, []byte("detected dubious ownership")) {
		t.Fatalf("scoped root admitted an unrelated sibling: %v %s", err, output)
	}
	configAfter, configErr := os.ReadFile(filepath.Join(repository, ".git", "config"))
	indexAfter, indexErr := os.ReadFile(filepath.Join(repository, ".git", "index"))
	if configErr != nil || indexErr != nil || !bytes.Equal(configBefore, configAfter) || !bytes.Equal(indexBefore, indexAfter) {
		t.Fatal("ownership exception persisted configuration or refreshed a sealed index")
	}
	if _, err := os.Lstat(marker); !os.IsNotExist(err) {
		t.Fatal("inherited/local Git execution helper ran", err)
	}
}

// An empty stdout from failed status must not be mistaken for a clean worktree.
func TestSourceBuildSandboxStatusFailureCannotLookClean(t *testing.T) {
	_, script := sourceSandboxGitArgumentEnvironment(t, "check")
	start := strings.Index(script, "source_head=$(git ")
	if start < 0 {
		t.Fatal("guest preflight lacks assign-and-check Git commands")
	}
	end := strings.Index(script[start:], "cd /workspace/server;")
	if end < 0 {
		t.Fatal("guest preflight boundary is missing")
	}
	root := t.TempDir()
	program := `#!/bin/sh
case "$*" in
    *"rev-parse HEAD") printf '%s\n' "$SOURCE_BUILD_COMMIT" ;;
    *"status --porcelain --untracked-files=all") exit 23 ;;
    *) exit 24 ;;
esac
`
	if err := os.WriteFile(filepath.Join(root, "git"), []byte(program), 0o700); err != nil {
		t.Fatal(err)
	}
	command := exec.CommandContext(t.Context(), "/bin/bash", "-ceu", script[start:start+end]+"printf 'unexpected clean\\n'")
	command.Env = []string{"PATH=" + root + string(os.PathListSeparator) + os.Getenv("PATH"), "SOURCE_BUILD_COMMIT=" + sourceSandboxFixture().Commit}
	output, err := command.CombinedOutput()
	exit, ok := err.(*exec.ExitError)
	if !ok || exit.ExitCode() != 23 || bytes.Contains(output, []byte("unexpected clean")) {
		t.Fatalf("failed status was swallowed as clean: %v %s", err, output)
	}
}
