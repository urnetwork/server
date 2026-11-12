// `sidecarctl evs purge-turn-on` against the env as server reaches it: sidecar.yml through server's loader and
// server/task's queue in Postgres. These need the private test stack, so they run through .stack/server-test.sh
// (server.DefaultTestEnv), under its WARP_ENV.

package main

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/sidecar/evsservice"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/task"
)

// The turn-on requests pending on the env's queue: sidecar jobs once-keyed by evsservice's turn-on request.
func pendingEvsTurnOnTasks(ctx context.Context) []*task.Task {
	onceKey := evsservice.EvsSweepTurnOnTaskRequest().OnceKey
	pending := []*task.Task{}
	for _, queued := range task.GetTasks(ctx, task.ListPendingTasks(ctx)...) {
		if strings.Contains(queued.RunOnceKey, `"`+onceKey+`"`) {
			pending = append(pending, queued)
		}
	}
	return pending
}

// Runs the registered command as the binary would, with the clock fixed, and returns its exit code and output.
func runServerEvsPurgeTurnOn(env string, now time.Time) (int, string, string) {
	settings := defaultEvsPurgeTurnOnSettings()
	settings.now = func() time.Time { return now }
	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	code := evsPurgeTurnOnTestCommands(settings).run([]string{"evs", "purge-turn-on", env}, stdout, stderr)
	return code, stdout.String(), stderr.String()
}

// With format2 enabled and purge on in the env's sidecar.yml, a run queues evsservice's turn-on request once, due
// startDelay after the run, and a second run folds into it.
func TestEvsPurgeTurnOnSchedulesOnTheEnvsTaskQueue(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(tb testing.TB) {
		ctx := context.Background()
		defer server.Testing_PushSidecarConfig("sidecar:\n  format2: {enabled: true, purge: on}\n")()
		env := server.RequireEnv()
		// whole seconds, as the queue's table holds a time
		now := server.NowUtc().Add(time.Hour).Truncate(time.Second)
		wantDueTime := now.Add(defaultEvsPurgeTurnOnSettings().startDelay)

		for run := range 2 {
			code, stdout, stderr := runServerEvsPurgeTurnOn(env, now)
			if code != 0 {
				tb.Fatalf("run %d exited %d: %s", run, code, stderr)
			}
			request := evsservice.EvsSweepTurnOnTaskRequest()
			for _, want := range []string{"scheduled " + request.Name, request.OnceKey, wantDueTime.Format(time.RFC3339)} {
				if !strings.Contains(stdout, want) {
					tb.Errorf("run %d printed %q, want %q in it", run, stdout, want)
				}
			}
			pending := pendingEvsTurnOnTasks(ctx)
			if len(pending) != 1 {
				tb.Fatalf("run %d left %d turn-on requests pending, want one", run, len(pending))
			}
			if !pending[0].RunAt.Equal(wantDueTime) {
				tb.Errorf("run %d: the request is due %s, want %s", run, pending[0].RunAt, wantDueTime)
			}
		}
	})
}

// Under observe, with the gate off, and with no sidecar.yml at all, the env's queue takes nothing and the run
// exits 1.
func TestEvsPurgeTurnOnQueuesNothingUnlessTheEnvsGateReadsOn(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(tb testing.TB) {
		ctx := context.Background()
		env := server.RequireEnv()
		cases := []struct {
			file string
			want string
		}{
			{file: "sidecar:\n  format2: {enabled: true, purge: observe}\n", want: `format2.purge reads "observe"`},
			{file: "sidecar:\n  format2: {enabled: true}\n", want: `format2.purge reads "observe"`},
			{file: "sidecar:\n  format2: {enabled: false, purge: on}\n", want: "format2 is not enabled"},
			{file: "sidecar: {}\n", want: "format2 is not enabled"},
		}
		for _, c := range cases {
			func() {
				defer server.Testing_PushSidecarConfig(c.file)()
				code, stdout, stderr := runServerEvsPurgeTurnOn(env, server.NowUtc())
				if code != 1 || !strings.Contains(stderr, c.want) || !strings.Contains(stderr, "nothing was scheduled") {
					tb.Errorf("%q: exited %d with %q, want 1 and %q", c.file, code, stderr, c.want)
				}
				if stdout != "" {
					tb.Errorf("%q: printed %q", c.file, stdout)
				}
			}()
			if pending := pendingEvsTurnOnTasks(ctx); len(pending) != 0 {
				tb.Fatalf("%q: %d turn-on requests pending, want none", c.file, len(pending))
			}
		}
	})
}
