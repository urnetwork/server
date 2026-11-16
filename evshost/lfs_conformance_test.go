//go:build !windows

// §22.9's git-lfs row on stock clients (FS-73, X19; EVS2-M10-29): the matrix's rows this host holds (git-lfs 3.x as
// Ubuntu 22.04 and 24.04 ship it, and the latest, each beside its git; sidecartest.PlanGitLfsMatrix), each run as a
// stock client writes a repository with LFS files: the pointer blob stock git-lfs's clean filter commits parses as the
// pointer evs's git-lfs code reads (sidecar.EvsLfsParsePointer) and names the content's SHA-256 and size, byte-exact
// against the pointer format git-lfs documents; the oid the commit's tree names for the file is git's own oid of those
// pointer bytes, which export keeps; and R, the required objects the tip check and the prune read
// (sidecar.EvsLfsRequired), finds every object the commit's pointers name and none of a file outside filter=lfs.
//
// The report names exactly what ran and what was skipped (sidecartest.GitLfsMatrixReport), since a pass on this host's
// versions alone is not §22.9's evidence. The flows through the daemon's transfer agent over http, ssh and
// git-remote-ur (the agent writing each object's path before the ref update, smudge at checkout, the prune and the tip
// check against evs) wait on the daemon's composition of its EvsClient, and are not run here.

package evshost_test

import (
	"context"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/sidecartest"
)

// One row's run: a work tree with an LFS file and a plain file, committed with the row's git and git-lfs, then
// checked as the file header says.
func lfsConformanceRow(t *testing.T, run *sidecartest.GitLfsMatrixRun) {
	ctx := t.Context()
	dir := t.TempDir()
	bin := filepath.Join(dir, "bin")
	work := filepath.Join(dir, "work")
	home := filepath.Join(dir, "home")
	for _, directory := range []string{bin, work, home} {
		if err := os.MkdirAll(directory, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	// the row's git-lfs as the git-lfs its git finds first
	if err := os.Symlink(run.GitLfsPath, filepath.Join(bin, "git-lfs")); err != nil {
		t.Fatal(err)
	}
	environment := append(os.Environ(),
		"PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"),
		"HOME="+home, "GIT_CONFIG_NOSYSTEM=1",
		"GIT_AUTHOR_NAME=T", "GIT_AUTHOR_EMAIL=t@x.example", "GIT_COMMITTER_NAME=T", "GIT_COMMITTER_EMAIL=t@x.example",
	)
	git := func(args ...string) string {
		t.Helper()
		command := exec.CommandContext(ctx, run.GitPath, args...)
		command.Dir = work
		command.Env = environment
		out, err := command.CombinedOutput()
		if err != nil {
			t.Fatalf("%s: git %v: %v\n%s", run.Row.Name, args, err, out)
		}
		return string(out)
	}
	git("init", "-q")
	git("lfs", "install", "--local")
	content := []byte(fmt.Sprintf("an LFS object of %s\n", run.Row.Name))
	files := map[string][]byte{
		".gitattributes": []byte("*.bin filter=lfs diff=lfs merge=lfs -text\n"),
		"object.bin":     content,
		"plain.txt":      []byte("not under filter=lfs\n"),
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(work, name), body, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	git("add", ".")
	git("commit", "-q", "-m", "lfs conformance")

	// the pointer blob, byte-exact, parsed as evs's code reads it
	pointer := []byte(git("cat-file", "-p", "HEAD:object.bin"))
	oid := sha256.Sum256(content)
	want := fmt.Sprintf("version https://git-lfs.github.com/spec/v1\noid sha256:%x\nsize %d\n", oid, len(content))
	if string(pointer) != want {
		t.Fatalf("%s: the pointer blob is %q, not git-lfs's documented form %q", run.Row.Name, pointer, want)
	}
	parsed, ok := sidecar.EvsLfsParsePointer(pointer, sidecar.DefaultEvsSettings().LfsPointerMaxByteCount)
	if !ok || parsed.Oid != sidecar.EvsLfsOid(oid) || parsed.Size != int64(len(content)) {
		t.Fatalf("%s: evs reads the pointer as %+v, %v", run.Row.Name, parsed, ok)
	}
	// export's oid: git's own oid of the pointer bytes
	blobHash := sha1.New()
	fmt.Fprintf(blobHash, "blob %d\x00", len(pointer))
	blobHash.Write(pointer)
	if treeOid := strings.TrimSpace(git("rev-parse", "HEAD:object.bin")); treeOid != fmt.Sprintf("%x", blobHash.Sum(nil)) {
		t.Fatalf("%s: the tree names %s for the pointer", run.Row.Name, treeOid)
	}

	// R on a bare clone, by the row's git, as the tip check and the prune read it
	bare := filepath.Join(dir, "bare.git")
	git("clone", "-q", "--bare", work, bare)
	runner, err := sidecar.NewEvsGitRunner(ctx, run.GitPath, home, sidecar.DefaultEvsSettings(), sidecar.RealClock{})
	if err != nil {
		t.Fatalf("%s: %v", run.Row.Name, err)
	}
	tipText := strings.TrimSpace(git("rev-parse", "HEAD"))
	tip := sidecar.EvsGitOid{}
	if _, err := hex.Decode(tip[:], []byte(tipText)); err != nil || len(tipText) != hex.EncodedLen(len(tip)) {
		t.Fatalf("%s: HEAD is %q", run.Row.Name, tipText)
	}
	required, err := sidecar.EvsLfsRequired(context.WithoutCancel(ctx), &sidecar.EvsLfsGitRepo{Git: runner, Dir: bare, Settings: sidecar.DefaultEvsSettings()}, tip)
	if err != nil {
		t.Fatalf("%s: R: %v", run.Row.Name, err)
	}
	if len(required) != 1 || required[0].Oid != sidecar.EvsLfsOid(oid) || required[0].Path != "object.bin" {
		t.Fatalf("%s: R found %+v", run.Row.Name, required)
	}
}

func TestLfsConformanceAcrossTheMatrix(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	runs := sidecartest.PlanGitLfsMatrix(ctx, sidecartest.HostGitLfsLookup())
	for _, line := range sidecartest.GitLfsMatrixReport(runs) {
		t.Log(line)
	}
	runnable := sidecartest.GitLfsMatrixRunnable(runs)
	if len(runnable) == 0 {
		t.Skip("no row of §22.9's git-lfs matrix runs on this host: see the report")
	}
	for _, run := range runnable {
		lfsConformanceRow(t, run)
	}
}
