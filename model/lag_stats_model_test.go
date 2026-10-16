package model

import (
	"context"
	"testing"
	"time"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/server"
)

// A stored result for the stats tests: no load, and one ping whose round trip
// is the given idle latency, lost when there is none; the given end reason,
// family and speeds; stored at createdAt under a track that exists.
func testLagStatsResult(
	trackId server.Id,
	position int,
	ipFamily int,
	endReason string,
	createdAt time.Time,
	idle *float32,
	download *float32,
	upload *float32,
) *LagResult {
	pingMicros := int32(0)
	if idle != nil {
		pingMicros = int32(*idle * 1000)
	}
	return &LagResult{
		LagResultId:             server.NewId(),
		TrackId:                 trackId,
		TrackPosition:           position,
		TicketId:                server.NewId(),
		ExtenderId:              server.NewId(),
		ExtenderIpFamily:        ipFamily,
		SessionStartTime:        createdAt.Add(-time.Minute),
		WindowStartTime:         createdAt.Add(-time.Minute),
		PingOffsetMs:            []int32{0},
		PingMicros:              []int32{pingMicros},
		IntervalMs:              250,
		DownloadReceivedBytes:   []int64{},
		DownloadSentBytes:       []int64{},
		UploadBytes:             []int64{},
		EndReason:               endReason,
		OutsideLoadLatencyP50Ms: idle,
		DownloadP50Mbps:         download,
		UploadP50Mbps:           upload,
		CreatedAt:               createdAt,
	}
}

func testLagStatsFloat(value float32) *float32 {
	return &value
}

// A track started at startedAt by the guest network, placed in countryCode
// ("" for a place with no country).
func testLagStatsTrack(ctx context.Context, networkId server.Id, startedAt time.Time, countryCode string) server.Id {
	trackId := server.NewId()
	CreateLagTrack(ctx, &LagTrack{
		TrackId:   trackId,
		NetworkId: networkId,
		ClientId:  server.NewId(),
		StartedAt: startedAt,
		Client:    &LagTrackClient{CountryCode: countryCode},
	}, []*LagTicket{})
	return trackId
}

func TestLagStatsCountsAndMedians(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		now := server.NowUtc()

		guestA := server.NewId()
		guestB := server.NewId()
		trackA := testLagStatsTrack(ctx, guestA, now.Add(-30*time.Minute), "au")
		// a second track by the same visitor in the same country is one visitor
		testLagStatsTrack(ctx, guestA, now.Add(-20*time.Minute), "au")
		trackB := testLagStatsTrack(ctx, guestB, now.Add(-10*time.Minute), "jp")
		// a place with no country, and a track older than the hour
		testLagStatsTrack(ctx, server.NewId(), now.Add(-10*time.Minute), "")
		old := testLagStatsTrack(ctx, server.NewId(), now.Add(-2*time.Hour), "au")

		for _, result := range []*LagResult{
			testLagStatsResult(trackA, 0, 4, "complete", now.Add(-25*time.Minute), testLagStatsFloat(10), testLagStatsFloat(100), testLagStatsFloat(40)),
			testLagStatsResult(trackA, 1, 4, "complete", now.Add(-24*time.Minute), testLagStatsFloat(20), testLagStatsFloat(300), nil),
			testLagStatsResult(trackB, 0, 6, "complete", now.Add(-9*time.Minute), testLagStatsFloat(30), testLagStatsFloat(50), testLagStatsFloat(5)),
			// a load given up stores no speeds and never counts in the medians
			testLagStatsResult(trackB, 1, 6, "client_closed", now.Add(-8*time.Minute), testLagStatsFloat(500), nil, nil),
			// outside the hour
			testLagStatsResult(old, 0, 4, "complete", now.Add(-110*time.Minute), testLagStatsFloat(1), testLagStatsFloat(1), testLagStatsFloat(1)),
		} {
			connect.AssertEqual(t, AddLagResult(ctx, result), true)
		}

		counts := map[[2]any]int64{}
		for _, count := range CountLagResultsSince(ctx, now.Add(-time.Hour)) {
			counts[[2]any{count.EndReason, count.IpFamily}] = count.Count
		}
		connect.AssertEqual(t, counts, map[[2]any]int64{
			{"complete", 4}:      2,
			{"complete", 6}:      1,
			{"client_closed", 6}: 1,
		})

		medians := GetLagResultMediansSince(ctx, now.Add(-time.Hour), "complete")
		connect.AssertEqual(t, len(medians), 2)
		ipv4 := medians[0]
		connect.AssertEqual(t, ipv4.IpFamily, 4)
		connect.AssertEqual(t, ipv4.Count, int64(2))
		connect.AssertEqual(t, *ipv4.IdleLatencyP50Ms, float64(15))
		connect.AssertEqual(t, *ipv4.DownloadP50Mbps, float64(200))
		// a result with no upload figure is left out of the upload median
		connect.AssertEqual(t, *ipv4.UploadP50Mbps, float64(40))
		ipv6 := medians[1]
		connect.AssertEqual(t, ipv6.IpFamily, 6)
		connect.AssertEqual(t, ipv6.Count, int64(1))
		connect.AssertEqual(t, *ipv6.IdleLatencyP50Ms, float64(30))

		// an end reason with no result in the window has no family at all
		connect.AssertEqual(t, len(GetLagResultMediansSince(ctx, now.Add(-time.Hour), "busy")), 0)

		connect.AssertEqual(t, CountLagTracksSince(ctx, now.Add(-time.Hour)), int64(4))
		connect.AssertEqual(t, CountLagVisitorsByCountrySince(ctx, now.Add(-time.Hour)), map[string]int64{
			"au": 1,
			"jp": 1,
			"":   1,
		})
	})
}

func TestLagStatsGuests(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		now := server.NowUtc()

		CreateLagGuestNetwork(ctx, now.Add(-10*time.Minute), now.Add(24*time.Hour))
		CreateLagGuestNetwork(ctx, now.Add(-50*time.Minute), now.Add(24*time.Hour))
		CreateLagGuestNetwork(ctx, now.Add(-3*time.Hour), now.Add(21*time.Hour))

		connect.AssertEqual(t, CountLagGuestsSince(ctx, now.Add(-time.Hour)), int64(2))
		connect.AssertEqual(t, CountLagGuestsSince(ctx, now.Add(-4*time.Hour)), int64(3))
	})
}

func TestLagStatsMeasurementStates(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		now := server.NowUtc()
		optOut := "the extender's owner opted out of measurement"

		insert := func(state string, reason string) {
			server.Tx(ctx, func(tx server.PgTx) {
				server.RaisePgResult(tx.Exec(
					ctx,
					`
					INSERT INTO network_extender_measurement (
						extender_id,
						state,
						reason,
						request_time,
						update_time
					)
					VALUES ($1, $2, $3, $4, $4)
					`,
					server.NewId(),
					state,
					reason,
					now,
				))
			})
		}
		insert(NetworkExtenderMeasurementStatePending, "")
		insert(NetworkExtenderMeasurementStateIssued, "")
		insert(NetworkExtenderMeasurementStateIssued, "")
		insert(NetworkExtenderMeasurementStateRefused, "the request named the wrong key")
		insert(NetworkExtenderMeasurementStateWithdrawn, optOut)
		insert(NetworkExtenderMeasurementStateWithdrawn, "the extender is not active")

		connect.AssertEqual(t, CountNetworkExtenderMeasurementsByState(ctx), map[string]int64{
			NetworkExtenderMeasurementStatePending:   1,
			NetworkExtenderMeasurementStateIssued:    2,
			NetworkExtenderMeasurementStateRefused:   1,
			NetworkExtenderMeasurementStateWithdrawn: 2,
		})
		connect.AssertEqual(t, CountNetworkExtenderMeasurementsWithdrawnFor(ctx, optOut), int64(1))
	})
}

// Automated tracks (amendment 12) count in none of the visitor figures: not
// their results, the medians, the tracks, their guests or their countries.
// The automated count holds them, by reason.
func TestLagStatsLeaveOutAutomated(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		now := server.NowUtc()

		addTrack := func(reason string, countryCode string) server.Id {
			guest := CreateLagGuestNetwork(ctx, now.Add(-40*time.Minute), now.Add(24*time.Hour))
			trackId := server.NewId()
			CreateLagTrack(ctx, &LagTrack{
				TrackId:         trackId,
				NetworkId:       guest.NetworkId,
				ClientId:        server.NewId(),
				StartedAt:       now.Add(-30 * time.Minute),
				Client:          &LagTrackClient{CountryCode: countryCode},
				AutomatedReason: reason,
			}, []*LagTicket{})
			return trackId
		}
		visitor := addTrack("", "au")
		cloud := addTrack(LagAutomatedReasonDatacenterAsn, "us")
		agent := addTrack(LagAutomatedReasonSignatureAgent, "us")
		for _, result := range []*LagResult{
			testLagStatsResult(visitor, 0, 4, "complete", now.Add(-20*time.Minute), testLagStatsFloat(10), testLagStatsFloat(100), testLagStatsFloat(40)),
			testLagStatsResult(cloud, 0, 4, "complete", now.Add(-20*time.Minute), testLagStatsFloat(1), testLagStatsFloat(5000), testLagStatsFloat(5000)),
			testLagStatsResult(cloud, 1, 4, "complete", now.Add(-19*time.Minute), testLagStatsFloat(1), testLagStatsFloat(5000), testLagStatsFloat(5000)),
			testLagStatsResult(agent, 0, 6, "complete", now.Add(-18*time.Minute), testLagStatsFloat(2), testLagStatsFloat(900), testLagStatsFloat(900)),
		} {
			connect.AssertEqual(t, AddLagResult(ctx, result), true)
		}

		counts := map[[2]any]int64{}
		for _, count := range CountLagResultsSince(ctx, now.Add(-time.Hour)) {
			counts[[2]any{count.EndReason, count.IpFamily}] = count.Count
		}
		connect.AssertEqual(t, counts, map[[2]any]int64{{"complete", 4}: 1})

		medians := GetLagResultMediansSince(ctx, now.Add(-time.Hour), "complete")
		connect.AssertEqual(t, len(medians), 1)
		connect.AssertEqual(t, medians[0].IpFamily, 4)
		connect.AssertEqual(t, medians[0].Count, int64(1))
		connect.AssertEqual(t, *medians[0].DownloadP50Mbps, float64(100))

		connect.AssertEqual(t, CountLagTracksSince(ctx, now.Add(-time.Hour)), int64(1))
		connect.AssertEqual(t, CountLagGuestsSince(ctx, now.Add(-time.Hour)), int64(1))
		connect.AssertEqual(t, CountLagVisitorsByCountrySince(ctx, now.Add(-time.Hour)), map[string]int64{"au": 1})
		connect.AssertEqual(t, CountLagAutomatedResultsSince(ctx, now.Add(-time.Hour)), map[string]int64{
			LagAutomatedReasonDatacenterAsn:  2,
			LagAutomatedReasonSignatureAgent: 1,
		})

		track := GetLagTrack(ctx, cloud)
		connect.AssertEqual(t, track.AutomatedReason, LagAutomatedReasonDatacenterAsn)
		connect.AssertEqual(t, GetLagTrack(ctx, visitor).Automated(), false)
		// a track stored with no source is web's
		connect.AssertEqual(t, GetLagTrack(ctx, visitor).Source, LagSourceWeb)
	})
}

// A load's span runs LagLoadSpanTail past its upload window (lag.dev/PLAN.md
// L4, X9, amendment 18): on a slow uplink the path stays loaded while what
// the browser queued drains, past the extender's 2 s close wait, so a ping
// sent then is not idle. On lag.dev's local stack a 0.5 Mbps path's pings
// sent 2.06–2.11 s after the extender's upload window still met the load, at
// 6–9 times idle, and its path was empty about 2.3 s after it. A ping sent
// once the span is over is idle again.
func TestLagStatsIdleLeavesOutUploadDrain(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		now := server.NowUtc()

		trackId := testLagStatsTrack(ctx, server.NewId(), now.Add(-20*time.Minute), "us")
		result := testLagStatsResult(trackId, 0, 4, "complete", now.Add(-10*time.Minute), testLagStatsFloat(20), testLagStatsFloat(1), testLagStatsFloat(0.5))
		// M8: the minute runs from 25 s before the download's first byte; the
		// upload window ends at 35.5 s
		result.DownloadStartMs = 25000
		result.DownloadMs = 5000
		result.UploadStartMs = 30500
		result.UploadMs = 5000
		uploadEndMs := int32(result.UploadStartMs + result.UploadMs)
		result.PingOffsetMs = []int32{
			// idle, before the load
			1000,
			// the drain, 2.05–2.25 s after the upload window
			uploadEndMs + 2050,
			uploadEndMs + 2150,
			uploadEndMs + 2250,
			// idle again once the span is over
			uploadEndMs + int32(LagLoadSpanTail.Milliseconds()) + 100,
		}
		result.PingMicros = []int32{20000, 150000, 150000, 150000, 20000}
		connect.AssertEqual(t, AddLagResult(ctx, result), true)

		medians := GetLagResultMediansSince(ctx, now.Add(-time.Hour), "complete")
		connect.AssertEqual(t, len(medians), 1)
		connect.AssertEqual(t, medians[0].Count, int64(1))
		// with a 2 s tail the three drain pings would be the median
		connect.AssertEqual(t, *medians[0].IdleLatencyP50Ms, float64(20))
	})
}
