package model

import (
	"context"
	"net/netip"
	"testing"
	"time"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/server"
)

// Tests of the drift test's storage (lag.dev/DRIFTTEST.md P, E1, K3, R1).

// A drift test for the visitor's /29, allocated at `now`.
func testLagDriftTest(ctx context.Context, now time.Time) *LagDriftTest {
	clientIpHash := server.ClientIpHashForAddr(netip.MustParseAddr("198.51.100.30"))
	test := &LagDriftTest{
		DriftTestId:    server.NewId(),
		NetworkId:      server.NewId(),
		TokenHash:      server.NewId().Bytes(),
		ClientIpHash:   clientIpHash[:],
		ClientIpFamily: 4,
		Confidence:     0.99,
		CreatedAt:      now,
		ExpiresAt:      now.Add(5 * time.Minute),
	}
	CreateLagDriftTest(ctx, test)
	return test
}

// The next run of a drift test, started from addr at `now` with one ticket.
func testLagDriftRun(ctx context.Context, test *LagDriftTest, addr string, now time.Time) *AddLagDriftRunResult {
	trackId := server.NewId()
	extenderId := server.NewId()
	return AddLagDriftRun(ctx, &AddLagDriftRunArgs{
		DriftTestId:   test.DriftTestId,
		StartIpHash:   server.ClientIpHashForAddr(netip.MustParseAddr(addr)),
		StartIpFamily: server.IpVersionForAddr(netip.MustParseAddr(addr)),
		Track: &LagTrack{
			TrackId:   trackId,
			NetworkId: test.NetworkId,
			ClientId:  test.DriftTestId,
			StartedAt: now,
			Client:    &LagTrackClient{CountryCode: "de"},
		},
		Tickets: []*LagTicket{{
			TicketId:   server.NewId(),
			TrackId:    trackId,
			Position:   0,
			ClientId:   test.DriftTestId,
			NetworkId:  test.NetworkId,
			ExtenderId: extenderId,
			IssuedAt:   now,
			ExpiresAt:  now.Add(time.Minute),
		}},
		IpCountryCode:   "de",
		IpCountryName:   "Germany",
		Extenders:       []*LagDriftRunExtender{{Position: 0, ExtenderId: extenderId, CountryCode: "de", Latitude: 52.5, Longitude: 13.4, PlaceErrorKm: 20}},
		Now:             now,
		MaxRuns:         6,
		RetestWindow:    15 * time.Minute,
		ProcessingAfter: 30 * time.Second,
	})
}

// Runs are numbered, one at a time and at most 6, bound to client_ip by its
// /29's hash within a family (K3), as lag tracks of kind drift (E1); the last
// one deletes the hash.
func TestLagDriftRuns(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		now := server.NowUtc().Truncate(time.Millisecond)
		test := testLagDriftTest(ctx, now)

		added := testLagDriftRun(ctx, test, "198.51.100.31", now)
		connect.AssertEqual(t, added.Refusal, "")
		connect.AssertEqual(t, added.Run, 1)
		connect.AssertEqual(t, added.RunsLeft, 5)
		connect.AssertEqual(t, *added.ClientIpMatches, true)
		connect.AssertEqual(t, added.RetestUntil.Equal(now.Add(15*time.Minute)), true)
		connect.AssertEqual(t, testLagDriftRun(ctx, test, "198.51.100.31", now.Add(time.Second)).Refusal, LagDriftRefusalBusy)

		runs := GetLagDriftRuns(ctx, test.DriftTestId)
		connect.AssertEqual(t, len(runs), 1)
		track := GetLagTrack(ctx, runs[0].TrackId)
		connect.AssertEqual(t, track.ClientId, test.DriftTestId)
		connect.AssertEqual(t, GetLagDriftRunByTrack(ctx, runs[0].TrackId).Run, 1)
		connect.AssertEqual(t, GetLagDriftRunByTrack(ctx, server.NewId()), (*LagDriftRun)(nil))

		// finish moves it to processing once, with the first stop
		finished := FinishLagDriftRun(ctx, test.DriftTestId, 1, "converged", now.Add(4*time.Second))
		connect.AssertEqual(t, finished.State, LagDriftRunStateProcessing)
		connect.AssertEqual(t, finished.Stop, "converged")
		finished = FinishLagDriftRun(ctx, test.DriftTestId, 1, "cap", now.Add(5*time.Second))
		connect.AssertEqual(t, finished.Stop, "converged")
		connect.AssertEqual(t, finished.FinishedAt.Equal(now.Add(4*time.Second)), true)
		connect.AssertEqual(t, FinishLagDriftRun(ctx, test.DriftTestId, 2, "cap", now), (*LagDriftRun)(nil))

		// another /29, and the other family
		connect.AssertEqual(t, *testLagDriftRun(ctx, test, "198.51.100.99", now.Add(5*time.Second)).ClientIpMatches, false)
		FinishLagDriftRun(ctx, test.DriftTestId, 2, "cap", now.Add(6*time.Second))
		connect.AssertEqual(t, testLagDriftRun(ctx, test, "2001:db8::30", now.Add(7*time.Second)).ClientIpMatches, (*bool)(nil))
		for run := 3; run <= 6; run += 1 {
			FinishLagDriftRun(ctx, test.DriftTestId, run, "cap", now.Add(time.Duration(5+run)*time.Second))
			if run < 6 {
				connect.AssertEqual(t, testLagDriftRun(ctx, test, "198.51.100.31", now.Add(time.Duration(6+run)*time.Second)).Run, run+1)
			}
		}
		connect.AssertEqual(t, GetLagDriftTest(ctx, test.DriftTestId).RunCount, 6)
		connect.AssertEqual(t, GetLagDriftTest(ctx, test.DriftTestId).ClientIpHash, []byte(nil))
		connect.AssertEqual(t, testLagDriftRun(ctx, test, "198.51.100.31", now.Add(time.Minute)).Refusal, LagDriftRefusalClosed)

		// past its expiry with no run, expired; revoked, expired too
		expired := testLagDriftTest(ctx, now)
		connect.AssertEqual(t, testLagDriftRun(ctx, expired, "198.51.100.31", now.Add(5*time.Minute)).Refusal, LagDriftRefusalExpired)
		revoked := testLagDriftTest(ctx, now)
		connect.AssertEqual(t, RevokeLagDriftTest(ctx, revoked.DriftTestId, now), true)
		connect.AssertEqual(t, testLagDriftRun(ctx, revoked, "198.51.100.31", now).Refusal, LagDriftRefusalExpired)
	})
}

// A drift test's run never counts in lag.dev's stats (E1).
func TestLagDriftTracksLeaveTheStats(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		now := server.NowUtc()
		since := now.Add(-time.Hour)
		_, tickets := testLagTrack(ctx, now, 1)
		AddLagResult(ctx, testLagResult(tickets[0], now))
		test := testLagDriftTest(ctx, now)
		testLagDriftRun(ctx, test, "198.51.100.31", now)
		driftTrackId := GetLagDriftRuns(ctx, test.DriftTestId)[0].TrackId
		driftTicket := &LagTicket{TicketId: server.NewId(), TrackId: driftTrackId, ExtenderId: server.NewId()}
		AddLagResult(ctx, testLagResult(driftTicket, now))

		connect.AssertEqual(t, CountLagTracksSince(ctx, since), int64(1))
		total := int64(0)
		for _, count := range CountLagResultsSince(ctx, since) {
			total += count.Count
		}
		connect.AssertEqual(t, total, int64(1))
		visitors := int64(0)
		for _, count := range CountLagVisitorsByCountrySince(ctx, since) {
			visitors += count
		}
		connect.AssertEqual(t, visitors, int64(1))
	})
}

// The sweep's reads (E5, K3, R1): the tests with a run due, the hashes of
// tokens that can start no run, retention, and the runs left open.
func TestLagDriftSweepReads(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		now := server.NowUtc().Truncate(time.Millisecond)

		started := testLagDriftTest(ctx, now)
		testLagDriftRun(ctx, started, "198.51.100.31", now)
		processing := testLagDriftTest(ctx, now)
		testLagDriftRun(ctx, processing, "198.51.100.31", now)
		FinishLagDriftRun(ctx, processing.DriftTestId, 1, "converged", now.Add(5*time.Second))
		connect.AssertEqual(t, len(GetDueLagDriftTests(ctx, now.Add(-time.Second), now, 10)), 0)
		connect.AssertEqual(t, GetDueLagDriftTests(ctx, now, now, 10), []server.Id{started.DriftTestId})
		connect.AssertEqual(t, len(GetDueLagDriftTests(ctx, now, now.Add(5*time.Second), 10)), 2)
		connect.AssertEqual(t, CountStuckLagDriftRuns(ctx, now.Add(time.Second)), 2)

		// hashes: a token never started past its expiry, one past its retest
		// window, one revoked
		neverStarted := testLagDriftTest(ctx, now)
		revoked := testLagDriftTest(ctx, now)
		connect.AssertEqual(t, RemoveClosedLagDriftClientIpHashes(ctx, now, 6), 0)
		RevokeLagDriftTest(ctx, revoked.DriftTestId, now)
		connect.AssertEqual(t, GetLagDriftTest(ctx, revoked.DriftTestId).ClientIpHash, []byte(nil))
		connect.AssertEqual(t, RemoveClosedLagDriftClientIpHashes(ctx, now.Add(5*time.Minute), 6), 1)
		connect.AssertEqual(t, GetLagDriftTest(ctx, neverStarted.DriftTestId).ClientIpHash, []byte(nil))
		connect.AssertEqual(t, RemoveClosedLagDriftClientIpHashes(ctx, now.Add(15*time.Minute), 6), 2)

		// retention: tests and their runs created before the cut
		removed := RemoveExpiredLagDriftTests(ctx, now.Add(time.Second))
		connect.AssertEqual(t, removed, RemoveExpiredLagDriftTestsResult{Tests: 4, Runs: 2})
		connect.AssertEqual(t, GetLagDriftTest(ctx, started.DriftTestId), (*LagDriftTest)(nil))
		connect.AssertEqual(t, len(GetLagDriftRuns(ctx, started.DriftTestId)), 0)
	})
}

// A network's drift settings (P3): origins set and cleared, and the switch,
// apart from each other.
func TestLagDriftNetworkSettings(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		networkId := server.NewId()
		Testing_CreateNetwork(ctx, networkId, "drift-settings", server.NewId())

		connect.AssertEqual(t, *GetLagDriftNetwork(ctx, networkId), LagDriftNetwork{})
		connect.AssertEqual(t, SetLagDriftNetworkOrigins(ctx, networkId, []string{"https://example.com"}), nil)
		connect.AssertEqual(t, SetLagDriftNetworkDisabled(ctx, networkId, true), nil)
		connect.AssertEqual(t, *GetLagDriftNetwork(ctx, networkId), LagDriftNetwork{ParentOrigins: []string{"https://example.com"}, Disabled: true})
		connect.AssertEqual(t, SetLagDriftNetworkOrigins(ctx, networkId, nil), nil)
		connect.AssertEqual(t, *GetLagDriftNetwork(ctx, networkId), LagDriftNetwork{Disabled: true})
		connect.AssertEqual(t, SetLagDriftNetworkDisabled(ctx, networkId, false), nil)
		connect.AssertEqual(t, *GetLagDriftNetwork(ctx, networkId), LagDriftNetwork{})
		connect.AssertEqual(t, SetLagDriftNetworkOrigins(ctx, server.NewId(), nil), ErrNetworkNotFound)
	})
}

// An extender's place error is the accuracy radius of the address its place
// came from, held to [10, 100] km, and 50 km for a derived place (V3). Pure,
// with GeoLite2 replaced.
func TestLagDriftExtenderPlaceErrors(t *testing.T) {
	restore := Testing_ReplaceLagAddressPlaces(func(ip netip.Addr) *LagAddressPlace {
		switch ip.String() {
		case "192.0.2.1":
			return &LagAddressPlace{Place: &LagPlace{CountryCode: "de", Latitude: 52.5, Longitude: 13.4}, AccuracyRadiusKm: 5}
		case "192.0.2.2":
			return &LagAddressPlace{Place: &LagPlace{CountryCode: "de", Latitude: 50.1, Longitude: 8.7}, AccuracyRadiusKm: 500}
		case "192.0.2.3":
			return &LagAddressPlace{Place: &LagPlace{CountryCode: "fr", Latitude: 48.9, Longitude: 2.4}, AccuracyRadiusKm: 30}
		}
		return nil
	})
	defer restore()
	placeErrors := GetLagDriftExtenderPlaceErrorsKm([]*LagDriftPlacedExtender{
		{Place: &LagPlace{Latitude: 52.5, Longitude: 13.4}, Ips: []netip.Addr{netip.MustParseAddr("192.0.2.1")}},
		{Place: &LagPlace{Latitude: 50.1, Longitude: 8.7}, Ips: []netip.Addr{netip.MustParseAddr("192.0.2.2")}},
		{Place: &LagPlace{Latitude: 48.9, Longitude: 2.4}, Ips: []netip.Addr{netip.MustParseAddr("::ffff:192.0.2.3")}},
		// moved to a derived place
		{Place: &LagPlace{Latitude: 48.0, Longitude: 2.0}, Ips: []netip.Addr{netip.MustParseAddr("192.0.2.3")}},
	})
	connect.AssertEqual(t, placeErrors, []float64{10, 100, 30, 50})
}
