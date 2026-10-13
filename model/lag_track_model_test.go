package model

import (
	"context"
	"testing"
	"time"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/server"
)

// Tests of the lag.dev tracks, tickets and results (lag.dev/PLAN.md A5-A7).

func testLagTrack(ctx context.Context, startedAt time.Time, positions int) (*LagTrack, []*LagTicket) {
	track := &LagTrack{
		TrackId:   server.NewId(),
		NetworkId: server.NewId(),
		ClientId:  server.NewId(),
		StartedAt: startedAt,
	}
	tickets := []*LagTicket{}
	for position := range positions {
		tickets = append(tickets, &LagTicket{
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
	CreateLagTrack(ctx, track, tickets)
	return track, tickets
}

func testLagResult(ticket *LagTicket, createdAt time.Time) *LagResult {
	p50 := float32(21.5)
	return &LagResult{
		LagResultId:             server.NewId(),
		TrackId:                 ticket.TrackId,
		TrackPosition:           ticket.Position,
		TicketId:                ticket.TicketId,
		ExtenderId:              ticket.ExtenderId,
		ExtenderIpFamily:        4,
		WindowStartTime:         createdAt.Add(-time.Minute),
		PingOffsetMs:            []int32{0, 250, 500},
		PingMicros:              []int32{21000, 0, 22000},
		DownloadStartMs:         25000,
		DownloadMs:              5000,
		UploadStartMs:           30100,
		UploadMs:                5000,
		IntervalMs:              250,
		DownloadReceivedBytes:   []int64{1000, 2000},
		DownloadSentBytes:       []int64{1100, 2100},
		UploadBytes:             []int64{500},
		DownloadStreams:         4,
		UploadStreams:           4,
		UploadVerified:          true,
		EndReason:               "complete",
		OutsideLoadLatencyP50Ms: &p50,
		LostCount:               1,
		CreatedAt:               createdAt,
	}
}

type testLagTrackRow struct {
	asn         *int64
	asnOrg      *string
	countryCode *string
	region      *string
	city        *string
}

func testGetLagTrackRow(t testing.TB, ctx context.Context, trackId server.Id) *testLagTrackRow {
	t.Helper()
	row := &testLagTrackRow{}
	server.Db(ctx, func(conn server.PgConn) {
		server.Raise(conn.QueryRow(
			ctx,
			`
			SELECT client_asn, client_asn_org, client_country_code, client_region, client_city
			FROM lag_track
			WHERE track_id = $1
			`,
			trackId,
		).Scan(&row.asn, &row.asnOrg, &row.countryCode, &row.region, &row.city))
	})
	return row
}

// A track's tickets read back as issued.
func TestLagTrackTickets(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		startedAt := server.NowUtc().Truncate(time.Millisecond)
		_, tickets := testLagTrack(ctx, startedAt, 12)
		for _, ticket := range tickets {
			stored := GetLagTicket(ctx, ticket.TicketId)
			connect.AssertEqual(t, stored.TrackId, ticket.TrackId)
			connect.AssertEqual(t, stored.Position, ticket.Position)
			connect.AssertEqual(t, stored.ClientId, ticket.ClientId)
			connect.AssertEqual(t, stored.NetworkId, ticket.NetworkId)
			connect.AssertEqual(t, stored.ExtenderId, ticket.ExtenderId)
			connect.AssertEqual(t, stored.IssuedAt.Equal(ticket.IssuedAt), true)
			connect.AssertEqual(t, stored.ExpiresAt.Equal(ticket.ExpiresAt), true)
		}
		connect.AssertEqual(t, GetLagTicket(ctx, server.NewId()), (*LagTicket)(nil))
	})
}

// A track stores the client fields it was issued with (A5, amendment 9),
// and none when it was issued without them.
func TestCreateLagTrackClientFields(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		now := server.NowUtc().Truncate(time.Millisecond)

		asn := int64(64500)
		track := &LagTrack{
			TrackId:   server.NewId(),
			NetworkId: server.NewId(),
			ClientId:  server.NewId(),
			StartedAt: now,
			Client: &LagTrackClient{
				Asn:         &asn,
				AsnOrg:      "Example",
				CountryCode: "gb",
				Region:      "England",
				City:        "London",
			},
		}
		CreateLagTrack(ctx, track, nil)
		row := testGetLagTrackRow(t, ctx, track.TrackId)
		connect.AssertEqual(t, *row.asn, int64(64500))
		connect.AssertEqual(t, *row.asnOrg, "Example")
		connect.AssertEqual(t, *row.countryCode, "gb")
		connect.AssertEqual(t, *row.region, "England")
		connect.AssertEqual(t, *row.city, "London")

		bare, _ := testLagTrack(ctx, now, 0)
		row = testGetLagTrackRow(t, ctx, bare.TrackId)
		connect.AssertEqual(t, row.asn, (*int64)(nil))
		connect.AssertEqual(t, row.asnOrg, (*string)(nil))
		connect.AssertEqual(t, row.countryCode, (*string)(nil))
		connect.AssertEqual(t, row.region, (*string)(nil))
		connect.AssertEqual(t, row.city, (*string)(nil))
	})
}

// A ticket takes one report.
func TestAddLagResult(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		now := server.NowUtc().Truncate(time.Millisecond)
		track, tickets := testLagTrack(ctx, now, 2)

		connect.AssertEqual(t, AddLagResult(ctx, testLagResult(tickets[0], now)), true)
		// the same ticket again is no second report, whatever its row id
		connect.AssertEqual(t, AddLagResult(ctx, testLagResult(tickets[0], now)), false)
		connect.AssertEqual(t, AddLagResult(ctx, testLagResult(tickets[1], now)), true)

		var count int
		var pingMicros []int32
		var receivedBytes []int64
		var endReason string
		var p50 *float32
		var p95 *float32
		server.Db(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(
				ctx,
				`
				SELECT COUNT(*) OVER (), ping_micros, download_received_bytes, end_reason, outside_load_latency_p50_ms, own_load_latency_p95_ms
				FROM lag_result
				WHERE track_id = $1 AND track_position = 0
				`,
				track.TrackId,
			).Scan(&count, &pingMicros, &receivedBytes, &endReason, &p50, &p95))
		})
		connect.AssertEqual(t, count, 1)
		connect.AssertEqual(t, pingMicros, []int32{21000, 0, 22000})
		connect.AssertEqual(t, receivedBytes, []int64{1000, 2000})
		connect.AssertEqual(t, endReason, "complete")
		connect.AssertEqual(t, *p50, float32(21.5))
		connect.AssertEqual(t, p95, (*float32)(nil))
	})
}

// Retention removes tickets, results and tracks by their own cuts.
func TestRemoveExpiredLagResults(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		now := server.NowUtc().Truncate(time.Millisecond)
		oldTrack, oldTickets := testLagTrack(ctx, now.Add(-200*24*time.Hour), 1)
		newTrack, newTickets := testLagTrack(ctx, now, 1)
		connect.AssertEqual(t, AddLagResult(ctx, testLagResult(oldTickets[0], now.Add(-190*24*time.Hour))), true)
		connect.AssertEqual(t, AddLagResult(ctx, testLagResult(newTickets[0], now)), true)

		removed := RemoveExpiredLagResults(
			ctx,
			now.Add(-7*24*time.Hour),
			now.Add(-180*24*time.Hour),
			now.Add(-187*24*time.Hour),
		)
		connect.AssertEqual(t, removed, RemoveExpiredLagResultsResult{Tickets: 1, Results: 1, Tracks: 1})
		connect.AssertEqual(t, GetLagTicket(ctx, oldTickets[0].TicketId), (*LagTicket)(nil))
		connect.AssertNotEqual(t, GetLagTicket(ctx, newTickets[0].TicketId), (*LagTicket)(nil))

		var oldCount, newCount int
		server.Db(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(
				ctx,
				`
				SELECT
					(SELECT COUNT(*) FROM lag_result WHERE track_id = $1) + (SELECT COUNT(*) FROM lag_track WHERE track_id = $1),
					(SELECT COUNT(*) FROM lag_result WHERE track_id = $2) + (SELECT COUNT(*) FROM lag_track WHERE track_id = $2)
				`,
				oldTrack.TrackId,
				newTrack.TrackId,
			).Scan(&oldCount, &newCount))
		})
		connect.AssertEqual(t, oldCount, 0)
		connect.AssertEqual(t, newCount, 2)
	})
}
