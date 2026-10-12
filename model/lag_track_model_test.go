package model

import (
	"context"
	"net/netip"
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
			Ip:         netip.AddrFrom4([4]byte{203, 0, 113, byte(position + 1)}),
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
	clientFieldsAt *time.Time
	addressHash    []byte
	prefix         *string
	asn            *int64
	countryCode    *string
}

func testGetLagTrackRow(t testing.TB, ctx context.Context, trackId server.Id) *testLagTrackRow {
	t.Helper()
	row := &testLagTrackRow{}
	server.Db(ctx, func(conn server.PgConn) {
		server.Raise(conn.QueryRow(
			ctx,
			`
			SELECT client_fields_at, client_address_hash, client_prefix, client_asn, client_country_code
			FROM lag_track
			WHERE track_id = $1
			`,
			trackId,
		).Scan(&row.clientFieldsAt, &row.addressHash, &row.prefix, &row.asn, &row.countryCode))
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
			connect.AssertEqual(t, stored.Ip, ticket.Ip)
			connect.AssertEqual(t, stored.IssuedAt.Equal(ticket.IssuedAt), true)
			connect.AssertEqual(t, stored.ExpiresAt.Equal(ticket.ExpiresAt), true)
		}
		connect.AssertEqual(t, GetLagTicket(ctx, server.NewId()), (*LagTicket)(nil))
	})
}

// A ticket takes one report; only the track's first report fills the
// track's client fields.
func TestAddLagResult(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		now := server.NowUtc().Truncate(time.Millisecond)
		track, tickets := testLagTrack(ctx, now, 2)

		asn := int64(64500)
		latitude := 51.5
		longitude := -0.1
		first := &LagTrackClient{
			AddressHash: []byte{1, 2, 3},
			Prefix:      "198.51.100.0/24",
			Asn:         &asn,
			AsnOrg:      "Example",
			CountryCode: "gb",
			Region:      "England",
			City:        "London",
			Latitude:    &latitude,
			Longitude:   &longitude,
		}
		connect.AssertEqual(t, AddLagResult(ctx, testLagResult(tickets[0], now), first), true)
		// the same ticket again is no second report, whatever its row id
		connect.AssertEqual(t, AddLagResult(ctx, testLagResult(tickets[0], now), first), false)

		second := &LagTrackClient{
			AddressHash: []byte{9},
			Prefix:      "192.0.2.0/24",
			CountryCode: "us",
		}
		connect.AssertEqual(t, AddLagResult(ctx, testLagResult(tickets[1], now), second), true)

		row := testGetLagTrackRow(t, ctx, track.TrackId)
		connect.AssertNotEqual(t, row.clientFieldsAt, (*time.Time)(nil))
		connect.AssertEqual(t, row.addressHash, []byte{1, 2, 3})
		connect.AssertEqual(t, *row.prefix, "198.51.100.0/24")
		connect.AssertEqual(t, *row.asn, int64(64500))
		connect.AssertEqual(t, *row.countryCode, "gb")

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
		connect.AssertEqual(t, AddLagResult(ctx, testLagResult(oldTickets[0], now.Add(-190*24*time.Hour)), nil), true)
		connect.AssertEqual(t, AddLagResult(ctx, testLagResult(newTickets[0], now), nil), true)

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
