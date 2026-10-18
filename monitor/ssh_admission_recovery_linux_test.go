// Real private queue files force failed cleanup at the transport ownership
// boundary. All owners, hosts and clocks are synthetic; no command contacts a host.
package monitor

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// Reads the published queue without rewriting it or changing admission.
func readRecoverySshState(t *testing.T, store *sshAdmissionStore) sshAdmissionState {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(store.directory, "queue.json"))
	if err != nil {
		t.Fatal(err)
	}
	var state sshAdmissionState
	if err := sshAdmissionDecode(raw, &state); err != nil {
		t.Fatal(err)
	}
	return state
}

// The store validates exactly0600 even under root. Keep the same lock inode
// while forcing removal to fail before it can read or mutate state.
func setRecoverySshLockMode(t *testing.T, store *sshAdmissionStore, mode os.FileMode) {
	t.Helper()
	if err := os.Chmod(filepath.Join(store.directory, "queue.lock"), mode); err != nil {
		t.Fatal(err)
	}
}

// The transport returned its join boundary, but one failed release discarded
// the only local cleanup owner and left a live watcher's active claim forever.
func TestSharedSshAdmissionRecoversFailedJoinedRelease(t *testing.T) {
	store, state := newSyntheticSshAdmissionStore(t)
	owner := state.Watcher
	release, err := store.acquire(context.Background(), owner, owner.Identity, sshAdmissionWatch, []string{"a.example"})
	if err != nil {
		t.Fatal(err)
	}
	setRecoverySshLockMode(t, store, 0644)
	if err := release(); err == nil {
		t.Fatal("invalid lock did not force failed removal")
	}
	setRecoverySshLockMode(t, store, 0600)
	nextRelease, err := store.acquire(context.Background(), owner, owner.Identity, sshAdmissionWatch, []string{"b.example"})
	if err != nil {
		t.Fatal(err)
	}
	defer nextRelease()
	current := readRecoverySshState(t, store)
	if len(current.Requests) != 1 || current.Requests[0].Hosts[0] != "b.example" {
		t.Fatalf("joined release lost its cleanup owner: %+v", current.Requests)
	}
}

// Cancellation after the grant is published is still unstarted work. Failed
// cleanup must remain owned even though no release callback reached the runner.
func TestSharedSshAdmissionRecoversCanceledPublishedGrant(t *testing.T) {
	store, state := newSyntheticSshAdmissionStore(t)
	owner := state.Watcher
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	store.status = func(context.Context, sshAdmissionOwner) sshAdmissionOwnerStatus {
		setRecoverySshLockMode(t, store, 0644)
		cancel()
		return sshAdmissionOwnerLive
	}
	if release, err := store.acquire(ctx, owner, owner.Identity, sshAdmissionWatch, []string{"a.example"}); release != nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("published cancellation returned transport ownership: release=%t err=%v", release != nil, err)
	}
	current := readRecoverySshState(t, store)
	if len(current.Requests) != 1 || !current.Requests[0].Active {
		t.Fatal("control did not publish a grant before failed cleanup")
	}
	setRecoverySshLockMode(t, store, 0600)
	store.status = func(context.Context, sshAdmissionOwner) sshAdmissionOwnerStatus { return sshAdmissionOwnerLive }
	release, err := store.acquire(context.Background(), owner, owner.Identity, sshAdmissionWatch, []string{"b.example"})
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	current = readRecoverySshState(t, store)
	if len(current.Requests) != 1 || current.Requests[0].Hosts[0] != "b.example" {
		t.Fatalf("unstarted grant lost its cleanup owner: %+v", current.Requests)
	}
}

// A new store has no proof that an old live owner's work joined. Neither
// expired admission clocks nor a new acquisition may infer that proof.
func TestSharedSshAdmissionFreshStorePreservesUnknownClaim(t *testing.T) {
	store, state := newSyntheticSshAdmissionStore(t)
	owner := state.Watcher
	release, err := store.acquire(context.Background(), owner, owner.Identity, sshAdmissionWatch, []string{"a.example"})
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	if err := store.update(context.Background(), func(current *sshAdmissionState, _ int64) error {
		current.Requests[0].ExpiresNanos = 1
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	fresh, err := newSshAdmissionStore(store.directory, store.bootId)
	if err != nil {
		t.Fatal(err)
	}
	fresh.status = store.status
	nextRelease, err := fresh.acquire(context.Background(), owner, owner.Identity, sshAdmissionWatch, []string{"b.example"})
	if err != nil {
		t.Fatal(err)
	}
	defer nextRelease()
	if current := readRecoverySshState(t, store); len(current.Requests) != 2 {
		t.Fatalf("unknown active claim was freed: %+v", current.Requests)
	}
}

// Cancellation does not release an admitted transport until its owned callback
// returns. The barrier represents that existing runner join contract.
func TestSharedSshAdmissionCanceledTransportRetainsClaimUntilReturn(t *testing.T) {
	store, state := newSyntheticSshAdmissionStore(t)
	owner := state.Watcher
	runner := newRunner(&monitorConfig{addressMode: addressModeOverlay, sharedSshAdmission: &sharedSshAdmission{store: store, owner: owner}})
	entered := make(chan struct{})
	finish := make(chan struct{})
	runner.runSSH = func(context.Context, []string, string) (string, string, error) {
		close(entered)
		<-finish
		return "", "", nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	returned := make(chan error, 1)
	go func() {
		_, err := runner.ssh(ctx, &host{name: "a.example", overlayIp: "192.0.2.19"}, "true", "")
		returned <- err
	}()
	defer func() {
		close(finish)
		if err := <-returned; !errors.Is(err, context.Canceled) {
			t.Errorf("transport cancellation lost: %v", err)
		}
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("transport did not reach entry barrier")
	}
	cancel()
	release, err := store.acquire(context.Background(), owner, owner.Identity, sshAdmissionWatch, []string{"b.example"})
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	if current := readRecoverySshState(t, store); len(current.Requests) != 2 {
		t.Fatalf("cancellation freed an unjoined transport: %+v", current.Requests)
	}
}

// Recovery requires both the token and the complete original owner identity.
func TestSharedSshAdmissionRecoveryPreservesOwnerMismatch(t *testing.T) {
	store, state := newSyntheticSshAdmissionStore(t)
	owner := syntheticSshAdmissionOwner(202)
	release, err := store.acquire(context.Background(), owner, state.Watcher.Identity, sshAdmissionDiagnostic, []string{"a.example"})
	if err != nil {
		t.Fatal(err)
	}
	setRecoverySshLockMode(t, store, 0644)
	if err := release(); err == nil {
		t.Fatal("control removal succeeded")
	}
	setRecoverySshLockMode(t, store, 0600)
	replacement := syntheticSshAdmissionOwner(303)
	if err := store.update(context.Background(), func(current *sshAdmissionState, _ int64) error {
		current.Requests[0].Owner = replacement
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if next, err := store.acquire(context.Background(), owner, state.Watcher.Identity, sshAdmissionDiagnostic, []string{"b.example"}); err == nil {
		next()
		t.Fatal("mismatched claim was admitted")
	}
	if current := readRecoverySshState(t, store); len(current.Requests) != 1 || current.Requests[0].Owner != replacement {
		t.Fatalf("recovery removed another owner: %+v", current.Requests)
	}
}
