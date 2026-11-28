// Native Git ownership fixtures model a service without sudo's interactive
// ownership exception. Every repository and origin remains synthetic and local.
package main

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Forces native ownership refusal only for the named checkout and its children;
// local bare transports and freshly created private clones keep their real owner.
func sourceTestForeignGit(t *testing.T, root string) string {
	t.Helper()
	gitPath, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	toolsRoot := t.TempDir()
	const wrapper = `#!/bin/sh
directory=
previous=
for argument do
    if [ "$previous" = '-C' ]; then directory=$argument; fi
    previous=$argument
done
case "$directory" in
    "$SOURCE_GIT_TEST_FOREIGN_ROOT"|"$SOURCE_GIT_TEST_FOREIGN_ROOT"/*)
        export GIT_TEST_ASSUME_DIFFERENT_OWNER=1 ;;
esac
exec "$SOURCE_GIT_TEST_EXECUTABLE" "$@"
`
	if err := os.WriteFile(filepath.Join(toolsRoot, "git"), []byte(wrapper), 0o700); err != nil {
		t.Fatal(err)
	}
	globalConfig := filepath.Join(toolsRoot, "gitconfig")
	t.Setenv("SOURCE_GIT_TEST_FOREIGN_ROOT", root)
	t.Setenv("SOURCE_GIT_TEST_EXECUTABLE", gitPath)
	t.Setenv("GIT_TEST_ASSUME_DIFFERENT_OWNER", "0")
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_CONFIG_GLOBAL", globalConfig)
	t.Setenv("GIT_CONFIG_COUNT", "0")
	t.Setenv("GIT_CONFIG_PARAMETERS", "")
	t.Setenv("PATH", toolsRoot+string(os.PathListSeparator)+os.Getenv("PATH"))
	for _, name := range []string{"SUDO_UID", "SUDO_GID", "SUDO_USER"} {
		t.Setenv(name, "")
		if err := os.Unsetenv(name); err != nil {
			t.Fatal(err)
		}
	}
	return globalConfig
}

// The negative control must reach real Git, not merely a fixture wrapper error.
func sourceTestUnscopedGitRefusal(t *testing.T, directory string) {
	t.Helper()
	output, err := exec.Command("git", "-c", "safe.directory=", "-C", directory, "rev-parse", "HEAD").CombinedOutput()
	if err == nil || !strings.Contains(string(output), "detected dubious ownership") {
		t.Fatalf("native ownership refusal was not reproduced: %v: %s", err, output)
	}
}

// Config discovery starts in main/, but Git's exception must name its actual
// repository root. The exception must not persist or change authenticated bytes.
func TestSourceGitReadsForeignOwnedRootAndSubdirectory(t *testing.T) {
	root := t.TempDir()
	commit := sourceTestRepository(t, root, "config")
	repository := filepath.Join(root, "config")
	mainRoot := filepath.Join(repository, "main")
	if err := os.Mkdir(mainRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	sourceTestGit(t, repository, "remote", "add", "origin", "ssh://source@code.example/config")
	configBefore, err := os.ReadFile(filepath.Join(repository, ".git", "config"))
	if err != nil {
		t.Fatal(err)
	}
	globalConfig := sourceTestForeignGit(t, repository)
	sourceTestUnscopedGitRefusal(t, repository)
	sourceTestUnscopedGitRefusal(t, mainRoot)
	if head, err := sourceGitText(t.Context(), repository, "rev-parse", "HEAD"); err != nil || head != commit {
		t.Fatalf("foreign-owned head = %q: %v", head, err)
	}
	if top, err := sourceGitText(t.Context(), mainRoot, "rev-parse", "--show-toplevel"); err != nil || top != repository {
		t.Fatalf("foreign-owned config discovery = %q: %v", top, err)
	}
	if origin, err := sourceGitText(t.Context(), repository, "remote", "get-url", "origin"); err != nil || origin != "ssh://source@code.example/config" {
		t.Fatalf("foreign-owned origin = %q: %v", origin, err)
	}
	if content, err := sourceGitBytes(t.Context(), mainRoot, 4096, "show", commit+":source.txt"); err != nil || string(content) != "config\n" {
		t.Fatalf("exact foreign-owned blob = %q: %v", content, err)
	}
	sourceTestUnscopedGitRefusal(t, repository)
	configAfter, err := os.ReadFile(filepath.Join(repository, ".git", "config"))
	if err != nil || !bytes.Equal(configBefore, configAfter) {
		t.Fatal("command-scoped read changed repository configuration", err)
	}
	if _, err := os.Lstat(globalConfig); !os.IsNotExist(err) {
		t.Fatal("command-scoped read created persistent Git configuration", err)
	}
}

// Ownership trust does not authorize hooks, fsmonitor or an inherited alternate
// repository. Trusted transport credentials must remain available for later publication.
func TestSourceGitScopesExecutionWithoutDroppingTransportEnvironment(t *testing.T) {
	root := t.TempDir()
	commit := sourceTestRepository(t, root, "server")
	otherCommit := sourceTestRepository(t, root, "other")
	repository, other := filepath.Join(root, "server"), filepath.Join(root, "other")
	hooksRoot := filepath.Join(root, "hooks")
	if err := os.Mkdir(hooksRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	program := []byte("#!/bin/sh\nprintf 'unexpected execution\\n' > \"$SOURCE_GIT_TEST_EXECUTED\"\nexit 1\n")
	for _, path := range []string{filepath.Join(hooksRoot, "pre-commit"), filepath.Join(root, "fsmonitor")} {
		if err := os.WriteFile(path, program, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	sourceTestGit(t, repository, "config", "core.hooksPath", hooksRoot)
	sourceTestGit(t, repository, "config", "core.fsmonitor", filepath.Join(root, "fsmonitor"))
	if err := os.WriteFile(filepath.Join(repository, "source.txt"), []byte("synthetic staged source\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	sourceTestGit(t, repository, "-c", "core.fsmonitor=false", "add", "source.txt")
	sourceTestForeignGit(t, repository)
	sourceTestUnscopedGitRefusal(t, repository)
	marker := filepath.Join(root, "executed")
	t.Setenv("SOURCE_GIT_TEST_EXECUTED", marker)
	t.Setenv("GIT_DIR", filepath.Join(other, ".git"))
	t.Setenv("GIT_WORK_TREE", other)
	t.Setenv("GIT_INDEX_FILE", filepath.Join(other, ".git", "index"))
	t.Setenv("GIT_OBJECT_DIRECTORY", filepath.Join(other, ".git", "objects"))
	t.Setenv("GIT_SSH_COMMAND", "synthetic-transport-helper")
	command, err := sourceGitCommand(t.Context(), repository, "rev-parse", "HEAD")
	if err != nil || !strings.Contains(strings.Join(command.Env, "\n"), "GIT_SSH_COMMAND=synthetic-transport-helper") {
		t.Fatal("trusted transport environment was lost", err)
	}
	if head, err := sourceGitText(t.Context(), repository, "rev-parse", "HEAD"); err != nil || head != commit || head == otherCommit {
		t.Fatalf("inherited Git redirection changed source: %q %v", head, err)
	}
	if _, err := sourceGitText(t.Context(), repository, "status", "--porcelain=v1"); err != nil {
		t.Fatal(err)
	}
	if _, err := sourceGitText(t.Context(), repository, "-c", "user.name=Synthetic Source", "-c", "user.email=source@account.example", "commit", "--quiet", "--no-gpg-sign", "-m", "synthetic contained staging"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(marker); !os.IsNotExist(err) {
		t.Fatal("source staging executed a hook or fsmonitor program", err)
	}
	if head, err := sourceGitText(t.Context(), other, "rev-parse", "HEAD"); err != nil || head != otherCommit {
		t.Fatal("source staging changed the redirected repository", err)
	}
}

// Trust cannot be widened by a linked path, a gitfile, world-writable metadata,
// or a path which Git would interpret as a wildcard safe.directory entry.
func TestSourceGitRejectsAmbiguousRepositoryPaths(t *testing.T) {
	root := t.TempDir()
	sourceTestRepository(t, root, "repository")
	sourceTestRepository(t, root, "*")
	repository := filepath.Join(root, "repository")
	alias := filepath.Join(root, "alias")
	if err := os.Symlink(repository, alias); err != nil {
		t.Fatal(err)
	}
	linked, gitfile := filepath.Join(root, "linked"), filepath.Join(root, "gitfile")
	for _, path := range []string{linked, gitfile} {
		if err := os.Mkdir(path, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(filepath.Join(repository, ".git"), filepath.Join(linked, ".git")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(gitfile, ".git"), []byte("gitdir: "+filepath.Join(repository, ".git")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{alias, linked, gitfile, filepath.Join(root, "*"), "/", "relative"} {
		if _, err := sourceGitText(t.Context(), path, "rev-parse", "HEAD"); err == nil {
			t.Errorf("ambiguous source Git path accepted: %q", path)
		}
	}
	for _, name := range []string{"HEAD", "config", "objects"} {
		path := filepath.Join(repository, ".git", name)
		retained := path + ".retained"
		if err := os.Rename(path, retained); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(retained, path); err != nil {
			t.Fatal(err)
		}
		if _, err := sourceGitText(t.Context(), repository, "rev-parse", "HEAD"); err == nil {
			t.Errorf("redirected source Git metadata accepted: %s", name)
		}
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(retained, path); err != nil {
			t.Fatal(err)
		}
	}
	commonPath := filepath.Join(repository, ".git", "commondir")
	if err := os.WriteFile(commonPath, []byte("../synthetic-common\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := sourceGitText(t.Context(), repository, "rev-parse", "HEAD"); err == nil {
		t.Fatal("linked common Git metadata accepted")
	}
	if err := os.Remove(commonPath); err != nil {
		t.Fatal(err)
	}
	privateChild := filepath.Join(repository, "private-child")
	if err := os.Mkdir(privateChild, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(repository, 0o777); err != nil {
		t.Fatal(err)
	}
	if _, err := sourceGitText(t.Context(), privateChild, "rev-parse", "HEAD"); err == nil {
		t.Fatal("private subdirectory bypassed a world-writable repository root")
	}
	if err := os.Chmod(repository, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(repository, ".git"), 0o777); err != nil {
		t.Fatal(err)
	}
	if _, err := sourceGitText(t.Context(), repository, "rev-parse", "HEAD"); err == nil {
		t.Fatal("world-writable source Git metadata accepted")
	}
}

// The composed origin preflight precedes allocation for every retry. It can
// read foreign-owned configured checkouts while retaining all earlier evidence.
func TestSourceStagePreflightsBeforeAllocatingAttempt(t *testing.T) {
	root := t.TempDir()
	for _, name := range append(sourceRepositoryNames(), "config") {
		sourceTestRepository(t, root, name)
		sourceTestGit(t, filepath.Join(root, name), "remote", "add", "origin", "ssh://source@code.example/"+name)
	}
	configRoot := filepath.Join(root, "config")
	if err := os.Mkdir(filepath.Join(configRoot, "main"), 0o700); err != nil {
		t.Fatal(err)
	}
	stateRoot := t.TempDir()
	historical := filepath.Join(stateRoot, "historical-incomplete")
	if err := os.Mkdir(historical, 0o700); err != nil {
		t.Fatal(err)
	}
	evidencePath := filepath.Join(historical, "evidence")
	if err := os.WriteFile(evidencePath, []byte("retained evidence\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	sourceTestForeignGit(t, root)
	sourceTestUnscopedGitRefusal(t, filepath.Join(configRoot, "main"))
	stagedRoot, origins, err := newSourceStage(context.Background(), stateRoot, ".source-2-", root, filepath.Join(configRoot, "main", "source.yml"), false)
	if err != nil || stagedRoot == "" || len(origins) != len(sourceRepositoryNames())+1 || origins["config"] != "ssh://source@code.example/config" {
		t.Fatalf("foreign-owned stage preflight: %q %v %v", stagedRoot, origins, err)
	}
	alias := filepath.Join(root, "config-alias")
	if err := os.Symlink(configRoot, alias); err != nil {
		t.Fatal(err)
	}
	for attempt := 0; attempt < 3; attempt++ {
		if path, _, err := newSourceStage(context.Background(), stateRoot, ".source-2-", root, filepath.Join(alias, "main", "source.yml"), false); err == nil || path != "" {
			t.Fatalf("unsafe config origin allocated attempt %d: %q %v", attempt, path, err)
		}
	}
	entries, err := os.ReadDir(stateRoot)
	if err != nil || len(entries) != 2 {
		t.Fatalf("metadata retries leaked new source attempts: %v %v", entries, err)
	}
	if content, err := os.ReadFile(evidencePath); err != nil || string(content) != "retained evidence\n" {
		t.Fatal("source preflight changed historical evidence", err)
	}
}
