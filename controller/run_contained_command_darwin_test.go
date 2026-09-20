// Verifies Darwin's process-state boundary independently of reaper scheduling.
package controller

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// Keep an owned child unreaped so the zombie-only process group cannot disappear.
func TestContainedProcessGroupDarwinTracksUnreapedMember(t *testing.T) {
	processGroupId := startContainedDarwinZombie(t)
	t.Logf("zombie-only signal-zero probe = %v", syscall.Kill(-processGroupId, 0))
	if running, err := containedProcessGroupRunning(processGroupId); err != nil || running {
		t.Fatalf("unreaped private process group = %v, %v", running, err)
	}
}

// Killing an already-dead group must succeed even before its parent reaps it.
func TestTerminateContainedProcessGroupDarwinIgnoresUnreapedMember(t *testing.T) {
	processGroupId := startContainedDarwinZombie(t)
	t.Logf("zombie-only kill probe = %v", syscall.Kill(-processGroupId, syscall.SIGKILL))
	if err := terminateContainedProcessGroup(processGroupId); err != nil {
		t.Fatalf("terminate unreaped private process group: %v", err)
	}
}

// A zombie leader must neither mask its live group member nor block its termination.
func TestTerminateContainedProcessGroupDarwinTerminatesLiveMemberWithZombieLeader(t *testing.T) {
	processGroupId := startContainedDarwinZombie(t)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, "/bin/sh", "-c", "printf ready; read release")
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Pgid: processGroupId}
	input, err := command.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	defer input.Close()
	output, err := command.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	defer output.Close()
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if command.ProcessState == nil {
			_ = command.Process.Kill()
			_ = command.Wait()
		}
	}()
	var ready [5]byte
	if _, err := io.ReadFull(output, ready[:]); err != nil || string(ready[:]) != "ready" {
		t.Fatalf("group member readiness = %q, %v", ready, err)
	}
	if running, err := containedProcessGroupRunning(processGroupId); err != nil || !running {
		t.Fatalf("live member masked by zombie leader: running = %v, error = %v", running, err)
	}
	if err := terminateContainedProcessGroup(processGroupId); err != nil {
		t.Fatal(err)
	}
	var exitErr *exec.ExitError
	if err := command.Wait(); !errors.As(err, &exitErr) || exitErr.ProcessState.Sys().(syscall.WaitStatus).Signal() != syscall.SIGKILL {
		t.Fatalf("live member was not killed: %v", err)
	}
	if running, err := containedProcessGroupRunning(processGroupId); err != nil || running {
		t.Fatalf("only the zombie leader should remain: running = %v, error = %v", running, err)
	}
}

// Darwin sets SZOMB before delivering SIGCHLD; cleanup alone reaps this exact child.
func startContainedDarwinZombie(t *testing.T) int {
	t.Helper()
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGCHLD)
	defer signal.Stop(signals)
	command := exec.Command("/bin/sh", "-c", "read release")
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	input, err := command.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = input.Close() })
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = command.Process.Kill()
		_ = command.Wait()
	})
	if _, err := fmt.Fprintln(input, "release"); err != nil {
		t.Fatal(err)
	}
	deadline := time.NewTimer(10 * time.Second)
	defer deadline.Stop()
	for {
		select {
		case <-signals:
			processes, err := unix.SysctlKinfoProcSlice("kern.proc.pgrp", command.Process.Pid)
			if err != nil || len(processes) != 1 {
				t.Fatalf("unreaped process group = %d members, %v", len(processes), err)
			}
			if process := processes[0]; int(process.Proc.P_pid) == command.Process.Pid && process.Proc.P_stat == 5 {
				return command.Process.Pid
			}
			// A different child can signal first; wait for this child's exact state.
		case <-deadline.C:
			t.Fatal("owned child did not report its zombie state")
		}
	}
}

// Sleeping, stopped, and unknown states must not be mistaken for exited children.
func TestContainedProcessGroupDarwinIgnoresOnlyZombies(t *testing.T) {
	for _, testCase := range []struct {
		name    string
		state   int8
		groupId int32
		running bool
	}{
		{name: "starting", state: 1, groupId: 100, running: true},
		{name: "runnable", state: 2, groupId: 100, running: true},
		{name: "sleeping", state: 3, groupId: 100, running: true},
		{name: "stopped", state: 4, groupId: 100, running: true},
		{name: "zombie", state: 5, groupId: 100},
		{name: "unknown", state: 0, groupId: 100, running: true},
		{name: "other group", state: 2, groupId: 101},
	} {
		processes := []unix.KinfoProc{{
			Proc:  unix.ExternProc{P_stat: testCase.state},
			Eproc: unix.Eproc{Pgid: testCase.groupId},
		}}
		if running := containedDarwinProcessGroupHasLiveMembers(100, processes); running != testCase.running {
			t.Errorf("%s running = %v, want %v", testCase.name, running, testCase.running)
		}
	}
	processes := []unix.KinfoProc{
		{Proc: unix.ExternProc{P_stat: 5}, Eproc: unix.Eproc{Pgid: 100}},
		{Proc: unix.ExternProc{P_stat: 3}, Eproc: unix.Eproc{Pgid: 100}},
	}
	if !containedDarwinProcessGroupHasLiveMembers(100, processes) {
		t.Fatal("a zombie masked the group's live descendant")
	}
	if containedDarwinProcessGroupHasLiveMembers(100, nil) {
		t.Fatal("an empty process group is running")
	}
}
