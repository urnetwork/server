//go:build linux

package server

// These roots execute the actual compiled test binary under fresh, explicitly
// delegated cgroups. No PG/Redis service is provisioned. Missing containment is
// a hard prerequisite failure, never a skip or a process-group-only fallback.

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// Channel publication joins the real Run call. Only the parent test goroutine
// reads fixture state; cleanup cancels and joins before closing owned handles.
type testProcessExecutionFixture struct {
	owner           *TestProcessCgroup
	spec            TestProcessSpec
	ctx             context.Context
	cancel          context.CancelFunc
	deadline        time.Time
	input           *os.File
	output          *os.File
	scanner         *bufio.Scanner
	resultChannel   chan testProcessExecutionOutcome
	started         bool
	finished        bool
	outcome         testProcessExecutionOutcome
	beforeEventJoin func()
}

// A complete result, not a shared mutable side channel from the Run goroutine.
type testProcessExecutionOutcome struct {
	result TestProcessResult
	err    error
}

// Acquires an explicit inherited delegation without opening any default host
// path. The test runner must retain the original descriptor for every root.
func openTestProcessTestDelegation(t *testing.T) *os.File {
	t.Helper()
	value := os.Getenv("URNETWORK_TEST_PROCESS_DELEGATION_FD")
	fd, err := strconv.Atoi(value)
	if err != nil || fd < 3 || strconv.Itoa(fd) != value {
		t.Fatal("actual process tests require an explicit fresh cgroup-v2 delegation descriptor")
	}
	duplicate, err := unix.FcntlInt(uintptr(fd), unix.F_DUPFD_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	return os.NewFile(uintptr(duplicate), "explicit-test-delegation-copy")
}

// Uses the test runner's original absolute deadline, never a reset per child.
func newTestProcessExecutionFixture(t *testing.T) *testProcessExecutionFixture {
	t.Helper()
	deadline, ok := t.Deadline()
	if !ok {
		t.Fatal("actual process tests require the runner's original deadline")
	}
	if original := os.Getenv("URNETWORK_TEST_PROCESS_FIXTURE_ORIGINAL_DEADLINE"); original != "" {
		nanos, err := strconv.ParseInt(original, 10, 64)
		if err != nil || !time.Unix(0, nanos).Before(deadline) {
			t.Fatal("middle owner did not retain the original earlier absolute deadline")
		}
		deadline = time.Unix(0, nanos)
	}
	delegation := openTestProcessTestDelegation(t)
	owner, err := CreateTestProcessCgroup(delegation)
	delegation.Close()
	if err != nil {
		if owner != nil {
			ctx, cancel := context.WithDeadline(context.Background(), deadline)
			defer cancel()
			_ = owner.Close(ctx)
		}
		t.Fatalf("fresh contained process prerequisite: %v", err)
	}
	ctx, cancel := context.WithDeadline(t.Context(), deadline)
	configuration := newTestProcessConfigurationFixture(t)
	executable, err := os.Open("/proc/self/exe")
	if err != nil {
		t.Fatal(err)
	}
	info, err := executable.Stat()
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.New()
	if _, err := io.Copy(digest, io.NewSectionReader(executable, 0, info.Size())); err != nil {
		t.Fatal(err)
	}
	stderr, err := os.CreateTemp(t.TempDir(), "owned-process-stderr")
	if err != nil {
		t.Fatal(err)
	}
	fixture := &testProcessExecutionFixture{
		owner: owner, ctx: ctx, cancel: cancel, deadline: deadline,
		resultChannel: make(chan testProcessExecutionOutcome, 1),
		spec: TestProcessSpec{
			Executable: executable, ExecutableBytes: info.Size(), ExecutableSHA256: hex.EncodeToString(digest.Sum(nil)),
			WorkingDirectory: filepath.Dir(executable.Name()), Configuration: configuration, JoinReserve: 5 * time.Second,
			Parallel: 1, Environment: []string{"WARP_ENV=local", "WARP_HOST=owned-process-test"}, Stderr: stderr,
		},
	}
	// Work in this package's actual caller-selected cwd, not /proc/self.
	if fixture.spec.WorkingDirectory, err = os.Getwd(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cancel()
		if fixture.input != nil {
			fixture.input.Close()
		}
		if fixture.started && !fixture.finished {
			select {
			case fixture.outcome = <-fixture.resultChannel:
				fixture.finished = true
			case <-time.After(max(time.Until(deadline), 0)):
				t.Error("actual process fixture could not join its Run owner")
			}
		}
		if fixture.output != nil {
			fixture.output.Close()
		}
		cleanupContext, cleanupCancel := context.WithDeadline(context.Background(), deadline)
		defer cleanupCancel()
		if err := owner.Close(cleanupContext); err != nil {
			t.Errorf("owned containment preserved after unjoined cleanup: %v", err)
		}
		if t.Failed() {
			value, _ := os.ReadFile(stderr.Name())
			t.Logf("owned child stderr: %s", value)
		}
		stderr.Close()
		executable.Close()
	})
	return fixture
}

// Starts a complete root with only standard-stream barriers; private guardian
// status/control and caller delegation are never exposed to the worker.
func (self *testProcessExecutionFixture) start(t *testing.T, scenario string) {
	t.Helper()
	inputRead, inputWrite, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	outputRead, outputWrite, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	self.input, self.output = inputWrite, outputRead
	self.spec.Stdin, self.spec.Stdout = inputRead, outputWrite
	self.spec.Root = t.Name()
	self.spec.Environment = append(self.spec.Environment, "URNETWORK_TEST_PROCESS_FIXTURE_CASE="+scenario)
	if err := outputRead.SetReadDeadline(self.deadline); err != nil {
		t.Fatal(err)
	}
	self.scanner = bufio.NewScanner(outputRead)
	self.scanner.Buffer(make([]byte, 4096), 65536)
	self.started = true
	go func() {
		result, err := self.owner.Run(self.ctx, self.spec)
		inputRead.Close()
		outputWrite.Close()
		self.resultChannel <- testProcessExecutionOutcome{result: result, err: err}
	}()
}

// Reads bounded explicit readiness, not a sleep or filesystem polling loop.
func (self *testProcessExecutionFixture) event(t *testing.T, prefix string) string {
	t.Helper()
	value, err := self.readEvent(prefix)
	if err != nil {
		t.Fatal(err)
	}
	return value
}

// The fixture retains its stdout writer until Run returns, so true EOF has an
// already-completed result. Other read failures must first cancel a live owner.
func (self *testProcessExecutionFixture) readEvent(prefix string) (string, error) {
	for lines := 0; lines < 4096 && self.scanner.Scan(); lines++ {
		value := self.scanner.Text()
		if strings.HasPrefix(value, prefix) {
			return strings.TrimPrefix(value, prefix), nil
		}
	}
	self.cancel()
	if self.beforeEventJoin != nil {
		self.beforeEventJoin()
	}
	if !self.finished {
		select {
		case self.outcome = <-self.resultChannel:
			self.finished = true
		case <-time.After(max(time.Until(self.deadline), 0)):
			return "", fmt.Errorf("owned child did not reach explicit %q barrier: scan_error=%v; Run did not join before original deadline", prefix, self.scanner.Err())
		}
	}
	return "", fmt.Errorf("owned child did not reach explicit %q barrier: scan_error=%v; run_result=%+v; run_error=%v", prefix, self.scanner.Err(), self.outcome.result, self.outcome.err)
}

// Exactly one release byte advances the child past its witnessed barrier.
func (self *testProcessExecutionFixture) release(t *testing.T) {
	t.Helper()
	if count, err := self.input.Write([]byte{1}); err != nil || count != 1 {
		t.Fatalf("owned child barrier release: %d %v", count, err)
	}
}

// Joins the actual owner result before asserting its terminal authority.
func (self *testProcessExecutionFixture) finish(t *testing.T) testProcessExecutionOutcome {
	t.Helper()
	if !self.finished {
		select {
		case self.outcome = <-self.resultChannel:
			self.finished = true
		case <-time.After(max(time.Until(self.deadline), 0)):
			t.Fatal("actual process Run exceeded its original deadline")
		}
	}
	return self.outcome
}

// Child blocking is controlled by an explicit parent barrier and the original
// overall owner deadline; it never adds a new timeout to test execution.
func waitTestProcessParentRelease(t *testing.T) {
	t.Helper()
	var value [1]byte
	if _, err := io.ReadFull(os.Stdin, value[:]); err != nil || value[0] != 1 {
		t.Fatalf("owned parent release is unavailable: %v", err)
	}
}

// A private configuration file is installed before the read-only snapshot.
func addTestProcessConfigurationFile(t *testing.T, configuration *TestProcessConfiguration, relative string, value []byte) {
	t.Helper()
	root := configuration.Directory.Name()
	target := filepath.Join(root, relative)
	parent := filepath.Dir(target)
	if err := os.Chmod(parent, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, value, 0o400); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(parent, 0o500); err != nil {
		t.Fatal(err)
	}
	var err error
	configuration.SHA256, err = SnapshotTestProcessConfiguration(configuration.Directory, configuration.Limits)
	if err != nil {
		t.Fatal(err)
	}
}

// The application Cleanup runs on the real testing.T while its route is still
// owned. The original wrong-target assertion is retained in the actual child.
func TestTestProcessOwnedRootKeepsTestingCleanupTarget(t *testing.T) {
	if !IsOwnedTestProcess() {
		fixture := newTestProcessExecutionFixture(t)
		fixture.start(t, "cleanup")
		outcome := fixture.finish(t)
		if outcome.err != nil || !outcome.result.Joined || !outcome.result.GenerationClaimed {
			t.Fatalf("owned cleanup root did not complete: %+v %v", outcome.result, outcome.err)
		}
		return
	}
	newTestEnvRedisDNSGuard(t)
	fixture := newTestEnvRedisDispatchFixture(t)
	var cleanupCalls atomic.Int32
	t.Cleanup(func() {
		if cleanupCalls.Load() != 1 {
			t.Fatalf("owned testing cleanup did not run exactly once: %d", cleanupCalls.Load())
		}
		for _, command := range fixture.snapshot() {
			if command.name == "flushdb" && command.database != 2 {
				t.Fatalf("test cleanup dispatched through restored parent client: owner_db=2 dispatched_db=%d", command.database)
			}
		}
		commands := fixture.snapshot()
		if len(commands) != 4 || fixture.releases[2] != 0 {
			t.Fatalf("child released resources before complete process join: commands=%+v releases=%v", commands, fixture.releases)
		}
		requireTestEnvRedisDispatchPair(t, commands[:2], 2)
		requireTestEnvRedisDispatchPair(t, commands[2:], 2)
	})
	runOwnedTestRootWithSetup(t, func() error {
		// The parent owns eventual resource release. This hermetic lifecycle
		// test intentionally never invokes the child's old teardown callback.
		_ = fixture.setup(2)
		return nil
	}, func(tb testing.TB) {
		tb.Cleanup(func() {
			ctx := context.Background()
			Redis(ctx, func(client RedisClient) { Raise(client.FlushDB(ctx).Err()) }, OptNoRetry())
			cleanupCalls.Add(1)
		})
	})
	if cleanupCalls.Load() != 0 {
		t.Fatal("application testing.Cleanup ran before post-callback root assertions")
	}
}

// The retained Background worker is released only after callback success, and
// its real Redis wrapper still uses the process-owned route.
func TestTestProcessOwnedRootKeepsBackgroundWorkerTarget(t *testing.T) {
	if !IsOwnedTestProcess() {
		fixture := newTestProcessExecutionFixture(t)
		fixture.start(t, "worker")
		outcome := fixture.finish(t)
		if outcome.err != nil || !outcome.result.Joined || !outcome.result.GenerationClaimed {
			t.Fatalf("owned Background root did not complete: %+v %v", outcome.result, outcome.err)
		}
		return
	}
	newTestEnvRedisDNSGuard(t)
	fixture := newTestEnvRedisDispatchFixture(t)
	var worker *testEnvLifetimeWorker
	t.Cleanup(func() {
		if worker != nil {
			worker.resumeAndJoin()
		}
		if fixture.releases[2] != 0 {
			t.Fatal("child released the namespace before process completion")
		}
	})
	runOwnedTestRootWithSetup(t, func() error { _ = fixture.setup(2); return nil }, func(testing.TB) {
		worker = newTestEnvLifetimeWorker()
	})
	if worker == nil {
		t.Fatal("successful callback never created the owned worker")
	}
	before := len(fixture.snapshot())
	worker.resumeAndJoin()
	if worker.failure != nil || !worker.returned {
		t.Fatalf("owned Background worker failed before target observation: failure=%v returned=%t", worker.failure, worker.returned)
	}
	commands := fixture.snapshot()
	if len(commands) != before+2 {
		t.Fatalf("owned Background worker command census differs: before=%d commands=%+v", before, commands)
	}
	for _, command := range commands[before:] {
		if command.name == "flushdb" && command.database != 2 {
			t.Fatalf("successful attempt worker dispatched through restored parent client: owner_db=2 dispatched_db=%d", command.database)
		}
	}
	requireTestEnvRedisDispatchPair(t, commands[before:], 2)
}

// Actual worker admission consumes one generation before any second effects.
func TestTestProcessRejectsSecondGenerationBeforeEffects(t *testing.T) {
	if !IsOwnedTestProcess() {
		fixture := newTestProcessExecutionFixture(t)
		fixture.start(t, "generation")
		outcome := fixture.finish(t)
		if outcome.err != nil || !outcome.result.Joined {
			t.Fatalf("owned generation root failed: %+v %v", outcome.result, outcome.err)
		}
		if _, err := fixture.owner.Run(fixture.ctx, fixture.spec); !errors.Is(err, ErrTestProcessGenerationUsed) {
			t.Fatalf("spent process capability was reusable: %v", err)
		}
		return
	}
	if err := ClaimTestProcessGeneration(t.Name()); err != nil {
		t.Fatal(err)
	}
	if err := ClaimTestProcessGeneration(t.Name()); !errors.Is(err, ErrTestProcessGenerationUsed) {
		t.Fatalf("second generation was admitted: %v", err)
	}
}

// Cancellation occurs only after real worker readiness; the result must retain
// cancellation while proving the complete tree was actually joined.
func TestTestProcessCancellationJoinsAndPreservesCause(t *testing.T) {
	if IsOwnedTestProcess() {
		if err := ClaimTestProcessGeneration(t.Name()); err != nil {
			t.Fatal(err)
		}
		fmt.Fprintln(os.Stdout, "OWNED_READY")
		waitTestProcessParentRelease(t)
		return
	}
	fixture := newTestProcessExecutionFixture(t)
	fixture.start(t, "cancel")
	fixture.event(t, "OWNED_READY")
	fixture.cancel()
	outcome := fixture.finish(t)
	if !errors.Is(outcome.err, context.Canceled) || !outcome.result.Joined || !outcome.result.Started {
		t.Fatalf("canceled execution lost cause or complete join: %+v %v", outcome.result, outcome.err)
	}
}

// A canceled owner never starts a guardian or consumes the fresh capability.
func TestTestProcessCanceledAdmissionHasNoProcessEffects(t *testing.T) {
	fixture := newTestProcessExecutionFixture(t)
	fixture.cancel()
	if result, err := fixture.owner.Run(fixture.ctx, fixture.spec); !errors.Is(err, context.Canceled) || result.Started || fixture.owner.used {
		t.Fatalf("canceled admission performed process effects: %+v %v", result, err)
	}
}

// A Setpgid descendant remains parked after leader success. The guardian must
// kill and reap it; leader Wait alone is not a successful completion witness.
func TestTestProcessLeaderExitJoinsSetpgidDescendant(t *testing.T) {
	if os.Getenv("URNETWORK_TEST_PROCESS_FIXTURE_CASE") == "grandchild" {
		fmt.Fprintf(os.Stdout, "DESCENDANT_READY %d\n", os.Getpid())
		deadline, err := strconv.ParseInt(os.Getenv("URNETWORK_TEST_PROCESS_FIXTURE_DEADLINE"), 10, 64)
		if err != nil {
			t.Fatal(err)
		}
		<-time.After(max(time.Until(time.Unix(0, deadline)), 0))
		return
	}
	if IsOwnedTestProcess() {
		if err := ClaimTestProcessGeneration(t.Name()); err != nil {
			t.Fatal(err)
		}
		outputRead, outputWrite, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		command := exec.Command("/proc/self/exe", "-test.run=^"+t.Name()+"$", "-test.count=1",
			"-test.timeout="+time.Until(testProcessChild.deadline).String())
		command.Env = []string{
			"URNETWORK_TEST_PROCESS_FIXTURE_CASE=grandchild",
			"URNETWORK_TEST_PROCESS_FIXTURE_DEADLINE=" + strconv.FormatInt(testProcessChild.deadline.UnixNano(), 10),
		}
		command.Stdout, command.Stderr = outputWrite, os.Stderr
		command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		if err := command.Start(); err != nil {
			t.Fatal(err)
		}
		outputWrite.Close()
		scanner := bufio.NewScanner(outputRead)
		for scanner.Scan() {
			if strings.HasPrefix(scanner.Text(), "DESCENDANT_READY ") {
				fmt.Fprintln(os.Stdout, scanner.Text())
				break
			}
		}
		outputRead.Close()
		waitTestProcessParentRelease(t)
		// Intentionally do not Wait: this is the actual leaked descendant
		// topology under test, not a dropped fixture goroutine.
		return
	}
	fixture := newTestProcessExecutionFixture(t)
	fixture.start(t, "leader")
	pid, err := strconv.Atoi(fixture.event(t, "DESCENDANT_READY "))
	if err != nil {
		t.Fatal(err)
	}
	pidfd, err := unix.PidfdOpen(pid, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(pidfd)
	group, err := unix.Getpgid(pid)
	if err != nil || group != pid {
		t.Fatalf("descendant did not establish its independent process group: %d %v", group, err)
	}
	fixture.release(t)
	outcome := fixture.finish(t)
	if outcome.err == nil || !strings.Contains(outcome.err.Error(), "descendants-after-leader") ||
		!outcome.result.Joined || outcome.result.ReapedProcesses < 2 {
		t.Fatalf("surviving descendant bypassed complete-tree join: %+v %v", outcome.result, outcome.err)
	}
	pollers := []unix.PollFd{{Fd: int32(pidfd), Events: unix.POLLIN}}
	if _, err := unix.Poll(pollers, 0); err != nil || pollers[0].Revents&unix.POLLIN == 0 {
		t.Fatalf("joined descendant pidfd is still live: %+v %v", pollers, err)
	}
}

// This neutral witness restores the same bytes before final snapshot validation.
// Its failure must come from the real lazy resolver observing the transient
// rewrite, not a different terminal hash or an earlier metadata guard.
func TestTestProcessLazyResolverRejectsTransientConfigurationRewrite(t *testing.T) {
	if IsOwnedTestProcess() {
		if err := ClaimTestProcessGeneration(t.Name()); err != nil {
			t.Fatal(err)
		}
		fmt.Fprintln(os.Stdout, "COLD_RESOURCE_READY")
		waitTestProcessParentRelease(t)
		resource, err := Config.SimpleResource("runtime.yml")
		if err != nil {
			t.Fatal(err)
		}
		actual := resource.RequireString("marker")
		fmt.Fprintf(os.Stdout, "COLD_RESOURCE_READ %s\n", actual)
		waitTestProcessParentRelease(t)
		if actual != "original" {
			t.Fatalf("owned lazy resolver observed transient configuration rewrite: got=%s want=original", actual)
		}
		return
	}
	fixture := newTestProcessExecutionFixture(t)
	original := []byte("marker: original\n")
	addTestProcessConfigurationFile(t, &fixture.spec.Configuration, "config/runtime.yml", original)
	target := filepath.Join(fixture.spec.Configuration.Directory.Name(), "config", "runtime.yml")
	rewrite := func(value []byte) {
		t.Helper()
		if err := os.Chmod(target, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(target, value, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(target, 0o400); err != nil {
			t.Fatal(err)
		}
	}
	fixture.start(t, "lazy-config")
	fixture.event(t, "COLD_RESOURCE_READY")
	rewrite([]byte("marker: tampered\n"))
	fixture.release(t)
	actual := fixture.event(t, "COLD_RESOURCE_READ ")
	rewrite(original)
	fixture.release(t)
	outcome := fixture.finish(t)
	if !outcome.result.Joined {
		t.Fatalf("lazy configuration witness did not join its actual worker: %+v %v", outcome.result, outcome.err)
	}
	if actual != "original" {
		t.Fatalf("owned lazy resolver observed transient configuration rewrite: got=%s want=original", actual)
	}
	if outcome.err != nil || !outcome.result.Joined {
		t.Fatalf("owned lazy resolver did not retain sealed configuration authority: %+v %v", outcome.result, outcome.err)
	}
}
