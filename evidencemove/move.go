// Package evidencemove moves the legacy flat evidence content objects,
// <prefix>/st/v1/evidence/content/sha256/<h>.json, to the sharded keys new
// evidence has been written at since 2026-11-15,
// <prefix>/st/v1/evidence/content/sha256/<h[0:2]>/<h[2:4]>/<h>.json
// (startifact/evidence_content.go).
//
// On edge-5 MinIO keeps each object as a directory, so the flat folder is one
// directory holding hundreds of millions of entries. MinIO's data scanner
// lists a folder wholly into memory (scanFolder, cmd/data-scanner.go), and
// its listing of this one grows until MinIO is OOM-killed at its cgroup cap,
// every 1-2 hours (2026-11-17). Every reader tries the sharded key first and
// falls back to the flat key, so an object reads the same before, during and
// after its move.
//
// The mover never lists the flat folder: a listing makes MinIO walk the same
// directory. It takes content hashes from Postgres instead, the evidence
// rows of st_client_key_history in primary key order (ClientKeyHistorySource,
// read only), and for each hash:
//   - HEADs the flat and the sharded key;
//   - copies flat to sharded on the server when only the flat object exists;
//   - verifies the sharded copy: read whole, its bytes have the flat object's
//     size and ETag (the MD5 of its bytes), and are a signed evidence envelope
//     whose recomputed content hash is the key's hash;
//   - only then removes the flat object.
//
// A flat object is removed only after its verified sharded copy exists, and a
// failed verification keeps it (OutcomeRefused, recorded for the operator).
// Every step is idempotent, so a rerun from the durable checkpoint, after a
// stop or a crash at any point, completes what an earlier run began.
//
// The mover runs only while its gates are open (gate.go): the SN25 validator
// idle, with its current epoch's intent finalized or applied and at least 15
// minutes to the next boundary, and MinIO's cluster health answering within a
// second. Objects are paced to a fixed rate (Pacer), and a failed storage
// request closes the gates for a growing backoff.
package evidencemove

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/startifact"
)

// The keys one content hash may be stored at in a store with prefix: the
// sharded key readers try first, then the legacy flat key. Composed exactly as
// the readers compose them (startifact.OpenEvidenceContent).
type ContentKeys struct {
	// the content hash, lowercase hex without its algorithm prefix
	Hash    string
	Sharded string
	Flat    string
}

func KeysFor(prefix string, hash string) (ContentKeys, error) {
	suffixes, err := startifact.EvidenceContentKeySuffixes(hash)
	if err != nil {
		return ContentKeys{}, err
	}
	join := func(suffix string) string {
		return filepath.ToSlash(filepath.Join(prefix, filepath.FromSlash(suffix)))
	}
	return ContentKeys{
		Hash:    strings.TrimSuffix(path.Base(suffixes[1]), ".json"),
		Sharded: join(suffixes[0]),
		Flat:    join(suffixes[1]),
	}, nil
}

// What the mover did, or in a dry run would do, with one content hash.
type Outcome string

const (
	// copied to its sharded key, the copy verified, and the flat object removed
	OutcomeMoved Outcome = "moved"
	// a sharded copy already existed (the server wrote it), verified, and the
	// flat object removed
	OutcomeDeduped Outcome = "deduped"
	// only the sharded object exists: nothing to move
	OutcomeDone Outcome = "done"
	// neither key holds an object
	OutcomeAbsent Outcome = "absent"
	// the flat object is kept: it differs from the source's record of the
	// evidence, or its sharded copy failed verification
	OutcomeRefused Outcome = "refused"
	// dry run: only the flat object exists; a run would copy and remove it
	OutcomeWouldMove Outcome = "would_move"
	// dry run: both objects exist; a run would verify and remove the flat one
	OutcomeWouldDedupe Outcome = "would_dedupe"
)

// Whether the flat object is gone because of this outcome (or, in a dry run,
// would be).
func (self Outcome) Removes() bool {
	switch self {
	case OutcomeMoved, OutcomeDeduped, OutcomeWouldMove, OutcomeWouldDedupe:
		return true
	}
	return false
}

// One content hash from a source, at its position in the source's order.
type Item struct {
	// the source's position of this item: the checkpoint's cursor once this
	// item and every earlier one have completed
	Cursor Cursor
	// "sha256:<hex>" or bare hex
	Hash string
	// the evidence's size in bytes as the source records it, or 0 for none
	Size int64
}

// The outcome for one item, as the journal and the refusal log record it.
type Result struct {
	Time    time.Time `json:"time"`
	Hash    string    `json:"hash"`
	Outcome Outcome   `json:"outcome"`
	// the flat object's size and ETag, when it existed
	Size   int64  `json:"size,omitempty"`
	ETag   string `json:"etag,omitempty"`
	Reason string `json:"reason,omitempty"`
}

// Counts of outcomes, by outcome.
type Counts struct {
	Items       int64 `json:"items"`
	Moved       int64 `json:"moved"`
	Deduped     int64 `json:"deduped"`
	Done        int64 `json:"done"`
	Absent      int64 `json:"absent"`
	Refused     int64 `json:"refused"`
	WouldMove   int64 `json:"would_move,omitempty"`
	WouldDedupe int64 `json:"would_dedupe,omitempty"`
}

func (self *Counts) add(outcome Outcome) {
	self.Items += 1
	switch outcome {
	case OutcomeMoved:
		self.Moved += 1
	case OutcomeDeduped:
		self.Deduped += 1
	case OutcomeDone:
		self.Done += 1
	case OutcomeAbsent:
		self.Absent += 1
	case OutcomeRefused:
		self.Refused += 1
	case OutcomeWouldMove:
		self.WouldMove += 1
	case OutcomeWouldDedupe:
		self.WouldDedupe += 1
	}
}

// Flat objects removed (in a dry run, that would be).
func (self Counts) Removed() int64 {
	return self.Moved + self.Deduped + self.WouldMove + self.WouldDedupe
}

func (self *Counts) plus(other Counts) {
	self.Items += other.Items
	self.Moved += other.Moved
	self.Deduped += other.Deduped
	self.Done += other.Done
	self.Absent += other.Absent
	self.Refused += other.Refused
	self.WouldMove += other.WouldMove
	self.WouldDedupe += other.WouldDedupe
}

type Settings struct {
	// objects begun a second, across all workers
	ObjectsPerSecond float64
	// objects in flight at once
	Workers int
	// rows read from the source a batch; the checkpoint advances per batch
	BatchRows int
	// stop once at least this many flat objects are removed (0: no bound);
	// objects in flight then finish, so up to Workers more may be removed
	MaxMoves int64
	// HEAD only: classify every object, change nothing, write no checkpoint
	DryRun bool
	// attempts of one object, counting only attempts made with the gates open
	MaxAttempts int
	// the bound of one storage request
	RequestTimeout time.Duration
	// the largest object moved; a larger flat object is refused
	MaxObjectBytes int64
	// when set, the run ends before the first row of a client created at or
	// after this time (its id's leading 48 bits): every writer has written
	// sharded keys since the cutover, so those rows need no move
	UntilCreated time.Time
	// how often progress is logged
	ProgressInterval time.Duration
}

// The defaults run from the operator's Mac over the management VPN (about 75
// ms to edge-5): an object takes about four round trips (two HEADs at once,
// the copy, the verifying read, the delete), so 96 workers sustain the rate.
func DefaultSettings() Settings {
	return Settings{
		ObjectsPerSecond: 200,
		Workers:          96,
		BatchRows:        1000,
		MaxAttempts:      8,
		RequestTimeout:   30 * time.Second,
		// client-key evidence is at most 16 KiB (MaxClientKeyEvidenceBytes)
		MaxObjectBytes:   1024 * 1024,
		ProgressInterval: 30 * time.Second,
	}
}

func (self Settings) Validate() error {
	switch {
	case !(0 < self.ObjectsPerSecond && self.ObjectsPerSecond <= 5000):
		return fmt.Errorf("objects a second must be in (0, 5000], not %v", self.ObjectsPerSecond)
	case self.Workers < 1 || 1024 < self.Workers:
		return fmt.Errorf("workers must be 1 to 1024, not %d", self.Workers)
	case self.BatchRows < 1 || 100000 < self.BatchRows:
		return fmt.Errorf("batch rows must be 1 to 100000, not %d", self.BatchRows)
	case self.MaxMoves < 0:
		return fmt.Errorf("max moves must not be negative")
	case self.MaxAttempts < 1:
		return fmt.Errorf("attempts must be at least 1")
	case self.RequestTimeout <= 0 || self.MaxObjectBytes <= 0 || self.ProgressInterval <= 0:
		return fmt.Errorf("request timeout, object bound and progress interval must be positive")
	}
	return nil
}

// Reports what Run did. Counts covers only the items the checkpoint passed.
type Report struct {
	Counts   Counts
	Cursor   Cursor
	Finished bool
	// why the run ended before the source did
	Stopped string
	Elapsed time.Duration
	// time with the gates closed
	Paused  time.Duration
	Latency map[string]LatencySummary
	// failed storage attempts, each retried after a backoff unless refused:
	// transient, throttled and refused
	Failures map[string]int64
}

type Mover struct {
	Store  ObjectStore
	Prefix string
	Source Source
	Gates  *GateKeeper
	Pacer  *Pacer
	// the durable cursor; nil in a dry run that starts at the beginning
	Checkpoint *Checkpoint
	// every result but done and absent, appended per batch (optional)
	Journal *JSONLWriter
	// every refusal, appended per batch (optional)
	Refusals *JSONLWriter
	Settings Settings
	Logf     func(format string, args ...any)
	// request latencies by operation; the health gate may share it
	Latency *Latencies

	failureLock sync.Mutex
	failures    map[string]int64
}

func (self *Mover) countFailure(class string) {
	self.failureLock.Lock()
	defer self.failureLock.Unlock()
	if self.failures == nil {
		self.failures = map[string]int64{}
	}
	self.failures[class] += 1
}

func (self *Mover) failureCounts() map[string]int64 {
	self.failureLock.Lock()
	defer self.failureLock.Unlock()
	counts := map[string]int64{}
	for class, count := range self.failures {
		counts[class] = count
	}
	return counts
}

// A failure that more attempts will not cure: a refused request (credentials,
// a missing bucket) or an exhausted object.
type fatalError struct {
	err error
}

func (self *fatalError) Error() string { return self.err.Error() }
func (self *fatalError) Unwrap() error { return self.err }

func (self *Mover) logf(format string, args ...any) {
	if self.Logf != nil {
		self.Logf(format, args...)
	}
}

// Moves items from the checkpoint's cursor until the source ends, MaxMoves
// flat objects are removed, ctx ends or an object fails past its attempts.
// After each batch the checkpoint advances past the longest prefix of the
// batch whose items all completed, so a stop or crash anywhere never skips an
// item: a later run redoes the rest, and every step is idempotent.
func (self *Mover) Run(ctx context.Context) (*Report, error) {
	if err := self.Settings.Validate(); err != nil {
		return nil, err
	}
	if self.Store == nil || self.Source == nil || self.Gates == nil || self.Pacer == nil {
		return nil, errors.New("evidence move is missing its store, source, gates or pacer")
	}
	if self.Checkpoint == nil && !self.Settings.DryRun {
		return nil, errors.New("a run that removes objects needs a checkpoint")
	}
	if self.Latency == nil {
		self.Latency = NewLatencies()
	}
	start := time.Now()
	report := &Report{}
	if self.Checkpoint != nil {
		report.Cursor = self.Checkpoint.State.Cursor
	}
	pausedAtStart := self.Gates.Paused()
	progressCtx, progressCancel := context.WithCancel(ctx)
	defer progressCancel()
	progress := &Counts{}
	var progressLock sync.Mutex
	go self.logProgress(progressCtx, start, progress, &progressLock, func() Cursor {
		progressLock.Lock()
		defer progressLock.Unlock()
		return report.Cursor
	})
	finish := func(err error) (*Report, error) {
		report.Elapsed = time.Since(start)
		report.Paused = self.Gates.Paused() - pausedAtStart
		report.Latency = self.Latency.Summaries()
		report.Failures = self.failureCounts()
		return report, err
	}
	for {
		if 0 < self.Settings.MaxMoves && self.Settings.MaxMoves <= report.Counts.Removed() {
			report.Stopped = fmt.Sprintf("removed %d flat objects, the run's bound of %d", report.Counts.Removed(), self.Settings.MaxMoves)
			return finish(nil)
		}
		if err := ctx.Err(); err != nil {
			report.Stopped = "stopped: " + err.Error()
			return finish(nil)
		}
		items, err := self.nextItems(ctx, report.Cursor)
		if err != nil {
			if ctx.Err() != nil {
				report.Stopped = "stopped: " + ctx.Err().Error()
				return finish(nil)
			}
			return finish(err)
		}
		reachedUntil := false
		if !self.Settings.UntilCreated.IsZero() {
			for index, item := range items {
				if !item.Cursor.Time().Before(self.Settings.UntilCreated) {
					items, reachedUntil = items[:index], true
					break
				}
			}
		}
		until := func() (*Report, error) {
			report.Finished = true
			report.Stopped = "reached the clients created at or after " + self.Settings.UntilCreated.UTC().Format(time.RFC3339)
			return finish(nil)
		}
		if len(items) == 0 {
			if reachedUntil {
				return until()
			}
			report.Finished = true
			return finish(nil)
		}
		results, completed, batchErr := self.runBatch(ctx, items, report.Counts.Removed())
		var batch Counts
		for _, result := range results[:completed] {
			batch.add(result.Outcome)
		}
		if 0 < completed {
			if err := self.record(results[:completed]); err != nil {
				return finish(err)
			}
			cursor := items[completed-1].Cursor
			if self.Checkpoint != nil && !self.Settings.DryRun {
				if err := self.Checkpoint.Advance(cursor, batch); err != nil {
					return finish(fmt.Errorf("checkpoint: %w", err))
				}
			}
			progressLock.Lock()
			report.Cursor = cursor
			report.Counts.plus(batch)
			progress.plus(batch)
			progressLock.Unlock()
		}
		if batchErr != nil {
			if ctx.Err() != nil && !isFatal(batchErr) {
				report.Stopped = "stopped: " + ctx.Err().Error()
				return finish(nil)
			}
			return finish(batchErr)
		}
		if completed < len(items) {
			// the bound stopped dispatch inside the batch
			continue
		}
		if reachedUntil {
			return until()
		}
	}
}

// The next batch after cursor. A failed read is retried with a growing wait,
// and ends the run after a few minutes of failures.
func (self *Mover) nextItems(ctx context.Context, cursor Cursor) ([]Item, error) {
	wait := time.Second
	deadline := time.Now().Add(5 * time.Minute)
	for {
		items, err := self.Source.Next(ctx, cursor, self.Settings.BatchRows)
		if err == nil {
			return items, nil
		}
		if ctx.Err() != nil || time.Now().After(deadline) {
			return nil, fmt.Errorf("source read after %s: %w", cursor, err)
		}
		self.logf("source read after %s failed, retrying in %s: %v", cursor, wait, err)
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(wait):
		}
		wait = min(2*wait, time.Minute)
	}
}

func isFatal(err error) bool {
	var fatal *fatalError
	return errors.As(err, &fatal)
}

// Runs one batch on the workers in source order. completed is the length of
// the longest prefix of items that all reached an outcome; results past it
// may be set but are not recorded, and a later run redoes them. Dispatch
// stops at the run's bound of removals, at ctx's end and at a fatal error.
func (self *Mover) runBatch(ctx context.Context, items []Item, removedBefore int64) (results []Result, completed int, resultErr error) {
	results = make([]Result, len(items))
	done := make([]bool, len(items))
	var removed atomic.Int64
	removed.Store(removedBefore)
	batchCtx, batchCancel := context.WithCancel(ctx)
	defer batchCancel()
	var errLock sync.Mutex
	var firstErr error
	fail := func(err error) {
		errLock.Lock()
		defer errLock.Unlock()
		if firstErr == nil || (isFatal(err) && !isFatal(firstErr)) {
			firstErr = err
		}
		batchCancel()
	}
	indexes := make(chan int)
	var workers sync.WaitGroup
	for range min(self.Settings.Workers, len(items)) {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for index := range indexes {
				result, err := self.process(batchCtx, items[index])
				if err != nil {
					fail(err)
					continue
				}
				results[index] = result
				done[index] = true
				if result.Outcome.Removes() {
					removed.Add(1)
				}
			}
		}()
	}
dispatch:
	for index := range items {
		if 0 < self.Settings.MaxMoves && self.Settings.MaxMoves <= removed.Load() {
			break
		}
		select {
		case <-batchCtx.Done():
			break dispatch
		case indexes <- index:
		}
	}
	close(indexes)
	workers.Wait()
	for completed < len(items) && done[completed] {
		completed += 1
	}
	errLock.Lock()
	defer errLock.Unlock()
	return results, completed, firstErr
}

// One item through its attempts. Each attempt waits for the gates and the
// pacer; a failed attempt closes the gates for the keeper's backoff. A refused
// request, or an object that fails all its attempts, is fatal.
func (self *Mover) process(ctx context.Context, item Item) (Result, error) {
	var state attemptState
	for attempt := 1; ; attempt += 1 {
		if err := self.waitTurn(ctx); err != nil {
			return Result{}, err
		}
		result, err := self.moveOne(ctx, item, &state)
		if err == nil {
			self.Gates.Succeeded()
			result.Time = time.Now().UTC()
			return result, nil
		}
		if ctx.Err() != nil {
			return Result{}, ctx.Err()
		}
		switch classifyStoreError(err) {
		case storeErrorRefused:
			self.countFailure("refused")
			return Result{}, &fatalError{fmt.Errorf("storage refused a request for %s: %w", item.Hash, err)}
		case storeErrorThrottled:
			self.countFailure("throttled")
			self.Gates.Failed(err, true)
		default:
			self.countFailure("transient")
			self.Gates.Failed(err, false)
		}
		if self.Settings.MaxAttempts <= attempt {
			return Result{}, &fatalError{fmt.Errorf("gave up on %s after %d attempts: %w", item.Hash, attempt, err)}
		}
	}
}

// Waits for open gates, then for this object's start in the pacer, and starts
// only if the gates are still open: workers released together after a pause
// still start one pacer interval apart, and none starts once a gate closed
// during its wait.
func (self *Mover) waitTurn(ctx context.Context) error {
	for {
		if err := self.Gates.Wait(ctx); err != nil {
			return err
		}
		if err := self.Pacer.Wait(ctx); err != nil {
			return err
		}
		if open, _ := self.Gates.Open(); open {
			return nil
		}
	}
}

type storeErrorClass int

const (
	storeErrorTransient storeErrorClass = iota
	storeErrorThrottled
	storeErrorRefused
)

// A missing object mid-step (another process moved it) and a failed copy
// precondition (the source changed) are transient: the next attempt starts
// from fresh HEADs. Throttling slows every worker. Other 4xx answers, such as
// AccessDenied or NoSuchBucket, will not change.
func classifyStoreError(err error) storeErrorClass {
	switch {
	case server.IsBlobNotFound(err), errors.Is(err, errCopyPrecondition):
		return storeErrorTransient
	case server.IsBlobThrottled(err):
		return storeErrorThrottled
	case server.IsBlobRequestRefused(err):
		return storeErrorRefused
	}
	return storeErrorTransient
}

// What earlier attempts at one item did.
type attemptState struct {
	// an attempt copied the flat object to the sharded key
	copied bool
}

// One attempt at one content hash. Any error leaves both objects as they
// were or with the verified sharded copy added; the flat object is removed
// only after its sharded copy verifies. A copy this item's attempts made that
// fails verification is removed again, so it never shadows the flat object
// for readers, who read the sharded key first.
func (self *Mover) moveOne(ctx context.Context, item Item, state *attemptState) (Result, error) {
	keys, err := KeysFor(self.Prefix, item.Hash)
	if err != nil {
		// a malformed hash in the source can never move
		return Result{Hash: item.Hash, Outcome: OutcomeRefused, Reason: err.Error()}, nil
	}
	result := Result{Hash: keys.Hash}
	flat, sharded, err := self.statBoth(ctx, keys)
	if err != nil {
		return result, err
	}
	switch {
	case flat == nil && sharded != nil:
		result.Outcome = OutcomeDone
		return result, nil
	case flat == nil:
		result.Outcome = OutcomeAbsent
		return result, nil
	}
	result.Size, result.ETag = flat.Size, flat.ETag
	if 0 < item.Size && flat.Size != item.Size {
		result.Outcome = OutcomeRefused
		result.Reason = fmt.Sprintf("the flat object holds %d bytes and the source's evidence %d", flat.Size, item.Size)
		return result, nil
	}
	if self.Settings.MaxObjectBytes < flat.Size {
		result.Outcome = OutcomeRefused
		result.Reason = fmt.Sprintf("the flat object holds %d bytes, more than the bound of %d", flat.Size, self.Settings.MaxObjectBytes)
		return result, nil
	}
	if self.Settings.DryRun {
		result.Outcome = OutcomeWouldDedupe
		if sharded == nil {
			result.Outcome = OutcomeWouldMove
		}
		return result, nil
	}
	result.Outcome = OutcomeDeduped
	if sharded == nil || state.copied {
		result.Outcome = OutcomeMoved
	}
	if sharded == nil {
		state.copied = true
		if err := self.timed(ctx, "copy", func(ctx context.Context) error {
			return self.Store.Copy(ctx, *flat, keys.Sharded)
		}); err != nil {
			return result, err
		}
	}
	var data []byte
	var copied ObjectInfo
	if err := self.timed(ctx, "read", func(ctx context.Context) (err error) {
		data, copied, err = self.Store.Read(ctx, keys.Sharded, flat.Size)
		return
	}); err != nil {
		return result, err
	}
	var flatData []byte
	if !plainMD5(flat.ETag) {
		// an ETag that is not the bytes' MD5 cannot prove the copy: compare
		// the flat object's own bytes
		if err := self.timed(ctx, "read", func(ctx context.Context) (err error) {
			flatData, _, err = self.Store.Read(ctx, keys.Flat, flat.Size)
			return
		}); err != nil {
			return result, err
		}
	}
	if reason := verifyCopy(keys.Hash, *flat, flatData, copied, data); reason != "" {
		if state.copied {
			// this item's own copy: restore the layout it found
			if err := self.timed(ctx, "remove", func(ctx context.Context) error {
				return self.Store.Remove(ctx, keys.Sharded)
			}); err != nil {
				return result, err
			}
			reason += "; the copy was removed"
		}
		result.Outcome = OutcomeRefused
		result.Reason = reason
		return result, nil
	}
	if err := self.timed(ctx, "remove", func(ctx context.Context) error {
		return self.Store.Remove(ctx, keys.Flat)
	}); err != nil {
		return result, err
	}
	return result, nil
}

// Both keys' HEADs at once. nil is a key that holds no object.
func (self *Mover) statBoth(ctx context.Context, keys ContentKeys) (flat *ObjectInfo, sharded *ObjectInfo, err error) {
	var flatErr, shardedErr error
	var wait sync.WaitGroup
	wait.Add(1)
	go func() {
		defer wait.Done()
		flat, flatErr = self.stat(ctx, keys.Flat)
	}()
	sharded, shardedErr = self.stat(ctx, keys.Sharded)
	wait.Wait()
	return flat, sharded, errors.Join(flatErr, shardedErr)
}

func (self *Mover) stat(ctx context.Context, key string) (*ObjectInfo, error) {
	var info ObjectInfo
	err := self.timed(ctx, "stat", func(ctx context.Context) (err error) {
		info, err = self.Store.Stat(ctx, key)
		return
	})
	if err != nil {
		if server.IsBlobNotFound(err) {
			return nil, nil
		}
		return nil, err
	}
	return &info, nil
}

// Runs one storage request under the request bound and records its latency.
func (self *Mover) timed(ctx context.Context, operation string, request func(ctx context.Context) error) error {
	requestCtx, cancel := context.WithTimeout(ctx, self.Settings.RequestTimeout)
	defer cancel()
	start := time.Now()
	err := request(requestCtx)
	if self.Latency != nil && (err == nil || server.IsBlobNotFound(err)) {
		self.Latency.Observe(operation, time.Since(start))
	}
	return err
}

// Why the sharded copy does not prove itself the flat object's evidence, or
// "" when it does: its bytes have the flat object's size, ETag and MD5 (or,
// for an ETag that is not an MD5, equal the flat object's bytes), and are a
// signed evidence envelope whose recomputed content hash is the key's hash.
func verifyCopy(hash string, flat ObjectInfo, flatData []byte, copied ObjectInfo, data []byte) string {
	if int64(len(data)) != flat.Size || copied.Size != flat.Size {
		return fmt.Sprintf("the sharded object holds %d bytes (%d read) and the flat object %d", copied.Size, len(data), flat.Size)
	}
	if !strings.EqualFold(copied.ETag, flat.ETag) {
		return fmt.Sprintf("the sharded object's ETag %q differs from the flat object's %q", copied.ETag, flat.ETag)
	}
	if plainMD5(flat.ETag) {
		sum := md5.Sum(data)
		if !strings.EqualFold(hex.EncodeToString(sum[:]), flat.ETag) {
			return fmt.Sprintf("the sharded bytes' MD5 %x is not the flat object's ETag %q", sum, flat.ETag)
		}
	} else if string(flatData) != string(data) {
		return "the sharded bytes differ from the flat object's"
	}
	var envelope startifact.EvidenceEnvelope
	if err := json.Unmarshal(data, &envelope); err != nil {
		return fmt.Sprintf("the copy is not an evidence envelope: %v", err)
	}
	if err := startifact.VerifyEvidence(&envelope); err != nil {
		return fmt.Sprintf("the copy's envelope does not verify: %v", err)
	}
	if !strings.EqualFold(envelope.ContentHash, "sha256:"+hash) {
		return fmt.Sprintf("the copy's content hash %s is not the key's sha256:%s", envelope.ContentHash, hash)
	}
	return ""
}

// An S3 ETag of a single-part, unencrypted upload is its bytes' MD5.
func plainMD5(etag string) bool {
	if len(etag) != 32 {
		return false
	}
	_, err := hex.DecodeString(etag)
	return err == nil
}

// Appends the batch's journal and refusal records.
func (self *Mover) record(results []Result) error {
	var journal, refusals []any
	for _, result := range results {
		switch result.Outcome {
		case OutcomeDone, OutcomeAbsent:
		default:
			journal = append(journal, result)
		}
		if result.Outcome == OutcomeRefused {
			refusals = append(refusals, result)
			self.logf("refused %s: %s", result.Hash, result.Reason)
		}
	}
	if self.Journal != nil && 0 < len(journal) {
		if err := self.Journal.Append(journal...); err != nil {
			return fmt.Errorf("journal: %w", err)
		}
	}
	if self.Refusals != nil && 0 < len(refusals) {
		if err := self.Refusals.Append(refusals...); err != nil {
			return fmt.Errorf("refusals: %w", err)
		}
	}
	return nil
}

func (self *Mover) logProgress(ctx context.Context, start time.Time, counts *Counts, lock *sync.Mutex, cursor func() Cursor) {
	ticker := time.NewTicker(self.Settings.ProgressInterval)
	defer ticker.Stop()
	var last Counts
	lastTime := start
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			lock.Lock()
			current := *counts
			lock.Unlock()
			rate := float64(current.Items-last.Items) / now.Sub(lastTime).Seconds()
			open, reason := self.Gates.Open()
			gate := "open"
			if !open {
				gate = "closed: " + reason
			}
			self.logf(
				"progress: %d items (%.1f/s): moved %d, deduped %d, done %d, absent %d, refused %d, would move %d, would dedupe %d; cursor %s; gates %s; latency %s",
				current.Items, rate, current.Moved, current.Deduped, current.Done, current.Absent, current.Refused,
				current.WouldMove, current.WouldDedupe, cursor(), gate, self.Latency.Line(),
			)
			last, lastTime = current, now
		}
	}
}

// A JSONL file appended to and synced per call.
type JSONLWriter struct {
	lock sync.Mutex
	file fileSyncWriter
}

type fileSyncWriter interface {
	Write([]byte) (int, error)
	Sync() error
	Close() error
}

func (self *JSONLWriter) Append(records ...any) error {
	self.lock.Lock()
	defer self.lock.Unlock()
	var buffer []byte
	for _, record := range records {
		line, err := json.Marshal(record)
		if err != nil {
			return err
		}
		buffer = append(append(buffer, line...), '\n')
	}
	if _, err := self.file.Write(buffer); err != nil {
		return err
	}
	return self.file.Sync()
}

func (self *JSONLWriter) Close() error {
	self.lock.Lock()
	defer self.lock.Unlock()
	return self.file.Close()
}

// errNotFound wraps fs.ErrNotExist, which server.IsBlobNotFound reports.
func errNotFound(key string) error {
	return fmt.Errorf("%s: %w", key, fs.ErrNotExist)
}
