//go:build linux || darwin

package server

// Competing real filesystem writers force admission, contention and release
// states directly. The subprocess control uses the current test executable;
// no sleep or a negative scheduler observation supplies a quota verdict.

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

// A result belongs to one actual public PutIfAbsent invocation.
type localBlobCapacityWriteResult struct {
	created bool
	err     error
}

// The first writer owns quota and its real staged bytes until explicitly
// released. Cleanup always cancels and joins it, including failed assertions.
func holdLocalBlobCapacityWrite(t *testing.T, store *localBlobStore, key, source string) (func(), <-chan localBlobCapacityWriteResult) {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	entered, release, joined := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	results := make(chan localBlobCapacityWriteResult, 1)
	store.beforeCreateCommitForTest = func() {
		close(entered)
		select {
		case <-release:
		case <-ctx.Done():
		}
	}
	go func() {
		defer close(joined)
		created, err := store.PutIfAbsent(ctx, key, source, "application/json")
		results <- localBlobCapacityWriteResult{created: created, err: err}
	}()
	t.Cleanup(func() {
		cancel()
		unblock()
		<-joined
	})
	select {
	case <-entered:
	case result := <-results:
		t.Fatalf("first writer ended before quota/commit barrier: %+v", result)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	return unblock, results
}

// Waiting admission is witnessed by a real operating-system lock refusal.
func startLocalBlobCapacityContender(t *testing.T, ctx context.Context, store *localBlobStore, key, source string) (<-chan struct{}, <-chan localBlobCapacityWriteResult) {
	t.Helper()
	ctx, cancel := context.WithCancel(ctx)
	contended, joined := make(chan struct{}), make(chan struct{})
	var once sync.Once
	store.afterCapacityContentionForTest = func() { once.Do(func() { close(contended) }) }
	results := make(chan localBlobCapacityWriteResult, 1)
	go func() {
		defer close(joined)
		created, err := store.PutIfAbsent(ctx, key, source, "application/json")
		results <- localBlobCapacityWriteResult{created: created, err: err}
	}()
	t.Cleanup(func() { cancel(); <-joined })
	return contended, results
}

// Both winner bytes and the committed usage census are actual store readbacks.
func assertLocalBlobCapacityWinner(t *testing.T, store *localBlobStore, key, content string, usage int64) {
	t.Helper()
	reader, err := store.Get(t.Context(), key)
	if err != nil {
		t.Fatal(err)
	}
	raw, readErr := io.ReadAll(reader)
	closeErr := reader.Close()
	if readErr != nil || closeErr != nil || string(raw) != content {
		t.Fatalf("winner differs: %q/%v/%v", raw, readErr, closeErr)
	}
	if actual, err := store.usageBytes(); err != nil || actual != usage {
		t.Fatalf("committed usage=%d/%v, want %d", actual, err, usage)
	}
}

// Distinct four-byte objects cannot both consume one five-byte root allowance.
// The old implementation completes the second write before any contention;
// that is an explicit alternate outcome, not a test that hangs without a lock.
func TestLocalBlobStoreConcurrentInstancesShareCapacity(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	first := NewLocalBlobStoreWithMaxBytes(root, "first", 5).(*localBlobStore)
	second := NewLocalBlobStoreWithMaxBytes(root, "second", 5).(*localBlobStore)
	release, firstResult := holdLocalBlobCapacityWrite(t, first, "first/object.json", localBlobUsageSource(t, "1234"))
	contended, secondResult := startLocalBlobCapacityContender(t, t.Context(), second, "second/object.json", localBlobUsageSource(t, "5678"))
	var early *localBlobCapacityWriteResult
	select {
	case <-contended:
	case result := <-secondResult:
		early = &result
	case <-t.Context().Done():
		t.Fatal(t.Context().Err())
	}
	release()
	winner := <-firstResult
	loser := localBlobCapacityWriteResult{}
	if early != nil {
		loser = *early
	} else {
		loser = <-secondResult
	}
	if winner.err != nil || !winner.created || loser.created || loser.err == nil || !strings.Contains(loser.err.Error(), "local blob capacity exceeded") {
		t.Fatalf("independent writers exceeded one root allowance: first=%+v second=%+v", winner, loser)
	}
	assertLocalBlobCapacityWinner(t, first, "first/object.json", "1234", 4)
	if _, err := os.Lstat(second.pathFor("second/object.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("over-capacity loser published a second key: %v", err)
	}
}

// A canceled waiter neither stages bytes nor consumes future quota ownership.
func TestLocalBlobStoreCapacityContentionPreservesCancellation(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	first := NewLocalBlobStoreWithMaxBytes(root, "blob", 5).(*localBlobStore)
	second := NewLocalBlobStoreWithMaxBytes(root, "blob", 5).(*localBlobStore)
	release, firstResult := holdLocalBlobCapacityWrite(t, first, "blob/first.json", localBlobUsageSource(t, "1234"))
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	contended, secondResult := startLocalBlobCapacityContender(t, ctx, second, "blob/canceled.json", localBlobUsageSource(t, "5"))
	select {
	case <-contended:
	case result := <-secondResult:
		t.Fatalf("competing write did not wait for its root owner: %+v", result)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	cancel()
	result := <-secondResult
	if result.created || !errors.Is(result.err, context.Canceled) {
		t.Fatalf("canceled contender was admitted: %+v", result)
	}
	release()
	if result := <-firstResult; result.err != nil || !result.created {
		t.Fatalf("canceled peer damaged active writer: %+v", result)
	}
	if created, err := second.PutIfAbsent(t.Context(), "blob/last.json", localBlobUsageSource(t, "5"), "application/json"); err != nil || !created {
		t.Fatalf("canceled waiter retained root ownership: %t/%v", created, err)
	}
	assertLocalBlobCapacityWinner(t, first, "blob/first.json", "1234", 5)
	if _, err := os.Lstat(second.pathFor("blob/canceled.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("canceled waiter staged or published a key: %v", err)
	}
}

// Cancellation after the actual copy leaves neither a published key nor a
// staged file, and the next independent owner can consume the full allowance.
func TestLocalBlobStoreCapacityCanceledOwnerReleasesStagedBytes(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	store := NewLocalBlobStoreWithMaxBytes(root, "blob", 5).(*localBlobStore)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	staged := false
	store.beforeCreateCommitForTest = func() { staged = true; cancel() }
	created, err := store.PutIfAbsent(ctx, "blob/canceled.json", localBlobUsageSource(t, "12345"), "application/json")
	if !staged || created || !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled staged owner published: staged=%t created=%t error=%v", staged, created, err)
	}
	if entries, err := os.ReadDir(filepath.Join(root, "blob")); err != nil || len(entries) != 0 {
		t.Fatalf("canceled owner left staged or committed bytes: %v/%v", entries, err)
	}
	second := NewLocalBlobStoreWithMaxBytes(root, "blob", 5).(*localBlobStore)
	if created, err := second.PutIfAbsent(t.Context(), "blob/next.json", localBlobUsageSource(t, "12345"), "application/json"); err != nil || !created {
		t.Fatalf("canceled owner did not release exact capacity: %t/%v", created, err)
	}
	assertLocalBlobCapacityWinner(t, second, "blob/next.json", "12345", 5)
	underlying := strings.NewReader("unconsumed")
	reader := &localBlobCapacityReader{ctx: ctx, reader: underlying}
	var next [1]byte
	if n, err := reader.Read(next[:]); n != 0 || !errors.Is(err, context.Canceled) || underlying.Len() != len("unconsumed") {
		t.Fatalf("canceled copy consumed another source byte: %d/%v/%d", n, err, underlying.Len())
	}
}

// Replacement credit is shared across different store prefixes in one root.
func TestLocalBlobStoreCapacityReplacementUsesSharedCommittedBytes(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	first := NewLocalBlobStoreWithMaxBytes(root, "first", 5).(*localBlobStore)
	second := NewLocalBlobStoreWithMaxBytes(root, "second", 5).(*localBlobStore)
	for _, object := range []struct {
		store   *localBlobStore
		key     string
		content string
	}{{store: first, key: "first/object.json", content: "1234"}, {store: second, key: "second/object.json", content: "5"}} {
		if err := object.store.Put(t.Context(), object.key, localBlobUsageSource(t, object.content), "application/json"); err != nil {
			t.Fatal(err)
		}
	}
	if err := second.Put(t.Context(), "second/object.json", localBlobUsageSource(t, "5678"), "application/json"); err == nil || !strings.Contains(err.Error(), "local blob capacity exceeded") {
		t.Fatalf("replacement credited another namespace's bytes: %v", err)
	}
	if err := first.Put(t.Context(), "first/object.json", localBlobUsageSource(t, "1"), "application/json"); err != nil {
		t.Fatal(err)
	}
	if err := second.Put(t.Context(), "second/object.json", localBlobUsageSource(t, "5678"), "application/json"); err != nil {
		t.Fatal("exact shared replacement capacity refused", err)
	}
	assertLocalBlobCapacityWinner(t, first, "first/object.json", "1", 5)
	assertLocalBlobCapacityWinner(t, second, "second/object.json", "5678", 5)
}

// Stat precedes the blocking admission boundary. Growing that exact source
// while it waits cannot publish more bytes than the remaining root allowance.
func TestLocalBlobStoreCapacityCountsSourceGrowthAfterStat(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	first := NewLocalBlobStoreWithMaxBytes(root, "blob", 5).(*localBlobStore)
	second := NewLocalBlobStoreWithMaxBytes(root, "blob", 5).(*localBlobStore)
	release, firstResult := holdLocalBlobCapacityWrite(t, first, "blob/first.json", localBlobUsageSource(t, "1"))
	source := localBlobUsageSource(t, "2")
	contended, secondResult := startLocalBlobCapacityContender(t, t.Context(), second, "blob/grown.json", source)
	select {
	case <-contended:
	case result := <-secondResult:
		t.Fatalf("source never reached the post-Stat contention boundary: %+v", result)
	case <-t.Context().Done():
		t.Fatal(t.Context().Err())
	}
	if err := os.WriteFile(source, []byte("23456"), 0o600); err != nil {
		t.Fatal(err)
	}
	release()
	if result := <-firstResult; result.err != nil || !result.created {
		t.Fatalf("first writer failed: %+v", result)
	}
	if result := <-secondResult; result.created || result.err == nil || !strings.Contains(result.err.Error(), "local blob capacity exceeded") {
		t.Fatalf("source growth escaped actual copied-byte admission: %+v", result)
	}
	if _, err := os.Lstat(second.pathFor("blob/grown.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("grown source published a destination: %v", err)
	}
	if created, err := second.PutIfAbsent(t.Context(), "blob/exact.json", localBlobUsageSource(t, "2345"), "application/json"); err != nil || !created {
		t.Fatalf("failed copy retained quota or owner: %t/%v", created, err)
	}
	assertLocalBlobCapacityWinner(t, second, "blob/exact.json", "2345", 5)
}

// Public keys cannot bypass capacity through reserved temporary names, path
// cleanup, absolute paths, or a directory alias leading outside the root.
func TestLocalBlobStoreCapacityRejectsUnchargedWriteNamespaces(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	outside := t.TempDir()
	store := NewLocalBlobStoreWithMaxBytes(root, "blob", 5).(*localBlobStore)
	if err := os.Symlink(outside, filepath.Join(root, "alias")); err != nil {
		t.Fatal(err)
	}
	source := localBlobUsageSource(t, "1234")
	for _, key := range []string{"blob/hidden.partial", localBlobCapacityLockName, "../escaped.json", "blob/../cleaned.json", "/absolute.json", "blob\\escape.json", "alias/escaped.json"} {
		if err := store.Put(t.Context(), key, source, "application/json"); err == nil {
			t.Fatalf("ordinary write accepted uncharged key %q", key)
		}
		if created, err := store.PutIfAbsent(t.Context(), key, source, "application/json"); err == nil || created {
			t.Fatalf("immutable write accepted uncharged key %q: %t/%v", key, created, err)
		}
	}
	if entries, err := os.ReadDir(outside); err != nil || len(entries) != 0 {
		t.Fatalf("refused key escaped into another root: %v/%v", entries, err)
	}
}

// Configured root aliases must rendezvous at the same physical lock inode.
func TestLocalBlobStoreCapacitySharesPhysicalRootAliases(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	alias := filepath.Join(t.TempDir(), "root-alias")
	if err := os.Symlink(root, alias); err != nil {
		t.Fatal(err)
	}
	first := NewLocalBlobStoreWithMaxBytes(root, "blob", 5).(*localBlobStore)
	second := NewLocalBlobStoreWithMaxBytes(alias, "blob", 5).(*localBlobStore)
	release, firstResult := holdLocalBlobCapacityWrite(t, first, "blob/first.json", localBlobUsageSource(t, "1234"))
	contended, secondResult := startLocalBlobCapacityContender(t, t.Context(), second, "blob/second.json", localBlobUsageSource(t, "56"))
	select {
	case <-contended:
	case result := <-secondResult:
		t.Fatalf("physical root alias bypassed the shared owner: %+v", result)
	case <-t.Context().Done():
		t.Fatal(t.Context().Err())
	}
	release()
	if result := <-firstResult; result.err != nil || !result.created {
		t.Fatalf("first writer failed: %+v", result)
	}
	if result := <-secondResult; result.created || result.err == nil || !strings.Contains(result.err.Error(), "local blob capacity exceeded") {
		t.Fatalf("alias escaped shared capacity: %+v", result)
	}
	assertLocalBlobCapacityWinner(t, second, "blob/first.json", "1234", 4)
}

// Lifecycle waits use the same cancellable owner, and private owner/stage
// names remain untouched even when an old timestamp matches a broad TTL rule.
func TestLocalBlobStoreCapacityReaperJoinsOwnerAndPreservesPrivateFiles(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writer := NewLocalBlobStoreWithMaxBytes(root, "blob", 5).(*localBlobStore)
	reaper := NewLocalBlobStoreWithMaxBytes(root, "blob", 5).(*localBlobStore)
	reaper.rules = []BlobLifecycleRule{{KeyPrefix: "", TTL: time.Hour}}
	release, writerResult := holdLocalBlobCapacityWrite(t, writer, "blob/current.json", localBlobUsageSource(t, "1234"))
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	contended, joined := make(chan struct{}), make(chan struct{})
	var once sync.Once
	reaper.afterCapacityContentionForTest = func() { once.Do(func() { close(contended) }) }
	results := make(chan error, 1)
	go func() { defer close(joined); results <- reaper.reapPass(ctx) }()
	t.Cleanup(func() { cancel(); <-joined })
	select {
	case <-contended:
	case err := <-results:
		t.Fatalf("reaper did not wait for the active root writer: %v", err)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	cancel()
	if err := <-results; !errors.Is(err, context.Canceled) {
		t.Fatalf("waiting reaper ignored cancellation: %v", err)
	}
	release()
	if result := <-writerResult; result.err != nil || !result.created {
		t.Fatalf("reaper damaged an active partial: %+v", result)
	}
	pending := filepath.Join(root, ".old-uncommitted.partial")
	if err := os.WriteFile(pending, []byte("private"), 0o600); err != nil {
		t.Fatal(err)
	}
	old := time.Unix(1, 0)
	for _, path := range []string{pending, filepath.Join(root, localBlobCapacityLockName)} {
		if err := os.Chtimes(path, old, old); err != nil {
			t.Fatal(err)
		}
	}
	if err := reaper.reapPass(t.Context()); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{pending, filepath.Join(root, localBlobCapacityLockName)} {
		if _, err := os.Lstat(path); err != nil {
			t.Fatalf("lifecycle removed a private ownership file: %v", err)
		}
	}
	assertLocalBlobCapacityWinner(t, writer, "blob/current.json", "1234", 4)
}

// A separate process must observe actual contention before the first process
// commits, then refuse its over-capacity object after that owner releases.
func TestLocalBlobStoreCapacityAcrossProcesses(t *testing.T) {
	if root := os.Getenv("URNETWORK_BLOB_CAPACITY_TEST_ROOT"); root != "" {
		store := NewLocalBlobStoreWithMaxBytes(root, "blob", 5).(*localBlobStore)
		var once sync.Once
		store.afterCapacityContentionForTest = func() { once.Do(func() { fmt.Println("capacity-contended") }) }
		created, err := store.PutIfAbsent(t.Context(), "blob/child.json", os.Getenv("URNETWORK_BLOB_CAPACITY_TEST_SOURCE"), "application/json")
		if created || err == nil || !strings.Contains(err.Error(), "local blob capacity exceeded") {
			t.Fatalf("child escaped the parent process allowance: %t/%v", created, err)
		}
		return
	}
	t.Parallel()
	root := t.TempDir()
	writer := NewLocalBlobStoreWithMaxBytes(root, "blob", 5).(*localBlobStore)
	release, firstResult := holdLocalBlobCapacityWrite(t, writer, "blob/parent.json", localBlobUsageSource(t, "1234"))
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	command := exec.CommandContext(ctx, executable, "-test.run=^TestLocalBlobStoreCapacityAcrossProcesses$", "-test.count=1")
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
	contended := false
	scanner := bufio.NewScanner(stdout)
	for scanner.Scan() {
		line := scanner.Text()
		fmt.Fprintln(&output, line)
		if line == "capacity-contended" {
			contended = true
			break
		}
	}
	release()
	if _, err := io.Copy(&output, stdout); err != nil {
		cancel()
		_ = command.Wait()
		t.Fatal(err)
	}
	waitErr := command.Wait()
	first := <-firstResult
	if !contended || scanner.Err() != nil || waitErr != nil || first.err != nil || !first.created {
		t.Fatalf("cross-process ownership failed: contended=%t scan=%v child=%v parent=%+v stdout=%s stderr=%s", contended, scanner.Err(), waitErr, first, output.String(), stderr.String())
	}
	assertLocalBlobCapacityWinner(t, writer, "blob/parent.json", "1234", 4)
}
