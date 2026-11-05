//go:build linux

// A complete but delayed old transfer cannot reserve a host after its original
// deadline. Only private input files, fake units and an acknowledged read gate
// are used; no wall-clock sleeps or native evaluator workloads are involved.
package competitionagent

import (
	"bytes"
	"context"
	"errors"
	"os"
	"sync"
	"testing"
)

func TestAgentLateUncommittedDispatchCannotConsumeNextJobSlot(t *testing.T) {
	fixture := newAgentTestFixture(t)
	body := &bytes.Buffer{}
	if err := WriteFiles(body, fixture.inputDirectory, fixture.manifest.Inputs, fixture.config.Limits.InputBytes, fixture.config.Limits); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), fixture.settings.ControlTimeout)
	defer cancel()
	entered, release := make(chan struct{}, 1), make(chan struct{})
	input := &gatedStartInput{reader: bytes.NewReader(body.Bytes()), entered: entered, release: release, closed: make(chan struct{})}
	done, joined := make(chan error, 1), make(chan struct{})
	go func() {
		defer close(joined)
		_, err := fixture.agent.start(ctx, Request{Schema: Schema, HostId: fixture.config.HostId, Operation: "start", RunId: fixture.manifest.Identity.RunId,
			FencingToken: 1, Start: &fixture.manifest}, input)
		done <- err
	}()
	var once sync.Once
	t.Cleanup(func() { once.Do(func() { close(release) }); cancel(); _ = input.Close(); <-joined })
	select {
	case <-entered:
	case err := <-done:
		t.Fatal("delayed dispatch did not reach its actual input barrier", err)
	case <-ctx.Done():
		t.Fatal("delayed dispatch did not enter its bounded input read")
	}
	fixture.now = fixture.manifest.Identity.DeadlineAt
	next := fixture.anotherManifest(t, "synthetic-next-physical-job", 1)
	next.Identity.StartedAt, next.Identity.DeadlineAt = fixture.now, fixture.now.Add(fixture.config.Limits.MaximumJobDuration)
	if _, err := fixture.call(t, "start", 2, &next); err != nil {
		t.Fatal("pending old transfer reserved a new job's physical slot", err)
	}
	once.Do(func() { close(release) })
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("an elapsed original deadline admitted an uncommitted old dispatch")
		}
	case <-ctx.Done():
		t.Fatal("released old transfer did not join its original request")
	}
	slot, err := fixture.agent.loadSlot()
	if err != nil || slot.RunId != next.Identity.RunId || fixture.runner.launchCount() != 1 {
		t.Fatal("late old dispatch stole current custody or reached a second evaluator", err)
	}
	if _, err := fixture.agent.loadState(fixture.manifest.Identity.RunId); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("expired uncommitted transfer published a new old-run authority", err)
	}
}
