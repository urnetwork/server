// Completed-item attestation keeps a checkpoint through item-local stops only.
package task

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/urnetwork/server"
)

// A target that finished every item may checkpoint despite one item's own
// deadline. The failure still counts, ordinary hints keep refusing the same
// cause, and task-context cancellation withdraws both checkpoint and cadence.
func TestCompletedItemsRetryKeepsCheckpointThroughItemStops(t *testing.T) {
	items := &taskCauseTestOne{cause: server.NewErrorCauseBatch([]error{
		fmt.Errorf("synthetic item: %w", fmt.Errorf("timeout: %w", context.DeadlineExceeded)),
		errors.New("synthetic item refusal"),
	})}
	args := map[string]string{"cursor": "next"}
	hint := WithCompletedItemsRetryDelayAndArgs(items, 3*time.Second, args)
	if retryArgs := taskRetryArgsJson(hint); retryArgs == nil || *retryArgs != `{"cursor":"next"}` {
		t.Fatal("a completed page lost its checkpoint to one item's own deadline")
	}
	if delay, delta := taskErrorRetryDelay(hint, 16, 0.5); delay != 3*time.Second || delta != 1 {
		t.Fatal("the attested item failure lost its cadence or stopped counting as a failure", delay, delta)
	}
	if hint.Error() != items.Error() || !errors.Is(hint, context.DeadlineExceeded) {
		t.Fatal("the attestation changed the stored failure or hid its typed cause")
	}
	if taskRetryArgsJson(WithRetryDelayAndArgs(items, 3*time.Second, args)) != nil {
		t.Fatal("an unattested hint acquired a checkpoint through a deadline")
	}
	withdrawn := withoutTaskRetryArgs(hint)
	if delay, _ := taskErrorRetryDelay(withdrawn, 16, 0.5); taskRetryArgsJson(withdrawn) != nil || delay < 30*time.Minute {
		t.Fatal("task-context cancellation kept the attested checkpoint or cadence", delay)
	}
	// Seven seconds differs from every ordinary drain, version-skew and
	// backoff delay at this count, so an accepted hint is unambiguous.
	for _, c := range []struct {
		err   error
		delay time.Duration
		args  any
	}{
		{err: &taskCauseTestOne{cause: ErrDrained}, delay: 7 * time.Second, args: args},
		{err: &taskCauseTestOne{cause: ErrTargetNotFound}, delay: 7 * time.Second, args: args},
		{err: errors.Join(context.DeadlineExceeded, errors.New("synthetic joined root")), delay: 7 * time.Second, args: args},
		{err: items, delay: time.Millisecond, args: args},
		{err: items, delay: 7 * time.Second, args: make(chan int)},
		{err: items, delay: 7 * time.Second, args: []string{"not an object"}},
	} {
		hinted := WithCompletedItemsRetryDelayAndArgs(c.err, c.delay, c.args)
		if taskRetryArgsJson(hinted) != nil {
			t.Fatal("drain, missing target, joined root, invalid delay or argument acquired a checkpoint", c.err, c.delay)
		}
		if delay, _ := taskErrorRetryDelay(hinted, 16, 0.5); delay == c.delay {
			t.Fatal("an unaccepted attestation still shortened the ordinary backoff", c.err, c.delay)
		}
	}
}
