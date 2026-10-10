// Real selection and financial owners keep bounded prefetch separate from
// financial cohort admission, including fairness and interrupted prefixes.
package model

import (
	"context"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
)

// The existing page seam changes only its observer and financial-call wrapper.
// Selection SQL, cursor ownership and individual accounting remain production.
func legacyMetadataPrefetchRun(ctx context.Context, bounded context.Context, payerNetworkId server.Id,
	after *LegacySettlementCursor, limit int, observer *legacySettlementTimingObserver,
	settle func(context.Context, server.Id, *legacySettlementGrantWait) (bool, bool, legacySettlementBusyGate, error),
	cohort ...func(context.Context, []server.Id) ([]legacyFinancialCohortAttempt, error)) (LegacyPayerSettlementResult, error) {
	bounded = context.WithValue(bounded, legacySettlementTimingKey{}, observer)
	result, err := flushLegacyPayerSettlementPages(ctx, bounded, payerNetworkId, after, limit, false,
		func(parent context.Context, page context.Context, shard int, cursor *LegacySettlementCursor, pageLimit int) (LegacySettlementFlushResult, error) {
			return flushLegacySettlementsPage(parent, page, shard, cursor, pageLimit, settle, cohort...)
		})
	result.Timings = observer.snapshot()
	return result, err
}

// A frozen owned cooldown disables transactions in cohorts, but must not turn
// seventeen exact financial outcomes into seventeen independent selections.
func TestLegacyMetadataPrefetchWithoutCohorts(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
		defer cancel()
		f := legacyFinancialCohortSeed(t, ctx, 17, LegacySettlementShardCount)
		cooldown := &legacyFinancialCohortCooldown{now: func() time.Duration { return 0 }}
		cooldown.deferProbe(int(f.payer.sourceNetworkId[15]) % LegacySettlementShardCount)
		ctx = context.WithValue(ctx, legacyFinancialCohortCooldownKey{}, cooldown)
		page, err := FlushLegacyPayerSettlements(ctx, f.payer.sourceNetworkId, nil, len(f.ids))
		if err != nil || page.Completed != 17 || page.Visited != 17 || page.BusyOrGone != 0 || page.Failed != 0 || page.FinancialCohortAttempts != 0 {
			t.Fatal("individual payer owners did not complete the fixed fixture", page, err)
		}
		legacyFinancialCohortRequire(t, ctx, f, legacyFinancialCohortCompleted(f.ids))
		if page.Timings == nil || page.Timings.Selection.Count != 3 {
			t.Fatalf("disabled financial cohorts forced per-contract selection: got %+v, want three bounded selections for seventeen outcomes", page.Timings)
		}
		if replay, err := FlushLegacyPayerSettlements(ctx, f.payer.sourceNetworkId, nil, len(f.ids)); err != nil || replay.Visited != 0 || replay.Completed != 0 {
			t.Fatal("prefetch replay repeated financial work", replay, err)
		}
		legacyFinancialCohortRequire(t, ctx, f, legacyFinancialCohortCompleted(f.ids))
	})
}

// The unchanged-member fallback seam disables further financial cohorts. The
// following selections still contain exactly eight, then eight metadata rows.
func TestLegacyMetadataPrefetchAfterFallbackKeepsEightBound(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
		defer cancel()
		f := legacyFinancialCohortSeed(t, ctx, 17, LegacySettlementShardCount)
		cooldown := &legacyFinancialCohortCooldown{now: func() time.Duration { return 0 }}
		ctx = context.WithValue(ctx, legacyFinancialCohortCooldownKey{}, cooldown)
		bounded, cancelPage := context.WithTimeoutCause(ctx, 15*time.Second, errLegacySettlementPageBudget)
		defer cancelPage()
		observer := &legacySettlementTimingObserver{now: time.Now}
		financialCalls, cohortCalls := 0, 0
		page, err := legacyMetadataPrefetchRun(ctx, bounded, f.payer.sourceNetworkId, nil, len(f.ids), observer,
			func(call context.Context, id server.Id, wait *legacySettlementGrantWait) (bool, bool, legacySettlementBusyGate, error) {
				if financialCalls >= len(f.ids) || id != f.ids[financialCalls] || wait != nil {
					t.Fatal("fallback changed individual order or head policy")
				}
				wantSelections := 1
				if financialCalls > 0 {
					wantSelections = 2 + (financialCalls-1)/8
				}
				if observer.snapshot().Selection.Count != wantSelections {
					t.Fatal("fallback did not retain exact eight-row metadata groups", financialCalls, observer.snapshot())
				}
				financialCalls++
				return flushLegacySettlementWithGrantWait(call, id, wait)
			}, func(call context.Context, ids []server.Id) ([]legacyFinancialCohortAttempt, error) {
				cohortCalls++
				if cohortCalls != 1 || len(ids) != 8 || ids[0] != f.ids[0] || ids[7] != f.ids[7] {
					t.Fatal("fallback exceeded its original bounded financial input")
				}
				return []legacyFinancialCohortAttempt{{contractId: ids[0], fallback: true, deadlineFallback: true}}, nil
			})
		if err != nil || page.Completed != 17 || page.Visited != 17 || page.BusyOrGone != 0 || page.Failed != 0 || financialCalls != 17 || cohortCalls != 1 ||
			page.FinancialCohortAttempts != 1 || page.FinancialCohortSelected != 8 || page.FinancialCohortCompleted != 0 || page.FinancialCohortFallbacks != 1 || page.Timings.Selection.Count != 3 {
			t.Fatal("fallback changed bounded individual progress", page, err)
		}
		if cooldown.ready(int(f.payer.sourceNetworkId[15]) % LegacySettlementShardCount) {
			t.Fatal("controlled deadline fallback did not enter its owned cooldown")
		}
		legacyFinancialCohortRequire(t, ctx, f, legacyFinancialCohortCompleted(f.ids))
	})
}

// A real financial cohort returns only its completed prefix and the first
// fallback. Unreturned prefetched members retain custody until resumed.
func TestLegacyMetadataPrefetchPartialCohortPrefixResumes(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
		defer cancel()
		f := legacyFinancialCohortSeed(t, ctx, 8, LegacySettlementShardCount)
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_escrow SET balance_byte_count=2 WHERE contract_id=$1`, f.ids[3]))
		}, server.TxReadCommitted, server.OptNoRetry())
		f.reservationAmounts[3] = 2
		cooldown := &legacyFinancialCohortCooldown{now: func() time.Duration { return 0 }}
		ctx = context.WithValue(ctx, legacyFinancialCohortCooldownKey{}, cooldown)
		bounded, cancelPage := context.WithTimeoutCause(ctx, 15*time.Second, errLegacySettlementPageBudget)
		defer cancelPage()
		observer := &legacySettlementTimingObserver{now: time.Now}
		financialCalls, cohortCalls := 0, 0
		page, err := legacyMetadataPrefetchRun(ctx, bounded, f.payer.sourceNetworkId, nil, len(f.ids), observer,
			func(call context.Context, id server.Id, wait *legacySettlementGrantWait) (bool, bool, legacySettlementBusyGate, error) {
				financialCalls++
				if financialCalls != 1 || id != f.ids[3] || wait != nil || observer.snapshot().Selection.Count != 1 {
					t.Fatal("partial cohort reselected or replayed its completed prefix")
				}
				return flushLegacySettlementWithGrantWait(call, id, wait)
			}, func(call context.Context, ids []server.Id) ([]legacyFinancialCohortAttempt, error) {
				cohortCalls++
				if cohortCalls != 1 || len(ids) != 8 || ids[0] != f.ids[0] || ids[7] != f.ids[7] {
					t.Fatal("partial cohort changed its bounded selected input")
				}
				attempts, err := flushLegacySettlementCohort(call, ids)
				if err != nil || len(attempts) != 4 {
					t.Fatal("actual financial owner did not return a shorter prefix", len(attempts), err)
				}
				for index, attempt := range attempts {
					if attempt.contractId != f.ids[index] || attempt.completed != (index < 3) || attempt.fallback != (index == 3) || attempt.busy {
						t.Fatal("actual financial prefix changed member custody", index)
					}
				}
				return attempts, nil
			})
		if err != nil || page.Completed != 3 || page.Visited != 4 || page.Failed != 1 || page.BusyOrGone != 0 || page.More ||
			financialCalls != 1 || cohortCalls != 1 || page.Cursor == nil || page.Cursor.ContractId != f.ids[3] ||
			page.FinancialCohortAttempts != 1 || page.FinancialCohortSelected != 8 || page.FinancialCohortCompleted != 3 || page.FinancialCohortFallbacks != 1 || page.Timings.Selection.Count != 1 {
			t.Fatal("short financial prefix changed confirmed progress", page, err)
		}
		legacyFinancialCohortRequire(t, ctx, f, legacyFinancialCohortCompleted(f.ids[:3]))
		var deferred bool
		server.Db(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(ctx, `SELECT failure_code='accounting' AND next_attempt_time>statement_timestamp() AT TIME ZONE 'UTC'
            FROM legacy_settlement_intent WHERE contract_id=$1`, f.ids[3]).Scan(&deferred))
		})
		if !deferred {
			t.Fatal("partial financial fallback lost durable retry custody")
		}
		cooldown.deferProbe(int(f.payer.sourceNetworkId[15]) % LegacySettlementShardCount)
		resumed, err := FlushLegacyPayerSettlements(ctx, f.payer.sourceNetworkId, page.Cursor, len(f.ids))
		if err != nil || resumed.Completed != 4 || resumed.Visited != 4 || resumed.Failed != 0 || resumed.BusyOrGone != 0 || resumed.FinancialCohortAttempts != 0 {
			t.Fatal("unreturned prefetched suffix was skipped or replayed", resumed, err)
		}
		expected := legacyFinancialCohortCompleted(f.ids)
		delete(expected, f.ids[3])
		legacyFinancialCohortRequire(t, ctx, f, expected)
	})
}

// Head visits remain singletons with one early head and three forward visits
// between later heads. Prefetch must not consume or skip the reserved head slot.
func TestLegacyMetadataPrefetchPreservesHeadFairness(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
		defer cancel()
		f := legacyFinancialCohortSeed(t, ctx, 24, LegacySettlementShardCount)
		after := &LegacySettlementCursor{NextAttemptTime: time.Date(2010, 1, 1, 0, 0, 0, 0, time.UTC),
			ContractId: f.ids[7], PassEndTime: server.NowUtc()}
		bounded, cancelPage := context.WithTimeoutCause(ctx, 15*time.Second, errLegacySettlementPageBudget)
		defer cancelPage()
		observer := &legacySettlementTimingObserver{now: time.Now}
		order := []int{8, 0, 9, 10, 11, 1, 12, 13, 14, 2, 15, 16, 17, 3, 18, 19}
		selectionCounts := []int{1, 2, 3, 3, 3, 4, 5, 5, 5, 6, 7, 7, 7, 8, 9, 9}
		financialCalls := 0
		expected := map[server.Id]bool{}
		page, err := legacyMetadataPrefetchRun(ctx, bounded, f.payer.sourceNetworkId, after, len(order), observer,
			func(call context.Context, id server.Id, wait *legacySettlementGrantWait) (bool, bool, legacySettlementBusyGate, error) {
				if financialCalls >= len(order) || id != f.ids[order[financialCalls]] || observer.snapshot().Selection.Count != selectionCounts[financialCalls] {
					t.Fatal("metadata prefetch crossed a head boundary", financialCalls, observer.snapshot())
				}
				head, _ := call.Value(legacySettlementAdmissionHeadKey{}).(bool)
				if head != (order[financialCalls] < 8) || (wait != nil) != (financialCalls == 1) {
					t.Fatal("prefetch changed head admission or grant-wait allocation")
				}
				expected[id] = true
				financialCalls++
				return flushLegacySettlementWithGrantWait(call, id, wait)
			})
		if err != nil || financialCalls != 16 || page.Completed != 16 || page.Visited != 16 || page.HeadVisited != 4 || page.HeadCompleted != 4 ||
			page.BusyOrGone != 0 || page.Failed != 0 || page.FinancialCohortAttempts != 0 || page.Timings.Selection.Count != 9 || page.Cursor == nil || page.Cursor.ContractId != f.ids[19] {
			t.Fatal("prefetch changed the forward/head committed prefix", page, err)
		}
		legacyFinancialCohortRequire(t, ctx, f, expected)
	})
}

// Cancellation occurs after three real commits with another five rows already
// selected. Only the committed prefix may become the persisted resume cursor.
func TestLegacyMetadataPrefetchInterruptedPrefixResumes(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
		defer cancel()
		f := legacyFinancialCohortSeed(t, ctx, 12, LegacySettlementShardCount)
		turn, cancelTurn := context.WithTimeoutCause(ctx, 15*time.Second, errLegacySettlementPageBudget)
		defer cancelTurn()
		bounded, cancelPage := context.WithCancelCause(turn)
		defer cancelPage(nil)
		observer := &legacySettlementTimingObserver{now: time.Now}
		financialCalls := 0
		page, err := legacyMetadataPrefetchRun(ctx, bounded, f.payer.sourceNetworkId, nil, len(f.ids), observer,
			func(call context.Context, id server.Id, wait *legacySettlementGrantWait) (bool, bool, legacySettlementBusyGate, error) {
				if financialCalls >= 4 || id != f.ids[financialCalls] || observer.snapshot().Selection.Count != 1 {
					t.Fatal("interrupted prefix was not selected in one bounded group")
				}
				financialCalls++
				if financialCalls == 4 {
					cancelPage(errLegacySettlementPageBudget)
					return false, false, legacySettlementBusyNone, context.Canceled
				}
				return flushLegacySettlementWithGrantWait(call, id, wait)
			})
		if err != nil || financialCalls != 4 || page.Completed != 3 || page.Visited != 3 || page.Failed != 0 || page.BusyOrGone != 0 || !page.More ||
			page.Cursor == nil || page.Cursor.ContractId != f.ids[2] || page.Timings.Selection.Count != 1 {
			t.Fatal("interrupted prefetch advanced beyond confirmed custody", page, err)
		}
		legacyFinancialCohortRequire(t, ctx, f, legacyFinancialCohortCompleted(f.ids[:3]))
		cooldown := &legacyFinancialCohortCooldown{now: func() time.Duration { return 0 }}
		cooldown.deferProbe(int(f.payer.sourceNetworkId[15]) % LegacySettlementShardCount)
		resume := context.WithValue(ctx, legacyFinancialCohortCooldownKey{}, cooldown)
		resumed, err := FlushLegacyPayerSettlements(resume, f.payer.sourceNetworkId, page.Cursor, len(f.ids))
		if err != nil || resumed.Completed != 9 || resumed.Visited != 9 || resumed.BusyOrGone != 0 || resumed.Failed != 0 || resumed.FinancialCohortAttempts != 0 {
			t.Fatal("prefetched tail was skipped or replayed after interruption", resumed, err)
		}
		legacyFinancialCohortRequire(t, ctx, f, legacyFinancialCohortCompleted(f.ids))
		if replay, err := FlushLegacyPayerSettlements(resume, f.payer.sourceNetworkId, nil, len(f.ids)); err != nil || replay.Visited != 0 || replay.Completed != 0 {
			t.Fatal("interrupted prefix replay repeated accounting", replay, err)
		}
	})
}

// A separate transaction owns one prefetched intent. Later members can commit;
// the busy member retains custody and is recovered by the ordinary EOF rewind.
func TestLegacyMetadataPrefetchBusyMemberRetainsCustody(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
		defer cancel()
		f := legacyFinancialCohortSeed(t, ctx, 12, LegacySettlementShardCount)
		cooldown := &legacyFinancialCohortCooldown{now: func() time.Duration { return 0 }}
		cooldown.deferProbe(int(f.payer.sourceNetworkId[15]) % LegacySettlementShardCount)
		ctx = context.WithValue(ctx, legacyFinancialCohortCooldownKey{}, cooldown)
		ready := make(chan error, 1)
		release := make(chan struct{})
		joined := make(chan error, 1)
		go func() {
			entered := false
			var returnErr error
			server.HandleError(func() {
				server.Tx(ctx, func(tx server.PgTx) {
					server.RaisePgResult(tx.Exec(ctx, `SELECT contract_id FROM legacy_settlement_intent WHERE contract_id=$1 FOR UPDATE`, f.ids[3]))
					entered = true
					ready <- nil
					select {
					case <-release:
					case <-ctx.Done():
					}
				}, server.TxReadCommitted, server.OptNoRetry())
			}, func(err error) { returnErr = err })
			if !entered {
				ready <- returnErr
			}
			joined <- returnErr
		}()
		released := false
		finishOwner := func() {
			if !released {
				released = true
				close(release)
				if err := <-joined; err != nil {
					t.Fatal("independent intent owner did not join", err)
				}
			}
		}
		defer finishOwner()
		if err := <-ready; err != nil {
			t.Fatal("independent intent owner could not enter", err)
		}
		page, err := FlushLegacyPayerSettlements(ctx, f.payer.sourceNetworkId, nil, len(f.ids))
		if err != nil || page.Visited != 12 || page.Completed != 11 || page.BusyOrGone != 1 || page.BusyIntentUnavailable != 1 || page.Failed != 0 ||
			page.FinancialCohortAttempts != 0 || page.Timings == nil || page.Timings.Selection.Count != 2 {
			t.Fatal("busy prefetched member changed healthy progress", page, err)
		}
		expected := legacyFinancialCohortCompleted(f.ids)
		delete(expected, f.ids[3])
		legacyFinancialCohortRequire(t, ctx, f, expected)
		finishOwner()
		resumed, err := FlushLegacyPayerSettlements(ctx, f.payer.sourceNetworkId, page.Cursor, len(f.ids))
		if err != nil || resumed.Failed != 0 || resumed.BusyOrGone != 0 {
			t.Fatal("released prefetch member could not resume", resumed, err)
		}
		if resumed.Visited == 0 && resumed.Cursor == nil {
			resumed, err = FlushLegacyPayerSettlements(ctx, f.payer.sourceNetworkId, nil, len(f.ids))
		}
		if err != nil || resumed.Visited != 1 || resumed.Completed != 1 || resumed.Failed != 0 || resumed.BusyOrGone != 0 {
			t.Fatal("busy prefetched member was skipped or financial work replayed", resumed, err)
		}
		legacyFinancialCohortRequire(t, ctx, f, legacyFinancialCohortCompleted(f.ids))
	})
}
