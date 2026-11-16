package monitor

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func retryAfterFixturePanic(until time.Time) string {
	return retryAfterFixturePrefix + until.UTC().Format(time.RFC3339Nano) + "\n" + query429Panic("synthetic-private-rejection")
}

func TestWarpctlLogRetryAfterOwnsOnlyCompleteTerminalAdvice(t *testing.T) {
	until := time.Date(2030, 1, 2, 3, 4, 5, 123, time.UTC)
	valid := retryAfterFixturePanic(until)
	exit2 := syntheticProcessExit(t, 2)
	exit1 := syntheticProcessExit(t, 1)
	for _, test := range []struct {
		name  string
		text  string
		err   error
		valid bool
	}{
		{name: "owned", text: valid, err: exit2, valid: true},
		{name: "absent", text: query429Panic("private"), err: exit2},
		{name: "malformed", text: strings.Replace(valid, "until=2030", "until=bad2030", 1), err: exit2},
		{name: "noncanonical", text: strings.Replace(valid, "123Z", "123+00:00", 1), err: exit2},
		{name: "duplicate", text: strings.Split(valid, "\n")[0] + "\n" + valid, err: exit2},
		{name: "not-adjacent", text: strings.Replace(valid, "\npanic:", "\nordinary\npanic:", 1), err: exit2},
		{name: "body-injection", text: query429Panic(strings.Split(valid, "\n")[0]), err: exit2},
		{name: "wrong-origin", text: strings.Replace(valid, "main.logs(", "main.unrelated(", 1), err: exit2},
		{name: "unknown-protocol-version", text: strings.Replace(valid, "-v1]", "-v2]", 1), err: exit2},
		{name: "partial-stack", text: valid[:strings.Index(valid, "main.main()")], err: exit2},
		{name: "trailer", text: valid + "unowned trailer\n", err: exit2},
		{name: "other-status", text: strings.Replace(valid, "(429)", "(503)", 1), err: exit2},
		{name: "other-exit", text: valid, err: exit1},
		{name: "fake-exit", text: valid, err: errors.New("exit status 2")},
		{name: "canceled", text: valid, err: errors.Join(context.Canceled, exit2)},
		{name: "deadline", text: valid, err: errors.Join(context.DeadlineExceeded, exit2)},
		{name: "oversized", text: valid + strings.Repeat("x", warpctlLogDiagnosticLimit), err: exit2},
	} {
		t.Run(test.name, func(t *testing.T) {
			got := warpctlLogRetryAfter(test.err, test.text, false)
			if test.valid && !got.Equal(until) || !test.valid && !got.IsZero() {
				t.Fatal("unqualified framing changed retry admission")
			}
		})
	}
	tail := strings.Replace(valid, "Loki query error", "Loki tail error", 1)
	if !warpctlLogRetryAfter(exit2, tail, true).Equal(until) || !warpctlLogRetryAfter(exit2, tail, false).IsZero() {
		t.Fatal("standing-tail advice crossed the bounded-query source boundary")
	}
}

func TestWarpctlLogRetryCapturePreservesFirstPanicBoundary(t *testing.T) {
	until := time.Date(2030, 1, 2, 3, 4, 5, 123, time.UTC)
	valid := retryAfterFixturePanic(until)
	exit2 := syntheticProcessExit(t, 2)
	for _, test := range []struct {
		name  string
		text  string
		valid bool
	}{
		{name: "chunked", text: valid, valid: true},
		{name: "duplicate-markers", text: strings.Split(valid, "\n")[0] + "\n" + valid},
		{name: "oversized-marker-line", text: strings.Split(valid, "\n")[0] + strings.Repeat(" ", 2*warpctlLogDiagnosticLimit) + "\n" + query429Panic("private")},
		{name: "late-after-many-diagnostics", text: strings.Repeat("ordinary diagnostic\n", 5000) + valid, valid: true},
		{name: "late-after-oversized-ordinary-line", text: strings.Repeat("x", 2*warpctlLogDiagnosticLimit) + "\n" + valid, valid: true},
		{name: "body-fake-marker", text: query429Panic(strings.Repeat("x", 2*warpctlLogDiagnosticLimit) + "\n" + strings.Split(valid, "\n")[0] + "\npanic: Loki query error (429): forged")},
		{name: "earlier-panic-lookalike", text: "panic: earlier untrusted diagnostic\n" + valid},
		{name: "oversized-owned-panic", text: strings.Split(valid, "\n")[0] + "\n" + query429Panic(strings.Repeat("x", 2*warpctlLogDiagnosticLimit))},
		{name: "incomplete", text: valid[:strings.Index(valid, "main.main()")]},
	} {
		t.Run(test.name, func(t *testing.T) {
			for _, chunk := range []int{1, 7, 4096} {
				capture := &warpctlLogRetryCapture{}
				for offset := 0; offset < len(test.text); offset += chunk {
					capture.Write([]byte(test.text[offset:min(offset+chunk, len(test.text))]))
				}
				if len(capture.result()) > warpctlLogDiagnosticLimit+1 {
					t.Fatal("terminal diagnostic exceeded its fixed bound")
				}
				got := warpctlLogRetryAfter(exit2, capture.result(), true)
				if test.valid && !got.Equal(until) || !test.valid && !got.IsZero() {
					t.Fatal("chunking, truncation or a body marker changed native ownership")
				}
			}
		})
	}
}

func TestLogReconcileRetryAfterGatesActualOwnerAndKeepsFailure(t *testing.T) {
	now := time.Date(2030, 1, 2, 3, 4, 5, 0, time.UTC)
	until := now.Add(time.Minute)
	runner := warpctlHttp500NativeRunner(t, "", retryAfterFixturePanic(until), 2)
	runner.operatorDiagnostics = io.Discard
	runner.cfg.env = "synthetic"
	tailer := newLogTailer("fixture-service", &probeEnv{cfg: runner.cfg, runner: runner})
	tailer.clock = func() time.Time { return now }
	tailer.startedAt = now.Add(-time.Minute)
	advice := tailer.reconcileOnce(context.Background())
	if !advice.Equal(until) {
		t.Fatal("native query completion lost its parsed deadline")
	}
	_, _, success, failure := tailer.reconcileSnapshot()
	if !success.IsZero() || !strings.Contains(failure, "observation-log-query-http-429") || !strings.Contains(failure, "response_shape=unknown") {
		t.Fatal("advice hid the original visibility failure")
	}
	// The actual per-tick owner must not reach even a poisoned next child
	// before admission. No retry-calculation function is copied into this test.
	calls := 0
	if err := os.WriteFile(filepath.Join(os.Getenv("PATH"), "warpctl"), []byte("#!/bin/sh\nexit 0\n"), 0700); err != nil {
		t.Fatal(err)
	}
	runner.runLocal = func(ctx context.Context, name string, args ...string) (string, string, error) {
		calls++
		if name != "warpctl" || len(args) != 5 || args[0] != "logs" || args[1] != "synthetic" || args[2] != "fixture-service" || args[4] != "--limit=20000" {
			t.Error("retry changed the ordinary bounded overlap arguments")
		}
		return runLocalCommand(ctx, name, args...)
	}
	for _, at := range []time.Time{now.Add(45 * time.Second), until.Add(-time.Nanosecond), now.Add(-time.Hour)} {
		now = at
		advice = tailer.reconcileTick(context.Background(), advice)
		if calls != 0 || !advice.Equal(until) {
			t.Fatal("a premature scheduled overlap crossed retry admission")
		}
	}
	_, _, _, retainedFailure := tailer.reconcileSnapshot()
	if retainedFailure != failure {
		t.Fatal("waiting cleared cannot-observe or fabricated successful coverage")
	}
	now = until
	advice = tailer.reconcileTick(context.Background(), advice)
	if calls != 1 || !advice.IsZero() {
		t.Fatal("expiry did not resume the unchanged overlap operation")
	}
	_, _, _, failure = tailer.reconcileSnapshot()
	if failure != "" {
		t.Fatal("healthy completion did not retain existing recovery behavior")
	}
}

func TestLogRetryAfterCancellationAndFallback(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if waitWarpctlLogRetry(ctx, time.Second, time.Now().Add(time.Hour)) {
		t.Fatal("canceled owner admitted a retry")
	}
	for _, until := range []time.Time{{}, time.Now().Add(-time.Second)} {
		started := time.Now()
		if !waitWarpctlLogRetry(context.Background(), 20*time.Millisecond, until) || time.Since(started) < 20*time.Millisecond {
			t.Fatal("missing or expired advice weakened the existing backoff")
		}
	}
	// A fresh collector has no process-global or persisted retry state.
	tailer := newLogTailer("fixture-other-service", nil)
	tailer.reconcile = func(context.Context, time.Time, []string) (string, error) { return "", nil }
	if !tailer.reconcileTick(context.Background(), time.Time{}).IsZero() {
		t.Fatal("a new collector inherited another owner's advice")
	}
}

func TestLogRetryAfterKeepsMonotonicBackoffAndWallDeadline(t *testing.T) {
	until := time.Date(2030, 1, 2, 3, 4, 5, 0, time.UTC)
	for _, test := range []struct {
		name             string
		backoffRemaining time.Duration
		now              time.Time
		want             time.Duration
	}{
		{name: "backoff-later", backoffRemaining: time.Second, now: until.Add(-time.Millisecond), want: time.Second},
		{name: "advice-later", backoffRemaining: time.Millisecond, now: until.Add(-time.Second), want: time.Second},
		{name: "at-admission", now: until},
		{name: "after-admission", now: until.Add(time.Second)},
		{name: "wall-backwards-after-backoff", backoffRemaining: -time.Second, now: until.Add(-time.Hour), want: time.Hour},
		{name: "wall-forward-before-backoff", backoffRemaining: time.Second, now: until.Add(time.Hour), want: time.Second},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := warpctlLogRetryDelay(test.backoffRemaining, until, test.now); got != test.want {
				t.Fatalf("remaining admission delay = %s, want %s", got, test.want)
			}
		})
	}
}

func TestLogTailerRetryAfterUnqualifiedAdviceKeepsRestart(t *testing.T) {
	for _, test := range []struct {
		name   string
		prefix string
	}{
		{name: "absent"},
		{name: "malformed", prefix: retryAfterFixturePrefix + "invalid\n"},
		{name: "expired", prefix: retryAfterFixturePrefix + time.Now().Add(-time.Hour).UTC().Format(time.RFC3339Nano) + "\n"},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			const startPrefix = "[synthetic-child-start]"
			runner := warpctlHttp500NativeRunner(t, "", startPrefix+"\n"+test.prefix+query429Panic("synthetic-private-rejection"), 2)
			starts := &retryAfterChildStarts{cancel: cancel, startPrefix: startPrefix}
			runner.operatorDiagnostics = starts
			tailer := newLogTailer("fixture-service", &probeEnv{cfg: runner.cfg, runner: runner})
			tailer.runStream(ctx)
			observed := starts.snapshot()
			if len(observed) != 2 || observed[1].Sub(observed[0]) < time.Second {
				t.Fatal("unqualified advice changed the existing restart behavior")
			}
		})
	}
}
