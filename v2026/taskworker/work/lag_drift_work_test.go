package work

import (
	"context"
	"encoding/json"
	"net/netip"
	"testing"
	"time"

	"github.com/urnetwork/connect/v2026"
	"github.com/urnetwork/connect/v2026/lagdrift"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/controller"
	"github.com/urnetwork/server/v2026/model"
	"github.com/urnetwork/server/v2026/session"
)

// The drift sweep (lag.dev/DRIFTTEST.md E5, K3, R1): the runs it completes,
// the hashes it deletes, its retention, the runs it reports stuck, and its
// chain.

// A drift test allocated at `now`, with one run started then that never
// finishes or reports.
func testLagDriftStartedRun(t testing.TB, ctx context.Context, now time.Time) *model.LagDriftTest {
	t.Helper()
	addr := netip.MustParseAddr("198.51.100.30")
	clientIpHash := server.ClientIpHashForAddr(addr)
	test := &model.LagDriftTest{
		DriftTestId:    server.NewId(),
		NetworkId:      server.NewId(),
		TokenHash:      server.NewId().Bytes(),
		ClientIpHash:   clientIpHash[:],
		ClientIpFamily: 4,
		Confidence:     0.99,
		CreatedAt:      now,
		ExpiresAt:      now.Add(5 * time.Minute),
	}
	model.CreateLagDriftTest(ctx, test)
	trackId := server.NewId()
	added := model.AddLagDriftRun(ctx, &model.AddLagDriftRunArgs{
		DriftTestId:   test.DriftTestId,
		StartIpHash:   clientIpHash,
		StartIpFamily: 4,
		Track: &model.LagTrack{
			TrackId:   trackId,
			NetworkId: test.NetworkId,
			ClientId:  test.DriftTestId,
			StartedAt: now,
		},
		IpCountryCode:   "de",
		IpCountryName:   "Germany",
		Extenders:       []*model.LagDriftRunExtender{{Position: 0, ExtenderId: server.NewId(), CountryCode: "de", Latitude: 52.5, Longitude: 13.4, PlaceErrorKm: 20}},
		Now:             now,
		MaxRuns:         6,
		RetestWindow:    15 * time.Minute,
		ProcessingAfter: 30 * time.Second,
	})
	connect.AssertEqual(t, added.Run, 1)
	return test
}

func testLagDriftSweepBorders(t testing.TB) *lagdrift.Borders {
	t.Helper()
	borders, err := lagdrift.NewBorders("ne-admin0-10m-test", []lagdrift.Country{{
		Code: "de",
		Polygons: [][][]lagdrift.Point{{{
			{Lat: 47.3, Lon: 5.9}, {Lat: 47.3, Lon: 15.0}, {Lat: 55.0, Lon: 15.0}, {Lat: 55.0, Lon: 5.9},
		}}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	return borders
}

// A run nobody looks at is completed once its deadlines pass: one that never
// finished stops measuring after processing_after and completes after the
// report wait (P6, E5). A token's client_ip hash goes once its retest window
// is over (K3), and the test with its runs after the retention (R1).
func TestSweepLagDrift(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		settings := controller.DefaultLagSettings()
		defer controller.Testing_SetLagSettings(settings)()
		defer controller.Testing_SetLagDriftBorders(testLagDriftSweepBorders(t))()
		now := server.NowUtc().Truncate(time.Millisecond)
		test := testLagDriftStartedRun(t, ctx, now)

		connect.AssertEqual(t, *sweepLagDrift(ctx, now.Add(10*time.Second), settings), SweepLagDriftResult{})
		connect.AssertEqual(t, model.GetLagDriftRuns(ctx, test.DriftTestId)[0].State, model.LagDriftRunStateStarted)

		result := sweepLagDrift(ctx, now.Add(50*time.Second), settings)
		connect.AssertEqual(t, result.AdvancedTests, 1)
		connect.AssertEqual(t, result.StuckRuns, 0)
		run := model.GetLagDriftRuns(ctx, test.DriftTestId)[0]
		connect.AssertEqual(t, run.State, model.LagDriftRunStateComplete)
		connect.AssertEqual(t, run.Outcome, controller.LagDriftRunOutcomeReportsMissing)
		connect.AssertEqual(t, run.FinishedAt.Equal(now.Add(30*time.Second)), true)
		verdict := &controller.LagDriftResult{}
		connect.AssertEqual(t, json.Unmarshal(model.GetLagDriftTest(ctx, test.DriftTestId).ResultJson, verdict), nil)
		connect.AssertEqual(t, verdict.Verdict, controller.LagDriftVerdictInconclusive)
		connect.AssertEqual(t, verdict.Reason, controller.LagDriftReasonReportsMissing)
		connect.AssertEqual(t, verdict.GeometryVersion, "ne-admin0-10m-test")

		result = sweepLagDrift(ctx, now.Add(15*time.Minute), settings)
		connect.AssertEqual(t, result.RemovedClientIpHashes, 1)
		connect.AssertEqual(t, model.GetLagDriftTest(ctx, test.DriftTestId).ClientIpHash, []byte(nil))

		result = sweepLagDrift(ctx, now.Add(7*24*time.Hour+time.Second), settings)
		connect.AssertEqual(t, result.RemovedTests, 1)
		connect.AssertEqual(t, result.RemovedRuns, 1)
		connect.AssertEqual(t, model.GetLagDriftTest(ctx, test.DriftTestId), (*model.LagDriftTest)(nil))
	})
}

// A taskworker without the borders completes no run (G1), and the runs it
// leaves open past every deadline are reported stuck, for the alert.
func TestSweepLagDriftWithoutBorders(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		settings := controller.DefaultLagSettings()
		defer controller.Testing_SetLagSettings(settings)()
		defer controller.Testing_SetLagDriftBorders(nil)()
		now := server.NowUtc().Truncate(time.Millisecond)
		test := testLagDriftStartedRun(t, ctx, now)

		result := sweepLagDrift(ctx, now.Add(2*time.Minute), settings)
		connect.AssertEqual(t, result.AdvancedTests, 1)
		connect.AssertEqual(t, result.StuckRuns, 1)
		connect.AssertNotEqual(t, model.GetLagDriftRuns(ctx, test.DriftTestId)[0].State, model.LagDriftRunStateComplete)
		connect.AssertEqual(t, model.GetLagDriftTest(ctx, test.DriftTestId).ResultJson, []byte(nil))
	})
}

// The sweep re-arms itself one sweep interval out, once.
func TestSweepLagDriftChain(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		clientSession := session.NewLocalClientSession(ctx, "0.0.0.0:0", nil)
		defer clientSession.Cancel()

		result, err := SweepLagDrift(&SweepLagDriftArgs{}, clientSession)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, *result, SweepLagDriftResult{})

		for range 2 {
			server.Tx(ctx, func(tx server.PgTx) {
				connect.AssertEqual(t, SweepLagDriftPost(&SweepLagDriftArgs{}, result, clientSession, tx), nil)
			})
		}
		var count int
		var runAt time.Time
		server.Db(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(
				ctx,
				`SELECT COUNT(*), MIN(run_at) FROM pending_task WHERE run_once_key = '["sweep_lag_drift"]'`,
			).Scan(&count, &runAt))
		})
		connect.AssertEqual(t, count, 1)
		interval := controller.GetLagSettings().Drift.SweepInterval
		if until := runAt.Sub(server.NowUtc()); until <= 0 || interval < until {
			t.Fatalf("the next sweep is %s out, want within %s", until, interval)
		}
	})
}
