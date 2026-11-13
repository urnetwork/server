package model

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
)

func TestProviderUrlProbeFleetObservedRetainsExactCensus(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		now := server.NowUtc().Truncate(time.Microsecond)
		testingSeedUrlProbeFleet(t, now.Add(-5*time.Hour), 3)
		ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
		defer cancel()
		before := GetProviderUrlProbeFleet(ctx, now)
		observation := server.NewDbReadObservation()
		observed := GetProviderUrlProbeFleetObserved(ctx, now, observation)
		read := observation.Snapshot()
		if !reflect.DeepEqual(before, observed) || observed.MatureEligible != 3 || observed.MatureRunsNeeded != 30 {
			t.Fatal("instrumentation changed exact mature quota or diagnostic fields")
		}
		if read.AcquireSucceeded != 1 || read.QuerySucceeded != 1 || read.Rows != 1 || read.Phase != server.DbReadQueryDone || read.Finished || read.PhaseCounts[server.DbReadAcquireBegin] != 1 || read.PhaseCounts[server.DbReadAcquireDone] != 1 || read.PhaseCounts[server.DbReadQueryBegin] != 1 || read.PhaseCounts[server.DbReadQueryDone] != 1 {
			t.Fatalf("model did not publish complete actual read phases: %+v", read)
		}
		if !read.PhaseAt[server.DbReadQueryBegin].Before(read.PhaseAt[server.DbReadRows]) || !read.PhaseAt[server.DbReadRows].Before(read.PhaseAt[server.DbReadQueryDone]) || read.AcquireDuration <= 0 || read.QueryDuration <= 0 {
			t.Fatal("read phases lost their source ordering or duration")
		}
	})
}
