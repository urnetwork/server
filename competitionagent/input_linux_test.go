// Upload cancellation and authority-lock regressions use acknowledged reader
// boundaries, not sleeps. No host daemon or database is contacted.
package competitionagent

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

// Close explicitly interrupts the acknowledged pending Read. A shared release
// allows two complete uploads to reach the durable single-launch race together.
type gatedStartInput struct {
	reader    io.Reader
	entered   chan<- struct{}
	release   <-chan struct{}
	closed    chan struct{}
	enterOnce sync.Once
	closeOnce sync.Once
}

// Blocking occurs inside the actual Read call, after its explicit acknowledgement.
func (self *gatedStartInput) Read(content []byte) (int, error) {
	self.enterOnce.Do(func() { self.entered <- struct{}{} })
	select {
	case <-self.closed:
		return 0, io.ErrClosedPipe
	case <-self.release:
		return self.reader.Read(content)
	}
}

// No goroutine remains blocked after cancellation closes this stream owner.
func (self *gatedStartInput) Close() error {
	self.closeOnce.Do(func() { close(self.closed) })
	return nil
}

// Both new and duplicate signed uploads may stall without retaining any lock
// needed by ExecStopPost's physical reap or its durable cleanup publication.
func TestAgentStalledUploadCannotBlockCleanup(t *testing.T) {
	for _, duplicate := range []bool{false, true} {
		fixture := newAgentTestFixture(t)
		if _, err := fixture.call(t, "start", 1, &fixture.manifest); err != nil {
			t.Fatal(err)
		}
		manifest := fixture.manifest
		if !duplicate {
			manifest = fixture.anotherManifest(t, "synthetic-stalled-other", 1)
		}
		watchdog, stopWatchdog := context.WithTimeout(t.Context(), fixture.settings.ControlTimeout)
		defer stopWatchdog()
		ctx, cancel := context.WithCancel(watchdog)
		entered := make(chan struct{}, 1)
		input := &gatedStartInput{reader: bytes.NewReader(nil), entered: entered, release: make(chan struct{}), closed: make(chan struct{})}
		done, joined := make(chan error, 1), make(chan struct{})
		go func() {
			defer close(joined)
			_, err := fixture.agent.start(ctx, Request{Schema: Schema, HostId: fixture.config.HostId, Operation: "start", RunId: manifest.Identity.RunId, FencingToken: 1, Start: &manifest}, input)
			done <- err
		}()
		defer func() {
			cancel()
			_ = input.Close()
			select {
			case <-joined:
			case <-watchdog.Done():
				t.Error("canceled upload did not join its reader owner")
			}
		}()
		select {
		case <-entered:
		case err := <-done:
			t.Fatal("upload bypassed its actual blocked-read barrier", duplicate, err)
		case <-ctx.Done():
			t.Fatal("upload did not enter its bounded stream read")
		}
		if err := fixture.agent.Cleanup(ctx, fixture.manifest.Identity.RunId); err != nil {
			t.Fatal("stalled upload blocked physical cleanup", duplicate, err)
		}
		state, err := fixture.agent.loadState(fixture.manifest.Identity.RunId)
		if err != nil || !state.StopCleanupVerified || !state.CleanupVerified || fixture.cleaner.count() != 1 {
			t.Fatal("cleanup did not publish while upload remained blocked", duplicate, err)
		}
		select {
		case err := <-done:
			t.Fatal("upload was not still waiting at its input boundary", err)
		default:
		}
		cancel()
		select {
		case err := <-done:
			if !errors.Is(err, context.Canceled) {
				t.Fatal("actual stream cancellation lost its owner cause", err)
			}
		case <-watchdog.Done():
			t.Fatal("cancellation did not interrupt actual blocked Read")
		}
		if fixture.runner.launchCount() != 1 {
			t.Fatal("stalled transfer dispatched or replayed evaluation")
		}
	}
}

// Even an unrelated briefly held publication lock cannot prevent the first
// physical kill/join; only committing its proof waits for the lock to release.
func TestAgentCleanupReapsBeforeAdmissionPublicationLock(t *testing.T) {
	fixture := newAgentTestFixture(t)
	if _, err := fixture.call(t, "start", 1, &fixture.manifest); err != nil {
		t.Fatal(err)
	}
	watchdog, stopWatchdog := context.WithTimeout(t.Context(), fixture.settings.ControlTimeout)
	defer stopWatchdog()
	ctx, cancel := context.WithCancel(watchdog)
	publication, err := fixture.agent.lock(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer publication.Close()
	entered := make(chan struct{})
	fixture.cleaner.before = func(context.Context, cleanupPlan) error { close(entered); return nil }
	done := make(chan error, 1)
	joined := make(chan struct{})
	go func() {
		defer close(joined)
		done <- fixture.agent.Cleanup(ctx, fixture.manifest.Identity.RunId)
	}()
	defer func() {
		_ = publication.Close()
		cancel()
		select {
		case <-joined:
		case <-watchdog.Done():
			t.Error("cleanup did not join its canceled publication owner")
		}
	}()
	select {
	case <-entered:
	case err := <-done:
		t.Fatal("cleanup returned before reaching its physical reap", err)
	case <-ctx.Done():
		t.Fatal("publication lock blocked physical cleanup")
	}
	select {
	case err := <-done:
		t.Fatal("cleanup proof bypassed publication serialization", err)
	default:
	}
	if err := publication.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal("joined cleanup lost publication after lock release", err)
		}
	case <-ctx.Done():
		t.Fatal("cleanup did not join its bounded publication owner")
	}
}

// The production stdin pipe is duplicated into a real pollable descriptor, not
// wrapped in an abandoned goroutine blocked in syscall.Read.
func TestAgentControlInputPipeHasInterruptibleReadOwner(t *testing.T) {
	input, output, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer input.Close()
	defer output.Close()
	reader, closer, err := cancellableControlInput(input)
	if err != nil {
		t.Fatal(err)
	}
	file := reader.(*os.File)
	ctx, cancel := context.WithTimeout(t.Context(), DefaultAgentSettings().ControlTimeout)
	defer cancel()
	deadline, _ := ctx.Deadline()
	if err := file.SetReadDeadline(deadline); err != nil {
		t.Fatal("duplicated production pipe did not register an interruptible poll owner", err)
	}
	entered, done := make(chan struct{}), make(chan error, 1)
	go func() {
		close(entered)
		_, err := reader.Read(make([]byte, 1))
		done <- err
	}()
	<-entered
	if err := closer.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if !errors.Is(err, os.ErrClosed) {
			t.Fatal("closed production pipe retained a blocked read or wrong result", err)
		}
	case <-ctx.Done():
		t.Fatal("closed production pipe left an unjoined reader")
	}
}

// Concurrent fully authenticated copies recheck one immutable job/clock under
// the lock after transfer. Only one launch marker and systemd start are possible.
func TestAgentConcurrentUploadsRecheckSingleLaunchAfterTransfer(t *testing.T) {
	fixture := newAgentTestFixture(t)
	body := &bytes.Buffer{}
	if err := WriteFiles(body, fixture.inputDirectory, fixture.manifest.Inputs, fixture.config.Limits.InputBytes, fixture.config.Limits); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), fixture.settings.ControlTimeout)
	defer cancel()
	entered, release := make(chan struct{}, 2), make(chan struct{})
	done := make(chan error, 2)
	var workers sync.WaitGroup
	for index := 0; index < 2; index++ {
		input := &gatedStartInput{reader: bytes.NewReader(body.Bytes()), entered: entered, release: release, closed: make(chan struct{})}
		workers.Add(1)
		go func() {
			defer workers.Done()
			_, err := fixture.agent.start(ctx, Request{Schema: Schema, HostId: fixture.config.HostId, Operation: "start", RunId: fixture.manifest.Identity.RunId,
				FencingToken: 1, Start: &fixture.manifest}, input)
			done <- err
		}()
	}
	var releaseOnce sync.Once
	defer func() { releaseOnce.Do(func() { close(release) }); cancel(); workers.Wait() }()
	for index := 0; index < 2; index++ {
		select {
		case <-entered:
		case err := <-done:
			t.Fatal("concurrent upload did not reach its input barrier", err)
		case <-ctx.Done():
			t.Fatal("concurrent upload missed its bounded transfer barrier")
		}
	}
	releaseOnce.Do(func() { close(release) })
	for index := 0; index < 2; index++ {
		select {
		case err := <-done:
			if err != nil {
				t.Fatal("same immutable concurrent upload was not reconciled", err)
			}
		case <-ctx.Done():
			t.Fatal("concurrent upload did not join its one execution authority")
		}
	}
	if fixture.runner.launchCount() != 1 {
		t.Fatal("concurrent transfers dispatched the same run more than once")
	}
	entries, err := filepath.Glob(filepath.Join(fixture.config.WorkDirectory, ".incoming-*"))
	if err != nil || len(entries) != 0 {
		t.Fatal("joined uploads retained a private transfer staging directory", entries, err)
	}
}
