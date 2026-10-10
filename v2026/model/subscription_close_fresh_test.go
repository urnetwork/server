// Fresh arrivals must not wait for either retained backlog pass to finish.
package model

import (
	"encoding/json"
	"errors"
	"testing"
	"time"
)

// The old two-lane scheduler advances both million-row backlogs but cannot
// select any arrival beyond their fixed upper bounds. The added lane admits a
// finite arrival load below its raw capacity and retains both middle cursors.
func TestExpiryFreshTailAdmitsBeyondFixedBacklogs(t *testing.T) {
	const backlogRows = 1_000_000
	const pageSize = 256
	const arrivalTurns = 96
	const arrivalsPerTurn = 80
	const maximumDelay = 24
	epoch := time.Date(2026, 1, 3, 0, 0, 0, 0, time.UTC)
	middleUpper := epoch.Add(48 * time.Hour)
	cutoff := middleUpper.Add(4 * time.Hour)
	cursor := &ContractExpirySweepCursor{Historical: &ContractExpiryCursor{ScanBefore: epoch, DisputeDone: true},
		Recent: &ContractExpiryCursor{ScanBefore: middleUpper, DisputeDone: true}, RecentAfter: epoch}
	type arrival struct {
		position ContractExpiryPosition
		stream   int
		turn     int
		visited  bool
	}
	arrivals := []*arrival{}
	oldVisited, middleVisited, largestDelay := 0, 0, 0
	for turn := 0; turn < arrivalTurns+maximumDelay; turn++ {
		cutoff = cutoff.Add(time.Minute)
		if turn < arrivalTurns {
			for stream := range 2 {
				for index := range arrivalsPerTurn {
					arrivals = append(arrivals, &arrival{position: ContractExpiryPosition{
						CreateTime: cutoff.Add(-time.Second + time.Duration(index)*time.Microsecond), ContractId: expiryFairTestId(len(arrivals))}, stream: stream, turn: turn})
				}
			}
		}
		_, next, err := forceCloseContractExpiryFreshPage(cutoff, cutoff.Add(12*time.Minute), cursor,
			func(position *ContractExpiryCursor) (int64, *ContractExpiryCursor, error) {
				nextPage := *position
				if position.ScanBefore.Equal(epoch) || position.ScanBefore.Equal(middleUpper) {
					visited := &oldVisited
					start := epoch.Add(-time.Hour)
					if position.ScanBefore.Equal(middleUpper) {
						visited, start = &middleVisited, epoch.Add(time.Hour)
					}
					*visited += pageSize
					if *visited >= backlogRows {
						t.Fatal("test unexpectedly exhausted its retained backlog")
					}
					nextPage.Open = &ContractExpiryPosition{CreateTime: start.Add(time.Duration(*visited) * time.Microsecond), ContractId: expiryFairTestId(*visited)}
					return 0, &nextPage, nil
				}
				count := 0
				for stream, saved := range []**ContractExpiryPosition{&nextPage.Open, &nextPage.Dispute} {
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
						largestDelay = max(largestDelay, turn-row.turn)
						if seen == pageSize {
							break
						}
					}
					count += seen
					if stream == 0 {
						nextPage.OpenDone = seen < pageSize
					} else {
						nextPage.DisputeDone = seen < pageSize
					}
				}
				if nextPage.OpenDone && nextPage.DisputeDone {
					return int64(count), nil, nil
				}
				return int64(count), &nextPage, nil
			})
		if err != nil || next == nil {
			t.Fatal("fresh service lost the unfinished full backlog passes")
		}
		raw, _ := json.Marshal(next)
		var stored *ContractExpirySweepCursor
		if json.Unmarshal(raw, &stored) != nil {
			t.Fatal("fresh ownership did not survive durable JSON state")
		}
		cursor = stored
	}
	for _, row := range arrivals {
		if !row.visited {
			t.Fatal("fresh expired arrival was excluded behind a retained fixed pass")
		}
	}
	if oldVisited < pageSize || middleVisited < pageSize || largestDelay > maximumDelay ||
		!cursor.Historical.ScanBefore.Equal(epoch) || !cursor.Recent.ScanBefore.Equal(middleUpper) || !cursor.RecentAfter.Equal(epoch) {
		t.Fatal("fresh admission discarded the old middle or exceeded its admitted-load visit bound")
	}
	t.Logf("historical_raw=%d middle_raw=%d fresh_arrivals=%d maximum_visit_turns=%d", oldVisited, middleVisited, len(arrivals), largestDelay)
}

// Completing the older ranges must not drop an unfinished fresh continuation.
func TestExpiryFreshRetainsWorkAfterBacklogCompletion(t *testing.T) {
	epoch := time.Date(2026, 1, 3, 0, 0, 0, 0, time.UTC)
	fresh := &ContractExpiryCursor{ScanBefore: epoch.Add(72 * time.Hour)}
	before := &ContractExpirySweepCursor{HistoricalDone: true, RecentAfter: epoch,
		Recent: &ContractExpiryCursor{ScanBefore: epoch.Add(time.Hour)}, Fresh: fresh}
	_, next, err := forceCloseContractExpiryFreshPage(fresh.ScanBefore, fresh.ScanBefore, before,
		func(position *ContractExpiryCursor) (int64, *ContractExpiryCursor, error) {
			if position != before.Recent {
				t.Fatal("unfinished historical policy did not own its next turn")
			}
			return 1, nil, nil
		})
	if err != nil || next == nil || !next.BacklogDone || next.Fresh != fresh || !next.FreshNext || before.BacklogDone {
		t.Fatal("finished backlog discarded unfinished fresh work or mutated its caller")
	}
	_, next, err = forceCloseContractExpiryFreshPage(fresh.ScanBefore, fresh.ScanBefore, next,
		func(position *ContractExpiryCursor) (int64, *ContractExpiryCursor, error) {
			if position != fresh {
				t.Fatal("finished backlog replaced the fixed fresh continuation")
			}
			return 1, nil, nil
		})
	if err != nil || next == nil || next.CatchupAfter.IsZero() {
		t.Fatal("completed original passes dropped the interval above their recent upper")
	}
	_, next, err = forceCloseContractExpiryFreshPage(fresh.ScanBefore, fresh.ScanBefore, next,
		func(position *ContractExpiryCursor) (int64, *ContractExpiryCursor, error) {
			if !position.ScanBefore.Equal(fresh.ScanBefore.Add(-forceCloseFreshWindow)) ||
				!position.Open.CreateTime.Equal(before.Recent.ScanBefore) {
				t.Fatal("completion changed the retained gap bounds")
			}
			return 1, nil, nil
		})
	if err != nil || next != nil {
		t.Fatal("complete fresh, backlog and gap passes failed to return to ordinary cadence")
	}
}

// A speculative fresh result alone grants no retry checkpoint authority.
func TestExpiryFreshOperationalFailureKeepsPriorState(t *testing.T) {
	epoch := time.Date(2026, 1, 3, 0, 0, 0, 0, time.UTC)
	before := &ContractExpirySweepCursor{Historical: &ContractExpiryCursor{ScanBefore: epoch}}
	failure := errors.New("synthetic fresh selector unavailable")
	_, next, err := forceCloseContractExpiryFreshPage(epoch.Add(72*time.Hour), epoch.Add(72*time.Hour), before,
		func(*ContractExpiryCursor) (int64, *ContractExpiryCursor, error) { return 0, nil, failure })
	if err != failure || next != before || before.Fresh != nil || !before.FreshBefore.IsZero() {
		t.Fatal("incomplete fresh work gained checkpoint authority")
	}
}
