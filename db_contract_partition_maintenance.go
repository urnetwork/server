package server

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/urnetwork/glog"
)

// Partition maintenance and retention for the contract tables
// (CONTRACT-PARTITION.md "Ongoing maintenance" and "Retention"), one pass per
// task run:
//
//   - keep partitions for the current week and ContractPartitionAheadWeeks after
//     it, created standalone and attached so the parent sees only SHARE UPDATE
//     EXCLUSIVE;
//   - analyze each weekly partition once, a day after its week, and the parents
//     once a week (autovacuum does not analyze a partitioned parent);
//   - retire each week that ended more than ContractRetention ago: once every
//     contract is resolved, every escrow settled and no sweep belongs to a
//     pending payment, archive its usage proof by deleting its credit-bearing
//     contracts through the archive capture trigger, then detach and drop the
//     week's six partitions in one short transaction;
//   - delete close evidence older than the horizon (migration 857).
//
// Before the cutover it only guards a prepared cutover from outliving its
// week, and removes evidence.

// The contract tables' family in the partition registry
// (PARTITION-MAINTENANCE-PLAN.md A7, db_partition_registry.go). This task keeps
// its own pass: it analyzes and retires through the family, and keeps the
// archive step, the evidence deletes and the cutover guard. The registry's
// runway task creates its weeks ahead too, apart from the archive work.
// Registered from init, since its create refers back to the family.
var contractPartitionFamily *PartitionFamily

func init() {
	contractPartitionFamily = RegisterPartitionFamily(&PartitionFamily{
		Name:                  "contract",
		Description:           "the six contract tables, weekly by contract_id; maintain_contract_partitions analyzes and retires them",
		Tables:                append([]string{}, ContractPartitionTables...),
		KeyColumn:             "contract_id",
		KeyKind:               PartitionKeyId,
		Granularity:           PartitionGranularityWeekly,
		AheadRanges:           ContractPartitionAheadWeeks,
		Retention:             ContractRetention,
		LeafAnalyzeDelay:      contractPartitionAnalyzeDelay,
		AnalyzeByOwnTask:      true,
		RetireByOwnTask:       true,
		ReindexExempt:         true,
		MonitorRunwayRanges:   2,
		MonitorRetentionGrace: 2 * contractPartitionWeek,
		RunwaySignal:          "2.4h",
		RetentionSignal:       "2.4i",
		CreateLeaf:            createContractWeekLeaf,
		DropPrecondition: func(ctx context.Context, conn PgQuerier, r PartitionRange) string {
			return contractWeekBlocker(ctx, conn, contractWeekOfRange(r))
		},
	})
}

// Where an unfinished pass resumes.
type ContractRetentionCursor struct {
	// the partition whose usage proof is being archived, and the next id
	ArchivePartition string `json:"archive_partition,omitempty"`
	ArchiveCursor    *Id    `json:"archive_cursor,omitempty"`
	// where the close evidence row retention resumes, or starts its next
	// pass (RemoveExpiredCloseEvidence)
	EvidenceRemoval *CloseEvidenceRemovalCursor `json:"evidence_removal,omitempty"`
	// the close evidence cursor before 2026-11-02, a pass in progress: the
	// next contract id to consider and the partition it is in. Read once into
	// EvidenceRemoval.
	EvidenceCursor *Id    `json:"evidence_cursor,omitempty"`
	EvidenceLeaf   string `json:"evidence_leaf,omitempty"`
	// the week whose parents were last analyzed
	AnalyzedWeek string `json:"analyzed_week,omitempty"`
}

type ContractPartitionReport struct {
	Partitioned      bool
	AbandonedCutover bool
	Created          []string
	Analyzed         []string
	Dropped          []string
	Archived         int64
	EvidenceRemoved  int64
	// the retention the close evidence row deletes applied, and why they
	// stopped (CloseEvidenceRemovalReport)
	EvidenceRetention time.Duration
	EvidenceStop      string
	// expired weeks that stay, with the reason
	Blocked map[string]string
	// the fewest weeks after the current one with a partition, across tables
	FutureWeeks int
	// work remains for an immediate rerun
	More bool
	// a prepared close evidence cutover outlived its week's margin
	AbandonedEvidenceCutover bool
}

const contractArchiveBatchSize = 10000

// A weekly partition is analyzed this long after its week, when late closes
// and settlements have landed.
const contractPartitionAnalyzeDelay = 24 * time.Hour

func contractPartitionGlogf(format string, args ...any) {
	glog.Infof("[cp]"+format+"\n", args...)
}

// budget bounds the run: two thirds for the partitions and the rest for the
// close evidence, so neither backlog starves the other. The close evidence's
// own partitions (db_contract_close_evidence_partition.go) are created and
// dropped by the partition registry's tasks; this task keeps their row
// deletes and abandons a close evidence cutover that outlives its week's
// margin, as it does the contract cutover.
func MaintainContractPartitions(ctx context.Context, now time.Time, cursor ContractRetentionCursor, budget time.Duration) (report *ContractPartitionReport, next ContractRetentionCursor) {
	report = &ContractPartitionReport{Blocked: map[string]string{}}
	next = cursor
	deadline := time.Now().Add(budget * 2 / 3)

	if IsContractPartitioned(ctx) {
		report.Partitioned = true
		created, futureWeeks, err := ensureContractWeekPartitions(ctx, now)
		report.Created = created
		report.FutureWeeks = futureWeeks
		if err != nil {
			glog.Errorf("[cp]could not create the next contract partitions: %s\n", err)
		}
		report.Analyzed, next.AnalyzedWeek = analyzeContractPartitions(ctx, now, cursor.AnalyzedWeek, deadline)
		next = retireExpiredContractWeeks(ctx, now, next, deadline, report)
	} else if cutoverWeek, prepared := preparedContractCutoverWeek(ctx); prepared && !cutoverWeek.IsZero() && !now.Before(cutoverWeek.Add(-contractCutoverAbandonMargin)) {
		glog.Errorf(
			"[cp]abandoning the contract partition cutover prepared for %s: it was not finalized by %s, and its legacy bound would refuse every contract from that week\n",
			cutoverWeek.Format(time.DateOnly),
			cutoverWeek.Add(-contractCutoverAbandonMargin).Format(time.RFC3339),
		)
		if err := abortContractPartitionCutover(ctx, false, contractPartitionGlogf); err != nil {
			glog.Errorf("[cp]could not abandon the prepared cutover: %s\n", err)
		} else {
			report.AbandonedCutover = true
		}
	}

	report.AbandonedEvidenceCutover = abandonStaleCloseEvidenceCutover(ctx, now, DefaultCloseEvidencePartitionSettings(), contractPartitionGlogf)

	evidenceCursor := CloseEvidenceRemovalCursor{Leaf: cursor.EvidenceLeaf, ContractId: cursor.EvidenceCursor}
	if cursor.EvidenceRemoval != nil {
		evidenceCursor = *cursor.EvidenceRemoval
	}
	evidenceReport, evidenceNext, more := RemoveExpiredCloseEvidence(
		ctx,
		now,
		evidenceCursor,
		time.Now().Add(budget/3),
	)
	report.EvidenceRemoved = evidenceReport.Removed
	report.EvidenceRetention = evidenceReport.Retention
	report.EvidenceStop = evidenceReport.Stop
	next.EvidenceRemoval = &evidenceNext
	next.EvidenceLeaf = ""
	next.EvidenceCursor = nil
	if more {
		report.More = true
	}
	return
}

// Creates the missing partitions of the current week and the weeks ahead
// through the registry (EnsureRunway), and counts the weeks after the current
// one that have one in every table. Until the cutover week starts, the legacy
// partition covers the weeks before it.
func ensureContractWeekPartitions(ctx context.Context, now time.Time) (created []string, futureWeeks int, err error) {
	runway := contractPartitionFamily.EnsureRunway(ctx, now)
	if 0 < len(runway.Errors) {
		// retried on the next run
		err = errors.New(strings.Join(runway.Errors, "; "))
	}
	return runway.Created, runway.FutureRanges, err
}

// Creates table's partition for the week r (the family's CreateLeaf): an empty
// table with the parent's columns and checks, the storage settings of the
// newest partition (so a setting changed there carries forward) and the
// TRUNCATE guards, attached so the parent sees only SHARE UPDATE EXCLUSIVE.
func createContractWeekLeaf(ctx context.Context, table string, r PartitionRange) error {
	partitions, err := listContractPartitions(ctx, table)
	if err != nil {
		return err
	}
	template := ContractLegacyPartitionName(table)
	var newest time.Time
	for _, partition := range partitions {
		if !partition.legacy && newest.Before(partition.weekStart) {
			newest = partition.weekStart
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
	statements, err := contractWeekPartitionSql(table, parentShape, table, r.Lower, templateShape.reloptions, parentShape, table)
	if err != nil {
		return err
	}
	lockTimeout := contractPartitionFamily.settings().CreateLockTimeout
	return contractPartitionExec(ctx, append([]string{fmt.Sprintf("SET LOCAL lock_timeout = '%dms'", lockTimeout.Milliseconds())}, statements...))
}

// Analyzes each weekly partition once its week has settled, and the parents
// once a week, through the registry.
func analyzeContractPartitions(ctx context.Context, now time.Time, analyzedWeek string, deadline time.Time) (analyzed []string, nextAnalyzedWeek string) {
	analyzed = contractPartitionFamily.AnalyzeClosedLeaves(ctx, now, deadline)
	parents, nextAnalyzedWeek := contractPartitionFamily.AnalyzeParents(ctx, now, analyzedWeek, deadline)
	analyzed = append(analyzed, parents...)
	return
}

// One week's partition in each table.
type contractWeek struct {
	partition contractPartition
	lower     Id
	upper     Id
}

func (self contractWeek) leaf(table string) string {
	if self.partition.legacy {
		return ContractLegacyPartitionName(table)
	}
	return ContractPartitionName(table, self.partition.weekStart)
}

// The week as the registry's range.
func (self contractWeek) partitionRange() PartitionRange {
	if self.partition.legacy {
		return PartitionRange{Upper: self.partition.upper, Legacy: true}
	}
	return PartitionRange{Lower: self.partition.weekStart, Upper: self.partition.upper}
}

// The week of a registry range of the contract tables.
func contractWeekOfRange(r PartitionRange) contractWeek {
	name := ContractLegacyPartitionName("transfer_contract")
	if !r.Legacy {
		name = ContractPartitionName("transfer_contract", r.Lower)
	}
	week := contractWeek{
		partition: contractPartition{
			name:      name,
			legacy:    r.Legacy,
			weekStart: r.Lower,
			upper:     r.Upper,
		},
		upper: ContractIdFloor(r.Upper),
	}
	if !r.Legacy {
		week.lower = ContractIdFloor(r.Lower)
	}
	return week
}

// Why an expired week cannot be dropped yet, or "" if it can. Each check
// reads a partial or key index of the week's own partitions.
func contractWeekBlocker(ctx context.Context, conn contractCatalogQuerier, week contractWeek) string {
	exists := func(sql string, args ...any) (found bool) {
		result, err := conn.Query(ctx, sql, args...)
		WithPgResult(result, err, func() {
			if result.Next() {
				Raise(result.Scan(&found))
			}
		})
		return
	}
	if exists(fmt.Sprintf(`SELECT EXISTS (SELECT 1 FROM public.%s WHERE outcome IS NULL)`, quoteIdentifier(week.leaf("transfer_contract")))) {
		return "an unresolved contract"
	}
	if exists(fmt.Sprintf(`SELECT EXISTS (SELECT 1 FROM public.%s WHERE settled = false)`, quoteIdentifier(week.leaf("transfer_escrow")))) {
		return "an unsettled escrow"
	}
	if exists(fmt.Sprintf(
		`
		SELECT EXISTS (
			SELECT 1 FROM account_payment
			WHERE `+ContractPaymentRetentionHoldSql+` AND EXISTS (
				SELECT 1 FROM public.%s sweep WHERE sweep.payment_id = account_payment.payment_id
			)
		)
		`,
		quoteIdentifier(week.leaf("transfer_escrow_sweep")),
	)) {
		return "a sweep of a pending payment"
	}
	return ""
}

// The weeks whose upper bound is ContractRetention past, oldest first: the
// registry's expired ranges of the contract tables.
func expiredContractWeeks(ctx context.Context, now time.Time) (weeks []contractWeek, err error) {
	ranges, err := contractPartitionFamily.ExpiredRanges(ctx, now)
	if err != nil {
		return nil, err
	}
	for _, r := range ranges {
		weeks = append(weeks, contractWeekOfRange(r))
	}
	return
}

// Deletes the next batch of the week's credit-bearing contracts through the
// parent: the archive capture trigger copies each one's usage proof in the same
// transaction, exactly as the reaper does. The delete repeats the week's range
// so it prunes to the week's partition.
const contractArchiveBatchSql = `
	WITH batch AS (
		SELECT contract_id FROM transfer_contract
		WHERE $1 <= contract_id AND contract_id < $2 AND outcome IN ` + contractCreditOutcomesSql + `
		ORDER BY contract_id
		LIMIT $3
	)
	DELETE FROM transfer_contract
	WHERE $1 <= contract_id AND contract_id < $2 AND contract_id IN (SELECT contract_id FROM batch)
	RETURNING contract_id
`

func archiveContractBatch(ctx context.Context, from Id, upper Id) (count int64, last Id, err error) {
	err = contractPartitionTx(ctx, func(tx PgTx) {
		result, queryErr := tx.Query(
			ctx,
			contractArchiveBatchSql,
			from,
			upper,
			contractArchiveBatchSize,
		)
		WithPgResult(result, queryErr, func() {
			for result.Next() {
				var contractId Id
				Raise(result.Scan(&contractId))
				count += 1
				if last.Less(contractId) {
					last = contractId
				}
			}
		})
	})
	return
}

func retireExpiredContractWeeks(ctx context.Context, now time.Time, cursor ContractRetentionCursor, deadline time.Time, report *ContractPartitionReport) (next ContractRetentionCursor) {
	next = cursor
	weeks, err := expiredContractWeeks(ctx, now)
	if err != nil {
		glog.Errorf("[cp]%s\n", err)
		return
	}
	for _, week := range weeks {
		name := week.leaf("transfer_contract")
		var blocker string
		MaintenanceDb(ctx, func(conn PgConn) {
			blocker = contractWeekBlocker(ctx, conn, week)
		})
		if blocker != "" {
			report.Blocked[name] = blocker
			continue
		}

		from := week.lower
		if cursor.ArchivePartition == name && cursor.ArchiveCursor != nil {
			from = *cursor.ArchiveCursor
		}
		archived := false
		for time.Now().Before(deadline) {
			count, last, err := archiveContractBatch(ctx, from, week.upper)
			if err != nil {
				report.Blocked[name] = fmt.Sprintf("archive: %s", err)
				break
			}
			report.Archived += count
			if count == 0 {
				archived = true
				break
			}
			from = last
		}
		if !archived {
			if _, failed := report.Blocked[name]; !failed {
				next.ArchivePartition = name
				next.ArchiveCursor = &from
				report.More = true
			}
			return
		}
		next.ArchivePartition = ""
		next.ArchiveCursor = nil

		dropped, err := dropContractWeek(ctx, week)
		if err != nil {
			report.Blocked[name] = err.Error()
			continue
		}
		report.Dropped = append(report.Dropped, dropped...)
	}
	return
}

// Detaches and drops the week's six partitions in one transaction, children
// first, through the registry's guarded drop (PartitionFamily.DropRange): the
// blockers are checked again on the detached partitions, which no statement
// can reach. A detach takes a brief ACCESS EXCLUSIVE lock on each parent, so
// the drop first waits out no long transaction that holds a contract table
// lock, the lock wait is short, and a busy run retries next time.
func dropContractWeek(ctx context.Context, week contractWeek) (dropped []string, err error) {
	if busy, busyErr := contractTablesBusy(ctx, DefaultContractPartitionLockSettings().MaxTransactionAge); busyErr != nil {
		return nil, busyErr
	} else if 0 < busy {
		return nil, fmt.Errorf("%d long transaction(s) hold contract table locks; retried next run", busy)
	}
	return contractPartitionFamily.DropRange(ctx, week.partitionRange())
}
