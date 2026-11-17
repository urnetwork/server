package controller

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/connect/lagdrift"
	"github.com/urnetwork/connect/protocol"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// Tests of processing (lag.dev/DRIFTTEST.md E5, V5-V7, V10, T4): reports
// posted as extenders post them (A7), the run's completion, the server's
// check of the stop, and the verdict over the pooled window.

// Where the synthetic visitors are.
var (
	lagDriftTestBerlin = lagdrift.Point{Lat: 52.52, Lon: 13.40}
	// 7 km inside the box's border with Poland: no early stop can be clear
	lagDriftTestBorderTown = lagdrift.Point{Lat: 52.35, Lon: 14.9}
	lagDriftTestMadrid     = lagdrift.Point{Lat: 40.42, Lon: -3.70}
	// beside the far band's nearest extender, which every first run has
	lagDriftTestCairo = lagdrift.Point{Lat: 30.06, Lon: 31.25}
)

// One run's synthetic sessions: a visitor's true place and last mile, how
// long the sessions pinged, and which positions report.
type lagDriftTestSessions struct {
	visitor    lagdrift.Point
	lastMileMs float64
	durationMs int
	// nil for the 12 own positions
	positions []int
	// the positions that pinged only this long, if any
	shortPositions  map[int]int
	ipFamily        uint32
	endReason       protocol.LagEndReason
	sessionStartsAt time.Time
}

// Posts each reporting session's report of a started run, signed by its
// extender, as the extender's reporter does (M8, A7), at `now`. Each round
// trip is the visitor's last mile plus the path's propagation, off the great
// circle by a few percent per path, with a little jitter per ping.
func (self *lagDriftTestFixture) report(t testing.TB, start *LagDriftStartResult, sessions lagDriftTestSessions, now time.Time) {
	t.Helper()
	if sessions.ipFamily == 0 {
		sessions.ipFamily = 4
	}
	if sessions.endReason == protocol.LagEndReason_LAG_END_COMPLETE {
		sessions.endReason = protocol.LagEndReason_LAG_END_NO_LOAD
	}
	positions := sessions.positions
	if positions == nil {
		positions = []int{0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11}
	}
	for _, ticket := range start.Tickets {
		if !slices.Contains(positions, ticket.Position) {
			continue
		}
		extender := self.extenders[ticket.ExtenderId]
		place := self.places[ticket.ExtenderId]
		ticketBody := verifyLagTestTicket(t, ticket.Ticket, self.rootPublicKey, start.expiresAtTime(t).Add(-time.Second))
		body := &protocol.LagReportBody{
			TicketId:           ticketBody.TicketId,
			ExtenderPublicKey:  extender.publicKey,
			TrackId:            ticketBody.TrackId,
			TrackPosition:      ticketBody.TrackPosition,
			IpFamily:           sessions.ipFamily,
			SessionStartTimeMs: uint64(sessions.sessionStartsAt.UnixMilli()),
			WindowStartTimeMs:  uint64(sessions.sessionStartsAt.UnixMilli()),
			IntervalMs:         250,
			EndReason:          sessions.endReason,
		}
		durationMs := sessions.durationMs
		if short, ok := sessions.shortPositions[ticket.Position]; ok {
			durationMs = short
		}
		distanceKm := lagdrift.DistanceKm(sessions.visitor, lagdrift.Point{Lat: place.Latitude, Lon: place.Longitude})
		detour := 1 + 0.02*float64(ticket.Position%5-2)
		for i, offsetMs := 0, 0; offsetMs <= durationMs; i, offsetMs = i+1, offsetMs+250 {
			rttMs := sessions.lastMileMs + lagdrift.MsPerKm*distanceKm*detour + 0.25*float64(i%4)
			body.PingOffsetMs = append(body.PingOffsetMs, uint32(offsetMs))
			body.PingMicros = append(body.PingMicros, uint32(rttMs*1000))
		}
		result := postLagTestReports(t, extender, now, signLagTestReport(t, body, extender.privateKey))
		if result.Accepted != 1 {
			t.Fatalf("a report was refused: %+v", lagTestRejections(result))
		}
	}
}

func (self *LagDriftStartResult) expiresAtTime(t testing.TB) time.Time {
	t.Helper()
	expiresAt, err := time.Parse(time.RFC3339, self.ExpiresAt)
	if err != nil {
		t.Fatal(err)
	}
	return expiresAt
}

// Starts a run from the visitor's address, reports its sessions as given and
// finishes it with stop, the reports first; answers the run's start.
func (self *lagDriftTestFixture) runAt(
	t testing.TB,
	token string,
	clientAddress string,
	sessions lagDriftTestSessions,
	stop string,
	startsAt time.Time,
) *LagDriftStartResult {
	t.Helper()
	start := self.start(t, token, clientAddress, "", startsAt)
	if start.Error != nil {
		t.Fatalf("start refused: %+v", start.Error)
	}
	sessions.sessionStartsAt = startsAt.Add(300 * time.Millisecond)
	closesAt := sessions.sessionStartsAt.Add(time.Duration(sessions.durationMs) * time.Millisecond)
	self.report(t, start, sessions, closesAt.Add(200*time.Millisecond))
	self.finish(t, token, start.Run, stop, closesAt.Add(300*time.Millisecond))
	return start
}

func (self *lagDriftTestFixture) runView(t testing.TB, driftTestId server.Id, run int, now time.Time) *LagDriftRunView {
	t.Helper()
	for _, runView := range self.read(t, driftTestId, now).Runs {
		if runView.Run == run {
			return runView
		}
	}
	t.Fatalf("no run %d", run)
	return nil
}

// A visitor whose pings place them where their address says: every report in
// completes the run at its finish, which counts and agrees, with the
// figures, the model it was judged with, and nothing of the fit's place. The
// widget reads only its fields. A relay's last mile is advisory.
func TestLagDriftProcessAgree(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		fixture := newLagDriftTestFixture(t, ctx)
		now := server.NowUtc().Truncate(time.Millisecond)
		allocated := fixture.allocate(t, now, nil)

		start := fixture.runAt(t, allocated.Token, lagDriftTestVisitorAddress, lagDriftTestSessions{
			visitor:    lagDriftTestBerlin,
			lastMileMs: 5,
			durationMs: 5000,
		}, LagDriftStopConverged, now)

		// complete at the finish, with every report in
		finishedAt := now.Add(5600 * time.Millisecond)
		view := fixture.read(t, allocated.DriftTestId, finishedAt)
		connect.AssertEqual(t, view.State, LagDriftStateActive)
		run := view.Runs[0]
		connect.AssertEqual(t, run.State, model.LagDriftRunStateComplete)
		connect.AssertEqual(t, run.Outcome, LagDriftRunOutcomeCounted)
		connect.AssertEqual(t, *run.Stop, LagDriftStopConverged)
		connect.AssertEqual(t, *run.Answered, 12)
		connect.AssertEqual(t, run.InVerdict, true)
		if *run.MeasuredMs < 5000 || 5200 < *run.MeasuredMs {
			t.Fatalf("measured %d ms", *run.MeasuredMs)
		}
		connect.AssertNotEqual(t, model.GetLagDriftRuns(ctx, allocated.DriftTestId)[0].SupportedStop, "")

		result := view.Result
		connect.AssertEqual(t, result.Verdict, LagDriftVerdictAgree)
		connect.AssertEqual(t, result.Reason, LagDriftReasonInsideIpCountry)
		connect.AssertEqual(t, result.RunsInVerdict, []int{1})
		connect.AssertEqual(t, result.PendingRun, (*int)(nil))
		connect.AssertEqual(t, result.Confidence, 0.99)
		connect.AssertEqual(t, result.GeometryVersion, "ne-admin0-10m-test")
		connect.AssertEqual(t, *result.Ip, LagDriftIp{CountryCode: "de", CountryName: "Germany", Family: 4})
		connect.AssertEqual(t, *result.Drift.CountryCode, "de")
		connect.AssertEqual(t, *result.Drift.DistanceKm, 0.0)
		if *result.Drift.MarginKm >= 0 {
			t.Fatalf("an agreeing margin of %f km", *result.Drift.MarginKm)
		}
		connect.AssertEqual(t, len(result.Drift.LastMileMs), 1)
		if result.Drift.LastMileMs[0] < 3 || 7 < result.Drift.LastMileMs[0] {
			t.Fatalf("last mile %v, want about 5 ms", result.Drift.LastMileMs)
		}
		connect.AssertEqual(t, result.Drift.RelaySuspected, false)
		connect.AssertEqual(t, result.Drift.Extenders, 12)
		if 100 < result.Drift.EllipseKm.Major {
			t.Fatalf("an ellipse of %+v", result.Drift.EllipseKm)
		}
		// the model it was judged with: the defaults, until a calibration
		connect.AssertEqual(t, *result.Calibration, LagDriftCalibrationView{Kappa: 0.25, Sigma0Ms: 1, RelayThresholdMs: 80})

		widget := fixture.widgetResult(t, allocated.Token, finishedAt)
		connect.AssertEqual(t, *widget.Verdict, LagDriftVerdictAgree)
		connect.AssertEqual(t, widget.Run, 1)
		connect.AssertEqual(t, widget.RunState, model.LagDriftRunStateComplete)
		connect.AssertEqual(t, widget.IpCountryName, "Germany")
		connect.AssertEqual(t, widget.RunsInVerdict, []int{1})
		connect.AssertEqual(t, widget.RetestAllowed, true)
		connect.AssertEqual(t, widget.RunsLeft, 5)

		// a late report, a spare's, changes nothing (E5)
		resultAt := model.GetLagDriftTest(ctx, allocated.DriftTestId).ResultAt
		fixture.report(t, start, lagDriftTestSessions{
			visitor:         lagDriftTestMadrid,
			lastMileMs:      5,
			durationMs:      5000,
			positions:       []int{12},
			sessionStartsAt: now.Add(300 * time.Millisecond),
		}, finishedAt.Add(time.Second))
		connect.AssertEqual(t, model.GetLagDriftTest(ctx, allocated.DriftTestId).ResultAt.Equal(*resultAt), true)
		connect.AssertEqual(t, fixture.read(t, allocated.DriftTestId, finishedAt.Add(2*time.Second)).Result.Verdict, LagDriftVerdictAgree)

		// a last mile far above normal suspects a relay, and agrees still
		relayed := fixture.allocate(t, now, nil)
		fixture.runAt(t, relayed.Token, lagDriftTestVisitorAddress, lagDriftTestSessions{
			visitor:    lagDriftTestBerlin,
			lastMileMs: 120,
			durationMs: 5000,
		}, LagDriftStopConverged, now)
		relayedResult := fixture.read(t, relayed.DriftTestId, finishedAt).Result
		connect.AssertEqual(t, relayedResult.Verdict, LagDriftVerdictAgree)
		connect.AssertEqual(t, relayedResult.Drift.RelaySuspected, true)
	})
}

// A visitor whose pings place them far from their address's country: the
// whole region is outside it, so the verdict disagrees, with a positive
// margin, whichever order the finish and the reports come in. The visitor is
// beside one of the run's extenders: with route error a quarter of the
// distance (κ, V3), extenders all far from the visitor leave a region wide
// enough to reach the country, which agrees, as it should.
func TestLagDriftProcessDisagree(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		fixture := newLagDriftTestFixture(t, ctx)
		now := server.NowUtc().Truncate(time.Millisecond)
		allocated := fixture.allocate(t, now, nil)

		// the finish first, then the reports: the last one completes the run
		start := fixture.start(t, allocated.Token, lagDriftTestVisitorAddress, "", now)
		fixture.finish(t, allocated.Token, 1, LagDriftStopClear, now.Add(5300*time.Millisecond))
		connect.AssertEqual(t, fixture.runView(t, allocated.DriftTestId, 1, now.Add(5400*time.Millisecond)).State, model.LagDriftRunStateProcessing)
		fixture.report(t, start, lagDriftTestSessions{
			visitor:         lagDriftTestCairo,
			lastMileMs:      5,
			durationMs:      5000,
			sessionStartsAt: now.Add(300 * time.Millisecond),
		}, now.Add(5500*time.Millisecond))

		view := fixture.read(t, allocated.DriftTestId, now.Add(5600*time.Millisecond))
		connect.AssertEqual(t, view.Runs[0].State, model.LagDriftRunStateComplete)
		connect.AssertEqual(t, view.Runs[0].Outcome, LagDriftRunOutcomeCounted)
		result := view.Result
		connect.AssertEqual(t, result.Verdict, LagDriftVerdictDisagree)
		connect.AssertEqual(t, result.Reason, LagDriftReasonRegionOutside)
		if *result.Drift.MarginKm <= 0 || *result.Drift.DistanceKm < 500 {
			t.Fatalf("a disagreement %f km away with a margin of %f km", *result.Drift.DistanceKm, *result.Drift.MarginKm)
		}
		if result.Drift.CountryCode != nil && *result.Drift.CountryCode == "de" {
			t.Fatal("the pings place a visitor in Cairo in Germany")
		}
		widget := fixture.widgetResult(t, allocated.Token, now.Add(6*time.Second))
		connect.AssertEqual(t, *widget.Verdict, LagDriftVerdictDisagree)
		connect.AssertEqual(t, widget.IpCountryName, "Germany")
	})
}

// The server checks the browser's stop against the reports (V10): a run that
// stopped before any rule held stays out of the window as stopped_early, and
// one that ran to its cap counts.
func TestLagDriftProcessStop(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		fixture := newLagDriftTestFixture(t, ctx)
		now := server.NowUtc().Truncate(time.Millisecond)
		allocated := fixture.allocate(t, now, nil)

		fixture.runAt(t, allocated.Token, lagDriftTestVisitorAddress, lagDriftTestSessions{
			visitor:    lagDriftTestBorderTown,
			lastMileMs: 5,
			durationMs: 3500,
		}, LagDriftStopConverged, now)
		view := fixture.read(t, allocated.DriftTestId, now.Add(5*time.Second))
		connect.AssertEqual(t, view.Runs[0].Outcome, LagDriftRunOutcomeStoppedEarly)
		connect.AssertEqual(t, view.Runs[0].InVerdict, false)
		connect.AssertEqual(t, view.Result.Verdict, LagDriftVerdictInconclusive)
		connect.AssertEqual(t, view.Result.Reason, LagDriftReasonStoppedEarly)
		connect.AssertEqual(t, view.Result.RunsInVerdict, []int{})

		// the cap
		at := now.Add(20 * time.Second)
		fixture.runAt(t, allocated.Token, lagDriftTestVisitorAddress, lagDriftTestSessions{
			visitor:    lagDriftTestBorderTown,
			lastMileMs: 5,
			durationMs: 12000,
		}, LagDriftStopCap, at)
		view = fixture.read(t, allocated.DriftTestId, at.Add(13*time.Second))
		connect.AssertEqual(t, view.Runs[1].Outcome, LagDriftRunOutcomeCounted)
		connect.AssertEqual(t, view.Result.RunsInVerdict, []int{2})
		connect.AssertEqual(t, view.Result.Verdict, LagDriftVerdictAgree)
	})
}

// Runs that stay out of the window (E5, K3, K4): fewer than 6 sessions
// reporting by the report wait is reports_missing, at least 6 reporting with
// fewer than 6 usable is too_few_extenders, sessions mostly on the start's
// other family is ip_family_mismatch, and a start from another address than
// client_ip is ip_mismatch. A run with no finish stops measuring after
// processing_after and completes after the report wait.
func TestLagDriftProcessExcluded(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		fixture := newLagDriftTestFixture(t, ctx)
		now := server.NowUtc().Truncate(time.Millisecond)
		reportWait := fixture.settings.Drift.ReportWait()
		outcome := func(clientAddress string, sessions lagDriftTestSessions) (*LagDriftRunView, *LagDriftResult) {
			allocated := fixture.allocate(t, now, nil)
			fixture.runAt(t, allocated.Token, clientAddress, sessions, LagDriftStopConverged, now)
			// partial reports wait for the report wait
			readAt := now.Add(6*time.Second + reportWait)
			view := fixture.read(t, allocated.DriftTestId, readAt)
			return view.Runs[0], view.Result
		}

		run, result := outcome(lagDriftTestVisitorAddress, lagDriftTestSessions{
			visitor: lagDriftTestBerlin, lastMileMs: 5, durationMs: 5000, positions: []int{0, 1, 2, 3, 4},
		})
		connect.AssertEqual(t, run.Outcome, LagDriftRunOutcomeReportsMissing)
		connect.AssertEqual(t, result.Reason, LagDriftReasonReportsMissing)
		connect.AssertEqual(t, result.Verdict, LagDriftVerdictInconclusive)

		run, result = outcome(lagDriftTestVisitorAddress, lagDriftTestSessions{
			visitor: lagDriftTestBerlin, lastMileMs: 5, durationMs: 5000,
			// 7 of 12 pinged 2 s, under 12 round trips
			shortPositions: map[int]int{0: 2000, 1: 2000, 2: 2000, 3: 2000, 4: 2000, 5: 2000, 6: 2000},
		})
		connect.AssertEqual(t, run.Outcome, LagDriftRunOutcomeTooFewExtenders)
		connect.AssertEqual(t, result.Reason, LagDriftReasonTooFewExtenders)

		run, result = outcome(lagDriftTestVisitorAddress, lagDriftTestSessions{
			visitor: lagDriftTestBerlin, lastMileMs: 5, durationMs: 5000, ipFamily: 6,
		})
		connect.AssertEqual(t, run.Outcome, LagDriftRunOutcomeIpFamilyMismatch)
		connect.AssertEqual(t, result.Reason, LagDriftReasonIpFamilyMismatch)

		run, result = outcome(lagDriftTestNeighborAddress, lagDriftTestSessions{
			visitor: lagDriftTestBerlin, lastMileMs: 5, durationMs: 5000,
		})
		connect.AssertEqual(t, *run.ClientIpMatches, false)
		connect.AssertEqual(t, run.Outcome, LagDriftRunOutcomeIpMismatch)
		connect.AssertEqual(t, result.Reason, LagDriftReasonIpMismatch)

		// the other family counts, and the customer sees null (K3)
		run, result = outcome(lagDriftTestVisitorIpv6, lagDriftTestSessions{
			visitor: lagDriftTestBerlin, lastMileMs: 5, durationMs: 5000, ipFamily: 6,
		})
		connect.AssertEqual(t, run.ClientIpMatches, (*bool)(nil))
		connect.AssertEqual(t, run.Outcome, LagDriftRunOutcomeCounted)
		connect.AssertEqual(t, result.Verdict, LagDriftVerdictAgree)
		connect.AssertEqual(t, result.Ip.Family, 6)

		// no finish and no report
		allocated := fixture.allocate(t, now, nil)
		fixture.start(t, allocated.Token, lagDriftTestVisitorAddress, "", now)
		connect.AssertEqual(t, fixture.runView(t, allocated.DriftTestId, 1, now.Add(29*time.Second)).State, model.LagDriftRunStateStarted)
		connect.AssertEqual(t, fixture.runView(t, allocated.DriftTestId, 1, now.Add(30*time.Second)).State, model.LagDriftRunStateProcessing)
		run = fixture.runView(t, allocated.DriftTestId, 1, now.Add(30*time.Second+reportWait))
		connect.AssertEqual(t, run.State, model.LagDriftRunStateComplete)
		connect.AssertEqual(t, run.Outcome, LagDriftRunOutcomeReportsMissing)
	})
}

// The verdict pools the last 3 runs that count (V7): a fourth drops the
// oldest, b is per run, a run the visitor just finished is pending until its
// reports are in, and a run that doesn't count leaves the window as it was.
func TestLagDriftProcessWindow(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		fixture := newLagDriftTestFixture(t, ctx)
		now := server.NowUtc().Truncate(time.Millisecond)
		allocated := fixture.allocate(t, now, nil)

		at := now
		for run := 1; run <= 4; run += 1 {
			start := fixture.runAt(t, allocated.Token, lagDriftTestVisitorAddress, lagDriftTestSessions{
				visitor:    lagDriftTestBerlin,
				lastMileMs: float64(4 + run),
				durationMs: 5000,
			}, LagDriftStopConverged, at)
			connect.AssertEqual(t, start.Run, run)
			at = at.Add(10 * time.Second)
		}
		view := fixture.read(t, allocated.DriftTestId, at)
		connect.AssertEqual(t, view.Result.RunsInVerdict, []int{2, 3, 4})
		connect.AssertEqual(t, len(view.Result.Drift.LastMileMs), 3)
		connect.AssertEqual(t, view.Runs[0].InVerdict, false)
		connect.AssertEqual(t, view.Runs[3].InVerdict, true)
		// a retest's paths are new where the bands have others (E2): 4 runs
		// of 12 among 21 extenders repeat some
		if view.Result.Drift.Extenders <= 12 {
			t.Fatalf("the window fits %d distinct extenders", view.Result.Drift.Extenders)
		}

		// run 5 finished, its reports not yet in: pending
		fixture.start(t, allocated.Token, lagDriftTestVisitorAddress, "", at)
		fixture.finish(t, allocated.Token, 5, LagDriftStopConverged, at.Add(5*time.Second))
		view = fixture.read(t, allocated.DriftTestId, at.Add(6*time.Second))
		connect.AssertEqual(t, *view.Result.PendingRun, 5)
		connect.AssertEqual(t, view.Result.RunsInVerdict, []int{2, 3, 4})
		connect.AssertEqual(t, view.State, LagDriftStateActive)
		widget := fixture.widgetResult(t, allocated.Token, at.Add(6*time.Second))
		connect.AssertEqual(t, widget.Run, 5)
		connect.AssertEqual(t, widget.RunState, model.LagDriftRunStateProcessing)
		connect.AssertEqual(t, *widget.Verdict, LagDriftVerdictAgree)

		// it never reports: it completes after the report wait and stays out
		view = fixture.read(t, allocated.DriftTestId, at.Add(5*time.Second+fixture.settings.Drift.ReportWait()))
		connect.AssertEqual(t, view.Runs[4].Outcome, LagDriftRunOutcomeReportsMissing)
		connect.AssertEqual(t, view.Result.PendingRun, (*int)(nil))
		connect.AssertEqual(t, view.Result.RunsInVerdict, []int{2, 3, 4})
		connect.AssertEqual(t, view.Result.Verdict, LagDriftVerdictAgree)

		// after the retest window, closed
		connect.AssertEqual(t, fixture.read(t, allocated.DriftTestId, now.Add(15*time.Minute)).State, LagDriftStateClosed)
		closed := fixture.widgetResult(t, allocated.Token, now.Add(15*time.Minute))
		connect.AssertEqual(t, closed.RetestAllowed, false)
		connect.AssertEqual(t, closed.RunsLeft, 0)
	})
}

// A report of a demo session, which an extender that predates NoReport still
// sends (E4, W8), is dropped without a row and counted.
func TestLagDriftDemoReportDropped(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		fixture := newLagDriftTestFixture(t, ctx)
		now := server.NowUtc().Truncate(time.Millisecond)
		start := fixture.start(t, LagDriftDemoToken, lagDriftTestVisitorAddress, "", now)
		connect.AssertEqual(t, start.Error, (*LagError)(nil))
		before := lagTestTableText(t, ctx, "lag_result")

		ticket := start.Tickets[0]
		extender := fixture.extenders[ticket.ExtenderId]
		ticketBody := verifyLagTestTicket(t, ticket.Ticket, fixture.rootPublicKey, now)
		connect.AssertEqual(t, ticketBody.NoReport, true)
		body := &protocol.LagReportBody{
			TicketId:           ticketBody.TicketId,
			ExtenderPublicKey:  extender.publicKey,
			TrackId:            ticketBody.TrackId,
			TrackPosition:      ticketBody.TrackPosition,
			IpFamily:           4,
			SessionStartTimeMs: uint64(now.UnixMilli()),
			WindowStartTimeMs:  uint64(now.UnixMilli()),
			IntervalMs:         250,
			PingOffsetMs:       []uint32{0, 250, 500, 750},
			PingMicros:         []uint32{9000, 9100, 9200, 9300},
			EndReason:          protocol.LagEndReason_LAG_END_NO_LOAD,
		}
		result := postLagTestReports(t, extender, now.Add(2*time.Second), signLagTestReport(t, body, extender.privateKey))
		assertLagReportRejected(t, result, LagReportRejectTicket)
		connect.AssertEqual(t, lagTestTableText(t, ctx, "lag_result"), before)
		connect.AssertEqual(t, lagDriftDemoReport(server.RequireIdFromBytes(ticketBody.TicketId), server.NewId()), false)
	})
}

// A process without the borders completes no run (G1): the runs wait, open,
// for one that has them, rather than complete as judged against nothing.
func TestLagDriftProcessWithoutBorders(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		fixture := newLagDriftTestFixture(t, ctx)
		now := server.NowUtc().Truncate(time.Millisecond)
		allocated := fixture.allocate(t, now, nil)
		start := fixture.start(t, allocated.Token, lagDriftTestVisitorAddress, "", now)

		restore := Testing_SetLagDriftBorders(nil)
		fixture.report(t, start, lagDriftTestSessions{
			visitor: lagDriftTestBerlin, lastMileMs: 5, durationMs: 5000, sessionStartsAt: now.Add(300 * time.Millisecond),
		}, now.Add(5500*time.Millisecond))
		fixture.finish(t, allocated.Token, 1, LagDriftStopConverged, now.Add(5600*time.Millisecond))
		later := now.Add(time.Minute)
		run := fixture.runView(t, allocated.DriftTestId, 1, later)
		connect.AssertEqual(t, run.State, model.LagDriftRunStateProcessing)
		connect.AssertEqual(t, model.CountStuckLagDriftRuns(ctx, later), 1)

		// with the borders back, the next look completes it
		restore()
		connect.AssertEqual(t, fixture.runView(t, allocated.DriftTestId, 1, later).State, model.LagDriftRunStateComplete)
		connect.AssertEqual(t, model.CountStuckLagDriftRuns(ctx, later), 0)
	})
}

// Verdicts use the calibration in force and record it (V4), unless an
// operator switched calibration off (lag.yml drift.calibration_enabled), when
// they use the design's defaults and record no calibration.
func TestLagDriftCalibrationSwitch(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		now := server.NowUtc().Truncate(time.Second)
		calibrationId := model.AddLagDriftCalibration(ctx, now, now.Add(-7*24*time.Hour), now, &model.LagDriftCalibrationResult{
			Kappa:            0.3,
			Sigma0Ms:         1.5,
			RelayThresholdMs: 60,
			FitTracks:        2000,
			FitObservations:  20000,
		})
		drift := DefaultLagDriftSettings()

		inForce := lagDriftCalibrationFor(ctx, drift)
		connect.AssertEqual(t, *inForce.CalibrationId, calibrationId)
		connect.AssertEqual(t, inForce.Kappa, 0.3)
		recorded := newLagDriftCalibrationView(inForce, inForce.RelayThresholdMs)
		connect.AssertEqual(t, *recorded.CalibrationId, calibrationId)
		connect.AssertEqual(t, recorded.Sigma0Ms, 1.5)

		off := false
		drift.CalibrationEnabled = &off
		defaults := lagDriftCalibrationFor(ctx, drift)
		connect.AssertEqual(t, defaults.CalibrationId, (*server.Id)(nil))
		connect.AssertEqual(t, defaults.Kappa, 0.25)
		connect.AssertEqual(t, defaults.Sigma0Ms, 1.0)
		connect.AssertEqual(t, defaults.RelayThresholdMs, 80.0)
		recorded = newLagDriftCalibrationView(defaults, defaults.RelayThresholdMs)
		connect.AssertEqual(t, recorded.CalibrationId, (*server.Id)(nil))
		connect.AssertEqual(t, recorded.CalibratedAt, (*string)(nil))
	})
}
