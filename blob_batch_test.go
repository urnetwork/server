//go:build linux || darwin

package server

// Real public writes exercise finite root-lock reuse, fresh final accounting,
// competing owners and caller cancellation without scheduler timing assertions.

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// Count whole censuses at the existing real directory-walk hook, not at a
// replacement quota callback. The default public writer remains under test.
func TestLocalBlobBatchScansOnceUntilFinalCensus(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	store := NewLocalBlobStoreWithMaxBytes(root, "blob", 900).(*localBlobStore)
	scans := 0
	store.afterUsageScanEntryForTest = func(path string) {
		if path == root {
			scans++
		}
	}
	batch, err := BeginLocalBlobWriteBatch(t.Context(), []BlobStore{store}, 901)
	if err != nil || batch == nil {
		t.Fatalf("batch admission: %v", err)
	}
	t.Cleanup(func() { _ = batch.Close() })
	source := localBlobUsageSource(t, "x")
	for index := 0; index < 900; index++ {
		key := fmt.Sprintf("blob/object-%04d.json", index)
		if created, err := store.PutIfAbsent(batch.Context(), key, source, "application/json"); err != nil || !created {
			t.Fatalf("actual immutable write %d: %t/%v", index, created, err)
		}
	}
	if scans != 1 {
		t.Fatalf("batch repeated a complete root census: scans=%d, want 1 before close", scans)
	}
	if err := batch.Close(); err != nil || scans != 2 {
		t.Fatalf("final fresh census missing: scans=%d error=%v", scans, err)
	}
	if created, err := store.PutIfAbsent(batch.Context(), "blob/escaped.json", source, "application/json"); err == nil || created || !strings.Contains(err.Error(), "closed") {
		t.Fatalf("closed batch admitted a later write: %t/%v", created, err)
	}
	if created, err := store.PutIfAbsent(t.Context(), "blob/over.json", source, "application/json"); err == nil || created || scans != 3 {
		t.Fatalf("independent write reused the old census: %t/%v scans=%d", created, err, scans)
	}
}

// Aliases and separate prefixes share one physical owner. Collisions consume
// an attempt but zero byte credit; refusals do not reserve uncommitted bytes.
func TestLocalBlobBatchAccountsAliasesCollisionsAndExactLimit(t *testing.T) {
	root := t.TempDir()
	alias := filepath.Join(t.TempDir(), "alias")
	if err := os.Symlink(root, alias); err != nil {
		t.Fatal(err)
	}
	first := NewLocalBlobStoreWithMaxBytes(root, "first", 5).(*localBlobStore)
	second := NewLocalBlobStoreWithMaxBytes(alias, "second", 5).(*localBlobStore)
	batch, err := BeginLocalBlobWriteBatch(t.Context(), []BlobStore{second, first}, 4)
	if err != nil || batch == nil || len(batch.roots) != 1 {
		t.Fatalf("physical aliases did not share one owner: %+v %v", batch, err)
	}
	t.Cleanup(func() { _ = batch.Close() })
	if created, err := first.PutIfAbsent(batch.Context(), "first/winner.json", localBlobUsageSource(t, "1234"), "application/json"); err != nil || !created {
		t.Fatal("first exact immutable owner", created, err)
	}
	if created, err := second.PutIfAbsent(batch.Context(), "first/winner.json", localBlobUsageSource(t, "different"), "application/json"); err != nil || created {
		t.Fatal("collision invented a second write", created, err)
	}
	if created, err := second.PutIfAbsent(batch.Context(), "second/over.json", localBlobUsageSource(t, "56"), "application/json"); err == nil || created || !strings.Contains(err.Error(), "capacity exceeded") {
		t.Fatal("one-over shared capacity accepted", created, err)
	}
	if created, err := second.PutIfAbsent(batch.Context(), "second/exact.json", localBlobUsageSource(t, "5"), "application/json"); err != nil || !created {
		t.Fatal("failed admission retained byte credit", created, err)
	}
	if created, err := first.PutIfAbsent(batch.Context(), "first/winner.json", localBlobUsageSource(t, "1234"), "application/json"); err == nil || created || !strings.Contains(err.Error(), "count is exhausted") {
		t.Fatal("collision bypassed the finite attempt count", created, err)
	}
	if err := batch.Close(); err != nil {
		t.Fatal(err)
	}
	assertLocalBlobCapacityWinner(t, first, "first/winner.json", "1234", 5)
	assertLocalBlobCapacityWinner(t, second, "second/exact.json", "5", 5)
}

// Ordinary mutable writes and undeclared store instances must never bypass the
// existing root lock merely because a caller copied an admitted batch context.
func TestLocalBlobBatchRejectsForeignMutableAndNestedOwners(t *testing.T) {
	root := t.TempDir()
	store := NewLocalBlobStoreWithMaxBytes(root, "blob", 5).(*localBlobStore)
	foreign := NewLocalBlobStoreWithMaxBytes(root, "foreign", 5).(*localBlobStore)
	batch, err := BeginLocalBlobWriteBatch(t.Context(), []BlobStore{store}, 2)
	if err != nil || batch == nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = batch.Close() })
	source := localBlobUsageSource(t, "12345")
	if created, err := foreign.PutIfAbsent(batch.Context(), "foreign/object.json", source, "application/json"); err == nil || created {
		t.Fatal("undeclared store borrowed the batch owner", created, err)
	}
	if err := store.Put(batch.Context(), "blob/object.json", source, "application/json"); err == nil {
		t.Fatal("mutable replacement borrowed immutable batch accounting")
	}
	if nested, err := BeginLocalBlobWriteBatch(batch.Context(), []BlobStore{store}, 1); err == nil || nested != nil {
		t.Fatal("nested lease could wait on its own root", err)
	}
	if created, err := store.PutIfAbsent(batch.Context(), "blob/object.json", source, "application/json"); err != nil || !created {
		t.Fatal("foreign refusal damaged the admitted owner", created, err)
	}
	if err := batch.Close(); err != nil {
		t.Fatal(err)
	}
}

// A real competing instance either waits until final release then observes the
// committed quota, or cancellation joins it without staging a destination.
func TestLocalBlobBatchRetainsCompetingWriterAndCancellation(t *testing.T) {
	for _, cancelWaiter := range []bool{false, true} {
		root := t.TempDir()
		first := NewLocalBlobStoreWithMaxBytes(root, "first", 5).(*localBlobStore)
		second := NewLocalBlobStoreWithMaxBytes(root, "second", 5).(*localBlobStore)
		batch, err := BeginLocalBlobWriteBatch(t.Context(), []BlobStore{first}, 1)
		if err != nil || batch == nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = batch.Close() })
		if created, err := first.PutIfAbsent(batch.Context(), "first/object.json", localBlobUsageSource(t, "1234"), "application/json"); err != nil || !created {
			t.Fatal(created, err)
		}
		ctx, cancel := context.WithCancel(t.Context())
		t.Cleanup(cancel)
		contended, results := startLocalBlobCapacityContender(t, ctx, second, "second/object.json", localBlobUsageSource(t, "56"))
		select {
		case <-contended:
		case result := <-results:
			t.Fatalf("competing writer bypassed the live batch: %+v", result)
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
		if cancelWaiter {
			cancel()
		} else if err := batch.Close(); err != nil {
			t.Fatal(err)
		}
		result := <-results
		if result.created || result.err == nil || cancelWaiter && !errors.Is(result.err, context.Canceled) || !cancelWaiter && !strings.Contains(result.err.Error(), "capacity exceeded") {
			t.Fatalf("competing writer result: canceled=%t %+v", cancelWaiter, result)
		}
		if err := batch.Close(); err != nil {
			t.Fatal(err)
		}
		assertLocalBlobCapacityWinner(t, first, "first/object.json", "1234", 4)
	}
}

// Cancellation at the real post-copy/pre-link barrier still removes its stage,
// closes the batch and releases all quota for an independent fresh writer.
func TestLocalBlobBatchCancellationDiscardsUncommittedBytes(t *testing.T) {
	root := t.TempDir()
	store := NewLocalBlobStoreWithMaxBytes(root, "blob", 5).(*localBlobStore)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	batch, err := BeginLocalBlobWriteBatch(ctx, []BlobStore{store}, 1)
	if err != nil || batch == nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = batch.Close() })
	copied := false
	store.beforeCreateCommitForTest = func() { copied = true; cancel() }
	if created, err := store.PutIfAbsent(batch.Context(), "blob/canceled.json", localBlobUsageSource(t, "12345"), "application/json"); created || !copied || !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled copied bytes committed: %t/%t/%v", created, copied, err)
	}
	if err := batch.Close(); !errors.Is(err, context.Canceled) {
		t.Fatal("batch lost cancellation", err)
	}
	if entries, err := os.ReadDir(filepath.Join(root, "blob")); err != nil || len(entries) != 0 {
		t.Fatalf("canceled stage survived: %v %v", entries, err)
	}
	independent := NewLocalBlobStoreWithMaxBytes(root, "blob", 5)
	if created, err := independent.PutIfAbsent(t.Context(), "blob/recovered.json", localBlobUsageSource(t, "12345"), "application/json"); err != nil || !created {
		t.Fatal("canceled batch retained the root lock", created, err)
	}
}

// Scope completion cannot silently accept external filesystem growth or reuse
// its old total afterward; a fresh normal writer observes the actual overage.
func TestLocalBlobBatchFinalCensusRejectsUntrackedGrowth(t *testing.T) {
	root := t.TempDir()
	store := NewLocalBlobStoreWithMaxBytes(root, "blob", 5).(*localBlobStore)
	batch, err := BeginLocalBlobWriteBatch(t.Context(), []BlobStore{store}, 1)
	if err != nil || batch == nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = batch.Close() })
	if created, err := store.PutIfAbsent(batch.Context(), "blob/object.json", localBlobUsageSource(t, "1234"), "application/json"); err != nil || !created {
		t.Fatal(created, err)
	}
	if err := os.WriteFile(filepath.Join(root, "untracked.json"), []byte("56"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := batch.Close(); err == nil || !strings.Contains(err.Error(), "unaccounted bytes") {
		t.Fatal("untracked filesystem growth was accepted", err)
	}
	if created, err := store.PutIfAbsent(t.Context(), "blob/later.json", localBlobUsageSource(t, "x"), "application/json"); err == nil || created {
		t.Fatal("fresh independent admission reused stale batch bytes", created, err)
	}
}

// Retargeting a configured root alias cannot redirect an existing lease; both
// the next write and its final completion must retain that ownership failure.
func TestLocalBlobBatchRejectsRetargetedPhysicalRoot(t *testing.T) {
	root, other := t.TempDir(), t.TempDir()
	alias := filepath.Join(t.TempDir(), "alias")
	if err := os.Symlink(root, alias); err != nil {
		t.Fatal(err)
	}
	store := NewLocalBlobStoreWithMaxBytes(alias, "blob", 5).(*localBlobStore)
	batch, err := BeginLocalBlobWriteBatch(t.Context(), []BlobStore{store}, 1)
	if err != nil || batch == nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = batch.Close() })
	if err := os.Remove(alias); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(other, alias); err != nil {
		t.Fatal(err)
	}
	if created, err := store.PutIfAbsent(batch.Context(), "blob/escaped.json", localBlobUsageSource(t, "x"), "application/json"); err == nil || created || !strings.Contains(err.Error(), "changed physical root") {
		t.Fatal("retargeted alias redirected the batch", created, err)
	}
	if err := batch.Close(); err == nil || !strings.Contains(err.Error(), "changed physical root") {
		t.Fatal("final scope lost the root-identity refusal", err)
	}
	if entries, err := os.ReadDir(other); err != nil || len(entries) != 0 {
		t.Fatal("retargeted root acquired staged bytes", entries, err)
	}
	independent := NewLocalBlobStoreWithMaxBytes(root, "blob", 5)
	if created, err := independent.PutIfAbsent(t.Context(), "blob/recovered.json", localBlobUsageSource(t, "12345"), "application/json"); err != nil || !created {
		t.Fatal("failed root check retained original lock", created, err)
	}
}

// A backend wrapper which does not advertise a local source keeps the ordinary
// path. Admission of invalid finite bounds must not create any store root.
func TestLocalBlobBatchValidatesBoundsAndUnknownBackends(t *testing.T) {
	root := filepath.Join(t.TempDir(), "uncreated")
	store := NewLocalBlobStore(root, "blob")
	for _, maximum := range []int{-1, 0, MaximumLocalBlobBatchWrites + 1} {
		if batch, err := BeginLocalBlobWriteBatch(t.Context(), []BlobStore{store}, maximum); err == nil || batch != nil {
			t.Fatal("invalid write bound admitted", maximum, err)
		}
	}
	if batch, err := BeginLocalBlobWriteBatch(nil, []BlobStore{store}, 1); err == nil || batch != nil {
		t.Fatal("missing context admitted", err)
	}
	if batch, err := BeginLocalBlobWriteBatch(t.Context(), nil, 1); err == nil || batch != nil {
		t.Fatal("empty store census admitted", err)
	}
	tooMany := make([]BlobStore, 257)
	for index := range tooMany {
		tooMany[index] = store
	}
	if batch, err := BeginLocalBlobWriteBatch(t.Context(), tooMany, 1); err == nil || batch != nil {
		t.Fatal("one-over store census admitted", err)
	}
	wrapper := struct{ BlobStore }{BlobStore: store}
	if batch, err := BeginLocalBlobWriteBatch(t.Context(), []BlobStore{store, wrapper}, 1); err != nil || batch != nil {
		t.Fatal("mixed unknown backend changed its admission path", err)
	}
	if _, err := os.Stat(root); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("refused or unavailable batch created a root", err)
	}
	batch, err := BeginLocalBlobWriteBatch(t.Context(), []BlobStore{store}, MaximumLocalBlobBatchWrites)
	if err != nil || batch == nil {
		t.Fatal("exact finite write bound refused", err)
	}
	if err := batch.Close(); err != nil {
		t.Fatal(err)
	}
}

// Both callers declare reverse store orders but acquire the same sorted root
// order. A real contention barrier proves one complete owner precedes the other.
func TestLocalBlobBatchAcquiresMultipleRootsInOneOrder(t *testing.T) {
	base := t.TempDir()
	first := NewLocalBlobStore(filepath.Join(base, "a"), "blob").(*localBlobStore)
	second := NewLocalBlobStore(filepath.Join(base, "b"), "blob").(*localBlobStore)
	batch, err := BeginLocalBlobWriteBatch(t.Context(), []BlobStore{second, first}, 1)
	if err != nil || batch == nil || len(batch.roots) != 2 || batch.roots[0].path >= batch.roots[1].path {
		t.Fatal("root acquisition was not canonical", err)
	}
	t.Cleanup(func() { _ = batch.Close() })
	peerFirst := NewLocalBlobStore(first.root, "peer").(*localBlobStore)
	peerSecond := NewLocalBlobStore(second.root, "peer").(*localBlobStore)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	contended := make(chan struct{})
	var once sync.Once
	peerFirst.afterCapacityContentionForTest = func() { once.Do(func() { close(contended) }) }
	type batchResult struct {
		batch *LocalBlobWriteBatch
		err   error
	}
	results := make(chan batchResult, 1)
	joined := make(chan struct{})
	go func() {
		defer close(joined)
		next, err := BeginLocalBlobWriteBatch(ctx, []BlobStore{peerFirst, peerSecond}, 1)
		results <- batchResult{batch: next, err: err}
	}()
	t.Cleanup(func() {
		cancel()
		<-joined
		select {
		case result := <-results:
			_ = result.batch.Close()
		default:
		}
	})
	select {
	case <-contended:
	case result := <-results:
		if result.batch != nil {
			_ = result.batch.Close()
		}
		t.Fatalf("second batch skipped actual contention: %+v", result)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	if err := batch.Close(); err != nil {
		t.Fatal(err)
	}
	result := <-results
	if result.err != nil || result.batch == nil {
		t.Fatal("ordered peer did not acquire both released roots", result.err)
	}
	if err := result.batch.Close(); err != nil {
		t.Fatal(err)
	}
}

// A contended second root must not trap an unrelated writer behind a partially
// acquired batch. Actual lock contention selects the ordering without sleeps.
func TestLocalBlobBatchReleasesPartialOwnersBeforeWaiting(t *testing.T) {
	base := t.TempDir()
	first := NewLocalBlobStore(filepath.Join(base, "a"), "blob").(*localBlobStore)
	second := NewLocalBlobStore(filepath.Join(base, "b"), "blob").(*localBlobStore)
	secondOwner, err := second.lockCapacity(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = secondOwner.Close() })
	contended := make(chan struct{})
	var once sync.Once
	second.afterCapacityContentionForTest = func() { once.Do(func() { close(contended) }) }
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	results := make(chan error, 1)
	joined := make(chan struct{})
	go func() {
		defer close(joined)
		batch, err := BeginLocalBlobWriteBatch(ctx, []BlobStore{first, second}, 1)
		results <- errors.Join(err, batch.Close())
	}()
	t.Cleanup(func() { cancel(); <-joined })
	select {
	case <-contended:
	case err := <-results:
		t.Fatal("batch omitted actual second-root contention", err)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	independent := NewLocalBlobStore(first.root, "independent")
	if created, err := independent.PutIfAbsent(t.Context(), "independent/progress.json", localBlobUsageSource(t, "x"), "application/json"); err != nil || !created {
		t.Fatal("partial batch owner blocked unrelated progress", created, err)
	}
	cancel()
	if err := <-results; !errors.Is(err, context.Canceled) {
		t.Fatal("partially contended batch lost cancellation", err)
	}
}

// The lifecycle worker uses the same process-shared lock. It can remove an
// expired object only after the batch's final accounting is complete.
func TestLocalBlobBatchKeepsLifecycleOutsideItsCensus(t *testing.T) {
	root := t.TempDir()
	writer := NewLocalBlobStoreWithMaxBytes(root, "blob", 5).(*localBlobStore)
	batch, err := BeginLocalBlobWriteBatch(t.Context(), []BlobStore{writer}, 1)
	if err != nil || batch == nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = batch.Close() })
	if created, err := writer.PutIfAbsent(batch.Context(), "blob/expired.json", localBlobUsageSource(t, "12345"), "application/json"); err != nil || !created {
		t.Fatal(created, err)
	}
	if err := os.Chtimes(writer.pathFor("blob/expired.json"), time.Unix(1, 0), time.Unix(1, 0)); err != nil {
		t.Fatal(err)
	}
	reaper := NewLocalBlobStore(root, "blob").(*localBlobStore)
	reaper.rules = []BlobLifecycleRule{{KeyPrefix: "blob/", TTL: time.Second}}
	contended := make(chan struct{})
	var once sync.Once
	reaper.afterCapacityContentionForTest = func() { once.Do(func() { close(contended) }) }
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	results := make(chan error, 1)
	joined := make(chan struct{})
	go func() { defer close(joined); results <- reaper.reapPass(ctx) }()
	t.Cleanup(func() { cancel(); <-joined })
	select {
	case <-contended:
	case err := <-results:
		t.Fatalf("lifecycle bypassed the batch owner: %v", err)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	if err := batch.Close(); err != nil {
		t.Fatal(err)
	}
	if err := <-results; err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(writer.pathFor("blob/expired.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("released lifecycle did not remove the expired object", err)
	}
	if _, err := os.Stat(filepath.Join(root, localBlobCapacityLockName)); err != nil {
		t.Fatal("lifecycle removed the persistent quota owner", err)
	}
}

// Reuse the existing separately executed child-writer root. Its exact
// contention marker and over-capacity assertion prove cross-process ownership.
func TestLocalBlobBatchBlocksSeparateProcessUntilFinalCensus(t *testing.T) {
	root := t.TempDir()
	store := NewLocalBlobStoreWithMaxBytes(root, "blob", 5).(*localBlobStore)
	batch, err := BeginLocalBlobWriteBatch(t.Context(), []BlobStore{store}, 1)
	if err != nil || batch == nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = batch.Close() })
	if created, err := store.PutIfAbsent(batch.Context(), "blob/parent.json", localBlobUsageSource(t, "1234"), "application/json"); err != nil || !created {
		t.Fatal(created, err)
	}
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	command := exec.CommandContext(ctx, binary, "-test.run=^TestLocalBlobStoreCapacityAcrossProcesses$", "-test.count=1")
	command.Env = append(os.Environ(), "URNETWORK_BLOB_CAPACITY_TEST_ROOT="+root, "URNETWORK_BLOB_CAPACITY_TEST_SOURCE="+localBlobUsageSource(t, "5678"))
	stdout, err := command.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	command.Stderr = &stderr
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	scanner := bufio.NewScanner(stdout)
	contended := false
	for scanner.Scan() {
		line := scanner.Text()
		fmt.Fprintln(&output, line)
		if line == "capacity-contended" {
			contended = true
			break
		}
	}
	closeErr := batch.Close()
	_, readErr := io.Copy(&output, stdout)
	waitErr := command.Wait()
	if !contended || scanner.Err() != nil || closeErr != nil || readErr != nil || waitErr != nil {
		t.Fatalf("separate process escaped final census: contended=%t scan=%v close=%v read=%v wait=%v stdout=%q stderr=%q", contended, scanner.Err(), closeErr, readErr, waitErr, output.String(), stderr.String())
	}
	assertLocalBlobCapacityWinner(t, store, "blob/parent.json", "1234", 4)
}
