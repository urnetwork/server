// The actual store uses a descriptor-free SQL clock read at the authority and
// opening boundaries, not an application host's differently skewed wall clock.
package controller

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

type competitionIntakeFirstClockTestReader struct {
	now   time.Time
	err   error
	calls int
}

func (self *competitionIntakeFirstClockTestReader) QueryRow(_ context.Context, query string, _ ...any) pgx.Row {
	self.calls++
	return roundBaselinePreparationScanRow(func(destinations ...any) error {
		if query != `SELECT timezone('UTC',clock_timestamp())` || len(destinations) != 1 {
			return errors.New("unexpected intake clock ownership query")
		}
		if self.err != nil {
			return self.err
		}
		*destinations[0].(*time.Time) = self.now
		return nil
	})
}

// A single SQL observation is normalized only by zone, never replaced by a
// process clock; a failed read is propagated without inventing a timestamp.
func TestCompetitionIntakeFirstAuthorityUsesDatabaseClock(t *testing.T) {
	databaseTime := time.Date(2040, 2, 3, 4, 5, 6, 123456000, time.FixedZone("synthetic", 3600))
	reader := &competitionIntakeFirstClockTestReader{now: databaseTime}
	observed, err := competitionIntakeFirstDatabaseTime(context.Background(), reader)
	if err != nil || !observed.Equal(databaseTime) || observed.Location() != time.UTC || reader.calls != 1 {
		t.Fatal("database time owner was substituted or read more than once", observed, err, reader.calls)
	}
	failure := errors.New("synthetic clock query failed")
	reader.err = failure
	if _, err := competitionIntakeFirstDatabaseTime(context.Background(), reader); !errors.Is(err, failure) || reader.calls != 2 {
		t.Fatal("failed authority clock was converted into success", err, reader.calls)
	}
}
