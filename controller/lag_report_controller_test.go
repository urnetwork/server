package controller

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"math"
	"net/netip"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/connect/protocol"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
)

// Tests of the lag report endpoint (lag.dev/PLAN.md A7, T2): verification
// under the calling extender's key, the ticket match, the bounds, what is
// derived at insert, and that the browser's address is stored nowhere.

// An extender with its identity key, activated by a client of its own, and
// that client's session, which is the activation credential a report is
// posted with.
type lagTestExtender struct {
	extenderId    server.Id
	clientId      server.Id
	publicKey     ed25519.PublicKey
	privateKey    ed25519.PrivateKey
	clientSession *session.ClientSession
}

func newLagTestExtender(t testing.TB, ctx context.Context, ips ...string) *lagTestExtender {
	t.Helper()
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	networkId := server.NewId()
	extender := &lagTestExtender{
		extenderId: server.NewId(),
		clientId:   server.NewId(),
		publicKey:  publicKey,
		privateKey: privateKey,
	}
	addresses := []*model.NetworkExtenderAddress{}
	for _, ip := range ips {
		addr := netip.MustParseAddr(ip)
		addresses = append(addresses, &model.NetworkExtenderAddress{
			ExtenderId:   extender.extenderId,
			IpVersion:    server.IpVersionForAddr(addr),
			Ip:           addr,
			Carriers:     []string{connect.ExtenderCarrierTcp},
			ActivateTime: server.NowUtc(),
			Active:       true,
		})
	}
	model.Testing_CreateNetworkExtender(ctx, &model.NetworkExtender{
		ExtenderId:  extender.extenderId,
		NetworkId:   networkId,
		ClientId:    extender.clientId,
		PublicKey:   publicKey,
		CreateTime:  server.NowUtc(),
		TcpPort:     443,
		CountryCode: "de",
		Active:      true,
	}, addresses)
	byJwt := session.NewByJwt(networkId, server.NewId(), "lag-test-extender", false, false).
		Client(server.NewId(), extender.clientId)
	extender.clientSession = session.NewLocalClientSession(ctx, "198.51.100.9:4433", byJwt)
	t.Cleanup(extender.clientSession.Cancel)
	return extender
}

// A track with one ticket per extender, in order.
func newLagTestTrack(ctx context.Context, issuedAt time.Time, extenders ...*lagTestExtender) (*model.LagTrack, []*model.LagTicket) {
	track := &model.LagTrack{
		TrackId:   server.NewId(),
		NetworkId: server.NewId(),
		ClientId:  server.NewId(),
		StartedAt: issuedAt,
	}
	tickets := []*model.LagTicket{}
	for position, extender := range extenders {
		tickets = append(tickets, &model.LagTicket{
			TicketId:   server.NewId(),
			TrackId:    track.TrackId,
			Position:   position,
			ClientId:   track.ClientId,
			NetworkId:  track.NetworkId,
			ExtenderId: extender.extenderId,
			IssuedAt:   issuedAt,
			ExpiresAt:  issuedAt.Add(2 * time.Minute),
		})
	}
	model.CreateLagTrack(ctx, track, tickets)
	return track, tickets
}

// The minute around a load, as an extender reports it: pings 250 ms apart
// from the window start, 20 ms idle and 80 ms during the download and the
// upload, one ping lost, and 20 intervals of each direction.
func newLagTestReportBody(extender *lagTestExtender, ticket *model.LagTicket, windowStartTime time.Time) *protocol.LagReportBody {
	body := &protocol.LagReportBody{
		TicketId:           ticket.TicketId.Bytes(),
		ExtenderPublicKey:  extender.publicKey,
		TrackId:            ticket.TrackId.Bytes(),
		TrackPosition:      uint32(ticket.Position),
		IpFamily:           4,
		SessionStartTimeMs: uint64(windowStartTime.Add(-time.Second).UnixMilli()),
		WindowStartTimeMs:  uint64(windowStartTime.UnixMilli()),
		DownloadStartMs:    25000,
		DownloadMs:         5000,
		UploadStartMs:      30000,
		UploadMs:           5000,
		IntervalMs:         250,
		DownloadStreams:    4,
		UploadStreams:      4,
		UploadVerified:     true,
		EndReason:          protocol.LagEndReason_LAG_END_COMPLETE,
	}
	for offsetMs := uint32(0); offsetMs < 60000; offsetMs += 250 {
		micros := uint32(20000)
		if 25000 <= offsetMs && offsetMs < 35000 {
			micros = 80000
		}
		if offsetMs == 1000 {
			// lost
			micros = 0
		}
		body.PingOffsetMs = append(body.PingOffsetMs, offsetMs)
		body.PingMicros = append(body.PingMicros, micros)
	}
	for range 20 {
		// 100 Mbps down, 20 Mbps up
		body.DownloadReceivedBytes = append(body.DownloadReceivedBytes, 3125000)
		body.DownloadSentBytes = append(body.DownloadSentBytes, 3200000)
		body.UploadBytes = append(body.UploadBytes, 625000)
	}
	return body
}

func signLagTestReport(t testing.TB, body *protocol.LagReportBody, privateKey ed25519.PrivateKey) string {
	t.Helper()
	report, err := connect.SignLagReport(body, privateKey)
	if err != nil {
		t.Fatal(err)
	}
	reportBytes, err := proto.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	return base64.StdEncoding.EncodeToString(reportBytes)
}

// Replaces the GeoLite2 City place lookup for the rest of the test.
func useLagAddressPlaces(t testing.TB, places map[string]*model.LagPlace) {
	t.Helper()
	previous := lagAddressPlace
	lagAddressPlace = func(ip netip.Addr) *model.LagPlace {
		return places[ip.Unmap().String()]
	}
	t.Cleanup(func() {
		lagAddressPlace = previous
	})
}

// Every row of a table as json text, for looking for what must not be there.
func lagTestTableText(t testing.TB, ctx context.Context, table string) string {
	t.Helper()
	rows := []string{}
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(ctx, `SELECT row_to_json(t)::text FROM `+table+` t`)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var row string
				server.Raise(result.Scan(&row))
				rows = append(rows, row)
			}
		})
	})
	return strings.Join(rows, "\n")
}

func postLagTestReports(t testing.TB, extender *lagTestExtender, now time.Time, reports ...string) *ExtenderLagReportResult {
	t.Helper()
	result, err := extenderLagReport(
		&ExtenderLagReportArgs{Reports: reports},
		extender.clientSession,
		now,
		DefaultLagSettings(),
	)
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func assertLagReportRejected(t testing.TB, result *ExtenderLagReportResult, reason string) {
	t.Helper()
	if result.Accepted != 0 || len(result.Rejected) != 1 || result.Rejected[0].Index != 0 || result.Rejected[0].Reason != reason {
		t.Fatalf("accepted %d, rejected %+v; want one rejection for %s", result.Accepted, lagTestRejections(result), reason)
	}
}

func lagTestRejections(result *ExtenderLagReportResult) []ExtenderLagReportRejection {
	rejections := []ExtenderLagReportRejection{}
	for _, rejection := range result.Rejected {
		rejections = append(rejections, *rejection)
	}
	return rejections
}

// The bounds of A7, each just inside and just outside. Pure.
func TestLagReportBounds(t *testing.T) {
	now := time.Unix(1790000000, 0).UTC()
	settings := DefaultLagSettings()
	ticket := &model.LagTicket{IssuedAt: now.Add(-time.Minute)}
	// the session opened 50 s ago and its minute started 20 s after that, on
	// a clock offset by `offset` from the operator's
	inBoundsAt := func(ticket *model.LagTicket, offset time.Duration, edit func(body *protocol.LagReportBody)) bool {
		body := &protocol.LagReportBody{
			SessionStartTimeMs: uint64(now.Add(offset - 50*time.Second).UnixMilli()),
			WindowStartTimeMs:  uint64(now.Add(offset - 30*time.Second).UnixMilli()),
			IntervalMs:         250,
		}
		edit(body)
		return lagReportInBounds(body, ticket, now, settings)
	}
	inBounds := func(edit func(body *protocol.LagReportBody)) bool {
		return inBoundsAt(ticket, 0, edit)
	}
	pings := func(count int, offsetMs uint32, micros uint32) func(body *protocol.LagReportBody) {
		return func(body *protocol.LagReportBody) {
			for range count {
				body.PingOffsetMs = append(body.PingOffsetMs, offsetMs)
				body.PingMicros = append(body.PingMicros, micros)
			}
		}
	}
	intervals := func(count int, byteCount uint64) []uint64 {
		values := []uint64{}
		for range count {
			values = append(values, byteCount)
		}
		return values
	}
	// 10 Gbps over a 250 ms interval, and half as much again for the slack
	const maxIntervalBytes = 10_000_000_000 / 8 / 4 * 3 / 2

	connect.AssertEqual(t, inBounds(func(body *protocol.LagReportBody) {}), true)

	// at most 600 pings, each offset within the minute, an offset per ping
	connect.AssertEqual(t, inBounds(pings(600, 60000, 5_000_000)), true)
	connect.AssertEqual(t, inBounds(pings(601, 0, 20000)), false)
	connect.AssertEqual(t, inBounds(pings(1, 60001, 20000)), false)
	connect.AssertEqual(t, inBounds(pings(1, 0, lagReportMaxPingMicros+1)), false)
	connect.AssertEqual(t, inBounds(func(body *protocol.LagReportBody) {
		body.PingOffsetMs = []uint32{0, 250}
		body.PingMicros = []uint32{20000}
	}), false)

	// at most 40 intervals per direction, none above 10 Gbps plus slack
	for _, set := range []func(body *protocol.LagReportBody, values []uint64){
		func(body *protocol.LagReportBody, values []uint64) { body.DownloadReceivedBytes = values },
		func(body *protocol.LagReportBody, values []uint64) { body.DownloadSentBytes = values },
		func(body *protocol.LagReportBody, values []uint64) { body.UploadBytes = values },
	} {
		connect.AssertEqual(t, inBounds(func(body *protocol.LagReportBody) { set(body, intervals(40, maxIntervalBytes)) }), true)
		connect.AssertEqual(t, inBounds(func(body *protocol.LagReportBody) { set(body, intervals(41, 1)) }), false)
		connect.AssertEqual(t, inBounds(func(body *protocol.LagReportBody) { set(body, intervals(1, maxIntervalBytes+1)) }), false)
		// a series needs an interval length to mean anything
		connect.AssertEqual(t, inBounds(func(body *protocol.LagReportBody) {
			set(body, intervals(1, 1))
			body.IntervalMs = 0
		}), false)
	}
	connect.AssertEqual(t, inBounds(func(body *protocol.LagReportBody) { body.IntervalMs = lagReportMaxIntervalMs + 1 }), false)

	// the load sits near its minute, and the stream counts are counts
	connect.AssertEqual(t, inBounds(func(body *protocol.LagReportBody) { body.DownloadStartMs = lagReportMaxLoadMs + 1 }), false)
	connect.AssertEqual(t, inBounds(func(body *protocol.LagReportBody) { body.UploadMs = lagReportMaxLoadMs + 1 }), false)
	connect.AssertEqual(t, inBounds(func(body *protocol.LagReportBody) { body.UploadStreams = lagReportMaxStreams + 1 }), false)

	// the extender's clock is its own: half an hour slow or fast, or a
	// month off, is a clock like any other
	for _, offset := range []time.Duration{-30 * time.Minute, 30 * time.Minute, -29 * 24 * time.Hour, 29 * 24 * time.Hour} {
		connect.AssertEqual(t, inBoundsAt(ticket, offset, func(body *protocol.LagReportBody) {}), true)
	}
	// but one more than 30 days off was never set
	connect.AssertEqual(t, inBoundsAt(ticket, -31*24*time.Hour, func(body *protocol.LagReportBody) {}), false)
	connect.AssertEqual(t, inBoundsAt(ticket, 31*24*time.Hour, func(body *protocol.LagReportBody) {}), false)

	// in the extender's own time, the minute starts at most a minute before
	// its session opened
	sessionStart := func(body *protocol.LagReportBody) time.Time {
		return time.UnixMilli(int64(body.SessionStartTimeMs))
	}
	connect.AssertEqual(t, inBounds(func(body *protocol.LagReportBody) {
		body.WindowStartTimeMs = uint64(sessionStart(body).Add(-time.Minute).UnixMilli())
	}), true)
	connect.AssertEqual(t, inBounds(func(body *protocol.LagReportBody) {
		body.WindowStartTimeMs = uint64(sessionStart(body).Add(-time.Minute - time.Millisecond).UnixMilli())
	}), false)
	// and no later than the session could have run: from the ticket's issue
	// to the report's arrival, a minute here, with a minute's slack
	connect.AssertEqual(t, inBounds(func(body *protocol.LagReportBody) {
		body.WindowStartTimeMs = uint64(sessionStart(body).Add(2 * time.Minute).UnixMilli())
	}), true)
	connect.AssertEqual(t, inBounds(func(body *protocol.LagReportBody) {
		body.WindowStartTimeMs = uint64(sessionStart(body).Add(2*time.Minute + time.Millisecond).UnixMilli())
	}), false)
	// a day-old ticket's session may have run a day
	connect.AssertEqual(t, inBoundsAt(&model.LagTicket{IssuedAt: now.Add(-23 * time.Hour)}, 0, func(body *protocol.LagReportBody) {
		body.SessionStartTimeMs = uint64(now.Add(-22 * time.Hour).UnixMilli())
	}), true)
	// both times are needed
	connect.AssertEqual(t, inBounds(func(body *protocol.LagReportBody) { body.WindowStartTimeMs = 0 }), false)
	connect.AssertEqual(t, inBounds(func(body *protocol.LagReportBody) { body.SessionStartTimeMs = 0 }), false)
	connect.AssertEqual(t, inBounds(func(body *protocol.LagReportBody) { body.WindowStartTimeMs = math.MaxUint64 }), false)

	// on the operator's clock, the report arrives within report_max_age of
	// its ticket's issue
	connect.AssertEqual(t, settings.ReportMaxAge, 24*time.Hour)
	connect.AssertEqual(t, inBoundsAt(&model.LagTicket{IssuedAt: now.Add(-24 * time.Hour)}, 0, func(body *protocol.LagReportBody) {}), true)
	connect.AssertEqual(t, inBoundsAt(&model.LagTicket{IssuedAt: now.Add(-24*time.Hour - time.Second)}, 0, func(body *protocol.LagReportBody) {}), false)
	// a ticket another replica issued a moment "after" the report arrived
	connect.AssertEqual(t, inBoundsAt(&model.LagTicket{IssuedAt: now.Add(time.Second)}, 0, func(body *protocol.LagReportBody) {}), true)
}

// What is derived at insert (A5): latency during the session's own load and
// outside it, the speeds over the intervals, and the lost pings. Pure.
func TestLagResultDerivedStatistics(t *testing.T) {
	now := time.Unix(1790000000, 0).UTC()
	extender := &lagTestExtender{extenderId: server.NewId()}
	extender.publicKey, extender.privateKey, _ = ed25519.GenerateKey(rand.Reader)
	ticket := &model.LagTicket{
		TicketId:   server.NewId(),
		TrackId:    server.NewId(),
		Position:   3,
		ExtenderId: extender.extenderId,
		IssuedAt:   now.Add(-time.Minute),
	}
	body := newLagTestReportBody(extender, ticket, now.Add(-time.Minute))
	// a few slow pings, too few to move either percentile
	body.PingMicros[0] = 400000
	body.PingMicros[1] = 300000
	body.PingMicros[100] = 900000

	result := newLagResult(body, ticket, now)
	connect.AssertEqual(t, result.TrackId, ticket.TrackId)
	connect.AssertEqual(t, result.TrackPosition, 3)
	connect.AssertEqual(t, result.TicketId, ticket.TicketId)
	connect.AssertEqual(t, result.ExtenderId, extender.extenderId)
	connect.AssertEqual(t, result.ExtenderIpFamily, 4)
	connect.AssertEqual(t, result.WindowStartTime.Equal(now.Add(-time.Minute)), true)
	connect.AssertEqual(t, len(result.PingOffsetMs), 240)
	connect.AssertEqual(t, result.PingMicros[4], int32(0))
	connect.AssertEqual(t, result.LostCount, 1)
	connect.AssertEqual(t, result.EndReason, "complete")

	// 40 pings in the 10 s of load, 199 that came back outside it
	connect.AssertEqual(t, *result.OwnLoadLatencyP50Ms, float32(80))
	connect.AssertEqual(t, *result.OwnLoadLatencyP95Ms, float32(80))
	connect.AssertEqual(t, *result.OutsideLoadLatencyP50Ms, float32(20))
	connect.AssertEqual(t, *result.OutsideLoadLatencyP95Ms, float32(20))
	connect.AssertEqual(t, *result.DownloadP50Mbps, float32(100))
	connect.AssertEqual(t, *result.DownloadP95Mbps, float32(100))
	connect.AssertEqual(t, *result.UploadP50Mbps, float32(20))
	connect.AssertEqual(t, *result.UploadP95Mbps, float32(20))

	// the family is the one the report names, which the browser reached the
	// extender on
	body.IpFamily = 6
	connect.AssertEqual(t, newLagResult(body, ticket, now).ExtenderIpFamily, 6)
	body.IpFamily = 4

	// a session that closed before its load has no load statistics
	noLoad := &protocol.LagReportBody{
		WindowStartTimeMs: uint64(now.Add(-time.Minute).UnixMilli()),
		PingOffsetMs:      []uint32{0, 250, 500, 750},
		PingMicros:        []uint32{10000, 30000, 0, 20000},
		EndReason:         protocol.LagEndReason_LAG_END_NO_LOAD,
	}
	result = newLagResult(noLoad, ticket, now)
	connect.AssertEqual(t, result.EndReason, "no_load")
	connect.AssertEqual(t, result.LostCount, 1)
	connect.AssertEqual(t, result.OwnLoadLatencyP50Ms, (*float32)(nil))
	connect.AssertEqual(t, *result.OutsideLoadLatencyP50Ms, float32(20))
	connect.AssertEqual(t, *result.OutsideLoadLatencyP95Ms, float32(30))
	connect.AssertEqual(t, result.DownloadP50Mbps, (*float32)(nil))
	connect.AssertEqual(t, result.UploadP95Mbps, (*float32)(nil))
}

// Only the intervals inside a direction's load count toward its statistics:
// the first ceil(load / interval) of the series. Intervals past the window,
// however fast they claim to be, are stored with the series and change no
// statistic. Pure.
func TestLagResultStatisticsCountOnlyTheLoad(t *testing.T) {
	now := time.Unix(1790000000, 0).UTC()
	extender := &lagTestExtender{extenderId: server.NewId()}
	extender.publicKey, extender.privateKey, _ = ed25519.GenerateKey(rand.Reader)
	ticket := &model.LagTicket{TicketId: server.NewId(), TrackId: server.NewId(), ExtenderId: extender.extenderId, IssuedAt: now.Add(-time.Minute)}
	body := newLagTestReportBody(extender, ticket, now.Add(-time.Minute))
	// 5 s of 250 ms intervals is 20; five more at 9 Gbps claimed past it
	for range 5 {
		body.DownloadReceivedBytes = append(body.DownloadReceivedBytes, 281_250_000)
		body.DownloadSentBytes = append(body.DownloadSentBytes, 281_250_000)
		body.UploadBytes = append(body.UploadBytes, 281_250_000)
	}
	result := newLagResult(body, ticket, now)
	connect.AssertEqual(t, *result.DownloadP50Mbps, float32(100))
	connect.AssertEqual(t, *result.DownloadP95Mbps, float32(100))
	connect.AssertEqual(t, *result.UploadP50Mbps, float32(20))
	connect.AssertEqual(t, *result.UploadP95Mbps, float32(20))
	// stored whole
	connect.AssertEqual(t, len(result.DownloadReceivedBytes), 25)
	connect.AssertEqual(t, len(result.DownloadSentBytes), 25)
	connect.AssertEqual(t, len(result.UploadBytes), 25)
	connect.AssertEqual(t, result.DownloadReceivedBytes[24], int64(281_250_000))

	// a part interval counts: 1.1 s of 250 ms intervals is 5
	connect.AssertEqual(t, len(lagLoadIntervals(body.DownloadReceivedBytes, 1100, 250)), 5)
	connect.AssertEqual(t, len(lagLoadIntervals(body.DownloadReceivedBytes, 1000, 250)), 4)
	// a series shorter than its load is counted whole
	connect.AssertEqual(t, len(lagLoadIntervals(body.DownloadReceivedBytes[:3], 5000, 250)), 3)
	// no load, or no interval length, counts nothing
	connect.AssertEqual(t, len(lagLoadIntervals(body.DownloadReceivedBytes, 0, 250)), 0)
	connect.AssertEqual(t, len(lagLoadIntervals(body.DownloadReceivedBytes, 5000, 0)), 0)

	// a session whose load was refused has a series and no load: no speeds
	body.DownloadMs = 0
	body.UploadMs = 0
	body.EndReason = protocol.LagEndReason_LAG_END_BUSY
	result = newLagResult(body, ticket, now)
	connect.AssertEqual(t, result.DownloadP50Mbps, (*float32)(nil))
	connect.AssertEqual(t, result.UploadP95Mbps, (*float32)(nil))
	connect.AssertEqual(t, len(result.DownloadReceivedBytes), 25)
}

// A load the browser gave up, or one an error cut short, gives no speeds
// and no own-load latency: only a load that ran to its end does. The latency
// outside the load stands, and the series are stored whole (A5). Pure.
func TestLagResultLoadStatisticsOnlyWhenComplete(t *testing.T) {
	now := time.Unix(1790000000, 0).UTC()
	extender := &lagTestExtender{extenderId: server.NewId()}
	extender.publicKey, extender.privateKey, _ = ed25519.GenerateKey(rand.Reader)
	ticket := &model.LagTicket{TicketId: server.NewId(), TrackId: server.NewId(), ExtenderId: extender.extenderId, IssuedAt: now.Add(-time.Minute)}
	for _, endReason := range []protocol.LagEndReason{
		protocol.LagEndReason_LAG_END_CLIENT_CLOSED,
		protocol.LagEndReason_LAG_END_ERROR,
	} {
		body := newLagTestReportBody(extender, ticket, now.Add(-time.Minute))
		// the load stopped 2.1 s into its download
		body.EndReason = endReason
		body.DownloadMs = 2100
		body.UploadStartMs = 0
		body.UploadMs = 0
		body.DownloadReceivedBytes = body.DownloadReceivedBytes[:9]
		body.DownloadSentBytes = body.DownloadSentBytes[:9]
		body.UploadBytes = nil

		result := newLagResult(body, ticket, now)
		connect.AssertEqual(t, result.EndReason, lagEndReasonName(endReason))
		connect.AssertEqual(t, result.OwnLoadLatencyP50Ms, (*float32)(nil))
		connect.AssertEqual(t, result.OwnLoadLatencyP95Ms, (*float32)(nil))
		connect.AssertEqual(t, result.DownloadP50Mbps, (*float32)(nil))
		connect.AssertEqual(t, result.DownloadP95Mbps, (*float32)(nil))
		connect.AssertEqual(t, result.UploadP50Mbps, (*float32)(nil))
		connect.AssertEqual(t, result.UploadP95Mbps, (*float32)(nil))
		connect.AssertEqual(t, *result.OutsideLoadLatencyP50Ms, float32(20))
		connect.AssertEqual(t, len(result.DownloadReceivedBytes), 9)
		connect.AssertEqual(t, result.LostCount, 1)
	}
}

// Percentiles are by nearest rank, as the browser computes them (S5). Pure.
func TestLagNearestRank(t *testing.T) {
	connect.AssertEqual(t, lagNearestRank([]float64{}, 50), (*float32)(nil))
	connect.AssertEqual(t, *lagNearestRank([]float64{7}, 50), float32(7))
	connect.AssertEqual(t, *lagNearestRank([]float64{7}, 95), float32(7))
	connect.AssertEqual(t, *lagNearestRank([]float64{4, 1, 3, 2}, 50), float32(2))
	connect.AssertEqual(t, *lagNearestRank([]float64{4, 1, 3, 2}, 95), float32(4))
	values := []float64{}
	for i := 100; 0 < i; i -= 1 {
		values = append(values, float64(i))
	}
	connect.AssertEqual(t, *lagNearestRank(values, 50), float32(50))
	connect.AssertEqual(t, *lagNearestRank(values, 95), float32(95))
}

// An end reason is stored under its name, and a newer one under its number.
func TestLagEndReasonName(t *testing.T) {
	connect.AssertEqual(t, lagEndReasonName(protocol.LagEndReason_LAG_END_COMPLETE), "complete")
	connect.AssertEqual(t, lagEndReasonName(protocol.LagEndReason_LAG_END_UPLOAD_MISMATCH), "upload_mismatch")
	connect.AssertEqual(t, lagEndReasonName(protocol.LagEndReason_LAG_END_NO_LOAD), "no_load")
	connect.AssertEqual(t, lagEndReasonName(protocol.LagEndReason_LAG_END_BUSY), "busy")
	connect.AssertEqual(t, lagEndReasonName(protocol.LagEndReason(250)), "250")
}

// A verified report is stored as its ticket's one result, with the address
// family it names and no address at all (amendment 9).
func TestExtenderLagReportStores(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		now := server.NowUtc().Truncate(time.Millisecond)
		// the extender saw the browser at these; the reports never carry them
		const clientIp = "203.0.113.77"
		const clientIp6 = "2001:db8:77:1::9"

		first := newLagTestExtender(t, ctx, "192.0.2.10")
		second := newLagTestExtender(t, ctx, "192.0.2.20", "2001:db8:20::1")
		_, tickets := newLagTestTrack(ctx, now.Add(-2*time.Minute), first, second)

		windowStartTime := now.Add(-time.Minute)
		report := signLagTestReport(t, newLagTestReportBody(first, tickets[0], windowStartTime), first.privateKey)
		result := postLagTestReports(t, first, now, report)
		connect.AssertEqual(t, result.Accepted, 1)
		connect.AssertEqual(t, len(result.Rejected), 0)

		// the result row
		var trackPosition, extenderIpFamily, lostCount, downloadMs, intervalMs int
		var storedWindowStartTime time.Time
		var pingOffsetMs, pingMicros []int32
		var downloadReceivedBytes, downloadSentBytes, uploadBytes []int64
		var uploadVerified bool
		var endReason string
		var ownLoadP50, outsideLoadP50, downloadP50, uploadP50 *float32
		var storedExtenderId server.Id
		server.Db(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(
				ctx,
				`
				SELECT
					track_position, extender_id, extender_ip_family, window_start_time,
					ping_offset_ms, ping_micros, download_ms, interval_ms,
					download_received_bytes, download_sent_bytes, upload_bytes,
					upload_verified, end_reason, lost_count,
					own_load_latency_p50_ms, outside_load_latency_p50_ms, download_p50_mbps, upload_p50_mbps
				FROM lag_result
				WHERE ticket_id = $1
				`,
				tickets[0].TicketId,
			).Scan(
				&trackPosition, &storedExtenderId, &extenderIpFamily, &storedWindowStartTime,
				&pingOffsetMs, &pingMicros, &downloadMs, &intervalMs,
				&downloadReceivedBytes, &downloadSentBytes, &uploadBytes,
				&uploadVerified, &endReason, &lostCount,
				&ownLoadP50, &outsideLoadP50, &downloadP50, &uploadP50,
			))
		})
		connect.AssertEqual(t, trackPosition, 0)
		connect.AssertEqual(t, storedExtenderId, first.extenderId)
		connect.AssertEqual(t, extenderIpFamily, 4)
		connect.AssertEqual(t, storedWindowStartTime.Equal(windowStartTime), true)
		connect.AssertEqual(t, len(pingOffsetMs), 240)
		connect.AssertEqual(t, pingOffsetMs[239], int32(59750))
		connect.AssertEqual(t, pingMicros[0], int32(20000))
		connect.AssertEqual(t, pingMicros[4], int32(0))
		connect.AssertEqual(t, pingMicros[100], int32(80000))
		connect.AssertEqual(t, downloadMs, 5000)
		connect.AssertEqual(t, intervalMs, 250)
		connect.AssertEqual(t, len(downloadReceivedBytes), 20)
		connect.AssertEqual(t, downloadReceivedBytes[0], int64(3125000))
		connect.AssertEqual(t, downloadSentBytes[0], int64(3200000))
		connect.AssertEqual(t, uploadBytes[19], int64(625000))
		connect.AssertEqual(t, uploadVerified, true)
		connect.AssertEqual(t, endReason, "complete")
		connect.AssertEqual(t, lostCount, 1)
		connect.AssertEqual(t, *ownLoadP50, float32(80))
		connect.AssertEqual(t, *outsideLoadP50, float32(20))
		connect.AssertEqual(t, *downloadP50, float32(100))
		connect.AssertEqual(t, *uploadP50, float32(20))

		// the same report again is no second result
		assertLagReportRejected(t, postLagTestReports(t, first, now, report), LagReportRejectDuplicate)

		// the track's second report, over ipv6, stores its own row with its family
		secondBody := newLagTestReportBody(second, tickets[1], windowStartTime)
		secondBody.IpFamily = 6
		secondReport := signLagTestReport(t, secondBody, second.privateKey)
		result = postLagTestReports(t, second, now, secondReport)
		connect.AssertEqual(t, result.Accepted, 1)
		server.Db(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(
				ctx,
				`SELECT extender_ip_family, track_position FROM lag_result WHERE ticket_id = $1`,
				tickets[1].TicketId,
			).Scan(&extenderIpFamily, &trackPosition))
		})
		connect.AssertEqual(t, extenderIpFamily, 6)
		connect.AssertEqual(t, trackPosition, 1)

		// The browser's address is stored nowhere: not as text, not as an
		// inet, and not as its bytes.
		for _, table := range []string{"lag_track", "lag_ticket", "lag_result"} {
			text := lagTestTableText(t, ctx, table)
			for _, ip := range []string{clientIp, clientIp6} {
				addr := netip.MustParseAddr(ip)
				for _, forbidden := range []string{ip, hex.EncodeToString(addr.AsSlice())} {
					if strings.Contains(strings.ToLower(text), forbidden) {
						t.Fatalf("%s holds the client address %s (as %q): %s", table, ip, forbidden, text)
					}
				}
			}
		}
	})
}

// An extender's clock half an hour slow or fast is its own business: the
// report is stored with the extender's own session and window times, beside
// its arrival on the operator's clock, so the offset can be estimated later.
// A report that arrives more than report_max_age after its ticket was issued
// is refused, whatever the extender's clock says.
func TestExtenderLagReportExtenderClock(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		now := server.NowUtc().Truncate(time.Millisecond)
		useIpAsnDb(t, "")
		useLagAddressPlaces(t, map[string]*model.LagPlace{})
		extender := newLagTestExtender(t, ctx, "192.0.2.10")
		issuedAt := now.Add(-3 * time.Minute)
		_, tickets := newLagTestTrack(ctx, issuedAt, extender, extender, extender)

		for i, offset := range []time.Duration{-30 * time.Minute, 30 * time.Minute} {
			windowStartTime := now.Add(offset - time.Minute)
			body := newLagTestReportBody(extender, tickets[i], windowStartTime)
			result := postLagTestReports(t, extender, now, signLagTestReport(t, body, extender.privateKey))
			if result.Accepted != 1 {
				t.Fatalf("an extender clock %s off: rejected %+v", offset, lagTestRejections(result))
			}

			var sessionStartTime, storedWindowStartTime, createdAt time.Time
			server.Db(ctx, func(conn server.PgConn) {
				server.Raise(conn.QueryRow(
					ctx,
					`SELECT session_start_time, window_start_time, created_at FROM lag_result WHERE ticket_id = $1`,
					tickets[i].TicketId,
				).Scan(&sessionStartTime, &storedWindowStartTime, &createdAt))
			})
			connect.AssertEqual(t, sessionStartTime.Equal(windowStartTime.Add(-time.Second)), true)
			connect.AssertEqual(t, storedWindowStartTime.Equal(windowStartTime), true)
			connect.AssertEqual(t, createdAt.Equal(now), true)
			// what an offset estimate would read: the extender's clock against
			// the arrival, a minute after the window started
			if estimate := storedWindowStartTime.Add(time.Minute).Sub(createdAt); estimate != offset {
				t.Fatalf("the stored times estimate an offset of %s, want %s", estimate, offset)
			}
		}

		// a day and a second after the ticket, on the operator's clock
		arrival := issuedAt.Add(24*time.Hour + time.Second)
		late := newLagTestReportBody(extender, tickets[2], arrival.Add(-time.Minute))
		assertLagReportRejected(t, postLagTestReports(t, extender, arrival, signLagTestReport(t, late, extender.privateKey)), LagReportRejectBounds)
		// a second earlier it is in time
		arrival = issuedAt.Add(24 * time.Hour)
		late = newLagTestReportBody(extender, tickets[2], arrival.Add(-time.Minute))
		connect.AssertEqual(t, postLagTestReports(t, extender, arrival, signLagTestReport(t, late, extender.privateKey)).Accepted, 1)
	})
}

// A report is refused unless it is the calling extender's own, verifies, and
// matches a ticket issued for that extender at that place in that track.
func TestExtenderLagReportRefusals(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		now := server.NowUtc().Truncate(time.Millisecond)
		extender := newLagTestExtender(t, ctx, "192.0.2.10")
		other := newLagTestExtender(t, ctx, "192.0.2.20")
		_, tickets := newLagTestTrack(ctx, now.Add(-2*time.Minute), extender, other)
		windowStartTime := now.Add(-time.Minute)
		body := func() *protocol.LagReportBody {
			return newLagTestReportBody(extender, tickets[0], windowStartTime)
		}

		// not a report at all
		assertLagReportRejected(t, postLagTestReports(t, extender, now, "not base64 !"), LagReportRejectMalformed)
		assertLagReportRejected(t, postLagTestReports(t, extender, now, base64.StdEncoding.EncodeToString([]byte{0xff, 0xff, 0xff})), LagReportRejectMalformed)
		assertLagReportRejected(t, postLagTestReports(t, extender, now, ""), LagReportRejectMalformed)

		// another client's extender: a client reports only for extenders it
		// activated, even with a report that extender really signed
		report := signLagTestReport(t, body(), extender.privateKey)
		assertLagReportRejected(t, postLagTestReports(t, other, now, report), LagReportRejectExtender)
		// a key no extender activated under
		_, strangerKey, _ := ed25519.GenerateKey(rand.Reader)
		strangerBody := body()
		strangerBody.ExtenderPublicKey = strangerKey.Public().(ed25519.PublicKey)
		assertLagReportRejected(t, postLagTestReports(t, extender, now, signLagTestReport(t, strangerBody, strangerKey)), LagReportRejectExtender)

		// signed by another key than the extender's stored one
		assertLagReportRejected(t, postLagTestReports(t, extender, now, signLagTestReport(t, body(), other.privateKey)), LagReportRejectSignature)
		// a body changed after it was signed
		signed, err := connect.SignLagReport(body(), extender.privateKey)
		connect.AssertEqual(t, err, nil)
		tampered := body()
		tampered.UploadVerified = false
		signed.Body, err = proto.Marshal(tampered)
		connect.AssertEqual(t, err, nil)
		tamperedBytes, err := proto.Marshal(signed)
		connect.AssertEqual(t, err, nil)
		assertLagReportRejected(t, postLagTestReports(t, extender, now, base64.StdEncoding.EncodeToString(tamperedBytes)), LagReportRejectSignature)

		// no such ticket, another extender's ticket, another track, another place
		for _, edit := range []func(body *protocol.LagReportBody){
			func(body *protocol.LagReportBody) { body.TicketId = server.NewId().Bytes() },
			func(body *protocol.LagReportBody) { body.TicketId = tickets[1].TicketId.Bytes() },
			func(body *protocol.LagReportBody) { body.TrackId = server.NewId().Bytes() },
			func(body *protocol.LagReportBody) { body.TrackPosition = 1 },
		} {
			edited := body()
			edit(edited)
			assertLagReportRejected(t, postLagTestReports(t, extender, now, signLagTestReport(t, edited, extender.privateKey)), LagReportRejectTicket)
		}

		// ids of the wrong size, and a family that is neither 4 nor 6
		for _, edit := range []func(body *protocol.LagReportBody){
			func(body *protocol.LagReportBody) { body.TicketId = []byte{1, 2, 3} },
			func(body *protocol.LagReportBody) { body.TrackId = nil },
			func(body *protocol.LagReportBody) { body.IpFamily = 0 },
			func(body *protocol.LagReportBody) { body.IpFamily = 5 },
		} {
			edited := body()
			edit(edited)
			assertLagReportRejected(t, postLagTestReports(t, extender, now, signLagTestReport(t, edited, extender.privateKey)), LagReportRejectMalformed)
		}

		// out of bounds
		for _, edit := range []func(body *protocol.LagReportBody){
			func(body *protocol.LagReportBody) {
				for len(body.PingOffsetMs) <= LagReportMaxPings {
					body.PingOffsetMs = append(body.PingOffsetMs, 0)
					body.PingMicros = append(body.PingMicros, 20000)
				}
			},
			func(body *protocol.LagReportBody) { body.PingOffsetMs[10] = LagReportMaxPingOffsetMs + 1 },
			func(body *protocol.LagReportBody) {
				for len(body.UploadBytes) <= LagReportMaxIntervals {
					body.UploadBytes = append(body.UploadBytes, 1)
				}
			},
			func(body *protocol.LagReportBody) { body.DownloadSentBytes[3] = 1 << 40 },
			// a minute later than the session could have run
			func(body *protocol.LagReportBody) {
				body.WindowStartTimeMs = uint64(now.Add(time.Hour).UnixMilli())
			},
			// a minute that starts more than a minute before its session
			func(body *protocol.LagReportBody) {
				body.SessionStartTimeMs = uint64(now.Add(time.Minute).UnixMilli())
			},
		} {
			edited := body()
			edit(edited)
			assertLagReportRejected(t, postLagTestReports(t, extender, now, signLagTestReport(t, edited, extender.privateKey)), LagReportRejectBounds)
		}

		// a track past its retention is gone, and its ticket's late report
		// brings none of it back (A5)
		sweptTrack, sweptTickets := newLagTestTrack(ctx, now.Add(-2*time.Minute), extender)
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `DELETE FROM lag_track WHERE track_id = $1`, sweptTrack.TrackId))
		})
		late := signLagTestReport(t, newLagTestReportBody(extender, sweptTickets[0], windowStartTime), extender.privateKey)
		assertLagReportRejected(t, postLagTestReports(t, extender, now, late), LagReportRejectTicket)
		connect.AssertEqual(t, model.GetLagTrack(ctx, sweptTrack.TrackId), (*model.LagTrack)(nil))

		// nothing above was stored
		var resultCount int
		server.Db(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(ctx, `SELECT COUNT(*) FROM lag_result`).Scan(&resultCount))
		})
		connect.AssertEqual(t, resultCount, 0)

		// a post mixes outcomes, each named by its index
		good := signLagTestReport(t, body(), extender.privateKey)
		result := postLagTestReports(t, extender, now, "junk", good, good)
		connect.AssertEqual(t, result.Accepted, 1)
		connect.AssertEqual(t, lagTestRejections(result), []ExtenderLagReportRejection{
			{Index: 0, Reason: LagReportRejectMalformed},
			{Index: 2, Reason: LagReportRejectDuplicate},
		})
	})
}

// A post is a batch of at most report_max_count, an empty one is nothing, and
// a client past its budget is told to retry rather than answered as delivered.
func TestExtenderLagReportLimits(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		now := server.NowUtc()
		extender := newLagTestExtender(t, ctx, "192.0.2.10")
		settings := DefaultLagSettings()
		settings.ReportLimit = 2

		result, err := extenderLagReport(&ExtenderLagReportArgs{}, extender.clientSession, now, settings)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, result.Accepted, 0)
		connect.AssertEqual(t, len(result.Rejected), 0)

		tooMany := make([]string, settings.ReportMaxCount+1)
		_, err = extenderLagReport(&ExtenderLagReportArgs{Reports: tooMany}, extender.clientSession, now, settings)
		if err == nil || !strings.HasPrefix(err.Error(), "400 ") {
			t.Fatalf("a post over the batch size answered %v, want a 400", err)
		}

		// neither of those was counted; the budget is two posts that carry reports
		for range 2 {
			result, err := extenderLagReport(&ExtenderLagReportArgs{Reports: []string{"junk"}}, extender.clientSession, now, settings)
			connect.AssertEqual(t, err, nil)
			connect.AssertEqual(t, len(result.Rejected), 1)
		}
		_, err = extenderLagReport(&ExtenderLagReportArgs{Reports: []string{"junk"}}, extender.clientSession, now, settings)
		if err == nil || !strings.HasPrefix(err.Error(), "429 ") {
			t.Fatalf("a post over the budget answered %v, want a 429", err)
		}
		retryAfter, ok := err.(interface{ RetryAfterSeconds() int })
		if !ok || retryAfter.RetryAfterSeconds() < 1 || int(settings.ReportLimitWindow.Seconds()) < retryAfter.RetryAfterSeconds() {
			t.Fatalf("the refusal names no usable retry time: %v", err)
		}

		// a report needs a client
		networkSession := session.NewLocalClientSession(ctx, "198.51.100.9:4433", session.NewByJwt(server.NewId(), server.NewId(), "lag-test", false, false))
		defer networkSession.Cancel()
		_, err = extenderLagReport(&ExtenderLagReportArgs{Reports: []string{"junk"}}, networkSession, now, settings)
		if err == nil || !strings.HasPrefix(err.Error(), "401 ") {
			t.Fatalf("a report without a client answered %v, want a 401", err)
		}
	})
}
