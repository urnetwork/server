//go:build linux

// Synthetic service owners and exact test-owned subprocesses exercise joins,
// cancellation and durable episode limits without PID1 or native kernel writes.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// Test callbacks cannot select a production process or bypass native authority.
type cacheServiceTestRunner func(context.Context) cacheServicePassOutcome

func (self cacheServiceTestRunner) Run(ctx context.Context) cacheServicePassOutcome { return self(ctx) }

// Joined success is an explicit fixture fact, not an elapsed-time assumption.
func cacheServiceTestJoined() cacheServicePassOutcome {
	return cacheServicePassOutcome{Started: true, Joined: true, Result: &cacheResult{Status: "target_observed"}}
}

// Scheduling occurs only after the prior Run returned its positive join.
func TestCacheServiceSchedulesOnlyAfterPositiveJoin(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	settings := defaultCacheSettings()
	calls, waits := 0, 0
	active := false
	runner := cacheServiceTestRunner(func(context.Context) cacheServicePassOutcome {
		if active || calls != waits {
			t.Fatal("another pass began before the previous joined interval")
		}
		active = true
		calls++
		defer func() { active = false }()
		return cacheServiceTestJoined()
	})
	output := &bytes.Buffer{}
	err := superviseCacheService(ctx, settings, runner, output, func(ctx context.Context, interval time.Duration) error {
		if active || interval != settings.Interval {
			t.Fatal("the owner waited before joining or changed its configured interval")
		}
		waits++
		if waits == 3 {
			cancel()
			return ctx.Err()
		}
		return nil
	})
	if err != nil || calls != 3 || waits != 3 {
		t.Fatal("bounded sequential scheduling did not stop with its owner", calls, waits, err)
	}
	decoder := json.NewDecoder(output)
	for range calls {
		var event cacheServiceEvent
		if err := decoder.Decode(&event); err != nil || event.Status != "pass_joined" || !event.ChildJoined || !event.ChildStarted {
			t.Fatal("joined liveness event omitted its positive custody facts", event, err)
		}
	}
	if err := decoder.Decode(&cacheServiceEvent{}); err != io.EOF {
		t.Fatal("service emitted unbounded or trailing status", err)
	}
}

// Notification records are synchronized so hold tests need no wall-clock race.
type cacheServiceTestOutput struct {
	lock    sync.Mutex
	content bytes.Buffer
	event   chan struct{}
}

func (self *cacheServiceTestOutput) Write(content []byte) (int, error) {
	self.lock.Lock()
	defer self.lock.Unlock()
	count, err := self.content.Write(content)
	select {
	case self.event <- struct{}{}:
	default:
	}
	return count, err
}

// Neither a lost join nor an ordinary failed pass can start a retry loop.
func TestCacheServiceFailureHoldsStableOwnerWithoutRetry(t *testing.T) {
	for _, joined := range []bool{false, true} {
		name := "unjoined"
		if joined {
			name = "joined_failure"
		}
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			calls, waits := 0, 0
			runner := cacheServiceTestRunner(func(context.Context) cacheServicePassOutcome {
				calls++
				return cacheServicePassOutcome{Started: true, Joined: joined, Err: errors.New("synthetic private cause must not be published")}
			})
			output := &cacheServiceTestOutput{event: make(chan struct{}, 1)}
			done := make(chan error, 1)
			go func() {
				done <- superviseCacheService(ctx, defaultCacheSettings(), runner, output, func(context.Context, time.Duration) error {
					waits++
					return nil
				})
			}()
			select {
			case <-output.event:
			case <-t.Context().Done():
				t.Fatal("service did not publish its held status")
			}
			select {
			case err := <-done:
				t.Fatal("failed owner exited instead of retaining its stable cgroup", err)
			default:
			}
			cancel()
			err := <-done
			if (err != nil) == joined || calls != 1 || waits != 0 {
				t.Fatal("held owner retried, lost join uncertainty, or failed to stop", calls, waits, err)
			}
			output.lock.Lock()
			defer output.lock.Unlock()
			if strings.Contains(output.content.String(), "synthetic private") {
				t.Fatal("raw private cause escaped the finite event")
			}
			var event cacheServiceEvent
			if err := json.Unmarshal(output.content.Bytes(), &event); err != nil || event.ChildJoined != joined || !strings.HasSuffix(event.Status, "_held") {
				t.Fatal("held status invented positive join", event, err)
			}
		})
	}
}

// Parent reconstruction cannot erase the existing journal's two-request cap.
func TestCacheServiceKeepsDurableEpisodeBudgetAndHoldsExhaustion(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	settings := defaultCacheSettings()
	journal := &cacheTestJournal{state: cacheState{Schema: 1}}
	kernel := &cacheTestKernel{snapshot: cacheStarvedSnapshot()}
	calls, waits := 0, 0
	runner := cacheServiceTestRunner(func(ctx context.Context) cacheServicePassOutcome {
		calls++
		result, err := maintainCache(ctx, settings, kernel, journal)
		return cacheServicePassOutcome{Started: true, Joined: true, Result: &result, Err: err}
	})
	output := &cacheServiceTestOutput{event: make(chan struct{}, 8)}
	done := make(chan error, 1)
	go func() {
		done <- superviseCacheService(ctx, settings, runner, output, func(context.Context, time.Duration) error {
			waits++
			return nil
		})
	}()
	for range settings.MaximumEpisodeAttempts + 1 {
		select {
		case <-output.event:
		case <-t.Context().Done():
			t.Fatal("service did not reach its held exhausted episode")
		}
	}
	cancel()
	if err := <-done; err != nil || calls != 3 || waits != 2 || len(kernel.requests) != 2 ||
		journal.state.Attempts != 2 || journal.state.RequestedBytes != settings.MaximumEpisodeBytes || !journal.state.Active {
		t.Fatal("stable owner reset, exceeded or retried an exhausted persisted episode", calls, waits, journal.state, err)
	}
}

// A canceled parent cannot issue its first child or even enter a retry interval.
func TestCacheServiceCanceledOwnerNeverStartsPass(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	runner := cacheServiceTestRunner(func(context.Context) cacheServicePassOutcome {
		t.Fatal("canceled service issued a pass")
		return cacheServicePassOutcome{}
	})
	if err := superviseCacheService(ctx, defaultCacheSettings(), runner, io.Discard, waitCacheService); err != nil {
		t.Fatal(err)
	}
	native := &nativeCacheServicePass{settings: defaultCacheSettings(), newCommand: func() *exec.Cmd {
		t.Fatal("canceled native owner resolved a process")
		return nil
	}}
	if result := native.Run(ctx); result.Started || !result.Joined || !errors.Is(result.Err, context.Canceled) {
		t.Fatal("canceled child acquired execution or lost its no-process proof", result)
	}
}

// This subprocess branch is test-owned and reachable only through a private
// test environment. Production has no factory, environment or executable knob.
func TestCacheServiceChildHelper(t *testing.T) {
	mode := os.Getenv("URNETWORK_CACHE_TEST_CHILD")
	if mode == "" {
		return
	}
	if path := os.Getenv("URNETWORK_CACHE_TEST_CGROUP"); path != "" {
		content, err := os.ReadFile("/proc/self/cgroup")
		if err != nil || os.WriteFile(path, content, 0o600) != nil {
			os.Exit(3)
		}
	}
	switch mode {
	case "healthy":
		_ = json.NewEncoder(os.Stdout).Encode(cacheResult{Status: "target_observed"})
	case "failed":
		_ = json.NewEncoder(os.Stdout).Encode(cacheResult{Status: "episode_budget_exhausted"})
		os.Exit(1)
	case "malformed":
		_, _ = io.WriteString(os.Stdout, "synthetic malformed result")
	case "trailing":
		_, _ = io.WriteString(os.Stdout, "{\"status\":\"target_observed\"}\n{\"status\":\"target_observed\"}\n")
	case "unknown":
		_ = json.NewEncoder(os.Stdout).Encode(cacheResult{Status: "synthetic private unexpected status"})
	case "flood":
		_, _ = io.WriteString(os.Stdout, strings.Repeat("x", 128*1024))
	case "ignore_term":
		signal.Ignore(syscall.SIGTERM)
		if err := os.WriteFile(os.Getenv("URNETWORK_CACHE_TEST_READY"), []byte("ready"), 0o600); err != nil {
			os.Exit(3)
		}
		for {
			time.Sleep(time.Hour)
		}
	default:
		os.Exit(3)
	}
	os.Exit(0)
}

// Only fixture helpers replace native command construction. No PID1, new
// cgroup, production path, daemon, pressure generator or kernel write is used.
func cacheServiceTestCommand(mode string, environment ...string) *exec.Cmd {
	command := exec.Command(os.Args[0], "-test.run=^TestCacheServiceChildHelper$")
	command.Env = append([]string{"URNETWORK_CACHE_TEST_CHILD=" + mode}, environment...)
	return command
}

// A real process wait and same-cgroup child are proved independently of fakes.
func TestCacheServiceNativeChildJoinsInsideItsExistingCgroup(t *testing.T) {
	path := filepath.Join(t.TempDir(), "child-cgroup")
	runner := &nativeCacheServicePass{settings: defaultCacheSettings(), newCommand: func() *exec.Cmd {
		return cacheServiceTestCommand("healthy", "URNETWORK_CACHE_TEST_CGROUP="+path)
	}}
	result := runner.Run(t.Context())
	if result.Err != nil || !result.Started || !result.Joined || result.Result == nil || result.Result.Status != "target_observed" {
		t.Fatal("native test-owned child did not positively join", result)
	}
	parent, err := os.ReadFile("/proc/self/cgroup")
	child, childErr := os.ReadFile(path)
	if err != nil || childErr != nil || !bytes.Equal(parent, child) {
		t.Fatal("pass child acquired another cgroup", err, childErr)
	}
}

// Malformed/flooded output and nonzero exits cannot become joined successes.
func TestCacheServiceNativeChildRefusesInvalidOutputAndFailure(t *testing.T) {
	for _, mode := range []string{"failed", "malformed", "trailing", "unknown", "flood"} {
		t.Run(mode, func(t *testing.T) {
			runner := &nativeCacheServicePass{settings: defaultCacheSettings(), newCommand: func() *exec.Cmd {
				return cacheServiceTestCommand(mode)
			}}
			result := runner.Run(t.Context())
			if result.Err == nil || !result.Started || !result.Joined {
				t.Fatal("invalid child became success or lost its real join", result)
			}
			if mode != "failed" && result.Result != nil {
				t.Fatal("malformed child output escaped the finite result vocabulary", mode)
			}
		})
	}
}

// Readiness handshake establishes ignored TERM before cancellation; only this
// exact test-owned process may receive the later hard kill and positive wait.
func TestCacheServiceNativeChildCancellationHardKillsAndJoins(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	ready := filepath.Join(t.TempDir(), "ready")
	settings := defaultCacheSettings()
	settings.PassTimeout, settings.StopTimeout = 10*time.Second, 2*time.Second
	command := cacheServiceTestCommand("ignore_term", "URNETWORK_CACHE_TEST_READY="+ready)
	runner := &nativeCacheServicePass{settings: settings, newCommand: func() *exec.Cmd { return command }}
	done := make(chan cacheServicePassOutcome, 1)
	go func() { done <- runner.Run(ctx) }()
	deadline := time.NewTimer(settings.PassTimeout)
	defer deadline.Stop()
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	for {
		if _, err := os.Stat(ready); err == nil {
			break
		}
		select {
		case <-ticker.C:
		case result := <-done:
			t.Fatal("test child exited before its readiness handshake", result)
		case <-deadline.C:
			t.Fatal("test child did not establish its private readiness handshake")
		}
	}
	cancel()
	result := <-done
	if !result.Started || !result.Joined || !errors.Is(result.Err, context.Canceled) || command.ProcessState == nil {
		t.Fatal("canceled native pass did not retain and join its exact child", result)
	}
	status, ok := command.ProcessState.Sys().(syscall.WaitStatus)
	if !ok || !status.Signaled() || status.Signal() != syscall.SIGKILL {
		t.Fatal("TERM-ignoring child was not positively hard-killed and joined")
	}
}
