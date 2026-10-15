package controller

import (
	"context"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/connect/protocol"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// Every end reason and family is published every refresh, zero included, so a
// reason's series never goes stale between results; a stored name this
// server does not know folds into "other", published only while it has any.
func TestStatsLagResultValuesPublishEveryReasonAndFamily(t *testing.T) {
	values := statsLagResultValues([]*model.LagResultCount{
		{EndReason: "complete", IpFamily: 4, Count: 5},
		{EndReason: "complete", IpFamily: 6, Count: 2},
		{EndReason: "client_closed", IpFamily: 4, Count: 1},
	})
	got := map[[2]string]float64{}
	for _, value := range values {
		got[[2]string{value.labelValues[0], value.labelValues[1]}] = value.value
	}
	reasons := statsLagEndReasons()
	connect.AssertEqual(t, len(got), 2*len(reasons))
	connect.AssertEqual(t, got[[2]string{"complete", "ipv4"}], float64(5))
	connect.AssertEqual(t, got[[2]string{"complete", "ipv6"}], float64(2))
	connect.AssertEqual(t, got[[2]string{"client_closed", "ipv4"}], float64(1))
	connect.AssertEqual(t, got[[2]string{"busy", "ipv6"}], float64(0))
	if _, ok := got[[2]string{"other", "ipv4"}]; ok {
		t.Fatal("other is published with no unknown reason")
	}

	values = statsLagResultValues([]*model.LagResultCount{
		{EndReason: "9", IpFamily: 6, Count: 3},
	})
	got = map[[2]string]float64{}
	for _, value := range values {
		got[[2]string{value.labelValues[0], value.labelValues[1]}] = value.value
	}
	connect.AssertEqual(t, got[[2]string{"other", "ipv6"}], float64(3))
	connect.AssertEqual(t, got[[2]string{"other", "ipv4"}], float64(0))
}

// The visitors gauge names at most 10 countries, the most visitors first and
// ties by code, and folds the rest and the places with no country into
// "other", so its label stays bounded.
func TestStatsLagVisitorValuesAreBounded(t *testing.T) {
	// au 12, br 11, ca 10 ... nl 1, us 12, and 2 with no country
	counts := map[string]int64{"": 2, "us": 12}
	for index, code := range []string{"au", "br", "ca", "de", "es", "fr", "gb", "in", "jp", "kr", "mx", "nl"} {
		counts[code] = int64(12 - index)
	}
	values := statsLagVisitorValues(counts)

	connect.AssertEqual(t, len(values), statsLagVisitorCountries+1)
	// au and us tie at 12, and au sorts first
	connect.AssertEqual(t, values[0].labelValues, []string{"au"})
	connect.AssertEqual(t, values[1].labelValues, []string{"us"})
	connect.AssertEqual(t, values[2].labelValues, []string{"br"})
	connect.AssertEqual(t, values[9].labelValues, []string{"jp"})
	// kr 3, mx 2 and nl 1 fall outside the top 10, and fold into other with
	// the 2 whose place has no country
	last := values[len(values)-1]
	connect.AssertEqual(t, last.labelValues, []string{"other"})
	connect.AssertEqual(t, last.value, float64(3+2+1+2))

	connect.AssertEqual(t, len(statsLagVisitorValues(map[string]int64{})), 0)
}

func TestStatsRefreshLagExportsGauges(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		now := server.NowUtc()

		model.CreateLagGuestNetwork(ctx, now.Add(-10*time.Minute), now.Add(24*time.Hour))

		trackId := server.NewId()
		model.CreateLagTrack(ctx, &model.LagTrack{
			TrackId:   trackId,
			NetworkId: server.NewId(),
			ClientId:  server.NewId(),
			StartedAt: now.Add(-20 * time.Minute),
			Client:    &model.LagTrackClient{CountryCode: "au"},
		}, []*model.LagTicket{})
		idle := float32(12)
		download := float32(80)
		upload := float32(20)
		connect.AssertEqual(t, model.AddLagResult(ctx, &model.LagResult{
			LagResultId:             server.NewId(),
			TrackId:                 trackId,
			TicketId:                server.NewId(),
			ExtenderId:              server.NewId(),
			ExtenderIpFamily:        6,
			SessionStartTime:        now.Add(-11 * time.Minute),
			WindowStartTime:         now.Add(-11 * time.Minute),
			PingOffsetMs:            []int32{0},
			PingMicros:              []int32{12000},
			IntervalMs:              250,
			DownloadReceivedBytes:   []int64{},
			DownloadSentBytes:       []int64{},
			UploadBytes:             []int64{},
			EndReason:               "complete",
			OutsideLoadLatencyP50Ms: &idle,
			DownloadP50Mbps:         &download,
			UploadP50Mbps:           &upload,
			CreatedAt:               now.Add(-10 * time.Minute),
		}), true)

		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(
				ctx,
				`
				INSERT INTO network_extender_measurement (extender_id, state, reason, request_time, update_time)
				VALUES ($1, $2, $3, $4, $4), ($5, $6, '', $4, $4)
				`,
				server.NewId(),
				model.NetworkExtenderMeasurementStateWithdrawn,
				ExtenderMeasurementOptOutReason,
				now,
				server.NewId(),
				model.NetworkExtenderMeasurementStateIssued,
			))
		})

		statsRefreshLag(ctx, now)

		connect.AssertEqual(t, testutil.ToFloat64(statsLagResults1hGauge.gauge.WithLabelValues("complete", "ipv6")), float64(1))
		connect.AssertEqual(t, testutil.ToFloat64(statsLagResults1hGauge.gauge.WithLabelValues("complete", "ipv4")), float64(0))
		connect.AssertEqual(t, testutil.ToFloat64(statsLagTracks1hGauge.gauge), float64(1))
		connect.AssertEqual(t, testutil.ToFloat64(statsLagGuests1hGauge.gauge), float64(1))
		connect.AssertEqual(t, testutil.ToFloat64(statsLagIdleLatencyP50Gauge.gauge.WithLabelValues("ipv6")), float64(12))
		connect.AssertEqual(t, testutil.ToFloat64(statsLagDownloadP50Gauge.gauge.WithLabelValues("ipv6")), float64(80))
		connect.AssertEqual(t, testutil.ToFloat64(statsLagUploadP50Gauge.gauge.WithLabelValues("ipv6")), float64(20))
		// a family with no complete result in the hour has no series
		connect.AssertEqual(t, testutil.CollectAndCount(statsLagIdleLatencyP50Gauge.gauge, "urnetwork_stats_lag_idle_latency_p50_ms"), 1)
		connect.AssertEqual(t, testutil.ToFloat64(statsLagMeasurementRequestsGauge.gauge.WithLabelValues(model.NetworkExtenderMeasurementStateIssued)), float64(1))
		connect.AssertEqual(t, testutil.ToFloat64(statsLagMeasurementRequestsGauge.gauge.WithLabelValues(model.NetworkExtenderMeasurementStatePending)), float64(0))
		connect.AssertEqual(t, testutil.ToFloat64(statsLagExtendersOptedOutGauge.gauge), float64(1))
		connect.AssertEqual(t, testutil.ToFloat64(statsLagVisitors24hByCountryGauge.gauge.WithLabelValues("au")), float64(1))
		// a test env with no extender network offers none
		connect.AssertEqual(t, testutil.ToFloat64(statsLagExtendersOfferedGauge.gauge), float64(0))
	})
}

// The idle latency gauge reads latency while no load of the track runs, as
// lag.dev shows it (lag.dev/PLAN.md L4, X9): a dot's pings during its
// neighbours' loads are not idle, though its report, stored before theirs,
// can leave out only its own load. This is the review's case of a middle dot
// on a link with bufferbloat, over a whole track: eight dots load back to
// back on the current schedule, each load raises every dot's latency from
// 20 ms to 300 ms, and each dot's minute holds its neighbours' loads.
func TestStatsLagIdleLatencyLeavesOutNeighboursLoads(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		now := server.NowUtc().Truncate(time.Millisecond)
		trackStart := now.Add(-10 * time.Minute)
		// L4: dot k's download starts 5 s + 15 s·k into the track, its upload
		// follows it at once, and each runs 5 s
		loadStart := func(position int) time.Time {
			return trackStart.Add(5*time.Second + time.Duration(position)*15*time.Second)
		}
		const dots = 8
		loaded := func(sent time.Time) bool {
			for position := range dots {
				start := loadStart(position)
				if !sent.Before(start) && sent.Before(start.Add(10*time.Second)) {
					return true
				}
			}
			return false
		}

		track, tickets := newLagStatsTestTrack(ctx, trackStart, dots)
		results := []*model.LagResult{}
		for position, ticket := range tickets {
			// M8: the minute runs from 25 s before the download's first byte
			windowStart := loadStart(position).Add(-25 * time.Second)
			body := &protocol.LagReportBody{
				TicketId:           ticket.TicketId.Bytes(),
				TrackId:            track.TrackId.Bytes(),
				TrackPosition:      uint32(position),
				IpFamily:           4,
				SessionStartTimeMs: uint64(trackStart.UnixMilli()),
				WindowStartTimeMs:  uint64(windowStart.UnixMilli()),
				DownloadStartMs:    25000,
				DownloadMs:         5000,
				UploadStartMs:      30000,
				UploadMs:           5000,
				IntervalMs:         250,
				EndReason:          protocol.LagEndReason_LAG_END_COMPLETE,
			}
			for offsetMs := uint32(0); offsetMs < 60000; offsetMs += 250 {
				micros := uint32(20000)
				if loaded(windowStart.Add(time.Duration(offsetMs) * time.Millisecond)) {
					micros = 300000
				}
				body.PingOffsetMs = append(body.PingOffsetMs, offsetMs)
				body.PingMicros = append(body.PingMicros, micros)
			}
			for range 20 {
				body.DownloadReceivedBytes = append(body.DownloadReceivedBytes, 3125000)
				body.UploadBytes = append(body.UploadBytes, 625000)
			}
			// derived as the report endpoint derives it when the report
			// arrives, its neighbours' reports not yet stored
			result := newLagResult(body, ticket, now)
			results = append(results, result)
			connect.AssertEqual(t, model.AddLagResult(ctx, result), true)
		}
		// the review's middle dot: outside its own load, 30 s of its 50 s are
		// its neighbours' loads, so the latency stored with it is the loaded
		// latency
		connect.AssertEqual(t, *results[3].OutsideLoadLatencyP50Ms, float32(300))

		medians := model.GetLagResultMediansSince(ctx, now.Add(-time.Hour), lagEndReasonName(protocol.LagEndReason_LAG_END_COMPLETE))
		connect.AssertEqual(t, len(medians), 1)
		connect.AssertEqual(t, medians[0].Count, int64(dots))
		connect.AssertEqual(t, *medians[0].IdleLatencyP50Ms, float64(20))

		statsRefreshLag(ctx, now)
		connect.AssertEqual(t, testutil.ToFloat64(statsLagIdleLatencyP50Gauge.gauge.WithLabelValues("ipv4")), float64(20))
	})
}

// A track started at startedAt with one ticket per dot, in order.
func newLagStatsTestTrack(ctx context.Context, startedAt time.Time, dots int) (*model.LagTrack, []*model.LagTicket) {
	track := &model.LagTrack{
		TrackId:   server.NewId(),
		NetworkId: server.NewId(),
		ClientId:  server.NewId(),
		StartedAt: startedAt,
	}
	tickets := []*model.LagTicket{}
	for position := range dots {
		tickets = append(tickets, &model.LagTicket{
			TicketId:   server.NewId(),
			TrackId:    track.TrackId,
			Position:   position,
			ClientId:   track.ClientId,
			NetworkId:  track.NetworkId,
			ExtenderId: server.NewId(),
			IssuedAt:   startedAt,
			ExpiresAt:  startedAt.Add(2 * time.Minute),
		})
	}
	model.CreateLagTrack(ctx, track, tickets)
	return track, tickets
}
