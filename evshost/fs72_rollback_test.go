//go:build sidecar_localstack && sidecar_testhooks

// FS-72's M10 cases on T3 (EVS2-M10-30; §22.10's "a migration of a format-1 repo set and a rollback to format-1
// binaries"): the migration and rollback cases sidecartest/fault registers, and the migration killed at each step of its
// push, on T3's Nets, where sidecart3 names this package's block as each world's instances, so every push and read
// goes through server/cli/evs's handler on the private Redis and the local stack's MinIO. In those cases a rolled-back
// binary is this code at EvsSettings.InstanceFormat 1.
//
// What old code itself does takes binaries built from the last format-1 commits, which
// sidecar/testdata/evs/format1_build.txt pins with every sibling server's go.mod replaces. TestFs72Format1BuildOnT3
// builds server's ./cli/evs and ./cli/taskworker from them, each repo's commit put in one directory with git archive
// and built offline from the module cache, so the pins stay buildable. The rollback run on those binaries is not here,
// for two reasons. A format-1 evs block built from them stays not ready by its own design, with no ServiceHost.Quota
// and no EvsAuthState (evshost's header at that commit), so it serves nothing whatever its config, and a run needs a
// harness built from the same trees that hosts that block with T3's seams. And the pinned sidecar predates format 1's
// guard for format-2 data (evsservice/format1_guard.go: no fold, page-out, path-epoch copy or marker collection on a
// format-2 repo), which landed in one commit with format 2's raise, so no commit is a format-1 binary carrying it. The
// owner's answer (QD-854) makes a rollback a configuration: evs's and taskworker's blocks take InstanceFormat from their
// block config, and the run is a real binary from a pinned release commit with InstanceFormat 1 set through it, the pins
// moving to the first commit with that field, which none has yet.
//
// The schedules hold instances at barriers, so this needs sidecar_testhooks; the stack must be up
// (sidecar/local/stack.sh), and these fail rather than skip without it.

package evshost_test

import (
	"bufio"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/urnetwork/sidecar/evsservice"
	"github.com/urnetwork/sidecar/sidecartest/fault"

	"github.com/urnetwork/server/sidecart3"
)

func TestFs72MigrationOnT3(t *testing.T) {
	fsCommitTestEnv().Run(t, func(tb testing.TB) {
		sidecart3.RunRow(tb, "FS-72", "migration", nil)
	})
}

func TestFs72RollbackOnT3(t *testing.T) {
	fsCommitTestEnv().Run(t, func(tb testing.TB) {
		sidecart3.RunRow(tb, "FS-72", "rollback", nil)
	})
}

func TestFs72MigrationKilledAtStepSixOnT3(t *testing.T) {
	runClauseOnT3(t, "FS-72", "migration killed after step 6", fault.Fs72MigrationKilledAt(evsservice.BarrierPushStep6After))
}

func TestFs72MigrationKilledBeforeTheRaiseOnT3(t *testing.T) {
	runClauseOnT3(t, "FS-72", "migration killed before the raise", fault.Fs72MigrationKilledAt(evsservice.BarrierMigrateRaiseBefore))
}

func TestFs72MigrationKilledAfterTheRaiseOnT3(t *testing.T) {
	runClauseOnT3(t, "FS-72", "migration killed after the raise", fault.Fs72MigrationKilledAt(evsservice.BarrierMigrateRaiseAfter))
}

func TestFs72MigrationKilledBeforeTheHeadCasOnT3(t *testing.T) {
	runClauseOnT3(t, "FS-72", "migration killed before the head CAS", fault.Fs72MigrationKilledAt(evsservice.BarrierHeadCasBefore))
}

func TestFs72MigrationKilledAfterTheHeadCasOnT3(t *testing.T) {
	runClauseOnT3(t, "FS-72", "migration killed after the head CAS", fault.Fs72MigrationKilledAt(evsservice.BarrierHeadCasAfter))
}

// format1_build.txt's path from this package, in the sibling sidecar checkout.
const fs72Format1BuildPath = "../../sidecar/testdata/evs/format1_build.txt"

// One repo's pin in format1_build.txt.
type fs72Format1Pin struct {
	repo   string
	commit string
}

// format1_build.txt's pins, in its order: each line not a comment names a repo and a 40-hex commit.
func fs72Format1Pins(t *testing.T) []*fs72Format1Pin {
	t.Helper()
	file, err := os.Open(fs72Format1BuildPath)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	pins := []*fs72Format1Pin{}
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 || len(fields[1]) != 40 {
			t.Fatalf("format1_build.txt's line %q names no repo and commit", line)
		}
		pins = append(pins, &fs72Format1Pin{repo: fields[0], commit: fields[1]})
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	return pins
}

// Each pin's commit put in dir/<repo> by git archive from the sibling checkout, server's connect/ tree left out,
// since nothing the two binaries link is in it.
func fs72ArchivePins(t *testing.T, pins []*fs72Format1Pin, dir string) {
	t.Helper()
	for _, pin := range pins {
		into := filepath.Join(dir, pin.repo)
		if err := os.MkdirAll(into, 0o755); err != nil {
			t.Fatal(err)
		}
		args := []string{"-C", filepath.Join("..", "..", pin.repo), "archive", "--format=tar", pin.commit}
		if pin.repo == "server" {
			args = append(args, "--", ".", ":(exclude)connect")
		}
		archive := exec.CommandContext(t.Context(), "git", args...)
		extract := exec.CommandContext(t.Context(), "tar", "-x", "-C", into)
		pipe, err := archive.StdoutPipe()
		if err != nil {
			t.Fatal(err)
		}
		extract.Stdin = pipe
		var archiveErr strings.Builder
		archive.Stderr = &archiveErr
		if err := archive.Start(); err != nil {
			t.Fatal(err)
		}
		extractOut, extractErr := extract.CombinedOutput()
		if err := archive.Wait(); err != nil {
			t.Fatalf("git archive of %s at %s: %v\n%s", pin.repo, pin.commit, err, archiveErr.String())
		}
		if extractErr != nil {
			t.Fatalf("extracting %s at %s: %v\n%s", pin.repo, pin.commit, extractErr, extractOut)
		}
	}
}

// The pins build server's evs and taskworker, the binaries FS-72's rollback runs, offline from the module cache: every
// repo server's go.mod replaces is pinned, on the go line they shared before QD-674, and their modules are in go.sum.
func TestFs72Format1BuildOnT3(t *testing.T) {
	pins := fs72Format1Pins(t)
	pinned := map[string]bool{}
	for _, pin := range pins {
		pinned[pin.repo] = true
	}
	for _, repo := range []string{"sidecar", "server"} {
		if !pinned[repo] {
			t.Fatalf("format1_build.txt pins no %s commit", repo)
		}
	}
	dir := t.TempDir()
	fs72ArchivePins(t, pins, dir)
	// every sibling server's go.mod replaces is pinned, or the build would read a newer tree
	goMod, err := os.ReadFile(filepath.Join(dir, "server", "go.mod"))
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(string(goMod), "\n") {
		_, target, ok := strings.Cut(line, "=> ../")
		if !ok || !strings.HasPrefix(strings.TrimSpace(line), "replace ") {
			continue
		}
		repo, _, _ := strings.Cut(strings.TrimSpace(target), "/")
		if !pinned[repo] {
			t.Errorf("server's go.mod at its pin replaces ../%s, which format1_build.txt does not pin", repo)
		}
	}
	if t.Failed() {
		return
	}
	bin := filepath.Join(dir, "bin")
	build := exec.CommandContext(t.Context(), "go", "build", "-o", bin+string(filepath.Separator), "./cli/evs", "./cli/taskworker")
	build.Dir = filepath.Join(dir, "server")
	build.Env = append(os.Environ(), "GOWORK=off", "GOTOOLCHAIN=local", "GOFLAGS=-mod=readonly -trimpath", "GOPROXY=off", "CGO_ENABLED=0")
	output, err := build.CombinedOutput()
	if err != nil {
		t.Fatalf("building the format-1 binaries: %v\n%s", err, output)
	}
	for _, binary := range []string{"evs", "taskworker"} {
		info, err := os.Stat(filepath.Join(bin, binary))
		if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o100 == 0 {
			t.Errorf("no %s binary: %v", binary, err)
		}
	}
}
