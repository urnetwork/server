package monitor

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func readSshRecoveryState(t *testing.T, store *sshAdmissionStore) sshAdmissionState {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(store.directory, "queue.json"))
	if err != nil {
		t.Fatal(err)
	}
	var state sshAdmissionState
	if err := sshAdmissionDecode(raw, &state); err != nil {
		t.Fatal(err)
	}
	if err := state.validate(); err != nil {
		t.Fatal(err)
	}
	return state
}

// Hold the real stable lock. The explicit lock-wait seam cancels cleanup only
// after its failed flock, so scheduler timing and the one-second timer are not
// the failure oracle. No native command or production identity is involved.
func failSshRecoveryReleaseUnderLock(t *testing.T, store *sshAdmissionStore, release func() error) {
	t.Helper()
	lock, err := os.OpenFile(filepath.Join(store.directory, "queue.lock"), os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	if err := unix.Flock(int(lock.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	defer unix.Flock(int(lock.Fd()), unix.LOCK_UN)
	cleanup, cancel := context.WithCancel(context.Background())
	defer cancel()
	store.cleanupContext = func() (context.Context, context.CancelFunc) { return cleanup, func() {} }
	lockObserved := false
	store.lockWait = func(ctx context.Context) error {
		lockObserved = true
		cancel()
		return ctx.Err()
	}
	defer func() { store.cleanupContext = nil; store.lockWait = nil }()
	if err := release(); !errors.Is(err, context.Canceled) || !lockObserved {
		t.Fatal("cleanup did not fail at the deliberately held queue lock")
	}
}

func TestSshAdmissionRecoveryRetriesJoinedReleaseAfterHeldLock(t *testing.T) {
	store, state := newSyntheticSshAdmissionStore(t)
	owner := state.Watcher
	release, err := store.acquire(context.Background(), owner, owner.Identity, sshAdmissionWatch, []string{"a.example"})
	if err != nil {
		t.Fatal(err)
	}
	failSshRecoveryReleaseUnderLock(t, store, release)
	stranded := readSshRecoveryState(t, store)
	if len(stranded.Requests) != 1 || !stranded.Requests[0].Active {
		t.Fatal("failed cleanup did not retain its original active reservation")
	}
	nextRelease, err := store.acquire(context.Background(), owner, owner.Identity, sshAdmissionWatch, []string{"b.example"})
	if err != nil {
		t.Fatal(err)
	}
	defer nextRelease()
	current := readSshRecoveryState(t, store)
	if len(current.Requests) != 1 || current.Requests[0].Ticket == stranded.Requests[0].Ticket {
		t.Fatal("future acquisition forgot the failed joined-release ownership")
	}
}

func TestSshAdmissionRecoveryDoesNotReclaimUnreleasedExpiredActive(t *testing.T) {
	store, state := newSyntheticSshAdmissionStore(t)
	owner := state.Watcher
	release, err := store.acquire(context.Background(), owner, owner.Identity, sshAdmissionWatch, []string{"a.example"})
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	original := readSshRecoveryState(t, store).Requests[0]
	store.now = func() (int64, error) { return original.ExpiresNanos + int64(time.Hour), nil }
	nextRelease, err := store.acquire(context.Background(), owner, owner.Identity, sshAdmissionWatch, []string{"b.example"})
	if err != nil {
		t.Fatal(err)
	}
	defer nextRelease()
	current := readSshRecoveryState(t, store)
	if len(current.Requests) != 2 || current.Requests[0].Ticket != original.Ticket || !current.Requests[0].Active {
		t.Fatal("same live owner or expiry reclaimed work before its release boundary")
	}
}

func TestSshAdmissionRecoveryCancellationAfterPublishedGrantNeverStartsTransport(t *testing.T) {
	store, state := newSyntheticSshAdmissionStore(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	store.afterPublish = func() error { cancel(); return nil }
	store.cleanupContext = func() (context.Context, context.CancelFunc) { return ctx, func() {} }
	runner := newRunner(&monitorConfig{addressMode: addressModeOverlay, sharedSshAdmission: &sharedSshAdmission{store: store, owner: state.Watcher}})
	runner.runSSH = func(context.Context, []string, string) (string, string, error) {
		t.Fatal("canceled acquire handed a published grant to native transport")
		return "", "", nil
	}
	_, err := runner.ssh(ctx, &host{name: "a.example", overlayIp: "192.0.2.10"}, "true", "")
	if !errors.Is(err, context.Canceled) {
		t.Fatal("cancellation lost its original source identity")
	}
	store.afterPublish = nil
	store.cleanupContext = nil
	if current := readSshRecoveryState(t, store); len(current.Requests) != 1 || !current.Requests[0].Active {
		t.Fatal("control did not publish a grant before the canceled cleanup")
	}
	release, err := store.acquire(context.Background(), state.Watcher, state.Watcher.Identity, sshAdmissionWatch, []string{"b.example"})
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	if current := readSshRecoveryState(t, store); len(current.Requests) != 1 || current.Requests[0].Hosts[0] != "b.example" {
		t.Fatal("failed never-started acquisition lost its recovery ownership")
	}
}

func TestSshAdmissionRecoveryAmbiguousPublishedGrantRetainsCleanupProof(t *testing.T) {
	store, state := newSyntheticSshAdmissionStore(t)
	publicationErr := errors.New("synthetic publication acknowledgement unavailable")
	cleanup, cancel := context.WithCancel(context.Background())
	cancel()
	store.cleanupContext = func() (context.Context, context.CancelFunc) { return cleanup, func() {} }
	store.afterPublish = func() error { return publicationErr }
	_, err := store.acquire(context.Background(), state.Watcher, state.Watcher.Identity, sshAdmissionWatch, []string{"a.example"})
	if !errors.Is(err, publicationErr) || !errors.Is(err, context.Canceled) {
		t.Fatal("ambiguous publication or failed cleanup was discarded")
	}
	store.cleanupContext = nil
	store.afterPublish = nil
	if current := readSshRecoveryState(t, store); len(current.Requests) != 1 || !current.Requests[0].Active {
		t.Fatal("synthetic ambiguous publication did not retain a real active claim")
	}
	release, err := store.acquire(context.Background(), state.Watcher, state.Watcher.Identity, sshAdmissionWatch, []string{"b.example"})
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	if current := readSshRecoveryState(t, store); len(current.Requests) != 1 || current.Requests[0].Hosts[0] != "b.example" {
		t.Fatal("ambiguous enrollment publication lost its recovery ownership")
	}
}

func TestSshAdmissionRecoveryFailedRemovalPublicationKeepsProof(t *testing.T) {
	store, state := newSyntheticSshAdmissionStore(t)
	release, err := store.acquire(context.Background(), state.Watcher, state.Watcher.Identity, sshAdmissionWatch, []string{"a.example"})
	if err != nil {
		t.Fatal(err)
	}
	publicationErr := errors.New("synthetic removal acknowledgement unavailable")
	store.afterPublish = func() error { return publicationErr }
	if err := release(); !errors.Is(err, publicationErr) {
		t.Fatal("failed removal acknowledgement was hidden")
	}
	store.afterPublish = nil
	retained := func() int {
		store.claims.mu.Lock()
		defer store.claims.mu.Unlock()
		return len(store.claims.records)
	}()
	if retained != 1 || len(readSshRecoveryState(t, store).Requests) != 0 {
		t.Fatal("uncertain removal publication discarded its retry proof")
	}
	if err := release(); err != nil {
		t.Fatal(err)
	}
	if len(store.claims.records) != 0 {
		t.Fatal("acknowledged absent claim did not retire its recovery proof")
	}
}

func TestSshAdmissionRecoveryLocalRecordsBoundedAndFailClosed(t *testing.T) {
	store, state := newSyntheticSshAdmissionStore(t)
	for i := 0; i < sshAdmissionQueueLimit; i++ {
		if _, err := store.trackClaim(fmt.Sprintf("%032x", i), state.Watcher); err != nil {
			t.Fatal(err)
		}
	}
	_, err := store.acquire(context.Background(), state.Watcher, state.Watcher.Identity, sshAdmissionWatch, []string{"a.example"})
	if err == nil || len(store.claims.records) != sshAdmissionQueueLimit || len(readSshRecoveryState(t, store).Requests) != 0 {
		t.Fatal("full local ownership records were overwritten or bypassed")
	}
}

func TestSshAdmissionRecoveryFreshStoreDoesNotInventJoinProof(t *testing.T) {
	store, state := newSyntheticSshAdmissionStore(t)
	release, err := store.acquire(context.Background(), state.Watcher, state.Watcher.Identity, sshAdmissionWatch, []string{"a.example"})
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	fresh, err := newSshAdmissionStore(store.directory, state.BootId)
	if err != nil {
		t.Fatal(err)
	}
	fresh.status = func(context.Context, sshAdmissionOwner) sshAdmissionOwnerStatus { return sshAdmissionOwnerLive }
	releaseNext, err := fresh.acquire(context.Background(), state.Watcher, state.Watcher.Identity, sshAdmissionWatch, []string{"b.example"})
	if err != nil {
		t.Fatal(err)
	}
	defer releaseNext()
	if len(readSshRecoveryState(t, store).Requests) != 2 {
		t.Fatal("fresh store inferred native closure from stored owner identity")
	}
}

func TestSshAdmissionRecoveryRunnerKeepsClaimUntilTransportReturns(t *testing.T) {
	store, state := newSyntheticSshAdmissionStore(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	entered := make(chan struct{})
	cancellationObserved := make(chan struct{})
	joined := make(chan struct{})
	var joinOnce sync.Once
	allowJoin := func() { joinOnce.Do(func() { close(joined) }) }
	defer allowJoin()
	runner := newRunner(&monitorConfig{addressMode: addressModeOverlay, sharedSshAdmission: &sharedSshAdmission{store: store, owner: state.Watcher}})
	runner.runSSH = func(commandCtx context.Context, _ []string, _ string) (string, string, error) {
		close(entered)
		<-commandCtx.Done()
		close(cancellationObserved)
		<-joined
		return "", "", commandCtx.Err()
	}
	result := make(chan error, 1)
	go func() {
		_, err := runner.ssh(ctx, &host{name: "a.example", overlayIp: "192.0.2.10"}, "true", "")
		result <- err
	}()
	resultConsumed := false
	defer func() {
		cancel()
		allowJoin()
		if !resultConsumed {
			select {
			case <-result:
			case <-time.After(5 * time.Second):
				t.Error("transport fixture did not join during cleanup")
			}
		}
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("transport did not reach its ownership barrier")
	}
	cancel()
	select {
	case <-cancellationObserved:
	case <-time.After(5 * time.Second):
		t.Fatal("transport did not observe cancellation")
	}
	// Cancellation is acknowledged, but the injected transport deliberately has
	// not returned its joined boundary. This is not a native-process assertion.
	nextRelease, err := store.acquire(context.Background(), state.Watcher, state.Watcher.Identity, sshAdmissionWatch, []string{"b.example"})
	if err != nil {
		t.Fatal(err)
	}
	if len(readSshRecoveryState(t, store).Requests) != 2 {
		t.Fatal("cancellation made a still-owned transport claim recoverable")
	}
	if err := nextRelease(); err != nil {
		t.Fatal(err)
	}
	allowJoin()
	select {
	case err := <-result:
		resultConsumed = true
		if !errors.Is(err, context.Canceled) {
			t.Fatal("joined transport lost its original cancellation")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("transport did not return after its explicit join barrier")
	}
	if len(readSshRecoveryState(t, store).Requests) != 0 {
		t.Fatal("joined transport failed to release its exact reservation")
	}
}

func TestSshAdmissionRecoveryOwnerMismatchKeepsClaimReserved(t *testing.T) {
	store, state := newSyntheticSshAdmissionStore(t)
	release, err := store.acquire(context.Background(), state.Watcher, state.Watcher.Identity, sshAdmissionWatch, []string{"a.example"})
	if err != nil {
		t.Fatal(err)
	}
	other := syntheticSshAdmissionOwner(303)
	if err := store.update(context.Background(), func(current *sshAdmissionState, _ int64) error {
		current.Watcher = other
		current.Requests[0].Owner = other
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := release(); err == nil {
		t.Fatal("recovery accepted a different stored owner generation")
	}
	if current := readSshRecoveryState(t, store); len(current.Requests) != 1 || current.Requests[0].Owner != other {
		t.Fatal("owner mismatch removed a reservation")
	}
}

func TestSshAdmissionRecoveryFutureAcquireHasBoundedCleanupContext(t *testing.T) {
	store, state := newSyntheticSshAdmissionStore(t)
	release, err := store.acquire(context.Background(), state.Watcher, state.Watcher.Identity, sshAdmissionWatch, []string{"a.example"})
	if err != nil {
		t.Fatal(err)
	}
	failSshRecoveryReleaseUnderLock(t, store, release)
	lock, err := os.OpenFile(filepath.Join(store.directory, "queue.lock"), os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	if err := unix.Flock(int(lock.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	defer unix.Flock(int(lock.Fd()), unix.LOCK_UN)
	bounded := false
	store.lockWait = func(ctx context.Context) error {
		_, bounded = ctx.Deadline()
		return context.DeadlineExceeded
	}
	defer func() { store.lockWait = nil }()
	_, err = store.acquire(context.Background(), state.Watcher, state.Watcher.Identity, sshAdmissionWatch, []string{"b.example"})
	if !errors.Is(err, context.DeadlineExceeded) || !bounded {
		t.Fatal("old cleanup can park an acquisition with a cancellation-only parent")
	}
	if current := readSshRecoveryState(t, store); len(current.Requests) != 1 || !current.Requests[0].Active {
		t.Fatal("blocked recovery overwrote an old claim or published a new request")
	}
}
