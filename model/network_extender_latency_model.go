package model

import (
	"context"
	"slices"
	"time"

	"github.com/urnetwork/server"
)

// The latency attestations providers make to extenders
// (connect/DESIGNNOTES4.md §3), one row per accepted attestation.
//
// A row is written only after the operator verified the provider's signature
// and the reporting extender's ownership, so what is here is what a provider
// signed and an extender saw. The unique key over the extender, the provider
// and the nonce makes a replayed report a no-op rather than a second sample.
//
// This is the target-reported ingest, kept accepting for one release so the
// extenders already in the field keep forwarding (connect/GEOMAP.md §2.5,
// D14). The pinger-reported, co-signed rows that supersede it are
// network_ping (network_ping_model.go); nothing new is built on this table,
// and it drains on its own thirty day retention.

// One accepted attestation.
type NetworkExtenderLatency struct {
	LatencyId  server.Id
	ExtenderId server.Id
	// the attesting provider
	ClientId   server.Id
	ProbeNonce []byte
	RttMs      int
	// the provider's own timestamp on the attestation
	ProbeTime  time.Time
	CreateTime time.Time
}

// Stores verified attestations and reports how many were new. A duplicate --
// the same extender, provider and nonce -- is skipped, which is what makes a
// replayed report harmless.
func AddNetworkExtenderLatencies(
	ctx context.Context,
	latencies []*NetworkExtenderLatency,
) (insertedCount int) {
	if len(latencies) == 0 {
		return 0
	}
	server.Tx(ctx, func(tx server.PgTx) {
		insertedCount = 0
		for _, latency := range latencies {
			latencyId := server.NewId()
			createTime := latency.CreateTime
			if createTime.IsZero() {
				createTime = server.NowUtc()
			}
			tag, err := tx.Exec(
				ctx,
				`
				INSERT INTO network_extender_latency (
					latency_id,
					extender_id,
					client_id,
					probe_nonce,
					rtt_ms,
					probe_time,
					create_time
				)
				VALUES ($1, $2, $3, $4, $5, $6, $7)
				ON CONFLICT (extender_id, client_id, probe_nonce) DO NOTHING
				`,
				latencyId,
				latency.ExtenderId,
				latency.ClientId,
				latency.ProbeNonce,
				latency.RttMs,
				latency.ProbeTime.UTC(),
				createTime.UTC(),
			)
			server.Raise(err)
			insertedCount += int(tag.RowsAffected())
		}
	})
	return insertedCount
}

// The attestations of one extender created at or after `minCreateTime`,
// oldest first.
func GetNetworkExtenderLatencies(
	ctx context.Context,
	extenderId server.Id,
	minCreateTime time.Time,
) []*NetworkExtenderLatency {
	latencies := []*NetworkExtenderLatency{}
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
			SELECT
				latency_id,
				client_id,
				probe_nonce,
				rtt_ms,
				probe_time,
				create_time
			FROM network_extender_latency
			WHERE extender_id = $1 AND $2 <= create_time
			ORDER BY create_time ASC
			`,
			extenderId,
			minCreateTime.UTC(),
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				latency := &NetworkExtenderLatency{ExtenderId: extenderId}
				server.Raise(result.Scan(
					&latency.LatencyId,
					&latency.ClientId,
					&latency.ProbeNonce,
					&latency.RttMs,
					&latency.ProbeTime,
					&latency.CreateTime,
				))
				latencies = append(latencies, latency)
			}
		})
	})
	return latencies
}

// The retention sweep: rows created before `minCreateTime` go.
func RemoveOldNetworkExtenderLatencies(ctx context.Context, minCreateTime time.Time) (removedCount int) {
	server.Tx(ctx, func(tx server.PgTx) {
		tag, err := tx.Exec(
			ctx,
			`
			DELETE FROM network_extender_latency
			WHERE create_time < $1
			`,
			minCreateTime.UTC(),
		)
		server.Raise(err)
		removedCount = int(tag.RowsAffected())
	})
	return removedCount
}

// The extender behind one identity key, active or not, or nil when none has
// activated under it. The latency report attributes an attestation to its
// extender by this key and then checks the reporting client owns it; the ping
// report resolves both its pinger extender and its target this way, and
// verifies each signature under the key stored here rather than one the
// report brings.
func GetNetworkExtenderByPublicKey(ctx context.Context, publicKey []byte) *NetworkExtender {
	var extender *NetworkExtender
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
			SELECT
				extender_id,
				network_id,
				client_id,
				create_time,
				tcp_port,
				udp_port,
				dns_port,
				dns_tld,
				country_code,
				active,
				revoke_time,
				record_issue_time,
				location_id,
				city_location_id,
				region_location_id,
				country_location_id
			FROM network_extender
			WHERE public_key = $1
			`,
			publicKey,
		)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				// the key is copied, not aliased: it is the caller's buffer
				e := &NetworkExtender{PublicKey: slices.Clone(publicKey)}
				server.Raise(result.Scan(
					&e.ExtenderId,
					&e.NetworkId,
					&e.ClientId,
					&e.CreateTime,
					&e.TcpPort,
					&e.UdpPort,
					&e.DnsPort,
					&e.DnsTld,
					&e.CountryCode,
					&e.Active,
					&e.RevokeTime,
					&e.RecordIssueTime,
					&e.LocationId,
					&e.CityLocationId,
					&e.RegionLocationId,
					&e.CountryLocationId,
				))
				extender = e
			}
		})
	})
	return extender
}
