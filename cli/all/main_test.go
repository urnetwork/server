// Command-boundary tests use synthetic configuration and injected runners;
// lifecycle barriers prove admission, cancellation and final drain ordering.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/api"
	connectserver "github.com/urnetwork/server/connect"
	"github.com/urnetwork/server/controller"
	"github.com/urnetwork/server/gossip"
	"github.com/urnetwork/server/router"
	"github.com/urnetwork/server/taskworker"
)

// No explicit command selects all production listeners and the full worker.
func TestParseCommandDefaults(t *testing.T) {
	for _, args := range [][]string{nil, {"run"}} {
		options, err := parseCommand(args)
		if err != nil {
			t.Fatalf("parse %v: %v", args, err)
		}
		want := commandOptions{command: "run", apiPort: 8080, connectPort: 8081, taskworkerPort: 8082, workerCount: 8, workerBatchSize: 4, privateHeapTarget: "config"}
		if options != want {
			t.Errorf("parse %v = %+v, want %+v", args, options, want)
		}
	}
}

// Explicit modes and runtime knobs are parsed without loading Warp resources.
func TestParseCommandModesAndFlags(t *testing.T) {
	for _, c := range []struct {
		args    []string
		command string
	}{
		{args: []string{"--help"}, command: "help"},
		{args: []string{"-h"}, command: "help"},
		{args: []string{"--version"}, command: "version"},
		{args: []string{"init-tasks"}, command: "init-tasks"},
		{args: []string{"db", "migrate"}, command: "migrate"},
	} {
		options, err := parseCommand(c.args)
		if err != nil || options.command != c.command {
			t.Errorf("parse %v = %+v, %v", c.args, options, err)
		}
	}
	options, err := parseCommand([]string{"run", "--api-port=18080", "--connect-port=18081", "--taskworker-port=18082", "--gossip-port=18083", "--worker-count=12", "--worker-batch-size=6", "--require-subnet", "--memory-owner-ledger", "--private-heap-profile-target=disabled"})
	if err != nil {
		t.Fatal(err)
	}
	want := commandOptions{command: "run", apiPort: 18080, connectPort: 18081, taskworkerPort: 18082, gossipPort: 18083, workerCount: 12, workerBatchSize: 6, requireSubnet: true, memoryOwnerLedger: true, privateHeapTarget: "disabled"}
	if options != want {
		t.Errorf("runtime options = %+v, want %+v", options, want)
	}
}

// Invalid commands, ranges and collisions cannot reach startup side effects.
func TestParseCommandRejectsInvalidArguments(t *testing.T) {
	for _, args := range [][]string{
		{"unknown"}, {"db"}, {"db", "unknown"}, {"run", "extra"}, {"init-tasks", "extra"},
		{"--unknown"}, {"--api-port=0"}, {"--connect-port=65536"}, {"--taskworker-port=-1"},
		{"--worker-count=0"}, {"--worker-count=1025"}, {"--worker-batch-size=0"}, {"--worker-batch-size=1025"},
		{"--gossip-port=-1"}, {"--gossip-port=65536"}, {"--connect-port=8080"}, {"--gossip-port=8082"},
		{"db", "migrate", "extra"}, {"db", "migrate", "--worker-count=8"},
	} {
		if _, err := parseCommand(args); err == nil {
			t.Errorf("accepted invalid arguments %v", args)
		}
	}
}

// A present empty pin must not silently select the ordinary migration path.
func TestParseCommandSchedulePin(t *testing.T) {
	pin := strings.Repeat("ab", 32)
	options, err := parseCommand([]string{"db", "migrate", "--sn-schedule-sha256=" + pin})
	if err != nil || options.command != "migrate" || options.scheduleSha256 != pin {
		t.Fatalf("selected schedule = %+v, %v", options, err)
	}
	for _, pin := range []string{"", strings.Repeat("0", 64), strings.Repeat("AB", 32), strings.Repeat("a", 63), strings.Repeat("g", 64)} {
		if _, err := parseCommand([]string{"db", "migrate", "--sn-schedule-sha256=" + pin}); err == nil {
			t.Errorf("accepted invalid selected schedule %q", pin)
		}
	}
}

// Fixtures never consult process configuration or bind the synthetic address.
func operatorTestEnvironment() map[string]string {
	return map[string]string{
		"WARP_ENV": "test", "WARP_HOST": "operator-test", "WARP_SERVICE": "all",
		"WARP_BLOCK": "test-block", "WARP_DOMAIN": "operator.example", "WARP_VERSION": "test-version",
		"WARP_HOST_IPV4": "192.0.2.10", "WARP_PORTS": "8080:18080,8081:18081,8082:18082,5080:15080",
	}
}

// TCP and UDP may share a mapped port; optional UDP listeners stay optional.
func TestValidateEnvironmentAcceptsProtocolOverlap(t *testing.T) {
	for _, c := range []struct {
		apiPort int
		ports   string
	}{
		{apiPort: 8080, ports: "8080:18080,8081:18081,8082:18082,5080:15080"},
		{apiPort: 8080, ports: "8080:18080,8081:18081,8082:18082,5080:15080,5081:15081"},
		{apiPort: 8080, ports: "8080:18080,8081:18081,8082:18082,5080:15080,443:18080,4053:18081,8053:18082"},
		{apiPort: 443, ports: "443:18080,8081:18081,8082:18082,5080:15080"},
	} {
		environment := operatorTestEnvironment()
		environment["WARP_PORTS"] = c.ports
		options := commandOptions{apiPort: c.apiPort, connectPort: 8081, taskworkerPort: 8082}
		if err := validateEnvironment(options, func(name string) string { return environment[name] }); err != nil {
			t.Errorf("valid mapping %q, api port %d: %v", c.ports, c.apiPort, err)
		}
	}
}

// Every Warp identity component and a usable IPv4 value are required.
func TestValidateEnvironmentRequiresIdentity(t *testing.T) {
	options := commandOptions{apiPort: 8080, connectPort: 8081, taskworkerPort: 8082}
	for _, name := range []string{"WARP_ENV", "WARP_HOST", "WARP_SERVICE", "WARP_BLOCK", "WARP_DOMAIN", "WARP_VERSION"} {
		environment := operatorTestEnvironment()
		environment[name] = " \t"
		err := validateEnvironment(options, func(name string) string { return environment[name] })
		if err == nil || !strings.Contains(err.Error(), name) {
			t.Errorf("missing %s: %v", name, err)
		}
	}
	for _, ip := range []string{"", "operator.example", "2001:db8::1", "192.0.2.256"} {
		environment := operatorTestEnvironment()
		environment["WARP_HOST_IPV4"] = ip
		if err := validateEnvironment(options, func(name string) string { return environment[name] }); err == nil {
			t.Errorf("accepted invalid IPv4 %q", ip)
		}
	}
}

// Source collisions, host collisions and a missing exchange mapping fail early.
func TestValidateEnvironmentRejectsPortMappings(t *testing.T) {
	for _, c := range []struct {
		ports      string
		apiPort    int
		gossipPort int
		want       string
	}{
		{ports: "", apiPort: 8080, want: "pairs"},
		{ports: "8080:0", apiPort: 8080, want: "integers"},
		{ports: "65536:18080", apiPort: 8080, want: "integers"},
		{ports: "8080:abc", apiPort: 8080, want: "integers"},
		{ports: "8080:18080:28080", apiPort: 8080, want: "pairs"},
		{ports: "8080:18080,8080:28080", apiPort: 8080, want: "repeats service port 8080"},
		{ports: "8080:18080,8081:18081,8082:18082", apiPort: 8080, want: "5080"},
		{ports: "8080:18080,8081:18080,8082:18082,5080:15080", apiPort: 8080, want: "host port 18080"},
		{ports: "8080:18080,8081:18081,8082:18082,5080:18080", apiPort: 8080, want: "host port 18080"},
		{ports: "8080:18080,8081:18081,8082:18082,5080:15080,5081:18081", apiPort: 8080, want: "host port 18081"},
		{ports: "5080:15080,8081:18081,8082:18082", apiPort: 5080, want: "host port 15080"},
		{ports: "8080:18080,8081:18081,8082:18082,5080:15080", apiPort: 8080, gossipPort: 8083, want: "8083"},
		{ports: "8080:18080,8081:18081,8082:18082,5080:15080,8083:18081", apiPort: 8080, gossipPort: 8083, want: "host port 18081"},
		{ports: "8080:18080,8081:18081,8082:18082,5080:15080,443:15443,4053:15443", apiPort: 8080, want: "host port 15443"},
	} {
		environment := operatorTestEnvironment()
		environment["WARP_PORTS"] = c.ports
		options := commandOptions{apiPort: c.apiPort, connectPort: 8081, taskworkerPort: 8082, gossipPort: c.gossipPort}
		err := validateEnvironment(options, func(name string) string { return environment[name] })
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("mapping %q with %+v: %v, want %q", c.ports, options, err, c.want)
		}
	}
}

// Composition preserves production task coverage, selected knobs, admission
// injection and independent readiness for every service and every process.
func TestOperatorServicesUseProductionRunners(t *testing.T) {
	for _, target := range []string{"config", "disabled", "operator-test/test-block"} {
		var apiOptions api.RunOptions
		var connectOptions connectserver.RunOptions
		var workerOptions taskworker.RunOptions
		var gossipOptions gossip.RunOptions
		var called []string
		runners := serviceRunners{
			api: func(ctx context.Context, options api.RunOptions) error {
				apiOptions = options
				called = append(called, "api")
				return nil
			},
			connect: func(ctx context.Context, options connectserver.RunOptions) error {
				connectOptions = options
				called = append(called, "connect")
				return nil
			},
			taskworker: func(ctx context.Context, options taskworker.RunOptions) error {
				workerOptions = options
				called = append(called, "taskworker")
				return nil
			},
			gossip: func(ctx context.Context, options gossip.RunOptions) error {
				gossipOptions = options
				called = append(called, "gossip")
				return nil
			},
		}
		options := commandOptions{apiPort: 18080, connectPort: 18081, taskworkerPort: 18082, gossipPort: 18083, workerCount: 12, workerBatchSize: 6, memoryOwnerLedger: true, privateHeapTarget: target}
		services := operatorServices(options, runners)
		if len(services) != 4 {
			t.Fatalf("service count = %d", len(services))
		}
		ctx := context.Background()
		startCount := 0
		flushCount := 0
		start := func(got context.Context) func() {
			if got != ctx {
				t.Error("admission context changed")
			}
			startCount++
			return func() { flushCount++ }
		}
		for _, service := range services {
			if err := service.run(ctx, start); err != nil {
				t.Fatal(err)
			}
		}
		if !slices.Equal(called, []string{"api", "connect", "taskworker", "gossip"}) {
			t.Errorf("runner calls = %v", called)
		}
		if apiOptions.Port != 18080 || connectOptions.Port != 18081 || workerOptions.Port != 18082 || gossipOptions.Port != 18083 || workerOptions.Count != 12 || workerOptions.BatchSize != 6 {
			t.Errorf("lost configured ports or worker bounds: %+v %+v %+v %+v", apiOptions, connectOptions, workerOptions, gossipOptions)
		}
		if workerOptions.WorkloadProfile != taskworker.WorkloadProfileProduction {
			t.Errorf("worker profile = %q", workerOptions.WorkloadProfile)
		}
		wantTarget := target
		if target == "config" {
			wantTarget = ""
		}
		if !connectOptions.MemoryOwnerLedger || connectOptions.PrivateHeapProfileTarget != wantTarget || connectOptions.DirectH3LoopbackMode {
			t.Errorf("connect runtime options = %+v", connectOptions)
		}
		for _, start := range []func(context.Context) func(){apiOptions.StartStatsPusher, connectOptions.StartStatsPusher, workerOptions.StartStatsPusher, gossipOptions.StartStatsPusher} {
			if start == nil {
				t.Fatal("runner did not receive admission callback")
			}
			start(ctx)()
		}
		if startCount != 4 || flushCount != 4 {
			t.Errorf("admission callbacks = %d, %d", startCount, flushCount)
		}
		statuses := []*router.WarpStatusState{apiOptions.WarpStatus, connectOptions.WarpStatus, workerOptions.WarpStatus}
		for i, status := range statuses {
			if status == nil {
				t.Fatal("nil composed readiness state")
			}
			if status.Status() == "ok" {
				t.Errorf("service %d is initially ready", i)
			}
			for j := 0; j < i; j++ {
				if status == statuses[j] {
					t.Errorf("services %d and %d share status", i, j)
				}
			}
		}
		statuses[0].SetReady()
		statuses[1].SetNotReady(errors.New("synthetic startup failure"))
		statuses[2].SetReady()
		statuses[2].SetDrainingIfReady()
		if statuses[0].Status() != "ok" || !strings.Contains(statuses[1].Status(), "synthetic startup failure") || statuses[2].Status() != "draining" {
			t.Errorf("service statuses crossed: %q, %q, %q", statuses[0].Status(), statuses[1].Status(), statuses[2].Status())
		}
		options.gossipPort = 0
		called = nil
		services = operatorServices(options, runners)
		for _, service := range services {
			if err := service.run(ctx, start); err != nil {
				t.Fatal(err)
			}
		}
		if len(services) != 3 || !slices.Equal(called, []string{"api", "connect", "taskworker"}) {
			t.Errorf("disabled gossip services = %v", called)
		}
		if apiOptions.WarpStatus == statuses[0] || connectOptions.WarpStatus == statuses[1] || workerOptions.WarpStatus == statuses[2] {
			t.Error("separate compositions share readiness")
		}
	}
}

// Missing ownership inputs and pre-canceled contexts start no runner.
func TestSuperviseRejectsInvalidInputs(t *testing.T) {
	called := false
	services := []operatorService{{name: "synthetic", run: func(context.Context, func(context.Context) func()) error { called = true; return nil }}}
	start := func(context.Context) func() { called = true; return func() {} }
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	for _, c := range []struct {
		ctx      context.Context
		services []operatorService
		start    func(context.Context) func()
	}{
		{ctx: nil, services: services, start: start},
		{ctx: context.Background(), services: nil, start: start},
		{ctx: context.Background(), services: services, start: nil},
		{ctx: ctx, services: services, start: start},
	} {
		if err := supervise(c.ctx, c.services, c.start); err == nil {
			t.Error("accepted invalid supervisor inputs")
		}
	}
	if called {
		t.Error("invalid supervisor launched runtime")
	}
}

// Force one admitted runner to terminate, then hold its canceled sibling at
// a drain barrier until the supervisor is observably waiting for that sibling.
func checkOperatorRunnerExit(t *testing.T, exit func(context.Context) error, want string, cause error) {
	t.Helper()
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		ready := make(chan struct{}, 2)
		releaseExit := make(chan struct{})
		releaseDrain := make(chan struct{})
		var drainRelease sync.Once
		defer drainRelease.Do(func() { close(releaseDrain) })
		canceled := make(chan struct{})
		completed := make(chan error, 1)
		var drained atomic.Bool
		var started atomic.Int32
		var flushed atomic.Int32
		var metricsCtx context.Context
		services := []operatorService{
			{name: "terminal", run: func(ctx context.Context, start func(context.Context) func()) error {
				start(ctx)()
				ready <- struct{}{}
				<-releaseExit
				return exit(ctx)
			}},
			{name: "sibling", run: func(ctx context.Context, start func(context.Context) func()) error {
				start(ctx)()
				ready <- struct{}{}
				<-ctx.Done()
				close(canceled)
				<-releaseDrain
				drained.Store(true)
				return ctx.Err()
			}},
		}
		go func() {
			completed <- supervise(ctx, services, func(ctx context.Context) func() {
				metricsCtx = ctx
				started.Add(1)
				return func() {
					flushed.Add(1)
					if !drained.Load() {
						t.Error("metrics flushed before sibling drain joined")
					}
					if ctx.Err() == nil {
						t.Error("metrics flush preceded metrics cancellation")
					}
				}
			})
		}()
		<-ready
		<-ready
		close(releaseExit)
		<-canceled
		synctest.Wait()
		select {
		case err := <-completed:
			t.Fatalf("supervisor returned before sibling drain: %v", err)
		default:
		}
		if started.Load() != 1 || flushed.Load() != 0 || metricsCtx.Err() != nil {
			t.Error("metrics lifetime did not cover sibling drain")
		}
		drainRelease.Do(func() { close(releaseDrain) })
		err := <-completed
		if err == nil || !strings.Contains(err.Error(), "terminal") || !strings.Contains(err.Error(), want) {
			t.Errorf("terminal error = %v, want %q", err, want)
		}
		if cause != nil && !errors.Is(err, cause) {
			t.Errorf("terminal error lost cause %v: %v", cause, err)
		}
		if !drained.Load() || flushed.Load() != 1 {
			t.Errorf("drained = %v, final flushes = %d", drained.Load(), flushed.Load())
		}
	})
}

// Operational failure cancels peers and preserves its original cause.
func TestSuperviseFailedRunnerJoinsSibling(t *testing.T) {
	cause := errors.New("synthetic listener failed")
	checkOperatorRunnerExit(t, func(context.Context) error { return cause }, cause.Error(), cause)
}

// A child cancellation before operator shutdown is still an unexpected exit.
func TestSuperviseCanceledRunnerJoinsSibling(t *testing.T) {
	checkOperatorRunnerExit(t, func(context.Context) error { return context.Canceled }, "context canceled", context.Canceled)
}

// A clean return from a live daemon must not leave its siblings serving.
func TestSuperviseNilReturnJoinsSibling(t *testing.T) {
	checkOperatorRunnerExit(t, func(context.Context) error { return nil }, "stopped before operator shutdown", nil)
}

// Panic recovery reports the crash while draining the other services.
func TestSupervisePanickedRunnerJoinsSibling(t *testing.T) {
	cause := errors.New("synthetic runtime panic")
	checkOperatorRunnerExit(t, func(context.Context) error { panic(cause) }, cause.Error(), nil)
}

// A missing runner is recovered before admission; its sibling is still joined.
func TestSuperviseNilRunnerJoinsSibling(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		release := make(chan struct{})
		var released sync.Once
		defer released.Do(func() { close(release) })
		canceled := make(chan struct{})
		completed := make(chan error, 1)
		var drained atomic.Bool
		var started atomic.Int32
		services := []operatorService{
			{name: "missing"},
			{name: "sibling", run: func(ctx context.Context, start func(context.Context) func()) error {
				start(ctx)()
				<-ctx.Done()
				close(canceled)
				<-release
				drained.Store(true)
				return ctx.Err()
			}},
		}
		go func() {
			completed <- supervise(context.Background(), services, func(context.Context) func() { started.Add(1); return func() {} })
		}()
		<-canceled
		synctest.Wait()
		select {
		case err := <-completed:
			t.Fatalf("returned before canceled sibling drained: %v", err)
		default:
		}
		released.Do(func() { close(release) })
		err := <-completed
		if err == nil || !strings.Contains(err.Error(), "missing") || !strings.Contains(err.Error(), "panic") {
			t.Errorf("nil runner error = %v", err)
		}
		if !drained.Load() || started.Load() != 0 {
			t.Errorf("drained = %v, metrics starts = %d", drained.Load(), started.Load())
		}
	})
}

// Duplicate admissions cannot substitute for another service. Process metrics
// remain active through cancellation and flush only after both explicit drains.
func TestSuperviseMetricsWaitForEveryAdmissionAndJoin(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		firstReady := make(chan struct{})
		secondAdmission := make(chan struct{})
		secondReady := make(chan struct{})
		releaseDrain := make(chan struct{})
		var released sync.Once
		defer released.Do(func() { close(releaseDrain) })
		canceled := make(chan struct{}, 2)
		completed := make(chan error, 1)
		var drained atomic.Int32
		var starts atomic.Int32
		var flushes atomic.Int32
		var metricsCtx context.Context
		services := []operatorService{
			{name: "first", run: func(ctx context.Context, start func(context.Context) func()) error {
				start(ctx)()
				start(ctx)()
				close(firstReady)
				<-ctx.Done()
				canceled <- struct{}{}
				<-releaseDrain
				drained.Add(1)
				return ctx.Err()
			}},
			{name: "second", run: func(ctx context.Context, start func(context.Context) func()) error {
				<-secondAdmission
				start(ctx)()
				close(secondReady)
				<-ctx.Done()
				canceled <- struct{}{}
				<-releaseDrain
				drained.Add(1)
				return nil
			}},
		}
		go func() {
			completed <- supervise(ctx, services, func(ctx context.Context) func() {
				metricsCtx = ctx
				starts.Add(1)
				return func() {
					flushes.Add(1)
					if drained.Load() != 2 || ctx.Err() == nil {
						t.Error("final flush preceded joined drains and metrics cancellation")
					}
				}
			})
		}()
		<-firstReady
		synctest.Wait()
		if starts.Load() != 0 || flushes.Load() != 0 {
			t.Error("duplicate first admission started process metrics early")
		}
		close(secondAdmission)
		<-secondReady
		synctest.Wait()
		if starts.Load() != 1 || flushes.Load() != 0 {
			t.Fatal("admitted process does not have exactly one metrics owner")
		}
		cancel()
		<-canceled
		<-canceled
		synctest.Wait()
		select {
		case err := <-completed:
			t.Fatalf("operator returned before both drains: %v", err)
		default:
		}
		if metricsCtx.Err() != nil || flushes.Load() != 0 {
			t.Error("metrics ended before service drains")
		}
		released.Do(func() { close(releaseDrain) })
		if err := <-completed; err != nil {
			t.Errorf("parent cancellation returned operational error: %v", err)
		}
		if starts.Load() != 1 || flushes.Load() != 1 || drained.Load() != 2 {
			t.Errorf("starts=%d flushes=%d drains=%d", starts.Load(), flushes.Load(), drained.Load())
		}
	})
}

// A service finishing readiness after process cancellation cannot start metrics.
func TestSuperviseLateAdmissionDoesNotStartMetrics(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		ready := make(chan struct{})
		lateAdmission := make(chan struct{})
		completed := make(chan error, 1)
		var starts atomic.Int32
		services := []operatorService{
			{name: "ready", run: func(ctx context.Context, start func(context.Context) func()) error {
				start(ctx)()
				close(ready)
				<-ctx.Done()
				return ctx.Err()
			}},
			{name: "late", run: func(ctx context.Context, start func(context.Context) func()) error {
				<-lateAdmission
				start(ctx)()
				return ctx.Err()
			}},
		}
		go func() {
			completed <- supervise(ctx, services, func(context.Context) func() { starts.Add(1); return func() {} })
		}()
		<-ready
		cancel()
		synctest.Wait()
		close(lateAdmission)
		if err := <-completed; err != nil {
			t.Error(err)
		}
		if starts.Load() != 0 {
			t.Error("canceled admission started metrics")
		}
	})
}

// Cancellation after the final admission but during metrics startup still
// joins that startup, then the runners, then the returned final flush.
func TestSuperviseCancellationDuringMetricsStart(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		started := make(chan struct{})
		releaseStart := make(chan struct{})
		var released sync.Once
		defer released.Do(func() { close(releaseStart) })
		completed := make(chan error, 1)
		var drains atomic.Int32
		var flushes atomic.Int32
		run := func(ctx context.Context, start func(context.Context) func()) error {
			start(ctx)()
			<-ctx.Done()
			drains.Add(1)
			return ctx.Err()
		}
		go func() {
			completed <- supervise(ctx, []operatorService{{name: "first", run: run}, {name: "second", run: run}}, func(ctx context.Context) func() {
				close(started)
				<-releaseStart
				return func() {
					flushes.Add(1)
					if drains.Load() != 2 || ctx.Err() == nil {
						t.Error("late metrics startup was not joined and flushed after service drains")
					}
				}
			})
		}()
		<-started
		cancel()
		synctest.Wait()
		select {
		case err := <-completed:
			t.Fatalf("returned with metrics startup still blocked: %v", err)
		default:
		}
		if drains.Load() != 1 || flushes.Load() != 0 {
			t.Errorf("before metrics release: drains=%d flushes=%d", drains.Load(), flushes.Load())
		}
		released.Do(func() { close(releaseStart) })
		if err := <-completed; err != nil {
			t.Error(err)
		}
		if drains.Load() != 2 || flushes.Load() != 1 {
			t.Errorf("after metrics release: drains=%d flushes=%d", drains.Load(), flushes.Load())
		}
	})
}

// Joining cancellation with a real shutdown fault must retain the fault.
func TestSupervisePreservesFailureJoinedWithCancellation(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		ready := make(chan struct{})
		completed := make(chan error, 1)
		cause := errors.New("synthetic final handback failure")
		services := []operatorService{{name: "worker", run: func(ctx context.Context, start func(context.Context) func()) error {
			start(ctx)()
			close(ready)
			<-ctx.Done()
			return errors.Join(ctx.Err(), cause)
		}}}
		go func() { completed <- supervise(ctx, services, func(context.Context) func() { return func() {} }) }()
		<-ready
		cancel()
		err := <-completed
		if !errors.Is(err, cause) || !errors.Is(err, context.Canceled) || cancellationOnly(err) {
			t.Errorf("joined drain failure was suppressed: %v", err)
		}
	})
}

// A panic remains an operational failure even when its value is cancellation.
func TestSupervisePanicDuringCancellationIsFailure(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		ready := make(chan struct{})
		completed := make(chan error, 1)
		services := []operatorService{{name: "panic-on-drain", run: func(ctx context.Context, start func(context.Context) func()) error {
			start(ctx)()
			close(ready)
			<-ctx.Done()
			panic(context.Canceled)
		}}}
		go func() { completed <- supervise(ctx, services, func(context.Context) func() { return func() {} }) }()
		<-ready
		cancel()
		err := <-completed
		if err == nil || !strings.Contains(err.Error(), "panic") || cancellationOnly(err) {
			t.Errorf("panic during cancellation was suppressed: %v", err)
		}
	})
}

// Error classification accepts only cancellation throughout an unwrap tree.
func TestCancellationOnlyRetainsOperationalCauses(t *testing.T) {
	cause := errors.New("synthetic operational failure")
	for _, err := range []error{context.Canceled, fmt.Errorf("wrapped: %w", context.Canceled), errors.Join(context.Canceled, fmt.Errorf("wrapped: %w", context.Canceled))} {
		if !cancellationOnly(err) {
			t.Errorf("pure cancellation rejected: %v", err)
		}
	}
	for _, err := range []error{nil, cause, context.DeadlineExceeded, errors.Join(context.Canceled, cause), fmt.Errorf("wrapped: %w", errors.Join(context.Canceled, cause))} {
		if cancellationOnly(err) {
			t.Errorf("operational cause classified as cancellation: %v", err)
		}
	}
}

// Selected migration must compare the reviewed digest before schema mutation
// and prepare against that same digest after schema mutation succeeds.
func TestMigrateDatabaseSelectedScheduleOrdering(t *testing.T) {
	pin := strings.Repeat("ab", 32)
	boundary := &server.ProviderEarningBoundary{IdentitySha256: strings.Repeat("cd", 32), InitialConfigSha256: pin}
	var events []string
	var output bytes.Buffer
	err := migrateDatabase(context.Background(), pin, &output,
		func(context.Context) { events = append(events, "migrate") },
		func(context.Context) (*server.ProviderPayoutTransition, error) {
			events = append(events, "load")
			return &server.ProviderPayoutTransition{ConfigSha256: pin}, nil
		},
		func(ctx context.Context, selected string) (*server.ProviderEarningBoundary, error) {
			events = append(events, "prepare")
			if selected != pin || !slices.Equal(events, []string{"load", "migrate", "prepare"}) {
				t.Errorf("prepare selection/order: %q %v", selected, events)
			}
			return boundary, nil
		})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(events, []string{"load", "migrate", "prepare"}) {
		t.Errorf("migration order = %v", events)
	}
	var result struct {
		Boundary                 *server.ProviderEarningBoundary `json:"earning_boundary"`
		DeploymentVerified       bool                            `json:"deployment_verified"`
		ChainReadinessAuthorized bool                            `json:"chain_readiness_authorized"`
	}
	if err := json.Unmarshal(output.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.Boundary == nil || result.Boundary.InitialConfigSha256 != pin || result.Boundary.IdentitySha256 != boundary.IdentitySha256 || result.DeploymentVerified || result.ChainReadinessAuthorized {
		t.Errorf("migration report = %+v", result)
	}
}

// A pin mismatch or unavailable schedule never permits the first schema write.
func TestMigrateDatabaseRejectsUnreviewedScheduleBeforeMutation(t *testing.T) {
	pin := strings.Repeat("ab", 32)
	loadErr := errors.New("synthetic schedule unreadable")
	for _, c := range []struct {
		policy *server.ProviderPayoutTransition
		err    error
	}{
		{policy: nil},
		{policy: &server.ProviderPayoutTransition{ConfigSha256: strings.Repeat("cd", 32)}},
		{err: loadErr},
	} {
		var output bytes.Buffer
		mutated := false
		prepared := false
		err := migrateDatabase(context.Background(), pin, &output,
			func(context.Context) { mutated = true },
			func(context.Context) (*server.ProviderPayoutTransition, error) { return c.policy, c.err },
			func(context.Context, string) (*server.ProviderEarningBoundary, error) {
				prepared = true
				return nil, nil
			})
		if err == nil || mutated || prepared || output.Len() != 0 {
			t.Errorf("unreviewed migration: err=%v mutated=%v prepared=%v output=%q", err, mutated, prepared, output.String())
		}
		if c.err != nil && !errors.Is(err, c.err) {
			t.Errorf("schedule read lost cause: %v", err)
		}
	}
}

// The ordinary migration path does not imply selection or boundary readiness.
func TestMigrateDatabaseWithoutSelection(t *testing.T) {
	var output bytes.Buffer
	migrations := 0
	err := migrateDatabase(context.Background(), "", &output,
		func(context.Context) { migrations++ },
		func(context.Context) (*server.ProviderPayoutTransition, error) {
			t.Error("unselected migration loaded schedule")
			return nil, nil
		},
		func(context.Context, string) (*server.ProviderEarningBoundary, error) {
			t.Error("unselected migration prepared boundary")
			return nil, nil
		})
	if err != nil || migrations != 1 || output.Len() != 0 {
		t.Errorf("ordinary migration: err=%v migrations=%d output=%q", err, migrations, output.String())
	}
}

// The post-migration exact-byte check can still fail and must emit no receipt.
func TestMigrateDatabasePropagatesPreparationFailure(t *testing.T) {
	pin := strings.Repeat("ab", 32)
	cause := errors.New("synthetic schedule changed during migration")
	var output bytes.Buffer
	migrated := false
	err := migrateDatabase(context.Background(), pin, &output,
		func(context.Context) { migrated = true },
		func(context.Context) (*server.ProviderPayoutTransition, error) {
			return &server.ProviderPayoutTransition{ConfigSha256: pin}, nil
		},
		func(context.Context, string) (*server.ProviderEarningBoundary, error) {
			if !migrated {
				t.Error("prepared before schema migration")
			}
			return nil, cause
		})
	if !errors.Is(err, cause) || !migrated || output.Len() != 0 {
		t.Errorf("preparation failure: err=%v migrated=%v output=%q", err, migrated, output.String())
	}
}

// Cancellation at every external phase prevents subsequent migration effects.
func TestMigrateDatabaseCancellationGates(t *testing.T) {
	pin := strings.Repeat("ab", 32)
	for _, cancelAt := range []string{"before-load", "after-load", "after-migrate"} {
		ctx, cancel := context.WithCancel(context.Background())
		var events []string
		var output bytes.Buffer
		if cancelAt == "before-load" {
			cancel()
		}
		err := migrateDatabase(ctx, pin, &output,
			func(context.Context) {
				events = append(events, "migrate")
				if cancelAt == "after-migrate" {
					cancel()
				}
			},
			func(context.Context) (*server.ProviderPayoutTransition, error) {
				events = append(events, "load")
				if cancelAt == "after-load" {
					cancel()
				}
				return &server.ProviderPayoutTransition{ConfigSha256: pin}, nil
			},
			func(context.Context, string) (*server.ProviderEarningBoundary, error) {
				events = append(events, "prepare")
				return nil, nil
			})
		cancel()
		want := map[string][]string{"before-load": nil, "after-load": {"load"}, "after-migrate": {"load", "migrate"}}
		if !errors.Is(err, context.Canceled) || !slices.Equal(events, want[cancelAt]) || output.Len() != 0 {
			t.Errorf("cancel %s: err=%v events=%v output=%q", cancelAt, err, events, output.String())
		}
	}
}

// Invalid direct inputs cannot reach even the injected schedule loader.
func TestMigrateDatabaseRejectsInvalidInputs(t *testing.T) {
	called := false
	for _, c := range []struct {
		ctx context.Context
		pin string
	}{
		{ctx: nil, pin: strings.Repeat("ab", 32)},
		{ctx: context.Background(), pin: "invalid"},
		{ctx: context.Background(), pin: strings.Repeat("0", 64)},
	} {
		err := migrateDatabase(c.ctx, c.pin, io.Discard,
			func(context.Context) { called = true },
			func(context.Context) (*server.ProviderPayoutTransition, error) { called = true; return nil, nil },
			func(context.Context, string) (*server.ProviderEarningBoundary, error) { called = true; return nil, nil })
		if err == nil {
			t.Errorf("accepted invalid migration input %+v", c)
		}
	}
	if called {
		t.Error("invalid migration reached side effect")
	}
}

// Feed-only trust configuration requires no gossip listener; advertised peer
// identity does, and unreadable configuration is distinct from absent config.
func TestValidateGossipModesAndResourceErrors(t *testing.T) {
	feed := &controller.ExtenderConfig{NetworkHost: "operator.example", RootPublicKeysHex: []string{strings.Repeat("ab", 32)}}
	identity := &controller.ExtenderConfig{NetworkHost: "operator.example", GossipIdentityKeyHex: strings.Repeat("cd", 32)}
	for _, c := range []struct {
		port   int
		config *controller.ExtenderConfig
		err    error
	}{
		{port: 0, config: feed},
		{port: 0, config: &controller.ExtenderConfig{GossipIdentityKeyHex: " \t"}},
		{port: 18083, config: identity},
		{port: 0, err: fmt.Errorf("synthetic absent resource: %w", server.ErrResourceNotFound)},
	} {
		if err := validateGossip(c.port, c.config, c.err); err != nil {
			t.Errorf("valid gossip mode %+v: %v", c, err)
		}
	}
	if err := validateGossip(0, identity, nil); err == nil || !strings.Contains(err.Error(), "gossip-port") {
		t.Errorf("advertised identity without listener: %v", err)
	}
	if err := validateGossip(18083, nil, server.ErrResourceNotFound); !errors.Is(err, server.ErrResourceNotFound) {
		t.Errorf("enabled gossip with absent config: %v", err)
	}
	if err := validateGossip(0, nil, nil); err == nil {
		t.Error("accepted nil config without resource error")
	}
	unreadable := errors.New("synthetic resource unreadable")
	for _, port := range []int{0, 18083} {
		if err := validateGossip(port, nil, unreadable); !errors.Is(err, unreadable) {
			t.Errorf("unreadable resource on port %d: %v", port, err)
		}
	}
}
