// Deterministic page ownership controls use logical time and explicit results.
package model

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/urnetwork/server"
)

// Synthetic ids only; creation ordering is always the explicit timestamp first.
func expiryFairTestId(sequence int) server.Id {
	var id server.Id
	binary.BigEndian.PutUint64(id[8:], uint64(sequence+1))
	return id
}

// A stored legacy cursor must survive the first post-epoch page unchanged.
func TestExpiryFairLegacyCursorMigrationAndBoundary(t *testing.T) {
	epoch := time.Date(2026, 1, 3, 0, 0, 0, 0, time.UTC)
	legacy := &ContractExpiryCursor{ScanBefore: epoch, DisputeDone: true,
		Open: &ContractExpiryPosition{CreateTime: epoch.Add(-time.Hour), ContractId: expiryFairTestId(4)}}
	before, _ := json.Marshal(legacy)
	var seen *ContractExpiryCursor
	_, next, err := forceCloseContractExpirySweepPage(epoch.Add(48*time.Hour), epoch.Add(49*time.Hour),
		&ContractExpirySweepCursor{Historical: legacy}, func(position *ContractExpiryCursor) (int64, *ContractExpiryCursor, error) {
			seen = position
			return 0, nil, nil
		})
	after, _ := json.Marshal(legacy)
	if err != nil || !bytes.Equal(before, after) || next.Historical != legacy || !next.HistoricalNext ||
		!next.RecentAfter.Equal(epoch) || !seen.ScanBefore.Equal(epoch.Add(48*time.Hour)) ||
		!seen.Open.CreateTime.Equal(epoch) || !seen.Dispute.CreateTime.Equal(epoch) {
		t.Fatal("legacy cursor or post-epoch range changed")
	}
	for _, value := range seen.Open.ContractId {
		if value != 255 {
			t.Fatal("epoch timestamp did not exclude all already-owned UUID ties")
		}
	}
}

// A million retained rows per selector cannot monopolize the next page.
// Arrivals consume three quarters of each post-epoch selector's capacity and
// cross multiple fixed pass boundaries while both historical cursors advance.
func TestExpiryFairLargeBacklogHighArrival(t *testing.T) {
	const oldRows = 1_000_000
	const pageSize = 256
	const arrivalTurns = 128
	const arrivalsPerTurn = 96
	const maximumVisitTurns = 32
	epoch := time.Date(2026, 1, 3, 0, 0, 0, 0, time.UTC)
	cutoff := epoch.Add(72 * time.Hour)
	oldStart := epoch.Add(-24 * time.Hour)
	type arrival struct {
		position ContractExpiryPosition
		stream   int
		turn     int
		visited  bool
	}
	arrivals := []*arrival{}
	// Already old at upgrade, but newer than the preserved historical epoch.
	for stream := range 2 {
		for i := range 512 {
			arrivals = append(arrivals, &arrival{stream: stream, position: ContractExpiryPosition{
				CreateTime: epoch.Add(time.Hour + time.Duration(i)*time.Microsecond), ContractId: expiryFairTestId(len(arrivals))}})
		}
	}
	var cursor *ContractExpirySweepCursor = &ContractExpirySweepCursor{Historical: &ContractExpiryCursor{ScanBefore: epoch}}
	oldVisited := [2]int{}
	maximumDelay, recentPasses := 0, 0
	for turn := 0; turn < arrivalTurns+maximumVisitTurns; turn++ {
		cutoff = cutoff.Add(time.Minute)
		if turn < arrivalTurns {
			for stream := range 2 {
				for i := range arrivalsPerTurn {
					arrivals = append(arrivals, &arrival{stream: stream, position: ContractExpiryPosition{
						CreateTime: cutoff.Add(-time.Second + time.Duration(i)*time.Microsecond),
						ContractId: expiryFairTestId(len(arrivals))}, turn: turn})
				}
			}
		}
		_, next, err := forceCloseContractExpirySweepPage(cutoff, cutoff.Add(12*time.Minute), cursor,
			func(position *ContractExpiryCursor) (int64, *ContractExpiryCursor, error) {
				nextPage := *position
				positions := []**ContractExpiryPosition{&nextPage.Open, &nextPage.Dispute}
				done := []*bool{&nextPage.OpenDone, &nextPage.DisputeDone}
				if position.ScanBefore.Equal(epoch) {
					for stream, saved := range positions {
						start := 0
						if *saved != nil {
							start = int((*saved).CreateTime.Sub(oldStart)/time.Microsecond) + 1
						}
						end := min(start+pageSize, oldRows)
						oldVisited[stream] += end - start
						*saved = &ContractExpiryPosition{CreateTime: oldStart.Add(time.Duration(end-1) * time.Microsecond), ContractId: expiryFairTestId(end)}
					}
					return 0, &nextPage, nil
				}
				total := 0
				for stream, saved := range positions {
					if *done[stream] {
						continue
					}
					seen := 0
					for _, row := range arrivals {
						if row.stream != stream || row.visited || row.position.CreateTime.After(position.ScanBefore) ||
							(*saved != nil && !row.position.CreateTime.After((*saved).CreateTime)) {
							continue
						}
						seen++
						copyPosition := row.position
						*saved = &copyPosition
						row.visited = true
						maximumDelay = max(maximumDelay, turn-row.turn)
						if seen == pageSize {
							break
						}
					}
					total += seen
					*done[stream] = seen < pageSize
				}
				if nextPage.OpenDone && nextPage.DisputeDone {
					recentPasses++
					return int64(total), nil, nil
				}
				return int64(total), &nextPage, nil
			})
		if err != nil || next == nil {
			t.Fatal("unfinished million-row historical pass was lost")
		}
		// Exercise the exact durable JSON boundary between every page.
		raw, err := json.Marshal(next)
		var stored *ContractExpirySweepCursor
		if err != nil || json.Unmarshal(raw, &stored) != nil {
			t.Fatal("persisted sweep state failed round trip")
		}
		cursor = stored
	}
	for _, row := range arrivals {
		if !row.visited {
			t.Fatal("eligible post-epoch arrival starved behind old backlog")
		}
	}
	if oldVisited[0] != oldVisited[1] || oldVisited[0] <= pageSize || oldVisited[0] >= oldRows || maximumDelay > maximumVisitTurns || recentPasses < 2 {
		t.Fatalf("fairness bound failed: old=%v max_turns=%d recent_passes=%d", oldVisited, maximumDelay, recentPasses)
	}
	t.Logf("old_retained_per_selector=%d old_visited=%v new_arrivals=%d max_visit_turns=%d recent_passes=%d", oldRows, oldVisited, len(arrivals), maximumDelay, recentPasses)
}

// Recent passes revisit skipped active rows even after they age beyond a day.
func TestExpiryFairActiveRecordDoesNotAgeOut(t *testing.T) {
	epoch := time.Date(2026, 1, 3, 0, 0, 0, 0, time.UTC)
	created := epoch.Add(time.Hour)
	legacy := &ContractExpiryCursor{ScanBefore: epoch}
	cursor := &ContractExpirySweepCursor{Historical: legacy}
	quiet, visited := false, false
	page := func(position *ContractExpiryCursor) (int64, *ContractExpiryCursor, error) {
		if position.ScanBefore.Equal(epoch) {
			return 0, legacy, nil
		}
		if !position.Open.CreateTime.Before(created) || position.ScanBefore.Before(created) {
			t.Fatal("deferred post-epoch row fell outside its next pass")
		}
		if quiet {
			visited = true
			return 1, nil, nil
		}
		return 0, nil, nil
	}
	for turn := range 3 {
		cutoff := epoch.Add(2 * time.Hour)
		if turn == 2 {
			quiet = true
			cutoff = cutoff.Add(72 * time.Hour)
		}
		_, next, err := forceCloseContractExpirySweepPage(cutoff, cutoff.Add(12*time.Minute), cursor, page)
		if err != nil {
			t.Fatal(err)
		}
		cursor = next
	}
	if !visited || !cursor.RecentAfter.Equal(epoch) {
		t.Fatal("active record aged out of automatic coverage")
	}
}

// Explicit logical-clock transitions force exactly one over-budget page.
func TestExpiryFairBudgetOverrunHandsOffBothLanes(t *testing.T) {
	epoch := time.Date(2026, 1, 3, 0, 0, 0, 0, time.UTC)
	cursor := &ContractExpirySweepCursor{Historical: &ContractExpiryCursor{ScanBefore: epoch}}
	for turn := range 2 {
		clock := epoch.Add(72 * time.Hour)
		calls := 0
		_, next, err := forceCloseContractPagesBudgeted(context.Background(), 1024, cursor, time.Second, 256,
			func() time.Time { return clock }, func(size int, after *ContractExpirySweepCursor) (int64, *ContractExpirySweepCursor, error) {
				if size != 256 {
					t.Fatal("shared raw-row cap changed")
				}
				return forceCloseContractExpirySweepPage(clock, clock, after, func(position *ContractExpiryCursor) (int64, *ContractExpiryCursor, error) {
					calls++
					if position.ScanBefore.Equal(epoch) != (turn == 1) {
						t.Fatal("the same slow lane monopolized the next invocation")
					}
					clock = clock.Add(2 * time.Second)
					return 0, position, nil
				})
			})
		if err != nil || calls != 1 || next == nil || next.HistoricalNext != (turn == 0) {
			t.Fatal("complete page did not persist the opposite lane")
		}
		cursor = next
	}
}

// Ordinary failures and cancellation retain the entire prior durable state.
func TestExpiryFairUnknownFailureAndCancellationKeepCursor(t *testing.T) {
	epoch := time.Date(2026, 1, 3, 0, 0, 0, 0, time.UTC)
	for _, cancelPage := range []bool{false, true} {
		ctx, cancel := context.WithCancel(context.Background())
		before := &ContractExpirySweepCursor{Historical: &ContractExpiryCursor{ScanBefore: epoch}}
		original, _ := json.Marshal(before)
		failure := errors.New("synthetic operational refusal")
		_, next, err := forceCloseContractPagesBudgeted(ctx, 256, before, time.Second, 256, time.Now,
			func(_ int, after *ContractExpirySweepCursor) (int64, *ContractExpirySweepCursor, error) {
				return forceCloseContractExpirySweepPage(epoch.Add(time.Hour), epoch.Add(time.Hour), after,
					func(position *ContractExpiryCursor) (int64, *ContractExpiryCursor, error) {
						if cancelPage {
							cancel()
							return 0, position, nil
						}
						return 0, position, failure
					})
			})
		cancel()
		after, _ := json.Marshal(before)
		if err == nil || next != before || !bytes.Equal(original, after) ||
			(cancelPage && !errors.Is(err, context.Canceled)) || (!cancelPage && !errors.Is(err, failure)) {
			t.Fatal("uncertain page gained checkpoint authority")
		}
	}
}

// Classified accounting refusals alone may checkpoint a completed page.
func TestExpiryFairAccountingAndCompletePass(t *testing.T) {
	epoch := time.Date(2026, 1, 3, 0, 0, 0, 0, time.UTC)
	legacy := &ContractExpiryCursor{ScanBefore: epoch}
	before := &ContractExpirySweepCursor{Historical: legacy}
	failure := &ForceCloseAccountingError{cause: errors.New("synthetic reserved dispute"), accountingRejectionCount: 1}
	_, next, err := forceCloseContractExpirySweepPage(epoch.Add(time.Hour), epoch.Add(time.Hour), before,
		func(*ContractExpiryCursor) (int64, *ContractExpiryCursor, error) { return 1, nil, failure })
	if err != failure || next == nil || !next.HistoricalNext || !reflect.DeepEqual(next.Historical, legacy) {
		t.Fatal("classified error lost its historical or next-lane state")
	}
	_, final, err := forceCloseContractExpirySweepPage(epoch.Add(time.Hour), epoch.Add(time.Hour), next,
		func(position *ContractExpiryCursor) (int64, *ContractExpiryCursor, error) {
			if !reflect.DeepEqual(position, legacy) {
				t.Fatal("historical continuation changed")
			}
			return 0, nil, nil
		})
	if err != nil || final != nil {
		t.Fatal("both complete passes failed to return to idle cadence")
	}
}
