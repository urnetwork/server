package work

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
)

// readSweepOrphanContractPending returns the scheduled args and run_at of the
// single sweep_orphan_contract_data row, failing if it is missing.
func readSweepOrphanContractPending(
	ctx context.Context,
	t testing.TB,
	tx server.PgTx,
) (args SweepOrphanContractDataArgs, runAt time.Time) {
	found := false
	result, err := tx.Query(
		ctx,
		`
		SELECT args_json, run_at
		FROM pending_task
		WHERE run_once_key = '["sweep_orphan_contract_data"]'
		`,
	)
	server.WithPgResult(result, err, func() {
		if result.Next() {
			found = true
			var argsJson string
			server.Raise(result.Scan(&argsJson, &runAt))
			server.Raise(json.Unmarshal([]byte(argsJson), &args))
		}
	})
	if !found {
		t.Fatal("no sweep_orphan_contract_data row scheduled")
	}
	return args, runAt
}

// The sweep pass is far bigger than any single run, so it is spread: a run pages
// a bounded number of rows, returns where it stopped, and Post hands that cursor
// to the next run. This pins the whole chain.
//
// It is the regression test for the 2026-08-11 production finding: the run used
// to page until the task deadline CANCELED it, which meant it never returned
// normally, so Post never ran, so the chain fell back to error-retry and every
// retry restarted the pass from row zero. It had never completed a single pass
// in its life, ran ~63% of wall clock, and cost 7.6% of all db time. The budget
// must therefore stay comfortably under MaxTime, and Post must carry the cursor.
func TestSweepOrphanContractDataResumesMidPass(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		clientSession := session.NewLocalClientSession(ctx, "0.0.0.0:0", nil)
		defer clientSession.Cancel()

		cursor := model.SweepOrphanCursor{
			Step: 1,
			Key:  []string{server.NewId().String(), server.NewId().String()},
		}

		before := server.NowUtc()
		server.Tx(ctx, func(tx server.PgTx) {
			err := SweepOrphanContractDataPost(
				&SweepOrphanContractDataArgs{},
				&SweepOrphanContractDataResult{
					Cursor: cursor,
					Done:   false,
				},
				clientSession,
				tx,
			)
			if err != nil {
				t.Fatalf("Post returned %v", err)
			}

			args, runAt := readSweepOrphanContractPending(ctx, t, tx)

			// the next run resumes at the exact position this run reached --
			// without this the pass restarts from zero forever
			if args.Cursor.Step != cursor.Step {
				t.Fatalf("resume step = %d, want %d", args.Cursor.Step, cursor.Step)
			}
			if len(args.Cursor.Key) != len(cursor.Key) {
				t.Fatalf("resume key = %v, want %v", args.Cursor.Key, cursor.Key)
			}
			for i := range cursor.Key {
				if args.Cursor.Key[i] != cursor.Key[i] {
					t.Fatalf("resume key = %v, want %v", args.Cursor.Key, cursor.Key)
				}
			}

			// mid-pass resumes run on the short resume cadence, not weekly
			delay := runAt.Sub(before)
			if delay < sweepOrphanContractResumeTimeout-time.Minute ||
				sweepOrphanContractResumeTimeout+time.Minute < delay {
				t.Fatalf("mid-pass resume scheduled %v out, want ~%v", delay, sweepOrphanContractResumeTimeout)
			}
		})
	})
}

// A completed pass resets to a fresh cursor on the weekly cadence, so the sweep
// does not simply run back to back forever.
func TestSweepOrphanContractDataRestartsPassWhenDone(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		clientSession := session.NewLocalClientSession(ctx, "0.0.0.0:0", nil)
		defer clientSession.Cancel()

		before := server.NowUtc()
		server.Tx(ctx, func(tx server.PgTx) {
			err := SweepOrphanContractDataPost(
				&SweepOrphanContractDataArgs{},
				&SweepOrphanContractDataResult{
					// a done run still reports the cursor it ended on; Post must
					// ignore it and start the next pass at the head
					Cursor: model.SweepOrphanCursor{Step: 2, Key: []string{server.NewId().String()}},
					Done:   true,
				},
				clientSession,
				tx,
			)
			if err != nil {
				t.Fatalf("Post returned %v", err)
			}

			args, runAt := readSweepOrphanContractPending(ctx, t, tx)

			if args.Cursor.Step != 0 || 0 < len(args.Cursor.Key) {
				t.Fatalf("completed pass reschedules with cursor %+v, want the zero cursor", args.Cursor)
			}

			// weekly, not the resume cadence
			delay := runAt.Sub(before)
			if delay < 24*time.Hour {
				t.Fatalf("completed pass reschedules %v out, want the weekly cadence", delay)
			}
		})
	})
}

// The row budget is what keeps a run finishing NORMALLY, which is what lets Post
// run at all. If the budget ever grows toward MaxTime the task goes back to
// being canceled mid-run and the chain breaks exactly as it did before
// 2026-08-11 -- silently, since a canceled task just retries.
func TestSweepOrphanContractBudgetFitsUnderMaxTime(t *testing.T) {
	// a slice is ~0.65s against production-sized tables (2026-08-11 measurement:
	// 650ms mean for a 50k-row slice of contract_close), so bound the expected
	// run against MaxTime with a wide margin for a slower db
	slices := sweepOrphanContractMaxRowCount / sweepOrphanContractSliceSize
	estimatedRun := time.Duration(slices) * 650 * time.Millisecond

	if sweepOrphanContractMaxTime <= estimatedRun {
		t.Fatalf(
			"a budgeted run is ~%v against production-sized tables, at or over MaxTime %v: "+
				"the run would be CANCELED, Post would not run, and the pass would restart from zero every retry",
			estimatedRun,
			sweepOrphanContractMaxTime,
		)
	}
	// and keep real headroom, not a hairline fit
	if sweepOrphanContractMaxTime < 2*estimatedRun {
		t.Fatalf(
			"budgeted run ~%v leaves under 2x headroom against MaxTime %v",
			estimatedRun,
			sweepOrphanContractMaxTime,
		)
	}
}
