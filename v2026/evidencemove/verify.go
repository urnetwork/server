package evidencemove

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"sync"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/startifact"
)

// Items held in memory, in cursor order: a sample to classify (a dry run over
// it) or a fixed list.
type SliceSource struct {
	items []Item
}

func NewSliceSource(items []Item) *SliceSource {
	sorted := slices.Clone(items)
	slices.SortFunc(sorted, func(a Item, b Item) int { return compareCursors(a.Cursor, b.Cursor) })
	return &SliceSource{items: sorted}
}

func compareCursors(a Cursor, b Cursor) int {
	if order := a.ClientId.Cmp(b.ClientId); order != 0 {
		return order
	}
	switch {
	case a.Generation < b.Generation:
		return -1
	case a.Generation > b.Generation:
		return 1
	}
	return 0
}

func (self *SliceSource) Next(ctx context.Context, after Cursor, limit int) ([]Item, error) {
	start, _ := slices.BinarySearchFunc(self.items, after, func(item Item, cursor Cursor) int {
		return compareCursors(item.Cursor, cursor)
	})
	for start < len(self.items) && compareCursors(self.items[start].Cursor, after) <= 0 {
		start += 1
	}
	return slices.Clone(self.items[start:min(len(self.items), start+limit)]), nil
}

// The content hashes a journal records as removed from the flat layout.
func ReadJournalRemovals(path string) ([]string, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	hashes := []string{}
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)
	for scanner.Scan() {
		var result Result
		if err := json.Unmarshal(scanner.Bytes(), &result); err != nil {
			return nil, fmt.Errorf("journal %s: %w", path, err)
		}
		if result.Outcome == OutcomeMoved || result.Outcome == OutcomeDeduped {
			hashes = append(hashes, result.Hash)
		}
	}
	return hashes, scanner.Err()
}

// One read through the readers' own path (startifact.OpenEvidenceContent: the
// sharded key, then the flat key), checked as GET /sn/evidence checks it: the
// envelope's content hash is the one asked for. The bytes must also verify.
func ReadThroughReaders(ctx context.Context, reader server.BlobStore, hash string, maxBytes int64) error {
	content, err := startifact.OpenEvidenceContent(ctx, reader, hash)
	if err != nil {
		return fmt.Errorf("open: %w", err)
	}
	data, readErr := io.ReadAll(io.LimitReader(content, maxBytes+1))
	closeErr := content.Close()
	if err := errors.Join(readErr, closeErr); err != nil {
		return fmt.Errorf("read: %w", err)
	}
	if maxBytes < int64(len(data)) {
		return fmt.Errorf("more than %d bytes", maxBytes)
	}
	var envelope startifact.EvidenceEnvelope
	if err := json.Unmarshal(data, &envelope); err != nil {
		return fmt.Errorf("not an envelope: %w", err)
	}
	if !strings.EqualFold(envelope.ContentHash, "sha256:"+strings.TrimPrefix(strings.ToLower(hash), "sha256:")) {
		return fmt.Errorf("content hash %s answered for %s", envelope.ContentHash, hash)
	}
	if err := startifact.VerifyEvidence(&envelope); err != nil {
		return fmt.Errorf("envelope does not verify: %w", err)
	}
	return nil
}

// What a verification found. Failures holds the first few failures.
type VerifyReport struct {
	// moved hashes whose sharded object reads through the readers
	Readable int
	// moved hashes whose flat object is gone
	FlatGone int
	// unmoved hashes (flat only) that read through the readers' fallback
	FallbackReadable int
	// unmoved hashes sampled, by layout
	FlatOnly, Both, ShardedOnly, Neither int
	Failures                             []string
}

func (self *VerifyReport) fail(lock *sync.Mutex, format string, args ...any) {
	lock.Lock()
	defer lock.Unlock()
	if len(self.Failures) < 50 {
		self.Failures = append(self.Failures, fmt.Sprintf(format, args...))
	} else if len(self.Failures) == 50 {
		self.Failures = append(self.Failures, "...")
	}
}

// Checks every moved hash: its flat key holds no object, and it reads through
// the readers from its sharded key. Then, for unmoved items, that each one
// stored only flat still reads through the readers' fallback. Requests are
// paced and gated like a move; nothing is written.
func Verify(ctx context.Context, store ObjectStore, reader server.BlobStore, prefix string, moved []string, unmoved []Item, gates *GateKeeper, pacer *Pacer, workers int, maxBytes int64) (*VerifyReport, error) {
	report := &VerifyReport{}
	var lock sync.Mutex
	run := func(count int, check func(ctx context.Context, index int) error) error {
		indexes := make(chan int)
		var wait sync.WaitGroup
		var firstErr error
		for range max(1, min(workers, count)) {
			wait.Add(1)
			go func() {
				defer wait.Done()
				for index := range indexes {
					if err := gates.Wait(ctx); err != nil {
						lock.Lock()
						firstErr = err
						lock.Unlock()
						continue
					}
					if err := pacer.Wait(ctx); err != nil {
						lock.Lock()
						firstErr = err
						lock.Unlock()
						continue
					}
					if err := check(ctx, index); err != nil {
						lock.Lock()
						firstErr = err
						lock.Unlock()
					}
				}
			}()
		}
		for index := range count {
			if ctx.Err() != nil {
				break
			}
			indexes <- index
		}
		close(indexes)
		wait.Wait()
		return firstErr
	}
	err := run(len(moved), func(ctx context.Context, index int) error {
		keys, err := KeysFor(prefix, moved[index])
		if err != nil {
			report.fail(&lock, "%s: %v", moved[index], err)
			return nil
		}
		if _, err := store.Stat(ctx, keys.Flat); err == nil {
			report.fail(&lock, "%s: the flat object is still stored", keys.Hash)
		} else if server.IsBlobNotFound(err) {
			lock.Lock()
			report.FlatGone += 1
			lock.Unlock()
		} else {
			return fmt.Errorf("%s: flat HEAD: %w", keys.Hash, err)
		}
		if _, err := store.Stat(ctx, keys.Sharded); err != nil {
			report.fail(&lock, "%s: the sharded object is not stored: %v", keys.Hash, err)
			return nil
		}
		if err := ReadThroughReaders(ctx, reader, keys.Hash, maxBytes); err != nil {
			report.fail(&lock, "%s: %v", keys.Hash, err)
			return nil
		}
		lock.Lock()
		report.Readable += 1
		lock.Unlock()
		return nil
	})
	if err != nil {
		return report, err
	}
	err = run(len(unmoved), func(ctx context.Context, index int) error {
		keys, err := KeysFor(prefix, unmoved[index].Hash)
		if err != nil {
			report.fail(&lock, "%s: %v", unmoved[index].Hash, err)
			return nil
		}
		_, flatErr := store.Stat(ctx, keys.Flat)
		_, shardedErr := store.Stat(ctx, keys.Sharded)
		for _, err := range []error{flatErr, shardedErr} {
			if err != nil && !server.IsBlobNotFound(err) {
				return fmt.Errorf("%s: HEAD: %w", keys.Hash, err)
			}
		}
		lock.Lock()
		switch {
		case flatErr == nil && shardedErr == nil:
			report.Both += 1
		case flatErr == nil:
			report.FlatOnly += 1
		case shardedErr == nil:
			report.ShardedOnly += 1
		default:
			report.Neither += 1
		}
		lock.Unlock()
		if flatErr != nil || shardedErr == nil {
			return nil
		}
		if err := ReadThroughReaders(ctx, reader, keys.Hash, maxBytes); err != nil {
			report.fail(&lock, "%s (flat only): %v", keys.Hash, err)
			return nil
		}
		lock.Lock()
		report.FallbackReadable += 1
		lock.Unlock()
		return nil
	})
	return report, err
}
