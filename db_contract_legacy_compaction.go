// The operator's compaction of the contract tables' legacy partition
// (CONTRACT-PARTITION-CUTOVER-NOW.md 4.1 item 4 and 4.6): S1's admission,
// count and CLUSTER on the six legacy leaves, under bounds the legacy needs.
//
// S1 (db_contract_day_retirement.go) retires the legacy partition as one range
// that expires at the cutover day plus the reaper's horizon and 6 hours, and
// compacts a leaf only while it holds at most 10,000 live rows, counted in 5 s
// and rewritten in 500 ms. The legacy keeps more than that for good: the
// reaper's unsettled-escrow contracts (60,337 on 2026-11-23), whose closes
// alone are about twice as many rows. So once the reaper has passed the
// cutover day and the legacy's primary keys have been rebuilt, an operator
// runs this, `bringyourctl db contract-legacy-compact`, in a quiet minute.
// Each leaf, smallest first:
//   - a leaf the statistics count more than ProbeGateRows (1M) live rows in,
//     the reaper has not emptied, and is reported without a read;
//   - its live rows are counted along its primary key, up to MaxCompactRows
//     (100,000) and within CountTimeout (5 s), read only; a leaf with more, or
//     whose count does not finish because its key still holds the reaper's
//     dead entries, is reported, untouched;
//   - it waits for nothing: the admission refuses while an autovacuum, a
//     prepared transaction or a transaction older than the admission's age
//     holds or awaits a lock on it or its TOAST table (S1's
//     contractLockAdmission), and the leaf is reported, untouched;
//   - CLUSTER rewrites the leaf along its key with only the live and recently
//     dead row versions, with their transaction ids, so every snapshot sees
//     the same rows after it; it waits at most 1 s for the leaf's lock and is
//     cut off after CompactTimeout (3 s), rolling back with nothing changed.
//     The old files go back to the filesystem at its commit.
//
// It deletes no row and judges none: every row the reaper keeps stays, as S1
// keeps it. It never detaches or drops the legacy partition; S1 drops it once
// its leaves hold no tuple. While a leaf's CLUSTER runs, statements that read
// that leaf wait for it, up to the application's 2 s lock bound: the hot reads
// prune to the current day, but reads without a contract id bound probe every
// leaf, so run it once those are bounded (decision 5) or in a quiet minute.
package server

import (
	"context"
	"fmt"
	"slices"
	"time"
)

// Bounds of the operator's legacy compaction.
type ContractLegacyCompactionSettings struct {
	// The most live rows a legacy leaf may hold to be rewritten: the rows the
	// reaper keeps for good (unsettled escrows, about 60k contracts and twice
	// as many closes on 2026-11-23), with room.
	MaxCompactRows int64
	// Bounds the count of a leaf's live rows, a read only walk of its key.
	CountTimeout time.Duration
	// Bounds CLUSTER's work once it holds the leaf's lock: the most an
	// application statement on that leaf waits behind it, beyond the lock wait.
	CompactTimeout time.Duration
	// Admission, lock wait and statistics gate, as S1's.
	LockSettings  ContractPartitionLockSettings
	LockTimeout   time.Duration
	ProbeGateRows int64
	// A leaf whose heap and TOAST are at most this large while it holds live
	// rows is compact already, as S1 judges it, and is not rewritten again.
	CompactAboveBytes int64
	// Counts and admits only, and rewrites nothing.
	DryRun bool
	// The contract tables whose legacy leaves to compact; every one when
	// empty.
	Tables []string
}

func DefaultContractLegacyCompactionSettings() *ContractLegacyCompactionSettings {
	retirement := DefaultContractDayRetirementSettings()
	return &ContractLegacyCompactionSettings{
		MaxCompactRows:    100000,
		CountTimeout:      retirement.ProbeTimeout,
		CompactTimeout:    3 * time.Second,
		LockSettings:      retirement.LockSettings,
		LockTimeout:       retirement.LockTimeout,
		ProbeGateRows:     retirement.ProbeGateRows,
		CompactAboveBytes: retirement.CompactAboveBytes,
	}
}

// The widest bounds an operator may set: the work under the leaf's lock stays
// a few seconds, and the count a read of at most a few hundred thousand keys.
const (
	contractLegacyCompactionMaxRows         = 250000
	contractLegacyCompactionMaxTimeout      = 10 * time.Second
	contractLegacyCompactionMaxCountTimeout = time.Minute
)

// Refuses bounds that would hold a legacy leaf's lock longer than a few
// seconds, wait longer for it than S1 does, or name a table that is not a
// contract table.
func (self *ContractLegacyCompactionSettings) Validate() error {
	if self.MaxCompactRows < 1 || contractLegacyCompactionMaxRows < self.MaxCompactRows {
		return fmt.Errorf("the row bound %d is outside 1 to %d", self.MaxCompactRows, contractLegacyCompactionMaxRows)
	}
	if self.CompactTimeout <= 0 || contractLegacyCompactionMaxTimeout < self.CompactTimeout {
		return fmt.Errorf("the compaction's statement bound %s is outside (0, %s]", self.CompactTimeout, contractLegacyCompactionMaxTimeout)
	}
	if self.CountTimeout <= 0 || contractLegacyCompactionMaxCountTimeout < self.CountTimeout {
		return fmt.Errorf("the count's statement bound %s is outside (0, %s]", self.CountTimeout, contractLegacyCompactionMaxCountTimeout)
	}
	if self.LockTimeout <= 0 || DefaultContractDayRetirementSettings().LockTimeout < self.LockTimeout {
		return fmt.Errorf("the lock wait %s is outside (0, %s]", self.LockTimeout, DefaultContractDayRetirementSettings().LockTimeout)
	}
	if self.LockSettings.MaxTransactionAge <= 0 || self.ProbeGateRows < self.MaxCompactRows {
		return fmt.Errorf("the admission's transaction age %s or the statistics gate %d is out of range", self.LockSettings.MaxTransactionAge, self.ProbeGateRows)
	}
	for _, table := range self.Tables {
		if !slices.Contains(ContractPartitionTables, table) {
			return fmt.Errorf("%q is not a contract table (%v)", table, ContractPartitionTables)
		}
	}
	return nil
}

// S1's settings with these bounds.
func (self *ContractLegacyCompactionSettings) retirementSettings() *ContractDayRetirementSettings {
	settings := DefaultContractDayRetirementSettings()
	settings.LockSettings = self.LockSettings
	settings.LockTimeout = self.LockTimeout
	settings.ProbeGateRows = self.ProbeGateRows
	settings.MaxCompactRows = self.MaxCompactRows
	settings.ProbeTimeout = self.CountTimeout
	settings.CompactTimeout = self.CompactTimeout
	settings.CompactAboveBytes = self.CompactAboveBytes
	return settings
}

// One legacy leaf's compaction.
type ContractLegacyLeafCompaction struct {
	Table string
	Leaf  string
	// heap and TOAST bytes before, and after a compaction
	BytesBefore int64
	BytesAfter  int64
	// the live rows the statistics count, -1 without statistics
	StatisticsLiveRows int64
	// the live rows counted along the key, -1 when not counted
	LiveRows  int64
	Compacted bool
	// why the leaf was not rewritten, or ""
	Reason string
}

// One run of the operator's legacy compaction.
type ContractLegacyCompactionReport struct {
	// the legacy partition's upper bound: the cutover day
	CutoverDay time.Time
	Leaves     []ContractLegacyLeafCompaction
}

// Compacts each legacy leaf of the contract tables down to the row versions it
// still holds, smallest first, under the settings' bounds (see the file
// comment). A leaf that waits is reported with the reason; the run goes on to
// the next. Errors only when the run cannot start: invalid settings, contract
// tables not partitioned, or no legacy partition.
func CompactContractLegacyPartition(ctx context.Context, settings *ContractLegacyCompactionSettings, logf func(string, ...any)) (*ContractLegacyCompactionReport, error) {
	if err := settings.Validate(); err != nil {
		return nil, err
	}
	if !IsContractPartitioned(ctx) {
		return nil, fmt.Errorf("the contract tables are not partitioned: there is no legacy partition")
	}
	partitions, err := listContractPartitions(ctx, ContractPartitionTables[0])
	if err != nil {
		return nil, err
	}
	report := &ContractLegacyCompactionReport{}
	for _, partition := range partitions {
		if partition.legacy {
			report.CutoverDay = partition.upper
		}
	}
	if report.CutoverDay.IsZero() {
		return nil, fmt.Errorf("%s has no legacy partition", ContractPartitionTables[0])
	}
	legacy := PartitionRange{Legacy: true, Upper: report.CutoverDay}
	leaves, err := readContractDayLeaves(ctx, legacy)
	if err != nil {
		return nil, err
	}
	slices.SortStableFunc(leaves, func(a contractDayLeaf, b contractDayLeaf) int {
		switch {
		case a.bytes() < b.bytes():
			return -1
		case b.bytes() < a.bytes():
			return 1
		default:
			return 0
		}
	})
	retirement := settings.retirementSettings()
	for _, leaf := range leaves {
		if 0 < len(settings.Tables) && !slices.Contains(settings.Tables, leaf.table) {
			continue
		}
		select {
		case <-ctx.Done():
			return report, ctx.Err()
		default:
		}
		compaction := compactContractLegacyLeaf(ctx, legacy, leaf, settings, retirement)
		if compaction.Compacted {
			logf("compacted %s: %d bytes to %d, %d live rows", compaction.Leaf, compaction.BytesBefore, compaction.BytesAfter, compaction.LiveRows)
		} else {
			logf("%s stays, %d bytes: %s", compaction.Leaf, compaction.BytesBefore, compaction.Reason)
		}
		report.Leaves = append(report.Leaves, compaction)
	}
	return report, nil
}

// One leaf: the statistics gate, the count, the admission, then CLUSTER.
func compactContractLegacyLeaf(ctx context.Context, legacy PartitionRange, leaf contractDayLeaf, settings *ContractLegacyCompactionSettings, retirement *ContractDayRetirementSettings) (compaction ContractLegacyLeafCompaction) {
	compaction = ContractLegacyLeafCompaction{
		Table:              leaf.table,
		Leaf:               leaf.name,
		BytesBefore:        leaf.bytes(),
		BytesAfter:         leaf.bytes(),
		StatisticsLiveRows: leaf.liveRows,
		LiveRows:           -1,
	}
	switch {
	case !leaf.exists:
		compaction.Reason = "the leaf is missing"
		return
	case leaf.bytes() == 0:
		compaction.Reason = "it holds no rows"
		return
	case settings.ProbeGateRows < leaf.liveRows:
		compaction.Reason = fmt.Sprintf("about %d live rows by the statistics: the reaper has not emptied it, or keeps more than %d", leaf.liveRows, settings.MaxCompactRows)
		return
	}
	// a read only walk of the key (ACCESS SHARE), which a vacuum does not
	// block; it waits at most the lock wait behind an exclusive lock
	live, err := contractDayLeafLiveRows(ctx, leaf, settings.MaxCompactRows+1, retirement)
	if err != nil {
		compaction.Reason = contractRetirementFailure("count its live rows", err) +
			"; a count its bound cuts off means the key still holds the reaper's dead entries: rebuild the leaf's primary key first"
		return
	}
	compaction.LiveRows = live
	if settings.MaxCompactRows < live {
		compaction.Reason = fmt.Sprintf("more than %d live rows: the reaper keeps them, or has not passed the cutover day", settings.MaxCompactRows)
		return
	}
	if 0 < live && leaf.bytes() <= settings.CompactAboveBytes {
		compaction.Reason = fmt.Sprintf("compact already: %d live rows in %d bytes", live, leaf.bytes())
		return
	}
	// read only: what the compaction's lock request would wait for
	blockers, err := contractLockAdmission(ctx, contractCompactScope(leaf, retirement))
	if err != nil {
		compaction.Reason = contractRetirementFailure("admit", err)
		return
	}
	if 0 < len(blockers) {
		compaction.Reason = contractRetirementFailure("compact", &ContractPartitionBusyError{Blockers: blockers})
		return
	}
	if settings.DryRun {
		compaction.Reason = fmt.Sprintf("dry run: %d live rows would be rewritten out of %d bytes", live, leaf.bytes())
		return
	}
	// admits again, right before its lock request
	if err := compactContractDayLeaf(ctx, leaf, retirement); err != nil {
		compaction.Reason = contractRetirementFailure("compact", err)
		return
	}
	compaction.Compacted = true
	if after, err := readContractDayLeaves(ctx, legacy); err == nil {
		for _, other := range after {
			if other.name == leaf.name {
				compaction.BytesAfter = other.bytes()
			}
		}
	}
	return
}
