package model

import (
	"context"
	"time"

	"github.com/urnetwork/server"
)

// lag.dev tracks, their tickets and their results (lag.dev/PLAN.md A5-A7).
//
//   - lag_track is one track: the 8 dots (and spares) a visitor measured
//     together, whose sessions share a track id. Its client fields describe
//     the visitor's network and city-level place, looked up when the track is
//     issued; the address is never stored (amendment 9). A track from an
//     agent browser or a hosting network is flagged automated, and its
//     results are left out of every aggregate (amendment 12). It counts the
//     tickets reissued in it, so a dropped session can reconnect within its
//     track, once per dot and a bounded number of times in all (A6).
//   - lag_ticket is one session the operator admitted (A6), kept for its
//     report, which arrives when the session's minute closes or the session
//     does. A session has no time limit, so a ticket is kept for
//     ticket_retention rather than to its expiry.
//   - lag_result is one report (A7): the one minute of pings kept per dot per
//     track (decision 17), the load, and statistics derived when it was
//     stored. A ticket has at most one.
//
// Geolocation never reads these tables (decision 8). Timestamps are naive
// columns holding utc, as everywhere else in this schema.

// Why a track is automated (amendment 12): an agent browser that signs its
// requests (Web Bot Auth's Signature-Agent), or an address in a hosting or
// cloud network. Only the reason is kept, never the agent's name or the
// address.
const (
	LagAutomatedReasonSignatureAgent = "signature_agent"
	LagAutomatedReasonDatacenterAsn  = "datacenter_asn"
)

// Every automated reason, in the order the stats publish them.
var LagAutomatedReasons = []string{
	LagAutomatedReasonSignatureAgent,
	LagAutomatedReasonDatacenterAsn,
}

// The client that ran a track: lag.dev in a browser, or the lagdev command
// line or MCP server, which name themselves in their requests. Every source
// counts in the stats; the stats also split by it.
const (
	LagSourceWeb = "web"
	LagSourceCli = "cli"
	LagSourceMcp = "mcp"
)

// Every source, in the order the stats publish them.
var LagSources = []string{
	LagSourceWeb,
	LagSourceCli,
	LagSourceMcp,
}

// One track as issued (A6).
type LagTrack struct {
	TrackId   server.Id
	NetworkId server.Id
	ClientId  server.Id
	StartedAt time.Time
	// the tickets reissued in the track so far
	ReissueCount int
	// the client fields stored with a new track; nil stores none
	Client *LagTrackClient
	// one of LagAutomatedReasons when the track came from an agent browser
	// or a hosting network, else "": its results count in no aggregate
	AutomatedReason string
	// one of LagSources; "" stores web
	Source string
}

// Whether the track came from an agent browser or a hosting network.
func (self *LagTrack) Automated() bool {
	return self.AutomatedReason != ""
}

// The source a track is stored with: web unless it names another known one.
func lagTrackSource(source string) string {
	switch source {
	case LagSourceCli, LagSourceMcp:
		return source
	default:
		return LagSourceWeb
	}
}

// One admitted session (A6). A ticket admits a session on an extender, by
// its measurement name, which serves both of its address families (N3).
type LagTicket struct {
	TicketId server.Id
	TrackId  server.Id
	// the dot's place: 0-7, spares 8-11
	Position   int
	ClientId   server.Id
	NetworkId  server.Id
	ExtenderId server.Id
	IssuedAt   time.Time
	ExpiresAt  time.Time
}

// Stores a track, its client fields and its tickets in one transaction.
func CreateLagTrack(ctx context.Context, track *LagTrack, tickets []*LagTicket) {
	client := track.Client
	if client == nil {
		client = &LagTrackClient{}
	}
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(
			ctx,
			`
			INSERT INTO lag_track (
				track_id,
				network_id,
				client_id,
				started_at,
				client_asn,
				client_asn_org,
				client_country_code,
				client_region,
				client_city,
				client_latitude,
				client_longitude,
				automated,
				automated_reason,
				source
			)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14)
			`,
			track.TrackId,
			track.NetworkId,
			track.ClientId,
			track.StartedAt.UTC(),
			client.Asn,
			nullString(client.AsnOrg),
			nullString(client.CountryCode),
			nullString(client.Region),
			nullString(client.City),
			client.Latitude,
			client.Longitude,
			track.Automated(),
			nullString(track.AutomatedReason),
			lagTrackSource(track.Source),
		))
		insertLagTicketsInTx(ctx, tx, tickets)
	})
}

func insertLagTicketsInTx(ctx context.Context, tx server.PgTx, tickets []*LagTicket) {
	server.BatchInTx(ctx, tx, func(batch server.PgBatch) {
		for _, ticket := range tickets {
			batch.Queue(
				`
				INSERT INTO lag_ticket (
					ticket_id,
					track_id,
					position,
					client_id,
					network_id,
					extender_id,
					issued_at,
					expires_at
				)
				VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
				`,
				ticket.TicketId,
				ticket.TrackId,
				ticket.Position,
				ticket.ClientId,
				ticket.NetworkId,
				ticket.ExtenderId,
				ticket.IssuedAt.UTC(),
				ticket.ExpiresAt.UTC(),
			)
		}
	})
}

// One track, or nil when there is none (never issued, or past retention).
func GetLagTrack(ctx context.Context, trackId server.Id) *LagTrack {
	var track *LagTrack
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
			SELECT network_id, client_id, started_at, reissue_count, automated_reason, source
			FROM lag_track
			WHERE track_id = $1
			`,
			trackId,
		)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				track = &LagTrack{TrackId: trackId}
				var automatedReason *string
				server.Raise(result.Scan(
					&track.NetworkId,
					&track.ClientId,
					&track.StartedAt,
					&track.ReissueCount,
					&automatedReason,
					&track.Source,
				))
				if automatedReason != nil {
					track.AutomatedReason = *automatedReason
				}
			}
		})
	})
	return track
}

// Every ticket of a track, reissued ones included, by position and then
// issue time.
func GetLagTrackTickets(ctx context.Context, trackId server.Id) []*LagTicket {
	tickets := []*LagTicket{}
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
			SELECT
				ticket_id,
				position,
				client_id,
				network_id,
				extender_id,
				issued_at,
				expires_at
			FROM lag_ticket
			WHERE track_id = $1
			ORDER BY position, issued_at, ticket_id
			`,
			trackId,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				ticket := &LagTicket{TrackId: trackId}
				server.Raise(result.Scan(
					&ticket.TicketId,
					&ticket.Position,
					&ticket.ClientId,
					&ticket.NetworkId,
					&ticket.ExtenderId,
					&ticket.IssuedAt,
					&ticket.ExpiresAt,
				))
				tickets = append(tickets, ticket)
			}
		})
	})
	return tickets
}

// Stores tickets reissued in a track, at most one per position and at most
// maxReissues in all, and answers false, storing nothing, when the track has
// no room for them (A6). Each position of a track got one ticket when the
// track was issued, so a position that already holds two was reissued. The
// tickets' positions are distinct. The track's row is locked first, so
// concurrent reissues of one track are checked one after the other.
func AddLagTrackReissue(ctx context.Context, trackId server.Id, tickets []*LagTicket, maxReissues int) (added bool) {
	positions := make([]int, 0, len(tickets))
	for _, ticket := range tickets {
		positions = append(positions, ticket.Position)
	}
	server.Tx(ctx, func(tx server.PgTx) {
		// a rerun starts over
		added = false

		var reissueCount int
		found := false
		result, err := tx.Query(
			ctx,
			`
			SELECT reissue_count
			FROM lag_track
			WHERE track_id = $1
			FOR UPDATE
			`,
			trackId,
		)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				server.Raise(result.Scan(&reissueCount))
				found = true
			}
		})
		if !found || maxReissues < reissueCount+len(tickets) {
			return
		}

		reissued := false
		result, err = tx.Query(
			ctx,
			`
			SELECT EXISTS (
				SELECT 1
				FROM lag_ticket
				WHERE track_id = $1 AND position = ANY($2)
				GROUP BY position
				HAVING 1 < COUNT(*)
			)
			`,
			trackId,
			positions,
		)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				server.Raise(result.Scan(&reissued))
			}
		})
		if reissued {
			return
		}

		server.RaisePgResult(tx.Exec(
			ctx,
			`
			UPDATE lag_track
			SET reissue_count = reissue_count + $2
			WHERE track_id = $1
			`,
			trackId,
			len(tickets),
		))
		insertLagTicketsInTx(ctx, tx, tickets)
		added = true
	})
	return
}

// One ticket, or nil when there is none (never issued, or past retention).
func GetLagTicket(ctx context.Context, ticketId server.Id) *LagTicket {
	var ticket *LagTicket
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
			SELECT
				track_id,
				position,
				client_id,
				network_id,
				extender_id,
				issued_at,
				expires_at
			FROM lag_ticket
			WHERE ticket_id = $1
			`,
			ticketId,
		)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				ticket = &LagTicket{TicketId: ticketId}
				server.Raise(result.Scan(
					&ticket.TrackId,
					&ticket.Position,
					&ticket.ClientId,
					&ticket.NetworkId,
					&ticket.ExtenderId,
					&ticket.IssuedAt,
					&ticket.ExpiresAt,
				))
			}
		})
	})
	return ticket
}

// The client fields of a track (A5): the network and the city-level place of
// the address the track was issued to, looked up when it was issued, with
// the city's coordinates as GeoLite2 City gives them. Never the address, a
// prefix of it or a hash of it (amendment 9). Each is nil or empty when the
// lookup did not reach it; the coordinates are nil unless it named a city.
type LagTrackClient struct {
	Asn         *int64
	AsnOrg      string
	CountryCode string
	Region      string
	City        string
	Latitude    *float64
	Longitude   *float64
}

// One report as stored (A5, A7).
type LagResult struct {
	LagResultId   server.Id
	TrackId       server.Id
	TrackPosition int
	TicketId      server.Id
	ExtenderId    server.Id
	// 4 or 6: the family the browser reached the extender on, which is the
	// family of the address the report saw it from
	ExtenderIpFamily int
	// the extender's clock: when the session opened, and when the minute
	// starts. CreatedAt, the report's arrival on the operator's clock, is
	// beside them, so the extender's offset can be estimated
	SessionStartTime time.Time
	// the minute
	WindowStartTime time.Time
	PingOffsetMs    []int32
	// 0 is a lost ping
	PingMicros []int32
	// the load, in ms after WindowStartTime; a zero duration is none
	DownloadStartMs       int
	DownloadMs            int
	UploadStartMs         int
	UploadMs              int
	IntervalMs            int
	DownloadReceivedBytes []int64
	DownloadSentBytes     []int64
	UploadBytes           []int64
	DownloadStreams       int
	UploadStreams         int
	UploadVerified        bool
	EndReason             string
	// derived when stored; nil when there was nothing to derive from
	OwnLoadLatencyP50Ms     *float32
	OwnLoadLatencyP95Ms     *float32
	OutsideLoadLatencyP50Ms *float32
	OutsideLoadLatencyP95Ms *float32
	DownloadP50Mbps         *float32
	DownloadP95Mbps         *float32
	UploadP50Mbps           *float32
	UploadP95Mbps           *float32
	LostCount               int
	CreatedAt               time.Time
}

// Stores one report. False when the ticket already has a report (A7), or
// when its track is gone, past its retention: a late report never brings back
// data for a swept track. Nothing is written then.
func AddLagResult(ctx context.Context, result *LagResult) (added bool) {
	server.Tx(ctx, func(tx server.PgTx) {
		// the track row is held until the result is in, so the sweep cannot
		// remove it underneath
		rows, err := tx.Query(
			ctx,
			`SELECT track_id FROM lag_track WHERE track_id = $1 FOR SHARE`,
			result.TrackId,
		)
		trackExists := false
		server.WithPgResult(rows, err, func() {
			trackExists = rows.Next()
		})
		if !trackExists {
			return
		}

		tag, err := tx.Exec(
			ctx,
			`
			INSERT INTO lag_result (
				lag_result_id,
				track_id,
				track_position,
				ticket_id,
				extender_id,
				extender_ip_family,
				session_start_time,
				window_start_time,
				ping_offset_ms,
				ping_micros,
				download_start_ms,
				download_ms,
				upload_start_ms,
				upload_ms,
				interval_ms,
				download_received_bytes,
				download_sent_bytes,
				upload_bytes,
				download_streams,
				upload_streams,
				upload_verified,
				end_reason,
				own_load_latency_p50_ms,
				own_load_latency_p95_ms,
				outside_load_latency_p50_ms,
				outside_load_latency_p95_ms,
				download_p50_mbps,
				download_p95_mbps,
				upload_p50_mbps,
				upload_p95_mbps,
				lost_count,
				created_at
			)
			VALUES (
				$1, $2, $3, $4, $5, $6, $7, $8, $9, $10,
				$11, $12, $13, $14, $15, $16, $17, $18, $19, $20,
				$21, $22, $23, $24, $25, $26, $27, $28, $29, $30,
				$31, $32
			)
			ON CONFLICT (ticket_id) DO NOTHING
			`,
			result.LagResultId,
			result.TrackId,
			result.TrackPosition,
			result.TicketId,
			result.ExtenderId,
			result.ExtenderIpFamily,
			result.SessionStartTime.UTC(),
			result.WindowStartTime.UTC(),
			nonNilInt32s(result.PingOffsetMs),
			nonNilInt32s(result.PingMicros),
			result.DownloadStartMs,
			result.DownloadMs,
			result.UploadStartMs,
			result.UploadMs,
			result.IntervalMs,
			nonNilInt64s(result.DownloadReceivedBytes),
			nonNilInt64s(result.DownloadSentBytes),
			nonNilInt64s(result.UploadBytes),
			result.DownloadStreams,
			result.UploadStreams,
			result.UploadVerified,
			result.EndReason,
			result.OwnLoadLatencyP50Ms,
			result.OwnLoadLatencyP95Ms,
			result.OutsideLoadLatencyP50Ms,
			result.OutsideLoadLatencyP95Ms,
			result.DownloadP50Mbps,
			result.DownloadP95Mbps,
			result.UploadP50Mbps,
			result.UploadP95Mbps,
			result.LostCount,
			result.CreatedAt.UTC(),
		)
		server.Raise(err)
		added = tag.RowsAffected() == 1
	})
	return
}

// An array column is never null; an absent series is an empty one.
func nonNilInt32s(values []int32) []int32 {
	if values == nil {
		return []int32{}
	}
	return values
}

func nonNilInt64s(values []int64) []int64 {
	if values == nil {
		return []int64{}
	}
	return values
}

func nullString(value string) *string {
	if value == "" {
		return nil
	}
	return &value
}

// What one retention pass removed.
type RemoveExpiredLagResultsResult struct {
	Tickets int `json:"tickets"`
	Results int `json:"results"`
	Tracks  int `json:"tracks"`
}

// The rows one retention statement deletes at most.
const lagRetentionBatchSize = 10000

// Deletes, in bounded batches, the tickets issued before minTicketIssuedAt,
// and everything of a track started before minRetainedAt: its results,
// however late they arrived, and then the track with its client fields once
// no result of it is left. A result stored before minRetainedAt goes too,
// whatever its track. So nothing of a track outlives the retention (A5).
func RemoveExpiredLagResults(
	ctx context.Context,
	minTicketIssuedAt time.Time,
	minRetainedAt time.Time,
) RemoveExpiredLagResultsResult {
	removed := RemoveExpiredLagResultsResult{}
	deleteBatches := func(statement string, minTime time.Time) int {
		total := 0
		for {
			var count int
			server.MaintenanceTx(ctx, func(tx server.PgTx) {
				tag, err := tx.Exec(ctx, statement, minTime.UTC(), lagRetentionBatchSize)
				server.Raise(err)
				count = int(tag.RowsAffected())
			})
			total += count
			if count < lagRetentionBatchSize || ctx.Err() != nil {
				return total
			}
		}
	}
	removed.Tickets = deleteBatches(
		`
		DELETE FROM lag_ticket
		WHERE ticket_id IN (
			SELECT ticket_id
			FROM lag_ticket
			WHERE issued_at < $1
			ORDER BY issued_at, ticket_id
			LIMIT $2
		)
		`,
		minTicketIssuedAt,
	)
	removed.Results = deleteBatches(
		`
		DELETE FROM lag_result
		WHERE lag_result_id IN (
			SELECT lag_result.lag_result_id
			FROM lag_track
			INNER JOIN lag_result ON lag_result.track_id = lag_track.track_id
			WHERE lag_track.started_at < $1
			ORDER BY lag_track.started_at, lag_result.lag_result_id
			LIMIT $2
		)
		`,
		minRetainedAt,
	)
	removed.Results += deleteBatches(
		`
		DELETE FROM lag_result
		WHERE lag_result_id IN (
			SELECT lag_result_id
			FROM lag_result
			WHERE created_at < $1
			ORDER BY created_at, lag_result_id
			LIMIT $2
		)
		`,
		minRetainedAt,
	)
	removed.Tracks = deleteBatches(
		`
		DELETE FROM lag_track
		WHERE track_id IN (
			SELECT track_id
			FROM lag_track
			WHERE
				started_at < $1 AND
				NOT EXISTS (
					SELECT 1 FROM lag_result WHERE lag_result.track_id = lag_track.track_id
				)
			ORDER BY started_at, track_id
			LIMIT $2
		)
		`,
		minRetainedAt,
	)
	return removed
}
