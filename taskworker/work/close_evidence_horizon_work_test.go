// Both tasks that remove close evidence read the contract horizon from the
// horizon reaper's runtime control: the contract task's row deletes, and the
// registry's retention task's weekly drops.
package work

import (
	"context"
	"testing"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
)

// A report of a deleted contract, accepted 50 days ago, stays while the
// control sets no horizon, and the contract task deletes it once horizonDays
// is 45; a report accepted 40 days ago stays at 45 days.
func TestMaintainContractPartitionsRemovesCloseEvidenceAtTheHorizon(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		owner := session.NewLocalClientSession(ctx, "", nil)
		defer owner.Cancel()
		defer removeCloseEvidenceHorizonTestControl(t, ctx)
		now := server.NowUtc()
		const day = 24 * time.Hour
		aged := insertCloseEvidenceHorizonTestReport(t, ctx, now.Add(-50*day))
		inside := insertCloseEvidenceHorizonTestReport(t, ctx, now.Add(-40*day))
		run := func() {
			if _, err := MaintainContractPartitions(&MaintainContractPartitionsArgs{}, owner); err != nil {
				t.Fatal(err)
			}
		}

		removeCloseEvidenceHorizonTestControl(t, ctx)
		run()
		if !closeEvidenceHorizonTestReportPresent(t, ctx, aged) || !closeEvidenceHorizonTestReportPresent(t, ctx, inside) {
			t.Fatal("without the control the task removed evidence inside 91 days")
		}

		setCloseEvidenceHorizonTestControl(t, ctx, "45")
		run()
		if closeEvidenceHorizonTestReportPresent(t, ctx, aged) {
			t.Fatal("at a 45-day horizon the task kept a deleted contract's report accepted 50 days ago")
		}
		if !closeEvidenceHorizonTestReportPresent(t, ctx, inside) {
			t.Fatal("at a 45-day horizon the task removed a report accepted 40 days ago")
		}
	})
}

// Partitioned evidence: a week whose upper bound is 49 to 56 days past, holding
// only a deleted contract's report, stays while the control sets no horizon,
// and the registry's retention task drops it once horizonDays is 45.
func TestMaintainPartitionRetentionDropsCloseEvidenceWeeksAtTheHorizon(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		owner := session.NewLocalClientSession(ctx, "", nil)
		defer owner.Cancel()
		defer removeCloseEvidenceHorizonTestControl(t, ctx)
		now := server.NowUtc()
		const day = 24 * time.Hour
		currentWeek := server.PartitionGranularityWeekly.RangeStart(now)
		server.Testing_PartitionCloseEvidence(ctx, currentWeek.Add(-20*7*day))
		family := server.GetPartitionFamily("close_evidence")
		week := family.RangeAt(currentWeek.Add(-8*7*day + day))
		leaf := family.LeafName("contract_close_report_evidence", week)
		server.Raise(family.CreateRange(ctx, "contract_close_report_evidence", week))
		report := insertCloseEvidenceHorizonTestReport(t, ctx, week.Lower.Add(day))
		attached := func() (exists bool) {
			server.Db(ctx, func(conn server.PgConn) {
				server.Raise(conn.QueryRow(ctx, `SELECT to_regclass($1) IS NOT NULL`, "public."+leaf).Scan(&exists))
			})
			return
		}
		run := func() {
			if _, err := MaintainPartitionRetention(&MaintainPartitionRetentionArgs{}, owner); err != nil {
				t.Fatal(err)
			}
		}

		removeCloseEvidenceHorizonTestControl(t, ctx)
		run()
		if !attached() || !closeEvidenceHorizonTestReportPresent(t, ctx, report) {
			t.Fatal("without the control the retention task dropped a week inside 91 days")
		}

		setCloseEvidenceHorizonTestControl(t, ctx, "45")
		run()
		if attached() || closeEvidenceHorizonTestReportPresent(t, ctx, report) {
			t.Fatal("at a 45-day horizon the retention task kept a week 49 days past")
		}
	})
}

// One report of a contract that no longer exists, minted and accepted at
// acceptedAt.
type closeEvidenceHorizonTestReport struct {
	clientId server.Id
	reportId server.Id
}

func insertCloseEvidenceHorizonTestReport(t testing.TB, ctx context.Context, acceptedAt time.Time) closeEvidenceHorizonTestReport {
	t.Helper()
	idAt := func(at time.Time) server.Id {
		id := server.NewId()
		floor := server.ContractIdFloor(at)
		copy(id[:6], floor[:6])
		return id
	}
	report := closeEvidenceHorizonTestReport{clientId: server.NewId(), reportId: idAt(acceptedAt)}
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(ctx, `INSERT INTO contract_close_report_evidence
			(client_id,report_id,contract_id,party,acked_byte_count,unacked_byte_count,checkpoint,accepted_at)
			VALUES($1,$2,$3,'source',0,0,true,$4)`,
			report.clientId, report.reportId, idAt(acceptedAt.Add(-time.Minute)), acceptedAt.UTC()))
	}, server.TxReadCommitted, server.OptNoRetry())
	return report
}

func closeEvidenceHorizonTestReportPresent(t testing.TB, ctx context.Context, report closeEvidenceHorizonTestReport) (present bool) {
	t.Helper()
	server.Db(ctx, func(conn server.PgConn) {
		server.Raise(conn.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM contract_close_report_evidence WHERE client_id=$1 AND report_id=$2)`,
			report.clientId, report.reportId).Scan(&present))
	})
	return
}

func setCloseEvidenceHorizonTestControl(t testing.TB, ctx context.Context, horizonDays string) {
	t.Helper()
	controlCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	server.Raise(model.SetContractHorizonControl(controlCtx, map[string]string{
		model.ContractHorizonControlFieldMode:        "on",
		model.ContractHorizonControlFieldHorizonDays: horizonDays,
	}))
}

func removeCloseEvidenceHorizonTestControl(t testing.TB, ctx context.Context) {
	t.Helper()
	controlCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	server.Raise(model.RemoveContractHorizonControl(controlCtx))
}
