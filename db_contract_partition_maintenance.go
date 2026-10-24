package server

import (
	"context"
	"fmt"
	"slices"
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
//     contract is resolved, every escrow settled, no sweep belongs to a pending
//     payment and no legacy settlement intent refers to it, archive its usage
//     proof by deleting its credit-bearing contracts through the archive capture
//     trigger, then detach and drop the week's six partitions in one short
//     transaction;
//   - delete close evidence older than the horizon (migration 857).
//
// Before the cutover it only guards a prepared cutover from outliving its
// week, and removes evidence.

// Where an unfinished pass resumes.
type ContractRetentionCursor struct {
	// the partition whose usage proof is being archived, and the next id
	ArchivePartition string `json:"archive_partition,omitempty"`
	ArchiveCursor    *Id    `json:"archive_cursor,omitempty"`
	// the next close evidence contract id to consider
	EvidenceCursor *Id `json:"evidence_cursor,omitempty"`
	// the close evidence partition that id is in, once the evidence is
	// partitioned (RemoveExpiredCloseEvidence)
	EvidenceLeaf string `json:"evidence_leaf,omitempty"`
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

const closeEvidenceRetentionBatchSize = 5000

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

	removed, evidenceCursor, more := RemoveExpiredCloseEvidence(
		ctx,
		now,
		CloseEvidenceRemovalCursor{Leaf: cursor.EvidenceLeaf, ContractId: cursor.EvidenceCursor},
		time.Now().Add(budget/3),
	)
	report.EvidenceRemoved = removed
	next.EvidenceLeaf = evidenceCursor.Leaf
	next.EvidenceCursor = evidenceCursor.ContractId
	if more {
		report.More = true
	}
	return
}

// Creates the missing partitions of the current week and the weeks ahead, and
// counts the weeks after the current one that have one in every table. Until
// the cutover week starts, the legacy partition covers the weeks before it.
func ensureContractWeekPartitions(ctx context.Context, now time.Time) (created []string, futureWeeks int, err error) {
	currentWeek := ContractPartitionWeekStart(now)
	futureWeeks = -1
	for _, table := range ContractPartitionTables {
		partitions, listErr := listContractPartitions(ctx, table)
		if listErr != nil {
			return created, 0, listErr
		}
		existing := map[string]bool{}
		var legacyUpper time.Time
		// new partitions take the newest partition's storage settings, so a
		// setting changed there carries forward
		template := ContractLegacyPartitionName(table)
		var newest time.Time
		for _, partition := range partitions {
			existing[partition.name] = true
			if partition.legacy {
				legacyUpper = partition.upper
			} else if newest.Before(partition.weekStart) {
				newest = partition.weekStart
				template = partition.name
			}
		}
		covered := func(weekStart time.Time) bool {
			return existing[ContractPartitionName(table, weekStart)] || !legacyUpper.Before(weekStart.Add(contractPartitionWeek))
		}
		var parentShape *contractTableShape
		var templateShape *contractTableShape
		for week := 0; week <= ContractPartitionAheadWeeks; week += 1 {
			weekStart := currentWeek.Add(time.Duration(week) * contractPartitionWeek)
			name := ContractPartitionName(table, weekStart)
			if covered(weekStart) {
				continue
			}
			if parentShape == nil {
				MaintenanceDb(ctx, func(conn PgConn) {
					parentShape = readContractTableShape(ctx, conn, table)
					templateShape = readContractTableShape(ctx, conn, template)
				})
				if parentShape == nil || templateShape == nil {
					return created, 0, fmt.Errorf("%s or %s is missing", table, template)
				}
			}
			statements, sqlErr := contractWeekPartitionSql(table, parentShape, table, weekStart, templateShape.reloptions, parentShape, table)
			if sqlErr != nil {
				return created, 0, sqlErr
			}
			if execErr := contractPartitionExec(ctx, append([]string{`SET LOCAL lock_timeout = '5s'`}, statements...)); execErr != nil {
				// retried on the next run
				err = fmt.Errorf("create %s: %w", name, execErr)
				continue
			}
			existing[name] = true
			created = append(created, name)
		}
		tableFutureWeeks := 0
		for week := 1; covered(currentWeek.Add(time.Duration(week) * contractPartitionWeek)); week += 1 {
			tableFutureWeeks += 1
		}
		if futureWeeks < 0 || tableFutureWeeks < futureWeeks {
			futureWeeks = tableFutureWeeks
		}
	}
	return
}

// Analyzes each weekly partition once its week has settled, and the parents
// once a week.
func analyzeContractPartitions(ctx context.Context, now time.Time, analyzedWeek string, deadline time.Time) (analyzed []string, nextAnalyzedWeek string) {
	nextAnalyzedWeek = analyzedWeek
	analyze := func(statement string) bool {
		if err := contractPartitionExec(ctx, []string{`SET LOCAL statement_timeout = 0`, statement}); err != nil {
			glog.Errorf("[cp]%s: %s\n", statement, err)
			return false
		}
		return true
	}
	for _, table := range ContractPartitionTables {
		partitions, err := listContractPartitions(ctx, table)
		if err != nil {
			glog.Errorf("[cp]%s\n", err)
			continue
		}
		for _, partition := range partitions {
			settled := partition.upper.Add(contractPartitionAnalyzeDelay)
			if partition.legacy || now.Before(settled) || !time.Now().Before(deadline) {
				continue
			}
			var lastAnalyze *time.Time
			MaintenanceDb(ctx, func(conn PgConn) {
				result, err := conn.Query(
					ctx,
					`SELECT GREATEST(last_analyze, last_autoanalyze) FROM pg_stat_user_tables WHERE relid = to_regclass($1)`,
					"public."+partition.name,
				)
				WithPgResult(result, err, func() {
					if result.Next() {
						Raise(result.Scan(&lastAnalyze))
					}
				})
			})
			if lastAnalyze != nil && !lastAnalyze.Before(settled) {
				continue
			}
			if analyze(fmt.Sprintf("ANALYZE public.%s", quoteIdentifier(partition.name))) {
				analyzed = append(analyzed, partition.name)
			}
		}
	}
	if week := ContractPartitionWeekStart(now).Format(time.DateOnly); week != analyzedWeek && time.Now().Before(deadline) {
		parents := true
		for _, table := range ContractPartitionTables {
			if analyze(fmt.Sprintf("ANALYZE ONLY public.%s", quoteIdentifier(table))) {
				analyzed = append(analyzed, table)
			} else {
				parents = false
			}
		}
		if parents {
			nextAnalyzedWeek = week
		}
	}
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
	if exists(`SELECT EXISTS (SELECT 1 FROM legacy_settlement_intent WHERE $1 <= contract_id AND contract_id < $2)`, week.lower, week.upper) {
		return "a legacy settlement intent"
	}
	return ""
}

func expiredContractWeeks(ctx context.Context, now time.Time) (weeks []contractWeek, err error) {
	partitions, err := listContractPartitions(ctx, "transfer_contract")
	if err != nil {
		return nil, err
	}
	for _, partition := range partitions {
		if now.Before(partition.upper.Add(ContractRetention)) {
			continue
		}
		week := contractWeek{partition: partition, upper: ContractIdFloor(partition.upper)}
		if !partition.legacy {
			week.lower = ContractIdFloor(partition.weekStart)
		}
		weeks = append(weeks, week)
	}
	slices.SortFunc(weeks, func(a contractWeek, b contractWeek) int {
		return a.partition.upper.Compare(b.partition.upper)
	})
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
// first. The detach takes a brief ACCESS EXCLUSIVE lock on each parent, so the
// lock wait is short and a busy run retries next time. Once detached, no
// statement can reach the partitions, so the blockers are checked again there
// before the drop.
func dropContractWeek(ctx context.Context, week contractWeek) (dropped []string, err error) {
	tables := slices.Clone(ContractPartitionTables)
	slices.Reverse(tables)
	if busy, busyErr := contractTablesBusy(ctx, DefaultContractPartitionLockSettings().MaxTransactionAge); busyErr != nil {
		return nil, busyErr
	} else if 0 < busy {
		return nil, fmt.Errorf("%d long transaction(s) hold contract table locks; retried next run", busy)
	}
	err = contractPartitionTx(ctx, func(tx PgTx) {
		RaisePgResult(tx.Exec(ctx, `SET LOCAL lock_timeout = '5s'`))
		RaisePgResult(tx.Exec(ctx, `SET LOCAL statement_timeout = '60s'`))
		for _, table := range tables {
			RaisePgResult(tx.Exec(ctx, fmt.Sprintf(
				"ALTER TABLE public.%s DETACH PARTITION public.%s",
				quoteIdentifier(table),
				quoteIdentifier(week.leaf(table)),
			)))
		}
		if blocker := contractWeekBlocker(ctx, tx, week); blocker != "" {
			panic(fmt.Errorf("%s after detach", blocker))
		}
		for _, table := range tables {
			RaisePgResult(tx.Exec(ctx, fmt.Sprintf("DROP TABLE public.%s", quoteIdentifier(week.leaf(table)))))
		}
	})
	if err != nil {
		return nil, err
	}
	for _, table := range tables {
		dropped = append(dropped, week.leaf(table))
	}
	return
}

// Deletes close evidence accepted more than ContractRetention ago whose
// contract is resolved or gone, in batches ordered by contract id. Only a
// transaction that opts in may delete evidence, and the guard of migration 857
// admits only rows past the same 91 days. Once the evidence is partitioned the
// pass deletes from the leaves the drop cannot take (closeEvidenceRemovalTargets):
// the DEFAULT partition, the expired weeks a straggler keeps, and the legacy
// partition, one leaf after another; a week the drop will take, and a week
// not yet expired, are left alone. A pass ends when the last leaf's batch
// comes up short; the next pass starts over to pick up rows whose contract
// resolved since.
func RemoveExpiredCloseEvidence(ctx context.Context, now time.Time, cursor CloseEvidenceRemovalCursor, deadline time.Time) (removed int64, next CloseEvidenceRemovalCursor, more bool) {
	targets, err := closeEvidenceRemovalTargets(ctx, now)
	if err != nil {
		glog.Errorf("[cp]close evidence retention: %s\n", err)
		return 0, cursor, false
	}
	start, from := cursor.resumeIn(targets)
	for _, target := range targets[start:] {
		count, last, complete, ok := removeExpiredCloseEvidenceFrom(ctx, target, now, from, deadline)
		removed += count
		if !ok {
			return removed, cursor, false
		}
		if !complete {
			return removed, CloseEvidenceRemovalCursor{Leaf: target, ContractId: &last}, true
		}
		from = Id{}
	}
	return removed, CloseEvidenceRemovalCursor{}, false
}

// Where an unfinished close evidence pass resumes: the leaf it was deleting
// from (the table itself while it is plain) and the next contract id there.
type CloseEvidenceRemovalCursor struct {
	Leaf       string
	ContractId *Id
}

// The index of the target the pass resumes at, and the contract id it resumes
// from there. A cursor without a leaf comes from before the leaf was recorded,
// so it belongs to the plain table. A leaf that is no longer a target (dropped,
// or no longer kept by a straggler) starts the pass over.
func (self CloseEvidenceRemovalCursor) resumeIn(targets []string) (start int, from Id) {
	leaf := self.Leaf
	if leaf == "" && self.ContractId != nil {
		leaf = closeEvidenceTable
	}
	start = slices.Index(targets, leaf)
	if start < 0 {
		return 0, Id{}
	}
	if self.ContractId != nil {
		from = *self.ContractId
	}
	return
}

// Deletes the expired evidence of one leaf (or of the plain table) from the
// contract id from, until deadline. complete says a batch came up short, and
// ok is false when a batch failed; otherwise last is where the leaf resumes.
func removeExpiredCloseEvidenceFrom(ctx context.Context, leaf string, now time.Time, from Id, deadline time.Time) (removed int64, last Id, complete bool, ok bool) {
	upper := ContractIdFloor(now.Add(-ContractRetention))
	last = from
	for time.Now().Before(deadline) {
		var count int64
		batchLast := last
		err := contractPartitionTx(ctx, func(tx PgTx) {
			count = 0
			batchLast = last
			RaisePgResult(tx.Exec(ctx, `SET LOCAL urnetwork.close_evidence_retention = 'on'`))
			result, err := tx.Query(
				ctx,
				fmt.Sprintf(
					`
					WITH batch AS (
						SELECT evidence.client_id, evidence.report_id
						FROM public.%[1]s evidence
						WHERE $1 <= evidence.contract_id AND evidence.contract_id < $2
							AND evidence.accepted_at < (now() AT TIME ZONE 'UTC') - interval '91 days'
							AND NOT EXISTS (
								SELECT 1 FROM transfer_contract
								WHERE transfer_contract.contract_id = evidence.contract_id AND transfer_contract.outcome IS NULL
							)
						ORDER BY evidence.contract_id
						LIMIT $3
					)
					DELETE FROM public.%[1]s evidence
					USING batch
					WHERE evidence.client_id = batch.client_id AND evidence.report_id = batch.report_id
					RETURNING evidence.contract_id
					`,
					quoteIdentifier(leaf),
				),
				last,
				upper,
				closeEvidenceRetentionBatchSize,
			)
			WithPgResult(result, err, func() {
				for result.Next() {
					var contractId Id
					Raise(result.Scan(&contractId))
					count += 1
					if batchLast.Less(contractId) {
						batchLast = contractId
					}
				}
			})
		})
		if err != nil {
			glog.Errorf("[cp]close evidence retention of %s: %s\n", leaf, err)
			return removed, last, false, false
		}
		removed += count
		last = batchLast
		if count < closeEvidenceRetentionBatchSize {
			return removed, last, true, true
		}
	}
	return removed, last, false, true
}
