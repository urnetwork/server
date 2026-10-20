//go:build linux

// Actual native cancellation proves the source retires its own command child.
package monitor

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// A FIFO rendezvous proves the blocking native process actually started.
// There are no wall-clock sleeps or external database fixture dependencies.
func TestRedisShardSubscriptionsNativeCancellationJoinsOwnedChild(t *testing.T) {
	redisShardNativeCancellation(t, false)
}

// Interpose the real Popen return at the ownership boundary. The source's
// unchanged program receives TERM after creation but before child assignment.
func TestRedisShardSubscriptionsNativeLaunchCancellationJoinsOwnedChild(t *testing.T) {
	redisShardNativeCancellation(t, true)
}

// Both boundaries must retire the exact child and preserve an unrelated child.
func redisShardNativeCancellation(t *testing.T, duringLaunch bool) {
	t.Helper()
	request, environment, _ := redisShardNativeFixture(t, "counts", "block")
	readyPath := filepath.Join(t.TempDir(), "ready")
	if err := syscall.Mkfifo(readyPath, 0600); err != nil {
		t.Fatal(err)
	}
	ready, err := os.OpenFile(readyPath, os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer ready.Close()
	program := redisShardSourceProgram
	if duringLaunch {
		nativeReadyPath := filepath.Join(t.TempDir(), "native-ready")
		if err := syscall.Mkfifo(nativeReadyPath, 0600); err != nil {
			t.Fatal(err)
		}
		environment = append(environment, "RSC_READY="+nativeReadyPath, "RSC_LAUNCH="+readyPath)
		program = redisShardLaunchCancellationBootstrap
	} else {
		environment = append(environment, "RSC_READY="+readyPath)
	}
	encoded, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Fatal("required native Python fixture unavailable")
	}
	sibling := exec.Command("/bin/cat")
	siblingInput, err := sibling.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	sibling.Stdout = io.Discard
	if err := sibling.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { siblingInput.Close(); sibling.Wait() }()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	arguments := []string{"-c", program}
	if duringLaunch {
		arguments = append(arguments, redisShardSourceProgram)
	}
	arguments = append(arguments, string(encoded))
	command := exec.CommandContext(ctx, python, arguments...)
	command.Env = environment
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if command.ProcessState == nil {
			command.Process.Kill()
			command.Wait()
		}
	}()
	readyResult := make(chan string, 1)
	go func() { line, _ := bufio.NewReader(ready).ReadString('\n'); readyResult <- line }()
	var childPid int
	select {
	case line := <-readyResult:
		childPid, err = strconv.Atoi(strings.TrimSpace(line))
		if err != nil || childPid <= 1 {
			t.Fatal("native child did not establish its exact identity")
		}
	case <-ctx.Done():
		t.Fatal("native cancellation fixture did not reach the start barrier")
	}
	if !duringLaunch {
		if err := command.Process.Signal(syscall.SIGTERM); err != nil {
			t.Fatal(err)
		}
	}
	if err := command.Wait(); err == nil {
		t.Fatal("cancelled native source returned success")
	}
	if err := sibling.Process.Signal(syscall.Signal(0)); err != nil {
		t.Fatal("native cancellation disturbed an independent healthy sibling")
	}
	if err := syscall.Kill(childPid, 0); !errors.Is(err, syscall.ESRCH) {
		t.Fatal("cancelled source left its owned native child alive")
	}
}

// The bootstrap changes only the test process's Popen call boundary. The real
// native child confirms its PID before TERM is delivered to the collector;
// no production-source replacement or timing sleep creates the schedule.
const redisShardLaunchCancellationBootstrap = `import os, signal, subprocess, sys
program = sys.argv.pop(1)
real_popen = subprocess.Popen

def at_launch(*args, **kwargs):
    child = real_popen(*args, **kwargs)
    with open(os.environ['RSC_READY'], 'r') as ready:
        assert int(ready.readline().strip()) == child.pid
    with open(os.environ['RSC_LAUNCH'], 'w') as ready:
        ready.write(str(child.pid)+'\n'); ready.flush()
    os.kill(os.getpid(), signal.SIGTERM)
    return child

subprocess.Popen = at_launch
exec(compile(program, '<unchanged-sharded-census>', 'exec'))
`
