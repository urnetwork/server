// The successor the maintenance post schedules for each outcome of its body.
package work

import (
	"encoding/json"
	"slices"
	"testing"
	"time"

	"github.com/urnetwork/server"
)

// Every stop keeps the epoch and what it finished; only a finished epoch
// advances. An interruption continues once a rolling deploy's drain wave has
// passed; a deferral waits for the next maintenance slot.
func TestDbMaintenanceNextTaskKeepsAStoppedEpoch(t *testing.T) {
	now := time.Date(2026, 1, 2, 5, 30, 0, 0, time.UTC)
	slot := time.Date(2026, 1, 2, 7, 0, 0, 0, time.UTC)
	if got := dbMaintenanceNextRunAt(now); !got.Equal(slot) {
		t.Fatalf("next slot after %s = %s, want %s", now, got, slot)
	}
	if got := dbMaintenanceNextRunAt(slot); !got.Equal(time.Date(2026, 1, 3, 3, 0, 0, 0, time.UTC)) {
		t.Fatalf("next slot at %s = %s", slot, got)
	}

	args := &DbMaintenanceArgs{Epoch: 41, DoneTables: []string{"synthetic_a"}}
	for _, c := range []struct {
		result      *DbMaintenanceResult
		epoch       uint64
		doneTables  []string
		doneIndexes []string
		runAt       time.Time
	}{
		{
			result: &DbMaintenanceResult{DoneTables: []string{"synthetic_a", "synthetic_b"}},
			epoch:  42,
			runAt:  slot,
		},
		{
			result: &DbMaintenanceResult{
				Stopped:     server.DbMaintenanceInterrupted,
				DoneTables:  []string{"synthetic_a", "synthetic_b"},
				DoneIndexes: []string{"synthetic_index"},
			},
			epoch:       41,
			doneTables:  []string{"synthetic_a", "synthetic_b"},
			doneIndexes: []string{"synthetic_index"},
			runAt:       now.Add(5 * time.Minute),
		},
		{
			result: &DbMaintenanceResult{
				Stopped:    server.DbMaintenanceDeferred,
				DoneTables: []string{"synthetic_a"},
			},
			epoch:      41,
			doneTables: []string{"synthetic_a"},
			runAt:      slot,
		},
	} {
		next, runAt := dbMaintenanceNextTask(args, c.result, now)
		if next.Epoch != c.epoch || !slices.Equal(next.DoneTables, c.doneTables) ||
			!slices.Equal(next.DoneIndexes, c.doneIndexes) || !runAt.Equal(c.runAt) {
			t.Errorf("after %+v: next %+v at %s, want epoch %d done %v %v at %s",
				*c.result, *next, runAt, c.epoch, c.doneTables, c.doneIndexes, c.runAt)
		}
	}

	// tasks queued before the done lists existed still decode
	var legacy DbMaintenanceArgs
	if err := json.Unmarshal([]byte(`{"epoch":7}`), &legacy); err != nil || legacy.Epoch != 7 || legacy.DoneTables != nil {
		t.Fatalf("legacy args = %+v, %v", legacy, err)
	}
}
