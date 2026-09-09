//go:build linux || darwin

// Independent request contexts borrow only the admitted quota route, never
// another member's deadline, values or cancellation authority.
package server

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// The actual writer reuses one census even though each operation retains its
// own context identity. Neither quota nor a request value comes from a stub.
func TestLocalBlobBatchContextKeepsMemberDeadlineValuesAndCensus(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	store := NewLocalBlobStoreWithMaxBytes(root, "synthetic", 5).(*localBlobStore)
	scans := 0
	store.afterUsageScanEntryForTest = func(path string) {
		if path == root {
			scans++
		}
	}
	type memberKey struct{}
	type ownerKey struct{}
	batch, err := BeginLocalBlobWriteBatch(context.WithValue(t.Context(), ownerKey{}, "quota-only"), []BlobStore{store}, 2)
	if err != nil || batch == nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = batch.Close() })
	deadline := time.Now().Add(time.Hour)
	first, cancel := context.WithDeadline(context.WithValue(t.Context(), memberKey{}, "first"), deadline)
	defer cancel()
	second := context.WithValue(t.Context(), memberKey{}, "second")
	for index, member := range []context.Context{first, second} {
		bound, err := batch.ContextFor(member)
		if err != nil {
			t.Fatal(err)
		}
		wantDeadline, wantTimed := member.Deadline()
		gotDeadline, gotTimed := bound.Deadline()
		if gotTimed != wantTimed || !gotDeadline.Equal(wantDeadline) || bound.Done() != member.Done() || bound.Value(memberKey{}) != member.Value(memberKey{}) || bound.Value(ownerKey{}) != nil {
			t.Fatal("quota binding replaced a member's independent context")
		}
		content := []string{"abc", "de"}[index]
		if created, err := store.PutIfAbsent(bound, []string{"synthetic/first.json", "synthetic/second.json"}[index], localBlobUsageSource(t, content), "application/json"); err != nil || !created {
			t.Fatal(created, err)
		}
	}
	if scans != 1 {
		t.Fatalf("independent contexts repeated the quota census: %d", scans)
	}
	if err := batch.Close(); err != nil || scans != 2 {
		t.Fatalf("final census differs: %d/%v", scans, err)
	}
	assertLocalBlobCapacityWinner(t, store, "synthetic/first.json", "abc", 5)
	assertLocalBlobCapacityWinner(t, store, "synthetic/second.json", "de", 5)
}

// A canceled copied member cannot commit, consume bytes, or cancel a peer;
// its attempted write still consumes one of the fixed operation slots.
func TestLocalBlobBatchContextCancelsOneMemberWithoutCancelingPeer(t *testing.T) {
	store := NewLocalBlobStoreWithMaxBytes(t.TempDir(), "synthetic", 5).(*localBlobStore)
	batch, err := BeginLocalBlobWriteBatch(t.Context(), []BlobStore{store}, 2)
	if err != nil || batch == nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = batch.Close() })
	member, cancel := context.WithCancel(t.Context())
	defer cancel()
	bound, err := batch.ContextFor(member)
	if err != nil {
		t.Fatal(err)
	}
	copied := false
	store.beforeCreateCommitForTest = func() { copied = true; cancel() }
	if created, err := store.PutIfAbsent(bound, "synthetic/canceled.json", localBlobUsageSource(t, "12345"), "application/json"); created || !copied || !errors.Is(err, context.Canceled) {
		t.Fatal("canceled independent member committed bytes", created, copied, err)
	}
	store.beforeCreateCommitForTest = nil
	peer, err := batch.ContextFor(t.Context())
	if err != nil || peer.Err() != nil || batch.Context().Err() != nil {
		t.Fatal("member canceled its quota peer", err)
	}
	if created, err := store.PutIfAbsent(peer, "synthetic/peer.json", localBlobUsageSource(t, "12345"), "application/json"); err != nil || !created {
		t.Fatal(created, err)
	}
	if created, err := store.PutIfAbsent(peer, "synthetic/peer.json", localBlobUsageSource(t, "12345"), "application/json"); err == nil || created {
		t.Fatal("cancellation refunded an attempt slot", created, err)
	}
	if err := batch.Close(); err != nil {
		t.Fatal(err)
	}
	assertLocalBlobCapacityWinner(t, store, "synthetic/peer.json", "12345", 5)
}

// Binding cannot authorize another batch, store, mutable API or future write.
func TestLocalBlobBatchContextRejectsForeignAndClosedOwners(t *testing.T) {
	store := NewLocalBlobStoreWithMaxBytes(t.TempDir(), "synthetic", 5).(*localBlobStore)
	other := NewLocalBlobStoreWithMaxBytes(t.TempDir(), "other", 5).(*localBlobStore)
	batch, err := BeginLocalBlobWriteBatch(t.Context(), []BlobStore{store}, 1)
	if err != nil || batch == nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = batch.Close() })
	foreign, err := BeginLocalBlobWriteBatch(t.Context(), []BlobStore{other}, 1)
	if err != nil || foreign == nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = foreign.Close() })
	if bound, err := batch.ContextFor(foreign.Context()); err == nil || bound != nil {
		t.Fatal("foreign batch was silently replaced")
	}
	if bound, err := batch.ContextFor(nil); err == nil || bound != nil {
		t.Fatal("nil caller acquired a quota context")
	}
	var absent *LocalBlobWriteBatch
	if bound, err := absent.ContextFor(t.Context()); err == nil || bound != nil {
		t.Fatal("nil batch acquired a quota context")
	}
	if bound, err := (&LocalBlobWriteBatch{}).ContextFor(t.Context()); err == nil || bound != nil {
		t.Fatal("uninitialized batch acquired a quota context")
	}
	member, err := batch.ContextFor(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	undeclared := NewLocalBlobStoreWithMaxBytes(store.root, "foreign", 5)
	source := localBlobUsageSource(t, "12345")
	if created, err := undeclared.PutIfAbsent(member, "foreign/object.json", source, "application/json"); err == nil || created {
		t.Fatal("bound context authorized an undeclared store")
	}
	if err := store.Put(member, "synthetic/object.json", source, "application/json"); err == nil {
		t.Fatal("bound context authorized mutable quota reuse")
	}
	if nested, err := BeginLocalBlobWriteBatch(member, []BlobStore{store}, 1); err == nil || nested != nil {
		t.Fatal("bound context hid a nested owner")
	}
	if err := batch.Close(); err != nil {
		t.Fatal(err)
	}
	if created, err := store.PutIfAbsent(member, "synthetic/object.json", source, "application/json"); err == nil || created {
		t.Fatal("bound context resurrected its closed owner")
	}
}

// A member's independent lifetime cannot outlive the quota owner's authority.
func TestLocalBlobBatchContextRetainsParentCancellation(t *testing.T) {
	store := NewLocalBlobStoreWithMaxBytes(t.TempDir(), "synthetic", 5).(*localBlobStore)
	owner, cancel := context.WithCancel(t.Context())
	defer cancel()
	batch, err := BeginLocalBlobWriteBatch(owner, []BlobStore{store}, 1)
	if err != nil || batch == nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = batch.Close() })
	member, err := batch.ContextFor(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	if member.Err() != nil {
		t.Fatal("quota cancellation overwrote independent caller identity")
	}
	if created, err := store.PutIfAbsent(member, "synthetic/object.json", localBlobUsageSource(t, "12345"), "application/json"); created || !errors.Is(err, context.Canceled) {
		t.Fatal("member escaped canceled quota owner", created, err)
	}
	if bound, err := batch.ContextFor(t.Context()); bound != nil || !errors.Is(err, context.Canceled) {
		t.Fatal("canceled quota rebound a new member", err)
	}
	if err := batch.Close(); !errors.Is(err, context.Canceled) {
		t.Fatal("final close lost owner cancellation", err)
	}
}

// Cancel the quota owner at the actual post-copy/pre-link boundary while the
// member remains live. Checking the quota context only at admission is too early.
func TestLocalBlobBatchContextRechecksOwnerAtActualCommit(t *testing.T) {
	root := t.TempDir()
	store := NewLocalBlobStoreWithMaxBytes(root, "synthetic", 5).(*localBlobStore)
	owner, cancel := context.WithCancel(t.Context())
	defer cancel()
	batch, err := BeginLocalBlobWriteBatch(owner, []BlobStore{store}, 1)
	if err != nil || batch == nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = batch.Close() })
	member, err := batch.ContextFor(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	copied := false
	store.beforeCreateCommitForTest = func() {
		copied = true
		cancel()
	}
	source := localBlobUsageSource(t, "12345")
	created, err := store.PutIfAbsent(member, "synthetic/canceled.json", source, "application/json")
	if created || !copied || !errors.Is(err, context.Canceled) || member.Err() != nil {
		t.Fatalf("independent member escaped the quota owner's commit cancellation: created=%t copied=%t error=%v member=%v", created, copied, err, member.Err())
	}
	if err := batch.Close(); !errors.Is(err, context.Canceled) {
		t.Fatal("final census lost the canceled quota owner", err)
	}
	if entries, err := os.ReadDir(filepath.Join(root, "synthetic")); err != nil || len(entries) != 0 {
		t.Fatalf("canceled quota left committed or staged bytes: %v %v", entries, err)
	}
	store.beforeCreateCommitForTest = nil
	if created, err := store.PutIfAbsent(t.Context(), "synthetic/recovered.json", source, "application/json"); err != nil || !created {
		t.Fatal("canceled quota retained its root owner or byte debit", created, err)
	}
	assertLocalBlobCapacityWinner(t, store, "synthetic/recovered.json", "12345", 5)
}

// Hold the actual operation token before canceling its owner. The independent
// member must observe quota cancellation even though its own Done remains open.
func TestLocalBlobBatchContextCanceledOwnerCannotWaitForOperationToken(t *testing.T) {
	store := NewLocalBlobStoreWithMaxBytes(t.TempDir(), "synthetic", 5).(*localBlobStore)
	owner, cancel := context.WithCancel(t.Context())
	defer cancel()
	batch, err := BeginLocalBlobWriteBatch(owner, []BlobStore{store}, 1)
	if err != nil || batch == nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = batch.Close() })
	// This broad guard only bounds a broken implementation's liveness. The
	// assertion distinguishes owner cancellation from member deadline expiry.
	memberOwner, cancelMember := context.WithTimeout(t.Context(), time.Minute)
	defer cancelMember()
	member, err := batch.ContextFor(memberOwner)
	if err != nil {
		t.Fatal(err)
	}
	<-batch.operation
	defer func() { batch.operation <- struct{}{} }()
	cancel()
	created, err := store.PutIfAbsent(member, "synthetic/refused.json", localBlobUsageSource(t, "12345"), "application/json")
	if created || !errors.Is(err, context.Canceled) || member.Err() != nil {
		t.Fatalf("operation token wait ignored its canceled quota owner: created=%t error=%v member=%v", created, err, member.Err())
	}
}
