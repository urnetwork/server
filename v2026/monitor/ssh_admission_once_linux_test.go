package monitor

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"
)

func TestSharedSshOnceAttachmentPreservesWatcherAndClaims(t *testing.T) {
	store, initial := newSyntheticSshAdmissionStore(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	releaseWatch, err := store.acquire(ctx, initial.Watcher, initial.Watcher.Identity, sshAdmissionWatch, []string{"a.example"})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := releaseWatch(); err != nil {
			t.Error(err)
		}
	}()
	before, err := os.ReadFile(filepath.Join(store.directory, "queue.json"))
	if err != nil {
		t.Fatal(err)
	}
	owner := syntheticSshAdmissionOwner(202)
	backend, err := attachSharedSshDiagnosticAdmission(ctx, store, owner, initial.Hosts)
	if err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(filepath.Join(store.directory, "queue.json"))
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("diagnostic attachment rewrote watcher registration or existing claims")
	}
	release, err := backend.acquire(ctx, "b.example")
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := release(); err != nil {
			t.Error(err)
		}
	}()
	if err := store.update(ctx, func(state *sshAdmissionState, _ int64) error {
		if state.Watcher != initial.Watcher || !slices.Equal(state.Hosts, initial.Hosts) || len(state.Requests) != 2 {
			t.Fatal("diagnostic replaced watcher or its active work")
		}
		request := state.Requests[1]
		if request.Class != sshAdmissionDiagnostic || request.Owner != owner || !request.Active {
			t.Fatal("one-shot transport did not take the existing diagnostic class")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestSharedSshOnceAttachmentRejectsUnknownAuthority(t *testing.T) {
	for _, name := range []string{"missing", "dead", "gone", "unknown", "boot", "inventory", "same-unit", "same-cgroup"} {
		store, initial := newSyntheticSshAdmissionStore(t)
		owner := syntheticSshAdmissionOwner(202)
		hosts := slices.Clone(initial.Hosts)
		switch name {
		case "missing":
			if err := os.Remove(filepath.Join(store.directory, "queue.json")); err != nil {
				t.Fatal(err)
			}
		case "dead", "gone", "unknown":
			status := sshAdmissionOwnerDead
			if name == "gone" {
				status = sshAdmissionOwnerGone
			}
			if name == "unknown" {
				status = sshAdmissionOwnerUnknown
			}
			store.status = func(context.Context, sshAdmissionOwner) sshAdmissionOwnerStatus { return status }
		case "boot":
			owner.Identity.BootId = "00000000-0000-0000-0000-000000000002"
		case "inventory":
			hosts = hosts[:len(hosts)-1]
		case "same-unit":
			owner.Unit = initial.Watcher.Unit
		case "same-cgroup":
			owner.Cgroup = initial.Watcher.Cgroup
		}
		if _, err := attachSharedSshDiagnosticAdmission(context.Background(), store, owner, hosts); err == nil {
			t.Fatalf("%s diagnostic attachment acquired unknown/shared authority", name)
		}
	}
}

func TestSharedSshOnceReplacementNeverRebindsDiagnostic(t *testing.T) {
	for _, name := range []string{"replacement", "reused-pid", "dead-after-attach"} {
		store, initial := newSyntheticSshAdmissionStore(t)
		backend, err := attachSharedSshDiagnosticAdmission(context.Background(), store, syntheticSshAdmissionOwner(202), initial.Hosts)
		if err != nil {
			t.Fatal(err)
		}
		if name == "dead-after-attach" {
			store.status = func(context.Context, sshAdmissionOwner) sshAdmissionOwnerStatus { return sshAdmissionOwnerDead }
		} else if err := store.update(context.Background(), func(state *sshAdmissionState, _ int64) error {
			if name == "replacement" {
				state.Watcher = syntheticSshAdmissionOwner(303)
			} else {
				state.Watcher.Identity.StartTicks++
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		if release, err := backend.acquire(context.Background(), "a.example"); err == nil {
			_ = release()
			t.Fatalf("%s diagnostic silently rebound to another watcher generation", name)
		}
	}
}

func TestSharedSshOnceTransportRetainsLimitsAndJoinedRelease(t *testing.T) {
	store, initial := newSyntheticSshAdmissionStore(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var watchReleases []func() error
	defer func() {
		for _, release := range watchReleases {
			if release != nil {
				if err := release(); err != nil {
					t.Error(err)
				}
			}
		}
	}()
	for _, host := range []string{"a.example", "b.example", "c.example", "d.example"} {
		release, err := store.acquire(ctx, initial.Watcher, initial.Watcher.Identity, sshAdmissionWatch, []string{host})
		if err != nil {
			t.Fatal(err)
		}
		watchReleases = append(watchReleases, release)
	}
	backend, err := attachSharedSshDiagnosticAdmission(ctx, store, syntheticSshAdmissionOwner(202), initial.Hosts)
	if err != nil {
		t.Fatal(err)
	}
	other, err := attachSharedSshDiagnosticAdmission(ctx, store, syntheticSshAdmissionOwner(303), initial.Hosts)
	if err != nil {
		t.Fatal(err)
	}
	published := make(chan struct{}, 1)
	store.afterPublish = func() error {
		select {
		case published <- struct{}{}:
		default:
		}
		return nil
	}
	started, finish := make(chan struct{}), make(chan struct{})
	var finishOnce sync.Once
	joinTransport := func() { finishOnce.Do(func() { close(finish) }) }
	defer joinTransport()
	runner := newRunner(&monitorConfig{addressMode: addressModeOverlay, sshUser: "synthetic", commandTimeout: time.Minute, sharedSshAdmission: backend})
	runner.runSSH = func(commandCtx context.Context, _ []string, _ string) (string, string, error) {
		close(started)
		<-finish
		return "", "", commandCtx.Err()
	}
	callCtx, cancelCall := context.WithCancel(ctx)
	defer cancelCall()
	result := make(chan error, 1)
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, err := runner.ssh(callCtx, &host{name: "e.example", overlayIp: "192.0.2.10"}, "true", "")
		result <- err
	}()
	defer func() { cancelCall(); joinTransport(); <-done }()
	select {
	case <-published:
	case <-ctx.Done():
		t.Fatal("diagnostic did not publish its causal queue barrier")
	}
	select {
	case <-started:
		t.Fatal("one-shot bypassed global four-command limit")
	default:
	}
	if err := store.update(ctx, func(state *sshAdmissionState, _ int64) error {
		if len(state.Requests) != 5 || state.Requests[4].Class != sshAdmissionDiagnostic || state.Requests[4].Active {
			t.Fatal("diagnostic did not queue behind four active commands")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if release, err := other.acquire(ctx, "e.example"); err == nil {
		_ = release()
		t.Fatal("second diagnostic bypassed exclusive turn")
	}
	if err := watchReleases[0](); err != nil {
		t.Fatal(err)
	}
	watchReleases[0] = nil
	select {
	case <-started:
	case <-ctx.Done():
		t.Fatal("diagnostic never entered transport after capacity release")
	}
	cancelCall()
	if err := store.update(ctx, func(state *sshAdmissionState, _ int64) error {
		found := false
		for _, request := range state.Requests {
			if request.Class == sshAdmissionDiagnostic {
				found = request.Active
			}
		}
		if !found {
			t.Fatal("cancellation released diagnostic before transport joined")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	joinTransport()
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("original cancellation lost: %v", err)
		}
	case <-ctx.Done():
		t.Fatal("canceled diagnostic transport did not join")
	}
	<-done
	store.afterPublish = nil
	if err := store.update(ctx, func(state *sshAdmissionState, _ int64) error {
		if state.Watcher != initial.Watcher || len(state.Requests) != 3 {
			t.Fatal("joined diagnostic erased watcher claims or retained its own turn")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	release, err := other.acquire(ctx, "e.example")
	if err != nil {
		t.Fatal(err)
	}
	if err := release(); err != nil {
		t.Fatal(err)
	}
}
