// The recurring network-client orphan sweep spreads one pass over budgeted
// runs, so a pass larger than one run still completes instead of restarting.
package model

import (
	"context"
	"reflect"
	"slices"
	"sync/atomic"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/urnetwork/server"
)

// Counts provide_key pages sent by the real sweep.
type orphanProvidePageCounter struct {
	count atomic.Int64
}

func (self *orphanProvidePageCounter) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	if data.SQL == orphanProvideKeyFirstPageSql || data.SQL == orphanProvideKeyResumedPageSql {
		self.count.Add(1)
	}
	return ctx
}

func (self *orphanProvidePageCounter) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {
}

// Before, one run paged every table to its end. provide_key holds about 1.1B
// keys on Main, so a run outlived its 4-hour deadline, was canceled before its
// Post, and every retry restarted the pass at the first key. A run must stop at
// its row budget and the next run must resume after the last examined key.
func TestSweepOrphanNetworkClientDataResumesWithinRowBudget(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		// keys 0, 2 and 4 have parents; 1 and 3 are orphans
		for n := range 5 {
			if n%2 == 0 {
				statsInsertNetworkClient(ctx, server.NewId(), orphanSweepTestId(n))
			}
			statsInsertProvideKey(ctx, orphanSweepTestId(n), 1)
		}
		provideStep := slices.IndexFunc(sweepOrphanNetworkClientSteps(), func(step sweepOrphanStep) bool {
			return step.table == "provide_key"
		})

		counter := &orphanProvidePageCounter{}
		scope, err := server.NewTestPgQueryScope(ctx, counter)
		if err != nil {
			t.Fatal(err)
		}
		defer func() {
			if err := scope.Close(); err != nil {
				t.Error(err)
			}
		}()
		const sliceSize = 1
		const maxRowCount = 2
		cursor := SweepOrphanCursor{}
		var removed int64
		runs := 0
		for {
			before := counter.count.Load()
			runRemoved, end, done := SweepOrphanNetworkClientDataFrom(ctx, cursor, maxRowCount, sliceSize)
			removed += runRemoved
			runs++
			// the run's keys, plus the empty successor page that ends the table
			if pages := counter.count.Load() - before; maxRowCount/sliceSize < pages {
				t.Fatalf("run %d sent %d provide_key pages over its budget of %d keys", runs, pages, maxRowCount)
			}
			if done {
				break
			}
			if runs == 3 {
				t.Fatal("the pass did not complete within its budgeted runs")
			}
			last := orphanSweepTestId(maxRowCount*runs - 1).String()
			if end.Step != provideStep || len(end.Key) != 2 || end.Key[0] != last || end.Key[1] != "1" {
				t.Fatalf("run %d stopped at %+v, want provide_key step %d after key %s", runs, end, provideStep, last)
			}
			cursor = end
		}
		// one page per key and one empty successor: no run revisited a key
		if runs != 3 || removed != 2 || counter.count.Load() != 6 {
			t.Fatalf("pass took %d runs, removed %d keys and sent %d provide_key pages, want 3/2/6", runs, removed, counter.count.Load())
		}
		server.Db(ctx, func(conn server.PgConn) {
			for n := range 5 {
				var count int
				server.Raise(conn.QueryRow(ctx, `SELECT count(*) FROM provide_key WHERE client_id=$1`, orphanSweepTestId(n)).Scan(&count))
				if (count == 1) != (n%2 == 0) {
					t.Fatalf("key %d retained=%t, want retained only with a parent", n, count == 1)
				}
			}
		})
	})
}

// A resumed run decodes its key with the step's own targets. A key column the
// codec cannot render would panic mid-pass and send the run back to retry.
func TestSweepOrphanNetworkClientCursorKeysRoundTrip(t *testing.T) {
	for _, step := range sweepOrphanNetworkClientSteps() {
		targets := step.newCursorTargets()
		values := make([]any, len(targets))
		for i, target := range targets {
			switch target.(type) {
			case *server.Id:
				values[i] = server.NewId()
			case *string:
				values[i] = "synthetic"
			case *int:
				values[i] = -3
			case *int64:
				values[i] = int64(1) << 40
			default:
				t.Fatalf("%s key column %d has no test value for %T", step.table, i, target)
			}
		}
		decoded, ok := decodeSweepCursorKey(encodeSweepCursorKey(values), step.newCursorTargets())
		if !ok || !reflect.DeepEqual(decoded, values) {
			t.Fatalf("%s cursor %v decoded as %v (ok=%t)", step.table, values, decoded, ok)
		}
	}
}
