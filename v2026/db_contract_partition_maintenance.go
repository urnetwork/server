package server

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/urnetwork/glog/v2026"
)

// Partition maintenance and retention for the contract tables
// (CONTRACT-PARTITION.md "Ongoing maintenance" and "S1"), one pass per task
// run:
//
//   - keep partitions for the current day and ContractPartitionAheadDays after
//     it, created standalone and attached so the parent sees only SHARE UPDATE
//     EXCLUSIVE;
//   - analyze each daily partition once, a day after its day, and the parents
//     once a day (autovacuum does not analyze a partitioned parent);
//   - retire each day at the contract horizon reaper's horizon
//     (db_contract_day_retirement.go): the reaper alone deletes contract rows,
//     by its keep rules; retirement compacts a day's leaves down to the rows
//     the reaper keeps and drops the day once it holds none;
//   - delete close evidence older than the horizon (migration 857).
//
// Before the cutover it only guards a prepared cutover from outliving its
// day, and removes evidence.

// The contract tables' family in the partition registry
// (PARTITION-MAINTENANCE-PLAN.md A7, db_partition_registry.go). This task keeps
// its own pass: it analyzes and retires through the family, and keeps the
// evidence deletes and the cutover guard. The registry's runway task creates
// its days ahead too. Registered from init, since its create refers back to
// the family.
var contractPartitionFamily *PartitionFamily

func init() {
	contractPartitionFamily = RegisterPartitionFamily(&PartitionFamily{
		Name:        "contract",
		Description: "the six contract tables, daily by contract_id; maintain_contract_partitions analyzes them, compacts each day the contract horizon reaper has emptied down to the rows it keeps, and drops a day once it holds none",
		Tables:      append([]string{}, ContractPartitionTables...),
		KeyColumn:   "contract_id",
		KeyKind:     PartitionKeyId,
		Granularity: ContractPartitionGranularity,
		AheadRanges: ContractPartitionAheadDays,
		// the longest retention, which the monitor judges blocked days by; a
		// day expires at the reaper's horizon (contractDayRangeExpired)
		Retention:           ContractRetention,
		LeafAnalyzeDelay:    contractPartitionAnalyzeDelay,
		AnalyzeByOwnTask:    true,
		RetireByOwnTask:     true,
		ReindexExempt:       true,
		MonitorRunwayRanges: 2,
		// two weeks past the longest retention, as when the ranges were weeks
		MonitorRetentionGrace: 14 * contractPartitionDay,
		RunwaySignal:          "2.4h",
		RetentionSignal:       "2.4i",
		Settings: &PartitionFamilySettings{
			CreateLockTimeout:    5 * time.Second,
			DropLockTimeout:      time.Second,
			DropStatementTimeout: 60 * time.Second,
		},
		// a leaf or parent ANALYZE never queues behind an autovacuum or a
		// long transaction (db_contract_partition_admission.go)
		AnalyzeAdmission:   contractAnalyzeAdmission,
		AnalyzeLockTimeout: time.Second,
		CreateLeaf:         createContractDayLeaf,
		RangeExpired:       contractDayRangeExpired,
		// a drop takes no tuple with it
		DropPrecondition: contractDayNotEmpty,
	})
}

// Where an unfinished pass resumes. A cursor from a build before S1 may carry
// archive_partition and archive_cursor, the archive step's position; they are
// read and ignored, since retirement no longer deletes contracts.
type ContractRetentionCursor struct {
	// where the close evidence row retention resumes, or starts its next
	// pass (RemoveExpiredCloseEvidence)
	EvidenceRemoval *CloseEvidenceRemovalCursor `json:"evidence_removal,omitempty"`
	// the close evidence cursor before 2026-11-02, a pass in progress: the
	// next contract id to consider and the partition it is in. Read once into
	// EvidenceRemoval.
	EvidenceCursor *Id    `json:"evidence_cursor,omitempty"`
	EvidenceLeaf   string `json:"evidence_leaf,omitempty"`
	// the day whose parents were last analyzed. A cursor from the weekly
	// build carries analyzed_week instead, which is ignored: the parents are
	// analyzed once more.
	AnalyzedDay string `json:"analyzed_day,omitempty"`
}

type ContractPartitionReport struct {
	Partitioned      bool
	AbandonedCutover bool
	Created          []string
	Analyzed         []string
	// leaves of expired days rewritten down to the rows they still hold
	Compacted []string
	// expired days that hold only rows the reaper keeps, by their
	// transfer_contract leaf, with the live rows their leaves hold
	Kept            map[string]int64
	Dropped         []string
	EvidenceRemoved int64
	// the retention the close evidence row deletes applied, and why they
	// stopped (CloseEvidenceRemovalReport)
	EvidenceRetention time.Duration
	EvidenceStop      string
	// expired days or their leaves that stay unretired this run, with the
	// reason
	Blocked map[string]string
	// the fewest days after the current one with a partition, across tables
	FutureDays int
	// work remains for an immediate rerun
	More bool
	// a prepared close evidence cutover outlived its week's margin
	AbandonedEvidenceCutover bool
}

// A daily partition is analyzed this long after its day, when late closes and
// settlements have landed.
const contractPartitionAnalyzeDelay = 24 * time.Hour

func contractPartitionGlogf(format string, args ...any) {
	glog.Infof("[cp]"+format+"\n", args...)
}

// Where the close evidence row retention resumes: evidence_removal, or the
// pass in progress that a cursor from before 2026-11-02 holds in its old fields.
func (self ContractRetentionCursor) evidenceRemovalCursor() CloseEvidenceRemovalCursor {
	if self.EvidenceRemoval != nil {
		return *self.EvidenceRemoval
	}
	return CloseEvidenceRemovalCursor{Leaf: self.EvidenceLeaf, ContractId: self.EvidenceCursor}
}

// This cursor with where the close evidence row retention resumes, in
// evidence_removal alone.
func (self ContractRetentionCursor) withEvidenceRemoval(removal CloseEvidenceRemovalCursor) ContractRetentionCursor {
	self.EvidenceRemoval = &removal
	self.EvidenceLeaf = ""
	self.EvidenceCursor = nil
	return self
}

// budget bounds the run: two thirds for the partitions and the rest for the
// close evidence, so neither backlog starves the other. The close evidence's
// own partitions (db_contract_close_evidence_partition.go) are created and
// dropped by the partition registry's tasks; this task keeps their row
// deletes and abandons a close evidence cutover that outlives its week's
// margin, as it does the contract cutover.
func MaintainContractPartitions(ctx context.Context, now time.Time, cursor ContractRetentionCursor, budget time.Duration) (report *ContractPartitionReport, next ContractRetentionCursor) {
	return maintainContractPartitions(ctx, now, cursor, budget, DefaultContractDayRetirementSettings())
}

func maintainContractPartitions(ctx context.Context, now time.Time, cursor ContractRetentionCursor, budget time.Duration, settings *ContractDayRetirementSettings) (report *ContractPartitionReport, next ContractRetentionCursor) {
	report = &ContractPartitionReport{Blocked: map[string]string{}, Kept: map[string]int64{}}
	next = cursor
	deadline := time.Now().Add(budget * 2 / 3)

	if IsContractPartitioned(ctx) {
		report.Partitioned = true
		created, futureDays, err := ensureContractDayPartitions(ctx, now)
		report.Created = created
		report.FutureDays = futureDays
		if err != nil {
			glog.Errorf("[cp]could not create the next contract partitions: %s\n", err)
		}
		report.Analyzed, next.AnalyzedDay = analyzeContractPartitions(ctx, now, cursor.AnalyzedDay, deadline)
		retireExpiredContractDays(ctx, now, settings, deadline, report)
	} else if cutoverDay, prepared := preparedContractCutoverDay(ctx); prepared && !cutoverDay.IsZero() && !now.Before(cutoverDay.Add(-contractCutoverAbandonMargin)) {
		glog.Errorf(
			"[cp]abandoning the contract partition cutover prepared for %s: it was not finalized by %s, and its legacy bound would refuse every contract from that day\n",
			cutoverDay.Format(time.DateOnly),
			cutoverDay.Add(-contractCutoverAbandonMargin).Format(time.RFC3339),
		)
		if err := abortContractPartitionCutover(ctx, false, contractPartitionGlogf, DefaultContractPartitionCutoverSettings()); err != nil {
			glog.Errorf("[cp]could not abandon the prepared cutover: %s\n", err)
		} else {
			report.AbandonedCutover = true
		}
	}

	report.AbandonedEvidenceCutover = abandonStaleCloseEvidenceCutover(ctx, now, DefaultCloseEvidencePartitionSettings(), contractPartitionGlogf)

	evidenceReport, evidenceNext, more := RemoveExpiredCloseEvidence(
		ctx,
		now,
		cursor.evidenceRemovalCursor(),
		time.Now().Add(budget/3),
	)
	report.EvidenceRemoved = evidenceReport.Removed
	report.EvidenceRetention = evidenceReport.Retention
	report.EvidenceStop = evidenceReport.Stop
	next = next.withEvidenceRemoval(evidenceNext)
	if more {
		report.More = true
	}
	return
}

// Creates the missing partitions of the current day and the days ahead
// through the registry (EnsureRunway), and counts the days after the current
// one that have one in every table. Until the cutover day starts, the legacy
// partition covers the days before it.
func ensureContractDayPartitions(ctx context.Context, now time.Time) (created []string, futureDays int, err error) {
	runway := contractPartitionFamily.EnsureRunway(ctx, now)
	if 0 < len(runway.Errors) {
		// retried on the next run
		err = errors.New(strings.Join(runway.Errors, "; "))
	}
	return runway.Created, runway.FutureRanges, err
}

// Creates table's partition for the day r (the family's CreateLeaf): an empty
// table with the parent's columns and checks, the storage settings of the
// newest partition (so a setting changed there carries forward) and the
// TRUNCATE guards, attached so the parent sees only SHARE UPDATE EXCLUSIVE.
func createContractDayLeaf(ctx context.Context, table string, r PartitionRange) error {
	partitions, err := listContractPartitions(ctx, table)
	if err != nil {
		return err
	}
	template := ContractLegacyPartitionName(table)
	var newest time.Time
	for _, partition := range partitions {
		if !partition.legacy && newest.Before(partition.dayStart) {
			newest = partition.dayStart
			template = partition.name
		}
	}
	var parentShape *contractTableShape
	var templateShape *contractTableShape
	MaintenanceDb(ctx, func(conn PgConn) {
		parentShape = readContractTableShape(ctx, conn, table)
		templateShape = readContractTableShape(ctx, conn, template)
	})
	if parentShape == nil || templateShape == nil {
		return fmt.Errorf("%s or %s is missing", table, template)
	}
	statements, err := contractDayPartitionSql(table, parentShape, table, r.Lower, templateShape.reloptions, parentShape, table)
	if err != nil {
		return err
	}
	lockTimeout := contractPartitionFamily.settings().CreateLockTimeout
	return contractPartitionExec(ctx, append([]string{fmt.Sprintf("SET LOCAL lock_timeout = '%dms'", lockTimeout.Milliseconds())}, statements...))
}

// Why an ANALYZE of a contract leaf or parent waits for a later run: an
// autovacuum, a prepared transaction or a transaction older than the
// admission's age holding a lock ANALYZE's SHARE UPDATE EXCLUSIVE conflicts
// with; or "". A parent's ANALYZE ONLY locks the parent alone that way (its
// leaves only for sampling, ACCESS SHARE).
func contractAnalyzeAdmission(ctx context.Context, relation string, leaf bool) string {
	scope := &contractLockScope{request: contractLockShareUpdateExclusive, settings: *DefaultContractPartitionLockSettings()}
	if leaf {
		scope.relations = []string{"public." + relation}
	} else {
		scope.only = []string{"public." + relation}
	}
	blockers, err := contractLockAdmission(ctx, scope)
	if err != nil {
		return fmt.Sprintf("its admission failed: %s", err)
	}
	return strings.Join(blockers, "; ")
}

// Analyzes each daily partition once its day has settled, and the parents
// once a day, through the registry.
func analyzeContractPartitions(ctx context.Context, now time.Time, analyzedDay string, deadline time.Time) (analyzed []string, nextAnalyzedDay string) {
	analyzed = contractPartitionFamily.AnalyzeClosedLeaves(ctx, now, deadline)
	parents, nextAnalyzedDay := contractPartitionFamily.AnalyzeParents(ctx, now, analyzedDay, deadline)
	analyzed = append(analyzed, parents...)
	return
}
