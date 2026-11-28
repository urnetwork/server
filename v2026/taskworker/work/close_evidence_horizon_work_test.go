// The tasks that remove close evidence keep its own window, 7 days, while the
// evidence brake is on, read once per run (2026-11-23): the evidence's own
// chain deletes the rows, the registry's retention task drops the weeks, and
// the contract task no longer touches either. The horizon reaper's control
// moves none of them.
package work

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
	"github.com/urnetwork/server/v2026/session"
	"github.com/urnetwork/server/v2026/task"
)

// A report of a deleted contract accepted 10 days ago goes at the evidence's
// window once the brake allows, whatever the reaper's control says; one
// accepted 5 days ago stays. With the brake off nothing goes. The contract
// task deletes no evidence, even past 35 days with the reaper on at 18 days,
// where it deleted it before.
func TestRemoveExpiredCloseEvidenceRemovesAtItsWindow(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		owner := session.NewLocalClientSession(ctx, "", nil)
		defer owner.Cancel()
		defer removeCloseEvidenceHorizonTestControl(t, ctx)
		defer removeCloseEvidenceTestBrake(t, ctx)
		now := server.NowUtc()
		const day = 24 * time.Hour
		aged := insertCloseEvidenceHorizonTestReport(t, ctx, now.Add(-40*day))
		tenDay := insertCloseEvidenceHorizonTestReport(t, ctx, now.Add(-10*day))
		inside := insertCloseEvidenceHorizonTestReport(t, ctx, now.Add(-5*day))
		run := func() *RemoveExpiredCloseEvidenceResult {
			result, err := RemoveExpiredCloseEvidence(&RemoveExpiredCloseEvidenceArgs{}, owner)
			if err != nil {
				t.Fatal(err)
			}
			return result
		}

		// the contract task leaves the evidence, at the reaper's 18 days
		setCloseEvidenceHorizonTestControl(t, ctx, "18")
		removeCloseEvidenceTestBrake(t, ctx)
		if _, err := MaintainContractPartitions(&MaintainContractPartitionsArgs{}, owner); err != nil {
			t.Fatal(err)
		}
		if !closeEvidenceHorizonTestReportPresent(t, ctx, aged) || !closeEvidenceHorizonTestReportPresent(t, ctx, tenDay) {
			t.Fatal("the contract task removed close evidence")
		}

		// the brake off: nothing goes
		setCloseEvidenceTestBrake(t, ctx, map[string]string{server.CloseEvidenceRetentionControlFieldMode: "off"})
		if result := run(); result.More || !closeEvidenceHorizonTestReportPresent(t, ctx, aged) || !closeEvidenceHorizonTestReportPresent(t, ctx, tenDay) {
			t.Fatalf("with the brake off the evidence task removed evidence or asked for more: %+v", result)
		}

		// the brake on, the reaper off or at 18 days: the window applies
		for _, reaper := range []string{"off", "18"} {
			if reaper == "off" {
				removeCloseEvidenceHorizonTestControl(t, ctx)
			} else {
				setCloseEvidenceHorizonTestControl(t, ctx, reaper)
			}
			removeCloseEvidenceTestBrake(t, ctx)
			result := run()
			if result.More || result.Cursor.InProgress() || result.Cursor.LowWater == nil {
				t.Fatalf("a finished pass left %+v", result)
			}
			if closeEvidenceHorizonTestReportPresent(t, ctx, aged) || closeEvidenceHorizonTestReportPresent(t, ctx, tenDay) {
				t.Fatalf("with the reaper %s the evidence task kept a deleted contract's report accepted 10 or 40 days ago", reaper)
			}
			if !closeEvidenceHorizonTestReportPresent(t, ctx, inside) {
				t.Fatalf("with the reaper %s the evidence task removed a report accepted 5 days ago", reaper)
			}
			tenDay = insertCloseEvidenceHorizonTestReport(t, ctx, now.Add(-10*day))
		}
	})
}

// Post hands the run's cursor to the next run, through the task's stored
// arguments: MaxBatchWait later while the pass has work left, 15 minutes later
// once it is done or the brake stopped it.
func TestRemoveExpiredCloseEvidencePostCarriesTheCursor(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		owner := session.NewLocalClientSession(ctx, "", nil)
		defer owner.Cancel()
		key := task.RunOnce("remove_expired_close_evidence").String()
		lowWater := server.ContractIdFloor(server.NowUtc().Add(-7 * 24 * time.Hour))
		cursor := server.CloseEvidenceRemovalCursor{LowWater: &lowWater, FullPassDay: server.NowUtc().Format(time.DateOnly)}
		for _, test := range []struct {
			more  bool
			delay time.Duration
		}{
			{true, server.DefaultCloseEvidenceRetentionSettings().MaxBatchWait},
			{false, 15 * time.Minute},
		} {
			server.Tx(ctx, func(tx server.PgTx) {
				server.RaisePgResult(tx.Exec(ctx, `DELETE FROM pending_task WHERE run_once_key = $1`, key))
			})
			before := server.NowUtc()
			server.Tx(ctx, func(tx server.PgTx) {
				server.Raise(RemoveExpiredCloseEvidencePost(&RemoveExpiredCloseEvidenceArgs{}, &RemoveExpiredCloseEvidenceResult{Cursor: cursor, More: test.more}, owner, tx))
			})
			after := server.NowUtc()
			var argsJson string
			var runAt time.Time
			server.Db(ctx, func(conn server.PgConn) {
				server.Raise(conn.QueryRow(ctx, `SELECT args_json, run_at FROM pending_task WHERE run_once_key = $1`, key).Scan(&argsJson, &runAt))
			})
			var args RemoveExpiredCloseEvidenceArgs
			server.Raise(json.Unmarshal([]byte(argsJson), &args))
			if args.Cursor.LowWater == nil || *args.Cursor.LowWater != lowWater || args.Cursor.FullPassDay != cursor.FullPassDay {
				t.Fatalf("the next run's cursor is %+v, want %+v", args.Cursor, cursor)
			}
			if runAt.Before(before.Add(test.delay).Add(-time.Second)) || after.Add(test.delay).Add(time.Second).Before(runAt) {
				t.Fatalf("more %t: the next run is at %s, want %s after %s", test.more, runAt, test.delay, before)
			}
		}
	})
}

// Partitioned evidence: a week whose upper bound is 14 to 21 days past,
// holding only a deleted contract's report, drops at the registry's retention
// task's next run once the brake is on, whatever the reaper's control, and
// stays while the brake is off.
func TestMaintainPartitionRetentionDropsCloseEvidenceWeeksAtTheWindow(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		owner := session.NewLocalClientSession(ctx, "", nil)
		defer owner.Cancel()
		defer removeCloseEvidenceHorizonTestControl(t, ctx)
		defer removeCloseEvidenceTestBrake(t, ctx)
		now := server.NowUtc()
		const day = 24 * time.Hour
		currentWeek := server.PartitionGranularityWeekly.RangeStart(now)
		server.Testing_PartitionCloseEvidence(ctx, currentWeek.Add(-20*7*day))
		family := server.GetPartitionFamily("close_evidence")
		week := family.RangeAt(currentWeek.Add(-3*7*day + day))
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

		// the brake off, with the reaper on at 18 days: the week stays
		setCloseEvidenceHorizonTestControl(t, ctx, "18")
		setCloseEvidenceTestBrake(t, ctx, map[string]string{server.CloseEvidenceRetentionControlFieldMode: "off"})
		run()
		if !attached() || !closeEvidenceHorizonTestReportPresent(t, ctx, report) {
			t.Fatal("with the brake off the retention task dropped a week inside 91 days")
		}

		removeCloseEvidenceTestBrake(t, ctx)
		run()
		if attached() || closeEvidenceHorizonTestReportPresent(t, ctx, report) {
			t.Fatal("at the 7-day window the retention task kept a week 14 days past")
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

func setCloseEvidenceTestBrake(t testing.TB, ctx context.Context, fields map[string]string) {
	t.Helper()
	controlCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	server.Raise(server.RemoveCloseEvidenceRetentionControl(controlCtx))
	server.Raise(server.SetCloseEvidenceRetentionControl(controlCtx, fields))
}

func removeCloseEvidenceTestBrake(t testing.TB, ctx context.Context) {
	t.Helper()
	controlCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	server.Raise(server.RemoveCloseEvidenceRetentionControl(controlCtx))
}
