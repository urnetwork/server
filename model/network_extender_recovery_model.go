package model

import (
	"context"
	"net/netip"
	"time"

	"github.com/urnetwork/server"
)

// The way back for an address the uptime probes deactivated (connect/EXTENDER.md
// C3; factury research extender-health-2026-11-11 §4 item 5).
//
// Six consecutive failed probes deactivate an address, and losing the last one
// revokes the extender. Until now only a new activation of the extender's own
// brought either back: a released activator posts one on its daily tick, at a
// restart or a network change, or when it sees its own revocation, which only
// gossip carries. An extender that answered again minutes after a false
// revocation stayed out of every directory until then.
//
// So the probe keeps probing an address it deactivated, for a while, and
// restores it once it answers: the same tcp challenge under the extender's
// published key at the address the record named, which is the whole liveness
// test an active address passes every five minutes. A restored address is
// active again with a clean failure count; a revoked extender is active again,
// and the record signed for it is queued in the same transaction, issued after
// the revocation so it wins over it in every directory (B5).
//
// What is restored is only what the probes took away. An address qualifies
// while it is inactive with the failure count that deactivated it, on the ip
// it was deactivated on, and last worked -- an uptime probe or its activation
// passed -- recently enough: the probes are the only writer that deactivates
// an address or revokes an extender, and an activation clears the count of an
// address it brings back. A new way of withdrawing an extender must leave the
// count below the probe's limit, or this would undo it.
//
// Nothing here weakens the removal. A dead extender still fails its six
// probes, is revoked, and never answers a probe for its return.

// One address the uptime probes deactivated, as its recovery probe reads it.
type NetworkExtenderRecoveryProbeTarget struct {
	NetworkExtenderProbeTarget
	// when the address last worked: its last passing uptime probe or its
	// last activation, whichever is later. The recovery window and its
	// backoff are counted from here.
	LastWorkTime time.Time
	// the address's last probe, nil if it never had one. A failed recovery
	// probe stamps it, so a probe for the return backs off.
	LastProbeTime *time.Time
	// when the extender was revoked; nil while another of its addresses keeps
	// it active
	RevokeTime *time.Time
}

// GetNetworkExtenderRecoveryProbeTargets reads every address the uptime probes
// deactivated that last worked at or after lastWorkedSince, oldest probe first.
//
// An address the probes deactivated holds the failure count that deactivated
// it, minConsecutiveProbeFailures or more. The extender is active, with
// another address, or revoked; the probes leave no other state.
func GetNetworkExtenderRecoveryProbeTargets(
	ctx context.Context,
	lastWorkedSince time.Time,
	minConsecutiveProbeFailures int,
) []*NetworkExtenderRecoveryProbeTarget {
	targets := []*NetworkExtenderRecoveryProbeTarget{}

	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
			SELECT
				network_extender.extender_id,
				network_extender.network_id,
				network_extender.client_id,
				network_extender.public_key,
				network_extender.tcp_port,
				network_extender.dns_tld,
				network_extender_address.ip_version,
				network_extender_address.ip,
				network_extender_address.carriers,
				CASE WHEN network_extender.reality_public_key_network_id = network_extender.network_id THEN network_extender.reality_public_key END,
				GREATEST(network_extender_address.activate_time, network_extender_address.last_probe_success_time),
				network_extender_address.last_probe_time,
				network_extender.revoke_time,
				COALESCE(network_extender_address.tcp_port, 0)
			FROM network_extender_address
			INNER JOIN network_extender ON
				network_extender.extender_id = network_extender_address.extender_id
			WHERE
				NOT network_extender_address.active AND
				$2 <= network_extender_address.consecutive_probe_failures AND
				$1 <= GREATEST(network_extender_address.activate_time, network_extender_address.last_probe_success_time) AND
				(network_extender.active OR network_extender.revoke_time IS NOT NULL)
			ORDER BY network_extender_address.last_probe_time ASC NULLS FIRST
			`,
			lastWorkedSince.UTC(),
			minConsecutiveProbeFailures,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				target := &NetworkExtenderRecoveryProbeTarget{}
				var carriers string
				var addressTcpPort int
				server.Raise(result.Scan(
					&target.ExtenderId,
					&target.NetworkId,
					// the client a webrtc address's return is probed through
					// (connect/EXTENDER.md S8)
					&target.ClientId,
					&target.PublicKey,
					&target.TcpPort,
					&target.DnsTld,
					&target.IpVersion,
					&target.Ip,
					&carriers,
					&target.RealityPublicKey,
					&target.LastWorkTime,
					&target.LastProbeTime,
					&target.RevokeTime,
					&addressTcpPort,
				))
				target.Carriers = splitExtenderCarriers(carriers)
				// probed for its return where it was probed before: on its own
				// port (connect/EXTENDER.md S10), or the extender's for an
				// address activated before its own was kept
				if 0 < addressTcpPort {
					target.TcpPort = addressTcpPort
				}
				targets = append(targets, target)
			}
		})
	})

	return targets
}

// What one recovery probe result changed.
type NetworkExtenderRecoveryOutcome struct {
	// the address is active again
	AddressRestored bool
	// the extender had been revoked and is active again
	ExtenderRestored bool
	// how long the extender had been revoked, when ExtenderRestored
	RevokedDuration time.Duration
	// a record naming the restored address was queued for the open channels
	RecordQueued bool
}

// RecordNetworkExtenderRecoveryProbeResult applies one recovery probe result.
//
// A failure changes nothing but the address's last probe time, which is what
// the recovery backs off on. A success restores the address when it still
// qualifies (GetNetworkExtenderRecoveryProbeTargets) on the ip that was probed:
// it is active again with no failures, and the probe that passed is its last
// probe and its last success. A revoked extender is active again, its
// revocation cleared. Either way a record of the extender's active addresses
// is signed and, for an extender the open channels carry, queued in the same
// transaction, with record_issue_time raised to it, exactly as an activation
// queues one. The record is issued after the revocation, which is what makes a
// directory that holds the revocation take the extender back (B5).
//
// A result for an address that no longer qualifies -- re-activated meanwhile,
// moved to another ip, out of the window -- changes nothing.
func RecordNetworkExtenderRecoveryProbeResult(
	ctx context.Context,
	extenderId server.Id,
	ipVersion int,
	ip netip.Addr,
	success bool,
	probeTime time.Time,
	lastWorkedSince time.Time,
	minConsecutiveProbeFailures int,
	signRecord NetworkExtenderRecordSigner,
) *NetworkExtenderRecoveryOutcome {
	outcome := &NetworkExtenderRecoveryOutcome{}

	server.Tx(ctx, func(tx server.PgTx) {
		*outcome = NetworkExtenderRecoveryOutcome{}

		if !success {
			server.RaisePgResult(tx.Exec(
				ctx,
				`
				UPDATE network_extender_address
				SET last_probe_time = $4
				WHERE extender_id = $1 AND ip_version = $2 AND ip = $3 AND NOT active
				`,
				extenderId,
				ipVersion,
				ip,
				probeTime.UTC(),
			))
			return
		}

		// the extender first, then the address, in the order an activation
		// locks them
		extender := getNetworkExtenderInTx(ctx, tx, extenderId)
		if extender == nil || (!extender.Active && extender.RevokeTime == nil) {
			// not a state the probes leave
			return
		}

		restored := false
		result, err := tx.Query(
			ctx,
			`
			UPDATE network_extender_address
			SET
				active = true,
				consecutive_probe_failures = 0,
				last_probe_time = $4,
				last_probe_success_time = $4
			WHERE
				extender_id = $1 AND
				ip_version = $2 AND
				ip = $3 AND
				NOT active AND
				$5 <= consecutive_probe_failures AND
				$6 <= GREATEST(activate_time, last_probe_success_time)
			RETURNING ip_version
			`,
			extenderId,
			ipVersion,
			ip,
			probeTime.UTC(),
			minConsecutiveProbeFailures,
			lastWorkedSince.UTC(),
		)
		server.WithPgResult(result, err, func() {
			restored = result.Next()
		})
		if !restored {
			return
		}

		issueTime := server.NowUtc()
		if extender.RevokeTime != nil {
			// a record at or before the revocation's issue time loses to it
			// (B5), whatever the clocks of the hosts that wrote them say
			if revokedAfter := extender.RevokeTime.Add(time.Millisecond); issueTime.Before(revokedAfter) {
				issueTime = revokedAfter
			}
		}
		outcome.AddressRestored = true

		if !extender.Active {
			server.RaisePgResult(tx.Exec(
				ctx,
				`
				UPDATE network_extender
				SET active = true, revoke_time = NULL
				WHERE extender_id = $1
				`,
				extenderId,
			))
			outcome.ExtenderRestored = true
			outcome.RevokedDuration = max(0, probeTime.Sub(*extender.RevokeTime))
			extender.Active = true
			extender.RevokeTime = nil
		}

		if !extender.OpenChannelPublishable() {
			// a gated extender and a canary are carried by no open channel
			// (R1, R4); a release signs the gated one's record when asked
			return
		}
		addresses := getActiveNetworkExtenderAddressesInTx(ctx, tx, extenderId)
		extender.DerivedCountryCode = derivedCountryCodeInTx(ctx, tx, extenderId)
		extender.MeasurementCertNotAfter = networkExtenderMeasurementCertNotAfterInTx(ctx, tx, extenderId)
		extender.RecordIssueTime = &issueTime
		message, err := signRecord(extender, addresses, issueTime)
		server.Raise(err)
		insertNetworkExtenderPublishInTx(
			ctx,
			tx,
			extenderId,
			NetworkExtenderPublishKindRecord,
			message,
			issueTime,
		)
		server.RaisePgResult(tx.Exec(
			ctx,
			`
			UPDATE network_extender
			SET record_issue_time = $2
			WHERE extender_id = $1
			`,
			extenderId,
			issueTime,
		))
		outcome.RecordQueued = true
	})

	return outcome
}
