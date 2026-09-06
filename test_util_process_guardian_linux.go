//go:build linux

package server

// An independent subreaper owns the real worker and every orphaned descendant.
// Owner IPC loss, leader death and the original execution deadline all kill the
// whole cgroup; terminal success requires empty containment and no children.

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// Emits at most two records, each smaller than PIPE_BUF. The caller routes a
// private pipe unavailable to the worker or any of its unrelated descendants.
func writeTestProcessGuardianStatus(file *os.File, status testProcessGuardianStatus) error {
	value, err := json.Marshal(status)
	if err != nil || len(value) >= testProcessIPCBytes {
		return errors.New("owned test guardian status exceeds its bound")
	}
	value = append(value, '\n')
	if count, err := file.Write(value); err != nil {
		return err
	} else if count != len(value) {
		return io.ErrShortWrite
	}
	return nil
}

// Uses actual kernel child status and population, not leader Wait alone.
// Any uncertain terminal state preserves the cgroup and forbids reuse.
func runTestProcessGuardian(capsule testProcessCapsule, executable, cgroup *os.File) int {
	owner := os.NewFile(6, "owned-test-owner-liveness")
	configuration := os.NewFile(7, "owned-test-configuration")
	status, err := adoptTestProcessPipe(8, "owned-test-guardian-status")
	if err != nil {
		return 125
	}
	for _, fd := range []int{6, 7, 8} {
		unix.CloseOnExec(fd)
	}
	defer owner.Close()
	defer configuration.Close()
	defer status.Close()
	defer executable.Close()
	defer cgroup.Close()
	var observer *os.File
	if capsule.TerminalObserver {
		observer, err = adoptTestProcessPipe(9, "owned-test-terminal-observer")
		if err != nil {
			return 125
		}
		defer observer.Close()
	}
	overallDeadline := time.Unix(0, capsule.OverallDeadline)
	executionDeadline := time.Unix(0, capsule.ExecutionDeadline)
	if err := status.SetWriteDeadline(overallDeadline); err != nil {
		return 125
	}
	if observer != nil {
		if err := observer.SetWriteDeadline(overallDeadline); err != nil {
			return 125
		}
	}
	if err := unix.Prctl(unix.PR_SET_CHILD_SUBREAPER, 1, 0, 0, 0); err != nil {
		return 125
	}
	if err := writeTestProcessGuardianStatus(status, testProcessGuardianStatus{
		Kind: "armed", Token: capsule.Token, Root: capsule.Root,
		CgroupDevice: capsule.CgroupDevice, CgroupInode: capsule.CgroupInode,
	}); err != nil {
		return 125
	}
	terminal := testProcessGuardianStatus{
		Kind: "terminal", Token: capsule.Token, Root: capsule.Root, ExitCode: -1, Reason: "admission-failed",
		CgroupDevice: capsule.CgroupDevice, CgroupInode: capsule.CgroupInode,
	}
	finish := func() int {
		empty, err := testProcessCgroupEmpty(cgroup)
		terminal.Empty = err == nil && empty
		if !terminal.Empty || !terminal.NoChildren || (terminal.PID != 0 && !terminal.LeaderJoined) {
			terminal.Reason = "unjoined"
		}
		// The write may fail after parent death; joining still precedes exit.
		_ = writeTestProcessGuardianStatus(status, terminal)
		if observer != nil {
			if err := writeTestProcessGuardianStatus(observer, terminal); err != nil {
				return 125
			}
		}
		if terminal.Reason == "complete" {
			return 0
		}
		return 125
	}
	beforeExecution, cancel := context.WithDeadline(context.Background(), executionDeadline)
	_, err = readTestProcessConfigurationArchive(beforeExecution, configuration, capsule.ConfigurationLimits, capsule.ConfigurationSHA256)
	cancel()
	if err != nil {
		terminal.NoChildren = true
		return finish()
	}
	ownerEvents := []unix.PollFd{{Fd: int32(owner.Fd()), Events: unix.POLLIN | unix.POLLHUP | unix.POLLERR}}
	if _, err := unix.Poll(ownerEvents, 0); err != nil || ownerEvents[0].Revents != 0 {
		terminal.Reason = "owner-lost"
		terminal.NoChildren = true
		return finish()
	}
	environment, err := testProcessEnvironment(capsule.Environment, "worker")
	if err != nil {
		terminal.NoChildren = true
		return finish()
	}
	remaining := time.Until(executionDeadline)
	if remaining <= 0 {
		terminal.Reason = "execution-deadline"
		terminal.NoChildren = true
		return finish()
	}
	workerCapsule := capsule
	workerCapsule.Role = "worker"
	workerCapsule.TerminalObserver = false
	workerCapsule.ParentPID = os.Getpid()
	workerCapsule.Arguments = []string{
		"/proc/self/fd/3", "-test.run=^" + capsule.Root + "$", "-test.count=1",
		"-test.parallel=" + strconv.Itoa(capsule.Parallel), "-test.timeout=" + remaining.String(), "-test.v",
	}
	capsuleFile, err := sealTestProcessCapsule(workerCapsule)
	if err != nil {
		terminal.NoChildren = true
		return finish()
	}
	defer capsuleFile.Close()
	workerStatusRead, workerStatusWrite, err := os.Pipe()
	if err != nil {
		terminal.NoChildren = true
		return finish()
	}
	defer workerStatusRead.Close()
	defer workerStatusWrite.Close()
	command := exec.Command("/proc/self/fd/3", workerCapsule.Arguments[1:]...)
	command.Args = workerCapsule.Arguments
	command.Env = environment
	command.Dir = capsule.WorkingDirectory
	command.Stdin, command.Stdout, command.Stderr = os.Stdin, os.Stdout, os.Stderr
	command.ExtraFiles = []*os.File{executable, capsuleFile, cgroup, workerStatusWrite, configuration}
	command.SysProcAttr = &syscall.SysProcAttr{
		Setpgid: true, UseCgroupFD: true, CgroupFD: int(cgroup.Fd()),
	}
	// clone3 performs containment entry atomically before any worker code.
	if err := command.Start(); err != nil {
		terminal.Reason = "atomic-entry-failed"
		terminal.NoChildren = true
		return finish()
	}
	terminal.PID = command.Process.Pid
	defer command.Process.Release()
	workerStatusWrite.Close()
	leaderFD, err := unix.PidfdOpen(terminal.PID, 0)
	if err != nil {
		terminal.Reason = "leader-observer-failed"
		_ = killTestProcessCgroup(cgroup)
	} else {
		defer unix.Close(leaderFD)
		terminal.Reason = "complete"
	}
	stopping := err != nil
	killFailed := false
	for {
		// This guardian is the direct parent and child subreaper. ECHILD is
		// required in addition to populated=0, including after owner death.
		for {
			var childStatus unix.WaitStatus
			pid, waitErr := unix.Wait4(-1, &childStatus, unix.WNOHANG, nil)
			if errors.Is(waitErr, unix.EINTR) {
				continue
			}
			if errors.Is(waitErr, unix.ECHILD) {
				terminal.NoChildren = true
				break
			}
			if waitErr != nil {
				terminal.Reason = "child-wait-failed"
				stopping = true
				break
			}
			if pid == 0 {
				terminal.NoChildren = false
				break
			}
			terminal.Reaped++
			if pid == terminal.PID {
				terminal.LeaderJoined = true
				if childStatus.Exited() {
					terminal.ExitCode = childStatus.ExitStatus()
				} else {
					terminal.ExitCode = 128 + int(childStatus.Signal())
				}
				if terminal.ExitCode != 0 && terminal.Reason == "complete" {
					terminal.Reason = "root-failed"
				}
				stopping = true
			}
		}
		empty, emptyErr := testProcessCgroupEmpty(cgroup)
		if emptyErr != nil {
			terminal.Reason = "population-unavailable"
			stopping = true
		}
		if terminal.LeaderJoined && !empty && terminal.Reason == "complete" {
			terminal.Reason = "descendants-after-leader"
		}
		if stopping {
			if err := killTestProcessCgroup(cgroup); err != nil {
				killFailed = true
			}
		}
		if terminal.LeaderJoined && terminal.NoChildren && emptyErr == nil && empty {
			break
		}
		if !time.Now().Before(overallDeadline) {
			_ = killTestProcessCgroup(cgroup)
			terminal.Reason = "unjoined"
			return finish()
		}
		pollUntil := executionDeadline
		if stopping {
			pollUntil = overallDeadline
		}
		wait := time.Until(pollUntil)
		if wait <= 0 {
			terminal.Reason = "execution-deadline"
			stopping = true
			continue
		}
		if wait > 100*time.Millisecond {
			wait = 100 * time.Millisecond
		}
		pollers := []unix.PollFd{{Fd: int32(owner.Fd()), Events: unix.POLLIN | unix.POLLHUP | unix.POLLERR}}
		if leaderFD >= 0 && !terminal.LeaderJoined {
			pollers = append(pollers, unix.PollFd{Fd: int32(leaderFD), Events: unix.POLLIN | unix.POLLHUP | unix.POLLERR})
		}
		// Once cancellation is observed, omit its permanently readable pipe.
		// Poll still rate-limits the join loop instead of spinning on EOF.
		if stopping {
			pollers = nil
		}
		if _, err := unix.Poll(pollers, int((wait+time.Millisecond-1)/time.Millisecond)); err != nil && !errors.Is(err, unix.EINTR) {
			terminal.Reason = "guardian-poll-failed"
			stopping = true
		}
		if len(pollers) != 0 && pollers[0].Revents != 0 {
			terminal.Reason = "owner-lost"
			stopping = true
		}
	}
	if killFailed {
		terminal.Reason = "containment-kill-failed"
	}
	value, err := io.ReadAll(io.LimitReader(workerStatusRead, 2*testProcessIPCBytes+1))
	scanner := bufio.NewScanner(strings.NewReader(string(value)))
	scanner.Buffer(make([]byte, testProcessIPCBytes), testProcessIPCBytes)
	messages := 0
	protocolOK := err == nil && len(value) <= 2*testProcessIPCBytes
	for scanner.Scan() {
		messages++
		var observed testProcessStatus
		if decodeTestProcessJSON(scanner.Bytes(), &observed) != nil || observed.Token != capsule.Token ||
			observed.Root != capsule.Root || observed.PID != terminal.PID ||
			(messages == 1 && observed.Kind != "ready") || (messages == 2 && observed.Kind != "claimed") || messages > 2 {
			protocolOK = false
		}
	}
	protocolOK = protocolOK && scanner.Err() == nil
	terminal.Ready = protocolOK && messages >= 1
	terminal.Claimed = protocolOK && messages == 2
	if (!terminal.Ready || !terminal.Claimed) && terminal.Reason == "complete" {
		terminal.Reason = "generation-not-claimed"
	}
	finalContext, finalCancel := context.WithDeadline(context.Background(), overallDeadline)
	_, finalErr := readTestProcessConfigurationArchive(finalContext, configuration, capsule.ConfigurationLimits, capsule.ConfigurationSHA256)
	finalCancel()
	terminal.ConfigurationChecked = finalErr == nil
	if !terminal.ConfigurationChecked {
		terminal.Reason = "configuration-changed"
	}
	return finish()
}
