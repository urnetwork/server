// Recovery memory and replay controls supplement the causal file-store tests.
package monitor

import (
	"context"
	"errors"
	"fmt"
	"testing"
)

// Full local ownership refuses enrollment before another claim can be lost.
func TestSharedSshAdmissionRecoveryRecordLimit(t *testing.T) {
	store, state := newSyntheticSshAdmissionStore(t)
	owner := state.Watcher
	for i := range sshAdmissionQueueLimit {
		if _, err := store.trackClaim(fmt.Sprintf("%032x", i), owner); err != nil {
			t.Fatal(err)
		}
	}
	if release, err := store.acquire(context.Background(), owner, owner.Identity, sshAdmissionWatch, []string{"a.example"}); err == nil {
		release()
		t.Fatal("full local claim set admitted another request")
	}
	if current := readRecoverySshState(t, store); len(current.Requests) != 0 || len(store.claims.records) != sshAdmissionQueueLimit {
		t.Fatalf("record cap lost claims or published work: records=%d queue=%d", len(store.claims.records), len(current.Requests))
	}
}

// A failed recovery keeps its proof, and an already-published removal can be
// replayed without resurrecting the token or deleting another request.
func TestSharedSshAdmissionRecoveryRetainsProofThroughFailureAndAbsentToken(t *testing.T) {
	store, state := newSyntheticSshAdmissionStore(t)
	owner := state.Watcher
	release, err := store.acquire(context.Background(), owner, owner.Identity, sshAdmissionWatch, []string{"a.example"})
	if err != nil {
		t.Fatal(err)
	}
	setRecoverySshLockMode(t, store, 0644)
	if err := release(); err == nil {
		t.Fatal("control removal succeeded")
	}
	if next, err := store.acquire(context.Background(), owner, owner.Identity, sshAdmissionWatch, []string{"b.example"}); err == nil {
		next()
		t.Fatal("failed recovery continued enrollment")
	}
	if len(store.claims.records) != 1 {
		t.Fatal("failed recovery lost its cleanup proof")
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if err := store.recoverClaims(canceled); !errors.Is(err, context.Canceled) || len(store.claims.records) != 1 {
		t.Fatalf("canceled recovery lost ownership: %v", err)
	}
	setRecoverySshLockMode(t, store, 0600)
	// Model the persisted result of a removal whose acknowledgement was lost.
	if err := store.update(context.Background(), func(current *sshAdmissionState, _ int64) error {
		current.Requests = nil
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	next, err := store.acquire(context.Background(), owner, owner.Identity, sshAdmissionWatch, []string{"b.example"})
	if err != nil {
		t.Fatal(err)
	}
	if err := release(); err != nil {
		t.Fatal("already recovered release was not idempotent", err)
	}
	if current := readRecoverySshState(t, store); len(current.Requests) != 1 || current.Requests[0].Hosts[0] != "b.example" || len(store.claims.records) != 1 {
		t.Fatalf("recovery replay changed another claim: %+v", current.Requests)
	}
	if err := next(); err != nil || len(store.claims.records) != 0 {
		t.Fatalf("successful release retained local proof: %v", err)
	}
}
