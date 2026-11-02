package model

import (
	"context"
	"testing"
	"time"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/server"
)

// Tests of the extender activation history.

// The activation history keeps the genesis accuracy radius of the lookup that
// placed the activating address (connect/GEOMAP.md §5.1), and a lookup
// without one stores NULL rather than a zero that would read as an address
// placed exactly.
func TestExtenderActivationStoresTheAccuracyRadius(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		sign, _ := testRecordSigner()
		sydney := testStatsCityLocation(ctx, "Sydney", "New South Wales", "Australia", "au")

		publicKey := []byte("extender-public-key-accuracy-001")
		located := testExtenderActivation(publicKey, 4, "192.0.2.60")
		accuracyKm := float32(25)
		located.AccuracyKm = &accuracyKm
		activated := ActivateNetworkExtender(ctx, located.WithLocation(sydney), sign)
		if activated == nil {
			t.Fatal("the activation was not stored")
		}

		// a second activation whose lookup gave no radius
		unplaced := testExtenderActivation(publicKey, 6, "2001:db8::60")
		ActivateNetworkExtender(ctx, unplaced, sign)

		activations := GetNetworkExtenderActivations(ctx, activated.Extender.ExtenderId, time.Time{})
		connect.AssertEqual(t, len(activations), 2)
		if activations[0].AccuracyKm == nil || *activations[0].AccuracyKm != accuracyKm {
			t.Fatalf("the located activation stored radius %v, want %f", activations[0].AccuracyKm, accuracyKm)
		}
		connect.AssertEqual(t, activations[0].CityLocationId != nil, true)
		if activations[1].AccuracyKm != nil {
			t.Fatalf("the unplaced activation stored radius %f", *activations[1].AccuracyKm)
		}
	})
}

// The history keeps the run of the extender process that sent each activation
// (connect/EXTENDER.md G3), its start in utc whatever zone it was written in,
// and an activation without a run stores null rather than an empty string or
// a zero time.
func TestExtenderActivationStoresTheRun(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		sign, _ := testRecordSigner()
		publicKey := []byte("extender-public-key-run-00000001")

		withRun := testExtenderActivation(publicKey, 4, "192.0.2.61")
		runId := "00112233445566778899aabbccddeeff"
		runStartTime := time.Date(2026, 11, 2, 3, 4, 5, 6000, time.FixedZone("test", -6*60*60))
		previousExit := "exit-137"
		withRun.RunId = &runId
		withRun.RunStartTime = &runStartTime
		withRun.PreviousExit = &previousExit
		activated := ActivateNetworkExtender(ctx, withRun, sign)
		if activated == nil {
			t.Fatal("the activation was not stored")
		}

		// an activation from an extender that predates the run
		ActivateNetworkExtender(ctx, testExtenderActivation(publicKey, 6, "2001:db8::61"), sign)

		activations := GetNetworkExtenderActivations(ctx, activated.Extender.ExtenderId, time.Time{})
		connect.AssertEqual(t, len(activations), 2)

		stored := activations[0]
		if stored.RunId == nil || *stored.RunId != runId {
			t.Fatalf("stored run id = %v, want %s", stored.RunId, runId)
		}
		if stored.RunStartTime == nil || !stored.RunStartTime.Equal(runStartTime) {
			t.Fatalf("stored run start = %v, want %s", stored.RunStartTime, runStartTime)
		}
		if stored.PreviousExit == nil || *stored.PreviousExit != previousExit {
			t.Fatalf("stored previous exit = %v, want %s", stored.PreviousExit, previousExit)
		}

		withoutRun := activations[1]
		if withoutRun.RunId != nil || withoutRun.RunStartTime != nil || withoutRun.PreviousExit != nil {
			t.Fatalf("an activation with no run stored %v %v %v", withoutRun.RunId, withoutRun.RunStartTime, withoutRun.PreviousExit)
		}
	})
}

// The run counts are of extenders, by the distinct runs among their
// activations in the window: not activations, not families, and not anything
// that left the window.
func TestCountExtenderRunsCountsDistinctRunsInTheWindow(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		sign, _ := testRecordSigner()
		now := server.NowUtc()
		since := now.Add(-24 * time.Hour)

		// one activation per run id, an empty id being none
		activate := func(name string, runIds ...string) server.Id {
			publicKey := []byte("extender-public-key-count-" + name)
			var extenderId server.Id
			for _, runId := range runIds {
				activation := testExtenderActivation(publicKey, 4, "192.0.2.62")
				if runId != "" {
					activation.RunId = &runId
				}
				extenderId = ActivateNetworkExtender(ctx, activation, sign).Extender.ExtenderId
			}
			return extenderId
		}
		// moves the rows of the given runs out of the window
		age := func(extenderId server.Id, runIds ...string) {
			server.Tx(ctx, func(tx server.PgTx) {
				server.RaisePgResult(tx.Exec(
					ctx,
					`
					UPDATE network_extender_activation
					SET activate_time = activate_time - interval '25 hours'
					WHERE extender_id = $1 AND run_id = ANY($2)
					`,
					extenderId,
					runIds,
				))
			})
		}

		connect.AssertEqual(t, CountExtenderRuns(ctx, since, 5), ExtenderRunCounts{})

		activate("none", "", "", "", "", "", "")
		connect.AssertEqual(t, CountExtenderRuns(ctx, since, 5), ExtenderRunCounts{})

		activate("five", "a", "b", "c", "d", "e")
		activate("four", "a", "b", "c", "d")
		activate("one-repeating", "a", "a", "a", "a", "a", "a")
		// a mix of runs and rows that carry none counts only the runs
		activate("mixed", "a", "", "b", "", "c", "")
		connect.AssertEqual(t, CountExtenderRuns(ctx, since, 5), ExtenderRunCounts{Reporting: 4, Restarting: 1})

		// five runs of which two left the window are three in it
		leaving := activate("leaving", "a", "b", "c", "d", "e")
		age(leaving, "a", "b")
		connect.AssertEqual(t, CountExtenderRuns(ctx, since, 5), ExtenderRunCounts{Reporting: 5, Restarting: 1})

		// five runs all gone from the window are not reporting at all
		gone := activate("gone", "a", "b", "c", "d", "e")
		age(gone, "a", "b", "c", "d", "e")
		connect.AssertEqual(t, CountExtenderRuns(ctx, since, 5), ExtenderRunCounts{Reporting: 5, Restarting: 1})

		// the threshold is the caller's
		connect.AssertEqual(t, CountExtenderRuns(ctx, since, 4), ExtenderRunCounts{Reporting: 5, Restarting: 2})
	})
}
