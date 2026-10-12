package model

import (
	"context"
	"net/netip"
	"time"

	"github.com/urnetwork/server"
)

// lag.dev tracks, their tickets and their results (lag.dev/PLAN.md A5-A7).
//
//   - lag_track is one track: the 8 dots (and spares) a visitor measured
//     together, whose sessions share a track id. Its client fields describe
//     the visitor's address, filled from the track's first report; the raw
//     address is never stored.
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

// One track as issued (A6).
type LagTrack struct {
	TrackId   server.Id
	NetworkId server.Id
	ClientId  server.Id
	StartedAt time.Time
}

// One admitted session (A6).
type LagTicket struct {
	TicketId server.Id
	TrackId  server.Id
	// the dot's place: 0-7, spares 8-11
	Position   int
	ClientId   server.Id
	NetworkId  server.Id
	ExtenderId server.Id
	// the extender address the ticket admits a session on
	Ip        netip.Addr
	IssuedAt  time.Time
	ExpiresAt time.Time
}

// Stores a track and its tickets in one transaction.
func CreateLagTrack(ctx context.Context, track *LagTrack, tickets []*LagTicket) {
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(
			ctx,
			`
			INSERT INTO lag_track (track_id, network_id, client_id, started_at)
			VALUES ($1, $2, $3, $4)
			`,
			track.TrackId,
			track.NetworkId,
			track.ClientId,
			track.StartedAt.UTC(),
		))
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
						ip,
						issued_at,
						expires_at
					)
					VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
					`,
					ticket.TicketId,
					ticket.TrackId,
					ticket.Position,
					ticket.ClientId,
					ticket.NetworkId,
					ticket.ExtenderId,
					ticket.Ip,
					ticket.IssuedAt.UTC(),
					ticket.ExpiresAt.UTC(),
				)
			}
		})
	})
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
				ip,
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
					&ticket.Ip,
					&ticket.IssuedAt,
					&ticket.ExpiresAt,
				))
			}
		})
	})
	return ticket
}

// The client fields of a track (A5), derived from the address its first
// report names. Each is nil or empty when the lookup did not reach it.
type LagTrackClient struct {
	// server.ClientIpHashForAddr: peppered, over the address's /29 (/56)
	AddressHash []byte
	// the /24 (/48) as text
	Prefix      string
	Asn         *int64
	AsnOrg      string
	CountryCode string
	Region      string
	City        string
	// rounded to 0.1 degree
	Latitude  *float64
	Longitude *float64
}

// One report as stored (A5, A7).
type LagResult struct {
	LagResultId   server.Id
	TrackId       server.Id
	TrackPosition int
	TicketId      server.Id
	ExtenderId    server.Id
	// 4 or 6
	ExtenderIpFamily int
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

// Stores one report and, when it is the track's first, the track's client
// fields, in one transaction. False when the ticket already has a report (A7),
// in which case nothing is written.
func AddLagResult(ctx context.Context, result *LagResult, client *LagTrackClient) (added bool) {
	server.Tx(ctx, func(tx server.PgTx) {
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
				$31
			)
			ON CONFLICT (ticket_id) DO NOTHING
			`,
			result.LagResultId,
			result.TrackId,
			result.TrackPosition,
			result.TicketId,
			result.ExtenderId,
			result.ExtenderIpFamily,
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
		if !added || client == nil {
			return
		}
		// only the track's first report fills the fields; a concurrent first
		// report waits on the row and then finds them filled
		server.RaisePgResult(tx.Exec(
			ctx,
			`
			UPDATE lag_track
			SET
				client_fields_at = $2,
				client_address_hash = $3,
				client_prefix = $4,
				client_asn = $5,
				client_asn_org = $6,
				client_country_code = $7,
				client_region = $8,
				client_city = $9,
				client_latitude = $10,
				client_longitude = $11
			WHERE track_id = $1 AND client_fields_at IS NULL
			`,
			result.TrackId,
			result.CreatedAt.UTC(),
			client.AddressHash,
			nullString(client.Prefix),
			client.Asn,
			nullString(client.AsnOrg),
			nullString(client.CountryCode),
			nullString(client.Region),
			nullString(client.City),
			client.Latitude,
			client.Longitude,
		))
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
// the results stored before minResultCreatedAt and the tracks started before
// minTrackStartedAt. A track's reports all arrive while its tickets are kept,
// so a track cut at the result retention plus the ticket retention outlives
// every result it has.
func RemoveExpiredLagResults(
	ctx context.Context,
	minTicketIssuedAt time.Time,
	minResultCreatedAt time.Time,
	minTrackStartedAt time.Time,
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
			SELECT lag_result_id
			FROM lag_result
			WHERE created_at < $1
			ORDER BY created_at, lag_result_id
			LIMIT $2
		)
		`,
		minResultCreatedAt,
	)
	removed.Tracks = deleteBatches(
		`
		DELETE FROM lag_track
		WHERE track_id IN (
			SELECT track_id
			FROM lag_track
			WHERE started_at < $1
			ORDER BY started_at, track_id
			LIMIT $2
		)
		`,
		minTrackStartedAt,
	)
	return removed
}
