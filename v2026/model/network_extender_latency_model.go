package model

import (
	"context"
	"time"

	"github.com/urnetwork/server/v2026"
)

// The latency attestations providers make to extenders
// (connect/DESIGNNOTES4.md §3), one row per accepted attestation.
//
// A row is written only after the operator verified the provider's signature
// and the reporting extender's ownership, so what is here is what a provider
// signed and an extender saw. The unique key over the extender, the provider
// and the nonce makes a replayed report a no-op rather than a second sample.

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

// AddNetworkExtenderLatencies stores verified attestations and reports how
// many were new. A duplicate -- the same extender, provider and nonce -- is
// skipped, which is what makes a replayed report harmless.
func AddNetworkExtenderLatencies(
	ctx context.Context,
	latencies []*NetworkExtenderLatency,
) (inserted int) {
	if len(latencies) == 0 {
		return 0
	}
	server.Tx(ctx, func(tx server.PgTx) {
		inserted = 0
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
			inserted += int(tag.RowsAffected())
		}
	})
	return inserted
}

// GetNetworkExtenderLatencies reads the attestations of one extender created
// at or after `minCreateTime`, oldest first.
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

// RemoveOldNetworkExtenderLatencies is the retention sweep: rows created
// before `minCreateTime` go.
func RemoveOldNetworkExtenderLatencies(ctx context.Context, minCreateTime time.Time) (removed int) {
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
		removed = int(tag.RowsAffected())
	})
	return removed
}

// GetNetworkExtenderByPublicKey reads the extender behind one identity key,
// active or not, or nil when none has activated under it. The latency report
// attributes an attestation to its extender by this key and then checks the
// reporting client owns it.
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
				record_issue_time
			FROM network_extender
			WHERE public_key = $1
			`,
			publicKey,
		)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				e := &NetworkExtender{PublicKey: publicKey}
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
				))
				extender = e
			}
		})
	})
	return extender
}
