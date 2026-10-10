package work

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/session"
)

func TestTransferAuditRollupDefersBackupWithoutLosingDays(t *testing.T) {
	ctx := context.Background()
	first := time.Date(2026, 9, 27, 14, 0, 0, 0, time.UTC)
	backup := func(context.Context) bool { return true }
	noRollup := func(context.Context, time.Time, time.Time) int {
		t.Fatal("backup deferral opened the rollup write path")
		return 0
	}
	result := rollupTransferAuditEvents(ctx, &RollupTransferAuditEventsArgs{}, first, backup, noRollup)
	wantMin := first.Add(-3 * 24 * time.Hour)
	if !result.Deferred || result.DayCount != 0 || result.MinTime == nil || !result.MinTime.Equal(wantMin) {
		t.Fatalf("first deferral=%+v, want no completed days and lower bound %s", result, wantMin)
	}
	args, runAt := nextTransferAuditRollup(first, result)
	if !runAt.Equal(first.Add(30*time.Minute)) || args.MinTime == nil || !args.MinTime.Equal(wantMin) {
		t.Fatalf("deferred schedule=(%+v,%s)", args, runAt)
	}
	if args.MinTime == result.MinTime {
		t.Fatal("continuation aliases the result's timestamp storage")
	}
	// A pathological multi-day delay must not discard the earlier days simply
	// because they are no longer in the normal three-day refresh window.
	resumed := first.Add(5 * 24 * time.Hour)
	result = rollupTransferAuditEvents(ctx, args, resumed, backup, noRollup)
	if result.MinTime == nil || !result.MinTime.Equal(wantMin) {
		t.Fatalf("repeat deferral lost first lower bound: %+v", result)
	}
	args, _ = nextTransferAuditRollup(resumed, result)
	calls := 0
	for index := range 8 {
		wantDays := 1
		result = rollupTransferAuditEvents(ctx, args, resumed, func(context.Context) bool { return false }, func(gotCtx context.Context, min, max time.Time) int {
			calls++
			if gotCtx != ctx || !min.Equal(wantMin) {
				t.Fatalf("resumed lower bound=%s, want %s", min, wantMin)
			}
			gotDays := int(max.UTC().Truncate(24*time.Hour).Sub(min.UTC().Truncate(24*time.Hour)) / (24 * time.Hour))
			if gotDays != wantDays {
				t.Fatalf("resumed batch days=%d, want %d", gotDays, wantDays)
			}
			wantMin = max
			return gotDays
		})
		if result.Deferred || result.DayCount != wantDays {
			t.Fatalf("resumed result=%+v", result)
		}
		args, runAt = nextTransferAuditRollup(resumed, result)
		if index < 7 && (args.MinTime == nil || !runAt.Equal(resumed)) {
			t.Fatalf("backlog continuation=(%+v,%s)", args, runAt)
		}
	}
	if calls != 8 || args.MinTime != nil || !runAt.Equal(resumed.Add(6*time.Hour)) {
		t.Fatalf("completed rollup did not restore ordinary cadence: (%+v,%s)", args, runAt)
	}
}

func TestTransferAuditRollupQuietPathPreservesNormalWindow(t *testing.T) {
	now := time.Date(2026, 9, 27, 14, 0, 0, 0, time.UTC)
	futureMin := now
	for _, args := range []*RollupTransferAuditEventsArgs{nil, {}, {MinTime: &futureMin}} {
		wantMin := now.Add(-3 * 24 * time.Hour)
		for index := range 3 {
			checks, calls := 0, 0
			wantMax := wantMin.UTC().Truncate(24 * time.Hour).Add(24 * time.Hour)
			if index == 2 {
				wantMax = now
			}
			result := rollupTransferAuditEvents(context.Background(), args, now, func(context.Context) bool { checks++; return false }, func(_ context.Context, min, max time.Time) int {
				calls++
				if !min.Equal(wantMin) || !max.Equal(wantMax) {
					t.Fatalf("ordinary day range changed: [%s,%s)", min, max)
				}
				return 1
			})
			if checks != 1 || calls != 1 || result.Deferred || result.DayCount != 1 {
				t.Fatalf("quiet result=%+v checks=%d calls=%d", result, checks, calls)
			}
			var runAt time.Time
			args, runAt = nextTransferAuditRollup(now, result)
			wantMin = wantMax
			if index < 2 && (args.MinTime == nil || !args.MinTime.Equal(wantMin) || !runAt.Equal(now)) {
				t.Fatal("completed day lost its immediate continuation")
			}
			if index == 2 && (args.MinTime != nil || !runAt.Equal(now.Add(6*time.Hour))) {
				t.Fatal("completed three-day window lost its ordinary refresh cadence")
			}
		}
	}
}

func TestTransferAuditRollupContinuationPersistsBoundedRange(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		clientSession := session.NewLocalClientSession(ctx, "0.0.0.0:0", nil)
		defer clientSession.Cancel()
		minTime := time.Date(2026, 9, 24, 0, 0, 0, 0, time.UTC)
		for _, result := range []*RollupTransferAuditEventsResult{
			{Deferred: true, MinTime: &minTime},
			{DayCount: 1, MinTime: &minTime},
			{DayCount: 1},
		} {
			before := server.NowUtc()
			server.Tx(ctx, func(tx server.PgTx) {
				// The evaluator deletes its completed RunOnce row before Post.
				server.RaisePgResult(tx.Exec(ctx, `DELETE FROM pending_task WHERE run_once_key = '["rollup_transfer_audit_events"]'`))
				server.Raise(RollupTransferAuditEventsPost(&RollupTransferAuditEventsArgs{}, result, clientSession, tx))
				var argsJSON string
				var runAt time.Time
				var maxSeconds int
				server.Raise(tx.QueryRow(ctx, `SELECT args_json, run_at, run_max_time_seconds FROM pending_task WHERE run_once_key = '["rollup_transfer_audit_events"]'`).Scan(&argsJSON, &runAt, &maxSeconds))
				var args RollupTransferAuditEventsArgs
				server.Raise(json.Unmarshal([]byte(argsJSON), &args))
				wantDelay := 6 * time.Hour
				if result.MinTime != nil {
					wantDelay = 0
					if result.Deferred {
						wantDelay = transferAuditBackupRetry
					}
					if args.MinTime == nil || !args.MinTime.Equal(minTime) {
						t.Fatalf("continuation lost lower bound: %+v", args)
					}
				} else if args.MinTime != nil {
					t.Fatalf("completed catch-up retained stale lower bound: %+v", args)
				}
				if delay := runAt.Sub(before); delay < wantDelay || delay > wantDelay+time.Minute {
					t.Fatalf("continuation delay=%s, want about %s", delay, wantDelay)
				}
				if maxSeconds != 3600 {
					t.Fatalf("continuation task budget=%ds, want unchanged 1h", maxSeconds)
				}
			})
		}
	})
}

func TestTransferAuditRollupBackupObservationFailureDoesNotWrite(t *testing.T) {
	want := "synthetic catalog read failure"
	defer func() {
		if got := recover(); got != want {
			t.Fatalf("catalog failure=%v, want original failure", got)
		}
	}()
	rollupTransferAuditEvents(t.Context(), nil, time.Now(), func(context.Context) bool { panic(want) }, func(context.Context, time.Time, time.Time) int {
		t.Fatal("failed catalog observation entered rollup write path")
		return 0
	})
}
