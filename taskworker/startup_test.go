// Startup regressions use the real readiness/ownership orchestration with
// explicit barriers. No test waits for a database outage or a wall-clock retry.
package taskworker

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/router"
)

// Synthetic process identity permits the ordinary status and capture setup.
func taskworkerStartupTestEnvironment(t *testing.T) {
	t.Helper()
	for key, value := range map[string]string{
		"WARP_ENV": "test", "WARP_VERSION": "2026.10.4+1",
		"WARP_HOST": "operator-startup.example", "WARP_SERVICE": "taskworker", "WARP_BLOCK": "g1",
		"WARP_HOST_IPV4": "192.0.2.10", "WARP_HOST_IPV6": "", "WARP_PORTS": "8080:8080",
	} {
		t.Setenv(key, value)
	}
	t.Cleanup(router.SetWarpStatusReady)
}

// The production constructor must inspect admission before loading scheduling
// configuration or touching a queue, even when its runtime lifetime is live.
func TestTaskworkerRuntimeStoppedAdmissionCannotInitialize(t *testing.T) {
	for _, deadline := range []bool{false, true} {
		func() {
			admission, cancel := context.WithCancel(t.Context())
			cancel()
			want := context.Canceled
			if deadline {
				admission, cancel = context.WithDeadline(t.Context(), time.Unix(1, 0))
				defer cancel()
				want = context.DeadlineExceeded
			}
			stops := 0
			worker, err := startTaskworkerRuntime(admission, t.Context(), func() { stops++ }, RunOptions{Port: 8080, Count: 1, BatchSize: 1, WorkloadProfile: WorkloadProfileSubnetOperator})
			if worker != nil || !errors.Is(err, want) || stops != 0 {
				t.Fatalf("stopped admission initialized or stopped a lifetime: deadline=%t err=%v stops=%d", deadline, err, stops)
			}
		}()
	}
}

// A finite failed read is followed by a fresh dependency observation. Runtime
// and metrics ownership are acquired once after real startup scheduling passes.
func TestTaskworkerStartupRetriesUnavailableReadAndInitialization(t *testing.T) {
	taskworkerStartupTestEnvironment(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	readUnavailable := &net.OpError{Op: "dial", Net: "tcp", Err: syscall.ECONNREFUSED}
	initializeUnavailable := &net.OpError{Op: "read", Net: "tcp", Err: syscall.ECONNRESET}
	reads, initializations, waits, metrics := 0, 0, 0, 0
	runtime := &taskworkerLifecycleRuntime{drainStarted: make(chan struct{}), handbackDone: make(chan struct{})}
	result := startTaskworkerAfterReadiness(ctx, ctx, cancel, RunOptions{Port: 8080, Count: 1, BatchSize: 1, WorkloadProfile: WorkloadProfileSubnetOperator},
		func(attempt context.Context) error {
			reads++
			deadline, bounded := attempt.Deadline()
			if !bounded || time.Until(deadline) < 295*time.Second || time.Until(deadline) > taskworkerStartupReadTimeout {
				t.Error("startup read lost its default 300-second operation budget")
			}
			if reads == 1 {
				return readUnavailable
			}
			return nil
		},
		func(context.Context) func() { metrics++; return func() {} },
		func(admission context.Context, lifetime context.Context, _ context.CancelFunc, options RunOptions) (taskworkerRuntime, error) {
			initializations++
			if options.WorkloadProfile != WorkloadProfileSubnetOperator || admission.Err() != nil || lifetime != ctx {
				t.Fatal("startup retry changed workload or ownership context")
			}
			if initializations == 1 {
				return nil, initializeUnavailable
			}
			return runtime, nil
		},
		func(owner context.Context, delay time.Duration) error {
			waits++
			if owner != ctx || delay != taskworkerStartupRetryDelay || metrics != 0 {
				t.Fatal("startup wait changed ownership or published a premature cohort")
			}
			return nil
		},
	)
	if result.closeCapture != nil {
		defer result.closeCapture()
	}
	if result.err != nil || result.worker != runtime || reads != 3 || initializations != 2 || waits != 2 || metrics != 1 {
		t.Fatalf("temporary startup dependency loss stranded or duplicated the runtime: err=%v reads=%d initializations=%d waits=%d metrics=%d", result.err, reads, initializations, waits, metrics)
	}
}

// A completed migration refusal cannot be hidden by another timeout, even
// while the process remains alive to serve its closed status.
func TestTaskworkerStartupKeepsMixedContradictionHard(t *testing.T) {
	taskworkerStartupTestEnvironment(t)
	hard := errors.New("synthetic required migration is absent")
	reads, starts, waits := 0, 0, 0
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	result := startTaskworkerAfterReadiness(ctx, ctx, cancel, RunOptions{Port: 8080, Count: 1, BatchSize: 1},
		func(context.Context) error { reads++; return errors.Join(context.DeadlineExceeded, hard) },
		func(context.Context) func() { starts++; return nil },
		func(context.Context, context.Context, context.CancelFunc, RunOptions) (taskworkerRuntime, error) {
			starts++
			return nil, nil
		},
		func(context.Context, time.Duration) error { waits++; return errors.New("synthetic unexpected retry") },
	)
	if !errors.Is(result.err, hard) || !errors.Is(result.err, context.DeadlineExceeded) || result.worker != nil || reads != 1 || starts != 0 || waits != 0 {
		t.Fatal("mixed startup contradiction acquired retry or runtime authority", result.err, reads, starts, waits)
	}
}

// The read failure remains visible when the caller stops at the retry boundary.
// No new worker, metric cohort or re-created operation is produced by stopping.
func TestTaskworkerStartupCancellationRetainsOriginalReadCause(t *testing.T) {
	taskworkerStartupTestEnvironment(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	original := &net.OpError{Op: "read", Net: "tcp", Err: syscall.ECONNRESET}
	reads, starts, waits := 0, 0, 0
	result := startTaskworkerAfterReadiness(ctx, ctx, cancel, RunOptions{Port: 8080, Count: 1, BatchSize: 1},
		func(context.Context) error { reads++; return original },
		func(context.Context) func() { starts++; return nil },
		func(context.Context, context.Context, context.CancelFunc, RunOptions) (taskworkerRuntime, error) {
			starts++
			return nil, nil
		},
		func(owner context.Context, _ time.Duration) error { waits++; cancel(); return owner.Err() },
	)
	if !errors.Is(result.err, context.Canceled) || !errors.Is(result.err, original) || result.worker != nil || reads != 1 || waits != 1 || starts != 0 {
		t.Fatal("canceled startup lost original read cause or acquired runtime authority", result.err, reads, waits, starts)
	}
}

// Cancellation at the exact construction return still transfers the original
// worker to drain and final metrics handback; it cannot start another attempt.
func TestTaskworkerStartupCancellationAfterOwnershipKeepsFinalHandback(t *testing.T) {
	taskworkerStartupTestEnvironment(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	runtime := &taskworkerLifecycleRuntime{drainStarted: make(chan struct{}), handbackDone: make(chan struct{})}
	var starts, pushes, flushes atomic.Uint64
	err := runWithDependencies(ctx, RunOptions{Port: 8080, Count: 1, BatchSize: 1},
		func(context.Context) error { return nil },
		func(context.Context) func() {
			pushes.Add(1)
			return func() {
				flushes.Add(1)
				if runtime.stage.Load() != 2 {
					t.Error("startup-boundary cancellation flushed before final handback")
				}
			}
		},
		func(lifetime context.Context, _ string, _ http.Handler, _ bool, _ server.HttpServerOptions) error {
			<-lifetime.Done()
			return nil
		},
		func(context.Context, context.Context, context.CancelFunc, RunOptions) (taskworkerRuntime, error) {
			starts.Add(1)
			cancel()
			return runtime, nil
		},
	)
	if err != nil || starts.Load() != 1 || pushes.Load() != 1 || flushes.Load() != 1 || runtime.stage.Load() != 2 {
		t.Fatal("startup-boundary cancellation lost original runtime or final handback", err, starts.Load(), pushes.Load(), flushes.Load(), runtime.stage.Load())
	}
}

// A partially constructed runtime is retained for handback even when startup
// returns a physical failure; no fresh owner may replay its activation.
func TestTaskworkerStartupFailureAfterOwnershipDrainsOriginalRuntime(t *testing.T) {
	taskworkerStartupTestEnvironment(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	runtime := &taskworkerLifecycleRuntime{drainStarted: make(chan struct{}), handbackDone: make(chan struct{})}
	original := &net.OpError{Op: "read", Net: "tcp", Err: syscall.ECONNRESET}
	var starts, waits, metrics atomic.Uint64
	err := runWithStartupWait(ctx, RunOptions{Port: 8080, Count: 1, BatchSize: 1},
		func(context.Context) error { return nil },
		func(context.Context) func() { metrics.Add(1); return nil },
		func(lifetime context.Context, _ string, _ http.Handler, _ bool, _ server.HttpServerOptions) error {
			<-lifetime.Done()
			return nil
		},
		func(context.Context, context.Context, context.CancelFunc, RunOptions) (taskworkerRuntime, error) {
			starts.Add(1)
			return runtime, original
		}, nil,
		func(context.Context, time.Duration) error { waits.Add(1); return nil },
	)
	if !errors.Is(err, original) || starts.Load() != 1 || waits.Load() != 0 || metrics.Load() != 0 || runtime.stage.Load() != 2 {
		t.Fatal("partial startup ownership was lost or retried", err, starts.Load(), waits.Load(), metrics.Load(), runtime.stage.Load())
	}
}

// The actual Run orchestration serves a closed status while the original check
// remains blocked. Caller cancellation then joins that check before returning.
func TestTaskworkerRunServesStatusDuringCancelableStartup(t *testing.T) {
	taskworkerStartupTestEnvironment(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	entered, served, joined := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var starts atomic.Uint64
	done := make(chan error, 1)
	go func() {
		done <- runWithDependencies(ctx, RunOptions{Port: 8080, Count: 1, BatchSize: 1},
			func(attempt context.Context) error {
				close(entered)
				<-attempt.Done()
				close(joined)
				return attempt.Err()
			},
			func(context.Context) func() { starts.Add(1); return nil },
			func(lifetime context.Context, _ string, handler http.Handler, _ bool, _ server.HttpServerOptions) error {
				<-entered
				response := httptest.NewRecorder()
				handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/status", nil))
				var status router.WarpStatusResult
				if json.Unmarshal(response.Body.Bytes(), &status) != nil || !strings.HasPrefix(status.Status, "error not ready:") || starts.Load() != 0 {
					t.Error("startup advertised readiness or work before dependency admission", response.Body.String())
				}
				close(served)
				<-lifetime.Done()
				return nil
			},
			func(context.Context, context.Context, context.CancelFunc, RunOptions) (taskworkerRuntime, error) {
				starts.Add(1)
				return nil, nil
			},
		)
	}()
	select {
	case <-served:
	case err := <-done:
		t.Fatal("startup owner returned before exposing its closed status", err)
	case <-time.After(10 * time.Second):
		cancel()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Fatal("unavailable startup status also failed to join after cancellation")
		}
		t.Fatal("status was unavailable while the dependency read was in flight")
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("caller cancellation did not join the active startup read")
	}
	select {
	case <-joined:
	default:
		t.Fatal("Run returned before its startup read joined")
	}
	if starts.Load() != 0 {
		t.Fatal("canceled startup launched a runtime")
	}
}
