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

func TestTransferAuditRollupDefersBackupWithoutLosingDays(t *testing.T) {
	ctx := context.Background()
	first := time.Date(2026, 9, 27, 14, 0, 0, 0, time.UTC)
	backup := func(context.Context) bool { return true }
	noAdvance := func(context.Context, *model.TransferCloseByteSum, int) bool {
		t.Fatal("backup deferral opened the rollup read path")
		return false
	}
	result := rollupTransferAuditEvents(ctx, &RollupTransferAuditEventsArgs{}, first, backup, noAdvance)
	wantMin := first.Add(-3 * 24 * time.Hour)
	if !result.Deferred || result.DayCount != 0 || result.MinTime == nil || !result.MinTime.Equal(wantMin) || result.Day != nil {
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
	result = rollupTransferAuditEvents(ctx, args, resumed, backup, noAdvance)
	if result.MinTime == nil || !result.MinTime.Equal(wantMin) {
		t.Fatalf("repeat deferral lost first lower bound: %+v", result)
	}
	args, _ = nextTransferAuditRollup(resumed, result)
	calls := 0
	wantDay := wantMin.UTC().Truncate(24 * time.Hour)
	for index := range 8 {
		result = rollupTransferAuditEvents(ctx, args, resumed, func(context.Context) bool { return false }, func(gotCtx context.Context, day *model.TransferCloseByteSum, chunkCount int) bool {
			calls++
			if gotCtx != ctx || !day.Start.Equal(wantDay) || !day.End.Equal(wantDay.Add(24*time.Hour)) || day.TerminalOnly {
				t.Fatalf("resumed day=[%s,%s) terminal_only=%t, want the day of %s", day.Start, day.End, day.TerminalOnly, wantDay)
			}
			if chunkCount != transferAuditRollupChunkCount {
				t.Fatalf("run chunk budget=%d, want %d", chunkCount, transferAuditRollupChunkCount)
			}
			return true
		})
		if result.Deferred || result.DayCount != 1 || result.Day != nil {
			t.Fatalf("resumed result=%+v", result)
		}
		wantDay = wantDay.Add(24 * time.Hour)
		args, runAt = nextTransferAuditRollup(resumed, result)
		if index < 7 && (args.MinTime == nil || !args.MinTime.Equal(wantDay) || !runAt.Equal(resumed)) {
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
		wantDay := now.Add(-3 * 24 * time.Hour).Truncate(24 * time.Hour)
		for index := range 3 {
			checks, calls := 0, 0
			result := rollupTransferAuditEvents(context.Background(), args, now, func(context.Context) bool { checks++; return false }, func(_ context.Context, day *model.TransferCloseByteSum, _ int) bool {
				calls++
				if !day.Start.Equal(wantDay) || !day.End.Equal(wantDay.Add(24*time.Hour)) {
					t.Fatalf("ordinary day changed: [%s,%s)", day.Start, day.End)
				}
				return true
			})
			if checks != 1 || calls != 1 || result.Deferred || result.DayCount != 1 {
				t.Fatalf("quiet result=%+v checks=%d calls=%d", result, checks, calls)
			}
			var runAt time.Time
			args, runAt = nextTransferAuditRollup(now, result)
			wantDay = wantDay.Add(24 * time.Hour)
			if index < 2 && (args.MinTime == nil || !args.MinTime.Equal(wantDay) || !runAt.Equal(now)) {
				t.Fatal("completed day lost its immediate continuation")
			}
			if index == 2 && (args.MinTime != nil || !runAt.Equal(now.Add(6*time.Hour))) {
				t.Fatal("completed three-day window lost its ordinary refresh cadence")
			}
		}
	}
}

// A day that one run does not finish must hand its checkpoint to an immediate
// continuation, through a backup deferral too, and a checkpoint for another
// window must never be resumed.
func TestTransferAuditRollupCarriesPartialDayCheckpoint(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 9, 27, 14, 0, 0, 0, time.UTC)
	dayStart := time.Date(2026, 9, 24, 0, 0, 0, 0, time.UTC)
	// each run reads one more synthetic chunk; the third completes the day
	advance := func(_ context.Context, day *model.TransferCloseByteSum, _ int) bool {
		day.ContractCount += 1
		day.ByteCount += 10
		day.AfterCloseTime = day.Start.Add(time.Duration(day.ContractCount) * time.Hour)
		day.Complete = day.ContractCount == 3
		return day.Complete
	}
	idle := func(context.Context) bool { return false }

	result := rollupTransferAuditEvents(ctx, nil, now, idle, advance)
	if result.DayCount != 0 || result.MinTime == nil || !result.MinTime.Equal(dayStart) ||
		result.Day == nil || result.Day.ContractCount != 1 || !model.IsTransferAuditRollupDay(result.Day, dayStart) {
		t.Fatalf("unfinished day did not checkpoint: %+v", result)
	}
	args, runAt := nextTransferAuditRollup(now, result)
	if !runAt.Equal(now) || args.Day == nil || args.Day == result.Day || args.Day.ContractCount != 1 {
		t.Fatalf("checkpoint continuation=(%+v,%s)", args, runAt)
	}

	// a backup deferral keeps the checkpoint for the retry
	result = rollupTransferAuditEvents(ctx, args, now, func(context.Context) bool { return true }, func(context.Context, *model.TransferCloseByteSum, int) bool {
		t.Fatal("backup deferral advanced the day")
		return false
	})
	args, runAt = nextTransferAuditRollup(now, result)
	if !result.Deferred || !runAt.Equal(now.Add(transferAuditBackupRetry)) || args.Day == nil || args.Day.ContractCount != 1 {
		t.Fatalf("deferral lost the checkpoint: (%+v,%s)", args, runAt)
	}

	// the checkpoint survives the args json round trip and resumes in place
	argsJson, err := json.Marshal(args)
	server.Raise(err)
	var stored RollupTransferAuditEventsArgs
	server.Raise(json.Unmarshal(argsJson, &stored))
	result = rollupTransferAuditEvents(ctx, &stored, now, idle, advance)
	if result.Day == nil || result.Day.ContractCount != 2 || !result.Day.AfterCloseTime.Equal(dayStart.Add(2*time.Hour)) {
		t.Fatalf("stored checkpoint did not resume in place: %+v", result)
	}
	args, _ = nextTransferAuditRollup(now, result)
	result = rollupTransferAuditEvents(ctx, args, now, idle, advance)
	if result.DayCount != 1 || result.Day != nil || result.MinTime == nil || !result.MinTime.Equal(dayStart.Add(24*time.Hour)) {
		t.Fatalf("completed day did not move to the next day: %+v", result)
	}

	// a checkpoint of another window starts the day over
	foreign := model.NewTransferAuditRollupDay(dayStart.Add(24 * time.Hour))
	foreign.ContractCount = 7
	terminal := model.NewTransferAuditRollupDay(dayStart)
	terminal.TerminalOnly = true
	terminal.ContractCount = 7
	for _, checkpoint := range []*model.TransferCloseByteSum{foreign, terminal} {
		minTime := dayStart
		result = rollupTransferAuditEvents(ctx, &RollupTransferAuditEventsArgs{MinTime: &minTime, Day: checkpoint}, now, idle, advance)
		if result.Day == nil || result.Day.ContractCount != 1 || !model.IsTransferAuditRollupDay(result.Day, dayStart) {
			t.Fatalf("resumed a checkpoint of another window: %+v", result.Day)
		}
	}
}

func TestTransferAuditRollupContinuationPersistsBoundedRange(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		clientSession := session.NewLocalClientSession(ctx, "0.0.0.0:0", nil)
		defer clientSession.Cancel()
		minTime := time.Date(2026, 9, 24, 0, 0, 0, 0, time.UTC)
		checkpoint := model.NewTransferAuditRollupDay(minTime)
		checkpoint.AfterCloseTime = minTime.Add(5*time.Hour + 7*time.Microsecond)
		checkpoint.AfterContractId = server.NewId()
		checkpoint.ContractCount = 2000
		checkpoint.ByteCount = 123456789
		for _, result := range []*RollupTransferAuditEventsResult{
			{Deferred: true, MinTime: &minTime},
			{DayCount: 1, MinTime: &minTime},
			{DayCount: 1},
			{MinTime: &minTime, Day: checkpoint},
			{Deferred: true, MinTime: &minTime, Day: checkpoint},
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
				gotDayJson, err := json.Marshal(args.Day)
				server.Raise(err)
				wantDayJson, err := json.Marshal(result.Day)
				server.Raise(err)
				if string(gotDayJson) != string(wantDayJson) {
					t.Fatalf("continuation changed the partial day: %s, want %s", gotDayJson, wantDayJson)
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
	rollupTransferAuditEvents(t.Context(), nil, time.Now(), func(context.Context) bool { panic(want) }, func(context.Context, *model.TransferCloseByteSum, int) bool {
		t.Fatal("failed catalog observation entered rollup read path")
		return false
	})
}
