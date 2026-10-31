// A complete approved tree may live in a partial checkout without old commit
// objects. Only that tree, never unrelated source history, is needed to build.
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

func TestSourceBuildFetchDoesNotRequireMissingParentHistory(t *testing.T) {
	root := t.TempDir()
	parent := sourceTestRepository(t, root, "server")
	source := filepath.Join(root, "server")
	if err := os.WriteFile(filepath.Join(source, "source.txt"), []byte("approved synthetic tree\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	sourceTestGit(t, source, "add", "source.txt")
	sourceTestGit(t, source, "commit", "--quiet", "--no-gpg-sign", "-m", "synthetic approved tree")
	approved := sourceTestGit(t, source, "rev-parse", "HEAD")
	if err := os.Remove(filepath.Join(source, ".git", "objects", parent[:2], parent[2:])); err != nil {
		t.Fatal(err)
	}
	// Force the pre-fix failure without a network, promisor remote or sleeps.
	legacy := exec.Command("git", "-c", "core.hooksPath=/dev/null", "clone", "--quiet", "--no-checkout", "--no-local", "--no-hardlinks", source, filepath.Join(root, "legacy"))
	if output, err := legacy.CombinedOutput(); err == nil {
		t.Fatalf("fixture did not reproduce full-clone missing-history failure: %s", output)
	}
	destination := filepath.Join(root, "approved")
	if err := cloneSourceBuildRepository(context.Background(), "server", source, destination, approved); err != nil {
		t.Fatal("approved complete tree was rejected for unrelated history", err)
	}
	if got := sourceTestGit(t, destination, "rev-parse", "HEAD"); got != approved {
		t.Fatal("shallow fetch changed approved identity", got)
	}
	for _, relative := range []string{".git/FETCH_HEAD", ".git/objects/info/alternates"} {
		if _, err := os.Lstat(filepath.Join(destination, filepath.FromSlash(relative))); !os.IsNotExist(err) {
			t.Fatal("private transport metadata escaped into build context", relative, err)
		}
	}
}

// A successful process exit cannot bless truncated stdout/stderr or retain
// the temporary trust file when the bounded copy fails.
func TestSourceBuildFetchRejectsSuccessfulTransportOutputOverflow(t *testing.T) {
	settings := defaultSourceGitExecutionSettings()
	if settings.MaximumDiagnosticBytes != 4*1024 {
		t.Fatal("source fetch diagnostic default changed", settings.MaximumDiagnosticBytes)
	}
	gitPath, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	originalPath := os.Getenv("PATH")
	for _, channel := range []string{"stdout", "stderr"} {
		t.Setenv("PATH", originalPath)
		root := t.TempDir()
		commit := sourceTestRepository(t, root, "server")
		source := filepath.Join(root, "server")
		tools := t.TempDir()
		payload := filepath.Join(root, "transport-output")
		size := maximumSourceConfigBytes + 1
		if channel == "stderr" {
			size = settings.MaximumDiagnosticBytes + 1
		}
		if err := os.WriteFile(payload, bytes.Repeat([]byte("x"), size), 0o600); err != nil {
			t.Fatal(err)
		}
		const wrapper = `#!/bin/sh
for argument do
    if [ "$argument" = fetch ]; then
        trap '' PIPE
        if [ "$SOURCE_BUILD_FETCH_CHANNEL" = stderr ]; then
            /bin/cat "$SOURCE_BUILD_FETCH_OUTPUT" >&2 || :
        else
            /bin/cat "$SOURCE_BUILD_FETCH_OUTPUT" || :
        fi
        exit 0
    fi
done
exec "$SOURCE_BUILD_FETCH_REAL_GIT" "$@"
`
		if err := os.WriteFile(filepath.Join(tools, "git"), []byte(wrapper), 0o700); err != nil {
			t.Fatal(err)
		}
		t.Setenv("SOURCE_BUILD_FETCH_REAL_GIT", gitPath)
		t.Setenv("SOURCE_BUILD_FETCH_OUTPUT", payload)
		t.Setenv("SOURCE_BUILD_FETCH_CHANNEL", channel)
		t.Setenv("PATH", tools+string(os.PathListSeparator)+originalPath)
		err = cloneSourceBuildRepository(t.Context(), "server", source, filepath.Join(root, "rejected"), commit)
		if err == nil || !strings.Contains(err.Error(), "exact approved source fetch") || !strings.Contains(err.Error(), "exceeds its bound") {
			t.Fatalf("%s output prefix was accepted or failed after fetch admission: %v", channel, err)
		}
		entries, err := os.ReadDir(root)
		if err != nil {
			t.Fatal(err)
		}
		for _, entry := range entries {
			if strings.HasPrefix(entry.Name(), ".source-build-git-") {
				t.Fatal("failed transport retained private trust configuration", entry.Name())
			}
		}
	}
}

// Git's native ownership guard reaches the real upload-pack child; no OS UID
// or container claim is made by this portable deterministic fixture.
func TestSourceBuildFetchScopesForeignOwnedSourceAndUploadPack(t *testing.T) {
	root := t.TempDir()
	// The parent path is data, even when shell/config quoting would be required.
	sourceParent := filepath.Join(root, "source space ' \" $(not-a-command)")
	commit := sourceTestRepository(t, sourceParent, "server")
	sourceTestRepository(t, root, "sibling")
	source := filepath.Join(sourceParent, "server")
	legacy := filepath.Join(root, "worktree-only")
	sourceTestGit(t, root, "init", "--quiet", "--template=", "--", legacy)
	marker, program := filepath.Join(root, "unexpected-helper"), filepath.Join(root, "local-program")
	if err := os.WriteFile(program, []byte("#!/bin/sh\n: > \"$SOURCE_BUILD_FETCH_EXECUTED\"\nexit 1\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	sourceTestGit(t, source, "config", "core.fsmonitor", program)
	sourceTestGit(t, source, "config", "core.hooksPath", root)
	t.Setenv("SOURCE_BUILD_FETCH_EXECUTED", marker)
	configBefore, err := os.ReadFile(filepath.Join(source, ".git", "config"))
	if err != nil {
		t.Fatal(err)
	}
	indexBefore, err := os.ReadFile(filepath.Join(source, ".git", "index"))
	if err != nil {
		t.Fatal(err)
	}
	// The wrapper sets the native guard on both the client and inherited child.
	global := sourceTestForeignGit(t, root)
	// Even exact client-side trust is removed before the uploader starts.
	commandConfigOnly := exec.CommandContext(t.Context(), "git", "-c", "safe.directory=", "-c", "safe.directory="+legacy,
		"-c", "safe.directory="+source, "-c", "safe.directory="+filepath.Join(source, ".git"), "-c", "protocol.file.allow=always", "-C", legacy,
		"fetch", "--quiet", "--no-tags", "--depth=1", "--", source, commit)
	commandConfigOnly.WaitDelay = defaultSourceGitExecutionSettings().WaitDelay
	if output, err := commandConfigOnly.CombinedOutput(); err == nil || !bytes.Contains(output, []byte("detected dubious ownership")) ||
		!bytes.Contains(output, []byte(".git")) {
		t.Fatalf("upload-pack ownership refusal was not reproduced: %v %s", err, output)
	}
	destination := filepath.Join(root, "approved")
	if err := cloneSourceBuildRepository(t.Context(), "server", source, destination, commit); err != nil {
		t.Fatal("exact authenticated source and upload-pack were refused", err)
	}
	if head, err := sourceGitText(t.Context(), destination, "rev-parse", "HEAD"); err != nil || head != commit {
		t.Fatal("local transport changed approved identity", head, err)
	}
	if origin, err := sourceGitText(t.Context(), destination, "remote", "get-url", "origin"); err != nil || origin != sourceBuildPublicOrigin("server") {
		t.Fatal("local transport leaked its private source path", origin, err)
	}
	sourceTestUnscopedGitRefusal(t, source)
	sourceTestUnscopedGitRefusal(t, filepath.Join(root, "sibling"))
	configAfter, err := os.ReadFile(filepath.Join(source, ".git", "config"))
	if err != nil || !bytes.Equal(configBefore, configAfter) {
		t.Fatal("fetch persisted source trust configuration", err)
	}
	indexAfter, err := os.ReadFile(filepath.Join(source, ".git", "index"))
	if err != nil || !bytes.Equal(indexBefore, indexAfter) {
		t.Fatal("fetch changed the source index", err)
	}
	for _, directory := range []string{source, destination} {
		content, err := os.ReadFile(filepath.Join(directory, ".git", "config"))
		if err != nil || strings.Contains(string(content), "[safe]") {
			t.Fatal("transport-only trust escaped into repository configuration", directory, err)
		}
	}
	if _, err := os.Lstat(global); !os.IsNotExist(err) {
		t.Fatal("transport created persistent global trust", err)
	}
	if _, err := os.Lstat(marker); !os.IsNotExist(err) {
		t.Fatal("transport executed an untrusted local Git helper", err)
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".source-build-git-") {
			t.Fatal("transport retained its private scoped configuration", entry.Name())
		}
	}
	for _, relative := range []string{".git/FETCH_HEAD", ".git/objects/info/alternates"} {
		if _, err := os.Lstat(filepath.Join(destination, filepath.FromSlash(relative))); !os.IsNotExist(err) {
			t.Fatal("transport metadata escaped into the private context", relative, err)
		}
	}
}

// Stage constructors supply independent ordinary top-level clones; aliases,
// linked metadata, bare roots and nested paths cannot gain extra trust.
func TestSourceBuildFetchRejectsRedirectedOrNonCheckoutSourceBeforeAllocation(t *testing.T) {
	for _, mutation := range []string{"source alias", "git alias", "linked metadata", "bare metadata", "subdirectory"} {
		root := t.TempDir()
		commit := sourceTestRepository(t, root, "server")
		source := filepath.Join(root, "server")
		var err error
		switch mutation {
		case "source alias":
			alias := filepath.Join(root, "alias")
			err = os.Symlink(source, alias)
			source = alias
		case "git alias", "linked metadata":
			metadata := filepath.Join(source, ".git")
			other := filepath.Join(root, "external-metadata")
			if err = os.Rename(metadata, other); err == nil {
				if mutation == "git alias" {
					err = os.Symlink(other, metadata)
				} else {
					err = os.WriteFile(metadata, []byte("gitdir: "+other+"\n"), 0o600)
				}
			}
		case "bare metadata":
			source = filepath.Join(source, ".git")
		case "subdirectory":
			source = filepath.Join(source, "connect", "sim-latency")
		}
		if err != nil {
			t.Fatal(mutation, err)
		}
		destination := filepath.Join(root, "not-allocated")
		if err := cloneSourceBuildRepository(t.Context(), "server", source, destination, commit); err == nil {
			t.Errorf("%s acquired source-side transport trust", mutation)
		}
		if _, err := os.Lstat(destination); !os.IsNotExist(err) {
			t.Errorf("%s allocated a context before source admission: %v", mutation, err)
		}
	}
}
