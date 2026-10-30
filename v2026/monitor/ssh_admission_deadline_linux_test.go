// A cancellation-only caller still needs the existing admission ceiling on
// the context used by file locking and native owner-authority observations.
package monitor

import (
	"context"
	"testing"
	"time"
)

// Observe the real acquisition's external authority seam, not a timer helper.
// The old path checked its deadline only between calls and passed no deadline
// into a potentially blocking call when the parent supplied cancellation alone.
func TestSharedSshAdmissionBoundsCancellationOnlyParent(t *testing.T) {
	store, state := newSyntheticSshAdmissionStore(t)
	parent, cancel := context.WithCancel(context.Background())
	defer cancel()
	started := time.Now()
	observed, bounded := false, true
	store.status = func(ctx context.Context, _ sshAdmissionOwner) sshAdmissionOwnerStatus {
		observed = true
		deadline, present := ctx.Deadline()
		// One second allows local test setup/scheduling, while absence of a
		// deadline is the deterministic pre-fix failure, without a sleep.
		bounded = bounded && present && deadline.After(started) && !deadline.After(started.Add(121*time.Second))
		return sshAdmissionOwnerLive
	}
	owner := state.Watcher
	release, err := store.acquire(parent, owner, owner.Identity, sshAdmissionWatch, []string{"a.example"})
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	if !observed || !bounded {
		t.Fatal("acquisition authority received an unbounded cancellation-only context")
	}
}
