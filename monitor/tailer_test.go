package main

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"testing"
	"time"
)

// fakeStream builds a stream shaped exactly like runner.warpctlStream (child
// process writing into an os.Pipe) around an arbitrary shell script.
func fakeStream(script string) func(ctx context.Context) (*exec.Cmd, io.ReadCloser, error) {
	return func(ctx context.Context) (*exec.Cmd, io.ReadCloser, error) {
		cmd := exec.CommandContext(ctx, "sh", "-c", script)
		pr, pw, err := os.Pipe()
		if err != nil {
			return nil, nil, err
		}
		cmd.Stdout = pw
		cmd.Stderr = pw
		if err := cmd.Start(); err != nil {
			pr.Close()
			pw.Close()
			return nil, nil, err
		}
		pw.Close()
		return cmd, pr, nil
	}
}

// a > 1MB line must cost one counted stream restart, not a dead tailer:
// before the fix the scan loop exited on bufio.ErrTooLong but cmd.Wait()
// blocked forever on the still-writing child and the full pipe.
func TestTailerOversizedLineDoesNotWedge(t *testing.T) {
	tailer := newLogTailer("api", nil)
	// one classifiable line, then a ~2MB single line (overflowing the 1MB
	// scanner buffer), then the child keeps the pipe open forever — the wedge
	// shape.
	tailer.stream = fakeStream(
		`echo "short line"; head -c 2097152 /dev/zero | tr '\0' 'a'; echo; sleep 3600`)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	type result struct{ err error }
	done := make(chan result, 1)
	go func() {
		done <- result{err: tailer.tailOnce(ctx)}
	}()

	select {
	case r := <-done:
		if r.err != bufio.ErrTooLong {
			t.Fatalf("tailOnce err = %v; want bufio.ErrTooLong", r.err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("tailOnce wedged on an oversized line (child not killed before Wait)")
	}

	_, _, scanErrors := tailer.healthSnapshot()
	if scanErrors != 1 {
		t.Fatalf("scanErrorCount = %d; want 1", scanErrors)
	}
}

// a clean stream end (child exits, pipe closes) returns nil so run() resets
// its backoff, and the lines were classified.
func TestTailerCleanStreamEnd(t *testing.T) {
	tailer := newLogTailer("api", nil)
	tailer.stream = fakeStream(`echo "a error b"; echo "plain line"`)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() {
		done <- tailer.tailOnce(ctx)
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("tailOnce err = %v; want nil", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("tailOnce did not return after a clean stream end")
	}

	lastLine, _, scanErrors := tailer.healthSnapshot()
	if scanErrors != 0 {
		t.Fatalf("scanErrorCount = %d; want 0", scanErrors)
	}
	if time.Since(lastLine) > time.Minute {
		t.Fatalf("lastLineTime not updated by classify: %s", lastLine)
	}
}

func findingByClass(t *testing.T, findings []finding, class string) finding {
	t.Helper()
	for _, f := range findings {
		if f.class == class {
			return f
		}
	}
	t.Fatalf("no finding with class %q in %d findings", class, len(findings))
	return finding{}
}

// the §3.7 tailer self-health thresholds: silent-too-long and restarting-hot
// raise monitor/visibility findings; a live, stable tailer reports healthy.
func TestTailerHealthFindings(t *testing.T) {
	now := time.Now()

	healthy := tailerHealthFindings("api", now, now.Add(-time.Minute), 0, 0)
	if f := findingByClass(t, healthy, "tailer-silent"); !f.healthy {
		t.Fatalf("recent line reported silent: %+v", f)
	}
	if f := findingByClass(t, healthy, "tailer-restarting"); !f.healthy {
		t.Fatalf("no restarts reported hot: %+v", f)
	}

	silent := tailerHealthFindings("api", now, now.Add(-11*time.Minute), 0, 0)
	f := findingByClass(t, silent, "tailer-silent")
	if f.healthy {
		t.Fatal("11 minutes silent must raise a tailer-silent finding")
	}
	if f.probeId != "monitor/visibility" || f.target != "logs/api" {
		t.Fatalf("wrong identity: probeId=%s target=%s", f.probeId, f.target)
	}
	if f := findingByClass(t, silent, "tailer-restarting"); !f.healthy {
		t.Fatalf("silent-only case reported restarting: %+v", f)
	}

	hot := tailerHealthFindings("api", now, now.Add(-time.Minute), tailerHotRestartThreshold, 2)
	if f := findingByClass(t, hot, "tailer-restarting"); f.healthy {
		t.Fatal("restart delta at threshold must raise a tailer-restarting finding")
	}
	calm := tailerHealthFindings("api", now, now.Add(-time.Minute), tailerHotRestartThreshold-1, 0)
	if f := findingByClass(t, calm, "tailer-restarting"); !f.healthy {
		t.Fatalf("restart delta below threshold reported hot: %+v", f)
	}
}

type recordingEmitter struct {
	events []ticketEvent
}

func (self *recordingEmitter) emit(ctx context.Context, ev ticketEvent) error {
	self.events = append(self.events, ev)
	return nil
}

// the novel class carries a varying top shape; if that shape were the ticket
// frame (as it once was), two minutes with different shapes would never
// accumulate the sustain-2 streak and the ticket could never open.
func TestNovelTicketOpensAcrossVaryingShapes(t *testing.T) {
	ctx := context.Background()
	emitter := &recordingEmitter{}
	manager := newTicketManager("test", emitter)
	tailer := newLogTailer("api", nil)

	// minute 1: one novel shape at rate
	for i := 0; i < novelRateThreshold+5; i += 1 {
		tailer.classify(fmt.Sprintf("widget error: alpha failure %d", i))
	}
	findings := tailer.drainWindow()
	novel := findingByClass(t, findings, "novel")
	if novel.healthy {
		t.Fatal("novel lines at rate must produce a broken finding")
	}
	if novel.frame != "" {
		t.Fatalf("novel finding frame = %q; the varying shape must not be identity", novel.frame)
	}
	manager.ingest(ctx, findings)
	for _, ev := range emitter.events {
		if ev.kind == ticketOpen {
			t.Fatalf("ticket opened after one tick despite sustain 2: %+v", ev.t.ticketIdentity)
		}
	}

	// minute 2: a different top shape — the streak must still accumulate
	for i := 0; i < novelRateThreshold+5; i += 1 {
		tailer.classify(fmt.Sprintf("gadget error: beta mode %d", i))
	}
	manager.ingest(ctx, tailer.drainWindow())

	opened := false
	for _, ev := range emitter.events {
		if ev.kind == ticketOpen && ev.t.probeId == "logs/novel" {
			opened = true
		}
	}
	if !opened {
		t.Fatal("two consecutive novel minutes with different top shapes did not open a ticket")
	}
}
