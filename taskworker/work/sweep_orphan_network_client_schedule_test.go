// The network-client orphan sweep pass is spread over budgeted runs, chained by
// Post through the run-once task's cursor.
package work

import (
	"context"
	"encoding/json"
	"slices"
	"testing"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
)

// Returns the scheduled args and run_at of the single
// sweep_orphan_network_client_data row, failing if it is missing.
func readSweepOrphanNetworkClientPending(
	ctx context.Context,
	t testing.TB,
	tx server.PgTx,
) (args SweepOrphanNetworkClientDataArgs, runAt time.Time) {
	found := false
	result, err := tx.Query(
		ctx,
		`
		SELECT args_json, run_at
		FROM pending_task
		WHERE run_once_key = '["sweep_orphan_network_client_data"]'
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
		t.Fatal("no sweep_orphan_network_client_data row scheduled")
	}
	return args, runAt
}

// Before, Post always scheduled the next weekly pass from row zero, and a run
// that could not finish was canceled at its deadline before Post ran. A run that
// stops at its row budget must hand its exact position to a prompt resume run.
func TestSweepOrphanNetworkClientDataResumesMidPass(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		clientSession := session.NewLocalClientSession(ctx, "0.0.0.0:0", nil)
		defer clientSession.Cancel()

		cursor := model.SweepOrphanCursor{
			Step: 6,
			Key:  []string{server.NewId().String(), "4"},
		}
		before := server.NowUtc()
		server.Tx(ctx, func(tx server.PgTx) {
			err := SweepOrphanNetworkClientDataPost(
				&SweepOrphanNetworkClientDataArgs{},
				&SweepOrphanNetworkClientDataResult{Cursor: cursor, Done: false},
				clientSession,
				tx,
			)
			if err != nil {
				t.Fatalf("Post returned %v", err)
			}
			args, runAt := readSweepOrphanNetworkClientPending(ctx, t, tx)
			if args.Cursor.Step != cursor.Step || !slices.Equal(args.Cursor.Key, cursor.Key) {
				t.Fatalf("resume cursor = %+v, want %+v", args.Cursor, cursor)
			}
			delay := runAt.Sub(before)
			if delay < sweepOrphanNetworkClientResumeTimeout-time.Minute ||
				sweepOrphanNetworkClientResumeTimeout+time.Minute < delay {
				t.Fatalf("mid-pass resume scheduled %v out, want ~%v", delay, sweepOrphanNetworkClientResumeTimeout)
			}
		})
	})
}

// A completed pass restarts at the zero cursor on the weekly cadence, so the
// sweep does not run back to back forever.
func TestSweepOrphanNetworkClientDataRestartsPassWhenDone(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		clientSession := session.NewLocalClientSession(ctx, "0.0.0.0:0", nil)
		defer clientSession.Cancel()

		before := server.NowUtc()
		server.Tx(ctx, func(tx server.PgTx) {
			err := SweepOrphanNetworkClientDataPost(
				&SweepOrphanNetworkClientDataArgs{},
				&SweepOrphanNetworkClientDataResult{
					// a done run still reports the cursor it ended on; Post must
					// ignore it and start the next pass at the head
					Cursor: model.SweepOrphanCursor{Step: 8, Key: []string{server.NewId().String()}},
					Done:   true,
				},
				clientSession,
				tx,
			)
			if err != nil {
				t.Fatalf("Post returned %v", err)
			}
			args, runAt := readSweepOrphanNetworkClientPending(ctx, t, tx)
			if args.Cursor.Step != 0 || 0 < len(args.Cursor.Key) {
				t.Fatalf("completed pass reschedules with cursor %+v, want the zero cursor", args.Cursor)
			}
			if delay := runAt.Sub(before); delay < 24*time.Hour {
				t.Fatalf("completed pass reschedules %v out, want the weekly cadence", delay)
			}
		})
	})
}

// The row budget keeps a run finishing normally, which is what lets Post run.
// If it grows toward MaxTime the run is canceled again and the pass restarts
// from zero on every retry, silently.
func TestSweepOrphanNetworkClientBudgetFitsUnderMaxTime(t *testing.T) {
	// The slowest network-client slice measured on Main, 2026-10-11, was a
	// proxy_device_config page at 8.1 s mean; provide_key pages were 1.2 s.
	sliceCount := sweepOrphanNetworkClientMaxRowCount / sweepOrphanNetworkClientSliceSize
	estimatedRun := time.Duration(sliceCount) * 8100 * time.Millisecond
	if sweepOrphanNetworkClientMaxTime < 2*estimatedRun {
		t.Fatalf(
			"a budgeted run is ~%v at the slowest measured slice, without 2x headroom under MaxTime %v: "+
				"the run would be canceled, Post would not run, and the pass would restart from zero every retry",
			estimatedRun,
			sweepOrphanNetworkClientMaxTime,
		)
	}
}
