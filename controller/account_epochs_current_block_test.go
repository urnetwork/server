package controller

// The epoch the open operator block of GET /account/epochs counts in
// (snEpochOfBlockEnd).

import (
	"testing"
	"time"

	"github.com/urnetwork/server/model"
)

// A block counts in the epoch whose window holds its end, as the points do:
// the open epoch while the block ends inside its window, else each later
// epoch of the same length, and an end exactly at an epoch's end belongs to
// the next, as the half-open windows count it. On Main's clock an epoch opens
// Friday about 18:09 UTC and blocks end Sunday 00:00 UTC, so the open block
// ends in the open epoch from Friday to Sunday and in the next epoch from
// Sunday to Friday.
func TestSnEpochOfBlockEndPlacesTheBlockByItsEnd(t *testing.T) {
	start := time.Date(2026, 11, 13, 18, 9, 0, 0, time.UTC)
	week := 7 * 24 * time.Hour
	sunday := time.Date(2026, 11, 15, 0, 0, 0, 0, time.UTC)
	open := model.PointsEpochWindow{Epoch: 6, Start: start, End: start.Add(week)}
	for _, c := range []struct {
		end   time.Time
		epoch uint64
		ok    bool
	}{
		{end: start, epoch: 6, ok: true},
		{end: sunday, epoch: 6, ok: true},
		{end: start.Add(week - time.Nanosecond), epoch: 6, ok: true},
		{end: start.Add(week), epoch: 7, ok: true},
		{end: sunday.Add(week), epoch: 7, ok: true},
		{end: start.Add(2 * week), epoch: 8, ok: true},
		{end: start.Add(-time.Hour), epoch: 0, ok: false},
	} {
		epoch, ok := snEpochOfBlockEnd(open, c.end)
		if epoch != c.epoch || ok != c.ok {
			t.Errorf("block end %s: epoch %d ok %t, want epoch %d ok %t", c.end, epoch, ok, c.epoch, c.ok)
		}
	}
	if _, ok := snEpochOfBlockEnd(model.PointsEpochWindow{Epoch: 6, Start: start, End: start}, start.Add(time.Hour)); ok {
		t.Error("an epoch of no length placed a block")
	}
}
