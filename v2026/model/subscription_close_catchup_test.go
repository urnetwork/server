// Explicit raw-page transitions reproduce the gap without wall-clock timing.
package model

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"
	"time"
)

// Each original pass retains a million-row prefix. A quiet row between the
// recent upper and fresh lower must receive a visit before any prefix completes.
// This fails on the original three-lane coordinator: the row receives no visit.
func TestExpiryCatchupVisitsGapBeforeRetainedPassesComplete(t *testing.T) {
	epoch := time.Date(2026, 1, 3, 0, 0, 0, 0, time.UTC)
	recentUpper, cutoff := epoch.Add(48*time.Hour), epoch.Add(72*time.Hour)
	gap := ContractExpiryPosition{CreateTime: epoch.Add(60 * time.Hour), ContractId: expiryFairTestId(1)}
	cursor := &ContractExpirySweepCursor{
		Historical: &ContractExpiryCursor{ScanBefore: epoch}, RecentAfter: epoch,
		Recent: &ContractExpiryCursor{ScanBefore: recentUpper},
		Fresh:  &ContractExpiryCursor{ScanBefore: cutoff},
	}
	original, _ := json.Marshal(cursor)
	visits := [3]int{}
	gapVisits := 0
	for range 12 {
		before := cursor
		beforeJson, _ := json.Marshal(before)
		_, next, err := forceCloseContractExpiryFreshPage(cutoff, cutoff.Add(12*time.Minute), cursor,
			func(position *ContractExpiryCursor) (int64, *ContractExpiryCursor, error) {
				for lane, upper := range []time.Time{epoch, recentUpper, cutoff} {
					if position.ScanBefore.Equal(upper) {
						visits[lane] += 256
						advanced := *position
						advanced.Open = &ContractExpiryPosition{CreateTime: upper.Add(-time.Minute + time.Duration(visits[lane])*time.Microsecond), ContractId: expiryFairTestId(visits[lane])}
						return 0, &advanced, nil
					}
				}
				if position.Open != nil && position.Dispute != nil && position.Open.CreateTime.Before(gap.CreateTime) &&
					!position.ScanBefore.Before(gap.CreateTime) {
					gapVisits++
				}
				return 1, nil, nil
			})
		if err != nil || next == nil {
			t.Fatal("a finite gap page discarded an unfinished retained pass", err)
		}
		afterJson, _ := json.Marshal(before)
		if !bytes.Equal(beforeJson, afterJson) {
			t.Fatal("page selection mutated the durable input checkpoint")
		}
		raw, _ := json.Marshal(next)
		var stored *ContractExpirySweepCursor
		if json.Unmarshal(raw, &stored) != nil {
			t.Fatal("gap ownership did not survive the task JSON boundary")
		}
		cursor = stored
	}
	if gapVisits != 1 {
		t.Fatalf("quiet gap row received %d visits before retained passes completed; want 1", gapVisits)
	}
	for _, visited := range visits {
		if visited == 0 || visited >= 1_000_000 {
			t.Fatal("catch-up service starved or replaced a retained pass")
		}
	}
	if !cursor.Historical.ScanBefore.Equal(epoch) || !cursor.Recent.ScanBefore.Equal(recentUpper) ||
		!cursor.Fresh.ScanBefore.Equal(cutoff) || !cursor.RecentAfter.Equal(epoch) {
		t.Fatal("gap admission changed original pass bounds")
	}
	t.Logf("retained_raw=%v gap_visits=%d original_checkpoint_bytes=%d", visits, gapVisits, len(original))
}

// Recent can finish before the first catch-up turn. Its replacement starts at
// the historical epoch and must not steal the gap or its later quiet recheck.
func TestExpiryCatchupRecentResetKeepsOriginalGap(t *testing.T) {
	epoch := time.Date(2026, 1, 3, 0, 0, 0, 0, time.UTC)
	recentUpper, cutoff := epoch.Add(48*time.Hour), epoch.Add(72*time.Hour)
	created := epoch.Add(60 * time.Hour)
	cursor := &ContractExpirySweepCursor{Historical: &ContractExpiryCursor{ScanBefore: epoch}, RecentAfter: epoch,
		Recent: &ContractExpiryCursor{ScanBefore: recentUpper},
		Fresh:  &ContractExpiryCursor{ScanBefore: cutoff, Open: &ContractExpiryPosition{CreateTime: cutoff.Add(-time.Hour)}}}
	gapVisits, quietCloses, historicalVisits, replacementVisits := 0, 0, 0, 0
	for turn := range 18 {
		bound := cutoff
		if turn >= 12 {
			bound = cutoff.Add(time.Minute)
		}
		_, next, err := forceCloseContractExpiryFreshPage(bound, bound.Add(12*time.Minute), cursor,
			func(position *ContractExpiryCursor) (int64, *ContractExpiryCursor, error) {
				if position.ScanBefore.Equal(recentUpper) {
					return 0, nil, nil
				}
				if position.ScanBefore.Equal(epoch) {
					historicalVisits++
					return 0, position, nil
				}
				if position.ScanBefore.Equal(cutoff) {
					if position.Open.CreateTime.Equal(epoch) {
						replacementVisits++
					}
					return 0, position, nil
				}
				if !position.Open.CreateTime.Before(created) || position.ScanBefore.Before(created) {
					t.Fatal("recent reset discarded the original gap bounds")
				}
				gapVisits++
				if bound.After(cutoff) {
					quietCloses++
					return 1, nil, nil
				}
				return 0, nil, nil
			})
		if err != nil || next == nil {
			t.Fatal("recent reset discarded the unfinished historical pass", err)
		}
		raw, _ := json.Marshal(next)
		var stored *ContractExpirySweepCursor
		if json.Unmarshal(raw, &stored) != nil {
			t.Fatal("original gap bounds did not survive checkpoint serialization")
		}
		cursor = stored
		if turn == 11 && gapVisits != 1 {
			t.Fatal("finishing recent before catch-up postponed the first gap visit")
		}
	}
	if gapVisits != 2 || quietCloses != 1 || historicalVisits == 0 || replacementVisits == 0 ||
		!cursor.CatchupAfter.Equal(recentUpper) || !cursor.Recent.ScanBefore.Equal(cutoff) {
		t.Fatal("recent reset abandoned the quiet gap recheck or another retained pass")
	}
}

// Completing an initial visit must not age out a row whose report becomes quiet.
// A newer fresh pass expands the gap, but never extends an unfinished gap pass.
func TestExpiryCatchupRevisitsQuietAndExpandingGap(t *testing.T) {
	epoch := time.Date(2026, 1, 3, 0, 0, 0, 0, time.UTC)
	recentUpper, cutoff := epoch.Add(48*time.Hour), epoch.Add(72*time.Hour)
	cursor := &ContractExpirySweepCursor{Historical: &ContractExpiryCursor{ScanBefore: epoch}, RecentAfter: epoch,
		Recent: &ContractExpiryCursor{ScanBefore: recentUpper}, Fresh: &ContractExpiryCursor{ScanBefore: cutoff}, CatchupTurn: 2}
	quietAt := cutoff.Add(time.Minute)
	quietCreated := recentUpper.Add(time.Hour)
	expandedCreated := cutoff.Add(time.Hour)
	quietVisits, expandedVisits := 0, 0
	step := func(bound time.Time, retain bool) {
		t.Helper()
		cursor.CatchupTurn, cursor.FreshNext = 2, false
		_, next, err := forceCloseContractExpiryFreshPage(bound, bound.Add(12*time.Minute), cursor,
			func(position *ContractExpiryCursor) (int64, *ContractExpiryCursor, error) {
				if !position.Open.CreateTime.Equal(recentUpper) || !position.Dispute.CreateTime.Equal(recentUpper) {
					t.Fatal("recheck slid past the original skipped gap row")
				}
				if !bound.Before(quietAt) && !position.ScanBefore.Before(quietCreated) {
					quietVisits++
				}
				if !position.ScanBefore.Before(expandedCreated) {
					expandedVisits++
				}
				if retain {
					return 0, position, nil
				}
				return 0, nil, nil
			})
		if err != nil || next == nil {
			t.Fatal("gap recheck discarded retained work", err)
		}
		cursor = next
	}
	step(cutoff, false)
	if quietVisits != 0 || expandedVisits != 0 {
		t.Fatal("initial selection admitted a recent report or a future gap")
	}
	step(quietAt, true)
	if quietVisits != 1 || !cursor.Catchup.ScanBefore.Equal(cutoff.Add(-time.Hour)) {
		t.Fatal("logical quiet transition failed to reopen the original gap")
	}
	cursor.Fresh = &ContractExpiryCursor{ScanBefore: cutoff.Add(3 * time.Hour)}
	step(cutoff.Add(3*time.Hour), false)
	if expandedVisits != 0 {
		t.Fatal("a newer fresh bound extended an unfinished catch-up pass")
	}
	if !cursor.CatchupChecked.Equal(quietAt) {
		t.Fatal("completion replaced the earliest quiet check with a later page's cutoff")
	}
	step(cutoff.Add(3*time.Hour), false)
	if expandedVisits != 1 {
		t.Fatal("next catch-up pass did not own the newly expanded gap")
	}
}

// Time bounds partition all UUID ties: recent owns its upper timestamp and
// catch-up owns the timestamp excluded by the fresh lower position.
func TestExpiryCatchupBoundaryTies(t *testing.T) {
	epoch := time.Date(2026, 1, 3, 0, 0, 0, 0, time.UTC)
	upper := epoch.Add(3 * time.Hour)
	before := &ContractExpirySweepCursor{Recent: &ContractExpiryCursor{ScanBefore: epoch},
		Fresh: &ContractExpiryCursor{ScanBefore: upper.Add(time.Hour)}, CatchupTurn: 2}
	_, _, handled, err := forceCloseContractExpiryCatchupPage(upper.Add(time.Hour), before,
		func(position *ContractExpiryCursor) (int64, *ContractExpiryCursor, error) {
			if !position.ScanBefore.Equal(upper) || !position.Open.CreateTime.Equal(epoch) ||
				!position.Dispute.CreateTime.Equal(epoch) {
				t.Fatal("gap bounds overlap the retained recent pass or omit the fresh lower timestamp")
			}
			for _, value := range position.Open.ContractId {
				if value != 255 {
					t.Fatal("gap lower bound admitted a timestamp tie owned by recent")
				}
			}
			return 0, nil, nil
		})
	if !handled || err != nil {
		t.Fatal("boundary page was not selected", err)
	}
}

// A slow completed gap page hands the next invocation to fresh work. Operational
// failure and cancellation grant no new cursor; a classified refusal keeps it.
func TestExpiryCatchupBudgetAndCheckpointAuthority(t *testing.T) {
	epoch := time.Date(2026, 1, 3, 0, 0, 0, 0, time.UTC)
	cutoff := epoch.Add(4 * time.Hour)
	before := &ContractExpirySweepCursor{Historical: &ContractExpiryCursor{ScanBefore: epoch}, RecentAfter: epoch,
		Recent: &ContractExpiryCursor{ScanBefore: epoch.Add(time.Hour)},
		Fresh:  &ContractExpiryCursor{ScanBefore: cutoff}, CatchupTurn: 2}
	failure := errors.New("synthetic gap selection unavailable")
	accounting := &ForceCloseAccountingError{cause: failure, accountingRejectionCount: 1}
	for _, mode := range []string{"success", "operational", "cancellation", "accounting"} {
		ctx, cancel := context.WithCancel(context.Background())
		clock, calls := cutoff, 0
		original, _ := json.Marshal(before)
		count, next, err := forceCloseContractPagesBudgeted(ctx, 1024, before, time.Second, 256,
			func() time.Time { return clock }, func(size int, after *ContractExpirySweepCursor) (int64, *ContractExpirySweepCursor, error) {
				return forceCloseContractExpiryFreshPage(cutoff, cutoff, after,
					func(position *ContractExpiryCursor) (int64, *ContractExpiryCursor, error) {
						calls++
						if size != 256 || !position.ScanBefore.Equal(cutoff.Add(-time.Hour)) {
							t.Fatal("catch-up bypassed the shared raw page limit or selected another lane")
						}
						clock = clock.Add(2 * time.Second)
						switch mode {
						case "operational":
							return 0, position, failure
						case "cancellation":
							cancel()
						case "accounting":
							return 1, position, accounting
						}
						return 0, position, nil
					})
			})
		cancel()
		after, _ := json.Marshal(before)
		if calls != 1 || !bytes.Equal(original, after) {
			t.Fatalf("%s changed the durable input or ignored the cooperative budget", mode)
		}
		if mode == "operational" || mode == "cancellation" {
			if err == nil || next != before {
				t.Fatalf("%s acquired gap checkpoint authority", mode)
			}
		} else if next == before || next == nil || next.Catchup == nil || !next.FreshNext || next.Historical != before.Historical || next.Recent != before.Recent {
			t.Fatalf("%s lost the catch-up continuation or next-lane handoff", mode)
		} else if mode == "accounting" {
			classified, ok := err.(*ForceCloseAccountingError)
			if !ok || count != 1 || classified.AccountingRejectionCount() != 1 || classified.VerifiedCloseCount() != 0 {
				t.Fatal("gap checkpoint reclassified a reserved accounting refusal")
			}
		} else if err != nil || count != 0 {
			t.Fatal("raw gap progress claimed a verified financial close")
		}
	}
}

// Completing both original full passes cannot discard an already-started gap.
func TestExpiryCatchupRetainsWorkAfterBacklogCompletion(t *testing.T) {
	epoch := time.Date(2026, 1, 3, 0, 0, 0, 0, time.UTC)
	cutoff := epoch.Add(4 * time.Hour)
	catchup := &ContractExpiryCursor{ScanBefore: cutoff.Add(-time.Hour)}
	before := &ContractExpirySweepCursor{HistoricalDone: true, RecentAfter: epoch,
		Recent: &ContractExpiryCursor{ScanBefore: epoch.Add(time.Hour)}, Catchup: catchup, FreshBefore: cutoff}
	_, next, err := forceCloseContractExpiryFreshPage(cutoff, cutoff, before,
		func(position *ContractExpiryCursor) (int64, *ContractExpiryCursor, error) {
			if position != before.Recent {
				t.Fatal("existing full pass lost its next turn")
			}
			return 0, nil, nil
		})
	if err != nil || next == nil || !next.BacklogDone || next.Catchup != catchup {
		t.Fatal("backlog completion abandoned the fixed catch-up range")
	}
	_, next, err = forceCloseContractExpiryFreshPage(cutoff, cutoff, next,
		func(position *ContractExpiryCursor) (int64, *ContractExpiryCursor, error) {
			if position != catchup {
				t.Fatal("backlog completion rewound its catch-up continuation")
			}
			return 0, nil, nil
		})
	if err != nil || next != nil {
		t.Fatal("all completed passes failed to return to ordinary cadence")
	}
}

// A completed catch-up pass cannot consume the final ownership marker while
// fresh still has multiple raw pages after the original backlog completes.
func TestExpiryCatchupCompletionRetainsRemainingFreshPages(t *testing.T) {
	epoch := time.Date(2026, 1, 3, 0, 0, 0, 0, time.UTC)
	cutoff := epoch.Add(4 * time.Hour)
	cursor := &ContractExpirySweepCursor{HistoricalDone: true, RecentAfter: epoch, BacklogDone: true,
		Fresh: &ContractExpiryCursor{ScanBefore: cutoff}, FreshBefore: cutoff.Add(-time.Hour),
		CatchupAfter: epoch.Add(time.Hour), CatchupBefore: cutoff.Add(-time.Hour), CatchupChecked: cutoff}
	calls := 0
	for range 4 {
		_, next, err := forceCloseContractExpiryFreshPage(cutoff, cutoff, cursor,
			func(position *ContractExpiryCursor) (int64, *ContractExpiryCursor, error) {
				if !position.ScanBefore.Equal(cutoff) {
					t.Fatal("already-completed catch-up replaced unfinished fresh work")
				}
				calls++
				if calls < 3 {
					return 0, position, nil
				}
				return 0, nil, nil
			})
		if err != nil || calls < 3 && (next == nil || next.Fresh == nil) {
			t.Fatal("completed backlog and catch-up discarded an unfinished fresh page", err)
		}
		cursor = next
		if cursor == nil {
			break
		}
	}
	if calls != 3 || cursor != nil {
		t.Fatal("fresh continuation did not finish before returning to ordinary cadence")
	}
}

// The previous reader ignores additive catch-up fields while retaining every
// original cursor and turn. Rollback loses acceleration, not complete coverage.
func TestExpiryCatchupOldReaderKeepsOriginalPasses(t *testing.T) {
	epoch := time.Date(2026, 1, 3, 0, 0, 0, 0, time.UTC)
	before := &ContractExpirySweepCursor{Historical: &ContractExpiryCursor{ScanBefore: epoch}, RecentAfter: epoch,
		Recent: &ContractExpiryCursor{ScanBefore: epoch.Add(time.Hour)}, HistoricalNext: true,
		Fresh: &ContractExpiryCursor{ScanBefore: epoch.Add(4 * time.Hour)}, FreshBefore: epoch.Add(3 * time.Hour), FreshNext: true,
		Catchup: &ContractExpiryCursor{ScanBefore: epoch.Add(2 * time.Hour)}, CatchupBefore: epoch.Add(90 * time.Minute),
		CatchupAfter: epoch.Add(time.Hour), CatchupChecked: epoch.Add(4 * time.Hour), CatchupTurn: 2}
	var old struct {
		Historical     *ContractExpiryCursor `json:"historical,omitempty"`
		Recent         *ContractExpiryCursor `json:"recent,omitempty"`
		RecentAfter    time.Time             `json:"recent_after"`
		HistoricalDone bool                  `json:"historical_done,omitempty"`
		HistoricalNext bool                  `json:"historical_next,omitempty"`
		Fresh          *ContractExpiryCursor `json:"fresh,omitempty"`
		FreshBefore    time.Time             `json:"fresh_before,omitzero"`
		FreshNext      bool                  `json:"fresh_next,omitempty"`
		BacklogDone    bool                  `json:"backlog_done,omitempty"`
	}
	raw, _ := json.Marshal(before)
	if json.Unmarshal(raw, &old) != nil {
		t.Fatal("old reader rejected the additive checkpoint fields")
	}
	raw, _ = json.Marshal(old)
	var restored ContractExpirySweepCursor
	if json.Unmarshal(raw, &restored) != nil {
		t.Fatal("old checkpoint failed to return to the current reader")
	}
	expected := *before
	expected.Catchup, expected.CatchupAfter, expected.CatchupBefore, expected.CatchupChecked, expected.CatchupTurn = nil, time.Time{}, time.Time{}, time.Time{}, 0
	if !reflect.DeepEqual(expected, restored) {
		t.Fatal("rollback changed an original pass, epoch, completion marker, or handoff")
	}
}
