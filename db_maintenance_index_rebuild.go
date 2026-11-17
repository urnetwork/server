package server

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// An operator's rebuild of one index (bringyourctl db reindex-index): the
// bloated indexes of the contract and identity tables, which the scheduled
// rotation never rebuilds whole (dbMaintenanceSkipReindexTables) and rebuilds
// as priority partials only behind priority_reindex_enabled (disk runway O2,
// owner 2026-11-10). It is the hot vacuum pass's rebuild
// (dbHotVacuumRun.rebuildIndex) of one named index: not while its table is
// vacuumed, analyzed or indexed, not while debris of an earlier rebuild
// remains on it, REINDEX INDEX CONCURRENTLY on a direct session of its own
// that waits for the table's lock to clear instead of queuing behind it
// (dbMaintenanceRunReindexWithin), and the debris dropped after a failure.
//
// Reads and writes continue throughout. The rebuild holds the table's SHARE
// UPDATE EXCLUSIVE lock to its end, so autovacuum skips the table meanwhile;
// it reads the table's heap twice, holds the vacuum horizon while it builds a
// partial or expression index, and needs disk for a whole new copy of the
// index until the old one is dropped. Its caller checks the free space and
// Main's health between rebuilds.
type IndexRebuildOptions struct {
	// bounds the wait for the table's lock to clear before the rebuild starts,
	// and then each lock and snapshot wait of its later phases
	LockTimeout time.Duration
	// bounds the whole statement
	Timeout time.Duration
	// max_parallel_maintenance_workers for the build; 0 builds on one cpu
	ParallelWorkers int
	// reads and reports the index, rebuilds nothing
	DryRun bool
	Logf   func(string, ...any)
}

const maxIndexRebuildParallelWorkers = 8

// The defaults wait out the hour-long payout and reliability transactions, as
// a priority rebuild does (DefaultDbMaintenancePriorityReindexLockTimeout), and
// allow the largest contract indexes their two heap reads.
func DefaultIndexRebuildOptions() *IndexRebuildOptions {
	return &IndexRebuildOptions{
		LockTimeout: DefaultDbMaintenancePriorityReindexLockTimeout,
		Timeout:     8 * time.Hour,
	}
}

func (self *IndexRebuildOptions) Validate() error {
	if self.LockTimeout < time.Millisecond || self.Timeout < time.Second {
		return errors.New("an index rebuild needs a lock timeout of at least a millisecond and a timeout of at least a second")
	}
	if self.ParallelWorkers < 0 || maxIndexRebuildParallelWorkers < self.ParallelWorkers {
		return fmt.Errorf("an index rebuild takes 0 to %d parallel workers, not %d", maxIndexRebuildParallelWorkers, self.ParallelWorkers)
	}
	return nil
}

// What one rebuild found and did.
type IndexRebuildResult struct {
	IndexName string
	TableName string
	// the index's size before and after, its compact estimate, and the heap
	// the rebuild reads twice
	BytesBefore  int64
	BytesAfter   int64
	CompactBytes int64
	HeapBytes    int64
	Rebuilt      bool
	Elapsed      time.Duration
}

// A public index name as PostgreSQL folds an unquoted identifier.
var indexRebuildNamePattern = regexp.MustCompile(`^[a-z_][a-z0-9_]*$`)

// Why indexName cannot be rebuilt by name, or "": it must be a plain lower
// case identifier of at most 63 bytes (optionally schema-qualified with
// public), and not the transient copy a concurrent rebuild makes.
func indexRebuildNameProblem(indexName string) string {
	if !indexRebuildNamePattern.MatchString(indexName) || pgMaxIdentifierLength < len(indexName) {
		return fmt.Sprintf("%q is not a plain public index name", indexName)
	}
	if label, ok := reindexArtifactLabel(indexName); ok {
		return fmt.Sprintf("%s is the transient %s copy of a concurrent rebuild; its cleanup drops it", indexName, label)
	}
	return ""
}

// The statement that rebuilds the index.
func indexRebuildStatement(indexName string) string {
	return `REINDEX INDEX CONCURRENTLY ` + pgx.Identifier{"public", indexName}.Sanitize()
}

// RebuildIndexConcurrently rebuilds one valid btree index of a plain public
// table, or a leaf index of a partitioned one, named directly. A partitioned
// index is refused: rebuild its leaves one at a time.
func RebuildIndexConcurrently(ctx context.Context, indexName string, options *IndexRebuildOptions) (*IndexRebuildResult, error) {
	if options == nil {
		options = DefaultIndexRebuildOptions()
	}
	if err := options.Validate(); err != nil {
		return nil, err
	}
	logf := options.Logf
	if logf == nil {
		logf = func(string, ...any) {}
	}
	indexName = strings.TrimPrefix(indexName, "public.")
	if problem := indexRebuildNameProblem(indexName); problem != "" {
		return nil, errors.New(problem)
	}
	indexes, err := dbMaintenancePriorityIndexSizes(ctx, []string{indexName})
	if err != nil {
		return nil, err
	}
	if len(indexes) != 1 || indexes[0].indexName != indexName {
		return nil, fmt.Errorf("%s is not a valid btree index of a plain public table (a partitioned index lists %d leaves: rebuild them one at a time)", indexName, len(indexes))
	}
	index := indexes[0]
	result := &IndexRebuildResult{
		IndexName:    index.indexName,
		TableName:    index.tableName,
		BytesBefore:  index.indexBytes,
		CompactBytes: index.compactBytes,
		HeapBytes:    index.heapBytes,
	}
	logf(
		"%s on %s: %d bytes, compact estimate %d bytes (0: unknown), heap %d bytes read twice",
		index.indexName, index.tableName, index.indexBytes, index.compactBytes, index.heapBytes,
	)
	if locked, err := dbMaintenanceTableLocked(ctx, index.tableName); err != nil {
		return result, err
	} else if locked {
		return result, fmt.Errorf("%s is being vacuumed, analyzed or indexed; rebuild %s once that ends", index.tableName, index.indexName)
	}
	statement := indexRebuildStatement(index.indexName)
	if options.DryRun {
		logf("would drop the debris of earlier rebuilds on %s, then run with lock_timeout %s, max_parallel_maintenance_workers %d, for at most %s:\n%s",
			index.tableName, options.LockTimeout, options.ParallelWorkers, options.Timeout, statement)
		logf("dry run: nothing changed")
		return result, nil
	}
	if remaining, err := dbMaintenanceDropReindexDebris(ctx, index.tableName, options.LockTimeout); err != nil {
		return result, err
	} else if 0 < remaining {
		return result, fmt.Errorf("%d incomplete index(es) remain on %s; they are dropped first", remaining, index.tableName)
	}

	logf("rebuilding %s: %s", index.indexName, statement)
	start := time.Now()
	pid, err := dbMaintenanceRunReindexWithin(ctx, index.tableName, statement, options.LockTimeout, options.Timeout, options.ParallelWorkers)
	result.Elapsed = time.Since(start)
	var lockConflict *dbMaintenanceLockConflict
	if errors.As(err, &lockConflict) {
		// nothing started, so nothing to clean up after
		return result, fmt.Errorf("%s did not start: %s", index.indexName, lockConflict.reason)
	}
	if err != nil {
		remaining, cleanupErr := dbMaintenanceCleanupAfterFailedReindex(ctx, index.tableName, pid, options.LockTimeout)
		if cleanupErr != nil || 0 < remaining {
			return result, fmt.Errorf("rebuild %s: %w; %d incomplete index(es) left for a later cleanup (err %v)", index.indexName, err, remaining, cleanupErr)
		}
		return result, fmt.Errorf("rebuild %s: %w; its incomplete copy was dropped", index.indexName, err)
	}
	result.Rebuilt = true
	if remaining, err := dbMaintenanceDropReindexDebris(ctx, index.tableName, options.LockTimeout); err != nil || 0 < remaining {
		logf("%d incomplete index(es) left on %s for a later cleanup (err %v)", remaining, index.tableName, err)
	}
	if rebuilt, err := dbMaintenancePriorityIndexSizes(ctx, []string{index.indexName}); err == nil && len(rebuilt) == 1 {
		result.BytesAfter = rebuilt[0].indexBytes
	}
	logf("rebuilt %s in %s: %d -> %d bytes", index.indexName, result.Elapsed.Round(time.Second), result.BytesBefore, result.BytesAfter)
	return result, nil
}
