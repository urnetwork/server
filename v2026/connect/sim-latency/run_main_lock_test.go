// Exercises the shell's native lock lifetime and inherited descriptor boundary
// with private fixtures and pipe barriers, without a deployed worker or API.
package main

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"
)

// Exposes only the actual tools needed by the script. Darwin deliberately has
// no flock and uses system stat, even when Homebrew tools are on the host path.
func runMainTestCommandPath(t *testing.T) string {
	t.Helper()
	directory := t.TempDir()
	commandNames := []string{
		"awk", "bash", "basename", "cat", "chmod", "curl", "date", "dirname",
		"git", "go", "hostname", "jq", "make", "mkdir", "mktemp", "mv", "rm",
		"sleep", "stat", "sudo", "tr", "uname",
	}
	if runtime.GOOS == "linux" {
		commandNames = append(commandNames, "flock")
	}
	for _, name := range commandNames {
		path, err := exec.LookPath(name)
		if err != nil {
			t.Fatal(err)
		}
		if runtime.GOOS == "darwin" && name == "stat" {
			path = "/usr/bin/stat"
		}
		if err := os.Symlink(path, filepath.Join(directory, name)); err != nil {
			t.Fatal(err)
		}
	}
	return directory
}

// Owns a completed source ledger and a harness whose controlled child reports
// readiness only after the real shell has acquired its state lock.
type runMainLockFixture struct {
	environment    []string
	stateDirectory string
}

// Keeps every external effect synthetic, including accidental worker builds
// if a regression allows a competing command through the shell lock.
func newRunMainLockFixture(t *testing.T) *runMainLockFixture {
	t.Helper()
	root := t.TempDir()
	commandDirectory := runMainTestCommandPath(t)
	stubDirectory := filepath.Join(root, "stubs")
	stateDirectory := filepath.Join(root, "state")
	for _, directory := range []string{stubDirectory, stateDirectory} {
		if err := os.Mkdir(directory, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	for name, content := range map[string]string{
		"operator.token": "synthetic-lock-token\n",
		"source.yml":     "epochs:\n  - epoch: 6\n",
		"stubs/go":       "#!/bin/bash\nexit 91\n",
		"stubs/sudo":     "#!/bin/bash\nexit 91\n",
		"stubs/curl":     "#!/bin/bash\nprintf '%s\\n' '{}'\n",
		"sim-latency": `#!/bin/bash
set -euo pipefail
[[ ${RUN_MAIN_TEST_HOLD:-} == 1 && ${1:-} == epoch-review ]] || exit 91
printf 'inherited descriptor preserved\n' >&9
exec >/dev/null 2>&1
printf 'ready\n' >&4
IFS= read -r result <&3
exit "$result"
`,
	} {
		if err := os.WriteFile(filepath.Join(root, name), []byte(content), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	return &runMainLockFixture{
		environment: append(os.Environ(),
			"PATH="+stubDirectory+string(os.PathListSeparator)+commandDirectory,
			"SIM_LATENCY_API_URL=http://api.example",
			"SIM_LATENCY_OPERATOR_TOKEN_FILE="+filepath.Join(root, "operator.token"),
			"SIM_LATENCY_SOURCE_CONFIG="+filepath.Join(root, "source.yml"),
			"SIM_LATENCY_BINARY="+filepath.Join(root, "sim-latency"),
			"SIM_LATENCY_STATE_DIR="+stateDirectory,
			"SIM_LATENCY_REVIEWER_ID=synthetic-reviewer",
		),
		stateDirectory: stateDirectory,
	}
}

// The deadline is a deadlock backstop; pipe handshakes establish ordering.
func (self *runMainLockFixture) command(t *testing.T, arguments ...string) *exec.Cmd {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	t.Cleanup(cancel)
	command := exec.CommandContext(ctx, "/bin/bash", append([]string{"./run-main.sh"}, arguments...)...)
	command.Env = self.environment
	command.WaitDelay = time.Second
	return command
}

// Retains the release pipe separately from the shell, so a surviving child can
// continue holding the lock after its shell owner is killed.
type runMainLockOwner struct {
	command *exec.Cmd
	release *os.File
	output  bytes.Buffer
}

// Passes explicit control descriptors and an unrelated fd9 into the real
// entrypoint. The helper's ready message proves lock acquisition completed.
func startRunMainLockOwner(t *testing.T, fixture *runMainLockFixture) *runMainLockOwner {
	t.Helper()
	releaseRead, releaseWrite, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = releaseWrite.Close() })
	defer releaseRead.Close()
	readyRead, readyWrite, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer readyRead.Close()
	defer readyWrite.Close()
	inheritedPath := filepath.Join(t.TempDir(), "inherited.lock")
	inheritedFile, err := os.OpenFile(inheritedPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer inheritedFile.Close()
	owner := &runMainLockOwner{
		command: fixture.command(t, "candidate", "--epoch", "1"),
		release: releaseWrite,
	}
	owner.command.Env = append(owner.command.Environ(), "RUN_MAIN_TEST_HOLD=1")
	owner.command.ExtraFiles = []*os.File{releaseRead, readyWrite, nil, nil, nil, nil, inheritedFile}
	owner.command.Stdout = &owner.output
	owner.command.Stderr = &owner.output
	if err := owner.command.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = releaseWrite.Close()
		if owner.command.ProcessState == nil {
			_ = owner.command.Process.Kill()
			_ = owner.command.Wait()
		}
	})
	_ = readyWrite.Close()
	ready, err := bufio.NewReader(readyRead).ReadString('\n')
	if err != nil || ready != "ready\n" {
		_ = owner.command.Wait()
		t.Fatalf("lock owner readiness = %q, %v: %s", ready, err, owner.output.String())
	}
	inheritedContent, err := os.ReadFile(inheritedPath)
	if err != nil || string(inheritedContent) != "inherited descriptor preserved\n" {
		t.Fatalf("run-main replaced inherited fd9: content=%q, err=%v", inheritedContent, err)
	}
	return owner
}

// Every dispatch path must refuse entry while another shell owns the state.
// Successful and failed children both release ownership without inode removal.
func TestRunMainSerializesAllCommandsAndReleasesLock(t *testing.T) {
	fixture := newRunMainLockFixture(t)
	lockPath := filepath.Join(fixture.stateDirectory, "RUN-MAIN.lock")
	lockContent := []byte("retained lock inode\n")
	if err := os.WriteFile(lockPath, lockContent, 0o600); err != nil {
		t.Fatal(err)
	}
	lockInfo, err := os.Stat(lockPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, exitStatus := range []int{0, 23} {
		owner := startRunMainLockOwner(t, fixture)
		for _, arguments := range [][]string{
			{"staging"}, {"staging", "--replace-current"}, {"advance-staging"},
			{"staging-worker"}, {"run"}, {"status", "--epoch", "1"},
			{"candidate", "--epoch", "1"}, {"approve"}, {"reject"},
		} {
			command := fixture.command(t, arguments...)
			output, err := command.CombinedOutput()
			if err == nil || command.ProcessState.ExitCode() != 1 ||
				!strings.Contains(string(output), "another RUN-MAIN process holds "+lockPath) {
				t.Fatalf("competing %v: %v, output=%s", arguments, err, output)
			}
		}
		if _, err := fmt.Fprintln(owner.release, exitStatus); err != nil {
			t.Fatal(err)
		}
		_ = owner.command.Wait()
		if owner.command.ProcessState.ExitCode() != exitStatus {
			t.Fatalf("owner exit = %d, want %d: %s", owner.command.ProcessState.ExitCode(), exitStatus, owner.output.String())
		}
		if output, err := fixture.command(t, "run").CombinedOutput(); err != nil {
			t.Fatalf("reacquire after exit %d: %v, %s", exitStatus, err, output)
		}
	}
	retainedInfo, err := os.Stat(lockPath)
	if err != nil || !os.SameFile(lockInfo, retainedInfo) {
		t.Fatalf("lock inode was replaced: %v", err)
	}
	retainedContent, err := os.ReadFile(lockPath)
	if err != nil || !bytes.Equal(retainedContent, lockContent) {
		t.Fatalf("lock content was truncated: %q, %v", retainedContent, err)
	}
}

// Killing the shell must not admit another command while its child still owns
// work; the inherited open description retains the lock until that child exits.
func TestRunMainRetainsLockUntilChildExits(t *testing.T) {
	fixture := newRunMainLockFixture(t)
	owner := startRunMainLockOwner(t, fixture)
	lockFile, err := os.OpenFile(filepath.Join(fixture.stateDirectory, "RUN-MAIN.lock"), os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer lockFile.Close()
	if err := owner.command.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = owner.command.Wait()
	if err := syscall.Flock(int(lockFile.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); !errors.Is(err, syscall.EWOULDBLOCK) {
		t.Fatalf("child lost its inherited lock after shell exit: %v", err)
	}
	if _, err := fmt.Fprintln(owner.release, 0); err != nil {
		t.Fatal(err)
	}
	acquired := make(chan error, 1)
	go func() {
		acquired <- syscall.Flock(int(lockFile.Fd()), syscall.LOCK_EX)
	}()
	select {
	case err := <-acquired:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("child exit did not release its inherited lock")
	}
}

// Lock paths must keep a single regular inode; a linked target or a FIFO must
// never be opened, truncated, or mistaken for an ownership record.
func TestRunMainRejectsUnsafeLockPaths(t *testing.T) {
	for _, kind := range []string{"symlink", "directory", "fifo"} {
		fixture := newRunMainLockFixture(t)
		lockPath := filepath.Join(fixture.stateDirectory, "RUN-MAIN.lock")
		targetPath := filepath.Join(t.TempDir(), "target")
		targetContent := []byte("preserve synthetic lock target\n")
		if err := os.WriteFile(targetPath, targetContent, 0o600); err != nil {
			t.Fatal(err)
		}
		var err error
		switch kind {
		case "symlink":
			err = os.Symlink(targetPath, lockPath)
		case "directory":
			err = os.Mkdir(lockPath, 0o700)
		case "fifo":
			err = syscall.Mkfifo(lockPath, 0o600)
		}
		if err != nil {
			t.Fatal(err)
		}
		output, err := fixture.command(t, "run").CombinedOutput()
		if err == nil || !strings.Contains(string(output), "state lock path must be a regular file") {
			t.Fatalf("%s lock path: %v, %s", kind, err, output)
		}
		retainedContent, err := os.ReadFile(targetPath)
		if err != nil || !bytes.Equal(retainedContent, targetContent) {
			t.Fatalf("%s lock modified target: %q, %v", kind, retainedContent, err)
		}
	}
}

// Both native stat dialects must reject public modes and unsafe file types for
// API tokens and review evidence before either can authorize an operation.
func TestRunMainRejectsUnsafePrivateFiles(t *testing.T) {
	fixture := newRunMainLockFixture(t)
	for _, kind := range []string{"group-readable", "world-readable", "symlink", "directory", "empty"} {
		path := filepath.Join(t.TempDir(), "private.json")
		if err := os.WriteFile(path, []byte("synthetic-private-content\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		var err error
		switch kind {
		case "group-readable":
			err = os.Chmod(path, 0o640)
		case "world-readable":
			err = os.Chmod(path, 0o604)
		case "symlink":
			linkedPath := filepath.Join(t.TempDir(), "linked.json")
			err = os.Symlink(path, linkedPath)
			path = linkedPath
		case "directory":
			path = t.TempDir()
		case "empty":
			err = os.Truncate(path, 0)
		}
		if err != nil {
			t.Fatal(err)
		}
		tokenCommand := fixture.command(t, "status")
		tokenCommand.Env = append(tokenCommand.Environ(), "SIM_LATENCY_OPERATOR_TOKEN_FILE="+path)
		if output, err := tokenCommand.CombinedOutput(); err == nil ||
			!strings.Contains(string(output), "operator token must be a nonempty regular file") {
			t.Fatalf("%s token: %v, %s", kind, err, output)
		}
		reviewCommand := fixture.command(t, "approve", "--epoch", "1", "--job-id", "synthetic-job",
			"--evidence", path, "--reason", "synthetic review")
		if output, err := reviewCommand.CombinedOutput(); err == nil ||
			!strings.Contains(string(output), "--evidence must be a private nonempty regular JSON file") {
			t.Fatalf("%s review evidence: %v, %s", kind, err, output)
		}
	}
}
