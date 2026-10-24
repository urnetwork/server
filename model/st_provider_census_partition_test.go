// The epoch census over partitioned close evidence (PARTITION-MAINTENANCE-PLAN.md
// C5): each contract's reports are read within its receipts' report id range,
// so the read prunes to the partitions holding them and stays complete for a
// client clock far off; an archived contract reads every partition; and while
// a cutover holds moved rows outside the table, the census waits for the swap.
package model

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/server"
)

// The bounded read comes from the contract's receipts in the same statement,
// and the unbounded control keeps the plain lookup.
func TestStCensusBoundsReportsByReceipts(t *testing.T) {
	sql := strings.Join(strings.Fields(stEpochProviderOriginalUsageSql), " ")
	for _, want := range []string{
		"(SELECT receipt.report_id FROM contract_close_report receipt WHERE receipt.contract_id=usage.contract_id ORDER BY receipt.report_id LIMIT 1) AS first_report_id",
		"(SELECT receipt.report_id FROM contract_close_report receipt WHERE receipt.contract_id=usage.contract_id ORDER BY receipt.report_id DESC LIMIT 1) AS last_report_id",
		"EXISTS(SELECT 1 FROM transfer_contract live WHERE live.contract_id=usage.contract_id) AS live",
		"FROM contract_close_report_evidence WHERE contract_id=usage.contract_id AND CASE WHEN report_range.live THEN report_range.first_report_id ELSE '00000000-0000-0000-0000-000000000000'::uuid END<=report_id AND report_id<=CASE WHEN report_range.live THEN report_range.last_report_id ELSE 'ffffffff-ffff-ffff-ffff-ffffffffffff'::uuid END",
	} {
		if !strings.Contains(sql, want) {
			t.Errorf("census lacks %q", want)
		}
	}
	if strings.Contains(stOriginalUsageUnboundedWindowTestSql, "report_range") {
		t.Error("the unbounded control reads the receipts")
	}
}

// One settled contract in the census window with a receipt and its retained
// report for each report id.
func addStCensusPartitionTestContract(t testing.TB, ctx context.Context, closeTime time.Time, reportIds ...server.Id) server.Id {
	t.Helper()
	contractId := addStContractUsageSnapshotTestRow(t, ctx, closeTime, &contractUsageSnapshot{
		Version: 1, ByteCount: 7, Providers: []contractProviderUsage{{ClientId: server.NewId(), NetworkId: server.NewId(), ByteCount: 7}},
	})
	server.Tx(ctx, func(tx server.PgTx) {
		for _, reportId := range reportIds {
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO contract_close_report (contract_id,party,report_id,used_transfer_byte_count,checkpoint) VALUES ($1,'source',$2,7,false)`, contractId, reportId))
			server.RaisePgResult(tx.Exec(ctx, `
				INSERT INTO contract_close_report_evidence (client_id,report_id,contract_id,party,acked_byte_count,unacked_byte_count,checkpoint,accepted_at)
				VALUES ($1,$2,$3,'source',7,0,false,$4)
			`, server.NewId(), reportId, contractId, closeTime))
		}
	})
	return contractId
}

// A report id its client minted at at.
func stCensusPartitionTestReportId(at time.Time) server.Id {
	id := server.NewId()
	floor := server.ContractIdFloor(at)
	copy(id[0:6], floor[0:6])
	return id
}

// The loops of every plan node that reads relation.
func stCensusPartitionTestLoops(value any, relation string) (loops float64) {
	node, ok := value.(map[string]any)
	if !ok {
		return 0
	}
	if node["Relation Name"] == relation {
		nodeLoops, _ := node["Actual Loops"].(float64)
		loops += nodeLoops
	}
	if children, ok := node["Plans"].([]any); ok {
		for _, child := range children {
			loops += stCensusPartitionTestLoops(child, relation)
		}
	}
	return
}

// The reports of every census row, by contract.
func stCensusPartitionTestReports(t testing.TB, rows map[server.Id]stCensusPlanTestRow) map[server.Id]int {
	t.Helper()
	counts := map[server.Id]int{}
	for contractId, row := range rows {
		if contractId == (server.Id{}) || len(row.Reports) == 0 {
			continue
		}
		var census struct {
			Count int `json:"count"`
		}
		server.Raise(json.Unmarshal(row.Reports, &census))
		counts[contractId] = census.Count
	}
	return counts
}

// Reports in the current week, a week ahead (a client clock running ahead),
// two years behind (in the legacy partition) and far ahead (in DEFAULT) are
// all in the census, exactly as the unbounded read finds them, while the
// weeks no census contract has a report in are never read. An archived
// contract, whose receipts left with it, still has its report in the census.
func TestStCensusReadsOnlyTheWeeksOfItsReports(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		now := server.NowUtc().Truncate(time.Microsecond)
		currentWeek := server.PartitionGranularityWeekly.RangeStart(now)
		server.Testing_PartitionCloseEvidence(ctx, currentWeek)
		week := func(weeks int) time.Time {
			return currentWeek.Add(time.Duration(weeks)*7*24*time.Hour + time.Hour)
		}
		start := now.Add(-time.Hour)
		end := now.Add(time.Hour)

		current := addStCensusPartitionTestContract(t, ctx, now, stCensusPartitionTestReportId(now), stCensusPartitionTestReportId(now.Add(time.Second)))
		ahead := addStCensusPartitionTestContract(t, ctx, now, stCensusPartitionTestReportId(now), stCensusPartitionTestReportId(week(1)))
		behind := addStCensusPartitionTestContract(t, ctx, now, stCensusPartitionTestReportId(now.Add(-2*365*24*time.Hour)))
		farAhead := addStCensusPartitionTestContract(t, ctx, now, stCensusPartitionTestReportId(week(40)))
		// reports in the other weeks belong to contracts outside the window
		for _, weeks := range []int{2, 3, 4} {
			addStCensusPartitionTestContract(t, ctx, now.Add(-30*24*time.Hour), stCensusPartitionTestReportId(week(weeks)))
		}

		check := func(want map[server.Id]int) {
			t.Helper()
			server.Db(ctx, func(conn server.PgConn) {
				bounded := stCensusPlanTestRows(t, ctx, conn, stEpochProviderOriginalUsageSql, start, end)
				unbounded := stCensusPlanTestRows(t, ctx, conn, stOriginalUsageUnboundedWindowTestSql, start, end)
				if !reflect.DeepEqual(bounded, unbounded) {
					t.Fatal("the bounded census differs from the unbounded read")
				}
				if reports := stCensusPartitionTestReports(t, bounded); !reflect.DeepEqual(reports, want) {
					t.Fatalf("census reports %v, want %v", reports, want)
				}
			})
		}
		check(map[server.Id]int{current: 2, ahead: 2, behind: 1, farAhead: 1})

		server.Db(ctx, func(conn server.PgConn) {
			var raw []byte
			server.Raise(conn.QueryRow(ctx, `EXPLAIN (ANALYZE, FORMAT JSON) `+stEpochProviderOriginalUsageSql, start, end).Scan(&raw))
			var documents []map[string]any
			server.Raise(json.Unmarshal(raw, &documents))
			plan := documents[0]["Plan"]
			family := server.GetPartitionFamily("close_evidence")
			for _, weeks := range []int{2, 3, 4} {
				leaf := family.LeafName("contract_close_report_evidence", family.RangeAt(week(weeks)))
				if loops := stCensusPartitionTestLoops(plan, leaf); loops != 0 {
					t.Errorf("the census read %s %v times; no census report is there", leaf, loops)
				}
			}
			for _, leaf := range []string{
				family.LeafName("contract_close_report_evidence", family.RangeAt(now)),
				family.LeafName("contract_close_report_evidence", family.RangeAt(week(1))),
				"contract_close_report_evidence_legacy",
				"contract_close_report_evidence_default",
			} {
				if stCensusPartitionTestLoops(plan, leaf) == 0 {
					t.Errorf("the census did not read %s, which holds its reports", leaf)
				}
			}
		})

		// archived, the contract has no receipts: its reports are read from
		// every partition
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `DELETE FROM transfer_contract WHERE contract_id=$1`, ahead))
		})
		check(map[server.Id]int{current: 2, ahead: 2, behind: 1, farAhead: 1})
	})
}

// While a prepared cutover holds rows moved out of the evidence table, the
// census refuses rather than miss their reports; usage alone still reads. After
// the swap the census is the one from before the cutover.
func TestStCensusWaitsForCloseEvidenceCutoverSwap(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		now := server.NowUtc().Truncate(time.Microsecond)
		cutoverWeek := server.PartitionGranularityWeekly.RangeStart(now).Add(7 * 24 * time.Hour)
		start := now.Add(-time.Hour)
		end := now.Add(time.Hour)
		addStCensusPartitionTestContract(t, ctx, now, stCensusPartitionTestReportId(now), stCensusPartitionTestReportId(cutoverWeek.Add(14*24*time.Hour)))
		_, before, err := GetStEpochProviderUsageCensus(ctx, 17, start, end)
		if err != nil || before == nil || before.Count != 1 {
			t.Fatal("census before the cutover", before, err)
		}

		settings := server.DefaultCloseEvidencePartitionSettings()
		settings.PrepareLeadTime = 0
		server.Raise(server.MigrateCloseEvidenceToPartitions(ctx, server.CloseEvidencePartitionMigrateOptions{CutoverWeek: cutoverWeek, Settings: settings, Logf: t.Logf}))
		if _, _, err := GetStEpochProviderUsageCensus(ctx, 17, start, end); !errors.Is(err, server.ErrCloseEvidenceCutoverMoving) {
			t.Fatal("the census read while moved reports were outside the table", err)
		}
		if usages, err := GetStEpochProviderUsage(ctx, start, end); err != nil || len(usages) != 1 {
			t.Fatal("usage alone did not read during the cutover", usages, err)
		}

		server.Raise(server.MigrateCloseEvidenceToPartitions(ctx, server.CloseEvidencePartitionMigrateOptions{Finalize: true, Settings: settings, Logf: t.Logf}))
		_, after, err := GetStEpochProviderUsageCensus(ctx, 17, start, end)
		if err != nil || after == nil || after.Hash() != before.Hash() {
			t.Fatal("the census after the swap differs from before the cutover", after, err)
		}
	})
}
