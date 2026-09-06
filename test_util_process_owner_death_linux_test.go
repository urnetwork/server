//go:build linux

package server

// Actual owner death and descendant exec tests use private pipe barriers and
// pidfds. Only task-created containment is touched; no services or host scope
// paths are discovered, adopted, reaped by age, or guessed from process names.

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"
)

// Compares stable identities of private worker descriptors after unrelated exec;
// runtime reuse of an FD number is harmless and must not produce a false alarm.
func TestTestProcessDescendantExecDoesNotInheritControlDescriptors(t *testing.T) {
	if os.Getenv("URNETWORK_TEST_PROCESS_FIXTURE_CASE") == "check-descriptors" {
		if IsOwnedTestProcess() || os.Getenv(testProcessModeKey) != "" {
			t.Fatal("unrelated descendant inherited execution admission")
		}
		for _, encoded := range strings.Split(os.Getenv("URNETWORK_TEST_PROCESS_FIXTURE_PRIVATE_INODES"), ",") {
			values := strings.Split(encoded, ":")
			if len(values) != 2 {
				t.Fatal("descriptor identity fixture is malformed")
			}
			device, err := strconv.ParseUint(values[0], 10, 64)
			if err != nil {
				t.Fatal(err)
			}
			inode, err := strconv.ParseUint(values[1], 10, 64)
			if err != nil {
				t.Fatal(err)
			}
			directory, err := os.Open("/proc/self/fd")
			if err != nil {
				t.Fatal(err)
			}
			entries, err := directory.ReadDir(1025)
			directory.Close()
			if err != nil && !errors.Is(err, io.EOF) || len(entries) > 1024 {
				t.Fatal("descriptor census is unavailable or exceeds its fixture bound")
			}
			for _, entry := range entries {
				fd, err := strconv.Atoi(entry.Name())
				if err != nil {
					t.Fatal(err)
				}
				var stat unix.Stat_t
				if err := unix.Fstat(fd, &stat); err == nil && uint64(stat.Dev) == device && stat.Ino == inode {
					t.Fatalf("unrelated descendant inherited private control/config descriptor: fd=%d", fd)
				}
			}
		}
		return
	}
	if IsOwnedTestProcess() {
		if err := ClaimTestProcessGeneration(t.Name()); err != nil {
			t.Fatal(err)
		}
		identities := []string{}
		for _, file := range []*os.File{testProcessChild.status, retainedTestProcessConfiguration} {
			var stat unix.Stat_t
			if err := unix.Fstat(int(file.Fd()), &stat); err != nil {
				t.Fatal(err)
			}
			identities = append(identities, fmt.Sprintf("%d:%d", stat.Dev, stat.Ino))
		}
		command := exec.Command("/proc/self/exe", "-test.run=^"+t.Name()+"$", "-test.count=1",
			"-test.timeout="+time.Until(testProcessChild.deadline).String())
		command.Env = []string{
			"URNETWORK_TEST_PROCESS_FIXTURE_CASE=check-descriptors",
			"URNETWORK_TEST_PROCESS_FIXTURE_PRIVATE_INODES=" + strings.Join(identities, ","),
		}
		command.Stdout, command.Stderr = os.Stdout, os.Stderr
		if err := command.Run(); err != nil {
			t.Fatalf("actual descendant descriptor assertions failed: %v", err)
		}
		return
	}
	fixture := newTestProcessExecutionFixture(t)
	fixture.start(t, "descriptor-owner")
	outcome := fixture.finish(t)
	if outcome.err != nil || !outcome.result.Joined || outcome.result.ReapedProcesses != 1 {
		t.Fatalf("owned descriptor root did not join: %+v %v", outcome.result, outcome.err)
	}
}

// A separate outer manager survives immediate owner SIGKILL. The guardian's
// private terminal pipe, real child Waits, pidfds and populated=0 all agree
// before the test removes only its fresh, exact nested cgroup.
func TestTestProcessOwnerDeathJoinsIndependentDescendant(t *testing.T) {
	scenario := os.Getenv("URNETWORK_TEST_PROCESS_FIXTURE_CASE")
	if scenario == "owner-grandchild" {
		fmt.Fprintf(os.Stdout, "OWNER_DESCENDANT %d\n", os.Getpid())
		nanos, err := strconv.ParseInt(os.Getenv("URNETWORK_TEST_PROCESS_FIXTURE_DEADLINE"), 10, 64)
		if err != nil {
			t.Fatal(err)
		}
		<-time.After(max(time.Until(time.Unix(0, nanos)), 0))
		return
	}
	if IsOwnedTestProcess() {
		if err := ClaimTestProcessGeneration(t.Name()); err != nil {
			t.Fatal(err)
		}
		read, write, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		command := exec.Command("/proc/self/exe", "-test.run=^"+t.Name()+"$", "-test.count=1",
			"-test.timeout="+time.Until(testProcessChild.deadline).String())
		command.Env = []string{
			"URNETWORK_TEST_PROCESS_FIXTURE_CASE=owner-grandchild",
			"URNETWORK_TEST_PROCESS_FIXTURE_DEADLINE=" + strconv.FormatInt(testProcessChild.deadline.UnixNano(), 10),
		}
		command.Stdout, command.Stderr = write, os.Stderr
		command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		if err := command.Start(); err != nil {
			t.Fatal(err)
		}
		write.Close()
		scanner := bufio.NewScanner(read)
		ready := false
		for scanner.Scan() {
			if strings.HasPrefix(scanner.Text(), "OWNER_DESCENDANT ") {
				fmt.Fprintf(os.Stdout, "LIVE_TREE %d %d %d\n", os.Getpid(), command.Process.Pid, os.Getppid())
				ready = true
				break
			}
		}
		read.Close()
		if !ready {
			t.Fatal("actual independently grouped descendant never reached readiness")
		}
		// Losing the middle owner's standard streams cannot cause voluntary
		// exit; only guardian containment kill or the original deadline can.
		<-time.After(max(time.Until(testProcessChild.deadline), 0))
		return
	}
	if scenario == "middle-owner" {
		observer, err := adoptTestProcessPipe(4, "outer-manager-terminal")
		if err != nil {
			t.Fatal(err)
		}
		defer observer.Close()
		fixture := newTestProcessExecutionFixture(t)
		fixture.spec.TerminalStatus = observer
		fixture.start(t, "owner-worker")
		tree := fixture.event(t, "LIVE_TREE ")
		fmt.Fprintf(os.Stdout, "MIDDLE_READY %s %d %d %s\n",
			fixture.owner.name, fixture.owner.identity.Dev, fixture.owner.identity.Ino, tree)
		outcome := fixture.finish(t)
		t.Fatalf("middle owner unexpectedly survived until root completion: %+v %v", outcome.result, outcome.err)
	}
	fixture := newTestProcessExecutionFixture(t)
	var previous int32
	if err := unix.Prctl(unix.PR_GET_CHILD_SUBREAPER, uintptr(unsafe.Pointer(&previous)), 0, 0, 0); err != nil {
		t.Fatal(err)
	}
	if err := unix.Prctl(unix.PR_SET_CHILD_SUBREAPER, 1, 0, 0, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := unix.Prctl(unix.PR_SET_CHILD_SUBREAPER, uintptr(previous), 0, 0, 0); err != nil {
			t.Error(err)
		}
	})
	terminalRead, terminalWrite, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer terminalRead.Close()
	defer terminalWrite.Close()
	readyRead, readyWrite, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer readyRead.Close()
	defer readyWrite.Close()
	if err := readyRead.SetReadDeadline(fixture.deadline); err != nil {
		t.Fatal(err)
	}
	if err := terminalRead.SetReadDeadline(fixture.deadline); err != nil {
		t.Fatal(err)
	}
	command := exec.Command("/proc/self/exe", "-test.run=^"+t.Name()+"$", "-test.count=1",
		"-test.timeout="+time.Until(fixture.deadline).String())
	command.Env = []string{
		"URNETWORK_TEST_PROCESS_FIXTURE_CASE=middle-owner",
		"URNETWORK_TEST_PROCESS_FIXTURE_ORIGINAL_DEADLINE=" + strconv.FormatInt(fixture.deadline.UnixNano(), 10),
		"URNETWORK_TEST_PROCESS_DELEGATION_FD=3",
		"WARP_HOME=/proc/self/fd/7", "WARP_VAULT_HOME=/proc/self/fd/7/vault",
		"WARP_CONFIG_HOME=/proc/self/fd/7/config", "WARP_SITE_HOME=/proc/self/fd/7/site",
		"WARP_ENV=local", "WARP_HOST=owned-process-test",
	}
	command.Stdout, command.Stderr = readyWrite, fixture.spec.Stderr
	// The middle owner may create only under this outer test's own fresh
	// empty cgroup, not the runner's full delegation or an existing scope.
	command.ExtraFiles = []*os.File{fixture.owner.directory, terminalWrite, nil, nil, fixture.spec.Configuration.Directory}
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	middleWaited := false
	t.Cleanup(func() {
		if !middleWaited {
			_ = command.Process.Kill()
			_ = command.Wait()
		}
	})
	readyWrite.Close()
	terminalWrite.Close()
	scanner := bufio.NewScanner(readyRead)
	var fields []string
	for scanner.Scan() {
		if strings.HasPrefix(scanner.Text(), "MIDDLE_READY ") {
			fields = strings.Fields(strings.TrimPrefix(scanner.Text(), "MIDDLE_READY "))
			break
		}
	}
	if len(fields) != 6 || !strings.HasPrefix(fields[0], "urnetwork-test-") ||
		!validTestProcessDigest(strings.TrimPrefix(fields[0], "urnetwork-test-")) {
		t.Fatalf("middle owner did not publish a bounded exact tree identity: %v %v", fields, scanner.Err())
	}
	integers := make([]uint64, 5)
	for index := range integers {
		integers[index], err = strconv.ParseUint(fields[index+1], 10, 64)
		if err != nil {
			t.Fatal(err)
		}
	}
	workerPID, descendantPID, guardianPID := int(integers[2]), int(integers[3]), int(integers[4])
	nested, err := openTestProcessAt(fixture.owner.directory, fields[0], unix.O_RDONLY|unix.O_DIRECTORY)
	if err != nil {
		t.Fatal(err)
	}
	defer nested.Close()
	var nestedIdentity unix.Stat_t
	if err := unix.Fstat(int(nested.Fd()), &nestedIdentity); err != nil ||
		uint64(nestedIdentity.Dev) != integers[0] || nestedIdentity.Ino != integers[1] {
		t.Fatal("middle-owner nested containment identity differs")
	}
	pidfds := []int{}
	for _, pid := range []int{workerPID, descendantPID, guardianPID} {
		fd, err := unix.PidfdOpen(pid, 0)
		if err != nil {
			t.Fatal(err)
		}
		pidfds = append(pidfds, fd)
		defer unix.Close(fd)
	}
	if group, err := unix.Getpgid(descendantPID); err != nil || group != descendantPID {
		t.Fatal("owner-death fixture did not contain an independent Setpgid descendant")
	}
	if err := command.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	waitErr := command.Wait()
	middleWaited = true
	if waitErr == nil || command.ProcessState.Success() {
		t.Fatal("middle owner was not actually terminated")
	}
	reader := bufio.NewScanner(terminalRead)
	reader.Buffer(make([]byte, testProcessIPCBytes), testProcessIPCBytes)
	if !reader.Scan() {
		t.Fatalf("surviving guardian did not provide terminal join evidence: %v", reader.Err())
	}
	var terminal testProcessGuardianStatus
	if err := decodeTestProcessJSON(reader.Bytes(), &terminal); err != nil || terminal.Kind != "terminal" ||
		terminal.Root != t.Name() || !validTestProcessDigest(terminal.Token) ||
		terminal.PID != workerPID || terminal.CgroupDevice != integers[0] || terminal.CgroupInode != integers[1] ||
		!terminal.Empty || !terminal.NoChildren || !terminal.LeaderJoined || terminal.Reaped < 2 {
		t.Fatalf("parent death lost complete-tree join authority: %+v %v", terminal, err)
	}
	for _, fd := range pidfds {
		pollers := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLIN}}
		wait := max(time.Until(fixture.deadline), 0)
		if _, err := unix.Poll(pollers, int(wait/time.Millisecond)); err != nil || pollers[0].Revents&unix.POLLIN == 0 {
			t.Fatalf("terminal child tree still has a live pidfd: %+v %v", pollers, err)
		}
	}
	var guardianStatus unix.WaitStatus
	if pid, err := unix.Wait4(guardianPID, &guardianStatus, unix.WNOHANG, nil); err != nil || pid != guardianPID {
		t.Fatalf("surviving outer manager did not reap its adopted guardian: %d %v", pid, err)
	}
	if empty, err := testProcessCgroupEmpty(nested); err != nil || !empty {
		t.Fatalf("terminal cgroup is not empty: %t %v", empty, err)
	}
	var current unix.Stat_t
	if err := unix.Fstatat(int(fixture.owner.directory.Fd()), fields[0], &current, unix.AT_SYMLINK_NOFOLLOW); err != nil ||
		current.Dev != nestedIdentity.Dev || current.Ino != nestedIdentity.Ino {
		t.Fatal("completed nested containment was replaced before removal")
	}
	if err := unix.Unlinkat(int(fixture.owner.directory.Fd()), fields[0], unix.AT_REMOVEDIR); err != nil {
		t.Fatal(err)
	}
}

// Cancellation followed by leader EOF still carries actual joined state and
// the caller's cancellation, not merely the direct child's exit classification.
func TestTestProcessCompletedCallerCancellationIsNotSuccess(t *testing.T) {
	if IsOwnedTestProcess() {
		if err := ClaimTestProcessGeneration(t.Name()); err != nil {
			t.Fatal(err)
		}
		fmt.Fprintln(os.Stdout, "COMPLETE_READY")
		waitTestProcessParentRelease(t)
		return
	}
	fixture := newTestProcessExecutionFixture(t)
	fixture.start(t, "completion-cancel")
	fixture.event(t, "COMPLETE_READY")
	fixture.cancel()
	fixture.input.Close()
	outcome := fixture.finish(t)
	if !errors.Is(outcome.err, context.Canceled) || !outcome.result.Joined {
		t.Fatalf("canceled caller completion was reported as success or unjoined: %+v %v", outcome.result, outcome.err)
	}
}
