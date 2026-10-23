package server

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/connect"
)

// Inserts close evidence for contractId accepted at acceptedAt.
func testInsertCloseEvidence(t testing.TB, ctx context.Context, contractId Id, acceptedAt time.Time) (clientId Id, reportId Id) {
	clientId = NewId()
	reportId = NewId()
	Tx(ctx, func(tx PgTx) {
		RaisePgResult(tx.Exec(
			ctx,
			`
			INSERT INTO contract_close_report_evidence (
				client_id, report_id, contract_id, party,
				acked_byte_count, unacked_byte_count, checkpoint, accepted_at
			) VALUES ($1, $2, $3, 'source', 1, 0, false, $4)
			`,
			clientId, reportId, contractId, acceptedAt,
		))
	})
	return
}

// Exercises a maintenance pass on partitioned contract tables 20 weeks after
// the cutover: partitions are created ahead, an expired week is dropped only
// when nothing in it must stay, its usage proof is archived first, a blocked
// week is reported with its reason, and expired close evidence is deleted
// unless its contract is unresolved.
func TestContractPartitionMaintenance(t *testing.T) {
	DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		now := NowUtc()

		// before the cutover week, so in the legacy partition
		legacyContractId := testContractIdAt(ContractPartitionWeekStart(now).Add(-24 * time.Hour))
		testInsertContractRows(t, ctx, legacyContractId, true)
		// the legacy partition keeps an unsettled escrow
		Tx(ctx, func(tx PgTx) {
			RaisePgResult(tx.Exec(ctx, `UPDATE transfer_escrow SET settled = false WHERE contract_id = $1`, legacyContractId))
		})

		Testing_PartitionContracts(ctx)
		cutoverWeek := ContractPartitionWeekStart(now)
		week := func(i int) time.Time {
			return cutoverWeek.Add(time.Duration(i) * contractPartitionWeek)
		}

		// week 0: a sweep of a pending payment
		pendingContractId := testContractIdAt(week(0).Add(time.Hour))
		testInsertContractRows(t, ctx, pendingContractId, true)
		Tx(ctx, func(tx PgTx) {
			paymentId := NewId()
			RaisePgResult(tx.Exec(
				ctx,
				`
				INSERT INTO account_payment (payment_id, payment_plan_id, wallet_id, payout_byte_count, payout_nano_cents, min_sweep_time)
				VALUES ($1, $2, $3, 1, 1, now())
				`,
				paymentId, NewId(), NewId(),
			))
			RaisePgResult(tx.Exec(ctx, `UPDATE transfer_escrow_sweep SET payment_id = $2 WHERE contract_id = $1`, pendingContractId, paymentId))
		})
		// week 1: settled contracts only, so it retires
		settledContractIds := []Id{
			testContractIdAt(week(1).Add(time.Hour)),
			testContractIdAt(week(1).Add(2 * time.Hour)),
		}
		for _, contractId := range settledContractIds {
			testInsertContractRows(t, ctx, contractId, true)
		}
		// week 2: an unresolved contract
		unresolvedContractId := testContractIdAt(week(2).Add(time.Hour))
		testInsertContractRows(t, ctx, unresolvedContractId, false)
		Tx(ctx, func(tx PgTx) {
			RaisePgResult(tx.Exec(ctx, `UPDATE transfer_escrow SET settled = true WHERE contract_id = $1`, unresolvedContractId))
		})
		// week 3: a legacy settlement intent
		intentContractId := testContractIdAt(week(3).Add(time.Hour))
		testInsertContractRows(t, ctx, intentContractId, true)
		Tx(ctx, func(tx PgTx) {
			RaisePgResult(tx.Exec(
				ctx,
				`INSERT INTO legacy_settlement_intent (contract_id, shard, outcome) VALUES ($1, get_byte(uuid_send($1), 15) % 16, 'settled')`,
				intentContractId,
			))
		})

		// close evidence: expired and resolved, expired but unresolved, recent
		expiredContractId := testContractIdAt(now.Add(-100 * 24 * time.Hour))
		expiredClientId, expiredReportId := testInsertCloseEvidence(t, ctx, expiredContractId, now.Add(-100*24*time.Hour))
		openClientId, openReportId := testInsertCloseEvidence(t, ctx, unresolvedContractId, now.Add(-100*24*time.Hour))
		recentClientId, recentReportId := testInsertCloseEvidence(t, ctx, NewId(), now.Add(-30*24*time.Hour))
		evidenceExists := func(clientId Id, reportId Id) bool {
			return testContractPartitionScalar[bool](t, ctx, `SELECT EXISTS (SELECT 1 FROM contract_close_report_evidence WHERE client_id = $1 AND report_id = $2)`, clientId, reportId)
		}

		later := week(20).Add(time.Hour)

		// without time to archive, the pass stops at the first retiring week
		// and keeps its cursor
		report, cursor := MaintainContractPartitions(ctx, later, ContractRetentionCursor{}, 0)
		connect.AssertEqual(t, report.Partitioned, true)
		connect.AssertEqual(t, report.More, true)
		connect.AssertEqual(t, cursor.ArchivePartition, ContractPartitionName("transfer_contract", week(1)))
		connect.AssertEqual(t, *cursor.ArchiveCursor, ContractIdFloor(week(1)))
		connect.AssertEqual(t, len(report.Dropped), 0)
		// the weeks ahead exist anyway
		connect.AssertEqual(t, report.FutureWeeks, ContractPartitionAheadWeeks)
		for i := 20; i <= 20+ContractPartitionAheadWeeks; i += 1 {
			for _, table := range ContractPartitionTables {
				connect.AssertEqual(t, slices.Contains(report.Created, ContractPartitionName(table, week(i))), true)
			}
		}

		report, cursor = MaintainContractPartitions(ctx, later, cursor, time.Minute)
		connect.AssertEqual(t, report.Blocked[ContractLegacyPartitionName("transfer_contract")], "an unsettled escrow")
		connect.AssertEqual(t, report.Blocked[ContractPartitionName("transfer_contract", week(0))], "a sweep of a pending payment")
		connect.AssertEqual(t, report.Blocked[ContractPartitionName("transfer_contract", week(2))], "an unresolved contract")
		connect.AssertEqual(t, report.Blocked[ContractPartitionName("transfer_contract", week(3))], "a legacy settlement intent")
		connect.AssertEqual(t, len(report.Blocked), 4)
		connect.AssertEqual(t, report.Archived, int64(len(settledContractIds)))
		connect.AssertEqual(t, cursor.ArchivePartition, "")
		for _, i := range []int{1, 4} {
			for _, table := range ContractPartitionTables {
				connect.AssertEqual(t, slices.Contains(report.Dropped, ContractPartitionName(table, week(i))), true)
			}
		}
		connect.AssertEqual(t, len(report.Dropped), 2*len(ContractPartitionTables))
		for _, contractId := range settledContractIds {
			connect.AssertEqual(t, testContractPartitionScalar[bool](t, ctx, `SELECT EXISTS (SELECT 1 FROM st_provider_usage_archive WHERE contract_id = $1)`, contractId), true)
			for _, table := range ContractPartitionTables {
				connect.AssertEqual(t, testContractPartitionScalar[bool](t, ctx, fmt.Sprintf(`SELECT EXISTS (SELECT 1 FROM public.%s WHERE contract_id = $1)`, quoteIdentifier(table)), contractId), false)
			}
		}
		// blocked weeks keep every row
		for _, contractId := range []Id{legacyContractId, pendingContractId, unresolvedContractId, intentContractId} {
			connect.AssertEqual(t, testContractPartitionScalar[bool](t, ctx, `SELECT EXISTS (SELECT 1 FROM transfer_contract WHERE contract_id = $1)`, contractId), true)
		}
		// the closed weeks and the parents were analyzed
		connect.AssertEqual(t, slices.Contains(report.Analyzed, ContractPartitionName("transfer_contract", week(0))), true)
		connect.AssertEqual(t, slices.Contains(report.Analyzed, "transfer_contract"), true)
		connect.AssertEqual(t, cursor.AnalyzedWeek, ContractPartitionWeekStart(later).Format(time.DateOnly))

		connect.AssertEqual(t, evidenceExists(expiredClientId, expiredReportId), false)
		connect.AssertEqual(t, evidenceExists(openClientId, openReportId), true)
		connect.AssertEqual(t, evidenceExists(recentClientId, recentReportId), true)
		connect.AssertEqual(t, report.EvidenceRemoved, int64(1))

		// a later pass is idempotent: nothing more to create, analyze or drop
		report, _ = MaintainContractPartitions(ctx, later, cursor, time.Minute)
		connect.AssertEqual(t, len(report.Created), 0)
		connect.AssertEqual(t, len(report.Dropped), 0)
		connect.AssertEqual(t, len(report.Blocked), 4)
		connect.AssertEqual(t, slices.Contains(report.Analyzed, "transfer_contract"), false)

		// once its payment completes, week 0 retires
		Tx(ctx, func(tx PgTx) {
			RaisePgResult(tx.Exec(ctx, `UPDATE account_payment SET completed = true, complete_time = now() WHERE payment_id = (SELECT payment_id FROM transfer_escrow_sweep WHERE contract_id = $1)`, pendingContractId))
		})
		report, _ = MaintainContractPartitions(ctx, later, cursor, time.Minute)
		connect.AssertEqual(t, slices.Contains(report.Dropped, ContractPartitionName("transfer_contract", week(0))), true)
		connect.AssertEqual(t, report.Archived, int64(1))
	})
}

// A cutover prepared and not finalized 6 hours before its week is abandoned,
// so its legacy bound never refuses a contract.
func TestContractPartitionAbandonsUnfinalizedCutover(t *testing.T) {
	DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		cutoverWeek := ContractPartitionWeekStart(NowUtc()).Add(2 * contractPartitionWeek)
		connect.AssertEqual(t, MigrateContractsToPartitions(ctx, ContractPartitionMigrateOptions{CutoverWeek: cutoverWeek, Logf: testContractPartitionLogf(t)}), nil)

		report, _ := MaintainContractPartitions(ctx, cutoverWeek.Add(-contractCutoverAbandonMargin-time.Minute), ContractRetentionCursor{}, time.Minute)
		connect.AssertEqual(t, report.AbandonedCutover, false)
		_, prepared := preparedContractCutoverWeek(ctx)
		connect.AssertEqual(t, prepared, true)

		report, _ = MaintainContractPartitions(ctx, cutoverWeek.Add(-contractCutoverAbandonMargin+time.Minute), ContractRetentionCursor{}, time.Minute)
		connect.AssertEqual(t, report.AbandonedCutover, true)
		connect.AssertEqual(t, report.Partitioned, false)
		_, prepared = preparedContractCutoverWeek(ctx)
		connect.AssertEqual(t, prepared, false)
		// a contract of the cutover week is admitted again
		testInsertContractRows(t, ctx, testContractIdAt(cutoverWeek.Add(time.Hour)), false)
	})
}

// The relations a statement's plan reads.
func testContractPlanRelations(t testing.TB, ctx context.Context, sql string, args ...any) (relations []string) {
	var plan string
	MaintenanceTx(ctx, func(tx PgTx) {
		Raise(tx.QueryRow(ctx, "EXPLAIN (FORMAT JSON) "+sql, args...).Scan(&plan))
	})
	var walk func(node any)
	walk = func(node any) {
		switch value := node.(type) {
		case map[string]any:
			if name, ok := value["Relation Name"].(string); ok {
				relations = append(relations, name)
			}
			for _, child := range value {
				walk(child)
			}
		case []any:
			for _, child := range value {
				walk(child)
			}
		}
	}
	var decoded any
	Raise(json.Unmarshal([]byte(plan), &decoded))
	walk(decoded)
	return
}

// A week's archive batch reads and deletes only that week's partition.
func TestContractArchiveBatchPrunesToItsWeek(t *testing.T) {
	DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		Testing_PartitionContracts(ctx)
		weekStart := ContractPartitionWeekStart(NowUtc()).Add(contractPartitionWeek)
		relations := testContractPlanRelations(
			t,
			ctx,
			contractArchiveBatchSql,
			ContractIdFloor(weekStart),
			ContractIdFloor(weekStart.Add(contractPartitionWeek)),
			contractArchiveBatchSize,
		)
		// the delete names the parent; every partition it reads is the week's
		leaf := ContractPartitionName("transfer_contract", weekStart)
		leaves := 0
		for _, relation := range relations {
			if strings.HasPrefix(relation, "transfer_contract_") {
				connect.AssertEqual(t, relation, leaf)
				leaves += 1
			}
		}
		connect.AssertEqual(t, 0 < leaves, true)
	})
}
